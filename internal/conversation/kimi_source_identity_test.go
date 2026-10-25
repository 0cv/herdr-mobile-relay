package conversation

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/0cv/herdr-mobile-relay/internal/agentroots"
)

func TestKimiInferredDuplicateRootArchiveCursor(t *testing.T) {
	home, first, second := t.TempDir(), t.TempDir(), t.TempDir()
	t.Setenv(agentroots.KimiCodeListEnv, first+string(os.PathListSeparator)+second)
	t.Setenv("KIMI_CODE_HOME", "")
	cwd, id := "/fixture", "duplicate_session"
	wire := []byte("{\"type\":\"metadata\",\"protocol_version\":\"1.5\"}\n{\"type\":\"context.append_message\",\"message\":{\"role\":\"user\",\"content\":[{\"type\":\"text\",\"text\":\"ROOT_A prompt\"}]}}\n{\"type\":\"context.append_message\",\"message\":{\"role\":\"assistant\",\"content\":[{\"type\":\"text\",\"text\":\"ROOT_A answer\"}]}}\n")
	path := kimiFixture(t, first, kimiWorkspaceKey(cwd), id, cwd, "First", 1, wire)
	kimiFixture(t, second, kimiWorkspaceKey(cwd), id, cwd, "Second", 2, []byte(strings.ReplaceAll(string(wire), "ROOT_A", "ROOT_B")))
	reader := NewReader(home)
	browser, err := NewBrowser(reader, t.TempDir(), DefaultBrowserOptions())
	if err != nil {
		t.Fatal(err)
	}
	defer browser.Close()
	project := ProjectContext{CWD: cwd}
	selected, location, code := reader.SelectKimiSession(project, "")
	scope := BrowseScope{Provider: "kimi", CWD: cwd, SessionID: selected, InferredSession: true, SelectionSource: KimiSourceIdentity(location)}
	if code != "" || location.Root != first {
		t.Fatalf("initial selection=%+v %s", location, code)
	}
	page, err := browser.ReadPage(t.Context(), BrowseRequest{Scope: scope, Limit: 1})
	if err != nil || !page.Available || page.NextCursor == "" || !strings.Contains(page.Entries[0].Text, "ROOT_A") {
		t.Fatalf("initial page=%+v %v", page, err)
	}
	state := filepath.Join(filepath.Dir(filepath.Dir(filepath.Dir(path))), "state.json")
	if err := os.WriteFile(state, []byte(`{"id":"duplicate_session","version":2,"cwd":"/fixture","updatedAt":1,"archived":true}`), 0600); err != nil {
		t.Fatal(err)
	}
	selected, location, code = reader.SelectKimiSession(project, "")
	if selected != id || code != "" || location.Root != second || reader.KimiSelectionCurrent(scope) {
		t.Fatalf("replacement selection=%+v %s", location, code)
	}
	if token, err := reader.ChangeToken(t.Context(), scope); err != nil || token != "source_changed" {
		t.Fatalf("old feed token=%q %v", token, err)
	}
	for _, cursor := range []string{"", page.NextCursor} {
		stale, err := browser.ReadPage(t.Context(), BrowseRequest{Scope: scope, Cursor: cursor, Limit: 1})
		if err != nil || stale.Available || len(stale.Entries) != 0 || stale.ReasonCode != "source_changed" {
			t.Fatalf("old source served=%+v %v", stale, err)
		}
	}
	next := scope
	next.SelectionSource = KimiSourceIdentity(location)
	fresh, err := browser.ReadPage(t.Context(), BrowseRequest{Scope: next, Limit: 1})
	if err != nil || !fresh.Available || len(fresh.Entries) != 1 || !strings.Contains(fresh.Entries[0].Text, "ROOT_B") {
		t.Fatalf("replacement read archived root=%+v %v", fresh, err)
	}
	stale, err := browser.ReadPage(t.Context(), BrowseRequest{Scope: next, Cursor: page.NextCursor, Limit: 1})
	if err != nil || len(stale.Entries) != 0 || stale.ReasonCode != "invalid_cursor" {
		t.Fatalf("cross-root cursor=%+v %v", stale, err)
	}
	bound, err := reader.ReadKimiSource(t.Context(), next, "", 1)
	if err != nil || !bound.Available || len(bound.Entries) != 1 || !strings.Contains(bound.Entries[0].Text, "ROOT_B") {
		t.Fatalf("source-bound read=%+v %v", bound, err)
	}
	obsolete, err := reader.ReadKimiSource(t.Context(), scope, "", 1)
	if err != nil || obsolete.Available || len(obsolete.Entries) != 0 {
		t.Fatalf("obsolete source read=%+v %v", obsolete, err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if page, err := reader.ReadKimiSource(ctx, next, "", 1); err == nil || len(page.Entries) != 0 {
		t.Fatal("canceled source read exported entries")
	}
	reported := scope
	reported.InferredSession, reported.SelectionSource = false, ""
	old, err := browser.ReadPage(t.Context(), BrowseRequest{Scope: reported, Limit: 1})
	if err != nil || !old.Available || !strings.Contains(old.Entries[0].Text, "ROOT_A") {
		t.Fatalf("reported-ID policy changed=%+v %v", old, err)
	}
	legacy, err := reader.ReadWithProject("kimi", project, id, "", 1)
	if err != nil || !legacy.Available || !strings.Contains(legacy.Entries[0].Text, "ROOT_A") {
		t.Fatal("reported-ID reader policy changed")
	}
	if err := os.Remove(location.Path); err != nil {
		t.Fatal(err)
	}
	missing, err := reader.ReadKimiSource(t.Context(), next, "", 1)
	if err != nil || missing.Available || len(missing.Entries) != 0 {
		t.Fatal("missing selected source fell back to archived duplicate")
	}
}
