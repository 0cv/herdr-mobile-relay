package deviceauth

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

type snapshotQueuedContext struct {
	context.Context
	once   sync.Once
	queued chan struct{}
}

func (c *snapshotQueuedContext) Done() <-chan struct{} {
	c.once.Do(func() { close(c.queued) })
	return c.Context.Done()
}

// Done is observed only after the failed try/acquisition, while the writer
// remains held. Every timeout releases the writer and joins before failing.
func TestBootstrapSnapshotHeldWriterCancellation(t *testing.T) {
	for _, target := range []string{"store", "gate"} {
		for _, mode := range []string{"already", "queued"} {
			t.Run(target+"/"+mode, func(t *testing.T) {
				s := snapshotFixture(t)
				g := NewBootstrapGate()
				if err := g.Attach(s); err != nil {
					t.Fatal(err)
				}
				if err := g.Open(); err != nil {
					t.Fatal(err)
				}
				lock := &s.mu
				read := s.CurrentBootstrapInvitation
				if target == "gate" {
					lock = &g.mu
					read = g.CurrentBootstrapInvitation
				}
				lock.Lock()
				held := true
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				queued := &snapshotQueuedContext{Context: ctx, queued: make(chan struct{})}
				done := make(chan error, 1)
				if mode == "already" {
					cancel()
				}
				go func() { _, err := read(queued); done <- err }()
				joined := false
				defer func() {
					if held {
						lock.Unlock()
					}
					if !joined {
						<-done
					}
				}()
				if mode == "queued" {
					select {
					case <-queued.queued:
					case <-time.After(time.Second):
						t.Fatal("snapshot did not queue")
					}
					cancel()
				}
				select {
				case err := <-done:
					joined = true
					if !errors.Is(err, context.Canceled) {
						t.Fatal("cancellation lost")
					}
				case <-time.After(time.Second):
					t.Fatal("cancel waited for writer")
				}
				lock.Unlock()
				held = false
				if _, err := read(context.Background()); err != nil {
					t.Fatal(err)
				}
			})
		}
	}
}
