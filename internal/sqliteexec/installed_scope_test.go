package sqliteexec

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"

	"github.com/0cv/herdr-mobile-relay/internal/conversationlife"
)

func TestA1InstalledNestedScope(t *testing.T) {
	owner, other := conversationlife.New(), conversationlife.New()
	f := newInstalledFixture(t, owner, other)
	childHold, releaseChild := f.hold()
	parentHold, releaseParent := f.hold()
	entered := newInstalledCapture[context.Context]()
	parentReady := newInstalledCapture[*conversationlife.Call]()
	deps := deniedInstalledDeps()
	rows := &logicalRows{}
	db := &logicalDB{rows: rows}
	db.pingHook = func(ctx context.Context) { entered.notify(ctx); <-childHold }
	var permit atomic.Bool
	var opens, acquisitions atomic.Int32
	deps.open = func(string) (installedDB, error) {
		opens.Add(1)
		if !permit.CompareAndSwap(true, false) {
			return nil, installedDenied
		}
		acquisitions.Add(1)
		return db, nil
	}
	e, err := resolveInstalled(context.Background(), owner, BackendNative, "denied", deps)
	if err != nil {
		t.Fatal(err)
	}
	var parentErr, childErr error
	parentReturned := f.launch(func() {
		parentErr = owner.Do(context.Background(), func(c *conversationlife.Call) error {
			parentReady.notify(c)
			<-parentHold
			return nil
		})
	})
	f.require(t, parentReady.ch, "parent scope entry")
	parent := parentReady.value
	var foreignErr, foreignCallErr error
	foreign := f.launch(func() {
		foreignCallErr = other.Do(context.Background(), func(c *conversationlife.Call) error {
			_, foreignErr = e.QueryJSONIn(c, "/logical/db", "logical", 32)
			return nil
		})
	})
	f.require(t, foreign, "live foreign probe return")
	foreignOpens := opens.Load() // Must be zero BEFORE granting the child permit.
	permit.Store(true)           // only the intended child may acquire the held logical DB
	childReturned := f.launch(func() {
		_, childErr = e.QueryJSONIn(parent, "/logical/db", "logical", 32)
	})
	f.require(t, entered.ch, "SQL child Ping entry")
	receipt := f.fence(t, owner)
	f.require(t, entered.value.Done(), "SQL child cancellation")
	// A live same-owner scope stays authorized after fencing; SQL's cancellation
	// checkpoint must refuse acquisition, NOT classify admission as refused.
	var afterFenceErr error
	afterFence := f.launch(func() {
		_, afterFenceErr = e.QueryJSONIn(parent, "/logical/after-fence", "logical", 32)
	})
	f.require(t, afterFence, "live parent after-fence probe return")
	releaseParent()
	f.require(t, parentReturned, "parent return and revocation")
	var staleErr error
	stale := f.launch(func() {
		_, staleErr = e.QueryJSONIn(parent, "/logical/stale", "logical", 32)
	})
	f.require(t, stale, "stale parent probe return")
	// The parent has actually returned; only the independently held child can
	// keep the real operation-zero receipt open here.
	countHeld := installedHeld(receipt)
	childHeld := installedHeld(childReturned)
	releaseChild()
	f.require(t, childReturned, "SQL child return")
	f.require(t, receipt, "nested operation-zero completion")
	f.finish()
	if foreignCallErr != nil || !errors.Is(foreignErr, conversationlife.ErrRefused) || foreignOpens != 0 {
		t.Error("live foreign admitted/effect", foreignCallErr, foreignErr, foreignOpens)
	}
	if !errors.Is(afterFenceErr, context.Canceled) || errors.Is(afterFenceErr, conversationlife.ErrRefused) {
		t.Error("nested fence semantics", afterFenceErr)
	}
	if !errors.Is(staleErr, conversationlife.ErrRefused) {
		t.Error("revoked parent accepted", staleErr)
	}
	if !countHeld || !childHeld {
		t.Error("actual operation receipt/child completed while resource held")
	}
	if parentReady.count.Load() != 1 || entered.count.Load() != 1 || opens.Load() != 1 || acquisitions.Load() != 1 {
		t.Error("unexpected scope/acquisition counts", parentReady.count.Load(), entered.count.Load(), opens.Load(), acquisitions.Load())
	}
	if parentErr != nil || !errors.Is(childErr, context.Canceled) || db.closes != 1 || rows.closes != 0 {
		t.Error("nested completion", parentErr, childErr, db.closes, rows.closes)
	}
	if owner.Witnesses() != nil || other.Witnesses() != nil {
		t.Error("operation poisoned ledger")
	}
}

func TestA1InstalledHeldLookup(t *testing.T) {
	for _, name := range []string{"factory", "ready"} {
		t.Run(name, func(t *testing.T) {
			owner := conversationlife.New()
			f := newInstalledFixture(t, owner)
			hold, release := f.hold()
			entered := newInstalledCapture[struct{}]()
			deps := deniedInstalledDeps()
			lookup := func(binary string) (string, error) {
				entered.notify(struct{}{})
				<-hold
				return binary, nil
			}
			var e *InstalledExecutor
			var result error
			var ready bool
			if name == "ready" {
				deps.lookup = func(binary string) (string, error) { return binary, nil }
				e, result = resolveInstalled(context.Background(), owner, BackendCLI, "logical", deps)
				if result != nil {
					t.Fatal(result)
				}
				e.deps.lookup = lookup // instance remains unpublished until the goroutine starts
			} else {
				deps.lookup = lookup
			}
			returned := f.launch(func() {
				if name == "factory" {
					e, result = resolveInstalled(context.Background(), owner, BackendCLI, "logical", deps)
				} else {
					ready = e.Ready()
				}
			})
			f.require(t, entered.ch, "held lookup entry")
			receipt := f.fence(t, owner)
			countHeld := installedHeld(receipt)
			lookupHeld := installedHeld(returned)
			release()
			f.require(t, returned, "held lookup return")
			f.require(t, receipt, "lookup operation-zero completion")
			f.finish()
			if !countHeld || !lookupHeld {
				t.Error("actual operation receipt/lookup completed while resource held")
			}
			if entered.count.Load() != 1 {
				t.Error("unexpected lookup entries", entered.count.Load())
			}
			if name == "factory" && (!errors.Is(result, context.Canceled) || e != nil) {
				t.Error("factory published after cancellation", result)
			}
			if name == "ready" && ready {
				t.Error("ready after cancellation")
			}
			if owner.Witnesses() != nil {
				t.Error("lookup poisoned ledger")
			}
		})
	}
}
