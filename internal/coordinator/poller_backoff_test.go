package coordinator

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/0cv/herdr-mobile-relay/internal/herdr"
)

func TestPollRetryIntervalDoublesAndSaturates(t *testing.T) {
	const healthy = 2 * time.Second
	tests := []struct {
		failures int
		want     time.Duration
	}{
		{failures: 0, want: 2 * time.Second},
		{failures: 1, want: 4 * time.Second},
		{failures: 2, want: 8 * time.Second},
		{failures: 3, want: 16 * time.Second},
		{failures: 4, want: 32 * time.Second},
		{failures: 5, want: maxPollRetryInterval},
		{failures: 64, want: maxPollRetryInterval},
	}
	for _, tt := range tests {
		t.Run(fmt.Sprintf("failures_%d", tt.failures), func(t *testing.T) {
			if got := pollRetryInterval(healthy, tt.failures); got != tt.want {
				t.Fatalf("pollRetryInterval(%v, %d) = %v, want %v", healthy, tt.failures, got, tt.want)
			}
		})
	}

	maxInt := int(^uint(0) >> 1)
	if got := pollRetryInterval(time.Nanosecond, maxInt); got != maxPollRetryInterval {
		t.Fatalf("very large failure streak = %v, want %v", got, maxPollRetryInterval)
	}
	if got := pollRetryInterval(0, 1); got != 2*idlePollInterval {
		t.Fatalf("nonpositive interval retry = %v, want %v", got, 2*idlePollInterval)
	}
	if got := pollRetryInterval(time.Hour, 0); got != idlePollInterval {
		t.Fatalf("oversized interval = %v, want %v", got, idlePollInterval)
	}
}

func TestPollerRetryFailureStreakSaturates(t *testing.T) {
	poller := NewPoller(nil, testState(), time.Second, testLogger())
	for range maxPollRetryFailures + 10 {
		poller.recordPollFailure()
	}
	if got := poller.pollRetryFailures; got != maxPollRetryFailures {
		t.Fatalf("retry failure streak = %d, want cap %d", got, maxPollRetryFailures)
	}
}

func TestPollerRetryIntervalUsesEventReconciliationCadence(t *testing.T) {
	poller := NewPoller(nil, testState(), time.Second, testLogger())
	poller.eventsActive.Store(true)

	for failures, want := range map[int]time.Duration{
		0: idlePollInterval,
		1: 30 * time.Second,
		2: maxPollRetryInterval,
	} {
		poller.pollRetryFailures = failures
		if got := poller.nextPollInterval(); got != want {
			t.Fatalf("event-active retry interval after %d failures = %v, want %v", failures, got, want)
		}
	}
}

func TestPollerRequiredFetchFailuresAccumulateAndRecover(t *testing.T) {
	t.Run("inventory", func(t *testing.T) {
		var mode atomic.Int32
		var logs bytes.Buffer
		socketPath := startPollerTestSocket(t, func(method string) (any, string) {
			if method == "agent.list" && mode.Load() == 0 {
				return nil, "upstream_down"
			}
			return pollerSuccessResult(method), ""
		})
		poller := NewPoller(
			herdr.NewClient(filepath.Join(t.TempDir(), "missing-herdr"), socketPath),
			testState(),
			time.Second,
			slog.New(slog.NewTextHandler(&logs, nil)),
		)

		for range 3 {
			poller.poll(context.Background())
		}
		if got := poller.pollRetryFailures; got != 3 {
			t.Fatalf("inventory retry failures = %d, want 3", got)
		}
		if got := poller.ConsecutiveFailures(); got != 3 {
			t.Fatalf("inventory health failures = %d, want 3", got)
		}
		if got := strings.Count(logs.String(), "level=WARN"); got != 3 {
			t.Fatalf("WARN records = %d, want one per failed attempt", got)
		}

		mode.Store(1)
		poller.poll(context.Background())
		if poller.pollRetryFailures != 0 || poller.ConsecutiveFailures() != 0 {
			t.Fatalf("successful required fetches did not reset failures: retry=%d health=%d", poller.pollRetryFailures, poller.ConsecutiveFailures())
		}
		if !poller.state.InventoryReady() {
			t.Fatal("successful required fetches did not restore inventory readiness")
		}

		mode.Store(0)
		poller.poll(context.Background())
		if poller.pollRetryFailures != 1 {
			t.Fatalf("later outage retry failures = %d, want a fresh streak", poller.pollRetryFailures)
		}
	})

	t.Run("workspace", func(t *testing.T) {
		var mode atomic.Int32
		socketPath := startPollerTestSocket(t, func(method string) (any, string) {
			if method == "workspace.list" && mode.Load() == 0 {
				return nil, "upstream_down"
			}
			return pollerSuccessResult(method), ""
		})
		poller := NewPoller(
			herdr.NewClient(filepath.Join(t.TempDir(), "missing-herdr"), socketPath),
			testState(),
			time.Second,
			testLogger(),
		)

		for range 3 {
			poller.poll(context.Background())
		}
		if got := poller.pollRetryFailures; got != 3 {
			t.Fatalf("workspace retry failures = %d, want 3", got)
		}
		if got := poller.ConsecutiveFailures(); got != 3 {
			t.Fatalf("workspace health failures = %d, want 3", got)
		}

		mode.Store(1)
		poller.poll(context.Background())
		if poller.pollRetryFailures != 0 || poller.ConsecutiveFailures() != 0 {
			t.Fatalf("workspace recovery did not reset failures: retry=%d health=%d", poller.pollRetryFailures, poller.ConsecutiveFailures())
		}
	})
}

func TestPollerOptionalFetchFailuresDoNotBackOff(t *testing.T) {
	var optionalFailures atomic.Bool
	socketPath := startPollerTestSocket(t, func(method string) (any, string) {
		if optionalFailures.Load() && (method == "tab.list" || method == "pane.list") {
			return nil, "optional_fetch_failed"
		}
		return pollerSuccessResult(method), ""
	})
	poller := NewPoller(
		herdr.NewClient(filepath.Join(t.TempDir(), "missing-herdr"), socketPath),
		testState(),
		time.Second,
		testLogger(),
	)

	poller.poll(context.Background())
	optionalFailures.Store(true)
	poller.poll(context.Background())
	if poller.pollRetryFailures != 0 || poller.ConsecutiveFailures() != 0 {
		t.Fatalf("optional failures changed retry state: retry=%d health=%d", poller.pollRetryFailures, poller.ConsecutiveFailures())
	}
	if !poller.state.InventoryReady() {
		t.Fatal("optional fallback failure prevented a successful required poll")
	}
}

func TestPollerTopologyStaleCommitDoesNotBackOff(t *testing.T) {
	state := testState()
	socketPath := startPollerTestSocket(t, func(method string) (any, string) {
		return pollerSuccessResult(method), ""
	})
	poller := NewPoller(
		herdr.NewClient(filepath.Join(t.TempDir(), "missing-herdr"), socketPath),
		state,
		time.Second,
		testLogger(),
	)
	poller.SetEnrich(func(context.Context, []*AgentState) {
		state.MarkTopologyChanged()
	})

	poller.poll(context.Background())
	if poller.pollRetryFailures != 0 || poller.ConsecutiveFailures() != 0 {
		t.Fatalf("stale topology changed retry state: retry=%d health=%d", poller.pollRetryFailures, poller.ConsecutiveFailures())
	}
	if got := state.TopologyRetryCount(); got != 1 {
		t.Fatalf("topology retries = %d, want 1", got)
	}
}

func TestEventTopologyRecoveryDoesNotResetPollRetryStreak(t *testing.T) {
	poller := NewPoller(nil, testState(), time.Second, testLogger())
	poller.pollRetryFailures = 3
	poller.consecutiveFailures.Store(3)

	poller.commitEventTopology(context.Background(), herdr.TopologySnapshot{}, poller.state.RevisionCounter())
	if got := poller.pollRetryFailures; got != 3 {
		t.Fatalf("event commit reset poll retry failures to %d, want 3", got)
	}
	if got := poller.ConsecutiveFailures(); got != 0 {
		t.Fatalf("event commit did not preserve health recovery: health failures = %d", got)
	}
}

func TestPollerWakeDuringBackoffIsPromptAndCoalesced(t *testing.T) {
	attempts := make(chan struct{}, 8)
	warnings := make(chan struct{}, 8)
	socketPath := startPollerTestSocket(t, func(method string) (any, string) {
		if method == "agent.list" {
			attempts <- struct{}{}
			return nil, "upstream_down"
		}
		return pollerSuccessResult(method), ""
	})
	poller := NewPoller(
		herdr.NewClient(filepath.Join(t.TempDir(), "missing-herdr"), socketPath),
		testState(),
		500*time.Millisecond,
		slog.New(&pollerSignalHandler{signals: warnings}),
	)
	ctx, cancel := context.WithCancel(context.Background())
	runDone := make(chan struct{})
	go func() {
		poller.Run(ctx)
		close(runDone)
	}()

	select {
	case <-attempts:
	case <-time.After(time.Second):
		t.Fatal("initial poll did not run")
	}
	select {
	case <-warnings:
	case <-time.After(time.Second):
		t.Fatal("initial poll did not finish")
	}
	poller.Wake()
	select {
	case <-attempts:
	case <-time.After(250 * time.Millisecond):
		cancel()
		<-runDone
		t.Fatal("wake did not interrupt the retry backoff")
	}
	select {
	case <-warnings:
	case <-time.After(time.Second):
		cancel()
		<-runDone
		t.Fatal("wake-triggered poll did not finish")
	}
	cancel()
	select {
	case <-runDone:
	case <-time.After(500 * time.Millisecond):
		t.Fatal("poller did not stop after cancellation")
	}
	if got := poller.pollRetryFailures; got != 2 {
		t.Fatalf("retry failures after wake = %d, want 2", got)
	}
	if got := poller.nextPollInterval(); got != 2*time.Second {
		t.Fatalf("next retry after wake = %v, want 2s", got)
	}
}

func TestPollerWakeRequestsAreCoalesced(t *testing.T) {
	poller := NewPoller(nil, testState(), time.Second, testLogger())
	for range 16 {
		poller.Wake()
	}
	select {
	case <-poller.wakeup:
	default:
		t.Fatal("wake request was not queued")
	}
	select {
	case <-poller.wakeup:
		t.Fatal("multiple wake requests were not coalesced")
	default:
	}
}

func TestPollerCancellationDuringFetchDoesNotRecordFailure(t *testing.T) {
	fetchStarted := make(chan struct{}, 1)
	releaseFetch := make(chan struct{})
	socketPath := startPollerTestSocket(t, func(method string) (any, string) {
		if method == "agent.list" {
			fetchStarted <- struct{}{}
			<-releaseFetch
		}
		return nil, "upstream_down"
	})
	var logs bytes.Buffer
	poller := NewPoller(
		herdr.NewClient(filepath.Join(t.TempDir(), "missing-herdr"), socketPath),
		testState(),
		time.Second,
		slog.New(slog.NewTextHandler(&logs, nil)),
	)
	ctx, cancel := context.WithCancel(context.Background())
	pollDone := make(chan struct{})
	go func() {
		poller.poll(ctx)
		close(pollDone)
	}()
	select {
	case <-fetchStarted:
	case <-time.After(time.Second):
		close(releaseFetch)
		t.Fatal("inventory fetch did not start")
	}
	cancel()
	select {
	case <-pollDone:
	case <-time.After(500 * time.Millisecond):
		close(releaseFetch)
		t.Fatal("cancelled inventory fetch did not stop promptly")
	}
	close(releaseFetch)
	if poller.pollRetryFailures != 0 || poller.ConsecutiveFailures() != 0 {
		t.Fatalf("cancelled fetch changed retry state: retry=%d health=%d", poller.pollRetryFailures, poller.ConsecutiveFailures())
	}
	if strings.Contains(logs.String(), "level=WARN") {
		t.Fatalf("cancelled fetch emitted a warning: %s", logs.String())
	}
}

type pollerSignalHandler struct {
	signals chan<- struct{}
}

func (h *pollerSignalHandler) Enabled(context.Context, slog.Level) bool {
	return true
}

func (h *pollerSignalHandler) Handle(context.Context, slog.Record) error {
	h.signals <- struct{}{}
	return nil
}

func (h *pollerSignalHandler) WithAttrs([]slog.Attr) slog.Handler {
	return h
}

func (h *pollerSignalHandler) WithGroup(string) slog.Handler {
	return h
}

func startPollerTestSocket(t *testing.T, responder func(method string) (result any, errorCode string)) string {
	t.Helper()
	socketPath := filepath.Join(t.TempDir(), "herdr.sock")
	listener, err := net.Listen("unix", socketPath)
	if err != nil {
		t.Fatalf("listen poller socket: %v", err)
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			conn, acceptErr := listener.Accept()
			if acceptErr != nil {
				return
			}
			func() {
				defer conn.Close()
				request := struct {
					ID     string `json:"id"`
					Method string `json:"method"`
				}{}
				if err := json.NewDecoder(bufio.NewReader(conn)).Decode(&request); err != nil {
					return
				}
				result, errorCode := responder(request.Method)
				if errorCode != "" {
					_ = json.NewEncoder(conn).Encode(map[string]any{
						"id": request.ID,
						"error": map[string]string{
							"code": errorCode, "message": errorCode,
						},
					})
					return
				}
				_ = json.NewEncoder(conn).Encode(map[string]any{
					"id": request.ID, "result": result,
				})
			}()
		}
	}()
	t.Cleanup(func() {
		_ = listener.Close()
		<-done
	})
	return socketPath
}

func pollerSuccessResult(method string) any {
	switch method {
	case "agent.list":
		return map[string]any{"type": "agent_list", "agents": []any{}}
	case "workspace.list":
		return map[string]any{"type": "workspace_list", "workspaces": []any{}}
	case "tab.list":
		return map[string]any{"type": "tab_list", "tabs": []any{}}
	case "pane.list":
		return map[string]any{"type": "pane_list", "panes": []any{}}
	default:
		return map[string]any{"type": method}
	}
}
