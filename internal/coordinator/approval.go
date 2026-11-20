package coordinator

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/0cv/herdr-mobile-relay/internal/herdr"
	"github.com/0cv/herdr-mobile-relay/internal/protocol"
	"github.com/0cv/herdr-mobile-relay/internal/question"
)

const (
	approvalPollInterval = 350 * time.Millisecond
	approvalPollTimeout  = 5 * time.Second
	questionKeyDelay     = 150 * time.Millisecond
)

var errQuestionTextUnsupported = errors.New("This answer needs typed text, which this backend cannot send")

type approvalPayload struct {
	EventID     string
	Fingerprint string
	Choice      string
	Index       int
	Total       int
}

type questionPayload struct {
	InteractionID string
	Selected      []int
	OtherSelected bool
	OtherText     string
	Clarify       bool
	Navigation    string
}

func (d *Dispatcher) handleApproval(ctx context.Context, req commandRequest) *CommandResult {
	payload := approvalPayload{
		EventID:     req.in.EventID,
		Fingerprint: req.in.ApprovalFingerprint,
		Choice:      req.in.Choice,
		Total:       2,
	}
	if req.in.Index != nil {
		payload.Index = *req.in.Index
	}
	if req.in.Total != nil {
		payload.Total = *req.in.Total
	}
	if req.paneID == "" || payload.EventID == "" || payload.Fingerprint == "" || payload.Choice == "" {
		return d.fail(req.requestID, "approval", req.paneID, "Agent and exact approval identity are required")
	}
	if payload.Total < 2 || payload.Total > 20 || payload.Index < 0 || payload.Index >= payload.Total {
		return d.fail(req.requestID, "approval", req.paneID, "Approval choice is no longer available")
	}
	ledgerKey := approvalLedgerKey(req.paneID, payload.EventID)
	payloadHash := hashPayload(payload)
	agent, ok := d.state.Agent(req.paneID)
	if ok && agent.Status == "blocked" &&
		(agent.BlockedEventID == "" || agent.BlockedEventID != payload.EventID) {
		return d.fail(req.requestID, "approval", req.paneID, "This approval request is no longer current")
	}
	if ok && agent.Status == "blocked" &&
		(agent.AttentionKind != question.AttentionApproval ||
			len(agent.Options) != payload.Total ||
			agent.ApprovalFingerprint != payload.Fingerprint ||
			agent.Options[payload.Index] != payload.Choice) {
		return d.fail(req.requestID, "approval", req.paneID, "Approval choices are no longer available")
	}
	if !ok || agent.Status != "blocked" {
		replay, found, replayErr := d.scheduler.ReplayLedger(ledgerKey, payloadHash)
		switch {
		case errors.Is(replayErr, ErrConflict):
			return d.fail(req.requestID, "approval", req.paneID, "A different response was already submitted")
		case replayErr == nil && replay != nil:
			replay.RequestID = req.requestID
			return replay
		case replayErr == nil && found:
			// The matching operation is still in flight. Submit below to attach
			// this caller to its existing scheduler waiter set.
		default:
			return d.fail(req.requestID, "approval", req.paneID, "Agent is no longer waiting for approval")
		}
	}

	generation := d.state.Generation(req.paneID)

	result := d.schedule(ctx, req, ScheduleOptions{
		Command:     d.command(req, "approval", approvalDeadline),
		LedgerKey:   ledgerKey,
		PayloadHash: payloadHash,
	}, EffectFunc(func(effectCtx context.Context, token WorkerToken) EffectResult {
		current, ok := d.state.Agent(req.paneID)
		if !ok || !approvalCurrent(current, payload) || (agent != nil && agent.StrictAttention && !current.StrictAttention) {
			return EffectResult{Result: d.fail(req.requestID, "approval", req.paneID, "This approval request is no longer current")}
		}
		classification, err := d.readApproval(effectCtx, current)
		if err != nil {
			return EffectResult{Result: d.failErr(req.requestID, "approval", req.paneID, err)}
		}
		fingerprint := payload.Fingerprint
		if current.StrictAttention {
			fingerprint = current.LiveIdentity
		}
		if classification.Kind != question.AttentionApproval ||
			len(classification.Options) != payload.Total ||
			payload.Index >= len(classification.Options) ||
			classification.Options[payload.Index] != payload.Choice ||
			question.ApprovalFingerprint(classification) != fingerprint {
			return EffectResult{Result: d.fail(req.requestID, "approval", req.paneID, "Approval choices are no longer available")}
		}
		if stale := d.paneSessionCurrent(token, req.requestID, "approval"); stale != nil {
			return EffectResult{Result: stale}
		}
		after, ok := d.state.Agent(req.paneID)
		if !ok || after.Generation != current.Generation || !approvalCurrent(after, payload) ||
			after.StrictAttention != current.StrictAttention || after.LiveIdentity != current.LiveIdentity {
			return EffectResult{Result: d.fail(req.requestID, "approval", req.paneID, "This approval request is no longer current")}
		}
		if err := d.herdr.SendKeys(
			effectCtx,
			req.paneID,
			approvalKeys(payload.Index, classification.ApprovalFocus),
		); err != nil {
			return EffectResult{Result: d.failErr(req.requestID, "approval", req.paneID, err)}
		}
		accepted := completed(req.requestID, "approval", req.paneID, nil)
		accepted.Phase = "accepted"
		return EffectResult{Result: accepted}
	}))
	if !result.OK {
		return result
	}
	if result.replayed {
		return result
	}
	d.activities.record("approval", "approved", fmt.Sprintf("Approved option %d", payload.Index+1), req.paneID, req.requestID)
	d.wake()
	d.startWatcher(func(ctx context.Context) {
		d.watchApproval(ctx, req.requestID, req.paneID, payload.EventID, generation)
	})
	return result
}

func approvalCurrent(agent *AgentState, payload approvalPayload) bool {
	return agent.Status == "blocked" && !agent.ProbePending && agent.BlockedEventID == payload.EventID &&
		agent.AttentionKind == question.AttentionApproval && len(agent.Options) == payload.Total &&
		agent.ApprovalFingerprint == payload.Fingerprint && agent.Options[payload.Index] == payload.Choice
}

func (d *Dispatcher) readLive(ctx context.Context, agent *AgentState) (question.Classification, error) {
	readCtx, cancel := context.WithTimeout(ctx, ProbeTimeout)
	defer cancel()
	read, err := d.herdr.ReadPaneVisible(readCtx, agent.PaneID, question.VisibleRows, "ansi")
	if err != nil {
		return question.Classification{}, err
	}
	classification, reliable := question.ClassifyLive(string(read.Content), agent.Agent)
	if !reliable {
		return question.Classification{}, errors.New("The visible screen could not be verified")
	}
	return classification, nil
}

func (d *Dispatcher) readApproval(ctx context.Context, agent *AgentState) (question.Classification, error) {
	if agent.StrictAttention {
		return d.readLive(ctx, agent)
	}
	read, err := d.herdr.ReadPane(ctx, agent.PaneID, 80, "ansi")
	if err != nil {
		return question.Classification{}, err
	}
	return question.Classify(string(read.Content), agent.Agent), nil
}

func approvalKeys(target, current int) []string {
	distance := target - current
	key := "Down"
	if distance < 0 {
		key = "Up"
		distance = -distance
	}
	keys := make([]string, 0, distance+1)
	for range distance {
		keys = append(keys, key)
	}
	return append(keys, "Enter")
}

func (d *Dispatcher) watchApproval(parent context.Context, requestID, paneID, eventID string, generation int64) {
	ctx, cancel := context.WithTimeout(parent, approvalPollTimeout)
	defer cancel()
	ticker := time.NewTicker(approvalPollInterval)
	defer ticker.Stop()
	phase := "unconfirmed"
	for {
		select {
		case <-ctx.Done():
			d.commitAndBroadcastPhase(
				approvalLedgerKey(paneID, eventID),
				generation,
				requestID,
				"approval",
				paneID,
				phase,
			)
			return
		case <-ticker.C:
			if d.state.Generation(paneID) != generation {
				return
			}
			agent, ok := d.state.Agent(paneID)
			if !ok || agent.Status != "blocked" || agent.BlockedEventID == "" || agent.BlockedEventID != eventID {
				phase = "confirmed"
				d.commitAndBroadcastPhase(
					approvalLedgerKey(paneID, eventID),
					generation,
					requestID,
					"approval",
					paneID,
					phase,
				)
				return
			}
			if agent.AttentionKind != question.AttentionApproval {
				phase = "confirmed"
				d.commitAndBroadcastPhase(
					approvalLedgerKey(paneID, eventID),
					generation,
					requestID,
					"approval",
					paneID,
					phase,
				)
				return
			}
		}
	}
}

func (d *Dispatcher) handleQuestion(ctx context.Context, req commandRequest) *CommandResult {
	payload, err := decodeQuestionPayload(req.in)
	if err != nil {
		return d.fail(req.requestID, "answer_question", req.paneID, err.Error())
	}
	return d.submitQuestion(ctx, req, payload)
}

func (d *Dispatcher) handleClarifyQuestion(ctx context.Context, req commandRequest) *CommandResult {
	payload := questionPayload{
		InteractionID: req.in.InteractionID,
		Clarify:       true,
	}
	if payload.InteractionID == "" {
		return d.fail(req.requestID, "clarify_question", req.paneID, "Agent and question are required")
	}
	return d.submitQuestion(ctx, req, payload)
}

func (d *Dispatcher) handleNavigateQuestion(ctx context.Context, req commandRequest) *CommandResult {
	direction := req.in.Direction
	if direction != "previous" && direction != "next" {
		return d.fail(req.requestID, "navigate_question", req.paneID, "Question navigation is no longer available")
	}
	payload := questionPayload{InteractionID: req.in.InteractionID, Navigation: direction}
	if payload.InteractionID == "" {
		return d.fail(req.requestID, "navigate_question", req.paneID, "Question is required")
	}
	return d.submitQuestion(ctx, req, payload)
}

// A null element decodes to 0 in a typed list, so it is found in the raw message.
func selectionHasNull(raw json.RawMessage) bool {
	var selection struct {
		Indices []*int `json:"selected_indices"`
	}
	if json.Unmarshal(raw, &selection) != nil {
		return false
	}
	return slices.Contains(selection.Indices, nil)
}

func decodeQuestionPayload(in protocol.Inbound) (questionPayload, error) {
	payload := questionPayload{
		InteractionID: in.InteractionID,
		OtherText:     in.OtherText,
		OtherSelected: in.OtherSelected,
	}
	if payload.InteractionID == "" {
		return payload, fmt.Errorf("agent and question are required")
	}
	if len([]rune(payload.OtherText)) > promptMaxChars {
		return payload, fmt.Errorf("other answer is longer than 100,000 characters")
	}
	if in.SelectedIndices == nil || slices.ContainsFunc(in.SelectedIndices, func(index int) bool { return index < 0 }) || selectionHasNull(in.Raw) {
		return payload, fmt.Errorf("invalid question selection")
	}
	payload.Selected = slices.Clone(in.SelectedIndices)
	sort.Ints(payload.Selected)
	payload.Selected = slices.Compact(payload.Selected)
	if len(payload.Selected) == 0 && payload.OtherText == "" && !payload.OtherSelected {
		return payload, fmt.Errorf("choose an answer or enter an Other answer")
	}
	if payload.OtherText != "" && !payload.OtherSelected {
		return payload, fmt.Errorf("other text must be selected")
	}
	return payload, nil
}

func (d *Dispatcher) submitQuestion(ctx context.Context, req commandRequest, payload questionPayload) *CommandResult {
	if req.paneID == "" {
		return d.fail(req.requestID, "answer_question", req.paneID, "Agent is required")
	}
	action := "answer_question"
	if payload.Clarify {
		action = "clarify_question"
	}
	if payload.Navigation != "" {
		action = "navigate_question"
	}
	ledgerKey := questionOperationLedgerKey(action, req.paneID, payload.InteractionID, req.requestID)
	payloadHash := hashPayload(payload)
	var submittedInteraction *question.Interaction
	var submittedAgent *AgentState
	replay, found, replayErr := d.scheduler.ReplayLedger(ledgerKey, payloadHash)
	if errors.Is(replayErr, ErrConflict) {
		return d.fail(req.requestID, action, req.paneID, "A different response was already submitted")
	}

	agent, ok := d.state.Agent(req.paneID)
	if !ok || !questionCurrent(agent, payload.InteractionID) {
		switch {
		case replayErr == nil && replay != nil:
			replay.RequestID = req.requestID
			return replay
		case replayErr == nil && found:
			// Attach to the matching in-flight operation below.
		default:
			return d.fail(req.requestID, action, req.paneID, "Agent is no longer waiting for a question")
		}
	} else if replay != nil {
		replay.RequestID = req.requestID
		return replay
	}
	result := d.schedule(ctx, req, ScheduleOptions{
		Command:     d.command(req, action, questionDeadline),
		LedgerKey:   ledgerKey,
		PayloadHash: payloadHash,
	}, EffectFunc(func(effectCtx context.Context, token WorkerToken) EffectResult {
		current, ok := d.state.Agent(req.paneID)
		if !ok || !questionCurrent(current, payload.InteractionID) ||
			(agent != nil && strictQuestion(agent) && (!strictQuestion(current) || agent.BlockedEventID != current.BlockedEventID)) {
			return EffectResult{Result: d.fail(req.requestID, action, req.paneID, "The question changed before the answer was applied")}
		}
		interaction, err := d.readQuestion(effectCtx, current, strictQuestion(current))
		if err != nil {
			return EffectResult{Result: d.failErr(req.requestID, action, req.paneID, err)}
		}
		if interaction == nil || interaction.ID != payload.InteractionID {
			return EffectResult{Result: d.fail(req.requestID, action, req.paneID, "The question changed before the answer was applied")}
		}
		if err := validateQuestionPayload(payload, interaction); err != nil {
			return EffectResult{Result: d.fail(req.requestID, action, req.paneID, err.Error())}
		}
		if stale := d.paneSessionCurrent(token, req.requestID, action); stale != nil {
			return EffectResult{Result: stale}
		}
		after, ok := d.state.Agent(req.paneID)
		if !ok || after.Generation != current.Generation || !questionCurrent(after, payload.InteractionID) ||
			(strictQuestion(current) || strictQuestion(after)) &&
				(strictQuestion(after) != strictQuestion(current) || after.BlockedEventID != current.BlockedEventID) {
			return EffectResult{Result: d.fail(req.requestID, action, req.paneID, "The question changed before the answer was applied")}
		}
		if payload.OtherSelected {
			d.state.RecordCustomAnswer(req.paneID, interaction.Question, payload.OtherText)
		}
		submittedInteraction = interaction
		submittedAgent = current
		if err := d.executeQuestion(effectCtx, token, req.paneID, payload, interaction); err != nil {
			if errors.Is(err, errQuestionTextUnsupported) {
				result := d.fail(req.requestID, action, req.paneID, err.Error())
				result.Phase = "not_started"
				return EffectResult{Result: result}
			}
			if errors.Is(err, ErrPaneReplaced) {
				return EffectResult{Result: d.fail(req.requestID, action, req.paneID, ErrPaneReplaced.Error())}
			}
			return EffectResult{Result: d.failErr(req.requestID, action, req.paneID, err)}
		}
		accepted := completed(req.requestID, action, req.paneID, nil)
		accepted.Phase = "accepted"
		return EffectResult{Result: accepted}
	}))
	if result.OK && !result.replayed {
		d.wake()
		d.startWatcher(func(ctx context.Context) {
			d.watchQuestion(
				ctx,
				ledgerKey,
				req.requestID,
				action,
				req.paneID,
				interactionSnapshot(payload.InteractionID, submittedInteraction),
				payload.Navigation,
				submittedAgent,
			)
		})
	}
	return result
}

func strictQuestion(agent *AgentState) bool {
	return agent.StrictAttention || agent.ProbeEnabled && doneStatuses[agent.RawStatus]
}

func questionCurrent(agent *AgentState, interactionID string) bool {
	if agent.ProbePending || agent.ScreenAttention == ScreenCleared || (agent.Status != "blocked" && agent.Status != "done") {
		return false
	}
	return !agent.StrictAttention || agent.Status == "blocked" && agent.AttentionKind == question.AttentionQuestion && agent.InteractionID == interactionID
}

func (d *Dispatcher) readQuestion(ctx context.Context, agent *AgentState, strict bool) (*question.Interaction, error) {
	if strict {
		classification, err := d.readLive(ctx, agent)
		return classification.Interaction, err
	}
	read, err := d.herdr.ReadPane(ctx, agent.PaneID, 80, "ansi")
	if err != nil {
		return nil, err
	}
	return question.Parse(string(read.Content), agent.Agent), nil
}

func validateQuestionPayload(payload questionPayload, interaction *question.Interaction) error {
	switch {
	case payload.Navigation == "previous" && !interaction.CanGoBack:
		return fmt.Errorf("there is no previous question to open")
	case payload.Navigation == "next" &&
		(interaction.QuestionIndex == 0 || interaction.QuestionIndex >= interaction.QuestionTotal):
		return fmt.Errorf("there is no next question to open")
	case payload.Clarify && (!interaction.CanChat || interaction.Agent != "claude"):
		return fmt.Errorf("this question can no longer be discussed")
	case payload.Navigation != "", payload.Clarify:
		return nil
	}
	for _, index := range payload.Selected {
		if index < 0 || index >= len(interaction.Options) {
			return fmt.Errorf("question selection is no longer available")
		}
	}
	otherIsChoice := payload.OtherSelected &&
		(strings.TrimSpace(payload.OtherText) != "" || interaction.Other.AllowEmpty)
	if interaction.Other.Hidden && payload.OtherSelected {
		return fmt.Errorf("this question does not accept a custom answer")
	}
	if interaction.Kind == "single_select" && len(payload.Selected)+boolInt(otherIsChoice) != 1 {
		return fmt.Errorf("choose one answer or enter an Other answer")
	}
	return nil
}

func boolInt(value bool) int {
	if value {
		return 1
	}
	return 0
}

func (d *Dispatcher) executeQuestion(
	ctx context.Context,
	token WorkerToken,
	paneID string,
	payload questionPayload,
	interaction *question.Interaction,
) error {
	var dispatched bool
	keys := func(ks []string) error {
		err := d.sendQuestionKeysForSession(ctx, token, paneID, ks)
		if err == nil {
			dispatched = true
			return nil
		}
		if dispatched {
			return partiallyApplied("earlier question input was already applied", err)
		}
		return err
	}
	text := func(s string) error {
		if err := d.paneSessionError(token); err != nil {
			return err
		}
		err := d.herdr.SendText(ctx, paneID, s)
		if err == nil {
			dispatched = true
			return nil
		}
		if dispatched {
			return partiallyApplied("earlier question input was already applied", err)
		}
		return err
	}

	steps := question.PlanInput(interaction, question.InputIntent{
		Navigation:    payload.Navigation,
		Clarify:       payload.Clarify,
		Selected:      payload.Selected,
		OtherSelected: payload.OtherSelected,
		OtherText:     payload.OtherText,
	})
	for _, step := range steps {
		if step.Text != "" && !d.herdr.CapabilityStatus().Supports(herdr.FeatureManualTerminalInput) {
			return errQuestionTextUnsupported
		}
	}
	for index, step := range steps {
		if len(step.Keys) > 0 {
			if err := keys(step.Keys); err != nil {
				return err
			}
		}
		if step.Text != "" {
			if err := text(step.Text); err != nil {
				return err
			}
		}
		if index+1 < len(steps) {
			if err := contextDelay(ctx, questionKeyDelay); err != nil {
				return partiallyApplied("question input was only partially applied", err)
			}
		}
	}
	return nil
}

func (d *Dispatcher) sendQuestionKeysForSession(ctx context.Context, token WorkerToken, paneID string, keys []string) error {
	for index, key := range keys {
		if err := d.paneSessionError(token); err != nil {
			if index > 0 {
				return partiallyApplied("question input was only partially applied", err)
			}
			return err
		}
		if err := d.herdr.SendKeys(ctx, paneID, []string{key}); err != nil {
			if index > 0 {
				return partiallyApplied("question input was only partially applied", err)
			}
			return err
		}
		if index+1 < len(keys) {
			if err := contextDelay(ctx, questionKeyDelay); err != nil {
				return partiallyApplied("question input was only partially applied", err)
			}
		}
	}
	return nil
}

func contextDelay(ctx context.Context, delay time.Duration) error {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func (d *Dispatcher) watchQuestion(
	parent context.Context,
	ledgerKey, requestID, action, paneID string,
	original *question.Interaction,
	navigation string,
	submitted *AgentState,
) {
	generation := submitted.Generation
	strict := strictQuestion(submitted)
	ctx, cancel := context.WithTimeout(parent, approvalPollTimeout)
	defer cancel()
	ticker := time.NewTicker(approvalPollInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			d.commitAndBroadcastResult(ledgerKey, generation, &CommandResult{
				RequestID: requestID,
				Action:    action,
				OK:        false,
				Phase:     "unconfirmed",
				Error:     "The agent still shows the same question; try again",
				PaneID:    paneID,
			})
			return
		case <-ticker.C:
			if d.state.Generation(paneID) != generation {
				return
			}
			agent, ok := d.state.Agent(paneID)
			if !ok {
				d.finishQuestionWatch(ledgerKey, generation, requestID, action, paneID, original, navigation, nil)
				return
			}
			if strict && (agent.ScreenAttention == ScreenCleared || agent.Status == "working" || agent.BlockedEventID != submitted.BlockedEventID) {
				d.finishQuestionWatch(ledgerKey, generation, requestID, action, paneID, original, navigation, nil)
				return
			}
			if agent.ProbePending {
				continue
			}
			strictRead := strict || strictQuestion(agent)
			current, err := d.readQuestion(ctx, agent, strictRead)
			if err != nil {
				continue
			}
			after, ok := d.state.Agent(paneID)
			if !ok || after.Generation != generation {
				return
			}
			if (strictRead || strictQuestion(after)) && (!strictRead || after.ProbePending || after.Status != agent.Status ||
				after.ScreenAttention == ScreenCleared || after.BlockedEventID != agent.BlockedEventID ||
				after.InteractionID != agent.InteractionID && (current == nil || after.InteractionID != current.ID)) {
				continue
			}
			if current != nil {
				if text := strings.TrimSpace(current.Other.Text); text != "" {
					d.state.RecordCustomAnswer(paneID, current.Question, text)
				}
				question.FillCustomAnswers(current, d.state.CustomAnswers(paneID))
			}
			expectsQuestion := original != nil && (navigation != "" ||
				action == "answer_question" &&
					original.QuestionIndex > 0 &&
					original.QuestionIndex < original.QuestionTotal)
			if current == nil && expectsQuestion && agent.Status == "blocked" &&
				(agent.AttentionKind == "" ||
					agent.AttentionKind == question.AttentionUnknown ||
					agent.AttentionKind == question.AttentionQuestion) {
				continue
			}
			if current == nil || original == nil || current.ID != original.ID {
				d.finishQuestionWatch(ledgerKey, generation, requestID, action, paneID, original, navigation, current)
				return
			}
		}
	}
}

func interactionSnapshot(interactionID string, stateInteraction *question.Interaction) *question.Interaction {
	if stateInteraction != nil && stateInteraction.ID == interactionID {
		copy := *stateInteraction
		return &copy
	}
	return &question.Interaction{ID: interactionID}
}

func (d *Dispatcher) finishQuestionWatch(
	ledgerKey string,
	generation int64,
	requestID, action, paneID string,
	original *question.Interaction,
	navigation string,
	current *question.Interaction,
) {
	result := &CommandResult{
		RequestID: requestID,
		Action:    action,
		OK:        true,
		Phase:     "confirmed",
		PaneID:    paneID,
	}
	switch {
	case navigation != "":
		expected := original.QuestionIndex
		if navigation == "previous" {
			expected--
		} else {
			expected++
		}
		if current != nil && expected > 0 && current.QuestionIndex == expected {
			result.Phase = "navigated"
			result.Data = map[string]any{"interaction": current}
		} else {
			result.OK = false
			result.Phase = "failed"
			result.Error = "The agent opened an unexpected question; the screen was refreshed"
			if current != nil {
				result.Data = map[string]any{"interaction": current}
			}
		}
	case action == "answer_question" && current != nil &&
		original.QuestionIndex > 0 && current.QuestionIndex == original.QuestionIndex+1:
		result.Phase = "advanced"
		result.Data = map[string]any{"interaction": current}
	case action == "answer_question" && current != nil:
		result.OK = false
		result.Phase = "failed"
		result.Error = "The agent opened an unexpected question; the screen was refreshed"
		result.Data = map[string]any{"interaction": current}
	}
	if !d.commitAndBroadcastResult(ledgerKey, generation, result) || !result.OK {
		return
	}
	switch {
	case navigation == "previous":
		d.activities.record("question", "navigated", "Opened previous question", paneID, requestID)
	case navigation == "next":
		d.activities.record("question", "navigated", "Opened next question", paneID, requestID)
	case action == "answer_question":
		d.activities.record("question", "answered", "Answered question", paneID, requestID)
	}
}

func approvalLedgerKey(paneID, eventID string) string {
	return "approval\x00" + paneID + "\x00" + eventID
}

func questionOperationLedgerKey(action, paneID, interactionID, requestID string) string {
	switch action {
	case "navigate_question":
		return "question-navigation\x00" + paneID + "\x00" + interactionID + "\x00" + requestID
	case "clarify_question":
		return "question-clarification\x00" + paneID + "\x00" + interactionID + "\x00" + requestID
	default:
		return "question-answer\x00" + paneID + "\x00" + interactionID + "\x00" + requestID
	}
}

func (d *Dispatcher) commitAndBroadcastPhase(
	ledgerKey string,
	generation int64,
	requestID string,
	action string,
	paneID string,
	phase string,
) {
	result := &CommandResult{
		RequestID: requestID,
		Action:    action,
		OK:        phase == "confirmed",
		Phase:     phase,
		PaneID:    paneID,
	}
	if !d.commitAndBroadcastResult(ledgerKey, generation, result) {
		return
	}
}

func (d *Dispatcher) commitAndBroadcastResult(ledgerKey string, generation int64, result *CommandResult) bool {
	if !d.scheduler.UpdateLedgerResult(ledgerKey, generation, result) {
		return false
	}
	d.broadcastResult(result)
	return true
}

func (d *Dispatcher) broadcastResult(result *CommandResult) {
	if d.broadcast == nil {
		return
	}
	d.broadcast(CommandResultMessage(result))
}
