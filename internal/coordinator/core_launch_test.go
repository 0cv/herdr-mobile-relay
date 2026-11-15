package coordinator

import (
	"context"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/0cv/herdr-mobile-relay/internal/herdr"
)

func coreCalls(client *fakeBackend) []string {
	var calls []string
	for _, call := range client.recorded() {
		switch call.Method {
		case "LaunchTerminal":
			calls = append(calls, "run")
		case "StopPane":
			calls = append(calls, "kill:"+call.Args[0].(string))
		case "Prompt":
			calls = append(calls, "prompt")
		}
	}
	return calls
}

func lastLaunch(t *testing.T, client *fakeBackend) fakeCall {
	t.Helper()
	launches := client.callsTo("LaunchTerminal")
	if len(launches) != 1 {
		t.Fatalf("LaunchTerminal calls = %+v", launches)
	}
	return launches[0]
}

func TestCodexLaunchArgs(t *testing.T) {
	for _, core := range []bool{false, true} {
		t.Run(fmt.Sprintf("core=%v", core), func(t *testing.T) {
			f := newLaunchFixture(t)
			profile, _ := f.dispatcher.profiles.Profile("claude")
			profile.Kind = "codex"
			profile.Argv = []string{"/fixture/codex", "--model", "fixture"}
			original := append([]string(nil), profile.Argv...)
			if core {
				f.client.enableOneStepLaunch()
			}
			_, err := f.dispatcher.lifecycle.Start(f.ctx, profile, StartRequest{Name: "fixture", Cwd: f.cwd})
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(profile.Argv, original) {
				t.Fatal("profile argv mutated")
			}
			if core {
				if argv := lastLaunch(t, f.client).Args[0]; !reflect.DeepEqual(argv, []string{"/fixture/codex", "--no-daemon", "--model", "fixture"}) {
					t.Fatalf("argv=%v", argv)
				}
			} else {
				starts := f.client.callsTo("StartAgent")
				if len(starts) != 1 || starts[0].Args[1] != "codex" || starts[0].Args[2] != "pane-new" || len(starts[0].Args[4].([]string)) != 0 || f.client.count("LaunchTerminal") != 0 {
					t.Fatalf("TUI launch changed: %+v", f.client.recorded())
				}
			}
		})
	}
}

func TestCoreLaunchReusesWorkspaceAndHermesArgs(t *testing.T) {
	f := newLaunchFixtureWithOptions(t, launchFixtureOptions{})
	f.client.enableOneStepLaunch()
	f.client.update(func(b *fakeBackend) {
		b.panes = []herdr.Pane{{ID: "other", WorkspaceID: "selected", Cwd: f.cwd}}
		b.workspaces = []herdr.Workspace{{ID: "selected"}}
	})
	profile, ok := f.dispatcher.profiles.Profile("claude")
	if !ok {
		t.Fatal("missing fixture profile")
	}
	profile.Kind = "hermes"
	profile.Argv = []string{"/bin/sleep", "120"}
	ctx, cancel := context.WithTimeout(f.ctx, 10*time.Second)
	defer cancel()
	_, err := f.dispatcher.lifecycle.Start(ctx, profile, StartRequest{Name: "fixture", Cwd: f.cwd, HermesProfile: "default"})
	launch := lastLaunch(t, f.client)
	if err != nil || launch.Args[3] != "selected" || !reflect.DeepEqual(launch.Args[0], []string{"/bin/sleep", "120", "-p", "default"}) {
		t.Fatalf("launch=%+v err=%v", launch, err)
	}
}

func TestCoreClear(t *testing.T) {
	for _, action := range []string{"agent_clear", "agent_clear_without_launch_record"} {
		t.Run(action, func(t *testing.T) {
			f, old := newClearFixture(t, launchFixtureOptions{})
			action, unrecorded := strings.CutSuffix(action, "_without_launch_record")
			if unrecorded {
				f.dispatcher.profiles.Forget(old.PaneID)
			}
			f.client.enableOneStepLaunch()
			f.client.update(func(b *fakeBackend) { b.workspaces = []herdr.Workspace{{ID: old.WorkspaceID}} })
			message := clearMessage()
			message["type"] = action
			result := f.dispatcher.Handle(f.ctx, message)
			if calls := coreCalls(f.client); !result.OK || !reflect.DeepEqual(calls, []string{"run", "kill:" + old.PaneID}) {
				t.Fatalf("result=%+v calls=%v", result, calls)
			}
			if launch := lastLaunch(t, f.client); launch.Args[4] != old.TabID || launch.Args[3] != old.WorkspaceID {
				t.Fatalf("replacement location = %s/%s, want %s/%s", launch.Args[3], launch.Args[4], old.WorkspaceID, old.TabID)
			}
			if !f.dispatcher.profiles.OwnsTarget(f.target, "claude") {
				t.Fatal("missing replacement ownership")
			}
		})
	}
}

func TestCoreClearUnconfirmedAdoptionKillsOldTerminal(t *testing.T) {
	f, old := newClearFixture(t, launchFixtureOptions{})
	f.client.enableOneStepLaunch()
	f.client.update(func(b *fakeBackend) {
		b.workspaces = []herdr.Workspace{{ID: old.WorkspaceID}}
		b.target.TabID, b.target.WorkspaceID = old.TabID, old.WorkspaceID
	})
	result := f.dispatcher.Handle(f.ctx, clearMessage())
	if calls := coreCalls(f.client); !result.OK || !reflect.DeepEqual(calls, []string{"run", "kill:" + old.PaneID}) {
		t.Fatalf("result=%+v calls=%v", result, calls)
	}
	ownership := f.target
	ownership.TabID, ownership.WorkspaceID = old.TabID, old.WorkspaceID
	if !f.dispatcher.profiles.OwnsTarget(ownership, "claude") {
		t.Fatal("replacement ownership lost after unconfirmed adoption")
	}
}

func TestClearNamesDoNotCollide(t *testing.T) {
	names := make(map[string]bool)
	receivedAt := time.Now()
	for i := range 2 {
		f, _ := newClearFixture(t, launchFixtureOptions{})
		result := f.dispatcher.handleClear(f.ctx, f.dispatcher.newCommandRequest(receivedAt.Add(time.Duration(i)), "clear", "pane-old", nil, nil))
		if !result.OK {
			t.Fatalf("clear: %+v", result)
		}
		name := result.Data.(map[string]any)["name"].(string)
		if want := fmt.Sprintf("clear-%08x", uint32(receivedAt.Add(time.Duration(i)).UnixNano())); name != want {
			t.Fatalf("replacement name = %q, want %q", name, want)
		}
		if names[name] {
			t.Fatalf("replacement name reused: %s", name)
		}
		names[name] = true
	}
}

func TestCoreLaunchPromptAndReplay(t *testing.T) {
	for _, mode := range []string{"ready", "unready", "unknown", "prompt-unknown"} {
		t.Run(mode, func(t *testing.T) {
			f := newLaunchFixtureWithOptions(t, launchFixtureOptions{})
			b := f.client
			b.enableOneStepLaunch()
			b.update(func(b *fakeBackend) { b.ready = mode == "ready" || mode == "prompt-unknown" })
			if mode == "unknown" {
				b.setHook("LaunchTerminal", func(context.Context, fakeCall) error { return herdr.ErrDispatchedUnknown })
			}
			if mode == "prompt-unknown" {
				b.setHook("Prompt", func(context.Context, fakeCall) error { return herdr.ErrDispatchedUnknown })
			}
			message := map[string]any{"type": "agent_start", "request_id": "core-launch", "profile_id": "claude", "name": "fixture", "cwd": f.cwd, "prompt": "hello"}
			result := f.dispatcher.Handle(f.ctx, message)
			wantPhase := "completed"
			wantCalls := []string{"run", "prompt"}
			if mode == "prompt-unknown" {
				wantPhase = "completed_with_warning"
			}
			if mode == "unready" {
				wantPhase = "completed_with_warning"
				wantCalls = []string{"run"}
			}
			if mode == "unknown" {
				wantPhase = "dispatched_unknown"
				wantCalls = []string{"run"}
			}
			if calls := coreCalls(b); result.Phase != wantPhase || !reflect.DeepEqual(calls, wantCalls) {
				t.Fatalf("result=%+v calls=%v", result, calls)
			}
			f.dispatcher.Handle(f.ctx, message)
			if calls := coreCalls(b); !reflect.DeepEqual(calls, wantCalls) {
				t.Fatalf("replayed calls=%v", calls)
			}
		})
	}
}
