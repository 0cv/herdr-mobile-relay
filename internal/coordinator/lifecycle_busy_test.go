package coordinator

// Agent creation against a pane whose shell has not reached a prompt: Herdr
// answers agent_pane_busy in about a millisecond, before its own --timeout
// window opens. The relay must absorb that refusal and must never destroy the
// target Herdr created.

import (
	"context"
	"errors"
	"strconv"
	"testing"
	"time"

	"github.com/0cv/herdr-mobile-relay/internal/herdr"
	"github.com/0cv/herdr-mobile-relay/internal/profiles"
)

// busyLifecycle builds a Lifecycle whose profile resolver discovers a "codex"
// profile: profiles are discovered from PATH, and the discovered kind is what
// routes the start through `herdr agent start`.
func busyLifecycle(t *testing.T, client *fakeBackend) (*Lifecycle, string) {
	t.Helper()
	env := newLaunchEnv(t, "codex")
	return &Lifecycle{
		herdr:    client,
		profiles: profiles.NewResolver(env.config, nil),
		home:     env.home,
	}, env.cwd
}

func stopAfterRefusals(lifecycle *Lifecycle, count int) {
	lifecycle.waitForRetry = func(ctx context.Context, _ time.Duration) bool {
		count--
		if count == 0 {
			return false
		}
		return ctx.Err() == nil
	}
}

func busyClient(refusals int) *fakeBackend {
	client := newLaunchFake()
	client.setHook("StartAgent", refuseStarts(refusals))
	return client
}

func TestAgentStartRetriesWhileHerdrRefusesTheNewPane(t *testing.T) {
	client := busyClient(2)
	lifecycle, cwd := busyLifecycle(t, client)

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

	if starts := client.count("StartAgent"); starts != 3 {
		t.Fatalf("agent start invocations = %d, want 3 (two refusals then success)", starts)
	}
	if client.count("StopPane") != 0 {
		t.Fatalf("target was closed during the retry: %+v", client.recorded())
	}
}

// Every attempt must ask Herdr to wait no longer than the request's own
// remaining budget. A fixed --timeout would let the first attempt consume the
// whole window once Herdr learns to honour it, and no retry would ever run.
func TestAgentStartPassesTheRemainingBudgetAsTimeout(t *testing.T) {
	client := busyClient(1)
	lifecycle, cwd := busyLifecycle(t, client)

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

	starts := client.callsTo("StartAgent")
	if len(starts) != 2 {
		t.Fatalf("agent start invocations = %d, want 2: %+v", len(starts), starts)
	}
	for _, start := range starts {
		if timeout := start.Args[3].(int); timeout <= 0 || timeout > 3000 {
			t.Fatalf("--timeout = %d, want the remaining budget (0 < t <= 3000): %+v", timeout, start)
		}
	}
}

// Defect 2 of issue #8: the relay used to close the pane after a failed start,
// so the user lost the workspace Herdr had just created and had nothing to
// retry into.
func TestAgentStartKeepsTheTargetWhenHerdrKeepsRefusing(t *testing.T) {
	client := busyClient(-1)
	lifecycle, cwd := busyLifecycle(t, client)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	stopAfterRefusals(lifecycle, 2)
	result, err := lifecycle.Start(ctx, profiles.Profile{ID: "codex", Kind: "codex"}, StartRequest{
		ProfileID: "codex",
		Name:      "project-codex",
		Cwd:       cwd,
	})
	if err == nil {
		t.Fatal("Start() succeeded, want the persistent refusal to surface")
	}
	if !herdr.IsRefused(err) {
		outcomeStarted := "none"
		var outcome *herdr.OutcomeError
		if errors.As(err, &outcome) {
			outcomeStarted = strconv.FormatBool(outcome.Started)
		}
		t.Fatalf("Start() error = %v, want the refusal preserved for a safe-retry classification"+
			" (not_started=%t dispatched_unknown=%t deadline_exceeded=%t outcome_started=%s agent_start_invocations=%d)",
			err, errors.Is(err, herdr.ErrNotStarted), errors.Is(err, herdr.ErrDispatchedUnknown),
			errors.Is(err, context.DeadlineExceeded), outcomeStarted, client.count("StartAgent"))
	}
	if result.PaneID != "pane-new" {
		t.Fatalf("Start() pane_id = %q, want the kept target pane-new", result.PaneID)
	}
	if client.count("StopPane") != 0 {
		t.Fatalf("failed start destroyed the target Herdr created: %+v", client.recorded())
	}
	if starts := client.count("StartAgent"); starts < 2 {
		t.Fatalf("agent start invocations = %d, want the refusal retried at least once", starts)
	}
}

// The kept pane is only useful if the phone learns about it: the failure must
// carry the pane id and the topology must be published.
func TestAgentStartFailureSurfacesTheKeptPane(t *testing.T) {
	client := busyClient(-1)
	lifecycle, cwd := busyLifecycle(t, client)

	state := NewState(testLogger())
	d := newTestDispatcher(t, client, state)
	d.profiles = lifecycle.profiles
	d.lifecycle = lifecycle
	before := state.TopologyGeneration()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	stopAfterRefusals(lifecycle, 2)
	result := d.handleAgentStart(ctx, d.newCommandRequest(time.Now(), "request-1", "", map[string]any{
		"profile_id": "codex",
		"name":       "project-codex",
		"cwd":        cwd,
	}, nil))
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
