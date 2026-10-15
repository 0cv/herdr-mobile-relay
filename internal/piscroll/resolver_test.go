package piscroll

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/0cv/herdr-mobile-relay/internal/herdr"
	"github.com/0cv/herdr-mobile-relay/internal/panesize"
	"github.com/0cv/herdr-mobile-relay/internal/pibridge"
)

func TestResolverFreshOwnershipAndQueryBudget(t *testing.T) {
	target := testTarget()
	var metrics ResolveMetrics
	reads, bridges, geometry := 0, 0, 0
	resolver := &PiResolver{
		Instance: func() (string, error) { return "instance", nil }, Supported: func() bool { return true }, Current: func(got Target) bool { return got == target },
		Process: func(ctx context.Context, pane string) (*herdr.PaneProcessInfo, error) {
			reads++
			return &herdr.PaneProcessInfo{PaneID: pane, ShellPID: 1, ForegroundProcessGroupID: 42, ForegroundProcesses: []herdr.PaneProcess{{PID: 42}}}, nil
		},
		Bridge: func(ctx context.Context, id pibridge.Identity) (pibridge.ScrollResponse, error) {
			bridges++
			if id.Instance != "instance" || id.PID != 42 || id.Pane != target.Ref.PaneID || id.Session != target.Ref.AgentSessionID {
				t.Fatalf("unbound bridge: %+v", id)
			}
			return pibridge.ScrollResponse{Incarnation: "bridge", Renderer: "fullscreen", Wheel: "sgr_cell"}, nil
		},
		Geometry: func(ctx context.Context, pid int) (panesize.Grid, error) {
			geometry++
			if pid != 42 {
				t.Fatal("wrong process measured")
			}
			return testEvidence().Grid, nil
		}, Observe: func(got ResolveMetrics) { metrics = got },
	}
	e, err := resolver.Resolve(context.Background(), target, true)
	if err != nil || e != testEvidence() || reads != 2 || bridges != 1 || geometry != 1 || metrics.Failed || metrics.Phase != "complete" {
		t.Fatalf("evidence/budget: %+v %v %+v", e, err, metrics)
	}
	expectedPhases := []string{"target", "instance_before", "ownership_before", "geometry", "bridge", "ownership_after", "instance_after", "target_recheck", "complete"}
	if len(metrics.Phases) != len(expectedPhases) {
		t.Fatalf("phase coverage: %+v", metrics.Phases)
	}
	for i, phase := range metrics.Phases {
		if phase.Phase != expectedPhases[i] || phase.Duration < 0 {
			t.Fatalf("phase measurement: %+v", metrics.Phases)
		}
	}
	reads, bridges, geometry = 0, 0, 0
	if _, err := resolver.Resolve(context.Background(), target, false); err != nil || reads != 2 || bridges != 1 || geometry != 0 {
		t.Fatalf("cheap evidence: %v %d/%d/%d", err, reads, bridges, geometry)
	}
	original := resolver.Process
	resolver.Process = func(ctx context.Context, pane string) (*herdr.PaneProcessInfo, error) {
		info, err := original(ctx, pane)
		if reads%2 == 0 {
			info.ForegroundProcesses = append(info.ForegroundProcesses, herdr.PaneProcess{PID: 43})
		}
		return info, err
	}
	if _, err := resolver.Resolve(context.Background(), target, true); err == nil {
		t.Fatal("same-group editor accepted")
	}
	resolver.Process = original
	resolver.Supported = func() bool { return false }
	reads = 0
	if _, err := resolver.Resolve(context.Background(), target, true); err == nil || reads != 0 {
		t.Fatal("unknown capability allowed reads")
	}
	resolver.Supported = func() bool { return true }
	resolver.Bridge = func(ctx context.Context, id pibridge.Identity) (pibridge.ScrollResponse, error) {
		<-ctx.Done()
		return pibridge.ScrollResponse{}, ctx.Err()
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()
	if _, err := resolver.Resolve(ctx, target, true); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("stalled bridge: %v", err)
	}
}
func TestForegroundFailsClosedForWholeGroup(t *testing.T) {
	for _, info := range []*herdr.PaneProcessInfo{nil, {},
		{PaneID: "other", ShellPID: 1, ForegroundProcessGroupID: 42, ForegroundProcesses: []herdr.PaneProcess{{PID: 42}}},
		{PaneID: "pane", ShellPID: 42, ForegroundProcessGroupID: 42, ForegroundProcesses: []herdr.PaneProcess{{PID: 42}}},
		{PaneID: "pane", ShellPID: 1, ForegroundProcessGroupID: 42, ForegroundProcesses: []herdr.PaneProcess{{PID: 43}}},
		{PaneID: "pane", ShellPID: 1, ForegroundProcessGroupID: 42, ForegroundProcesses: []herdr.PaneProcess{{PID: 42}, {PID: 43}}},
	} {
		if _, err := Foreground(info, "pane"); err == nil {
			t.Fatalf("unsafe foreground: %+v", info)
		}
	}
}
