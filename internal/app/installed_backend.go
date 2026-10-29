package app

import (
	"context"
	"net"
	"sync"

	"github.com/0cv/herdr-mobile-relay/internal/tailscalecli"
)

// installedBackend is the sole physical-close owner. Closing cancels admitted
// publication, drains its leases, then closes the resource exactly once. It is
// deliberately private: callers cannot supply or activate service authority.
type installedBackend struct {
	net.Listener
	mu         sync.Mutex
	active     bool
	lifetime   context.Context
	cancel     context.CancelFunc
	operations sync.WaitGroup
	once       sync.Once
	closeErr   error
}

func newInstalledBackend(ctx context.Context, listener net.Listener) *installedBackend {
	lifetime, cancel := context.WithCancel(ctx)
	return &installedBackend{Listener: listener, active: true, lifetime: lifetime, cancel: cancel}
}

func (b *installedBackend) WithLease(ctx context.Context, publish func() error) error {
	if b == nil || ctx == nil || publish == nil {
		return tailscalecli.ErrWorkflowRequired
	}
	b.mu.Lock()
	if !b.active || b.lifetime.Err() != nil || ctx.Err() != nil {
		b.mu.Unlock()
		return tailscalecli.ErrWorkflowRequired
	}
	b.operations.Add(1)
	b.mu.Unlock()
	defer b.operations.Done()
	return publish()
}

func (b *installedBackend) Close() error {
	b.once.Do(func() {
		b.mu.Lock()
		b.active = false
		b.cancel()
		b.mu.Unlock()
		b.operations.Wait()
		b.closeErr = b.Listener.Close()
	})
	return b.closeErr
}
