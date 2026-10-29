package upload

import (
	"sync"
	"sync/atomic"
)

// Only the owning test goroutine starts callers, including the once-only cleanup
// fence, before Wait. Callbacks never Add, wait, assert, or hold observer locks.
type fixtureReceipt struct {
	count atomic.Int32
	once  sync.Once
	done  chan struct{}
}

func (r *fixtureReceipt) record() {
	r.count.Add(1)
	r.once.Do(func() { close(r.done) })
}

func fixtureFence(m *Manager, wg *sync.WaitGroup) (func(), <-chan struct{}) {
	var once sync.Once
	done := make(chan struct{})
	request := func() {
		once.Do(func() {
			wg.Add(1)
			go func() { defer wg.Done(); defer close(done); _ = m.BeginInstalledDrain() }()
		})
	}
	return request, done
}
