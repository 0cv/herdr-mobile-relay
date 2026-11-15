package coordinator

import (
	"context"
	"testing"
	"time"

	"github.com/0cv/herdr-mobile-relay/internal/profiles"
)

// launchFixture uses the server's dispatcher/resolver wiring, not a fake lifecycle.
type launchFixture struct {
	dispatcher             *Dispatcher
	client                 *fakeBackend
	ctx                    context.Context
	cwd, config, ownership string
	target                 profiles.PaneIdentity
}

// Each hook runs when the launch reaches that Herdr call; a returned error fails the call.
type launchFixtureOptions struct {
	start, prompt, enter, close fakeHook
}

func newLaunchFixture(t *testing.T) *launchFixture {
	t.Helper()
	return newLaunchFixtureWithOptions(t, launchFixtureOptions{})
}

func newLaunchFixtureWithOptions(t *testing.T, options launchFixtureOptions) *launchFixture {
	t.Helper()
	env := newLaunchEnv(t, "claude", "qodercli")
	f := &launchFixture{
		client: newLaunchFake(), cwd: env.cwd, config: env.config, ownership: env.ownership,
		target: profiles.PaneIdentity{PaneID: "pane-new", TerminalID: "terminal-new", TabID: "tab-new", WorkspaceID: "workspace-new"},
	}
	f.client.target.Cwd = f.cwd
	f.client.setHook("StartAgent", func(ctx context.Context, call fakeCall) error {
		if err := requirePendingOwnership(f.ownership); err != nil {
			return err
		}
		if options.start == nil {
			return nil
		}
		return options.start(ctx, call)
	})
	for method, hook := range map[string]fakeHook{"Prompt": options.prompt, "SendKeys": options.enter, "StopPane": options.close} {
		if hook != nil {
			f.client.setHook(method, hook)
		}
	}
	resolver := profiles.NewResolver(f.config, nil)
	if err := resolver.SetOwnershipPath(f.ownership); err != nil {
		t.Fatal(err)
	}
	f.dispatcher = newTestDispatcher(t, f.client, NewState(testLogger()))
	f.dispatcher.SetProfiles(resolver)
	if f.dispatcher.lifecycle == nil || f.dispatcher.lifecycle.profiles != resolver || f.dispatcher.lifecycle.state != f.dispatcher.state {
		t.Fatal("production lifecycle wiring missing")
	}
	var cancel context.CancelFunc
	f.ctx, cancel = context.WithTimeout(t.Context(), 15*time.Second)
	t.Cleanup(cancel)
	return f
}

func (f *launchFixture) assertLaunch(t *testing.T, result *CommandResult) {
	t.Helper()
	if result == nil || !result.OK || result.Phase != "completed" || result.PaneID != f.target.PaneID {
		t.Fatalf("launch result = %+v", result)
	}
	started, ok := result.Data.(StartResult)
	if !ok || started.PaneID != f.target.PaneID || started.WorkspaceID != f.target.WorkspaceID || started.Cwd != f.cwd || !started.Identity.Valid || started.Identity.TerminalID != f.target.TerminalID {
		t.Fatalf("launch identity = %+v", result.Data)
	}
	// Reload from disk: an in-memory association alone is insufficient.
	resolver := profiles.NewResolver(f.config, nil)
	if err := resolver.SetOwnershipPath(f.ownership); err != nil {
		t.Fatal(err)
	}
	if !resolver.OwnsTarget(f.target, "claude") {
		t.Fatal("verified profile ownership was not persisted")
	}
}

func (f *launchFixture) countCalls(method string, args ...any) int {
	count := 0
	for _, call := range f.client.callsTo(method) {
		if hasArgPrefix(call, args) {
			count++
		}
	}
	return count
}

func (f *launchFixture) assertCalls(t *testing.T, prompts int) {
	t.Helper()
	for _, want := range []struct {
		method string
		args   []any
		count  int
	}{
		{"WorkspaceCreate", nil, 1},
		{"TabCreate", nil, 0},
		{"TabRename", []any{"tab-new", "proj"}, 1},
		{"StartAgent", nil, 1},
		{"StartAgent", []any{"proj", "claude", "pane-new"}, 1},
		{"Prompt", nil, prompts},
		{"Prompt", []any{"pane-new", "hello"}, prompts},
	} {
		if got := f.countCalls(want.method, want.args...); got != want.count {
			t.Fatalf("%s%v calls = %d, want %d\n%+v", want.method, want.args, got, want.count, f.client.recorded())
		}
	}
}
