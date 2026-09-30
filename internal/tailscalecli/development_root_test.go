package tailscalecli

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func developmentManagerFixturePaths(t *testing.T) (root, state, coordination string) {
	t.Helper()
	base, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	root = filepath.Join(base, "development")
	state = filepath.Join(root, "registration")
	coordination = filepath.Join(base, "coordination")
	for _, path := range []string{root, state, coordination} {
		if err := os.Mkdir(path, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	return root, state, coordination
}

func developmentManagerFixtureClient() *Client {
	return newTestClientForPlatform("/fixture/tailscale", func(context.Context, string, ...string) (commandResult, error) {
		return commandResult{}, errors.New("unexpected CLI invocation")
	}, "darwin", "arm64")
}

func TestDevelopmentManagerRequiresProcessLocalWorkflow(t *testing.T) {
	root, state, coordination := developmentManagerFixturePaths(t)
	client := developmentManagerFixtureClient()
	if _, err := NewDevelopmentManager(root, state, coordination, client); !errors.Is(err, ErrWorkflowRequired) {
		t.Fatalf("standalone manager constructor returned an operation capability: %v", err)
	}
	manager, err := newDevelopmentManager(root, state, coordination, client)
	if err != nil {
		t.Fatal(err)
	}
	if manager.developmentRoot != root || manager.developmentHTTPSPort != DevelopmentHTTPSPort ||
		manager.developmentBackendPort != DevelopmentBackendPort || manager.developmentPluginPort != DevelopmentPluginPort {
		t.Fatalf("workflow manager tuple = root %q, HTTPS %d, backend %d, plugin %d", manager.developmentRoot,
			manager.developmentHTTPSPort, manager.developmentBackendPort, manager.developmentPluginPort)
	}

	otherStateParent, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	otherState := filepath.Join(otherStateParent, "registration")
	if err := os.Mkdir(otherState, 0o700); err != nil {
		t.Fatal(err)
	}
	otherCoordinationParent, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	otherCoordination := filepath.Join(otherCoordinationParent, "coordination")
	if err := os.Mkdir(otherCoordination, 0o700); err != nil {
		t.Fatal(err)
	}
	for _, paths := range [][3]string{
		{root, otherState, coordination},
		{root, state, root},
	} {
		if _, err := newDevelopmentManager(paths[0], paths[1], paths[2], client); !errors.Is(err, ErrPermissionDenied) {
			t.Errorf("unsafe development roots accepted (%q, %q, %q): %v", paths[0], paths[1], paths[2], err)
		}
	}
	alternate, err := newDevelopmentManager(root, state, otherCoordination, client)
	if err != nil || alternate.coordinationRoot != otherCoordination {
		t.Fatalf("workflow did not bind its selected private coordination root: manager=%+v err=%v", alternate, err)
	}
}

func TestUnboundManagerCannotUseRealDevelopmentScope(t *testing.T) {
	fixture := newFakeCLI(t)
	base, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	stateRoot := filepath.Join(base, "registration")
	coordinationRoot := filepath.Join(base, "coordination")
	for _, root := range []string{stateRoot, coordinationRoot} {
		if err := os.Mkdir(root, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	manager, err := NewManager(stateRoot, coordinationRoot,
		newTestClientForPlatform("/fixture path/tailscale", fixture.run, "darwin", "arm64"))
	if err != nil {
		t.Fatal(err)
	}
	if err := manager.Publish(context.Background(), fixtureRequest(true)); !errors.Is(err, ErrUnsupported) {
		t.Fatalf("unbound development manager publish = %v", err)
	}
	if len(fixture.calls) != 0 {
		t.Fatalf("unbound development manager reached CLI: %v", fixture.calls)
	}
}
