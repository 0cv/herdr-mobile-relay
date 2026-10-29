package app

import (
	"context"
	"errors"
	"sync"
)

type installedDispatcherDrainer interface {
	BeginInstalledDrain() error
	DrainInstalled() error
}

// installedDispatcherCleanup owns the Run acquisition, not the transport mode.
// Begin is prompt; drain has no abandonment timeout. sync.Once publishes the
// same joined result to all callers. Run collects it only at deferred return.
type installedDispatcherCleanup struct {
	cancel     context.CancelFunc
	dispatcher installedDispatcherDrainer
	beginOnce  sync.Once
	drainOnce  sync.Once
	beginErr   error
	result     error
}

func newInstalledDispatcherCleanup(cancel context.CancelFunc, dispatcher installedDispatcherDrainer) *installedDispatcherCleanup {
	return &installedDispatcherCleanup{cancel: cancel, dispatcher: dispatcher}
}

func (o *installedDispatcherCleanup) begin() {
	o.beginOnce.Do(func() {
		o.cancel()
		o.beginErr = o.dispatcher.BeginInstalledDrain()
	})
}

func (o *installedDispatcherCleanup) drain() error {
	o.begin()
	o.drainOnce.Do(func() {
		o.result = errors.Join(o.beginErr, o.dispatcher.DrainInstalled())
	})
	return o.result
}
