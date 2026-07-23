package app

import (
	"context"
	"encoding/json"
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
	"github.com/0cv/herdr-mobile-relay/internal/config"
	"github.com/0cv/herdr-mobile-relay/internal/coordinator"
	"github.com/0cv/herdr-mobile-relay/internal/fsutil"
	"github.com/0cv/herdr-mobile-relay/internal/herdr"
	"github.com/0cv/herdr-mobile-relay/internal/history"
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
)

type Server struct {
	cfg      *config.Config
	version  string
	revision string
	hostname string
	logger   *slog.Logger

	state      *coordinator.State
	hub        *transport.Hub
	poller     *coordinator.Poller
	udp        *coordinator.UDPListener
	journal    *activity.Journal
	pushM      *push.Manager
	historyM   *history.Manager
	profiles   *profiles.Resolver
	sessions   *session.Resolver
	webH       *web.Handler
	herdrC     *herdr.Client
	dispatcher *coordinator.Dispatcher
	updateM    *relayupdate.Manager
	appDeployM *appdeploy.Manager

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
}

func New(cfg *config.Config, version, revision string, logger *slog.Logger) *Server {
	state := coordinator.NewState(logger)
	hub := transport.NewHub(cfg, logger)
	herdrClient := herdr.NewClient(cfg.HerdrBin, cfg.SocketPath)

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
		cfg:            cfg,
		version:        version,
		revision:       revision,
		hostname:       hostname,
		logger:         logger,
		state:          state,
		hub:            hub,
		poller:         poller,
		herdrC:         herdrClient,
		profiles:       profResolver,
		sessions:       sessResolver,
		historyM:       histManager,
		updateM:        relayupdate.NewManager(cfg.ReleaseRoot, cfg.RuntimeDir, version, revision, cfg.ServiceName, healthURL),
		appDeployM:     appdeploy.NewManager(cfg.RuntimeDir, cfg.WebRoot, version, revision),
		startedAt:      time.Now(),
		refreshClients: make(map[string]bool),
		inventoryView:  cloneStringMap(state.InventoryStatus()),
	}
}

func (s *Server) Run(ctx context.Context) error {
	journal, err := activity.OpenJournal(s.cfg.CacheDir)
	if err != nil {
		s.recordSafeError("activity persistence unavailable", err)
		s.logger.Warn("activity journal unavailable", "error", err)
	} else {
		s.journal = journal
		s.activityView = journal.Recent(500)
	}

	pushDir := filepath.Join(s.cfg.RuntimeDir, "push")
	if pm, err := push.NewManager(pushDir, s.logger); err != nil {
		s.recordSafeError("push manager unavailable", err)
		s.logger.Warn("push manager unavailable", "error", err)
	} else {
		s.pushM = pm
	}

	s.state.SetOnTransition(func(paneID, agent, project, status string, revision int64) {
		if !s.state.TransitionCurrent(paneID, status, revision) {
			return
		}
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()

		var session string
		if agentState, ok := s.state.Agent(paneID); ok {
			session = agentState.Session
		}

		if status == "blocked" {
			eventID := paneID
			hasApproval := false
			approvalTotal := 0
			if agentState, ok := s.state.Agent(paneID); ok {
				if agentState.BlockedEventID != "" {
					eventID = agentState.BlockedEventID
				}
				hasApproval = agentState.Interaction == nil && !agentState.QuestionLayout
				approvalTotal = len(agentState.Options)
			}
			summary := agent + " needs approval"
			if s.dispatcher != nil {
				s.dispatcher.RecordTransitionActivity("blocked", "attention", summary, paneID, status, revision,
					map[string]any{"event_id": eventID}, agent, project, s.hostname, session)
			}
			if s.pushM != nil {
				payload := push.BuildBlockedPayload(agent, project, eventID, paneID, s.hostname, hasApproval, approvalTotal)
				s.pushM.Send(ctx, payload)
			}
			if agentState, ok := s.state.Agent(paneID); ok {
				s.hub.Broadcast(map[string]any{
					"type":            "blocked",
					"pane_id":         agentState.PaneID,
					"raw_pane_id":     agentState.RawPaneID,
					"terminal_id":     agentState.TerminalID,
					"tab_id":          agentState.TabID,
					"tab_label":       agentState.TabLabel,
					"tab_number":      agentState.TabNumber,
					"workspace_id":    agentState.WorkspaceID,
					"agent":           agentState.Agent,
					"name":            agentState.Name,
					"status":          "blocked",
					"cwd":             agentState.Cwd,
					"project":         agentState.Project,
					"host":            agentState.Host,
					"session":         agentState.Session,
					"updated_at":      agentState.UpdatedAt,
					"event_id":        eventID,
					"prompt":          agentState.Prompt,
					"command":         agentState.Command,
					"options":         agentState.Options,
					"interaction":     agentState.Interaction,
					"interaction_id":  agentState.InteractionID,
					"question_layout": agentState.QuestionLayout,
				})
			}
		} else if s.state.RegisterFinishedNotificationForTransition(paneID, status, revision) {
			eventID := fmt.Sprintf("%d-%s", time.Now().UnixNano(), paneID)
			summary := agent + " finished"
			if s.dispatcher != nil {
				s.dispatcher.RecordTransitionActivity("finished", "completed", summary, paneID, status, revision,
					map[string]any{"event_id": eventID}, agent, project, s.hostname, session)
			}
			if s.pushM != nil {
				payload := push.BuildFinishedPayload(agent, project, paneID, s.hostname, eventID)
				s.pushM.Send(ctx, payload)
			}
		} else if s.dispatcher != nil {
			kind, activityStatus, summary := "finished", "completed", agent+" finished"
			if status == "blocked" {
				kind, activityStatus, summary = "blocked", "attention", agent+" needs approval"
			}
			s.dispatcher.RecordTransitionActivity(kind, activityStatus, summary, paneID, status, revision, nil, agent, project, s.hostname, session)
		}
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
		if s.appDeployM.State().Configured {
			capabilities = append(capabilities, "app_deploy")
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
		if !coordinated {
			admitted()
		}

		ctx := context.Background()

		switch action {
		case "check_update":
			s.hub.Broadcast(map[string]any{"type": "update_status", "update": map[string]any{
				"state":            "checking",
				"current_version":  s.version,
				"current_revision": s.revision,
			}})
			updateState := s.updateM.Check(ctx)
			s.hub.Broadcast(map[string]any{"type": "update_status", "update": updateState})
			s.sendCommandResult(client, inbound.RequestID, "check_update", true, "completed", "", map[string]any{"update": updateState})
		case "install_update":
			job, updateState, scheduleErr := s.updateM.Schedule(ctx, inbound.ExpectedVersion, inbound.ExpectedRevision)
			if scheduleErr != nil {
				s.sendCommandResult(client, inbound.RequestID, "install_update", false, "failed", scheduleErr.Error(), map[string]any{"update": updateState})
				break
			}
			s.hub.Broadcast(map[string]any{"type": "update_status", "update": updateState})
			s.sendCommandResult(client, inbound.RequestID, "install_update", true, "scheduled", "", map[string]any{"job": job, "update": updateState})
		case "deploy_app_update":
			job, deployState, scheduleErr := s.appDeployM.Schedule(ctx, inbound.ExpectedVersion, inbound.ExpectedRevision, inbound.ExpectedOrigin)
			if scheduleErr != nil {
				s.sendCommandResult(client, inbound.RequestID, "deploy_app_update", false, "failed", scheduleErr.Error(), map[string]any{"app_deploy": deployState})
				break
			}
			s.hub.Broadcast(map[string]any{"type": "app_deploy_status", "app_deploy": deployState})
			s.sendCommandResult(client, inbound.RequestID, "deploy_app_update", true, "scheduled", "", map[string]any{"job": job, "app_deploy": deployState})
		case "read_pane":
			resp := s.dispatcher.HandleReadPane(ctx, msg)
			paneID, _ := msg["pane_id"].(string)
			agent, _ := s.agentInfo(paneID)
			agentLower := strings.ToLower(agent)
			if content, ok := resp["content"].(string); ok {
				interaction := question.Parse(content, agent)
				resp["interaction"] = interaction
				resp["question_layout"] = question.LayoutHint(content)
				if interaction == nil && (strings.Contains(agentLower, "claude") || strings.Contains(agentLower, "qoder")) {
					resp["content"] = s.historyM.Merge(paneID, content)
				}
			}
			s.hub.Send(client, resp)
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
				s.hub.Send(client, map[string]any{
					"type":       "command_result",
					"request_id": result.RequestID,
					"action":     result.Action,
					"ok":         result.OK,
					"phase":      result.Phase,
					"error":      result.Error,
				})
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
		case "list_directories":
			requestID, _ := msg["request_id"].(string)
			path, _ := msg["path"].(string)
			home, _ := os.UserHomeDir()
			listing := fsutil.ListDirectories(path, home)
			s.hub.Send(client, map[string]any{
				"type":       "command_result",
				"request_id": requestID,
				"action":     "list_directories",
				"ok":         true,
				"phase":      "completed",
				"data":       listing,
			})
		case "list_slash_commands":
			requestID, _ := msg["request_id"].(string)
			paneID, _ := msg["pane_id"].(string)
			if paneID == "" {
				s.sendCommandResult(client, requestID, "list_slash_commands", false, "failed", "Agent is required", nil)
				break
			}
			if _, ok := s.state.Agent(paneID); !ok {
				s.sendCommandResult(client, requestID, "list_slash_commands", false, "failed", "Agent pane not found", nil)
				break
			}
			generation := s.state.Generation(paneID)
			agent, cwd := s.agentInfo(paneID)
			home, _ := os.UserHomeDir()
			profileID := s.profiles.ResolvePane(paneID, agent)
			skillDirs, commandFormat, _ := s.profiles.CommandConfig(profileID)
			catalog := slashcmd.CatalogForProfile(profileID, agent, cwd, home, skillDirs, commandFormat)
			if s.state.Generation(paneID) != generation {
				s.sendCommandResult(
					client,
					requestID,
					"list_slash_commands",
					false,
					"failed",
					"The agent pane was replaced while commands were being listed",
					nil,
				)
				break
			}
			s.hub.Send(client, map[string]any{
				"type":       "command_result",
				"request_id": requestID,
				"action":     "list_slash_commands",
				"ok":         true,
				"phase":      "completed",
				"pane_id":    paneID,
				"data":       catalog,
			})
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
		default:
			var result *coordinator.CommandResult
			if coordinated {
				result = s.dispatcher.HandleAdmitted(ctx, msg, admitted)
			} else {
				result = s.dispatcher.Handle(ctx, msg)
			}
			s.hub.Send(client, map[string]any{
				"type":       "command_result",
				"request_id": result.RequestID,
				"action":     result.Action,
				"ok":         result.OK,
				"phase":      result.Phase,
				"error":      result.Error,
				"pane_id":    result.PaneID,
				"data":       result.Data,
			})
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
				"type":         "agent_update",
				"pane_id":      agent.PaneID,
				"raw_pane_id":  agent.RawPaneID,
				"status":       agent.Status,
				"agent":        agent.Agent,
				"tab_id":       agent.TabID,
				"tab_label":    agent.TabLabel,
				"tab_number":   agent.TabNumber,
				"workspace_id": agent.WorkspaceID,
				"cwd":          agent.Cwd,
				"project":      agent.Project,
				"host":         agent.Host,
				"updated_at":   agent.UpdatedAt,
				"event_id":     agent.BlockedEventID,
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
		s.dispatcher.PruneSlots(active)
	})
	s.poller.SetOnInventoryStatus(func(status map[string]any) {
		s.broadcastCommitted(inventoryStatusMessage(status))
	})

	s.poller.SetEnrich(func(ctx context.Context, agents []*coordinator.AgentState) {
		for _, a := range agents {
			if a.Session != "" {
				if title := s.sessions.SessionName(a.Agent, a.Cwd, a.Session); title != "" {
					a.Session = title
				}
			}
			if a.Status != "blocked" {
				continue
			}
			readCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
			content, err := s.herdrC.ReadPane(readCtx, a.PaneID, 80, "ansi")
			cancel()
			if err != nil {
				s.recordSafeError("blocked pane enrichment failed", err)
				s.logger.Warn("blocked pane enrichment failed", "pane_id", a.PaneID, "error", err)
				a.Prompt = "Agent is blocked"
				_, _, a.Options = question.ApprovalDetails("")
				continue
			}
			raw := string(content)
			a.QuestionLayout = question.LayoutHint(raw)
			a.Interaction = question.Parse(raw, a.Agent)
			a.Prompt, a.Command, a.Options = question.ApprovalDetails(raw)
			if a.Interaction != nil {
				a.Command = a.Interaction.Question
				a.Options = nil
				a.InteractionID = a.Interaction.ID
			} else if a.QuestionLayout {
				a.Options = nil
			}
		}
	})

	mux := http.NewServeMux()
	mux.HandleFunc("GET /health", s.handleHealth)
	mux.HandleFunc("GET /healthz", s.handleHealthz)
	mux.HandleFunc("GET /readyz", s.handleReadyz)
	mux.HandleFunc("/ws", s.hub.HandleWebSocket)
	mux.HandleFunc("/", s.handleRoot)

	ln, err := net.Listen("tcp", s.cfg.Addr())
	if err != nil {
		return fmt.Errorf("listen %s: %w", s.cfg.Addr(), err)
	}

	srv := &http.Server{
		Handler:      mux,
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

	go s.poller.Run(ctx)
	profileSignals := make(chan os.Signal, 1)
	signal.Notify(profileSignals, syscall.SIGHUP)
	defer signal.Stop(profileSignals)
	go s.reloadProfilesLoop(ctx, profileSignals)
	if s.udp != nil {
		go s.udp.Run(ctx)
	}
	go s.pruneUploads(ctx)
	go s.writeSupportLoop(ctx)
	go s.watchJobStates(ctx)
	go s.updateCheckLoop(ctx)

	errCh := make(chan error, 1)
	go func() {
		errCh <- srv.Serve(ln)
	}()

	select {
	case <-ctx.Done():
		s.logger.Info("shutting down")
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if s.dispatcher != nil {
			s.dispatcher.CancelInflight()
		}
		_ = srv.Shutdown(shutdownCtx)
		_ = s.hub.Shutdown(shutdownCtx)
		if s.dispatcher != nil {
			_ = s.dispatcher.Close(shutdownCtx)
		}
		if s.udp != nil {
			_ = s.udp.Close()
		}
		if s.webH != nil {
			_ = s.webH.Close()
		}
		return nil
	case err := <-errCh:
		if err == http.ErrServerClosed {
			return nil
		}
		return err
	}
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

func (s *Server) sendCommandResult(
	client *transport.ClientConn,
	requestID, action string,
	ok bool,
	phase, publicError string,
	data any,
) {
	s.hub.Send(client, map[string]any{
		"type":       "command_result",
		"request_id": requestID,
		"action":     action,
		"ok":         ok,
		"phase":      phase,
		"error":      publicError,
		"data":       data,
	})
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

func isCoordinatorMutation(action string) bool {
	switch action {
	case "submit_prompt", "prompt", "send_keys", "keys", "send_text", "text",
		"respond", "answer_question", "navigate_question", "clarify_question",
		"agent_stop", "agent_rename", "acknowledge_pane", "agent_start",
		"agent_clear", "agent_restart":
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
			s.agentView = cloneAgents(agents)
			s.stateViewMu.Unlock()
		})
	case "agent_update":
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
				applyAgentDelta(agent, envelope)
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
	if value, exists := delta["updated_at"]; exists {
		switch v := value.(type) {
		case float64:
			agent.UpdatedAt = int64(v)
		case string:
			agent.UpdatedAt = parseTimestamp(v)
		}
	}
	setString("event_id", &agent.BlockedEventID)
	if value, exists := delta["tab_number"]; exists {
		agent.TabNumber = messageInt(value, agent.TabNumber)
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
	defer s.activityMu.RUnlock()
	if limit <= 0 || limit > len(s.activityView) {
		limit = len(s.activityView)
	}
	start := len(s.activityView) - limit
	return append([]activity.Entry(nil), s.activityView[start:]...)
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
