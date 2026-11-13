package coordinator

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/0cv/herdr-mobile-relay/internal/backend"
	"github.com/0cv/herdr-mobile-relay/internal/herdr"
	"github.com/0cv/herdr-mobile-relay/internal/profiles"
)

func newClearFixture(t *testing.T, options launchFixtureOptions) (*launchFixture, profiles.PaneIdentity) {
	t.Helper()
	f := newLaunchFixtureWithOptions(t, options)
	old := profiles.PaneIdentity{PaneID: "pane-old", TerminalID: "terminal-old", TabID: "tab-old", WorkspaceID: "workspace-old"}
	f.dispatcher.state.CommitInventory([]*AgentState{{PaneID: old.PaneID, RawPaneID: old.PaneID, TerminalID: old.TerminalID, TabID: old.TabID, WorkspaceID: old.WorkspaceID, Agent: "claude", Status: "idle", Cwd: f.cwd}}, f.dispatcher.state.RevisionCounter())
	if err := f.dispatcher.profiles.RememberVerified(old, "claude"); err != nil {
		t.Fatal(err)
	}
	return f, old
}

func reportAgent(f *launchFixture, old profiles.PaneIdentity, agent string) {
	f.dispatcher.state.CommitInventory([]*AgentState{{PaneID: old.PaneID, RawPaneID: old.PaneID, TerminalID: old.TerminalID, TabID: old.TabID, WorkspaceID: old.WorkspaceID, Agent: agent, Status: "idle", Cwd: f.cwd}}, f.dispatcher.state.RevisionCounter())
}

func clearMessage() map[string]any {
	return map[string]any{"type": "agent_clear", "request_id": "clear", "pane_id": "pane-old"}
}

func clearCalls(t *testing.T, f *launchFixture) string {
	t.Helper()
	data, err := os.ReadFile(f.calls)
	if os.IsNotExist(err) {
		return ""
	}
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func TestClearConfiguredOutcomes(t *testing.T) {
	for _, mode := range []string{"success", "close-failure", "uncertain-start", "no-launch-record", "alias-no-launch-record", "record-wins-over-name", "no-profile", "invalid-cwd"} {
		t.Run(mode, func(t *testing.T) {
			options := launchFixtureOptions{}
			if mode == "close-failure" {
				options.close = "exit 2"
			}
			if mode == "uncertain-start" {
				options.start = "exit 2"
			}
			f, old := newClearFixture(t, options)
			switch mode {
			case "no-launch-record", "alias-no-launch-record", "no-profile":
				f.dispatcher.profiles.Forget(old.PaneID)
			}
			switch mode {
			case "alias-no-launch-record":
				reportAgent(f, old, "claude-code")
			case "record-wins-over-name":
				reportAgent(f, old, "qodercli")
			case "no-profile":
				reportAgent(f, old, "mystery")
			}
			ownershipBefore, err := os.ReadFile(f.ownership)
			if err != nil {
				t.Fatal(err)
			}
			if mode == "invalid-cwd" {
				if err := os.Remove(f.cwd); err != nil {
					t.Fatal(err)
				}
			}
			result := f.dispatcher.Handle(f.ctx, clearMessage())
			calls := clearCalls(t, f)
			switch mode {
			case "no-profile", "invalid-cwd":
				if result.OK || calls != "" {
					t.Fatalf("refusal = %+v; calls %s", result, calls)
				}
				if mode == "no-profile" && !strings.Contains(result.Error, "No launch profile matches") {
					t.Fatalf("refusal message = %q", result.Error)
				}
				ownershipAfter, err := os.ReadFile(f.ownership)
				if err != nil || string(ownershipBefore) != string(ownershipAfter) {
					t.Fatalf("ownership changed: %v", err)
				}
			case "uncertain-start":
				if result.OK || result.Phase != "dispatched_unknown" || strings.Contains(calls, "pane close") || !strings.Contains(calls, "agent start") {
					t.Fatalf("uncertain = %+v; calls %s", result, calls)
				}
				if !f.dispatcher.profiles.OwnsTarget(old, "claude") {
					t.Fatal("old ownership lost")
				}
			default:
				if !result.OK {
					t.Fatalf("clear = %+v", result)
				}
				data, ok := result.Data.(map[string]any)
				if !ok || data["pane_id"] != f.target.PaneID || data["cwd"] != f.cwd {
					t.Fatalf("replacement = %+v", result)
				}
				start, stop := strings.Index(calls, "agent start"), strings.Index(calls, "pane close pane-old")
				if start < 0 || stop < start || strings.Count(calls, "workspace create") != 1 || strings.Count(calls, "pane close") != 1 {
					t.Fatalf("order/count: %s", calls)
				}
				if !f.dispatcher.profiles.OwnsTarget(f.target, "claude") {
					t.Fatal("replacement ownership missing")
				}
				if !strings.Contains(calls, "--kind claude") || strings.Contains(calls, "qodercli") {
					t.Fatalf("replacement profile: %s", calls)
				}
				if mode == "close-failure" {
					if result.Phase != "completed_with_warning" || data["warning"] == nil || !f.dispatcher.profiles.OwnsTarget(old, "claude") {
						t.Fatalf("warning = %+v", result)
					}
				} else if result.Phase != "completed" || f.dispatcher.profiles.OwnsTarget(old, "claude") {
					t.Fatalf("success = %+v", result)
				}
				retry := f.dispatcher.Handle(f.ctx, clearMessage())
				if !retry.OK || !retry.replayed {
					t.Fatalf("retry = %+v", retry)
				}
				if clearCalls(t, f) != calls {
					t.Fatal("retry duplicated mutation")
				}
			}
		})
	}
}

type clearInventoryBackend struct {
	backend.Backend
	pane    herdr.Pane
	checked bool
}

func (b *clearInventoryBackend) GetInventory(ctx context.Context) (*herdr.Inventory, error) {
	if b.checked {
		return b.Backend.GetInventory(ctx)
	}
	b.checked = true
	return &herdr.Inventory{Panes: []herdr.Pane{b.pane}}, nil
}

func TestClearThenRestartDoesNotReuseTarget(t *testing.T) {
	f, old := newClearFixture(t, launchFixtureOptions{})
	cleared := f.dispatcher.Handle(f.ctx, clearMessage())
	if !cleared.OK {
		t.Fatalf("clear: %+v", cleared)
	}
	name := cleared.Data.(map[string]any)["name"].(string)
	pane := herdr.Pane{ID: old.PaneID, TerminalID: old.TerminalID, TabID: old.TabID, WorkspaceID: old.WorkspaceID, Name: name, Cwd: f.cwd}
	if err := f.dispatcher.profiles.RememberVerified(old, "claude"); err != nil {
		t.Fatal(err)
	}
	b := &clearInventoryBackend{Backend: f.dispatcher.herdr, pane: pane}
	f.dispatcher.herdr, f.dispatcher.lifecycle.herdr = b, b
	restarted := f.dispatcher.Handle(f.ctx, map[string]any{"type": "agent_restart", "request_id": "restart", "pane_id": old.PaneID})
	if !restarted.OK {
		t.Fatalf("restart: %+v", restarted)
	}
	if restarted.Data.(map[string]any)["pane_id"] == old.PaneID || strings.Count(clearCalls(t, f), "agent start") != 2 {
		t.Fatalf("restart reused its target: %+v", restarted)
	}
}

func TestClearWithoutWiringDoesNotMutate(t *testing.T) {
	for _, missing := range []string{"lifecycle", "profiles", "lifecycle profiles"} {
		t.Run(missing, func(t *testing.T) {
			f, _ := newClearFixture(t, launchFixtureOptions{})
			d := f.dispatcher
			before, err := os.ReadFile(f.ownership)
			if err != nil {
				t.Fatal(err)
			}
			topology := d.state.TopologyGeneration()
			switch missing {
			case "lifecycle":
				d.lifecycle = nil
			case "profiles":
				d.profiles = nil
			case "lifecycle profiles":
				d.lifecycle.profiles = nil
			}
			for _, input := range []struct{ pane, want string }{{"", "Agent is required"}, {"absent", "no longer available"}, {"pane-old", "lifecycle is unavailable"}} {
				result := d.Handle(f.ctx, map[string]any{"type": "agent_clear", "pane_id": input.pane, "request_id": "unwired"})
				if result.OK || !strings.Contains(result.Error, input.want) {
					t.Fatalf("result = %+v", result)
				}
			}
			after, err := os.ReadFile(f.ownership)
			if err != nil || string(before) != string(after) || clearCalls(t, f) != "" || d.state.TopologyGeneration() != topology {
				t.Fatalf("unwired mutation: %v", err)
			}
		})
	}
}

func runClearSerialization(t *testing.T) {
	t.Helper()
	dir := t.TempDir()
	entered, release := filepath.Join(dir, "entered"), filepath.Join(dir, "release")
	f, _ := newClearFixture(t, launchFixtureOptions{start: "printf entered > '" + entered + "'; while [ ! -f '" + release + "' ]; do /bin/sleep 0.01; done"})
	t.Cleanup(func() { _ = os.WriteFile(release, nil, 0600) })
	first := make(chan *CommandResult, 1)
	go func() { first <- f.dispatcher.Handle(f.ctx, clearMessage()) }()
	for {
		if _, err := os.Stat(entered); err == nil {
			break
		}
		select {
		case <-f.ctx.Done():
			t.Fatal("start gate not reached")
		case <-time.After(time.Millisecond):
		}
	}
	prompt, duplicate := make(chan *CommandResult, 1), make(chan *CommandResult, 1)
	admitted := make(chan struct{})
	go func() {
		prompt <- f.dispatcher.HandleAdmitted(f.ctx, map[string]any{"type": "submit_prompt", "request_id": "queued", "pane_id": "pane-old", "text": "one"}, func() { close(admitted) })
	}()
	select {
	case <-admitted:
	case <-f.ctx.Done():
		t.Fatal("prompt not admitted")
	}
	dupAdmitted := make(chan struct{})
	go func() { duplicate <- f.dispatcher.HandleAdmitted(f.ctx, clearMessage(), func() { close(dupAdmitted) }) }()
	select {
	case <-dupAdmitted:
	case <-f.ctx.Done():
		t.Fatal("duplicate not admitted")
	}
	select {
	case r := <-prompt:
		t.Fatalf("prompt escaped gate: %+v", r)
	default:
	}
	select {
	case r := <-duplicate:
		t.Fatalf("duplicate escaped gate: %+v", r)
	default:
	}
	if err := os.WriteFile(release, nil, 0600); err != nil {
		t.Fatal(err)
	}
	for _, ch := range []chan *CommandResult{first, duplicate} {
		select {
		case r := <-ch:
			if !r.OK {
				t.Fatalf("clear result %+v", r)
			}
		case <-f.ctx.Done():
			t.Fatal("clear timed out")
		}
	}
	select {
	case r := <-prompt:
		if r.OK {
			t.Fatalf("stale prompt succeeded: %+v", r)
		}
	case <-f.ctx.Done():
		t.Fatal("prompt timed out")
	}
	calls := clearCalls(t, f)
	if strings.Contains(calls, "agent prompt") || strings.Count(calls, "agent start") != 1 || strings.Count(calls, "pane close pane-old") != 1 {
		t.Fatalf("serialized calls: %s", calls)
	}
}
