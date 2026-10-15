package piscroll

import (
	"context"
	"testing"

	"github.com/0cv/herdr-mobile-relay/internal/herdr"
	"github.com/0cv/herdr-mobile-relay/internal/panesize"
	"github.com/0cv/herdr-mobile-relay/internal/pibridge"
)

func TestCatalogSharesFreshIdentityWithoutWheelCapability(t *testing.T) {
	for _, change := range []string{"unchanged", "target", "instance", "editor"} {
		t.Run(change, func(t *testing.T) {
			target := testTarget()
			target.Ref.AgentSessionID = "/tmp/actual-session.jsonl"
			reads, queries, instances := 0, 0, 0
			changed := false
			r := &PiResolver{
				Supported: func() bool { return false },
				Current:   func(got Target) bool { return got == target && !(changed && change == "target") },
				Instance: func() (string, error) {
					instances++
					if instances > 1 && change == "instance" {
						return "replacement", nil
					}
					return "instance", nil
				},
				Process: func(_ context.Context, pane string) (*herdr.PaneProcessInfo, error) {
					reads++
					info := &herdr.PaneProcessInfo{PaneID: pane, ShellPID: 1, ForegroundProcessGroupID: 42, ForegroundProcesses: []herdr.PaneProcess{{PID: 42}}}
					if reads == 2 && change == "editor" {
						info.ForegroundProcesses = append(info.ForegroundProcesses, herdr.PaneProcess{PID: 43})
					}
					return info, nil
				},
				Catalog: func(_ context.Context, id pibridge.Identity) (pibridge.Response, error) {
					queries++
					changed = true
					if id.Instance != "instance" || id.Pane != target.Ref.PaneID || id.Session != target.Ref.AgentSessionID || id.PID != 42 {
						t.Fatalf("unbound catalog query: %+v", id)
					}
					return pibridge.Response{Incarnation: "bridge"}, nil
				},
				Geometry: func(context.Context, int) (panesize.Grid, error) {
					t.Fatal("catalog measured terminal geometry")
					return panesize.Grid{}, nil
				},
			}
			if _, err := r.Resolve(context.Background(), target, false); err == nil || reads != 0 {
				t.Fatal("wheel display ignored unqualified capability")
			}
			_, err := r.ResolveCatalog(context.Background(), target)
			if (err != nil) != (change != "unchanged") || reads != 2 || queries != 1 {
				t.Fatalf("catalog safety/budget: %v reads=%d queries=%d", err, reads, queries)
			}
		})
	}
}
