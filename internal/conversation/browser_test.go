package conversation

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func waitForReadySnapshot(t *testing.T, browser *Browser, scope BrowseScope, cursor string, limit int) BrowsePage {
	t.Helper()
	page := BrowsePage{}
	var err error
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		page, err = browser.ReadPage(context.Background(), BrowseRequest{Scope: scope, Cursor: cursor, Limit: limit})
		if err != nil {
			t.Fatal(err)
		}
		if page.State != BrowsePreparing {
			break
		}
		time.Sleep(time.Millisecond)
	}
	if page.State != BrowseReady || page.Mode != BrowseSnapshot {
		t.Fatalf("snapshot page = %#v", page)
	}
	return page
}

func TestJSONLReaderReportsTruncatedCapturedRange(t *testing.T) {
	path := filepath.Join(t.TempDir(), "conversation.jsonl")
	if err := os.WriteFile(path, []byte("one\nsecond\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	file, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		t.Fatal(err)
	}
	reader, err := NewJSONLRecordReader(context.Background(), file, 0, info.Size(), 128, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Truncate(path, 4); err != nil {
		t.Fatal(err)
	}
	if _, err := reader.Next(); err != nil {
		t.Fatal(err)
	}
	if _, err := reader.Next(); !errors.Is(err, errJSONLSourceTruncated) {
		t.Fatalf("truncated read error = %v, want %v", err, errJSONLSourceTruncated)
	}
}

func TestTrailingJSONLFragmentBecomesVisibleWithoutCorruption(t *testing.T) {
	reader, home := testReader(t)
	path := filepath.Join(home, ".claude", "projects", "-work", testSessionID+".jsonl")
	first := `{"type":"user","uuid":"u1","message":{"content":"first"}}`
	second := `{"type":"assistant","uuid":"a1","message":{"content":"second"}}`
	partial := `{"type":"user","uuid":"u2","message":{"content":"third"}`
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(first+"\n"+second+"\n"+partial), 0o600); err != nil {
		t.Fatal(err)
	}
	options := DefaultBrowserOptions()
	options.RecentBytes = int64(len(second) + 1 + len(partial))
	options.DefaultPageSize = 1
	options.MaxPageSize = 4
	browser, err := NewBrowser(reader, t.TempDir(), options)
	if err != nil {
		t.Fatal(err)
	}
	defer browser.Close()
	scope := BrowseScope{Provider: "claude", CWD: "/work", SessionID: testSessionID}
	recent, err := browser.ReadPage(context.Background(), BrowseRequest{Scope: scope, Limit: 1})
	if err != nil || len(recent.Entries) != 1 || recent.Entries[0].Text != "second" || recent.Diagnostics.CorruptRecords != 0 || recent.NextCursor == "" {
		t.Fatalf("recent fragment page = %#v, err = %v", recent, err)
	}
	preparing, err := browser.ReadPage(context.Background(), BrowseRequest{Scope: scope, Cursor: recent.NextCursor, Limit: 1})
	if err != nil || preparing.NextCursor == "" {
		t.Fatalf("preparation fragment page = %#v, err = %v", preparing, err)
	}
	snapshot := waitForReadySnapshot(t, browser, scope, preparing.NextCursor, 1)
	if len(snapshot.Entries) != 1 || snapshot.Entries[0].Text != "first" || snapshot.Diagnostics.CorruptRecords != 0 {
		t.Fatalf("indexed fragment page = %#v", snapshot)
	}
	file, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := file.WriteString("}\n"); err != nil {
		_ = file.Close()
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	completed, err := browser.ReadPage(context.Background(), BrowseRequest{Scope: scope, Limit: 1})
	if err != nil || len(completed.Entries) != 1 || completed.Entries[0].Text != "third" || completed.Diagnostics.CorruptRecords != 0 {
		t.Fatalf("completed recent page = %#v, err = %v", completed, err)
	}
	stable, err := browser.ReadPage(context.Background(), BrowseRequest{Scope: scope, Cursor: preparing.NextCursor, Limit: 1})
	if err != nil || stable.State != BrowseReady || stable.Mode != BrowseSnapshot || stable.Diagnostics.CorruptRecords != 0 {
		t.Fatalf("completed indexed page = %#v, err = %v", stable, err)
	}
}

func TestJSONLReaderCheckpointStopsLargeRecord(t *testing.T) {
	path := filepath.Join(t.TempDir(), "conversation.jsonl")
	if err := os.WriteFile(path, []byte(strings.Repeat("x", 2*defaultJSONLBufferBytes)), 0o600); err != nil {
		t.Fatal(err)
	}
	file, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		t.Fatal(err)
	}
	reader, err := NewJSONLRecordReader(context.Background(), file, 0, info.Size(), info.Size()+1, nil)
	if err != nil {
		t.Fatal(err)
	}
	checks := 0
	reader.SetCheckpoint(func() error {
		checks++
		if checks >= 2 {
			return errJSONLCheckpoint
		}
		return nil
	})
	if _, err := reader.Next(); !errors.Is(err, errJSONLCheckpoint) {
		t.Fatalf("checkpoint error = %v, want %v", err, errJSONLCheckpoint)
	}
	if checks != 2 {
		t.Fatalf("checkpoint calls = %d, want 2", checks)
	}
}

func TestOMOProjectionKeepsToolActivityForBothAliases(t *testing.T) {
	for _, agent := range []string{"omo", "ohmyopencode"} {
		t.Run(agent, func(t *testing.T) {
			projector := newMemoryProjector(agent, "revision")
			call := JSONLRecord{Start: 1, End: 2, Complete: true, Raw: []byte(`{"type":"message","message":{"role":"assistant","content":[{"type":"toolCall","id":"call-1","name":"shell","arguments":{"command":"pwd"}}]}}`)}
			if result := projector.apply(call); result.Entry == nil || len(result.Entry.Entry.Tools) != 1 {
				t.Fatalf("tool call projection = %#v", result.Entry)
			}
			result := projector.apply(JSONLRecord{Start: 3, End: 4, Complete: true, Raw: []byte(`{"type":"message","message":{"role":"toolResult","toolCallId":"call-1","content":"output"}}`)})
			if result.Entry != nil {
				t.Fatalf("tool result created visible entry = %#v", result.Entry)
			}
			if len(projector.entries) != 1 || projector.entries[0].Entry.Tools[0].Output != "output" {
				t.Fatalf("tool activity projection = %#v", projector.entries)
			}
		})
	}
}

func TestCapturedSourceHandleRejectsRetargetedPath(t *testing.T) {
	reader, home := testReader(t)
	path := filepath.Join(home, ".claude", "projects", "-work", testSessionID+".jsonl")
	writeRows(t, path, map[string]any{"type": "user", "uuid": "u1", "message": map[string]any{"content": "original"}})
	original, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	browser, err := NewBrowser(reader, "", DefaultBrowserOptions())
	if err != nil {
		t.Fatal(err)
	}
	defer browser.Close()
	scope := BrowseScope{Provider: "claude", CWD: "/work", SessionID: testSessionID}
	source, code := browser.sourceFor(scope, false)
	if code != "" {
		t.Fatalf("source code = %s", code)
	}
	defer source.close()
	originalPath := path + ".original"
	outsidePath := filepath.Join(t.TempDir(), "outside.jsonl")
	if err := os.WriteFile(outsidePath, []byte(`{"type":"user","uuid":"outside","message":{"content":"outside"}}\n`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(path, originalPath); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outsidePath, path); err != nil {
		t.Fatal(err)
	}
	defer func() {
		_ = os.Remove(path)
		_ = os.Rename(originalPath, path)
	}()
	captured := make([]byte, len(original))
	if _, err := source.file.ReadAt(captured, 0); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(captured, original) {
		t.Fatalf("captured source content changed: %q", captured)
	}
	if err := validateSnapshotSource(source); err == nil {
		t.Fatal("retargeted source passed validation")
	}
	page, err := browser.ReadPage(context.Background(), BrowseRequest{Scope: scope, Limit: 1})
	if err != nil || page.Available || page.ReasonCode != "source_unavailable" {
		t.Fatalf("retargeted page = %#v, err = %v", page, err)
	}
}

func TestOMOBadRecentTodoStillOffersOlderContinuation(t *testing.T) {
	home := t.TempDir()
	cwd := "/work/project"
	sessionID := "session-invalid-tail"
	directory := filepath.Join(home, ".omo", "agent", "sessions", encodeOMOCWD(cwd))
	if err := os.MkdirAll(directory, 0o700); err != nil {
		t.Fatal(err)
	}
	session := fmt.Sprintf(`{"type":"session","id":%q,"cwd":%q}
`, sessionID, cwd)
	validPlan := `{"type":"custom","customType":"senpi.todo-state","data":{"schema":"v2","phases":[{"name":"Build","tasks":[{"content":"Ship","status":"pending"}]}]}}
`
	useful := `{"type":"message","message":{"role":"user","content":"useful"}}
`
	invalidPlan := `{"type":"custom","customType":"senpi.todo-state","data":{"schema":"v2","phases":[{"name":"","tasks":[]}]}}
`
	path := filepath.Join(directory, "2026-09-02T12-00-00-000Z_"+sessionID+".jsonl")
	if err := os.WriteFile(path, []byte(session+validPlan+useful+invalidPlan), 0o600); err != nil {
		t.Fatal(err)
	}
	options := DefaultBrowserOptions()
	options.RecentBytes = int64(len([]byte(useful + invalidPlan)))
	browser, err := NewBrowser(NewReader(home), "", options)
	if err != nil {
		t.Fatal(err)
	}
	defer browser.Close()
	page, err := browser.ReadPage(context.Background(), BrowseRequest{Scope: BrowseScope{Provider: "omo", CWD: cwd, SessionID: sessionID}, Limit: 10})
	if err != nil || !page.Available || len(page.Entries) != 1 || page.Entries[0].Text != "useful" || page.NextCursor == "" || page.Diagnostics.CorruptRecords != 1 {
		t.Fatalf("OMO invalid recent page = %#v, err = %v", page, err)
	}
}

func TestBrowserRecentCursorAndSnapshot(t *testing.T) {
	reader, home := testReader(t)
	path := filepath.Join(home, ".claude", "projects", "-work", testSessionID+".jsonl")
	writeRows(t, path,
		map[string]any{"type": "user", "uuid": "u1", "timestamp": "2026-08-12T10:00:00Z", "message": map[string]any{"content": "first"}},
		map[string]any{"type": "assistant", "uuid": "a1", "timestamp": "2026-08-12T10:00:01Z", "message": map[string]any{"content": []any{map[string]any{"type": "text", "text": "second"}}}},
		map[string]any{"type": "user", "uuid": "u2", "timestamp": "2026-08-12T10:00:02Z", "message": map[string]any{"content": "third"}},
	)
	cache := t.TempDir()
	options := DefaultBrowserOptions()
	options.RecentBytes = 1
	options.DefaultPageSize = 1
	options.MaxPageSize = 4
	browser, err := NewBrowser(reader, cache, options)
	if err != nil {
		t.Fatal(err)
	}
	defer browser.Close()
	scope := BrowseScope{Provider: "claude", CWD: "/work", SessionID: testSessionID}
	page, err := browser.ReadPage(context.Background(), BrowseRequest{Scope: scope, Limit: 1})
	if err != nil {
		t.Fatal(err)
	}
	if !page.Available || len(page.Entries) != 0 || !page.HasMore || page.NextCursor == "" {
		t.Fatalf("recent page = %#v, want a bounded recent page with preparation cursor", page)
	}
	if page.Mode != BrowseRecent || page.State != BrowseReady {
		t.Fatalf("recent state = %#v", page)
	}
	preparing, err := browser.ReadPage(context.Background(), BrowseRequest{Scope: scope, Cursor: page.NextCursor, Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if preparing.State != BrowsePreparing || preparing.NextCursor == "" {
		t.Fatalf("preparing page = %#v", preparing)
	}
	var snapshot BrowsePage
	for attempt := 0; attempt < 100; attempt++ {
		time.Sleep(time.Millisecond)
		snapshot, err = browser.ReadPage(context.Background(), BrowseRequest{Scope: scope, Cursor: preparing.NextCursor, Limit: 10})
		if err != nil {
			t.Fatal(err)
		}
		if snapshot.State != BrowsePreparing {
			break
		}
	}
	if snapshot.State != BrowseReady || snapshot.Mode != BrowseSnapshot || len(snapshot.Entries) != 3 || snapshot.Total == nil || *snapshot.Total != 3 {
		t.Fatalf("snapshot page = %#v, want all indexed entries", snapshot)
	}
	if snapshot.Entries[0].ID == "" || snapshot.Entries[0].Text != "first" {
		t.Fatalf("snapshot entries = %#v", snapshot.Entries)
	}
	cacheInfo, err := os.Stat(cache)
	if err != nil {
		t.Fatal(err)
	}
	if cacheInfo.Mode().Perm() != 0o700 {
		t.Fatalf("snapshot cache permissions = %v", cacheInfo.Mode().Perm())
	}
	cacheEntries, err := os.ReadDir(cache)
	if err != nil {
		t.Fatal(err)
	}
	foundSnapshot := false
	for _, entry := range cacheEntries {
		if strings.HasPrefix(entry.Name(), "snapshot-") && strings.HasSuffix(entry.Name(), ".db") {
			info, statErr := entry.Info()
			if statErr != nil {
				t.Fatal(statErr)
			}
			if info.Mode().Perm() != 0o600 {
				t.Fatalf("snapshot permissions = %v", info.Mode().Perm())
			}
			foundSnapshot = true
		}
	}
	if !foundSnapshot {
		t.Fatal("snapshot cache file was not created")
	}
	if err := appendRows(path,
		map[string]any{"type": "assistant", "uuid": "a2", "message": map[string]any{"content": "new answer"}},
	); err != nil {
		t.Fatal(err)
	}
	stable, err := browser.ReadPage(context.Background(), BrowseRequest{Scope: scope, Cursor: preparing.NextCursor, Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if stable.Mode != BrowseSnapshot || len(stable.Entries) != 3 {
		t.Fatalf("stable snapshot after append = %#v", stable)
	}
	if _, err := browser.ReadPage(context.Background(), BrowseRequest{Scope: scope, Cursor: page.NextCursor, Limit: 1}); err != nil {
		t.Fatal(err)
	}
}

func TestBrowserSnapshotRejectsInPlaceRewrite(t *testing.T) {
	reader, home := testReader(t)
	path := filepath.Join(home, ".claude", "projects", "-work", testSessionID+".jsonl")
	writeRows(t, path,
		map[string]any{"type": "user", "uuid": "u1", "message": map[string]any{"content": "first"}},
		map[string]any{"type": "assistant", "uuid": "a1", "message": map[string]any{"content": "second"}},
		map[string]any{"type": "user", "uuid": "u2", "message": map[string]any{"content": "third"}},
	)
	options := DefaultBrowserOptions()
	options.RecentBytes = 1
	options.DefaultPageSize = 1
	browser, err := NewBrowser(reader, t.TempDir(), options)
	if err != nil {
		t.Fatal(err)
	}
	defer browser.Close()
	scope := BrowseScope{Provider: "claude", CWD: "/work", SessionID: testSessionID}
	latest, err := browser.ReadPage(context.Background(), BrowseRequest{Scope: scope, Limit: 1})
	if err != nil || latest.NextCursor == "" {
		t.Fatalf("latest page = %#v, err = %v", latest, err)
	}
	page := latest
	for attempt := 0; attempt < 100; attempt++ {
		page, err = browser.ReadPage(context.Background(), BrowseRequest{Scope: scope, Cursor: latest.NextCursor, Limit: 1})
		if err != nil {
			t.Fatal(err)
		}
		if page.State != BrowsePreparing {
			break
		}
		time.Sleep(time.Millisecond)
	}
	if page.State != BrowseReady || page.Mode != BrowseSnapshot || page.NextCursor == "" {
		t.Fatalf("snapshot page = %#v", page)
	}
	if err := os.WriteFile(path, []byte(
		`{"type":"user","uuid":"u1","message":{"content":"first"}}
`+
			`{"type":"assistant","uuid":"a1","message":{"content":"rewritten older turn"}}
`+
			`{"type":"user","uuid":"u2","message":{"content":"third"}}
`), 0o600); err != nil {
		t.Fatal(err)
	}
	changed, err := browser.ReadPage(context.Background(), BrowseRequest{Scope: scope, Cursor: page.NextCursor, Limit: 1})
	if err != nil || changed.ReasonCode != "source_changed" {
		t.Fatalf("rewritten snapshot page = %#v, err = %v", changed, err)
	}
}

func appendRows(path string, rows ...map[string]any) error {
	file, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	defer file.Close()
	encoder := json.NewEncoder(file)
	for _, row := range rows {
		if err := encoder.Encode(row); err != nil {
			return err
		}
	}
	return nil
}

func TestBrowserCursorsAreSignedAndScopeBound(t *testing.T) {
	reader, home := testReader(t)
	path := filepath.Join(home, ".claude", "projects", "-work", testSessionID+".jsonl")
	writeRows(t, path,
		map[string]any{"type": "user", "uuid": "u1", "message": map[string]any{"content": "hello"}},
		map[string]any{"type": "assistant", "uuid": "a1", "message": map[string]any{"content": "answer"}},
	)
	options := DefaultBrowserOptions()
	options.RecentBytes = 1
	browser, err := NewBrowser(reader, t.TempDir(), options)
	if err != nil {
		t.Fatal(err)
	}
	defer browser.Close()
	scope := BrowseScope{Provider: "claude", CWD: "/work", SessionID: testSessionID}
	page, err := browser.ReadPage(context.Background(), BrowseRequest{Scope: scope, Limit: 1})
	if err != nil || page.NextCursor == "" {
		t.Fatalf("initial page = %#v, err = %v", page, err)
	}
	signatureEnd := len(page.NextCursor)
	tamperedByte := byte('a')
	if page.NextCursor[signatureEnd-1] == tamperedByte {
		tamperedByte = 'b'
	}
	tampered := page.NextCursor[:signatureEnd-1] + string(tamperedByte)
	invalid, err := browser.ReadPage(context.Background(), BrowseRequest{Scope: scope, Cursor: tampered, Limit: 1})
	if err != nil || invalid.ReasonCode != "invalid_cursor" {
		t.Fatalf("tampered cursor page = %#v, err = %v", invalid, err)
	}
	otherScope := scope
	otherScope.PaneID = "other-pane"
	invalid, err = browser.ReadPage(context.Background(), BrowseRequest{Scope: otherScope, Cursor: page.NextCursor, Limit: 1})
	if err != nil || invalid.ReasonCode != "invalid_cursor" {
		t.Fatalf("cross-scope cursor page = %#v, err = %v", invalid, err)
	}
}

func TestBrowserRejectsExpiredCursorsAndCancelledRequests(t *testing.T) {
	reader, home := testReader(t)
	path := filepath.Join(home, ".claude", "projects", "-work", testSessionID+".jsonl")
	writeRows(t, path,
		map[string]any{"type": "user", "uuid": "u1", "message": map[string]any{"content": "hello"}},
		map[string]any{"type": "assistant", "uuid": "a1", "message": map[string]any{"content": "answer"}},
	)
	options := DefaultBrowserOptions()
	options.RecentBytes = 1
	browser, err := NewBrowser(reader, t.TempDir(), options)
	if err != nil {
		t.Fatal(err)
	}
	defer browser.Close()
	scope := BrowseScope{Provider: "claude", CWD: "/work", SessionID: testSessionID}
	latest, err := browser.ReadPage(context.Background(), BrowseRequest{Scope: scope, Limit: 1})
	if err != nil || latest.NextCursor == "" {
		t.Fatalf("latest page = %#v, err = %v", latest, err)
	}
	decoded, err := decodeBrowseCursor(browser.key, latest.NextCursor, normalizeBrowseScope(scope))
	if err != nil {
		t.Fatal(err)
	}
	decoded.ExpiresAt = time.Now().Add(-time.Minute).Unix()
	expired, err := encodeBrowseCursor(browser.key, decoded)
	if err != nil {
		t.Fatal(err)
	}
	page, err := browser.ReadPage(context.Background(), BrowseRequest{Scope: scope, Cursor: expired, Limit: 1})
	if err != nil || page.ReasonCode != "cursor_expired" || page.Error == nil || !page.Error.Retryable {
		t.Fatalf("expired cursor page = %#v, err = %v", page, err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	page, err = browser.ReadPage(ctx, BrowseRequest{Scope: scope, Limit: 1})
	if err != nil || page.ReasonCode != "request_cancelled" || page.Error == nil {
		t.Fatalf("cancelled page = %#v, err = %v", page, err)
	}
}

func TestBrowserBoundedScanReportsOversizedAndCorruptRecords(t *testing.T) {
	reader, home := testReader(t)
	path := filepath.Join(home, ".claude", "projects", "-work", testSessionID+".jsonl")
	writeRows(t, path,
		map[string]any{"type": "user", "uuid": "u1", "message": map[string]any{"content": "first"}},
	)
	if err := appendRows(path,
		map[string]any{"type": "assistant", "message": map[string]any{"content": strings.Repeat("x", 128)}},
	); err != nil {
		t.Fatal(err)
	}
	file, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	_, writeErr := file.WriteString("not-json\n")
	closeErr := file.Close()
	if writeErr != nil {
		t.Fatal(writeErr)
	}
	if closeErr != nil {
		t.Fatal(closeErr)
	}
	options := DefaultBrowserOptions()
	options.MaxRecordBytes = 64
	browser, err := NewBrowser(reader, "", options)
	if err != nil {
		t.Fatal(err)
	}
	defer browser.Close()
	page, err := browser.ReadPage(context.Background(), BrowseRequest{
		Scope: BrowseScope{Provider: "claude", CWD: "/work", SessionID: testSessionID}, Limit: 10,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !page.Available || len(page.Entries) != 1 || page.Diagnostics.OversizedRecords != 1 || page.Diagnostics.CorruptRecords != 1 {
		t.Fatalf("bounded scan page = %#v", page)
	}
}

func TestBrowserPreparationReportsQuotaFailureAndSupportsExplicitRetry(t *testing.T) {
	reader, home := testReader(t)
	path := filepath.Join(home, ".claude", "projects", "-work", testSessionID+".jsonl")
	writeRows(t, path,
		map[string]any{"type": "user", "uuid": "u1", "message": map[string]any{"content": "hello"}},
		map[string]any{"type": "assistant", "uuid": "a1", "message": map[string]any{"content": "answer"}},
	)
	options := DefaultBrowserOptions()
	options.RecentBytes = 1
	options.SnapshotQuota = 1024
	options.AggregateQuota = 1024 * 1024
	browser, err := NewBrowser(reader, t.TempDir(), options)
	if err != nil {
		t.Fatal(err)
	}
	defer browser.Close()
	scope := BrowseScope{Provider: "claude", CWD: "/work", SessionID: testSessionID}
	latest, err := browser.ReadPage(context.Background(), BrowseRequest{Scope: scope, Limit: 1})
	if err != nil || latest.NextCursor == "" {
		t.Fatalf("latest page = %#v, err = %v", latest, err)
	}
	page := latest
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		page, err = browser.ReadPage(context.Background(), BrowseRequest{Scope: scope, Cursor: latest.NextCursor, Limit: 1})
		if err != nil {
			t.Fatal(err)
		}
		if page.State == BrowseFailed {
			break
		}
		time.Sleep(time.Millisecond)
	}
	if page.State != BrowseFailed || page.ReasonCode != "index_capacity_exceeded" || page.Error == nil || !page.Error.Retryable || !page.HasMore {
		t.Fatalf("quota page = %#v, want an explicit retryable quota failure", page)
	}
	retry, err := browser.ReadPage(context.Background(), BrowseRequest{Scope: scope, Cursor: latest.NextCursor, Limit: 1, Retry: true})
	if err != nil || retry.State != BrowsePreparing {
		t.Fatalf("explicit retry page = %#v, err = %v", retry, err)
	}
}

func TestBrowserRecentWorksWithoutCacheAndRejectsChangedCursor(t *testing.T) {
	reader, home := testReader(t)
	path := filepath.Join(home, ".claude", "projects", "-work", testSessionID+".jsonl")
	writeRows(t, path,
		map[string]any{"type": "user", "uuid": "u1", "message": map[string]any{"content": "hello"}},
		map[string]any{"type": "assistant", "uuid": "a1", "message": map[string]any{"content": "answer"}},
	)
	options := DefaultBrowserOptions()
	options.RecentBytes = 1
	browser, err := NewBrowser(reader, "", options)
	if err != nil {
		t.Fatal(err)
	}
	defer browser.Close()
	scope := BrowseScope{Provider: "claude", CWD: "/work", SessionID: testSessionID}
	page, err := browser.ReadPage(context.Background(), BrowseRequest{Scope: scope, Limit: 1})
	if err != nil || !page.Available || page.NextCursor == "" || !page.HasMore {
		t.Fatalf("recent page = %#v, err = %v", page, err)
	}
	writeRows(t, path,
		map[string]any{"type": "user", "uuid": "u2", "message": map[string]any{"content": "changed"}},
		map[string]any{"type": "assistant", "uuid": "a2", "message": map[string]any{"content": "answer"}},
	)
	if page.NextCursor == "" {
		return
	}
	changed, err := browser.ReadPage(context.Background(), BrowseRequest{Scope: scope, Cursor: page.NextCursor, Limit: 1})
	if err != nil {
		t.Fatal(err)
	}
	if changed.ReasonCode != "source_changed" {
		t.Fatalf("changed cursor page = %#v, want source_changed", changed)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatal(err)
	}
}
