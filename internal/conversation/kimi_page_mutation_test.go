package conversation

import (
	"os"
	"reflect"
	"testing"
	"time"

	"github.com/0cv/herdr-mobile-relay/internal/agentroots"
)

func TestKimiPageProjectionMutationDifferential(t *testing.T) {
	root, home := t.TempDir(), t.TempDir()
	t.Setenv(agentroots.KimiCodeListEnv, root)
	t.Setenv("KIMI_CODE_HOME", "")
	cwd, id := "/fixture/kimi-mutations", "session_mutations"
	base := string(kimiSample(t, "rewrites/before.jsonl"))
	rewrite := string(kimiSample(t, "rewrites/after.jsonl"))
	answer := "{\"type\":\"context.append_message\",\"message\":{\"role\":\"assistant\",\"content\":\"Stable recent answer\"}}\n"
	path := kimiFixture(t, root, kimiWorkspaceKey(cwd), id, cwd, "Mutations", 1, []byte(base+answer))
	browser, err := NewBrowser(NewReader(home), t.TempDir(), DefaultBrowserOptions())
	if err != nil {
		t.Fatal(err)
	}
	defer browser.Close()
	referenceReader := NewReader(home)
	scope := BrowseScope{Provider: "kimi", CWD: cwd, SessionID: id, PaneID: "pane", ProcessIdentity: "1:test"}
	cursor, cursorRevision := "", ""
	wires := []string{base + answer, base + answer + answer[:len(answer)-2], base + answer + answer, rewrite + answer + answer, rewrite + answer + answer + "{\"type\":\"context.undo\",\"count\":1}\n", base + answer + "{\"type\":\"context.apply_compaction\",\"keptUserMessageCount\":1}\n", base + answer + "{\"type\":\"context.clear\"}\n{\"type\":\"context.undo\",\"count\":1}\n"}
	for i, wire := range wires {
		before, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(wire), 0600); err != nil {
			t.Fatal(err)
		}
		if i == 3 {
			if err := os.Chtimes(path, time.Now(), before.ModTime()); err != nil {
				t.Fatal(err)
			}
		}
		reference := referenceProjectKimi(t.Context(), []byte(wire))
		page, err := browser.ReadPage(t.Context(), BrowseRequest{Scope: scope, Limit: 1})
		if err != nil || page.ReasonCode != reference.code {
			t.Fatalf("mutation %d: page=%+v err=%v expected=%s", i, page, err, reference.code)
		}
		if reference.code != "" {
			if page.Available || len(page.Entries) != 0 {
				t.Fatal("invalid mutation exported content")
			}
			continue
		}
		info, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		revision := referenceReader.kimiRevision(Location{Path: path, Root: root}, info, kimiProjection{complete: reference.complete, mutation: reference.mutation})
		if page.SourceRevision != revision || page.Diagnostics != reference.diagnostics {
			t.Fatalf("mutation %d metadata differs", i)
		}
		start := max(0, len(reference.entries)-1)
		responseEntries := make([]Entry, len(reference.entries)-start)
		for j, entry := range reference.entries[start:] {
			responseEntries[j] = cloneConversationEntry(entry)
		}
		normalizeEntriesForResponse(responseEntries)
		if !reflect.DeepEqual(page.Entries, responseEntries) {
			t.Fatalf("mutation %d rows differ", i)
		}
		if cursor != "" {
			older, err := browser.ReadPage(t.Context(), BrowseRequest{Scope: scope, Limit: 1, Cursor: cursor})
			if err != nil {
				t.Fatal(err)
			}
			if cursorRevision != revision && older.ReasonCode != "source_changed" {
				t.Fatal("mutation accepted stale cursor")
			}
			if cursorRevision == revision && (!older.Available || older.ReasonCode != "") {
				t.Fatal("append invalidated stable older cursor")
			}
		}
		if page.HasMore {
			cursor, cursorRevision = page.NextCursor, revision
		}
	}
}
