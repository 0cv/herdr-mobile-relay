package app

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/0cv/herdr-mobile-relay/internal/coordinator"
	"github.com/0cv/herdr-mobile-relay/internal/history"
	"github.com/0cv/herdr-mobile-relay/internal/noecho"
	"github.com/0cv/herdr-mobile-relay/internal/panesize"
	"github.com/0cv/herdr-mobile-relay/internal/protocol"
	"github.com/0cv/herdr-mobile-relay/internal/question"
	"github.com/0cv/herdr-mobile-relay/internal/transport"
)

func (s *Server) handleReadPane(ctx context.Context, client *transport.ClientConn, inbound protocol.Inbound) {
	request := s.paneReadRequest(client.ID(), inbound)
	s.stopPaneWatch(client.ID(), inbound.PaneID)
	resp := s.preparePaneResponse(request, s.dispatcher.HandleReadPane(ctx, request))
	resp["request_id"] = inbound.RequestID
	if inbound.Target != nil {
		resp["target"] = *inbound.Target
	}
	if unchanged := unchangedPaneResponse(inbound.ContentFingerprint, resp); unchanged != nil {
		s.hub.Send(client, unchanged)
		return
	}
	s.hub.Send(client, resp)
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

// preparePaneResponse annotates a pane frame with everything the phone needs
// to decide what it may offer the operator. The no-echo flag is computed last,
// on the content the phone will actually render, so full reads, deltas and the
// history-merged frames all agree on the tail.
func (s *Server) preparePaneResponse(request coordinator.PaneReadRequest, response map[string]any) map[string]any {
	response = s.classifyPaneResponse(request, response)
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

func (s *Server) classifyPaneResponse(request coordinator.PaneReadRequest, response map[string]any) map[string]any {
	content, ok := successfulPaneContent(response)
	if !ok {
		return response
	}
	paneID := request.PaneID
	agent, _ := s.state.Agent(paneID)
	agentName := ""
	if agent != nil {
		agentName = agent.Agent
	}
	classification := question.Classify(content, agentName)
	liveDialog := false
	if agent != nil && (agent.ProbeGoverned() || agent.StrictAttention) {
		live, _ := question.ClassifyLive(content, agentName)
		identity := coordinator.LiveIdentity(live)
		liveDialog = identity != ""
		if agent.StrictAttention && !agent.ProbePending && liveDialog && identity == agent.LiveIdentity && live.Kind == agent.AttentionKind {
			classification = classificationFromAgent(agent)
		} else if liveDialog || classification.Kind == question.AttentionQuestion || classification.Kind == question.AttentionApproval {
			classification = unknownPaneClassification()
		}
	}
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
	if liveDialog || classification.Interaction != nil || !isClaudeLike(agentName) ||
		agent != nil && (agent.ProbePending || agent.ScreenAttention == coordinator.ScreenWaiting) {
		return response
	}
	if viewportOnly, _ := response["viewport_only"].(bool); viewportOnly {
		return response
	}
	historyLimit := request.Lines
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

func unchangedPaneResponse(knownFingerprint string, response map[string]any) map[string]any {
	content, ok := successfulPaneContent(response)
	if !ok {
		return nil
	}
	fingerprint := paneFingerprint(content)
	response["content_fingerprint"] = fingerprint
	if knownFingerprint == "" || knownFingerprint != fingerprint {
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
