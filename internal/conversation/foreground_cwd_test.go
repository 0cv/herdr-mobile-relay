package conversation

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/0cv/herdr-mobile-relay/internal/agentroots"
)

func writeForegroundTranscript(t *testing.T, path, answer string) {
	t.Helper()
	writeRows(t, path,
		map[string]any{"type": "user", "message": map[string]any{"content": "prompt"}},
		map[string]any{"type": "assistant", "message": map[string]any{"content": answer}},
	)
}

func TestClaudeForegroundHintIsDistinctFromWhitespacePaneCWD(t *testing.T) {
	reader, home := testReader(t)
	paneCWD := "/work/app "
	foregroundCWD := "/work/app"
	panePath := filepath.Join(home, ".claude", "projects", "-work-app-", testSessionID+".jsonl")
	foregroundPath := filepath.Join(home, ".claude", "projects", "-work-app", testSessionID+".jsonl")
	writeForegroundTranscript(t, panePath, "pane answer")
	writeForegroundTranscript(t, foregroundPath, "foreground answer")
	project := ProjectContext{CWD: paneCWD, ForegroundCWD: foregroundCWD}

	location := reader.LocateWithProject("claude", project, testSessionID)
	if location.Path != foregroundPath {
		t.Fatalf("location = %q, want foreground path %q", location.Path, foregroundPath)
	}
	browser, err := NewBrowser(reader, t.TempDir(), DefaultBrowserOptions())
	if err != nil {
		t.Fatal(err)
	}
	defer browser.Close()
	foregroundScope := BrowseScope{Provider: "claude", CWD: paneCWD, ForegroundCWD: foregroundCWD, SessionID: testSessionID}
	page, err := browser.ReadPage(context.Background(), BrowseRequest{Scope: foregroundScope, Limit: 1})
	if err != nil || !page.Available || len(page.Entries) != 1 || page.Entries[0].Text != "foreground answer" || page.NextCursor == "" {
		t.Fatalf("browser foreground page = %#v, err = %v; want the same foreground copy with a cursor", page, err)
	}

	paneScope := BrowseScope{Provider: "claude", CWD: paneCWD, SessionID: testSessionID}
	stale, err := browser.ReadPage(context.Background(), BrowseRequest{Scope: paneScope, Cursor: page.NextCursor, Limit: 1})
	if err != nil || stale.ReasonCode != "invalid_cursor" {
		t.Fatalf("foreground cursor accepted after clearing hint: page=%#v err=%v", stale, err)
	}
	panePage, err := browser.ReadPage(context.Background(), BrowseRequest{Scope: paneScope, Limit: 1})
	if err != nil || !panePage.Available || len(panePage.Entries) != 1 || panePage.Entries[0].Text != "pane answer" {
		t.Fatalf("browser pane page = %#v, err = %v; want the pane copy after clearing hint", panePage, err)
	}
}

func TestClaudeLocateWithProjectUsesForegroundDirectory(t *testing.T) {
	reader, home := testReader(t)
	foreground := "/work/foreground"
	path := filepath.Join(home, ".claude", "projects", "-work-foreground", testSessionID+".jsonl")
	writeForegroundTranscript(t, path, "foreground answer")

	location := reader.LocateWithProject("claudecode", ProjectContext{
		CWD: "/work/pane", ForegroundCWD: "  " + foreground + "  ",
	}, testSessionID)
	if location.Path != path || location.Root != filepath.Join(home, ".claude", "projects") {
		t.Fatalf("location = %#v, want foreground path %q in default root", location, path)
	}
	page, err := reader.ReadWithProject("claude", ProjectContext{CWD: "/work/pane", ForegroundCWD: foreground}, testSessionID, "", 80)
	if err != nil || !page.Available || len(page.Entries) != 2 || page.Entries[1].Text != "foreground answer" {
		t.Fatalf("foreground page = %#v, err = %v", page, err)
	}
}

func TestClaudeForegroundHintFallbackPolicy(t *testing.T) {
	reader, home := testReader(t)
	pane := "/work/pane"
	panePath := filepath.Join(home, ".claude", "projects", "-work-pane", testSessionID+".jsonl")
	writeForegroundTranscript(t, panePath, "pane answer")
	foregroundPath := filepath.Join(home, ".claude", "projects", "-work-foreground", testSessionID+".jsonl")
	writeForegroundTranscript(t, foregroundPath, "foreground answer")

	for _, test := range []struct {
		name string
		hint string
		want string
	}{
		{name: "missing", want: panePath},
		{name: "empty", hint: "", want: panePath},
		{name: "whitespace", hint: "   ", want: panePath},
		{name: "relative", hint: "work/foreground", want: panePath},
		{name: "equal", hint: pane, want: panePath},
	} {
		t.Run(test.name, func(t *testing.T) {
			got := reader.LocateWithProject("claude", ProjectContext{CWD: pane, ForegroundCWD: test.hint}, testSessionID)
			if got.Path != test.want {
				t.Fatalf("location = %q, want %q", got.Path, test.want)
			}
		})
	}
}

func TestClaudeForegroundProjectEncodingsAndMarkers(t *testing.T) {
	reader, home := testReader(t)
	projects := filepath.Join(home, ".claude", "projects")
	for _, test := range []struct {
		name, paneCWD, foregroundCWD, directory, answer string
		marker                                          bool
	}{
		{name: "preferred punctuation", paneCWD: "/work/pane", foregroundCWD: "/work/tree_with.dot and space", directory: "-work-tree-with-dot-and-space", answer: "preferred answer"},
		{name: "legacy punctuation", paneCWD: "/work/pane", foregroundCWD: "/legacy/tree_with.dot and space", directory: "-legacy-tree_with.dot and space", answer: "legacy answer"},
		{name: "foreground cwd marker", paneCWD: "/work/pane", foregroundCWD: "/marker/tree_with.dot and space", directory: "marker-only", answer: "marker answer", marker: true},
		{name: "pane cwd marker", paneCWD: "/pane/tree_with.dot and space", directory: "pane-marker", answer: "pane marker answer", marker: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			path := filepath.Join(projects, test.directory, testSessionID+".jsonl")
			writeForegroundTranscript(t, path, test.answer)
			if test.marker {
				markerCWD := test.foregroundCWD
				if markerCWD == "" {
					markerCWD = test.paneCWD
				}
				if err := os.WriteFile(filepath.Join(projects, test.directory, "cwd"), []byte(markerCWD+"\n"), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			location := reader.LocateWithProject("claude", ProjectContext{
				CWD: test.paneCWD, ForegroundCWD: test.foregroundCWD,
			}, testSessionID)
			if location.Path != path {
				t.Fatalf("location = %#v, want %q", location, path)
			}
		})
	}
}

func TestClaudeForegroundMissFallsBackToPaneAndPrefersForegroundWithinRoot(t *testing.T) {
	reader, home := testReader(t)
	pane := "/work/pane"
	foreground := "/work/foreground"
	panePath := filepath.Join(home, ".claude", "projects", "-work-pane", testSessionID+".jsonl")
	foregroundPath := filepath.Join(home, ".claude", "projects", "-work-foreground", testSessionID+".jsonl")
	writeForegroundTranscript(t, panePath, "pane answer")

	got := reader.LocateWithProject("claude", ProjectContext{CWD: pane, ForegroundCWD: foreground}, testSessionID)
	if got.Path != panePath {
		t.Fatalf("foreground miss location = %q, want pane path %q", got.Path, panePath)
	}

	writeForegroundTranscript(t, foregroundPath, "foreground answer")
	got = reader.LocateWithProject("claude", ProjectContext{CWD: pane, ForegroundCWD: foreground}, testSessionID)
	if got.Path != panePath {
		// The old pane hit is intentionally cached for the unchanged context;
		// a filesystem change does not invalidate a stable location decision.
		t.Fatalf("unexpected cache behavior after adding foreground file: %q", got.Path)
	}

	fresh := NewReader(home)
	got = fresh.LocateWithProject("claude", ProjectContext{CWD: pane, ForegroundCWD: foreground}, testSessionID)
	if got.Path != foregroundPath {
		t.Fatalf("foreground location = %q, want %q", got.Path, foregroundPath)
	}
}

func TestClaudeForegroundPreservesRootPrecedence(t *testing.T) {
	profile := t.TempDir()
	home := t.TempDir()
	t.Setenv(agentroots.ClaudeListEnv, profile)
	reader := NewReader(home)
	panePath := filepath.Join(profile, "projects", "-work-pane", testSessionID+".jsonl")
	foregroundPath := filepath.Join(home, ".claude", "projects", "-work-foreground", testSessionID+".jsonl")
	writeForegroundTranscript(t, panePath, "earlier root pane")
	writeForegroundTranscript(t, foregroundPath, "later root foreground")

	got := reader.LocateWithProject("claude", ProjectContext{
		CWD: "/work/pane", ForegroundCWD: "/work/foreground",
	}, testSessionID)
	if got.Path != panePath || got.Root != filepath.Join(profile, "projects") {
		t.Fatalf("location = %#v, want earlier root pane copy %#v", got, Location{Path: panePath, Root: filepath.Join(profile, "projects")})
	}
}

func TestClaudeForegroundDoesNotTriggerUnknownCWDSearch(t *testing.T) {
	reader, home := testReader(t)
	unrelated := filepath.Join(home, ".claude", "projects", "-unrelated")
	writeForegroundTranscript(t, filepath.Join(unrelated, testSessionID+".jsonl"), "unrelated")

	got := reader.LocateWithProject("claude", ProjectContext{ForegroundCWD: "/work/foreground"}, testSessionID)
	if got.Path != "" {
		t.Fatalf("foreground-only miss selected unrelated project %q", got.Path)
	}

	got = NewReader(home).LocateWithProject("claude", ProjectContext{}, testSessionID)
	if got.Path == "" {
		t.Fatal("both absent did not preserve historical unknown-cwd search")
	}
}

func TestClaudeForegroundLocationCacheUsesEffectiveHint(t *testing.T) {
	reader, home := testReader(t)
	first := "/work/first"
	second := "/work/second"
	firstPath := filepath.Join(home, ".claude", "projects", "-work-first", testSessionID+".jsonl")
	secondPath := filepath.Join(home, ".claude", "projects", "-work-second", testSessionID+".jsonl")
	writeForegroundTranscript(t, firstPath, "first")
	writeForegroundTranscript(t, secondPath, "second")

	if got := reader.LocateWithProject("claude", ProjectContext{CWD: "/wrong", ForegroundCWD: first}, testSessionID); got.Path != firstPath {
		t.Fatalf("first location = %q, want %q", got.Path, firstPath)
	}
	if got := reader.LocateWithProject("claude", ProjectContext{CWD: "/wrong", ForegroundCWD: second}, testSessionID); got.Path != secondPath {
		t.Fatalf("second location reused the first context: %q", got.Path)
	}
	if got := reader.LocateWithProject("claude", ProjectContext{CWD: "/wrong", ForegroundCWD: first}, testSessionID); got.Path != firstPath {
		t.Fatalf("same context was not cache-stable: %q", got.Path)
	}

	missReader := NewReader(home)
	if got := missReader.LocateWithProject("claude", ProjectContext{CWD: "/wrong", ForegroundCWD: "/work/missing"}, testSessionID); got.Path != "" {
		t.Fatalf("unexpected initial miss location %q", got.Path)
	}
	newHintPath := filepath.Join(home, ".claude", "projects", "-work-new-hint", testSessionID+".jsonl")
	writeForegroundTranscript(t, newHintPath, "new hint")
	if got := missReader.LocateWithProject("claude", ProjectContext{CWD: "/wrong", ForegroundCWD: "/work/new-hint"}, testSessionID); got.Path != newHintPath {
		t.Fatalf("new foreground hint did not bypass cached miss: %q", got.Path)
	}
}

func TestProjectContextLeavesNonClaudeProvidersPaneOnly(t *testing.T) {
	reader, home := testReader(t)
	panePath := filepath.Join(home, ".qoder", "projects", "-work-pane", testSessionID+".jsonl")
	foregroundPath := filepath.Join(home, ".qoder", "projects", "-work-foreground", testSessionID+".jsonl")
	writeForegroundTranscript(t, panePath, "pane")
	writeForegroundTranscript(t, foregroundPath, "foreground")

	project := NormalizeProjectContext("qoder", ProjectContext{CWD: "/work/pane", ForegroundCWD: "/work/foreground"})
	if project.ForegroundCWD != "" {
		t.Fatalf("non-Claude project retained foreground hint %q", project.ForegroundCWD)
	}
	projectContext := ProjectContext{CWD: "/work/pane", ForegroundCWD: "/work/foreground"}
	got := reader.LocateWithProject("qoder", projectContext, testSessionID)
	if got.Path != panePath {
		t.Fatalf("qoder location = %q, want pane path %q", got.Path, panePath)
	}
	page, err := reader.ReadWithProject("qoder", projectContext, testSessionID, "", 10)
	if err != nil || !page.Available || len(page.Entries) == 0 || page.Entries[len(page.Entries)-1].Text != "pane" {
		t.Fatalf("qoder page = %#v, err = %v; want pane transcript", page, err)
	}
}

func TestClaudeBrowserUsesForegroundScopeAndRejectsOldCursor(t *testing.T) {
	reader, home := testReader(t)
	firstPath := filepath.Join(home, ".claude", "projects", "-work-first", testSessionID+".jsonl")
	secondPath := filepath.Join(home, ".claude", "projects", "-work-second", testSessionID+".jsonl")
	writeForegroundTranscript(t, firstPath, "first answer")
	writeForegroundTranscript(t, secondPath, "second answer")
	cache := t.TempDir()
	browser, err := NewBrowser(reader, cache, DefaultBrowserOptions())
	if err != nil {
		t.Fatal(err)
	}
	defer browser.Close()

	firstScope := BrowseScope{Provider: "claude", CWD: "/wrong", ForegroundCWD: "/work/first", SessionID: testSessionID}
	firstPage, err := browser.ReadPage(context.Background(), BrowseRequest{Scope: firstScope, Limit: 1})
	if err != nil || !firstPage.Available || len(firstPage.Entries) != 1 || firstPage.Entries[0].Text != "first answer" || firstPage.NextCursor == "" {
		t.Fatalf("first browser page = %#v, err = %v", firstPage, err)
	}
	secondScope := BrowseScope{Provider: "claude", CWD: "/wrong", ForegroundCWD: "/work/second", SessionID: testSessionID}
	stale, err := browser.ReadPage(context.Background(), BrowseRequest{Scope: secondScope, Cursor: firstPage.NextCursor, Limit: 1})
	if err != nil || stale.ReasonCode != "invalid_cursor" {
		t.Fatalf("old foreground cursor = %#v, err = %v; want invalid_cursor", stale, err)
	}
	fresh, err := browser.ReadPage(context.Background(), BrowseRequest{Scope: secondScope, Limit: 1})
	if err != nil || !fresh.Available || len(fresh.Entries) != 1 || fresh.Entries[0].Text != "second answer" {
		t.Fatalf("fresh foreground page = %#v, err = %v", fresh, err)
	}
}

func TestClaudeBrowserRecoversWhenForegroundHintChangesAfterMiss(t *testing.T) {
	reader, home := testReader(t)
	browser, err := NewBrowser(reader, t.TempDir(), DefaultBrowserOptions())
	if err != nil {
		t.Fatal(err)
	}
	defer browser.Close()
	missing := BrowseScope{Provider: "claude", CWD: "/work/pane", ForegroundCWD: "/work/missing", SessionID: testSessionID}
	miss, err := browser.ReadPage(context.Background(), BrowseRequest{Scope: missing, Limit: 1})
	if err != nil || miss.Available || miss.ReasonCode != "invalid_session" {
		t.Fatalf("missing foreground page = %#v, err = %v", miss, err)
	}
	path := filepath.Join(home, ".claude", "projects", "-work-recovered", testSessionID+".jsonl")
	writeForegroundTranscript(t, path, "recovered answer")
	recovered := missing
	recovered.ForegroundCWD = "/work/recovered"
	page, err := browser.ReadPage(context.Background(), BrowseRequest{Scope: recovered, Limit: 1})
	if err != nil || !page.Available || len(page.Entries) != 1 || page.Entries[0].Text != "recovered answer" {
		t.Fatalf("recovered foreground page = %#v, err = %v", page, err)
	}
}

func TestClaudeBrowserCursorIgnoresRedundantForegroundHints(t *testing.T) {
	reader, home := testReader(t)
	path := filepath.Join(home, ".claude", "projects", "-work-pane", testSessionID+".jsonl")
	writeRows(t, path,
		map[string]any{"type": "assistant", "uuid": "first", "message": map[string]any{"content": "first answer"}},
		map[string]any{"type": "assistant", "uuid": "second", "message": map[string]any{"content": "second answer"}},
	)
	browser, err := NewBrowser(reader, t.TempDir(), DefaultBrowserOptions())
	if err != nil {
		t.Fatal(err)
	}
	defer browser.Close()
	base := BrowseScope{Provider: "claude", CWD: "/work/pane", SessionID: testSessionID}
	first, err := browser.ReadPage(context.Background(), BrowseRequest{Scope: base, Limit: 1})
	if err != nil || !first.Available || first.NextCursor == "" {
		t.Fatalf("initial cursor page = %#v, err = %v", first, err)
	}
	for _, hint := range []string{"", "   ", "work/pane", "/work/pane"} {
		scope := base
		scope.ForegroundCWD = hint
		page, readErr := browser.ReadPage(context.Background(), BrowseRequest{Scope: scope, Cursor: first.NextCursor, Limit: 1})
		if readErr != nil || page.ReasonCode == "invalid_cursor" || page.ReasonCode == "cursor_expired" {
			t.Fatalf("hint %q rejected an equivalent cursor: page=%#v err=%v", hint, page, readErr)
		}
	}
}

func TestClaudeForegroundContainedSourcesRemainProtected(t *testing.T) {
	reader, home := testReader(t)
	outside := filepath.Join(t.TempDir(), "outside.jsonl")
	writeForegroundTranscript(t, outside, "outside")
	projectDir := filepath.Join(home, ".claude", "projects", "-work-foreground")
	if err := os.MkdirAll(projectDir, 0o700); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(projectDir, testSessionID+".jsonl")
	if err := os.Symlink(outside, link); err != nil {
		t.Fatal(err)
	}
	if got := reader.LocateWithProject("claude", ProjectContext{CWD: "/wrong", ForegroundCWD: "/work/foreground"}, testSessionID); got.Path != "" {
		t.Fatalf("outside-root foreground symlink was accepted: %q", got.Path)
	}

	containedProject := filepath.Join(home, ".claude", "projects", "-work-contained")
	if err := os.MkdirAll(containedProject, 0o700); err != nil {
		t.Fatal(err)
	}
	containedTarget := filepath.Join(home, ".claude", "projects", "contained-target.jsonl")
	writeForegroundTranscript(t, containedTarget, "contained")
	containedLink := filepath.Join(containedProject, testSessionID+".jsonl")
	if err := os.Symlink(containedTarget, containedLink); err != nil {
		t.Fatal(err)
	}
	got := reader.LocateWithProject("claude", ProjectContext{CWD: "/wrong", ForegroundCWD: "/work/contained"}, testSessionID)
	if got.Path != containedTarget {
		t.Fatalf("contained foreground symlink location = %q, want %q", got.Path, containedTarget)
	}
}

func TestClaudeForegroundRejectsInvalidIDsAndNonregularSources(t *testing.T) {
	reader, home := testReader(t)
	project := filepath.Join(home, ".claude", "projects", "-work-nonregular")
	if err := os.MkdirAll(filepath.Join(project, testSessionID+".jsonl"), 0o700); err != nil {
		t.Fatal(err)
	}
	projectContext := ProjectContext{CWD: "/wrong", ForegroundCWD: "/work/nonregular"}
	if got := reader.LocateWithProject("claude", projectContext, testSessionID); got.Path != "" {
		t.Fatalf("directory transcript was accepted: %q", got.Path)
	}
	if got := reader.LocateWithProject("claude", projectContext, "../outside"); got.Path != "" {
		t.Fatalf("invalid session ID was accepted: %q", got.Path)
	}
}
