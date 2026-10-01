package coordinator

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/0cv/herdr-mobile-relay/internal/herdr"
	"github.com/0cv/herdr-mobile-relay/internal/profiles"
)

type launchOwnershipFixture struct {
	lifecycle  *Lifecycle
	profile    profiles.Profile
	request    StartRequest
	store      string
	calls      string
	nextPane   string
	configHome string
}

func newLaunchOwnershipFixture(t *testing.T, profileID string, paneList func([]herdr.Pane) (any, string)) *launchOwnershipFixture {
	t.Helper()
	dir := t.TempDir()
	home := filepath.Join(dir, "home")
	cwd := filepath.Join(home, "project")
	if err := os.MkdirAll(cwd, 0700); err != nil {
		t.Fatal(err)
	}
	writeScript(t, dir, profileID, "#!/bin/sh\nexit 0\n")
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	f := &launchOwnershipFixture{
		store:      filepath.Join(dir, "runtime", "profile-ownership.json"),
		calls:      filepath.Join(dir, "calls"),
		nextPane:   filepath.Join(dir, "next-pane"),
		configHome: filepath.Join(dir, "config"),
		request:    StartRequest{ProfileID: profileID, Name: "project-agent", Cwd: cwd},
	}
	if err := os.MkdirAll(filepath.Join(f.configHome, "herdr"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(f.configHome, "herdr", "agent-profiles.ini"), []byte("[profiles]\n"+profileID+" = Test agent\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(f.nextPane, []byte("pane-new"), 0600); err != nil {
		t.Fatal(err)
	}
	created := filepath.Join(dir, "created")
	bin := writeScript(t, dir, "herdr", `#!/bin/sh
printf '%s\n' "$*" >> '`+f.calls+`'
pane=$(cat '`+f.nextPane+`')
case "$1 $2" in
 'workspace create'|'tab create')
   printf '%s' "$pane" > '`+created+`'
   printf '{"result":{"root_pane":{"pane_id":"%s","tab_id":"tab-new","workspace_id":"workspace-new"}}}\n' "$pane" ;;
 'agent start'|'pane run')
   grep -q '"pending":true' '`+f.store+`' || exit 2
   printf '{"result":{"pane_id":"%s"}}\n' "$pane" ;;
 'agent get') printf '{"result":{"running":true}}\n' ;;
 'tab rename'|'agent rename'|'pane close') printf '{"result":{}}\n' ;;
 *) exit 2 ;;
esac
`)
	socketPath := startPollerTestSocket(t, func(method string) (any, string) {
		if method != "pane.list" {
			return pollerSuccessResult(method), ""
		}
		panes := []herdr.Pane{}
		if data, err := os.ReadFile(created); err == nil {
			panes = append(panes, herdr.Pane{ID: string(data), TerminalID: "terminal-" + string(data), TabID: "tab-new", WorkspaceID: "workspace-new", Cwd: cwd})
		}
		if paneList != nil {
			return paneList(panes)
		}
		return map[string]any{"type": "pane_list", "panes": panes}, ""
	})
	client := herdr.NewClient(bin, socketPath)
	t.Cleanup(func() { _ = client.Close() })
	resolver := profiles.NewResolver(f.configHome, nil)
	if err := resolver.SetOwnershipPath(f.store); err != nil {
		t.Fatal(err)
	}
	var ok bool
	f.profile, ok = resolver.Profile(profileID)
	if !ok {
		t.Fatalf("profile %s not discovered", profileID)
	}
	f.lifecycle = &Lifecycle{herdr: client, profiles: resolver, home: home}
	return f
}

func TestLifecyclePersistsShellOwnershipBeforeAgentDiscovery(t *testing.T) {
	for _, profileID := range []string{"pi", "codex", "test-wrapper"} {
		t.Run(profileID, func(t *testing.T) {
			f := newLaunchOwnershipFixture(t, profileID, nil)
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
			if got := f.lifecycle.profiles.ResolveOwnedPane(result.PaneID); got != profileID {
				t.Fatalf("profile before restart = %q", got)
			}
			resolver := profiles.NewResolver(f.configHome, nil)
			if err := resolver.SetOwnershipPath(f.store); err != nil {
				t.Fatal(err)
			}
			state := NewState(testLogger())
			d := NewDispatcher(f.lifecycle.herdr, state, nil, testLogger())
			d.SetProfiles(resolver)
			d.lifecycle.home = f.lifecycle.home
			t.Cleanup(func() { _ = d.Close(context.Background()) })
			state.CommitInventory([]*AgentState{{PaneID: result.PaneID, RawPaneID: result.PaneID, TerminalID: "terminal-pane-new", TabID: "tab-new", WorkspaceID: "workspace-new", Agent: profileID, Status: "idle", Cwd: f.request.Cwd}}, state.RevisionCounter())
			if got := resolver.ResolveOwnedPane(result.PaneID); got != profileID {
				t.Fatalf("profile after restart = %q", got)
			}
			if err := os.WriteFile(f.nextPane, []byte("pane-replacement"), 0600); err != nil {
				t.Fatal(err)
			}
			cleared := d.handleClear(ctx, time.Now(), "clear-test", result.PaneID)
			if !cleared.OK {
				t.Fatalf("Clear after restart = %+v", cleared)
			}
			calls, err := os.ReadFile(f.calls)
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(string(calls), "pane close pane-new") {
				t.Fatalf("old pane not cleared: %s", calls)
			}
			if f.profile.Kind == "" && strings.Count(string(calls), "pane run") != 2 {
				t.Fatalf("custom profile not relaunched: %s", calls)
			}
			if f.profile.Kind != "" && strings.Count(string(calls), "--kind "+profileID) != 2 {
				t.Fatalf("built-in profile not relaunched: %s", calls)
			}
		})
	}
}

func TestLifecycleDoesNotLaunchWithoutTerminalOwnership(t *testing.T) {
	for _, mode := range []string{"missing-pane", "missing-terminal", "inventory-error"} {
		t.Run(mode, func(t *testing.T) {
			f := newLaunchOwnershipFixture(t, "pi", func(panes []herdr.Pane) (any, string) {
				switch mode {
				case "missing-pane":
					panes = []herdr.Pane{}
				case "missing-terminal":
					for i := range panes {
						panes[i].TerminalID = ""
					}
				case "inventory-error":
					return nil, "internal_error"
				}
				return map[string]any{"type": "pane_list", "panes": panes}, ""
			})
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			result, err := f.lifecycle.Start(ctx, f.profile, f.request)
			if err == nil {
				t.Fatal("launch succeeded without verified terminal identity")
			}
			if result.PaneID != "pane-new" {
				t.Fatalf("created pane lost: %+v", result)
			}
			calls, readErr := os.ReadFile(f.calls)
			if readErr != nil {
				t.Fatal(readErr)
			}
			if strings.Contains(string(calls), "agent start") || strings.Contains(string(calls), "pane close") {
				t.Fatalf("unverified pane mutated: %s", calls)
			}
		})
	}
}

func TestLifecycleDoesNotLaunchWhenOwnershipCannotBeSaved(t *testing.T) {
	f := newLaunchOwnershipFixture(t, "pi", nil)
	if err := os.WriteFile(filepath.Dir(f.store), []byte("not a directory"), 0600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	result, err := f.lifecycle.Start(ctx, f.profile, f.request)
	if err == nil || result.PaneID != "pane-new" {
		t.Fatalf("Start = %+v, %v; want a retained pane without launching", result, err)
	}
	calls, err := os.ReadFile(f.calls)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(calls), "agent start") || strings.Contains(string(calls), "pane close") {
		t.Fatalf("unsaved ownership allowed mutation: %s", calls)
	}
}
