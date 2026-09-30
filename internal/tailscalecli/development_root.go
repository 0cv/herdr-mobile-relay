package tailscalecli

import (
	"context"
	"path/filepath"
)

// NewDevelopmentManager is deliberately disabled: a manager is an operational
// capability and may only be created by a process-local DevelopmentWorkflow.
// Callers must not turn private paths or persisted markers into authorization.
func NewDevelopmentManager(string, string, string, *Client) (*Manager, error) {
	return nil, ErrWorkflowRequired
}

func newDevelopmentManager(developmentRoot, stateRoot, coordinationRoot string, client *Client) (*Manager, error) {
	root, err := validatePrivateDirectory(developmentRoot)
	if err != nil || filepath.Clean(developmentRoot) != root {
		return nil, ErrPermissionDenied
	}
	state, err := validatePrivateDirectory(stateRoot)
	if err != nil || filepath.Clean(stateRoot) != state || state != filepath.Join(root, "registration") {
		return nil, ErrPermissionDenied
	}
	coordination, err := validatePrivateDirectory(coordinationRoot)
	if err != nil || filepath.Clean(coordinationRoot) != coordination || pathsOverlap(root, coordination) {
		return nil, ErrPermissionDenied
	}
	manager, err := NewManager(state, coordination, client)
	if err != nil {
		return nil, err
	}
	manager.developmentRoot = root
	manager.developmentHTTPSPort = DevelopmentHTTPSPort
	manager.developmentBackendPort = DevelopmentBackendPort
	manager.developmentPluginPort = DevelopmentPluginPort
	return manager, nil
}

// NewDevelopmentWorkflow performs a fresh read-only profile preflight and then
// binds the real-operation manager to this process-local, non-serializable
// handle. The fixed tuple is enforced again by every manager operation.
func NewDevelopmentWorkflow(ctx context.Context, developmentRoot, stateRoot, coordinationRoot, binary string) (*DevelopmentWorkflow, PreflightReport, error) {
	client, err := NewClient(binary)
	if err != nil {
		return nil, PreflightReport{}, err
	}
	return newDevelopmentWorkflow(ctx, developmentRoot, stateRoot, coordinationRoot, client)
}

func newDevelopmentWorkflow(ctx context.Context, developmentRoot, stateRoot, coordinationRoot string, client *Client) (*DevelopmentWorkflow, PreflightReport, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if client == nil || client.profileOS != "darwin" || client.profileArch != "arm64" {
		return nil, PreflightReport{}, ErrUnsupported
	}
	preflight, err := client.Preflight(ctx, DevelopmentHTTPSPort)
	if err != nil {
		return nil, PreflightReport{}, err
	}
	if preflight.Profile != ProfileAppStoreSupplied || !preflight.DevelopmentQualificationEnabled ||
		preflight.HTTPSPort != DevelopmentHTTPSPort {
		return nil, preflight, ErrUnsupported
	}
	manager, err := newDevelopmentManager(developmentRoot, stateRoot, coordinationRoot, client)
	if err != nil {
		return nil, preflight, err
	}
	return &DevelopmentWorkflow{manager: manager, preflight: preflight}, preflight, nil
}
