package conversation

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestClaudeChainPreparesAClippedParent(t *testing.T) {
	reader, home := testReader(t)
	anchor := testSessionID
	child := "123e4567-e89b-12d3-a456-426614174001"
	root := filepath.Join(home, ".claude", "projects", "-work")
	parentRows := make([]map[string]any, 0, 360)
	for index := 0; index < 360; index++ {
		parentRows = append(parentRows, map[string]any{
			"type": "assistant", "uuid": fmt.Sprintf("parent-%d", index),
			"message": map[string]any{"content": strings.Repeat("parent ", 8000)},
		})
	}
	parentRows = append(parentRows, map[string]any{"type": "continued-in", "sessionId": anchor, "continuedInSessionId": child})
	writeRows(t, filepath.Join(root, anchor+".jsonl"), parentRows...)
	writeRows(t, filepath.Join(root, child+".jsonl"), map[string]any{
		"type": "assistant", "uuid": "child-1", "message": map[string]any{"content": "child answer"},
	})

	options := DefaultBrowserOptions()
	options.RecentBytes = 128
	options.DefaultPageSize = 1
	options.MaxPageSize = 2
	browser, err := NewBrowser(reader, t.TempDir(), options)
	if err != nil {
		t.Fatal(err)
	}
	defer browser.Close()
	scope := BrowseScope{Provider: "claude", CWD: "/work", SessionID: anchor}
	latest, err := browser.ReadPage(context.Background(), BrowseRequest{Scope: scope, Limit: 1})
	if err != nil || len(latest.Entries) != 1 || latest.Entries[0].Text != "child answer" || latest.NextCursor == "" {
		t.Fatalf("latest = %#v, err=%v", latest, err)
	}
	preparing, err := browser.ReadPage(context.Background(), BrowseRequest{Scope: scope, Cursor: latest.NextCursor, Limit: 1})
	if err != nil || preparing.State != BrowsePreparing || preparing.NextCursor == "" {
		t.Fatalf("preparing parent = %#v, err=%v", preparing, err)
	}
	parent, err := browser.ReadPage(context.Background(), BrowseRequest{Scope: scope, Cursor: preparing.NextCursor, Limit: 1})
	if err != nil {
		t.Fatal(err)
	}
	if parent.State == BrowsePreparing {
		parent = waitForReadySnapshot(t, browser, scope, preparing.NextCursor, 1)
	}
	if parent.State != BrowseReady || len(parent.Entries) != 1 || !strings.HasPrefix(parent.Entries[0].Text, "parent ") {
		t.Fatalf("prepared parent = %#v", parent)
	}
}

func TestClaudeChainLatestAndOlderSegments(t *testing.T) {
	reader, home := testReader(t)
	a := testSessionID
	b := "123e4567-e89b-12d3-a456-426614174001"
	root := filepath.Join(home, ".claude", "projects", "-work")
	writeRows(t, filepath.Join(root, a+".jsonl"),
		map[string]any{"type": "user", "uuid": "a1", "message": map[string]any{"content": "a1"}},
		map[string]any{"type": "assistant", "uuid": "a2", "message": map[string]any{"content": "a2"}},
		map[string]any{"type": "continued-in", "sessionId": a, "continuedInSessionId": b},
	)
	writeRows(t, filepath.Join(root, b+".jsonl"),
		map[string]any{"type": "user", "uuid": "b1", "message": map[string]any{"content": "b1"}},
		map[string]any{"type": "assistant", "uuid": "b2", "message": map[string]any{"content": "b2"}},
	)
	page, err := reader.ReadFor("claude", "/work", a, "", 1)
	if err != nil || len(page.Entries) != 1 || page.Entries[0].Text != "b2" || page.ContinuationIncomplete {
		t.Fatalf("latest = %#v, err=%v", page, err)
	}
	before := page.Entries[0].ID
	older, err := reader.ReadFor("claude", "/work", a, before, 10)
	if err != nil || len(older.Entries) != 3 || older.Entries[0].Text != "a1" || older.Entries[2].Text != "b1" {
		t.Fatalf("older = %#v, err=%v", older, err)
	}

	browser, err := NewBrowser(reader, t.TempDir(), func() BrowserOptions {
		o := DefaultBrowserOptions()
		o.DefaultPageSize = 1
		o.MaxPageSize = 4
		return o
	}())
	if err != nil {
		t.Fatal(err)
	}
	defer browser.Close()
	scope := BrowseScope{Provider: "claude", CWD: "/work", SessionID: a}
	latest, err := browser.ReadPage(context.Background(), BrowseRequest{Scope: scope, Limit: 1})
	if err != nil || len(latest.Entries) != 1 || latest.Entries[0].Text != "b2" || latest.NextCursor == "" {
		t.Fatalf("browser latest = %#v, err=%v", latest, err)
	}
	want := []string{"b1", "a2", "a1"}
	cursor := latest.NextCursor
	for _, expected := range want {
		olderPage, readErr := browser.ReadPage(context.Background(), BrowseRequest{Scope: scope, Cursor: cursor, Limit: 1})
		if readErr != nil || len(olderPage.Entries) != 1 || olderPage.Entries[0].Text != expected {
			t.Fatalf("browser older = %#v, err=%v, want %q", olderPage, readErr, expected)
		}
		cursor = olderPage.NextCursor
	}
	if cursor != "" {
		t.Fatalf("browser chain retained an EOF cursor %q", cursor)
	}

	options := DefaultBrowserOptions()
	options.RecentBytes = 100
	options.DefaultPageSize = 1
	options.MaxPageSize = 4
	preparedBrowser, err := NewBrowser(reader, t.TempDir(), options)
	if err != nil {
		t.Fatal(err)
	}
	defer preparedBrowser.Close()
	prepared, err := preparedBrowser.ReadPage(context.Background(), BrowseRequest{Scope: scope, Limit: 1})
	if err != nil || len(prepared.Entries) != 1 || prepared.Entries[0].Text != "b2" || prepared.NextCursor == "" {
		t.Fatalf("prepared latest = %#v, err=%v", prepared, err)
	}
	status, err := preparedBrowser.ReadPage(context.Background(), BrowseRequest{Scope: scope, Cursor: prepared.NextCursor, Limit: 1})
	if err != nil || status.State != BrowsePreparing || status.NextCursor == "" {
		t.Fatalf("prepared status = %#v, err=%v", status, err)
	}
	ready, err := preparedBrowser.ReadPage(context.Background(), BrowseRequest{Scope: scope, Cursor: status.NextCursor, Limit: 1})
	if err != nil {
		t.Fatal(err)
	}
	if ready.State == BrowsePreparing {
		ready = waitForReadySnapshot(t, preparedBrowser, scope, status.NextCursor, 1)
	}
	if ready.State != BrowseReady || len(ready.Entries) != 1 || ready.Entries[0].Text != "b1" {
		t.Fatalf("prepared ready = %#v, err=%v", ready, err)
	}
}

func TestClaudeChainEmptyContinuationKeepsParentVisible(t *testing.T) {
	reader, home := testReader(t)
	anchor := testSessionID
	child := "123e4567-e89b-12d3-a456-426614174001"
	root := filepath.Join(home, ".claude", "projects", "-work")
	writeRows(t, filepath.Join(root, anchor+".jsonl"),
		map[string]any{"type": "assistant", "uuid": "a1", "message": map[string]any{"content": "parent answer"}},
		map[string]any{"type": "continued-in", "sessionId": anchor, "continuedInSessionId": child},
	)
	writeRows(t, filepath.Join(root, child+".jsonl"))
	legacy, err := reader.ReadFor("claude", "/work", anchor, "", 10)
	if err != nil || len(legacy.Entries) != 1 || legacy.Entries[0].Text != "parent answer" {
		t.Fatalf("empty child legacy page = %#v, err=%v", legacy, err)
	}
	browser, err := NewBrowser(reader, t.TempDir(), DefaultBrowserOptions())
	if err != nil {
		t.Fatal(err)
	}
	defer browser.Close()
	page, err := browser.ReadPage(context.Background(), BrowseRequest{Scope: BrowseScope{Provider: "claude", CWD: "/work", SessionID: anchor}, Limit: 10})
	if err != nil || len(page.Entries) != 1 || page.Entries[0].Text != "parent answer" {
		t.Fatalf("empty child browser page = %#v, err=%v", page, err)
	}
}

func TestClaudeChainDefaultWindowPreparesTheCurrentClippedSegment(t *testing.T) {
	reader, home := testReader(t)
	anchor := testSessionID
	child := "123e4567-e89b-12d3-a456-426614174001"
	root := filepath.Join(home, ".claude", "projects", "-work")
	writeRows(t, filepath.Join(root, anchor+".jsonl"),
		map[string]any{"type": "assistant", "uuid": "a1", "message": map[string]any{"content": "parent"}},
		map[string]any{"type": "continued-in", "sessionId": anchor, "continuedInSessionId": child},
	)
	rows := make([]map[string]any, 0, 180)
	for index := 0; index < 180; index++ {
		rows = append(rows, map[string]any{
			"type": "assistant", "uuid": fmt.Sprintf("child-%d", index),
			"message": map[string]any{"content": strings.Repeat("child ", 16000)},
		})
	}
	writeRows(t, filepath.Join(root, child+".jsonl"), rows...)
	browser, err := NewBrowser(reader, t.TempDir(), DefaultBrowserOptions())
	if err != nil {
		t.Fatal(err)
	}
	defer browser.Close()
	scope := BrowseScope{Provider: "claude", CWD: "/work", SessionID: anchor}
	page, err := browser.ReadPage(context.Background(), BrowseRequest{Scope: scope})
	if err != nil || len(page.Entries) == 0 || page.Entries[0].Text == "parent" || page.NextCursor == "" {
		t.Fatalf("large child latest = %#v, err=%v", page, err)
	}
	cursor := page.NextCursor
	sawPreparation := false
	seenIDs := make(map[string]bool)
	sawParent, sawChild := false, false
	for _, entry := range page.Entries {
		seenIDs[entry.ID] = true
		sawParent = sawParent || entry.Text == "parent"
		sawChild = sawChild || strings.HasPrefix(entry.Text, "child ")
	}
	for attempt := 0; attempt < 500; attempt++ {
		decoded, decodeErr := decodeBrowseCursor(browser.key, cursor, normalizeBrowseScope(scope))
		if decodeErr != nil {
			t.Fatal(decodeErr)
		}
		if decoded.ChainOffset && decoded.Segment != nil && *decoded.Segment == 1 {
			sawPreparation = true
		}
		older, readErr := browser.ReadPage(context.Background(), BrowseRequest{Scope: scope, Cursor: cursor, Limit: 4})
		if readErr != nil {
			t.Fatal(readErr)
		}
		if older.State == BrowsePreparing {
			older = waitForReadySnapshot(t, browser, scope, cursor, 4)
		}
		for _, entry := range older.Entries {
			seenIDs[entry.ID] = true
			sawParent = sawParent || entry.Text == "parent"
			sawChild = sawChild || strings.HasPrefix(entry.Text, "child ")
		}
		if older.NextCursor == "" {
			break
		}
		cursor = older.NextCursor
		if attempt == 499 {
			t.Fatal("large Claude chain did not finish traversal")
		}
	}
	if !sawPreparation || !sawParent || !sawChild || len(seenIDs) < 181 {
		t.Fatalf("large child traversal lost the clipped segment or parent: sawPreparation=%v parent=%v child=%v entries=%d", sawPreparation, sawParent, sawChild, len(seenIDs))
	}

	latestAgain, err := browser.ReadPage(context.Background(), BrowseRequest{Scope: scope, Limit: 1})
	if err != nil || latestAgain.ReasonCode != "" || latestAgain.NextCursor == "" || len(latestAgain.Entries) != 1 {
		t.Fatalf("latest after preparation = %#v, err=%v", latestAgain, err)
	}
	cursor = latestAgain.NextCursor
	for attempt := 0; attempt < 500 && cursor != ""; attempt++ {
		older, readErr := browser.ReadPage(context.Background(), BrowseRequest{Scope: scope, Cursor: cursor, Limit: 4})
		if readErr != nil {
			t.Fatal(readErr)
		}
		if older.State == BrowsePreparing {
			older = waitForReadySnapshot(t, browser, scope, cursor, 4)
		}
		cursor = older.NextCursor
		if attempt == 499 && cursor != "" {
			t.Fatal("second large Claude chain traversal did not finish")
		}
	}
}

func TestClaudeChainRecentReadsShareAggregateBudget(t *testing.T) {
	reader, home := testReader(t)
	anchor := testSessionID
	sessions := []string{
		anchor,
		"123e4567-e89b-12d3-a456-426614174001",
		"123e4567-e89b-12d3-a456-426614174002",
		"123e4567-e89b-12d3-a456-426614174003",
		"123e4567-e89b-12d3-a456-426614174004",
		"123e4567-e89b-12d3-a456-426614174005",
	}
	root := filepath.Join(home, ".claude", "projects", "-work")
	for index, sessionID := range sessions {
		row := map[string]any{"type": "continued-in", "sessionId": sessionID}
		if index+1 < len(sessions) {
			row["continuedInSessionId"] = sessions[index+1]
		} else {
			row["continuedInSessionId"] = "not-a-session"
		}
		writeRows(t, filepath.Join(root, sessionID+".jsonl"), row)
	}
	options := DefaultBrowserOptions()
	options.RecentBytes = 256
	options.DefaultPageSize = 1
	browser, err := NewBrowser(reader, t.TempDir(), options)
	if err != nil {
		t.Fatal(err)
	}
	defer browser.Close()
	var observed int64
	browser.sourceReadObserver = func(bytes int64) { observed += bytes }
	page, err := browser.ReadPage(context.Background(), BrowseRequest{Scope: BrowseScope{Provider: "claude", CWD: "/work", SessionID: anchor}, Limit: 1})
	if err != nil || page.NextCursor == "" {
		t.Fatalf("empty chain budget page = %#v, err=%v", page, err)
	}
	if observed > options.RecentBytes {
		t.Fatalf("chain recent reads consumed %d bytes, want at most shared budget %d", observed, options.RecentBytes)
	}
}

func TestClaudeChainAppendRefreshesLatestWithoutRetargetingCursor(t *testing.T) {
	reader, home := testReader(t)
	anchor := testSessionID
	child := "123e4567-e89b-12d3-a456-426614174001"
	root := filepath.Join(home, ".claude", "projects", "-work")
	writeRows(t, filepath.Join(root, anchor+".jsonl"),
		map[string]any{"type": "assistant", "uuid": "a1", "message": map[string]any{"content": "parent"}},
		map[string]any{"type": "continued-in", "sessionId": anchor, "continuedInSessionId": child},
	)
	childPath := filepath.Join(root, child+".jsonl")
	writeRows(t, childPath,
		map[string]any{"type": "assistant", "uuid": "b1", "message": map[string]any{"content": "first"}},
		map[string]any{"type": "assistant", "uuid": "b2", "message": map[string]any{"content": "second"}},
	)
	browser, err := NewBrowser(reader, t.TempDir(), DefaultBrowserOptions())
	if err != nil {
		t.Fatal(err)
	}
	defer browser.Close()
	scope := BrowseScope{Provider: "claude", CWD: "/work", SessionID: anchor}
	initial, err := browser.ReadPage(context.Background(), BrowseRequest{Scope: scope, Limit: 1})
	if err != nil || len(initial.Entries) != 1 || initial.Entries[0].Text != "second" || initial.NextCursor == "" {
		t.Fatalf("initial append page = %#v, err=%v", initial, err)
	}
	appendRows := map[string]any{"type": "assistant", "uuid": "b3", "message": map[string]any{"content": "third"}}
	file, err := os.OpenFile(childPath, os.O_WRONLY|os.O_APPEND, 0)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.NewEncoder(file).Encode(appendRows); err != nil {
		_ = file.Close()
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	latest, err := browser.ReadPage(context.Background(), BrowseRequest{Scope: scope, Limit: 1})
	if err != nil || len(latest.Entries) != 1 || latest.Entries[0].Text != "third" {
		t.Fatalf("appended latest page = %#v, err=%v", latest, err)
	}
	older, err := browser.ReadPage(context.Background(), BrowseRequest{Scope: scope, Cursor: initial.NextCursor, Limit: 1})
	if err != nil || older.ReasonCode != "" || len(older.Entries) != 1 || older.Entries[0].Text != "first" {
		t.Fatalf("old append cursor page = %#v, err=%v", older, err)
	}
}

func TestClaudeChainLineageRejectsReplacementAndPreservesAppend(t *testing.T) {
	reader, home := testReader(t)
	anchor := testSessionID
	child := "123e4567-e89b-12d3-a456-426614174001"
	root := filepath.Join(home, ".claude", "projects", "-work")
	writeRows(t, filepath.Join(root, anchor+".jsonl"),
		map[string]any{"type": "assistant", "uuid": "a1", "message": map[string]any{"content": "parent"}},
		map[string]any{"type": "continued-in", "sessionId": anchor, "continuedInSessionId": child},
	)
	writeRows(t, filepath.Join(root, child+".jsonl"),
		map[string]any{"type": "assistant", "uuid": "b1", "message": map[string]any{"content": "first"}},
		map[string]any{"type": "assistant", "uuid": "b2", "message": map[string]any{"content": "second"}},
	)
	browser, err := NewBrowser(reader, t.TempDir(), DefaultBrowserOptions())
	if err != nil {
		t.Fatal(err)
	}
	defer browser.Close()
	scope := BrowseScope{Provider: "claude", CWD: "/work", SessionID: anchor}
	first, err := browser.ReadPage(context.Background(), BrowseRequest{Scope: scope, Limit: 1})
	if err != nil || len(first.Entries) != 1 || first.Entries[0].Text != "second" {
		t.Fatalf("initial lineage page = %#v, err=%v", first, err)
	}
	original, err := os.ReadFile(filepath.Join(root, child+".jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	rewritten := strings.Replace(string(original), "second", "rewrit", 1)
	if len(rewritten) != len(string(original)) {
		t.Fatal("replacement must keep the captured length")
	}
	childPath := filepath.Join(root, child+".jsonl")
	if err := os.WriteFile(childPath, []byte(rewritten), 0o600); err != nil {
		t.Fatal(err)
	}
	appendFile, err := os.OpenFile(childPath, os.O_WRONLY|os.O_APPEND, 0)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.NewEncoder(appendFile).Encode(map[string]any{
		"type": "assistant", "uuid": "b3", "message": map[string]any{"content": "appended after replacement"},
	}); err != nil {
		_ = appendFile.Close()
		t.Fatal(err)
	}
	if err := appendFile.Close(); err != nil {
		t.Fatal(err)
	}
	changedCursor, err := browser.ReadPage(context.Background(), BrowseRequest{Scope: scope, Cursor: first.NextCursor, Limit: 1})
	if err != nil || changedCursor.ReasonCode != "source_changed" {
		t.Fatalf("rewritten middle cursor page = %#v, err=%v", changedCursor, err)
	}
	changed, err := browser.ReadPage(context.Background(), BrowseRequest{Scope: scope, Limit: 1})
	if err != nil || changed.ReasonCode != "source_changed" {
		t.Fatalf("replacement lineage page = %#v, err=%v", changed, err)
	}
	recovered, err := browser.ReadPage(context.Background(), BrowseRequest{Scope: scope, Limit: 1})
	if err != nil || recovered.ReasonCode != "" || len(recovered.Entries) != 1 || recovered.Entries[0].Text != "appended after replacement" {
		t.Fatalf("replacement lineage did not recover = %#v, err=%v", recovered, err)
	}
}

func TestClaudeChainRejectsAliasedFileCycle(t *testing.T) {
	_, home := testReader(t)
	anchor := testSessionID
	child := "123e4567-e89b-12d3-a456-426614174001"
	root := filepath.Join(home, ".claude", "projects", "-work")
	anchorPath := filepath.Join(root, anchor+".jsonl")
	writeRows(t, anchorPath,
		map[string]any{"type": "assistant", "uuid": "a1", "message": map[string]any{"content": "only once"}},
		map[string]any{"type": "continued-in", "sessionId": anchor, "continuedInSessionId": child},
	)
	if err := os.Link(anchorPath, filepath.Join(root, child+".jsonl")); err != nil {
		t.Fatal(err)
	}
	chain, err := resolveClaudeChain(context.Background(), Location{Path: anchorPath, Root: home}, anchor)
	if err != nil {
		t.Fatal(err)
	}
	if len(chain.Segments) != 1 || chain.IncompleteReason != continuationReasonCycle {
		t.Fatalf("aliased chain = %#v, want one readable segment and cycle", chain)
	}
}

func TestClaudeChainRejectsMalformedSessionPointer(t *testing.T) {
	reader, home := testReader(t)
	_ = reader
	anchor := testSessionID
	root := filepath.Join(home, ".claude", "projects", "-work")
	path := filepath.Join(root, anchor+".jsonl")
	writeRows(t, path,
		map[string]any{"type": "assistant", "uuid": "a1", "message": map[string]any{"content": "readable"}},
		map[string]any{"type": "continued-in", "sessionId": nil, "continuedInSessionId": "123e4567-e89b-12d3-a456-426614174001"},
	)
	chain, err := resolveClaudeChain(context.Background(), Location{Path: path, Root: home}, anchor)
	if err != nil {
		t.Fatal(err)
	}
	if chain.IncompleteReason != continuationReasonInvalidLink || len(chain.Segments) != 1 {
		t.Fatalf("malformed session pointer = %#v", chain)
	}
}

func TestClaudeChainReportsOversizedFooter(t *testing.T) {
	_, home := testReader(t)
	anchor := testSessionID
	root := filepath.Join(home, ".claude", "projects", "-work")
	path := filepath.Join(root, anchor+".jsonl")
	writeRows(t, path,
		map[string]any{"type": "assistant", "uuid": "a1", "message": map[string]any{"content": "readable"}},
		map[string]any{"type": "continued-in", "sessionId": anchor, "continuedInSessionId": "123e4567-e89b-12d3-a456-426614174001", "padding": strings.Repeat("x", int(claudeContinuationFooterBytes)+1024)},
	)
	chain, err := resolveClaudeChain(context.Background(), Location{Path: path, Root: home}, anchor)
	if err != nil {
		t.Fatal(err)
	}
	if len(chain.Segments) != 1 || chain.IncompleteReason != continuationReasonLimit {
		t.Fatalf("oversized footer = %#v", chain)
	}
}

func TestClaudeChainPreparationDeduplicatesConcurrentHandles(t *testing.T) {
	reader, home := testReader(t)
	anchor := testSessionID
	child := "123e4567-e89b-12d3-a456-426614174001"
	root := filepath.Join(home, ".claude", "projects", "-work")
	writeRows(t, filepath.Join(root, anchor+".jsonl"),
		map[string]any{"type": "assistant", "uuid": "a1", "message": map[string]any{"content": "parent"}},
		map[string]any{"type": "continued-in", "sessionId": anchor, "continuedInSessionId": child},
	)
	writeRows(t, filepath.Join(root, child+".jsonl"),
		map[string]any{"type": "assistant", "uuid": "b1", "message": map[string]any{"content": "child"}},
	)
	options := DefaultBrowserOptions()
	options.RecentBytes = 1
	browser, err := NewBrowser(reader, t.TempDir(), options)
	if err != nil {
		t.Fatal(err)
	}
	defer browser.Close()
	scope := BrowseScope{Provider: "claude", CWD: "/work", SessionID: anchor}
	chain, err := resolveClaudeChain(context.Background(), reader.Locate(scope.Provider, scope.CWD, scope.SessionID), anchor)
	if err != nil {
		t.Fatal(err)
	}
	context, err := browser.acquireClaudeChainContext(context.Background(), scope, chain)
	if err != nil {
		t.Fatal(err)
	}
	defer browser.releaseClaudeChainContext(context)
	const callers = 24
	jobs := make(chan *browseJob, callers)
	pages := make(chan *BrowsePage, callers)
	var group sync.WaitGroup
	for range callers {
		group.Add(1)
		go func() {
			defer group.Done()
			job, page := browser.startClaudeChainJob(scope, context, 0)
			jobs <- job
			pages <- page
		}()
	}
	group.Wait()
	close(jobs)
	close(pages)
	var first *browseJob
	for job := range jobs {
		if job == nil {
			t.Fatal("concurrent preparation returned no job")
		}
		if first == nil {
			first = job
		} else if first != job {
			t.Fatalf("concurrent preparation returned duplicate handles %p and %p", first, job)
		}
	}
	for page := range pages {
		if page != nil {
			t.Fatalf("concurrent preparation returned an unexpected failure page: %#v", page)
		}
	}
}

func TestClaudeChainPreparationAdmissionFailureRetainsRetryIdentity(t *testing.T) {
	reader, home := testReader(t)
	anchor := testSessionID
	child := "123e4567-e89b-12d3-a456-426614174001"
	root := filepath.Join(home, ".claude", "projects", "-work")
	writeRows(t, filepath.Join(root, anchor+".jsonl"),
		map[string]any{"type": "assistant", "uuid": "a1", "message": map[string]any{"content": "parent"}},
		map[string]any{"type": "continued-in", "sessionId": anchor, "continuedInSessionId": child},
	)
	writeRows(t, filepath.Join(root, child+".jsonl"),
		map[string]any{"type": "assistant", "uuid": "b1", "message": map[string]any{"content": strings.Repeat("child ", 500)}},
	)
	options := DefaultBrowserOptions()
	options.RecentBytes = 1
	browser, err := NewBrowser(reader, t.TempDir(), options)
	if err != nil {
		t.Fatal(err)
	}
	defer browser.Close()
	scope := BrowseScope{Provider: "claude", CWD: "/work", SessionID: anchor}
	latest, err := browser.ReadPage(context.Background(), BrowseRequest{Scope: scope, Limit: 1})
	if err != nil || latest.NextCursor == "" {
		t.Fatalf("latest = %#v, err=%v", latest, err)
	}
	preparing, err := browser.ReadPage(context.Background(), BrowseRequest{Scope: scope, Cursor: latest.NextCursor, Limit: 1})
	if err != nil || preparing.State != BrowsePreparing || preparing.SnapshotID == "" {
		t.Fatalf("initial preparation = %#v, err=%v", preparing, err)
	}
	browser.cacheErr = errors.New("test storage unavailable")
	failed, err := browser.ReadPage(context.Background(), BrowseRequest{Scope: scope, Cursor: preparing.NextCursor, Limit: 1})
	if err != nil || failed.State != BrowseFailed || failed.ReasonCode != "index_storage_unavailable" ||
		failed.SnapshotID != preparing.SnapshotID || failed.NextCursor == "" || failed.Error == nil || !failed.Error.Retryable {
		t.Fatalf("admission failure lost chain identity = %#v, err=%v", failed, err)
	}
	browser.cacheErr = nil
	retry, err := browser.ReadPage(context.Background(), BrowseRequest{Scope: scope, Cursor: failed.NextCursor, Limit: 1})
	if err != nil || retry.State != BrowsePreparing || retry.SnapshotID != preparing.SnapshotID {
		t.Fatalf("retry preparation = %#v, err=%v", retry, err)
	}
	ready := waitForReadySnapshot(t, browser, scope, retry.NextCursor, 1)
	if ready.SnapshotID != preparing.SnapshotID || len(ready.Entries) != 1 || !strings.HasPrefix(ready.Entries[0].Text, "child ") {
		t.Fatalf("retry snapshot = %#v", ready)
	}
}

func TestClaudeChainConcurrentReadsShareChainMetadataSafely(t *testing.T) {
	reader, home := testReader(t)
	anchor := testSessionID
	child := "123e4567-e89b-12d3-a456-426614174001"
	root := filepath.Join(home, ".claude", "projects", "-work")
	writeRows(t, filepath.Join(root, anchor+".jsonl"),
		map[string]any{"type": "assistant", "uuid": "a1", "message": map[string]any{"content": "parent"}},
		map[string]any{"type": "continued-in", "sessionId": anchor, "continuedInSessionId": child},
	)
	writeRows(t, filepath.Join(root, child+".jsonl"),
		map[string]any{"type": "assistant", "uuid": "b1", "message": map[string]any{"content": "child"}},
	)
	options := DefaultBrowserOptions()
	options.RecentBytes = 1
	options.DefaultPageSize = 1
	browser, err := NewBrowser(reader, t.TempDir(), options)
	if err != nil {
		t.Fatal(err)
	}
	defer browser.Close()
	scope := BrowseScope{Provider: "claude", CWD: "/work", SessionID: anchor}
	latest, err := browser.ReadPage(context.Background(), BrowseRequest{Scope: scope, Limit: 1})
	if err != nil || latest.NextCursor == "" {
		t.Fatalf("initial concurrent chain page = %#v, err=%v", latest, err)
	}
	const callers = 32
	var group sync.WaitGroup
	failures := make(chan string, callers)
	for index := 0; index < callers; index++ {
		group.Add(1)
		go func(index int) {
			defer group.Done()
			request := BrowseRequest{Scope: scope, Limit: 1}
			if index%2 == 0 {
				request.Cursor = latest.NextCursor
			}
			page, readErr := browser.ReadPage(context.Background(), request)
			if readErr != nil || page.ReasonCode == "source_changed" || page.ReasonCode == "invalid_cursor" {
				failures <- fmt.Sprintf("%d: page=%#v err=%v", index, page, readErr)
			}
		}(index)
	}
	group.Wait()
	close(failures)
	for failure := range failures {
		t.Error(failure)
	}
}

func TestClaudeChainContextAdmissionIsAtomic(t *testing.T) {
	reader, _ := testReader(t)
	options := DefaultBrowserOptions()
	options.CursorTTL = time.Hour
	browser, err := NewBrowser(reader, t.TempDir(), options)
	if err != nil {
		t.Fatal(err)
	}
	defer browser.Close()
	const attempts = maxClaudeChainContexts + 32
	start := make(chan struct{})
	release := make(chan struct{})
	var group sync.WaitGroup
	var mu sync.Mutex
	acquired := 0
	failures := 0
	for index := 0; index < attempts; index++ {
		group.Add(1)
		go func(index int) {
			defer group.Done()
			<-start
			scope := BrowseScope{Provider: "claude", CWD: fmt.Sprintf("/work/%d", index), SessionID: fmt.Sprintf("123e4567-e89b-12d3-a456-%012d", index%1000000000000)}
			chain := claudeChain{Segments: []claudeSegment{{SessionID: scope.SessionID, Location: Location{Path: fmt.Sprintf("/tmp/%d.jsonl", index), Root: "/tmp"}, FileRevision: fmt.Sprintf("revision-%d", index), FileIdentity: fmt.Sprintf("identity-%d", index)}}}
			context, acquireErr := browser.acquireClaudeChainContext(context.Background(), scope, chain)
			mu.Lock()
			if acquireErr != nil {
				failures++
			} else {
				acquired++
			}
			mu.Unlock()
			if context != nil {
				<-release
				browser.releaseClaudeChainContext(context)
			}
		}(index)
	}
	close(start)
	deadline := time.After(5 * time.Second)
	for {
		mu.Lock()
		done := acquired+failures == attempts
		mu.Unlock()
		if done {
			break
		}
		select {
		case <-deadline:
			t.Fatal("chain admissions did not settle")
		default:
			time.Sleep(time.Millisecond)
		}
	}
	if acquired > maxClaudeChainContexts || len(browser.chains) > maxClaudeChainContexts {
		t.Fatalf("chain capacity exceeded: acquired=%d retained=%d", acquired, len(browser.chains))
	}
	close(release)
	group.Wait()
}
