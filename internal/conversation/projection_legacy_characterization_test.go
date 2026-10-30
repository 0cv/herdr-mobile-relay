package conversation

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestJSONLLegacyProjectionCharacterization(t *testing.T) {
	t.Run("aliases-and-original-agent", plAliases)
	t.Run("hidden-fallback-notices-and-privacy", plVisibility)
	t.Run("cursor-user-only-timestamp", plCursor)
	t.Run("owned-framing-and-independent-identities", plFraming)
	t.Run("original-call-shadowing-divergence", plShadowing)
	t.Run("full-private-ids-and-result-ordering", plOrdering)
	t.Run("sanitized-bounds-and-later-result-timing", plBounds)
	t.Run("omo-observations-mixed-with-visible-rows", plOMO)
}

// Expected emission positions are authored, never obtained from a consumer.
// Legacy identity hashes ORIGINAL line bytes, including spaces and CR, and
// counts only visible occurrences. Browser expectations use frozen pmID's
// independent standard-library revision/offset/raw oracle, not legacy IDs.
func plLegacy(t *testing.T, agent string, lines []string, positions []int, want []Entry) {
	t.Helper()
	if len(positions) != len(want) {
		t.Fatal("invalid independent legacy expectation")
	}
	got := parseTranscript(agent, strings.Join(lines, "\n"))
	if len(got) != len(want) {
		t.Fatalf("legacy count=%d want=%d", len(got), len(want))
	}
	seen := make(map[string]int)
	for i, position := range positions {
		digest := sha256.Sum256([]byte(lines[position]))
		base := hex.EncodeToString(digest[:12])
		id := base
		if occurrence := seen[base]; occurrence > 0 {
			id = fmt.Sprintf("%s-%d", base, occurrence)
		}
		seen[base]++
		expected := want[i]
		expected.ID = id
		pmCheckEntry(t, projectedEntry{Entry: got[i]}, projectedEntry{Entry: expected})
	}
}

func plLines(records []JSONLRecord) []string {
	lines := make([]string, len(records))
	for i, record := range records {
		lines[i] = string(record.Raw)
	}
	return lines
}

func plAliases(t *testing.T) {
	for _, group := range []struct {
		agents             []string
		visible, call, end string
	}{
		{[]string{"claude", "claudecode", "qoder", "qodercli", " ClAuDe-Code ", " QODER_cli "},
			`{"type":"user","timestamp":"outer","message":{"content":" question\u0000 "}}`,
			`{"type":"assistant","message":{"content":[{"type":"tool_use","id":"c","name":"Read","input":"{}"}]}}`,
			`{"type":"user","message":{"content":[{"type":"tool_result","tool_use_id":"c","content":" done ","is_error":true}]}}`},
		{[]string{"codex", "openaicodex", " OpenAI_Codex "},
			`{"type":"response_item","timestamp":"outer","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"question"}]}}`,
			`{"type":"response_item","payload":{"type":"function_call","call_id":"c","name":"Read","arguments":"{}"}}`,
			`{"type":"response_item","payload":{"type":"function_call_output","call_id":"c","output":"done","is_error":true}}`},
		{[]string{"pi", "picodingagent", "omp", "ohmypi", "omo", "ohmyopencode", " Pi-Coding_Agent "},
			`{"type":"message","timestamp":"outer","message":{"role":"user","content":"question"}}`,
			`{"type":"message","message":{"role":"assistant","content":[{"type":"toolCall","id":"c","name":"Read","arguments":"{}"}]}}`,
			`{"type":"message","message":{"role":"toolResult","toolCallId":"c","content":"done","isError":true}}`},
		{[]string{"cursor", "cursoragent", " Cursor_Agent "},
			`{"role":" USER ","timestamp":"outer","message":{"content":"<user_query>question</user_query>"}}`,
			`{"role":"assistant","message":{"content":[{"type":"tool_use","id":"c","name":"Read","input":"{}"}]}}`,
			`{"role":"user","message":{"content":[{"type":"tool_result","tool_use_id":"c","content":"done","is_error":true}]}}`},
	} {
		for _, agent := range group.agents {
			t.Run(agent, func(t *testing.T) {
				records := []JSONLRecord{pmRecord(0, group.visible), pmRecord(1000, group.call), pmRecord(2000, group.end)}
				plLegacy(t, agent, plLines(records), []int{0, 1}, []Entry{
					{Timestamp: "outer", Role: "user", Text: "question"},
					{Role: "assistant", Tools: []ToolActivity{{ID: "c", Name: "Read", Input: "{}", Output: "done", Error: true, associationID: "c"}}},
				})
				body := pmExpected(records[0], "user", "question")
				body.Entry.Entry.Timestamp = "outer"
				call := pmExpected(records[1], "assistant", "", pmTool("c", "Read", "{}"))
				final := *pmExpected(records[1], "assistant", "", ToolActivity{ID: "c", Name: "Read", Input: "{}", Output: "done", Error: true, associationID: "c"}).Entry
				pmSequence(t, agent, records, []projectionResult{body, call, {}}, []projectedEntry{*body.Entry, final})
			})
		}
	}
	for _, agent := range []string{"unknown", "", "-\tclaude-"} {
		t.Run("invisible/"+agent, func(t *testing.T) {
			// An ACTUAL TAB: normalizing twice would expose this ordinary text.
			raw := `{"type":"assistant","message":{"content":"ordinary visible Claude TEXT"}}`
			plLegacy(t, agent, []string{raw}, nil, nil)
			pmSingle(t, agent, pmRecord(0, raw), projectionResult{})
		})
	}
}

func plVisibility(t *testing.T) {
	for _, row := range []struct {
		name, agent, raw, role, text, kind string
		tools                              []ToolActivity
	}{
		{"sidechain", "claude", `{"type":"assistant","isSidechain":true,"message":{"content":[{"type":"text","text":"PRIVATE"},{"type":"tool_use","id":"c","name":"PRIVATE"}]}}`, "", "", "", nil},
		{"meta", "claude", `{"type":"user","isMeta":true,"message":{"content":"PRIVATE"}}`, "", "", "", nil},
		{"summary", "claude", `{"type":"user","isCompactSummary":true,"message":{"content":"PRIVATE"}}`, "", "", "", nil},
		{"claude-envelope", "claude", `{"type":"user","message":{"content":"<system-reminder>PRIVATE</system-reminder>"}}`, "", "", "", nil},
		{"meta-call", "claude", `{"type":"user","isMeta":true,"message":{"content":[{"type":"text","text":"PRIVATE"},{"type":"tool_use","id":"c","name":"Read","input":"{}"}]}}`, "assistant", "", "", []ToolActivity{pmTool("c", "Read", "{}")}},
		{"envelope-call", "claude", `{"type":"user","message":{"content":[{"type":"text","text":"<system-reminder>PRIVATE</system-reminder>"},{"type":"tool_use","id":"c","name":"Read","input":"{}"}]}}`, "assistant", "", "", []ToolActivity{pmTool("c", "Read", "{}")}},
		{"codex-envelope", "codex", `{"type":"response_item","payload":{"type":"message","role":"user","content":"<environment_context>PRIVATE</environment_context>"}}`, "", "", "", nil},
		{"codex-event", "codex", `{"type":"event_msg","payload":{"type":"message","role":"assistant","content":"PRIVATE"}}`, "", "", "", nil},
		{"codex-fallback", "codex", `{"type":"response_item","payload":{"type":"function_call","call_id":"c","name":"Read","arguments":"{}"}}`, "assistant", "", "", []ToolActivity{pmTool("c", "Read", "{}")}},
		{"cursor-envelope", "cursor", `{"role":"user","message":{"content":"<system_reminder>PRIVATE</system_reminder>"}}`, "", "", "", nil},
		{"cursor-fallback", "cursor", `{"role":"system","message":{"content":[{"type":"text","text":"PRIVATE"},{"type":"tool_use","id":"c","name":"Read","input":"{}"}]}}`, "assistant", "", "", []ToolActivity{pmTool("c", "Read", "{}")}},
		{"pi-metadata", "pi", `{"type":"session","message":{"role":"assistant","content":"PRIVATE"}}`, "", "", "", nil},
		{"pi-fallback", "pi", `{"type":"message","message":{"role":"system","content":[{"type":"text","text":"PRIVATE"},{"type":"toolCall","id":"c","name":"Read","arguments":"{}"}]}}`, "assistant", "", "", []ToolActivity{pmTool("c", "Read", "{}")}},
		{"notice", "pi", `{"type":"custom_message","display":true,"content":[{"type":"text","text":" First "},{"type":"text","text":"Second"}],"details":"PRIVATE"}`, "assistant", "First \nSecond", "notice", nil},
		{"hidden-notice", "pi", `{"type":"custom_message","display":false,"content":"PRIVATE","details":"PRIVATE"}`, "", "", "", nil},
		{"nontext-notice", "pi", `{"type":"custom_message","display":true,"content":[{"type":"image","data":"PRIVATE_IMAGE"}],"details":"PRIVATE"}`, "assistant", "Extension message has no displayable text.", "notice", nil},
		{"blank-notice", "pi", `{"type":"custom_message","display":true,"content":" \u0000 \u001b[31m\u001b[0m "}`, "assistant", "Extension message has no displayable text.", "notice", nil},
		{"empty", "claude", `{"type":"assistant","message":{"content":" \u0000 "}}`, "", "", "", nil},
	} {
		t.Run(row.name, func(t *testing.T) {
			record := pmRecord(7, row.raw)
			var legacy []Entry
			var positions []int
			browser := projectionResult{}
			if row.role != "" {
				positions = []int{0}
				legacy = []Entry{{Kind: row.kind, Role: row.role, Text: row.text, Tools: append([]ToolActivity(nil), row.tools...)}}
				browser = pmExpected(record, row.role, row.text, append([]ToolActivity(nil), row.tools...)...)
				browser.Entry.Entry.Kind = row.kind
			}
			plLegacy(t, row.agent, []string{row.raw}, positions, legacy)
			pmSingle(t, row.agent, record, browser)
		})
	}
}

func plCursor(t *testing.T) {
	content := "<timestamp>Monday, Jan 5, 2026, 3:04 PM (UTC+2)</timestamp><user_query>question</user_query>"
	for _, role := range []string{"user", "assistant"} {
		for _, outer := range []string{"", "outer-wins"} {
			t.Run(role+"/"+outer, func(t *testing.T) {
				raw := pmJSON(t, map[string]any{"role": role, "timestamp": outer, "message": map[string]any{"timestamp": "PRIVATE_IGNORED", "content": content}})
				text, stamp := content, outer
				if role == "user" {
					text = "question"
					if outer == "" {
						stamp = "2026-01-05T13:04:00Z"
					}
				}
				plLegacy(t, "cursor", []string{raw}, []int{0}, []Entry{{Role: role, Timestamp: stamp, Text: text}})
				record := pmRecord(2, raw)
				want := pmExpected(record, role, text)
				want.Entry.Entry.Timestamp = stamp
				pmSingle(t, "cursor", record, want)
			})
		}
	}
}

// Framing claims use an actual regular owned file, not synthetic Complete flags.
func plFrame(t *testing.T, data string, start, end int64) []JSONLRecord {
	t.Helper()
	path := filepath.Join(pmRoot(t), "owned-synthetic.jsonl")
	if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}
	file, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := file.Close(); err != nil {
			t.Errorf("close owned file: %v", err)
		}
	})
	reader, err := NewJSONLRecordReader(context.Background(), file, start, end, 2*1024*1024, nil)
	if err != nil {
		t.Fatal(err)
	}
	var records []JSONLRecord
	for {
		record, err := reader.Next()
		if err == io.EOF {
			return records
		}
		if err != nil {
			t.Fatal(err)
		}
		records = append(records, record)
	}
}

func plFraming(t *testing.T) {
	raw := `{"type":"user","message":{"content":"repeat"}}`
	hidden := `{"type":"user","isMeta":true,"message":{"content":"PRIVATE"}}`
	result := string(pmBlocks(t, 0, pmResult("unmatched", "PRIVATE_RESULT", false)).Raw)
	lines := []string{raw, hidden, result, "{", "", " \t", "null", "{}", "[]", `"str"`, "42", "true", raw, " " + raw + " ", raw + "\r", raw}
	data := strings.Join(lines, "\n") // Last valid JSON has no newline.
	plLegacy(t, "claude", lines, []int{0, 12, 13, 14, 15}, []Entry{
		{Role: "user", Text: "repeat"}, {Role: "user", Text: "repeat"}, {Role: "user", Text: "repeat"}, {Role: "user", Text: "repeat"}, {Role: "user", Text: "repeat"},
	})
	records := plFrame(t, data, 0, int64(len(data)))
	var framed []JSONLRecord
	var prepared []projectionResult
	var final []projectedEntry
	offset := int64(0)
	for i, line := range lines {
		end := offset + int64(len(line)) + 1
		if i == len(lines)-1 {
			end--
		}
		if i != 4 && i != 5 {
			record := JSONLRecord{Start: offset, End: end, Raw: []byte(strings.TrimSuffix(line, "\r")), Complete: true, Trailing: i == 15}
			framed = append(framed, record)
			want := projectionResult{}
			switch i {
			case 0, 12, 13, 14, 15:
				want = pmExpected(record, "user", "repeat")
				final = append(final, *want.Entry)
			case 3, 8, 9, 10, 11:
				want.Diagnostics.CorruptRecords = 1
			}
			prepared = append(prepared, want)
		}
		offset = end
	}
	if !reflect.DeepEqual(records, framed) {
		t.Fatal("LF/CRLF/blank/null/non-map/unterminated framing changed")
	}
	pmSequence(t, "claude", records, prepared, final)
	// Changing only revision changes browser identity; CRLF removal does not
	// erase source offsets, while legacy CR is part of its original-line hash.
	memory, index := newMemoryProjector("claude", "other-revision"), pmIndex(t)
	for i, record := range records {
		want := prepared[i]
		if want.Entry != nil {
			copy := *want.Entry
			copy.Entry.ID = pmID("other-revision", record)
			want.Entry = &copy
		}
		pmCheck(t, memory.apply(record), want)
		got, err := index.applyRecord("claude", "other-revision", record)
		if err != nil {
			t.Fatal(err)
		}
		pmCheck(t, got, want)
	}
	for _, start := range []int64{1, int64(len(raw) + 1)} {
		got := plFrame(t, data, start, int64(len(data)))
		if !reflect.DeepEqual(got, framed[1:]) {
			t.Fatal("partial leading record was not skipped")
		}
	}
	tail := raw + "\n" + `{"type":`
	partial := plFrame(t, tail, 0, int64(len(tail)))
	if len(partial) != 2 || partial[1].Complete || !partial[1].Trailing || string(partial[1].Raw) != `{"type":` {
		t.Fatal("incomplete tail framing changed")
	}
	plLegacy(t, "claude", []string{raw, `{"type":`}, []int{0}, []Entry{{Role: "user", Text: "repeat"}})
	pmSequence(t, "claude", partial, []projectionResult{pmExpected(partial[0], "user", "repeat"), {}}, []projectedEntry{*pmExpected(partial[0], "user", "repeat").Entry})
}

func plShadowing(t *testing.T) {
	for _, mode := range []string{"same-row-omitted", "later-row-omitted", "retained-duplicate", "exactly-128"} {
		t.Run(mode, func(t *testing.T) {
			var blocks []any
			var retained []ToolActivity
			for i := 0; i < 128; i++ {
				id := fmt.Sprint(i)
				if mode == "retained-duplicate" && i == 1 {
					id = "0"
				}
				blocks = append(blocks, pmCall(id, "Read"))
				retained = append(retained, pmTool(id, "Read", "{}"))
			}
			if mode == "same-row-omitted" {
				blocks = append(blocks, pmCall("0", "omitted"))
			}
			records := []JSONLRecord{pmBlocks(t, 0, blocks...)}
			legacy := []Entry{{Role: "assistant", Tools: append([]ToolActivity(nil), retained...), Truncated: mode == "same-row-omitted"}}
			prepared := []projectionResult{pmExpected(records[0], "assistant", "", append([]ToolActivity(nil), retained...)...)}
			prepared[0].Entry.Entry.Truncated = mode == "same-row-omitted"
			if mode == "same-row-omitted" {
				prepared[0].Diagnostics.OmittedTools = 1
			}
			positions := []int{0}
			if mode == "later-row-omitted" {
				later := make([]any, 0, 129)
				var tools []ToolActivity
				for i := 0; i < 128; i++ {
					id := fmt.Sprintf("later-%d", i)
					later = append(later, pmCall(id, "Read"))
					tools = append(tools, pmTool(id, "Read", "{}"))
				}
				records = append(records, pmBlocks(t, 100000, append(later, pmCall("0", "omitted"))...))
				legacy = append(legacy, Entry{Role: "assistant", Tools: append([]ToolActivity(nil), tools...), Truncated: true})
				positions = append(positions, 1)
				want := pmExpected(records[1], "assistant", "", tools...)
				want.Entry.Entry.Truncated, want.Diagnostics.OmittedTools = true, 1
				prepared = append(prepared, want)
			}
			records = append(records, pmBlocks(t, 200000, pmResult("0", "resolved", false), pmResult("0", "duplicate-ignored", true)))
			prepared = append(prepared, projectionResult{})
			var final []projectedEntry
			for _, want := range prepared[:len(prepared)-1] {
				entry := *want.Entry
				entry.Entry.Tools = append([]ToolActivity(nil), entry.Entry.Tools...)
				final = append(final, entry)
			}
			target := 0
			if mode == "retained-duplicate" {
				target = 1
			}
			final[0].Entry.Tools[target].Output = "resolved"
			if mode == "retained-duplicate" || mode == "exactly-128" {
				legacy[0].Tools[target].Output = "resolved"
			}
			// Legacy registers ORIGINAL omitted calls; browsers register retained
			// calls only. These are deliberately different expected outputs.
			plLegacy(t, "claude", plLines(records), positions, legacy)
			pmSequence(t, "claude", records, prepared, final)
		})
	}
}

func plOrdering(t *testing.T) {
	prefix := strings.Repeat("x", 256)
	a, b := prefix+"-A", prefix+"-B"
	records := []JSONLRecord{
		pmBlocks(t, 0, pmResult("future", "unmatched-no-buffer", true)),
		pmBlocks(t, 1000, pmCall("x", "first"), pmCall("x", "latest"), pmCall(a, "A"), pmCall(b, "B"), pmResult("x", "same-record-unmatched", true)),
		pmBlocks(t, 4000, pmResult("x", "", false), pmResult("x", "duplicate-ignored", true), pmResult(b, "B-out", true), pmResult(a, "A-out", false)),
		pmBlocks(t, 6000, pmCall("x", "reuse"), pmCall("future", "Future")),
		pmBlocks(t, 7000, pmResult("x", "prior-row", true), pmCall("x", "current"), map[string]any{"type": "text", "text": "visible result row"}),
		pmBlocks(t, 8000, pmResult("x", "current-row", false), pmResult("future", "late", false)),
	}
	// Full private IDs differ despite identical bounded visible IDs.
	la := ToolActivity{ID: prefix, Name: "A", Input: "{}", Output: "A-out", Truncated: true, associationID: a}
	lb := ToolActivity{ID: prefix, Name: "B", Input: "{}", Output: "B-out", Error: true, Truncated: true, associationID: b}
	legacy := []Entry{
		{Role: "assistant", Truncated: true, Tools: []ToolActivity{pmTool("x", "first", "{}"), pmTool("x", "latest", "{}"), la, lb}},
		{Role: "assistant", Tools: []ToolActivity{{ID: "x", Name: "reuse", Input: "{}", Output: "prior-row", Error: true, associationID: "x"}, {ID: "future", Name: "Future", Input: "{}", Output: "late", associationID: "future"}}},
		{Role: "assistant", Text: "visible result row", Tools: []ToolActivity{{ID: "x", Name: "current", Input: "{}", Output: "current-row", associationID: "x"}}},
	}
	plLegacy(t, " ClAuDe-Code ", plLines(records), []int{1, 3, 4}, legacy)
	pa, pb := pmTool(a, "A", "{}"), pmTool(b, "B", "{}")
	pa.ID, pb.ID, pa.Truncated, pb.Truncated = prefix, prefix, true, true
	first := pmExpected(records[1], "assistant", "", pmTool("x", "first", "{}"), pmTool("x", "latest", "{}"), pa, pb)
	first.Entry.Entry.Truncated, first.Diagnostics.OmittedPayloads = true, 2
	reuse := pmExpected(records[3], "assistant", "", pmTool("x", "reuse", "{}"), pmTool("future", "Future", "{}"))
	current := pmExpected(records[4], "assistant", "visible result row", pmTool("x", "current", "{}"))
	finalFirst := *pmExpected(records[1], "assistant", "", pmTool("x", "first", "{}"), pmTool("x", "latest", "{}"), la, lb).Entry
	finalFirst.Entry.Truncated = true
	finalReuse := *pmExpected(records[3], "assistant", "", ToolActivity{ID: "x", Name: "reuse", Input: "{}", Output: "prior-row", Error: true, associationID: "x"}, ToolActivity{ID: "future", Name: "Future", Input: "{}", Output: "late", associationID: "future"}).Entry
	finalCurrent := *pmExpected(records[4], "assistant", "visible result row", ToolActivity{ID: "x", Name: "current", Input: "{}", Output: "current-row", associationID: "x"}).Entry
	pmSequence(t, " ClAuDe-Code ", records, []projectionResult{{}, first, {}, reuse, current, {}}, []projectedEntry{finalFirst, finalReuse, finalCurrent})
	// Exact-agent browser separation remains independently covered by frozen
	// pmAgentKeys; parseTranscript has one once-normalized agent per invocation.
}

func plBounds(t *testing.T) {
	for _, prior := range []bool{false, true} {
		for _, output := range []struct {
			name, raw, want string
			truncated       bool
		}{
			{"empty", "", "", false},
			{"sanitized", " \x1b[31mshort\x1b[0m\x00 \r\n", "short", false},
			{"inclusive", " \x1b[31m\x00" + strings.Repeat("b", 131072) + "\x1b[0m\x00 ", strings.Repeat("b", 131072), false},
			{"utf8-overflow", "x" + strings.Repeat("界", 43691), "x" + strings.Repeat("界", 43690), true},
		} {
			for _, failed := range []bool{false, true} {
				t.Run(fmt.Sprintf("prior-truncated=%t/%s/error=%t", prior, output.name, failed), func(t *testing.T) {
					name, wantName := "Read", "Read"
					if prior {
						name, wantName = strings.Repeat("界", 54), strings.Repeat("界", 53)
					}
					call := pmBlocks(t, 0, map[string]any{"type": "text", "text": "retained"}, pmCall("c", name))
					end := pmBlocks(t, 1000000, pmResult("c", output.raw, failed), pmResult("c", "ignored", !failed))
					plLegacy(t, "claude", plLines([]JSONLRecord{call, end}), []int{0}, []Entry{{Role: "assistant", Text: "retained", Truncated: prior, Tools: []ToolActivity{{ID: "c", Name: wantName, Input: "{}", Output: output.want, Error: failed, Truncated: prior || output.truncated, associationID: "c"}}}})
					prepared := pmExpected(call, "assistant", "retained", pmTool("c", wantName, "{}"))
					prepared.Entry.Entry.Truncated, prepared.Entry.Entry.Tools[0].Truncated = prior, prior
					if prior {
						prepared.Diagnostics.OmittedPayloads = 1
					}
					final := *pmExpected(call, "assistant", "retained", ToolActivity{ID: "c", Name: wantName, Input: "{}", Output: output.want, Error: failed, Truncated: prior || output.truncated, associationID: "c"}).Entry
					final.Entry.Truncated = prior // Later output does NOT truncate Entry.
					pmSequence(t, "claude", []JSONLRecord{call, end}, []projectionResult{prepared, {}}, []projectedEntry{final})
				})
			}
		}
	}
	for _, body := range []struct {
		name, raw, want string
		truncated       bool
	}{
		{"inclusive-sanitized", " \x1b[31m\x00" + strings.Repeat("b", 131072) + "\x1b[0m\x00 \r", strings.Repeat("b", 131072), false},
		{"utf8-body", "x" + strings.Repeat("界", 43691), "x" + strings.Repeat("界", 43690), true},
	} {
		t.Run(body.name, func(t *testing.T) {
			raw := pmJSON(t, map[string]any{"type": "assistant", "message": map[string]any{"content": body.raw}})
			plLegacy(t, "claude", []string{raw}, []int{0}, []Entry{{Role: "assistant", Text: body.want, Truncated: body.truncated}})
			record := pmRecord(0, raw)
			want := pmExpected(record, "assistant", body.want)
			want.Entry.Entry.Truncated = body.truncated
			pmSingle(t, "claude", record, want)
		})
	}
	id, name, input := strings.Repeat("界", 100), strings.Repeat("界", 60), strings.Repeat("界", 30000)
	call := pmBlocks(t, 0, map[string]any{"type": "tool_use", "id": id, "name": name, "input": input})
	tool := ToolActivity{ID: strings.Repeat("界", 85), Name: strings.Repeat("界", 53), Input: strings.Repeat("界", 21845), Truncated: true, associationID: id}
	plLegacy(t, "claude", plLines([]JSONLRecord{call}), []int{0}, []Entry{{Role: "assistant", Tools: []ToolActivity{tool}, Truncated: true}})
	want := pmExpected(call, "assistant", "", tool)
	want.Entry.Entry.Truncated, want.Diagnostics.OmittedPayloads = true, 1
	pmSingle(t, "claude", call, want)
}

func plOMO(t *testing.T) {
	valid := `{"type":"custom","customType":"senpi.todo-state","timestamp":"source-time","data":{"schema":"v2","phases":[{"name":" phase ","tasks":[{"id":"t","content":" task ","status":"pending"}]}]}}`
	visible := `{"type":"message","message":{"role":"assistant","content":"visible"}}`
	for _, agent := range []string{"omo", "ohmyopencode"} {
		for _, invalid := range []string{
			`{"type":"custom","customType":"senpi.todo-state"}`,
			`{"type":"custom","customType":"senpi.todo-state","data":null}`,
			`{"type":"custom","customType":"senpi.todo-state","data":{"schema":"v1","phases":[]}}`,
		} {
			t.Run(agent+"/"+invalid, func(t *testing.T) {
				records := []JSONLRecord{pmRecord(0, invalid), pmRecord(1000, visible), pmRecord(2000, valid), pmRecord(3000, invalid), pmRecord(4000, visible)}
				plLegacy(t, agent, plLines(records), []int{1, 4}, []Entry{{Role: "assistant", Text: "visible"}, {Role: "assistant", Text: "visible"}})
				plan := &OMOTodoState{Version: 2, UpdatedAt: "source-time", Phases: []OMOTodoPhase{{Name: "phase", Tasks: []OMOTodoTask{{ID: "t", Content: "task", Status: "pending"}}}}}
				bad := projectionResult{TodoSeen: true, TodoInvalid: true, Diagnostics: projectionDiagnostics{PlanCorrupt: true}}
				one, two := pmExpected(records[1], "assistant", "visible"), pmExpected(records[4], "assistant", "visible")
				pmSequence(t, agent, records, []projectionResult{bad, one, {Plan: plan, TodoSeen: true}, bad, two}, []projectedEntry{*one.Entry, *two.Entry})
				memory := newMemoryProjector(agent, "revision")
				for _, record := range records {
					memory.apply(record)
				}
				if !memory.todoSeen || !memory.todoValid || !reflect.DeepEqual(memory.plan, plan) {
					t.Fatal("invalid observation erased accumulated valid plan")
				}
			})
		}
	}
}
