package conversation

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/0cv/herdr-mobile-relay/internal/agentroots"
)

func TestKimiOpeningProjectionWorkBudget(t *testing.T) {
	home, root := t.TempDir(), t.TempDir()
	t.Setenv(agentroots.KimiCodeListEnv, root)
	t.Setenv("KIMI_CODE_HOME", "")
	cwd, id := "/fixture/kimi-project", "session_budget"
	var wire bytes.Buffer
	wire.WriteString("{\"type\":\"metadata\",\"protocol_version\":\"1.5\"}\n")
	const count = 12000
	for i := 0; i < count; i++ {
		fmt.Fprintf(&wire, "{\"type\":\"context.append_message\",\"message\":{\"role\":\"assistant\",\"content\":\"%08d:%s\"}}\n", i, strings.Repeat("x", 160))
	}
	path := kimiFixture(t, root, kimiWorkspaceKey(cwd), id, cwd, "Budget", 1, wire.Bytes())
	browser, err := NewBrowser(NewReader(home), t.TempDir(), DefaultBrowserOptions())
	if err != nil {
		t.Fatal(err)
	}
	defer browser.Close()
	scope := BrowseScope{Provider: "kimi", CWD: cwd, SessionID: id, PaneID: "pane", ProcessIdentity: "1:argv"}
	var acquired int64
	projections := 0
	browser.physicalReadObserver = func(n int64) { acquired += n }
	browser.kimiProjectionObserver = func() { projections++ }
	read := func(cursor string) BrowsePage {
		t.Helper()
		page, err := browser.ReadPage(t.Context(), BrowseRequest{Scope: scope, Limit: 24, Cursor: cursor})
		if err != nil || !page.Available || page.Error != nil || page.ReasonCode != "" {
			t.Fatalf("page=%+v err=%v", page, err)
		}
		return page
	}
	first := read("")
	coldBytes := acquired
	if len(first.Entries) != 24 || !first.HasMore || coldBytes != int64(wire.Len()) || coldBytes > maxConversationBytes || projections != 1 || browser.kimiProjection == nil {
		t.Fatalf("cold: entries=%d bytes=%d projections=%d cached=%t", len(first.Entries), acquired, projections, browser.kimiProjection != nil)
	}
	acquired = 0
	warm := read("")
	if warm.Entries[0].ID != first.Entries[0].ID || acquired != 0 || projections != 1 {
		t.Fatalf("warm repeated preparation: bytes=%d projections=%d", acquired, projections)
	}
	seen := map[string]bool{}
	last := count
	page := warm
	for {
		for i := len(page.Entries) - 1; i >= 0; i-- {
			entry := page.Entries[i]
			last--
			if seen[entry.ID] || !strings.HasPrefix(entry.Text, fmt.Sprintf("%08d:", last)) {
				t.Fatalf("gap or duplicate at %d: %+v", last, entry)
			}
			seen[entry.ID] = true
		}
		if !page.HasMore {
			break
		}
		if page.NextCursor == "" {
			t.Fatal("missing cursor")
		}
		page = read(page.NextCursor)
	}
	if last != 0 || len(seen) != count || acquired != 0 || projections != 1 {
		t.Fatalf("paging repeated preparation: remaining=%d seen=%d bytes=%d projections=%d", last, len(seen), acquired, projections)
	}
	t.Logf("cold verified Kimi preparation: %d journal bytes, 1 replay; warm opening and %d older entries: %d journal bytes, 0 replays; wire page: 24 entries", coldBytes, count-24, acquired)

	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	rewritten := bytes.Replace(wire.Bytes(), []byte("00000000:"), []byte("changed!:"), 1)
	if err := os.WriteFile(path, rewritten, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(path, time.Now(), info.ModTime()); err != nil {
		t.Fatal(err)
	}
	changed := read("")
	if projections != 2 || acquired != int64(len(rewritten)) || changed.SourceRevision == first.SourceRevision {
		t.Fatal("same-size restored-mtime rewrite reused a stale projection")
	}
	stale, err := browser.ReadPage(t.Context(), BrowseRequest{Scope: scope, Limit: 24, Cursor: first.NextCursor})
	if err != nil || stale.ReasonCode != "source_changed" {
		t.Fatalf("stale cursor=%+v err=%v", stale, err)
	}
	acquired = 0
	browser.kimiProjection.lastUsed = time.Now().Add(-2 * browser.options.RecentCacheTTL)
	read("")
	if projections != 3 || acquired != int64(len(rewritten)) {
		t.Fatal("expired projection was retained")
	}

	replacement := filepath.Join(filepath.Dir(path), "replacement.jsonl")
	if err := os.WriteFile(replacement, rewritten, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(replacement, path); err != nil {
		t.Fatal(err)
	}
	if replaced := read(""); projections != 4 || replaced.SourceRevision == changed.SourceRevision {
		t.Fatal("replaced file reused cached source identity")
	}
	scope.ProcessIdentity = "2:new"
	read("")
	if projections != 5 {
		t.Fatal("new process reused a different scope's projection")
	}
	browser.options.RecentCacheItemBytes = 1
	browser.kimiProjection = nil
	read("")
	read("")
	if browser.kimiProjection != nil || projections != 7 {
		t.Fatal("projection cache exceeded its item budget")
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	cancelled, err := browser.ReadPage(ctx, BrowseRequest{Scope: scope, Limit: 24})
	if err != nil || cancelled.ReasonCode != "request_cancelled" || projections != 7 {
		t.Fatal("cancelled request acquired a projection")
	}
}
