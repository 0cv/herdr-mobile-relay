package conversation

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"syscall"
	"testing"
	"unicode/utf8"

	bolt "go.etcd.io/bbolt"
)

func TestJSONLProjectionMemoryIndexCharacterization(t *testing.T) {
	t.Run("provider-alias-dispatch-and-hidden-envelopes", pmDispatch)
	t.Run("pi-notices-nontext-and-privacy", pmNotices)
	t.Run("cursor-timestamp-precedence", pmTimestamps)
	t.Run("framed-record-diagnostics", pmDiagnostics)
	t.Run("owned-file-crlf-boundaries-and-unterminated-json", pmFraming)
	t.Run("revision-offset-raw-and-legacy-occurrence-identity", pmIdentity)
	t.Run("omo-observations-and-accumulation-without-writes", pmPlans)
	t.Run("result-before-call-latest-registration-and-no-buffering", pmOrdering)
	t.Run("exact-supplied-agent-association-separation", pmAgentKeys)
	t.Run("utf8-body-tools-payloads-and-omitted-registration", pmBounds)
	t.Run("memory-struct-copy-slice-alias-and-index-prepared-values", pmReturnOwnership)
	t.Run("invalid-memory-locations-are-ignored", pmInvalidMemory)
	t.Run("pending-corruption-stale-deletion-and-atomic-rollback", pmCorruption)
	t.Run("closed-index-guard", pmClosed)
}

// These oracles use only standard-library primitives, not production ID helpers.
func pmID(revision string, record JSONLRecord) string {
	prefix := append([]byte(revision), 0)
	prefix = binary.BigEndian.AppendUint64(prefix, uint64(record.Start))
	prefix = append(prefix, 0)
	rawHash := sha256.Sum256(record.Raw)
	digest := sha256.Sum256(append(prefix, rawHash[:]...))
	return hex.EncodeToString(digest[:16])
}

func pmKey(agent, id string) string {
	digest := sha256.Sum256([]byte(agent + "\x00" + id))
	return hex.EncodeToString(digest[:])
}

func pmRecord(offset int64, raw string) JSONLRecord {
	return JSONLRecord{Start: offset, End: offset + int64(len(raw)) + 1, Raw: []byte(raw), Complete: true}
}

func pmJSON(t *testing.T, value any) string {
	t.Helper()
	encoded, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return string(encoded)
}

func pmRoot(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	canonical, err := filepath.EvalSymlinks(root)
	if err != nil || canonical != root {
		t.Fatalf("owned temp root is not canonical: %q, %v", root, err)
	}
	info, err := os.Lstat(root)
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		t.Fatalf("invalid owned temp root: %v", err)
	}
	owner, ok := info.Sys().(*syscall.Stat_t)
	if !ok || int(owner.Uid) != os.Getuid() {
		t.Fatal("temp root is not owned by this test process")
	}
	if err := os.Chmod(root, 0o700); err != nil {
		t.Fatal(err)
	}
	return root
}

func pmIndex(t *testing.T) *snapshotIndex {
	t.Helper()
	index, err := openSnapshotIndex(pmRoot(t), "characterization", snapshotMetadata{Schema: 1, State: "building", SnapshotID: "characterization"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := index.remove(); err != nil {
			t.Errorf("remove owned index: %v", err)
		}
	})
	return index
}

func pmTool(id, name, input string) ToolActivity {
	return ToolActivity{ID: id, Name: name, Input: input, associationID: id}
}

func pmExpected(record JSONLRecord, role, text string, tools ...ToolActivity) projectionResult {
	return projectionResult{Entry: &projectedEntry{Offset: record.Start, Entry: Entry{
		ID: pmID("revision", record), Role: role, Text: text, Tools: tools,
	}}}
}

func pmWire(entry projectedEntry) projectedEntry {
	entry.Entry.Tools = append([]ToolActivity(nil), entry.Entry.Tools...)
	for i := range entry.Entry.Tools {
		entry.Entry.Tools[i].associationID = ""
	}
	return entry
}

func pmCheckEntry(t *testing.T, got, want projectedEntry) {
	t.Helper()
	// JSON comparison treats nil/empty omitted Tools equivalently. Private IDs are
	// checked separately: retained/prepared values keep them, bbolt JSON does not.
	if pmJSON(t, got) != pmJSON(t, want) {
		t.Fatalf("entry mismatch at offset %d (wire bytes got=%d want=%d)", want.Offset, len(pmJSON(t, got)), len(pmJSON(t, want)))
	}
	for i := range want.Entry.Tools {
		if got.Entry.Tools[i].associationID != want.Entry.Tools[i].associationID {
			t.Fatalf("private association ID differs at tool %d", i)
		}
	}
}

func pmCheck(t *testing.T, got, want projectionResult) {
	t.Helper()
	if got.Diagnostics != want.Diagnostics || got.TodoSeen != want.TodoSeen || got.TodoInvalid != want.TodoInvalid || !reflect.DeepEqual(got.Plan, want.Plan) {
		t.Fatalf("observations got=%+v want=%+v", got, want)
	}
	if (got.Entry == nil) != (want.Entry == nil) {
		t.Fatalf("entry presence got=%t want=%t", got.Entry != nil, want.Entry != nil)
	}
	if want.Entry != nil {
		pmCheckEntry(t, *got.Entry, *want.Entry)
	}
}

func pmStored(t *testing.T, index *snapshotIndex, records []JSONLRecord, want []projectedEntry) {
	t.Helper()
	got, more, err := index.entriesBefore(1<<62, len(want)+1)
	if err != nil || more || len(got) != len(want) {
		t.Fatalf("stored entries count=%d want=%d more=%t err=%v", len(got), len(want), more, err)
	}
	for i := range want {
		pmCheckEntry(t, got[i], pmWire(want[i]))
	}
	if err := index.db.View(func(tx *bolt.Tx) error {
		for _, entry := range want {
			var stored indexedEntry
			if err := json.Unmarshal(tx.Bucket(entriesBucket).Get(entryOffsetKey(entry.Offset)), &stored); err != nil {
				return err
			}
			var record *JSONLRecord
			for i := range records {
				if records[i].Start == entry.Offset {
					record = &records[i]
				}
			}
			if record == nil {
				return errors.New("missing independent record expectation")
			}
			digest := sha256.Sum256(record.Raw)
			if stored.Offset != entry.Offset || stored.End != record.End || stored.Digest != hex.EncodeToString(digest[:]) {
				return fmt.Errorf("stored offset/end/digest mismatch at %d", entry.Offset)
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func pmSequence(t *testing.T, agent string, records []JSONLRecord, prepared []projectionResult, final []projectedEntry) {
	t.Helper()
	memory := newMemoryProjector(agent, "revision")
	for i, record := range records {
		pmCheck(t, memory.apply(record), prepared[i])
	}
	if len(memory.entries) != len(final) {
		t.Fatalf("memory count=%d want=%d", len(memory.entries), len(final))
	}
	for i := range final {
		pmCheckEntry(t, memory.entries[i], final[i])
	}
	for _, batch := range []bool{false, true} {
		index := pmIndex(t)
		var got []projectionResult
		if batch {
			var err error
			got, err = index.applyRecords(agent, "revision", records)
			if err != nil {
				t.Fatal(err)
			}
		} else {
			for _, record := range records {
				result, err := index.applyRecord(agent, "revision", record)
				if err != nil {
					t.Fatal(err)
				}
				got = append(got, result)
			}
		}
		if len(got) != len(prepared) {
			t.Fatalf("prepared count=%d want=%d", len(got), len(prepared))
		}
		for i := range got {
			pmCheck(t, got[i], prepared[i])
		}
		pmStored(t, index, records, final)
	}
}

func pmSingle(t *testing.T, agent string, record JSONLRecord, want projectionResult) {
	t.Helper()
	var entries []projectedEntry
	if want.Entry != nil {
		entries = append(entries, *want.Entry)
	}
	pmSequence(t, agent, []JSONLRecord{record}, []projectionResult{want}, entries)
}

func pmDispatch(t *testing.T) {
	groups := []struct {
		agents []string
		raw    string
	}{
		{[]string{"claude", "claudecode", "qoder", "qodercli", " Claude-Code "}, `{"type":"user","timestamp":"outer","message":{"timestamp":"inner","content":" question\u0000 "}}`},
		{[]string{"codex", "openaicodex", "OpenAI_Codex"}, `{"type":"response_item","timestamp":"outer","payload":{"type":"message","role":"user","timestamp":"inner","content":[{"type":"input_text","text":"question"}]}}`},
		{[]string{"pi", "picodingagent", "omp", "ohmypi", "omo", "ohmyopencode", "Pi-Coding-Agent"}, `{"type":"message","timestamp":"outer","message":{"timestamp":"inner","role":"user","content":"question"}}`},
		{[]string{"cursor", "cursoragent", "Cursor_Agent"}, `{"role":" USER ","timestamp":"outer","message":{"content":"<timestamp>Monday, Jan 5, 2026, 3:04 PM (UTC+2)</timestamp><user_query>question</user_query>"}}`},
	}
	for _, group := range groups {
		for _, agent := range group.agents {
			t.Run(agent, func(t *testing.T) {
				record := pmRecord(17, group.raw)
				want := pmExpected(record, "user", "question")
				want.Entry.Entry.Timestamp = "outer"
				pmSingle(t, agent, record, want)
			})
			t.Run(agent+"/tool-result", func(t *testing.T) {
				callRaw, resultRaw := "", ""
				switch group.agents[0] {
				case "claude":
					callRaw = `{"type":"assistant","message":{"content":[{"type":"tool_use","id":"c","name":"Read","input":"{}"}]}}`
					resultRaw = `{"type":"user","message":{"content":[{"type":"tool_result","tool_use_id":"c","content":" output ","is_error":true}]}}`
				case "codex":
					callRaw = `{"type":"response_item","payload":{"type":"function_call","call_id":"c","name":"Read","arguments":"{}"}}`
					resultRaw = `{"type":"response_item","payload":{"type":"function_call_output","call_id":"c","output":" output ","is_error":true}}`
				case "pi":
					callRaw = `{"type":"message","message":{"role":"assistant","content":[{"type":"toolCall","id":"c","name":"Read","arguments":"{}"}]}}`
					resultRaw = `{"type":"message","message":{"role":"toolResult","toolCallId":"c","content":" output ","isError":true}}`
				case "cursor":
					callRaw = `{"role":"assistant","message":{"content":[{"type":"tool_use","id":"c","name":"Read","input":"{}"}]}}`
					resultRaw = `{"role":"user","message":{"content":[{"type":"tool_result","tool_use_id":"c","content":" output ","is_error":true}]}}`
				}
				call, result := pmRecord(1, callRaw), pmRecord(1000, resultRaw)
				prepared := pmExpected(call, "assistant", "", pmTool("c", "Read", "{}"))
				final := *pmExpected(call, "assistant", "", pmTool("c", "Read", "{}")).Entry
				final.Entry.Tools[0].Output, final.Entry.Tools[0].Error = "output", true
				pmSequence(t, agent, []JSONLRecord{call, result}, []projectionResult{prepared, {}}, []projectedEntry{final})
			})
		}
	}
	for _, row := range []struct{ name, agent, raw string }{
		{"claude-sidechain", "claude", `{"type":"assistant","isSidechain":true,"message":{"content":[{"type":"text","text":"PRIVATE"},{"type":"tool_use","id":"x","name":"PRIVATE"}]}}`},
		{"claude-meta", "claude", `{"type":"user","isMeta":true,"message":{"content":"PRIVATE"}}`},
		{"claude-summary", "claude", `{"type":"user","isCompactSummary":true,"message":{"content":"PRIVATE"}}`},
		{"claude-envelope", "claude", `{"type":"user","message":{"content":[{"type":"text","text":"<system-reminder>PRIVATE</system-reminder>"}]}}`},
		{"codex-envelope", "codex", `{"type":"response_item","payload":{"type":"message","role":"user","content":"<environment_context>PRIVATE</environment_context>"}}`},
		{"codex-event", "codex", `{"type":"event_msg","payload":{"type":"message","role":"assistant","content":"PRIVATE"}}`},
		{"cursor-envelope", "cursor", `{"role":"user","message":{"content":"<system_reminder>PRIVATE</system_reminder>"}}`},
		{"pi-metadata", "pi", `{"type":"session","message":{"role":"assistant","content":"PRIVATE"}}`},
		{"unknown", "unknown", `{"type":"assistant","message":{"content":"PRIVATE"}}`},
	} {
		t.Run(row.name, func(t *testing.T) { pmSingle(t, row.agent, pmRecord(7, row.raw), projectionResult{}) })
	}
	// Hidden visible-text envelopes do not universally hide tool activity.
	record := pmRecord(29, `{"type":"user","isMeta":true,"message":{"content":[{"type":"tool_use","id":"m","name":"Read","input":null}]}}`)
	pmSingle(t, "claude", record, pmExpected(record, "assistant", "", pmTool("m", "Read", "")))
	for _, row := range []struct{ agent, raw string }{
		{"codex", `{"type":"response_item","payload":{"type":"function_call","call_id":"c","name":"Read","arguments":"{}"}}`},
		{"pi", `{"type":"message","message":{"role":"assistant","content":[{"type":"toolCall","id":"c","name":"Read","arguments":"{}"}]}}`},
		{"cursor", `{"role":"system","message":{"content":[{"type":"tool_use","id":"c","name":"Read","input":"{}"}]}}`},
	} {
		record := pmRecord(31, row.raw)
		pmSingle(t, row.agent, record, pmExpected(record, "assistant", "", pmTool("c", "Read", "{}")))
	}
}

func pmNotices(t *testing.T) {
	rows := piNoticeRows()
	rows = append(rows, map[string]any{"type": "custom_message", "display": true, "content": []any{map[string]any{"type": "image", "data": "PRIVATE_IMAGE"}}, "details": "PRIVATE_DETAILS"})
	for i, row := range rows {
		record := pmRecord(int64(i*1000), pmJSON(t, row))
		want := projectionResult{}
		switch i {
		case 0:
			want = pmExpected(record, "user", "Question")
		case 1:
			want = pmExpected(record, "assistant", "Answer")
		case 6:
			want = pmExpected(record, "assistant", "Use /orchestrate-answer synthetic-run guidance")
			want.Entry.Entry.Kind, want.Entry.Entry.Timestamp = "notice", "2026-10-16T00:00:00Z"
		case 7:
			want = pmExpected(record, "assistant", "First\nSecond")
			want.Entry.Entry.Kind = "notice"
		case 8:
			want = pmExpected(record, "assistant", "Extension message has no displayable text.")
			want.Entry.Entry.Kind = "notice"
		}
		t.Run(fmt.Sprint(i), func(t *testing.T) { pmSingle(t, "pi", record, want) })
	}
}

func pmTimestamps(t *testing.T) {
	content := "<timestamp>Monday, Jan 5, 2026, 3:04 PM (UTC+2)</timestamp><user_query>question</user_query>"
	for _, role := range []string{"user", "assistant"} {
		for _, top := range []string{"", "top-level-wins"} {
			record := pmRecord(8, pmJSON(t, map[string]any{"role": role, "timestamp": top, "message": map[string]any{"timestamp": "nested-ignored", "content": content}}))
			text, stamp := content, top
			if role == "user" {
				text = "question"
				if top == "" {
					stamp = "2026-01-05T13:04:00Z"
				}
			}
			want := pmExpected(record, role, text)
			want.Entry.Entry.Timestamp = stamp
			pmSingle(t, "cursor", record, want)
		}
	}
}

func pmDiagnostics(t *testing.T) {
	for _, row := range []struct {
		name       string
		record     JSONLRecord
		diagnostic projectionDiagnostics
	}{
		{"oversized-before-incomplete", JSONLRecord{Raw: []byte("{"), Oversized: true}, projectionDiagnostics{OversizedRecords: 1}},
		{"oversized-complete", JSONLRecord{Raw: []byte("null"), Oversized: true, Complete: true}, projectionDiagnostics{OversizedRecords: 1}},
		{"incomplete-is-not-corrupt", JSONLRecord{Raw: []byte("{")}, projectionDiagnostics{}},
		{"complete-corrupt", pmRecord(0, "{"), projectionDiagnostics{CorruptRecords: 1}},
		{"null-nil-map", pmRecord(0, "null"), projectionDiagnostics{}},
		{"array", pmRecord(0, "[]"), projectionDiagnostics{CorruptRecords: 1}},
		{"string", pmRecord(0, `"text"`), projectionDiagnostics{CorruptRecords: 1}},
		{"number", pmRecord(0, "42"), projectionDiagnostics{CorruptRecords: 1}},
		{"boolean", pmRecord(0, "true"), projectionDiagnostics{CorruptRecords: 1}},
		{"empty-object", pmRecord(0, "{}"), projectionDiagnostics{}},
	} {
		t.Run(row.name, func(t *testing.T) { pmSingle(t, "omo", row.record, projectionResult{Diagnostics: row.diagnostic}) })
	}
}

func pmFraming(t *testing.T) {
	first := `{"type":"user","message":{"content":"first"}}`
	last := `{"type":"assistant","message":{"content":"last"}}`
	data := first + "\r\n\n" + last
	path := filepath.Join(pmRoot(t), "synthetic.jsonl")
	if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0o600); err != nil {
		t.Fatal(err)
	}
	file, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := file.Close(); err != nil {
			t.Errorf("close owned JSONL: %v", err)
		}
	})
	lastStart := int64(len(first) + 3)
	for _, start := range []int64{0, 1, int64(len(first) + 2), lastStart} {
		reader, err := NewJSONLRecordReader(context.Background(), file, start, int64(len(data)), 1024, nil)
		if err != nil {
			t.Fatal(err)
		}
		digest := sha256.New()
		reader.SetDigestWriter(digest)
		var got []JSONLRecord
		for {
			record, err := reader.Next()
			if errors.Is(err, io.EOF) {
				break
			}
			if err != nil {
				t.Fatal(err)
			}
			got = append(got, record)
		}
		want := []JSONLRecord{{Start: lastStart, End: int64(len(data)), Raw: []byte(last), Complete: true, Trailing: true}}
		if start == 0 {
			want = append([]JSONLRecord{{Start: 0, End: int64(len(first) + 2), Raw: []byte(first), Complete: true}}, want...)
		}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("framing at %d got=%+v want=%+v", start, got, want)
		}
		wantDigest := sha256.Sum256([]byte(data[start:]))
		if !reflect.DeepEqual(digest.Sum(nil), wantDigest[:]) {
			t.Fatal("range digest includes boundary byte or omits CRLF")
		}
		for _, record := range got {
			role, text := "assistant", "last"
			if record.Start == 0 {
				role, text = "user", "first"
			}
			pmSingle(t, "claude", record, pmExpected(record, role, text))
		}
	}
	reader, err := NewJSONLRecordReader(context.Background(), file, lastStart, int64(len(data)-1), 1024, nil)
	if err != nil {
		t.Fatal(err)
	}
	partial, err := reader.Next()
	if err != nil || partial.Complete || !partial.Trailing || partial.Oversized {
		t.Fatalf("partial framing=%+v err=%v", partial, err)
	}
	pmSingle(t, "claude", partial, projectionResult{})
}

func pmIdentity(t *testing.T) {
	raw := `{"type":"user","message":{"content":"repeat"}}`
	a, b := pmRecord(0, raw), pmRecord(1234, raw)
	for _, revision := range []string{"revision", "other-revision"} {
		memory, index := newMemoryProjector("claude", revision), pmIndex(t)
		for _, record := range []JSONLRecord{a, b} {
			want := pmExpected(record, "user", "repeat")
			want.Entry.Entry.ID = pmID(revision, record)
			pmCheck(t, memory.apply(record), want)
			got, err := index.applyRecord("claude", revision, record)
			if err != nil {
				t.Fatal(err)
			}
			pmCheck(t, got, want)
		}
	}
	legacy := parseTranscript("claude", raw+"\n"+raw)
	digest := sha256.Sum256([]byte(raw))
	base := hex.EncodeToString(digest[:12])
	if len(legacy) != 2 || legacy[0].ID != base || legacy[1].ID != base+"-1" || pmID("revision", a) == pmID("revision", b) || pmID("revision", a) == pmID("other-revision", a) {
		t.Fatal("raw/revision/offset IDs or legacy occurrence IDs changed")
	}
}

func pmPlans(t *testing.T) {
	valid := pmRecord(1, `{"type":"custom","customType":"senpi.todo-state","timestamp":"source-time","data":{"schema":"v2","phases":[{"name":" phase ","tasks":[{"id":"t","content":" task ","status":"pending"}]}]}}`)
	plan := &OMOTodoState{Version: 2, UpdatedAt: "source-time", Phases: []OMOTodoPhase{{Name: "phase", Tasks: []OMOTodoTask{{ID: "t", Content: "task", Status: "pending"}}}}}
	good := projectionResult{Plan: plan, TodoSeen: true}
	bad := projectionResult{TodoSeen: true, TodoInvalid: true, Diagnostics: projectionDiagnostics{PlanCorrupt: true}}
	for _, agent := range []string{"omo", "ohmyopencode"} {
		memory, index := newMemoryProjector(agent, "revision"), pmIndex(t)
		// A plan-only/invisible batch must not attempt an entries transaction.
		pmUpdate(t, index, func(tx *bolt.Tx) error { return tx.DeleteBucket(entriesBucket) })
		pmCheck(t, memory.apply(valid), good)
		for _, raw := range []string{
			`{"type":"custom","customType":"senpi.todo-state"}`,
			`{"type":"custom","customType":"senpi.todo-state","data":null}`,
			`{"type":"custom","customType":"senpi.todo-state","data":{"schema":"v1","phases":[]}}`,
		} {
			invalid := pmRecord(1000, raw)
			pmCheck(t, memory.apply(invalid), bad)
			if !memory.todoSeen || !memory.todoValid || !reflect.DeepEqual(memory.plan, plan) || len(memory.entries) != 0 {
				t.Fatal("invalid observation erased accumulated valid memory plan")
			}
			results, err := index.applyRecords(agent, "revision", []JSONLRecord{valid, invalid, pmRecord(2000, "null")})
			if err != nil || len(results) != 3 {
				t.Fatalf("no-write observations=%+v err=%v", results, err)
			}
			pmCheck(t, results[0], good)
			pmCheck(t, results[1], bad)
			pmCheck(t, results[2], projectionResult{})
		}
	}
	pmSingle(t, "pi", valid, projectionResult{})
}

func pmCall(id, name string) map[string]any {
	return map[string]any{"type": "tool_use", "id": id, "name": name, "input": "{}"}
}

func pmResult(id, output string, failed bool) map[string]any {
	return map[string]any{"type": "tool_result", "tool_use_id": id, "content": output, "is_error": failed}
}

func pmBlocks(t *testing.T, offset int64, blocks ...any) JSONLRecord {
	return pmRecord(offset, pmJSON(t, map[string]any{"type": "assistant", "message": map[string]any{"content": blocks}}))
}

func pmOrdering(t *testing.T) {
	prefix := strings.Repeat("x", 256)
	longA, longB := prefix+"-a", prefix+"-b"
	records := []JSONLRecord{
		pmBlocks(t, 0, pmResult("future", "early-unmatched", false)),
		pmBlocks(t, 1000, pmCall("x", "first"), pmCall("x", "latest"), pmCall("", "empty"), pmCall(longA, "long-a"), pmCall(longB, "long-b"), pmResult("x", "same-record-unmatched", false)),
		pmBlocks(t, 3000, pmResult("x", " \u001b[31mone\u001b[0m\x00 ", true), pmResult("x", "duplicate-ignored", false), pmResult("", "empty-ignored", false), pmResult(longB, "B", false), pmResult(longA, "A", false)),
		pmBlocks(t, 4000, pmCall("x", "reused"), pmCall("future", "future"), pmResult("x", "deleted-pending-not-buffered", false)),
		pmBlocks(t, 5000, pmCall("x", "new-current"), pmResult("x", "previous-call", false)),
		pmBlocks(t, 6000, pmResult("x", "current-call", false), pmResult("future", "late-matched", false)),
	}
	la, lb := pmTool(longA, "long-a", "{}"), pmTool(longB, "long-b", "{}")
	la.ID, lb.ID, la.Truncated, lb.Truncated = prefix, prefix, true, true
	first := pmExpected(records[1], "assistant", "", pmTool("x", "first", "{}"), pmTool("x", "latest", "{}"), pmTool("", "empty", "{}"), la, lb)
	first.Diagnostics.OmittedPayloads, first.Entry.Entry.Truncated = 2, true
	reused := pmExpected(records[3], "assistant", "", pmTool("x", "reused", "{}"), pmTool("future", "future", "{}"))
	current := pmExpected(records[4], "assistant", "", pmTool("x", "new-current", "{}"))
	finalFirst := pmWire(*first.Entry)
	finalFirst.Entry.Tools = append([]ToolActivity(nil), first.Entry.Entry.Tools...)
	finalFirst.Entry.Tools[1].Output, finalFirst.Entry.Tools[1].Error = "one", true
	finalFirst.Entry.Tools[3].Output, finalFirst.Entry.Tools[4].Output = "A", "B"
	finalReused := *reused.Entry
	finalReused.Entry.Tools = append([]ToolActivity(nil), reused.Entry.Entry.Tools...)
	finalReused.Entry.Tools[0].Output, finalReused.Entry.Tools[1].Output = "previous-call", "late-matched"
	finalCurrent := *current.Entry
	finalCurrent.Entry.Tools = append([]ToolActivity(nil), current.Entry.Entry.Tools...)
	finalCurrent.Entry.Tools[0].Output = "current-call"
	pmSequence(t, "claude", records, []projectionResult{{}, first, {}, reused, current, {}}, []projectedEntry{finalFirst, finalReused, finalCurrent})
}

func pmAgentKeys(t *testing.T) {
	call := pmBlocks(t, 1, pmCall("shared", "Read"))
	result := pmBlocks(t, 1000, pmResult("shared", "matched", false))
	memory, index := newMemoryProjector("claude", "revision"), pmIndex(t)
	memory.apply(call)
	if _, err := index.applyRecord("claude", "revision", call); err != nil {
		t.Fatal(err)
	}
	if _, ok := memory.pending[pmKey("claude", "shared")]; !ok {
		t.Fatal("pending key is not exact agent + NUL + full ID")
	}
	for _, agent := range []string{"claude-code", "CLAUDE", "claude ", "qoder"} {
		// A deliberately mixed synthetic consumer proves parser-equivalent agents
		// cannot resolve each other's associations. Real memory consumers stay fixed.
		memory.agent = agent
		pmCheck(t, memory.apply(result), projectionResult{})
		got, err := index.applyRecord(agent, "revision", result)
		if err != nil {
			t.Fatal(err)
		}
		pmCheck(t, got, projectionResult{})
		if memory.entries[0].Entry.Tools[0].Output != "" {
			t.Fatal("alias unexpectedly resolved another exact agent")
		}
		pmStored(t, index, []JSONLRecord{call}, []projectedEntry{*pmExpected(call, "assistant", "", pmTool("shared", "Read", "{}")).Entry})
	}
	memory.agent = "claude"
	memory.apply(result)
	if _, err := index.applyRecord("claude", "revision", result); err != nil {
		t.Fatal(err)
	}
	want := *pmExpected(call, "assistant", "", pmTool("shared", "Read", "{}")).Entry
	want.Entry.Tools[0].Output = "matched"
	pmCheckEntry(t, memory.entries[0], want)
	pmStored(t, index, []JSONLRecord{call}, []projectedEntry{want})
	if len(memory.pending) != 0 {
		t.Fatal("successful resolution did not delete pending association")
	}
}

func pmBounds(t *testing.T) {
	body := strings.Repeat("界", 50000)
	longID, longName, longInput := strings.Repeat("界", 100), strings.Repeat("界", 60), strings.Repeat("界", 30000)
	blocks := []any{map[string]any{"type": "text", "text": body}}
	tools := make([]ToolActivity, 128)
	for i := 0; i < 130; i++ {
		id, name, input := fmt.Sprint(i), "Read", "{}"
		if i == 0 {
			id, name, input = longID, longName, longInput
		}
		blocks = append(blocks, map[string]any{"type": "tool_use", "id": id, "name": name, "input": input})
		if i < 128 {
			tools[i] = pmTool(id, name, input)
		}
	}
	tools[0].ID, tools[0].Name, tools[0].Input, tools[0].Truncated = strings.Repeat("界", 85), strings.Repeat("界", 53), strings.Repeat("界", 21845), true
	call := pmBlocks(t, 1, blocks...)
	result := pmBlocks(t, 1000000, pmResult(longID, body, false), pmResult("128", "omitted", false), pmResult("129", "omitted", false))
	want := pmExpected(call, "assistant", strings.Repeat("界", 43690), tools...)
	want.Entry.Entry.Truncated = true
	want.Diagnostics = projectionDiagnostics{OmittedTools: 2, OmittedPayloads: 1}
	final := *want.Entry
	final.Entry.Tools = append([]ToolActivity(nil), tools...)
	final.Entry.Tools[0].Output = strings.Repeat("界", 43690)
	pmSequence(t, "claude", []JSONLRecord{call, result}, []projectionResult{want, {}}, []projectedEntry{final})
	if !utf8.ValidString(final.Entry.Text) || !utf8.ValidString(final.Entry.Tools[0].Input) || !utf8.ValidString(final.Entry.Tools[0].Output) {
		t.Fatal("independent UTF-8 expectations are invalid")
	}
	memory := newMemoryProjector("claude", "revision")
	memory.apply(call)
	if _, ok := memory.pending[pmKey("claude", "128")]; ok {
		t.Fatal("omitted call registered in memory")
	}
	index := pmIndex(t)
	if _, err := index.applyRecord("claude", "revision", call); err != nil {
		t.Fatal(err)
	}
	if got := pmPending(t, index, "claude", "128"); len(got) != 0 {
		t.Fatal("omitted call registered in index")
	}
	notice := pmRecord(1, pmJSON(t, map[string]any{"type": "custom_message", "display": true, "content": body}))
	noticeWant := pmExpected(notice, "assistant", strings.Repeat("界", 43690))
	noticeWant.Entry.Entry.Kind, noticeWant.Entry.Entry.Truncated = "notice", true
	pmSingle(t, "pi", notice, noticeWant)
}

func pmReturnOwnership(t *testing.T) {
	call, result := pmBlocks(t, 1, pmCall("x", "Read")), pmBlocks(t, 1000, pmResult("x", "later", false))
	memory := newMemoryProjector("claude", "revision")
	returned := memory.apply(call)
	if returned.Entry == &memory.entries[0] || &returned.Entry.Entry.Tools[0] != &memory.entries[0].Entry.Tools[0] {
		t.Fatal("memory return must be a struct copy with aliased tool backing slice")
	}
	returned.Entry.Entry.Text = "copy-only"
	if memory.entries[0].Entry.Text != "" {
		t.Fatal("memory returned struct aliases retained entry struct")
	}
	memory.apply(result)
	if returned.Entry.Entry.Tools[0].Output != "later" || returned.Entry.Entry.Text != "copy-only" {
		t.Fatal("memory tool alias or struct copy changed")
	}
	for _, batch := range []bool{false, true} {
		index := pmIndex(t)
		var prepared projectionResult
		if batch {
			got, err := index.applyRecords("claude", "revision", []JSONLRecord{call, result})
			if err != nil {
				t.Fatal(err)
			}
			prepared = got[0]
		} else {
			var err error
			prepared, err = index.applyRecord("claude", "revision", call)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := index.applyRecord("claude", "revision", result); err != nil {
				t.Fatal(err)
			}
		}
		pmCheck(t, prepared, pmExpected(call, "assistant", "", pmTool("x", "Read", "{}")))
		prepared.Entry.Entry.Tools[0].Output = "prepared-only"
		want := *pmExpected(call, "assistant", "", pmTool("x", "Read", "{}")).Entry
		want.Entry.Tools[0].Output = "later"
		pmStored(t, index, []JSONLRecord{call}, []projectedEntry{want})
	}
}

func pmInvalidMemory(t *testing.T) {
	for _, location := range []toolLocation{{entry: -1}, {entry: 1}, {tool: -1}, {tool: 1}} {
		memory := newMemoryProjector("claude", "revision")
		call := pmBlocks(t, 1, pmCall("x", "Read"))
		memory.apply(call)
		memory.pending[pmKey("claude", "x")] = location
		pmCheck(t, memory.apply(pmBlocks(t, 1000, pmResult("x", "ignored", false))), projectionResult{})
		if memory.entries[0].Entry.Tools[0].Output != "" || memory.pending[pmKey("claude", "x")] != location {
			t.Fatal("invalid memory location was changed or deleted")
		}
	}
}

func pmUpdate(t *testing.T, index *snapshotIndex, update func(*bolt.Tx) error) {
	t.Helper()
	if err := index.db.Update(update); err != nil {
		t.Fatal(err)
	}
}

func pmPending(t *testing.T, index *snapshotIndex, agent, id string) []byte {
	t.Helper()
	var value []byte
	if err := index.db.View(func(tx *bolt.Tx) error {
		value = append([]byte(nil), tx.Bucket(pendingBucket).Get([]byte(pmKey(agent, id)))...)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	return value
}

func pmDBState(t *testing.T, index *snapshotIndex) map[string]string {
	t.Helper()
	state := make(map[string]string)
	if err := index.db.View(func(tx *bolt.Tx) error {
		for _, name := range [][]byte{entriesBucket, pendingBucket, metaBucket} {
			if err := tx.Bucket(name).ForEach(func(k, v []byte) error {
				state[string(name)+"/"+hex.EncodeToString(k)] = string(v)
				return nil
			}); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	return state
}

func pmCorruption(t *testing.T) {
	result := pmBlocks(t, 1000, pmResult("x", "output", false))
	for _, raw := range []string{"{", `{"id":"different","entry_start":77,"tool_index":0}`, `{"id":"x","entry_start":77,"tool_index":0}`} {
		index := pmIndex(t)
		pmUpdate(t, index, func(tx *bolt.Tx) error {
			return tx.Bucket(pendingBucket).Put([]byte(pmKey("claude", "x")), []byte(raw))
		})
		got, err := index.applyRecord("claude", "revision", result)
		if err != nil {
			t.Fatal(err)
		}
		pmCheck(t, got, projectionResult{})
		want := raw
		if strings.Contains(raw, `"id":"x"`) {
			want = "" // Valid pointer to absent entry is deleted, unlike memory.
		}
		if string(pmPending(t, index, "claude", "x")) != want {
			t.Fatal("malformed/mismatched pending retention or stale deletion changed")
		}
	}
	for _, row := range []struct {
		name      string
		stored    string
		toolIndex int
	}{
		{"bad-json", "{", 0},
		{"negative-tool", `{"offset":77,"entry":{"id":"stored","role":"assistant","tools":[{"id":"x","name":"Read"}]}}`, -1},
		{"out-of-range-tool", `{"offset":77,"entry":{"id":"stored","role":"assistant","tools":[{"id":"x","name":"Read"}]}}`, 1},
	} {
		t.Run(row.name, func(t *testing.T) {
			index := pmIndex(t)
			pointer := pmJSON(t, pendingIndexedTool{ID: "x", EntryStart: 77, ToolIndex: row.toolIndex})
			pmUpdate(t, index, func(tx *bolt.Tx) error {
				if err := tx.Bucket(entriesBucket).Put(entryOffsetKey(77), []byte(row.stored)); err != nil {
					return err
				}
				return tx.Bucket(pendingBucket).Put([]byte(pmKey("claude", "x")), []byte(pointer))
			})
			before := pmDBState(t, index)
			first := pmBlocks(t, 1, pmCall("new", "Read"))
			got, err := index.applyRecords("claude", "revision", []JSONLRecord{first, result})
			if !errors.Is(err, errSnapshotCorrupt) || got != nil {
				t.Fatalf("corrupt stored entry result=%+v err=%v", got, err)
			}
			if !reflect.DeepEqual(pmDBState(t, index), before) {
				t.Fatal("failing owned transaction retained earlier entry/pending writes")
			}
		})
	}
	index := pmIndex(t)
	pmUpdate(t, index, func(tx *bolt.Tx) error { return tx.DeleteBucket(pendingBucket) })
	if got, err := index.applyRecords("claude", "revision", []JSONLRecord{pmBlocks(t, 1, pmCall("x", "Read"))}); !errors.Is(err, errSnapshotCorrupt) || got != nil {
		t.Fatalf("missing bucket result=%+v err=%v", got, err)
	}
}

func pmClosed(t *testing.T) {
	index := pmIndex(t)
	if err := index.close(); err != nil {
		t.Fatal(err)
	}
	for _, closed := range []*snapshotIndex{nil, index} {
		got, err := closed.applyRecords("omo", "revision", []JSONLRecord{{Oversized: true}})
		if err == nil || err.Error() != "conversation snapshot is closed" || got != nil {
			t.Fatalf("closed guard result=%+v err=%v", got, err)
		}
	}
}
