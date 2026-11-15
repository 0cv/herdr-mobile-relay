package coordinator

import (
	"context"
	"os"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

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

func callIndex(calls []fakeCall, method string, args ...any) int {
	return slices.IndexFunc(calls, func(call fakeCall) bool {
		return call.Method == method && hasArgPrefix(call, args)
	})
}

func TestClearConfiguredOutcomes(t *testing.T) {
	for _, mode := range []string{"success", "close-failure", "uncertain-start", "no-launch-record", "alias-no-launch-record", "record-wins-over-name", "no-profile", "invalid-cwd"} {
		t.Run(mode, func(t *testing.T) {
			options := launchFixtureOptions{}
			if mode == "close-failure" {
				options.close = failCall("exit status 2")
			}
			if mode == "uncertain-start" {
				options.start = failCall("exit status 2")
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
			calls := f.client.recorded()
			switch mode {
			case "no-profile", "invalid-cwd":
				if result.OK || len(calls) != 0 {
					t.Fatalf("refusal = %+v; calls %+v", result, calls)
				}
				if mode == "no-profile" && !strings.Contains(result.Error, "No launch profile matches") {
					t.Fatalf("refusal message = %q", result.Error)
				}
				ownershipAfter, err := os.ReadFile(f.ownership)
				if err != nil || string(ownershipBefore) != string(ownershipAfter) {
					t.Fatalf("ownership changed: %v", err)
				}
			case "uncertain-start":
				if result.OK || result.Phase != "dispatched_unknown" || f.client.count("StopPane") != 0 || f.client.count("StartAgent") == 0 {
					t.Fatalf("uncertain = %+v; calls %+v", result, calls)
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
				start, stop := callIndex(calls, "StartAgent"), callIndex(calls, "StopPane", "pane-old")
				if start < 0 || stop < start || f.client.count("WorkspaceCreate") != 1 || f.client.count("StopPane") != 1 {
					t.Fatalf("order/count: %+v", calls)
				}
				if !f.dispatcher.profiles.OwnsTarget(f.target, "claude") {
					t.Fatal("replacement ownership missing")
				}
				if callIndex(calls, "StartAgent", result.Data.(map[string]any)["name"], "claude") < 0 || f.client.count("StartAgent") != 1 {
					t.Fatalf("replacement profile: %+v", calls)
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
				if !reflect.DeepEqual(f.client.recorded(), calls) {
					t.Fatal("retry duplicated mutation")
				}
			}
		})
	}
}

func TestRepeatedClearDoesNotReuseTarget(t *testing.T) {
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
	f.client.update(func(b *fakeBackend) { b.inventoryOverrides = [][]herdr.Pane{{pane}} })
	clearedAgain := f.dispatcher.Handle(f.ctx, map[string]any{"type": "agent_clear", "request_id": "clear-again", "pane_id": old.PaneID})
	if !clearedAgain.OK {
		t.Fatalf("clear again: %+v", clearedAgain)
	}
	if clearedAgain.Data.(map[string]any)["pane_id"] == old.PaneID || f.client.count("StartAgent") != 2 {
		t.Fatalf("clear reused its target: %+v", clearedAgain)
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
			if err != nil || string(before) != string(after) || len(f.client.recorded()) != 0 || d.state.TopologyGeneration() != topology {
				t.Fatalf("unwired mutation: %v", err)
			}
		})
	}
}

func runClearSerialization(t *testing.T) {
	t.Helper()
	env := newLaunchEnv(t, "claude")
	cwd := env.cwd
	resolver := profiles.NewResolver(env.config, nil)
	if err := resolver.SetOwnershipPath(env.ownership); err != nil {
		t.Fatal(err)
	}

	recorder := newSerializationRecorder()
	client := newLaunchFake()
	client.target.Cwd = cwd
	client.setHook("StartAgent", recorder.hook("agent_start", true))
	state := NewState(testLogger())
	old := profiles.PaneIdentity{PaneID: "pane-old", TerminalID: "terminal-old", TabID: "tab-old", WorkspaceID: "workspace-old"}
	state.CommitInventory([]*AgentState{{PaneID: old.PaneID, RawPaneID: old.PaneID, TerminalID: old.TerminalID, TabID: old.TabID, WorkspaceID: old.WorkspaceID, Agent: "claude", Status: "idle", Cwd: cwd}}, state.RevisionCounter())
	dispatcher := newTestDispatcher(t, client, state)
	dispatcher.SetProfiles(resolver)
	if err := resolver.RememberVerified(old, "claude"); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
	t.Cleanup(cancel)
	t.Cleanup(recorder.unblock)

	first := make(chan *CommandResult, 1)
	go func() { first <- dispatcher.Handle(ctx, clearMessage()) }()
	select {
	case <-recorder.entered:
	case <-ctx.Done():
		t.Fatal("start gate not reached")
	}
	prompt, duplicate := make(chan *CommandResult, 1), make(chan *CommandResult, 1)
	admitted := make(chan struct{})
	go func() {
		prompt <- dispatcher.HandleAdmitted(ctx, map[string]any{"type": "submit_prompt", "request_id": "queued", "pane_id": "pane-old", "text": "one"}, func() { close(admitted) })
	}()
	select {
	case <-admitted:
	case <-ctx.Done():
		t.Fatal("prompt not admitted")
	}
	dupAdmitted := make(chan struct{})
	go func() { duplicate <- dispatcher.HandleAdmitted(ctx, clearMessage(), func() { close(dupAdmitted) }) }()
	select {
	case <-dupAdmitted:
	case <-ctx.Done():
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
	recorder.unblock()
	for _, ch := range []chan *CommandResult{first, duplicate} {
		select {
		case r := <-ch:
			if !r.OK {
				t.Fatalf("clear result %+v", r)
			}
		case <-ctx.Done():
			t.Fatal("clear timed out")
		}
	}
	select {
	case r := <-prompt:
		if r.OK {
			t.Fatalf("stale prompt succeeded: %+v", r)
		}
	case <-ctx.Done():
		t.Fatal("prompt timed out")
	}
	closed := client.callsTo("StopPane")
	if client.count("Prompt") != 0 || client.count("StartAgent") != 1 || len(closed) != 1 || closed[0].Args[0] != "pane-old" {
		t.Fatalf("serialized calls: %+v", client.recorded())
	}
}
