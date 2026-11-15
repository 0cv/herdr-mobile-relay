package coordinator

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/0cv/herdr-mobile-relay/internal/activity"
	"github.com/0cv/herdr-mobile-relay/internal/agentkind"
	"github.com/0cv/herdr-mobile-relay/internal/backend"
	"github.com/0cv/herdr-mobile-relay/internal/herdr"
	"github.com/0cv/herdr-mobile-relay/internal/noecho"
	"github.com/0cv/herdr-mobile-relay/internal/profiles"
	"github.com/0cv/herdr-mobile-relay/internal/protocol"
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

func CommandResultMessage(result *CommandResult) map[string]any {
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

type Dispatcher struct {
	terminalTextKey string
	herdr           backend.Backend
	state           *State
	activities      activityRecorder
	panes           *paneReader
	logger          *slog.Logger
	scheduler       *Scheduler
	profiles        *profiles.Resolver
	lifecycle       *Lifecycle
	broadcast       func(any)
	wakePoll        func()
	topologyMu      sync.Mutex
	watcherMu       sync.Mutex
	watcherCtx      context.Context
	watcherCancel   context.CancelFunc
	watcherClosed   bool
	watcherWG       sync.WaitGroup

	testFilterWait func(context.Context, time.Duration) error
}

type admissionContextKey struct{}

type commandRequest struct {
	receivedAt  time.Time
	requestID   string
	paneID      string
	in          protocol.Inbound
	receipt     CommandID
	paneSession int64
	admitted    func()
}

func (d *Dispatcher) newCommandRequest(receivedAt time.Time, requestID, paneID string, in protocol.Inbound, admitted func()) commandRequest {
	generation, _ := d.state.PaneSession(paneID)
	return commandRequest{
		receivedAt:  receivedAt,
		requestID:   requestID,
		paneID:      paneID,
		in:          in,
		paneSession: generation,
		admitted:    admitted,
	}
}

func NewDispatcher(client backend.Backend, state *State, journal *activity.Journal, logger *slog.Logger) *Dispatcher {
	watcherCtx, watcherCancel := context.WithCancel(context.Background())
	dispatcher := &Dispatcher{
		terminalTextKey: rand.Text(),
		herdr:           client,
		state:           state,
		logger:          logger,
		scheduler:       NewScheduler(defaultHerdrCapacity),
		panes:           &paneReader{herdr: client, state: state, reads: make(map[string]*paneRead)},
		activities:      activityRecorder{state: state, logger: logger},
		watcherCtx:      watcherCtx,
		watcherCancel:   watcherCancel,
	}
	dispatcher.scheduler.SetGenerationCurrent(state.PaneSessionCurrent)
	if journal != nil {
		dispatcher.activities.journal = journal
	}
	return dispatcher
}

func (d *Dispatcher) SetProfiles(resolver *profiles.Resolver) {
	d.profiles = resolver
	d.lifecycle = NewLifecycle(d.herdr, resolver)
	d.lifecycle.state = d.state
	d.state.mu.Lock()
	d.state.onInventory = func(agents []*AgentState) {
		targets := make([]profiles.PaneIdentity, 0, len(agents))
		if d.herdr.CapabilityStatus().OneStepLaunch {
			ctx, cancel := context.WithTimeout(d.watcherCtx, 15*time.Second)
			defer cancel()
			panes, err := d.herdr.PaneList(ctx)
			if err != nil {
				d.logger.Warn("profile ownership inventory failed", "error", err)
				return
			}
			for _, pane := range panes {
				targets = append(targets, profiles.PaneIdentity{PaneID: pane.ID, TerminalID: pane.TerminalID, TabID: pane.TabID, WorkspaceID: pane.WorkspaceID})
			}
		} else {
			for _, agent := range agents {
				targets = append(targets, profiles.PaneIdentity{PaneID: agent.PaneID, TerminalID: agent.TerminalID, TabID: agent.TabID, WorkspaceID: agent.WorkspaceID})
			}
		}
		if err := resolver.ReconcileOwnership(targets); err != nil {
			d.logger.Warn("profile ownership reconciliation failed", "error", err)
		}
	}
	d.state.mu.Unlock()
}

func (d *Dispatcher) CancelInflight() {
	d.scheduler.CancelInflight()
}

func (d *Dispatcher) Close(ctx context.Context) error {
	d.closeWatchers()
	return d.scheduler.Close(ctx)
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
	return d.activities.failures.Load()
}

func (d *Dispatcher) PruneSlots(active map[string]bool) {
	generations := make(map[string]int64, len(active))
	for paneID := range active {
		generations[paneID] = d.state.Generation(paneID)
	}
	d.scheduler.ApplyTopology(active, generations)
}

func (d *Dispatcher) paneSessionCurrent(token WorkerToken, requestID, action string) *CommandResult {
	if d.paneSessionError(token) == nil {
		return nil
	}
	return d.fail(requestID, action, token.PaneID, ErrPaneReplaced.Error())
}

func (d *Dispatcher) paneSessionError(token WorkerToken) error {
	generation, active := d.state.PaneSession(token.PaneID)
	if generation != token.Generation || (!active && !token.AllowAbsent) {
		return ErrPaneReplaced
	}
	return nil
}

// AgentUpdate carries the typed agent beside its wire message so the server
// can commit the agent to its view without parsing the message.
type AgentUpdate struct {
	Agent   *AgentState
	Message map[string]any
}

func (d *Dispatcher) SetBroadcast(fn func(any)) {
	d.broadcast = fn
	d.activities.broadcast = fn
}

func (d *Dispatcher) SetWakePoll(fn func()) {
	d.wakePoll = fn
}

func (d *Dispatcher) Handle(ctx context.Context, in protocol.Inbound) *CommandResult {
	return d.handle(ctx, in, nil)
}

func (d *Dispatcher) HandleAdmitted(
	ctx context.Context,
	in protocol.Inbound,
	admitted func(),
) *CommandResult {
	signal := sync.OnceFunc(admitted)
	result := d.handle(ctx, in, signal)
	signal()
	return result
}

func (d *Dispatcher) handle(ctx context.Context, in protocol.Inbound, admitted func()) *CommandResult {
	receivedAt := in.ReceivedAt
	if receivedAt.IsZero() {
		receivedAt = time.Now()
	}
	action := in.Type
	requestID := in.RequestID
	if requestID == "" {
		requestID = fmt.Sprintf("req-%d", receivedAt.UnixNano())
	}
	paneID := in.PaneID
	req := d.newCommandRequest(receivedAt, requestID, paneID, in, admitted)
	req.receipt = CommandID(in.ReceiptSequence)

	d.logger.Debug("dispatching command", "action", action, "request_id", requestID, "pane_id", paneID)

	switch action {
	case "submit_prompt":
		return d.handlePrompt(ctx, req)
	case "send_keys":
		return d.handleKeys(ctx, req)
	case "send_filter_text":
		return d.handleFilterText(ctx, req)
	case "send_input":
		return d.handleInput(ctx, req)
	case "send_terminal_text":
		return d.handleTerminalText(ctx, req)
	case "send_secret":
		return d.handleSecret(ctx, req)
	case "respond":
		return d.handleApproval(ctx, req)
	case "answer_question":
		return d.handleQuestion(ctx, req)
	case "navigate_question":
		return d.handleNavigateQuestion(ctx, req)
	case "clarify_question":
		return d.handleClarifyQuestion(ctx, req)
	case "agent_stop":
		return d.handleStop(ctx, req)
	case "agent_rename":
		return d.handleTabRename(ctx, req)
	case "tab_reorder":
		return d.handleTabReorder(ctx, req)
	case "acknowledge_pane":
		return d.handleAcknowledge(requestID, paneID)
	case "agent_start", "agent_clear":
		if !d.herdr.CapabilityStatus().Supports(herdr.FeatureAgentLaunch) {
			return d.fail(requestID, action, paneID, "This backend does not support launching agents")
		}
		if action == "agent_start" {
			return d.handleAgentStart(ctx, req)
		}
		return d.handleClear(ctx, req)
	default:
		return &CommandResult{RequestID: requestID, Action: action, OK: false, Phase: "failed", Error: "Unknown command"}
	}
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
	signal := sync.OnceFunc(admitted)
	ctx = context.WithValue(ctx, admissionContextKey{}, signal)
	result := handle(ctx)
	signal()
	return result
}

func (d *Dispatcher) handlePrompt(ctx context.Context, req commandRequest) *CommandResult {
	const action = "submit_prompt"
	text := req.in.Text
	if req.paneID == "" || text == "" {
		return d.fail(req.requestID, action, req.paneID, "Text and agent are required")
	}
	if len([]rune(text)) > promptMaxChars {
		return d.fail(req.requestID, action, req.paneID, "Prompt is longer than 100,000 characters")
	}
	requiresEnter := false
	if agent, ok := d.state.Agent(req.paneID); ok {
		requiresEnter = agentkind.Of(agent.Agent) == agentkind.Qoder
	}
	result := d.schedule(ctx, req, ScheduleOptions{
		Command: d.command(req, action, commandDeadline),
	}, EffectFunc(func(effectCtx context.Context, token WorkerToken) EffectResult {
		if err := d.deliverPrompt(effectCtx, token, text, requiresEnter); err != nil {
			return EffectResult{Result: d.failErr(req.requestID, action, req.paneID, err)}
		}
		return EffectResult{Result: completed(req.requestID, action, req.paneID, nil)}
	}))
	if result.OK {
		d.activities.recordWithExtract(action, "sent", "Prompt sent", text, req.paneID, req.requestID)
		d.wake()
	}
	return result
}

func (d *Dispatcher) deliverPrompt(ctx context.Context, token WorkerToken, text string, requiresEnter bool) error {
	ctx = herdr.WithDispatchCheck(ctx, func() error { return d.paneSessionError(token) })
	if err := d.paneSessionError(token); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return errors.Join(herdr.ErrNotStarted, err)
	}
	if !requiresEnter || !d.herdr.CapabilityStatus().Supports(herdr.FeatureManualTerminalInput) {
		return d.herdr.Prompt(ctx, token.PaneID, text)
	}
	if err := d.herdr.SendText(ctx, token.PaneID, text); err != nil {
		return err
	}
	if err := d.paneSessionError(token); err != nil {
		return partiallyApplied("prompt text was already delivered", err)
	}
	if err := d.herdr.SendKeys(ctx, token.PaneID, []string{"Enter"}); err != nil {
		return partiallyApplied("prompt text was already delivered", err)
	}
	return nil
}

func (d *Dispatcher) handleKeys(ctx context.Context, req commandRequest) *CommandResult {
	const action = "send_keys"
	keys := req.in.Keys
	if req.paneID == "" || len(keys) == 0 || slices.Contains(keys, "") {
		return d.fail(req.requestID, action, req.paneID, "Keys and agent are required")
	}
	result := d.callPane(ctx, req, paneCall{
		action: action,
		run: func(callCtx context.Context) error {
			return d.herdr.SendKeys(callCtx, req.paneID, keys)
		},
	})
	if result.OK {
		label := req.in.ActivityLabel
		if label == "" {
			label = "keys"
		}
		d.activities.record(action, "sent", label, req.paneID, req.requestID)
		d.wake()
	}
	return result
}

func (d *Dispatcher) handleInput(ctx context.Context, req commandRequest) *CommandResult {
	input, inputErr := herdr.ValidatePaneInput(herdr.PaneInput{
		Text: req.in.Text,
		Keys: req.in.Keys,
	})
	if req.paneID == "" || inputErr != nil {
		return d.fail(req.requestID, "send_input", req.paneID, "Valid text or keys and an agent are required")
	}
	result := d.callPane(ctx, req, paneCall{
		action: "send_input",
		run: func(callCtx context.Context) error {
			return d.herdr.SendInput(callCtx, req.paneID, input)
		},
	})
	if result.OK {
		label := req.in.ActivityLabel
		if label == "" {
			label = "Terminal input sent"
		}
		d.activities.recordWithExtract("input", "sent", label, input.Text, req.paneID, req.requestID)
		d.wake()
	}
	return result
}

// handleSecret answers a terminal prompt that reads with echo disabled — a
// sudo password, an ssh passphrase, a smartcard PIN. The secret travels as one
// key per rune plus Enter rather than through SendText: Herdr wraps sent text
// in a bracketed paste whenever the pane enables it, and a reader using termios
// noecho takes the paste markers as part of the secret. The single SendKeys
// call keeps the whole secret plus its submission atomic.
func (d *Dispatcher) handleSecret(ctx context.Context, req commandRequest) *CommandResult {
	text := req.in.Text
	if req.paneID == "" || text == "" {
		return d.fail(req.requestID, "send_secret", req.paneID, "Secret and agent are required")
	}
	runes := []rune(text)
	if len(runes) > secretMaxRunes {
		return d.fail(req.requestID, "send_secret", req.paneID, "Secret is too long")
	}
	for _, value := range runes {
		if value < 0x20 || value == 0x7f {
			return d.fail(req.requestID, "send_secret", req.paneID, "Secret must not contain control characters")
		}
	}
	result := d.schedule(ctx, req, ScheduleOptions{
		Command: d.command(req, "send_secret", commandDeadline),
	}, EffectFunc(func(effectCtx context.Context, token WorkerToken) EffectResult {
		if stale := d.paneSessionCurrent(token, req.requestID, "send_secret"); stale != nil {
			return EffectResult{Result: stale}
		}
		read, err := d.herdr.ReadPaneVisible(effectCtx, req.paneID, 30, "text")
		if err != nil {
			return EffectResult{Result: d.fail(req.requestID, "send_secret", req.paneID, "Hidden prompt could not be verified")}
		}
		if _, ok := noecho.Match(string(read.Content)); !ok {
			return EffectResult{Result: d.fail(req.requestID, "send_secret", req.paneID, "The terminal is no longer asking for a hidden value")}
		}
		if stale := d.paneSessionCurrent(token, req.requestID, "send_secret"); stale != nil {
			return EffectResult{Result: stale}
		}
		if err := d.herdr.SendSecret(effectCtx, req.paneID, text); err != nil {
			return EffectResult{Result: d.failErr(req.requestID, "send_secret", req.paneID, err)}
		}
		return EffectResult{Result: completed(req.requestID, "send_secret", req.paneID, nil)}
	}))
	if result.OK {
		// Constant label, no extract: the journal is readable by anyone who can
		// read the activity feed.
		d.activities.record("send_secret", "sent", "Password entered", req.paneID, req.requestID)
		d.wake()
	}
	return result
}

func (d *Dispatcher) handleStop(ctx context.Context, req commandRequest) *CommandResult {
	if req.paneID == "" {
		return d.fail(req.requestID, "agent_stop", req.paneID, "Agent is required")
	}
	result := d.callPane(ctx, req, paneCall{
		action:         "agent_stop",
		ledgerKey:      "stop\x00" + req.paneID + "\x00" + req.requestID,
		payloadHash:    hashPayload(struct{}{}),
		bumpGeneration: true,
		run: func(callCtx context.Context) error {
			return d.herdr.StopPane(callCtx, req.paneID)
		},
	})
	if result.OK && !result.replayed {
		d.state.BumpGeneration(req.paneID)
		d.state.MarkTopologyChanged()
		if d.profiles != nil {
			d.profiles.Forget(req.paneID)
		}
		d.activities.record("agent_stop", "sent", "Stopped agent", req.paneID, req.requestID)
		d.wake()
	}
	return result
}

func (d *Dispatcher) handleTabRename(ctx context.Context, req commandRequest) *CommandResult {
	label := strings.TrimSpace(req.in.Name)
	if req.paneID == "" {
		return d.fail(req.requestID, "agent_rename", req.paneID, "Agent is required")
	}
	if label == "" {
		return d.fail(req.requestID, "agent_rename", req.paneID, "Tab name is required")
	}
	agent, ok := d.state.Agent(req.paneID)
	if !ok || agent.TabID == "" {
		return d.fail(req.requestID, "agent_rename", req.paneID, "Tab is unavailable")
	}
	result := d.callPane(ctx, req, paneCall{
		action: "agent_rename",
		run: func(callCtx context.Context) error {
			return d.herdr.TabRename(callCtx, agent.TabID, label)
		},
	})
	if result.OK {
		d.activities.record("agent_rename", "renamed", "Renamed tab to "+label, req.paneID, req.requestID)
		d.wake()
	}
	return result
}

func (d *Dispatcher) handleTabReorder(ctx context.Context, req commandRequest) *CommandResult {
	insertIndex, valid := 0, false
	if req.in.InsertIndex != nil {
		insertIndex = *req.in.InsertIndex
		valid = insertIndex >= 0 && insertIndex <= maxTabInsertIndex
	}
	if req.paneID == "" {
		return d.fail(req.requestID, "tab_reorder", req.paneID, "Agent is required")
	}
	if !valid {
		return d.fail(req.requestID, "tab_reorder", req.paneID, "Tab position is invalid")
	}
	agent, ok := d.state.Agent(req.paneID)
	if !ok || agent.TabID == "" {
		return d.fail(req.requestID, "tab_reorder", req.paneID, "Tab is unavailable")
	}
	result := d.callPane(ctx, req, paneCall{
		action: "tab_reorder",
		data:   map[string]any{"insert_index": insertIndex},
		run: func(callCtx context.Context) error {
			return d.herdr.TabMove(callCtx, agent.TabID, insertIndex)
		},
	})
	if result.OK {
		d.activities.record("tab_reorder", "reordered", "Reordered tab", req.paneID, req.requestID)
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
	if agent, ok := d.state.Agent(paneID); ok && d.broadcast != nil && before != after {
		agent.Status = after
		d.broadcast(AgentUpdate{Agent: agent, Message: map[string]any{
			"type":          "agent_update",
			"pane_id":       paneID,
			"raw_pane_id":   paneID,
			"status":        after,
			"pane_revision": agent.StateRevision,
		}})
	}
	return completed(requestID, "acknowledge_pane", paneID, nil)
}

func (d *Dispatcher) handleAgentStart(ctx context.Context, req commandRequest) *CommandResult {
	req.paneID = ""
	request := StartRequest{
		ProfileID:     req.in.ProfileID,
		WorkspaceID:   req.in.WorkspaceID,
		Name:          req.in.Name,
		Cwd:           req.in.Cwd,
		Prompt:        req.in.Prompt,
		HermesProfile: req.in.HermesProfile,
	}
	if request.ProfileID == "" || request.Name == "" || request.Cwd == "" {
		return d.fail(req.requestID, "agent_start", "", "Profile, name, and working directory are required")
	}

	if len([]rune(request.Prompt)) > promptMaxChars {
		return d.fail(req.requestID, "agent_start", "", "Prompt is longer than 100,000 characters")
	}
	ledgerKey := "start\x00" + req.requestID
	payloadHash := hashPayload(request)
	if d.lifecycle == nil || d.profiles == nil || d.lifecycle.profiles == nil {
		return d.fail(req.requestID, "agent_start", "", "Agent launch is unavailable: profile lifecycle is not configured")
	}
	profile, request, err := d.lifecycle.ValidateStart(request)
	if err != nil {
		return d.fail(req.requestID, "agent_start", "", err.Error())
	}

	budget := agentStartDeadline
	if request.Prompt != "" {
		budget += commandDeadline
	}
	result := d.schedule(ctx, req, ScheduleOptions{
		Command:     d.command(req, "agent_start", budget),
		RelayLevel:  true,
		LedgerKey:   ledgerKey,
		PayloadHash: payloadHash,
	}, EffectFunc(func(effectCtx context.Context, _ WorkerToken) EffectResult {
		startupCtx, cancel := context.WithDeadline(effectCtx, req.receivedAt.Add(agentStartDeadline))
		defer cancel()
		started, err := d.lifecycle.Start(startupCtx, profile, request)
		if err != nil {
			// started.PaneID is set when Herdr created the target before the
			// start failed. The pane is kept, so it must travel with the
			// failure: the phone shows it instead of losing the workspace.
			return EffectResult{Result: d.failErr(req.requestID, "agent_start", started.PaneID, err)}
		}
		launchResult := completed(req.requestID, "agent_start", started.PaneID, started)
		if started.PromptNotReady {
			launchResult.Phase = "completed_with_warning"
			launchResult.Data = map[string]any{
				"pane_id": started.PaneID, "name": request.Name, "cwd": request.Cwd,
				"warning": "Agent started, but the initial prompt was not confirmed",
			}
			return EffectResult{Result: launchResult}
		}
		if request.Prompt == "" || started.PaneID == "" {
			return EffectResult{Result: launchResult}
		}
		requiresEnter := agentkind.Of(profile.Kind) == agentkind.Qoder || agentkind.Of(request.ProfileID) == agentkind.Qoder
		if agent, ok := d.state.Agent(started.PaneID); ok {
			requiresEnter = requiresEnter || agentkind.Of(agent.Agent) == agentkind.Qoder
		}
		return EffectResult{Continuation: &EffectContinuation{
			PaneID:      started.PaneID,
			Generation:  started.Identity.Generation,
			AllowAbsent: true,
			Deadline:    time.Now().Add(commandDeadline),
			Runner: EffectFunc(func(promptCtx context.Context, token WorkerToken) EffectResult {
				if !started.Identity.Valid {
					return EffectResult{Result: d.failErr(req.requestID, "submit_prompt", started.PaneID, ErrPaneReplaced)}
				}
				promptCtx = herdr.WithDispatchCheck(promptCtx, func() error {
					if err := d.state.launchIdentityCurrent(started.PaneID, started.Identity); err != nil {
						return err
					}
					return herdr.CheckDispatch(ctx)
				})
				if err := d.deliverPrompt(promptCtx, token, request.Prompt, requiresEnter); err != nil {
					return EffectResult{Result: d.failErr(req.requestID, "submit_prompt", started.PaneID, err)}
				}
				if err := d.paneSessionError(token); err != nil {
					return EffectResult{Result: d.failErr(req.requestID, "submit_prompt", started.PaneID, partiallyApplied("initial prompt was already delivered", err))}
				}
				d.activities.recordWithExtract("submit_prompt", "sent", "Prompt sent", request.Prompt, started.PaneID, req.requestID+"-initial")
				d.wake()
				return EffectResult{Result: completed(req.requestID, "submit_prompt", started.PaneID, nil)}
			}),
			Finalize: func(promptResult *CommandResult, err error) *CommandResult {
				if err != nil || promptResult == nil || !promptResult.OK {
					launchResult.Phase = "completed_with_warning"
					launchResult.Data = map[string]any{
						"pane_id": started.PaneID, "name": request.Name, "cwd": request.Cwd,
						"warning": "Agent started, but the initial prompt was not confirmed",
					}
				}
				return launchResult
			},
		}}
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
	d.activities.record("agent_start", "started", "Started "+request.Name, data.PaneID, req.requestID)
	d.state.MarkTopologyChanged()
	d.wake()
	return result
}

func (d *Dispatcher) handleClear(ctx context.Context, req commandRequest) *CommandResult {
	if req.paneID == "" {
		return d.fail(req.requestID, "agent_clear", req.paneID, "Agent is required")
	}
	agent, ok := d.state.Agent(req.paneID)
	if !ok {
		return d.fail(req.requestID, "agent_clear", req.paneID, "Agent is no longer available")
	}
	if d.lifecycle == nil || d.profiles == nil || d.lifecycle.profiles == nil {
		return d.fail(req.requestID, "agent_clear", req.paneID, "Agent lifecycle is unavailable")
	}

	profileID := d.profiles.ResolvePane(req.paneID, agent.Agent)
	profile, exists := d.profiles.Profile(profileID)
	if !exists {
		return d.fail(req.requestID, "agent_clear", req.paneID, "No launch profile matches this agent; start a new agent with + instead")
	}
	name := fmt.Sprintf("clear-%08x", uint32(req.receivedAt.UnixNano()))
	request := StartRequest{ProfileID: profile.ID, Name: name, Cwd: agent.Cwd}
	if d.herdr.CapabilityStatus().OneStepLaunch {
		request.WorkspaceID, request.TabID = agent.WorkspaceID, agent.TabID
	}
	_, request, err := d.lifecycle.ValidateStart(request)
	if err != nil {
		return d.fail(req.requestID, "agent_clear", req.paneID, err.Error())
	}

	result := d.schedule(ctx, req, ScheduleOptions{
		Command:   d.command(req, "agent_clear", agentStartDeadline),
		LedgerKey: "clear\x00" + req.paneID + "\x00" + req.requestID,
	}, EffectFunc(func(effectCtx context.Context, token WorkerToken) EffectResult {
		if stale := d.paneSessionCurrent(token, req.requestID, "agent_clear"); stale != nil {
			return EffectResult{Result: stale}
		}
		replacement, err := d.lifecycle.Start(effectCtx, profile, request)
		if err != nil {
			return EffectResult{Result: d.failErr(req.requestID, "agent_clear", req.paneID, err)}
		}
		data := map[string]any{"pane_id": replacement.PaneID, "name": replacement.Name, "cwd": replacement.Cwd}
		if stale := d.paneSessionCurrent(token, req.requestID, "agent_clear"); stale != nil {
			return EffectResult{Result: stale}
		}
		if err := d.herdr.StopPane(effectCtx, req.paneID); err != nil {
			data["warning"] = "Replacement started, but the old pane could not be closed"
			result := completed(req.requestID, "agent_clear", req.paneID, data)
			result.Phase = "completed_with_warning"
			return EffectResult{Result: result, BumpGeneration: true}
		}
		return EffectResult{Result: completed(req.requestID, "agent_clear", req.paneID, data), BumpGeneration: true}
	}))
	if result.OK && !result.replayed {
		d.state.BumpGeneration(req.paneID)
		d.state.MarkTopologyChanged()
		if result.Phase != "completed_with_warning" {
			d.profiles.Forget(req.paneID)
		}
		d.activities.record("agent_clear", "cleared", "Cleared agent", req.paneID, req.requestID)
		d.wake()
	}
	return result
}

func (d *Dispatcher) command(req commandRequest, action string, budget time.Duration) Command {
	id := req.receipt
	if id == 0 {
		id = d.scheduler.NextCommandID()
	}
	return Command{
		ID:         id,
		RequestID:  req.requestID,
		ReceivedAt: req.receivedAt,
		Deadline:   req.receivedAt.Add(budget),
		Action:     action,
		PaneID:     req.paneID,
	}
}

type paneCall struct {
	action         string
	ledgerKey      string
	payloadHash    string
	data           any
	bumpGeneration bool
	run            func(context.Context) error
}

func (d *Dispatcher) callPane(ctx context.Context, req commandRequest, call paneCall) *CommandResult {
	return d.schedule(ctx, req, ScheduleOptions{
		Command:     d.command(req, call.action, commandDeadline),
		LedgerKey:   call.ledgerKey,
		PayloadHash: call.payloadHash,
	}, EffectFunc(func(effectCtx context.Context, token WorkerToken) EffectResult {
		if stale := d.paneSessionCurrent(token, req.requestID, call.action); stale != nil {
			return EffectResult{Result: stale}
		}
		if err := call.run(effectCtx); err != nil {
			return EffectResult{Result: d.failErr(req.requestID, call.action, req.paneID, err)}
		}
		return EffectResult{Result: completed(req.requestID, call.action, req.paneID, call.data), BumpGeneration: call.bumpGeneration}
	}))
}

func (d *Dispatcher) schedule(ctx context.Context, req commandRequest, options ScheduleOptions, runner EffectRunner) *CommandResult {
	if !options.RelayLevel {
		generation, active := d.state.PaneSession(options.PaneID)
		if generation != req.paneSession || !active {
			return d.fail(options.RequestID, options.Action, options.PaneID, ErrPaneReplaced.Error())
		}
		options.PaneGeneration = req.paneSession
	}
	checkedRunner := EffectFunc(func(effectCtx context.Context, token WorkerToken) EffectResult {
		effectCtx = herdr.WithDispatchCheck(effectCtx, func() error { return herdr.CheckDispatch(ctx) })
		if !options.RelayLevel {
			effectCtx = herdr.WithDispatchCheck(effectCtx, func() error { return d.paneSessionError(token) })
		}
		return runner.Run(effectCtx, token)
	})
	result, err := d.scheduler.ExecuteAdmitted(ctx, options, checkedRunner, req.admitted)
	switch {
	case err == nil && result != nil:
		return result
	case errors.Is(err, ErrConflict):
		return d.fail(options.RequestID, options.Action, options.PaneID, "A different response was already submitted")
	case errors.Is(err, ErrIngressFull), errors.Is(err, ErrClosed):
		return &CommandResult{RequestID: options.RequestID, Action: options.Action, OK: false, Phase: "not_started", Error: queueRejectedRetrySafe, PaneID: options.PaneID}
	case err != nil:
		return d.failErr(options.RequestID, options.Action, options.PaneID, err)
	default:
		return d.fail(options.RequestID, options.Action, options.PaneID, "Command failed")
	}
}

func (d *Dispatcher) fail(requestID, action, paneID, message string) *CommandResult {
	if action != "" {
		d.logger.Warn("command failed", "action", action, "request_id", requestID, "pane_id", paneID, "error", message)
		d.activities.record(action, "failed", failureSummary(action, message), paneID, requestID)
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
	class := classifyFailure(err)
	phase := class.phase()
	public := "Command failed"
	var data any
	switch class.kind {
	case failurePartiallyApplied:
		public = partiallyAppliedMessage
	case failureRefused:
		if class.transient {
			public = notSentRetrySafe
		} else {
			public = herdr.RefusalMessage(class.code)
			data = map[string]any{"code": class.code}
		}
	case failureCreatedTargetUnknown:
		public = "Herdr may have created an empty target; review Herdr before retrying"
	case failureDispatchedUnknown:
		public = "Command may have executed; review the agent before retrying"
	case failureNotStarted:
		public = notSentRetrySafe
	}
	if phase == "dispatched_unknown" {
		data = uncertainOutcomeData()
	}
	d.logger.Warn("command failed", "action", action, "request_id", requestID, "pane_id", paneID, "phase", phase, "error", err)
	if action != "" {
		d.activities.record(action, "failed", failureSummary(action, public), paneID, requestID)
	}
	return &CommandResult{RequestID: requestID, Action: action, OK: false, Phase: phase, Error: public, PaneID: paneID, Data: data}
}

func completed(requestID, action, paneID string, data any) *CommandResult {
	return &CommandResult{RequestID: requestID, Action: action, OK: true, Phase: "completed", PaneID: paneID, Data: data}
}

func (d *Dispatcher) RecordActivity(kind, status, summary, paneID, requestID string) {
	d.activities.record(kind, status, summary, paneID, requestID)
}

func (d *Dispatcher) RecordTransitionActivity(transition TransitionActivity) bool {
	return d.activities.recordTransition(transition)
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
	if d.activities.journal == nil {
		return finish(d.fail(requestID, "clear_activities", "", "Activity storage is unavailable"))
	}
	return d.activities.clear(func(err error) *CommandResult {
		if err != nil {
			return finish(d.fail(requestID, "clear_activities", "", "Activity history could not be cleared"))
		}
		return finish(completed(requestID, "clear_activities", "", nil))
	})
}

func (d *Dispatcher) HandleReadPane(ctx context.Context, request PaneReadRequest) map[string]any {
	if request.PaneID != "" {
		d.handleAcknowledge(request.RequestID, request.PaneID)
	}
	return d.panes.read(ctx, request)
}

func (d *Dispatcher) HandleProbePane(ctx context.Context, request PaneReadRequest) map[string]any {
	return d.panes.probe(ctx, request)
}

func (d *Dispatcher) wake() {
	if d.wakePoll != nil {
		d.wakePoll()
	}
}

func hashPayload(value any) string {
	data, _ := json.Marshal(value)
	// This is an in-memory conflict key, not an artifact integrity digest.
	return fmt.Sprintf("%x", data)
}
