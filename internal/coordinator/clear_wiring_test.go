package coordinator

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

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

func clearMessage() map[string]any {
	return map[string]any{"action": "agent_clear", "request_id": "clear", "pane_id": "pane-old"}
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
	for _, mode := range []string{"success", "close-failure", "uncertain-start", "unknown-owner", "invalid-cwd"} {
		t.Run(mode, func(t *testing.T) {
			options := launchFixtureOptions{}
			if mode == "close-failure" {
				options.close = "exit 2"
			}
			if mode == "uncertain-start" {
				options.start = "exit 2"
			}
			f, old := newClearFixture(t, options)
			if mode == "unknown-owner" {
				f.dispatcher.profiles.Forget(old.PaneID)
			}
			if mode == "invalid-cwd" {
				if err := os.Remove(f.cwd); err != nil {
					t.Fatal(err)
				}
			}
			result := f.dispatcher.Handle(f.ctx, clearMessage())
			calls := clearCalls(t, f)
			switch mode {
			case "unknown-owner", "invalid-cwd":
				if result.OK || calls != "" {
					t.Fatalf("refusal = %+v; calls %s", result, calls)
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
				if mode == "close-failure" {
					if result.Phase != "completed_with_warning" || data["warning"] == nil || !f.dispatcher.profiles.OwnsTarget(old, "claude") {
						t.Fatalf("warning = %+v", result)
					}
				} else if result.Phase != "completed" || f.dispatcher.profiles.OwnsTarget(old, "claude") {
					t.Fatalf("success = %+v", result)
				}
				retry := f.dispatcher.Handle(f.ctx, clearMessage())
				if mode == "success" && (retry.OK || !strings.Contains(retry.Error, "ownership is unknown")) {
					t.Fatalf("post-forget retry = %+v", retry)
				}
				if mode == "close-failure" && (!retry.OK || !retry.replayed) {
					t.Fatalf("warning replay = %+v", retry)
				}
				if clearCalls(t, f) != calls {
					t.Fatal("retry duplicated mutation")
				}
			}
		})
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
				result := d.Handle(f.ctx, map[string]any{"action": "agent_clear", "pane_id": input.pane, "request_id": "unwired"})
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
		prompt <- f.dispatcher.HandleAdmitted(f.ctx, map[string]any{"action": "submit_prompt", "request_id": "queued", "pane_id": "pane-old", "text": "one"}, func() { close(admitted) })
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
