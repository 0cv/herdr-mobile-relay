package app

import (
	"encoding/base64"
	"time"

	"github.com/0cv/herdr-mobile-relay/internal/coordinator"
	"github.com/0cv/herdr-mobile-relay/internal/herdr"
	"github.com/0cv/herdr-mobile-relay/internal/transport"
)

const (
	inventorySnapshotDeadline   = 15 * time.Second
	maxInventorySnapshotClients = 1024
)

type inventorySnapshotRequest struct {
	nonce     string
	expiresAt time.Time
}

func validInventorySnapshotNonce(nonce string) bool {
	if len(nonce) != 22 {
		return false
	}
	decoded, err := base64.RawURLEncoding.Strict().DecodeString(nonce)
	return err == nil && len(decoded) == 16
}

// One bounded pending nonce per client. In-flight polls cannot consume it: the
// poller captures pending requests before starting its next inventory query.
func (s *Server) requestInventorySnapshot(client *transport.ClientConn, nonce string) {
	if client == nil || s.poller == nil || !validInventorySnapshotNonce(nonce) {
		return
	}
	now := time.Now()
	s.refreshMu.Lock()
	if s.snapshotRequests == nil {
		s.snapshotRequests = make(map[string]inventorySnapshotRequest)
	}
	for id, request := range s.snapshotRequests {
		if !now.Before(request.expiresAt) {
			delete(s.snapshotRequests, id)
		}
	}
	_, existing := s.snapshotRequests[client.ID()]
	if !existing && len(s.snapshotRequests) >= maxInventorySnapshotClients {
		s.refreshMu.Unlock()
		return
	}
	s.snapshotRequests[client.ID()] = inventorySnapshotRequest{
		nonce: nonce, expiresAt: now.Add(inventorySnapshotDeadline),
	}
	s.refreshMu.Unlock()
	s.poller.Wake()
}

// This callback runs only on the serial polling goroutine, never on inventory
// publications or event updates. Requests admitted during a query stay pending
// for the next poll. The closure holds at most one request per bounded client.
func (s *Server) beginRequestedInventorySnapshots() func([]*coordinator.AgentState, []herdr.Workspace, bool) {
	s.refreshMu.Lock()
	requests := s.snapshotRequests
	s.snapshotRequests = nil
	s.refreshMu.Unlock()
	if len(requests) == 0 {
		return nil
	}
	return func(_ []*coordinator.AgentState, _ []herdr.Workspace, ready bool) {
		for clientID, request := range requests {
			if !time.Now().Before(request.expiresAt) {
				continue
			}
			s.hub.SendBatchPreparedByID(clientID, func() []any {
				// Recheck inside the send barrier, which can itself be delayed.
				if !time.Now().Before(request.expiresAt) {
					return nil
				}
				// Coordinator state supplies real target generations and events
				// that landed during/after the successful poll. Do not use the
				// sticky presentation merge in agentView. Recheck readiness at
				// the send barrier so a later error cannot publish ready rows.
				fresh := s.state.InventorySnapshot()
				rows := fresh.Agents
				groups := fresh.Workspaces
				status := map[string]any{"state": "ready", "stale": false}
				if ready && fresh.Status["state"] == "ready" && fresh.Status["stale"] != true {
					s.projectAgentResources(rows)
				} else {
					rows = make([]*coordinator.AgentState, 0)
					groups = make([]herdr.Workspace, 0)
					status = map[string]any{"state": "error", "stale": true}
				}
				return []any{map[string]any{
					"type": "inventory_snapshot", "snapshot_request_id": request.nonce,
					"inventory": status, "agents": rows, "workspaces": groups,
				}}
			})
		}
	}
}
