package hc

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/0cv/herdr-mobile-relay/internal/hc/hctest"
)

func TestRealCoreAgentSession(t *testing.T) {
	var root string
	socket, _ := hctest.StartCore(t, func(scratch string, env []string) []string {
		root = scratch
		filtered := make([]string, 0, len(env))
		for _, value := range env {
			if !strings.HasPrefix(value, "CURSOR_VERSION=") && !strings.HasPrefix(value, "CODEX_THREAD_ID=") {
				filtered = append(filtered, value)
			}
		}
		return filtered
	})
	binary := filepath.Join(root, "relay")
	build := exec.Command("go", "build", "-o", binary, "../../cmd/herdr-mobile-relay")
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build: %v: %s", err, output)
	}
	release := filepath.Join(root, "release")
	script := filepath.Join(root, "agent.sh")
	body := "sleep 1\nprintf '%s' '{\"session_id\":\"abc\",\"hook_event_name\":\"SessionStart\"}' | " + binary + " agent-session claude\nwhile [ ! -f " + release + " ]; do sleep 0.1; done\n"
	if err := os.WriteFile(script, []byte(body), 0600); err != nil {
		t.Fatal(err)
	}
	client := New(socket)
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()
	raw, err := client.call(ctx, "terminal.run", map[string]any{"argv": []string{"/bin/sh"}, "cwd": root, "no_parent": true})
	if err != nil {
		t.Fatal(err)
	}
	var result struct {
		Address string `json:"address"`
	}
	if err := json.Unmarshal(raw, &result); err != nil || result.Address == "" {
		t.Fatalf("run: %s %v", raw, err)
	}
	t.Cleanup(func() {
		for _, method := range []string{"terminal.kill", "terminal.rm"} {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			_, err := client.call(ctx, method, map[string]any{"terminal": result.Address})
			cancel()
			if err != nil {
				t.Errorf("cleanup: %v", err)
			}
		}
	})
	if err := client.Prompt(ctx, result.Address, "sh "+script); err != nil {
		t.Fatal(err)
	}
	wait := func(check func() bool) {
		t.Helper()
		for ctx.Err() == nil {
			if check() {
				return
			}
			time.Sleep(50 * time.Millisecond)
		}
		t.Fatal("condition timed out")
	}
	var labelled *terminal
	wait(func() bool {
		term, err := client.getTerminal(ctx, result.Address)
		if err != nil {
			t.Fatal(err)
		}
		labelled = term
		return term.Labels[SessionLabel] == "abc"
	})
	if labelled.Foreground == nil || labelled.Labels[SessionPIDLabel] != strconv.Itoa(labelled.Foreground.PID) {
		t.Fatalf("labels not bound to foreground: %+v", labelled)
	}
	t.Logf("fake agent foreground pid %d; both labels verified", labelled.Foreground.PID)
	assertSession := func(want string) bool {
		panes, err := client.PaneList(ctx)
		if err != nil {
			t.Fatal(err)
		}
		for _, p := range panes {
			if p.ID == result.Address {
				return p.Session == want && p.SessionRaw.Value == want
			}
		}
		return false
	}
	if !assertSession("abc") {
		t.Fatal("PaneList session missing")
	}
	if err := os.WriteFile(release, nil, 0600); err != nil {
		t.Fatal(err)
	}
	wait(func() bool { return assertSession("") })
	after, err := client.getTerminal(ctx, result.Address)
	if err != nil || after.Labels[SessionLabel] != "abc" {
		t.Fatalf("stale label missing: %+v %v", after, err)
	}
	t.Log("fake agent exited; retained label ignored")
}
