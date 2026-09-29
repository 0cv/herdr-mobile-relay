package tailscalecli

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func writeDevelopmentRootMarkerForTest(t *testing.T, root, state, coordination string) {
	t.Helper()
	marker := []byte("HERDR_DEV_TAILSCALE_CLI_ROOT=1\n" +
		"HERDR_DEV_TAILSCALE_CLI_STATE_ROOT=" + state + "\n" +
		"HERDR_DEV_TAILSCALE_CLI_COORDINATION_ROOT=" + coordination + "\n")
	path := filepath.Join(root, developmentRootMarker)
	if err := os.WriteFile(path, marker, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0o600); err != nil {
		t.Fatal(err)
	}
}

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
	writeDevelopmentRootMarkerForTest(t, root, state, coordination)
	return root, state, coordination
}

func developmentManagerFixtureClient() *Client {
	return newTestClientForPlatform("/fixture/tailscale", func(context.Context, string, ...string) (commandResult, error) {
		return commandResult{}, errors.New("unexpected CLI invocation")
	}, "darwin", "arm64")
}

func TestNewDevelopmentManagerRequiresExactMarkedPrivateRoots(t *testing.T) {
	root, state, coordination := developmentManagerFixturePaths(t)
	manager, err := NewDevelopmentManager(root, state, coordination, developmentManagerFixtureClient())
	if err != nil {
		t.Fatal(err)
	}
	if manager.developmentRoot != root {
		t.Fatalf("development root binding = %q, want %q", manager.developmentRoot, root)
	}

	otherState := filepath.Join(t.TempDir(), "registration")
	if err := os.Mkdir(otherState, 0o700); err != nil {
		t.Fatal(err)
	}
	otherCoordination := filepath.Join(t.TempDir(), "coordination")
	if err := os.Mkdir(otherCoordination, 0o700); err != nil {
		t.Fatal(err)
	}
	for _, paths := range [][3]string{
		{root, otherState, coordination},
		{root, state, otherCoordination},
		{root, state, root},
	} {
		if _, err := NewDevelopmentManager(paths[0], paths[1], paths[2], developmentManagerFixtureClient()); !errors.Is(err, ErrPermissionDenied) {
			t.Errorf("unbound development roots accepted (%q, %q, %q): %v", paths[0], paths[1], paths[2], err)
		}
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
