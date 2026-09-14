package conversation

import (
	"bytes"
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

// Current-run F002: P authenticates E1, Q publishes E2, then P attempts
// admission. P's later stale-source response must not roll the ledger back.
func TestClaudeChainLateLatestAdmissionPreservesNewerEvidence(t *testing.T) {
	for _, schedule := range []struct{ afterBuild, rewrite bool }{{false, false}, {false, true}, {true, false}, {true, true}} {
		t.Run(fmt.Sprintf("afterBuild=%v/rewrite=%v", schedule.afterBuild, schedule.rewrite), func(t *testing.T) {
			rewrite := schedule.rewrite
			reader, home := testReader(t)
			root := filepath.Join(home, ".claude", "projects", "-work")
			child := "123e4567-e89b-12d3-a456-426614174001"
			writeRows(t, filepath.Join(root, testSessionID+".jsonl"),
				map[string]any{"type": "continued-in", "sessionId": testSessionID, "continuedInSessionId": child})
			path := filepath.Join(root, child+".jsonl")
			writeRows(t, path,
				map[string]any{"type": "user", "uuid": "b1", "message": map[string]any{"content": "stable first"}},
				map[string]any{"type": "assistant", "uuid": "b2", "message": map[string]any{"content": "stable middle"}},
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
			appendClaudeTestRow(t, path, map[string]any{"type": "assistant", "uuid": "r1", "message": map[string]any{"content": "legitimate R1"}})
			prefix, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			e1 := int64(len(prefix))
			entered, release := make(chan struct{}), make(chan struct{})
			var stopped atomic.Bool
			var releaseOnce sync.Once
			defer releaseOnce.Do(func() { close(release) })
			b.chainContextAdmissionObserver = func(candidate claudeChain, afterBuild bool) {
				if afterBuild == schedule.afterBuild && len(candidate.Segments) == 2 && candidate.Segments[1].CapturedEnd == e1 && stopped.CompareAndSwap(false, true) {
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
			appendClaudeTestRow(t, path, map[string]any{"type": "assistant", "uuid": "r2", "message": map[string]any{"content": "R2-original"}})
			newer, err := b.ReadPage(context.Background(), BrowseRequest{Scope: scope, Limit: 1})
			if err != nil || newer.ReasonCode != "" || len(newer.Entries) != 1 || newer.Entries[0].Text != "R2-original" || newer.SourceRevision != initial.SourceRevision {
				t.Fatalf("Q did not publish R2: %#v %v", newer, err)
			}
			b.mu.Lock()
			q := cloneClaudeSegments(b.chainLineage[browseScopeID(scope)].segments)[1]
			b.mu.Unlock()
			qEvidence := claudeChainEvidence(q)
			if q.CapturedEnd <= e1 || q.CapturedEnd >= 64*1024 || q.EvidenceCompactionPending {
				t.Fatal("not the small, non-compacting E2 schedule")
			}
			full := false
			for _, item := range qEvidence {
				if item.Start == 0 && item.End == q.CapturedEnd {
					full = true
				}
			}
			if !full {
				t.Fatal("Q did not publish its complete E2 range")
			}
			releaseOnce.Do(func() { close(release) })
			select {
			case p := <-done:
				if p.ReasonCode != "validation_pending" || p.Error == nil || !p.Error.Retryable {
					t.Fatalf("P must rediscover: %#v", p)
				}
			case <-time.After(10 * time.Second):
				t.Fatal("P did not finish")
			}
			b.mu.Lock()
			retained := cloneClaudeSegments(b.chainLineage[browseScopeID(scope)].segments)[1]
			b.mu.Unlock()
			if retained.CapturedEnd < q.CapturedEnd {
				t.Errorf("P rolled lineage end back from E2=%d to %d", q.CapturedEnd, retained.CapturedEnd)
			}
			for _, obligation := range qEvidence {
				found := false
				for _, item := range claudeChainEvidence(retained) {
					if item == obligation {
						found = true
					}
				}
				if !found {
					t.Errorf("P dropped Q obligation [%d,%d]", obligation.Start, obligation.End)
				}
			}
			current, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if rewrite {
				changed := bytes.Replace(current, []byte("R2-original"), []byte("R2-rewriten"), 1)
				if bytes.Equal(current, changed) || len(changed) != len(current) || !bytes.Equal(changed[:e1], prefix) || int64(bytes.Index(changed, []byte("R2-rewriten"))) < e1 {
					t.Fatal("rewrite did not exclusively change the equal-length R2 beyond E1")
				}
				if err := os.WriteFile(path, changed, 0600); err != nil {
					t.Fatal(err)
				}
			}
			appendClaudeTestRow(t, path, map[string]any{"type": "assistant", "uuid": "r3", "message": map[string]any{"content": "legitimate R3"}})
			latest, err := b.ReadPage(context.Background(), BrowseRequest{Scope: scope, Limit: 10})
			if err != nil {
				t.Fatal(err)
			}
			if rewrite {
				if latest.ReasonCode != "source_changed" {
					t.Fatalf("rewritten Q bytes accepted under revision %q (original %q): %#v", latest.SourceRevision, initial.SourceRevision, latest)
				}
				return
			}
			if latest.ReasonCode != "" || latest.SourceRevision != initial.SourceRevision {
				t.Fatalf("append lost identity: %#v", latest)
			}
			for _, saved := range []BrowsePage{initial, newer} {
				page, err := b.ReadPage(context.Background(), BrowseRequest{Scope: scope, Cursor: saved.NextCursor, Limit: 10})
				if err != nil || page.ReasonCode != "" || page.State != BrowseReady || page.SourceRevision != initial.SourceRevision {
					t.Fatalf("saved cursor=%#v %v", page, err)
				}
				for _, row := range page.Entries {
					if strings.Contains(row.Text, "R3") {
						t.Fatal("saved cursor leaked new append")
					}
				}
			}
			t.Logf("P(E1=%d) returned stale after Q(E2=%d); retained end=%d, obligations=%d", e1, q.CapturedEnd, retained.CapturedEnd, len(qEvidence))
		})
	}
}
