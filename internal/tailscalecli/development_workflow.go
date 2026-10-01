package tailscalecli

import (
	"context"
	"path/filepath"
)

// DevelopmentWorkflow is the in-process operation handle for the foreground
// CLI-backed development path. A caller can request one only after the Go
// profile and isolation checks pass; this is not a privilege boundary against a
// deliberately fabricating process running as the same user.
type DevelopmentWorkflow struct {
	manager   *Manager
	preflight PreflightReport
}

func (w *DevelopmentWorkflow) Preflight() PreflightReport {
	if w == nil {
		return PreflightReport{}
	}
	return w.preflight
}

// ValidateRuntimeBinding is used at the app/config boundary. It binds the
// server to the same roots and CLI selected by this workflow and rejects any
// port tuple other than the profile's authorized values.
func (w *DevelopmentWorkflow) ValidateRuntimeBinding(root, stateRoot, coordinationRoot, binary, scope, installationID, origin string, httpsPort, backendPort, pluginPort int) error {
	if w == nil || w.manager == nil ||
		(!w.manager.fixtureMutations && w.manager.developmentIsolation == nil) {
		return ErrWorkflowRequired
	}
	if w.manager.developmentIsolation != nil {
		if err := w.manager.developmentIsolation.validate(false, false); err != nil {
			return err
		}
	}
	if filepath.Clean(root) != w.manager.developmentRoot ||
		filepath.Clean(stateRoot) != w.manager.stateRoot ||
		filepath.Clean(coordinationRoot) != w.manager.coordinationRoot ||
		binary != w.manager.client.binary || scope != "development" ||
		!validLabel(installationID) ||
		origin != w.preflight.Origin || httpsPort != DevelopmentHTTPSPort ||
		backendPort != DevelopmentBackendPort || pluginPort != DevelopmentPluginPort {
		return ErrWorkflowRequired
	}
	return nil
}

func (w *DevelopmentWorkflow) ReserveBackendPort(ctx context.Context, installationID, nodeID, origin, reservationID string) error {
	if err := w.requireBoundIdentity(installationID, nodeID, origin); err != nil {
		return err
	}
	return w.manager.ReserveBackendPort(ctx, installationID, "development", nodeID, origin,
		DevelopmentHTTPSPort, DevelopmentBackendPort, reservationID)
}

func (w *DevelopmentWorkflow) ReleaseBackendPort(ctx context.Context, installationID, nodeID, origin, reservationID string, serviceStopped bool) error {
	if err := w.requireBoundIdentity(installationID, nodeID, origin); err != nil {
		return err
	}
	return w.manager.ReleaseBackendPort(ctx, installationID, "development", nodeID, origin,
		DevelopmentHTTPSPort, DevelopmentBackendPort, reservationID, serviceStopped)
}

func (w *DevelopmentWorkflow) VerifyRegisteredRoute(ctx context.Context, scope, installationID, origin string, httpsPort, backendPort int) (RouteStatus, error) {
	if scope != "development" || httpsPort != DevelopmentHTTPSPort || backendPort != DevelopmentBackendPort {
		return RouteStatus{}, ErrWorkflowRequired
	}
	if err := w.requireBoundIdentity(installationID, w.preflight.NodeID, origin); err != nil {
		return RouteStatus{}, err
	}
	return w.manager.VerifyRegisteredRoute(ctx, "development", installationID, origin,
		DevelopmentHTTPSPort, DevelopmentBackendPort)
}

func (w *DevelopmentWorkflow) Recover(ctx context.Context, installationID, origin string) (RecoveryReport, error) {
	if err := w.requireBoundIdentity(installationID, w.preflight.NodeID, origin); err != nil {
		return RecoveryReport{}, err
	}
	return w.manager.Recover(ctx, "development", installationID, origin,
		DevelopmentHTTPSPort, DevelopmentBackendPort)
}

func (w *DevelopmentWorkflow) Publish(ctx context.Context, request PublishRequest) error {
	if w == nil || w.manager == nil || request.Scope != "development" || request.BackendLease == nil ||
		request.ExpectedNodeID != w.preflight.NodeID || request.Origin != w.preflight.Origin ||
		request.HTTPSPort != DevelopmentHTTPSPort || request.BackendPort != DevelopmentBackendPort {
		return ErrWorkflowRequired
	}
	return w.manager.Publish(ctx, request)
}

func (w *DevelopmentWorkflow) Unpublish(ctx context.Context, consent Consent) error {
	if w == nil || w.manager == nil {
		return ErrWorkflowRequired
	}
	return w.manager.Unpublish(ctx, consent)
}

func (w *DevelopmentWorkflow) Reconcile(ctx context.Context, installationID, origin string, consent Consent) error {
	if err := w.requireBoundIdentity(installationID, w.preflight.NodeID, origin); err != nil {
		return err
	}
	return w.manager.Reconcile(ctx, "development", installationID, origin,
		DevelopmentHTTPSPort, DevelopmentBackendPort, consent)
}

func (w *DevelopmentWorkflow) AbandonMissing(ctx context.Context, installationID, origin string, consent Consent) error {
	if err := w.requireBoundIdentity(installationID, w.preflight.NodeID, origin); err != nil {
		return err
	}
	return w.manager.AbandonMissing(ctx, "development", installationID, origin,
		DevelopmentHTTPSPort, DevelopmentBackendPort, consent)
}

func (w *DevelopmentWorkflow) RepairMissing(ctx context.Context, request PublishRequest) error {
	if w == nil || w.manager == nil || request.Scope != "development" || request.BackendLease == nil ||
		request.ExpectedNodeID != w.preflight.NodeID || request.Origin != w.preflight.Origin ||
		request.HTTPSPort != DevelopmentHTTPSPort || request.BackendPort != DevelopmentBackendPort {
		return ErrWorkflowRequired
	}
	return w.manager.RepairMissing(ctx, request)
}

func (w *DevelopmentWorkflow) requireBoundIdentity(installationID, nodeID, origin string) error {
	if w == nil || w.manager == nil || installationID == "" ||
		nodeID != w.preflight.NodeID || origin != w.preflight.Origin {
		return ErrWorkflowRequired
	}
	return nil
}
