package conversation

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"unicode/utf8"
)

func previewForTool(agent string, tool ToolActivity) *EditDiff {
	cells := editDiffCells
	return tool.editDiff(agent, &cells)
}

func expectPreview(t *testing.T, agent string, tool ToolActivity, want *EditDiff) {
	t.Helper()
	if got := previewForTool(agent, tool); (got == nil) != (want == nil) || got != nil && *got != *want {
		t.Fatalf("%s %s preview = %+v, want %+v", agent, tool.Name, got, want)
	}
}

func TestEditDiffReplacementAndWrite(t *testing.T) {
	for _, tc := range []struct {
		name, tool, input, text string
		added, removed          int
	}{
		{"insertion", "edit", `"oldText":"","newText":"  +new\r\n\r\n"`, "+  +new\n+", 2, 0},
		{"deletion", "edit", `"oldText":"old\n","newText":""`, "-old", 0, 1},
		{"unchanged middle", "edit", `"oldText":"start\na\nkeep\nc\nend","newText":"start\nb\nkeep\nd\nend"`, "-a\n+b\n@@\n-c\n+d", 2, 2},
		{"pairs", "edit", `"edits":[{"oldText":"a","newText":"b"},{"oldText":"same","newText":"same"},{"oldText":"c","newText":"d"}]`, "-a\n+b\n@@\n-c\n+d", 2, 2},
		{"empty write", "write", `"content":""`, "", 0, 0},
		{"trailing newline", "write", `"content":"a\n\t--- literal\n"`, "+a\n+\t--- literal", 2, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tool := ToolActivity{Name: tc.tool, Input: `{"path":"src/a",` + tc.input + `}`}
			expectPreview(t, "pi", tool, &EditDiff{Kind: tc.tool, Path: "src/a", Added: tc.added, Removed: tc.removed, Text: tc.text})
		})
	}
}

func TestEditDiffLimits(t *testing.T) {
	for _, tc := range []struct {
		content, text string
		added         int
		truncated     bool
	}{
		{strings.Repeat("a\n", 200), strings.TrimSuffix(strings.Repeat("+a\n", 200), "\n"), 200, false},
		{strings.Repeat("a\n", 201), strings.TrimSuffix(strings.Repeat("+a\n", 200), "\n"), 201, true},
		{strings.Repeat("a", 8191), "+" + strings.Repeat("a", 8191), 1, false},
		{strings.Repeat("a", 8192), "+" + strings.Repeat("a", 8191), 1, true},
		{strings.Repeat("日", 3000), "+" + strings.Repeat("日", 2730), 1, true},
		{strings.Repeat("a", 8190) + "\nb", "+" + strings.Repeat("a", 8190), 2, true},
	} {
		input, _ := json.Marshal(map[string]string{"path": "a", "content": tc.content})
		expectPreview(t, "pi", ToolActivity{Name: "write", Input: string(input)}, &EditDiff{Kind: "write", Path: "a", Added: tc.added, Text: tc.text, Truncated: tc.truncated})
		if !utf8.ValidString(tc.text) {
			t.Fatal("fixture text is not valid UTF-8")
		}
	}
	arguments := func(n int) string {
		input, _ := json.Marshal(map[string]string{"path": "a", "oldText": strings.Repeat("a\n", 500), "newText": strings.Repeat("b\n", n)})
		return string(input)
	}
	cells := 2_000_000
	if buildEditDiff("pi", "edit", arguments(501), false, &cells) != nil || cells != 2_000_000 {
		t.Fatal("over-limit comparison was not refused")
	}
	for i := 0; i < 8; i++ {
		if got := buildEditDiff("pi", "edit", arguments(500), false, &cells); got == nil || got.Added != 500 || got.Removed != 500 || !got.Truncated {
			t.Fatalf("comparison at ceiling = %+v", got)
		}
	}
	if buildEditDiff("pi", "edit", arguments(1), false, &cells) != nil || cells != 0 {
		t.Fatal("response ceiling was not enforced")
	}
}

func TestEditDiffUnqualifiedInputs(t *testing.T) {
	const args = `{"path":"a","oldText":"old","newText":"new"}`
	for _, tc := range []struct {
		agent string
		tool  ToolActivity
	}{
		{"omp", ToolActivity{Name: "edit", Input: args}},
		{"omo", ToolActivity{Name: "edit", Input: args}},
		{"pi", ToolActivity{Name: "Edit", Input: args}},
		{"pi", ToolActivity{Name: "edit", Input: args, Truncated: true}},
		{"pi", ToolActivity{Name: "edit", Input: args + " {}"}},
		{"pi", ToolActivity{Name: "edit", Input: `{"path":"a","oldText":"same","newText":"same"}`}},
		{"pi", ToolActivity{Name: "edit", Input: `{"path":"a","edits":[{"oldText":"a","newText":0}]}`}},
		{"pi", ToolActivity{Name: "edit", Input: `{"path":"a","edits":[{"oldText":"a","newText":"b","new\u0054ext":"c"}]}`}},
		{"pi", ToolActivity{Name: "edit", Input: `{"path":"a","edits":[{"path":"b","oldText":"a","newText":"b"}]}`}},
		{"claude", ToolActivity{Name: "Edit", Input: `{"file_path":"a","old_string":"a","new_string":"b","replace_all":true}`}},
		{"opencode", ToolActivity{Name: "edit", Input: `{"path":"a","filePath":"b","oldString":"a","newString":"b"}`}},
		{"opencode", ToolActivity{Name: "edit", Input: `{"filePath":"a","oldString":"a","newString":"b","replaceAll":true}`}},
		{"pi", ToolActivity{Name: "write", Input: `{"path":"\n","content":"ok"}`}},
		{"pi", ToolActivity{Name: "write", Input: `{"path":"a","content":"x\u0000"}`}},
		{"pi", ToolActivity{Name: "write", Input: `{"path":"` + strings.Repeat("日", 342) + `","content":"ok"}`}},
		{"hermes", ToolActivity{Name: "patch", Input: `{"mode":"other","path":"a","old_string":"a","new_string":"b"}`}},
	} {
		expectPreview(t, tc.agent, tc.tool, nil)
	}
}

func TestEditDiffJSONLTools(t *testing.T) {
	for _, tc := range []struct {
		agent, name, args, text string
		added, removed          int
	}{
		{"claude", "Edit", `{"file_path":"a","old_string":"old","new_string":"new"}`, "-old\n+new", 1, 1},
		{"claude", "MultiEdit", `{"file_path":"a","edits":[{"old_string":"old","new_string":"new"},{"old_string":"","new_string":"next"}]}`, "-old\n+new\n@@\n+next", 2, 1},
		{"claude", "Write", `{"file_path":"a","content":"new"}`, "+new", 1, 0},
		{"pi", "edit", `{"path":"a","oldText":"old","newText":"new"}`, "-old\n+new", 1, 1},
		{"pi", "edit", `{"path":"a","edits":[{"oldText":"old","newText":"new"}]}`, "-old\n+new", 1, 1},
		{"pi", "write", `{"path":"a","content":"new"}`, "+new", 1, 0},
		{"omp", "write", `{"path":"a","content":"new"}`, "+new", 1, 0},
	} {
		t.Run(tc.agent+"/"+tc.name, func(t *testing.T) {
			var record string
			if tc.agent == "claude" {
				record = fmt.Sprintf(`{"type":"assistant","message":{"content":[{"type":"tool_use","id":"c1","name":%q,"input":%s}]}}`, tc.name, tc.args)
				projector := newMemoryProjector(tc.agent, "r")
				hidden := strings.Replace(record, `"type":"assistant"`, `"type":"assistant","isSidechain":true`, 1)
				if got := projector.apply(JSONLRecord{Raw: []byte(hidden), Complete: true}); got.Entry != nil {
					t.Fatal("sidechain edit projected")
				}
			} else {
				record = fmt.Sprintf(`{"type":"message","message":{"role":"assistant","content":[{"type":"toolCall","id":"c1","name":%q,"arguments":%s}]}}`, tc.name, tc.args)
			}
			kind := "edit"
			if strings.EqualFold(tc.name, "write") {
				kind = "write"
			}
			expectPreview(t, tc.agent, projectedEditTool(t, tc.agent, record), &EditDiff{Kind: kind, Path: "a", Added: tc.added, Removed: tc.removed, Text: tc.text})
		})
	}
}

func TestEditDiffAmbiguousJSONLArgumentsStayRawInIndex(t *testing.T) {
	const ambiguous = `{"file_path":"a","file_path":"b","old_string":"old","new_string":"new"}`
	const valid = `{"file_path":"b","new_string":"new","old_string":"old"}`
	const record = `{"type":"assistant","message":{"content":[` +
		`{"type":"tool_use","id":"nested-duplicate","name":"Edit","input":` + ambiguous + `},` +
		`{"type":"tool_use","id":"envelope-duplicate","name":"Edit","input":{"file_path":"a","old_string":"old","new_string":"new"},"\u0069nput":` + valid + `},` +
		`{"type":"tool_use","id":"valid","name":"Edit","input":` + valid + `}]}}`
	call := JSONLRecord{Raw: []byte(record), Complete: true, End: int64(len(record))}
	result := pmBlocks(t, call.End, pmResult("nested-duplicate", "saved", false), pmResult("envelope-duplicate", "saved", false))
	memory := newMemoryProjector("claude", "revision")
	memory.apply(call)
	memory.apply(result)
	index := pmIndex(t)
	if _, err := index.applyRecords("claude", "revision", []JSONLRecord{call, result}); err != nil {
		t.Fatal(err)
	}
	indexed, more, err := index.entriesBefore(result.End, 1)
	if err != nil || more || len(indexed) != 1 {
		t.Fatalf("indexed entries=%d more=%v err=%v", len(indexed), more, err)
	}
	for name, entries := range map[string][]projectedEntry{"recent": memory.entries, "indexed": indexed} {
		t.Run(name, func(t *testing.T) {
			page := BrowsePage{Entries: projectedEntries(entries)}.WithEditDiffs("claude")
			tools := page.Entries[0].Tools
			for _, tool := range tools[:2] {
				if tool.Input != valid || tool.Output != "saved" || tool.Truncated || tool.Diff != nil {
					t.Fatalf("ambiguous edit card = %+v", tool)
				}
			}
			if diff := tools[2].Diff; diff == nil || diff.Path != "b" || diff.Text != "-old\n+new" {
				t.Fatalf("unambiguous edit in the same record lost its preview: %+v", diff)
			}
		})
	}
}

func projectedEditTool(t *testing.T, agent, record string) ToolActivity {
	t.Helper()
	projector := newMemoryProjector(agent, "r")
	result := projector.apply(JSONLRecord{Raw: []byte(record), Complete: true})
	if result.Entry == nil || len(result.Entry.Entry.Tools) != 1 {
		t.Fatalf("unexpected projection: %+v", result)
	}
	return result.Entry.Entry.Tools[0]
}

func TestEditDiffGrokLateInput(t *testing.T) {
	reader, home := testReader(t)
	f := &grokFixture{}
	f.user(0, "edit a file")
	f.toolCall("edit", "search_replace", nil)
	f.toolProgress("edit", "search_replace", map[string]any{"file_path": "a", "old_string": "old", "new_string": "new"})
	f.toolDiff("edit")
	f.write(t, grokUpdatesPath(home, grokCWD, testSessionID))
	entries := readGrok(t, reader, 10)
	if len(entries) != 2 || len(entries[1].Tools) != 1 {
		t.Fatalf("entries = %+v", entries)
	}
	expectPreview(t, "grok", entries[1].Tools[0], &EditDiff{Kind: "edit", Path: "a", Added: 1, Removed: 1, Text: "-old\n+new"})
}

func TestEditDiffGrokDuplicateInputStaysRaw(t *testing.T) {
	const first = `{"file_path":"a","old_string":"old","new_string":"new"}`
	const last = `{"file_path":"b","old_string":"old","new_string":"new"}`
	for _, event := range []string{"tool_call", "tool_call_update"} {
		t.Run(event, func(t *testing.T) {
			reader, home := testReader(t)
			f := &grokFixture{}
			if event == "tool_call_update" {
				f.toolCall("edit", "search_replace", nil)
			}
			update := json.RawMessage(fmt.Sprintf(`{"sessionUpdate":%q,"toolCallId":"edit","title":"search_replace","rawInput":%s,"rawInput":%s}`, event, first, last))
			f.rows = append(f.rows, map[string]any{"params": map[string]any{"update": update}})
			f.toolResult("edit", "completed", "saved")
			f.write(t, grokUpdatesPath(home, grokCWD, testSessionID))
			page := readGrokBrowser(t, reader, 10).WithEditDiffs("grok")
			if len(page.Entries) != 1 || len(page.Entries[0].Tools) != 1 {
				t.Fatalf("entries = %+v", page.Entries)
			}
			tool := page.Entries[0].Tools[0]
			if tool.Input != last || tool.Output != "saved" || tool.Truncated || tool.Diff != nil {
				t.Fatalf("ambiguous edit did not stay raw: %+v", tool)
			}
		})
	}
}

const multiHunkEditPatch = "*** Begin Patch\n*** Update File: src/main.go\n@@ context hint @@\n before\n-old\n+new\n@@ later\n another\n-gone\n*** End of File\n*** End Patch"

func TestEditDiffPatchCodex(t *testing.T) {
	const add = "*** Begin Patch\n*** Add File: a\n+new\n*** End Patch"
	for _, tc := range []struct {
		name, tool, patch string
		want              *EditDiff
	}{
		{"add", "apply_patch", add, &EditDiff{Kind: "edit", Path: "a", Added: 1, Text: "+new"}},
		{"update", "apply_patch", "*** Begin Patch\n*** Update File: a\n-old\n+new\n*** End Patch", &EditDiff{Kind: "edit", Path: "a", Added: 1, Removed: 1, Text: "-old\n+new"}},
		{"multiple hunks", "apply_patch", multiHunkEditPatch, &EditDiff{Kind: "edit", Path: "src/main.go", Added: 1, Removed: 2, Text: "-old\n+new\n@@\n-gone"}},
		{"incomplete", "apply_patch", strings.TrimSuffix(add, "*** End Patch"), nil},
		{"move", "apply_patch", "*** Begin Patch\n*** Update File: a\n*** Move to: b\n-old\n+new\n*** End Patch", nil},
		{"delete", "apply_patch", "*** Begin Patch\n*** Delete File: a\n*** End Patch", nil},
		{"multiple files", "apply_patch", strings.Replace(add, "*** End Patch", "*** Add File: b\n+other\n*** End Patch", 1), nil},
		{"shell", "exec_command", multiHunkEditPatch, nil},
		{"malformed body", "apply_patch", strings.Replace(add, "+new", "new", 1), nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			record := fmt.Sprintf(`{"type":"response_item","payload":{"type":"custom_tool_call","call_id":"c1","name":%q,"input":%q}}`, tc.tool, tc.patch)
			expectPreview(t, "codex", projectedEditTool(t, "codex", record), tc.want)
		})
	}
}

func TestEditDiffCodexDuplicateInputStaysRaw(t *testing.T) {
	const patch = "*** Begin Patch\n*** Add File: %s\n+new\n*** End Patch"
	first, last := fmt.Sprintf(patch, "a"), fmt.Sprintf(patch, "b")
	projector := newMemoryProjector("codex", "revision")
	call := pmRecord(0, fmt.Sprintf(`{"type":"response_item","payload":{"type":"custom_tool_call","call_id":"edit","name":"apply_patch","input":%q,"input":%q}}`, first, last))
	projector.apply(call)
	projector.apply(pmRecord(call.End, `{"type":"response_item","payload":{"type":"custom_tool_call_output","call_id":"edit","output":"saved"}}`))
	page := BrowsePage{Entries: projectedEntries(projector.entries)}.WithEditDiffs("codex")
	if len(page.Entries) != 1 || len(page.Entries[0].Tools) != 1 {
		t.Fatalf("entries = %+v", page.Entries)
	}
	tool := page.Entries[0].Tools[0]
	if tool.Input != last || tool.Output != "saved" || tool.Truncated || tool.Diff != nil {
		t.Fatalf("ambiguous edit did not stay raw: %+v", tool)
	}
}

func TestEditDiffOpenCodeLayouts(t *testing.T) {
	for _, v2 := range []bool{false, true} {
		t.Run(fmt.Sprintf("v2=%v", v2), func(t *testing.T) {
			root := t.TempDir()
			patchArgs, _ := json.Marshal(map[string]string{"patchText": multiHunkEditPatch})
			const lastInput = `{"path":"b","content":"new"}`
			inputs := []struct{ name, args string }{
				{"edit", `{"filePath":"a","oldString":"old","newString":"new"}`},
				{"write", `{"path":"a","content":"new"}`},
				{"patch", string(patchArgs)}, {"apply_patch", string(patchArgs)},
				{"write", `{"path":"a","content":"` + strings.Repeat("x", maxEntryBytes/2) + `"}`},
				{"write", `{"path":"a","content":"old"},"input":` + lastInput},
			}
			sql := openCodeV1Schema + `INSERT INTO session VALUES('ses_edits','/work','Edits',2,'build');INSERT INTO message VALUES('m1','ses_edits',1,'{"role":"assistant"}');`
			var parts []string
			for i, input := range inputs {
				state := fmt.Sprintf(`{"status":"completed","input":%s,"output":"saved"}`, input.args)
				sql += fmt.Sprintf(`INSERT INTO part VALUES('p%d','m1','{"type":"tool","tool":%q,"callID":"c%d","state":%s}');`, i, input.name, i, state)
				parts = append(parts, fmt.Sprintf(`{"type":"tool","name":%q,"id":"c%d","state":%s}`, input.name, i, state))
			}
			if v2 {
				sql = openCodeV2Schema + `INSERT INTO session_v2(id,directory) VALUES('ses_edits','/work');` +
					openCodeV2Message("m1", "ses_edits", "assistant", 1, 1, `{"content":[`+strings.Join(parts, ",")+`]}`)
			}
			createOpenCodeFixture(t, filepath.Join(root, "opencode.db"), sql)
			entries, err := openCodeFixtureReader(t, root).ReadLatest("opencode", ProjectContext{}, "ses_edits", 10)
			if err != nil || len(entries) != 1 || len(entries[0].Tools) != len(inputs) {
				t.Fatalf("entries = %+v, err = %v", entries, err)
			}
			expectPreview(t, "opencode", entries[0].Tools[0], &EditDiff{Kind: "edit", Path: "a", Added: 1, Removed: 1, Text: "-old\n+new"})
			expectPreview(t, "opencode", entries[0].Tools[1], &EditDiff{Kind: "write", Path: "a", Added: 1, Text: "+new"})
			for _, tool := range entries[0].Tools[2:4] {
				expectPreview(t, "opencode", tool, &EditDiff{Kind: "edit", Path: "src/main.go", Added: 1, Removed: 2, Text: "-old\n+new\n@@\n-gone"})
			}
			clipped := entries[0].Tools[4]
			if !clipped.Truncated || clipped.Output != "saved" || len(clipped.Input) != maxEntryBytes/2 {
				t.Fatalf("short result did not retain the input truncation flag: truncated=%v, input bytes=%d, output=%q", clipped.Truncated, len(clipped.Input), clipped.Output)
			}
			expectPreview(t, "opencode", clipped, nil)
			ambiguous := (BrowsePage{Entries: entries}).WithEditDiffs("opencode").Entries[0].Tools[5]
			if ambiguous.Input != lastInput || ambiguous.Output != "saved" || ambiguous.State != "completed" || ambiguous.Truncated || ambiguous.Diff != nil {
				t.Fatalf("ambiguous edit did not stay raw: %+v", ambiguous)
			}
		})
	}
}

func TestEditDiffHermesArguments(t *testing.T) {
	const session = "20260812_110000_edits"
	const replacement = `{"path":"a","old_string":"old","new_string":"new"}`
	const conflicting = `{"path":"a","path":"b","old_string":"old","new_string":"new"}`
	patchArgs, _ := json.Marshal(map[string]string{"mode": "patch", "patch": multiHunkEditPatch})
	calls := []map[string]any{}
	for i, tc := range []struct {
		name string
		args any
	}{
		{"write_file", map[string]string{"path": "a", "content": "new"}},
		{"patch", replacement},
		{"patch", string(patchArgs)},
		{"patch", conflicting},
		{"patch", replacement + strings.Repeat(" ", maxEntryBytes/2) + `{"extra":true}`},
		{"patch", json.RawMessage(conflicting)},
	} {
		calls = append(calls, map[string]any{"id": fmt.Sprint(i), "type": "function", "function": map[string]any{"name": tc.name, "arguments": tc.args}})
	}
	const lastArgs = `{"path":"b","old_string":"old","new_string":"new"}`
	calls = append(calls, map[string]any{"id": "6", "type": "function", "function": json.RawMessage(fmt.Sprintf(
		`{"name":"patch","arguments":%s,"arguments":%q}`, replacement, lastArgs))})
	raw, _ := json.Marshal(calls)
	browser, scope, _ := hermesBrowserFixture(t, session, fmt.Sprintf(
		`INSERT INTO messages(session_id,role,tool_calls,timestamp) VALUES('%[1]s','assistant','%[2]s',1);
		INSERT INTO messages(session_id,role,tool_call_id,content,timestamp) VALUES('%[1]s','tool','4','saved',2),('%[1]s','tool','5','saved',3),('%[1]s','tool','6','saved',4);`, session, raw))
	page, err := browser.ReadPage(t.Context(), BrowseRequest{Scope: scope, Limit: 10})
	entries := page.Entries
	if err != nil || len(entries) != 1 || len(entries[0].Tools) != len(calls) {
		t.Fatalf("entries = %+v, err = %v", entries, err)
	}
	expectPreview(t, "hermes", entries[0].Tools[0], &EditDiff{Kind: "write", Path: "a", Added: 1, Text: "+new"})
	expectPreview(t, "hermes", entries[0].Tools[1], &EditDiff{Kind: "edit", Path: "a", Added: 1, Removed: 1, Text: "-old\n+new"})
	expectPreview(t, "hermes", entries[0].Tools[2], &EditDiff{Kind: "edit", Path: "src/main.go", Added: 1, Removed: 2, Text: "-old\n+new\n@@\n-gone"})
	ambiguous := entries[0].Tools[3]
	if ambiguous.Truncated || ambiguous.Input != conflicting {
		t.Fatalf("conflicting input did not survive projection: %+v", ambiguous)
	}
	expectPreview(t, "hermes", ambiguous, nil)
	clipped := entries[0].Tools[4]
	if !clipped.Truncated || clipped.Input != replacement || clipped.Output != "saved" || clipped.State != "completed" {
		t.Fatalf("complete-looking clipped input after result attachment: %+v", clipped)
	}
	expectPreview(t, "hermes", clipped, nil)
	tools := page.WithEditDiffs("hermes").Entries[0].Tools
	for index, input := range []string{`{"new_string":"new","old_string":"old","path":"b"}`, lastArgs} {
		tool := tools[5+index]
		if tool.Input != input || tool.Truncated || tool.Diff != nil || tool.Output != "saved" || tool.State != "completed" {
			t.Fatalf("ambiguous input did not stay raw: %+v", tool)
		}
	}
}
