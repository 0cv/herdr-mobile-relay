package coordinator

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/0cv/herdr-mobile-relay/internal/herdr"
	"github.com/0cv/herdr-mobile-relay/internal/question"
)

const (
	approvalPollInterval = 350 * time.Millisecond
	approvalPollTimeout  = 5 * time.Second
	questionKeyDelay     = 150 * time.Millisecond
)

type approvalPayload struct {
	EventID string
	Index   int
	Total   int
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
		EventID: stringValue(message, "event_id"),
		Index:   intValue(message["index"], 0),
		Total:   intValue(message["total"], 2),
	}
	if paneID == "" || payload.EventID == "" {
		return d.fail(requestID, "approval", paneID, "Agent and approval event are required")
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
	}, EffectFunc(func(effectCtx context.Context, _ WorkerToken) EffectResult {
		if current, ok := d.state.Agent(paneID); !ok || current.Status != "blocked" ||
			current.BlockedEventID == "" || current.BlockedEventID != payload.EventID {
			return EffectResult{Result: d.fail(requestID, "approval", paneID, "This approval request is no longer current")}
		}
		content, err := d.herdr.ReadPane(effectCtx, paneID, 80, "ansi")
		if err != nil {
			return EffectResult{Result: d.failErr(requestID, "approval", paneID, err)}
		}
		if question.LayoutHint(string(content)) {
			return EffectResult{Result: d.fail(requestID, "approval", paneID, "Use the question form for this request")}
		}
		if err := d.herdr.SendKeys(effectCtx, paneID, approvalKeys(payload.Index, payload.Total)); err != nil {
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
	go d.watchApproval(requestID, paneID, payload.EventID, uint64(generation))
	return result
}

func approvalKeys(index, total int) []string {
	if index == 0 {
		return []string{"Enter"}
	}
	if index == total-1 {
		return []string{"Escape"}
	}
	keys := make([]string, 0, index+1)
	for range index {
		keys = append(keys, "Down")
	}
	return append(keys, "Enter")
}

func (d *Dispatcher) watchApproval(requestID, paneID, eventID string, generation uint64) {
	ctx, cancel := context.WithTimeout(context.Background(), approvalPollTimeout)
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
		}
	}
}

func (d *Dispatcher) handleQuestion(ctx context.Context, receivedAt time.Time, requestID, paneID string, message map[string]any) *CommandResult {
	payload, err := decodeQuestionPayload(message)
	if err != nil {
		return d.fail(requestID, "question", paneID, err.Error())
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
		return d.fail(requestID, "question", paneID, "Agent is required")
	}
	action := "question"
	if payload.Clarify {
		action = "clarify_question"
	}
	if payload.Navigation != "" {
		action = "navigate_question"
	}
	ledgerKey := questionLedgerKey(paneID, payload.InteractionID)
	payloadHash := hashPayload(payload)
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
	} else {
		readCtx, cancel := context.WithDeadline(ctx, receivedAt.Add(questionDeadline))
		content, err := d.herdr.ReadPane(readCtx, paneID, 80, "ansi")
		cancel()
		if err != nil {
			return d.failErr(requestID, action, paneID, err)
		}
		interaction := question.Parse(string(content), agent.Agent)
		if interaction == nil || interaction.ID != payload.InteractionID {
			return d.fail(requestID, action, paneID, "The question changed before the answer was applied")
		}
		if err := validateQuestionPayload(payload, interaction); err != nil {
			return d.fail(requestID, action, paneID, err.Error())
		}
		if replay != nil {
			replay.RequestID = requestID
			return replay
		}
	}
	result := d.schedule(ctx, ScheduleOptions{
		Command:     d.command(ctx, receivedAt, requestID, CommandQuestion, paneID, questionDeadline, payload),
		LedgerKey:   ledgerKey,
		PayloadHash: payloadHash,
	}, EffectFunc(func(effectCtx context.Context, _ WorkerToken) EffectResult {
		current, ok := d.state.Agent(paneID)
		if !ok || (current.Status != "blocked" && current.Status != "done") {
			return EffectResult{Result: d.fail(requestID, action, paneID, "The question changed before the answer was applied")}
		}
		content, err := d.herdr.ReadPane(effectCtx, paneID, 80, "ansi")
		if err != nil {
			return EffectResult{Result: d.failErr(requestID, action, paneID, err)}
		}
		interaction := question.Parse(string(content), current.Agent)
		if interaction == nil || interaction.ID != payload.InteractionID {
			return EffectResult{Result: d.fail(requestID, action, paneID, "The question changed before the answer was applied")}
		}
		if err := validateQuestionPayload(payload, interaction); err != nil {
			return EffectResult{Result: d.fail(requestID, action, paneID, err.Error())}
		}
		if err := d.executeQuestion(effectCtx, paneID, payload, interaction); err != nil {
			return EffectResult{Result: d.failErr(requestID, action, paneID, err)}
		}
		accepted := completed(requestID, action, paneID, nil)
		accepted.Phase = "accepted"
		return EffectResult{Result: accepted}
	}))
	if result.OK && !result.replayed {
		d.recordActivity("question", "answered", "Answered question", paneID, requestID)
		d.wake()
		go d.watchQuestion(requestID, action, paneID, payload.InteractionID, uint64(d.state.Generation(paneID)))
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
	paneID string,
	payload questionPayload,
	interaction *question.Interaction,
) error {
	var dispatched bool
	keys := func(ks []string) error {
		err := d.sendQuestionKeys(ctx, paneID, ks)
		if err == nil {
			dispatched = true
			return nil
		}
		if dispatched && !errors.Is(err, herdr.ErrDispatchedUnknown) {
			return fmt.Errorf("%w: earlier question input was already applied: %w", herdr.ErrDispatchedUnknown, err)
		}
		return err
	}
	text := func(s string) error {
		err := d.herdr.SendText(ctx, paneID, s)
		if err == nil {
			dispatched = true
			return nil
		}
		if dispatched && !errors.Is(err, herdr.ErrDispatchedUnknown) {
			return fmt.Errorf("%w: earlier question input was already applied: %w", herdr.ErrDispatchedUnknown, err)
		}
		return err
	}

	switch {
	case payload.Navigation == "previous":
		return keys([]string{"Left"})
	case payload.Navigation == "next":
		return keys([]string{"Right"})
	case payload.Clarify:
		nav := navigationKeys(interaction, question.Focus{Kind: "chat"})
		return keys(append(nav, "Enter"))
	}

	if interaction.Agent == "codex" {
		if len(payload.Selected) > 0 {
			nav := navigationKeys(interaction, question.Focus{Kind: "option", Index: payload.Selected[0]})
			return keys(append(nav, "Enter"))
		}
		nav := navigationKeys(interaction, question.Focus{Kind: "option", Index: interaction.AllOptionCount - 1})
		if interaction.NotesActive {
			nav = nil
		}
		if payload.OtherText == "" {
			return keys(append(nav, "Enter"))
		}
		if !interaction.NotesActive {
			nav = append(nav, "Tab")
		}
		nav = append(nav, "Ctrl+U")
		if err := keys(nav); err != nil {
			return err
		}
		if err := text(payload.OtherText); err != nil {
			return err
		}
		return keys([]string{"Enter"})
	}

	if interaction.Kind == "single_select" {
		if len(payload.Selected) > 0 {
			nav := navigationKeys(interaction, question.Focus{Kind: "option", Index: payload.Selected[0]})
			return keys(append(nav, "Enter"))
		}
		target := question.Focus{Kind: "option", Index: interaction.AllOptionCount - 1}
		nav := navigationKeys(interaction, target)
		if err := keys(append(nav, "Ctrl+U")); err != nil {
			return err
		}
		if payload.OtherText != "" {
			if err := text(payload.OtherText); err != nil {
				return err
			}
		}
		return keys([]string{"Enter"})
	}

	current := *interaction
	for index, option := range current.Options {
		desired := containsInt(payload.Selected, index)
		if option.Selected == desired {
			continue
		}
		target := question.Focus{Kind: "option", Index: index}
		if err := keys(append(navigationKeys(&current, target), "Enter")); err != nil {
			return err
		}
		current.Focus = target
		current.Options[index].Selected = desired
	}
	otherTarget := question.Focus{Kind: "option", Index: current.AllOptionCount - 1}
	if current.Other.Text != payload.OtherText {
		if err := keys(append(navigationKeys(&current, otherTarget), "Ctrl+U")); err != nil {
			return err
		}
		current.Focus = otherTarget
		if payload.OtherText != "" {
			if err := text(payload.OtherText); err != nil {
				return err
			}
		}
	}
	if current.Other.Selected != payload.OtherSelected {
		if err := keys(append(navigationKeys(&current, otherTarget), "Enter")); err != nil {
			return err
		}
		current.Focus = otherTarget
	}
	submit := question.Focus{Kind: "submit"}
	return keys(append(navigationKeys(&current, submit), "Enter"))
}

func navigationKeys(interaction *question.Interaction, target question.Focus) []string {
	position := func(focus question.Focus) int {
		switch focus.Kind {
		case "option":
			return focus.Index
		case "submit":
			if interaction.Kind == "multi_select" {
				return interaction.AllOptionCount
			}
		case "chat":
			position := interaction.AllOptionCount
			if interaction.Kind == "multi_select" {
				position++
			}
			return position
		}
		return 0
	}
	distance := position(target) - position(interaction.Focus)
	key := "Down"
	if distance < 0 {
		key = "Up"
		distance = -distance
	}
	keys := make([]string, distance)
	for index := range keys {
		keys[index] = key
	}
	return keys
}

func (d *Dispatcher) sendQuestionKeys(ctx context.Context, paneID string, keys []string) error {
	for index, key := range keys {
		if err := d.herdr.SendKeys(ctx, paneID, []string{key}); err != nil {
			if index > 0 && !errors.Is(err, herdr.ErrDispatchedUnknown) {
				return fmt.Errorf("%w: question input was only partially applied: %w", herdr.ErrDispatchedUnknown, err)
			}
			return err
		}
		if index+1 < len(keys) {
			if err := contextDelay(ctx, questionKeyDelay); err != nil {
				return fmt.Errorf("%w: question input was only partially applied: %w", herdr.ErrDispatchedUnknown, err)
			}
		}
	}
	return nil
}

func containsInt(values []int, target int) bool {
	for _, value := range values {
		if value == target {
			return true
		}
	}
	return false
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

func (d *Dispatcher) watchQuestion(requestID, action, paneID, interactionID string, generation uint64) {
	ctx, cancel := context.WithTimeout(context.Background(), approvalPollTimeout)
	defer cancel()
	ticker := time.NewTicker(approvalPollInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			d.commitAndBroadcastPhase(
				questionLedgerKey(paneID, interactionID),
				generation,
				requestID,
				action,
				paneID,
				"unconfirmed",
			)
			return
		case <-ticker.C:
			if uint64(d.state.Generation(paneID)) != generation {
				return
			}
			agent, ok := d.state.Agent(paneID)
			if !ok {
				d.commitAndBroadcastPhase(
					questionLedgerKey(paneID, interactionID),
					generation,
					requestID,
					action,
					paneID,
					"confirmed",
				)
				return
			}
			content, err := d.herdr.ReadPane(ctx, paneID, 80, "ansi")
			if err != nil {
				continue
			}
			current := question.Parse(string(content), agent.Agent)
			if current == nil || current.ID != interactionID {
				d.commitAndBroadcastPhase(
					questionLedgerKey(paneID, interactionID),
					generation,
					requestID,
					action,
					paneID,
					"confirmed",
				)
				return
			}
		}
	}
}

func approvalLedgerKey(paneID, eventID string) string {
	return "approval\x00" + paneID + "\x00" + eventID
}

func questionLedgerKey(paneID, interactionID string) string {
	return "question\x00" + paneID + "\x00" + interactionID
}

func (d *Dispatcher) commitAndBroadcastPhase(
	ledgerKey string,
	generation uint64,
	requestID string,
	action string,
	paneID string,
	phase string,
) {
	if !d.scheduler.UpdateLedgerPhase(ledgerKey, generation, phase) {
		return
	}
	d.broadcastPhase(requestID, action, paneID, phase)
}

func (d *Dispatcher) broadcastPhase(requestID, action, paneID, phase string) {
	if d.broadcast == nil {
		return
	}
	d.broadcast(map[string]any{
		"type":       "command_result",
		"request_id": requestID,
		"action":     action,
		"ok":         phase == "confirmed" || phase == "unconfirmed",
		"phase":      phase,
		"pane_id":    paneID,
	})
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
