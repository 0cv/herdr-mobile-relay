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
	promptMaxChars     = 100000
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
	done    chan struct{}
	content []byte
	err     error
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
		return d.handleRename(ctx, receivedAt, requestID, paneID, message)
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
	result := d.schedule(ctx, ScheduleOptions{
		Command: d.command(ctx, receivedAt, requestID, CommandPrompt, paneID, commandDeadline, text),
	}, EffectFunc(func(effectCtx context.Context, token WorkerToken) EffectResult {
		if stale := d.paneSessionCurrent(token, requestID, "prompt"); stale != nil {
			return EffectResult{Result: stale}
		}
		if err := d.herdr.Prompt(effectCtx, paneID, text); err != nil {
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

func (d *Dispatcher) handleRename(ctx context.Context, receivedAt time.Time, requestID, paneID string, message map[string]any) *CommandResult {
	name := stringValue(message, "name")
	if paneID == "" || name == "" {
		return d.fail(requestID, "agent_rename", paneID, "Name and agent are required")
	}
	if !agentNamePattern.MatchString(name) {
		return d.fail(requestID, "agent_rename", paneID, "Invalid agent name")
	}
	agent, _ := d.state.Agent(paneID)
	result := d.schedule(ctx, ScheduleOptions{
		Command: d.command(ctx, receivedAt, requestID, CommandRename, paneID, commandDeadline, name),
	}, EffectFunc(func(effectCtx context.Context, token WorkerToken) EffectResult {
		if stale := d.paneSessionCurrent(token, requestID, "agent_rename"); stale != nil {
			return EffectResult{Result: stale}
		}
		if err := d.herdr.RenameAgent(effectCtx, paneID, name); err != nil {
			return EffectResult{Result: d.failErr(requestID, "agent_rename", paneID, err)}
		}
		if agent != nil && agent.TabID != "" {
			if err := d.herdr.TabRename(effectCtx, agent.TabID, name); err != nil {
				result := completed(requestID, "agent_rename", paneID, map[string]any{"warning": "Agent renamed, but its tab label could not be updated"})
				result.Phase = "completed_with_warning"
				return EffectResult{Result: result}
			}
		}
		return EffectResult{Result: completed(requestID, "agent_rename", paneID, nil)}
	}))
	if result.OK {
		d.recordActivity("agent_rename", "renamed", "Renamed agent to "+name, paneID, requestID)
		d.wake()
	}
	return result
}

func (d *Dispatcher) handleAcknowledge(requestID, paneID string) *CommandResult {
	if paneID == "" || !d.state.AcknowledgePane(paneID) {
		return d.fail(requestID, "acknowledge_pane", paneID, "Agent is unavailable")
	}
	d.wake()
	if d.broadcast != nil {
		d.broadcast(map[string]any{
			"type":          "agent_update",
			"pane_id":       paneID,
			"raw_pane_id":   paneID,
			"status":        d.state.DisplayedStatus(paneID),
			"pane_revision": d.state.Revision(paneID),
		})
	}
	return completed(requestID, "acknowledge_pane", paneID, nil)
}

func (d *Dispatcher) handleAgentStart(ctx context.Context, receivedAt time.Time, requestID string, message map[string]any) *CommandResult {
	request := StartRequest{
		ProfileID: stringValue(message, "profile_id"),
		Name:      stringValue(message, "name"),
		Cwd:       stringValue(message, "cwd"),
		Prompt:    stringValue(message, "prompt"),
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
			return EffectResult{Result: d.failErr(requestID, "agent_start", "", err)}
		}
		return EffectResult{Result: completed(requestID, "agent_start", started.PaneID, started)}
	}))
	if !result.OK {
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

func (d *Dispatcher) failErr(requestID, action, paneID string, err error) *CommandResult {
	phase := "failed"
	public := "Command failed"
	switch {
	case errors.Is(err, herdr.ErrDispatchedUnknown):
		phase = "dispatched_unknown"
		public = "Command may have executed; review the agent before retrying"
	case errors.Is(err, herdr.ErrNotStarted), errors.Is(err, context.DeadlineExceeded):
		phase = "not_started"
		public = "Command was not sent; retry is safe"
	}
	d.logger.Warn("command failed", "action", action, "request_id", requestID, "pane_id", paneID, "phase", phase, "error", err)
	if action != "" {
		summary := strings.ReplaceAll(action, "_", " ") + " failed: " + public
		d.recordActivity(action, "failed", summary, paneID, requestID)
	}
	return &CommandResult{RequestID: requestID, Action: action, OK: false, Phase: phase, Error: public, PaneID: paneID}
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
		if kind == "finished" {
			return d.state.CompletionCurrent(paneID, revision)
		}
		if kind == "blocked" && len(blockedIdentity) == 2 {
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
	readCtx, cancel := context.WithTimeout(ctx, commandDeadline)
	defer cancel()
	generation := d.state.Generation(paneID)
	revision := d.state.ContentRevision(paneID)
	key := fmt.Sprintf("%s\x00%d\x00%s\x00%d", paneID, lines, format, generation)
	d.readMu.Lock()
	call := d.reads[key]
	owner := call == nil
	if owner {
		call = &paneRead{done: make(chan struct{})}
		d.reads[key] = call
	}
	d.readMu.Unlock()
	if owner {
		call.content, call.err = d.herdr.ReadPane(readCtx, paneID, lines, format)
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
	content, err := call.content, call.err
	if err != nil {
		return map[string]any{"type": "pane_content", "pane_id": paneID, "content": "", "format": format, "error": "Unable to read the agent pane"}
	}
	if d.state.Generation(paneID) != generation {
		return map[string]any{"type": "pane_content", "pane_id": paneID, "content": "", "format": format, "error": "The agent pane was replaced while it was being read"}
	}
	if d.state.ContentRevision(paneID) != revision {
		return map[string]any{"type": "pane_content", "pane_id": paneID, "content": "", "format": format, "error": "The agent state changed while the pane was being read"}
	}
	return map[string]any{"type": "pane_content", "pane_id": paneID, "content": string(content), "format": format, "interaction": nil, "question_layout": false}
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
