package deviceauth

import (
	"context"
	"sync"
)

// Legacy operations retain ordinary RW exclusion. Only snapshot acquisition
// waits cancellably. Unlock notifications avoid polling and detached workers.
type snapshotMutex struct {
	sync.RWMutex
	notifyMu sync.Mutex
	changed  chan struct{}
}

func (m *snapshotMutex) Unlock() {
	m.notifyMu.Lock()
	m.RWMutex.Unlock()
	m.notifyLocked()
	m.notifyMu.Unlock()
}
func (m *snapshotMutex) RUnlock() {
	m.notifyMu.Lock()
	m.RWMutex.RUnlock()
	m.notifyLocked()
	m.notifyMu.Unlock()
}
func (m *snapshotMutex) notifyLocked() {
	if m.changed != nil {
		close(m.changed)
		m.changed = nil
	}
}
func (m *snapshotMutex) acquire(ctx context.Context, read bool) error {
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		m.notifyMu.Lock()
		if m.changed == nil {
			m.changed = make(chan struct{})
		}
		changed := m.changed
		var ok bool
		if read {
			ok = m.TryRLock()
		} else {
			ok = m.TryLock()
		}
		m.notifyMu.Unlock()
		if ok {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-changed:
		}
	}
}
