package tailscalecli

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
)

// NewDevelopmentManager is deliberately disabled. Real operations require the
// validated DevelopmentWorkflow construction path; caller-selected roots and
// persisted markers alone are not sufficient.
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
	manager.fixtureDevelopment = client != nil && !client.validateBinary
	manager.developmentHTTPSPort = DevelopmentHTTPSPort
	manager.developmentBackendPort = DevelopmentBackendPort
	manager.developmentPluginPort = DevelopmentPluginPort
	return manager, nil
}

// NewDevelopmentWorkflow validates the Go-owned isolated development layout
// before read-only CLI preflight, then retains the manager in this process. The
// manager repeats the layout and fixed-tuple checks before real operations. This
// is not a privilege boundary against deliberate same-user fabrication.
func NewDevelopmentWorkflow(ctx context.Context, developmentRoot, stateRoot, coordinationRoot, binary string) (*DevelopmentWorkflow, PreflightReport, error) {
	if runtime.GOOS != "darwin" || runtime.GOARCH != "arm64" {
		return nil, PreflightReport{}, ErrUnsupported
	}
	isolation, err := developmentIsolationFromEnvironment(developmentRoot, stateRoot, coordinationRoot)
	if err != nil {
		return nil, PreflightReport{}, err
	}
	client, err := NewClient(binary)
	if err != nil {
		return nil, PreflightReport{}, err
	}
	if client.binary != os.Getenv("HERDR_TAILSCALE_CLI_BIN") {
		return nil, PreflightReport{}, ErrWorkflowRequired
	}
	return newDevelopmentWorkflowWithIsolation(ctx, developmentRoot, stateRoot, coordinationRoot, client, isolation, false)
}

func newDevelopmentWorkflow(ctx context.Context, developmentRoot, stateRoot, coordinationRoot string, client *Client) (*DevelopmentWorkflow, PreflightReport, error) {
	if client == nil || client.profileOS != "darwin" || client.profileArch != "arm64" {
		return nil, PreflightReport{}, ErrUnsupported
	}
	if client.validateBinary {
		isolation, err := developmentIsolationFromEnvironment(developmentRoot, stateRoot, coordinationRoot)
		if err != nil {
			return nil, PreflightReport{}, err
		}
		return newDevelopmentWorkflowWithIsolation(ctx, developmentRoot, stateRoot, coordinationRoot, client, isolation, false)
	}
	return newDevelopmentWorkflowWithIsolation(ctx, developmentRoot, stateRoot, coordinationRoot, client, nil, true)
}

func newDevelopmentWorkflowWithIsolation(ctx context.Context, developmentRoot, stateRoot, coordinationRoot string, client *Client, isolation *developmentIsolation, fixture bool) (*DevelopmentWorkflow, PreflightReport, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if client == nil || client.profileOS != "darwin" || client.profileArch != "arm64" {
		return nil, PreflightReport{}, ErrUnsupported
	}
	if !fixture {
		if isolation == nil {
			return nil, PreflightReport{}, ErrWorkflowRequired
		}
		if err := isolation.validate(false, false); err != nil {
			return nil, PreflightReport{}, err
		}
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
	manager.developmentIsolation = isolation
	manager.fixtureMutations = fixture
	return &DevelopmentWorkflow{manager: manager, preflight: preflight}, preflight, nil
}
