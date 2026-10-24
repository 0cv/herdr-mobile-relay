package conversation

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"reflect"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/0cv/herdr-mobile-relay/internal/agentroots"
)

func kimiPageCorpus(t *testing.T) map[string][]byte {
	t.Helper()
	header := "{\"type\":\"metadata\",\"protocol_version\":\"1.5\"}\n"
	message := func(role string, value any) string {
		raw, _ := json.Marshal(map[string]any{"type": "context.append_message", "message": map[string]any{"role": role, "content": value}})
		return string(raw) + "\n"
	}
	cases := map[string][]byte{
		"messages":         []byte(header + message("user", "Prompt") + message("assistant", "Answer")),
		"empty-tail":       []byte(header + message("user", "Prompt") + strings.Repeat(message("assistant", " \r\u0000\x1b[0m"), 70)),
		"empty-user":       []byte(header + message("user", nil) + message("user", []any{map[string]any{"type": "text", "text": ""}, map[string]any{"type": "text", "text": ""}})),
		"long-unicode":     []byte(header + message("assistant", strings.Repeat(" café\x1b[32m", 20000))),
		"incomplete":       []byte(header + message("assistant", "Complete") + "{\"type\":\"context.append_message\",\"message\":\"caf\xc3"),
		"bad-json":         []byte(header + "{broken}\n"),
		"bad-utf8":         []byte(header + "{\"type\":\"x\",\"text\":\"\xff\"}\n"),
		"deep":             []byte(header + strings.Repeat("[", 33) + "0" + strings.Repeat("]", 33) + "\n"),
		"unsupported":      []byte(header + "{\"type\":\"context.replace\"}\n"),
		"oversized-record": []byte(header + message("assistant", strings.Repeat("x", maxKimiRecord))),
		"record-limit":     []byte(header + strings.Repeat("{\"type\":\"turn.ended\"}\n", maxKimiRecords)),
		"duplicate-call":   []byte(header + "{\"type\":\"context.append_message\",\"message\":{\"role\":\"assistant\",\"toolCalls\":[{\"type\":\"function\",\"id\":\"t\",\"name\":\"run\"},{\"type\":\"function\",\"id\":\"t\",\"name\":\"run\"}]}}\n"),
	}
	var manifest struct{ Cases []struct{ ID, Wire string } }
	if err := json.Unmarshal(kimiSample(t, "manifest.json"), &manifest); err != nil {
		t.Fatal(err)
	}
	for _, sample := range manifest.Cases {
		if sample.Wire != "" {
			cases["golden-"+sample.ID] = kimiSample(t, sample.Wire)
		}
	}
	for _, file := range []string{"v0.36.1-v2/wire.jsonl", "v0.36.1-mutations/wire.jsonl", "v2.2.0-branches/wire.jsonl", "rewrites/before.jsonl", "rewrites/after.jsonl"} {
		wire := kimiSample(t, file)
		cases[file] = wire
		end := 0
		for i, line := range bytes.SplitAfter(wire, []byte("\n")) {
			if len(line) == 0 {
				continue
			}
			end += len(line)
			cases[fmt.Sprintf("%s-prefix-%d", file, i)] = wire[:end]
		}
	}
	var journal strings.Builder
	journal.WriteString(header)
	for i := 0; i < 35; i++ {
		fmt.Fprintf(&journal, "{\"type\":\"context.append_loop_event\",\"event\":{\"type\":\"step.begin\",\"uuid\":\"s%d\"}}\n", i)
		fmt.Fprintf(&journal, "{\"type\":\"context.append_loop_event\",\"event\":{\"type\":\"content.part\",\"stepUuid\":\"s%d\",\"part\":{\"type\":\"text\",\"text\":\"part %d\"}}}\n", i, i)
		fmt.Fprintf(&journal, "{\"type\":\"context.append_loop_event\",\"event\":{\"type\":\"tool.call\",\"stepUuid\":\"s%d\",\"toolCallId\":\"t%d\",\"name\":\"run\",\"args\":{\"text\":\"café\",\"index\":%d}}}\n", i, i, i)
	}
	for i := 34; i >= 0; i-- {
		fmt.Fprintf(&journal, "{\"type\":\"context.append_loop_event\",\"event\":{\"type\":\"tool.result\",\"toolCallId\":\"t%d\",\"result\":{\"output\":[{\"type\":\"text\",\"text\":\"result %d\"}],\"isError\":%t}}}\n", i, i, i%3 == 0)
		fmt.Fprintf(&journal, "{\"type\":\"context.append_loop_event\",\"event\":{\"type\":\"step.end\",\"uuid\":\"s%d\"}}\n", i)
	}
	cases["delayed-cross-page-steps"] = []byte(journal.String())
	toolTail := header
	for i := 0; i < 70; i++ {
		toolTail += fmt.Sprintf("{\"type\":\"context.append_message\",\"message\":{\"role\":\"assistant\",\"content\":\"\",\"toolCalls\":[{\"type\":\"function\",\"id\":\"t%d\",\"name\":\"run\",\"arguments\":\"{}\"}]}}\n", i)
		toolTail += fmt.Sprintf("{\"type\":\"context.append_message\",\"message\":{\"role\":\"tool\",\"toolCallId\":\"t%d\",\"content\":\"result\"}}\n", i)
	}
	cases["tool-only-tail"] = []byte(toolTail)
	for _, origin := range []string{"user", "hook", "plugin"} {
		cases["origin-"+origin] = []byte(header + fmt.Sprintf("{\"type\":\"context.append_message\",\"message\":{\"role\":\"user\",\"content\":\"Hidden?\",\"origin\":{\"kind\":%q}}}\n", origin) + message("assistant", "Visible") + "{\"type\":\"context.undo\",\"count\":1}\n")
	}
	return cases
}

func TestKimiPageProjectionDifferential(t *testing.T) {
	root, home := t.TempDir(), t.TempDir()
	t.Setenv(agentroots.KimiCodeListEnv, root)
	t.Setenv("KIMI_CODE_HOME", "")
	cwd, id := "/fixture/kimi-pages", "session_differential"
	for name, wire := range kimiPageCorpus(t) {
		t.Run(name, func(t *testing.T) {
			reference := referenceProjectKimi(t.Context(), wire)
			projection := projectKimi(t.Context(), wire)
			if !reflect.DeepEqual(projection.entries, reference.entries) || projection.code != reference.code || projection.diagnostics != reference.diagnostics || projection.mutation != reference.mutation || !bytes.Equal(projection.complete, reference.complete) {
				t.Fatalf("projector differs: new=%+v old=%+v", projection, reference)
			}
			kimiFixture(t, root, kimiWorkspaceKey(cwd), id, cwd, "Differential", 1, wire)
			browser, err := NewBrowser(NewReader(home), t.TempDir(), DefaultBrowserOptions())
			if err != nil {
				t.Fatal(err)
			}
			defer browser.Close()
			scope := BrowseScope{Provider: "kimi", CWD: cwd, SessionID: id, PaneID: "pane", ProcessIdentity: "1:test"}
			var work kimiMaterializationWork
			browser.kimiMaterializationObserver = func(value kimiMaterializationWork) { work = value }
			cursor, remaining, revision := "", len(reference.entries), ""
			seen := make(map[string]bool)
			for {
				page, err := browser.ReadPage(t.Context(), BrowseRequest{Scope: scope, Limit: 7, Cursor: cursor})
				if err != nil || page.ReasonCode != reference.code {
					t.Fatalf("page=%+v err=%v expected=%s", page, err, reference.code)
				}
				if reference.code != "" {
					if page.Available || len(page.Entries) != 0 {
						t.Fatal("rejected source exported content")
					}
					break
				}
				if !page.Available || page.Diagnostics != reference.diagnostics || page.Total == nil || *page.Total != len(reference.entries) {
					t.Fatalf("page metadata=%+v", page)
				}
				if revision != "" && revision != page.SourceRevision {
					t.Fatal("unchanged traversal changed revision")
				}
				revision = page.SourceRevision
				start := max(0, remaining-7)
				if name == "delayed-cross-page-steps" && work.Records != 4*(remaining-start) {
					t.Fatalf("materialized unrelated steps: %+v", work)
				}
				if name == "tool-only-tail" && work.Records != 2*(remaining-start) {
					t.Fatalf("materialized unrelated tools: %+v", work)
				}
				responseEntries := make([]Entry, remaining-start)
				for i, entry := range reference.entries[start:remaining] {
					responseEntries[i] = cloneConversationEntry(entry)
				}
				normalizeEntriesForResponse(responseEntries)
				if !reflect.DeepEqual(page.Entries, responseEntries) {
					t.Fatalf("page rows differ: %+v != %+v", page.Entries, responseEntries)
				}
				for _, entry := range page.Entries {
					if seen[entry.ID] {
						t.Fatal("duplicate entry")
					}
					seen[entry.ID] = true
				}
				remaining = start
				if page.HasMore != (remaining > 0) {
					t.Fatal("incomplete cursor traversal")
				}
				if !page.HasMore {
					break
				}
				if page.NextCursor == "" || page.NextCursor == cursor {
					t.Fatal("cursor did not advance")
				}
				cursor = page.NextCursor
			}
			if reference.code == "" && (remaining != 0 || len(seen) != len(reference.entries)) {
				t.Fatal("gap in complete traversal")
			}
		})
	}
}

func TestKimiTextShapeMatchesDisplayNormalization(t *testing.T) {
	pieces := []string{"", " ", "\r\x00", "café", "\x1b[31mred\x1b[0m", "\x1b]hidden", "\x07", "\u2003", "\x1b[?25l", "\x1b(0", "\x1b>", "\x1b[3\r1m", "\x1b[3\x001m", strings.Repeat(" x", maxEntryBytes)}
	for _, a := range pieces {
		for _, b := range pieces {
			parts := []string{a, "\n", b}
			want, _ := clampText(sanitizeText(strings.Join(parts, "")), maxEntryBytes)
			if got := strings.Join(kimiTextShape(parts), ""); got != want {
				t.Fatalf("shape=%q want=%q", got, want)
			}
			var shape []string
			body := ""
			for _, part := range []string{a, b, "\x07 final"} {
				if part == "" {
					continue
				}
				if body != "" {
					body += "\n"
				}
				body, _ = clampText(sanitizeText(body+part), maxEntryBytes)
				if len(shape) > 0 {
					shape = append(shape, "\n")
				}
				shape = kimiTextShape(append(shape, part))
				if got := strings.Join(shape, ""); got != body {
					t.Fatalf("incremental=%q want=%q", got, body)
				}
			}
		}
	}
}

func kimiAllocated(run func()) uint64 {
	best := ^uint64(0)
	for i := 0; i < 3; i++ {
		runtime.GC()
		var before, after runtime.MemStats
		runtime.ReadMemStats(&before)
		run()
		runtime.ReadMemStats(&after)
		best = min(best, after.TotalAlloc-before.TotalAlloc)
	}
	return best
}

func TestKimiPageProjectionWorkAndMemory(t *testing.T) {
	for _, test := range []struct {
		count, payload int
		tools          bool
	}{{384, 160, false}, {4096, 160, false}, {12000, 160, false}, {2048, 2048, false}, {1024, 2048, true}} {
		t.Run(fmt.Sprintf("%d-%d-tools-%t", test.count, test.payload, test.tools), func(t *testing.T) {
			wire := kimiWorkJournal(test.count, test.payload, test.tools)
			oldStart := time.Now()
			reference := referenceProjectKimi(t.Context(), wire)
			oldElapsed := time.Since(oldStart)
			start := time.Now()
			plan := validateKimi(t.Context(), wire)
			entries, work, code := plan.materialize(t.Context(), len(plan.entries)-24, len(plan.entries))
			elapsed := time.Since(start)
			if code != "" || plan.metadata.code != "" || !reflect.DeepEqual(entries, reference.entries[len(reference.entries)-24:]) {
				t.Fatal("measured projection differs")
			}
			recordsPerEntry := 1
			if test.tools {
				recordsPerEntry = 2
			}
			if len(plan.records) != 1+recordsPerEntry*test.count || work.Entries != 24 || work.Records != recordsPerEntry*24 || work.PayloadBytes > 24*(2*test.payload+256) || plan.retainedBytes() > int64(cap(wire)+test.count*512+len(plan.records)*128+4096) {
				t.Fatalf("unbounded page work: %+v retained=%d", work, plan.retainedBytes())
			}
			for _, record := range plan.records {
				if record.raw != nil {
					t.Fatal("decoded payload retained by validated plan")
				}
			}
			oldAlloc := kimiAllocated(func() { result := referenceProjectKimi(t.Context(), wire); runtime.KeepAlive(result) })
			newAlloc := kimiAllocated(func() {
				result := validateKimi(t.Context(), wire)
				page, _, _ := result.materialize(t.Context(), len(result.entries)-24, len(result.entries))
				runtime.KeepAlive(result)
				runtime.KeepAlive(page)
			})
			if test.payload >= 2048 && newAlloc*100 >= oldAlloc*90 {
				t.Fatalf("content-heavy allocations not reduced by 10%%: new=%d old=%d", newAlloc, oldAlloc)
			}
			root, home := t.TempDir(), t.TempDir()
			t.Setenv(agentroots.KimiCodeListEnv, root)
			t.Setenv("KIMI_CODE_HOME", "")
			cwd, id := "/fixture/kimi-work", "session_work"
			kimiFixture(t, root, kimiWorkspaceKey(cwd), id, cwd, "Work", 1, wire)
			browser, err := NewBrowser(NewReader(home), t.TempDir(), DefaultBrowserOptions())
			if err != nil {
				t.Fatal(err)
			}
			defer browser.Close()
			var physical int64
			validations := 0
			browser.physicalReadObserver = func(n int64) { physical += n }
			browser.kimiProjectionObserver = func() { validations++ }
			browser.kimiMaterializationObserver = func(value kimiMaterializationWork) { work = value }
			scope := BrowseScope{Provider: "kimi", CWD: cwd, SessionID: id, PaneID: "pane"}
			read := func(cursor string) BrowsePage {
				t.Helper()
				page, err := browser.ReadPage(t.Context(), BrowseRequest{Scope: scope, Limit: 24, Cursor: cursor})
				if err != nil || !page.Available {
					t.Fatalf("page=%+v err=%v", page, err)
				}
				return page
			}
			first := read("")
			coldBytes := physical
			if coldBytes != int64(len(wire)) || validations != 1 || browser.kimiProjection == nil {
				t.Fatal("cold validation/cache mismatch")
			}
			physical = 0
			warmStart := time.Now()
			warm := read("")
			warmElapsed := time.Since(warmStart)
			if physical != 0 || validations != 1 || !reflect.DeepEqual(first.Entries, warm.Entries) || work.Records != 24*recordsPerEntry {
				t.Fatal("warm page repeated whole-journal work")
			}
			older := read(first.NextCursor)
			if physical != 0 || validations != 1 || len(older.Entries) != 24 || work.Records != 24*recordsPerEntry {
				t.Fatal("older page repeated whole-journal work")
			}
			t.Logf("KIMI_PAGE_MEASUREMENT count=%d payload=%d tools=%t physical_cold=%d physical_warm=%d structural_records=%d materialized_records=%d materialized_payload_bytes=%d old_alloc_bytes=%d new_alloc_bytes=%d retained_plan_bytes=%d old_elapsed_us=%d new_elapsed_us=%d warm_elapsed_us=%d", test.count, test.payload, test.tools, coldBytes, physical, len(plan.records), work.Records, work.PayloadBytes, oldAlloc, newAlloc, browser.kimiProjection.projection.retainedBytes(), oldElapsed.Microseconds(), elapsed.Microseconds(), warmElapsed.Microseconds())
		})
	}
}

func TestKimiPageProjectionConcurrentCacheAndClose(t *testing.T) {
	root, home := t.TempDir(), t.TempDir()
	t.Setenv(agentroots.KimiCodeListEnv, root)
	t.Setenv("KIMI_CODE_HOME", "")
	cwd, id := "/fixture/kimi-race", "session_race"
	path := kimiFixture(t, root, kimiWorkspaceKey(cwd), id, cwd, "Race", 1, kimiWorkJournal(384, 160, true))
	browser, err := NewBrowser(NewReader(home), t.TempDir(), DefaultBrowserOptions())
	if err != nil {
		t.Fatal(err)
	}
	defer browser.Close()
	scope := BrowseScope{Provider: "kimi", CWD: cwd, SessionID: id, PaneID: "pane"}
	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Go(func() {
			for j := 0; j < 8; j++ {
				page, err := browser.ReadPage(t.Context(), BrowseRequest{Scope: scope, Limit: 24})
				if err != nil || !page.Available || len(page.Entries) != 24 {
					t.Errorf("page=%+v err=%v", page, err)
					return
				}
				if len(page.Entries[0].Tools) != 1 || page.Entries[0].Tools[0].Name != "run" {
					t.Error("page reused caller-mutated tool activity")
					return
				}
				page.Entries[0].Tools[0].Name = "caller mutation"
			}
		})
	}
	wg.Wait()
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if page, err := browser.ReadPage(ctx, BrowseRequest{Scope: scope, Limit: 24}); err != nil || page.ReasonCode != "request_cancelled" {
		t.Fatalf("cancel=%+v err=%v", page, err)
	}
	updated := kimiWorkJournal(385, 160, true)
	if err := os.WriteFile(path, updated, 0600); err != nil {
		t.Fatal(err)
	}
	expected := referenceProjectKimi(t.Context(), updated)
	if page, err := browser.ReadPage(t.Context(), BrowseRequest{Scope: scope, Limit: 24}); err != nil || !page.Available || page.Entries[len(page.Entries)-1].ID != expected.entries[len(expected.entries)-1].ID {
		t.Fatalf("live append=%+v err=%v", page, err)
	}
	browser.Close()
	if browser.kimiProjection != nil {
		t.Fatal("close retained source/display data")
	}
}
