package conversation

import (
	"encoding/json"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"unicode/utf8"
)

func piNoticeRows() []map[string]any {
	return []map[string]any{
		{"type": "message", "id": "user", "message": map[string]any{"role": "user", "content": "Question"}},
		{"type": "message", "id": "answer", "message": map[string]any{"role": "assistant", "content": "Answer"}},
		{"type": "custom_message", "id": "hidden", "display": false, "content": "PRIVATE_HIDDEN"},
		{"type": "custom_message", "id": "missing-display", "content": "PRIVATE_MISSING"},
		{"type": "custom_message", "id": "string-display", "display": "true", "content": "PRIVATE_STRING"},
		{"type": "custom", "id": "state", "display": true, "data": "PRIVATE_STATE"},
		{"type": "custom_message", "id": "notice", "timestamp": "2026-10-16T00:00:00Z", "customType": "arbitrary-extension", "display": true, "content": "Use /orchestrate-answer synthetic-run guidance", "details": map[string]any{"secret": "PRIVATE_DETAILS"}},
		{"type": "custom_message", "id": "blocks", "display": true, "content": []any{map[string]any{"type": "text", "text": "First"}, map[string]any{"type": "image", "data": "PRIVATE_IMAGE"}, map[string]any{"type": "thinking", "thinking": "PRIVATE_THINKING"}, map[string]any{"type": "toolCall", "name": "PRIVATE_TOOL"}, map[string]any{"type": "text", "text": "Second"}}},
	}
}

func TestPiNoticeReaderVisibilityOrderingAndPaging(t *testing.T) {
	reader, home := testReader(t)
	path := filepath.Join(home, ".pi", "agent", "sessions", "--work--", "notices.jsonl")
	writeRows(t, path, piNoticeRows()...)
	page, err := reader.Read("pi", path, "", 2)
	if err != nil || !page.Available || !page.HasMore || page.Total != 4 || len(page.Entries) != 2 {
		t.Fatalf("notice page = %+v, %v", page, err)
	}
	if page.Entries[0].Kind != "notice" || page.Entries[0].Timestamp != "2026-10-16T00:00:00Z" || page.Entries[1].Text != "First\nSecond" || len(page.Entries[1].Tools) != 0 {
		t.Fatalf("notices = %+v", page.Entries)
	}
	older, err := reader.Read("pi", path, page.Entries[0].ID, 2)
	if err != nil || older.HasMore || len(older.Entries) != 2 || older.Entries[0].Text != "Question" || older.Entries[1].Text != "Answer" {
		t.Fatalf("older = %+v, %v", older, err)
	}
	again, err := reader.Read("pi", path, "", 2)
	if err != nil || !reflect.DeepEqual(page, again) {
		t.Fatalf("unstable notice page = %+v, %v", again, err)
	}
	encoded, _ := json.Marshal(page)
	if strings.Contains(string(encoded), "PRIVATE_") {
		t.Fatal("notice exported hidden content or metadata")
	}
}

func TestPiNoticeProjectionAndIndexedPaging(t *testing.T) {
	projector := newMemoryProjector("pi", "revision")
	records := []JSONLRecord{}
	var offset int64
	for _, row := range piNoticeRows() {
		raw, err := json.Marshal(row)
		if err != nil {
			t.Fatal(err)
		}
		record := JSONLRecord{Start: offset, End: offset + int64(len(raw)) + 1, Raw: raw, Complete: true}
		records = append(records, record)
		projector.apply(record)
		offset = record.End
	}
	if len(projector.entries) != 4 || projector.entries[2].Entry.Kind != "notice" || projector.entries[3].Entry.Text != "First\nSecond" {
		t.Fatalf("projection = %+v", projector.entries)
	}
	index, err := openSnapshotIndex(t.TempDir(), "pi-notices", snapshotMetadata{Schema: 1, State: "building", SnapshotID: "pi-notices"})
	if err != nil {
		t.Fatal(err)
	}
	defer index.remove()
	if _, err := index.applyRecords("pi", "revision", records); err != nil {
		t.Fatal(err)
	}
	equal := func(left, right []projectedEntry) bool {
		a, _ := json.Marshal(left)
		b, _ := json.Marshal(right)
		return string(a) == string(b)
	}
	latest, more, err := index.entriesBefore(offset, 2)
	if err != nil || !more || !equal(latest, projector.entries[2:]) {
		t.Fatalf("latest = %+v, more=%v, %v", latest, more, err)
	}
	older, more, err := index.entriesBefore(latest[0].Offset, 2)
	if err != nil || more || !equal(older, projector.entries[:2]) {
		t.Fatalf("older = %+v, more=%v, %v", older, more, err)
	}
}

func TestPiNoticeBoundsAndNonTextContent(t *testing.T) {
	for _, content := range []any{strings.Repeat("界", maxEntryBytes), []any{map[string]any{"type": "image", "data": "PRIVATE_IMAGE"}}} {
		row := map[string]any{"type": "custom_message", "display": true, "content": content}
		raw, _ := json.Marshal(row)
		entries := parseTranscript("pi", string(raw))
		projector := newMemoryProjector("pi", "revision")
		projector.apply(JSONLRecord{Raw: raw, Complete: true})
		if len(entries) != 1 || len(projector.entries) != 1 || entries[0].Kind != "notice" || entries[0].Text == "" || !utf8.ValidString(entries[0].Text) || len(entries[0].Text) > maxEntryBytes || strings.Contains(entries[0].Text, "PRIVATE_") {
			t.Fatalf("bounded notice = %+v", entries)
		}
		if _, ok := content.(string); ok && !entries[0].Truncated {
			t.Fatal("oversized notice did not disclose truncation")
		}
	}
}
