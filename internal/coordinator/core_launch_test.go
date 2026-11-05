package coordinator

import (
	"context"
	"fmt"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/0cv/herdr-mobile-relay/internal/backend"
	"github.com/0cv/herdr-mobile-relay/internal/herdr"
)

type coreLaunchBackend struct {
	backend.Backend
	pane               herdr.Pane
	ready              bool
	err                error
	promptErr          error
	calls              []string
	argv               []string
	workspace          string
	tab                string
	inventoryWorkspace string
	cwd                string
}

func (b *coreLaunchBackend) GetInventory(context.Context) (*herdr.Inventory, error) {
	return &herdr.Inventory{Panes: []herdr.Pane{{ID: "other", WorkspaceID: b.inventoryWorkspace, Cwd: b.cwd}}}, nil
}

func (b *coreLaunchBackend) WorkspaceList(context.Context) ([]herdr.Workspace, error) {
	return []herdr.Workspace{{ID: b.inventoryWorkspace}}, nil
}

func (b *coreLaunchBackend) CapabilityStatus() herdr.ServerStatus {
	return herdr.ServerStatus{OneStepLaunch: true, Features: map[string]herdr.FeatureEvidence{herdr.FeatureAgentLaunch: {State: herdr.FeatureSupported}}}
}
func (b *coreLaunchBackend) LaunchTerminal(_ context.Context, argv []string, cwd, name, workspace, tab string, _ bool) (*herdr.Pane, bool, error) {
	b.calls = append(b.calls, "run")
	b.argv = argv
	b.workspace, b.tab = workspace, tab
	b.pane.Cwd, b.pane.Name = cwd, name
	return &b.pane, b.ready, b.err
}
func (b *coreLaunchBackend) StopPane(_ context.Context, id string) error {
	b.calls = append(b.calls, "kill:"+id)
	return nil
}
func (b *coreLaunchBackend) Prompt(context.Context, string, string) error {
	b.calls = append(b.calls, "prompt")
	return b.promptErr
}

func TestCodexLaunchArgs(t *testing.T) {
	for _, core := range []bool{false, true} {
		t.Run(fmt.Sprintf("core=%v", core), func(t *testing.T) {
			f := newLaunchFixture(t)
			profile, _ := f.dispatcher.profiles.Profile("claude")
			profile.Kind = "codex"
			profile.Argv = []string{"/fixture/codex", "--model", "fixture"}
			original := append([]string(nil), profile.Argv...)
			var b *coreLaunchBackend
			if core {
				b = &coreLaunchBackend{Backend: f.dispatcher.herdr, pane: herdr.Pane{ID: f.target.PaneID, TerminalID: f.target.TerminalID}}
				f.dispatcher.lifecycle.herdr = b
			}
			_, err := f.dispatcher.lifecycle.Start(f.ctx, profile, StartRequest{Name: "fixture", Cwd: f.cwd})
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(profile.Argv, original) {
				t.Fatal("profile argv mutated")
			}
			if core {
				if !reflect.DeepEqual(b.argv, []string{"/fixture/codex", "--no-daemon", "--model", "fixture"}) {
					t.Fatalf("argv=%v", b.argv)
				}
			} else {
				calls, err := os.ReadFile(f.calls)
				if err != nil {
					t.Fatal(err)
				}
				if strings.Contains(string(calls), "--no-daemon") || !strings.Contains(string(calls), "--kind codex --pane") {
					t.Fatalf("TUI launch changed: %s", calls)
				}
			}
		})
	}
}

func TestCoreLaunchReusesWorkspaceAndHermesArgs(t *testing.T) {
	f := newLaunchFixtureWithOptions(t, launchFixtureOptions{})
	b := &coreLaunchBackend{Backend: f.dispatcher.herdr, inventoryWorkspace: "selected", cwd: f.cwd, pane: herdr.Pane{ID: f.target.PaneID, TerminalID: f.target.TerminalID}}
	f.dispatcher.lifecycle.herdr = b
	profile, ok := f.dispatcher.profiles.Profile("claude")
	if !ok {
		t.Fatal("missing fixture profile")
	}
	profile.Kind = "hermes"
	profile.Argv = []string{"/bin/sleep", "120"}
	ctx, cancel := context.WithTimeout(f.ctx, 10*time.Second)
	defer cancel()
	_, err := f.dispatcher.lifecycle.Start(ctx, profile, StartRequest{Name: "fixture", Cwd: f.cwd, HermesProfile: "default"})
	if err != nil || b.workspace != "selected" || !reflect.DeepEqual(b.argv, []string{"/bin/sleep", "120", "-p", "default"}) {
		t.Fatalf("workspace=%s argv=%v err=%v", b.workspace, b.argv, err)
	}
}

func TestCoreClearAndRestart(t *testing.T) {
	for _, action := range []string{"agent_clear", "agent_restart"} {
		t.Run(action, func(t *testing.T) {
			f, old := newClearFixture(t, launchFixtureOptions{})
			b := &coreLaunchBackend{Backend: f.dispatcher.herdr, inventoryWorkspace: old.WorkspaceID, pane: herdr.Pane{ID: f.target.PaneID, TerminalID: f.target.TerminalID, TabID: f.target.TabID, WorkspaceID: f.target.WorkspaceID}}
			f.dispatcher.herdr, f.dispatcher.lifecycle.herdr = b, b
			message := clearMessage()
			message["action"] = action
			result := f.dispatcher.Handle(f.ctx, message)
			if !result.OK || !reflect.DeepEqual(b.calls, []string{"run", "kill:" + old.PaneID}) {
				t.Fatalf("result=%+v calls=%v", result, b.calls)
			}
			if b.tab != old.TabID || b.workspace != old.WorkspaceID {
				t.Fatalf("replacement location = %s/%s, want %s/%s", b.workspace, b.tab, old.WorkspaceID, old.TabID)
			}
			if !f.dispatcher.profiles.OwnsTarget(f.target, "claude") {
				t.Fatal("missing replacement ownership")
			}
		})
	}
}

func TestCoreClearUnconfirmedAdoptionKillsOldTerminal(t *testing.T) {
	f, old := newClearFixture(t, launchFixtureOptions{})
	b := &coreLaunchBackend{
		Backend: f.dispatcher.herdr, inventoryWorkspace: old.WorkspaceID,
		pane: herdr.Pane{ID: f.target.PaneID, TerminalID: f.target.TerminalID, TabID: old.TabID, WorkspaceID: old.WorkspaceID},
	}
	f.dispatcher.herdr, f.dispatcher.lifecycle.herdr = b, b
	result := f.dispatcher.Handle(f.ctx, clearMessage())
	if !result.OK || !reflect.DeepEqual(b.calls, []string{"run", "kill:" + old.PaneID}) {
		t.Fatalf("result=%+v calls=%v", result, b.calls)
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
		result := f.dispatcher.handleClear(f.ctx, receivedAt.Add(time.Duration(i)), "clear", "pane-old")
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
			b := &coreLaunchBackend{Backend: f.dispatcher.herdr, ready: mode == "ready" || mode == "prompt-unknown", pane: herdr.Pane{ID: f.target.PaneID, TerminalID: f.target.TerminalID, TabID: f.target.TabID, WorkspaceID: f.target.WorkspaceID}}
			if mode == "unknown" {
				b.err = herdr.ErrDispatchedUnknown
			}
			if mode == "prompt-unknown" {
				b.promptErr = herdr.ErrDispatchedUnknown
			}
			f.dispatcher.herdr, f.dispatcher.lifecycle.herdr = b, b
			message := map[string]any{"action": "agent_start", "request_id": "core-launch", "profile_id": "claude", "name": "fixture", "cwd": f.cwd, "prompt": "hello"}
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
			if result.Phase != wantPhase || !reflect.DeepEqual(b.calls, wantCalls) {
				t.Fatalf("result=%+v calls=%v", result, b.calls)
			}
			f.dispatcher.Handle(f.ctx, message)
			if !reflect.DeepEqual(b.calls, wantCalls) {
				t.Fatalf("replayed calls=%v", b.calls)
			}
		})
	}
}
