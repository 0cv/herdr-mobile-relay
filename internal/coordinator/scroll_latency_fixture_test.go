package coordinator

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"testing"
	"time"

	"github.com/0cv/herdr-mobile-relay/internal/herdr"
	"github.com/0cv/herdr-mobile-relay/internal/p1fixture"
	"github.com/0cv/herdr-mobile-relay/internal/piscroll"
)

func verifyDisposablePiFixture(t *testing.T, socket string) {
	t.Helper()
	contract, err := p1fixture.Current()
	if err != nil || contract.SocketPath != socket {
		t.Fatalf("owned fixture contract refused: %v", err)
	}

}

func TestDisposablePiReadOnlyLatency(t *testing.T) {
	socket := os.Getenv("HERDR_LATENCY_FIXTURE_SOCKET")
	if socket == "" {
		t.Skip("requires an owned disposable actual Herdr/Pi fixture")
	}
	verifyDisposablePiFixture(t, socket)
	contract, err := p1fixture.Current()
	if err != nil {
		t.Fatal(err)
	}
	pane, terminal, session := os.Getenv("HERDR_LATENCY_FIXTURE_PANE"), os.Getenv("HERDR_LATENCY_FIXTURE_TERMINAL"), os.Getenv("HERDR_LATENCY_FIXTURE_SESSION")
	if pane == "" || terminal == "" || session == "" {
		t.Fatal("missing exact fixture identity")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	client := herdr.NewClient(os.Getenv("HERDR_LATENCY_FIXTURE_BIN"), socket)
	defer client.Close()
	observer, err := herdr.NewScrollEvidence(1024*1024, 128)
	if err != nil {
		t.Fatal(err)
	}
	if err := client.SetScrollEvidence(observer); err != nil {
		t.Fatal(err)
	}
	client.RefreshCapabilities(ctx)
	if !client.SupportsPiScrollSocket() {
		t.Fatal("selected native schema not qualified")
	}
	runner := &p1fixture.Runner{}
	state := NewState(testLogger())
	state.CommitInventory([]*AgentState{{PaneID: pane, TerminalID: terminal, SessionID: session, Agent: "pi", Status: "idle"}}, 0)
	d := NewDispatcherWithPiScroll(client, state, nil, slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelDebug})), socket, runner)
	defer func() {
		done, stop := context.WithTimeout(context.Background(), 2*time.Second)
		defer stop()
		if err := d.Close(done); err != nil {
			t.Errorf("dispatcher closure: %v", err)
		}
	}()
	target, ok := d.scrollTarget(pane)
	if !ok {
		t.Fatal("target unavailable")
	}
	type sample struct {
		Intent    string                  `json:"intent"`
		ElapsedMS float64                 `json:"elapsed_ms"`
		ElapsedNS int64                   `json:"elapsed_ns"`
		Error     string                  `json:"error,omitempty"`
		Metrics   piscroll.ResolveMetrics `json:"metrics"`
		Evidence  piscroll.Evidence       `json:"evidence"`
	}
	samples := make([]sample, 0, 16)
	observe := d.piResolver.Observe
	for i := 0; i < 16; i++ {
		id := fmt.Sprintf("read-only-%02d", i)
		start := time.Now()
		entry := sample{Intent: id}
		d.piResolver.Observe = func(metrics piscroll.ResolveMetrics) {
			entry.Metrics = metrics
			if observe != nil {
				observe(metrics)
			}
		}
		probe, stop := context.WithDeadline(piscroll.WithIntentID(ctx, id), start.Add(250*time.Millisecond))
		entry.Evidence, err = d.piResolver.Resolve(probe, target, true)
		stop()
		entry.ElapsedNS = time.Since(start).Nanoseconds()
		entry.ElapsedMS = float64(entry.ElapsedNS) / 1000000
		if err != nil {
			entry.Error = err.Error()
		}
		samples = append(samples, entry)
		if saveErr := contract.Save(fmt.Sprintf("latency-%02d.json", i), map[string]any{"schema_version": 1, "kind": "p1-latency-sample", "source_kind": "real", "complete": err == nil && !entry.Metrics.Failed && entry.ElapsedMS <= 250, "truncated": false, "ambiguous": false, "skipped": false, "sample": entry, "started": start}); saveErr != nil {
			t.Fatal(saveErr)
		}
		if err != nil || time.Now().After(start.Add(250*time.Millisecond)) || entry.Metrics.Failed || entry.Metrics.ProcessReads != 2 || entry.Metrics.BridgeQueries != 1 || entry.Metrics.GeometryReads != 1 {
			t.Fatalf("read-only sample %s failed: %+v %v", id, entry, err)
		}
		if ctx.Err() != nil {
			t.Fatal(ctx.Err())
		}
	}
	commands, err := runner.Observations()
	if err != nil || len(commands) != 32 {
		t.Fatalf("geometry command count: %d %v", len(commands), err)
	}
	if err := contract.Save("latency-summary.json", map[string]any{"schema_version": 1, "kind": "p1-latency-summary", "source_kind": "real", "complete": true, "truncated": false, "ambiguous": false, "skipped": false, "samples": samples, "commands": commands, "drops": 16 - len(samples)}); err != nil {
		t.Fatal(err)
	}
	records, err := observer.Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	for index, record := range records {
		if err := contract.Save(fmt.Sprintf("latency-wire-%03d.json", index), map[string]any{"schema_version": 1, "kind": "p1-unary-observation", "source_kind": "real", "complete": true, "truncated": false, "ambiguous": false, "skipped": false, "record": record}); err != nil {
			t.Fatal(err)
		}
	}
}
