package coordinator

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/0cv/herdr-mobile-relay/internal/herdr"
	"github.com/0cv/herdr-mobile-relay/internal/profiles"
)

type launchOwnershipFixture struct {
	lifecycle  *Lifecycle
	client     *fakeBackend
	profile    profiles.Profile
	request    StartRequest
	store      string
	configHome string
}

func newLaunchOwnershipFixture(t *testing.T, profileID string) *launchOwnershipFixture {
	t.Helper()
	env := newLaunchEnv(t, profileID)
	f := &launchOwnershipFixture{
		client:     newLaunchFake(),
		store:      env.ownership,
		configHome: env.config,
		request:    StartRequest{ProfileID: profileID, Name: "project-agent", Cwd: env.cwd},
	}
	f.client.target.TerminalID, f.client.target.Cwd = "terminal-pane-new", env.cwd
	f.client.agent = herdr.AgentInfo{Running: true}
	requirePending := func(context.Context, fakeCall) error { return requirePendingOwnership(f.store) }
	f.client.setHook("StartAgent", requirePending)
	f.client.setHook("PaneRun", requirePending)
	resolver := profiles.NewResolver(f.configHome, nil)
	if err := resolver.SetOwnershipPath(f.store); err != nil {
		t.Fatal(err)
	}
	var ok bool
	f.profile, ok = resolver.Profile(profileID)
	if !ok {
		t.Fatalf("profile %s not discovered", profileID)
	}
	f.lifecycle = &Lifecycle{herdr: f.client, profiles: resolver, home: env.home}
	return f
}

func (f *launchOwnershipFixture) countStarts(kind string) int {
	count := 0
	for _, call := range f.client.callsTo("StartAgent") {
		if call.Args[1] == kind {
			count++
		}
	}
	return count
}

func TestLifecyclePersistsShellOwnershipBeforeAgentDiscovery(t *testing.T) {
	for _, profileID := range []string{"pi", "codex", "test-wrapper"} {
		t.Run(profileID, func(t *testing.T) {
			f := newLaunchOwnershipFixture(t, profileID)
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			result, err := f.lifecycle.Start(ctx, f.profile, f.request)
			if err != nil {
				t.Fatal(err)
			}
			data, err := os.ReadFile(f.store)
			if err != nil {
				t.Fatalf("successful launch did not persist ownership: %v", err)
			}
			var records []struct {
				Target    profiles.PaneIdentity `json:"target"`
				ProfileID string                `json:"profile_id"`
				Pending   bool                  `json:"pending"`
			}
			if err := json.Unmarshal(data, &records); err != nil {
				t.Fatal(err)
			}
			if len(records) != 1 || records[0].Pending || records[0].ProfileID != profileID || records[0].Target.TerminalID != "terminal-pane-new" {
				t.Fatalf("ownership = %s", data)
			}
			info, err := os.Stat(f.store)
			if err != nil || info.Mode().Perm() != 0600 {
				t.Fatalf("ownership permissions: %v, %v", info, err)
			}
			if err := f.lifecycle.profiles.ReconcileOwnership([]profiles.PaneIdentity{records[0].Target}); err != nil {
				t.Fatal(err)
			}
			if got := f.lifecycle.profiles.ResolvePane(result.PaneID, ""); got != profileID {
				t.Fatalf("profile before restart = %q", got)
			}
			resolver := profiles.NewResolver(f.configHome, nil)
			if err := resolver.SetOwnershipPath(f.store); err != nil {
				t.Fatal(err)
			}
			state := NewState(testLogger())
			d := newTestDispatcher(t, f.client, state)
			d.SetProfiles(resolver)
			d.lifecycle.home = f.lifecycle.home
			state.CommitInventory([]*AgentState{{PaneID: result.PaneID, RawPaneID: result.PaneID, TerminalID: "terminal-pane-new", TabID: "tab-new", WorkspaceID: "workspace-new", Agent: profileID, Status: "idle", Cwd: f.request.Cwd}}, state.RevisionCounter())
			if got := resolver.ResolvePane(result.PaneID, ""); got != profileID {
				t.Fatalf("profile after restart = %q", got)
			}
			f.client.update(func(b *fakeBackend) {
				b.target.ID, b.target.TerminalID = "pane-replacement", "terminal-pane-replacement"
			})
			cleared := d.handleClear(ctx, d.newCommandRequest(time.Now(), "clear-test", result.PaneID, nil, nil))
			if !cleared.OK {
				t.Fatalf("Clear after restart = %+v", cleared)
			}
			if f.client.count("StopPane") != 1 || f.client.callsTo("StopPane")[0].Args[0] != "pane-new" {
				t.Fatalf("old pane not cleared: %+v", f.client.recorded())
			}
			if f.profile.Kind == "" && f.client.count("PaneRun") != 2 {
				t.Fatalf("custom profile not relaunched: %+v", f.client.recorded())
			}
			if f.profile.Kind != "" && f.countStarts(profileID) != 2 {
				t.Fatalf("built-in profile not relaunched: %+v", f.client.recorded())
			}
		})
	}
}

func TestLifecycleDoesNotLaunchWithoutTerminalOwnership(t *testing.T) {
	for _, mode := range []string{"missing-pane", "missing-terminal", "inventory-error"} {
		t.Run(mode, func(t *testing.T) {
			f := newLaunchOwnershipFixture(t, "pi")
			switch mode {
			case "missing-pane":
				f.client.setHook("PaneList", func(context.Context, fakeCall) error {
					f.client.update(func(b *fakeBackend) { b.panes = nil })
					return nil
				})
			case "missing-terminal":
				f.client.target.TerminalID = ""
			case "inventory-error":
				f.client.setHook("PaneList", func(context.Context, fakeCall) error {
					return &herdr.CLIError{Code: "internal_error"}
				})
			}
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			result, err := f.lifecycle.Start(ctx, f.profile, f.request)
			if err == nil {
				t.Fatal("launch succeeded without verified terminal identity")
			}
			if result.PaneID != "pane-new" {
				t.Fatalf("created pane lost: %+v", result)
			}
			if f.client.count("StartAgent") != 0 || f.client.count("PaneRun") != 0 || f.client.count("StopPane") != 0 {
				t.Fatalf("unverified pane mutated: %+v", f.client.recorded())
			}
		})
	}
}

func TestLifecycleDoesNotLaunchWhenOwnershipCannotBeSaved(t *testing.T) {
	f := newLaunchOwnershipFixture(t, "pi")
	if err := os.WriteFile(filepath.Dir(f.store), []byte("not a directory"), 0600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	result, err := f.lifecycle.Start(ctx, f.profile, f.request)
	if err == nil || result.PaneID != "pane-new" {
		t.Fatalf("Start = %+v, %v; want a retained pane without launching", result, err)
	}
	if f.client.count("StartAgent") != 0 || f.client.count("PaneRun") != 0 || f.client.count("StopPane") != 0 {
		t.Fatalf("unsaved ownership allowed mutation: %+v", f.client.recorded())
	}
}
