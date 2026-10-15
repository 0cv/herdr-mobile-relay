package panesize

import (
	"context"
	"reflect"
	"testing"
)

func TestResizeObserverCoversLeaseRestoration(t *testing.T) {
	for _, operation := range []string{"release-grace", "expiry", "disconnect", "shutdown"} {
		t.Run(operation, func(t *testing.T) {
			f := newFixture(t)
			device := f.addPane("pane", 321, "/dev/pts/7", 7)
			var events []string
			f.m.SetOnResize(func(pane string) {
				if !f.m.mu.TryLock() {
					t.Fatal("resize callback ran under manager lock")
				}
				f.m.mu.Unlock()
				events = append(events, pane)
			})
			f.acquire("owner", "pane", 80, 22)
			f.acquire("owner", "pane", 80, 22)
			if !reflect.DeepEqual(events, []string{"pane"}) {
				t.Fatalf("renewal emitted resize: %v", events)
			}
			ctx := context.Background()
			var err error
			switch operation {
			case "release-grace":
				err = f.m.Release(ctx, "owner", "pane", false)
				if len(events) != 1 {
					t.Fatal("release grace resized early")
				}
				f.clock.Advance(ReleaseGrace)
				f.m.SweepExpired(ctx)
			case "expiry":
				f.clock.Advance(LeaseTTL)
				f.m.SweepExpired(ctx)
			case "disconnect":
				err = f.m.ReleaseClient(ctx, "owner")
			case "shutdown":
				err = f.m.Shutdown(ctx)
			}
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(events, []string{"pane", "pane"}) || f.size(device).Columns != 120 || f.size(device).Rows != 40 {
				t.Fatalf("restoration notification/dimensions: %v %+v", events, f.size(device))
			}
		})
	}
}

func TestResizeObserverDoesNotInvalidateUnchangedBaseline(t *testing.T) {
	f := newFixture(t)
	f.addPane("pane", 321, "/dev/pts/7", 7)
	events := 0
	f.m.SetOnResize(func(string) { events++ })
	f.acquire("owner", "pane", 120, 0)
	if err := f.m.ReleaseClient(context.Background(), "owner"); err != nil {
		t.Fatal(err)
	}
	if events != 0 {
		t.Fatalf("unchanged baseline invalidated geometry %d times", events)
	}
}

func TestResizeObserverCoversRemainingOwnerAndExternalHeightChange(t *testing.T) {
	f := newFixture(t)
	device := f.addPane("pane", 321, "/dev/pts/7", 7)
	events := 0
	f.m.SetOnResize(func(string) { events++ })
	f.acquire("wide", "pane", 100, 0)
	f.acquire("narrow", "pane", 80, 0)
	before := events
	if err := f.m.ReleaseClient(context.Background(), "narrow"); err != nil {
		t.Fatal(err)
	}
	if events != before+1 || f.size(device).Columns != 100 {
		t.Fatal("remaining owner resize was not observed")
	}
	f.setSize(device, 100, 42)
	before = events
	f.acquire("wide", "pane", 100, 0)
	if events != before+1 || f.size(device).Rows != 42 {
		t.Fatal("external height change was not observed, or height was leased")
	}
}
