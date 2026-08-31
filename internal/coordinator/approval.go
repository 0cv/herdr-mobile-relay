package coordinator

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/0cv/herdr-mobile-relay/internal/question"
)

const (
	approvalPollInterval = 350 * time.Millisecond
	approvalPollTimeout  = 5 * time.Second
	questionKeyDelay     = 150 * time.Millisecond
)

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

func (d *Dispatcher) handleApproval(ctx context.Context, receivedAt time.Time, requestID, paneID string, message map[string]any) *CommandResult {
	payload := approvalPayload{
		EventID:     stringValue(message, "event_id"),
		Fingerprint: stringValue(message, "approval_fingerprint"),
		Choice:      stringValue(message, "choice"),
		Index:       intValue(message["index"], 0),
		Total:       intValue(message["total"], 2),
	}
	if paneID == "" || payload.EventID == "" || payload.Fingerprint == "" || payload.Choice == "" {
		return d.fail(requestID, "approval", paneID, "Agent and exact approval identity are required")
	}
	if payload.Total < 2 || payload.Total > 20 || payload.Index < 0 || payload.Index >= payload.Total {
		return d.fail(requestID, "approval", paneID, "Approval choice is no longer available")
	}
	ledgerKey := approvalLedgerKey(paneID, payload.EventID)
	payloadHash := hashPayload(payload)
	agent, ok := d.state.Agent(paneID)
	if ok && agent.Status == "blocked" &&
		(agent.BlockedEventID == "" || agent.BlockedEventID != payload.EventID) {
		return d.fail(requestID, "approval", paneID, "This approval request is no longer current")
	}
	if ok && agent.Status == "blocked" &&
		(agent.AttentionKind != question.AttentionApproval ||
			len(agent.Options) != payload.Total ||
			agent.ApprovalFingerprint != payload.Fingerprint ||
			agent.Options[payload.Index] != payload.Choice) {
		return d.fail(requestID, "approval", paneID, "Approval choices are no longer available")
	}
	if !ok || agent.Status != "blocked" {
		replay, found, replayErr := d.scheduler.ReplayLedger(ledgerKey, payloadHash)
		switch {
		case errors.Is(replayErr, ErrConflict):
			return d.fail(requestID, "approval", paneID, "A different response was already submitted")
		case replayErr == nil && replay != nil:
			replay.RequestID = requestID
			return replay
		case replayErr == nil && found:
			// The matching operation is still in flight. Submit below to attach
			// this caller to its existing scheduler waiter set.
		default:
			return d.fail(requestID, "approval", paneID, "Agent is no longer waiting for approval")
		}
	}

	generation := d.state.Generation(paneID)
	if stale := d.waitTestGate(ctx, paneID, generation); stale != nil {
		stale.RequestID, stale.Action = requestID, "approval"
		return stale
	}

	result := d.schedule(ctx, ScheduleOptions{
		Command:     d.command(ctx, receivedAt, requestID, CommandApproval, paneID, approvalDeadline, payload),
		LedgerKey:   ledgerKey,
		PayloadHash: payloadHash,
	}, EffectFunc(func(effectCtx context.Context, token WorkerToken) EffectResult {
		current, ok := d.state.Agent(paneID)
		if !ok || current.Status != "blocked" ||
			current.BlockedEventID == "" || current.BlockedEventID != payload.EventID ||
			current.AttentionKind != question.AttentionApproval ||
			len(current.Options) != payload.Total ||
			current.ApprovalFingerprint != payload.Fingerprint ||
			current.Options[payload.Index] != payload.Choice {
			return EffectResult{Result: d.fail(requestID, "approval", paneID, "This approval request is no longer current")}
		}
		read, err := d.herdr.ReadPane(effectCtx, paneID, 80, "ansi")
		if err != nil {
			return EffectResult{Result: d.failErr(requestID, "approval", paneID, err)}
		}
		classification := question.Classify(string(read.Content), current.Agent)
		if classification.Kind != question.AttentionApproval ||
			len(classification.Options) != payload.Total ||
			payload.Index >= len(classification.Options) ||
			classification.Options[payload.Index] != payload.Choice ||
			question.ApprovalFingerprint(classification) != payload.Fingerprint {
			return EffectResult{Result: d.fail(requestID, "approval", paneID, "Approval choices are no longer available")}
		}
		if stale := d.paneSessionCurrent(token, requestID, "approval"); stale != nil {
			return EffectResult{Result: stale}
		}
		if err := d.herdr.SendKeys(
			effectCtx,
			paneID,
			approvalKeys(payload.Index, classification.ApprovalFocus),
		); err != nil {
			return EffectResult{Result: d.failErr(requestID, "approval", paneID, err)}
		}
		accepted := completed(requestID, "approval", paneID, nil)
		accepted.Phase = "accepted"
		return EffectResult{Result: accepted}
	}))
	if !result.OK {
		return result
	}
	if result.replayed {
		return result
	}
	d.recordActivity("approval", "approved", fmt.Sprintf("Approved option %d", payload.Index+1), paneID, requestID)
	d.wake()
	d.startWatcher(func(ctx context.Context) {
		d.watchApproval(ctx, requestID, paneID, payload.EventID, uint64(generation))
	})
	return result
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

func (d *Dispatcher) watchApproval(parent context.Context, requestID, paneID, eventID string, generation uint64) {
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
			if uint64(d.state.Generation(paneID)) != generation {
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

func (d *Dispatcher) handleQuestion(ctx context.Context, receivedAt time.Time, requestID, paneID string, message map[string]any) *CommandResult {
	payload, err := decodeQuestionPayload(message)
	if err != nil {
		return d.fail(requestID, "answer_question", paneID, err.Error())
	}
	return d.submitQuestion(ctx, receivedAt, requestID, paneID, payload)
}

func (d *Dispatcher) handleClarifyQuestion(ctx context.Context, receivedAt time.Time, requestID, paneID string, message map[string]any) *CommandResult {
	payload := questionPayload{
		InteractionID: stringValue(message, "interaction_id"),
		Clarify:       true,
	}
	if payload.InteractionID == "" {
		return d.fail(requestID, "clarify_question", paneID, "Agent and question are required")
	}
	return d.submitQuestion(ctx, receivedAt, requestID, paneID, payload)
}

func (d *Dispatcher) handleNavigateQuestion(ctx context.Context, receivedAt time.Time, requestID, paneID string, message map[string]any) *CommandResult {
	direction := stringValue(message, "direction")
	if direction != "previous" && direction != "next" {
		return d.fail(requestID, "navigate_question", paneID, "Question navigation is no longer available")
	}
	payload := questionPayload{InteractionID: stringValue(message, "interaction_id"), Navigation: direction}
	if payload.InteractionID == "" {
		return d.fail(requestID, "navigate_question", paneID, "Question is required")
	}
	return d.submitQuestion(ctx, receivedAt, requestID, paneID, payload)
}

func decodeQuestionPayload(message map[string]any) (questionPayload, error) {
	payload := questionPayload{
		InteractionID: stringValue(message, "interaction_id"),
		OtherText:     stringValue(message, "other_text"),
	}
	payload.OtherSelected, _ = message["other_selected"].(bool)
	if payload.InteractionID == "" {
		return payload, fmt.Errorf("agent and question are required")
	}
	if len([]rune(payload.OtherText)) > promptMaxChars {
		return payload, fmt.Errorf("other answer is longer than 100,000 characters")
	}
	raw, ok := message["selected_indices"].([]any)
	if !ok {
		if typed, typedOK := message["selected_indices"].([]int); typedOK {
			payload.Selected = append([]int(nil), typed...)
		} else {
			return payload, fmt.Errorf("invalid question selection")
		}
	} else {
		for _, value := range raw {
			number, ok := value.(float64)
			if !ok || number < 0 || number != float64(int(number)) {
				return payload, fmt.Errorf("invalid question selection")
			}
			payload.Selected = append(payload.Selected, int(number))
		}
	}
	sort.Ints(payload.Selected)
	payload.Selected = uniqueInts(payload.Selected)
	if len(payload.Selected) == 0 && payload.OtherText == "" && !payload.OtherSelected {
		return payload, fmt.Errorf("choose an answer or enter an Other answer")
	}
	if payload.OtherText != "" && !payload.OtherSelected {
		return payload, fmt.Errorf("other text must be selected")
	}
	return payload, nil
}

func (d *Dispatcher) submitQuestion(ctx context.Context, receivedAt time.Time, requestID, paneID string, payload questionPayload) *CommandResult {
	if paneID == "" {
		return d.fail(requestID, "answer_question", paneID, "Agent is required")
	}
	action := "answer_question"
	if payload.Clarify {
		action = "clarify_question"
	}
	if payload.Navigation != "" {
		action = "navigate_question"
	}
	ledgerKey := questionOperationLedgerKey(action, paneID, payload.InteractionID, requestID)
	payloadHash := hashPayload(payload)
	var submittedInteraction *question.Interaction
	replay, found, replayErr := d.scheduler.ReplayLedger(ledgerKey, payloadHash)
	if errors.Is(replayErr, ErrConflict) {
		return d.fail(requestID, action, paneID, "A different response was already submitted")
	}

	agent, ok := d.state.Agent(paneID)
	if !ok || (agent.Status != "blocked" && agent.Status != "done") {
		switch {
		case replayErr == nil && replay != nil:
			replay.RequestID = requestID
			return replay
		case replayErr == nil && found:
			// Attach to the matching in-flight operation below.
		default:
			return d.fail(requestID, action, paneID, "Agent is no longer waiting for a question")
		}
	} else if replay != nil {
		replay.RequestID = requestID
		return replay
	}
	result := d.schedule(ctx, ScheduleOptions{
		Command:     d.command(ctx, receivedAt, requestID, CommandQuestion, paneID, questionDeadline, payload),
		LedgerKey:   ledgerKey,
		PayloadHash: payloadHash,
	}, EffectFunc(func(effectCtx context.Context, token WorkerToken) EffectResult {
		current, ok := d.state.Agent(paneID)
		if !ok || (current.Status != "blocked" && current.Status != "done") {
			return EffectResult{Result: d.fail(requestID, action, paneID, "The question changed before the answer was applied")}
		}
		read, err := d.herdr.ReadPane(effectCtx, paneID, 80, "ansi")
		if err != nil {
			return EffectResult{Result: d.failErr(requestID, action, paneID, err)}
		}
		interaction := question.Parse(string(read.Content), current.Agent)
		if payload.OtherSelected && interaction != nil {
			d.state.RecordCustomAnswer(paneID, interaction.Question, payload.OtherText)
		}
		if interaction == nil || interaction.ID != payload.InteractionID {
			return EffectResult{Result: d.fail(requestID, action, paneID, "The question changed before the answer was applied")}
		}
		if err := validateQuestionPayload(payload, interaction); err != nil {
			return EffectResult{Result: d.fail(requestID, action, paneID, err.Error())}
		}
		copy := *interaction
		submittedInteraction = &copy
		if stale := d.paneSessionCurrent(token, requestID, action); stale != nil {
			return EffectResult{Result: stale}
		}
		if err := d.executeQuestion(effectCtx, token, paneID, payload, interaction); err != nil {
			if errors.Is(err, ErrPaneReplaced) {
				return EffectResult{Result: d.fail(requestID, action, paneID, ErrPaneReplaced.Error())}
			}
			return EffectResult{Result: d.failErr(requestID, action, paneID, err)}
		}
		accepted := completed(requestID, action, paneID, nil)
		accepted.Phase = "accepted"
		return EffectResult{Result: accepted}
	}))
	if result.OK && !result.replayed {
		d.wake()
		d.startWatcher(func(ctx context.Context) {
			d.watchQuestion(
				ctx,
				ledgerKey,
				requestID,
				action,
				paneID,
				interactionSnapshot(payload.InteractionID, submittedInteraction),
				payload.Navigation,
				uint64(d.state.Generation(paneID)),
			)
		})
	}
	return result
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

func (d *Dispatcher) sendQuestionKeys(ctx context.Context, paneID string, keys []string) error {
	return d.sendQuestionKeysForSession(ctx, WorkerToken{
		PaneID:      paneID,
		Generation:  uint64(d.state.Generation(paneID)),
		AllowAbsent: true,
	}, paneID, keys)
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
	generation uint64,
) {
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
			if uint64(d.state.Generation(paneID)) != generation {
				return
			}
			agent, ok := d.state.Agent(paneID)
			if !ok {
				d.finishQuestionWatch(ledgerKey, generation, requestID, action, paneID, original, navigation, nil)
				return
			}
			read, err := d.herdr.ReadPane(ctx, paneID, 80, "ansi")
			if err != nil {
				continue
			}
			current := question.Parse(string(read.Content), agent.Agent)
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
	generation uint64,
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
		d.recordActivity("question", "navigated", "Opened previous question", paneID, requestID)
	case navigation == "next":
		d.recordActivity("question", "navigated", "Opened next question", paneID, requestID)
	case action == "answer_question":
		d.recordActivity("question", "answered", "Answered question", paneID, requestID)
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
	generation uint64,
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

func (d *Dispatcher) commitAndBroadcastResult(ledgerKey string, generation uint64, result *CommandResult) bool {
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
	message := map[string]any{
		"type":       "command_result",
		"request_id": result.RequestID,
		"action":     result.Action,
		"ok":         result.OK,
		"phase":      result.Phase,
		"pane_id":    result.PaneID,
	}
	if result.Error != "" {
		message["error"] = result.Error
	}
	if result.Data != nil {
		message["data"] = result.Data
	}
	d.broadcast(message)
}

func uniqueInts(values []int) []int {
	if len(values) < 2 {
		return values
	}
	out := values[:1]
	for _, value := range values[1:] {
		if value != out[len(out)-1] {
			out = append(out, value)
		}
	}
	return out
}
