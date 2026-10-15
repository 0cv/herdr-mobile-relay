package piscroll

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/0cv/herdr-mobile-relay/internal/panesize"
	"github.com/0cv/herdr-mobile-relay/internal/protocol"
)

type resolverFunc func(context.Context, Target, bool) (Evidence, error)

func (f resolverFunc) Resolve(ctx context.Context, target Target, measure bool) (Evidence, error) {
	return f(ctx, target, measure)
}
func testTarget() Target {
	return Target{Ref: protocol.TargetRef{ServerSessionID: "primary", PaneID: "w1:p1", TerminalID: "terminal", Generation: 1, AgentSessionID: "session"}, Agent: "pi"}
}
func testEvidence() Evidence {
	return Evidence{Instance: "instance", Incarnation: "bridge", PID: 42, Renderer: "fullscreen", Wheel: "sgr_cell", Grid: panesize.Grid{Rows: 30, Columns: 79}}
}

func TestSelectionAndRevisionBinding(t *testing.T) {
	target, e := testTarget(), testEvidence()
	manager := New(resolverFunc(func(context.Context, Target, bool) (Evidence, error) { return e, nil }))
	if err := manager.Validate(context.Background(), target, true); err != nil {
		t.Fatal(err)
	}
	initial := manager.Snapshot(target, "controller")
	if !initial.Available || initial.Route != "application" || initial.Presentation != "live_screen" {
		t.Fatalf("state: %+v", initial)
	}
	local, err := manager.SelectAtRevision(target, "controller", "local", initial.Revision)
	if err != nil {
		t.Fatal(err)
	}
	if local.Available || local.Route != "local" || local.Revision == initial.Revision {
		t.Fatalf("local: %+v", local)
	}
	if other := manager.Snapshot(target, "reader"); other.Selection != "auto" {
		t.Fatalf("selection leaked: %+v", other)
	}
	manager.ForgetOwner("controller")
	if got := manager.Snapshot(target, "controller"); got.Selection != "auto" {
		t.Fatalf("selection retained: %+v", got)
	}
	for _, change := range []string{"session", "terminal", "generation", "bridge", "instance", "pid"} {
		t.Run(change, func(t *testing.T) {
			before := manager.Snapshot(target, "controller")
			if _, err := manager.SelectAtRevision(target, "controller", "local", before.Revision); err != nil {
				t.Fatal(err)
			}
			switch change {
			case "session":
				target.Ref.AgentSessionID += "x"
			case "terminal":
				target.Ref.TerminalID += "x"
			case "generation":
				target.Ref.Generation++
			case "bridge":
				e.Incarnation += "x"
			case "instance":
				e.Instance += "x"
			case "pid":
				e.PID++
			}
			if err := manager.Validate(context.Background(), target, true); err != nil {
				t.Fatal(err)
			}
			got := manager.Snapshot(target, "controller")
			if got.Selection != "auto" || got.Revision == before.Revision || got.GeometryEpoch <= before.GeometryEpoch {
				t.Fatalf("replacement retained authority: %+v", got)
			}
		})
	}
}
func TestGeometryAndRendererInvalidation(t *testing.T) {
	target, e := testTarget(), testEvidence()
	var resolveErr error
	manager := New(resolverFunc(func(context.Context, Target, bool) (Evidence, error) { return e, resolveErr }))
	manager.Validate(context.Background(), target, true)
	first := manager.Snapshot(target, "owner")
	manager.Validate(context.Background(), target, false)
	if got := manager.Snapshot(target, "owner"); got.Revision != first.Revision {
		t.Fatal("unchanged evidence changed revision")
	}
	manager.InvalidateGeometry(target.Ref.PaneID)
	invalid := manager.Snapshot(target, "owner")
	if invalid.Available || invalid.Revision == first.Revision || invalid.GeometryEpoch <= first.GeometryEpoch {
		t.Fatalf("resize: %+v", invalid)
	}
	e.Grid.Columns = 100
	manager.Validate(context.Background(), target, true)
	if got := manager.Snapshot(target, "owner"); !got.Available || got.Columns != 100 || got.Revision == first.Revision {
		t.Fatalf("new geometry: %+v", got)
	}
	for _, mode := range []string{"regular", "unknown"} {
		e.Renderer = mode
		e.Wheel = "unavailable"
		manager.Validate(context.Background(), target, false)
		if got := manager.Snapshot(target, "owner"); got.Available || got.Route != "local" || got.Presentation != "history" {
			t.Fatalf("mode: %+v", got)
		}
	}
	e = testEvidence()
	manager.Validate(context.Background(), target, true)
	manager.Refuse(target)
	if got := manager.Snapshot(target, "owner"); got.Available || got.Reason != "delivery_unconfirmed" {
		t.Fatalf("uncertain: %+v", got)
	}
	resolveErr = errors.New("bridge gone")
	manager.Validate(context.Background(), target, true)
	if got := manager.Snapshot(target, "owner"); got.Available || got.Source != "unknown" {
		t.Fatalf("failure: %+v", got)
	}
}
func TestAdmissionIsNonblockingAndBounded(t *testing.T) {
	manager := New(nil)
	var releases []func()
	for i := 0; i < 8; i++ {
		release, err := manager.Begin(fmt.Sprint(i))
		if err != nil {
			t.Fatal(err)
		}
		releases = append(releases, release)
	}
	if _, err := manager.Begin("0"); err == nil {
		t.Fatal("same pane admitted")
	}
	if _, err := manager.Begin("ninth"); err == nil {
		t.Fatal("capacity exceeded")
	}
	for _, release := range releases {
		release()
	}
	release, err := manager.Begin("0")
	if err != nil {
		t.Fatal(err)
	}
	release()
}
func TestWheelReportHasOneCenteredReport(t *testing.T) {
	for _, tc := range []struct {
		direction string
		grid      panesize.Grid
		want      string
	}{
		{"up", panesize.Grid{Rows: 30, Columns: 79}, "\x1b[<64;40;15M"},
		{"down", panesize.Grid{Rows: 1, Columns: 1}, "\x1b[<65;1;1M"},
		{"up", panesize.Grid{Rows: 65535, Columns: 65535}, "\x1b[<64;32768;32768M"},
	} {
		got, err := Report(tc.direction, tc.grid)
		if err != nil || got != tc.want {
			t.Fatalf("report: %q %v", got, err)
		}
	}
	for _, grid := range []panesize.Grid{{}, {Rows: -1, Columns: 30}, {Rows: 30, Columns: 65536}} {
		if _, err := Report("up", grid); err == nil {
			t.Fatal("invalid dimensions accepted")
		}
	}
	if _, err := Report("left", testEvidence().Grid); err == nil {
		t.Fatal("invalid direction accepted")
	}
}
