package app

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/0cv/herdr-mobile-relay/internal/activity"
	"github.com/0cv/herdr-mobile-relay/internal/appdeploy"
	"github.com/0cv/herdr-mobile-relay/internal/audit"
	"github.com/0cv/herdr-mobile-relay/internal/clipboard"
	"github.com/0cv/herdr-mobile-relay/internal/config"
	"github.com/0cv/herdr-mobile-relay/internal/conversation"
	"github.com/0cv/herdr-mobile-relay/internal/coordinator"
	"github.com/0cv/herdr-mobile-relay/internal/copyresponse"
	"github.com/0cv/herdr-mobile-relay/internal/fsutil"
	"github.com/0cv/herdr-mobile-relay/internal/herdr"
	"github.com/0cv/herdr-mobile-relay/internal/history"
	"github.com/0cv/herdr-mobile-relay/internal/noecho"
	"github.com/0cv/herdr-mobile-relay/internal/panesize"
	"github.com/0cv/herdr-mobile-relay/internal/profiles"
	"github.com/0cv/herdr-mobile-relay/internal/protocol"
	"github.com/0cv/herdr-mobile-relay/internal/push"
	"github.com/0cv/herdr-mobile-relay/internal/question"
	"github.com/0cv/herdr-mobile-relay/internal/session"
	"github.com/0cv/herdr-mobile-relay/internal/slashcmd"
	"github.com/0cv/herdr-mobile-relay/internal/support"
	"github.com/0cv/herdr-mobile-relay/internal/transport"
	relayupdate "github.com/0cv/herdr-mobile-relay/internal/update"
	"github.com/0cv/herdr-mobile-relay/internal/upload"
	"github.com/0cv/herdr-mobile-relay/internal/web"
	"github.com/0cv/herdr-mobile-relay/internal/workspace"
)

type copyResponseRunner func(
	context.Context,
	string,
	slashcmd.CopyProfile,
	copyresponse.Pane,
	copyresponse.ClipboardReader,
	copyresponse.ClipboardWriter,
	int64,
	copyresponse.RevisionReader,
) (copyresponse.Result, error)

type Server struct {
	cfg      *config.Config
	version  string
	revision string
	hostname string
	logger   *slog.Logger

	state          *coordinator.State
	hub            *transport.Hub
	poller         *coordinator.Poller
	udp            *coordinator.UDPListener
	journal        *activity.Journal
	auditLog       *audit.Logger
	pushM          *push.Manager
	transitionPush interface {
		Send(context.Context, []byte)
	}
	transitionBroadcast interface {
		Broadcast(any)
	}
	transitionEnrich func(context.Context, *coordinator.AgentState)
	sessions         *session.Resolver
	historyM         *history.Manager
	conversationM    *conversation.Reader
	profiles         *profiles.Resolver
	webH             *web.Handler
	herdrC           *herdr.Client
	clipboardRead    func(context.Context) ([]byte, error)
	clipboardWrite   func(context.Context, []byte) error
	copyRunner       copyResponseRunner
	copyMu           sync.Mutex
	paneSizeM        *panesize.Manager
	dispatcher       *coordinator.Dispatcher
	updateM          *relayupdate.Manager
	appDeployM       *appdeploy.Manager
	hybrid           *hybridTransport

	mu        sync.RWMutex
	ready     bool
	startedAt time.Time
	errors    []string

	activityMu   sync.RWMutex
	activityView []activity.Entry

	stateViewMu   sync.RWMutex
	agentView     []*coordinator.AgentState
	inventoryView map[string]any

	refreshMu      sync.Mutex
	refreshClients map[string]bool

	paneWatchMu sync.Mutex
	paneWatches map[string]*paneWatch

	historyCaptureMu  sync.Mutex
	historyInflight   map[string]bool
	historyLast       map[string]time.Time
	historyActive     map[string]bool
	historyReconciled bool
	transitionTasks   *lifecycleTasks
	historyTasks      *lifecycleTasks
}

func New(cfg *config.Config, version, revision string, logger *slog.Logger) *Server {
	state := coordinator.NewState(logger)
	hub := transport.NewHub(cfg, logger)
	herdrClient := herdr.NewClient(cfg.HerdrBin, cfg.SocketPath)
	_, clipboardRead, _ := clipboard.Reader()
	pollInterval := time.Duration(cfg.PollInterval * float64(time.Second))
	poller := coordinator.NewPoller(herdrClient, state, pollInterval, logger)

	home, _ := os.UserHomeDir()
	hostname, _ := os.Hostname()
	if hostname == "" {
		hostname = "relay"
	} else if idx := strings.Index(hostname, "."); idx > 0 {
		hostname = hostname[:idx]
	}
	profResolver := profiles.NewResolver(cfg.ConfigHome, herdrClient)
	sessResolver := session.NewResolver(home)
	histManager := history.NewManager(cfg.CacheDir)
	healthURL := fmt.Sprintf("http://127.0.0.1:%d/healthz", cfg.Port)

	return &Server{
		cfg:                 cfg,
		version:             version,
		revision:            revision,
		hostname:            hostname,
		logger:              logger,
		state:               state,
		hub:                 hub,
		transitionBroadcast: hub,
		poller:              poller,
		clipboardRead:       clipboardRead,
		clipboardWrite:      clipboard.Write,
		copyRunner:          copyresponse.Run,
		herdrC:              herdrClient,
		paneSizeM:           panesize.NewManager(herdrClient, logger),
		profiles:            profResolver,
		sessions:            sessResolver,
		historyM:            histManager,
		conversationM:       conversation.NewReader(home),
		updateM:             relayupdate.NewManager(cfg.ReleaseRoot, cfg.RuntimeDir, cfg.HerdrBin, version, revision, healthURL),
		appDeployM:          appdeploy.NewManager(cfg.RuntimeDir, cfg.WebRoot, version, revision),
		startedAt:           time.Now(),
		refreshClients:      make(map[string]bool),
		paneWatches:         make(map[string]*paneWatch),
		inventoryView:       cloneStringMap(state.InventoryStatus()),
		historyInflight:     make(map[string]bool),
		historyLast:         make(map[string]time.Time),
		historyActive:       make(map[string]bool),
	}
}

func (s *Server) resolveAgentSessionName(agent *coordinator.AgentState) {
	agent.SessionName = ""
	// Every other consumer of a pane's reported session (Reader.Read,
	// latestConversationResponse, the activity backfill path) TrimSpaces it
	// before deciding anything. Trimming once here, before it reaches
	// SessionID or the resolver, is what keeps a whitespace-only value from
	// looking like a real session id: untrimmed, it survives the `== ""`
	// checks below and can still reach the resolver's empty-id
	// sole-transcript heuristic, inventing a title over a conversation view
	// that Reader.Read reports as unavailable.
	sessionID := strings.TrimSpace(agent.Session)
	agent.SessionID = sessionID
	agent.ConversationHistoryAvailable = sessionID != "" && conversation.Supported(agent.Agent)
	if sessionID == "" {
		return
	}
	title := s.sessions.SessionName(agent.Agent, agent.Cwd, sessionID)
	if title == "" {
		return
	}
	agent.SessionName = title
	agent.Session = title
}

func (s *Server) Run(ctx context.Context) error {
	runCtx, cancelRun := context.WithCancel(ctx)
	defer cancelRun()
	ctx = runCtx
	s.transitionTasks = newLifecycleTasks(ctx)
	s.historyTasks = newLifecycleTasks(ctx)
	defer s.drainLifecycleWork()
	if err := s.state.EnableTriagePersistence(s.cfg.CacheDir); err != nil {
		s.recordSafeError("durable agent triage unavailable", err)
		s.logger.Warn("durable agent triage unavailable", "error", err)
	}

	journal, err := activity.OpenJournal(s.cfg.CacheDir)
	if err != nil {
		s.recordSafeError("activity persistence unavailable", err)
		s.logger.Warn("activity journal unavailable", "error", err)
	} else {
		s.journal = journal
		s.activityView = journal.Recent(500)
	}

	auditLog, auditErr := audit.Open(s.cfg.CacheDir)
	if auditErr != nil {
		s.recordSafeError("remote write audit unavailable", auditErr)
		s.logger.Warn("remote write audit unavailable", "error", auditErr)
	} else {
		s.auditLog = auditLog
	}

	pushDir := filepath.Join(s.cfg.RuntimeDir, "push")
	pm, err := push.NewManager(pushDir, s.logger)
	if err != nil {
		return fmt.Errorf("initialize push manager: %w", err)
	}
	s.pushM = pm
	s.transitionPush = pm

	s.state.SetOnTransition(func(paneID, agent, project, status string, revision int64) {
		transitionAt := time.Now().UnixMilli()
		s.transitionTasks.Start(func(taskCtx context.Context) {
			s.handleTransition(taskCtx, paneID, agent, project, status, revision, transitionAt)
		})
	})

	s.dispatcher = coordinator.NewDispatcher(s.herdrC, s.state, s.journal, s.logger)
	s.dispatcher.SetProfiles(s.profiles)
	s.dispatcher.SetBroadcast(s.broadcastCommitted)
	s.dispatcher.SetWakePoll(func() { s.poller.Wake() })
	// The handshake has no "profiles loading" state. Resolve integrations
	// before accepting the first WebSocket.
	_ = s.profiles.Profiles()

	s.hub.SetOnConnect(func(client *transport.ClientConn) {
		vapidPublicKey := ""
		if s.pushM != nil {
			vapidPublicKey = s.pushM.VAPIDPublicKey()
		}
		inventory := s.committedInventoryStatus()
		capabilities := append([]string(nil), protocol.Capabilities...)
		if s.clipboardRead != nil {
			capabilities = append(capabilities, protocol.AgentResponseCopyCapability)
		}
		if s.herdrC.SupportsRealtimePane(client.Context()) {
			capabilities = append(capabilities, "pane_realtime_delta", "tab_reorder")
		}
		if s.appDeployM.State().Configured {
			capabilities = append(capabilities, "app_deploy")
		}
		if s.hybrid.directEnabled() {
			capabilities = append(capabilities, "webrtc_direct")
		}
		s.hub.Send(client, protocol.PushConfig{
			Type:           "push_config",
			VAPIDPublicKey: vapidPublicKey,
			Host:           s.hostname,
			Protocol:       protocol.Version,
			Version:        s.version,
			ReleaseVersion: s.version,
			Revision:       s.revision,
			Update:         s.updateM.State(),
			AppDeploy:      s.appDeployM.State(),
			Capabilities:   capabilities,
			Inventory:      inventory,
			AgentProfiles:  s.profiles.Profiles(),
			Hybrid:         s.hybridDescriptor(),
		})
		s.hub.Send(client, map[string]any{
			"type":   "agents",
			"agents": s.committedAgents(),
		})
		activities := s.recentActivities(500)
		s.hub.Send(client, map[string]any{
			"type":       "activity_history",
			"activities": activities,
		})
		s.hub.Send(client, map[string]any{
			"type":            "inventory_status",
			"state":           inventory["state"],
			"error_code":      inventory["error_code"],
			"message":         inventory["message"],
			"last_attempt_at": inventory["last_attempt_at"],
			"last_success_at": inventory["last_success_at"],
			"stale":           inventory["stale"],
		})
	})

	s.hub.SetOnDisconnect(func(client *transport.ClientConn) {
		s.stopPaneWatch(client.ID(), "")
		if s.hybrid != nil {
			s.hybrid.forgetClient(client.ID())
		}
		releaseCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := s.paneSizeM.ReleaseClient(releaseCtx, client.ID()); err != nil {
			s.logger.Warn("client pane size leases were not fully restored", "client_id", client.ID(), "error", err)
		}
	})

	s.hub.SetHandler(func(client *transport.ClientConn, msg map[string]any, admitted func()) {
		defer admitted()
		inbound, err := protocol.DecodeMap(msg)
		if err != nil {
			admitted()
			s.hub.Send(client, protocol.DecodeFailureResponse(msg))
			return
		}
		if !protocol.Compatible(inbound) {
			admitted()
			s.hub.Send(client, protocol.IncompatibleResponse(inbound))
			return
		}
		action := inbound.Type
		coordinated := isCoordinatorMutation(action)
		auditedWrite := isAuditedWrite(action)
		if auditedWrite {
			s.recordWriteAudit(client, msg, nil)
		}
		if !coordinated {
			admitted()
		}

		commandCtx := ctx

		switch action {
		case "check_update":
			s.hub.Broadcast(map[string]any{"type": "update_status", "update": map[string]any{
				"state":            "checking",
				"current_version":  s.version,
				"current_revision": s.revision,
			}})
			updateState := s.updateM.Check(ctx)
			s.hub.Broadcast(map[string]any{"type": "update_status", "update": updateState})
			s.sendCommandResult(client, inbound.RequestID, "check_update", true, "completed", "", "", map[string]any{"update": updateState})
		case "install_update":
			deployAppFirst := s.appDeployM.Required()
			expectedAppOrigin := ""
			if deployAppFirst {
				if originErr := s.appDeployM.ValidateOrigin(inbound.ExpectedOrigin); originErr != nil {
					s.sendCommandResult(client, inbound.RequestID, "install_update", false, "failed", originErr.Error(), "", map[string]any{"update": s.updateM.State()})
					break
				}
				expectedAppOrigin = inbound.ExpectedOrigin
			}
			job, updateState, scheduleErr := s.updateM.Schedule(
				ctx,
				inbound.ExpectedVersion,
				inbound.ExpectedRevision,
				deployAppFirst,
				expectedAppOrigin,
			)
			if scheduleErr != nil {
				s.sendCommandResult(client, inbound.RequestID, "install_update", false, "failed", scheduleErr.Error(), "", map[string]any{"update": updateState})
				break
			}
			s.hub.Broadcast(map[string]any{"type": "update_status", "update": updateState})
			s.sendCommandResult(client, inbound.RequestID, "install_update", true, "scheduled", "", "", map[string]any{"job": job, "update": updateState})
		case "deploy_app_update":
			job, deployState, scheduleErr := s.appDeployM.Schedule(ctx, inbound.ExpectedVersion, inbound.ExpectedRevision, inbound.ExpectedOrigin)
			if scheduleErr != nil {
				s.sendCommandResult(client, inbound.RequestID, "deploy_app_update", false, "failed", scheduleErr.Error(), "", map[string]any{"app_deploy": deployState})
				break
			}
			s.hub.Broadcast(map[string]any{"type": "app_deploy_status", "app_deploy": deployState})
			s.sendCommandResult(client, inbound.RequestID, "deploy_app_update", true, "scheduled", "", "", map[string]any{"job": job, "app_deploy": deployState})
		case "lease_pane_size":
			columns, rows, leaseErr := s.paneSizeM.Acquire(client.Context(), client.ID(), inbound.PaneID, inbound.Columns, inbound.Rows)
			if leaseErr != nil {
				s.sendCommandResult(client, inbound.RequestID, "lease_pane_size", false, "failed", leaseErr.Error(), inbound.PaneID, nil)
				break
			}
			s.sendCommandResult(
				client,
				inbound.RequestID,
				"lease_pane_size",
				true,
				"completed",
				"",
				inbound.PaneID,
				map[string]any{"columns": columns, "rows": rows},
			)
		case "release_pane_size":
			leaseErr := s.paneSizeM.Release(client.Context(), client.ID(), inbound.PaneID)
			if leaseErr != nil {
				s.sendCommandResult(client, inbound.RequestID, "release_pane_size", false, "failed", leaseErr.Error(), inbound.PaneID, nil)
				break
			}
			s.sendCommandResult(client, inbound.RequestID, "release_pane_size", true, "completed", "", inbound.PaneID, nil)
		case "read_pane":
			s.applyPaneReadLease(msg)
			s.stopPaneWatch(client.ID(), inbound.PaneID)
			resp := s.preparePaneResponse(msg, s.dispatcher.HandleReadPane(ctx, msg))
			if unchanged := unchangedPaneResponse(msg, resp); unchanged != nil {
				s.hub.Send(client, unchanged)
				break
			}
			s.hub.Send(client, resp)
		case "watch_pane":
			s.startPaneWatch(client, msg)
		case "unwatch_pane":
			s.stopPaneWatch(client.ID(), inbound.PaneID)
		case "pane_applied":
			s.handlePaneApplied(client, msg)
		case "get_conversation_history":
			agent, exists := s.state.Agent(inbound.PaneID)
			if !exists {
				s.sendCommandResult(client, inbound.RequestID, action, false, "failed", "Agent is unavailable", inbound.PaneID, nil)
				break
			}
			generation := s.state.Generation(inbound.PaneID)
			page, historyErr := s.conversationM.Read(agent.Agent, agent.SessionID, inbound.Before, inbound.Limit)
			if historyErr != nil {
				s.logger.Warn("conversation history read failed", "pane_id", inbound.PaneID, "error", historyErr)
				s.sendCommandResult(client, inbound.RequestID, action, false, "failed", "Conversation history could not be read", inbound.PaneID, nil)
				break
			}
			if !s.state.PaneSessionCurrent(inbound.PaneID, uint64(generation)) {
				s.sendCommandResult(client, inbound.RequestID, action, false, "failed", "Agent changed while conversation history was loading", inbound.PaneID, nil)
				break
			}
			s.sendCommandResult(client, inbound.RequestID, action, true, "completed", "", inbound.PaneID, page)
		case "get_activity":
			limit := messageInt(msg["limit"], 500)
			if limit < 1 || limit > 500 {
				limit = 500
			}
			s.hub.Send(client, map[string]any{
				"type":       "activity_history",
				"activities": s.recentActivities(limit),
			})
		case "clear_activities":
			requestID, _ := msg["request_id"].(string)
			s.dispatcher.HandleClearActivities(requestID, func(result *coordinator.CommandResult) {
				s.hub.Send(client, commandResultMessage(result))
			})
		case "upload_image":
			requestID, _ := msg["request_id"].(string)
			paneID, _ := msg["pane_id"].(string)
			filename, _ := msg["filename"].(string)
			mime, _ := msg["mime"].(string)
			data, _ := msg["data"].(string)

			uploadDir := filepath.Join(s.cfg.CacheDir, "uploads")
			res := upload.Store(uploadDir, filename, mime, data)

			s.hub.Send(client, map[string]any{
				"type":       "upload_result",
				"ok":         res.OK,
				"error":      res.Error,
				"path":       res.Path,
				"pane_id":    paneID,
				"request_id": requestID,
			})

			if s.dispatcher != nil {
				status := "completed"
				summary := "Attached " + filename
				if !res.OK {
					status = "failed"
					summary = "Image upload failed: " + res.Error
				}
				s.dispatcher.RecordActivity("upload", status, summary, paneID, requestID)
			}
			if auditedWrite {
				s.recordWriteAudit(client, msg, &coordinator.CommandResult{
					RequestID: requestID,
					Action:    "upload_image",
					OK:        res.OK,
					Phase:     map[bool]string{true: "completed", false: "failed"}[res.OK],
					Error:     res.Error,
					PaneID:    paneID,
				})
			}
		case "list_directories":
			requestID, _ := msg["request_id"].(string)
			path, _ := msg["path"].(string)
			home, _ := os.UserHomeDir()
			listing := fsutil.ListDirectories(path, home)
			s.sendCommandResult(client, requestID, "list_directories", true, "completed", "", "", listing)
		case "list_slash_commands":
			requestID, _ := msg["request_id"].(string)
			paneID, _ := msg["pane_id"].(string)
			if paneID == "" {
				s.sendCommandResult(client, requestID, "list_slash_commands", false, "failed", "Agent is required", paneID, nil)
				break
			}
			if _, ok := s.state.Agent(paneID); !ok {
				s.sendCommandResult(client, requestID, "list_slash_commands", false, "failed", "Agent pane not found", paneID, nil)
				break
			}
			generation := s.state.Generation(paneID)
			agent, cwd := s.agentInfo(paneID)
			home, _ := os.UserHomeDir()
			profileID := s.profiles.ResolvePane(paneID, agent)
			skillDirs, commandFormat, _ := s.profiles.CommandConfig(profileID)
			agentVersion := s.profiles.AgentVersion(profileID)
			catalog := slashcmd.CatalogForProfile(profileID, agent, cwd, home, skillDirs, commandFormat, agentVersion)
			if s.state.Generation(paneID) != generation {
				s.sendCommandResult(
					client,
					requestID,
					"list_slash_commands",
					false,
					"failed",
					"The agent pane was replaced while commands were being listed",
					paneID,
					nil,
				)
				break
			}
			s.sendCommandResult(client, requestID, "list_slash_commands", true, "completed", "", paneID, catalog)
		case "workspace_tree", "workspace_file", "workspace_git_status", "workspace_git_diff":
			requestID := inbound.RequestID
			paneID := inbound.PaneID
			agent, exists := s.state.Agent(paneID)
			if paneID == "" || !exists {
				s.sendCommandResult(client, requestID, inbound.Type, false, "failed", "Agent pane not found", paneID, nil)
				break
			}
			generation := s.state.Generation(paneID)
			cwd := strings.TrimSpace(agent.Cwd)
			var data any
			var inspectErr error
			switch inbound.Type {
			case "workspace_tree":
				data, inspectErr = workspace.TreeFor(cwd)
			case "workspace_file":
				data, inspectErr = workspace.ReadFile(cwd, inbound.Path)
			case "workspace_git_status":
				data, inspectErr = workspace.GitStatusFor(client.Context(), cwd)
			case "workspace_git_diff":
				data, inspectErr = workspace.GitDiffFor(client.Context(), cwd, inbound.Path)
			}
			currentAgent, currentExists := s.state.Agent(paneID)
			if !currentExists || s.state.Generation(paneID) != generation || strings.TrimSpace(currentAgent.Cwd) != cwd {
				s.sendCommandResult(client, requestID, inbound.Type, false, "failed", "Agent workspace changed during inspection", paneID, nil)
				break
			}
			if inspectErr != nil {
				var public *workspace.Error
				message := "Workspace inspection failed"
				if errors.As(inspectErr, &public) {
					message = public.Message
				}
				s.sendCommandResult(client, requestID, inbound.Type, false, "failed", message, paneID, nil)
				break
			}
			s.sendCommandResult(client, requestID, inbound.Type, true, "completed", "", paneID, data)
		case "copy_agent_response":
			requestID, _ := msg["request_id"].(string)
			paneID, _ := msg["pane_id"].(string)
			s.copyAgentResponse(client, requestID, paneID)
		case "push_subscribe":
			ok := false
			if s.pushM != nil {
				var sub push.Subscription
				if raw, exists := msg["subscription"]; exists {
					data, _ := json.Marshal(raw)
					if json.Unmarshal(data, &sub) == nil {
						sub.ClientID = inbound.ClientID
						sub.NotifyFinished = inbound.NotifyFinished
						if ua, valid := msg["user_agent"].(string); valid {
							sub.UserAgent = ua
						}
						ok = s.pushM.Subscribe(sub, inbound.ReplaceEndpoints) == nil
					}
				}
			}
			s.hub.Send(client, map[string]any{"type": "push_subscribed", "ok": ok})
		case "push_unsubscribe":
			ok := false
			if s.pushM != nil {
				ok = s.pushM.Unsubscribe(inbound.Endpoints, inbound.ClientID) == nil
			}
			s.hub.Send(client, map[string]any{"type": "push_unsubscribed", "ok": ok})
		case "register_app_origin":
			if err := s.storePhoneAppOrigin(inbound.Origin); err != nil {
				s.recordSafeError("phone app origin was not stored", err)
				s.logger.Warn("phone app origin was not stored", "error", err)
			}
		case "refresh_agents":
			inventory := s.committedInventoryStatus()
			s.hub.Send(client, map[string]any{
				"type":            "inventory_status",
				"state":           inventory["state"],
				"error_code":      inventory["error_code"],
				"message":         inventory["message"],
				"last_attempt_at": inventory["last_attempt_at"],
				"last_success_at": inventory["last_success_at"],
				"stale":           inventory["stale"],
			})
			s.hub.Send(client, map[string]any{"type": "agents", "agents": s.committedAgents()})
			s.refreshMu.Lock()
			s.refreshClients[client.ID()] = true
			s.refreshMu.Unlock()
			s.poller.Wake()
		case "webrtc_offer", "webrtc_ice", "webrtc_close":
			s.handleWebRTCSignal(commandCtx, client, action, inbound.RequestID, msg)
		default:
			var result *coordinator.CommandResult
			if coordinated {
				result = s.dispatcher.HandleAdmitted(commandCtx, msg, admitted)
			} else {
				result = s.dispatcher.Handle(commandCtx, msg)
			}
			if auditedWrite {
				s.recordWriteAudit(client, msg, result)
			}
			s.hub.Send(client, commandResultMessage(result))
		}
	})

	webHandler, err := web.NewHandler(s.cfg.WebRoot)
	if err != nil {
		s.recordSafeError("web bundle unavailable", err)
		s.logger.Warn("web root unavailable, static serving disabled", "error", err)
	} else {
		s.webH = webHandler
	}

	udpAddr := net.JoinHostPort("127.0.0.1", strconv.Itoa(s.cfg.PluginPort))
	udpListener, err := coordinator.NewUDPListener(udpAddr, s.state, s.cfg.SocketPath, s.logger)
	if err != nil {
		s.recordSafeError("UDP event listener unavailable", err)
		s.logger.Warn("udp listener unavailable", "error", err)
	} else {
		s.udp = udpListener
		s.udp.SetOnDirty(func() { s.poller.Wake() })
		s.udp.SetOnChange(func(agent *coordinator.AgentState) {
			s.broadcastCommitted(map[string]any{
				"type":           "agent_update",
				"pane_id":        agent.PaneID,
				"raw_pane_id":    agent.RawPaneID,
				"status":         agent.Status,
				"agent":          agent.Agent,
				"tab_id":         agent.TabID,
				"tab_label":      agent.TabLabel,
				"tab_number":     agent.TabNumber,
				"workspace_id":   agent.WorkspaceID,
				"cwd":            agent.Cwd,
				"project":        agent.Project,
				"host":           agent.Host,
				"session":        agent.Session,
				"session_name":   agent.SessionName,
				"updated_at":     agent.UpdatedAt,
				"event_id":       agent.BlockedEventID,
				"attention_kind": agent.AttentionKind,
				"pane_revision":  agent.StateRevision,
			})
			s.poller.Wake()
		})
	}

	s.poller.SetOnChange(func(agents []*coordinator.AgentState) {
		s.broadcastCommitted(map[string]any{
			"type":   "agents",
			"agents": agents,
		})
		s.sendRequestedAgentRefreshes(agents)
		active := make(map[string]bool, len(agents))
		for _, a := range agents {
			active[a.PaneID] = true
		}
		s.syncHistoryPanes(agents)
		for _, a := range agents {
			if isClaudeLike(a.Agent) && (a.Status == "working" || a.Status == "blocked") {
				s.scheduleHistoryCapture(ctx, a.PaneID)
			}
		}
		s.dispatcher.PruneSlots(active)
	})
	s.poller.SetOnInventoryStatus(func(status map[string]any) {
		s.broadcastCommitted(inventoryStatusMessage(status))
	})

	s.poller.SetEnrich(func(ctx context.Context, agents []*coordinator.AgentState) {
		for _, a := range agents {
			s.resolveAgentSessionName(a)
			if a.Status != "blocked" {
				continue
			}
			readCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
			read, err := s.herdrC.ReadPane(readCtx, a.PaneID, 80, "ansi")
			cancel()
			if err != nil {
				s.recordSafeError("blocked pane enrichment failed", err)
				s.logger.Warn("blocked pane enrichment failed", "pane_id", a.PaneID, "error", err)
				setAgentAttention(s.state, a, question.Classification{
					Kind:   question.AttentionUnknown,
					Prompt: "Agent needs inspection",
				})
				continue
			}
			setAgentAttention(s.state, a, question.Classify(string(read.Content), a.Agent))
		}
	})

	mux := http.NewServeMux()
	mux.HandleFunc("GET /health", s.handleHealth)
	mux.HandleFunc("GET /healthz", s.handleHealthz)
	mux.HandleFunc("GET /readyz", s.handleReadyz)
	mux.HandleFunc("/ws", s.hub.HandleWebSocket)
	mux.HandleFunc("/", s.handleRoot)
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !canonicalHTTPPath(r.URL.Path) {
			http.NotFound(w, r)
			return
		}
		mux.ServeHTTP(w, r)
	})

	ln, err := net.Listen("tcp", s.cfg.Addr())
	if err != nil {
		return fmt.Errorf("listen %s: %w", s.cfg.Addr(), err)
	}

	srv := &http.Server{
		Handler:      handler,
		ReadTimeout:  10 * time.Second,
		WriteTimeout: 30 * time.Second,
		IdleTimeout:  120 * time.Second,
	}

	s.mu.Lock()
	s.ready = true
	s.mu.Unlock()

	s.logger.Info("relay listening",
		"addr", s.cfg.Addr(),
		"version", s.version,
		"revision", s.revision,
		"instance", s.cfg.InstanceID,
		"web_root", s.cfg.WebRoot,
	)

	var bg sync.WaitGroup
	startBackground := func(work func()) {
		bg.Add(1)
		go func() {
			defer bg.Done()
			work()
		}()
	}
	startBackground(func() { s.poller.Run(ctx) })
	eventClient := herdr.NewEventClient(s.cfg.SocketPath)
	startBackground(func() { s.poller.RunEvents(ctx, eventClient) })
	startBackground(func() { s.captureHistoryLoop(ctx) })
	startBackground(func() { s.paneSizeM.Run(ctx) })
	profileSignals := make(chan os.Signal, 1)
	signal.Notify(profileSignals, syscall.SIGHUP)
	defer signal.Stop(profileSignals)
	startBackground(func() { s.reloadProfilesLoop(ctx, profileSignals) })
	if s.udp != nil {
		startBackground(func() { s.udp.Run(ctx) })
	}
	startBackground(func() { s.pruneUploads(ctx) })
	startBackground(func() { s.writeSupportLoop(ctx) })
	startBackground(func() { s.watchJobStates(ctx) })
	startBackground(func() { s.updateCheckLoop(ctx) })
	s.hybrid = s.startHybridTransport(ctx)
	if s.hybrid != nil {
		s.hybrid.run(ctx, startBackground)
	}

	errCh := make(chan error, 1)
	go func() {
		errCh <- srv.Serve(ln)
	}()

	var runErr error
	select {
	case <-ctx.Done():
		s.logger.Info("shutting down")
	case err := <-errCh:
		if err != http.ErrServerClosed {
			runErr = err
		}
	}

	cancelRun()
	if s.dispatcher != nil {
		s.dispatcher.CancelInflight()
	}
	paneSizeShutdownCtx, cancelPaneSizes := context.WithTimeout(context.Background(), 5*time.Second)
	if err := s.paneSizeM.Shutdown(paneSizeShutdownCtx); runErr == nil && err != nil {
		runErr = fmt.Errorf("pane size lease shutdown: %w", err)
	}
	cancelPaneSizes()
	httpShutdownCtx, cancelHTTP := context.WithTimeout(context.Background(), 5*time.Second)
	if err := srv.Shutdown(httpShutdownCtx); runErr == nil && err != nil {
		runErr = fmt.Errorf("http shutdown: %w", err)
	}
	cancelHTTP()
	hubShutdownCtx, cancelHub := context.WithTimeout(context.Background(), 5*time.Second)
	if err := s.hub.Shutdown(hubShutdownCtx); runErr == nil && err != nil {
		runErr = fmt.Errorf("websocket shutdown: %w", err)
	}
	cancelHub()
	if s.hybrid != nil {
		s.hybrid.close()
	}
	if s.udp != nil {
		_ = s.udp.Close()
	}
	bg.Wait()
	s.drainLifecycleWork()
	if s.dispatcher != nil {
		dispatcherCloseCtx, cancelDispatcher := context.WithTimeout(context.Background(), 5*time.Second)
		if err := s.dispatcher.Close(dispatcherCloseCtx); runErr == nil && err != nil {
			runErr = err
		}
		cancelDispatcher()
	}
	if err := s.herdrC.Close(); runErr == nil && err != nil {
		runErr = fmt.Errorf("Herdr socket client shutdown: %w", err)
	}
	if s.webH != nil {
		_ = s.webH.Close()
	}
	return runErr
}

func canonicalHTTPPath(raw string) bool {
	if raw == "" || raw == "/" {
		return true
	}
	if !strings.HasPrefix(raw, "/") || strings.Contains(raw, "\\") || strings.Contains(raw, "\x00") {
		return false
	}
	for _, segment := range strings.Split(strings.TrimPrefix(raw, "/"), "/") {
		if segment == "" || segment == "." || segment == ".." {
			return false
		}
	}
	return true
}

func (s *Server) drainLifecycleWork() {
	s.state.SetOnTransition(nil)
	s.transitionTasks.Stop()
	s.historyTasks.Stop()
	s.historyM.SaveAll()
}

func (s *Server) handleTransition(
	parent context.Context,
	paneID, agent, project, status string,
	revision int64,
	observedAt ...int64,
) {
	transitionAt := time.Now().UnixMilli()
	if len(observedAt) > 0 && observedAt[0] > 0 {
		transitionAt = observedAt[0]
	}
	agentState, agentExists := s.state.Agent(paneID)
	var session string
	var sessionID string
	var blockedEventID string
	var paneGeneration uint64
	var blockedContentRevision int64
	if agentExists {
		if status != "working" || s.state.TransitionCurrent(paneID, status, revision) {
			session = agentState.Session
		}
		sessionID = agentState.SessionID
		blockedEventID = agentState.BlockedEventID
		blockedContentRevision = s.state.ContentRevision(paneID)
	}
	if status == "blocked" {
		generation, active := s.state.PaneSession(paneID)
		if !active || blockedEventID == "" {
			return
		}
		paneGeneration = uint64(generation)
	}
	transitionCurrent := func() bool {
		if status == "blocked" {
			return s.state.BlockedTransitionCurrent(paneID, blockedEventID, paneGeneration)
		}
		if status == "working" {
			return true
		}
		return s.state.CompletionCurrent(paneID, revision)
	}
	if !transitionCurrent() {
		return
	}
	ctx, cancel := context.WithTimeout(parent, 10*time.Second)
	defer cancel()

	if status == "working" {
		summary := agent + " started working"
		if agent == "" {
			summary = "Agent started working"
		}
		if s.dispatcher != nil {
			s.dispatcher.RecordTransitionActivity(
				"working", "working", summary, paneID, status, revision,
				map[string]any{"transition": "working", "transition_at": transitionAt},
				agent, project, s.hostname, session, "",
			)
		}
		return
	}

	if status == "blocked" {
		if !agentExists {
			return
		}
		if s.transitionEnrich != nil {
			s.transitionEnrich(ctx, agentState)
		} else {
			s.enrichBlockedTransition(ctx, agentState)
		}
		if !transitionCurrent() {
			return
		}
		persisted, ok := s.state.CommitAttentionClassification(
			paneID,
			blockedEventID,
			paneGeneration,
			blockedContentRevision,
			classificationFromAgent(agentState),
		)
		if !ok || !transitionCurrent() {
			return
		}
		agentState = persisted
		classifiedAttentionRevision := s.state.AttentionRevision(paneID)
		classifiedCurrent := func() bool {
			return s.state.AttentionTransitionCurrent(
				paneID,
				blockedEventID,
				paneGeneration,
				string(agentState.AttentionKind),
				classifiedAttentionRevision,
			)
		}
		if !classifiedCurrent() {
			return
		}
		if agentState.AttentionKind == question.AttentionChat {
			s.broadcastBlockedAttention(agentState)
			s.handleChatCompletion(ctx, agentState)
			return
		}

		eventID := agentState.BlockedEventID
		command := agentState.Command
		activityKind := "blocked"
		if command == "" {
			switch agentState.AttentionKind {
			case question.AttentionQuestion:
				command = "Agent needs an answer"
			case question.AttentionUnknown:
				command = "Agent needs inspection"
			default:
				command = "Agent needs approval"
			}
		}
		if agentState.AttentionKind == question.AttentionQuestion {
			activityKind = "question"
		}
		if s.dispatcher != nil && !s.dispatcher.RecordTransitionActivity(
			activityKind, "attention", command, paneID, status, agentState.StateRevision,
			map[string]any{
				"event_id":       eventID,
				"attention_kind": agentState.AttentionKind,
				"transition_at":  transitionAt,
			},
			agent, project, s.hostname, session, agentState.Prompt,
			blockedEventID, paneGeneration,
			string(agentState.AttentionKind), classifiedAttentionRevision,
		) {
			return
		}
		if !classifiedCurrent() {
			return
		}
		if s.transitionPush != nil {
			payload := push.BuildAttentionPayload(
				agent,
				project,
				command,
				eventID,
				paneID,
				s.hostname,
				string(agentState.AttentionKind),
				len(agentState.Options),
			)
			s.sendTransitionPush(ctx, payload, classifiedCurrent)
		}
		if !classifiedCurrent() {
			return
		}
		s.broadcastBlockedAttention(agentState)
		return
	}
	if !s.state.RegisterFinishedNotificationForTransition(paneID, status, revision) {
		return
	}
	eventID := fmt.Sprintf("finished-%d-%s", time.Now().UnixNano(), paneID)
	extract := s.captureFinishedPane(ctx, paneID, agent, sessionID)
	if !transitionCurrent() {
		return
	}
	summary := agent + " completed"
	if agent == "" {
		summary = "Agent completed"
	}
	if s.dispatcher != nil && !s.dispatcher.RecordTransitionActivity(
		"finished", "completed", summary, paneID, status, revision,
		map[string]any{"event_id": eventID, "transition_at": transitionAt},
		agent, project, s.hostname, session, extract,
	) {
		return
	}
	if !transitionCurrent() {
		return
	}
	if s.transitionPush != nil {
		payload := push.BuildFinishedPayload(agent, project, paneID, s.hostname, eventID)
		s.sendTransitionPush(ctx, payload, transitionCurrent)
	}
}

func (s *Server) sendTransitionPush(
	ctx context.Context,
	payload []byte,
	transitionCurrent func() bool,
) {
	pushCtx, cancel := context.WithCancel(ctx)
	finished := make(chan struct{})
	watcherDone := make(chan struct{})
	go func() {
		defer close(watcherDone)
		ticker := time.NewTicker(10 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-finished:
				return
			case <-pushCtx.Done():
				return
			case <-ticker.C:
				if !transitionCurrent() {
					cancel()
					return
				}
			}
		}
	}()
	s.transitionPush.Send(pushCtx, payload)
	close(finished)
	cancel()
	<-watcherDone
}

const (
	blockedClassificationAttempts   = 4
	blockedClassificationRetryDelay = 100 * time.Millisecond
)

func (s *Server) enrichBlockedTransition(ctx context.Context, agent *coordinator.AgentState) {
	if agent == nil {
		return
	}
	classification, err := classifyBlockedTransition(ctx, agent.Agent, func(readCtx context.Context) (string, error) {
		attemptCtx, cancel := context.WithTimeout(readCtx, 3*time.Second)
		defer cancel()
		read, readErr := s.herdrC.ReadPane(attemptCtx, agent.PaneID, 80, "ansi")
		return string(read.Content), readErr
	})
	if err != nil {
		setAgentAttention(s.state, agent, question.Classification{
			Kind:   question.AttentionUnknown,
			Prompt: "Agent needs inspection",
		})
		return
	}
	setAgentAttention(s.state, agent, classification)
}

func classifyBlockedTransition(
	ctx context.Context,
	agent string,
	read func(context.Context) (string, error),
) (question.Classification, error) {
	classification := question.Classification{
		Kind:   question.AttentionUnknown,
		Prompt: "Agent needs inspection",
	}
	for attempt := range blockedClassificationAttempts {
		content, err := read(ctx)
		if err != nil {
			return classification, err
		}
		classification = question.Classify(content, agent)
		if classification.Kind != question.AttentionUnknown ||
			attempt+1 == blockedClassificationAttempts {
			return classification, nil
		}
		timer := time.NewTimer(blockedClassificationRetryDelay)
		select {
		case <-ctx.Done():
			if !timer.Stop() {
				select {
				case <-timer.C:
				default:
				}
			}
			return classification, ctx.Err()
		case <-timer.C:
		}
	}
	return classification, nil
}

func setAgentAttention(
	state *coordinator.State,
	agent *coordinator.AgentState,
	classification question.Classification,
) {
	if classification.Kind == "" {
		classification.Kind = question.AttentionUnknown
	}
	agent.AttentionKind = classification.Kind
	agent.Prompt = classification.Prompt
	agent.Command = classification.Command
	agent.Options = nil
	agent.Interaction = nil
	agent.QuestionLayout = false
	agent.InteractionID = ""
	switch classification.Kind {
	case question.AttentionApproval:
		agent.Options = append([]string(nil), classification.Options...)
	case question.AttentionQuestion:
		agent.Interaction = classification.Interaction
		agent.QuestionLayout = classification.QuestionLayout
		if agent.Interaction != nil {
			agent.InteractionID = agent.Interaction.ID
			if state != nil {
				if text := strings.TrimSpace(agent.Interaction.Other.Text); text != "" {
					state.RecordCustomAnswer(agent.PaneID, agent.Interaction.Question, text)
				}
				question.FillCustomAnswers(agent.Interaction, state.CustomAnswers(agent.PaneID))
			}
		}
	}
}

func classificationFromAgent(agent *coordinator.AgentState) question.Classification {
	if agent == nil {
		return question.Classification{Kind: question.AttentionUnknown}
	}
	return question.Classification{
		Kind:           agent.AttentionKind,
		Prompt:         agent.Prompt,
		Command:        agent.Command,
		Options:        append([]string(nil), agent.Options...),
		Interaction:    agent.Interaction,
		QuestionLayout: agent.QuestionLayout,
	}
}

func (s *Server) broadcastBlockedAttention(agent *coordinator.AgentState) {
	if agent == nil || s.transitionBroadcast == nil {
		return
	}
	message := map[string]any{
		"type":            "blocked",
		"pane_id":         agent.PaneID,
		"raw_pane_id":     agent.RawPaneID,
		"terminal_id":     agent.TerminalID,
		"tab_id":          agent.TabID,
		"tab_label":       agent.TabLabel,
		"tab_number":      agent.TabNumber,
		"workspace_id":    agent.WorkspaceID,
		"agent":           agent.Agent,
		"name":            agent.Name,
		"status":          "blocked",
		"cwd":             agent.Cwd,
		"project":         agent.Project,
		"host":            agent.Host,
		"session":         agent.Session,
		"session_name":    agent.SessionName,
		"updated_at":      agent.UpdatedAt,
		"event_id":        agent.BlockedEventID,
		"attention_kind":  agent.AttentionKind,
		"prompt":          agent.Prompt,
		"command":         agent.Command,
		"options":         agent.Options,
		"interaction":     agent.Interaction,
		"interaction_id":  agent.InteractionID,
		"question_layout": agent.QuestionLayout,
		"pane_revision":   agent.StateRevision,
	}
	if s.transitionBroadcast == s.hub {
		s.broadcastCommitted(message)
		return
	}
	s.transitionBroadcast.Broadcast(message)
}

func (s *Server) handleChatCompletion(
	ctx context.Context,
	agent *coordinator.AgentState,
) {
	if agent == nil {
		return
	}
	revision := agent.StateRevision
	current := func() bool {
		return s.state.CompletionCurrent(agent.PaneID, revision)
	}
	if !current() ||
		!s.state.RegisterFinishedNotificationForTransition(agent.PaneID, "blocked", revision) {
		return
	}
	eventID := fmt.Sprintf("finished-%d-%s", time.Now().UnixNano(), agent.PaneID)
	summary := agent.Agent + " completed"
	if agent.Agent == "" {
		summary = "Agent completed"
	}
	if s.dispatcher != nil && !s.dispatcher.RecordTransitionActivity(
		"finished",
		"completed",
		summary,
		agent.PaneID,
		"blocked",
		revision,
		map[string]any{
			"event_id":       eventID,
			"attention_kind": agent.AttentionKind,
		},
		agent.Agent,
		agent.Project,
		s.hostname,
		agent.Session,
		agent.Prompt,
	) {
		return
	}
	if !current() {
		return
	}
	if s.transitionPush != nil {
		payload := push.BuildFinishedPayload(
			agent.Agent,
			agent.Project,
			agent.PaneID,
			s.hostname,
			eventID,
		)
		s.sendTransitionPush(ctx, payload, current)
	}
}

func (s *Server) captureHistoryLoop(ctx context.Context) {
	ticker := time.NewTicker(history.CaptureInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			for _, agent := range s.state.Snapshot() {
				if !isClaudeLike(agent.Agent) || (agent.Status != "working" && agent.Status != "blocked") {
					continue
				}
				s.scheduleHistoryCapture(ctx, agent.PaneID)
			}
		}
	}
}

func (s *Server) scheduleHistoryCapture(ctx context.Context, paneID string) {
	if s.historyTasks == nil {
		return
	}
	s.historyCaptureMu.Lock()
	if s.historyInflight[paneID] || time.Since(s.historyLast[paneID]) < history.CaptureInterval {
		s.historyCaptureMu.Unlock()
		return
	}
	s.historyInflight[paneID] = true
	s.historyLast[paneID] = time.Now()
	s.historyCaptureMu.Unlock()

	started := s.historyTasks.Start(func(taskCtx context.Context) {
		defer func() {
			s.historyCaptureMu.Lock()
			delete(s.historyInflight, paneID)
			s.historyCaptureMu.Unlock()
		}()
		readCtx, cancel := context.WithTimeout(taskCtx, 3*time.Second)
		defer cancel()
		read, err := s.herdrC.ReadPane(readCtx, paneID, history.MaxLines, "ansi")
		content := read.Content
		if err != nil || len(content) == 0 || question.LayoutHint(string(content)) {
			return
		}
		agent, ok := s.state.Agent(paneID)
		if !ok || !isClaudeLike(agent.Agent) {
			return
		}
		s.historyCaptureMu.Lock()
		defer s.historyCaptureMu.Unlock()
		if !s.historyActive[paneID] {
			return
		}
		s.historyM.Merge(paneID, string(content))
	})
	if started {
		return
	}
	s.historyCaptureMu.Lock()
	delete(s.historyInflight, paneID)
	s.historyCaptureMu.Unlock()
}

func (s *Server) syncHistoryPanes(agents []*coordinator.AgentState) {
	active := make(map[string]bool, len(agents))
	for _, agent := range agents {
		if isClaudeLike(agent.Agent) {
			active[agent.PaneID] = true
		}
	}

	s.historyCaptureMu.Lock()
	defer s.historyCaptureMu.Unlock()
	if !s.historyReconciled {
		s.historyM.Reconcile(active)
		s.historyReconciled = true
	}
	for paneID := range s.historyActive {
		if active[paneID] {
			continue
		}
		delete(s.historyActive, paneID)
		delete(s.historyLast, paneID)
		s.historyM.Discard(paneID)
	}
	for paneID := range active {
		s.historyActive[paneID] = true
	}
}

// Conversation logs preserve the full assistant message; the terminal pane is
// only a bounded fallback for agents without a readable transcript.
func (s *Server) latestConversationResponse(agent, sessionID string) string {
	if s.conversationM == nil || strings.TrimSpace(sessionID) == "" || !conversation.Supported(agent) {
		return ""
	}
	page, err := s.conversationM.Read(agent, sessionID, "", 1)
	if err != nil || !page.Available || len(page.Entries) == 0 {
		return ""
	}
	entry := page.Entries[len(page.Entries)-1]
	if entry.Role != "assistant" || strings.TrimSpace(entry.Text) == "" {
		return ""
	}
	return entry.Text
}

func (s *Server) captureFinishedPane(ctx context.Context, paneID, agent, sessionID string) string {
	if response := s.latestConversationResponse(agent, sessionID); response != "" {
		return response
	}
	readCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	read, err := s.herdrC.ReadPane(readCtx, paneID, history.MaxLines, "ansi")
	content := read.Content
	if err != nil || len(content) == 0 {
		return ""
	}
	raw := string(content)
	completionContent := raw
	if isClaudeLike(agent) && !question.LayoutHint(raw) {
		completionContent = s.historyM.Merge(paneID, raw)
	}
	if response := question.LatestCompletedResponse(completionContent); response != "" {
		return response
	}
	return question.PaneSummary(completionContent)
}

func isClaudeLike(agent string) bool {
	lower := strings.ToLower(agent)
	return strings.Contains(lower, "claude") || strings.Contains(lower, "qoder")
}

func (s *Server) handleRoot(w http.ResponseWriter, r *http.Request) {
	if strings.EqualFold(r.Header.Get("Upgrade"), "websocket") {
		s.hub.HandleWebSocket(w, r)
		return
	}
	if s.webH != nil {
		s.webH.ServeHTTP(w, r)
		return
	}
	http.NotFound(w, r)
}

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("X-Herdr-Relay-Instance", s.cfg.InstanceID)
	fmt.Fprint(w, "ok\n")
}

func (s *Server) handleHealthz(w http.ResponseWriter, r *http.Request) {
	s.mu.RLock()
	ready := s.ready
	s.mu.RUnlock()

	inventory := s.state.InventoryStatus()
	delete(inventory, "message")

	readiness := "starting"
	if ready {
		switch inventory["state"] {
		case "ready":
			readiness = "ready"
		case "error":
			readiness = "degraded"
		}
	}

	resp := map[string]any{
		"status":          "ok",
		"readiness":       readiness,
		"inventory":       inventory,
		"instance":        s.cfg.InstanceID,
		"version":         s.version,
		"release_version": s.version,
		"revision":        s.revision,
		"protocol":        protocol.Version,
	}
	gateway := s.hybrid.status()
	resp["gateway"] = gateway
	resp["gateway_url"] = gateway["url"]
	resp["gateway_version"] = gateway["version"]
	resp["gateway_revision"] = gateway["revision"]
	resp["gateway_available_version"] = s.gatewayAvailableVersion()

	if s.webH != nil {
		resp["bundle_hash"] = s.webH.BundleHash()
		resp["bundle_version"] = s.webH.BundleVersion()
		resp["bundle_revision"] = s.webH.BundleRevision()
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(resp)
}

func (s *Server) handleReadyz(w http.ResponseWriter, r *http.Request) {
	s.mu.RLock()
	ready := s.ready
	s.mu.RUnlock()

	inventoryOK := s.state.InventoryReady()

	status := "unavailable"
	code := http.StatusServiceUnavailable
	if ready && inventoryOK {
		status = "ready"
		code = http.StatusOK
	}

	inventory := s.state.InventoryStatus()
	delete(inventory, "message")

	resp := map[string]any{
		"status":    status,
		"inventory": inventory,
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(resp)
}

func (s *Server) pruneUploads(ctx context.Context) {
	uploadDir := filepath.Join(s.cfg.CacheDir, "uploads")
	upload.Prune(uploadDir)

	ticker := time.NewTicker(24 * time.Hour)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if n := upload.Prune(uploadDir); n > 0 {
				s.logger.Info("pruned expired uploads", "count", n)
			}
		}
	}
}

func (s *Server) copyAgentResponse(client *transport.ClientConn, requestID, paneID string) {
	if paneID == "" {
		s.sendCommandResult(client, requestID, "copy_agent_response", false, "failed", "Agent is required", paneID, nil)
		return
	}
	agent, ok := s.state.Agent(paneID)
	if !ok {
		s.sendCommandResult(client, requestID, "copy_agent_response", false, "failed", "Agent pane not found", paneID, nil)
		return
	}
	if agent.Status == "working" {
		s.sendCommandResult(client, requestID, "copy_agent_response", false, "failed", "Agent is still working; wait for the current turn to finish", paneID, nil)
		return
	}
	if message := copyBlockedMessage(agent); message != "" {
		s.sendCommandResult(client, requestID, "copy_agent_response", false, "failed", message, paneID, nil)
		return
	}
	if s.clipboardRead == nil || s.clipboardWrite == nil {
		s.sendCommandResult(client, requestID, "copy_agent_response", false, "failed", "Host clipboard is unavailable", paneID, nil)
		return
	}
	generation := s.state.Generation(paneID)
	agentName, _ := s.agentInfo(paneID)
	profileID := s.profiles.ResolvePane(paneID, agentName)
	profile, ok := slashcmd.CopyProfileFor(profileID, agentName)
	if !ok {
		s.sendCommandResult(client, requestID, "copy_agent_response", false, "failed", "Agent does not support response copying", paneID, nil)
		return
	}
	s.copyMu.Lock()
	defer s.copyMu.Unlock()
	ctx, cancel := context.WithTimeout(client.Context(), 10*time.Second)
	defer cancel()
	runCopy := s.copyRunner
	if runCopy == nil {
		runCopy = copyresponse.Run
	}
	result, err := runCopy(
		ctx,
		paneID,
		profile,
		s.herdrC,
		s.clipboardRead,
		s.clipboardWrite,
		int64(agent.PaneRevision),
		s.currentPaneRevision,
	)
	if err != nil {
		s.logger.Warn("agent response copy failed", "pane_id", paneID, "error", err)
		s.sendCommandResult(client, requestID, "copy_agent_response", false, "failed", copyResponseError(err), paneID, nil)
		return
	}
	if s.state.Generation(paneID) != generation {
		s.sendCommandResult(client, requestID, "copy_agent_response", false, "failed", "The agent pane was replaced while the response was being copied", paneID, nil)
		return
	}
	s.sendCommandResult(client, requestID, "copy_agent_response", true, "completed", "", paneID, map[string]any{
		"text":   result.Text,
		"source": result.Source,
		"chars":  result.Chars,
		"lines":  result.Lines,
	})
}

func copyBlockedMessage(agent *coordinator.AgentState) string {
	if agent == nil || agent.Status != "blocked" {
		return ""
	}
	switch agent.AttentionKind {
	case question.AttentionQuestion:
		return "Agent is waiting for an answer"
	case question.AttentionApproval:
		return "Agent is waiting for approval"
	default:
		return ""
	}
}

func copyResponseError(err error) string {
	switch {
	case errors.Is(err, copyresponse.ErrComposerBusy):
		return "The agent composer is busy; finish or clear the current prompt first"
	case errors.Is(err, copyresponse.ErrPickerOpen):
		return "The agent already has a copy menu open; close it and try again"
	case errors.Is(err, copyresponse.ErrStaleOutput):
		return "The copied response changed before it could be read; try again"
	case errors.Is(err, copyresponse.ErrNoCopy):
		return "The agent did not confirm a copied response; try again"
	case errors.Is(err, context.DeadlineExceeded):
		return "Copying the agent response timed out; try again"
	default:
		return "Could not copy the agent response; try again"
	}
}

func (s *Server) currentPaneRevision(ctx context.Context, paneID string) (int64, error) {
	inventory, err := s.herdrC.GetInventory(ctx)
	if err != nil {
		return 0, err
	}
	for _, pane := range inventory.Panes {
		if pane.ID == paneID {
			return int64(pane.Revision), nil
		}
	}
	return 0, fmt.Errorf("pane %q was not found", paneID)
}

func (s *Server) agentInfo(paneID string) (agent, cwd string) {
	for _, a := range s.state.Snapshot() {
		if a.PaneID == paneID {
			return a.Agent, a.Cwd
		}
	}
	return "", ""
}

func (s *Server) sendRequestedAgentRefreshes(agents []*coordinator.AgentState) {
	s.refreshMu.Lock()
	clientIDs := make([]string, 0, len(s.refreshClients))
	for clientID := range s.refreshClients {
		clientIDs = append(clientIDs, clientID)
	}
	clear(s.refreshClients)
	s.refreshMu.Unlock()

	if len(clientIDs) == 0 {
		return
	}
	agents = s.committedAgents()
	status := inventoryStatusMessage(s.committedInventoryStatus())
	snapshot := map[string]any{"type": "agents", "agents": agents}
	for _, clientID := range clientIDs {
		s.hub.SendByID(clientID, status)
		s.hub.SendByID(clientID, snapshot)
	}
}

func inventoryStatusMessage(status map[string]any) map[string]any {
	return map[string]any{
		"type":            "inventory_status",
		"state":           status["state"],
		"error_code":      status["error_code"],
		"message":         status["message"],
		"last_attempt_at": status["last_attempt_at"],
		"last_success_at": status["last_success_at"],
		"stale":           status["stale"],
	}
}

func (s *Server) reloadProfilesLoop(ctx context.Context, signals <-chan os.Signal) {
	for {
		select {
		case <-ctx.Done():
			return
		case <-signals:
			s.profiles.Reload()
			profiles := s.profiles.Profiles()
			s.logger.Info("agent profiles reloaded", "profiles", len(profiles))
		}
	}
}

func (s *Server) publicInventoryStatus() map[string]any {
	return s.state.InventoryStatus()
}

func (s *Server) publicJobState(filename, defaultState string) map[string]any {
	result := map[string]any{
		"state":            defaultState,
		"current_version":  s.version,
		"current_revision": s.revision,
	}
	data, err := os.ReadFile(filepath.Join(s.cfg.RuntimeDir, filename))
	if err != nil {
		return result
	}
	var raw map[string]any
	if json.Unmarshal(data, &raw) != nil {
		return result
	}
	allowed := []string{
		"state", "current_version", "current_revision", "available_version",
		"available_revision", "target_version", "target_revision", "checked_at",
		"mode", "eligible", "reason", "error", "started_at", "finished_at",
	}
	for _, key := range allowed {
		if value, ok := raw[key]; ok {
			result[key] = value
		}
	}
	return result
}

// preparePaneResponse annotates a pane frame with everything the phone needs
// to decide what it may offer the operator. The no-echo flag is computed last,
// on the content the phone will actually render, so full reads, deltas and the
// history-merged frames all agree on the tail.
func (s *Server) preparePaneResponse(message, response map[string]any) map[string]any {
	response = s.classifyPaneResponse(message, response)
	content, ok := successfulPaneContent(response)
	if !ok {
		return response
	}
	prompt, secret := noecho.Match(content)
	response["no_echo"] = secret
	if secret {
		response["no_echo_prompt"] = prompt
	}
	return response
}

func (s *Server) classifyPaneResponse(message, response map[string]any) map[string]any {
	content, ok := successfulPaneContent(response)
	if !ok {
		return response
	}
	paneID, _ := message["pane_id"].(string)
	agent, _ := s.agentInfo(paneID)
	classification := question.Classify(content, agent)
	response["attention_kind"] = classification.Kind
	response["prompt"] = classification.Prompt
	response["command"] = classification.Command
	response["options"] = classification.Options
	response["interaction"] = classification.Interaction
	response["question_layout"] = classification.QuestionLayout
	if interaction := classification.Interaction; interaction != nil {
		if text := strings.TrimSpace(interaction.Other.Text); text != "" {
			s.state.RecordCustomAnswer(paneID, interaction.Question, text)
		}
		question.FillCustomAnswers(interaction, s.state.CustomAnswers(paneID))
	}
	if viewportOnly, _ := response["viewport_only"].(bool); viewportOnly &&
		s.paneSizeM != nil && s.paneSizeM.ResizedWithin(paneID, paneResizeSettleWindow) {
		// The agent re-renders its transcript after a width change and can push
		// a redrawn block into the scrollback for a while; the app must not
		// commit rows from frames read inside this window as history.
		response["resize_settling"] = true
	}
	agentLower := strings.ToLower(agent)
	if classification.Interaction != nil ||
		(!strings.Contains(agentLower, "claude") && !strings.Contains(agentLower, "qoder")) {
		return response
	}
	if viewportOnly, _ := response["viewport_only"].(bool); viewportOnly {
		return response
	}
	historyLimit := messageInt(message["lines"], 30)
	if historyLimit < 1 {
		historyLimit = 1
	} else if historyLimit > history.MaxLines {
		historyLimit = history.MaxLines
	}
	herdrTruncated, _ := response["truncated"].(bool)
	merged, historyTruncated := s.historyM.MergeLimited(paneID, content, historyLimit)
	response["content"] = merged
	response["truncated"] = herdrTruncated || historyTruncated
	return response
}

func successfulPaneContent(response map[string]any) (string, bool) {
	if _, failed := response["error"]; failed {
		return "", false
	}
	content, ok := response["content"].(string)
	return content, ok
}

func paneFingerprint(content string) string {
	sum := sha256.Sum256([]byte(content))
	return fmt.Sprintf("%x", sum[:8])
}

func paneFrameFingerprint(response map[string]any) string {
	state := []any{
		response["content"],
		response["format"],
		response["truncated"],
		response["viewport_only"],
		response["viewport_rows"],
		response["resize_settling"],
		response["attention_kind"],
		response["prompt"],
		response["command"],
		response["options"],
		response["interaction"],
		response["question_layout"],
	}
	encoded, err := json.Marshal(state)
	if err != nil {
		return paneFingerprint(fmt.Sprint(state...))
	}
	return paneFingerprint(string(encoded))
}

func unchangedPaneResponse(message, response map[string]any) map[string]any {
	content, ok := successfulPaneContent(response)
	if !ok {
		return nil
	}
	fingerprint := paneFingerprint(content)
	response["content_fingerprint"] = fingerprint
	known, _ := message["content_fingerprint"].(string)
	if known == "" || known != fingerprint {
		return nil
	}
	paneID, _ := response["pane_id"].(string)
	return map[string]any{
		"type":                "pane_unchanged",
		"pane_id":             paneID,
		"content_fingerprint": fingerprint,
	}
}

func (s *Server) sendCommandResult(
	client *transport.ClientConn,
	requestID, action string,
	ok bool,
	phase, publicError string,
	paneID string,
	data any,
) {
	result := &coordinator.CommandResult{
		RequestID: requestID,
		Action:    action,
		OK:        ok,
		Phase:     phase,
		Error:     publicError,
		PaneID:    paneID,
		Data:      data,
	}
	s.hub.Send(client, commandResultMessage(result))
}

func commandResultMessage(result *coordinator.CommandResult) map[string]any {
	message := map[string]any{
		"type":       "command_result",
		"request_id": result.RequestID,
		"action":     result.Action,
		"ok":         result.OK,
		"phase":      result.Phase,
		"error":      result.Error,
		"pane_id":    result.PaneID,
	}
	if result.Data != nil {
		message["data"] = result.Data
	}
	return message
}

func (s *Server) watchJobStates(ctx context.Context) {
	updateState := serializedState(s.updateM.State())
	deployState := serializedState(s.appDeployM.State())
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			nextUpdate := s.updateM.State()
			if serialized := serializedState(nextUpdate); serialized != updateState {
				updateState = serialized
				s.hub.Broadcast(map[string]any{"type": "update_status", "update": nextUpdate})
			}
			nextDeploy := s.appDeployM.State()
			if serialized := serializedState(nextDeploy); serialized != deployState {
				deployState = serialized
				s.hub.Broadcast(map[string]any{"type": "app_deploy_status", "app_deploy": nextDeploy})
			}
		}
	}
}

func (s *Server) updateCheckLoop(ctx context.Context) {
	timer := time.NewTimer(5 * time.Minute)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
			state := s.updateM.Check(ctx)
			s.hub.Broadcast(map[string]any{"type": "update_status", "update": state})
			timer.Reset(6 * time.Hour)
		}
	}
}

func serializedState(value any) string {
	data, _ := json.Marshal(value)
	return string(data)
}

func isAuditedWrite(action string) bool {
	switch action {
	case "submit_prompt", "prompt", "send_keys", "keys", "send_text", "text",
		"respond", "answer_question", "navigate_question", "clarify_question",
		"agent_stop", "agent_rename", "tab_reorder", "agent_start", "agent_clear",
		"agent_restart", "upload_image", "send_secret":
		return true
	default:
		return false
	}
}

func (s *Server) recordWriteAudit(
	client *transport.ClientConn,
	message map[string]any,
	result *coordinator.CommandResult,
) {
	if s.auditLog == nil {
		return
	}
	action := auditAction(message)
	requestID, _ := message["request_id"].(string)
	paneID, _ := message["pane_id"].(string)
	clientID, _ := message["client_id"].(string)
	if clientID == "" {
		clientID = "connection:" + client.ID()
	}
	record := audit.Record{
		Stage:        "attempt",
		Action:       action,
		RequestID:    requestID,
		ClientID:     clientID,
		ConnectionID: client.ID(),
		PaneID:       paneID,
		Details:      auditWriteDetails(message),
	}
	if agent, ok := s.state.Agent(paneID); ok {
		record.Agent = agent.Agent
		record.Project = agent.Project
		record.Session = agent.Session
		record.Host = agent.Host
	}
	if result != nil {
		ok := result.OK
		record.Stage = "result"
		record.OK = &ok
		record.Phase = result.Phase
		record.Error = result.Error
		record.Details = nil
		if result.PaneID != "" {
			record.PaneID = result.PaneID
		}
	}
	if err := s.auditLog.Append(record); err != nil {
		s.logger.Warn(
			"remote write audit append failed",
			"stage", record.Stage,
			"action", record.Action,
			"request_id", record.RequestID,
			"error", err,
		)
	}
}

func auditAction(message map[string]any) string {
	action, _ := message["type"].(string)
	if action == "" || action == "command" {
		action, _ = message["action"].(string)
	}
	return action
}

func auditWriteDetails(message map[string]any) map[string]any {
	// A secret answering a noecho prompt gets no payload digest and no keys: the
	// digest of a low-entropy secret is crackable offline and the keys spell the
	// secret out. Only its shape is auditable.
	if auditAction(message) == "send_secret" {
		details := make(map[string]any, 1)
		if text, ok := message["text"].(string); ok {
			details["text_bytes"] = len(text)
		}
		return details
	}
	details := make(map[string]any)
	if encoded, err := json.Marshal(message); err == nil {
		digest := sha256.Sum256(encoded)
		details["payload_sha256"] = fmt.Sprintf("%x", digest[:])
		details["payload_bytes"] = len(encoded)
	}
	stringLimits := map[string]int{
		"name": 256, "profile_id": 160, "cwd": 1024,
		"filename": 512, "mime": 128, "activity_label": 256,
	}
	for key, limit := range stringLimits {
		if value, ok := message[key].(string); ok && value != "" {
			details[key] = boundedAuditString(value, limit)
		}
	}
	for _, key := range []string{"text", "prompt", "choice", "data"} {
		if value, ok := message[key].(string); ok && value != "" {
			details[key+"_bytes"] = len(value)
		}
	}
	for _, key := range []string{"index", "insert_index", "total", "_server_sequence"} {
		if value, ok := auditInteger(message[key]); ok {
			details[key] = value
		}
	}
	keys := make([]string, 0, 16)
	switch values := message["keys"].(type) {
	case []any:
		for _, value := range values {
			key, valid := value.(string)
			if !valid {
				continue
			}
			keys = append(keys, boundedAuditString(key, 64))
			if len(keys) == 32 {
				break
			}
		}
	case []string:
		for _, key := range values {
			keys = append(keys, boundedAuditString(key, 64))
			if len(keys) == 32 {
				break
			}
		}
	}
	if len(keys) > 0 {
		details["keys"] = keys
	}
	indices := make([]int64, 0, 8)
	switch values := message["selected_indices"].(type) {
	case []any:
		for _, value := range values {
			if index, ok := auditInteger(value); ok {
				indices = append(indices, index)
			}
			if len(indices) == 128 {
				break
			}
		}
	case []int:
		for _, value := range values {
			if value >= 0 {
				indices = append(indices, int64(value))
			}
			if len(indices) == 128 {
				break
			}
		}
	}
	if len(indices) > 0 {
		details["selected_indices"] = indices
	}
	return details
}

func boundedAuditString(value string, limit int) string {
	value = strings.TrimSpace(value)
	runes := []rune(value)
	if len(runes) <= limit {
		return value
	}
	return string(runes[:limit])
}

func auditInteger(value any) (int64, bool) {
	switch number := value.(type) {
	case int:
		if number >= 0 {
			return int64(number), true
		}
	case int64:
		if number >= 0 {
			return number, true
		}
	case uint64:
		if number <= uint64(^uint64(0)>>1) {
			return int64(number), true
		}
	case float64:
		if number >= 0 && number <= 1<<53 && number == float64(int64(number)) {
			return int64(number), true
		}
	}
	return 0, false
}

func isCoordinatorMutation(action string) bool {
	switch action {
	case "submit_prompt", "prompt", "send_keys", "keys", "send_text", "text",
		"respond", "answer_question", "navigate_question", "clarify_question",
		"agent_stop", "agent_rename", "tab_reorder", "acknowledge_pane", "agent_start",
		"agent_clear", "agent_restart", "lease_pane_size", "release_pane_size",
		"send_secret":
		return true
	default:
		return false
	}
}

func (s *Server) storePhoneAppOrigin(raw string) error {
	if raw == "" {
		return fmt.Errorf("origin is required")
	}
	parsed, err := url.Parse(raw)
	if err != nil || parsed.Scheme != "https" || parsed.Host == "" || parsed.User != nil {
		return fmt.Errorf("origin must be an HTTPS origin")
	}
	if parsed.Path != "" && parsed.Path != "/" || parsed.RawQuery != "" || parsed.Fragment != "" {
		return fmt.Errorf("origin must not contain a path, query, or fragment")
	}
	origin := "https://" + parsed.Host
	if err := os.MkdirAll(s.cfg.RuntimeDir, 0o700); err != nil {
		return err
	}
	path := filepath.Join(s.cfg.RuntimeDir, "phone-app-origin")
	temp := path + ".tmp"
	if err := os.WriteFile(temp, []byte(origin+"\n"), 0o600); err != nil {
		return err
	}
	return os.Rename(temp, path)
}

func (s *Server) writeSupportLoop(ctx context.Context) {
	s.writeSupportSnapshot()
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			s.writeSupportSnapshot()
			return
		case <-ticker.C:
			s.writeSupportSnapshot()
		}
	}
}

func (s *Server) writeSupportSnapshot() {
	if s.cfg.RuntimeDir == "" {
		return
	}
	readiness := "starting"
	if s.state.InventoryReady() {
		readiness = "ready"
	}
	releaseDirectory := ""
	if executable, err := os.Executable(); err == nil {
		releaseDirectory = filepath.Dir(executable)
	}
	webHash, webVersion := "", ""
	if s.webH != nil {
		webHash = s.webH.BundleHash()
		webVersion = s.webH.BundleVersion()
	}
	metrics := coordinator.SchedulerMetrics{}
	activityFailures := uint64(0)
	if s.dispatcher != nil {
		metrics = s.dispatcher.Metrics()
		activityFailures = s.dispatcher.ActivityFailures()
	}
	udpMetrics := coordinator.UDPMetrics{}
	if s.udp != nil {
		udpMetrics = s.udp.Metrics()
	}
	snapshot := support.Snapshot{
		Version:          s.version,
		Revision:         s.revision,
		Protocol:         protocol.Version,
		ReleaseDirectory: releaseDirectory,
		WebHash:          webHash,
		WebVersion:       webVersion,
		Readiness:        readiness,
		Inventory:        s.publicInventoryStatus(),
		Components: map[string]string{
			"http":        "running",
			"poller":      "running",
			"udp":         componentState(s.udp != nil),
			"persistence": componentState(s.journal != nil),
			"push":        componentState(s.pushM != nil),
		},
		Scheduler:        metrics,
		Transport:        s.hub.Metrics(),
		UDP:              udpMetrics,
		ActivityFailures: activityFailures,
		TopologyRetries:  s.state.TopologyRetryCount(),
		PollFailures:     s.poller.ConsecutiveFailures(),
		RecentErrors:     s.recentSafeErrors(),
	}
	if err := support.Write(s.cfg.RuntimeDir, snapshot); err != nil {
		s.recordSafeError("support snapshot write failed", err)
		s.logger.Debug("support snapshot write failed", "error", err)
	}
}

func (s *Server) recordSafeError(component string, err error) {
	message := strings.Join(strings.Fields(component), " ")
	if err != nil {
		detail := strings.Join(strings.Fields(err.Error()), " ")
		runes := []rune(detail)
		if len(runes) > 300 {
			detail = string(runes[:300])
		}
		if detail != "" {
			message += ": " + detail
		}
	}
	message = time.Now().UTC().Format(time.RFC3339) + " " + message
	s.mu.Lock()
	s.errors = append(s.errors, message)
	if len(s.errors) > 20 {
		s.errors = append([]string(nil), s.errors[len(s.errors)-20:]...)
	}
	s.mu.Unlock()
}

func (s *Server) recentSafeErrors() []string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return append([]string(nil), s.errors...)
}

func (s *Server) broadcastCommitted(message any) {
	envelope, ok := message.(map[string]any)
	if !ok {
		s.hub.Broadcast(message)
		return
	}
	messageType, _ := envelope["type"].(string)
	switch messageType {
	case "agents":
		var agents []*coordinator.AgentState
		data, err := json.Marshal(envelope["agents"])
		if err != nil {
			s.recordSafeError("agent snapshot broadcast was malformed", err)
			return
		}
		if err := json.Unmarshal(data, &agents); err != nil {
			s.recordSafeError("agent snapshot broadcast was malformed", err)
			return
		}
		s.hub.BroadcastPrepared(message, func() {
			s.stateViewMu.Lock()
			s.agentView = mergeAgentSnapshot(s.agentView, agents)
			s.stateViewMu.Unlock()
		})
	case "agent_update", "blocked":
		paneID, _ := envelope["pane_id"].(string)
		if paneID == "" {
			s.recordSafeError("agent update broadcast was malformed", nil)
			return
		}
		s.hub.BroadcastPrepared(message, func() {
			s.stateViewMu.Lock()
			agents := cloneAgents(s.agentView)
			found := false
			for _, agent := range agents {
				if agent.PaneID != paneID {
					continue
				}
				if deltaRevision(envelope) >= agent.StateRevision {
					applyAgentDelta(agent, envelope)
				}
				found = true
				break
			}
			if !found {
				if current, ok := s.state.Agent(paneID); ok {
					applyAgentDelta(current, envelope)
					agents = append(agents, current)
				}
			}
			s.agentView = agents
			s.stateViewMu.Unlock()
		})
	case "inventory_status":
		status := cloneStringMap(envelope)
		delete(status, "type")
		s.hub.BroadcastPrepared(message, func() {
			s.stateViewMu.Lock()
			s.inventoryView = status
			s.stateViewMu.Unlock()
		})
	case "activity":
		var entry activity.Entry
		data, err := json.Marshal(envelope["activity"])
		if err != nil {
			s.recordSafeError("activity broadcast was malformed", err)
			return
		}
		if err := json.Unmarshal(data, &entry); err != nil || entry.ID == "" {
			s.recordSafeError("activity broadcast was malformed", err)
			return
		}
		s.hub.BroadcastPrepared(message, func() {
			s.activityMu.Lock()
			s.activityView = append(s.activityView, entry)
			if len(s.activityView) > 500 {
				s.activityView = append([]activity.Entry(nil), s.activityView[len(s.activityView)-500:]...)
			}
			s.activityMu.Unlock()
		})
	case "activity_history":
		var entries []activity.Entry
		data, err := json.Marshal(envelope["activities"])
		if err != nil {
			s.recordSafeError("activity history broadcast was malformed", err)
			return
		}
		if err := json.Unmarshal(data, &entries); err != nil {
			s.recordSafeError("activity history broadcast was malformed", err)
			return
		}
		s.hub.BroadcastPrepared(message, func() {
			s.activityMu.Lock()
			s.activityView = append([]activity.Entry(nil), entries...)
			s.activityMu.Unlock()
		})
	default:
		s.hub.Broadcast(message)
	}
}

func (s *Server) committedAgents() []*coordinator.AgentState {
	s.stateViewMu.RLock()
	defer s.stateViewMu.RUnlock()
	return cloneAgents(s.agentView)
}

func (s *Server) committedInventoryStatus() map[string]any {
	s.stateViewMu.RLock()
	defer s.stateViewMu.RUnlock()
	return cloneStringMap(s.inventoryView)
}

func cloneAgents(agents []*coordinator.AgentState) []*coordinator.AgentState {
	result := make([]*coordinator.AgentState, 0, len(agents))
	for _, agent := range agents {
		if agent == nil {
			continue
		}
		copy := *agent
		copy.Options = append([]string(nil), agent.Options...)
		if agent.Interaction != nil {
			interaction := *agent.Interaction
			interaction.Options = append([]question.Option(nil), agent.Interaction.Options...)
			copy.Interaction = &interaction
		}
		result = append(result, &copy)
	}
	return result
}

func mergeAgentSnapshot(current, incoming []*coordinator.AgentState) []*coordinator.AgentState {
	merged := cloneAgents(incoming)
	currentByPane := make(map[string]*coordinator.AgentState, len(current))
	for _, agent := range current {
		currentByPane[agent.PaneID] = agent
	}
	for index, agent := range merged {
		if existing := currentByPane[agent.PaneID]; existing != nil &&
			agent.StateRevision < existing.StateRevision {
			merged[index] = cloneAgents([]*coordinator.AgentState{existing})[0]
		}
	}
	return merged
}

func deltaRevision(delta map[string]any) int64 {
	switch number := delta["pane_revision"].(type) {
	case int:
		return int64(number)
	case int64:
		return number
	case float64:
		return int64(number)
	default:
		// Revision-less legacy/internal deltas remain applicable.
		return int64(^uint64(0) >> 1)
	}
}

func cloneStringMap(values map[string]any) map[string]any {
	result := make(map[string]any, len(values))
	for key, value := range values {
		result[key] = value
	}
	return result
}

func applyAgentDelta(agent *coordinator.AgentState, delta map[string]any) {
	setString := func(key string, destination *string) {
		if value, exists := delta[key]; exists {
			if text, ok := value.(string); ok {
				*destination = text
			}
		}
	}
	setString("raw_pane_id", &agent.RawPaneID)
	setString("status", &agent.Status)
	setString("agent", &agent.Agent)
	setString("tab_id", &agent.TabID)
	setString("tab_label", &agent.TabLabel)
	setString("workspace_id", &agent.WorkspaceID)
	setString("cwd", &agent.Cwd)
	setString("project", &agent.Project)
	setString("host", &agent.Host)
	setString("session", &agent.Session)
	setString("session_name", &agent.SessionName)
	if value, exists := delta["updated_at"]; exists {
		switch v := value.(type) {
		case float64:
			agent.UpdatedAt = int64(v)
		case string:
			agent.UpdatedAt = parseTimestamp(v)
		}
	}
	setString("event_id", &agent.BlockedEventID)
	if value, exists := delta["attention_kind"]; exists {
		agent.AttentionKind = question.AttentionKind(fmt.Sprint(value))
	}
	setString("prompt", &agent.Prompt)
	setString("command", &agent.Command)
	if value, exists := delta["options"]; exists {
		agent.Options = nil
		switch options := value.(type) {
		case []string:
			agent.Options = append([]string(nil), options...)
		case []any:
			for _, option := range options {
				if text, ok := option.(string); ok {
					agent.Options = append(agent.Options, text)
				}
			}
		}
	}
	if value, exists := delta["interaction"]; exists {
		agent.Interaction = nil
		if value != nil {
			if data, err := json.Marshal(value); err == nil {
				var interaction question.Interaction
				if json.Unmarshal(data, &interaction) == nil {
					agent.Interaction = &interaction
				}
			}
		}
	}
	if value, exists := delta["question_layout"]; exists {
		agent.QuestionLayout, _ = value.(bool)
	}
	if agent.Status != "blocked" {
		agent.AttentionKind = ""
		agent.Prompt = ""
		agent.Command = ""
		agent.Options = nil
		agent.Interaction = nil
		agent.QuestionLayout = false
	} else {
		if agent.AttentionKind != question.AttentionApproval {
			agent.Options = nil
		}
		if agent.AttentionKind != question.AttentionQuestion {
			agent.Interaction = nil
			agent.QuestionLayout = false
		}
	}
	if value, exists := delta["tab_number"]; exists {
		agent.TabNumber = messageInt(value, agent.TabNumber)
	}
	if value, exists := delta["pane_revision"]; exists {
		switch number := value.(type) {
		case int64:
			agent.StateRevision = number
		case float64:
			agent.StateRevision = int64(number)
		}
	}
}

func parseTimestamp(s string) int64 {
	if n, err := strconv.ParseInt(s, 10, 64); err == nil {
		return n
	}
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t.UnixMilli()
	}
	return time.Now().UnixMilli()
}

func (s *Server) recentActivities(limit int) []activity.Entry {
	s.activityMu.RLock()
	if limit <= 0 || limit > len(s.activityView) {
		limit = len(s.activityView)
	}
	start := len(s.activityView) - limit
	entries := append([]activity.Entry(nil), s.activityView[start:]...)
	s.activityMu.RUnlock()
	return s.enrichActivityResponses(entries)
}

func (s *Server) enrichActivityResponses(entries []activity.Entry) []activity.Entry {
	if s.conversationM == nil || len(entries) == 0 {
		return entries
	}
	agents := make(map[string]*coordinator.AgentState)
	for _, agent := range s.state.Snapshot() {
		agents[agent.PaneID] = agent
	}
	pages := make(map[string]conversation.Page)
	for index := range entries {
		entry := entries[index]
		if strings.ToLower(strings.TrimSpace(entry.Kind)) != "finished" {
			continue
		}
		agentName := strings.TrimSpace(entry.Agent)
		sessionID := strings.TrimSpace(entry.Session)
		if current := agents[entry.PaneID]; current != nil {
			if strings.TrimSpace(current.Agent) != "" {
				agentName = current.Agent
			}
			if strings.TrimSpace(current.SessionID) != "" {
				sessionID = current.SessionID
			}
		}
		if agentName == "" || sessionID == "" || !conversation.Supported(agentName) {
			continue
		}
		cacheKey := agentName + "\x00" + sessionID
		page, loaded := pages[cacheKey]
		if !loaded {
			page, _ = s.conversationM.Read(agentName, sessionID, "", 200)
			pages[cacheKey] = page
		}
		if response := conversationResponseAt(page.Entries, int64(entry.Timestamp)); response != "" {
			entries[index].Extract = response
		}
	}
	return entries
}

func conversationResponseAt(entries []conversation.Entry, activityTimestamp int64) string {
	for index := len(entries) - 1; index >= 0; index-- {
		entry := entries[index]
		if entry.Role != "assistant" || strings.TrimSpace(entry.Text) == "" {
			continue
		}
		if activityTimestamp <= 0 {
			return entry.Text
		}
		timestamp, err := time.Parse(time.RFC3339Nano, entry.Timestamp)
		if err != nil || timestamp.UnixMilli() > activityTimestamp {
			continue
		}
		return entry.Text
	}
	return ""
}

func messageInt(value any, fallback int) int {
	switch number := value.(type) {
	case int:
		return number
	case float64:
		if number == float64(int(number)) {
			return int(number)
		}
	}
	return fallback
}

func componentState(available bool) string {
	if available {
		return "running"
	}
	return "unavailable"
}
