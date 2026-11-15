package app

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	herdrmobilerelay "github.com/0cv/herdr-mobile-relay"
	"github.com/0cv/herdr-mobile-relay/internal/activity"
	"github.com/0cv/herdr-mobile-relay/internal/agentkind"
	"github.com/0cv/herdr-mobile-relay/internal/agentroots"
	"github.com/0cv/herdr-mobile-relay/internal/appdeploy"
	"github.com/0cv/herdr-mobile-relay/internal/audit"
	"github.com/0cv/herdr-mobile-relay/internal/backend"
	"github.com/0cv/herdr-mobile-relay/internal/clipboard"
	"github.com/0cv/herdr-mobile-relay/internal/config"
	"github.com/0cv/herdr-mobile-relay/internal/conversation"
	"github.com/0cv/herdr-mobile-relay/internal/coordinator"
	"github.com/0cv/herdr-mobile-relay/internal/copyresponse"
	"github.com/0cv/herdr-mobile-relay/internal/deviceauth"
	"github.com/0cv/herdr-mobile-relay/internal/fsutil"
	"github.com/0cv/herdr-mobile-relay/internal/hc"
	"github.com/0cv/herdr-mobile-relay/internal/herdr"
	"github.com/0cv/herdr-mobile-relay/internal/history"
	"github.com/0cv/herdr-mobile-relay/internal/noecho"
	"github.com/0cv/herdr-mobile-relay/internal/panesize"
	"github.com/0cv/herdr-mobile-relay/internal/piintegration"
	"github.com/0cv/herdr-mobile-relay/internal/profiles"
	"github.com/0cv/herdr-mobile-relay/internal/protocol"
	"github.com/0cv/herdr-mobile-relay/internal/push"
	"github.com/0cv/herdr-mobile-relay/internal/question"
	"github.com/0cv/herdr-mobile-relay/internal/session"
	"github.com/0cv/herdr-mobile-relay/internal/setuphelper"
	"github.com/0cv/herdr-mobile-relay/internal/speech"
	"github.com/0cv/herdr-mobile-relay/internal/transport"
	relayupdate "github.com/0cv/herdr-mobile-relay/internal/update"
	"github.com/0cv/herdr-mobile-relay/internal/upload"
	"github.com/0cv/herdr-mobile-relay/internal/web"
)

const pushTestInterval = 10 * time.Second

type paneSizer interface {
	Available() bool
	Acquire(context.Context, string, string, int, int) (int, int, error)
	Release(context.Context, string, string, bool) error
	ReleaseClient(context.Context, string) error
	ReadGeometry(string, string) (int, int, bool)
	ResizedWithin(string, time.Duration) bool
	Run(context.Context)
	Shutdown(context.Context) error
}

type piSessionIdentity struct {
	pid     int
	session string
}

type Server struct {
	piSessionsMu sync.Mutex
	piSessions   map[string]piSessionIdentity
	cfg          *config.Config
	version      string
	revision     string
	hostname     string
	home         string
	logger       *slog.Logger

	state               *coordinator.State
	hub                 *transport.Hub
	poller              *coordinator.Poller
	udp                 *coordinator.UDPListener
	journal             *activity.Journal
	auditLog            *audit.Logger
	pushM               *push.Manager
	transitionBroadcast interface {
		Broadcast(any)
	}
	transitionEnrich func(context.Context, *coordinator.AgentState)
	sessions         *session.Resolver
	historyM         *history.Manager
	conversationM    *conversation.Reader
	conversationB    *conversation.Browser
	profiles         *profiles.Resolver
	webH             *web.Handler
	herdrC           backend.Backend
	kimiProcessBirth func(int) (string, error)
	clipboardRead    func(context.Context) ([]byte, error)
	clipboardWrite   func(context.Context, []byte) error
	copyRunner       copyResponseRunner
	speechSynth      func(context.Context, string, string) ([]byte, error)
	speechStatus     func() speech.Catalog
	speechInstall    func(context.Context, string) error
	speechRemove     func(string) error
	speechMu         sync.Mutex
	speechLanguages  []string
	speechRequests   map[string]*speechRequest
	copyMu           sync.Mutex
	paneSizeM        paneSizer
	dispatcher       *coordinator.Dispatcher
	updateM          *relayupdate.Manager
	piIntegration    *piintegration.Manager
	appDeployM       *appdeploy.Manager
	hybrid           *hybridTransport
	uploadM          *upload.Manager
	deviceAuth       *deviceauth.Store
	initErr          error

	mu        sync.RWMutex
	ready     bool
	startedAt time.Time
	errors    []string

	activityMu   sync.RWMutex
	activityView []activity.Entry

	stateViewMu   sync.RWMutex
	agentView     []*coordinator.AgentState
	workspaceView []herdr.Workspace
	inventoryView map[string]any

	refreshMu      sync.Mutex
	refreshClients map[string]bool
	// Optional deterministic publication observer; installed before serving.
	inventoryPublicationObserver func(string)
	// Optional test hook between history loading and tuple revalidation.
	conversationHistoryReadObserver func()

	conversationLiveMu  sync.Mutex
	conversationFeeds   map[string]*conversationFeed
	conversationClients map[string]*conversationFeed

	paneWatchMu sync.Mutex
	paneWatches map[string]*paneWatch

	historyCaptureMu  sync.Mutex
	historyInflight   map[string]bool
	historyLast       map[string]time.Time
	historyActive     map[string]bool
	historyReconciled bool
	pushReconcileMu   sync.Mutex
	pushReconciled    bool
	pushTestMu        sync.Mutex
	pushTestLast      map[string]time.Time
	transitionTasks   *lifecycleTasks
	historyTasks      *lifecycleTasks
}

func newBackend(cfg *config.Config) backend.Backend {
	if cfg.Backend == "hc" {
		return hc.New(cfg.SocketPath)
	}
	return herdr.NewClient(cfg.HerdrBin, cfg.SocketPath)
}

func New(cfg *config.Config, version, revision string, logger *slog.Logger) *Server {
	state := coordinator.NewState(logger)
	hub := transport.NewHub(cfg, logger)
	herdrClient := newBackend(cfg)
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
	var paneSizeM paneSizer
	if cfg.Backend == "hc" {
		hostname += " (core)"
		poller.SetHostname(hostname)
		paneSizeM = hc.NewPaneSizer(herdrClient.(*hc.Client))
	} else {
		paneSizeM = panesize.NewManager(herdrClient, logger)
	}
	profResolver := profiles.NewResolver(cfg.ConfigHome, herdrClient)
	if cfg.RuntimeDir != "" {
		if err := profResolver.SetOwnershipPath(filepath.Join(cfg.RuntimeDir, "profile-ownership.json")); err != nil {
			logger.Warn("profile ownership requires recovery; relaunch is disabled", "error", err)
		}
	}
	conversationReader := conversation.NewReader(home)
	var conversationBrowser *conversation.Browser
	cacheRoot := ""
	if strings.TrimSpace(cfg.CacheDir) != "" {
		cacheRoot = filepath.Join(cfg.CacheDir, "conversation-history")
	}
	browserOptions := conversation.DefaultBrowserOptions()
	browserOptions.CapacityObserver = conversationCapacityLogger(logger)
	conversationBrowser, browserErr := conversation.NewBrowser(conversationReader, cacheRoot, browserOptions)
	if browserErr != nil {
		logger.Warn("interactive conversation browsing unavailable", "error", browserErr)
	}
	sessResolver := session.NewResolverWithReader(home, conversationReader)
	histManager := history.NewManager(cfg.CacheDir)
	healthURL := fmt.Sprintf("http://127.0.0.1:%d/healthz", cfg.Port)

	var uploadManager *upload.Manager
	var uploadErr error
	if strings.TrimSpace(cfg.CacheDir) == "" {
		uploadErr = errors.New("cache directory is required")
	} else {
		uploadManager, uploadErr = upload.NewManager(upload.Config{
			Root: filepath.Join(cfg.CacheDir, "uploads"), Logger: logger,
		})
	}
	if uploadErr != nil {
		logger.Warn("attachment uploads unavailable", "error", uploadErr)
	}
	var deviceStore *deviceauth.Store
	var deviceStoreErr error
	if cfg.Token != "" {
		var storeOptions []deviceauth.Option
		if cfg.RearmBootstrap {
			storeOptions = append(storeOptions, deviceauth.WithBootstrapReenrollment())
		}
		store, err := deviceauth.Open(filepath.Join(cfg.RuntimeDir, "device-auth"), storeOptions...)
		if err != nil {
			deviceStoreErr = fmt.Errorf("initialize device authentication: %w", err)
		} else if err := armBootstrap(store, cfg, hostname); err != nil {
			deviceStoreErr = fmt.Errorf("initialize device pairing: %w", err)
		} else {
			deviceStore = store
			hub.SetE2EEAuthResolver(store)
		}
	}

	speechLanguages := speech.Languages()
	logger.Info("speech synthesis available", "languages", strings.Join(speechLanguages, ","))
	initialInventory := state.InventorySnapshot()

	return &Server{
		cfg:                 cfg,
		version:             version,
		revision:            revision,
		hostname:            hostname,
		home:                home,
		logger:              logger,
		state:               state,
		hub:                 hub,
		transitionBroadcast: hub,
		poller:              poller,
		clipboardRead:       clipboardRead,
		clipboardWrite:      clipboard.Write,
		copyRunner:          copyresponse.Run,
		speechSynth:         speech.Synthesize,
		speechStatus:        speech.Status,
		speechInstall:       speech.Install,
		speechRemove:        speech.Remove,
		speechLanguages:     speechLanguages,
		herdrC:              herdrClient,
		paneSizeM:           paneSizeM,
		profiles:            profResolver,
		sessions:            sessResolver,
		historyM:            histManager,
		conversationM:       conversationReader,
		conversationB:       conversationBrowser,
		updateM:             relayupdate.NewManager(cfg.ReleaseRoot, cfg.RuntimeDir, cfg.HerdrBin, version, revision, healthURL),
		piIntegration:       piintegration.New(herdrmobilerelay.PiCommandBridge(), func() []string { return agentroots.PiConfigDirs(home) }),
		appDeployM:          appdeploy.NewManager(cfg.RuntimeDir, cfg.WebRoot, version, revision),
		uploadM:             uploadManager,
		deviceAuth:          deviceStore,
		initErr:             deviceStoreErr,
		startedAt:           time.Now(),
		refreshClients:      make(map[string]bool),
		paneWatches:         make(map[string]*paneWatch),
		agentView:           cloneAgents(initialInventory.Agents),
		workspaceView:       cloneWorkspaces(initialInventory.Workspaces),
		inventoryView:       cloneStringMap(initialInventory.Status),
		historyInflight:     make(map[string]bool),
		historyLast:         make(map[string]time.Time),
		historyActive:       make(map[string]bool),
		pushTestLast:        make(map[string]time.Time),
	}
}

// armBootstrap prepares the relay's one-use pairing invitation. A re-armed
// launch serves the app from a new hostname, so every device enrolled under
// the previous one is stranded; starting from an empty device list keeps those
// dead entries out of Settings instead of accumulating one per launch.
func armBootstrap(store *deviceauth.Store, cfg *config.Config, hostname string) error {
	if cfg.RearmBootstrap {
		return store.ResetWithBootstrap([]byte(cfg.Token), hostname, "en")
	}
	return store.EnsureBootstrapInvitation([]byte(cfg.Token), hostname, "en")
}

// reservePushTest rate limits delivery tests per device. Entries older than one
// interval carry no decision, so they are dropped on the way past: a relay that
// enrols and revokes devices for months must not accumulate one timestamp per
// device that ever asked for a test.
func (s *Server) reservePushTest(deviceID string, now time.Time) bool {
	s.pushTestMu.Lock()
	defer s.pushTestMu.Unlock()
	for device, last := range s.pushTestLast {
		if now.Sub(last) >= pushTestInterval {
			delete(s.pushTestLast, device)
		}
	}
	if last, exists := s.pushTestLast[deviceID]; exists && now.Sub(last) < pushTestInterval {
		return false
	}
	s.pushTestLast[deviceID] = now
	return true
}

func (s *Server) forgetPushTest(deviceID string) {
	s.pushTestMu.Lock()
	defer s.pushTestMu.Unlock()
	delete(s.pushTestLast, deviceID)
}

const deviceManagementUnavailable = "Device management is unavailable"

func deviceCredentialID(store *deviceauth.Store, deviceID string) (string, bool) {
	if store == nil || strings.TrimSpace(deviceID) == "" {
		return "", false
	}
	for _, credential := range store.ListCredentials("") {
		if credential.DeviceID == deviceID {
			return credential.CredentialID, true
		}
	}
	return "", false
}

func (s *Server) credentialIDOrFail(reply commandReply, deviceID string) (string, bool) {
	credentialID, found := deviceCredentialID(s.deviceAuth, deviceID)
	if !found {
		reply.fail("Device credential was not found")
	}
	return credentialID, found
}

func activeDeviceCredentials(store *deviceauth.Store, currentCredentialID string) []deviceauth.Credential {
	credentials := store.ListCredentials(currentCredentialID)
	active := credentials[:0]
	for _, credential := range credentials {
		if !credential.Revoked {
			active = append(active, credential)
		}
	}
	return active
}

func (s *Server) disconnectCredentials(credentials []deviceauth.Credential) {
	for _, credential := range credentials {
		s.hub.DisconnectCredential(credential.CredentialID, credential.Version)
	}
}

type pushPolicyWire struct {
	Categories  map[push.Category]bool `json:"categories"`
	SettleMS    int64                  `json:"settle_ms"`
	CooldownMS  int64                  `json:"cooldown_ms"`
	SnoozeUntil string                 `json:"snooze_until,omitempty"`
	Snoozed     bool                   `json:"snoozed"`
	UpdateOnce  bool                   `json:"update_once"`
}

func boundPushPolicy(raw json.RawMessage, deviceID, locale string, current push.DevicePolicy) (push.DevicePolicy, error) {
	var wire pushPolicyWire
	if len(raw) == 0 || json.Unmarshal(raw, &wire) != nil {
		return push.DevicePolicy{}, errors.New("push_invalid_policy")
	}
	if wire.SettleMS < 0 || wire.CooldownMS < 0 {
		return push.DevicePolicy{}, errors.New("push_invalid_duration")
	}
	current.DeviceID = deviceID
	current.Locale = locale
	current.Categories = wire.Categories
	current.Settle = time.Duration(wire.SettleMS) * time.Millisecond
	current.Cooldown = time.Duration(wire.CooldownMS) * time.Millisecond
	current.Snoozed = wire.Snoozed
	current.UpdateOnce = wire.UpdateOnce
	current.SnoozeUntil = time.Time{}
	if wire.SnoozeUntil != "" {
		until, err := time.Parse(time.RFC3339, wire.SnoozeUntil)
		if err != nil {
			return push.DevicePolicy{}, errors.New("push_invalid_snooze")
		}
		current.SnoozeUntil = until
	}
	return current, nil
}

func pushPolicyResponse(policy push.DevicePolicy) map[string]any {
	categories := make(map[push.Category]bool, len(policy.Categories))
	for category, enabled := range policy.Categories {
		categories[category] = enabled
	}
	result := map[string]any{
		"device_id":   policy.DeviceID,
		"locale":      policy.Locale,
		"categories":  categories,
		"settle_ms":   policy.Settle.Milliseconds(),
		"cooldown_ms": policy.Cooldown.Milliseconds(),
		"snoozed":     policy.Snoozed,
		"update_once": policy.UpdateOnce,
	}
	if !policy.SnoozeUntil.IsZero() {
		result["snooze_until"] = policy.SnoozeUntil.UTC().Format(time.RFC3339)
	}
	return result
}

func (s *Server) pushTargetCurrent(target protocol.TargetRef) bool {
	agent, ok := s.state.Agent(target.PaneID)
	return ok && targetMatchesAgent(target, agent)
}

func projectContextForAgent(agent *coordinator.AgentState) conversation.ProjectContext {
	if agent == nil {
		return conversation.ProjectContext{}
	}
	return conversation.NormalizeProjectContext(agent.Agent, conversation.ProjectContext{
		CWD: agent.Cwd, ForegroundCWD: agent.ForegroundCwd,
	})
}

func (s *Server) resolveAgentSessionName(agent *coordinator.AgentState) {
	if agent == nil {
		return
	}
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
	if conversation.IsKimi(agent.Agent) && agent.ConversationReason != "" {
		sessionID = ""
	}
	agent.SessionID = sessionID
	// AgentSessionID is the wire copy of SessionID. SessionID itself is
	// json:"-", and the agents broadcast deep-copies snapshots through JSON,
	// so only a field set before that round trip reaches the phone - and the
	// phone must echo it back for exact-target validation to ever pass.
	agent.AgentSessionID = sessionID
	agent.ConversationHistoryAvailable = sessionID != "" && conversation.Supported(agent.Agent)
	if sessionID == "" {
		return
	}
	title := ""
	if conversation.IsKimi(agent.Agent) && agent.ConversationInferred {
		id, location, code := s.conversationM.SelectKimiSession(projectContextForAgent(agent), "")
		if id != sessionID || code != "" || conversation.KimiSourceIdentity(location) != agent.ConversationSource {
			return
		}
		title = location.Title
	} else {
		title = s.sessions.SessionNameWithProject(agent.Agent, projectContextForAgent(agent), sessionID)
	}
	if title == "" {
		return
	}
	agent.SessionName = title
	agent.Session = title
}

func (s *Server) Run(ctx context.Context) error {
	if s.initErr != nil {
		if s.conversationB != nil {
			_ = s.conversationB.Close()
		}
		return s.initErr
	}
	if s.conversationB != nil {
		defer s.conversationB.Close()
	}
	runCtx, cancelRun := context.WithCancel(ctx)
	defer cancelRun()
	ctx = runCtx
	s.transitionTasks = newLifecycleTasks(ctx)
	s.historyTasks = newLifecycleTasks(ctx)
	defer s.drainLifecycleWork()
	if s.uploadM != nil {
		defer s.uploadM.Close()
	}
	if err := s.state.EnableTriagePersistence(s.cfg.CacheDir); err != nil {
		s.recordSafeError("durable agent triage unavailable", err)
		s.logger.Warn("durable agent triage unavailable", "error", err)
	}
	if err := s.writePIDFile(); err != nil {
		s.recordSafeError("relay pid file unavailable", err)
		s.logger.Warn("relay pid file unavailable", "error", err)
	} else {
		defer os.Remove(s.pidFilePath())
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

	s.refreshPiIntegration()

	pushDir := filepath.Join(s.cfg.RuntimeDir, "push")
	pm, err := push.NewManager(pushDir, s.logger)
	if err != nil {
		return fmt.Errorf("initialize push manager: %w", err)
	}
	s.pushM = pm

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
	s.herdrC.SetCapabilityChangeCallback(func(status herdr.ServerStatus) {
		s.hub.Broadcast(map[string]any{
			"type":         "herdr_status",
			"status":       status,
			"capabilities": s.effectiveCapabilitiesFor(status),
		})
	})
	s.hub.SetOnConnect(s.sendConnectionSnapshot)

	s.hub.SetOnDisconnect(s.handleDisconnect)

	s.hub.SetHandler(func(client *transport.ClientConn, msg map[string]any, admitted func()) {
		s.handleMessage(ctx, client, msg, admitted)
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
			s.commitAgent(agent, map[string]any{
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

	s.setInventoryPublisher(ctx)

	s.poller.SetEnrich(func(ctx context.Context, agents []*coordinator.AgentState) {
		s.enrichPiSessions(ctx, agents)
		s.enrichRegistrySessions(ctx, agents)
		s.enrichKimiSessions(ctx, agents)
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
				setAgentAttention(s.state, a, unknownPaneClassification())
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
	s.hybrid = s.startHybridTransport(ctx)
	startBackground(func() { s.pushM.Run(ctx) })
	startBackground(func() { s.poller.Run(ctx) })
	startBackground(func() { s.herdrC.RunCapabilityRefresh(ctx, 30*time.Second) })
	startBackground(func() { s.poller.RunEvents(ctx) })
	startBackground(func() { s.captureHistoryLoop(ctx) })
	startBackground(func() { s.paneSizeM.Run(ctx) })
	profileSignals := make(chan os.Signal, 1)
	signal.Notify(profileSignals, syscall.SIGHUP)
	defer signal.Stop(profileSignals)
	startBackground(func() { s.reloadProfilesLoop(ctx, profileSignals) })
	bootstrapSignals := make(chan os.Signal, 1)
	signal.Notify(bootstrapSignals, syscall.SIGUSR1)
	defer signal.Stop(bootstrapSignals)
	startBackground(func() { s.armBootstrapLoop(ctx, bootstrapSignals) })
	if s.udp != nil {
		startBackground(func() { s.udp.Run(ctx) })
	}
	startBackground(func() { s.pruneUploads(ctx) })
	startBackground(func() { s.writeSupportLoop(ctx) })
	startBackground(func() { s.watchJobStates(ctx) })
	startBackground(func() { s.updateCheckLoop(ctx) })
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

func (s *Server) rejectMessage(client *transport.ClientConn, admitted func(), response map[string]any) {
	admitted()
	s.hub.Send(client, response)
}

func (s *Server) sendResult(client *transport.ClientConn, msg map[string]any, auditedWrite bool, result *coordinator.CommandResult) {
	if auditedWrite {
		s.recordWriteAudit(client, msg, result)
	}
	s.hub.Send(client, coordinator.CommandResultMessage(result))
}

func (s *Server) handleMessage(ctx context.Context, client *transport.ClientConn, msg map[string]any, admitted func()) {
	defer admitted()
	inbound, err := protocol.DecodeMap(msg)
	if err != nil {
		s.rejectMessage(client, admitted, protocol.DecodeFailureResponse(msg))
		return
	}
	scope, knownAction := protocol.ScopeFor(inbound)
	if !knownAction {
		s.rejectMessage(client, admitted, protocol.ErrorResponse(inbound.RequestID, protocol.NewApiError(
			protocol.ErrorUnknownAction,
			map[string]any{"operation": inbound.Type},
		)))
		return
	}
	if !protocol.Compatible(inbound) {
		s.rejectMessage(client, admitted, protocol.IncompatibleResponse(inbound))
		return
	}
	requestedSessionID := scope.ServerSessionID
	if scope.Target != nil {
		if requestedSessionID != "" && scope.Target.ServerSessionID != "" && requestedSessionID != scope.Target.ServerSessionID {
			s.rejectMessage(client, admitted, protocol.ErrorResponse(inbound.RequestID, protocol.NewApiError(
				protocol.ErrorInvalidRequest,
				map[string]any{"field": "server_session_id"},
			)))
			return
		}
		if requestedSessionID == "" {
			requestedSessionID = scope.Target.ServerSessionID
		}
	}
	if requestedSessionID != "" && requestedSessionID != protocol.PrimaryServerSessionID {
		s.rejectMessage(client, admitted, protocol.ErrorResponse(inbound.RequestID, protocol.NewApiError(
			protocol.ErrorInvalidRequest,
			map[string]any{"field": "server_session_id"},
		)))
		return
	}
	if authorizationErr := s.authorizeDeviceAction(client, scope.Action, inbound.DeviceID); authorizationErr != nil {
		s.rejectMessage(client, admitted, protocol.ErrorResponse(inbound.RequestID, *authorizationErr))
		return
	}
	identity, authenticated := client.Identity()
	if targetErr := validateExactPaneTarget(s.state, inbound, authenticated); targetErr != nil {
		s.rejectMessage(client, admitted, protocol.ErrorResponse(inbound.RequestID, *targetErr))
		return
	}
	action := scope.Action.Operation
	coordinated := scope.Action.Coordinated
	auditedWrite := scope.Action.Audited
	if auditedWrite {
		s.recordWriteAudit(client, msg, nil)
	}
	if !coordinated {
		admitted()
	}

	commandCtx := herdr.WithDispatchCheck(client.Context(), func() error {
		if err := client.Context().Err(); err != nil {
			return err
		}
		if s.authorizeDeviceAction(client, scope.Action, inbound.DeviceID) != nil {
			return errors.New("device is no longer authorized")
		}
		if validateExactPaneTarget(s.state, inbound, authenticated) != nil {
			return coordinator.ErrPaneReplaced
		}
		return nil
	})
	switch action {
	case "get_pi_integration", "set_pi_integration":
		s.handlePiIntegration(commandCtx, client, inbound, msg)
	case "get_pi_prompt_capability", "send_pi_prompt_unconfirmed":
		result := s.handlePiPrompt(commandCtx, client, inbound, msg, admitted)
		s.sendResult(client, msg, auditedWrite, result)
	case "check_update", "install_update", "deploy_app_update":
		s.handleUpdate(ctx, client, inbound)
	case "lease_pane_size", "release_pane_size":
		s.handlePaneSizeCommand(client, inbound)
	case "read_pane":
		s.handleReadPane(ctx, client, inbound, msg)
	case "watch_pane":
		s.startPaneWatch(client, msg)
	case "unwatch_pane":
		s.stopPaneWatch(client.ID(), inbound.PaneID)
	case "pane_applied":
		s.handlePaneApplied(client, msg)
	case "get_conversation_history":
		s.handleConversationHistory(client, inbound)
	case "get_conversation_updates":
		s.handleConversationUpdates(client, inbound)
	case "unwatch_conversation":
		reply := s.reply(client, inbound.RequestID, action, inbound.PaneID)
		if inbound.SubscriptionID == "" {
			reply.fail("Conversation subscription is required")
			break
		}
		s.stopConversationFeed(client.ID(), inbound.SubscriptionID)
		reply.complete(nil)
	case "device_list":
		s.handleDeviceList(client, inbound, action, identity, authenticated)
	case "create_device_invitation":
		s.handleCreateDeviceInvitation(client, inbound, msg, action, identity, authenticated)
	case "rename_device":
		s.handleRenameDevice(client, inbound, msg, action)
	case "revoke_device":
		s.handleRevokeDevice(client, inbound, msg, action)
	case "reset_devices":
		s.handleResetDevices(client, inbound, msg, action, identity, authenticated)
	case "get_activity":
		s.handleGetActivity(client, msg)
	case "clear_activities":
		requestID, _ := msg["request_id"].(string)
		s.dispatcher.HandleClearActivities(requestID, func(result *coordinator.CommandResult) {
			s.hub.Send(client, coordinator.CommandResultMessage(result))
		})
	case "upload_begin":
		s.handleUploadBegin(client, inbound.RequestID, msg)
	case "upload_chunk":
		s.handleUploadChunk(client, inbound.RequestID, msg)
	case "upload_finish":
		s.handleUploadFinish(client, inbound.RequestID, msg)
	case "upload_cancel":
		s.handleUploadCancel(client, inbound.RequestID, msg)
	case "workspace_create", "workspace_rename", "workspace_reorder", "workspace_close",
		"worktree_list", "worktree_create", "worktree_open", "worktree_remove":
		s.handleTopology(commandCtx, client, inbound, msg, action, auditedWrite, admitted)
	case "list_directories":
		path, _ := msg["path"].(string)
		listing := fsutil.ListDirectories(path, s.home)
		s.reply(client, inbound.RequestID, "list_directories", "").complete(listing)
	case "list_slash_commands":
		s.handleListSlashCommands(client, inbound, msg)
	case "workspace_tree", "workspace_file", "workspace_git_status", "workspace_git_diff":
		s.handleWorkspaceInspect(client, inbound)
	case "copy_agent_response":
		paneID, _ := msg["pane_id"].(string)
		s.copyAgentResponse(client, inbound.RequestID, paneID)
	case "cancel_speech":
		speechRequestID, _ := msg["speech_request_id"].(string)
		s.cancelSpeech(client.ID(), speechRequestID)
	case "speak_text":
		speechRequestID, _ := msg["speech_request_id"].(string)
		text, _ := msg["text"].(string)
		language, _ := msg["language"].(string)
		s.speakText(client, inbound.RequestID, speechRequestID, text, language)
	case "speech_voices_list":
		s.reply(client, inbound.RequestID, action, "").complete(s.speechVoicePayload(nil))
	case "speech_voice_install", "speech_voice_remove":
		language, _ := msg["language"].(string)
		s.changeSpeechVoice(client, inbound.RequestID, action, language)
	case "qr_code":
		s.handleQRCode(client, inbound, msg, action)
	case "push_policy_get":
		s.handlePushPolicyGet(client, identity)
	case "push_policy_set":
		s.handlePushPolicySet(client, inbound, identity)
	case "push_snooze":
		s.handlePushSnooze(client, inbound, identity)
	case "push_viewed_pane":
		s.handlePushViewedPane(client, inbound, identity)
	case "push_open_ref":
		s.handlePushOpenRef(client, inbound, action, identity)
	case "push_test_device":
		s.handlePushTestDevice(ctx, client, identity)
	case "push_subscribe":
		s.handlePushSubscribe(client, inbound, identity)
	case "push_unsubscribe":
		s.handlePushUnsubscribe(client, inbound, identity)
	case "register_app_origin":
		if err := s.storePhoneAppOrigin(inbound.Origin); err != nil {
			s.recordSafeError("phone app origin was not stored", err)
			s.logger.Warn("phone app origin was not stored", "error", err)
		}
	case "refresh_agents":
		s.requestAgentRefresh(client)
	case "webrtc_offer", "webrtc_ice", "webrtc_close":
		s.handleWebRTCSignal(commandCtx, client, action, inbound.RequestID, msg)
	default:
		s.handleDispatched(commandCtx, client, inbound, msg, action, coordinated, auditedWrite, admitted)
	}
}

func (s *Server) handleDeviceList(client *transport.ClientConn, inbound protocol.Inbound, action string, identity transport.AuthenticatedIdentity, authenticated bool) {
	reply := s.reply(client, inbound.RequestID, action, "")
	if s.deviceAuth == nil || !authenticated {
		reply.fail(deviceManagementUnavailable)
		return
	}
	reply.complete(map[string]any{
		"devices":           activeDeviceCredentials(s.deviceAuth, identity.CredentialID),
		"current_device_id": identity.DeviceID,
		"role":              identity.Role,
	})
}

func (s *Server) handleCreateDeviceInvitation(client *transport.ClientConn, inbound protocol.Inbound, msg map[string]any, action string, identity transport.AuthenticatedIdentity, authenticated bool) {
	reply := s.auditedReply(client, msg, inbound.RequestID, action, "")
	if s.deviceAuth == nil || !authenticated {
		reply.fail(deviceManagementUnavailable)
		return
	}
	invitation, invitationErr := s.deviceAuth.CreateInvitation(inbound.Name, deviceauth.Role(inbound.Role), identity.Locale)
	if invitationErr != nil {
		reply.fail(invitationErr.Error())
		return
	}
	reply.complete(map[string]any{"invitation": invitation})
}

func (s *Server) handleRenameDevice(client *transport.ClientConn, inbound protocol.Inbound, msg map[string]any, action string) {
	reply := s.auditedReply(client, msg, inbound.RequestID, action, "")
	if s.deviceAuth == nil {
		reply.fail(deviceManagementUnavailable)
		return
	}
	credentialID, found := s.credentialIDOrFail(reply, inbound.DeviceID)
	if !found {
		return
	}
	credential, renameErr := s.deviceAuth.RenameCredential(credentialID, inbound.Name)
	if renameErr != nil {
		reply.fail(renameErr.Error())
		return
	}
	reply.complete(map[string]any{"device": credential})
}

func (s *Server) handleRevokeDevice(client *transport.ClientConn, inbound protocol.Inbound, msg map[string]any, action string) {
	reply := s.auditedReply(client, msg, inbound.RequestID, action, "")
	if s.deviceAuth == nil {
		reply.fail(deviceManagementUnavailable)
		return
	}
	credentialID, found := s.credentialIDOrFail(reply, inbound.DeviceID)
	if !found {
		return
	}
	credential, revokeErr := s.deviceAuth.RevokeCredential(credentialID)
	if revokeErr != nil {
		reply.fail(revokeErr.Error())
		return
	}
	s.forgetPushTest(credential.DeviceID)
	pushCleanupErr := s.pushM.RemoveDevice(credential.DeviceID)
	time.AfterFunc(250*time.Millisecond, func() {
		s.hub.DisconnectCredential(credential.CredentialID, credential.Version)
	})
	if pushCleanupErr != nil {
		reply.failWithData("Device revoked, but notification cleanup could not be persisted", map[string]any{"device": credential})
		return
	}
	reply.complete(map[string]any{"device": credential})
}

func (s *Server) handleResetDevices(client *transport.ClientConn, inbound protocol.Inbound, msg map[string]any, action string, identity transport.AuthenticatedIdentity, authenticated bool) {
	reply := s.auditedReply(client, msg, inbound.RequestID, action, "")
	if s.deviceAuth == nil || !authenticated {
		reply.fail(deviceManagementUnavailable)
		return
	}
	credentials := activeDeviceCredentials(s.deviceAuth, "")
	if resetErr := s.deviceAuth.ResetWithBootstrap([]byte(s.cfg.Token), s.hostname, identity.Locale); resetErr != nil {
		reply.fail(resetErr.Error())
		return
	}
	for _, credential := range credentials {
		s.forgetPushTest(credential.DeviceID)
	}
	var resetPushErr error
	for _, credential := range credentials {
		if pushErr := s.pushM.RemoveDevice(credential.DeviceID); pushErr != nil && resetPushErr == nil {
			resetPushErr = pushErr
		}
	}
	time.AfterFunc(250*time.Millisecond, func() {
		s.disconnectCredentials(credentials)
	})
	if resetPushErr != nil {
		reply.fail("Devices reset, but notification cleanup could not be persisted")
		return
	}
	reply.complete(nil)
}

func (s *Server) handlePushPolicyGet(client *transport.ClientConn, identity transport.AuthenticatedIdentity) {
	policy := s.pushM.Policy(identity.DeviceID, identity.Locale)
	s.hub.Send(client, map[string]any{"type": "push_policy", "policy": pushPolicyResponse(policy)})
}

func (s *Server) handlePushPolicySet(client *transport.ClientConn, inbound protocol.Inbound, identity transport.AuthenticatedIdentity) {
	reply := s.reply(client, inbound.RequestID, "push_policy_set", "")
	current := s.pushM.Policy(identity.DeviceID, identity.Locale)
	policy, policyErr := boundPushPolicy(inbound.Policy, identity.DeviceID, identity.Locale, current)
	if policyErr != nil || s.pushM.SetPolicy(policy) != nil {
		reply.fail("Notification policy was rejected")
		s.hub.Send(client, map[string]any{"type": "push_policy_result", "ok": false, "code": "push_invalid_policy"})
		return
	}
	data := map[string]any{"policy": pushPolicyResponse(policy)}
	reply.complete(data)
	s.hub.Send(client, map[string]any{
		"type": "push_policy_result", "ok": true, "policy": pushPolicyResponse(policy),
	})
}

func (s *Server) handlePushSnooze(client *transport.ClientConn, inbound protocol.Inbound, identity transport.AuthenticatedIdentity) {
	policy := s.pushM.Policy(identity.DeviceID, identity.Locale)
	policy.Snoozed = inbound.Snoozed
	policy.SnoozeUntil = time.Time{}
	if inbound.SnoozeUntil != "" {
		until, parseErr := time.Parse(time.RFC3339, inbound.SnoozeUntil)
		if parseErr != nil {
			s.hub.Send(client, map[string]any{"type": "push_policy_result", "ok": false, "code": "push_invalid_snooze"})
			return
		}
		policy.SnoozeUntil = until
	}
	if policyErr := s.pushM.SetPolicy(policy); policyErr != nil {
		s.hub.Send(client, map[string]any{"type": "push_policy_result", "ok": false, "code": "push_invalid_snooze"})
		return
	}
	s.hub.Send(client, map[string]any{
		"type": "push_policy_result", "ok": true, "policy": pushPolicyResponse(policy),
	})
}

func (s *Server) handlePushViewedPane(client *transport.ClientConn, inbound protocol.Inbound, identity transport.AuthenticatedIdentity) {
	var target *protocol.TargetRef
	if inbound.Visible && inbound.Unlocked && inbound.Target != nil && s.pushTargetCurrent(*inbound.Target) {
		copyTarget := *inbound.Target
		target = &copyTarget
	}
	s.pushM.SetViewedPane(identity.DeviceID, target)
	s.hub.Send(client, map[string]any{"type": "push_viewed_pane_result", "ok": true})
}

func (s *Server) handlePushOpenRef(client *transport.ClientConn, inbound protocol.Inbound, action string, identity transport.AuthenticatedIdentity) {
	claims, verifyErr := s.pushM.VerifyEventReference(inbound.EventRef, time.Now().UTC())
	if verifyErr != nil || claims.Key.DeviceID != identity.DeviceID || !s.pushTargetCurrent(claims.Key.Target()) {
		s.reply(client, inbound.RequestID, action, "").fail("Notification target is no longer available")
		return
	}
	s.reply(client, inbound.RequestID, action, claims.Key.PaneID).complete(map[string]any{
		"target": claims.Key.Target(), "event_id": claims.Key.EventID, "category": claims.Key.Category,
	})
}

func (s *Server) handlePushTestDevice(ctx context.Context, client *transport.ClientConn, identity transport.AuthenticatedIdentity) {
	now := time.Now().UTC()
	if !s.reservePushTest(identity.DeviceID, now) {
		s.hub.Send(client, map[string]any{"type": "push_test_result", "stage": "rate_limited"})
		return
	}
	eventID := "test"
	key := push.PushEventKey{DeviceID: identity.DeviceID, EventID: eventID, Category: push.CategoryTest}
	published, publishErr := s.pushM.Publish(ctx, push.PublishRequest{
		Key: key, Preview: push.PreviewHidden, CreatedAt: now, ExpiresAt: now.Add(time.Minute),
	})
	stage := "dropped"
	if publishErr == nil && published.Queued > 0 {
		// The manager's worker performs the network send. Keeping it out
		// of this command path avoids holding the global queue processor
		// while one client waits for a push service.
		stage = "queued"
	}
	s.hub.Send(client, map[string]any{"type": "push_test_result", "stage": stage})
}

func (s *Server) handlePushSubscribe(client *transport.ClientConn, inbound protocol.Inbound, identity transport.AuthenticatedIdentity) {
	ok := false
	var sub push.Subscription
	if json.Unmarshal(inbound.Subscription, &sub) == nil {
		sub.ClientID = inbound.ClientID
		sub.DeviceID = identity.DeviceID
		sub.Locale = identity.Locale
		// The authenticated connection does not currently carry a
		// trusted browser platform. Defaulting to no actions is safe;
		// never infer an actionable platform from client JSON.
		sub.Platform = push.PlatformOther
		sub.UserAgent = ""
		ok = s.pushM.Subscribe(sub, inbound.ReplaceEndpoints) == nil
	}
	s.hub.Send(client, map[string]any{"type": "push_subscribed", "ok": ok})
}

func (s *Server) handlePushUnsubscribe(client *transport.ClientConn, inbound protocol.Inbound, identity transport.AuthenticatedIdentity) {
	ok := s.pushM.UnsubscribeDevice(identity.DeviceID, inbound.Endpoints, inbound.ClientID) == nil
	s.hub.Send(client, map[string]any{"type": "push_unsubscribed", "ok": ok})
}

func (s *Server) handleReadPane(ctx context.Context, client *transport.ClientConn, inbound protocol.Inbound, msg map[string]any) {
	s.applyPaneReadLease(client.ID(), msg)
	s.stopPaneWatch(client.ID(), inbound.PaneID)
	resp := s.preparePaneResponse(msg, s.dispatcher.HandleReadPane(ctx, msg))
	resp["request_id"] = inbound.RequestID
	if inbound.Target != nil {
		resp["target"] = *inbound.Target
	}
	if unchanged := unchangedPaneResponse(msg, resp); unchanged != nil {
		s.hub.Send(client, unchanged)
		return
	}
	s.hub.Send(client, resp)
}

func (s *Server) handleGetActivity(client *transport.ClientConn, msg map[string]any) {
	limit := messageInt(msg["limit"], 500)
	if limit < 1 || limit > 500 {
		limit = 500
	}
	s.hub.Send(client, map[string]any{
		"type":       "activity_history",
		"activities": s.recentActivities(limit),
	})
}

func (s *Server) handleQRCode(client *transport.ClientConn, inbound protocol.Inbound, msg map[string]any, action string) {
	reply := s.reply(client, inbound.RequestID, action, "")
	value, _ := msg["text"].(string)
	size, packed, qrErr := setuphelper.PackedQR(value)
	if qrErr != nil {
		s.logger.Warn("qr encoding failed", "error", qrErr)
		reply.fail("This computer could not encode that QR code")
		return
	}
	reply.complete(map[string]any{
		"size":    size,
		"modules": base64.StdEncoding.EncodeToString(packed),
	})
}

func (s *Server) handleDispatched(commandCtx context.Context, client *transport.ClientConn, inbound protocol.Inbound, msg map[string]any, action string, coordinated bool, auditedWrite bool, admitted func()) {
	if err := s.expandPromptAttachmentReferences(action, msg, inbound.Target); err != nil {
		result := &coordinator.CommandResult{
			RequestID: inbound.RequestID,
			Action:    action,
			Phase:     "failed",
			Error:     "One or more attachments are no longer available for this agent",
			PaneID:    inbound.PaneID,
		}
		s.sendResult(client, msg, auditedWrite, result)
		return
	}
	var result *coordinator.CommandResult
	if coordinated {
		result = s.dispatcher.HandleAdmitted(commandCtx, msg, admitted)
	} else {
		result = s.dispatcher.Handle(commandCtx, msg)
	}
	s.sendResult(client, msg, auditedWrite, result)
}

func (s *Server) handleDisconnect(client *transport.ClientConn) {
	s.stopConversationFeed(client.ID(), "")
	s.stopPaneWatch(client.ID(), "")
	if identity, authenticated := client.Identity(); authenticated {
		s.pushM.SetViewedPane(identity.DeviceID, nil)
	}
	if s.hybrid != nil {
		s.hybrid.forgetClient(client.ID())
	}
	// A departing phone gives its panes back at once, without the
	// navigation grace. Restores that cannot finish now continue in the
	// manager's bounded cleanup.
	releaseCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = s.paneSizeM.ReleaseClient(releaseCtx, client.ID())
}

// Resize Session is a per-device opt-in. A PTY resize Herdr does not perform
// leaves its grid at the layout size while the application draws at the
// leased one, so the desktop and SSH views of a leased pane look garbled
// (issue 60); Herdr exposes no absolute rows/columns operation to avoid that.
// Apps up to 0.22.7 leased automatically whenever the relay advertised
// pane_size_lease, so only a request carrying opt_in is honoured: anything
// else fails before any Herdr, process or TTY access. Release stays an
// idempotent cleanup; immediate skips the navigation grace.
func (s *Server) handlePaneSizeCommand(client *transport.ClientConn, inbound protocol.Inbound) {
	reply := s.reply(client, inbound.RequestID, inbound.Type, inbound.PaneID)
	if inbound.Type == "release_pane_size" {
		if err := s.paneSizeM.Release(client.Context(), client.ID(), inbound.PaneID, inbound.Immediate); err != nil {
			reply.fail(panesize.PublicMessage(err))
			return
		}
		reply.complete(nil)
		return
	}
	if !inbound.OptIn || !s.paneSizeM.Available() {
		reply.fail(panesize.ErrUnavailable.Error())
		return
	}
	columns, rows, err := s.paneSizeM.Acquire(client.Context(), client.ID(), inbound.PaneID, inbound.Columns, inbound.Rows)
	if err != nil {
		s.logger.Debug("pane size lease failed", "pane_id", inbound.PaneID, "error", err)
		reply.fail(panesize.PublicMessage(err))
		return
	}
	reply.complete(map[string]any{"columns": columns, "rows": rows})
}

func (s *Server) drainLifecycleWork() {
	s.state.SetOnTransition(nil)
	s.transitionTasks.Stop()
	s.historyTasks.Stop()
	s.historyM.SaveAll()
}

func (s *Server) reconcileRecoveredPush(ctx context.Context, agents []*coordinator.AgentState) {
	if s.pushM == nil {
		return
	}
	s.pushReconcileMu.Lock()
	defer s.pushReconcileMu.Unlock()
	if s.pushReconciled {
		return
	}
	subscriptions := s.pushM.Subscriptions()
	current := make([]push.PushEventKey, 0)
	for _, key := range s.pushM.RecoveredKeys() {
		if key.Category == push.CategoryFinished &&
			s.pushTargetCurrent(key.Target()) &&
			s.state.CompletionCurrent(key.PaneID, key.InteractionRevision) {
			current = append(current, key)
		}
	}
	for _, agent := range agents {
		if agent == nil || agent.Status != "blocked" || agent.BlockedEventID == "" ||
			agent.TerminalID == "" || agent.AttentionKind == question.AttentionChat {
			continue
		}
		category := push.CategoryAttention
		if agent.AttentionKind == question.AttentionQuestion {
			category = push.CategoryQuestion
		}
		for _, subscription := range subscriptions {
			if subscription.DeviceID == "" {
				continue
			}
			current = append(current, push.PushEventKey{
				DeviceID:            subscription.DeviceID,
				ServerSessionID:     protocol.PrimaryServerSessionID,
				PaneID:              agent.PaneID,
				TerminalID:          agent.TerminalID,
				AgentSessionID:      agent.SessionID,
				Generation:          agent.Generation,
				EventID:             agent.BlockedEventID,
				InteractionRevision: s.state.AttentionRevision(agent.PaneID),
				Category:            category,
			})
		}
	}
	if err := s.pushM.Reconcile(ctx, current); err != nil {
		s.logger.Warn("recovered push queue reconciliation failed", "error", err)
		return
	}
	s.pushReconciled = true
}

func (s *Server) publishAgentPush(
	ctx context.Context,
	agent *coordinator.AgentState,
	eventID string,
	revision int64,
	category push.Category,
	preview push.PreviewMode,
) {
	if s.pushM == nil || agent == nil || agent.TerminalID == "" || eventID == "" {
		return
	}
	key := push.PushEventKey{
		ServerSessionID:     protocol.PrimaryServerSessionID,
		PaneID:              agent.PaneID,
		TerminalID:          agent.TerminalID,
		AgentSessionID:      agent.SessionID,
		Generation:          agent.Generation,
		EventID:             eventID,
		InteractionRevision: revision,
		Category:            category,
	}
	if err := s.pushM.ResolvePaneID(ctx, agent.PaneID, eventID); err != nil {
		s.logger.Warn("push notification retraction failed", "pane_id", agent.PaneID, "error", err)
	}
	if _, err := s.pushM.Publish(ctx, push.PublishRequest{
		Key: key, Preview: preview, ExpiresAt: time.Now().UTC().Add(5 * time.Minute),
	}); err != nil {
		s.logger.Warn("push notification queueing failed", "pane_id", agent.PaneID, "error", err)
	}
}

func (s *Server) handleTransition(
	parent context.Context,
	paneID, agent, project, status string,
	revision, transitionAt int64,
) {
	switch status {
	case "working":
		s.transitionWorking(parent, paneID, agent, project, revision, transitionAt)
	case "blocked":
		s.transitionBlocked(parent, paneID, agent, project, revision, transitionAt)
	default:
		s.transitionFinished(parent, paneID, agent, project, status, revision, transitionAt)
	}
}

func (s *Server) transitionWorking(
	parent context.Context,
	paneID, agent, project string,
	revision, transitionAt int64,
) {
	var session string
	if agentState, ok := s.state.Agent(paneID); ok && s.state.TransitionCurrent(paneID, "working", revision) {
		session = agentState.Session
	}
	ctx, cancel := context.WithTimeout(parent, 10*time.Second)
	defer cancel()

	if s.pushM != nil {
		if err := s.pushM.ResolvePaneID(ctx, paneID, ""); err != nil {
			s.logger.Warn("push notification retraction failed", "pane_id", paneID, "error", err)
		}
	}
	summary := agent + " started working"
	if agent == "" {
		summary = "Agent started working"
	}
	if s.dispatcher != nil {
		s.dispatcher.RecordTransitionActivity(coordinator.TransitionActivity{
			Kind:           "working",
			Status:         "working",
			Summary:        summary,
			PaneID:         paneID,
			ExpectedStatus: "working",
			Revision:       revision,
			Details:        map[string]any{"transition": "working", "transition_at": transitionAt},
			Agent:          agent,
			Project:        project,
			Host:           s.hostname,
			Session:        session,
		})
	}
}

func (s *Server) transitionBlocked(
	parent context.Context,
	paneID, agent, project string,
	revision, transitionAt int64,
) {
	agentState, agentExists := s.state.Agent(paneID)
	if !agentExists {
		return
	}
	session := agentState.Session
	blockedEventID := agentState.BlockedEventID
	blockedContentRevision := s.state.ContentRevision(paneID)
	paneGeneration, active := s.state.PaneSession(paneID)
	if !active || blockedEventID == "" {
		return
	}
	transitionCurrent := func() bool {
		return s.state.BlockedTransitionCurrent(paneID, blockedEventID, paneGeneration)
	}
	if !transitionCurrent() {
		return
	}
	ctx, cancel := context.WithTimeout(parent, 10*time.Second)
	defer cancel()

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
	if s.dispatcher != nil && !s.dispatcher.RecordTransitionActivity(coordinator.TransitionActivity{
		Kind:           activityKind,
		Status:         "attention",
		Summary:        command,
		PaneID:         paneID,
		ExpectedStatus: "blocked",
		Revision:       agentState.StateRevision,
		Details: map[string]any{
			"event_id":       eventID,
			"attention_kind": agentState.AttentionKind,
			"transition_at":  transitionAt,
		},
		Agent:   agent,
		Project: project,
		Host:    s.hostname,
		Session: session,
		Extract: agentState.Prompt,
		BlockedIdentity: &coordinator.BlockedTransitionIdentity{
			EventID:           blockedEventID,
			Generation:        paneGeneration,
			AttentionKind:     string(agentState.AttentionKind),
			AttentionRevision: classifiedAttentionRevision,
		},
	}) {
		return
	}
	if !classifiedCurrent() {
		return
	}
	category := push.CategoryAttention
	preview := push.PreviewHidden
	if agentState.AttentionKind == question.AttentionQuestion {
		category = push.CategoryQuestion
		preview = push.PreviewQuestion
	}
	if s.pushM != nil {
		s.publishAgentPush(ctx, agentState, eventID, classifiedAttentionRevision, category, preview)
	}
	if !classifiedCurrent() {
		return
	}
	s.broadcastBlockedAttention(agentState)
}

func (s *Server) transitionFinished(
	parent context.Context,
	paneID, agent, project, status string,
	revision, transitionAt int64,
) {
	agentState, agentExists := s.state.Agent(paneID)
	var session string
	var sessionID string
	conversationAgent := agent
	var conversationCwd string
	conversationProject := conversation.ProjectContext{}
	if agentExists {
		session = agentState.Session
		sessionID = agentState.SessionID
		conversationAgent = agentState.Agent
		conversationCwd = agentState.Cwd
		conversationProject = projectContextForAgent(agentState)
	}
	transitionCurrent := func() bool {
		return s.state.CompletionCurrent(paneID, revision)
	}
	if !transitionCurrent() {
		return
	}
	ctx, cancel := context.WithTimeout(parent, 10*time.Second)
	defer cancel()

	if !s.state.RegisterFinishedNotificationForTransition(paneID, revision) {
		return
	}
	extract := s.captureFinishedPane(ctx, paneID, conversationAgent, conversationCwd, sessionID, conversationProject)
	currentAgent, currentExists := s.state.Agent(paneID)
	if !transitionCurrent() || agentExists != currentExists ||
		(agentExists && !sameConversationTuple(agentState, currentAgent)) {
		return
	}
	s.publishFinished(ctx, finishedNotice{
		paneID:         paneID,
		agent:          agent,
		project:        project,
		session:        session,
		expectedStatus: status,
		revision:       revision,
		details:        map[string]any{"transition_at": transitionAt},
		extract:        extract,
		pushAgent:      currentAgent,
	}, transitionCurrent)
}

type finishedNotice struct {
	paneID         string
	agent          string
	project        string
	session        string
	expectedStatus string
	revision       int64
	details        map[string]any
	extract        string
	pushAgent      *coordinator.AgentState
}

func (s *Server) publishFinished(ctx context.Context, notice finishedNotice, current func() bool) {
	eventID := fmt.Sprintf("finished-%d-%s", time.Now().UnixNano(), notice.paneID)
	notice.details["event_id"] = eventID
	summary := notice.agent + " completed"
	if notice.agent == "" {
		summary = "Agent completed"
	}
	if s.dispatcher != nil && !s.dispatcher.RecordTransitionActivity(coordinator.TransitionActivity{
		Kind:           "finished",
		Status:         "completed",
		Summary:        summary,
		PaneID:         notice.paneID,
		ExpectedStatus: notice.expectedStatus,
		Revision:       notice.revision,
		Details:        notice.details,
		Agent:          notice.agent,
		Project:        notice.project,
		Host:           s.hostname,
		Session:        notice.session,
		Extract:        notice.extract,
	}) {
		return
	}
	if !current() {
		return
	}
	if s.pushM == nil {
		return
	}
	if notice.pushAgent != nil {
		s.publishAgentPush(ctx, notice.pushAgent, eventID, notice.revision, push.CategoryFinished, push.PreviewBrief)
		return
	}
	if err := s.pushM.ResolvePaneID(ctx, notice.paneID, ""); err != nil {
		s.logger.Warn("push notification retraction failed", "pane_id", notice.paneID, "error", err)
	}
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
		setAgentAttention(s.state, agent, unknownPaneClassification())
		return
	}
	setAgentAttention(s.state, agent, classification)
}

func unknownPaneClassification() question.Classification {
	return question.Classification{
		Kind:   question.AttentionUnknown,
		Prompt: "Agent needs inspection",
	}
}

func classifyBlockedTransition(
	ctx context.Context,
	agent string,
	read func(context.Context) (string, error),
) (question.Classification, error) {
	classification := unknownPaneClassification()
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
	agent.ApprovalFingerprint = ""
	agent.Interaction = nil
	agent.InteractionID = ""
	switch classification.Kind {
	case question.AttentionApproval:
		agent.Options = append([]string(nil), classification.Options...)
		agent.ApprovalFingerprint = question.ApprovalFingerprint(classification)
	case question.AttentionQuestion:
		agent.Interaction = classification.Interaction
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
		Kind:             agent.AttentionKind,
		Prompt:           agent.Prompt,
		Command:          agent.Command,
		Options:          append([]string(nil), agent.Options...),
		ApprovalIdentity: agent.ApprovalFingerprint,
		Interaction:      agent.Interaction,
	}
}

func (s *Server) broadcastBlockedAttention(agent *coordinator.AgentState) {
	if agent == nil || s.transitionBroadcast == nil {
		return
	}
	message := map[string]any{
		"type":                 "blocked",
		"pane_id":              agent.PaneID,
		"raw_pane_id":          agent.RawPaneID,
		"terminal_id":          agent.TerminalID,
		"tab_id":               agent.TabID,
		"tab_label":            agent.TabLabel,
		"tab_number":           agent.TabNumber,
		"workspace_id":         agent.WorkspaceID,
		"agent":                agent.Agent,
		"name":                 agent.Name,
		"status":               "blocked",
		"cwd":                  agent.Cwd,
		"project":              agent.Project,
		"host":                 agent.Host,
		"session":              agent.Session,
		"session_name":         agent.SessionName,
		"server_session_id":    protocol.PrimaryServerSessionID,
		"generation":           s.state.Generation(agent.PaneID),
		"agent_session_id":     agent.SessionID,
		"updated_at":           agent.UpdatedAt,
		"event_id":             agent.BlockedEventID,
		"attention_kind":       agent.AttentionKind,
		"prompt":               agent.Prompt,
		"command":              agent.Command,
		"options":              agent.Options,
		"approval_fingerprint": agent.ApprovalFingerprint,
		"interaction":          agent.Interaction,
		"interaction_id":       agent.InteractionID,
		"pane_revision":        agent.StateRevision,
	}
	if s.transitionBroadcast == s.hub {
		s.commitAgent(agent, message)
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
		!s.state.RegisterFinishedNotificationForTransition(agent.PaneID, revision) {
		return
	}
	s.publishFinished(ctx, finishedNotice{
		paneID:         agent.PaneID,
		agent:          agent.Agent,
		project:        agent.Project,
		session:        agent.Session,
		expectedStatus: "blocked",
		revision:       revision,
		details:        map[string]any{"attention_kind": agent.AttentionKind},
		extract:        agent.Prompt,
		pushAgent:      agent,
	}, current)
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

func (s *Server) handleConversationHistory(client *transport.ClientConn, inbound protocol.Inbound) {
	action := "get_conversation_history"
	reply := s.reply(client, inbound.RequestID, action, inbound.PaneID)
	agent, exists := s.state.Agent(inbound.PaneID)
	if !exists {
		reply.fail("Agent is unavailable")
		return
	}
	generation := s.state.Generation(inbound.PaneID)
	browser := s.conversationB
	if browser == nil || browser.Reader() != s.conversationM {
		s.logger.Warn("conversation browser is unavailable", "pane_id", inbound.PaneID)
		reply.fail("Conversation history could not be read")
		return
	}
	if !s.kimiSelectionCurrent(client.Context(), agent) {
		reply.fail("Kimi session changed")
		return
	}
	var page conversation.BrowsePage
	var historyErr error
	if inbound.Observe && supportsConversationActivity(agent.Agent) {
		page, historyErr = s.conversationFeedPage(client, inbound, agent)
	} else {
		page, historyErr = browser.ReadPage(client.Context(), conversation.BrowseRequest{
			Scope: conversationScope(agent), Cursor: inbound.Cursor, Limit: inbound.Limit, Retry: inbound.Retry,
		})
	}
	if historyErr != nil {
		s.logger.Warn("conversation history read failed", "pane_id", inbound.PaneID)
		reply.fail("Conversation history could not be read")
		return
	}
	if s.conversationHistoryReadObserver != nil {
		s.conversationHistoryReadObserver()
	}
	current, currentExists := s.state.Agent(inbound.PaneID)
	if !currentExists || s.state.Generation(inbound.PaneID) != generation ||
		!sameConversationTuple(agent, current) || !s.kimiSelectionCurrent(client.Context(), agent) ||
		s.authorizeDeviceAction(client, protocol.ActionMetadata{Operation: action, Class: protocol.ActionReadOnly}, "") != nil {
		reply.fail("Agent changed while conversation history was loading")
		return
	}
	reply.complete(page)
}

func (s *Server) latestConversationResponse(agent, sessionID string, project conversation.ProjectContext) string {
	if s.conversationM == nil || conversation.IsKimi(agent) || strings.TrimSpace(sessionID) == "" || !conversation.Supported(agent) {
		return ""
	}
	page, err := s.conversationM.ReadWithProject(agent, project, sessionID, "", 1)
	if err != nil || !page.Available || len(page.Entries) == 0 {
		return ""
	}
	entry := page.Entries[len(page.Entries)-1]
	if entry.Role != "assistant" || strings.TrimSpace(entry.Text) == "" {
		return ""
	}
	return entry.Text
}

func (s *Server) captureFinishedPane(ctx context.Context, paneID, agent, cwd, sessionID string, project conversation.ProjectContext) string {
	if conversation.IsKimi(agent) {
		current, exists := s.state.Agent(paneID)
		if !exists || current.Agent != agent || current.SessionID != sessionID || current.Cwd != cwd || s.conversationM == nil {
			return ""
		}
		page, err := s.readAgentConversation(ctx, current, 1)
		if err != nil || !page.Available {
			return ""
		}
		if len(page.Entries) > 0 {
			entry := page.Entries[len(page.Entries)-1]
			if entry.Role == "assistant" && strings.TrimSpace(entry.Text) != "" {
				return entry.Text
			}
		}
	}
	if response := s.latestConversationResponse(agent, sessionID, project); response != "" {
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

func locatedAgentDir(home, agent string, location conversation.Location) string {
	return agentroots.AgentDirForSession(home, agent, location.Path)
}

func sameConversationTuple(left, right *coordinator.AgentState) bool {
	if left == nil || right == nil {
		return false
	}
	leftProject := projectContextForAgent(left)
	rightProject := projectContextForAgent(right)
	return left.Agent == right.Agent &&
		left.Cwd == right.Cwd &&
		leftProject == rightProject &&
		left.SessionID == right.SessionID &&
		left.ConversationProcess == right.ConversationProcess &&
		left.ConversationSource == right.ConversationSource &&
		left.ConversationReason == right.ConversationReason &&
		left.ConversationInferred == right.ConversationInferred
}
func isClaudeLike(agent string) bool {
	kind := agentkind.Of(agent)
	return kind == agentkind.Claude || kind == agentkind.Qoder
}

func (s *Server) pruneUploads(ctx context.Context) {
	if s.uploadM == nil {
		return
	}
	s.uploadM.Cleanup()
	ticker := time.NewTicker(time.Hour)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if n := s.uploadM.Cleanup(); n > 0 {
				s.logger.Info("pruned expired uploads", "count", n)
			}
		}
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

func (s *Server) observeInventoryPublication(phase string) {
	if s.inventoryPublicationObserver != nil {
		s.inventoryPublicationObserver(phase)
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

// armBootstrapLoop re-arms the one-use bootstrap invitation on SIGUSR1. The
// setup scripts send it right before printing the setup link, so every printed
// QR pairs one more phone without revoking the devices already enrolled. Only a
// local process can signal the relay, which is the trust the printed key needs.
func (s *Server) armBootstrapLoop(ctx context.Context, signals <-chan os.Signal) {
	for {
		select {
		case <-ctx.Done():
			return
		case <-signals:
			s.logger.Info("bootstrap invitation re-arm requested", "outcome", s.armBootstrapInvitation())
		}
	}
}

func (s *Server) armBootstrapInvitation() string {
	if s.deviceAuth == nil {
		return "no relay key configured"
	}
	if err := s.deviceAuth.ArmBootstrapInvitation([]byte(s.cfg.Token), s.hostname, "en"); err != nil {
		s.recordSafeError("bootstrap invitation re-arm failed", err)
		return err.Error()
	}
	return "armed for one more device"
}

func (s *Server) pidFilePath() string {
	return filepath.Join(s.cfg.RuntimeDir, "relay.pid")
}

func (s *Server) writePIDFile() error {
	if s.cfg.RuntimeDir == "" {
		return errors.New("runtime directory is not configured")
	}
	if err := os.MkdirAll(s.cfg.RuntimeDir, 0o700); err != nil {
		return err
	}
	return os.WriteFile(s.pidFilePath(), []byte(strconv.Itoa(os.Getpid())+"\n"), 0o600)
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
	agent, _ := s.state.Agent(paneID)
	agentName := ""
	if agent != nil {
		agentName = agent.Agent
	}
	classification := question.Classify(content, agentName)
	response["attention_kind"] = classification.Kind
	response["prompt"] = classification.Prompt
	response["command"] = classification.Command
	response["options"] = classification.Options
	response["interaction"] = classification.Interaction
	if interaction := classification.Interaction; interaction != nil {
		if text := strings.TrimSpace(interaction.Other.Text); text != "" {
			s.state.RecordCustomAnswer(paneID, interaction.Question, text)
		}
		question.FillCustomAnswers(interaction, s.state.CustomAnswers(paneID))
	}
	if viewportOnly, _ := response["viewport_only"].(bool); viewportOnly &&
		s.paneSizeM != nil && s.paneSizeM.ResizedWithin(paneID, paneResizeSettleWindow) {
		// The agent re-renders its transcript after a size change and can
		// push a redrawn block into the scrollback for a while; the app must
		// not commit rows from frames read inside this window as history.
		response["resize_settling"] = true
	}
	if classification.Interaction != nil || !isClaudeLike(agentName) {
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
		"request_id":          response["request_id"],
		"pane_id":             paneID,
		"content_fingerprint": fingerprint,
		"target":              response["target"],
	}
}

func (s *Server) projectAgentResource(agent *coordinator.AgentState) {
	if agent == nil {
		return
	}
	agent.ServerSessionID = protocol.PrimaryServerSessionID
}

func (s *Server) projectAgentResources(agents []*coordinator.AgentState) {
	for _, agent := range agents {
		s.projectAgentResource(agent)
	}
}

type committedInventory struct {
	status     map[string]any
	agents     []*coordinator.AgentState
	workspaces []herdr.Workspace
}

func (inventory committedInventory) messages() []any {
	return []any{
		inventoryStatusMessage(inventory.status),
		map[string]any{"type": "agents", "agents": inventory.agents},
		map[string]any{"type": "workspaces", "workspaces": inventory.workspaces},
	}
}

// publishCurrentInventory is the sole authoritative inventory writer. Fresh
// state is selected inside the Hub registration barrier, while the expensive
// reconciliation work remains after that barrier has been released.
func (s *Server) setInventoryPublisher(ctx context.Context) {
	if s.poller == nil {
		return
	}
	s.poller.SetOnInventoryChange(func() error {
		return s.publishCurrentInventory(ctx)
	})
}

func (s *Server) publishCurrentInventory(ctx context.Context) error {
	var sideEffectAgents []*coordinator.AgentState
	var runAgentSideEffects bool
	batchErr := s.hub.BroadcastBatchPrepared(func() ([]any, func(), error) {
		fresh := s.state.InventorySnapshot()
		s.observeInventoryPublication("publication")
		freshAgents := cloneAgents(fresh.Agents)
		s.projectAgentResources(freshAgents)

		s.stateViewMu.RLock()
		previousStatus := cloneStringMap(s.inventoryView)
		previousAgents := cloneAgents(s.agentView)
		previousWorkspaces := cloneWorkspaces(s.workspaceView)
		s.stateViewMu.RUnlock()

		mergedAgents := mergeAgentSnapshot(previousAgents, freshAgents)
		statusChanged := inventoryStatusChanged(previousStatus, fresh.Status)
		readyRecovery := fresh.Status["state"] == "ready" && previousStatus["state"] != "ready"
		agentsChanged := !agentSnapshotsEqual(previousAgents, mergedAgents)
		workspacesChanged := !workspaceSnapshotsEqual(previousWorkspaces, fresh.Workspaces)
		sendAgents := agentsChanged || readyRecovery
		sendWorkspaces := workspacesChanged || readyRecovery
		if fresh.Status["state"] == "ready" && (agentsChanged || readyRecovery) {
			runAgentSideEffects = true
			sideEffectAgents = cloneAgents(mergedAgents)
		}

		messages := make([]any, 0, 3)
		if statusChanged {
			messages = append(messages, inventoryStatusMessage(fresh.Status))
		}
		if sendAgents {
			messages = append(messages, map[string]any{"type": "agents", "agents": cloneAgents(mergedAgents)})
		}
		if sendWorkspaces {
			messages = append(messages, map[string]any{"type": "workspaces", "workspaces": cloneWorkspaces(fresh.Workspaces)})
		}

		commit := func() {
			s.stateViewMu.Lock()
			// Timestamps are metadata, not a wire-change trigger. Refreshing the
			// cached status here still makes a later handshake authoritative.
			s.inventoryView = cloneStringMap(fresh.Status)
			if sendAgents {
				s.agentView = cloneAgents(mergedAgents)
			}
			if sendWorkspaces {
				s.workspaceView = cloneWorkspaces(fresh.Workspaces)
			}
			s.stateViewMu.Unlock()
		}
		return messages, commit, nil
	})

	// A pending explicit refresh is a request for completion, not a request for
	// an agent diff. Drain it after every publication attempt, including an
	// unchanged state and an encode failure.
	s.sendRequestedAgentRefreshes()
	if batchErr != nil {
		return batchErr
	}
	if runAgentSideEffects {
		s.reconcileRecoveredPush(ctx, sideEffectAgents)
		s.syncHistoryPanes(sideEffectAgents)
		active := make(map[string]bool, len(sideEffectAgents))
		for _, agent := range sideEffectAgents {
			active[agent.PaneID] = true
			if isClaudeLike(agent.Agent) && (agent.Status == "working" || agent.Status == "blocked") {
				s.scheduleHistoryCapture(ctx, agent.PaneID)
			}
		}
		if s.dispatcher != nil {
			s.dispatcher.PruneSlots(active)
		}
	}
	return nil
}

func inventoryStatusChanged(previous, current map[string]any) bool {
	for _, key := range []string{"state", "error_code", "message", "stale"} {
		if previous[key] != current[key] {
			return true
		}
	}
	return false
}

func agentSnapshotsEqual(left, right []*coordinator.AgentState) bool {
	left = cloneAgents(left)
	right = cloneAgents(right)
	for _, agents := range [][]*coordinator.AgentState{left, right} {
		for _, agent := range agents {
			agent.StateRevision = 0
		}
	}
	leftData, leftErr := json.Marshal(left)
	rightData, rightErr := json.Marshal(right)
	return leftErr == nil && rightErr == nil && bytes.Equal(leftData, rightData)
}

func workspaceSnapshotsEqual(left, right []herdr.Workspace) bool {
	if len(left) == 0 && len(right) == 0 {
		return true
	}
	leftData, leftErr := json.Marshal(left)
	rightData, rightErr := json.Marshal(right)
	return leftErr == nil && rightErr == nil && bytes.Equal(leftData, rightData)
}

func cloneWorkspaces(workspaces []herdr.Workspace) []herdr.Workspace {
	result := make([]herdr.Workspace, len(workspaces))
	copy(result, workspaces)
	for index := range result {
		if result[index].Worktree != nil {
			worktree := *result[index].Worktree
			result[index].Worktree = &worktree
		}
	}
	return result
}

func (s *Server) committedInventorySnapshot() committedInventory {
	s.stateViewMu.RLock()
	result := committedInventory{
		status:     cloneStringMap(s.inventoryView),
		agents:     cloneAgents(s.agentView),
		workspaces: cloneWorkspaces(s.workspaceView),
	}
	s.stateViewMu.RUnlock()
	s.projectAgentResources(result.agents)
	return result
}

func (s *Server) commitAgent(agent *coordinator.AgentState, message map[string]any) {
	paneID := agent.PaneID
	if paneID == "" {
		s.recordSafeError("agent update broadcast was malformed", nil)
		return
	}
	message["server_session_id"] = protocol.PrimaryServerSessionID
	message["generation"] = s.state.Generation(paneID)
	if current, exists := s.state.Agent(paneID); exists {
		message["terminal_id"] = current.TerminalID
		message["agent_session_id"] = current.SessionID
	}
	s.hub.BroadcastPrepared(message, func() {
		s.stateViewMu.Lock()
		agents := cloneAgents(s.agentView)
		found := false
		for _, view := range agents {
			if view.PaneID != paneID {
				continue
			}
			if agent.StateRevision >= view.StateRevision {
				applyCommittedAgent(view, agent)
			}
			found = true
			break
		}
		if !found {
			if current, ok := s.state.Agent(paneID); ok {
				applyCommittedAgent(current, agent)
				agents = append(agents, current)
			}
		}
		s.agentView = agents
		s.stateViewMu.Unlock()
	})
}

func (s *Server) broadcastCommitted(message any) {
	if update, ok := message.(coordinator.AgentUpdate); ok {
		s.commitAgent(update.Agent, update.Message)
		return
	}
	envelope, ok := message.(map[string]any)
	if !ok {
		s.hub.Broadcast(message)
		return
	}
	messageType, _ := envelope["type"].(string)
	switch messageType {
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

func cloneAgents(agents []*coordinator.AgentState) []*coordinator.AgentState {
	result := make([]*coordinator.AgentState, 0, len(agents))
	for _, agent := range agents {
		if agent == nil {
			continue
		}
		result = append(result, agent.Clone())
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
			merged[index] = existing.Clone()
		}
	}
	return merged
}

func cloneStringMap(values map[string]any) map[string]any {
	result := make(map[string]any, len(values))
	for key, value := range values {
		result[key] = value
	}
	return result
}

func applyCommittedAgent(view, committed *coordinator.AgentState) {
	view.RawPaneID = committed.RawPaneID
	view.Status = committed.Status
	view.Agent = committed.Agent
	view.TabID = committed.TabID
	view.TabLabel = committed.TabLabel
	view.TabNumber = committed.TabNumber
	view.WorkspaceID = committed.WorkspaceID
	view.Cwd = committed.Cwd
	view.Project = committed.Project
	view.Host = committed.Host
	view.Session = committed.Session
	view.SessionName = committed.SessionName
	view.UpdatedAt = committed.UpdatedAt
	view.BlockedEventID = committed.BlockedEventID
	view.AttentionKind = committed.AttentionKind
	view.Prompt = committed.Prompt
	view.Command = committed.Command
	view.Options = append([]string(nil), committed.Options...)
	view.ApprovalFingerprint = committed.ApprovalFingerprint
	view.Interaction = committed.Clone().Interaction
	view.StateRevision = committed.StateRevision
	if view.Status != "blocked" {
		view.AttentionKind = ""
		view.Prompt = ""
		view.Command = ""
		view.Options = nil
		view.ApprovalFingerprint = ""
		view.Interaction = nil
		return
	}
	if view.AttentionKind != question.AttentionApproval {
		view.Options = nil
		view.ApprovalFingerprint = ""
	}
	if view.AttentionKind != question.AttentionQuestion {
		view.Interaction = nil
	}
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
		project := conversation.ProjectContext{}
		if current := agents[entry.PaneID]; current != nil {
			if strings.TrimSpace(current.Agent) != "" {
				agentName = current.Agent
			}
			if strings.TrimSpace(current.SessionID) != "" {
				sessionID = current.SessionID
				project = projectContextForAgent(current)
			}
		}
		if agentName == "" || sessionID == "" || !conversation.Supported(agentName) {
			continue
		}
		if conversation.IsKimi(agentName) && agents[entry.PaneID] == nil {
			continue
		}
		var page conversation.Page
		if current := agents[entry.PaneID]; current != nil && conversation.IsKimi(current.Agent) {
			var err error
			page, err = s.readAgentConversation(context.Background(), current, 200)
			if err != nil || !page.Available {
				continue
			}
		} else {
			cacheKey := agentName + "\x00" + project.CWD + "\x00" + project.ForegroundCWD + "\x00" + sessionID
			var loaded bool
			page, loaded = pages[cacheKey]
			if !loaded {
				page, _ = s.conversationM.ReadWithProject(agentName, project, sessionID, "", 200)
				pages[cacheKey] = page
			}
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
