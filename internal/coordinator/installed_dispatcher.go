package coordinator

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"

	"github.com/0cv/herdr-mobile-relay/internal/activity"
	"github.com/0cv/herdr-mobile-relay/internal/herdr"
)

var ErrNotInstalledDispatcher = errors.New("dispatcher has no installed ownership")
var ErrInstalledDispatcherClosed = errors.New("installed dispatcher is closed")
var ErrInstalledDispatcherContext = errors.New("installed dispatcher requires a context")

type installedDispatcher struct {
	mu        sync.Mutex
	closing   bool
	lifetime  context.Context
	cancel    context.CancelFunc
	calls     sync.WaitGroup
	done      chan struct{}
	result    error
	witnesses []error
	overflow  uint64
}

// InstalledDispatcherErrorOverflow preserves failure after the bounded witness
// budget is exhausted; request validation errors are never retained here.
type InstalledDispatcherErrorOverflow struct{ Count uint64 }

func (e *InstalledDispatcherErrorOverflow) Error() string {
	return fmt.Sprintf("installed dispatcher error witness overflow: %d", e.Count)
}

func (d *Dispatcher) retainInstalledCallbackError(err error) {
	if d.installed == nil || err == nil {
		return
	}
	s := d.installed
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.witnesses) < 16 {
		s.witnesses = append(s.witnesses, err)
	} else if s.overflow != ^uint64(0) {
		s.overflow++
	}
}

type dispatcherScopeKey struct{}
type dispatcherScope struct {
	owner  *installedDispatcher
	active bool // guarded by owner.mu; authority ends when this invocation returns
}

// NewInstalledDispatcher selects ownership before starting either worker. It
// reuses the supplied state, client and journal; it does not own journal closure.
func NewInstalledDispatcher(client *herdr.Client, state *State, journal *activity.Journal, logger *slog.Logger) *Dispatcher {
	lifetime, cancel := context.WithCancel(context.Background())
	return newDispatcher(client, state, journal, logger, &installedDispatcher{lifetime: lifetime, cancel: cancel, done: make(chan struct{})})
}

// enterInstalledCallback owns only invocation delivery, never work permission.
// The same fence as work admission prevents Add racing the supervisor's Wait.
// Losing the fence permits only synchronous pure refusal under caller/ingress
// ownership. Drain cannot cover future external calls; full A3 must join ingress
// callers before releasing the manager owner.
func (d *Dispatcher) enterInstalledCallback() func() {
	if d.installed == nil {
		return func() {}
	}
	s := d.installed
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closing {
		return func() {}
	}
	s.calls.Add(1)
	return s.calls.Done
}

// enterInstalled counts each invocation, including borrowed nested calls. A
// retained or foreign scope is never authority. No callbacks run under mu.
func (d *Dispatcher) enterInstalled(ctx context.Context) (context.Context, func(), error) {
	if d.installed == nil {
		return ctx, func() {}, nil
	}
	if ctx == nil {
		return ctx, nil, ErrInstalledDispatcherContext
	}
	if err := ctx.Err(); err != nil {
		return ctx, nil, err
	}
	s := d.installed
	parent, marked := ctx.Value(dispatcherScopeKey{}).(*dispatcherScope)
	s.mu.Lock()
	if marked && (parent.owner != s || !parent.active) {
		s.mu.Unlock()
		return ctx, nil, ErrInstalledDispatcherClosed
	}
	if s.closing {
		s.mu.Unlock()
		return ctx, nil, ErrInstalledDispatcherClosed
	}
	scope := &dispatcherScope{owner: s, active: true}
	s.calls.Add(1)
	s.mu.Unlock()
	callCtx, cancel := context.WithCancel(ctx)
	joined := make(chan struct{})
	stop := context.AfterFunc(s.lifetime, func() { cancel(); close(joined) })
	release := func() {
		cancel()
		if !stop() {
			<-joined
		}
		s.mu.Lock()
		scope.active = false
		s.mu.Unlock()
		s.calls.Done()
	}
	if s.lifetime.Err() != nil {
		cancel()
	}
	if err := callCtx.Err(); err != nil {
		release()
		return callCtx, nil, err
	}
	callCtx = herdr.WithDispatchCheck(callCtx, callCtx.Err)
	return context.WithValue(callCtx, dispatcherScopeKey{}, scope), release, nil
}

// BeginInstalledDrain is reentrant, including from an admitted callback. The
// sole supervisor performs resource locking and joins outside admission mu.
func (d *Dispatcher) BeginInstalledDrain() error {
	if d == nil || d.installed == nil {
		return ErrNotInstalledDispatcher
	}
	s := d.installed
	s.mu.Lock()
	first := !s.closing
	s.closing = true
	s.mu.Unlock()
	s.cancel()
	// This stop is prompt and does not wait for scheduler callbacks/effects.
	_ = d.scheduler.beginInstalledDrain()
	if first {
		go d.finishInstalledDispatcher()
	}
	return nil
}
func (d *Dispatcher) finishInstalledDispatcher() {
	s := d.installed
	// Stop scheduling before waiting for callers or watchers. Activity remains
	// usable by private finalizers until every effect, caller and watcher joins.
	schedulerErr := d.scheduler.beginInstalledDrain()
	d.stopWatchers()
	schedulerErr = errors.Join(schedulerErr, d.scheduler.drainInstalled())
	s.calls.Wait()
	d.watcherWG.Wait()
	var activityErr error
	if d.activityW != nil {
		activityErr = d.activityW.DrainInstalled()
	}
	// Every producer of these bounded callback witnesses has now joined.
	if s.overflow != 0 {
		s.witnesses = append(s.witnesses, &InstalledDispatcherErrorOverflow{Count: s.overflow})
	}
	s.result = errors.Join(schedulerErr, activityErr, errors.Join(s.witnesses...))
	close(s.done)
}

// InstalledDrainComplete is diagnostic data only: it grants no authority and
// makes no qualification claim. True means the supervisor joined all owned
// resources and published its cached result (which may be an error).
func (d *Dispatcher) InstalledDrainComplete() bool {
	if d == nil || d.installed == nil {
		return false
	}
	select {
	case <-d.installed.done:
		return true
	default:
		return false
	}
}

func (d *Dispatcher) DrainInstalled() error {
	if err := d.BeginInstalledDrain(); err != nil {
		return err
	}
	<-d.installed.done
	return d.installed.result
}
func (d *Dispatcher) closeInstalledDispatcher(ctx context.Context) error {
	if ctx == nil {
		return ErrInstalledDispatcherContext
	}
	if err := d.BeginInstalledDrain(); err != nil {
		return err
	}
	select {
	case <-d.installed.done:
		return d.installed.result
	case <-ctx.Done():
		select {
		case <-d.installed.done:
			return d.installed.result
		default:
			return ctx.Err()
		}
	}
}
func (d *Dispatcher) stopWatchers() {
	d.watcherMu.Lock()
	d.watcherClosed = true
	cancel := d.watcherCancel
	d.watcherMu.Unlock()
	cancel()
}

func installedMessageRefusal(message map[string]any, err error) *CommandResult {
	result := installedDispatchRefusal(err)
	result.RequestID = stringValue(message, "request_id")
	result.Action = stringValue(message, "action")
	if result.Action == "" {
		result.Action = stringValue(message, "type")
	}
	result.PaneID = stringValue(message, "pane_id")
	return result
}

func installedTopologyRefusal(requestID, action string, err error) *CommandResult {
	result := installedDispatchRefusal(err)
	result.RequestID, result.Action = requestID, action
	return result
}

func installedDispatchRefusal(err error) *CommandResult {
	return &CommandResult{Phase: "failed", Error: err.Error()}
}
