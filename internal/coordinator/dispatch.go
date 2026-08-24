package coordinator

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/0cv/herdr-mobile-relay/internal/activity"
	"github.com/0cv/herdr-mobile-relay/internal/herdr"
	"github.com/0cv/herdr-mobile-relay/internal/profiles"
)

const (
	commandDeadline    = 12 * time.Second
	approvalDeadline   = 9 * time.Second
	questionDeadline   = 16 * time.Second
	agentStartDeadline = 40 * time.Second
	maxTabInsertIndex  = 10_000
	promptMaxChars     = 100000
	secretMaxRunes     = 256
)

type CommandResult struct {
	RequestID string `json:"request_id"`
	Action    string `json:"action"`
	OK        bool   `json:"ok"`
	Phase     string `json:"phase"`
	Error     string `json:"error,omitempty"`
	PaneID    string `json:"pane_id,omitempty"`
	Data      any    `json:"data,omitempty"`
	replayed  bool
}

type Dispatcher struct {
	herdr            *herdr.Client
	state            *State
	journal          *activity.Journal
	activityW        *activity.Worker
	activityOrder    sync.Mutex
	activitySequence atomic.Uint64
	activityFailures atomic.Uint64
	logger           *slog.Logger
	scheduler        *Scheduler
	profiles         *profiles.Resolver
	lifecycle        *Lifecycle
	broadcast        func(any)
	wakePoll         func()
	readMu           sync.Mutex
	topologyMu       sync.Mutex
	reads            map[string]*paneRead
	watcherMu        sync.Mutex
	watcherCtx       context.Context
	watcherCancel    context.CancelFunc
	watcherClosed    bool
	watcherWG        sync.WaitGroup

	// testGates is a deterministic pre-admission hook retained for the existing
	// package tests. Production never creates an entry and never takes a lock.
	testGatesMu sync.Mutex
	testGates   map[string]chan struct{}
}

type receiptContextKey struct{}
type admissionContextKey struct{}

type paneSessionContextKey struct{}

type paneSessionAdmission struct {
	generation  uint64
	active      bool
	allowAbsent bool
}

type paneRead struct {
	done chan struct{}
	read herdr.PaneRead
	err  error
}

func NewDispatcher(client *herdr.Client, state *State, journal *activity.Journal, logger *slog.Logger) *Dispatcher {
	watcherCtx, watcherCancel := context.WithCancel(context.Background())
	dispatcher := &Dispatcher{
		herdr:         client,
		state:         state,
		journal:       journal,
		logger:        logger,
		scheduler:     NewScheduler(defaultHerdrCapacity, logger),
		testGates:     make(map[string]chan struct{}),
		reads:         make(map[string]*paneRead),
		watcherCtx:    watcherCtx,
		watcherCancel: watcherCancel,
	}
	dispatcher.scheduler.SetGenerationCurrent(state.PaneSessionCurrent)
	if journal != nil {
		dispatcher.activityW = activity.NewWorker(journal)
	}
	return dispatcher
}

func (d *Dispatcher) SetProfiles(resolver *profiles.Resolver) {
	d.profiles = resolver
	d.lifecycle = NewLifecycle(d.herdr, resolver)
}

func (d *Dispatcher) CancelInflight() {
	d.scheduler.CancelInflight()
}

func (d *Dispatcher) Close(ctx context.Context) error {
	d.closeWatchers()
	err := d.scheduler.Close(ctx)
	if d.activityW != nil {
		if workerErr := d.activityW.Close(ctx); err == nil {
			err = workerErr
		}
	}
	return err
}

func (d *Dispatcher) startWatcher(work func(context.Context)) bool {
	d.watcherMu.Lock()
	if d.watcherClosed {
		d.watcherMu.Unlock()
		return false
	}
	d.watcherWG.Add(1)
	ctx := d.watcherCtx
	d.watcherMu.Unlock()

	go func() {
		defer d.watcherWG.Done()
		work(ctx)
	}()
	return true
}

func (d *Dispatcher) closeWatchers() {
	d.watcherMu.Lock()
	if !d.watcherClosed {
		d.watcherClosed = true
		d.watcherCancel()
	}
	d.watcherMu.Unlock()
	d.watcherWG.Wait()
}

func (d *Dispatcher) Metrics() SchedulerMetrics {
	return d.scheduler.Metrics()
}

func (d *Dispatcher) ActivityFailures() uint64 {
	return d.activityFailures.Load()
}

func (d *Dispatcher) paneSlot(paneID string) chan struct{} {
	d.testGatesMu.Lock()
	defer d.testGatesMu.Unlock()
	gate := d.testGates[paneID]
	if gate == nil {
		gate = make(chan struct{}, 1)
		d.testGates[paneID] = gate
	}
	return gate
}

func (d *Dispatcher) waitTestGate(ctx context.Context, paneID string, generation int64) *CommandResult {
	d.testGatesMu.Lock()
	gate := d.testGates[paneID]
	d.testGatesMu.Unlock()
	if gate == nil {
		return nil
	}
	select {
	case gate <- struct{}{}:
		defer func() { <-gate }()
	case <-ctx.Done():
		return d.fail("", "", paneID, "Timed out waiting for the pane")
	}
	if d.state.Generation(paneID) != generation {
		return d.fail("", "", paneID, "pane session was replaced")
	}
	return nil
}

func (d *Dispatcher) PruneSlots(active map[string]bool) {
	generations := make(map[string]uint64, len(active))
	for paneID := range active {
		generations[paneID] = uint64(d.state.Generation(paneID))
	}
	d.ApplyTopology(active, generations)
}

func (d *Dispatcher) ApplyTopology(active map[string]bool, generations map[string]uint64) {
	d.scheduler.ApplyTopology(active, generations)
	d.testGatesMu.Lock()
	defer d.testGatesMu.Unlock()
	for paneID, gate := range d.testGates {
		if !active[paneID] && len(gate) == 0 {
			delete(d.testGates, paneID)
		}
	}
}

func (d *Dispatcher) paneSessionCurrent(token WorkerToken, requestID, action string) *CommandResult {
	if d.paneSessionError(token) == nil {
		return nil
	}
	return d.fail(requestID, action, token.PaneID, ErrPaneReplaced.Error())
}

func (d *Dispatcher) paneSessionError(token WorkerToken) error {
	generation, active := d.state.PaneSession(token.PaneID)
	if uint64(generation) != token.Generation || (!active && !token.AllowAbsent) {
		return ErrPaneReplaced
	}
	return nil
}

func (d *Dispatcher) SetBroadcast(fn func(any)) {
	d.broadcast = fn
}

func (d *Dispatcher) SetWakePoll(fn func()) {
	d.wakePoll = fn
}

func (d *Dispatcher) Handle(ctx context.Context, message map[string]any) *CommandResult {
	receivedAt := time.Now()
	if raw, ok := message["_server_received_at"].(string); ok {
		if parsed, err := time.Parse(time.RFC3339Nano, raw); err == nil {
			receivedAt = parsed
		}
	}
	if sequence := uint64Value(message["_server_sequence"]); sequence != 0 {
		ctx = context.WithValue(ctx, receiptContextKey{}, CommandID(sequence))
	}
	action := stringValue(message, "action")
	if action == "" {
		action = stringValue(message, "type")
	}
	requestID := stringValue(message, "request_id")
	if requestID == "" {
		requestID = fmt.Sprintf("req-%d", receivedAt.UnixNano())
	}
	paneID := stringValue(message, "pane_id")
	if paneID != "" {
		generation, active := d.state.PaneSession(paneID)
		ctx = context.WithValue(ctx, paneSessionContextKey{}, paneSessionAdmission{
			generation: uint64(generation),
			active:     active,
		})
	}

	d.logger.Debug("dispatching command", "action", action, "request_id", requestID, "pane_id", paneID)

	switch action {
	case "submit_prompt", "prompt":
		return d.handlePrompt(ctx, receivedAt, requestID, paneID, message)
	case "send_keys", "keys":
		return d.handleKeys(ctx, receivedAt, requestID, paneID, message)
	case "send_text", "text":
		return d.handleText(ctx, receivedAt, requestID, paneID, message)
	case "send_secret":
		return d.handleSecret(ctx, receivedAt, requestID, paneID, message)
	case "respond":
		return d.handleApproval(ctx, receivedAt, requestID, paneID, message)
	case "answer_question":
		return d.handleQuestion(ctx, receivedAt, requestID, paneID, message)
	case "navigate_question":
		return d.handleNavigateQuestion(ctx, receivedAt, requestID, paneID, message)
	case "clarify_question":
		return d.handleClarifyQuestion(ctx, receivedAt, requestID, paneID, message)
	case "agent_stop":
		return d.handleStop(ctx, receivedAt, requestID, paneID)
	case "agent_rename":
		return d.handleTabRename(ctx, receivedAt, requestID, paneID, message)
	case "tab_reorder":
		return d.handleTabReorder(ctx, receivedAt, requestID, paneID, message)
	case "acknowledge_pane":
		return d.handleAcknowledge(requestID, paneID)
	case "agent_start":
		return d.handleAgentStart(ctx, receivedAt, requestID, message)
	case "agent_clear", "agent_restart":
		return d.handleClear(ctx, receivedAt, requestID, paneID)
	default:
		return &CommandResult{RequestID: requestID, Action: action, OK: false, Phase: "failed", Error: "Unknown command"}
	}
}

func (d *Dispatcher) HandleAdmitted(
	ctx context.Context,
	message map[string]any,
	admitted func(),
) *CommandResult {
	var once sync.Once
	signal := func() {
		once.Do(admitted)
	}
	ctx = context.WithValue(ctx, admissionContextKey{}, signal)
	result := d.Handle(ctx, message)
	signal()
	return result
}

// HandleTopologyAdmitted mirrors HandleAdmitted for the workspace and
// worktree handlers, which are called directly rather than through Handle.
// The handler signals admission via admitTopology before it waits on
// topologyMu — the hub's global ordered ingress must not stay blocked for
// either the mutex wait or the Herdr command itself. The trailing signal
// covers handlers that return before reaching admission (validation
// failures, read-only paths).
func (d *Dispatcher) HandleTopologyAdmitted(
	ctx context.Context,
	admitted func(),
	handle func(context.Context) *CommandResult,
) *CommandResult {
	var once sync.Once
	signal := func() {
		once.Do(admitted)
	}
	ctx = context.WithValue(ctx, admissionContextKey{}, signal)
	result := handle(ctx)
	signal()
	return result
}

func isQoderAgent(agent string) bool {
	switch strings.ToLower(strings.TrimSpace(agent)) {
	case "qoder", "qodercli":
		return true
	default:
		return false
	}
}

func (d *Dispatcher) handlePrompt(ctx context.Context, receivedAt time.Time, requestID, paneID string, message map[string]any) *CommandResult {
	text := stringValue(message, "text")
	if paneID == "" || text == "" {
		return d.fail(requestID, "prompt", paneID, "Text and agent are required")
	}
	if len([]rune(text)) > promptMaxChars {
		return d.fail(requestID, "prompt", paneID, "Prompt is longer than 100,000 characters")
	}
	generation := d.state.Generation(paneID)
	if stale := d.waitTestGate(ctx, paneID, generation); stale != nil {
		stale.RequestID, stale.Action = requestID, "prompt"
		return stale
	}
	requiresEnter := false
	if agent, ok := d.state.Agent(paneID); ok {
		requiresEnter = isQoderAgent(agent.Agent)
	}
	result := d.schedule(ctx, ScheduleOptions{
		Command: d.command(ctx, receivedAt, requestID, CommandPrompt, paneID, commandDeadline, text),
	}, EffectFunc(func(effectCtx context.Context, token WorkerToken) EffectResult {
		if stale := d.paneSessionCurrent(token, requestID, "prompt"); stale != nil {
			return EffectResult{Result: stale}
		}
		if !requiresEnter {
			if err := d.herdr.Prompt(effectCtx, paneID, text); err != nil {
				return EffectResult{Result: d.failErr(requestID, "prompt", paneID, err)}
			}
			return EffectResult{Result: completed(requestID, "prompt", paneID, nil)}
		}
		if err := d.herdr.SendText(effectCtx, paneID, text); err != nil {
			return EffectResult{Result: d.failErr(requestID, "prompt", paneID, err)}
		}
		if err := d.paneSessionError(token); err != nil {
			err = partiallyApplied("prompt text was already delivered", err)
			return EffectResult{Result: d.failErr(requestID, "prompt", paneID, err)}
		}
		if err := d.herdr.SendKeys(effectCtx, paneID, []string{"Enter"}); err != nil {
			err = partiallyApplied("prompt text was already delivered", err)
			return EffectResult{Result: d.failErr(requestID, "prompt", paneID, err)}
		}
		return EffectResult{Result: completed(requestID, "prompt", paneID, nil)}
	}))
	if result.OK {
		d.recordActivityWithExtract("prompt", "sent", "Prompt sent", text, paneID, requestID)
		d.wake()
	}
	return result
}

func (d *Dispatcher) handleKeys(ctx context.Context, receivedAt time.Time, requestID, paneID string, message map[string]any) *CommandResult {
	keys, err := stringSlice(message["keys"])
	if paneID == "" || err != nil || len(keys) == 0 {
		return d.fail(requestID, "keys", paneID, "Keys and agent are required")
	}
	result := d.schedule(ctx, ScheduleOptions{
		Command: d.command(ctx, receivedAt, requestID, CommandKeys, paneID, commandDeadline, keys),
	}, EffectFunc(func(effectCtx context.Context, token WorkerToken) EffectResult {
		if stale := d.paneSessionCurrent(token, requestID, "keys"); stale != nil {
			return EffectResult{Result: stale}
		}
		if err := d.herdr.SendKeys(effectCtx, paneID, keys); err != nil {
			return EffectResult{Result: d.failErr(requestID, "keys", paneID, err)}
		}
		return EffectResult{Result: completed(requestID, "keys", paneID, nil)}
	}))
	if result.OK {
		label := stringValue(message, "activity_label")
		if label == "" {
			label = "keys"
		}
		d.recordActivity("keys", "sent", label, paneID, requestID)
		d.wake()
	}
	return result
}

func (d *Dispatcher) handleText(ctx context.Context, receivedAt time.Time, requestID, paneID string, message map[string]any) *CommandResult {
	text := stringValue(message, "text")
	if paneID == "" || text == "" {
		return d.fail(requestID, "text", paneID, "Text and agent are required")
	}
	result := d.schedule(ctx, ScheduleOptions{
		Command: d.command(ctx, receivedAt, requestID, CommandText, paneID, commandDeadline, text),
	}, EffectFunc(func(effectCtx context.Context, token WorkerToken) EffectResult {
		if stale := d.paneSessionCurrent(token, requestID, "text"); stale != nil {
			return EffectResult{Result: stale}
		}
		if err := d.herdr.SendText(effectCtx, paneID, text); err != nil {
			return EffectResult{Result: d.failErr(requestID, "text", paneID, err)}
		}
		return EffectResult{Result: completed(requestID, "text", paneID, nil)}
	}))
	if result.OK {
		d.recordActivityWithExtract("text", "sent", "Text inserted", text, paneID, requestID)
		d.wake()
	}
	return result
}

// handleSecret answers a terminal prompt that reads with echo disabled — a
// sudo password, an ssh passphrase, a smartcard PIN. The secret travels as one
// key per rune plus Enter rather than through send_text: Herdr wraps sent text
// in a bracketed paste whenever the pane enables it, and a reader using termios
// noecho takes the paste markers as part of the secret. The single SendKeys
// call keeps the whole secret plus its submission atomic.
func (d *Dispatcher) handleSecret(ctx context.Context, receivedAt time.Time, requestID, paneID string, message map[string]any) *CommandResult {
	text := stringValue(message, "text")
	if paneID == "" || text == "" {
		return d.fail(requestID, "send_secret", paneID, "Secret and agent are required")
	}
	runes := []rune(text)
	if len(runes) > secretMaxRunes {
		return d.fail(requestID, "send_secret", paneID, "Secret is too long")
	}
	keys := make([]string, 0, len(runes)+1)
	for _, value := range runes {
		if value < 0x20 || value == 0x7f {
			return d.fail(requestID, "send_secret", paneID, "Secret must not contain control characters")
		}
		keys = append(keys, string(value))
	}
	keys = append(keys, "Enter")
	result := d.schedule(ctx, ScheduleOptions{
		// The payload stands in for the secret: nothing that reaches the
		// scheduler, its ledger or its logs may carry the secret itself.
		Command: d.command(ctx, receivedAt, requestID, CommandSecret, paneID, commandDeadline, len(runes)),
	}, EffectFunc(func(effectCtx context.Context, token WorkerToken) EffectResult {
		if stale := d.paneSessionCurrent(token, requestID, "send_secret"); stale != nil {
			return EffectResult{Result: stale}
		}
		if err := d.herdr.SendKeys(effectCtx, paneID, keys); err != nil {
			return EffectResult{Result: d.failErr(requestID, "send_secret", paneID, err)}
		}
		return EffectResult{Result: completed(requestID, "send_secret", paneID, nil)}
	}))
	if result.OK {
		// Constant label, no extract: the journal is readable by anyone who can
		// read the activity feed.
		d.recordActivity("send_secret", "sent", "Password entered", paneID, requestID)
		d.wake()
	}
	return result
}

func (d *Dispatcher) handleStop(ctx context.Context, receivedAt time.Time, requestID, paneID string) *CommandResult {
	if paneID == "" {
		return d.fail(requestID, "agent_stop", paneID, "Agent is required")
	}
	result := d.schedule(ctx, ScheduleOptions{
		Command:     d.command(ctx, receivedAt, requestID, CommandStop, paneID, commandDeadline, nil),
		LedgerKey:   "stop\x00" + paneID + "\x00" + requestID,
		PayloadHash: hashPayload(struct{}{}),
	}, EffectFunc(func(effectCtx context.Context, token WorkerToken) EffectResult {
		if stale := d.paneSessionCurrent(token, requestID, "agent_stop"); stale != nil {
			return EffectResult{Result: stale}
		}
		if err := d.herdr.StopPane(effectCtx, paneID); err != nil {
			return EffectResult{Result: d.failErr(requestID, "agent_stop", paneID, err)}
		}
		return EffectResult{Result: completed(requestID, "agent_stop", paneID, nil), BumpGeneration: true}
	}))
	if result.OK && !result.replayed {
		d.state.BumpGeneration(paneID)
		d.state.MarkTopologyChanged()
		if d.profiles != nil {
			d.profiles.Forget(paneID)
		}
		d.recordActivity("agent_stop", "sent", "Stopped agent", paneID, requestID)
		d.wake()
	}
	return result
}

func (d *Dispatcher) handleTabRename(ctx context.Context, receivedAt time.Time, requestID, paneID string, message map[string]any) *CommandResult {
	label := strings.TrimSpace(stringValue(message, "name"))
	if paneID == "" {
		return d.fail(requestID, "agent_rename", paneID, "Agent is required")
	}
	if label == "" {
		return d.fail(requestID, "agent_rename", paneID, "Tab name is required")
	}
	agent, ok := d.state.Agent(paneID)
	if !ok || agent.TabID == "" {
		return d.fail(requestID, "agent_rename", paneID, "Tab is unavailable")
	}
	result := d.schedule(ctx, ScheduleOptions{
		Command: d.command(ctx, receivedAt, requestID, CommandTabRename, paneID, commandDeadline, label),
	}, EffectFunc(func(effectCtx context.Context, token WorkerToken) EffectResult {
		if stale := d.paneSessionCurrent(token, requestID, "agent_rename"); stale != nil {
			return EffectResult{Result: stale}
		}
		if err := d.herdr.TabRename(effectCtx, agent.TabID, label); err != nil {
			return EffectResult{Result: d.failErr(requestID, "agent_rename", paneID, err)}
		}
		return EffectResult{Result: completed(requestID, "agent_rename", paneID, nil)}
	}))
	if result.OK {
		d.recordActivity("agent_rename", "renamed", "Renamed tab to "+label, paneID, requestID)
		d.wake()
	}
	return result
}

func (d *Dispatcher) handleTabReorder(ctx context.Context, receivedAt time.Time, requestID, paneID string, message map[string]any) *CommandResult {
	insertIndex, valid := tabInsertIndex(message["insert_index"])
	if paneID == "" {
		return d.fail(requestID, "tab_reorder", paneID, "Agent is required")
	}
	if !valid {
		return d.fail(requestID, "tab_reorder", paneID, "Tab position is invalid")
	}
	agent, ok := d.state.Agent(paneID)
	if !ok || agent.TabID == "" {
		return d.fail(requestID, "tab_reorder", paneID, "Tab is unavailable")
	}
	result := d.schedule(ctx, ScheduleOptions{
		Command: d.command(ctx, receivedAt, requestID, CommandTabReorder, paneID, commandDeadline, insertIndex),
	}, EffectFunc(func(effectCtx context.Context, token WorkerToken) EffectResult {
		if stale := d.paneSessionCurrent(token, requestID, "tab_reorder"); stale != nil {
			return EffectResult{Result: stale}
		}
		if err := d.herdr.TabMove(effectCtx, agent.TabID, insertIndex); err != nil {
			return EffectResult{Result: d.failErr(requestID, "tab_reorder", paneID, err)}
		}
		return EffectResult{Result: completed(
			requestID,
			"tab_reorder",
			paneID,
			map[string]any{"insert_index": insertIndex},
		)}
	}))
	if result.OK {
		d.recordActivity("tab_reorder", "reordered", "Reordered tab", paneID, requestID)
		d.wake()
	}
	return result
}

func (d *Dispatcher) handleAcknowledge(requestID, paneID string) *CommandResult {
	before := d.state.DisplayedStatus(paneID)
	if paneID == "" || !d.state.AcknowledgePane(paneID) {
		return d.fail(requestID, "acknowledge_pane", paneID, "Agent is unavailable")
	}
	after := d.state.DisplayedStatus(paneID)
	d.wake()
	if d.broadcast != nil && before != after {
		d.broadcast(map[string]any{
			"type":          "agent_update",
			"pane_id":       paneID,
			"raw_pane_id":   paneID,
			"status":        after,
			"pane_revision": d.state.Revision(paneID),
		})
	}
	return completed(requestID, "acknowledge_pane", paneID, nil)
}

func (d *Dispatcher) handleAgentStart(ctx context.Context, receivedAt time.Time, requestID string, message map[string]any) *CommandResult {
	request := StartRequest{
		ProfileID:   stringValue(message, "profile_id"),
		WorkspaceID: stringValue(message, "workspace_id"),
		Name:        stringValue(message, "name"),
		Cwd:         stringValue(message, "cwd"),
		Prompt:      stringValue(message, "prompt"),
	}
	if request.ProfileID == "" || request.Name == "" || request.Cwd == "" {
		return d.fail(requestID, "agent_start", "", "Profile, name, and working directory are required")
	}

	ledgerKey := "start\x00" + requestID
	payloadHash := hashPayload(request)
	var profile profiles.Profile
	if d.lifecycle != nil {
		var err error
		profile, request, err = d.lifecycle.ValidateStart(request)
		if err != nil {
			return d.fail(requestID, "agent_start", "", err.Error())
		}
	}

	result := d.schedule(ctx, ScheduleOptions{
		Command:     d.command(ctx, receivedAt, requestID, CommandStart, "", agentStartDeadline, request),
		RelayLevel:  true,
		LedgerKey:   ledgerKey,
		PayloadHash: payloadHash,
	}, EffectFunc(func(effectCtx context.Context, _ WorkerToken) EffectResult {
		var started StartResult
		var err error
		if d.lifecycle != nil {
			started, err = d.lifecycle.Start(effectCtx, profile, request)
		} else {
			// Compatibility for direct unit construction. The production server
			// always installs a profile resolver and uses the full workflow.
			paneID := "pane-" + request.Name
			var returned string
			returned, err = d.herdr.StartAgent(effectCtx, request.Name, request.ProfileID, paneID, agentStartProcessTimeoutMS)
			started = StartResult{PaneID: returned, Name: request.Name, Cwd: request.Cwd}
		}
		if err != nil {
			// started.PaneID is set when Herdr created the target before the
			// start failed. The pane is kept, so it must travel with the
			// failure: the phone shows it instead of losing the workspace.
			return EffectResult{Result: d.failErr(requestID, "agent_start", started.PaneID, err)}
		}
		return EffectResult{Result: completed(requestID, "agent_start", started.PaneID, started)}
	}))
	if !result.OK {
		if result.PaneID != "" {
			// A target survived the failure. Publish the topology so the empty
			// pane appears on the phone and a retry can start into it.
			d.state.MarkTopologyChanged()
			d.wake()
		}
		return result
	}
	if result.replayed {
		return result
	}

	data, _ := result.Data.(StartResult)
	if data.PaneID == "" {
		if values, ok := result.Data.(map[string]any); ok {
			data.PaneID, _ = values["pane_id"].(string)
		}
	}
	if request.Prompt != "" && data.PaneID != "" {
		generation, active := d.state.PaneSession(data.PaneID)
		initialPromptCtx := context.WithValue(ctx, paneSessionContextKey{}, paneSessionAdmission{
			generation:  uint64(generation),
			active:      active,
			allowAbsent: true,
		})
		promptResult := d.handlePrompt(initialPromptCtx, receivedAt, requestID+"-initial", data.PaneID, map[string]any{"text": request.Prompt})
		if !promptResult.OK {
			result.Phase = "completed_with_warning"
			result.Data = map[string]any{
				"pane_id": data.PaneID,
				"name":    request.Name,
				"cwd":     request.Cwd,
				"warning": "Agent started, but the initial prompt was not confirmed",
			}
		}
	}
	d.recordActivity("agent_start", "started", "Started "+request.Name, data.PaneID, requestID)
	d.state.MarkTopologyChanged()
	d.wake()
	return result
}

func (d *Dispatcher) handleClear(ctx context.Context, receivedAt time.Time, requestID, paneID string) *CommandResult {
	if paneID == "" {
		return d.fail(requestID, "agent_clear", paneID, "Agent is required")
	}
	agent, ok := d.state.Agent(paneID)
	if !ok {
		return d.fail(requestID, "agent_clear", paneID, "Agent is no longer available")
	}
	if d.lifecycle == nil || d.profiles == nil {
		// Direct unit-test fallback still preserves serialization.
		return d.schedule(ctx, ScheduleOptions{
			Command: d.command(ctx, receivedAt, requestID, CommandClear, paneID, agentStartDeadline, nil),
		}, EffectFunc(func(effectCtx context.Context, token WorkerToken) EffectResult {
			if stale := d.paneSessionCurrent(token, requestID, "agent_clear"); stale != nil {
				return EffectResult{Result: stale}
			}
			if err := d.herdr.StopPane(effectCtx, paneID); err != nil {
				return EffectResult{Result: d.failErr(requestID, "agent_clear", paneID, err)}
			}
			return EffectResult{Result: completed(requestID, "agent_clear", paneID, nil), BumpGeneration: true}
		}))
	}

	profileID := d.profiles.ResolvePane(paneID, agent.Agent)
	profile, exists := d.profiles.Profile(profileID)
	if !exists {
		return d.fail(requestID, "agent_clear", paneID, "This agent does not match an available launch profile")
	}
	name := "clear-" + fmt.Sprintf("%x", receivedAt.UnixNano())[:8]
	request := StartRequest{ProfileID: profile.ID, Name: name, Cwd: agent.Cwd}
	_, request, err := d.lifecycle.ValidateStart(request)
	if err != nil {
		return d.fail(requestID, "agent_clear", paneID, err.Error())
	}

	result := d.schedule(ctx, ScheduleOptions{
		Command:   d.command(ctx, receivedAt, requestID, CommandClear, paneID, agentStartDeadline, request),
		LedgerKey: "clear\x00" + paneID + "\x00" + requestID,
	}, EffectFunc(func(effectCtx context.Context, token WorkerToken) EffectResult {
		if stale := d.paneSessionCurrent(token, requestID, "agent_clear"); stale != nil {
			return EffectResult{Result: stale}
		}
		replacement, err := d.lifecycle.Start(effectCtx, profile, request)
		if err != nil {
			return EffectResult{Result: d.failErr(requestID, "agent_clear", paneID, err)}
		}
		data := map[string]any{"pane_id": replacement.PaneID, "name": replacement.Name, "cwd": replacement.Cwd}
		if stale := d.paneSessionCurrent(token, requestID, "agent_clear"); stale != nil {
			return EffectResult{Result: stale}
		}
		if err := d.herdr.StopPane(effectCtx, paneID); err != nil {
			data["warning"] = "Replacement started, but the old pane could not be closed"
			result := completed(requestID, "agent_clear", paneID, data)
			result.Phase = "completed_with_warning"
			return EffectResult{Result: result, BumpGeneration: true}
		}
		return EffectResult{Result: completed(requestID, "agent_clear", paneID, data), BumpGeneration: true}
	}))
	if result.OK && !result.replayed {
		d.state.BumpGeneration(paneID)
		d.state.MarkTopologyChanged()
		d.profiles.Forget(paneID)
		d.recordActivity("agent_clear", "cleared", "Cleared agent", paneID, requestID)
		d.wake()
	}
	return result
}

func (d *Dispatcher) command(ctx context.Context, receivedAt time.Time, requestID string, kind CommandKind, paneID string, budget time.Duration, payload any) Command {
	deadline := receivedAt.Add(budget)
	id, _ := ctx.Value(receiptContextKey{}).(CommandID)
	if id == 0 {
		id = d.scheduler.NextCommandID()
	}
	return Command{
		ID:         id,
		RequestID:  requestID,
		ReceivedAt: receivedAt,
		Deadline:   deadline,
		Kind:       kind,
		PaneID:     paneID,
		Payload:    payload,
	}
}

func (d *Dispatcher) schedule(ctx context.Context, options ScheduleOptions, runner EffectRunner) *CommandResult {
	if !options.RelayLevel {
		admission, captured := ctx.Value(paneSessionContextKey{}).(paneSessionAdmission)
		if !captured {
			generation, active := d.state.PaneSession(options.PaneID)
			admission = paneSessionAdmission{generation: uint64(generation), active: active}
		}
		generation, active := d.state.PaneSession(options.PaneID)
		if uint64(generation) != admission.generation ||
			(!active && !admission.allowAbsent) {
			return d.fail(options.RequestID, string(options.Kind), options.PaneID, ErrPaneReplaced.Error())
		}
		options.PaneGeneration = admission.generation
		options.AllowAbsent = admission.allowAbsent
	}
	admitted, _ := ctx.Value(admissionContextKey{}).(func())
	result, err := d.scheduler.ExecuteAdmitted(ctx, options, runner, admitted)
	switch {
	case err == nil && result != nil:
		return result
	case errors.Is(err, ErrConflict):
		return d.fail(options.RequestID, string(options.Kind), options.PaneID, "A different response was already submitted")
	case errors.Is(err, ErrIngressFull), errors.Is(err, ErrClosed):
		return &CommandResult{RequestID: options.RequestID, Action: string(options.Kind), OK: false, Phase: "not_started", Error: "command was not sent; retry is safe", PaneID: options.PaneID}
	case err != nil:
		return d.failErr(options.RequestID, string(options.Kind), options.PaneID, err)
	default:
		return d.fail(options.RequestID, string(options.Kind), options.PaneID, "Command failed")
	}
}

func (d *Dispatcher) fail(requestID, action, paneID, message string) *CommandResult {
	if action != "" {
		d.logger.Warn("command failed", "action", action, "request_id", requestID, "pane_id", paneID, "error", message)
		summary := strings.ReplaceAll(action, "_", " ") + " failed: " + message
		d.recordActivity(action, "failed", summary, paneID, requestID)
	}
	return &CommandResult{RequestID: requestID, Action: action, OK: false, Phase: "failed", Error: message, PaneID: paneID}
}

// partiallyApplied marks a multi-step mutation whose earlier steps already
// reached the agent. It keeps ErrDispatchedUnknown in the chain for existing
// callers while letting failErr outrank any safe-to-retry classification the
// failing step alone would produce.
func partiallyApplied(reason string, err error) error {
	return fmt.Errorf("%w: %w: %s: %w", herdr.ErrDispatchedUnknown, herdr.ErrPartiallyApplied, reason, err)
}

func (d *Dispatcher) failErr(requestID, action, paneID string, err error) *CommandResult {
	phase := "failed"
	public := "Command failed"
	var data any
	switch {
	case errors.Is(err, herdr.ErrPartiallyApplied):
		phase = "dispatched_unknown"
		public = "Part of the command already reached the agent; review it before retrying"
		data = map[string]any{"dispatched_unknown": true}
	// Must follow ErrPartiallyApplied: a started subprocess always carries
	// ErrDispatchedUnknown, so an earlier applied step would otherwise be
	// advertised as safe to retry and duplicate the input that landed.
	case herdr.IsRefused(err):
		phase = "not_started"
		public = "Command was not sent; retry is safe"
	case errors.Is(err, herdr.ErrCreatedTargetUnknown):
		phase = "dispatched_unknown"
		public = "Herdr may have created an empty target; review Herdr before retrying"
		data = map[string]any{"dispatched_unknown": true}
	case errors.Is(err, herdr.ErrDispatchedUnknown):
		phase = "dispatched_unknown"
		public = "Command may have executed; review the agent before retrying"
		data = map[string]any{"dispatched_unknown": true}
	case errors.Is(err, herdr.ErrNotStarted), errors.Is(err, context.DeadlineExceeded):
		phase = "not_started"
		public = "Command was not sent; retry is safe"
	}
	d.logger.Warn("command failed", "action", action, "request_id", requestID, "pane_id", paneID, "phase", phase, "error", err)
	if action != "" {
		summary := strings.ReplaceAll(action, "_", " ") + " failed: " + public
		d.recordActivity(action, "failed", summary, paneID, requestID)
	}
	return &CommandResult{RequestID: requestID, Action: action, OK: false, Phase: phase, Error: public, PaneID: paneID, Data: data}
}

func completed(requestID, action, paneID string, data any) *CommandResult {
	return &CommandResult{RequestID: requestID, Action: action, OK: true, Phase: "completed", PaneID: paneID, Data: data}
}

func (d *Dispatcher) recordActivity(kind, status, summary, paneID, requestID string) {
	d.recordActivityWithExtract(kind, status, summary, "", paneID, requestID)
}

func (d *Dispatcher) recordActivityWithExtract(kind, status, summary, extract, paneID, requestID string) {
	if d.activityW == nil {
		return
	}
	d.activityOrder.Lock()
	defer d.activityOrder.Unlock()
	agentState, _ := d.state.Agent(paneID)
	agent, project, host, session := "", "", "", ""
	if agentState != nil {
		agent = agentState.Agent
		project = agentState.Project
		host = agentState.Host
		session = agentState.Session
	}
	entry := activity.NewEntry(kind, status, summary, paneID, agent, project, requestID)
	entry.Host = host
	entry.Session = session
	entry.Extract = extract
	entry.Details = map[string]any{"action": kind}
	committed, err := d.activityW.Commit(context.Background(), activity.ActivityCommitRequested{
		Sequence: d.activitySequence.Add(1),
		Entry:    entry,
	})
	if err != nil {
		d.activityFailures.Add(1)
		d.logger.Warn("activity append failed", "error", err)
		return
	}
	if d.broadcast != nil {
		d.broadcast(map[string]any{"type": "activity", "activity": committed.Entry})
	}
}

func (d *Dispatcher) RecordActivity(kind, status, summary, paneID, requestID string) {
	d.recordActivity(kind, status, summary, paneID, requestID)
}

// RecordTransitionActivity publishes only while the originating state
// revision is still current. If the state advances during the durable append,
// the worker tombstones and removes the stale record before it can be exposed.
func (d *Dispatcher) RecordTransitionActivity(
	kind string,
	status string,
	summary string,
	paneID string,
	expectedStatus string,
	revision int64,
	details map[string]any,
	agent string,
	project string,
	host string,
	session string,
	extract string,
	blockedIdentity ...any,
) bool {
	if d.activityW == nil {
		return false
	}
	d.activityOrder.Lock()
	defer d.activityOrder.Unlock()
	transitionCurrent := func() bool {
		if kind == "working" {
			return true
		}
		if kind == "finished" {
			return d.state.CompletionCurrent(paneID, revision)
		}
		if expectedStatus == "blocked" && len(blockedIdentity) == 4 {
			eventID, eventOK := blockedIdentity[0].(string)
			generation, generationOK := blockedIdentity[1].(uint64)
			attentionKind, attentionOK := blockedIdentity[2].(string)
			attentionRevision, revisionOK := blockedIdentity[3].(int64)
			if eventOK && generationOK && attentionOK && revisionOK {
				return d.state.AttentionTransitionCurrent(
					paneID,
					eventID,
					generation,
					attentionKind,
					attentionRevision,
				)
			}
		}
		if expectedStatus == "blocked" && len(blockedIdentity) == 2 {
			eventID, eventOK := blockedIdentity[0].(string)
			generation, generationOK := blockedIdentity[1].(uint64)
			if eventOK && generationOK {
				return d.state.BlockedTransitionCurrent(paneID, eventID, generation)
			}
		}
		return d.state.TransitionCurrent(paneID, expectedStatus, revision)
	}
	if !transitionCurrent() {
		return false
	}
	entry := activity.NewEntry(kind, status, summary, paneID, agent, project, "")
	entry.Host = host
	entry.Session = session
	entry.Details = details
	if transitionAt, ok := details["transition_at"].(int64); ok && transitionAt > 0 {
		entry.Timestamp = activity.MilliTimestamp(transitionAt)
	}
	entry.Extract = extract
	committed, err := d.activityW.Commit(context.Background(), activity.ActivityCommitRequested{
		Sequence: d.activitySequence.Add(1),
		Entry:    entry,
	})
	if err != nil {
		d.activityFailures.Add(1)
		d.logger.Warn("transition activity append failed", "error", err)
		return false
	}
	if !transitionCurrent() {
		_, discardErr := d.activityW.Discard(context.Background(), activity.ActivityDiscardRequested{
			Sequence: d.activitySequence.Add(1),
			ID:       committed.Entry.ID,
		})
		if discardErr != nil {
			d.activityFailures.Add(1)
			d.logger.Warn("stale transition activity discard failed", "error", discardErr)
		}
		return false
	}
	if d.broadcast != nil {
		d.broadcast(map[string]any{"type": "activity", "activity": committed.Entry})
	}
	return true
}

func (d *Dispatcher) wake() {
	if d.wakePoll != nil {
		d.wakePoll()
	}
}

func capPaneContentLines(content []byte, limit int) []byte {
	if limit < 1 || len(content) == 0 {
		return content
	}
	end := len(content)
	if content[end-1] == '\n' {
		end--
	}
	remaining := limit
	for index := end - 1; index >= 0; index-- {
		if content[index] != '\n' {
			continue
		}
		remaining--
		if remaining == 0 {
			return content[index+1:]
		}
	}
	return content
}

func isClaudeAgent(agent string) bool {
	return strings.Contains(strings.ToLower(agent), "claude")
}

// readPaneForDisplay returns physical rows ("recent") for every ansi display read:
// the visible screen alone loses rows that scroll past between watch polls,
// and the Resize Session baseline must share row semantics with the resized
// frames so the trailing desktop screen — including agent chrome — can be cut
// by exact row count. Claude keeps logical lines ("recent-unwrapped") because
// its alternate-screen history merge depends on them.
func (d *Dispatcher) readPaneForDisplay(
	ctx context.Context,
	paneID string,
	lines int,
	format string,
	resized bool,
) (herdr.PaneRead, error) {
	// A non-ansi read is the only shape Herdr can serve by harvesting
	// scrollback through the agent's mouse-scroll interface: asking "recent"
	// or "recent-unwrapped" in text format for more lines than the pane's
	// viewport holds makes Herdr scroll the operator's real pane up and snap
	// it back, once per read. Only "visible" cannot trigger the harvest.
	if format != "ansi" {
		return d.herdr.ReadPaneVisible(ctx, paneID, lines, format)
	}
	if !resized {
		if agent, ok := d.state.Agent(paneID); ok && isClaudeAgent(agent.Agent) {
			return d.herdr.ReadPane(ctx, paneID, lines, format)
		}
	}
	return d.herdr.ReadPaneRecent(ctx, paneID, lines, format)
}

// HarvestPaneTranscript returns the pane's transcript from beyond its visible
// screen. Herdr serves this only in text format, and it pays for it by scrolling
// the operator's real pane up and snapping it back — seconds of visible movement
// per call, measured at ~2s for a 1000-line read. No polling path may reach it
// (see TestTextFormatDisplayReadNeverHarvestsScrollback); it exists to seed the
// history of an alternate-screen pane, whose display reads can never return more
// than the screen because it keeps no scrollback of its own.
func (d *Dispatcher) HarvestPaneTranscript(ctx context.Context, paneID string, lines int) (string, bool, error) {
	read, err := d.herdr.ReadPane(ctx, paneID, lines, "text")
	if err != nil {
		return "", false, err
	}
	return string(read.Content), read.Truncated, nil
}

func (d *Dispatcher) HandleReadPane(ctx context.Context, message map[string]any) map[string]any {
	paneID := stringValue(message, "pane_id")
	if paneID == "" {
		return map[string]any{"type": "pane_content", "pane_id": "", "content": "", "format": "text"}
	}
	d.handleAcknowledge(stringValue(message, "request_id"), paneID)
	lines := intValue(message["lines"], 30)
	if lines < 1 {
		lines = 1
	}
	if lines > 10000 {
		lines = 10000
	}
	format := stringValue(message, "format")
	if format != "ansi" {
		format = "text"
	}
	terminalColumns := intValue(message["terminal_columns"], 0)
	if terminalColumns < 1 {
		terminalColumns = 0
	}
	readCtx, cancel := context.WithTimeout(ctx, commandDeadline)
	defer cancel()
	generation := d.state.Generation(paneID)
	revision := d.state.ContentRevision(paneID)
	key := fmt.Sprintf("%s\x00%d\x00%s\x00%d\x00%d", paneID, lines, format, terminalColumns, generation)
	d.readMu.Lock()
	call := d.reads[key]
	owner := call == nil
	if owner {
		call = &paneRead{done: make(chan struct{})}
		d.reads[key] = call
	}
	d.readMu.Unlock()
	if owner {
		call.read, call.err = d.readPaneForDisplay(readCtx, paneID, lines, format, terminalColumns > 0)
		d.readMu.Lock()
		delete(d.reads, key)
		close(call.done)
		d.readMu.Unlock()
	} else {
		select {
		case <-call.done:
		case <-readCtx.Done():
			return map[string]any{"type": "pane_content", "pane_id": paneID, "content": "", "format": format, "error": "Unable to read the agent pane"}
		}
	}
	read, err := call.read, call.err
	if err != nil {
		return map[string]any{"type": "pane_content", "pane_id": paneID, "content": "", "format": format, "error": "Unable to read the agent pane"}
	}
	if d.state.Generation(paneID) != generation {
		return map[string]any{"type": "pane_content", "pane_id": paneID, "content": "", "format": format, "error": "The agent pane was replaced while it was being read"}
	}
	if d.state.ContentRevision(paneID) != revision {
		return map[string]any{"type": "pane_content", "pane_id": paneID, "content": "", "format": format, "error": "The agent state changed while the pane was being read"}
	}
	content := capPaneContentLines(read.Content, lines)
	response := map[string]any{
		"type": "pane_content", "pane_id": paneID, "content": string(content),
		"format": format, "truncated": read.Truncated, "viewport_only": terminalColumns > 0,
		"interaction": nil, "question_layout": false,
	}
	if terminalRows := intValue(message["terminal_rows"], 0); terminalRows > 0 {
		response["viewport_rows"] = terminalRows
	}
	return response
}

func (d *Dispatcher) HandleProbePane(ctx context.Context, message map[string]any) map[string]any {
	paneID := stringValue(message, "pane_id")
	format := stringValue(message, "format")
	if format != "ansi" {
		format = "text"
	}
	if paneID == "" {
		return map[string]any{"type": "pane_probe", "pane_id": "", "format": format, "error": "Unable to read the agent pane"}
	}
	const lines = 500
	readCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	generation := d.state.Generation(paneID)
	contentRevision := d.state.ContentRevision(paneID)
	content, err := d.herdr.ProbePaneVisible(readCtx, paneID, lines, format)
	if err != nil {
		return map[string]any{"type": "pane_probe", "pane_id": paneID, "format": format, "error": "Unable to read the agent pane"}
	}
	if d.state.Generation(paneID) != generation || d.state.ContentRevision(paneID) != contentRevision {
		return map[string]any{"type": "pane_probe", "pane_id": paneID, "format": format, "error": "The agent pane changed while it was being read"}
	}
	return map[string]any{"type": "pane_probe", "pane_id": paneID, "content": string(content), "format": format}
}

func (d *Dispatcher) HandleGetActivity(message map[string]any) map[string]any {
	limit := intValue(message["limit"], 500)
	if limit < 1 || limit > 500 {
		limit = 500
	}
	var entries []activity.Entry
	if d.journal != nil {
		entries = d.journal.Recent(limit)
	}
	return map[string]any{"type": "activity_history", "activities": entries}
}

func (d *Dispatcher) HandleClearActivities(
	requestID string,
	deliver func(*CommandResult),
) *CommandResult {
	finish := func(result *CommandResult) *CommandResult {
		if deliver != nil {
			deliver(result)
		}
		return result
	}
	if d.activityW == nil {
		return finish(d.fail(requestID, "clear_activities", "", "Activity storage is unavailable"))
	}
	d.activityOrder.Lock()
	defer d.activityOrder.Unlock()
	if _, err := d.activityW.Clear(context.Background(), activity.ActivityClearRequested{
		Sequence: d.activitySequence.Add(1),
	}); err != nil {
		d.activityFailures.Add(1)
		return finish(d.fail(requestID, "clear_activities", "", "Activity history could not be cleared"))
	}
	if d.broadcast != nil {
		d.broadcast(map[string]any{"type": "activity_history", "activities": []any{}})
	}
	return finish(completed(requestID, "clear_activities", "", nil))
}

func ResultToJSON(result *CommandResult) []byte {
	data, _ := json.Marshal(struct {
		Type string `json:"type"`
		*CommandResult
	}{Type: "command_result", CommandResult: result})
	return data
}

func stringValue(message map[string]any, key string) string {
	value, _ := message[key].(string)
	return value
}

func stringSlice(value any) ([]string, error) {
	switch values := value.(type) {
	case []string:
		for _, item := range values {
			if item == "" {
				return nil, errors.New("empty string")
			}
		}
		return append([]string(nil), values...), nil
	case []any:
		result := make([]string, 0, len(values))
		for _, item := range values {
			text, ok := item.(string)
			if !ok || text == "" {
				return nil, errors.New("value is not a non-empty string")
			}
			result = append(result, text)
		}
		return result, nil
	default:
		return nil, errors.New("value is not an array")
	}
}

func intValue(value any, fallback int) int {
	switch number := value.(type) {
	case int:
		return number
	case float64:
		return int(number)
	default:
		return fallback
	}
}

func tabInsertIndex(value any) (int, bool) {
	switch number := value.(type) {
	case int:
		return number, number >= 0 && number <= maxTabInsertIndex
	case float64:
		index := int(number)
		return index, number >= 0 && number <= maxTabInsertIndex && number == float64(index)
	default:
		return 0, false
	}
}

func uint64Value(value any) uint64 {
	switch number := value.(type) {
	case uint64:
		return number
	case int:
		if number > 0 {
			return uint64(number)
		}
	case float64:
		if number > 0 {
			return uint64(number)
		}
	}
	return 0
}

func hashPayload(value any) string {
	data, _ := json.Marshal(value)
	// This is an in-memory conflict key, not an artifact integrity digest.
	return fmt.Sprintf("%x", data)
}
