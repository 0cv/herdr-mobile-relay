package conversation

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func waitForJobReady(t *testing.T, job *browseJob) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	for {
		job.mu.RLock()
		state, running, failure := job.state, job.running, job.err
		job.mu.RUnlock()
		if state == BrowseFailed {
			t.Fatalf("preparation failed: %+v", failure)
		}
		if state == BrowseReady && !running {
			return
		}
		select {
		case <-ctx.Done():
			t.Fatal("preparation did not finish")
		case <-time.After(time.Millisecond):
		}
	}
}

func TestClaudeChainWorkerHooksFireAtTheirSteps(t *testing.T) {
	b, scope, _ := claudeScheduleFixture(t, 2)
	if latest := scheduleLatest(t, b, scope); latest.ReasonCode != "" {
		t.Fatalf("latest = %#v", latest)
	}
	chain := scheduleChain(t, b, scope)

	var mu sync.Mutex
	var order []string
	var evidenceRanges, rangeReads, batches int
	var readsAtPreparation, batchesAtPreparation int
	var scannedAtPreparation int64
	var phaseAtStart string
	var runningAtStart bool
	setBrowserObserver(b, "source_read", func(browserObservation) {
		mu.Lock()
		rangeReads++
		mu.Unlock()
	})
	setBrowserObserver(b, "capacity_operation", func(observation browserObservation) {
		if observation.stage == "batch" && !observation.locked {
			mu.Lock()
			batches++
			mu.Unlock()
		}
	})
	setBrowserObserver(b, "chain_worker_before_capture", func(browserObservation) {
		job := soleJob(b)
		if job == nil {
			t.Error("no job at the first worker hook")
			return
		}
		job.mu.RLock()
		phase, running := job.phase, job.running
		job.mu.RUnlock()
		mu.Lock()
		order = append(order, "before_capture")
		phaseAtStart, runningAtStart = phase, running
		mu.Unlock()
	})
	setBrowserObserver(b, "chain_worker_capture", func(observation browserObservation) {
		mu.Lock()
		order = append(order, "capture")
		evidenceRanges = len(observation.evidence)
		mu.Unlock()
	})
	setBrowserObserver(b, "chain_preparation", func(browserObservation) {
		job := soleJob(b)
		if job == nil {
			t.Error("no job at the preparation hook")
			return
		}
		job.mu.RLock()
		scanned := job.scannedBytes
		job.mu.RUnlock()
		mu.Lock()
		order = append(order, "preparation")
		readsAtPreparation, batchesAtPreparation, scannedAtPreparation = rangeReads, batches, scanned
		mu.Unlock()
	})

	job, page := b.startClaudeChainJob(scope, chain, 1, newClaudeChainRequestBudget(b.options.RecentBytes).validation)
	if job == nil || page != nil {
		t.Fatalf("job = %v, page = %v", job, page)
	}
	waitForJobReady(t, job)

	mu.Lock()
	defer mu.Unlock()
	if phaseAtStart != "queued" || !runningAtStart {
		t.Errorf("first worker hook saw phase %q, running %v; want a started worker that has not begun scanning", phaseAtStart, runningAtStart)
	}
	if evidenceRanges == 0 {
		t.Fatal("fixture admitted no evidence ranges to validate")
	}
	if want := "before_capture|capture|preparation"; strings.Join(order, "|") != want {
		t.Errorf("hooks fired as %q, want %q", order, want)
	}
	if readsAtPreparation == 0 {
		t.Error("preparation hook fired before the evidence ranges were validated")
	}
	if batchesAtPreparation != 0 || scannedAtPreparation != 0 {
		t.Errorf("preparation hook fired after the scan started: %d batches stored, %d bytes scanned", batchesAtPreparation, scannedAtPreparation)
	}
}

func TestClaudeChainCaptureHookFiresBeforeSourceChecks(t *testing.T) {
	cases := []struct {
		name      string
		spoil     func(t *testing.T, paths []string, job *browseJob)
		code      string
		retryable bool
	}{
		{
			name: "source rewritten before the worker starts",
			spoil: func(t *testing.T, paths []string, job *browseJob) {
				current, err := os.ReadFile(paths[1])
				if err != nil {
					t.Error(err)
					return
				}
				rewritten := strings.Replace(string(current), "original-50", "rewriten-50", 1)
				if err := os.WriteFile(paths[1], []byte(rewritten), 0o600); err != nil {
					t.Error(err)
				}
			},
			code: "source_changed",
		},
		{
			name: "source handle released before the worker starts",
			spoil: func(t *testing.T, paths []string, job *browseJob) {
				job.mu.Lock()
				file := job.source.file
				job.source.file = nil
				job.mu.Unlock()
				_ = file.Close()
			},
			code: "source_unavailable", retryable: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			b, scope, paths := claudeScheduleFixture(t, 2)
			if latest := scheduleLatest(t, b, scope); latest.ReasonCode != "" {
				t.Fatalf("latest = %#v", latest)
			}
			chain := scheduleChain(t, b, scope)
			spoiled := make(chan struct{})
			var captures int
			var mu sync.Mutex
			setBrowserObserver(b, "chain_queue_admission", func(observation browserObservation) {
				tc.spoil(t, paths, observation.job)
				close(spoiled)
			})
			setBrowserObserver(b, "chain_worker_before_capture", func(browserObservation) { <-spoiled })
			setBrowserObserver(b, "chain_worker_capture", func(browserObservation) {
				mu.Lock()
				captures++
				mu.Unlock()
			})
			job, page := b.startClaudeChainJob(scope, chain, 1, newClaudeChainRequestBudget(b.options.RecentBytes).validation)
			if job == nil || page != nil {
				t.Fatalf("job = %v, page = %v", job, page)
			}
			failure := waitForJobFailure(t, job)
			if failure == nil || failure.Code != tc.code || failure.Retryable != tc.retryable {
				t.Fatalf("failure = %+v, want %s (retryable %v)", failure, tc.code, tc.retryable)
			}
			mu.Lock()
			defer mu.Unlock()
			if captures != 1 {
				t.Fatalf("capture hook fired %d times, want once before the source checks", captures)
			}
		})
	}
}

func TestChainDigestMismatchFailsBeforeVerificationRead(t *testing.T) {
	reader, home := testReader(t)
	path := filepath.Join(home, ".claude", "projects", "-work", testSessionID+".jsonl")
	writeRows(t, path,
		map[string]any{"type": "user", "uuid": "u1", "message": map[string]any{"content": "first"}},
		map[string]any{"type": "assistant", "uuid": "a1", "message": map[string]any{"content": "second"}},
	)
	b := newTestBrowser(t, reader, DefaultBrowserOptions())
	scope := normalizeBrowseScope(BrowseScope{Provider: "claude", CWD: "/work", SessionID: testSessionID})
	source, code := b.sourceFor(scope, false)
	if code != "" {
		t.Fatal(code)
	}
	var mu sync.Mutex
	reads := 0
	setBrowserObserver(b, "source_read", func(browserObservation) {
		mu.Lock()
		reads++
		mu.Unlock()
	})
	job, page := b.createJob(scope, source, 0, source.end, source.end, "not-the-captured-digest", "chain", 0, claudeChainJobBinding{})
	if job == nil || page != nil {
		source.close()
		t.Fatalf("job = %v, page = %v", job, page)
	}
	failure := waitForJobFailure(t, job)
	if failure == nil || failure.Code != "source_changed" || failure.Retryable {
		t.Fatalf("failure = %+v, want a source change", failure)
	}
	mu.Lock()
	defer mu.Unlock()
	if reads != 0 {
		t.Fatalf("the verification read was reported %d times for a scan that already disagreed with the admitted digest", reads)
	}
}

func TestBrowserVerificationReadHookFiresBeforeSecondDigest(t *testing.T) {
	reader, home := testReader(t)
	path := filepath.Join(home, ".claude", "projects", "-work", testSessionID+".jsonl")
	writeRows(t, path, jobOutcomeRows()...)
	options := DefaultBrowserOptions()
	options.RecentBytes = 1
	b := newTestBrowser(t, reader, options)
	var mu sync.Mutex
	var background, atHook int64
	hooks := 0
	setBrowserObserver(b, "background_read", func(observation browserObservation) {
		mu.Lock()
		background += observation.bytes
		mu.Unlock()
	})
	setBrowserObserver(b, "source_read", func(observation browserObservation) {
		mu.Lock()
		atHook = background
		hooks++
		mu.Unlock()
	})
	scope := BrowseScope{Provider: "claude", CWD: "/work", SessionID: testSessionID}
	latest, err := b.ReadPage(t.Context(), BrowseRequest{Scope: scope, Limit: 1})
	if err != nil || latest.NextCursor == "" {
		t.Fatalf("latest page = %#v, err = %v", latest, err)
	}
	if _, err := b.ReadPage(t.Context(), BrowseRequest{Scope: scope, Cursor: latest.NextCursor, Limit: 1}); err != nil {
		t.Fatal(err)
	}
	job := soleJob(b)
	if job == nil {
		t.Fatal("no preparation job was admitted")
	}
	waitForJobReady(t, job)
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	if hooks != 1 || atHook != info.Size() {
		t.Fatalf("source_read fired %d times after %d background bytes; want once after the scan's %d bytes and before the second digest", hooks, atHook, info.Size())
	}
}

func TestBrowserBackgroundReadReportsBothKinds(t *testing.T) {
	reader, _ := testReader(t)
	b := newTestBrowser(t, reader, DefaultBrowserOptions())
	var kinds []string
	setBrowserObserver(b, "background_read", func(browserObservation) { kinds = append(kinds, "background_read") })
	setBrowserObserver(b, "validation_read", func(browserObservation) { kinds = append(kinds, "validation_read") })
	if err := b.observeBackgroundRead(10); err != nil {
		t.Fatal(err)
	}
	if strings.Join(kinds, ",") != "background_read,validation_read" {
		t.Fatalf("a background read was reported as %v", kinds)
	}
}

func TestBrowserSnapshotReadHookFiresBeforeInterestIsRenewed(t *testing.T) {
	reader, home := testReader(t)
	path := filepath.Join(home, ".claude", "projects", "-work", testSessionID+".jsonl")
	writeRows(t, path,
		map[string]any{"type": "user", "uuid": "u1", "message": map[string]any{"content": "first"}},
		map[string]any{"type": "assistant", "uuid": "a1", "message": map[string]any{"content": "second"}},
	)
	options := DefaultBrowserOptions()
	options.RecentBytes = 1
	b := newTestBrowser(t, reader, options)
	scope := normalizeBrowseScope(BrowseScope{Provider: "claude", CWD: "/work", SessionID: testSessionID})
	latest, err := b.ReadPage(t.Context(), BrowseRequest{Scope: scope, Limit: 1})
	if err != nil || latest.NextCursor == "" {
		t.Fatalf("latest page = %#v, err = %v", latest, err)
	}
	if _, err := b.ReadPage(t.Context(), BrowseRequest{Scope: scope, Cursor: latest.NextCursor, Limit: 1}); err != nil {
		t.Fatal(err)
	}
	job := soleJob(b)
	if job == nil {
		t.Fatal("no preparation job was admitted")
	}
	waitForJobReady(t, job)

	stale := time.Now().Add(-time.Hour)
	job.mu.Lock()
	job.lastInterest, job.lastAccess = stale, stale
	boundary := job.boundary
	job.mu.Unlock()
	var seenInterest, seenAccess time.Time
	setBrowserObserver(b, "snapshot_read", func(observation browserObservation) {
		observation.job.mu.RLock()
		seenInterest, seenAccess = observation.job.lastInterest, observation.job.lastAccess
		observation.job.mu.RUnlock()
	})
	page, err := b.readSnapshotPageAt(t.Context(), scope, 10, job, boundary)
	if err != nil || page.State != BrowseReady {
		t.Fatalf("snapshot page = %#v, err = %v", page, err)
	}
	if !seenInterest.Equal(stale) || !seenAccess.Equal(stale) {
		t.Fatalf("snapshot_read fired after the job was renewed: interest %v, access %v", seenInterest, seenAccess)
	}
	job.mu.RLock()
	renewed := job.lastInterest.After(stale) && job.lastAccess.After(stale)
	job.mu.RUnlock()
	if !renewed {
		t.Fatal("reading the snapshot did not renew the job")
	}
}

func TestBrowserCapacityHooksFireOutsideTheCapacityLock(t *testing.T) {
	source := singleFileRetrySource(t)
	b := source.browser
	var mu sync.Mutex
	locked := map[string]bool{}
	setBrowserObserver(b, "capacity_operation", func(observation browserObservation) {
		if observation.stage != "admission" && observation.stage != "retry" {
			return
		}
		acquired := b.capacityMu.TryLock()
		if acquired {
			b.capacityMu.Unlock()
		}
		mu.Lock()
		locked[observation.stage] = !acquired
		mu.Unlock()
	})
	cursor := source.cursor(t)
	first, err := b.ReadPage(t.Context(), BrowseRequest{Scope: source.scope, Cursor: cursor, Limit: 1})
	if err != nil || first.State != BrowsePreparing {
		t.Fatalf("first read = %#v, err = %v", first, err)
	}
	job := source.job()
	if job == nil {
		t.Fatal("no preparation job was admitted")
	}
	waitForJobFailure(t, job)
	if retried, err := b.ReadPage(t.Context(), BrowseRequest{Scope: source.scope, Cursor: first.NextCursor, Limit: 1}); err != nil || retried.State != BrowsePreparing {
		t.Fatalf("automatic retry = %#v, err = %v", retried, err)
	}
	mu.Lock()
	defer mu.Unlock()
	for _, stage := range []string{"admission", "retry"} {
		isLocked, fired := locked[stage]
		if !fired || isLocked {
			t.Errorf("%s hook fired=%v with the capacity lock held=%v; want fired before the lock", stage, fired, isLocked)
		}
	}
}

func TestBrowserChainQueueAdmissionHookFollowsWakeupAndUnlock(t *testing.T) {
	reader, home := testReader(t)
	path := filepath.Join(home, ".claude", "projects", "-work", testSessionID+".jsonl")
	writeRows(t, path,
		map[string]any{"type": "user", "uuid": "u1", "message": map[string]any{"content": "first"}},
		map[string]any{"type": "assistant", "uuid": "a1", "message": map[string]any{"content": "second"}},
	)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	// No scheduler runs here, so a wakeup stays in the channel until the test reads it.
	b := &Browser{
		reader: reader, cacheRoot: t.TempDir(), options: DefaultBrowserOptions(), ctx: ctx, cancel: cancel,
		wake: make(chan struct{}, 1), done: make(chan *browseJob, 1),
		jobs: map[string]*browseJob{}, dedup: map[string]string{}, chains: map[string]*claudeChainContext{}, chainByKey: map[string]string{},
	}
	scope := normalizeBrowseScope(BrowseScope{Provider: "claude", CWD: "/work", SessionID: testSessionID})
	source, code := b.sourceFor(scope, false)
	if code != "" {
		t.Fatal(code)
	}
	digest, err := fileRangeDigest(ctx, source.file, 0, source.end)
	if err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	var woken, lockFree []bool
	setBrowserObserver(b, "chain_queue_admission", func(browserObservation) {
		free := b.capacityMu.TryLock()
		if free {
			b.capacityMu.Unlock()
		}
		mu.Lock()
		woken = append(woken, len(b.wake) == 1)
		lockFree = append(lockFree, free)
		mu.Unlock()
	})
	job, page := b.createJob(scope, source, 0, source.end, source.end, digest, "chain", 0,
		claudeChainJobBinding{footerStart: 0, footerEnd: source.end, footerDigest: digest})
	if job == nil || page != nil {
		source.close()
		t.Fatalf("job = %v, page = %v", job, page)
	}
	t.Cleanup(func() {
		job.mu.RLock()
		index, file := job.index, job.source.file
		job.mu.RUnlock()
		_ = index.remove()
		_ = file.Close()
	})

	<-b.wake
	b.mu.Lock()
	b.queue = nil
	b.mu.Unlock()
	job.mu.Lock()
	job.queued = false
	job.state = BrowseFailed
	job.err = browseErrorFor("index_failed", "History preparation failed.", true)
	job.mu.Unlock()
	if !b.retryJob(ctx, job, newClaudeChainRequestBudget(b.options.RecentBytes).validation) {
		t.Fatal("retry was refused")
	}

	mu.Lock()
	defer mu.Unlock()
	if len(woken) != 2 {
		t.Fatalf("queue admission hook fired %d times, want once for the creator and once for the retry", len(woken))
	}
	for index, name := range []string{"creator", "retry"} {
		if !woken[index] || !lockFree[index] {
			t.Errorf("%s: scheduler woken before the hook = %v, capacity lock free in the hook = %v; want both", name, woken[index], lockFree[index])
		}
	}
}

func TestBrowserLockedCapacityHookPrecedesTheFirstCapacityCheck(t *testing.T) {
	reader, home := testReader(t)
	path := filepath.Join(home, ".claude", "projects", "-work", testSessionID+".jsonl")
	writeRows(t, path, jobOutcomeRows()...)
	options := DefaultBrowserOptions()
	options.RecentBytes = 1
	b := newTestBrowser(t, reader, options)
	var mu sync.Mutex
	stored := -1
	b.capacityMu.Lock()
	b.capacityRemove = func(index *snapshotIndex) error {
		count, err := index.countWithCheckpoint(nil)
		if err != nil {
			t.Error(err)
		}
		mu.Lock()
		stored = count
		mu.Unlock()
		return index.remove()
	}
	b.unlockCapacity()
	var once sync.Once
	setBrowserObserver(b, "capacity_operation", func(observation browserObservation) {
		if observation.stage == "batch" && observation.locked {
			once.Do(func() { b.options.AggregateQuota = 1 })
		}
	})
	scope := BrowseScope{Provider: "claude", CWD: "/work", SessionID: testSessionID}
	latest, err := b.ReadPage(t.Context(), BrowseRequest{Scope: scope, Limit: 1})
	if err != nil || latest.NextCursor == "" {
		t.Fatalf("latest page = %#v, err = %v", latest, err)
	}
	if _, err := b.ReadPage(t.Context(), BrowseRequest{Scope: scope, Cursor: latest.NextCursor, Limit: 1}); err != nil {
		t.Fatal(err)
	}
	job := soleJob(b)
	if job == nil {
		t.Fatal("no preparation job was admitted")
	}
	failure := waitForJobFailure(t, job)
	if failure == nil || failure.Code != "index_capacity_exceeded" {
		t.Fatalf("failure = %+v, want the quota set in the hook to be enforced", failure)
	}
	mu.Lock()
	defer mu.Unlock()
	if stored != 0 {
		t.Fatalf("%d entries were stored although the quota was already exceeded before the write", stored)
	}
}
