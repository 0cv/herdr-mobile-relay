package app

import (
	"fmt"
	"net"
	"strconv"

	"github.com/0cv/herdr-mobile-relay/internal/coordinator"
)

// startUDPListener binds the managed UDP event listener. For a managed run the
// plugin UDP port is part of the relay contract, so a bind failure is fatal and
// must abort Run before readiness. Legacy runs keep the previous degraded
// behavior: record a safe error, warn, and continue without a listener.
func (s *Server) startUDPListener() error {
	udpAddr := net.JoinHostPort("127.0.0.1", strconv.Itoa(s.cfg.PluginPort))
	udpListener, err := coordinator.NewUDPListener(udpAddr, s.state, s.cfg.SocketPath, s.logger)
	if err != nil {
		if s.cfg.ManagedRunID != "" {
			return fmt.Errorf("managed UDP event listener unavailable on %s: %w", udpAddr, err)
		}
		s.recordSafeError("UDP event listener unavailable", err)
		s.logger.Warn("udp listener unavailable", "error", err)
		return nil
	}
	s.udp = udpListener
	s.udp.SetOnDirty(func() { s.poller.Wake() })
	s.udp.SetOnChange(func(agent *coordinator.AgentState) {
		s.broadcastCommitted(map[string]any{
			"type":           "agent_update",
			"pane_id":        agent.PaneID,
			"raw_pane_id":    agent.RawPaneID,
			"status":         agent.Status,
			"agent":          agent.Agent,
			"tab_id":         agent.TabID,
			"tab_label":      agent.TabLabel,
			"tab_number":     agent.TabNumber,
			"workspace_id":   agent.WorkspaceID,
			"cwd":            agent.Cwd,
			"project":        agent.Project,
			"host":           agent.Host,
			"session":        agent.Session,
			"session_name":   agent.SessionName,
			"updated_at":     agent.UpdatedAt,
			"event_id":       agent.BlockedEventID,
			"attention_kind": agent.AttentionKind,
			"pane_revision":  agent.StateRevision,
		})
		s.poller.Wake()
	})
	return nil
}
