package coordinator

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/0cv/herdr-mobile-relay/internal/herdr"
	"github.com/0cv/herdr-mobile-relay/internal/profiles"
)

// launchFixture uses the server's dispatcher/resolver wiring, not a fake lifecycle.
type launchFixture struct {
	dispatcher                    *Dispatcher
	ctx                           context.Context
	cwd, calls, config, ownership string
	target                        profiles.PaneIdentity
}

type launchFixtureOptions struct {
	start, prompt, enter string
	close                string
}

func newLaunchFixture(t *testing.T) *launchFixture {
	t.Helper()
	return newLaunchFixtureWithOptions(t, launchFixtureOptions{})
}

func newLaunchFixtureWithOptions(t *testing.T, options launchFixtureOptions) *launchFixture {
	t.Helper()
	dir := t.TempDir()
	home := filepath.Join(dir, "home")
	f := &launchFixture{
		cwd: filepath.Join(home, "project"), calls: filepath.Join(dir, "calls"),
		config: filepath.Join(dir, "config"), ownership: filepath.Join(dir, "ownership.json"),
		target: profiles.PaneIdentity{PaneID: "pane-new", TerminalID: "terminal-new", TabID: "tab-new", WorkspaceID: "workspace-new"},
	}
	for _, path := range []string{f.cwd, filepath.Join(f.config, "herdr")} {
		if err := os.MkdirAll(path, 0700); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", f.config)
	t.Setenv("PATH", dir)
	writeScript(t, dir, "claude", "#!/bin/sh\nexit 0\n")
	writeScript(t, dir, "qodercli", "#!/bin/sh\nexit 0\n")
	if err := os.WriteFile(filepath.Join(f.config, "herdr", "agent-profiles.ini"), []byte("[profiles]\nclaude = Test Claude\nqodercli = Test Qoder\n"), 0600); err != nil {
		t.Fatal(err)
	}
	created := filepath.Join(dir, "created")
	bin := writeScript(t, dir, "herdr", `#!/bin/sh
printf '%s\n' "$*" >> '`+f.calls+`'
case "$1 $2" in
 'workspace create')
  printf created > '`+created+`'
  printf '%s\n' '{"result":{"root_pane":{"pane_id":"pane-new","tab_id":"tab-new","workspace_id":"workspace-new"}}}' ;;
 'tab rename') printf '%s\n' '{"result":{}}' ;;
 'agent start')
  pending=false
  while IFS= read -r line || [ -n "$line" ]; do
   case "$line" in *'"pending":true'*) pending=true ;; esac
  done < '`+f.ownership+`'
  [ "$pending" = true ] || exit 2
  `+options.start+`
  printf '%s\n' '{"result":{"pane_id":"pane-new"}}' ;;
 'agent prompt') `+options.prompt+`
  printf '%s\n' '{"result":{}}' ;;
 'pane close') `+options.close+`
  printf '%s\n' '{"result":{}}' ;;
 'pane send-text') printf '%s\n' '{"result":{}}' ;;
 'pane send-keys') `+options.enter+`
  printf '%s\n' '{"result":{}}' ;;
 *) exit 2 ;;
esac
`)
	socket := startPollerTestSocket(t, func(method string) (any, string) {
		if method != "pane.list" {
			return pollerSuccessResult(method), ""
		}
		panes := []herdr.Pane{}
		if _, err := os.Stat(created); err == nil {
			panes = append(panes, herdr.Pane{ID: f.target.PaneID, TerminalID: f.target.TerminalID, TabID: f.target.TabID, WorkspaceID: f.target.WorkspaceID, Cwd: f.cwd})
		} else if !os.IsNotExist(err) {
			return nil, "internal_error"
		}
		return map[string]any{"type": "pane_list", "panes": panes}, ""
	})
	client := herdr.NewClient(bin, socket)
	t.Cleanup(func() {
		if err := client.Close(); err != nil {
			t.Error(err)
		}
	})
	resolver := profiles.NewResolver(f.config, nil)
	if err := resolver.SetOwnershipPath(f.ownership); err != nil {
		t.Fatal(err)
	}
	f.dispatcher = NewDispatcher(client, NewState(testLogger()), nil, testLogger())
	f.dispatcher.SetProfiles(resolver)
	if f.dispatcher.lifecycle == nil || f.dispatcher.lifecycle.profiles != resolver || f.dispatcher.lifecycle.state != f.dispatcher.state {
		t.Fatal("production lifecycle wiring missing")
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		if err := f.dispatcher.Close(ctx); err != nil {
			t.Error(err)
		}
	})
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

func (f *launchFixture) assertCalls(t *testing.T, prompts int) {
	t.Helper()
	data, err := os.ReadFile(f.calls)
	if err != nil {
		t.Fatal(err)
	}
	for command, want := range map[string]int{"workspace create ": 1, "tab create ": 0, "tab rename tab-new proj": 1, "agent start ": 1, "agent start proj --kind claude --pane pane-new --timeout ": 1, "agent prompt ": prompts, "agent prompt pane-new hello": prompts} {
		got := 0
		for _, line := range strings.Split(string(data), "\n") {
			if strings.HasPrefix(line, command) {
				got++
			}
		}
		if got != want {
			t.Fatalf("%q calls = %d, want %d\n%s", command, got, want, data)
		}
	}
}
