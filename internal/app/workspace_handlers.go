package app

import (
	"context"
	"errors"
	"strings"

	"github.com/0cv/herdr-mobile-relay/internal/coordinator"
	"github.com/0cv/herdr-mobile-relay/internal/protocol"
	"github.com/0cv/herdr-mobile-relay/internal/transport"
	"github.com/0cv/herdr-mobile-relay/internal/workspace"
)

func (s *Server) handleTopology(commandCtx context.Context, client *transport.ClientConn, inbound protocol.Inbound, msg map[string]any, action string, auditedWrite bool, admitted func()) {
	// The dispatcher signals admitted() as soon as it holds the
	// topology ordering lock; the Herdr command itself must not
	// block the hub's global ordered ingress.
	result := s.dispatcher.HandleTopologyAdmitted(commandCtx, admitted, func(handlerCtx context.Context) *coordinator.CommandResult {
		switch action {
		case "workspace_create":
			return s.dispatcher.HandleWorkspaceCreate(handlerCtx, inbound.RequestID, inbound.Cwd, inbound.Label)
		case "workspace_rename":
			return s.dispatcher.HandleWorkspaceRename(handlerCtx, inbound.RequestID, inbound.WorkspaceID, inbound.Label)
		case "workspace_reorder":
			if len(inbound.WorkspaceIDs) > 0 {
				return s.dispatcher.HandleWorkspaceReorderBlock(
					handlerCtx,
					inbound.RequestID,
					inbound.WorkspaceIDs,
					inbound.BeforeWorkspaceID,
				)
			}
			return s.dispatcher.HandleWorkspaceReorder(handlerCtx, inbound.RequestID, inbound.WorkspaceID, inbound.InsertIndex)
		case "workspace_close":
			return s.dispatcher.HandleWorkspaceClose(
				handlerCtx,
				inbound.RequestID,
				inbound.WorkspaceID,
				inbound.CloseGroup,
				inbound.ExpectedWorkspaceIDs,
			)
		case "worktree_list":
			return s.dispatcher.HandleWorktreeList(handlerCtx, inbound.RequestID, inbound.WorkspaceID)
		case "worktree_create":
			return s.dispatcher.HandleWorktreeCreate(
				handlerCtx,
				inbound.RequestID,
				inbound.WorkspaceID,
				inbound.Branch,
				inbound.Base,
				inbound.Path,
				inbound.Label,
			)
		case "worktree_open":
			return s.dispatcher.HandleWorktreeOpen(
				handlerCtx,
				inbound.RequestID,
				inbound.WorkspaceID,
				inbound.Path,
				inbound.Branch,
				inbound.Label,
			)
		default:
			return s.dispatcher.HandleWorktreeRemove(
				handlerCtx,
				inbound.RequestID,
				inbound.WorkspaceID,
				inbound.Force,
			)
		}
	})
	s.sendResult(client, msg, action, auditedWrite, result)
}

func (s *Server) handleWorkspaceInspect(client *transport.ClientConn, inbound protocol.Inbound) {
	paneID := inbound.PaneID
	reply := s.reply(client, inbound.RequestID, inbound.Type, paneID)
	agent, exists := s.state.Agent(paneID)
	if paneID == "" || !exists {
		reply.fail("Agent pane not found")
		return
	}
	generation := s.state.Generation(paneID)
	cwd := strings.TrimSpace(agent.Cwd)
	var data any
	var inspectErr error
	switch inbound.Type {
	case "workspace_tree":
		data, inspectErr = workspace.TreeFor(cwd)
	case "workspace_file":
		data, inspectErr = workspace.ReadFile(cwd, inbound.Path)
	case "workspace_git_status":
		data, inspectErr = workspace.GitStatusFor(client.Context(), cwd)
	case "workspace_git_diff":
		data, inspectErr = workspace.GitDiffFor(client.Context(), cwd, inbound.Path)
	}
	currentAgent, currentExists := s.state.Agent(paneID)
	if !currentExists || s.state.Generation(paneID) != generation || strings.TrimSpace(currentAgent.Cwd) != cwd {
		reply.fail("Agent workspace changed during inspection")
		return
	}
	if inspectErr != nil {
		var public *workspace.Error
		message := "Workspace inspection failed"
		if errors.As(inspectErr, &public) {
			message = public.Message
		}
		reply.fail(message)
		return
	}
	reply.complete(data)
}
