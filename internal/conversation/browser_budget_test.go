package conversation

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

const (
	budgetFillBytes = 120 * 1024
	hugeToolCount   = 16
	hugeToolInput   = 60 * 1024
	hugeToolOutput  = 120 * 1024
	budgetChildID   = "123e4567-e89b-12d3-a456-426614174001"
)

type budgetEntry struct {
	label string
	text  string
	tools int
}

func budgetText(label string, size int) string {
	return label + ":" + strings.Repeat("x", size-len(label)-1)
}

func largeBudgetEntries(count int) []budgetEntry {
	entries := make([]budgetEntry, 0, count)
	for index := 0; index < count; index++ {
		label := fmt.Sprintf("%02d", index)
		entries = append(entries, budgetEntry{label: label, text: budgetText(label, budgetFillBytes)})
	}
	return entries
}

func oversizedBudgetEntries() []budgetEntry {
	entries := make([]budgetEntry, 0, 4)
	for index := 0; index < 3; index++ {
		label := fmt.Sprintf("small%d", index)
		entries = append(entries, budgetEntry{label: label, text: budgetText(label, 1024)})
	}
	return append(entries, budgetEntry{label: "huge", text: "huge:", tools: hugeToolCount})
}

func entryLabel(entry Entry) string {
	label, _, _ := strings.Cut(entry.Text, ":")
	return label
}

func entryLabels(entries []Entry) []string {
	labels := make([]string, 0, len(entries))
	for _, entry := range entries {
		labels = append(labels, entryLabel(entry))
	}
	return labels
}

func claudeBudgetRows(prefix string, entries []budgetEntry) []map[string]any {
	rows := make([]map[string]any, 0, len(entries))
	for index, entry := range entries {
		uuid := fmt.Sprintf("%s-%d", prefix, index)
		if entry.tools == 0 {
			rows = append(rows, map[string]any{"type": "assistant", "uuid": uuid, "message": map[string]any{"content": entry.text}})
			continue
		}
		calls := []any{map[string]any{"type": "text", "text": entry.text}}
		results := make([]any, 0, entry.tools)
		for tool := 0; tool < entry.tools; tool++ {
			id := fmt.Sprintf("%s-tool-%d", uuid, tool)
			calls = append(calls, map[string]any{"type": "tool_use", "id": id, "name": "Read", "input": strings.Repeat("i", hugeToolInput)})
			results = append(results, map[string]any{"type": "tool_result", "tool_use_id": id, "content": strings.Repeat("o", hugeToolOutput)})
		}
		rows = append(rows,
			map[string]any{"type": "assistant", "uuid": uuid, "message": map[string]any{"content": calls}},
			map[string]any{"type": "user", "uuid": uuid + "-results", "message": map[string]any{"content": results}},
		)
	}
	return rows
}

// A Claude file whose last 64 KiB lies inside one huge record cannot be
// checked for continuation links, so its source ends with a small record that
// is not a conversation entry.
func claudeBudgetTail() map[string]any {
	return map[string]any{"type": "summary", "summary": "end"}
}

func projectClaudeFile(t *testing.T, path string) ([]Entry, BrowseDiagnostics) {
	t.Helper()
	file, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	projector := newMemoryProjector("claude", "revision")
	diagnostics := BrowseDiagnostics{}
	reader := bufio.NewReaderSize(file, 1024*1024)
	var offset int64
	for {
		line, readErr := reader.ReadBytes('\n')
		if len(line) > 0 {
			result := projector.apply(JSONLRecord{Start: offset, End: offset + int64(len(line)), Raw: line, Complete: true})
			diagnostics.OmittedTools += result.Diagnostics.OmittedTools
			diagnostics.OmittedPayloads += result.Diagnostics.OmittedPayloads
			offset += int64(len(line))
		}
		if readErr != nil {
			break
		}
	}
	entries := make([]Entry, 0, len(projector.entries))
	for _, projected := range projector.entries {
		entries = append(entries, projected.Entry)
	}
	return entries, diagnostics
}

type budgetFixture struct {
	browser *Browser
	scope   BrowseScope
	entries []budgetEntry
	project func(t *testing.T) ([]Entry, BrowseDiagnostics)
}

func claudeFileBudgetFixture(t *testing.T, recentBytes int64, entries []budgetEntry) budgetFixture {
	t.Helper()
	reader, home := testReader(t)
	path := filepath.Join(home, ".claude", "projects", "-work", testSessionID+".jsonl")
	writeRows(t, path, append(claudeBudgetRows("e", entries), claudeBudgetTail())...)
	options := DefaultBrowserOptions()
	options.RecentBytes = recentBytes
	return budgetFixture{
		browser: newTestBrowser(t, reader, options),
		scope:   BrowseScope{Provider: "claude", CWD: "/work", SessionID: testSessionID},
		entries: entries,
		project: func(t *testing.T) ([]Entry, BrowseDiagnostics) { return projectClaudeFile(t, path) },
	}
}

func claudeChainBudgetFixture(t *testing.T, recentBytes int64, entries []budgetEntry) budgetFixture {
	t.Helper()
	reader, home := testReader(t)
	root := filepath.Join(home, ".claude", "projects", "-work")
	parentPath := filepath.Join(root, testSessionID+".jsonl")
	childPath := filepath.Join(root, budgetChildID+".jsonl")
	split := len(entries) / 2
	parentRows := append(claudeBudgetRows("p", entries[:split]),
		map[string]any{"type": "continued-in", "sessionId": testSessionID, "continuedInSessionId": budgetChildID})
	writeRows(t, parentPath, parentRows...)
	writeRows(t, childPath, append(claudeBudgetRows("c", entries[split:]), claudeBudgetTail())...)
	options := DefaultBrowserOptions()
	options.RecentBytes = recentBytes
	return budgetFixture{
		browser: newTestBrowser(t, reader, options),
		scope:   BrowseScope{Provider: "claude", CWD: "/work", SessionID: testSessionID},
		entries: entries,
		project: func(t *testing.T) ([]Entry, BrowseDiagnostics) {
			parent, parentDiagnostics := projectClaudeFile(t, parentPath)
			child, childDiagnostics := projectClaudeFile(t, childPath)
			parentDiagnostics.OmittedTools += childDiagnostics.OmittedTools
			parentDiagnostics.OmittedPayloads += childDiagnostics.OmittedPayloads
			return append(parent, child...), parentDiagnostics
		},
	}
}

func grokBudgetFixture(t *testing.T, entries []budgetEntry) budgetFixture {
	t.Helper()
	reader, home := testReader(t)
	source := &grokFixture{}
	for index, entry := range entries {
		if entry.tools == 0 && index%2 == 0 {
			source.user(index, entry.text)
			continue
		}
		source.agent(entry.text)
		for tool := 0; tool < entry.tools; tool++ {
			id := fmt.Sprintf("tool-%d-%d", index, tool)
			source.toolCall(id, "read_file", map[string]any{"data": strings.Repeat("i", hugeToolInput)})
			source.toolResult(id, "completed", strings.Repeat("o", hugeToolOutput))
		}
	}
	source.write(t, grokUpdatesPath(home, grokCWD, testSessionID))
	return budgetFixture{
		browser: newTestBrowser(t, reader, DefaultBrowserOptions()),
		scope:   BrowseScope{Provider: "grok", CWD: grokCWD, SessionID: testSessionID},
		entries: entries,
		project: func(t *testing.T) ([]Entry, BrowseDiagnostics) {
			projected, err := reader.ReadLatest("grok", ProjectContext{CWD: grokCWD}, testSessionID, 200)
			if err != nil {
				t.Fatal(err)
			}
			entries := append([]Entry(nil), projected...)
			return entries, normalizeEntriesForResponse(entries)
		},
	}
}

type budgetPath struct {
	name string
	// prepared is true when the pages come from a prepared index.
	prepared bool
	// omissionsCounted is false on the chain pages built from a recent tail,
	// which never count what bounding an entry removed.
	omissionsCounted bool
	build            func(t *testing.T, entries []budgetEntry) budgetFixture
}

func budgetPaths() []budgetPath {
	const wholeFile = 8 * 1024 * 1024
	return []budgetPath{
		{"recent window", false, true, func(t *testing.T, entries []budgetEntry) budgetFixture {
			return claudeFileBudgetFixture(t, wholeFile, entries)
		}},
		{"prepared snapshot", true, true, func(t *testing.T, entries []budgetEntry) budgetFixture {
			return claudeFileBudgetFixture(t, 1, entries)
		}},
		{"native", false, true, func(t *testing.T, entries []budgetEntry) budgetFixture {
			return grokBudgetFixture(t, entries)
		}},
		{"chain recent window", false, false, func(t *testing.T, entries []budgetEntry) budgetFixture {
			return claudeChainBudgetFixture(t, wholeFile, entries)
		}},
		{"chain prepared segments", true, true, func(t *testing.T, entries []budgetEntry) budgetFixture {
			return claudeChainBudgetFixture(t, 1, entries)
		}},
	}
}

func (f budgetFixture) preparedJobs() int {
	f.browser.mu.Lock()
	defer f.browser.mu.Unlock()
	return len(f.browser.jobs)
}

func (f budgetFixture) labels() []string {
	labels := make([]string, 0, len(f.entries))
	for _, entry := range f.entries {
		labels = append(labels, entry.label)
	}
	return labels
}

// walkBrowsePages reads a source from its newest page to its oldest and
// returns the pages that were not preparing. Every page must fit the response
// budget and the entries, oldest to newest, must be exactly want.
func walkBrowsePages(t *testing.T, browser *Browser, scope BrowseScope, want []string) []BrowsePage {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
	defer cancel()
	var pages []BrowsePage
	var got []string
	cursor := ""
	for ctx.Err() == nil {
		page, err := browser.ReadPage(ctx, BrowseRequest{Scope: scope, Cursor: cursor, Limit: 100})
		if err != nil {
			t.Fatal(err)
		}
		if page.State == BrowsePreparing {
			cursor = page.NextCursor
			time.Sleep(time.Millisecond)
			continue
		}
		if page.State != BrowseReady || page.Error != nil {
			t.Fatalf("page %d = state %q, reason %q, error %#v", len(pages), page.State, page.Reason, page.Error)
		}
		if page.Diagnostics.ContinuationIncomplete {
			t.Fatalf("page %d reports an incomplete continuation chain: %q", len(pages), page.Diagnostics.ContinuationReason)
		}
		encoded, err := json.Marshal(page)
		if err != nil {
			t.Fatal(err)
		}
		if len(encoded) > browseResponseBytes {
			t.Fatalf("page %d marshals to %d bytes, over the %d byte budget", len(pages), len(encoded), browseResponseBytes)
		}
		pages = append(pages, page)
		got = append(entryLabels(page.Entries), got...)
		if !page.HasMore {
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("walk read %v, want %v", got, want)
			}
			return pages
		}
		if page.NextCursor == "" || page.NextCursor == cursor {
			t.Fatalf("page %d has more history but its cursor did not advance", len(pages)-1)
		}
		cursor = page.NextCursor
	}
	t.Fatalf("walk did not finish; read %v, want %v", got, want)
	return nil
}

func firstPageWithEntries(t *testing.T, pages []BrowsePage) int {
	t.Helper()
	for index, page := range pages {
		if len(page.Entries) > 0 {
			return index
		}
	}
	t.Fatal("no page has entries")
	return -1
}

func requireNextPageEndsBefore(t *testing.T, pages []BrowsePage, first int, entries []budgetEntry, kept int) {
	t.Helper()
	if !pages[first].HasMore || pages[first].NextCursor == "" {
		t.Fatalf("first page has no continuation: has_more=%v cursor=%q", pages[first].HasMore, pages[first].NextCursor)
	}
	if first+1 >= len(pages) || len(pages[first+1].Entries) == 0 {
		t.Fatalf("no page with entries follows the first page")
	}
	next := pages[first+1].Entries
	if got, want := entryLabel(next[len(next)-1]), entries[len(entries)-kept-1].label; got != want {
		t.Fatalf("next page ends with %q, want the newest dropped entry %q", got, want)
	}
}

func TestResponseBudgetDropsOldestEntriesWhole(t *testing.T) {
	entries := largeBudgetEntries(40)
	for _, path := range budgetPaths() {
		t.Run(path.name, func(t *testing.T) {
			fixture := path.build(t, entries)
			pages := walkBrowsePages(t, fixture.browser, fixture.scope, fixture.labels())
			if (fixture.preparedJobs() > 0) != path.prepared {
				t.Fatalf("prepared jobs = %d, want prepared=%v", fixture.preparedJobs(), path.prepared)
			}
			first := firstPageWithEntries(t, pages)
			page := pages[first]
			kept := len(page.Entries)
			if kept == len(entries) {
				t.Fatalf("first page kept all %d entries; the budget never cut it", kept)
			}
			for index, entry := range page.Entries {
				want := entries[len(entries)-kept+index]
				if entry.Text != want.text || entry.Truncated || len(entry.Tools) != 0 {
					t.Fatalf("first page entry %d = %q (%d bytes, truncated=%v), want entry %q whole", index, entryLabel(entry), len(entry.Text), entry.Truncated, want.label)
				}
			}
			if page.Diagnostics.OmittedTools != 0 || page.Diagnostics.OmittedPayloads != 0 {
				t.Fatalf("first page omissions = %#v, want none", page.Diagnostics)
			}
			requireNextPageEndsBefore(t, pages, first, entries, kept)
		})
	}
}

func TestResponseBudgetBoundsOneOversizedEntry(t *testing.T) {
	entries := oversizedBudgetEntries()
	for _, path := range budgetPaths() {
		t.Run(path.name, func(t *testing.T) {
			fixture := path.build(t, entries)
			projected, projectedDiagnostics := fixture.project(t)
			var huge Entry
			for _, entry := range projected {
				if entryLabel(entry) == "huge" {
					huge = entry
				}
			}
			encoded, err := json.Marshal(huge)
			if err != nil {
				t.Fatal(err)
			}
			if len(huge.Tools) != hugeToolCount || len(encoded) <= browseResponseBytes {
				t.Fatalf("projected entry has %d tools and marshals to %d bytes, want %d tools over the budget", len(huge.Tools), len(encoded), hugeToolCount)
			}
			if huge.Truncated || projectedDiagnostics.OmittedTools != 0 || projectedDiagnostics.OmittedPayloads != 0 {
				t.Fatalf("projection already cut the entry: truncated=%v diagnostics=%#v", huge.Truncated, projectedDiagnostics)
			}

			pages := walkBrowsePages(t, fixture.browser, fixture.scope, fixture.labels())
			first := firstPageWithEntries(t, pages)
			page := pages[first]
			if len(page.Entries) != 1 || entryLabel(page.Entries[0]) != "huge" {
				t.Fatalf("first page holds %v, want only the oversized entry", entryLabels(page.Entries))
			}
			bounded := page.Entries[0]
			if !bounded.Truncated || len(bounded.Tools) >= hugeToolCount {
				t.Fatalf("oversized entry truncated=%v with %d tools, want it cut", bounded.Truncated, len(bounded.Tools))
			}
			wantTools, wantPayloads := hugeToolCount-len(bounded.Tools), 1
			if !path.omissionsCounted {
				wantTools, wantPayloads = 0, 0
			}
			if page.Diagnostics.OmittedTools != wantTools || page.Diagnostics.OmittedPayloads != wantPayloads {
				t.Fatalf("omissions = tools %d, payloads %d; want tools %d, payloads %d (kept %d tools; diagnostics %#v, projected %#v)", page.Diagnostics.OmittedTools, page.Diagnostics.OmittedPayloads, wantTools, wantPayloads, len(bounded.Tools), page.Diagnostics, projectedDiagnostics)
			}
			requireNextPageEndsBefore(t, pages, first, entries, 1)
		})
	}
}
