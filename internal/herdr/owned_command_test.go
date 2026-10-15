package herdr

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/0cv/herdr-mobile-relay/internal/ownedproc"
)

type lifecycleInspector struct {
	mu      sync.Mutex
	rows    map[int]ownedproc.Process
	signals []int
}

func (i *lifecycleInspector) Lookup(ctx context.Context, pid int) (ownedproc.Process, error) {
	i.mu.Lock()
	defer i.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return ownedproc.Process{}, err
	}
	p, ok := i.rows[pid]
	if !ok {
		return p, ownedproc.ErrAbsent
	}
	return p, nil
}
func (i *lifecycleInspector) Table(ctx context.Context) ([]ownedproc.Process, error) {
	i.mu.Lock()
	defer i.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	var rows []ownedproc.Process
	for _, p := range i.rows {
		rows = append(rows, p)
	}
	return rows, nil
}
func (i *lifecycleInspector) Signal(pid int, force bool) error {
	i.mu.Lock()
	defer i.mu.Unlock()
	if pid <= 0 {
		panic("group fallback")
	}
	i.signals = append(i.signals, pid)
	delete(i.rows, pid)
	return nil
}
func lifecycleRegistry(t *testing.T) (*ownedproc.Registry, *lifecycleInspector) {
	t.Helper()
	i := &lifecycleInspector{rows: map[int]ownedproc.Process{1: {PID: 1, Birth: "parent"}, 2: {PID: 2, Parent: 1, Birth: "root"}}}
	r, err := ownedproc.New(context.Background(), i, 2, 1)
	if err != nil {
		t.Fatal(err)
	}
	return r, i
}
func TestOwnedCommandConcurrentCompletion(t *testing.T) {
	for range 20 {
		r, i := lifecycleRegistry(t)
		ctx, cancel := context.WithCancel(context.Background())
		wait := make(chan error, 1)
		i.mu.Lock()
		delete(i.rows, 2)
		i.mu.Unlock()
		wait <- nil
		cancel()
		_, _, cleanup := waitOwnedCommand(ctx, r, wait)
		if cleanup != nil {
			t.Fatal(cleanup)
		}
		if len(i.signals) != 0 {
			t.Fatalf("completed incarnation signalled: %v", i.signals)
		}
	}
}
func TestOwnedCommandCancellationIncompleteJoin(t *testing.T) {
	r, i := lifecycleRegistry(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	start := time.Now()
	_, cancelled, err := waitOwnedCommand(ctx, r, make(chan error))
	if !cancelled || !errors.Is(err, ownedproc.ErrIncomplete) {
		t.Fatalf("%v %v", cancelled, err)
	}
	if time.Since(start) > 5*time.Second {
		t.Fatal("join not bounded")
	}
	if len(i.signals) != 1 || i.signals[0] != 2 {
		t.Fatal(i.signals)
	}
}
func TestLimitedBufferFreezeConcurrentWriters(t *testing.T) {
	b := &limitedBuffer{limit: 32}
	var wg sync.WaitGroup
	for range 4 {
		wg.Go(func() {
			for range 10000 {
				_, _ = b.Write([]byte("abcdef"))
			}
		})
	}
	b.freeze()
	before := b.String()
	for range 10 {
		if b.String() != before {
			t.Fatal("diagnostic changed after freeze")
		}
	}
	wg.Wait()
	if len(b.Bytes()) > 32 {
		t.Fatal("output limit")
	}
	if b.String() != before {
		t.Fatal("late writes changed snapshot")
	}
}
