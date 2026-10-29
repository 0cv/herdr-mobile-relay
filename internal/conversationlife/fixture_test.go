package conversationlife

import (
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// All launches/holder registrations occur on the test goroutine. finish adds
// its fence callers before releasing holders and joining the fixed caller list.
// Fence callers never stand between cleanup and independent holder release.
type a1Fixture struct {
	owners   []*Owner
	releases []func()
	bridges  []func()
	jobs     []<-chan struct{}
	once     sync.Once
}

func a1NewFixture(t *testing.T, owners ...*Owner) *a1Fixture {
	f := &a1Fixture{owners: owners}
	t.Cleanup(f.finish)
	return f
}
func (f *a1Fixture) launch(fn func()) <-chan struct{} {
	done := make(chan struct{})
	f.jobs = append(f.jobs, done)
	go func() { defer close(done); fn() }()
	return done
}
func (f *a1Fixture) hold() (<-chan struct{}, func()) {
	ch := make(chan struct{})
	var once sync.Once
	release := func() { once.Do(func() { close(ch) }) }
	f.releases = append(f.releases, release)
	return ch, release
}
func (f *a1Fixture) finish() {
	f.once.Do(func() {
		for _, o := range f.owners {
			f.launch(func() { o.Begin() })
		}
		for _, release := range f.releases {
			release()
		}
		for _, done := range f.jobs {
			<-done
		}
		for _, stop := range f.bridges {
			stop()
		}
		for _, o := range f.owners {
			o.Join()
		}
	})
}
func a1Await(ch <-chan struct{}) bool {
	timer := time.NewTimer(time.Second)
	defer timer.Stop()
	select {
	case <-ch:
		return true
	case <-timer.C:
		return false
	}
}
func (f *a1Fixture) require(t *testing.T, ch <-chan struct{}, label string) {
	t.Helper()
	if !a1Await(ch) {
		f.finish()
		t.Fatal("missing bounded observation:", label)
	}
}

// Repeated notifications cannot block/panic; duplicates remain observable.
type a1Notice struct {
	ch    chan struct{}
	count atomic.Int32
}

func a1NewNotice() *a1Notice { return &a1Notice{ch: make(chan struct{}, 1)} }
func (n *a1Notice) notify() {
	n.count.Add(1)
	select {
	case n.ch <- struct{}{}:
	default:
	}
}
