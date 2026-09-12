package conversation

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func appendClaudeTestRow(t *testing.T, path string, row map[string]any) {
	t.Helper()
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0)
	if err != nil {
		t.Fatal(err)
	}
	err = json.NewEncoder(f).Encode(row)
	closeErr := f.Close()
	if err != nil {
		t.Fatal(err)
	}
	if closeErr != nil {
		t.Fatal(closeErr)
	}
}

func TestClaudeChainForegroundValidationMutationBeforeAdmission(t *testing.T) {
	for _, rewrite := range []bool{true, false} {
		t.Run(fmt.Sprintf("rewrite=%v", rewrite), func(t *testing.T) {
			reader, home := testReader(t)
			root := filepath.Join(home, ".claude", "projects", "-work")
			child := "123e4567-e89b-12d3-a456-426614174001"
			writeRows(t, filepath.Join(root, testSessionID+".jsonl"),
				map[string]any{"type": "assistant", "uuid": "a", "message": map[string]any{"content": "parent"}},
				map[string]any{"type": "continued-in", "sessionId": testSessionID, "continuedInSessionId": child})
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
			scope := BrowseScope{Provider: "claude", CWD: "/work", SessionID: testSessionID}
			initial, err := b.ReadPage(context.Background(), BrowseRequest{Scope: scope, Limit: 10})
			if err != nil || len(initial.Entries) != 3 || initial.ReasonCode != "" {
				t.Fatalf("initial: %#v %v", initial, err)
			}
			original, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			oldEnd := int64(len(original))
			b.mu.Lock()
			segments := cloneClaudeSegments(b.chainLineage[browseScopeID(normalizeBrowseScope(scope))].segments)
			b.mu.Unlock()
			if len(segments) != 2 {
				t.Fatalf("lineage segments = %d", len(segments))
			}
			evidence := claudeChainEvidence(segments[1])
			if len(evidence) != 1 || evidence[0].Start != 0 || evidence[0].End != oldEnd || oldEnd >= 32*1024 {
				t.Fatalf("not one small whole-file obligation: %#v", evidence)
			}
			appendClaudeTestRow(t, path, map[string]any{"type": "assistant", "uuid": "r1", "message": map[string]any{"content": "legitimate R1"}})
			entered, release := make(chan struct{}), make(chan struct{})
			var once atomic.Bool
			b.validationReadObserver = func(n int64) {
				// The revision anchor reads E1, not E0. Only stop after the old digest's
				// actual ReadAt returned its bytes, before hashing that buffered copy.
				if n == oldEnd && once.CompareAndSwap(false, true) {
					close(entered)
					<-release
				}
			}
			done := make(chan BrowsePage, 1)
			go func() { p, _ := b.ReadPage(context.Background(), BrowseRequest{Scope: scope, Limit: 10}); done <- p }()
			select {
			case <-entered:
			case <-time.After(3 * time.Second):
				close(release)
				t.Fatal("old evidence read did not reach barrier")
			}
			current, err := os.ReadFile(path)
			if err != nil {
				close(release)
				t.Fatal(err)
			}
			if rewrite {
				changed := strings.Replace(string(current), "observed-original", "observed-rewriten", 1)
				if len(changed) != len(current) || !strings.Contains(changed, "legitimate R1") {
					close(release)
					t.Fatal("mutation lost append or changed length")
				}
				if err := os.WriteFile(path, []byte(changed), 0600); err != nil {
					close(release)
					t.Fatal(err)
				}
			}
			appendClaudeTestRow(t, path, map[string]any{"type": "assistant", "uuid": "r2", "message": map[string]any{"content": "legitimate R2"}})
			close(release)
			var result BrowsePage
			select {
			case result = <-done:
			case <-time.After(3 * time.Second):
				t.Fatal("latest did not finish")
			}
			for _, row := range result.Entries {
				if strings.Contains(row.Text, "observed-rewriten") {
					t.Fatalf("admitted rewritten observed history: %#v", result)
				}
			}
			for attempt := 0; result.ReasonCode == "validation_pending" && attempt < 5; attempt++ {
				b.validationWG.Wait()
				result, err = b.ReadPage(context.Background(), BrowseRequest{Scope: scope, Limit: 10})
				if err != nil {
					t.Fatal(err)
				}
			}
			if rewrite {
				if result.ReasonCode != "source_changed" {
					t.Fatalf("rewrite accepted: %#v", result)
				}
				return
			}
			if result.ReasonCode != "" || result.SourceRevision != initial.SourceRevision {
				t.Fatalf("legitimate concurrent append lost identity: %#v", result)
			}
		})
	}
}
