package app

import (
	"context"
	"errors"
	"time"

	"github.com/0cv/herdr-mobile-relay/internal/coordinator"
	"github.com/0cv/herdr-mobile-relay/internal/copyresponse"
	"github.com/0cv/herdr-mobile-relay/internal/question"
	"github.com/0cv/herdr-mobile-relay/internal/slashcmd"
	"github.com/0cv/herdr-mobile-relay/internal/transport"
)

type copyResponseRunner func(
	context.Context,
	string,
	slashcmd.CopyProfile,
	copyresponse.Pane,
	copyresponse.ClipboardReader,
	copyresponse.ClipboardWriter,
	int64,
	copyresponse.RevisionReader,
) (copyresponse.Result, error)

func (s *Server) copyAgentResponse(client *transport.ClientConn, requestID, paneID string) {
	reply := s.reply(client, requestID, "copy_agent_response", paneID)
	if paneID == "" {
		reply.fail("Agent is required")
		return
	}
	agent, ok := s.state.Agent(paneID)
	if !ok {
		reply.fail("Agent pane not found")
		return
	}
	if agent.Status == "working" {
		reply.fail("Agent is still working; wait for the current turn to finish")
		return
	}
	if message := copyBlockedMessage(agent); message != "" {
		reply.fail(message)
		return
	}
	if s.clipboardRead == nil || s.clipboardWrite == nil {
		reply.fail("Host clipboard is unavailable")
		return
	}
	generation := s.state.Generation(paneID)
	profileID := s.profiles.ResolvePane(paneID, agent.Agent)
	profile, ok := slashcmd.CopyProfileFor(profileID, agent.Agent)
	if !ok {
		reply.fail("Agent does not support response copying")
		return
	}
	s.copyMu.Lock()
	defer s.copyMu.Unlock()
	ctx, cancel := context.WithTimeout(client.Context(), 10*time.Second)
	defer cancel()
	runCopy := s.copyRunner
	if runCopy == nil {
		runCopy = copyresponse.Run
	}
	result, err := runCopy(
		ctx,
		paneID,
		profile,
		s.herdrC,
		s.clipboardRead,
		s.clipboardWrite,
		int64(agent.PaneRevision),
		s.currentPaneRevision,
	)
	if err != nil {
		s.logger.Warn("agent response copy failed", "pane_id", paneID, "error", err)
		reply.fail(copyResponseError(err))
		return
	}
	if s.state.Generation(paneID) != generation {
		reply.fail("The agent pane was replaced while the response was being copied")
		return
	}
	reply.complete(map[string]any{
		"text":   result.Text,
		"source": result.Source,
		"chars":  result.Chars,
		"lines":  result.Lines,
	})
}

func copyBlockedMessage(agent *coordinator.AgentState) string {
	if agent == nil || agent.Status != "blocked" {
		return ""
	}
	switch agent.AttentionKind {
	case question.AttentionQuestion:
		return "Agent is waiting for an answer"
	case question.AttentionApproval:
		return "Agent is waiting for approval"
	default:
		return ""
	}
}

func copyResponseError(err error) string {
	switch {
	case errors.Is(err, copyresponse.ErrComposerBusy):
		return "The agent composer is busy; finish or clear the current prompt first"
	case errors.Is(err, copyresponse.ErrPickerOpen):
		return "The agent already has a copy menu open; close it and try again"
	case errors.Is(err, copyresponse.ErrStaleOutput):
		return "The copied response changed before it could be read; try again"
	case errors.Is(err, copyresponse.ErrNoCopy):
		return "The agent did not confirm a copied response; try again"
	case errors.Is(err, context.DeadlineExceeded):
		return "Copying the agent response timed out; try again"
	default:
		return "Could not copy the agent response; try again"
	}
}
