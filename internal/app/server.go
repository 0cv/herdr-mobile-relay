package app

import (
	"context"
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
	"github.com/0cv/herdr-mobile-relay/internal/hc"
	"github.com/0cv/herdr-mobile-relay/internal/herdr"
	"github.com/0cv/herdr-mobile-relay/internal/history"
	"github.com/0cv/herdr-mobile-relay/internal/panesize"
	"github.com/0cv/herdr-mobile-relay/internal/piintegration"
	"github.com/0cv/herdr-mobile-relay/internal/profiles"
	"github.com/0cv/herdr-mobile-relay/internal/push"
	"github.com/0cv/herdr-mobile-relay/internal/question"
	"github.com/0cv/herdr-mobile-relay/internal/session"
	"github.com/0cv/herdr-mobile-relay/internal/speech"
	"github.com/0cv/herdr-mobile-relay/internal/transport"
	relayupdate "github.com/0cv/herdr-mobile-relay/internal/update"
	"github.com/0cv/herdr-mobile-relay/internal/upload"
	"github.com/0cv/herdr-mobile-relay/internal/web"
)

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
	// Every other consumer of a pane's reported session (Reader.ReadLatest,
	// latestConversationResponse, the activity backfill path) TrimSpaces it
	// before deciding anything. Trimming once here, before it reaches
	// SessionID or the resolver, is what keeps a whitespace-only value from
	// looking like a real session id: untrimmed, it survives the `== ""`
	// checks below and can still reach the resolver's empty-id
	// sole-transcript heuristic, inventing a title over a conversation view
	// that Reader.ReadLatest reports as unavailable.
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

func (s *Server) drainLifecycleWork() {
	s.state.SetOnTransition(nil)
	s.transitionTasks.Stop()
	s.historyTasks.Stop()
	s.historyM.SaveAll()
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
