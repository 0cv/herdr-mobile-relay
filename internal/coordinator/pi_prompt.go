package coordinator

import (
	"context"
	"strings"
	"time"

	"github.com/0cv/herdr-mobile-relay/internal/herdr"
	"github.com/0cv/herdr-mobile-relay/internal/noecho"
)

func PiPromptEligible(agent *AgentState) bool {
	return agent != nil && strings.EqualFold(agent.Agent, "pi") && strings.EqualFold(strings.TrimSpace(agent.Status), "blocked") && (agent.AttentionKind == "" || agent.AttentionKind == "unknown") && agent.SessionID != ""
}

func (d *Dispatcher) HandlePiPrompt(ctx context.Context, requestID, paneID string, admitted func(), send func(context.Context) *CommandResult) *CommandResult {
	const action = "send_pi_prompt_unconfirmed"
	ctx = context.WithValue(ctx, admissionContextKey{}, admitted)
	return d.schedule(ctx, ScheduleOptions{Command: d.command(ctx, time.Now(), requestID, CommandPrompt, paneID, commandDeadline, nil)}, EffectFunc(func(effectCtx context.Context, token WorkerToken) EffectResult {
		reject := func(message string) EffectResult {
			return EffectResult{Result: &CommandResult{RequestID: requestID, Action: action, PaneID: paneID, Phase: "not_started", Error: message}}
		}
		agent, ok := d.state.Agent(paneID)
		if !ok || !PiPromptEligible(agent) {
			return reject("Pi prompt target is no longer blocked with unknown input")
		}
		read, err := d.herdr.ReadPaneVisible(effectCtx, paneID, 30, "text")
		if err != nil {
			return reject("Terminal input state could not be checked")
		}
		if _, secret := noecho.Match(string(read.Content)); secret {
			return reject("Hidden terminal input cannot be sent to Pi")
		}
		if err := herdr.CheckDispatch(effectCtx); err != nil {
			return reject("Pi prompt target changed")
		}
		return EffectResult{Result: send(effectCtx)}
	}))
}
