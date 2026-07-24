package app

import (
	"context"
	"sync"
)

// lifecycleTasks owns asynchronous work that must not outlive Server.Run.
// Stop closes admission before cancellation so sync.WaitGroup Add and Wait
// cannot race.
type lifecycleTasks struct {
	mu     sync.Mutex
	ctx    context.Context
	cancel context.CancelFunc
	closed bool
	wg     sync.WaitGroup
}

func newLifecycleTasks(parent context.Context) *lifecycleTasks {
	ctx, cancel := context.WithCancel(parent)
	return &lifecycleTasks{ctx: ctx, cancel: cancel}
}

func (g *lifecycleTasks) Start(work func(context.Context)) bool {
	g.mu.Lock()
	if g.closed {
		g.mu.Unlock()
		return false
	}
	g.wg.Add(1)
	ctx := g.ctx
	g.mu.Unlock()

	go func() {
		defer g.wg.Done()
		work(ctx)
	}()
	return true
}

func (g *lifecycleTasks) Stop() {
	g.mu.Lock()
	if !g.closed {
		g.closed = true
		g.cancel()
	}
	g.mu.Unlock()
	g.wg.Wait()
}
