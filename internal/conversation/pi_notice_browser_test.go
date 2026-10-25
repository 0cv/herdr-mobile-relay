package conversation

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestPiNoticeRecentToSnapshotPagingWithoutGapsOrDuplicates(t *testing.T) {
	reader, home := testReader(t)
	path := filepath.Join(home, ".pi", "agent", "sessions", "--work--", "notices.jsonl")
	rows := append(piNoticeRows(),
		map[string]any{"type": "message", "id": "later-user", "message": map[string]any{"role": "user", "content": "Later question"}},
		map[string]any{"type": "custom_message", "id": "recent-notice", "display": true, "content": "[orchestration-information-request] Answer with /orchestrate-answer", "details": "PRIVATE_DETAILS"},
		map[string]any{"type": "custom_message", "id": "recent-hidden", "display": false, "content": "PRIVATE_HIDDEN"},
	)
	writeRows(t, path, rows...)
	last, _ := json.Marshal(rows[len(rows)-1])
	prior, _ := json.Marshal(rows[len(rows)-2])
	options := DefaultBrowserOptions()
	options.RecentBytes = int64(len(last) + len(prior) + 3)
	browser, err := NewBrowser(reader, t.TempDir(), options)
	if err != nil {
		t.Fatal(err)
	}
	defer browser.Close()
	scope := BrowseScope{Provider: "pi", CWD: "/work", SessionID: path}
	recent, err := browser.ReadPage(t.Context(), BrowseRequest{Scope: scope, Limit: 2})
	if err != nil || !recent.Available || recent.Mode != BrowseRecent || len(recent.Entries) != 1 || recent.Entries[0].Kind != "notice" || recent.NextCursor == "" {
		t.Fatalf("recent notice = %+v, %v", recent, err)
	}
	warm, err := browser.ReadPage(t.Context(), BrowseRequest{Scope: scope, Limit: 2})
	if err != nil || !reflect.DeepEqual(recent.Entries, warm.Entries) {
		t.Fatalf("warm recent notice = %+v, %v", warm, err)
	}
	projector := newMemoryProjector("pi", recent.SourceRevision)
	var offset int64
	for _, row := range rows {
		raw, _ := json.Marshal(row)
		projector.apply(JSONLRecord{Start: offset, End: offset + int64(len(raw)) + 1, Raw: raw, Complete: true})
		offset += int64(len(raw)) + 1
	}
	all := append([]Entry(nil), recent.Entries...)
	cursor := recent.NextCursor
	for attempts := 0; cursor != "" && attempts < 10; attempts++ {
		older, err := browser.ReadPage(t.Context(), BrowseRequest{Scope: scope, Cursor: cursor, Limit: 2})
		if err != nil {
			t.Fatal(err)
		}
		if older.State == BrowsePreparing {
			older = waitForReadySnapshot(t, browser, scope, older.NextCursor, 2)
		}
		repeated, err := browser.ReadPage(t.Context(), BrowseRequest{Scope: scope, Cursor: cursor, Limit: 2})
		if err != nil || !reflect.DeepEqual(older.Entries, repeated.Entries) {
			t.Fatalf("replayed cursor = %+v, %v", repeated, err)
		}
		all = append(append([]Entry(nil), older.Entries...), all...)
		cursor = older.NextCursor
	}
	if cursor != "" || len(all) != len(projector.entries) {
		t.Fatalf("paging did not cover the transcript: cursor=%q, entries=%+v", cursor, all)
	}
	ids := map[string]bool{}
	for i, entry := range all {
		if ids[entry.ID] || entry.ID != projector.entries[i].Entry.ID || entry.Text != projector.entries[i].Entry.Text || entry.Kind != projector.entries[i].Entry.Kind {
			t.Fatalf("entry %d differs or is duplicated: %+v, want %+v", i, entry, projector.entries[i].Entry)
		}
		ids[entry.ID] = true
	}
	encoded, _ := json.Marshal(all)
	if strings.Contains(string(encoded), "PRIVATE_") {
		t.Fatal("paged notices exported hidden content or details")
	}
	file, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	err = json.NewEncoder(file).Encode(map[string]any{"type": "custom_message", "id": "appended", "display": true, "content": "New notice"})
	closeErr := file.Close()
	if err != nil || closeErr != nil {
		t.Fatalf("append failed: %v, %v", err, closeErr)
	}
	fresh, err := browser.ReadPage(t.Context(), BrowseRequest{Scope: scope, Limit: 2})
	if err != nil || len(fresh.Entries) == 0 || fresh.Entries[len(fresh.Entries)-1].Kind != "notice" || fresh.Entries[len(fresh.Entries)-1].Text != "New notice" {
		t.Fatalf("appended notice = %+v, %v", fresh, err)
	}
	old, err := browser.ReadPage(t.Context(), BrowseRequest{Scope: scope, Cursor: recent.NextCursor, Limit: 2})
	if err != nil || old.State != BrowseReady {
		t.Fatalf("append changed prior cursor: %+v, %v", old, err)
	}
	for _, entry := range old.Entries {
		if entry.Text == "New notice" {
			t.Fatal("append entered an older snapshot")
		}
	}
}
