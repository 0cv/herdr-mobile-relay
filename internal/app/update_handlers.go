package app

import (
	"context"
	"time"

	"github.com/0cv/herdr-mobile-relay/internal/protocol"
	"github.com/0cv/herdr-mobile-relay/internal/transport"
)

func (s *Server) handleUpdate(ctx context.Context, client *transport.ClientConn, inbound protocol.Inbound) {
	reply := s.reply(client, inbound.RequestID, inbound.Type, "")
	if s.cfg.Backend == "hc" {
		reply.fail("Updates are managed by the TUI relay, not the hc backend")
		return
	}
	switch inbound.Type {
	case "check_update":
		s.hub.Broadcast(map[string]any{"type": "update_status", "update": map[string]any{
			"state":            "checking",
			"current_version":  s.version,
			"current_revision": s.revision,
		}})
		updateState := s.updateM.Check(ctx)
		s.hub.Broadcast(map[string]any{"type": "update_status", "update": updateState})
		reply.complete(map[string]any{"update": updateState})
	case "install_update":
		deployAppFirst := s.appDeployM.Required()
		expectedAppOrigin := ""
		if deployAppFirst {
			if originErr := s.appDeployM.ValidateOrigin(inbound.ExpectedOrigin); originErr != nil {
				reply.failWithData(originErr.Error(), map[string]any{"update": s.updateM.State()})
				break
			}
			expectedAppOrigin = inbound.ExpectedOrigin
		}
		job, updateState, scheduleErr := s.updateM.Schedule(
			ctx,
			inbound.ExpectedVersion,
			inbound.ExpectedRevision,
			deployAppFirst,
			expectedAppOrigin,
		)
		if scheduleErr != nil {
			reply.failWithData(scheduleErr.Error(), map[string]any{"update": updateState})
			break
		}
		s.hub.Broadcast(map[string]any{"type": "update_status", "update": updateState})
		reply.schedule(map[string]any{"job": job, "update": updateState})
	case "deploy_app_update":
		job, deployState, scheduleErr := s.appDeployM.Schedule(ctx, inbound.ExpectedVersion, inbound.ExpectedRevision, inbound.ExpectedOrigin)
		if scheduleErr != nil {
			reply.failWithData(scheduleErr.Error(), map[string]any{"app_deploy": deployState})
			break
		}
		s.hub.Broadcast(map[string]any{"type": "app_deploy_status", "app_deploy": deployState})
		reply.schedule(map[string]any{"job": job, "app_deploy": deployState})
	}
}

func (s *Server) watchJobStates(ctx context.Context) {
	updateState := serializedState(s.updateM.State())
	deployState := serializedState(s.appDeployM.State())
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			nextUpdate := s.updateM.State()
			if serialized := serializedState(nextUpdate); serialized != updateState {
				updateState = serialized
				s.hub.Broadcast(map[string]any{"type": "update_status", "update": nextUpdate})
			}
			nextDeploy := s.appDeployM.State()
			if serialized := serializedState(nextDeploy); serialized != deployState {
				deployState = serialized
				s.hub.Broadcast(map[string]any{"type": "app_deploy_status", "app_deploy": nextDeploy})
			}
		}
	}
}

func (s *Server) updateCheckLoop(ctx context.Context) {
	timer := time.NewTimer(5 * time.Minute)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
			state := s.updateM.Check(ctx)
			s.hub.Broadcast(map[string]any{"type": "update_status", "update": state})
			timer.Reset(6 * time.Hour)
		}
	}
}
