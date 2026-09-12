package conversation

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// Regenerate the sanitized frontend contract from actual Browser pages:
// HERDR_CLAUDE_CONTROLLER_FIXTURE=<path> go test ./internal/conversation -run '^TestClaudeFirstContinuationControllerFixture$' -count=1
// Signed cursor bytes are fixture-only and the source lives in t.TempDir().
func TestClaudeFirstContinuationControllerFixture(t *testing.T) {
	reader, home := testReader(t)
	root := filepath.Join(home, ".claude", "projects", "-work")
	a, bID, c := testSessionID, "123e4567-e89b-12d3-a456-426614174001", "123e4567-e89b-12d3-a456-426614174002"
	path := func(id string) string { return filepath.Join(root, id+".jsonl") }
	row := func(id, role, text string) map[string]any {
		return map[string]any{"type": role, "uuid": id, "message": map[string]any{"content": text}}
	}
	writeRows(t, path(a), row("a-u1", "user", "A older question"), row("a-a1", "assistant", "A older answer"), row("a-u2", "user", "A latest question"), row("a-a2", "assistant", "A latest answer"))
	browser, err := NewBrowser(reader, t.TempDir(), DefaultBrowserOptions())
	if err != nil {
		t.Fatal(err)
	}
	defer browser.Close()
	scope := BrowseScope{Provider: "claude", CWD: "/work", SessionID: a}
	read := func(cursor string) BrowsePage {
		t.Helper()
		page, err := browser.ReadPage(context.Background(), BrowseRequest{Scope: scope, Cursor: cursor, Limit: 2})
		if err != nil {
			t.Fatal(err)
		}
		return page
	}
	initial := read("")
	if initial.ReasonCode != "" || len(initial.Entries) != 2 || initial.NextCursor == "" {
		t.Fatalf("ordinary A=%#v", initial)
	}
	appendClaudeTestRow(t, path(a), map[string]any{"type": "continued-in", "sessionId": a, "continuedInSessionId": bID})
	writeRows(t, path(bID), row("b-u1", "user", "B question"), row("b-a1", "assistant", "B answer"))
	latestB := read("")
	bridgeB := read(latestB.NextCursor)
	olderA := read(initial.NextCursor)
	appendClaudeTestRow(t, path(bID), map[string]any{"type": "continued-in", "sessionId": bID, "continuedInSessionId": c})
	writeRows(t, path(c), row("c-u1", "user", "C question"), row("c-a1", "assistant", "C answer"))
	latestC := read("")
	bridgeC := read(latestC.NextCursor)
	for name, page := range map[string]BrowsePage{"latestB": latestB, "bridgeB": bridgeB, "olderA": olderA, "latestC": latestC, "bridgeC": bridgeC} {
		if page.State != BrowseReady || page.ReasonCode != "" || page.SourceRevision != initial.SourceRevision || len(page.Entries) != 2 {
			t.Fatalf("%s incompatible page=%#v", name, page)
		}
	}
	if latestB.Entries[0].Text != "B question" || bridgeB.Entries[0].ID != initial.Entries[0].ID || olderA.Entries[0].Text != "A older question" || latestC.Entries[0].Text != "C question" || bridgeC.Entries[0].ID != latestB.Entries[0].ID {
		t.Fatal("fixture did not create disjoint latest and genuine cursor bridges")
	}
	replacementPath := path(c) + ".replacement"
	writeRows(t, replacementPath, row("replacement", "assistant", "replacement must not replace displayed lane"))
	if err := os.Rename(replacementPath, path(c)); err != nil {
		t.Fatal(err)
	}
	replacement := read("")
	if replacement.ReasonCode != "source_changed" {
		t.Fatalf("replacement=%#v", replacement)
	}
	fixture := map[string]BrowsePage{"initial": initial, "latestB": latestB, "bridgeB": bridgeB, "olderA": olderA, "latestC": latestC, "bridgeC": bridgeC, "replacement": replacement}
	if dest := os.Getenv("HERDR_CLAUDE_CONTROLLER_FIXTURE"); dest != "" {
		data, err := json.MarshalIndent(fixture, "", "  ")
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(dest, append(data, '\n'), 0o600); err != nil {
			t.Fatal(err)
		}
	}
}
