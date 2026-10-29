package sqliteexec

import (
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/0cv/herdr-mobile-relay/internal/conversationlife"
)

// Only the test goroutine registers holders/callers. Cleanup seals the list
// before joining: no worker can add work after the fixed list has been joined.
// This mirrors the shared lifetime fixture without exporting lifecycle hooks.
type installedFixture struct {
	owners   []*conversationlife.Owner
	releases []func()
	jobs     []<-chan struct{}
	finished bool
}

func newInstalledFixture(t *testing.T, owners ...*conversationlife.Owner) *installedFixture {
	f := &installedFixture{owners: owners}
	t.Cleanup(f.finish)
	return f
}
func (f *installedFixture) launch(fn func()) <-chan struct{} {
	if f.finished {
		panic("fixture launch after finish")
	}
	done := make(chan struct{})
	f.jobs = append(f.jobs, done)
	go func() { defer close(done); fn() }()
	return done
}
func (f *installedFixture) hold() (<-chan struct{}, func()) {
	ch := make(chan struct{})
	var once sync.Once
	release := func() { once.Do(func() { close(ch) }) }
	f.releases = append(f.releases, release)
	return ch, release
}
func (f *installedFixture) finish() {
	if f.finished {
		return
	}
	// Request every fence WITHOUT waiting, then release ALL holders. Even a
	// gate retained until a callback returns cannot block the sole releaser.
	for _, owner := range f.owners {
		f.launch(func() { owner.Begin() })
	}
	for _, release := range f.releases {
		release()
	}
	for _, owner := range f.owners {
		f.launch(owner.Join)
	}
	f.finished = true
	for _, done := range f.jobs {
		<-done // A timeout never licenses abandonment of actual work.
	}
}
func installedAwait(ch <-chan struct{}) bool {
	timer := time.NewTimer(time.Second)
	defer timer.Stop()
	select {
	case <-ch:
		return true
	case <-timer.C:
		return false
	}
}
func (f *installedFixture) require(t *testing.T, ch <-chan struct{}, label string) {
	t.Helper()
	if !installedAwait(ch) {
		// Failure is fixed BEFORE fallback cancellation can change the result.
		f.finish()
		t.Fatal("missing bounded observation:", label)
	}
}
func (f *installedFixture) fence(t *testing.T, owner *conversationlife.Owner) <-chan struct{} {
	t.Helper()
	var receipt <-chan struct{}
	returned := f.launch(func() { receipt = owner.Begin() })
	f.require(t, returned, "Begin return")
	if receipt == nil {
		f.finish()
		t.Fatal("missing actual operation-zero receipt")
	}
	return receipt
}

// The actual Begin receipt proves accounting, NOT actual Join waiting. The
// latter composes with the shared same-package lifetime boundary fixtures.
func installedHeld(receipt <-chan struct{}) bool {
	timer := time.NewTimer(20 * time.Millisecond)
	defer timer.Stop()
	select {
	case <-receipt:
		return false
	case <-timer.C:
		return true
	}
}

// Capture only the first value; repeated callbacks neither block nor overwrite
// a value read by the test. All invocations remain counted for post-join checks.
type installedCapture[T any] struct {
	once  sync.Once
	ch    chan struct{}
	count atomic.Int32
	value T
}

func newInstalledCapture[T any]() *installedCapture[T] {
	return &installedCapture[T]{ch: make(chan struct{})}
}
func (n *installedCapture[T]) notify(value T) {
	n.count.Add(1)
	n.once.Do(func() { n.value = value; close(n.ch) })
}
