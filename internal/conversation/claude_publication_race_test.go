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

func awaitClaudeBarrier(t *testing.T, done <-chan struct{}) {
	t.Helper()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("Claude schedule barrier timed out")
	}
}

// F002 from run 67b44cc5: a saved preparation publishes a NEW obligation
// while a compactor authenticates an older tail-only evidence generation.
func TestClaudeChainCompactionOverlapsSavedPreparation(t *testing.T) {
	for _, rewrite := range []bool{false, true} {
		t.Run(fmt.Sprintf("rewrite=%v", rewrite), func(t *testing.T) {
			b, scope, paths := claudeScheduleFixture(t, 2)
			var foreground, background, phases atomic.Int64
			b.foregroundReadObserver = func(n int64) { foreground.Add(n) }
			b.backgroundReadObserver = func(n int64) { background.Add(n) }
			initial := scheduleLatest(t, b, scope)
			if initial.ReasonCode != "" || initial.NextCursor == "" {
				t.Fatalf("initial=%#v", initial)
			}
			chain := scheduleChain(t, b, scope)
			e0 := b.claudeChainView(chain).chain.Segments[1].CapturedEnd
			preparing, releasePreparation := make(chan struct{}), make(chan struct{})
			compacting, releaseCompaction := make(chan struct{}), make(chan struct{})
			var preparationOnce, compactionOnce, hookOnce sync.Once
			defer preparationOnce.Do(func() { close(releasePreparation) })
			defer compactionOnce.Do(func() { close(releaseCompaction) })
			b.chainPreparationObserver = func() { close(preparing); <-releasePreparation }
			b.chainCompactionObserver = func() {
				phases.Add(1)
				hookOnce.Do(func() { close(compacting); <-releaseCompaction })
			}
			job, failure := b.startClaudeChainJob(scope, chain, 1)
			if job == nil || failure != nil {
				t.Fatalf("preparation admission=%v", failure)
			}
			defer b.releaseJob(job)
			awaitClaudeBarrier(t, preparing)
			// Advance the real scope, waiting only for individual validators: the
			// intentionally parked compactor also belongs to validationWG.
			for round := 0; ; round++ {
				if round >= 32 {
					t.Fatal("hot scope did not start compaction")
				}
				appendClaudeTestRow(t, paths[1], map[string]any{"type": "assistant", "uuid": fmt.Sprintf("hot-%d", round), "message": map[string]any{"content": "legitimate hot append"}})
				for attempt := 0; ; attempt++ {
					before := foreground.Load()
					page, err := b.ReadPage(context.Background(), BrowseRequest{Scope: scope, Limit: 1})
					budget := newClaudeChainRequestBudget(b.options.RecentBytes)
					if foreground.Load()-before > b.options.RecentBytes+budget.discovery+budget.validation.physicalRemaining {
						t.Fatal("foreground physical bound exceeded")
					}
					if err != nil || page.ReasonCode != "" && page.ReasonCode != "validation_pending" {
						t.Fatalf("append=%#v %v", page, err)
					}
					b.mu.Lock()
					pending := b.chainLineage[browseScopeID(scope)].segments[1].EvidenceCompactionPending
					var tasks []<-chan struct{}
					for _, task := range b.chainValidations {
						if task.running {
							tasks = append(tasks, task.done)
						}
					}
					b.mu.Unlock()
					if pending || page.ReasonCode == "" {
						break
					}
					if attempt >= 80 {
						t.Fatal("append validator stalled")
					}
					for _, done := range tasks {
						awaitClaudeBarrier(t, done)
					}
				}
				b.mu.Lock()
				pending := b.chainLineage[browseScopeID(scope)].segments[1].EvidenceCompactionPending
				b.mu.Unlock()
				if pending {
					break
				}
			}
			awaitClaudeBarrier(t, compacting)
			b.mu.Lock()
			segment := cloneClaudeSegments(b.chainLineage[browseScopeID(scope)].segments)[1]
			b.mu.Unlock()
			if segment.CapturedEnd <= e0 || segment.EvidenceCompactionStart <= 64*1024 {
				t.Fatalf("not a newer tail-only compaction: %#v", segment)
			}
			tailStart := segment.EvidenceCompactionStart
			preparationOnce.Do(func() { close(releasePreparation) })
			finished := make(chan struct{})
			go func() { b.workerWG.Wait(); close(finished) }()
			awaitClaudeBarrier(t, finished)
			job.mu.RLock()
			state, digest := job.state, job.sourceDigest
			job.mu.RUnlock()
			if state != BrowseReady || digest == "" {
				t.Fatalf("preparation=%s", state)
			}
			obligation := claudeRangeEvidence{Start: 0, End: e0, Digest: digest}
			b.mu.Lock()
			segment = cloneClaudeSegments(b.chainLineage[browseScopeID(scope)].segments)[1]
			b.mu.Unlock()
			found := false
			for _, item := range segment.ObservedRanges {
				if item == obligation {
					found = true
				}
			}
			if !found {
				t.Fatal("older preparation did not publish full-prefix obligation into newer lineage")
			}
			b.mu.Lock()
			generation := claudeChainEvidenceGeneration(segment)
			// Repeated interest/status publication cannot create a new generation.
			for range 12 {
				b.updateClaudeChainLineageEvidenceLocked(chain)
			}
			currentGeneration := claudeChainEvidenceGeneration(b.chainLineage[browseScopeID(scope)].segments[1])
			// Count actual retained copies, not only the non-pending union.
			transient := len(segment.ObservedRanges) + len(segment.EvidenceCompactionRanges) + 3
			for _, task := range b.chainCompactions {
				transient += len(task.evidence) + len(task.candidate.ObservedRanges) + len(task.candidate.EvidenceCompactionRanges) + 3
			}
			b.mu.Unlock()
			if generation != currentGeneration {
				t.Fatal("duplicate evidence advanced generation")
			}
			if transient > 4*(maxClaudeChainEvidenceRanges+4) {
				t.Fatalf("unbounded pending/task/delta copies: %d", transient)
			}
			// Latest admissions cannot manufacture additional contexts while the
			// old task is parked, even though its filesystem source is healthy.
			for range 4 {
				page, err := b.ReadPage(context.Background(), BrowseRequest{Scope: scope, Limit: 1})
				if err != nil || page.ReasonCode != "validation_pending" {
					t.Fatalf("no compaction backpressure: %#v %v", page, err)
				}
			}
			compactionOnce.Do(func() { close(releaseCompaction) })
			b.validationWG.Wait()
			if phases.Load() != 2 {
				t.Fatalf("superseded union did not compact exactly once more: %d", phases.Load())
			}
			b.mu.Lock()
			segment = cloneClaudeSegments(b.chainLineage[browseScopeID(scope)].segments)[1]
			b.mu.Unlock()
			protected := false
			for _, item := range claudeChainEvidence(segment) {
				if item.Start == 0 && item.End >= e0 {
					protected = true
				}
			}
			if !protected {
				t.Error("compaction dropped concurrently prepared full-prefix obligation")
			}
			if len(claudeChainEvidence(segment))+len(segment.EvidenceCompactionRanges) > maxClaudeChainEvidenceRanges+4 {
				t.Fatal("compaction did not converge to bounded evidence")
			}
			current, err := os.ReadFile(paths[1])
			if err != nil {
				t.Fatal(err)
			}
			offset := int64(strings.Index(string(current), "original-20"))
			if offset <= 64*1024 || offset+int64(len("original-20")) >= tailStart {
				t.Fatal("mutation not between anchor and surviving tails")
			}
			if rewrite {
				changed := strings.Replace(string(current), "original-20", "rewriten-20", 1)
				if len(changed) != len(current) || !strings.Contains(changed, "legitimate hot append") {
					t.Fatal("rewrite removed growth")
				}
				if err := os.WriteFile(paths[1], []byte(changed), 0600); err != nil {
					t.Fatal(err)
				}
			}
			appendClaudeTestRow(t, paths[1], map[string]any{"type": "assistant", "message": map[string]any{"content": "post compaction append"}})
			latest := scheduleLatest(t, b, scope)
			if rewrite {
				if latest.ReasonCode != "source_changed" {
					t.Fatalf("changed prepared prefix accepted: reason=%q revision=%q initial=%q", latest.ReasonCode, latest.SourceRevision, initial.SourceRevision)
				}
			} else {
				if latest.ReasonCode != "" || latest.SourceRevision != initial.SourceRevision {
					t.Fatalf("append lost lineage: %#v", latest)
				}
				cursor := initial.NextCursor
				var page BrowsePage
				for attempt := 0; attempt < 8; attempt++ {
					page, err = b.ReadPage(context.Background(), BrowseRequest{Scope: scope, Cursor: cursor, Limit: 1})
					if err != nil || page.State != BrowsePreparing {
						break
					}
					cursor = page.NextCursor
					b.validationWG.Wait()
				}
				if err != nil || page.ReasonCode != "" || page.State != BrowseReady || page.SourceRevision != initial.SourceRevision {
					t.Fatalf("saved cursor=%#v %v", page, err)
				}
				for _, row := range page.Entries {
					if strings.Contains(row.Text, "append") {
						t.Fatal("saved cursor leaked later growth")
					}
				}
			}
			t.Logf("overlap E0=%d Ek=%d tailStart=%d transientCopies=%d retained=%d phases=%d physical foreground=%d background=%d", e0, segment.CapturedEnd, tailStart, transient, len(claudeChainEvidence(segment)), phases.Load(), foreground.Load(), background.Load())
		})
	}
}

// F009 from run 67b44cc5: pause discovery AFTER B's E1 footer bytes,
// not its revision anchor and not the later foreground evidence read.
func TestClaudeChainGrowthBetweenDiscoveryAndValidationOpen(t *testing.T) {
	for _, rewrite := range []bool{false, true} {
		t.Run(fmt.Sprintf("rewrite=%v", rewrite), func(t *testing.T) {
			reader, home := testReader(t)
			root := filepath.Join(home, ".claude", "projects", "-work")
			child := "123e4567-e89b-12d3-a456-426614174001"
			writeRows(t, filepath.Join(root, testSessionID+".jsonl"), map[string]any{"type": "continued-in", "sessionId": testSessionID, "continuedInSessionId": child})
			path := filepath.Join(root, child+".jsonl")
			writeRows(t, path,
				map[string]any{"type": "user", "uuid": "b1", "message": map[string]any{"content": "stable first"}},
				map[string]any{"type": "assistant", "uuid": "b2", "message": map[string]any{"content": "observed-original"}},
				map[string]any{"type": "assistant", "uuid": "b3", "message": map[string]any{"content": "stable footer"}})
			b, err := NewBrowser(reader, t.TempDir(), DefaultBrowserOptions())
			if err != nil {
				t.Fatal(err)
			}
			defer b.Close()
			scope := normalizeBrowseScope(BrowseScope{Provider: "claude", CWD: "/work", SessionID: testSessionID})
			initial := scheduleLatest(t, b, scope)
			if initial.ReasonCode != "" || initial.NextCursor == "" {
				t.Fatalf("initial=%#v", initial)
			}
			appendClaudeTestRow(t, path, map[string]any{"type": "assistant", "message": map[string]any{"content": "legitimate R1"}})
			current, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			e1 := int64(len(current))
			entered, release := make(chan struct{}), make(chan struct{})
			var reads atomic.Int64
			var once sync.Once
			defer once.Do(func() { close(release) })
			b.discoveryReadObserver = func(n int64) {
				if n == e1 && reads.Add(1) == 2 {
					close(entered)
					<-release
				}
			}
			done := make(chan BrowsePage, 1)
			go func() {
				page, _ := b.ReadPage(context.Background(), BrowseRequest{Scope: scope, Limit: 10})
				done <- page
			}()
			awaitClaudeBarrier(t, entered)
			if rewrite {
				changed := strings.Replace(string(current), "observed-original", "observed-rewriten", 1)
				if len(changed) != len(current) || !strings.Contains(changed, "legitimate R1") {
					t.Fatal("mutation lost R1")
				}
				if err := os.WriteFile(path, []byte(changed), 0600); err != nil {
					t.Fatal(err)
				}
			}
			appendClaudeTestRow(t, path, map[string]any{"type": "assistant", "message": map[string]any{"content": "legitimate R2"}})
			once.Do(func() { close(release) })
			var result BrowsePage
			select {
			case result = <-done:
			case <-time.After(10 * time.Second):
				t.Fatal("latest stalled")
			}
			if result.ReasonCode != "validation_pending" || result.Error == nil || !result.Error.Retryable {
				t.Fatalf("advanced candidate not retryable: %#v", result)
			}
			latest := scheduleLatest(t, b, scope)
			if rewrite {
				if latest.ReasonCode != "source_changed" {
					t.Fatalf("rewrite accepted: %#v", latest)
				}
				return
			}
			if latest.ReasonCode != "" || latest.SourceRevision != initial.SourceRevision {
				t.Fatalf("append lost lineage: %#v", latest)
			}
			page, err := b.ReadPage(context.Background(), BrowseRequest{Scope: scope, Cursor: initial.NextCursor, Limit: 10})
			if err != nil || page.State != BrowseReady || page.ReasonCode != "" || page.SourceRevision != initial.SourceRevision {
				t.Fatalf("saved cursor=%#v %v", page, err)
			}
			for _, row := range page.Entries {
				if strings.Contains(row.Text, "legitimate R") {
					t.Fatal("saved cursor leaked append")
				}
			}
		})
	}
}
