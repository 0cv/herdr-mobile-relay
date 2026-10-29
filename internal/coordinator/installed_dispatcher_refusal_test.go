package coordinator

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/0cv/herdr-mobile-relay/internal/herdr"
)

func heldInstalledRefusal(t *testing.T, ctx context.Context, topology bool) {
	t.Helper()
	d := dispatcherFixture(t, nil)
	entered, release, returned := make(chan struct{}), make(chan struct{}), make(chan struct{})
	var calls atomic.Int32
	var observers sync.WaitGroup
	ack := func() {
		calls.Add(1)
		began := dispatcherObserve(&observers, func() { _ = d.BeginInstalledDrain() })
		select {
		case <-began:
			close(entered)
		case <-release:
			return
		}
		<-release
	}
	go func() {
		defer close(returned)
		if topology {
			d.HandleTopologyAdmitted(ctx, ack, func(context.Context) *CommandResult { t.Error("refused work invoked"); return nil })
		} else {
			d.HandleAdmitted(ctx, map[string]any{"action": "invalid"}, ack)
		}
	}()
	// Release every holder before joining, including on assertion failure.
	var joined chan struct{}
	defer func() {
		dispatcherStopForCleanup(d)
		close(release)
		<-returned
		observers.Wait()
		if joined != nil {
			<-joined
		}
		_ = d.DrainInstalled()
	}()
	dispatcherAwait(t, entered)
	if d.InstalledDrainComplete() {
		t.Error("drain completed inside refused callback")
	}
	// Entry and Begin return are already observed. This deadline bounds only
	// negative observation while the callback remains held, not admission proof.
	joined = make(chan struct{})
	started := make(chan struct{})
	go func() { close(started); d.installed.calls.Wait(); close(joined) }()
	dispatcherAwait(t, started)
	timer := time.NewTimer(100 * time.Millisecond)
	defer timer.Stop()
	select {
	case <-joined:
		t.Error("callback was not counted")
	case <-d.installed.done:
		t.Error("drain completed while refused callback held")
	case <-timer.C:
	}
	select {
	case <-joined:
		t.Error("callback ownership ended before release")
	default:
	}
	if d.InstalledDrainComplete() {
		t.Error("drain completed before callback release")
	}
	if calls.Load() != 1 {
		t.Error("acknowledgement count")
	}
}

func TestInstalledDispatcherHeldNilRefusalLifetime(t *testing.T) {
	heldInstalledRefusal(t, nil, false)
	heldInstalledRefusal(t, nil, true)
}

func TestInstalledDispatcherHeldCanceledRefusalLifetime(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	heldInstalledRefusal(t, ctx, false)
	heldInstalledRefusal(t, ctx, true)
}

func TestInstalledDispatcherLateCallerOwnedDelivery(t *testing.T) {
	d := dispatcherFixture(t, nil)
	if err := d.DrainInstalled(); err != nil {
		t.Fatal(err)
	}
	for kind := 0; kind < 3; kind++ {
		entered, release, returned := make(chan struct{}), make(chan struct{}), make(chan struct{})
		var calls atomic.Int32
		callback := func() { calls.Add(1); close(entered); <-release }
		func() {
			var observers sync.WaitGroup
			go func() {
				defer close(returned)
				switch kind {
				case 0:
					d.HandleAdmitted(nil, nil, callback)
				case 1:
					d.HandleTopologyAdmitted(nil, callback, func(context.Context) *CommandResult { t.Error("late work"); return nil })
				case 2:
					d.HandleClearActivities("late", func(r *CommandResult) {
						if r.RequestID != "late" || r.Action != "clear_activities" {
							t.Error("clear correlation")
						}
						callback()
					})
				}
			}()
			defer func() { close(release); <-returned; observers.Wait() }()
			dispatcherAwait(t, entered)
			if !d.InstalledDrainComplete() {
				t.Error("late callback reopened ownership")
			}
			var drainErr error
			drained := dispatcherObserve(&observers, func() { drainErr = d.DrainInstalled() })
			dispatcherAwait(t, drained)
			if drainErr != nil {
				t.Error(drainErr)
			}
		}()
		if calls.Load() != 1 {
			t.Error("late callback not exactly once")
		}
	}
}

// Both representative real handlers reach admitTopology with valid input.
// WithDispatchCheck is independently exercised with a LIVE context first:
// runCommand checks it before childenv.Command / cmd.Start (client.go).
// Thus cancellation alone, absent paths and nil clients are not the deny seam.
func TestInstalledDispatcherHeldTopologyCorrelation(t *testing.T) {
	d := dispatcherFixture(t, nil)
	defer d.DrainInstalled()
	d.herdr = herdr.NewClient("unused-denied", "unused-denied")
	d.state.CommitWorkspaces([]herdr.Workspace{{ID: "workspace", Label: "Workspace"}})
	denied := errors.New("fixture denies dispatch before I/O")
	var checks atomic.Int32
	base := herdr.WithDispatchCheck(context.Background(), func() error { checks.Add(1); return denied })
	if err := d.herdr.WorkspaceRename(base, "workspace", "Renamed"); !errors.Is(err, denied) {
		t.Fatal("rename denial boundary", err)
	}
	if _, err := d.herdr.WorktreeOpen(base, "workspace", "", "branch", ""); !errors.Is(err, denied) {
		t.Fatal("worktree denial boundary", err)
	}
	if checks.Load() != 2 {
		t.Fatal("dispatch checks not reached")
	}
	for _, action := range []string{"workspace_rename", "worktree_open"} {
		func() {
			ctx, cancel := context.WithCancel(base)
			entered, returned := make(chan struct{}), make(chan struct{})
			var result *CommandResult
			d.topologyMu.Lock()
			go func() {
				defer close(returned)
				result = d.HandleTopologyAdmitted(ctx, func() { close(entered) }, func(ctx context.Context) *CommandResult {
					if action == "workspace_rename" {
						return d.HandleWorkspaceRename(ctx, "exact-request", "workspace", "Renamed")
					}
					return d.HandleWorktreeOpen(ctx, "exact-request", "workspace", "", "branch", "")
				})
			}()
			defer func() {
				cancel()
				d.topologyMu.Unlock()
				<-returned
				if result == nil || result.RequestID != "exact-request" || result.Action != action || result.Error != context.Canceled.Error() || result.OK {
					t.Errorf("queued correlation: %+v", result)
				}
				if d.activitySequence.Load() != 0 {
					t.Error("refusal recorded activity")
				}
			}()
			dispatcherAwait(t, entered)
			cancel()
			if ctx.Err() != context.Canceled {
				t.Error("cancellation not confirmed with holder retained")
			}
			select {
			case <-returned:
				t.Error("valid request did not wait for topology mutex")
			default:
			}
		}()
	}
}
