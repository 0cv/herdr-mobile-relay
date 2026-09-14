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
	"sync/atomic"
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

func TestClaudeChainFirstContinuationPreservesSingleFileRevision(t *testing.T) {
	reader, home := testReader(t)
	anchor := testSessionID
	child := "123e4567-e89b-12d3-a456-426614174001"
	root := filepath.Join(home, ".claude", "projects", "-work")
	parentPath := filepath.Join(root, anchor+".jsonl")
	writeRows(t, parentPath,
		map[string]any{"type": "user", "uuid": "a1", "message": map[string]any{"content": "parent question"}},
		map[string]any{"type": "assistant", "uuid": "a2", "message": map[string]any{"content": "parent answer"}},
	)
	browser, err := NewBrowser(reader, t.TempDir(), DefaultBrowserOptions())
	if err != nil {
		t.Fatal(err)
	}
	defer browser.Close()
	scope := BrowseScope{Provider: "claude", CWD: "/work", SessionID: anchor}
	initial, err := browser.ReadPage(context.Background(), BrowseRequest{Scope: scope, Limit: 10})
	if err != nil || initial.ReasonCode != "" || initial.SourceRevision == "" || len(initial.Entries) != 2 {
		t.Fatalf("ordinary single-file page = %#v, err=%v", initial, err)
	}
	file, err := os.OpenFile(parentPath, os.O_WRONLY|os.O_APPEND, 0)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.NewEncoder(file).Encode(map[string]any{
		"type": "continued-in", "sessionId": anchor, "continuedInSessionId": child,
	}); err != nil {
		_ = file.Close()
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	writeRows(t, filepath.Join(root, child+".jsonl"),
		map[string]any{"type": "assistant", "uuid": "b1", "message": map[string]any{"content": "child answer"}},
	)
	continued, err := browser.ReadPage(context.Background(), BrowseRequest{Scope: scope, Limit: 10})
	if err != nil || continued.ReasonCode != "" || continued.SourceRevision != initial.SourceRevision {
		t.Fatalf("first continuation changed the public revision: initial=%q continued=%#v err=%v", initial.SourceRevision, continued, err)
	}
	if len(continued.Entries) != 1 || continued.Entries[0].Text != "child answer" {
		t.Fatalf("first continuation latest entries = %#v, want child history", continued.Entries)
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
			"message": map[string]any{"content": fmt.Sprintf("child-%03d %s", index, strings.Repeat("child ", 16000))},
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
	savedCursor := page.NextCursor
	cursor := page.NextCursor
	sawPreparation := false
	seenIDs := make(map[string]bool)
	seenChildIndexes := make(map[int]bool)
	sawParent, sawChild := false, false
	collectEntry := func(entry Entry) {
		if seenIDs[entry.ID] {
			t.Fatalf("duplicate entry ID during complete traversal: %q", entry.ID)
		}
		seenIDs[entry.ID] = true
		if entry.Text == "parent" {
			sawParent = true
		}
		if strings.HasPrefix(entry.Text, "child-") {
			var index int
			if _, scanErr := fmt.Sscanf(entry.Text, "child-%d", &index); scanErr != nil {
				t.Fatalf("child entry lost its stable ordinal: %q", entry.Text[:min(len(entry.Text), 32)])
			}
			seenChildIndexes[index] = true
			sawChild = true
		}
	}
	for _, entry := range page.Entries {
		collectEntry(entry)
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
			collectEntry(entry)
		}
		if older.NextCursor == "" {
			break
		}
		cursor = older.NextCursor
		if attempt == 499 {
			t.Fatal("large Claude chain did not finish traversal")
		}
	}
	if !sawPreparation || !sawParent || !sawChild || len(seenIDs) != 181 || len(seenChildIndexes) != 180 {
		t.Fatalf("large child traversal lost exact content: sawPreparation=%v parent=%v child=%v entries=%d childIndexes=%d", sawPreparation, sawParent, sawChild, len(seenIDs), len(seenChildIndexes))
	}
	for index := 0; index < 180; index++ {
		if !seenChildIndexes[index] {
			t.Fatalf("large child traversal omitted child index %d", index)
		}
	}
	// The cursor issued before preparation must remain a cursor into the
	// omitted prefix, not an entry-count cursor into the already-read tail.
	var postPreparationRead int64
	browser.sourceReadObserver = func(bytes int64) { postPreparationRead += bytes }
	saved, savedErr := browser.ReadPage(context.Background(), BrowseRequest{Scope: scope, Cursor: savedCursor, Limit: 4})
	if savedErr != nil {
		t.Fatal(savedErr)
	}
	if saved.State == BrowsePreparing {
		saved = waitForReadySnapshot(t, browser, scope, savedCursor, 4)
	}
	if len(saved.Entries) == 0 || saved.Entries[0].Text == "parent" || !strings.HasPrefix(saved.Entries[0].Text, "child-") {
		t.Fatalf("saved clipped cursor lost the prepared segment prefix: %#v", saved)
	}
	if postPreparationRead > 2*claudeChainForegroundValidationBytes {
		t.Fatalf("post-preparation cursor validation read %d bytes, want bounded work", postPreparationRead)
	}
	// Exhaust the same saved cursor, not only its first page. The initial
	// latest page is included so this verifies that the saved count boundary and
	// every translated index continuation together cover the exact chain once.
	browser.sourceReadObserver = nil
	savedIDs := make(map[string]bool)
	savedChildIndexes := make(map[int]bool)
	savedParent := false
	collectSaved := func(entry Entry) {
		if savedIDs[entry.ID] {
			t.Fatalf("duplicate entry ID while exhausting saved cursor: %q", entry.ID)
		}
		savedIDs[entry.ID] = true
		if entry.Text == "parent" {
			savedParent = true
			return
		}
		if strings.HasPrefix(entry.Text, "child-") {
			var index int
			if _, scanErr := fmt.Sscanf(entry.Text, "child-%d", &index); scanErr != nil {
				t.Fatalf("saved cursor lost child ordinal: %q", entry.Text[:min(len(entry.Text), 32)])
			}
			savedChildIndexes[index] = true
		}
	}
	for _, entry := range page.Entries {
		collectSaved(entry)
	}
	for _, entry := range saved.Entries {
		collectSaved(entry)
	}
	savedContinuation := saved.NextCursor
	for attempt := 0; attempt < 500 && savedContinuation != ""; attempt++ {
		older, readErr := browser.ReadPage(context.Background(), BrowseRequest{Scope: scope, Cursor: savedContinuation, Limit: 4})
		if readErr != nil {
			t.Fatal(readErr)
		}
		if older.State == BrowsePreparing {
			older = waitForReadySnapshot(t, browser, scope, savedContinuation, 4)
		}
		for _, entry := range older.Entries {
			collectSaved(entry)
		}
		savedContinuation = older.NextCursor
		if attempt == 499 && savedContinuation != "" {
			t.Fatal("saved large Claude cursor did not finish traversal")
		}
	}
	if len(savedIDs) != 181 || !savedParent || len(savedChildIndexes) != 180 {
		t.Fatalf("saved cursor traversal lost exact content: entries=%d parent=%v childIndexes=%d", len(savedIDs), savedParent, len(savedChildIndexes))
	}
	for index := 0; index < 180; index++ {
		if !savedChildIndexes[index] {
			t.Fatalf("saved cursor traversal omitted child index %d", index)
		}
	}

	latestAgain, err := browser.ReadPage(context.Background(), BrowseRequest{Scope: scope, Limit: 1})
	if err != nil || latestAgain.ReasonCode != "" || latestAgain.NextCursor == "" || len(latestAgain.Entries) != 1 || latestAgain.Mode != BrowseRecent || latestAgain.SnapshotID != "" {
		t.Fatalf("latest after preparation = %#v, err=%v", latestAgain, err)
	}
	cursor = latestAgain.NextCursor
	secondIDs := make(map[string]bool)
	secondChildIndexes := make(map[int]bool)
	secondParent := false
	collectSecond := func(entry Entry) {
		if secondIDs[entry.ID] {
			t.Fatalf("duplicate second-traversal entry ID %q", entry.ID)
		}
		secondIDs[entry.ID] = true
		if entry.Text == "parent" {
			secondParent = true
		}
		if strings.HasPrefix(entry.Text, "child-") {
			var index int
			if _, scanErr := fmt.Sscanf(entry.Text, "child-%d", &index); scanErr != nil {
				t.Fatalf("second traversal lost child ordinal: %q", entry.Text[:min(len(entry.Text), 32)])
			}
			secondChildIndexes[index] = true
		}
	}
	for _, entry := range latestAgain.Entries {
		collectSecond(entry)
	}
	for attempt := 0; attempt < 500 && cursor != ""; attempt++ {
		older, readErr := browser.ReadPage(context.Background(), BrowseRequest{Scope: scope, Cursor: cursor, Limit: 4})
		if readErr != nil {
			t.Fatal(readErr)
		}
		if older.State == BrowsePreparing {
			older = waitForReadySnapshot(t, browser, scope, cursor, 4)
		}
		for _, entry := range older.Entries {
			collectSecond(entry)
		}
		cursor = older.NextCursor
		if attempt == 499 && cursor != "" {
			t.Fatal("second large Claude chain traversal did not finish")
		}
	}
	if len(secondIDs) != 181 || !secondParent || len(secondChildIndexes) != 180 {
		t.Fatalf("second large traversal lost exact content: entries=%d parent=%v childIndexes=%d", len(secondIDs), secondParent, len(secondChildIndexes))
	}
}

func TestClaudeChainTraversesTwoClippedSegmentsWithSavedCursorExactOrder(t *testing.T) {
	reader, home := testReader(t)
	anchor := testSessionID
	middle := "123e4567-e89b-12d3-a456-426614174001"
	leaf := "123e4567-e89b-12d3-a456-426614174002"
	root := filepath.Join(home, ".claude", "projects", "-work")
	segmentRows := func(prefix string, count int) []map[string]any {
		rows := make([]map[string]any, 0, count+1)
		for index := 0; index < count; index++ {
			rows = append(rows, map[string]any{
				"type": "assistant", "uuid": fmt.Sprintf("%s-%02d", prefix, index),
				"message": map[string]any{"content": fmt.Sprintf("%s-%02d %s", prefix, index, strings.Repeat(prefix+" ", 180))},
			})
		}
		return rows
	}
	anchorRows := segmentRows("a", 28)
	anchorRows = append(anchorRows, map[string]any{"type": "continued-in", "sessionId": anchor, "continuedInSessionId": middle})
	middleRows := segmentRows("b", 28)
	middleRows = append(middleRows, map[string]any{"type": "continued-in", "sessionId": middle, "continuedInSessionId": leaf})
	writeRows(t, filepath.Join(root, anchor+".jsonl"), anchorRows...)
	writeRows(t, filepath.Join(root, middle+".jsonl"), middleRows...)
	writeRows(t, filepath.Join(root, leaf+".jsonl"), segmentRows("c", 4)...)
	options := DefaultBrowserOptions()
	options.RecentBytes = 1024
	options.DefaultPageSize = 2
	options.MaxPageSize = 4
	browser, err := NewBrowser(reader, t.TempDir(), options)
	if err != nil {
		t.Fatal(err)
	}
	defer browser.Close()
	scope := BrowseScope{Provider: "claude", CWD: "/work", SessionID: anchor}
	latest, err := browser.ReadPage(context.Background(), BrowseRequest{Scope: scope, Limit: 2})
	if err != nil || latest.ReasonCode != "" || len(latest.Entries) != 2 || latest.NextCursor == "" {
		t.Fatalf("two-clipped latest = %#v, err=%v", latest, err)
	}
	expected := make([]string, 0, 60)
	for _, prefix := range []string{"a", "b", "c"} {
		count := 28
		if prefix == "c" {
			count = 4
		}
		for index := 0; index < count; index++ {
			expected = append(expected, fmt.Sprintf("%s-%02d", prefix, index))
		}
	}
	traverse := func(savedCursor string, initial []Entry) ([]string, map[int]bool) {
		t.Helper()
		global := make([]string, 0, len(expected))
		// The initial page is the newest page, so prepend every older page to it.
		for _, entry := range initial {
			global = append(global, strings.Fields(entry.Text)[0])
		}
		cursor := savedCursor
		sawPreparation := make(map[int]bool)
		for attempt := 0; cursor != "" && attempt < 200; attempt++ {
			decoded, decodeErr := decodeBrowseCursor(browser.key, cursor, normalizeBrowseScope(scope))
			if decodeErr != nil {
				t.Fatal(decodeErr)
			}
			if decoded.ChainOffset && decoded.Segment != nil {
				sawPreparation[*decoded.Segment] = true
			}
			page, readErr := browser.ReadPage(context.Background(), BrowseRequest{Scope: scope, Cursor: cursor, Limit: 2})
			if readErr != nil {
				t.Fatal(readErr)
			}
			if page.State == BrowsePreparing {
				page = waitForReadySnapshot(t, browser, scope, cursor, 2)
			}
			if page.ReasonCode != "" {
				t.Fatalf("two-clipped traversal page = %#v", page)
			}
			labels := make([]string, 0, len(page.Entries))
			for _, entry := range page.Entries {
				words := strings.Fields(entry.Text)
				if len(words) == 0 {
					t.Fatalf("empty traversed entry = %#v", entry)
				}
				labels = append(labels, words[0])
			}
			global = append(labels, global...)
			cursor = page.NextCursor
			if attempt == 199 && cursor != "" {
				t.Fatal("two-clipped traversal did not terminate")
			}
		}
		return global, sawPreparation
	}
	global, saw := traverse(latest.NextCursor, latest.Entries)
	if !saw[0] || !saw[1] || len(global) != len(expected) {
		t.Fatalf("two-clipped traversal preparation/content = saw=%v entries=%d want=%d", saw, len(global), len(expected))
	}
	for index, want := range expected {
		if global[index] != want {
			t.Fatalf("two-clipped traversal order[%d] = %q, want %q", index, global[index], want)
		}
	}
	savedGlobal, savedSaw := traverse(latest.NextCursor, latest.Entries)
	if fmt.Sprint(savedSaw) != fmt.Sprint(saw) || fmt.Sprint(savedGlobal) != fmt.Sprint(global) {
		t.Fatalf("saved cursor traversal changed content/order: first=%v/%v saved=%v/%v", saw, global, savedSaw, savedGlobal)
	}
}

func TestClaudeChainRecentTailDoesNotSkipPrefixWhenPreparationFinishesConcurrently(t *testing.T) {
	reader, home := testReader(t)
	anchor := testSessionID
	child := "123e4567-e89b-12d3-a456-426614174001"
	root := filepath.Join(home, ".claude", "projects", "-work")
	writeRows(t, filepath.Join(root, anchor+".jsonl"),
		map[string]any{"type": "assistant", "uuid": "a1", "message": map[string]any{"content": "parent"}},
		map[string]any{"type": "continued-in", "sessionId": anchor, "continuedInSessionId": child},
	)
	childRows := make([]map[string]any, 0, 4)
	for index := 0; index < 4; index++ {
		childRows = append(childRows, map[string]any{
			"type": "assistant", "uuid": fmt.Sprintf("child-%d", index),
			"message": map[string]any{"content": fmt.Sprintf("child-%d %s", index, strings.Repeat("tail ", 20))},
		})
	}
	writeRows(t, filepath.Join(root, child+".jsonl"), childRows...)
	options := DefaultBrowserOptions()
	options.RecentBytes = 256
	options.DefaultPageSize = 1
	options.MaxPageSize = 2
	browser, err := NewBrowser(reader, t.TempDir(), options)
	if err != nil {
		t.Fatal(err)
	}
	defer browser.Close()
	scope := BrowseScope{Provider: "claude", CWD: "/work", SessionID: anchor}
	initial, err := browser.ReadPage(context.Background(), BrowseRequest{Scope: scope, Limit: 1})
	if err != nil || len(initial.Entries) != 1 || initial.Entries[0].Text == "parent" || initial.NextCursor == "" {
		t.Fatalf("initial recent page = %#v, err=%v", initial, err)
	}
	tail, err := browser.ReadPage(context.Background(), BrowseRequest{Scope: scope, Cursor: initial.NextCursor, Limit: 1})
	if err != nil || tail.NextCursor == "" {
		t.Fatalf("tail cursor = %#v, err=%v", tail, err)
	}

	readEntered := make(chan struct{})
	releaseRead := make(chan struct{})
	var readBlocked atomic.Int32
	browser.sourceReadObserver = func(int64) {
		if !readBlocked.CompareAndSwap(0, 1) {
			return
		}
		close(readEntered)
		<-releaseRead
	}
	latestDone := make(chan BrowsePage, 1)
	latestErr := make(chan error, 1)
	go func() {
		page, readErr := browser.ReadPage(context.Background(), BrowseRequest{Scope: scope, Limit: 1})
		latestDone <- page
		latestErr <- readErr
	}()
	select {
	case <-readEntered:
	case <-time.After(time.Second):
		t.Fatal("latest recent read did not pause")
	}

	preparing, err := browser.ReadPage(context.Background(), BrowseRequest{Scope: scope, Cursor: tail.NextCursor, Limit: 1})
	if err != nil || preparing.State != BrowsePreparing || preparing.NextCursor == "" {
		t.Fatalf("concurrent preparation = %#v, err=%v", preparing, err)
	}
	ready := waitForReadySnapshot(t, browser, scope, tail.NextCursor, 1)
	if ready.State != BrowseReady {
		t.Fatalf("concurrent prepared page = %#v", ready)
	}
	close(releaseRead)
	var latest BrowsePage
	select {
	case latest = <-latestDone:
	case <-time.After(time.Second):
		t.Fatal("paused latest request did not resume")
	}
	if err := <-latestErr; err != nil {
		t.Fatal(err)
	}
	if len(latest.Entries) != 1 || latest.NextCursor == "" {
		t.Fatalf("resumed latest page = %#v", latest)
	}
	decoded, err := decodeBrowseCursor(browser.key, latest.NextCursor, normalizeBrowseScope(scope))
	if err != nil {
		t.Fatal(err)
	}
	if decoded.Segment == nil || *decoded.Segment != 1 || !decoded.ChainOffset {
		t.Fatalf("resumed latest skipped the clipped child prefix: %#v", decoded)
	}
	seen := make(map[string]bool)
	collect := func(page BrowsePage) {
		for _, entry := range page.Entries {
			if seen[entry.Text] {
				t.Fatalf("resumed concurrent traversal duplicated %q", entry.Text)
			}
			seen[entry.Text] = true
		}
	}
	collect(latest)
	cursor := latest.NextCursor
	for attempt := 0; attempt < 20 && cursor != ""; attempt++ {
		page, readErr := browser.ReadPage(context.Background(), BrowseRequest{Scope: scope, Cursor: cursor, Limit: 1})
		if readErr != nil {
			t.Fatal(readErr)
		}
		if page.State == BrowsePreparing {
			page = waitForReadySnapshot(t, browser, scope, cursor, 1)
		}
		if page.ReasonCode != "" {
			t.Fatalf("resumed concurrent traversal failed: %#v", page)
		}
		collect(page)
		cursor = page.NextCursor
		if attempt == 19 && cursor != "" {
			t.Fatal("resumed concurrent traversal did not finish")
		}
	}
	if len(seen) != 5 || !seen["parent"] {
		t.Fatalf("resumed concurrent traversal lost exact history: %#v", seen)
	}
	for index := 0; index < 4; index++ {
		prefix := fmt.Sprintf("child-%d ", index)
		found := false
		for text := range seen {
			if strings.HasPrefix(text, prefix) {
				found = true
				break
			}
		}
		if !found {
			t.Fatalf("resumed concurrent traversal omitted child-%d: %#v", index, seen)
		}
	}
}

func TestClaudeChainLatestBudgetUsesCapturedEndForAPreviouslyReadSegment(t *testing.T) {
	reader, home := testReader(t)
	anchor := testSessionID
	child := "123e4567-e89b-12d3-a456-426614174001"
	root := filepath.Join(home, ".claude", "projects", "-work")
	anchorPath := filepath.Join(root, anchor+".jsonl")
	writeRows(t, anchorPath,
		map[string]any{"type": "assistant", "uuid": "a0", "message": map[string]any{"content": strings.Repeat("a0 ", 50)}},
		map[string]any{"type": "continued-in", "sessionId": anchor, "continuedInSessionId": child},
	)
	writeRows(t, filepath.Join(root, child+".jsonl"), map[string]any{
		"type": "progress", "sessionId": child, "padding": strings.Repeat("metadata ", 35),
	})
	options := DefaultBrowserOptions()
	options.RecentBytes = 512
	options.DefaultPageSize = 1
	options.MaxPageSize = 2
	browser, err := NewBrowser(reader, t.TempDir(), options)
	if err != nil {
		t.Fatal(err)
	}
	defer browser.Close()
	scope := BrowseScope{Provider: "claude", CWD: "/work", SessionID: anchor}
	first, err := browser.ReadPage(context.Background(), BrowseRequest{Scope: scope, Limit: 1})
	if err != nil || len(first.Entries) != 0 || first.NextCursor == "" {
		t.Fatalf("initial metadata-only latest = %#v, err=%v", first, err)
	}
	firstA, err := browser.ReadPage(context.Background(), BrowseRequest{Scope: scope, Cursor: first.NextCursor, Limit: 1})
	if err != nil {
		t.Fatal(err)
	}
	if firstA.State == BrowsePreparing {
		firstA = waitForReadySnapshot(t, browser, scope, first.NextCursor, 1)
	}
	if firstA.State != BrowseReady || len(firstA.Entries) != 1 || !strings.HasPrefix(firstA.Entries[0].Text, "a0") {
		t.Fatalf("first continuation page did not read A = %#v", firstA)
	}
	latest, err := browser.ReadPage(context.Background(), BrowseRequest{Scope: scope, Limit: 1})
	if err != nil || len(latest.Entries) != 0 || latest.NextCursor == "" {
		t.Fatalf("budgeted latest after child appeared = %#v, err=%v", latest, err)
	}
	decoded, err := decodeBrowseCursor(browser.key, latest.NextCursor, normalizeBrowseScope(scope))
	if err != nil {
		t.Fatal(err)
	}
	boundary, err := parseBrowseOffset(decoded.Boundary)
	if err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(anchorPath)
	if err != nil {
		t.Fatal(err)
	}
	if decoded.Segment == nil || *decoded.Segment != 0 || !decoded.ChainOffset || boundary != info.Size() {
		t.Fatalf("budget cursor did not preserve captured end: %#v boundary=%d size=%d", decoded, boundary, info.Size())
	}
	prepared, err := browser.ReadPage(context.Background(), BrowseRequest{Scope: scope, Cursor: latest.NextCursor, Limit: 1})
	if err != nil {
		t.Fatal(err)
	}
	if prepared.State == BrowsePreparing {
		prepared = waitForReadySnapshot(t, browser, scope, latest.NextCursor, 1)
	}
	if prepared.State != BrowseReady || len(prepared.Entries) != 1 || !strings.HasPrefix(prepared.Entries[0].Text, "a0") {
		t.Fatalf("prepared captured-end page = %#v", prepared)
	}
}

func TestClaudeChainMediumRecentCursorDoesNotPollForever(t *testing.T) {
	reader, home := testReader(t)
	anchor := testSessionID
	child := "123e4567-e89b-12d3-a456-426614174001"
	root := filepath.Join(home, ".claude", "projects", "-work")
	writeRows(t, filepath.Join(root, anchor+".jsonl"),
		map[string]any{"type": "assistant", "uuid": "a0", "message": map[string]any{"content": "parent answer"}},
		map[string]any{"type": "continued-in", "sessionId": anchor, "continuedInSessionId": child},
	)
	childRows := make([]map[string]any, 0, 10)
	for index := 0; index < 10; index++ {
		childRows = append(childRows, map[string]any{
			"type": "assistant", "uuid": fmt.Sprintf("medium-%02d", index),
			"message": map[string]any{"content": fmt.Sprintf("medium-%02d %s", index, strings.Repeat("payload ", 1200))},
		})
	}
	childPath := filepath.Join(root, child+".jsonl")
	writeRows(t, childPath, childRows...)
	info, err := os.Stat(childPath)
	if err != nil {
		t.Fatal(err)
	}
	if info.Size() <= claudeContinuationFooterBytes || info.Size() > claudeChainForegroundValidationBytes {
		t.Fatalf("medium fixture size = %d, want (%d, %d]", info.Size(), claudeContinuationFooterBytes, claudeChainForegroundValidationBytes)
	}
	options := DefaultBrowserOptions()
	options.DefaultPageSize = 1
	options.MaxPageSize = 2
	browser, err := NewBrowser(reader, t.TempDir(), options)
	if err != nil {
		t.Fatal(err)
	}
	defer browser.Close()
	scope := BrowseScope{Provider: "claude", CWD: "/work", SessionID: anchor}
	latest, err := browser.ReadPage(context.Background(), BrowseRequest{Scope: scope, Limit: 1})
	if err != nil || latest.ReasonCode != "" || len(latest.Entries) != 1 || !strings.HasPrefix(latest.Entries[0].Text, "medium-09") || latest.NextCursor == "" {
		t.Fatalf("medium latest = %#v, err=%v", latest, err)
	}
	seen := map[string]bool{latest.Entries[0].Text: true}
	cursor := latest.NextCursor
	for attempt := 0; cursor != "" && attempt < 20; attempt++ {
		page, readErr := browser.ReadPage(context.Background(), BrowseRequest{Scope: scope, Cursor: cursor, Limit: 1})
		if readErr != nil {
			t.Fatal(readErr)
		}
		if page.State == BrowsePreparing {
			t.Fatalf("medium cursor remained preparing without an owner: %#v", page)
		}
		if page.ReasonCode != "" || len(page.Entries) != 1 {
			t.Fatalf("medium cursor page = %#v", page)
		}
		if seen[page.Entries[0].Text] {
			t.Fatalf("medium cursor duplicated %q", page.Entries[0].Text)
		}
		seen[page.Entries[0].Text] = true
		cursor = page.NextCursor
		if attempt == 19 && cursor != "" {
			t.Fatal("medium cursor traversal did not terminate")
		}
	}
	if len(seen) != 11 || !seen["parent answer"] {
		t.Fatalf("medium cursor traversal lost readable history: %#v", seen)
	}
}

func TestClaudeChainPreparedBoundaryPreservesBudgetAndPhysicalReadAccounting(t *testing.T) {
	reader, home := testReader(t)
	anchor := testSessionID
	middle := "123e4567-e89b-12d3-a456-426614174001"
	leaf := "123e4567-e89b-12d3-a456-426614174002"
	root := filepath.Join(home, ".claude", "projects", "-work")
	writeRows(t, filepath.Join(root, anchor+".jsonl"),
		map[string]any{"type": "assistant", "uuid": "a1", "message": map[string]any{"content": strings.Repeat("ancestor ", 35)}},
		map[string]any{"type": "continued-in", "sessionId": anchor, "continuedInSessionId": middle},
	)
	writeRows(t, filepath.Join(root, middle+".jsonl"),
		map[string]any{"type": "progress", "sessionId": middle, "padding": strings.Repeat("metadata ", 18)},
		map[string]any{"type": "continued-in", "sessionId": middle, "continuedInSessionId": leaf},
	)
	writeRows(t, filepath.Join(root, leaf+".jsonl"),
		map[string]any{"type": "assistant", "uuid": "c0", "message": map[string]any{"content": "older leaf answer"}},
		map[string]any{"type": "progress", "sessionId": leaf, "padding": strings.Repeat("large ", 140)},
		map[string]any{"type": "progress", "sessionId": leaf, "padding": strings.Repeat("large ", 140)},
		map[string]any{"type": "assistant", "uuid": "c1", "message": map[string]any{"content": "leaf answer"}},
	)
	options := DefaultBrowserOptions()
	options.RecentBytes = 512
	options.DefaultPageSize = 1
	options.MaxPageSize = 2
	browser, err := NewBrowser(reader, t.TempDir(), options)
	if err != nil {
		t.Fatal(err)
	}
	defer browser.Close()
	scope := BrowseScope{Provider: "claude", CWD: "/work", SessionID: anchor}
	var discoveryReads int64
	var validationReads int64
	browser.discoveryReadObserver = func(bytes int64) { atomic.AddInt64(&discoveryReads, bytes) }
	browser.validationReadObserver = func(bytes int64) { atomic.AddInt64(&validationReads, bytes) }
	latest, err := browser.ReadPage(context.Background(), BrowseRequest{Scope: scope, Limit: 1})
	if err != nil || len(latest.Entries) != 1 || latest.Entries[0].Text != "leaf answer" || latest.NextCursor == "" {
		t.Fatalf("latest leaf page = %#v, err=%v", latest, err)
	}
	var recentReads int64
	var physicalReads int64
	browser.recentReadObserver = func(bytes int64) { recentReads += bytes }
	browser.physicalReadObserver = func(bytes int64) { atomic.AddInt64(&physicalReads, bytes) }
	preparing, err := browser.ReadPage(context.Background(), BrowseRequest{Scope: scope, Cursor: latest.NextCursor, Limit: 1})
	if err != nil || preparing.State != BrowsePreparing || preparing.NextCursor == "" {
		t.Fatalf("leaf preparation = %#v, err=%v", preparing, err)
	}
	ready := waitForReadySnapshot(t, browser, scope, preparing.NextCursor, 1)
	if ready.State != BrowseReady || ready.ReasonCode != "" || ready.Error != nil || len(ready.Entries) != 1 || ready.Entries[0].Text != "older leaf answer" {
		t.Fatalf("prepared leaf page reported a false source failure = %#v", ready)
	}
	if !ready.HasMore || ready.NextCursor == "" {
		t.Fatalf("prepared budget exhaustion lost its deferred continuation = %#v", ready)
	}
	if recentReads > options.RecentBytes {
		t.Fatalf("prepared boundary reread %d recent bytes, want at most %d", recentReads, options.RecentBytes)
	}
	if got := atomic.LoadInt64(&physicalReads); got > options.RecentBytes+int64(claudeContinuationMaxSegments) {
		t.Fatalf("prepared boundary performed %d physical recent reads, want at most %d", got, options.RecentBytes)
	}
	if got := atomic.LoadInt64(&discoveryReads); got == 0 || got > claudeContinuationFooterBudget+int64(claudeContinuationMaxSegments)*(claudeContinuationFooterBytes+1) {
		t.Fatalf("discovery bytes = %d, want a positive bounded read", got)
	}
	if got := atomic.LoadInt64(&validationReads); got == 0 || got > claudeChainForegroundPhysicalBytes {
		t.Fatalf("validation bytes = %d, want a positive bounded foreground read", got)
	}
	decoded, err := decodeBrowseCursor(browser.key, ready.NextCursor, normalizeBrowseScope(scope))
	if err != nil {
		t.Fatal(err)
	}
	if decoded.Segment == nil || *decoded.Segment != 0 || !decoded.ChainOffset {
		t.Fatalf("budget continuation = %#v, want ancestor byte cursor", decoded)
	}
	boundary, err := parseBrowseOffset(decoded.Boundary)
	if err != nil {
		t.Fatal(err)
	}
	ancestorInfo, err := os.Stat(filepath.Join(root, anchor+".jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	if boundary != ancestorInfo.Size() {
		t.Fatalf("budget continuation boundary = %d, want ancestor captured end %d", boundary, ancestorInfo.Size())
	}
	ancestor, err := browser.ReadPage(context.Background(), BrowseRequest{Scope: scope, Cursor: ready.NextCursor, Limit: 1})
	if err != nil {
		t.Fatal(err)
	}
	if ancestor.State == BrowsePreparing {
		ancestor = waitForReadySnapshot(t, browser, scope, ready.NextCursor, 1)
	}
	if ancestor.State != BrowseReady || len(ancestor.Entries) != 1 || !strings.HasPrefix(ancestor.Entries[0].Text, "ancestor ") {
		t.Fatalf("budget continuation could not exhaust into ancestor: %#v", ancestor)
	}
	middleInfo, err := os.Stat(filepath.Join(root, middle+".jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	if ancestorInfo.Size()+middleInfo.Size() <= options.RecentBytes {
		t.Fatalf("fixture did not require a shared budget: A=%d B=%d budget=%d", ancestorInfo.Size(), middleInfo.Size(), options.RecentBytes)
	}
}

func TestClaudeChainPreparedPagingDoesNotRehashTheCapturedTranscript(t *testing.T) {
	reader, home := testReader(t)
	anchor := testSessionID
	child := "123e4567-e89b-12d3-a456-426614174001"
	root := filepath.Join(home, ".claude", "projects", "-work")
	writeRows(t, filepath.Join(root, anchor+".jsonl"),
		map[string]any{"type": "assistant", "uuid": "a1", "message": map[string]any{"content": "parent"}},
		map[string]any{"type": "continued-in", "sessionId": anchor, "continuedInSessionId": child},
	)
	rows := make([]map[string]any, 0, 10)
	for index := 0; index < 10; index++ {
		rows = append(rows, map[string]any{
			"type": "assistant", "uuid": fmt.Sprintf("child-%d", index),
			"message": map[string]any{"content": fmt.Sprintf("child-%d %s", index, strings.Repeat("payload ", 5000))},
		})
	}
	childPath := filepath.Join(root, child+".jsonl")
	writeRows(t, childPath, rows...)
	childInfo, err := os.Stat(childPath)
	if err != nil {
		t.Fatal(err)
	}
	if childInfo.Size() <= claudeChainForegroundValidationBytes {
		t.Fatalf("prepared fixture is too small for a bounded-read regression: %d bytes", childInfo.Size())
	}
	options := DefaultBrowserOptions()
	options.RecentBytes = 64 * 1024
	options.DefaultPageSize = 1
	options.MaxPageSize = 2
	browser, err := NewBrowser(reader, t.TempDir(), options)
	if err != nil {
		t.Fatal(err)
	}
	defer browser.Close()
	scope := BrowseScope{Provider: "claude", CWD: "/work", SessionID: anchor}
	latest, err := browser.ReadPage(context.Background(), BrowseRequest{Scope: scope, Limit: 1})
	if err != nil || len(latest.Entries) != 1 || latest.Entries[0].Text == "parent" || latest.NextCursor == "" {
		t.Fatalf("large prepared latest = %#v, err=%v", latest, err)
	}
	preparing, err := browser.ReadPage(context.Background(), BrowseRequest{Scope: scope, Cursor: latest.NextCursor, Limit: 1})
	if err != nil || preparing.State != BrowsePreparing || preparing.NextCursor == "" {
		t.Fatalf("large prepared status = %#v, err=%v", preparing, err)
	}
	ready := waitForReadySnapshot(t, browser, scope, preparing.NextCursor, 1)
	if len(ready.Entries) != 1 || ready.Entries[0].Text == "parent" {
		t.Fatalf("large prepared first page = %#v", ready)
	}
	var validationReads int64
	var recentReads int64
	browser.validationReadObserver = func(bytes int64) { atomic.AddInt64(&validationReads, bytes) }
	browser.recentReadObserver = func(bytes int64) { atomic.AddInt64(&recentReads, bytes) }
	if ready.NextCursor != "" {
		page, readErr := browser.ReadPage(context.Background(), BrowseRequest{Scope: scope, Cursor: ready.NextCursor, Limit: 1})
		if readErr != nil || page.ReasonCode != "" {
			t.Fatalf("prepared paging after worker completion = %#v, err=%v", page, readErr)
		}
	}
	latestAgain, err := browser.ReadPage(context.Background(), BrowseRequest{Scope: scope, Limit: 1})
	if err != nil || latestAgain.ReasonCode != "" || latestAgain.Mode != BrowseRecent || latestAgain.SnapshotID != "" || len(latestAgain.Entries) != 1 {
		t.Fatalf("latest after prepared paging = %#v, err=%v", latestAgain, err)
	}
	if got := atomic.LoadInt64(&validationReads); got >= childInfo.Size() || got > claudeChainForegroundPhysicalBytes {
		t.Fatalf("post-preparation validation reread %d bytes of %d-byte transcript", got, childInfo.Size())
	}
	if got := atomic.LoadInt64(&recentReads); got > options.RecentBytes {
		t.Fatalf("post-preparation recent reads = %d, want at most %d", got, options.RecentBytes)
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
	file, err = os.OpenFile(childPath, os.O_WRONLY|os.O_APPEND, 0)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.NewEncoder(file).Encode(map[string]any{
		"type": "assistant", "uuid": "b4", "message": map[string]any{"content": "fourth"},
	}); err != nil {
		_ = file.Close()
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	latestAgain, err := browser.ReadPage(context.Background(), BrowseRequest{Scope: scope, Limit: 1})
	if err != nil || len(latestAgain.Entries) != 1 || latestAgain.Entries[0].Text != "fourth" {
		t.Fatalf("second appended latest page = %#v, err=%v", latestAgain, err)
	}
	olderAgain, err := browser.ReadPage(context.Background(), BrowseRequest{Scope: scope, Cursor: initial.NextCursor, Limit: 1})
	if err != nil || olderAgain.ReasonCode != "" || len(olderAgain.Entries) != 1 || olderAgain.Entries[0].Text != "first" {
		t.Fatalf("old cursor was retargeted after interleaved append = %#v, err=%v", olderAgain, err)
	}
	// Reading the frozen cursor after the second append must not overwrite the
	// newer lineage evidence. A subsequent latest discovery still accepts the
	// append and keeps the original public identity.
	postCursorLatest, err := browser.ReadPage(context.Background(), BrowseRequest{Scope: scope, Limit: 1})
	if err != nil || postCursorLatest.ReasonCode != "" || len(postCursorLatest.Entries) != 1 || postCursorLatest.Entries[0].Text != "fourth" || postCursorLatest.SourceRevision != initial.SourceRevision {
		t.Fatalf("latest after an interleaved old-cursor read = %#v, initial revision=%q, err=%v", postCursorLatest, initial.SourceRevision, err)
	}
}

func TestClaudeChainAppendToNewContinuationKeepsOldCursorOnSavedSegment(t *testing.T) {
	reader, home := testReader(t)
	anchor := testSessionID
	child := "123e4567-e89b-12d3-a456-426614174001"
	grandchild := "123e4567-e89b-12d3-a456-426614174002"
	root := filepath.Join(home, ".claude", "projects", "-work")
	writeRows(t, filepath.Join(root, anchor+".jsonl"),
		map[string]any{"type": "assistant", "uuid": "a1", "message": map[string]any{"content": "parent"}},
		map[string]any{"type": "continued-in", "sessionId": anchor, "continuedInSessionId": child},
	)
	childPath := filepath.Join(root, child+".jsonl")
	writeRows(t, childPath,
		map[string]any{"type": "assistant", "uuid": "b1", "message": map[string]any{"content": "child before C"}},
		map[string]any{"type": "assistant", "uuid": "b2", "message": map[string]any{"content": "child latest before C"}},
	)
	browser, err := NewBrowser(reader, t.TempDir(), DefaultBrowserOptions())
	if err != nil {
		t.Fatal(err)
	}
	defer browser.Close()
	scope := BrowseScope{Provider: "claude", CWD: "/work", SessionID: anchor}
	beforeC, err := browser.ReadPage(context.Background(), BrowseRequest{Scope: scope, Limit: 1})
	if err != nil || len(beforeC.Entries) != 1 || beforeC.Entries[0].Text != "child latest before C" || beforeC.NextCursor == "" {
		t.Fatalf("before-C latest = %#v, err=%v", beforeC, err)
	}
	file, err := os.OpenFile(childPath, os.O_WRONLY|os.O_APPEND, 0)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.NewEncoder(file).Encode(map[string]any{"type": "continued-in", "sessionId": child, "continuedInSessionId": grandchild}); err != nil {
		_ = file.Close()
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	writeRows(t, filepath.Join(root, grandchild+".jsonl"),
		map[string]any{"type": "assistant", "uuid": "c1", "message": map[string]any{"content": "new C answer"}},
	)
	latestC, err := browser.ReadPage(context.Background(), BrowseRequest{Scope: scope, Limit: 1})
	if err != nil || len(latestC.Entries) != 1 || latestC.Entries[0].Text != "new C answer" {
		t.Fatalf("append-to-C latest = %#v, err=%v", latestC, err)
	}
	oldCursor, err := browser.ReadPage(context.Background(), BrowseRequest{Scope: scope, Cursor: beforeC.NextCursor, Limit: 1})
	if err != nil || oldCursor.ReasonCode != "" || len(oldCursor.Entries) != 1 || oldCursor.Entries[0].Text != "child before C" {
		t.Fatalf("old cursor retargeted after C append = %#v, err=%v", oldCursor, err)
	}
}

func TestClaudeChainSavedPreparationUsesCapturedEndAfterAppend(t *testing.T) {
	reader, home := testReader(t)
	anchor := testSessionID
	child := "123e4567-e89b-12d3-a456-426614174001"
	root := filepath.Join(home, ".claude", "projects", "-work")
	writeRows(t, filepath.Join(root, anchor+".jsonl"),
		map[string]any{"type": "assistant", "uuid": "a1", "message": map[string]any{"content": "parent"}},
		map[string]any{"type": "continued-in", "sessionId": anchor, "continuedInSessionId": child},
	)
	childPath := filepath.Join(root, child+".jsonl")
	rows := make([]map[string]any, 0, 180)
	for index := 0; index < 180; index++ {
		rows = append(rows, map[string]any{
			"type": "assistant", "uuid": fmt.Sprintf("saved-%04d", index),
			"message": map[string]any{"content": strings.Repeat("saved payload ", 700) + fmt.Sprintf("saved-%04d", index)},
		})
	}
	writeRows(t, childPath, rows...)
	options := DefaultBrowserOptions()
	options.RecentBytes = 256 * 1024
	options.DefaultPageSize = 1
	options.MaxPageSize = 4
	browser, err := NewBrowser(reader, t.TempDir(), options)
	if err != nil {
		t.Fatal(err)
	}
	defer browser.Close()
	scope := BrowseScope{Provider: "claude", CWD: "/work", SessionID: anchor}
	initial, err := browser.ReadPage(context.Background(), BrowseRequest{Scope: scope, Limit: 1})
	if err != nil || initial.ReasonCode != "" || len(initial.Entries) != 1 || initial.NextCursor == "" {
		t.Fatalf("initial clipped page = %#v, err=%v", initial, err)
	}
	savedCursor := initial.NextCursor
	appendFile, err := os.OpenFile(childPath, os.O_WRONLY|os.O_APPEND, 0)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.NewEncoder(appendFile).Encode(map[string]any{
		"type": "assistant", "uuid": "newest-append", "message": map[string]any{"content": "newest append"},
	}); err != nil {
		_ = appendFile.Close()
		t.Fatal(err)
	}
	if err := appendFile.Close(); err != nil {
		t.Fatal(err)
	}
	var refreshed BrowsePage
	for attempt := 0; attempt < 4; attempt++ {
		refreshed, err = browser.ReadPage(context.Background(), BrowseRequest{Scope: scope, Limit: 1})
		if err != nil {
			t.Fatal(err)
		}
		if refreshed.ReasonCode != "validation_pending" {
			break
		}
		browser.validationWG.Wait()
	}
	if refreshed.ReasonCode != "" || len(refreshed.Entries) != 1 || refreshed.Entries[0].Text != "newest append" || refreshed.SourceRevision != initial.SourceRevision {
		t.Fatalf("latest append refresh = %#v, initial revision=%q", refreshed, initial.SourceRevision)
	}

	preparing, err := browser.ReadPage(context.Background(), BrowseRequest{Scope: scope, Cursor: savedCursor, Limit: 1})
	if err != nil || preparing.State != BrowsePreparing || preparing.NextCursor == "" {
		t.Fatalf("saved cursor preparation after append = %#v, err=%v", preparing, err)
	}
	ready := waitForReadySnapshot(t, browser, scope, savedCursor, 1)
	if ready.State != BrowseReady || ready.ReasonCode != "" || len(ready.Entries) != 1 || ready.SnapshotID == "" {
		t.Fatalf("saved cursor did not prepare its captured prefix = %#v", ready)
	}
	seenAppended := false
	cursor := ready.NextCursor
	for _, entry := range ready.Entries {
		seenAppended = seenAppended || entry.Text == "newest append"
	}
	for attempt := 0; cursor != "" && attempt < 300; attempt++ {
		page, readErr := browser.ReadPage(context.Background(), BrowseRequest{Scope: scope, Cursor: cursor, Limit: 1})
		if readErr != nil {
			t.Fatal(readErr)
		}
		if page.State == BrowsePreparing {
			page = waitForReadySnapshot(t, browser, scope, cursor, 1)
		}
		if page.ReasonCode != "" {
			t.Fatalf("saved cursor traversal after append = %#v", page)
		}
		for _, entry := range page.Entries {
			seenAppended = seenAppended || entry.Text == "newest append"
		}
		cursor = page.NextCursor
		if attempt == 299 && cursor != "" {
			t.Fatal("saved cursor after append did not terminate")
		}
	}
	if seenAppended {
		t.Fatal("saved cursor leaked the row appended after its captured end")
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
	if err != nil || recovered.ReasonCode != "" || len(recovered.Entries) != 1 || recovered.Entries[0].Text != "appended after replacement" || recovered.SourceRevision == first.SourceRevision {
		t.Fatalf("replacement lineage did not recover with a new public identity = %#v initial=%q, err=%v", recovered, first.SourceRevision, err)
	}
}

func TestClaudeChainRetainsSelectedRangeEvidenceAcrossAppend(t *testing.T) {
	reader, home := testReader(t)
	anchor := testSessionID
	child := "123e4567-e89b-12d3-a456-426614174001"
	root := filepath.Join(home, ".claude", "projects", "-work")
	writeRows(t, filepath.Join(root, anchor+".jsonl"),
		map[string]any{"type": "assistant", "uuid": "a1", "message": map[string]any{"content": "parent"}},
		map[string]any{"type": "continued-in", "sessionId": anchor, "continuedInSessionId": child},
	)
	childPath := filepath.Join(root, child+".jsonl")
	rows := make([]map[string]any, 0, 150)
	for index := 0; index < 150; index++ {
		rows = append(rows, map[string]any{
			"type": "assistant", "uuid": fmt.Sprintf("b-%04d", index),
			"message": map[string]any{"content": strings.Repeat("payload ", 700) + fmt.Sprintf("original-%04d", index)},
		})
	}
	writeRows(t, childPath, rows...)
	options := DefaultBrowserOptions()
	options.RecentBytes = 256 * 1024
	options.DefaultPageSize = 1
	browser, err := NewBrowser(reader, t.TempDir(), options)
	if err != nil {
		t.Fatal(err)
	}
	defer browser.Close()
	scope := BrowseScope{Provider: "claude", CWD: "/work", SessionID: anchor}
	first, err := browser.ReadPage(context.Background(), BrowseRequest{Scope: scope, Limit: 1})
	if err != nil || len(first.Entries) != 1 || first.Entries[0].Text == "parent" {
		t.Fatalf("initial selected range page = %#v, err=%v", first, err)
	}
	browser.mu.Lock()
	lineage := browser.chainLineage[browseScopeID(normalizeBrowseScope(scope))]
	lineageSegments := cloneClaudeSegments(lineage.segments)
	browser.mu.Unlock()
	if len(lineageSegments) != 2 || lineageSegments[1].RecentDigest == "" || len(lineageSegments[1].ObservedRanges) == 0 {
		t.Fatalf("selected range evidence was not initialized in lineage: %#v", lineageSegments)
	}
	if got := lineageSegments[1].RecentEnd - lineageSegments[1].RecentStart; got <= claudeChainForegroundValidationBytes {
		t.Fatalf("fixture did not establish a large selected range: %d bytes", got)
	}
	original, err := os.ReadFile(childPath)
	if err != nil {
		t.Fatal(err)
	}
	oldEnd := len(original)
	low, high := oldEnd-int(options.RecentBytes), oldEnd-int(claudeContinuationFooterBytes)
	marker := ""
	for index := 0; index < 150; index++ {
		candidate := fmt.Sprintf("original-%04d", index)
		position := strings.Index(string(original), candidate)
		if position >= low && position < high {
			marker = candidate
			break
		}
	}
	if marker == "" {
		t.Fatalf("fixture did not place a rewrite in the selected non-footer range: size=%d", oldEnd)
	}
	rewritten := strings.Replace(string(original), marker, strings.Replace(marker, "original", "rewriten", 1), 1)
	if len(rewritten) != len(original) {
		t.Fatal("selected-range rewrite changed the file length")
	}
	if err := os.WriteFile(childPath, []byte(rewritten), 0o600); err != nil {
		t.Fatal(err)
	}
	file, err := os.OpenFile(childPath, os.O_WRONLY|os.O_APPEND, 0)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.NewEncoder(file).Encode(map[string]any{
		"type": "assistant", "uuid": "after-rewrite", "message": map[string]any{"content": "append after selected-range rewrite"},
	}); err != nil {
		_ = file.Close()
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	changed, err := browser.ReadPage(context.Background(), BrowseRequest{Scope: scope, Limit: 1})
	if err != nil {
		t.Fatal(err)
	}
	if changed.ReasonCode == "validation_pending" {
		browser.validationWG.Wait()
		changed, err = browser.ReadPage(context.Background(), BrowseRequest{Scope: scope, Limit: 1})
		if err != nil {
			t.Fatal(err)
		}
	}
	if changed.ReasonCode != "source_changed" {
		t.Fatalf("selected-range rewrite+append was accepted: %#v, err=%v", changed, err)
	}
	recovered, err := browser.ReadPage(context.Background(), BrowseRequest{Scope: scope, Limit: 1})
	if err != nil || recovered.ReasonCode != "" || len(recovered.Entries) != 1 || recovered.Entries[0].Text != "append after selected-range rewrite" {
		t.Fatalf("latest did not recover after selected-range rejection: %#v, err=%v", recovered, err)
	}
}

func TestClaudeChainCancellationPreservesClaudeLineage(t *testing.T) {
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
	if err != nil || initial.ReasonCode != "" || len(initial.Entries) != 1 || initial.NextCursor == "" {
		t.Fatalf("initial lineage page = %#v, err=%v", initial, err)
	}
	scopeID := browseScopeID(normalizeBrowseScope(scope))
	browser.mu.Lock()
	beforeIdentity := browser.chainLineageIdentity[scopeID].id
	browser.mu.Unlock()
	file, err := os.OpenFile(childPath, os.O_WRONLY|os.O_APPEND, 0)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.NewEncoder(file).Encode(map[string]any{"type": "assistant", "uuid": "b3", "message": map[string]any{"content": "third"}}); err != nil {
		_ = file.Close()
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	entered := make(chan struct{})
	release := make(chan struct{})
	var enteredOnce atomic.Bool
	browser.sourceReadObserver = func(int64) {
		if !enteredOnce.CompareAndSwap(false, true) {
			return
		}
		close(entered)
		<-release
	}
	requestCtx, cancel := context.WithCancel(context.Background())
	result := make(chan BrowsePage, 1)
	go func() {
		page, readErr := browser.ReadPage(requestCtx, BrowseRequest{Scope: scope, Limit: 1})
		if readErr != nil {
			page.Reason = readErr.Error()
		}
		result <- page
	}()
	select {
	case <-entered:
	case <-time.After(time.Second):
		close(release)
		t.Fatal("Claude lineage validation did not reach the source-read barrier")
	}
	cancel()
	close(release)
	var cancelled BrowsePage
	select {
	case cancelled = <-result:
	case <-time.After(time.Second):
		t.Fatal("cancelled Claude lineage request did not return")
	}
	browser.sourceReadObserver = nil
	if cancelled.ReasonCode != "request_cancelled" {
		t.Fatalf("cancelled lineage request = %#v, want request_cancelled", cancelled)
	}
	browser.mu.Lock()
	afterIdentity := browser.chainLineageIdentity[scopeID].id
	browser.mu.Unlock()
	if afterIdentity != beforeIdentity {
		t.Fatalf("cancelled validation changed lineage identity: before=%q after=%q", beforeIdentity, afterIdentity)
	}
	oldCursor, err := browser.ReadPage(context.Background(), BrowseRequest{Scope: scope, Cursor: initial.NextCursor, Limit: 1})
	if err != nil || oldCursor.ReasonCode != "" || len(oldCursor.Entries) != 1 {
		t.Fatalf("saved cursor unusable after cancellation: %#v, err=%v", oldCursor, err)
	}
}

func TestClaudeChainEvidenceCompactionBoundsRetentionAndRejectsRewrite(t *testing.T) {
	reader, home := testReader(t)
	root := filepath.Join(home, ".claude", "projects", "-work")
	path := filepath.Join(root, testSessionID+".jsonl")
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatal(err)
	}
	content := "stable-first-record\n" + strings.Repeat("a", 128) + "\n"
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	location := Location{Root: root, Path: path}
	source, err := captureFileSource(location)
	if err != nil {
		t.Fatal(err)
	}
	segment := claudeSegment{
		SessionID: testSessionID, Location: location, CapturedEnd: source.end,
		FileRevision: source.revision, SourceModTime: source.info.ModTime().UnixNano(),
		FileIdentity: fileIdentity(source.info),
	}
	for index := int64(0); index < maxClaudeChainEvidenceRanges+2; index++ {
		start := int64(len("stable-first-record\n")) + index*4
		end := start + 3
		digest, digestErr := fileRangeDigest(context.Background(), source.file, start, end)
		if digestErr != nil {
			source.close()
			t.Fatal(digestErr)
		}
		segment.ObservedRanges = append(segment.ObservedRanges, claudeRangeEvidence{Start: start, End: end, Digest: digest})
	}
	source.close()
	browser, err := NewBrowser(reader, t.TempDir(), DefaultBrowserOptions())
	if err != nil {
		t.Fatal(err)
	}
	defer browser.Close()
	var compactionReads atomic.Int64
	browser.validationReadObserver = func(bytes int64) { compactionReads.Add(bytes) }
	appendResult := make(chan error, 1)
	var appendOnce atomic.Bool
	browser.chainCompactionObserver = func() {
		if !appendOnce.CompareAndSwap(false, true) {
			return
		}
		file, openErr := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0o600)
		if openErr == nil {
			_, openErr = file.WriteString("tail appended during compaction\n")
			if closeErr := file.Close(); openErr == nil {
				openErr = closeErr
			}
		}
		appendResult <- openErr
	}
	scopeID := "claude|/work|" + testSessionID
	browser.mu.Lock()
	browser.recordClaudeChainLineageLocked(scopeID, []claudeSegment{segment})
	browser.mu.Unlock()
	browser.validationWG.Wait()
	if err := <-appendResult; err != nil {
		t.Fatal(err)
	}
	if got, want := compactionReads.Load(), int64(4*segment.CapturedEnd+64*1024); got > want {
		t.Fatalf("compaction physical validation read %d bytes, want at most %d", got, want)
	}
	browser.chainCompactionObserver = nil
	browser.mu.Lock()
	lineage := browser.chainLineage[scopeID]
	if len(lineage.segments) != 1 {
		browser.mu.Unlock()
		t.Fatalf("compacted lineage segments = %#v", lineage.segments)
	}
	compacted := lineage.segments[0]
	browser.mu.Unlock()
	if compacted.EvidenceCompactionPending || compacted.EvidenceCompactionError != "" || len(compacted.ObservedRanges) != 1 {
		t.Fatalf("evidence compaction result = %#v", compacted)
	}
	if got := len(claudeChainEvidence(compacted)); got > maxClaudeChainEvidenceRanges {
		t.Fatalf("compacted evidence retained %d ranges, want at most %d", got, maxClaudeChainEvidenceRanges)
	}

	current, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	marker := strings.Index(string(current), "aaa")
	if marker < len("stable-first-record\n") {
		t.Fatal("compaction fixture did not contain a rewrite range")
	}
	current[marker] = 'b'
	if err := os.WriteFile(path, current, 0o600); err != nil {
		t.Fatal(err)
	}
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := file.WriteString("tail\n"); err != nil {
		_ = file.Close()
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	latest, err := captureFileSource(location)
	if err != nil {
		t.Fatal(err)
	}
	candidate := claudeSegment{
		SessionID: testSessionID, Location: location, CapturedEnd: latest.end,
		FileRevision: latest.revision, SourceModTime: latest.info.ModTime().UnixNano(),
		FileIdentity: fileIdentity(latest.info),
	}
	latest.close()
	if err := browser.claudeChainObservedRangesMatch(context.Background(), compacted, candidate); !errors.Is(err, errClaudeChainLineageChanged) {
		t.Fatalf("compacted evidence accepted rewrite: %v", err)
	}
}

func TestClaudeChainEvidenceCompactionRejectsRewriteAndAppendBetweenPhases(t *testing.T) {
	reader, home := testReader(t)
	root := filepath.Join(home, ".claude", "projects", "-work")
	path := filepath.Join(root, testSessionID+".jsonl")
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatal(err)
	}
	content := "stable-first-record\n" + strings.Repeat("a", 128) + "\n"
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	location := Location{Root: root, Path: path}
	source, err := captureFileSource(location)
	if err != nil {
		t.Fatal(err)
	}
	segment := claudeSegment{
		SessionID: testSessionID, Location: location, CapturedEnd: source.end,
		FileRevision: source.revision, SourceModTime: source.info.ModTime().UnixNano(),
		FileIdentity: fileIdentity(source.info),
	}
	markerStart := int64(len("stable-first-record\n"))
	for index := int64(0); index < maxClaudeChainEvidenceRanges+2; index++ {
		start := markerStart + index*4
		digest, digestErr := fileRangeDigest(context.Background(), source.file, start, start+3)
		if digestErr != nil {
			source.close()
			t.Fatal(digestErr)
		}
		segment.ObservedRanges = append(segment.ObservedRanges, claudeRangeEvidence{Start: start, End: start + 3, Digest: digest})
	}
	source.close()
	browser, err := NewBrowser(reader, t.TempDir(), DefaultBrowserOptions())
	if err != nil {
		t.Fatal(err)
	}
	defer browser.Close()
	mutationResult := make(chan error, 1)
	var mutationOnce atomic.Bool
	browser.chainCompactionObserver = func() {
		if !mutationOnce.CompareAndSwap(false, true) {
			return
		}
		data, readErr := os.ReadFile(path)
		if readErr == nil {
			data[markerStart] = 'b'
			readErr = os.WriteFile(path, data, 0o600)
		}
		if readErr == nil {
			file, openErr := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0o600)
			if openErr == nil {
				_, openErr = file.WriteString("tail appended after rewrite\n")
				if closeErr := file.Close(); openErr == nil {
					openErr = closeErr
				}
			}
			readErr = openErr
		}
		mutationResult <- readErr
	}
	scopeID := "claude|/work|" + testSessionID
	browser.mu.Lock()
	browser.recordClaudeChainLineageLocked(scopeID, []claudeSegment{segment})
	browser.mu.Unlock()
	browser.validationWG.Wait()
	if err := <-mutationResult; err != nil {
		t.Fatal(err)
	}
	browser.chainCompactionObserver = nil
	browser.mu.Lock()
	lineage := browser.chainLineage[scopeID]
	compacted := lineage.segments[0]
	browser.mu.Unlock()
	if compacted.EvidenceCompactionPending || compacted.EvidenceCompactionError == "" {
		t.Fatalf("rewrite-plus-append was compacted as authenticated evidence: %#v", compacted)
	}
}

func TestClaudeChainLargeAppendValidationIsDeferredAndDeduplicated(t *testing.T) {
	reader, home := testReader(t)
	anchor := testSessionID
	child := "123e4567-e89b-12d3-a456-426614174001"
	root := filepath.Join(home, ".claude", "projects", "-work")
	writeRows(t, filepath.Join(root, anchor+".jsonl"),
		map[string]any{"type": "assistant", "uuid": "a1", "message": map[string]any{"content": "parent"}},
		map[string]any{"type": "continued-in", "sessionId": anchor, "continuedInSessionId": child},
	)
	childPath := filepath.Join(root, child+".jsonl")
	rows := make([]map[string]any, 0, 180)
	for index := 0; index < 180; index++ {
		rows = append(rows, map[string]any{
			"type": "assistant", "uuid": fmt.Sprintf("large-%04d", index),
			"message": map[string]any{"content": strings.Repeat("large payload ", 700) + fmt.Sprintf("original-%04d", index)},
		})
	}
	writeRows(t, childPath, rows...)
	options := DefaultBrowserOptions()
	options.RecentBytes = 256 * 1024
	options.DefaultPageSize = 1
	browser, err := NewBrowser(reader, t.TempDir(), options)
	if err != nil {
		t.Fatal(err)
	}
	defer browser.Close()
	scope := BrowseScope{Provider: "claude", CWD: "/work", SessionID: anchor}
	initial, err := browser.ReadPage(context.Background(), BrowseRequest{Scope: scope, Limit: 1})
	if err != nil || initial.ReasonCode != "" || len(initial.Entries) != 1 {
		t.Fatalf("initial large page = %#v, err=%v", initial, err)
	}
	originalBeforeAppend, err := os.ReadFile(childPath)
	if err != nil {
		t.Fatal(err)
	}
	oldEnd := int64(len(originalBeforeAppend))
	marker := ""
	originalText := string(originalBeforeAppend)
	for index := 0; index < 180; index++ {
		candidate := fmt.Sprintf("original-%04d", index)
		position := int64(strings.Index(originalText, candidate))
		if position >= oldEnd-options.RecentBytes && position < oldEnd-claudeContinuationFooterBytes {
			marker = candidate
			break
		}
	}
	if marker == "" {
		t.Fatalf("large append fixture did not provide a selected-range marker: size=%d", oldEnd)
	}
	file, err := os.OpenFile(childPath, os.O_WRONLY|os.O_APPEND, 0)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.NewEncoder(file).Encode(map[string]any{
		"type": "assistant", "uuid": "after-append", "message": map[string]any{"content": "after append"},
	}); err != nil {
		_ = file.Close()
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	appendedInfo, err := os.Stat(childPath)
	if err != nil {
		t.Fatal(err)
	}
	appendedModTime := appendedInfo.ModTime()
	var foregroundBytes atomic.Int64
	var validationBytes atomic.Int64
	browser.sourceReadObserver = func(bytes int64) { foregroundBytes.Add(bytes) }
	entered := make(chan struct{})
	release := make(chan struct{})
	var enteredOnce atomic.Bool
	browser.validationReadObserver = func(bytes int64) {
		validationBytes.Add(bytes)
		if enteredOnce.CompareAndSwap(false, true) {
			close(entered)
			<-release
		}
	}
	first, err := browser.ReadPage(context.Background(), BrowseRequest{Scope: scope, Limit: 1})
	if err != nil || first.ReasonCode != "validation_pending" {
		t.Fatalf("large append validation was not deferred: %#v, err=%v", first, err)
	}
	foregroundBeforeRelease := foregroundBytes.Load()
	if foregroundBeforeRelease > options.RecentBytes {
		t.Fatalf("deferred latest request read %d foreground bytes, want at most %d", foregroundBeforeRelease, options.RecentBytes)
	}
	select {
	case <-entered:
	case <-time.After(time.Second):
		close(release)
		t.Fatal("deduplicated lineage validation did not start")
	}
	secondResult := make(chan BrowsePage, 1)
	go func() {
		page, _ := browser.ReadPage(context.Background(), BrowseRequest{Scope: scope, Limit: 1})
		secondResult <- page
	}()
	select {
	case second := <-secondResult:
		if second.ReasonCode != "validation_pending" {
			close(release)
			t.Fatalf("concurrent latest bypassed the shared validation task: %#v", second)
		}
	case <-time.After(time.Second):
		close(release)
		t.Fatal("concurrent latest waited for the deferred validation")
	}
	close(release)
	browser.validationWG.Wait()
	if validationBytes.Load() == 0 {
		t.Fatal("deferred validation did not account its source range")
	}
	current, err := os.ReadFile(childPath)
	if err != nil {
		t.Fatal(err)
	}
	rewritten := strings.Replace(string(current), marker, strings.Replace(marker, "original", "rewriten", 1), 1)
	if len(rewritten) != len(current) {
		t.Fatal("completion-before-retry rewrite changed the file length")
	}
	if err := os.WriteFile(childPath, []byte(rewritten), 0o600); err != nil {
		t.Fatal(err)
	}
	// Keep the candidate's size and modification time unchanged. The
	// filesystem change token must still prevent a completed validation for the
	// pre-rewrite descriptor from being reused.
	if err := os.Chtimes(childPath, appendedModTime, appendedModTime); err != nil {
		t.Fatal(err)
	}
	browser.validationReadObserver = nil
	changed, err := browser.ReadPage(context.Background(), BrowseRequest{Scope: scope, Limit: 1})
	if err != nil {
		t.Fatal(err)
	}
	if changed.ReasonCode == "validation_pending" {
		browser.validationWG.Wait()
		changed, err = browser.ReadPage(context.Background(), BrowseRequest{Scope: scope, Limit: 1})
		if err != nil {
			t.Fatal(err)
		}
	}
	if changed.ReasonCode != "source_changed" {
		t.Fatalf("completion-before-retry rewrite was accepted: %#v", changed)
	}
	recovered, err := browser.ReadPage(context.Background(), BrowseRequest{Scope: scope, Limit: 1})
	if err != nil || recovered.ReasonCode != "" || len(recovered.Entries) != 1 || recovered.Entries[0].Text != "after append" {
		t.Fatalf("latest after rejected completion-before-retry rewrite = %#v, err=%v", recovered, err)
	}
}

func TestClaudeChainSharesForegroundValidationBudgetAcrossAppendedSegments(t *testing.T) {
	reader, home := testReader(t)
	anchor := testSessionID
	root := filepath.Join(home, ".claude", "projects", "-work")
	sessions := []string{anchor}
	for index := 1; index < 5; index++ {
		sessions = append(sessions, fmt.Sprintf("123e4567-e89b-12d3-a456-42661417%04d", index))
	}
	for index, sessionID := range sessions {
		rows := []map[string]any{
			{"type": "assistant", "uuid": fmt.Sprintf("%s-0", sessionID), "message": map[string]any{"content": strings.Repeat(fmt.Sprintf("segment-%d ", index), 1500)}},
			{"type": "assistant", "uuid": fmt.Sprintf("%s-1", sessionID), "message": map[string]any{"content": strings.Repeat(fmt.Sprintf("segment-%d-latest ", index), 1500)}},
		}
		if index+1 < len(sessions) {
			rows = append(rows, map[string]any{"type": "continued-in", "sessionId": sessionID, "continuedInSessionId": sessions[index+1]})
		}
		writeRows(t, filepath.Join(root, sessionID+".jsonl"), rows...)
	}
	options := DefaultBrowserOptions()
	options.RecentBytes = 64 * 1024
	options.DefaultPageSize = 2
	options.MaxPageSize = 4
	browser, err := NewBrowser(reader, t.TempDir(), options)
	if err != nil {
		t.Fatal(err)
	}
	defer browser.Close()
	scope := BrowseScope{Provider: "claude", CWD: "/work", SessionID: anchor}
	latest, err := browser.ReadPage(context.Background(), BrowseRequest{Scope: scope, Limit: 2})
	if err != nil || latest.ReasonCode != "" || len(latest.Entries) != 2 || latest.NextCursor == "" {
		t.Fatalf("initial multi-segment latest = %#v, err=%v", latest, err)
	}
	// Exhaust every segment once so the lineage has one immutable captured
	// range per file before all five files grow together.
	cursor := latest.NextCursor
	for attempt := 0; cursor != "" && attempt < 100; attempt++ {
		page, readErr := browser.ReadPage(context.Background(), BrowseRequest{Scope: scope, Cursor: cursor, Limit: 2})
		if readErr != nil {
			t.Fatal(readErr)
		}
		if page.State == BrowsePreparing {
			page = waitForReadySnapshot(t, browser, scope, cursor, 2)
		}
		if page.ReasonCode != "" {
			t.Fatalf("initial multi-segment traversal = %#v", page)
		}
		cursor = page.NextCursor
		if attempt == 99 && cursor != "" {
			t.Fatal("initial multi-segment traversal did not terminate")
		}
	}
	for index, sessionID := range sessions {
		file, openErr := os.OpenFile(filepath.Join(root, sessionID+".jsonl"), os.O_WRONLY|os.O_APPEND, 0)
		if openErr != nil {
			t.Fatal(openErr)
		}
		if encodeErr := json.NewEncoder(file).Encode(map[string]any{
			"type": "assistant", "uuid": fmt.Sprintf("append-%d", index), "message": map[string]any{"content": fmt.Sprintf("append-%d", index)},
		}); encodeErr != nil {
			_ = file.Close()
			t.Fatal(encodeErr)
		}
		if closeErr := file.Close(); closeErr != nil {
			t.Fatal(closeErr)
		}
	}

	var recovered BrowsePage
	sawPending := false
	for attempt := 0; attempt < len(sessions)+4; attempt++ {
		recovered, err = browser.ReadPage(context.Background(), BrowseRequest{Scope: scope, Limit: 1})
		if err != nil {
			t.Fatal(err)
		}
		switch recovered.ReasonCode {
		case "":
			if len(recovered.Entries) != 1 || recovered.Entries[0].Text != "append-4" {
				t.Fatalf("multi-segment append latest = %#v", recovered)
			}
			if !sawPending {
				t.Fatal("multi-segment append validation never exercised the shared foreground limit")
			}
			return
		case "validation_pending":
			sawPending = true
			browser.validationWG.Wait()
		default:
			t.Fatalf("multi-segment append validation failed: %#v", recovered)
		}
	}
	t.Fatalf("multi-segment append validation remained pending: %#v", recovered)
}

func TestClaudeChainAuthenticatesSelectedRangeBeforeFirstPreparation(t *testing.T) {
	reader, home := testReader(t)
	anchor := testSessionID
	child := "123e4567-e89b-12d3-a456-426614174001"
	root := filepath.Join(home, ".claude", "projects", "-work")
	writeRows(t, filepath.Join(root, anchor+".jsonl"),
		map[string]any{"type": "assistant", "uuid": "a1", "message": map[string]any{"content": "parent"}},
		map[string]any{"type": "continued-in", "sessionId": anchor, "continuedInSessionId": child},
	)
	childPath := filepath.Join(root, child+".jsonl")
	rows := make([]map[string]any, 0, 170)
	for index := 0; index < 170; index++ {
		rows = append(rows, map[string]any{
			"type": "assistant", "uuid": fmt.Sprintf("prepared-%04d", index),
			"message": map[string]any{"content": strings.Repeat("prepared ", 700) + fmt.Sprintf("original-%04d", index)},
		})
	}
	writeRows(t, childPath, rows...)
	options := DefaultBrowserOptions()
	options.RecentBytes = 256 * 1024
	options.DefaultPageSize = 1
	options.MaxPageSize = 4
	browser, err := NewBrowser(reader, t.TempDir(), options)
	if err != nil {
		t.Fatal(err)
	}
	defer browser.Close()
	scope := BrowseScope{Provider: "claude", CWD: "/work", SessionID: anchor}
	latest, err := browser.ReadPage(context.Background(), BrowseRequest{Scope: scope, Limit: 1})
	if err != nil || latest.ReasonCode != "" || len(latest.Entries) != 1 || latest.NextCursor == "" {
		t.Fatalf("initial clipped latest = %#v, err=%v", latest, err)
	}
	original, err := os.ReadFile(childPath)
	if err != nil {
		t.Fatal(err)
	}
	oldEnd := int64(len(original))
	low := oldEnd - options.RecentBytes
	high := oldEnd - claudeContinuationFooterBytes
	marker := ""
	text := string(original)
	for index := 0; index < 170; index++ {
		candidate := fmt.Sprintf("original-%04d", index)
		position := int64(strings.Index(text, candidate))
		if position >= low && position < high {
			marker = candidate
			break
		}
	}
	if marker == "" {
		t.Fatalf("fixture did not place a selected-range marker: size=%d range=[%d,%d)", oldEnd, low, high)
	}
	rewritten := strings.Replace(text, marker, strings.Replace(marker, "original", "rewriten", 1), 1)
	if len(rewritten) != len(text) {
		t.Fatal("selected-range rewrite changed the file length")
	}
	if err := os.WriteFile(childPath, []byte(rewritten), 0o600); err != nil {
		t.Fatal(err)
	}
	file, err := os.OpenFile(childPath, os.O_WRONLY|os.O_APPEND, 0)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.NewEncoder(file).Encode(map[string]any{
		"type": "assistant", "uuid": "after-selected-rewrite", "message": map[string]any{"content": "append after selected rewrite"},
	}); err != nil {
		_ = file.Close()
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	page, err := browser.ReadPage(context.Background(), BrowseRequest{Scope: scope, Cursor: latest.NextCursor, Limit: 1})
	if err != nil {
		t.Fatal(err)
	}
	if page.State != BrowsePreparing {
		t.Fatalf("rewrite preparation did not enter the worker queue: %#v", page)
	}
	// The first byte-cursor request records the intent to prepare; the next
	// poll admits the job. Inspect it immediately after admission so the test
	// also guards the queue-publication/evidence-binding order.
	page, err = browser.ReadPage(context.Background(), BrowseRequest{Scope: scope, Cursor: latest.NextCursor, Limit: 1})
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := decodeBrowseCursor(browser.key, latest.NextCursor, normalizeBrowseScope(scope))
	if err != nil || decoded.Segment == nil {
		t.Fatalf("rewrite preparation cursor = %#v, err=%v", decoded, err)
	}
	browser.mu.Lock()
	chainContext := browser.chains[decoded.ChainID]
	var job *browseJob
	if chainContext != nil && *decoded.Segment >= 0 && *decoded.Segment < len(chainContext.jobs) {
		job = chainContext.jobs[*decoded.Segment]
	}
	browser.mu.Unlock()
	if job == nil {
		t.Fatal("rewrite preparation did not retain a job")
	}
	job.mu.RLock()
	boundEvidence := append([]claudeRangeEvidence(nil), job.chainObservedRanges...)
	job.mu.RUnlock()
	if len(boundEvidence) == 0 {
		t.Fatal("rewrite preparation queued without the selected-range evidence binding")
	}
	for attempt := 0; page.State == BrowsePreparing && attempt < 20; attempt++ {
		page, err = browser.ReadPage(context.Background(), BrowseRequest{Scope: scope, Cursor: latest.NextCursor, Limit: 1})
		if err != nil {
			t.Fatal(err)
		}
	}
	if page.ReasonCode != "source_changed" {
		t.Fatalf("prepared cursor accepted rewrite before first preparation: %#v", page)
	}
}

func TestClaudeChainPreparationRefusesRewriteAcrossEvidenceAndScanGap(t *testing.T) {
	reader, home := testReader(t)
	anchor := testSessionID
	child := "123e4567-e89b-12d3-a456-426614174001"
	root := filepath.Join(home, ".claude", "projects", "-work")
	writeRows(t, filepath.Join(root, anchor+".jsonl"),
		map[string]any{"type": "assistant", "uuid": "a1", "message": map[string]any{"content": "first record"}},
		map[string]any{"type": "continued-in", "sessionId": anchor, "continuedInSessionId": child},
	)
	childPath := filepath.Join(root, child+".jsonl")
	rows := make([]map[string]any, 0, 170)
	for index := 0; index < 170; index++ {
		rows = append(rows, map[string]any{
			"type": "assistant", "uuid": fmt.Sprintf("gap-%04d", index),
			"message": map[string]any{"content": strings.Repeat("gap payload ", 700) + fmt.Sprintf("gap-original-%04d", index)},
		})
	}
	writeRows(t, childPath, rows...)
	options := DefaultBrowserOptions()
	options.RecentBytes = 256 * 1024
	options.DefaultPageSize = 1
	options.MaxPageSize = 4
	browser, err := NewBrowser(reader, t.TempDir(), options)
	if err != nil {
		t.Fatal(err)
	}
	defer browser.Close()
	scope := BrowseScope{Provider: "claude", CWD: "/work", SessionID: anchor}
	latest, err := browser.ReadPage(context.Background(), BrowseRequest{Scope: scope, Limit: 1})
	if err != nil || latest.ReasonCode != "" || latest.NextCursor == "" {
		t.Fatalf("initial clipped page = %#v, err=%v", latest, err)
	}
	original, err := os.ReadFile(childPath)
	if err != nil {
		t.Fatal(err)
	}
	oldEnd := int64(len(original))
	low := oldEnd - options.RecentBytes
	if low < 0 {
		low = 0
	}
	marker := ""
	text := string(original)
	for index := 0; index < 170; index++ {
		candidate := fmt.Sprintf("gap-original-%04d", index)
		position := int64(strings.Index(text, candidate))
		if position >= low && position < oldEnd-claudeContinuationFooterBytes {
			marker = candidate
			break
		}
	}
	if marker == "" {
		t.Fatalf("fixture did not place an observed marker: size=%d range=[%d,%d)", oldEnd, low, oldEnd-claudeContinuationFooterBytes)
	}
	file, err := os.OpenFile(childPath, os.O_WRONLY|os.O_APPEND, 0)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.NewEncoder(file).Encode(map[string]any{
		"type": "assistant", "uuid": "gap-append", "message": map[string]any{"content": "legitimate append"},
	}); err != nil {
		_ = file.Close()
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}

	entered := make(chan struct{})
	release := make(chan struct{})
	var enteredOnce atomic.Bool
	browser.chainPreparationObserver = func() {
		if enteredOnce.CompareAndSwap(false, true) {
			close(entered)
			<-release
		}
	}
	queued, err := browser.ReadPage(context.Background(), BrowseRequest{Scope: scope, Cursor: latest.NextCursor, Limit: 1})
	if err != nil || queued.State != BrowsePreparing || queued.NextCursor == "" {
		t.Fatalf("gap preparation admission = %#v, err=%v", queued, err)
	}
	_, err = browser.ReadPage(context.Background(), BrowseRequest{Scope: scope, Cursor: latest.NextCursor, Limit: 1})
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-entered:
	case <-time.After(2 * time.Second):
		t.Fatal("preparation did not reach the evidence/scan barrier")
	}
	current, err := os.ReadFile(childPath)
	if err != nil {
		close(release)
		t.Fatal(err)
	}
	rewritten := strings.Replace(string(current), marker, strings.Replace(marker, "original", "rewriten", 1), 1)
	if len(rewritten) != len(current) || int64(len(current)) <= oldEnd ||
		rewritten[oldEnd:] != string(current[oldEnd:]) || !strings.Contains(rewritten, "legitimate append") {
		close(release)
		t.Fatal("scan-gap mutation did not preserve the current append/footer and physical length")
	}
	if err := os.WriteFile(childPath, []byte(rewritten), 0o600); err != nil {
		close(release)
		t.Fatal(err)
	}
	close(release)
	// Leave the hook installed until the job finishes; it is immutable while
	// workers may still call it, and the once guard prevents a second pause.
	browser.workerWG.Wait()

	var result BrowsePage
	for attempt := 0; attempt < 30; attempt++ {
		result, err = browser.ReadPage(context.Background(), BrowseRequest{Scope: scope, Cursor: latest.NextCursor, Limit: 1})
		if err != nil {
			t.Fatal(err)
		}
		if result.State != BrowsePreparing {
			break
		}
		time.Sleep(time.Millisecond)
	}
	if result.ReasonCode != "source_changed" || result.State != BrowseFailed {
		t.Fatalf("preparation accepted rewrite across evidence/scan gap: %#v", result)
	}
}

func TestClaudeChainRetainsShiftedEvidenceAcrossTwoAppends(t *testing.T) {
	reader, home := testReader(t)
	anchor := testSessionID
	child := "123e4567-e89b-12d3-a456-426614174001"
	root := filepath.Join(home, ".claude", "projects", "-work")
	writeRows(t, filepath.Join(root, anchor+".jsonl"),
		map[string]any{"type": "assistant", "uuid": "a1", "message": map[string]any{"content": "parent"}},
		map[string]any{"type": "continued-in", "sessionId": anchor, "continuedInSessionId": child},
	)
	childPath := filepath.Join(root, child+".jsonl")
	rows := make([]map[string]any, 0, 180)
	for index := 0; index < 180; index++ {
		rows = append(rows, map[string]any{
			"type": "assistant", "uuid": fmt.Sprintf("stable-%04d", index),
			"message": map[string]any{"content": strings.Repeat("payload ", 850) + fmt.Sprintf("original-%04d", index)},
		})
	}
	writeRows(t, childPath, rows...)
	options := DefaultBrowserOptions()
	options.RecentBytes = 256 * 1024
	options.DefaultPageSize = 1
	browser, err := NewBrowser(reader, t.TempDir(), options)
	if err != nil {
		t.Fatal(err)
	}
	defer browser.Close()
	scope := BrowseScope{Provider: "claude", CWD: "/work", SessionID: anchor}
	latestWithValidation := func() BrowsePage {
		t.Helper()
		for attempt := 0; attempt < 3; attempt++ {
			page, readErr := browser.ReadPage(context.Background(), BrowseRequest{Scope: scope, Limit: 1})
			if readErr != nil {
				t.Fatal(readErr)
			}
			if page.ReasonCode != "validation_pending" {
				return page
			}
			browser.validationWG.Wait()
		}
		t.Fatal("Claude lineage validation remained pending")
		return BrowsePage{}
	}
	initial := latestWithValidation()
	if initial.ReasonCode != "" || len(initial.Entries) != 1 {
		t.Fatalf("initial latest = %#v", initial)
	}
	original, err := os.ReadFile(childPath)
	if err != nil {
		t.Fatal(err)
	}
	oldEnd := int64(len(original))
	appendRow := func(uuid, text string) {
		t.Helper()
		file, openErr := os.OpenFile(childPath, os.O_WRONLY|os.O_APPEND, 0)
		if openErr != nil {
			t.Fatal(openErr)
		}
		if encodeErr := json.NewEncoder(file).Encode(map[string]any{
			"type": "assistant", "uuid": uuid, "message": map[string]any{"content": text},
		}); encodeErr != nil {
			_ = file.Close()
			t.Fatal(encodeErr)
		}
		if closeErr := file.Close(); closeErr != nil {
			t.Fatal(closeErr)
		}
	}
	appendRow("growth-one", strings.Repeat("first-growth ", 5000))
	firstGrowth := latestWithValidation()
	if firstGrowth.ReasonCode != "" || len(firstGrowth.Entries) != 1 || !strings.HasPrefix(firstGrowth.Entries[0].Text, "first-growth") || firstGrowth.SourceRevision != initial.SourceRevision {
		t.Fatalf("first append latest = %#v, initial revision=%q", firstGrowth, initial.SourceRevision)
	}
	info, err := os.Stat(childPath)
	if err != nil {
		t.Fatal(err)
	}
	newTailStart := info.Size() - options.RecentBytes
	oldTailStart := oldEnd - options.RecentBytes
	if oldTailStart < 0 {
		oldTailStart = 0
	}
	if newTailStart <= oldTailStart {
		t.Fatalf("append did not shift the selected range: old=%d new=%d", oldEnd, info.Size())
	}
	marker := ""
	originalText := string(original)
	for index := 0; index < 180; index++ {
		candidate := fmt.Sprintf("original-%04d", index)
		position := int64(strings.Index(originalText, candidate))
		if position >= oldTailStart && position < newTailStart {
			marker = candidate
			break
		}
	}
	if marker == "" {
		t.Fatalf("fixture did not provide an R0-only marker: old=[%d,%d) new-tail=%d", oldTailStart, oldEnd, newTailStart)
	}
	current, err := os.ReadFile(childPath)
	if err != nil {
		t.Fatal(err)
	}
	currentText := string(current)
	rewritten := strings.Replace(currentText, marker, strings.Replace(marker, "original", "rewriten", 1), 1)
	if len(rewritten) != len(currentText) {
		t.Fatal("shifted-range rewrite changed the file length")
	}
	if err := os.WriteFile(childPath, []byte(rewritten), 0o600); err != nil {
		t.Fatal(err)
	}
	appendRow("growth-two", strings.Repeat("second-growth ", 5000))
	changed := latestWithValidation()
	if changed.ReasonCode != "source_changed" {
		t.Fatalf("rewrite in shifted prior evidence was accepted: %#v", changed)
	}
	recovered := latestWithValidation()
	if recovered.ReasonCode != "" || len(recovered.Entries) != 1 || !strings.HasPrefix(recovered.Entries[0].Text, "second-growth") || recovered.SourceRevision == initial.SourceRevision {
		t.Fatalf("latest did not recover with a new lineage identity: %#v initial=%q", recovered, initial.SourceRevision)
	}
}

func TestClaudeChainPreparedCursorValidatesRewriteAfterAppend(t *testing.T) {
	reader, home := testReader(t)
	anchor := testSessionID
	child := "123e4567-e89b-12d3-a456-426614174001"
	root := filepath.Join(home, ".claude", "projects", "-work")
	writeRows(t, filepath.Join(root, anchor+".jsonl"),
		map[string]any{"type": "assistant", "uuid": "a1", "message": map[string]any{"content": "parent"}},
		map[string]any{"type": "continued-in", "sessionId": anchor, "continuedInSessionId": child},
	)
	childPath := filepath.Join(root, child+".jsonl")
	rows := make([]map[string]any, 0, 170)
	for index := 0; index < 170; index++ {
		rows = append(rows, map[string]any{
			"type": "assistant", "uuid": fmt.Sprintf("b-%04d", index),
			"message": map[string]any{"content": strings.Repeat("prepared ", 700) + fmt.Sprintf("original-%04d", index)},
		})
	}
	writeRows(t, childPath, rows...)
	options := DefaultBrowserOptions()
	options.RecentBytes = 256 * 1024
	options.DefaultPageSize = 1
	options.MaxPageSize = 512
	browser, err := NewBrowser(reader, t.TempDir(), options)
	if err != nil {
		t.Fatal(err)
	}
	defer browser.Close()
	scope := BrowseScope{Provider: "claude", CWD: "/work", SessionID: anchor}
	latest, err := browser.ReadPage(context.Background(), BrowseRequest{Scope: scope, Limit: 512})
	if err != nil || len(latest.Entries) == 0 || latest.NextCursor == "" {
		t.Fatalf("large latest page = %#v, err=%v", latest, err)
	}
	preparing, err := browser.ReadPage(context.Background(), BrowseRequest{Scope: scope, Cursor: latest.NextCursor, Limit: 512})
	if err != nil || preparing.State != BrowsePreparing || preparing.NextCursor == "" {
		t.Fatalf("large preparation page = %#v, err=%v", preparing, err)
	}
	preparedCursor := preparing.NextCursor
	ready := waitForReadySnapshot(t, browser, scope, preparedCursor, 512)
	if ready.State != BrowseReady {
		t.Fatalf("large prepared page = %#v", ready)
	}
	original, err := os.ReadFile(childPath)
	if err != nil {
		t.Fatal(err)
	}
	marker := ""
	// Rewrite the prepared prefix, outside both the first-record anchor and the
	// selected recent tail. Footer-only validation must not authenticate this
	// established captured range after an append.
	low, high := int64(64*1024), int64(len(original))-options.RecentBytes
	for index := 0; index < 170; index++ {
		candidate := fmt.Sprintf("original-%04d", index)
		position := int64(strings.Index(string(original), candidate))
		if position >= low && position < high {
			marker = candidate
			break
		}
	}
	if marker == "" {
		t.Fatalf("fixture did not place a captured-prefix rewrite marker: size=%d range=[%d,%d)", len(original), low, high)
	}
	appendRow := func(value string) {
		t.Helper()
		file, openErr := os.OpenFile(childPath, os.O_WRONLY|os.O_APPEND, 0)
		if openErr != nil {
			t.Fatal(openErr)
		}
		if encodeErr := json.NewEncoder(file).Encode(map[string]any{
			"type": "assistant", "uuid": value, "message": map[string]any{"content": value},
		}); encodeErr != nil {
			_ = file.Close()
			t.Fatal(encodeErr)
		}
		if closeErr := file.Close(); closeErr != nil {
			t.Fatal(closeErr)
		}
	}
	appendRow("legitimate-append")
	validated, err := browser.ReadPage(context.Background(), BrowseRequest{Scope: scope, Cursor: preparedCursor, Limit: 1})
	if err != nil || validated.State != BrowsePreparing {
		t.Fatalf("append validation did not enter background state: %#v, err=%v", validated, err)
	}
	browser.validationWG.Wait()
	validated, err = browser.ReadPage(context.Background(), BrowseRequest{Scope: scope, Cursor: preparedCursor, Limit: 1})
	if err != nil || validated.State != BrowseReady || validated.ReasonCode == "source_changed" {
		t.Fatalf("legitimate append invalidated prepared cursor: %#v, err=%v", validated, err)
	}

	rewritten := strings.Replace(string(original), marker, strings.Replace(marker, "original", "rewriten", 1), 1)
	if len(rewritten) != len(original) {
		t.Fatal("rewrite changed captured length")
	}
	if err := os.WriteFile(childPath, []byte(rewritten), 0o600); err != nil {
		t.Fatal(err)
	}
	appendRow("rewrite-after-append")
	changed, err := browser.ReadPage(context.Background(), BrowseRequest{Scope: scope, Cursor: preparedCursor, Limit: 1})
	if err != nil || changed.State != BrowsePreparing {
		t.Fatalf("rewrite validation did not enter background state: %#v, err=%v", changed, err)
	}
	browser.validationWG.Wait()
	changed, err = browser.ReadPage(context.Background(), BrowseRequest{Scope: scope, Cursor: preparedCursor, Limit: 1})
	if err != nil || changed.ReasonCode != "source_changed" {
		t.Fatalf("rewrite after append was accepted: %#v, err=%v", changed, err)
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

func TestClaudeChainPreparationPublishesEvidenceBeforeWorkerCapture(t *testing.T) {
	reader, home := testReader(t)
	anchor := testSessionID
	child := "123e4567-e89b-12d3-a456-426614174001"
	root := filepath.Join(home, ".claude", "projects", "-work")
	writeRows(t, filepath.Join(root, anchor+".jsonl"),
		map[string]any{"type": "assistant", "uuid": "queue-parent", "message": map[string]any{"content": "parent"}},
		map[string]any{"type": "continued-in", "sessionId": anchor, "continuedInSessionId": child},
	)
	rows := make([]map[string]any, 0, 180)
	for index := 0; index < 180; index++ {
		rows = append(rows, map[string]any{
			"type": "assistant", "uuid": fmt.Sprintf("queue-%04d", index),
			"message": map[string]any{"content": strings.Repeat("queue payload ", 700) + fmt.Sprintf("queue-%04d", index)},
		})
	}
	writeRows(t, filepath.Join(root, child+".jsonl"), rows...)
	options := DefaultBrowserOptions()
	options.RecentBytes = 256 * 1024
	options.DefaultPageSize = 1
	options.MaxPageSize = 4
	browser, err := NewBrowser(reader, t.TempDir(), options)
	if err != nil {
		t.Fatal(err)
	}
	defer browser.Close()
	scope := BrowseScope{Provider: "claude", CWD: "/work", SessionID: anchor}
	latest, err := browser.ReadPage(context.Background(), BrowseRequest{Scope: scope, Limit: 1})
	if err != nil || latest.ReasonCode != "" || latest.NextCursor == "" {
		t.Fatalf("queue fixture latest = %#v, err=%v", latest, err)
	}
	captured := make(chan []claudeRangeEvidence, 1)
	release := make(chan struct{})
	browser.chainWorkerCaptureObserver = func(evidence []claudeRangeEvidence) {
		captured <- evidence
		<-release
	}
	first, err := browser.ReadPage(context.Background(), BrowseRequest{Scope: scope, Cursor: latest.NextCursor, Limit: 1})
	if err != nil || first.State != BrowsePreparing || first.NextCursor == "" {
		t.Fatalf("queue preparation intent = %#v, err=%v", first, err)
	}
	second, err := browser.ReadPage(context.Background(), BrowseRequest{Scope: scope, Cursor: latest.NextCursor, Limit: 1})
	if err != nil || second.State != BrowsePreparing {
		t.Fatalf("queue preparation admission = %#v, err=%v", second, err)
	}
	var evidence []claudeRangeEvidence
	select {
	case evidence = <-captured:
	case <-time.After(2 * time.Second):
		close(release)
		t.Fatal("worker did not capture the published chain evidence")
	}
	if len(evidence) == 0 {
		close(release)
		t.Fatal("worker captured an empty chain evidence ledger")
	}
	for _, item := range evidence {
		if item.Start < 0 || item.End <= item.Start || item.Digest == "" {
			close(release)
			t.Fatalf("worker captured invalid evidence: %#v", item)
		}
	}
	const duplicateCallers = 12
	pages := make(chan BrowsePage, duplicateCallers)
	var group sync.WaitGroup
	for range duplicateCallers {
		group.Add(1)
		go func() {
			defer group.Done()
			page, readErr := browser.ReadPage(context.Background(), BrowseRequest{Scope: scope, Cursor: latest.NextCursor, Limit: 1})
			if readErr != nil {
				page.ReasonCode = readErr.Error()
			}
			pages <- page
		}()
	}
	group.Wait()
	close(pages)
	for page := range pages {
		if page.State != BrowsePreparing || page.ReasonCode != "" {
			close(release)
			t.Fatalf("duplicate preparation did not reuse the queued handle: %#v", page)
		}
	}
	close(release)
	ready := waitForReadySnapshot(t, browser, scope, latest.NextCursor, 1)
	browser.chainWorkerCaptureObserver = nil
	if ready.State != BrowseReady || ready.ReasonCode != "" || len(ready.Entries) != 1 {
		t.Fatalf("published evidence preparation = %#v", ready)
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
