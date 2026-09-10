package conversation

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestJSONLProjectorMatchesLegacyProjection(t *testing.T) {
	lines := []string{
		`{"type":"user","uuid":"u1","timestamp":"2026-09-02T10:00:00Z","message":{"content":"question"}}`,
		`{"type":"assistant","uuid":"a1","timestamp":"2026-09-02T10:00:01Z","message":{"content":[{"type":"text","text":"checking"},{"type":"tool_use","id":"call-1","name":"Read","input":{"file_path":"README.md"}}]}}`,
		`{"type":"user","uuid":"r1","timestamp":"2026-09-02T10:00:02Z","message":{"content":[{"type":"tool_result","tool_use_id":"call-1","content":"contents"}]}}`,
		`{"type":"assistant","uuid":"a2","timestamp":"2026-09-02T10:00:03Z","message":{"content":[{"type":"text","text":"finished"}]}}`,
	}
	transcript := ""
	for _, line := range lines {
		transcript += line + "\n"
	}
	legacy := parseTranscript("claude", transcript)
	projector := newMemoryProjector("claude", "revision")
	var offset int64
	for _, line := range lines {
		raw := []byte(line)
		result := projector.apply(JSONLRecord{Start: offset, End: offset + int64(len(raw)), Raw: raw, Complete: true})
		if result.Diagnostics.CorruptRecords != 0 {
			t.Fatalf("projection diagnostics = %#v", result.Diagnostics)
		}
		offset += int64(len(raw) + 1)
	}
	if len(projector.entries) != len(legacy) {
		t.Fatalf("projected entries = %d, legacy entries = %d", len(projector.entries), len(legacy))
	}
	for index, projected := range projector.entries {
		want := legacy[index]
		got := projected.Entry
		if got.Role != want.Role || got.Timestamp != want.Timestamp || got.Text != want.Text || got.Truncated != want.Truncated {
			t.Fatalf("entry %d differs: projected %#v, legacy %#v", index, got, want)
		}
		if len(got.Tools) != len(want.Tools) {
			t.Fatalf("entry %d tools = %#v, legacy %#v", index, got.Tools, want.Tools)
		}
		for toolIndex := range got.Tools {
			if got.Tools[toolIndex] != want.Tools[toolIndex] {
				t.Fatalf("entry %d tool %d differs: projected %#v, legacy %#v", index, toolIndex, got.Tools[toolIndex], want.Tools[toolIndex])
			}
		}
	}
}

func TestToolAssociationPreservesFullProviderScopedIDs(t *testing.T) {
	prefix := strings.Repeat("x", maxToolIDBytes)
	firstID := prefix + "-first"
	secondID := prefix + "-second"
	callRaw, err := json.Marshal(map[string]any{
		"type": "assistant",
		"message": map[string]any{"content": []any{
			map[string]any{"type": "tool_use", "id": firstID, "name": "first", "input": map[string]any{"index": 1}},
			map[string]any{"type": "tool_use", "id": secondID, "name": "second", "input": map[string]any{"index": 2}},
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	resultRaw, err := json.Marshal(map[string]any{
		"type": "user",
		"message": map[string]any{"content": []any{
			map[string]any{"type": "tool_result", "tool_use_id": firstID, "content": "first output"},
			map[string]any{"type": "tool_result", "tool_use_id": secondID, "content": "second output"},
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	records := []JSONLRecord{
		{Start: 0, End: int64(len(callRaw)), Raw: callRaw, Complete: true},
		{Start: int64(len(callRaw)), End: int64(len(callRaw) + len(resultRaw)), Raw: resultRaw, Complete: true},
	}

	legacy := parseTranscript("claude", string(callRaw)+"\n"+string(resultRaw))
	if len(legacy) != 1 || len(legacy[0].Tools) != 2 || legacy[0].Tools[0].Output != "first output" || legacy[0].Tools[1].Output != "second output" {
		t.Fatalf("legacy projection = %#v", legacy)
	}

	projector := newMemoryProjector("claude", "revision")
	for _, record := range records {
		projector.apply(record)
	}
	if len(projector.entries) != 1 || len(projector.entries[0].Entry.Tools) != 2 {
		t.Fatalf("memory projection = %#v", projector.entries)
	}
	if got := []string{projector.entries[0].Entry.Tools[0].Output, projector.entries[0].Entry.Tools[1].Output}; !reflect.DeepEqual(got, []string{"first output", "second output"}) {
		t.Fatalf("memory tool outputs = %#v", got)
	}

	index, err := openSnapshotIndex(t.TempDir(), "tool-association", snapshotMetadata{Schema: 1, State: "building", SnapshotID: "tool-association"})
	if err != nil {
		t.Fatal(err)
	}
	defer index.remove()
	if _, err := index.applyRecords("claude", "revision", records); err != nil {
		t.Fatal(err)
	}
	indexed, _, err := index.entriesBefore(1<<62, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(indexed) != 1 || len(indexed[0].Entry.Tools) != 2 {
		t.Fatalf("indexed projection = %#v", indexed)
	}
	if got := []string{indexed[0].Entry.Tools[0].Output, indexed[0].Entry.Tools[1].Output}; !reflect.DeepEqual(got, []string{"first output", "second output"}) {
		t.Fatalf("indexed tool outputs = %#v", got)
	}
	if toolAssociationKey("claude", firstID) == toolAssociationKey("codex", firstID) {
		t.Fatal("tool association key is not provider scoped")
	}
	if indexed[0].Entry.Tools[0].ID != indexed[0].Entry.Tools[1].ID {
		t.Fatalf("wire IDs unexpectedly collided: %#v", indexed[0].Entry.Tools)
	}
}

func TestToolProjectionFitsFrontendLimits(t *testing.T) {
	blocks := make([]any, 0, maxToolCount+2)
	for index := 0; index < maxToolCount+2; index++ {
		blocks = append(blocks, map[string]any{
			"type":  "tool_use",
			"id":    strings.Repeat("i", maxToolIDBytes+64),
			"name":  strings.Repeat("n", maxToolNameBytes+64),
			"input": strings.Repeat("x", maxEntryBytes),
		})
	}
	raw, err := json.Marshal(map[string]any{
		"type":    "assistant",
		"message": map[string]any{"content": blocks},
	})
	if err != nil {
		t.Fatal(err)
	}
	projector := newMemoryProjector("claude", "revision")
	result := projector.apply(JSONLRecord{Raw: raw, Complete: true})
	if result.Entry == nil {
		t.Fatal("tool-only assistant was not projected")
	}
	entry := result.Entry.Entry
	if len(entry.Tools) != maxToolCount || result.Diagnostics.OmittedTools != 2 || result.Diagnostics.OmittedPayloads != maxToolCount {
		t.Fatalf("bounded tools = %d, diagnostics = %#v", len(entry.Tools), result.Diagnostics)
	}
	if !entry.Truncated {
		t.Fatal("bounded tool entry was not marked truncated")
	}
	for _, tool := range entry.Tools {
		if len(tool.ID) > maxToolIDBytes || len(tool.Name) > maxToolNameBytes || len(tool.Input) > maxToolInputBytes {
			t.Fatalf("tool exceeded frontend limits: %#v", tool)
		}
	}
}

func TestSnapshotAppendGrowthAvoidsCapturedRangeRehash(t *testing.T) {
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
	options.MaxPageSize = 4
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
	preparing, err := browser.ReadPage(context.Background(), BrowseRequest{Scope: scope, Cursor: latest.NextCursor, Limit: 1})
	if err != nil || preparing.NextCursor == "" {
		t.Fatalf("preparing page = %#v, err = %v", preparing, err)
	}
	snapshot := waitForReadySnapshot(t, browser, scope, preparing.NextCursor, 1)
	if snapshot.State != BrowseReady || snapshot.Mode != BrowseSnapshot {
		t.Fatalf("snapshot page = %#v", snapshot)
	}
	var observed int64
	browser.sourceReadObserver = func(bytes int64) { observed += bytes }
	if err := appendRows(path, map[string]any{"type": "assistant", "uuid": "a2", "message": map[string]any{"content": "appended"}}); err != nil {
		t.Fatal(err)
	}
	if snapshot.NextCursor == "" {
		t.Fatal("snapshot page has no continuation")
	}
	if _, err := browser.ReadPage(context.Background(), BrowseRequest{Scope: scope, Cursor: snapshot.NextCursor, Limit: 1}); err != nil {
		t.Fatal(err)
	}
	if err := appendRows(path, map[string]any{"type": "user", "uuid": "u3", "message": map[string]any{"content": "appended again"}}); err != nil {
		t.Fatal(err)
	}
	if _, err := browser.ReadPage(context.Background(), BrowseRequest{Scope: scope, Cursor: snapshot.NextCursor, Limit: 1}); err != nil {
		t.Fatal(err)
	}
	if observed != 0 {
		t.Fatalf("append validation rehashed %d captured source bytes", observed)
	}
}

func TestSnapshotReferenceDefersExpiryUntilReleased(t *testing.T) {
	reader, home := testReader(t)
	path := filepath.Join(home, ".claude", "projects", "-work", testSessionID+".jsonl")
	writeRows(t, path,
		map[string]any{"type": "user", "uuid": "u1", "message": map[string]any{"content": "first"}},
		map[string]any{"type": "assistant", "uuid": "a1", "message": map[string]any{"content": "second"}},
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
	if err != nil {
		t.Fatal(err)
	}
	preparing, err := browser.ReadPage(context.Background(), BrowseRequest{Scope: scope, Cursor: latest.NextCursor, Limit: 1})
	if err != nil {
		t.Fatal(err)
	}
	snapshot := waitForReadySnapshot(t, browser, scope, preparing.NextCursor, 1)
	if snapshot.NextCursor == "" {
		t.Fatal("snapshot page has no continuation")
	}
	cursor, err := decodeBrowseCursor(browser.key, snapshot.NextCursor, normalizeBrowseScope(scope))
	if err != nil {
		t.Fatal(err)
	}
	job := browser.acquireJob(cursor.JobID)
	if job == nil {
		t.Fatal("snapshot job could not be acquired")
	}
	job.mu.Lock()
	job.lastAccess = time.Now().Add(-2 * options.SnapshotTTL)
	job.mu.Unlock()
	browser.evictExpired(time.Now())
	if browser.job(cursor.JobID) != job {
		t.Fatal("snapshot was evicted while a reader held a reference")
	}
	browser.releaseJob(job)
	job.mu.Lock()
	job.lastAccess = time.Now().Add(-2 * options.SnapshotTTL)
	job.mu.Unlock()
	browser.evictExpired(time.Now())
	if browser.job(cursor.JobID) != nil {
		t.Fatal("released expired snapshot was retained")
	}
}

func TestConcurrentSnapshotReadsCanOverlapShutdown(t *testing.T) {
	reader, home := testReader(t)
	path := filepath.Join(home, ".claude", "projects", "-work", testSessionID+".jsonl")
	rows := make([]map[string]any, 0, 32)
	for index := 0; index < cap(rows); index++ {
		rows = append(rows, map[string]any{"type": "user", "uuid": fmt.Sprintf("u-%d", index), "message": map[string]any{"content": fmt.Sprintf("message %d", index)}})
	}
	writeRows(t, path, rows...)
	options := DefaultBrowserOptions()
	options.RecentBytes = 1
	options.DefaultPageSize = 1
	browser, err := NewBrowser(reader, t.TempDir(), options)
	if err != nil {
		t.Fatal(err)
	}
	latest, err := browser.ReadPage(context.Background(), BrowseRequest{Scope: BrowseScope{Provider: "claude", CWD: "/work", SessionID: testSessionID}, Limit: 1})
	if err != nil {
		_ = browser.Close()
		t.Fatal(err)
	}
	preparing, err := browser.ReadPage(context.Background(), BrowseRequest{Scope: BrowseScope{Provider: "claude", CWD: "/work", SessionID: testSessionID}, Cursor: latest.NextCursor, Limit: 1})
	if err != nil {
		_ = browser.Close()
		t.Fatal(err)
	}
	scope := BrowseScope{Provider: "claude", CWD: "/work", SessionID: testSessionID}
	snapshot := waitForReadySnapshot(t, browser, scope, preparing.NextCursor, 1)
	if snapshot.NextCursor == "" {
		_ = browser.Close()
		t.Fatal("snapshot page has no continuation")
	}
	stop := make(chan struct{})
	var workers sync.WaitGroup
	for index := 0; index < 8; index++ {
		workers.Add(1)
		go func() {
			defer workers.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				_, _ = browser.ReadPage(context.Background(), BrowseRequest{Scope: scope, Cursor: snapshot.NextCursor, Limit: 1})
			}
		}()
	}
	time.Sleep(5 * time.Millisecond)
	if err := browser.Close(); err != nil {
		t.Fatal(err)
	}
	close(stop)
	workers.Wait()
}

func TestBrowserTraversesLargeJSONLSource(t *testing.T) {
	const entryCount = 2400
	const textBytes = 8 * 1024
	reader, home := testReader(t)
	path := filepath.Join(home, ".claude", "projects", "-work", testSessionID+".jsonl")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	file, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	body := make([]byte, textBytes)
	for index := range body {
		body[index] = 'x'
	}
	for index := 0; index < entryCount; index++ {
		if _, err := fmt.Fprintf(file, `{"type":"user","uuid":"u-%d","message":{"content":%q}}`+"\n", index, string(body)); err != nil {
			_ = file.Close()
			t.Fatal(err)
		}
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	options := DefaultBrowserOptions()
	options.RecentBytes = 1024
	options.DefaultPageSize = 128
	options.MaxPageSize = 128
	browser, err := NewBrowser(reader, t.TempDir(), options)
	if err != nil {
		t.Fatal(err)
	}
	defer browser.Close()
	scope := BrowseScope{Provider: "claude", CWD: "/work", SessionID: testSessionID}
	latest, err := browser.ReadPage(context.Background(), BrowseRequest{Scope: scope, Limit: 128})
	if err != nil || !latest.Available || latest.NextCursor == "" || !latest.HasMore {
		t.Fatalf("large source latest page = %#v, err = %v", latest, err)
	}
	preparing, err := browser.ReadPage(context.Background(), BrowseRequest{Scope: scope, Cursor: latest.NextCursor, Limit: 128})
	if err != nil || preparing.NextCursor == "" {
		t.Fatalf("large source preparation page = %#v, err = %v", preparing, err)
	}
	page := preparing
	deadline := time.Now().Add(15 * time.Second)
	for page.State == BrowsePreparing && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
		page, err = browser.ReadPage(context.Background(), BrowseRequest{Scope: scope, Cursor: preparing.NextCursor, Limit: 128})
		if err != nil {
			t.Fatal(err)
		}
	}
	if page.State != BrowseReady || page.Mode != BrowseSnapshot {
		t.Fatalf("large source prepared page = %#v", page)
	}
	seen := make(map[string]struct{}, entryCount)
	for _, entry := range page.Entries {
		seen[entry.ID] = struct{}{}
	}
	for page.HasMore {
		if page.NextCursor == "" {
			t.Fatal("large source page reported more without a cursor")
		}
		page, err = browser.ReadPage(context.Background(), BrowseRequest{Scope: scope, Cursor: page.NextCursor, Limit: 128})
		if err != nil || page.State != BrowseReady || page.Mode != BrowseSnapshot {
			t.Fatalf("large source older page = %#v, err = %v", page, err)
		}
		for _, entry := range page.Entries {
			seen[entry.ID] = struct{}{}
		}
	}
	if page.Total == nil || *page.Total != entryCount || len(seen) != entryCount {
		t.Fatalf("large source traversal total=%v unique=%d, want %d", page.Total, len(seen), entryCount)
	}
}

func FuzzBrowseCursorDecoder(f *testing.F) {
	f.Add("")
	f.Add("hb1.invalid.invalid")
	f.Add("hb1.eyJ2IjoxfQ.signature")
	var key [32]byte
	scope := BrowseScope{Provider: "claude", CWD: "/work", SessionID: testSessionID}
	f.Fuzz(func(t *testing.T, token string) {
		_, _ = decodeBrowseCursor(key, token, scope)
	})
}

func BenchmarkJSONLProjection(b *testing.B) {
	records := make([]JSONLRecord, 512)
	for index := range records {
		raw, err := json.Marshal(map[string]any{
			"type": "assistant", "uuid": fmt.Sprintf("a-%d", index),
			"message": map[string]any{"content": []any{map[string]any{"type": "text", "text": "response"}}},
		})
		if err != nil {
			b.Fatal(err)
		}
		records[index] = JSONLRecord{Start: int64(index * len(raw)), End: int64((index + 1) * len(raw)), Raw: raw, Complete: true}
	}
	b.SetBytes(int64(len(records) * len(records[0].Raw)))
	b.ReportAllocs()
	b.ResetTimer()
	for iteration := 0; iteration < b.N; iteration++ {
		projector := newMemoryProjector("claude", "benchmark")
		for _, record := range records {
			projector.apply(record)
		}
	}
}
