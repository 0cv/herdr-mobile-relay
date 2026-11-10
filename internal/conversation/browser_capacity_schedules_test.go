package conversation

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

type capacityBarrier struct {
	entered chan struct{}
	release chan struct{}
	once    sync.Once
}

func newCapacityBarrier(t *testing.T) *capacityBarrier {
	t.Helper()
	gate := &capacityBarrier{entered: make(chan struct{}, 1), release: make(chan struct{})}
	t.Cleanup(gate.open)
	return gate
}
func (gate *capacityBarrier) open() { gate.once.Do(func() { close(gate.release) }) }
func (gate *capacityBarrier) stop() { gate.entered <- struct{}{}; <-gate.release }
func awaitCapacitySignal(t *testing.T, signal <-chan struct{}) {
	t.Helper()
	select {
	case <-signal:
	case <-time.After(15 * time.Second):
		t.Fatal("capacity schedule did not reach barrier")
	}
}
func awaitCapacityJob(t *testing.T, job *browseJob, state BrowseState) {
	t.Helper()
	deadline := time.NewTimer(15 * time.Second)
	defer deadline.Stop()
	tick := time.NewTicker(time.Millisecond)
	defer tick.Stop()
	for {
		job.mu.RLock()
		done := job.state == state && !job.running && !job.queued
		job.mu.RUnlock()
		if done {
			return
		}
		select {
		case <-deadline.C:
			t.Fatalf("job did not finish as %s", state)
		case <-tick.C:
		}
	}
}

func capacityScheduleBrowser(t *testing.T) (*Browser, string) {
	t.Helper()
	reader, home := testReader(t)
	options := DefaultBrowserOptions()
	options.ActiveWorkers = 2
	options.InterestLease = time.Minute
	options.SnapshotTTL = time.Hour
	b, err := NewBrowser(reader, t.TempDir(), options)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := b.Close(); err != nil {
			t.Error(err)
		}
	})
	return b, home
}
func startCapacitySource(t *testing.T, b *Browser, home string, number int) *browseJob {
	t.Helper()
	session := fmt.Sprintf("123e4567-e89b-12d3-a456-%012d", number)
	path := filepath.Join(home, ".claude", "projects", "-work", session+".jsonl")
	var rows []map[string]any
	for i := 0; i < 32; i++ {
		rows = append(rows, map[string]any{"type": "user", "uuid": fmt.Sprintf("u%d", i), "message": map[string]any{"content": strings.Repeat("x", 16000)}})
	}
	writeRows(t, path, rows...)
	scope := normalizeBrowseScope(BrowseScope{Provider: "claude", CWD: "/work", SessionID: session})
	source, code := b.sourceFor(scope, false)
	if code != "" {
		t.Fatal(code)
	}
	digest, err := fileRangeDigest(t.Context(), source.file, 0, source.end)
	if err != nil {
		source.close()
		t.Fatal(err)
	}
	job, page := b.createJob(scope, source, 0, source.end, source.end, digest, false, "", 0)
	if page != nil || job == nil {
		source.close()
		t.Fatalf("admission: %+v", page)
	}
	return job
}
func capacityRead(t *testing.T, b *Browser, job *browseJob) <-chan BrowsePage {
	t.Helper()
	result := make(chan BrowsePage, 1)
	go func() {
		page, err := b.ReadPage(context.Background(), BrowseRequest{Scope: job.scope, Cursor: job.cursor, Limit: 1})
		if err != nil {
			page.ReasonCode = "go_error"
		}
		result <- page
	}()
	return result
}
func awaitCapacityPage(t *testing.T, result <-chan BrowsePage) BrowsePage {
	t.Helper()
	select {
	case page := <-result:
		return page
	case <-time.After(15 * time.Second):
		t.Fatal("page did not terminate")
		return BrowsePage{}
	}
}
func assertCapacityDisk(t *testing.T, b *Browser) {
	t.Helper()
	b.capacityMu.Lock()
	defer b.unlockCapacity()
	bytes, _, err := b.cacheUsage()
	if err != nil {
		t.Fatal(err)
	}
	if bytes > b.options.AggregateQuota {
		t.Fatalf("physical bytes %d exceed quota %d", bytes, b.options.AggregateQuota)
	}
}

func TestCapacityCompetingGrowthAdmissionAndExpiry(t *testing.T) {
	b, home := capacityScheduleBrowser(t)
	victim := startCapacitySource(t, b, home, 1)
	awaitCapacityJob(t, victim, BrowseReady)
	reader := startCapacitySource(t, b, home, 2)
	awaitCapacityJob(t, reader, BrowseReady)
	size := victim.index.size()
	initial := initialSnapshotBytes(t)
	if size <= 2*initial {
		t.Fatal("fixture must grow beyond two initial allocations")
	}
	readGate := newCapacityBarrier(t)
	firstGate := newCapacityBarrier(t)
	secondGate := newCapacityBarrier(t)
	lockedGate := newCapacityBarrier(t)
	finishGate := newCapacityBarrier(t)
	admissions := make(chan struct{}, 4)
	expiries := make(chan struct{}, 4)
	var removalMu sync.Mutex
	removals := map[string]int{}
	b.mu.Lock()
	b.snapshotReadObserver = func(job *browseJob) {
		if job == reader {
			readGate.stop()
		}
	}
	b.capacityOperationObserver = func(stage string, job *browseJob, locked bool) {
		if stage == "admission" {
			admissions <- struct{}{}
		}
		if stage == "expiry" {
			select {
			case expiries <- struct{}{}:
			default:
			}
		}
		if stage != "batch" {
			return
		}
		if job.scope.SessionID == "123e4567-e89b-12d3-a456-000000000003" {
			if locked {
				lockedGate.stop()
			} else {
				firstGate.stop()
			}
		}
		if job.scope.SessionID == "123e4567-e89b-12d3-a456-000000000004" && !locked {
			secondGate.stop()
		}
	}
	b.workerBeforeFinishObserver = func(job *browseJob) {
		if job.scope.SessionID == "123e4567-e89b-12d3-a456-000000000003" {
			finishGate.stop()
		}
	}
	b.mu.Unlock()
	b.capacityMu.Lock()
	b.options.AggregateQuota = 2*size + 2*initial
	b.capacityRemove = func(index *snapshotIndex) error {
		removalMu.Lock()
		removals[index.path]++
		removalMu.Unlock()
		return index.remove()
	}
	b.unlockCapacity()
	readResult := capacityRead(t, b, reader)
	awaitCapacitySignal(t, readGate.entered)
	first := startCapacitySource(t, b, home, 3)
	awaitCapacitySignal(t, admissions)
	awaitCapacitySignal(t, firstGate.entered)
	second := startCapacitySource(t, b, home, 4)
	awaitCapacitySignal(t, admissions)
	awaitCapacitySignal(t, secondGate.entered)
	victimPath := victim.index.path
	victim.mu.Lock()
	victim.lastAccess = time.Now().Add(-2 * time.Hour)
	victim.mu.Unlock()
	firstGate.open()
	awaitCapacitySignal(t, lockedGate.entered)
	secondGate.open()
	for _, job := range []*browseJob{first, second} {
		job.mu.RLock()
		running := job.running && !job.queued && !job.retired
		job.mu.RUnlock()
		if !running {
			t.Fatal("growth barriers did not hold two live workers")
		}
	}
	admissionDone := make(chan struct{})
	var third *browseJob
	go func() { defer close(admissionDone); third = startCapacitySource(t, b, home, 5) }()
	awaitCapacitySignal(t, admissions)
	expiryDone := make(chan struct{})
	go func() { b.evictExpired(time.Now()); close(expiryDone) }()
	awaitCapacitySignal(t, expiries)
	if b.capacityMu.TryLock() {
		b.capacityMu.Unlock()
		t.Fatal("growth did not hold capacity serialization")
	}
	lockedGate.open()
	awaitCapacitySignal(t, finishGate.entered)
	awaitCapacitySignal(t, admissionDone)
	awaitCapacitySignal(t, expiryDone)
	awaitCapacityJob(t, second, BrowseFailed)
	second.mu.RLock()
	failure := second.err
	index := second.index
	file := second.source.file
	second.mu.RUnlock()
	if failure == nil || failure.Code != "index_capacity_exceeded" || index != nil || file != nil {
		t.Fatalf("protected exhaustion: %v index=%v file=%v", failure, index, file)
	}
	reader.mu.RLock()
	protected := !reader.retired && reader.index != nil && reader.source.file != nil && reader.readers > 0
	reader.mu.RUnlock()
	if !protected {
		t.Fatal("active reader lost ownership")
	}
	removalMu.Lock()
	removed := removals[victimPath]
	removalMu.Unlock()
	if removed != 1 {
		t.Fatalf("victim cleanup count=%d", removed)
	}
	b.mu.Lock()
	detached := b.jobs[victim.id] == nil && b.dedup[victim.dedupKey] == ""
	b.mu.Unlock()
	victim.mu.RLock()
	detached = detached && victim.retired && victim.index == nil && victim.source.file == nil
	victim.mu.RUnlock()
	if !detached {
		t.Fatal("concurrent retirement retained ownership")
	}
	if _, err := os.Stat(victimPath); !os.IsNotExist(err) {
		t.Fatalf("victim file remains: %v", err)
	}
	assertCapacityDisk(t, b)
	readGate.open()
	if page := awaitCapacityPage(t, readResult); page.State != BrowseReady {
		t.Fatalf("protected read: %+v", page)
	}
	finishGate.open()
	awaitCapacityJob(t, first, BrowseReady)
	if third == nil {
		t.Fatal("concurrent admission did not publish a job")
	}
	b.workerWG.Wait()
	assertCapacityDisk(t, b)
}

func TestCapacityAggregateRetryAndShutdown(t *testing.T) {
	for _, closing := range []bool{false, true} {
		t.Run(fmt.Sprint(closing), func(t *testing.T) {
			b, home := capacityScheduleBrowser(t)
			victim := startCapacitySource(t, b, home, 11)
			awaitCapacityJob(t, victim, BrowseReady)
			size := victim.index.size()
			initial := initialSnapshotBytes(t)
			readGate := newCapacityBarrier(t)
			retryGate := newCapacityBarrier(t)
			b.mu.Lock()
			b.snapshotReadObserver = func(job *browseJob) {
				if job == victim {
					readGate.stop()
				}
			}
			b.capacityOperationObserver = func(stage string, _ *browseJob, _ bool) {
				if stage == "retry" {
					retryGate.stop()
				}
			}
			b.mu.Unlock()
			b.capacityMu.Lock()
			b.options.AggregateQuota = size + initial
			b.unlockCapacity()
			readResult := capacityRead(t, b, victim)
			awaitCapacitySignal(t, readGate.entered)
			job := startCapacitySource(t, b, home, 12)
			awaitCapacityJob(t, job, BrowseFailed)
			job.mu.Lock()
			job.autoRetried = true
			oldID := job.snapshotID
			failure := job.err
			job.mu.Unlock()
			if failure == nil || failure.Code != "index_capacity_exceeded" {
				t.Fatalf("failure=%v", failure)
			}
			if _, err := os.Stat(filepath.Join(b.cacheRoot, "snapshot-"+oldID+".db")); !os.IsNotExist(err) {
				t.Fatalf("failed allocation remains: %v", err)
			}
			job.mu.RLock()
			detached := job.index == nil && job.source.file == nil
			job.mu.RUnlock()
			if !detached {
				t.Fatal("failed worker retained ownership")
			}
			assertCapacityDisk(t, b)
			readGate.open()
			if page := awaitCapacityPage(t, readResult); page.State != BrowseReady {
				t.Fatalf("read=%+v", page)
			}
			b.capacityMu.Lock()
			b.options.AggregateQuota = size
			b.unlockCapacity()
			result := make(chan BrowsePage, 1)
			go func() {
				page, _ := b.ReadPage(context.Background(), BrowseRequest{Scope: job.scope, Cursor: job.cursor, Limit: 1, Retry: true})
				result <- page
			}()
			awaitCapacitySignal(t, retryGate.entered)
			if closing {
				closed := make(chan struct{})
				go func() { _ = b.Close(); close(closed) }()
				awaitCapacitySignal(t, b.ctx.Done())
				select {
				case <-closed:
					t.Fatal("Close passed the blocked retry request")
				default:
				}
				retryGate.open()
				awaitCapacityPage(t, result)
				awaitCapacitySignal(t, closed)
				job.mu.RLock()
				unchanged := job.snapshotID == oldID && job.index == nil && job.source.file == nil && !job.queued && !job.running
				job.mu.RUnlock()
				if !unchanged {
					t.Fatal("shutdown retry transferred ownership")
				}
				if bytes, files, err := b.cacheUsage(); err != nil || bytes != 0 || files != 0 {
					t.Fatalf("shutdown bytes=%d files=%d err=%v", bytes, files, err)
				}
			} else {
				retryGate.open()
				if page := awaitCapacityPage(t, result); page.State != BrowsePreparing {
					t.Fatalf("retry=%+v", page)
				}
				awaitCapacityJob(t, job, BrowseReady)
				job.mu.RLock()
				replaced := job.snapshotID != oldID && job.index != nil && job.source.file != nil
				job.mu.RUnlock()
				if !replaced || !victim.retired || victim.index != nil || victim.source.file != nil {
					t.Fatal("retry ownership/reclamation incorrect")
				}
				assertCapacityDisk(t, b)
			}
		})
	}
}

func TestCapacityCloseDuringGrowthAndRead(t *testing.T) {
	b, home := capacityScheduleBrowser(t)
	reader := startCapacitySource(t, b, home, 21)
	awaitCapacityJob(t, reader, BrowseReady)
	readGate := newCapacityBarrier(t)
	growthGate := newCapacityBarrier(t)
	b.mu.Lock()
	b.snapshotReadObserver = func(job *browseJob) {
		if job == reader {
			readGate.stop()
		}
	}
	b.capacityOperationObserver = func(stage string, _ *browseJob, locked bool) {
		if stage == "batch" && locked {
			growthGate.stop()
		}
	}
	b.mu.Unlock()
	b.capacityMu.Lock()
	b.options.AggregateQuota = reader.index.size() + initialSnapshotBytes(t)
	b.unlockCapacity()
	result := capacityRead(t, b, reader)
	awaitCapacitySignal(t, readGate.entered)
	worker := startCapacitySource(t, b, home, 22)
	awaitCapacitySignal(t, growthGate.entered)
	closed := make(chan struct{})
	go func() { _ = b.Close(); close(closed) }()
	awaitCapacitySignal(t, b.ctx.Done())
	select {
	case <-closed:
		t.Fatal("Close passed blocked growth/read")
	default:
	}
	growthGate.open()
	awaitCapacityJob(t, worker, BrowseFailed)
	select {
	case <-closed:
		t.Fatal("Close passed retained read")
	default:
	}
	readGate.open()
	awaitCapacityPage(t, result)
	awaitCapacitySignal(t, closed)
	for _, job := range []*browseJob{reader, worker} {
		job.mu.RLock()
		detached := job.retired && job.index == nil && job.source.file == nil && !job.running
		job.mu.RUnlock()
		if !detached {
			t.Fatal("shutdown left owned resources")
		}
	}
	if bytes, files, err := b.cacheUsage(); err != nil || bytes != 0 || files != 0 {
		t.Fatalf("shutdown bytes=%d files=%d err=%v", bytes, files, err)
	}
}
