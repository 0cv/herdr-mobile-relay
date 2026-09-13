package conversation

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func claudeScheduleFixture(t *testing.T, segments int) (*Browser, BrowseScope, []string) {
	t.Helper()
	reader, home := testReader(t)
	root := filepath.Join(home, ".claude", "projects", "-work")
	paths := make([]string, segments)
	for segment := range segments {
		session := fmt.Sprintf("123e4567-e89b-12d3-a456-426614174%03d", segment)
		paths[segment] = filepath.Join(root, session+".jsonl")
		rows := []map[string]any{}
		for row := range 90 {
			rows = append(rows, map[string]any{"type": "assistant", "uuid": fmt.Sprintf("s%d-r%d", segment, row), "message": map[string]any{"content": fmt.Sprintf("original-%02d ", row) + strings.Repeat("payload ", 512)}})
		}
		if segment+1 < segments {
			rows = append(rows, map[string]any{"type": "continued-in", "sessionId": session, "continuedInSessionId": fmt.Sprintf("123e4567-e89b-12d3-a456-426614174%03d", segment+1)})
		}
		writeRows(t, paths[segment], rows...)
	}
	options := DefaultBrowserOptions()
	options.RecentBytes = 256 * 1024
	options.MaxPageSize = 512
	b, err := NewBrowser(reader, t.TempDir(), options)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = b.Close() })
	return b, normalizeBrowseScope(BrowseScope{Provider: "claude", CWD: "/work", SessionID: testSessionID}), paths
}

func scheduleLatest(t *testing.T, b *Browser, scope BrowseScope) BrowsePage {
	t.Helper()
	for range 80 {
		page, err := b.ReadPage(context.Background(), BrowseRequest{Scope: scope, Limit: 1})
		if err != nil {
			t.Fatal(err)
		}
		if page.ReasonCode != "validation_pending" && page.State != BrowsePreparing {
			return page
		}
		b.validationWG.Wait()
	}
	t.Fatal("latest did not make progress")
	return BrowsePage{}
}

func scheduleChain(t *testing.T, b *Browser, scope BrowseScope) *claudeChainContext {
	t.Helper()
	b.mu.Lock()
	defer b.mu.Unlock()
	for _, chain := range b.chains {
		if chain.scopeID == browseScopeID(scope) {
			return chain
		}
	}
	t.Fatal("no admitted chain")
	return nil
}

func TestClaudeChainCreatorPausedAtQueueAdmission(t *testing.T) {
	for _, retry := range []bool{false, true} {
		t.Run(fmt.Sprintf("retry=%v", retry), func(t *testing.T) {
			b, scope, paths := claudeScheduleFixture(t, 2)
			latest := scheduleLatest(t, b, scope)
			if latest.ReasonCode != "" {
				t.Fatalf("latest=%#v", latest)
			}
			chain := scheduleChain(t, b, scope)
			var job *browseJob
			if retry {
				firstCapture, release := make(chan struct{}), make(chan struct{})
				b.chainWorkerCaptureObserver = func([]claudeRangeEvidence) { close(firstCapture); <-release }
				var page *BrowsePage
				job, page = b.startClaudeChainJob(scope, chain, 1)
				if job == nil || page != nil {
					t.Fatalf("first job=%v page=%v", job, page)
				}
				<-firstCapture
				job.mu.Lock()
				job.lastInterest = time.Now().Add(-2 * b.options.InterestLease)
				job.mu.Unlock()
				close(release)
				b.workerWG.Wait()
				b.releaseJob(job)
				job.mu.RLock()
				failed := job.state == BrowseFailed && job.err.Retryable && job.sourceDigest == ""
				job.mu.RUnlock()
				if !failed {
					t.Fatal("did not inject a pre-scan retryable lease failure")
				}
			}
			admitted := make(chan *browseJob, 1)
			creatorRelease := make(chan struct{})
			workerRelease := make(chan struct{})
			captured := make(chan []claudeRangeEvidence, 1)
			var creatorOnce, workerOnce sync.Once
			defer creatorOnce.Do(func() { close(creatorRelease) })
			defer workerOnce.Do(func() { close(workerRelease) })
			creatorPaused := make(chan struct{})
			b.chainQueueAdmissionObserver = func(j *browseJob) { close(creatorPaused); admitted <- j; <-creatorRelease }
			b.chainWorkerBeforeCaptureObserver = func() { <-creatorPaused }
			b.chainWorkerCaptureObserver = func(e []claudeRangeEvidence) { captured <- e; <-workerRelease }
			returned := make(chan bool, 1)
			go func() {
				if retry {
					returned <- b.retryJob(context.Background(), job)
					return
				}
				j, p := b.startClaudeChainJob(scope, chain, 1)
				if j != nil {
					b.releaseJob(j)
				}
				returned <- j != nil && p == nil
			}()
			select {
			case job = <-admitted:
			case <-time.After(3 * time.Second):
				t.Fatal("creator did not pause after queue wakeup")
			}
			var evidence []claudeRangeEvidence
			select {
			case evidence = <-captured:
			case <-time.After(3 * time.Second):
				t.Fatal("worker did not capture while creator paused")
			}
			select {
			case <-returned:
				t.Fatal("creator returned before worker capture")
			default:
			}
			// The selected marker is outside both the first-record anchor and footer.
			current, err := os.ReadFile(paths[1])
			if err != nil {
				t.Fatal(err)
			}
			marker := "original-50"
			offset := int64(strings.Index(string(current), marker))
			covered := false
			for _, item := range evidence {
				if item.Start <= offset && item.End > offset {
					covered = true
				}
			}
			if offset <= 64*1024 || offset >= int64(len(current))-64*1024 || !covered {
				t.Errorf("worker captured no selected non-footer obligation at %d: %#v", offset, evidence)
			}
			// Duplicate callers must see one admission even while its creator is paused.
			var wg sync.WaitGroup
			for range 12 {
				wg.Add(1)
				go func() {
					defer wg.Done()
					j, p := b.startClaudeChainJob(scope, chain, 1)
					if j != nil {
						b.releaseJob(j)
						if j != job {
							t.Error("duplicate job")
						}
					} else if p == nil || p.State != BrowsePreparing {
						t.Error("duplicate lost preparation")
					}
				}()
			}
			wg.Wait()
			b.mu.Lock()
			jobs := len(b.jobs)
			b.mu.Unlock()
			if jobs != 1 {
				t.Fatalf("jobs=%d", jobs)
			}
			rewritten := strings.Replace(string(current), marker, "rewriten-50", 1)
			if len(rewritten) != len(current) {
				t.Fatal("rewrite length")
			}
			if err := os.WriteFile(paths[1], []byte(rewritten), 0o600); err != nil {
				t.Fatal(err)
			}
			appendClaudeTestRow(t, paths[1], map[string]any{"type": "assistant", "uuid": "growth", "message": map[string]any{"content": "append beyond saved end"}})
			workerOnce.Do(func() { close(workerRelease) })
			b.workerWG.Wait()
			job.mu.RLock()
			state, failure := job.state, job.err
			job.mu.RUnlock()
			if state != BrowseFailed || failure == nil || failure.Code != "source_changed" {
				t.Errorf("queue race admitted rewrite: state=%s failure=%#v", state, failure)
			}
			creatorOnce.Do(func() { close(creatorRelease) })
			if !<-returned {
				t.Fatal("creator did not retain admitted job")
			}
			t.Logf("creator paused after wakeup; worker captured %d obligations; retry=%v state=%s failure=%#v", len(evidence), retry, state, failure)
		})
	}
}

func TestClaudeChainHotAppendEvidenceAndPhysicalBounds(t *testing.T) {
	b, scope, paths := claudeScheduleFixture(t, 2)
	var foreground, background, compactions atomic.Int64
	b.foregroundReadObserver = func(n int64) { foreground.Add(n) }
	b.backgroundReadObserver = func(n int64) { background.Add(n) }
	var mutateCompaction atomic.Bool
	mutationResult := make(chan error, 1)
	b.chainCompactionObserver = func() {
		compactions.Add(1)
		if !mutateCompaction.Swap(false) {
			return
		}
		// This hook runs after the real hot scope's old obligations have been
		// read and before either enclosing digest. Preserve the current append
		// and footer, change only an observed middle row, then grow again.
		current, err := os.ReadFile(paths[1])
		if err == nil {
			rewritten := strings.Replace(string(current), "original-50", "rewriten-50", 1)
			if rewritten == string(current) || len(rewritten) != len(current) {
				err = fmt.Errorf("missing equal-length compaction mutation")
			} else {
				err = os.WriteFile(paths[1], []byte(rewritten+"{\"type\":\"assistant\",\"message\":{\"content\":\"growth across compaction phases\"}}\n"), 0o600)
			}
		}
		mutationResult <- err
	}
	initial := scheduleLatest(t, b, scope)
	if initial.ReasonCode != "" {
		t.Fatalf("initial=%#v", initial)
	}
	// One long-lived Browser and scope, with genuinely overlapping footer/tail
	// evidence admitted by latest requests, rather than synthetic ranges.
	maxEvidence := 0
	for round := 0; round < 48; round++ {
		appendClaudeTestRow(t, paths[1], map[string]any{"type": "assistant", "uuid": fmt.Sprintf("hot-%d", round), "message": map[string]any{"content": strings.Repeat("hot append ", 100)}})
		page := BrowsePage{}
		for attempt := 0; attempt < 80; attempt++ {
			before := foreground.Load()
			page, _ = b.ReadPage(context.Background(), BrowseRequest{Scope: scope, Limit: 1})
			used := foreground.Load() - before
			budget := newClaudeChainRequestBudget(b.options.RecentBytes)
			bound := b.options.RecentBytes + budget.discovery + budget.validation.physicalRemaining
			if used > bound {
				t.Fatalf("request physical=%d > bound=%d", used, bound)
			}
			b.validationWG.Wait()
			b.mu.Lock()
			for _, segment := range b.chainLineage[browseScopeID(scope)].segments {
				count := len(uniqueClaudeRangeEvidence(claudeChainEvidence(segment)))
				if count > maxEvidence {
					maxEvidence = count
				}
				if count > maxClaudeChainEvidenceRanges+4 {
					t.Errorf("unbounded retained evidence: %d", count)
				}
			}
			if len(b.chainCompactions) > maxClaudeChainValidationTasks || len(b.chainValidations) > maxClaudeChainValidationResults || len(b.chains) > maxClaudeChainContexts {
				t.Error("unbounded scheduler/context retention")
			}
			for _, context := range b.chains {
				for _, segment := range context.chain.Segments {
					if len(uniqueClaudeRangeEvidence(claudeChainEvidence(segment))) > maxClaudeChainEvidenceRanges+4 {
						t.Error("old context retained an unbounded evidence ledger")
					}
				}
			}
			b.mu.Unlock()
			if page.State != BrowsePreparing && page.ReasonCode != "validation_pending" {
				break
			}
		}
		if page.ReasonCode != "" || page.SourceRevision != initial.SourceRevision {
			t.Fatalf("append %d=%#v", round, page)
		}
	}
	if compactions.Load() < 2 || background.Load() == 0 {
		t.Fatalf("did not exercise real compaction: count=%d background=%d", compactions.Load(), background.Load())
	}
	mutateCompaction.Store(true)
	var changed BrowsePage
	for round := range 16 {
		appendClaudeTestRow(t, paths[1], map[string]any{"type": "assistant", "uuid": fmt.Sprintf("phase-growth-%d", round), "message": map[string]any{"content": "append until real evidence compacts again"}})
		changed = scheduleLatest(t, b, scope)
		if changed.ReasonCode != "" {
			break
		}
	}
	select {
	case err := <-mutationResult:
		if err != nil {
			t.Fatal(err)
		}
	default:
		t.Fatal("hot scope never crossed the compaction mutation barrier")
	}
	if changed.ReasonCode != "source_changed" {
		t.Fatalf("compaction phase rewrite-plus-append accepted: %#v", changed)
	}
	t.Logf("48 hot appends: max retained=%d compactions=%d foreground physical=%d background physical=%d", maxEvidence, compactions.Load(), foreground.Load(), background.Load())
}

func TestClaudeChainPreparedAppendLatestConcurrentGrownSegments(t *testing.T) {
	b, scope, paths := claudeScheduleFixture(t, 4)
	latest := scheduleLatest(t, b, scope)
	chain := scheduleChain(t, b, scope)
	// Prepare every segment through the production job admission path.
	for segment := range paths {
		job, page := b.startClaudeChainJob(scope, chain, segment)
		if job == nil || page != nil {
			t.Fatalf("prepare %d=%v", segment, page)
		}
		b.releaseJob(job)
	}
	deadline := time.Now().Add(10 * time.Second)
	for {
		b.mu.Lock()
		active, queued := b.active, len(b.queue)
		b.mu.Unlock()
		if active == 0 && queued == 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("preparation stalled")
		}
		time.Sleep(time.Millisecond)
	}
	for _, job := range b.claudeChainView(chain).jobs {
		if job == nil {
			t.Fatal("missing job")
		}
		job.mu.RLock()
		ready := job.state == BrowseReady
		job.mu.RUnlock()
		if !ready {
			t.Fatal("preparation failed")
		}
	}
	savedCursors := make([]string, len(paths))
	for segment := range paths {
		var err error
		savedCursors[segment], err = b.claudeChainCursor(scope, chain, segment, b.claudeChainView(chain).chain.Segments[segment].CapturedEnd, true)
		if err != nil {
			t.Fatal(err)
		}
		for range 3 {
			page, err := b.ReadPage(context.Background(), BrowseRequest{Scope: scope, Cursor: savedCursors[segment], Limit: 1})
			if err != nil {
				t.Fatal(err)
			}
			if page.State == BrowseReady {
				break
			}
		}
		if !b.claudeChainSegmentPrepared(chain, segment) {
			t.Fatalf("segment %d was not activated as a prepared cursor", segment)
		}
	}
	for segment, path := range paths {
		appendClaudeTestRow(t, path, map[string]any{"type": "assistant", "uuid": fmt.Sprintf("growth-%d", segment), "message": map[string]any{"content": "grown segment"}})
		if segment+1 < len(paths) {
			appendClaudeTestRow(t, path, map[string]any{"type": "continued-in", "sessionId": strings.TrimSuffix(filepath.Base(path), ".jsonl"), "continuedInSessionId": strings.TrimSuffix(filepath.Base(paths[segment+1]), ".jsonl")})
		}
	}
	// Hold real background validation after an actual underlying read while
	// concurrent latest requests deduplicate all grown segment obligations.
	entered, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	var foreground, background atomic.Int64
	b.foregroundReadObserver = func(n int64) { foreground.Add(n) }
	b.backgroundReadObserver = func(n int64) { background.Add(n); once.Do(func() { close(entered); <-release }) }
	var wg sync.WaitGroup
	pages := make(chan BrowsePage, 12)
	for caller := range 12 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			cursor := ""
			if caller%3 != 0 {
				cursor = savedCursors[caller%len(savedCursors)]
			}
			page, _ := b.ReadPage(context.Background(), BrowseRequest{Scope: scope, Cursor: cursor, Limit: 1})
			pages <- page
		}()
	}
	select {
	case <-entered:
	case <-time.After(3 * time.Second):
		close(release)
		t.Fatal("no background validation")
	}
	wg.Wait()
	close(pages)
	b.mu.Lock()
	active := b.chainValidationActive
	tasks := len(b.chainValidations)
	b.mu.Unlock()
	close(release)
	b.validationWG.Wait()
	if active != maxClaudeChainValidationTasks || tasks > maxClaudeChainValidationResults {
		t.Fatalf("did not saturate the bounded scheduler across grown segments: active=%d results=%d", active, tasks)
	}
	for page := range pages {
		if page.ReasonCode != "validation_pending" && page.State != BrowsePreparing {
			t.Fatalf("did not defer grown prepared evidence: %#v", page)
		}
	}
	budget := newClaudeChainRequestBudget(b.options.RecentBytes)
	if foreground.Load() > 12*(b.options.RecentBytes+budget.discovery+budget.validation.physicalRemaining) {
		t.Fatal("concurrent foreground exceeded aggregate per-request bounds")
	}
	for segment, cursor := range savedCursors {
		var page BrowsePage
		for range 20 {
			page, _ = b.ReadPage(context.Background(), BrowseRequest{Scope: scope, Cursor: cursor, Limit: 1})
			if page.State != BrowsePreparing {
				break
			}
			b.validationWG.Wait()
		}
		if page.State != BrowseReady || page.ReasonCode != "" || len(page.Entries) != 1 || !strings.HasPrefix(page.Entries[0].Text, "original-89") {
			t.Fatalf("frozen prepared segment %d admitted growth or failed: %#v", segment, page)
		}
	}
	ready := scheduleLatest(t, b, scope)
	if ready.ReasonCode != "" || ready.SourceRevision != latest.SourceRevision || len(ready.Entries) != 1 || ready.Entries[0].Text != "grown segment" {
		t.Fatalf("prepared append/latest=%#v", ready)
	}
	current, err := os.ReadFile(paths[2])
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(paths[2], []byte(strings.Replace(string(current), "original-20", "rewriten-20", 1)), 0o600); err != nil {
		t.Fatal(err)
	}
	appendClaudeTestRow(t, paths[2], map[string]any{"type": "continued-in", "sessionId": strings.TrimSuffix(filepath.Base(paths[2]), ".jsonl"), "continuedInSessionId": strings.TrimSuffix(filepath.Base(paths[3]), ".jsonl")})
	if page := scheduleLatest(t, b, scope); page.ReasonCode != "source_changed" {
		t.Fatalf("prepared prefix rewrite accepted=%#v", page)
	}
	t.Logf("concurrent grown prepared segments: foreground=%d background=%d active=%d lineage-results=%d", foreground.Load(), background.Load(), active, tasks)
}
