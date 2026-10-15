package panesize

import (
	"context"
	"reflect"
	"testing"
	"time"

	"github.com/0cv/herdr-mobile-relay/internal/herdr"
)

func TestResizeObserverCoversLeaseRestorationWithoutHeightWrites(t *testing.T) {
	for _, operation := range []string{"release-grace", "expiry", "disconnect", "shutdown"} {
		t.Run(operation, func(t *testing.T) {
			now := time.Unix(100, 0)
			provider := &fakeProcessInfoProvider{infos: map[string]*herdr.PaneProcessInfo{"pane": processInfo("pane", 321)}}
			runner := &fakeCommandRunner{ttyByPID: map[int]string{321: "pts/7"}, sizes: map[string]terminalSize{"/dev/pts/7": {rows: 37, columns: 132}}}
			manager := testManager(provider, runner, func() time.Time { return now })
			var events []string
			manager.SetOnResize(func(pane string) {
				if !manager.mu.TryLock() {
					t.Fatal("resize callback ran under manager lock")
				}
				manager.mu.Unlock()
				events = append(events, pane)
			})
			ctx := context.Background()
			if _, _, err := manager.Acquire(ctx, "owner", "pane", 80, 0); err != nil {
				t.Fatal(err)
			}
			if _, _, err := manager.Acquire(ctx, "owner", "pane", 80, 0); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(events, []string{"pane"}) {
				t.Fatalf("renewal emitted resize: %v", events)
			}
			var err error
			switch operation {
			case "release-grace":
				err = manager.Release(ctx, "owner", "pane")
				if len(events) != 1 {
					t.Fatal("release grace resized early")
				}
				now = now.Add(ReleaseGrace)
				if err == nil {
					err = manager.SweepExpired(ctx)
				}
			case "expiry":
				now = now.Add(LeaseTTL)
				err = manager.SweepExpired(ctx)
			case "disconnect":
				err = manager.ReleaseClient(ctx, "owner")
			case "shutdown":
				err = manager.Shutdown(ctx)
			}
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(events, []string{"pane", "pane"}) || runner.sizes["/dev/pts/7"].columns != 132 || runner.sizes["/dev/pts/7"].rows != 37 || len(runner.setRowValues) != 0 {
				t.Fatalf("restoration notification/dimensions: %v %+v", events, runner)
			}
		})
	}
}
func TestResizeObserverDoesNotInvalidateUnchangedBaseline(t *testing.T) {
	now := time.Unix(100, 0)
	provider := &fakeProcessInfoProvider{infos: map[string]*herdr.PaneProcessInfo{"pane": processInfo("pane", 321)}}
	runner := &fakeCommandRunner{ttyByPID: map[int]string{321: "pts/7"}, sizes: map[string]terminalSize{"/dev/pts/7": {rows: 37, columns: 132}}}
	manager := testManager(provider, runner, func() time.Time { return now })
	events := 0
	manager.SetOnResize(func(string) { events++ })
	if _, _, err := manager.Acquire(context.Background(), "owner", "pane", 132, 0); err != nil {
		t.Fatal(err)
	}
	if err := manager.ReleaseClient(context.Background(), "owner"); err != nil {
		t.Fatal(err)
	}
	if events != 0 {
		t.Fatalf("unchanged baseline invalidated geometry %d times", events)
	}
}

func TestResizeObserverCoversRemainingOwnerAndExternalHeightChange(t *testing.T) {
	now := time.Unix(100, 0)
	provider := &fakeProcessInfoProvider{infos: map[string]*herdr.PaneProcessInfo{"pane": processInfo("pane", 321)}}
	runner := &fakeCommandRunner{ttyByPID: map[int]string{321: "pts/7"}, sizes: map[string]terminalSize{"/dev/pts/7": {rows: 37, columns: 132}}}
	manager := testManager(provider, runner, func() time.Time { return now })
	events := 0
	manager.SetOnResize(func(string) { events++ })
	ctx := context.Background()
	for owner, width := range map[string]int{"wide": 100, "narrow": 80} {
		if _, _, err := manager.Acquire(ctx, owner, "pane", width, 0); err != nil {
			t.Fatal(err)
		}
	}
	before := events
	if err := manager.ReleaseClient(ctx, "narrow"); err != nil {
		t.Fatal(err)
	}
	if events != before+1 || runner.sizes["/dev/pts/7"].columns != 100 {
		t.Fatal("remaining owner resize was not observed")
	}
	runner.sizes["/dev/pts/7"] = terminalSize{rows: 42, columns: 100}
	before = events
	if _, _, err := manager.Acquire(ctx, "wide", "pane", 100, 0); err != nil {
		t.Fatal(err)
	}
	if events != before+1 || len(runner.setRowValues) != 0 {
		t.Fatal("external height change was not observed, or height was leased")
	}
}
