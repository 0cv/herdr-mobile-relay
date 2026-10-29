// Package conversationlife supplies installed conversation operation ownership.
// It is stdlib-only to avoid cycles between conversation, session and sqliteexec.
package conversationlife

import (
	"context"
	"errors"
	"sync"
	"time"
)

var ErrRefused = errors.New("conversation lifetime admission refused")

// Owner must be constructed with New and must not be copied. Join is an
// operation barrier, NOT retained-resource finalization or result collection.
type Owner struct {
	mu         sync.Mutex
	ctx        context.Context
	cancel     context.CancelFunc
	fenced     bool
	live       uint64
	done       chan struct{}
	cancelDone chan struct{}
	ledgerMu   sync.Mutex
	witnesses  []error
	overflow   uint64

	// Private returning observations, configured before concurrent use. Never
	// invoked under mu; nil in production.
	joinCancelWait    func()
	joinOperationWait func()
}

func New() *Owner {
	ctx, cancel := context.WithCancel(context.Background())
	return &Owner{ctx: ctx, cancel: cancel, done: make(chan struct{}), cancelDone: make(chan struct{})}
}

// Call is authority only during its callback. Context alone grants no authority.
// Copies still refer to the same private scope, so cannot extend its lifetime.
type Call struct{ scope *scope }
type scope struct {
	owner  *Owner
	active bool
	ctx    context.Context
}

// Cancellation descends directly from the private owner, so Begin synchronously
// signals admitted contexts. Values still come from the explicit caller.
type operationContext struct {
	context.Context
	caller context.Context
}

func (c operationContext) Value(key any) any {
	// Preserve the base's internal cancellation identity; user values come from
	// the caller (the private owner has no user values).
	if value := c.Context.Value(key); value != nil {
		return value
	}
	return c.caller.Value(key)
}
func (c operationContext) Deadline() (time.Time, bool) { return c.caller.Deadline() }
func (c operationContext) Err() error                  { return context.Cause(c.Context) }

func (c *Call) Context() context.Context {
	if c == nil || c.scope == nil {
		return nil
	}
	return c.scope.ctx
}

func (o *Owner) Do(ctx context.Context, fn func(*Call) error) error {
	return o.enter(ctx, nil, false, fn)
}

// DoIn admits an independently counted child of an exact, active same-owner
// scope, even after Begin. The caller context is the parent's merged context.
func (o *Owner) DoIn(parent *Call, fn func(*Call) error) error {
	if parent == nil || parent.scope == nil {
		return ErrRefused
	}
	return o.enter(parent.Context(), parent, true, fn)
}

// DoInContext admits an independently pinned child using ctx's values and the
// earlier caller/parent deadline. Authority comes only from the live parent;
// cancellation also follows that parent, including its callback retirement.
func (o *Owner) DoInContext(ctx context.Context, parent *Call, fn func(*Call) error) error {
	if parent == nil || parent.scope == nil {
		return ErrRefused
	}
	return o.enterContext(ctx, parent, true, parent.Context(), fn)
}

// scopedOperationContext changes only deadline metadata, never value authority
// or the private owner's direct cancellation descent.
type scopedOperationContext struct {
	operationContext
	parent context.Context
}

func (c scopedOperationContext) Deadline() (time.Time, bool) {
	caller, callerOK := c.caller.Deadline()
	parent, parentOK := c.parent.Deadline()
	if parentOK && (!callerOK || parent.Before(caller)) {
		return parent, true
	}
	return caller, callerOK
}

func (o *Owner) enter(ctx context.Context, parent *Call, nested bool, fn func(*Call) error) error {
	return o.enterContext(ctx, parent, nested, nil, fn)
}

func (o *Owner) enterContext(ctx context.Context, parent *Call, nested bool, parentContext context.Context, fn func(*Call) error) error {
	if o == nil || o.ctx == nil || ctx == nil || fn == nil {
		return ErrRefused
	}
	o.mu.Lock()
	valid := !o.fenced
	if nested {
		valid = parent.scope.owner == o && parent.scope.active
	}
	if !valid {
		o.mu.Unlock()
		return ErrRefused
	}
	o.live++
	o.mu.Unlock()

	// Own the pin before any caller context method can exit abnormally.
	var current *scope
	var cancel context.CancelFunc
	var callerStop, ownerStop, parentStop func()
	defer func() {
		// Revoke before joining bridges. Previously entered children keep their pins.
		if current != nil {
			o.mu.Lock()
			current.active = false
			o.mu.Unlock()
		}
		if cancel != nil {
			cancel()
		}
		if callerStop != nil {
			callerStop()
		}
		if ownerStop != nil {
			ownerStop()
		}
		if parentStop != nil {
			parentStop()
		}
		o.mu.Lock()
		o.live--
		if o.fenced && o.live == 0 {
			close(o.done)
		}
		o.mu.Unlock()
	}()
	// No user callback, cancellation or wait runs under the admission lock.
	base, cancelCause := context.WithCancelCause(o.ctx)
	cancel = func() { cancelCause(context.Canceled) }
	merged := operationContext{Context: base, caller: ctx}
	callerStop, _ = bridge(ctx, func() { cancelCause(context.Cause(ctx)) })
	ownerStop, _ = bridge(o.ctx, cancel)
	var operation context.Context = merged
	if parentContext != nil {
		parentStop, _ = bridge(parentContext, func() { cancelCause(context.Cause(parentContext)) })
		operation = scopedOperationContext{operationContext: merged, parent: parentContext}
	}
	current = &scope{owner: o, active: true, ctx: operation}
	return fn(&Call{scope: current})
}

// bridge returns an actual stop-or-join, not merely a cancellation request.
func bridge(ctx context.Context, cancel context.CancelFunc) (func(), <-chan struct{}) {
	return observedBridge(ctx, cancel, nil, nil)
}

// Observations are private, per-call and returning. Registration ownership is
// armed before publication; a bridge that never returns still rolls back.
func observedBridge(ctx context.Context, cancel context.CancelFunc, waiting func(), registered func(func(), <-chan struct{})) (func(), <-chan struct{}) {
	done := make(chan struct{})
	stop := context.AfterFunc(ctx, func() { defer close(done); cancel() })
	var once sync.Once
	stopOrJoin := func() {
		once.Do(func() {
			if stop() {
				close(done)
				return
			}
			if waiting != nil {
				waiting()
			}
			<-done
		})
	}
	transferred := false
	defer func() {
		if !transferred {
			stopOrJoin()
		}
	}()
	if registered != nil {
		registered(stopOrJoin, done)
	}
	// Preserve immediate cancellation at entry, without relying on scheduling.
	if ctx.Err() != nil {
		cancel()
	}
	transferred = true
	return stopOrJoin, done
}

// Begin fences roots then requests cancellation. It never waits for operations.
// Its receipt closes at the actual fenced operation count reaching zero (not at
// a proxy observer starting). It does not certify retained-resource finalization.
func (o *Owner) Begin() <-chan struct{} {
	if o == nil || o.ctx == nil {
		return nil
	}
	o.mu.Lock()
	if o.fenced {
		o.mu.Unlock()
		return o.done
	}
	o.fenced = true
	if o.live == 0 {
		close(o.done)
	}
	o.mu.Unlock()
	o.cancel()
	close(o.cancelDone)
	return o.done
}

// Join returns only after actual callbacks and their cancellation bridges have
// returned. Never call from an operation owned by o. Synchronous I/O may block it.
func (o *Owner) Join() {
	if o == nil || o.ctx == nil {
		return
	}
	o.Begin()
	if o.joinCancelWait != nil {
		o.joinCancelWait()
	}
	<-o.cancelDone
	if o.joinOperationWait != nil {
		o.joinOperationWait()
	}
	<-o.done
}
