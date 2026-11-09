package conversation

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func (b *Browser) aggregateCacheBytes() int64 {
	total, _, err := b.cacheUsage()
	if err != nil {
		return math.MaxInt64
	}
	return total
}

func initialSnapshotBytes(t *testing.T) int64 {
	t.Helper()
	index, err := openSnapshotIndex(t.TempDir(), "measure", snapshotMetadata{Schema: 1, State: "building"})
	if err != nil {
		t.Fatal(err)
	}
	defer index.remove()
	return index.size()
}

func capacityTestBrowser(t *testing.T) *Browser {
	t.Helper()
	return &Browser{cacheRoot: t.TempDir(), options: DefaultBrowserOptions(), jobs: map[string]*browseJob{}, dedup: map[string]string{}, chains: map[string]*claudeChainContext{}, chainByKey: map[string]string{}}
}

func capacityTestJob(t *testing.T, b *Browser, id string) *browseJob {
	t.Helper()
	index, err := openSnapshotIndex(b.cacheRoot, id, snapshotMetadata{Schema: 1})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = index.remove() })
	file, err := os.CreateTemp(t.TempDir(), "source")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = file.Close() })
	job := &browseJob{id: id, dedupKey: id, index: index, state: BrowseReady, source: fileSource{file: file, revision: "revision", end: 1}, scopeID: "scope", lastAccess: time.Unix(1, 0)}
	b.jobs[id], b.dedup[id] = job, id
	return job
}

func TestCapacityAdmissionAllocationAndDedup(t *testing.T) {
	for _, headroom := range []bool{false, true} {
		t.Run(fmt.Sprint(headroom), func(t *testing.T) {
			b := capacityTestBrowser(t)
			victim := capacityTestJob(t, b, "victim")
			b.options.AggregateQuota = victim.index.size()
			if headroom {
				b.options.AggregateQuota++
			}
			sourceFile, err := os.CreateTemp(t.TempDir(), "private-source")
			if err != nil {
				t.Fatal(err)
			}
			defer sourceFile.Close()
			source := fileSource{file: sourceFile, revision: "new", end: 2}
			job, page := b.createJob(BrowseScope{Provider: "claude", SessionID: "private-id"}, source, 0, 2, 2, "digest", false, "", 0)
			if page != nil || job == nil || !victim.retired {
				t.Fatalf("job=%v page=%+v retired=%v", job, page, victim.retired)
			}
			defer job.index.remove()
			b.capacityMu.Lock()
			b.options.AggregateQuota = b.aggregateCacheBytes()
			b.unlockCapacity()
			duplicate, page := b.createJob(job.scope, source, 0, 2, 2, "digest", false, "", 0)
			if page != nil || duplicate != job {
				t.Fatalf("dedup at capacity: job=%v page=%+v", duplicate, page)
			}
			if len(b.jobs) != 1 {
				t.Fatalf("jobs=%d", len(b.jobs))
			}
		})
	}
}

func TestCapacityLRUAndNoPressure(t *testing.T) {
	b := capacityTestBrowser(t)
	first := capacityTestJob(t, b, "a")
	second := capacityTestJob(t, b, "b")
	newer := capacityTestJob(t, b, "c")
	newer.lastAccess = time.Unix(2, 0)
	b.capacityMu.Lock()
	if code := b.ensureCapacity(nil, nil, "admission", true); code != "" {
		t.Fatal(code)
	}
	b.unlockCapacity()
	if first.retired || second.retired || newer.retired {
		t.Fatal("eviction without pressure")
	}
	b.options.AggregateQuota = b.aggregateCacheBytes()
	for _, want := range []*browseJob{first, second} {
		b.capacityMu.Lock()
		code := b.ensureCapacity(nil, nil, "admission", true)
		b.unlockCapacity()
		if code != "" || !want.retired || newer.retired {
			t.Fatalf("LRU code=%s victim=%s", code, want.id)
		}
		b.options.AggregateQuota = b.aggregateCacheBytes()
	}
}

func TestCapacityPreservesIndexlessFailedJobs(t *testing.T) {
	for _, reclaimable := range []bool{true, false} {
		t.Run(fmt.Sprint(reclaimable), func(t *testing.T) {
			b := capacityTestBrowser(t)
			failed := capacityTestJob(t, b, "failed")
			failed.cursor = "retry-continuation"
			failed.autoRetried = true
			b.failJob(failed, "index_capacity_exceeded", "Cache is full.", true)
			failed.state = BrowseFailed
			if failed.index != nil || b.aggregateCacheBytes() != 0 {
				t.Fatal("failure did not remove its index")
			}
			victim := capacityTestJob(t, b, "victim")
			victim.lastAccess = failed.lastAccess.Add(time.Second)
			size := victim.index.size()
			b.options.AggregateQuota = size
			if !reclaimable {
				if err := os.WriteFile(filepath.Join(b.cacheRoot, "snapshot-orphan.db"), make([]byte, size), 0600); err != nil {
					t.Fatal(err)
				}
			}
			var events []CapacityEvent
			b.options.CapacityObserver = func(event CapacityEvent) { events = append(events, event) }
			b.capacityMu.Lock()
			code := b.ensureCapacity(nil, nil, "admission", true)
			b.unlockCapacity()
			wantCode := ""
			if !reclaimable {
				wantCode = "index_capacity_exceeded"
				if len(events) != 1 || events[0].Skipped != 1 || events[0].Protected != 0 || events[0].Eligible != 1 || events[0].Evicted != 1 || events[0].ReclaimedBytes != size {
					t.Errorf("capacity events = %+v", events)
				}
			}
			if code != wantCode || !victim.retired {
				t.Errorf("code=%q victim retired=%v", code, victim.retired)
			}
			if failed.retired || b.jobs[failed.id] != failed || b.dedup[failed.dedupKey] != failed.id {
				t.Error("pressure removed the index-less failed job from its registries")
			}
			page, err := b.continueJob(t.Context(), BrowseRequest{}, failed)
			if err != nil || page.State != BrowseFailed || page.ReasonCode != "index_capacity_exceeded" || !page.HasMore || page.NextCursor != failed.cursor || page.Error == nil || !page.Error.Retryable {
				t.Errorf("failed continuation = %+v, err=%v", page, err)
			}
			b.evictExpired(failed.lastAccess.Add(b.options.SnapshotTTL - time.Nanosecond))
			if failed.retired {
				t.Error("failed job expired before TTL")
			}
			b.evictExpired(failed.lastAccess.Add(b.options.SnapshotTTL))
			if !failed.retired || b.jobs[failed.id] != nil || b.dedup[failed.dedupKey] != "" {
				t.Error("TTL did not remove the failed job")
			}
		})
	}
}

func TestCapacityGrowthStages(t *testing.T) {
	for _, stage := range []string{"batch", "finalize"} {
		t.Run(stage, func(t *testing.T) {
			b := capacityTestBrowser(t)
			victim := capacityTestJob(t, b, "victim")
			request := capacityTestJob(t, b, "request")
			request.running = true
			before := request.index.size()
			grow := func(index *snapshotIndex) error {
				if stage == "finalize" {
					return index.setMetadata(snapshotMetadata{Schema: 1, SourceDigest: strings.Repeat("x", int(before)*2)})
				}
				_, err := index.applyRecords("claude", "revision", []JSONLRecord{{Complete: true, Raw: []byte(fmt.Sprintf(`{"type":"user","uuid":"u","message":{"content":%q}}`, strings.Repeat("x", int(before)*2)))}})
				return err
			}
			measured, err := openSnapshotIndex(t.TempDir(), "measure", snapshotMetadata{Schema: 1})
			if err != nil {
				t.Fatal(err)
			}
			defer measured.remove()
			if err := grow(measured); err != nil {
				t.Fatal(err)
			}
			after := measured.size()
			if after <= before {
				t.Fatal("fixture did not grow")
			}
			b.options.AggregateQuota = after + victim.index.size() - 1
			code := b.writeWithCapacity(request, stage, "write failed", func() error { return grow(request.index) })
			if code != "" || !victim.retired || request.retired || b.aggregateCacheBytes() != after {
				t.Fatalf("code=%s bytes=%d after=%d", code, b.aggregateCacheBytes(), after)
			}
		})
	}
}

func TestCapacityPriorityAndTombstones(t *testing.T) {
	b := capacityTestBrowser(t)
	unrelated := capacityTestJob(t, b, "a")
	unrelated.scopeID = "other"
	preferred := capacityTestJob(t, b, "b")
	preferred.lastAccess = time.Unix(2, 0)
	chain := &claudeChainContext{id: "chain", key: "key", jobs: []*browseJob{preferred}}
	b.chains[chain.id], b.chainByKey[chain.key] = chain, chain.id
	b.options.AggregateQuota = b.aggregateCacheBytes()
	request := &browseJob{scopeID: "scope", source: fileSource{revision: "revision", end: 2}}
	b.capacityMu.Lock()
	code := b.ensureCapacity(request, nil, "admission", true)
	b.unlockCapacity()
	if code != "" || !preferred.retired || unrelated.retired {
		t.Fatalf("code=%s preferred=%v unrelated=%v", code, preferred.retired, unrelated.retired)
	}
	if preferred.index != nil || preferred.source.file != nil || b.jobs[preferred.id] != nil || b.dedup[preferred.dedupKey] != "" {
		t.Fatal("retired ownership remains")
	}
	if chain.jobs[0] != preferred || b.chainByKey[chain.key] != "" {
		t.Fatal("chain tombstone or latest mapping incorrect")
	}
	if b.retainJob(preferred) {
		t.Fatal("retired cursor retained")
	}
}

func TestCapacityProtectsOwners(t *testing.T) {
	for _, protection := range []string{"reader", "queued", "running", "validation", "chain"} {
		t.Run(protection, func(t *testing.T) {
			b := capacityTestBrowser(t)
			job := capacityTestJob(t, b, "owned")
			switch protection {
			case "reader":
				job.readers = 1
			case "queued":
				job.queued = true
			case "running":
				job.running = true
			case "validation":
				job.chainValidationRunning = true
			case "chain":
				b.chains["chain"] = &claudeChainContext{refs: 1, jobs: []*browseJob{job}}
			}
			b.options.AggregateQuota = b.aggregateCacheBytes()
			var event CapacityEvent
			b.options.CapacityObserver = func(value CapacityEvent) { event = value }
			b.capacityMu.Lock()
			code := b.ensureCapacity(nil, nil, "admission", true)
			b.unlockCapacity()
			if code != "index_capacity_exceeded" || job.retired || event.Protected != 1 || event.Skipped != 0 {
				t.Fatalf("code=%s retired=%v", code, job.retired)
			}
			job.readers = 0
			job.queued = false
			job.running = false
			job.chainValidationRunning = false
			delete(b.chains, "chain")
			b.capacityMu.Lock()
			code = b.ensureCapacity(nil, nil, "admission", true)
			b.unlockCapacity()
			if code != "" || !job.retired {
				t.Fatalf("released owner code=%s retired=%v", code, job.retired)
			}
		})
	}
}

func TestCapacityPhysicalFailuresAndThrottle(t *testing.T) {
	b := capacityTestBrowser(t)
	job := capacityTestJob(t, b, "victim")
	size := b.aggregateCacheBytes()
	if err := os.WriteFile(filepath.Join(b.cacheRoot, "snapshot-orphan.db"), make([]byte, size), 0600); err != nil {
		t.Fatal(err)
	}
	b.options.AggregateQuota = size
	b.capacityRemove = func(index *snapshotIndex) error { _ = index.close(); return errors.New("private-path-do-not-log") }
	now := time.Unix(100, 0)
	b.capacityNow = func() time.Time { return now }
	var events []CapacityEvent
	b.options.CapacityObserver = func(event CapacityEvent) {
		if !b.capacityMu.TryLock() {
			t.Fatal("callback under capacity lock")
		}
		b.capacityMu.Unlock()
		if !b.mu.TryLock() {
			t.Fatal("callback under registry lock")
		}
		b.mu.Unlock()
		events = append(events, event)
	}
	deny := func() {
		b.capacityMu.Lock()
		code := b.ensureCapacity(nil, nil, "admission", true)
		b.unlockCapacity()
		if code != "index_capacity_exceeded" {
			t.Fatalf("code=%s", code)
		}
	}
	deny()
	deny()
	if !job.retired || b.aggregateCacheBytes() != 2*size || len(events) != 1 || events[0].CleanupFailures != 1 || events[0].ReclaimedBytes != 0 {
		t.Fatalf("events=%+v bytes=%d", events, b.aggregateCacheBytes())
	}
	now = now.Add(time.Minute)
	deny()
	if len(events) != 2 || events[1].Suppressed != 1 {
		t.Fatalf("events=%+v", events)
	}
	b.cacheRoot = filepath.Join(t.TempDir(), "missing")
	b.capacityMu.Lock()
	code := b.ensureCapacity(nil, nil, "retry", true)
	b.unlockCapacity()
	if code != "index_storage_unavailable" {
		t.Fatalf("accounting error code=%s", code)
	}
}

func TestCapacitySnapshotQuotaDoesNotEvict(t *testing.T) {
	b := capacityTestBrowser(t)
	victim := capacityTestJob(t, b, "victim")
	request := capacityTestJob(t, b, "request")
	b.options.SnapshotQuota = request.index.size() - 1
	b.capacityMu.Lock()
	code := b.ensureCapacity(request, request.index, "batch", false)
	b.unlockCapacity()
	if code != "index_capacity_exceeded" || victim.retired {
		t.Fatalf("code=%s victim=%v", code, victim.retired)
	}
}

func TestCapacityWriteFailures(t *testing.T) {
	for stage, message := range map[string]string{
		"batch":    "History preparation could not store its index.",
		"finalize": "The prepared conversation index could not be finalized.",
	} {
		for _, denial := range []string{"", "index_capacity_exceeded", "index_storage_unavailable"} {
			t.Run(stage+denial, func(t *testing.T) {
				b := capacityTestBrowser(t)
				job := capacityTestJob(t, b, "request")
				path := job.index.path
				if denial == "index_capacity_exceeded" {
					b.options.SnapshotQuota = 1
				} else if denial == "index_storage_unavailable" {
					b.cacheRoot = filepath.Join(t.TempDir(), "missing")
				}
				called := false
				code := b.writeWithCapacity(job, stage, message, func() error {
					called = true
					return errors.New("synthetic write failure")
				})
				wantCode, wantMessage := "index_failed", message
				if denial != "" {
					wantCode, wantMessage = denial, capacityFailurePage(denial).Reason
				}
				if code != wantCode || called != (denial == "") || job.err == nil || job.err.Code != wantCode || job.err.Message != wantMessage || !job.err.Retryable {
					t.Fatalf("code=%s called=%v error=%+v", code, called, job.err)
				}
				if job.index != nil || job.source.file != nil {
					t.Fatal("failed write retained resources")
				}
				if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
					t.Fatalf("index remains: %v", err)
				}
			})
		}
	}
}

func TestCacheUsageDisappearedEntry(t *testing.T) {
	b := capacityTestBrowser(t)
	path := filepath.Join(b.cacheRoot, "snapshot-gone.db")
	if err := os.WriteFile(path, []byte("gone"), 0600); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(b.cacheRoot)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if total, files, err := b.cacheEntryUsage(entries); err != nil || total != 0 || files != 0 {
		t.Fatalf("total=%d files=%d err=%v", total, files, err)
	}
	b.cacheRoot = path
	if err := os.WriteFile(path, nil, 0600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := b.cacheEntryUsage(entries); err == nil {
		t.Fatal("non-directory error swallowed")
	}
	if _, _, err := b.cacheUsage(); err == nil {
		t.Fatal("invalid root accepted")
	}
	b.cacheRoot = filepath.Join(t.TempDir(), "missing")
	if _, _, err := b.cacheUsage(); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("missing root: %v", err)
	}
}

func TestCapacityAdmissionClosing(t *testing.T) {
	b := capacityTestBrowser(t)
	b.closing = true
	job, page := b.createJob(BrowseScope{}, fileSource{}, 0, 0, 0, "", false, "", 0)
	if job != nil || page == nil || page.Reason != "Older history preparation is shutting down." || page.ReasonCode != "index_failed" || page.Error == nil || !page.Error.Retryable {
		t.Fatalf("job=%v page=%+v", job, page)
	}
	if len(b.jobs) != 0 || b.aggregateCacheBytes() != 0 {
		t.Fatal("closing admission allocated resources")
	}
}

func TestCapacityOMOTraversals(t *testing.T) {
	home := t.TempDir()
	sessionID := "synthetic-session"
	cwd := "/synthetic-work"
	path := filepath.Join(home, ".omo", "agent", "sessions", encodeOMOCWD(cwd), "2026-09-02T12-00-00-000Z_"+sessionID+".jsonl")
	rows := []map[string]any{
		{"type": "session", "id": sessionID, "cwd": cwd},
		{"type": "custom", "customType": "senpi.todo-state", "data": map[string]any{"schema": "v2", "phases": []any{map[string]any{"name": "Build", "tasks": []any{map[string]any{"content": "Ship", "status": "pending"}}}}}},
	}
	for i := 0; i < 40; i++ {
		rows = append(rows, map[string]any{"type": "message", "id": fmt.Sprint(i), "message": map[string]any{"role": "user", "content": fmt.Sprintf("%d %s", i, strings.Repeat("x", 512))}})
	}
	writeRows(t, path, rows...)
	options := DefaultBrowserOptions()
	options.RecentBytes = 1
	b, err := NewBrowser(NewReader(home), t.TempDir(), options)
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	scope := BrowseScope{Provider: "omo", CWD: cwd, SessionID: sessionID}
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	for generation := 0; generation < 6; generation++ {
		cursor := ""
		count := 0
		sawPlan := false
		for {
			page, err := b.ReadPage(ctx, BrowseRequest{Scope: scope, Cursor: cursor, Limit: 10})
			if err != nil || page.State == BrowseFailed {
				t.Fatalf("OMO: %+v %v", page, err)
			}
			if page.State == BrowsePreparing {
				select {
				case <-ctx.Done():
					t.Fatal("OMO timeout")
				case <-time.After(time.Millisecond):
				}
				continue
			}
			count += len(page.Entries)
			sawPlan = sawPlan || page.OMOPlan != nil
			if !page.HasMore {
				break
			}
			cursor = page.NextCursor
		}
		if count != 40+generation || !sawPlan {
			t.Fatalf("OMO count=%d plan=%v", count, sawPlan)
		}
		if generation == 0 {
			b.capacityMu.Lock()
			b.options.AggregateQuota = b.aggregateCacheBytes() * 2
			b.unlockCapacity()
		}
		if b.aggregateCacheBytes() > b.options.AggregateQuota {
			t.Fatal("OMO over quota")
		}
		file, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0600)
		if err != nil {
			t.Fatal(err)
		}
		err = json.NewEncoder(file).Encode(map[string]any{"type": "message", "id": fmt.Sprint(40 + generation), "message": map[string]any{"role": "user", "content": "appended"}})
		closeErr := file.Close()
		if err != nil || closeErr != nil {
			t.Fatalf("append %v %v", err, closeErr)
		}
	}
}

func TestCapacityAppendTraversals(t *testing.T) {
	reader, home := testReader(t)
	path := filepath.Join(home, ".claude", "projects", "-work", testSessionID+".jsonl")
	options := DefaultBrowserOptions()
	options.RecentBytes = 1
	browser, err := NewBrowser(reader, t.TempDir(), options)
	if err != nil {
		t.Fatal(err)
	}
	defer browser.Close()
	scope := BrowseScope{Provider: "claude", CWD: "/work", SessionID: testSessionID}
	var rows []map[string]any
	for i := 0; i < 100; i++ {
		rows = append(rows, map[string]any{"type": "user", "uuid": fmt.Sprintf("u%d", i), "message": map[string]any{"content": strings.Repeat("x", 512)}})
	}
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	traverse := func() {
		t.Helper()
		cursor := ""
		seen := map[string]bool{}
		for {
			page, err := browser.ReadPage(ctx, BrowseRequest{Scope: scope, Cursor: cursor, Limit: 10})
			if err != nil || page.State == BrowseFailed {
				t.Fatalf("page=%+v err=%v", page, err)
			}
			if page.State == BrowsePreparing {
				select {
				case <-ctx.Done():
					t.Fatal("preparation timed out")
				case <-time.After(time.Millisecond):
				}
				continue
			}
			for _, entry := range page.Entries {
				if seen[entry.ID] {
					t.Fatalf("duplicate %s", entry.ID)
				}
				seen[entry.ID] = true
			}
			if !page.HasMore {
				break
			}
			cursor = page.NextCursor
		}
		if len(seen) != len(rows) {
			t.Fatalf("got %d rows want %d", len(seen), len(rows))
		}
	}
	writeRows(t, path, rows...)
	traverse()
	browser.capacityMu.Lock()
	browser.options.AggregateQuota = browser.aggregateCacheBytes() * 2
	browser.capacityMu.Unlock()
	for generation := 0; generation < 8; generation++ {
		rows = append(rows, map[string]any{"type": "user", "uuid": fmt.Sprintf("append%d", generation), "message": map[string]any{"content": "append"}})
		file, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0600)
		if err != nil {
			t.Fatal(err)
		}
		err = json.NewEncoder(file).Encode(rows[len(rows)-1])
		closeErr := file.Close()
		if err != nil || closeErr != nil {
			t.Fatalf("append: %v %v", err, closeErr)
		}
		traverse()
		if bytes := browser.aggregateCacheBytes(); bytes > browser.options.AggregateQuota {
			t.Fatalf("bytes=%d quota=%d", bytes, browser.options.AggregateQuota)
		}
	}
}
