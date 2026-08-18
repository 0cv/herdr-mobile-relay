package coordinator

// Agent creation against a pane whose shell has not reached a prompt: Herdr
// answers agent_pane_busy in about a millisecond, before its own --timeout
// window opens. The relay must absorb that refusal and must never destroy the
// target Herdr created.

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/0cv/herdr-mobile-relay/internal/herdr"
	"github.com/0cv/herdr-mobile-relay/internal/profiles"
)

const paneBusyEnvelope = `{"error":{"code":"agent_pane_busy",` +
	`"message":"agent target pane wH:p1 is not an available shell"},"id":"cli:agent:start"}`

// busyHerdr answers the workspace-creation calls, then refuses the first
// refusals invocations of `agent start` exactly as Herdr 0.8.0 does before
// succeeding. A negative refusals count refuses forever.
func busyHerdr(t *testing.T, dir, record string, refusals int) string {
	t.Helper()
	counter := filepath.Join(dir, "attempts")
	return writeScript(t, dir, "herdr", "#!/bin/sh\n"+
		"printf '%s\\n' \"$*\" >> \""+record+"\"\n"+
		"case \"$1 $2\" in\n"+
		"  'pane list') printf '%s\\n' '{\"result\":{\"panes\":[]}}' ;;\n"+
		"  'workspace list') printf '%s\\n' '{\"result\":{\"workspaces\":[]}}' ;;\n"+
		"  'workspace create') printf '%s\\n' '{\"result\":{\"type\":\"workspace_created\",\"workspace\":{\"workspace_id\":\"workspace-new\"},\"tab\":{\"tab_id\":\"tab-new\",\"workspace_id\":\"workspace-new\"},\"root_pane\":{\"pane_id\":\"pane-new\",\"tab_id\":\"tab-new\",\"workspace_id\":\"workspace-new\"}}}' ;;\n"+
		"  'tab rename') printf '%s\\n' '{\"result\":{\"tab_id\":\"tab-new\"}}' ;;\n"+
		"  'agent start')\n"+
		"    attempts=$(cat \""+counter+"\" 2>/dev/null || printf 0)\n"+
		"    attempts=$((attempts + 1))\n"+
		"    printf '%s' \"$attempts\" > \""+counter+"\"\n"+
		"    if [ "+strconv.Itoa(refusals)+" -lt 0 ] || [ \"$attempts\" -le "+strconv.Itoa(refusals)+" ]; then\n"+
		"      printf '%s\\n' '"+paneBusyEnvelope+"' >&2\n"+
		"      exit 1\n"+
		"    fi\n"+
		"    printf '%s\\n' '{\"result\":{\"type\":\"agent_started\",\"agent\":{\"pane_id\":\"pane-new\",\"agent\":\"codex\"}}}' ;;\n"+
		"  *) exit 2 ;;\n"+
		"esac\n")
}

// busyLifecycle builds a Lifecycle whose profile resolver discovers a "codex"
// profile: profiles are discovered from PATH, and the discovered kind is what
// routes the start through `herdr agent start`.
func busyLifecycle(t *testing.T, dir, bin string) (*Lifecycle, string) {
	t.Helper()
	home := filepath.Join(dir, "home")
	cwd := filepath.Join(home, "project")
	if err := os.MkdirAll(cwd, 0o755); err != nil {
		t.Fatalf("create project directory: %v", err)
	}
	pathDir := filepath.Join(dir, "path")
	if err := os.MkdirAll(pathDir, 0o755); err != nil {
		t.Fatalf("create path directory: %v", err)
	}
	writeScript(t, pathDir, "codex", "#!/bin/sh\nexit 0\n")
	t.Setenv("PATH", pathDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	return &Lifecycle{
		herdr:    herdr.NewClient(bin, filepath.Join(dir, "herdr.sock")),
		profiles: profiles.NewResolver(filepath.Join(dir, "config"), nil),
		home:     home,
	}, cwd
}

func startInvocations(t *testing.T, record string) []string {
	t.Helper()
	data, err := os.ReadFile(record)
	if err != nil {
		t.Fatalf("read invocations: %v", err)
	}
	var starts []string
	for _, line := range strings.Split(string(data), "\n") {
		if strings.HasPrefix(line, "agent start ") {
			starts = append(starts, line)
		}
	}
	return starts
}

func TestAgentStartRetriesWhileHerdrRefusesTheNewPane(t *testing.T) {
	dir := t.TempDir()
	record := filepath.Join(dir, "invocations.log")
	lifecycle, cwd := busyLifecycle(t, dir, busyHerdr(t, dir, record, 2))

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	result, err := lifecycle.Start(ctx, profiles.Profile{ID: "codex", Kind: "codex"}, StartRequest{
		ProfileID: "codex",
		Name:      "project-codex",
		Cwd:       cwd,
	})
	if err != nil {
		t.Fatalf("Start() error = %v, want the retry to outlast two refusals", err)
	}
	if result.PaneID != "pane-new" {
		t.Fatalf("Start() pane_id = %q, want pane-new", result.PaneID)
	}

	starts := startInvocations(t, record)
	if len(starts) != 3 {
		t.Fatalf("agent start invocations = %d, want 3 (two refusals then success):\n%s",
			len(starts), strings.Join(starts, "\n"))
	}
	data, _ := os.ReadFile(record)
	if strings.Contains(string(data), "pane close") {
		t.Fatalf("target was closed during the retry:\n%s", data)
	}
}

// Every attempt must ask Herdr to wait no longer than the request's own
// remaining budget. A fixed --timeout would let the first attempt consume the
// whole window once Herdr learns to honour it, and no retry would ever run.
func TestAgentStartPassesTheRemainingBudgetAsTimeout(t *testing.T) {
	dir := t.TempDir()
	record := filepath.Join(dir, "invocations.log")
	lifecycle, cwd := busyLifecycle(t, dir, busyHerdr(t, dir, record, 1))

	// startupDeadline is this deadline minus agentStartResponseReserve, so
	// every attempt must report well under 3s of remaining budget.
	ctx, cancel := context.WithTimeout(context.Background(), agentStartResponseReserve+3*time.Second)
	defer cancel()
	if _, err := lifecycle.Start(ctx, profiles.Profile{ID: "codex", Kind: "codex"}, StartRequest{
		ProfileID: "codex",
		Name:      "project-codex",
		Cwd:       cwd,
	}); err != nil {
		t.Fatalf("Start() error = %v", err)
	}

	starts := startInvocations(t, record)
	if len(starts) != 2 {
		t.Fatalf("agent start invocations = %d, want 2:\n%s", len(starts), strings.Join(starts, "\n"))
	}
	for _, start := range starts {
		fields := strings.Fields(start)
		index := len(fields) - 1
		if index < 1 || fields[index-1] != "--timeout" {
			t.Fatalf("invocation has no trailing --timeout value: %q", start)
		}
		timeout, err := strconv.Atoi(fields[index])
		if err != nil {
			t.Fatalf("--timeout value %q is not a number: %v", fields[index], err)
		}
		if timeout <= 0 || timeout > 3000 {
			t.Fatalf("--timeout = %d, want the remaining budget (0 < t <= 3000): %q", timeout, start)
		}
	}
}

// Defect 2 of issue #8: the relay used to close the pane after a failed start,
// so the user lost the workspace Herdr had just created and had nothing to
// retry into.
func TestAgentStartKeepsTheTargetWhenHerdrKeepsRefusing(t *testing.T) {
	dir := t.TempDir()
	record := filepath.Join(dir, "invocations.log")
	lifecycle, cwd := busyLifecycle(t, dir, busyHerdr(t, dir, record, -1))

	ctx, cancel := context.WithTimeout(context.Background(), agentStartResponseReserve+400*time.Millisecond)
	defer cancel()
	result, err := lifecycle.Start(ctx, profiles.Profile{ID: "codex", Kind: "codex"}, StartRequest{
		ProfileID: "codex",
		Name:      "project-codex",
		Cwd:       cwd,
	})
	if err == nil {
		t.Fatal("Start() succeeded, want the persistent refusal to surface")
	}
	if !herdr.IsRefused(err) {
		t.Fatalf("Start() error = %v, want the refusal preserved for a safe-retry classification", err)
	}
	if result.PaneID != "pane-new" {
		t.Fatalf("Start() pane_id = %q, want the kept target pane-new", result.PaneID)
	}
	data, _ := os.ReadFile(record)
	if strings.Contains(string(data), "pane close") {
		t.Fatalf("failed start destroyed the target Herdr created:\n%s", data)
	}
	if starts := startInvocations(t, record); len(starts) < 2 {
		t.Fatalf("agent start invocations = %d, want the refusal retried at least once", len(starts))
	}
}

// The kept pane is only useful if the phone learns about it: the failure must
// carry the pane id and the topology must be published.
func TestAgentStartFailureSurfacesTheKeptPane(t *testing.T) {
	dir := t.TempDir()
	record := filepath.Join(dir, "invocations.log")
	bin := busyHerdr(t, dir, record, -1)
	lifecycle, cwd := busyLifecycle(t, dir, bin)

	state := NewState(testLogger())
	d := NewDispatcher(herdr.NewClient(bin, filepath.Join(dir, "herdr.sock")), state, nil, testLogger())
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		if err := d.Close(ctx); err != nil {
			t.Fatalf("close dispatcher: %v", err)
		}
	})
	d.profiles = lifecycle.profiles
	d.lifecycle = lifecycle
	before := state.TopologyGeneration()

	// The caller's deadline shortens the 40s command deadline, so the retry
	// window closes as soon as the startup reserve is exhausted.
	ctx, cancel := context.WithTimeout(context.Background(), agentStartResponseReserve+400*time.Millisecond)
	defer cancel()
	result := d.handleAgentStart(ctx, time.Now(), "request-1", map[string]any{
		"profile_id": "codex",
		"name":       "project-codex",
		"cwd":        cwd,
	})
	if result.OK {
		t.Fatal("agent_start reported success against a permanently refusing Herdr")
	}
	if result.Phase != "not_started" || result.Error != "Command was not sent; retry is safe" {
		t.Fatalf("result = %+v, want a safe-retry classification for agent_pane_busy", result)
	}
	if result.PaneID != "pane-new" {
		t.Fatalf("result pane_id = %q, want the kept target pane-new", result.PaneID)
	}
	if state.TopologyGeneration() == before {
		t.Fatal("topology was not published, so the phone never sees the pane that survived")
	}
}
