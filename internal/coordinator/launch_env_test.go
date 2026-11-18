package coordinator

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/0cv/herdr-mobile-relay/internal/herdr"
)

type launchEnv struct {
	dir, home, cwd, config, ownership string
}

// newLaunchEnv isolates HOME, the config directory and PATH, and puts an
// executable stub on PATH for each profile ID so profile discovery finds it.
func newLaunchEnv(t *testing.T, profileIDs ...string) launchEnv {
	t.Helper()
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	env := launchEnv{
		dir:       dir,
		home:      filepath.Join(dir, "home"),
		config:    filepath.Join(dir, "config"),
		ownership: filepath.Join(dir, "runtime", "profile-ownership.json"),
	}
	env.cwd = filepath.Join(env.home, "project")
	bin := filepath.Join(dir, "bin")
	for _, path := range []string{env.cwd, filepath.Join(env.config, "herdr"), bin} {
		if err := os.MkdirAll(path, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("HOME", env.home)
	t.Setenv("XDG_CONFIG_HOME", env.config)
	t.Setenv("PATH", bin)
	profilesINI := "[profiles]\n"
	for _, id := range profileIDs {
		writeScript(t, bin, id, "#!/bin/sh\nexit 0\n")
		profilesINI += id + " = Test " + id + "\n"
	}
	if err := os.WriteFile(filepath.Join(env.config, "herdr", "agent-profiles.ini"), []byte(profilesINI), 0o600); err != nil {
		t.Fatal(err)
	}
	return env
}

func writeScript(t *testing.T, dir, name, body string) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte(body), 0o755); err != nil {
		t.Fatalf("write script: %v", err)
	}
	return p
}

// newLaunchFake serves no panes until a create call makes the default target pane.
func newLaunchFake() *fakeBackend {
	client := newFakeBackend()
	client.target = herdr.Pane{ID: "pane-new", TerminalID: "terminal-new", TabID: "tab-new", WorkspaceID: "workspace-new"}
	return client
}

// requirePendingOwnership fails a start unless the launch was recorded as pending in the ownership file first.
func requirePendingOwnership(path string) error {
	data, err := os.ReadFile(path)
	if err != nil || !strings.Contains(string(data), `"pending":true`) {
		return startedFailure("ownership was not saved before the agent was started")
	}
	return nil
}

func hasArgPrefix(call fakeCall, args []any) bool {
	return len(args) == 0 || len(call.Args) >= len(args) && reflect.DeepEqual(call.Args[:len(args)], args)
}

func failCall(message string) fakeHook {
	return func(context.Context, fakeCall) error { return startedFailure(message) }
}

// refuseStarts refuses the first refusals starts as a busy pane, then lets them succeed. A negative count refuses forever.
func refuseStarts(refusals int) fakeHook {
	var attempts atomic.Int32
	return func(context.Context, fakeCall) error {
		if refusals < 0 || int(attempts.Add(1)) <= refusals {
			return refusalByCode("agent_pane_busy")
		}
		return nil
	}
}

// releaseGate holds the hooked call until it is opened or the call's context ends.
type releaseGate struct {
	entered     chan struct{}
	release     chan struct{}
	enteredOnce sync.Once
	releaseOnce sync.Once
}

func newReleaseGate() *releaseGate {
	return &releaseGate{entered: make(chan struct{}), release: make(chan struct{})}
}

func (g *releaseGate) open() {
	g.releaseOnce.Do(func() { close(g.release) })
}

// hook runs next, if any, after the gate opens.
func (g *releaseGate) hook(next fakeHook) fakeHook {
	return func(ctx context.Context, call fakeCall) error {
		g.enteredOnce.Do(func() { close(g.entered) })
		select {
		case <-g.release:
		case <-ctx.Done():
			return &herdr.OutcomeError{Started: true, Err: ctx.Err()}
		}
		if next == nil {
			return nil
		}
		return next(ctx, call)
	}
}
