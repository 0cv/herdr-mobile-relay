package conversation

// Root-resolution behaviour for the multi-root reader. Kept out of
// reader_test.go, which covers transcript parsing: the two concerns change for
// different reasons, and sharing one file's tail made this branch collide with
// unrelated parsing work.

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/0cv/herdr-mobile-relay/internal/agentroots"
)

const secondSessionID = "223e4567-e89b-12d3-a456-426614174001"

func TestClaudeConversationResolvesFromAdditionalConfiguredRoot(t *testing.T) {
	_, home := testReader(t)
	profileDir := t.TempDir()
	t.Setenv(agentroots.ClaudeListEnv, profileDir)
	reader := NewReader(home)

	profilePath := filepath.Join(profileDir, "projects", "-work", testSessionID+".jsonl")
	writeRows(t, profilePath,
		map[string]any{"type": "user", "uuid": "u1", "timestamp": "2026-08-12T10:00:00Z", "message": map[string]any{"content": "from profile root"}},
		map[string]any{"type": "assistant", "uuid": "a1", "timestamp": "2026-08-12T10:00:01Z", "message": map[string]any{"content": []any{map[string]any{"type": "text", "text": "profile answer"}}}},
	)
	profilePage, err := reader.Read("claude", testSessionID, "", 80)
	if err != nil {
		t.Fatal(err)
	}
	if !profilePage.Available || profilePage.Total != 2 ||
		profilePage.Entries[0].Text != "from profile root" || profilePage.Entries[1].Text != "profile answer" {
		t.Fatalf("profile page = %#v, want two entries resolved from the additional root", profilePage)
	}

	homePath := filepath.Join(home, ".claude", "projects", "-work2", secondSessionID+".jsonl")
	writeRows(t, homePath,
		map[string]any{"type": "user", "uuid": "u2", "timestamp": "2026-08-12T10:00:00Z", "message": map[string]any{"content": "from home root"}},
	)
	homePage, err := reader.Read("claude", secondSessionID, "", 80)
	if err != nil {
		t.Fatal(err)
	}
	if !homePage.Available || homePage.Total != 1 || homePage.Entries[0].Text != "from home root" {
		t.Fatalf("home page = %#v, want the home-default root to still resolve alongside the configured list", homePage)
	}
}

func TestClaudeConversationUnavailableWhenSessionMissingFromAllRoots(t *testing.T) {
	reader, _ := testReader(t)
	page, err := reader.Read("claude", testSessionID, "", 80)
	if err != nil {
		t.Fatal(err)
	}
	if page.Available {
		t.Fatalf("page = %#v, want unavailable", page)
	}
	if page.Reason != "No conversation log is available for this session." {
		t.Fatalf("reason = %q", page.Reason)
	}
}

func TestPiConversationConfinesReportedPathAcrossMultipleRoots(t *testing.T) {
	_, home := testReader(t)
	profileDir := t.TempDir()
	t.Setenv(agentroots.PiListEnv, profileDir)
	reader := NewReader(home)

	external := filepath.Join(t.TempDir(), "outside.jsonl")
	writeRows(t, external, map[string]any{"type": "message", "message": map[string]any{"role": "user", "content": "outside"}})

	linkDir := filepath.Join(profileDir, "sessions", "--work--")
	if err := os.MkdirAll(linkDir, 0o700); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(linkDir, "linked.jsonl")
	if err := os.Symlink(external, link); err != nil {
		t.Fatal(err)
	}

	page, err := reader.Read("pi", link, "", 80)
	if err != nil {
		t.Fatal(err)
	}
	if page.Available {
		t.Fatal("symlink inside a configured root pointing outside every root was served")
	}
}

func TestOMPConversationResolvesFromConfiguredProfileRoot(t *testing.T) {
	_, home := testReader(t)
	profileDir := t.TempDir()
	t.Setenv(agentroots.OMPListEnv, profileDir)
	reader := NewReader(home)

	path := filepath.Join(profileDir, "sessions", "-work", "session_"+testSessionID+".jsonl")
	writeRows(t, path,
		map[string]any{"type": "message", "id": "u1", "timestamp": "2026-08-12T10:00:00Z", "message": map[string]any{"role": "user", "content": []any{map[string]any{"type": "text", "text": "question"}}}},
		map[string]any{"type": "message", "id": "a1", "timestamp": "2026-08-12T10:00:01Z", "message": map[string]any{"role": "assistant", "content": []any{map[string]any{"type": "text", "text": "response"}}}},
	)

	page, err := reader.Read("omp", testSessionID, "", 80)
	if err != nil {
		t.Fatal(err)
	}
	if !page.Available || page.Total != 2 ||
		page.Entries[0].Text != "question" || page.Entries[1].Text != "response" {
		t.Fatalf("page = %#v, want the omp profile root to resolve", page)
	}
}

// TestClaudeConversationEarliestRootWinsForDuplicateSessionID plants the same
// session id in two roots with distinguishable content. Unlike
// TestClaudeConversationResolvesFromAdditionalConfiguredRoot, which puts a
// different id in each root and so only proves reachability, this fails under
// a reversed root order because the home default would win instead of the
// configured root that is supposed to outrank it.
func TestClaudeConversationEarliestRootWinsForDuplicateSessionID(t *testing.T) {
	_, home := testReader(t)
	profileDir := t.TempDir()
	t.Setenv(agentroots.ClaudeListEnv, profileDir)
	reader := NewReader(home)

	profilePath := filepath.Join(profileDir, "projects", "-work", testSessionID+".jsonl")
	writeRows(t, profilePath,
		map[string]any{"type": "user", "uuid": "u1", "timestamp": "2026-08-12T10:00:00Z", "message": map[string]any{"content": "earlier root copy"}},
	)
	homePath := filepath.Join(home, ".claude", "projects", "-work", testSessionID+".jsonl")
	writeRows(t, homePath,
		map[string]any{"type": "user", "uuid": "u2", "timestamp": "2026-08-12T10:00:00Z", "message": map[string]any{"content": "home default copy"}},
	)

	page, err := reader.Read("claude", testSessionID, "", 80)
	if err != nil {
		t.Fatal(err)
	}
	if !page.Available || page.Total != 1 || page.Entries[0].Text != "earlier root copy" {
		t.Fatalf("page = %#v, want the earliest configured root's copy served over the home default's", page)
	}
}

// TestOMPConversationDiscoversProfileCreatedAfterReaderConstructed guards
// against snapshotting roots in NewReader: the profile directory is created
// only after the Reader exists, so this fails unless every root list is
// re-resolved (and agentroots.OMP re-scans <configRoot>/profiles) on each
// call.
func TestOMPConversationDiscoversProfileCreatedAfterReaderConstructed(t *testing.T) {
	reader, home := testReader(t)

	profileAgentDir := filepath.Join(home, ".omp", "profiles", "work", "agent")
	path := filepath.Join(profileAgentDir, "sessions", "-work", "session_"+testSessionID+".jsonl")
	writeRows(t, path,
		map[string]any{"type": "message", "id": "u1", "timestamp": "2026-08-12T10:00:00Z", "message": map[string]any{"role": "user", "content": []any{map[string]any{"type": "text", "text": "question"}}}},
	)

	page, err := reader.Read("omp", testSessionID, "", 80)
	if err != nil {
		t.Fatal(err)
	}
	if !page.Available || page.Total != 1 || page.Entries[0].Text != "question" {
		t.Fatalf("page = %#v, want the profile created after NewReader to be discovered", page)
	}
}

// TestClaudeConversationSearchesProjectDirectorySymlinkedOutsideRoot is the
// case the symlink handling exists for: a project directory symlinked to a
// dotfiles repo or another volume. It fails two ways. Classify the entry with
// DirEntry.IsDir and the symlink is never descended into. Contain the
// candidate against the root instead of the project directory and the entry is
// accepted only for its real path - outside the root - to be dropped
// immediately after. A symlink whose target sits inside the root passes either
// way and pins neither half.
func TestClaudeConversationSearchesProjectDirectorySymlinkedOutsideRoot(t *testing.T) {
	reader, home := testReader(t)

	outside := filepath.Join(t.TempDir(), "dotfiles", "-real-work")
	writeRows(t, filepath.Join(outside, testSessionID+".jsonl"),
		map[string]any{"type": "user", "uuid": "u1", "timestamp": "2026-08-12T10:00:00Z", "message": map[string]any{"content": "from outside the root"}},
	)

	projectsDir := filepath.Join(home, ".claude", "projects")
	if err := os.MkdirAll(projectsDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(projectsDir, "-linked-alias")); err != nil {
		t.Fatal(err)
	}

	page, err := reader.Read("claude", testSessionID, "", 80)
	if err != nil {
		t.Fatal(err)
	}
	if !page.Available || page.Total != 1 || page.Entries[0].Text != "from outside the root" {
		t.Fatalf("page = %#v, want the project directory symlinked outside the root to be searched", page)
	}
}

// TestOMPConversationSearchesProjectDirectorySymlinkedOutsideRoot is the same
// guard for the Pi/OMP layout, which reaches the session through a second
// ReadDir of the project directory rather than a direct Join. That layout is
// the one most likely to be symlinked in practice, since a profile's agent
// directory is routinely linked in from a config repo.
func TestOMPConversationSearchesProjectDirectorySymlinkedOutsideRoot(t *testing.T) {
	_, home := testReader(t)
	profileDir := t.TempDir()
	t.Setenv(agentroots.OMPListEnv, profileDir)
	reader := NewReader(home)

	outside := filepath.Join(t.TempDir(), "dotfiles", "-real-work")
	writeRows(t, filepath.Join(outside, "session_"+testSessionID+".jsonl"),
		map[string]any{"type": "message", "id": "u1", "timestamp": "2026-08-12T10:00:00Z", "message": map[string]any{"role": "user", "content": []any{map[string]any{"type": "text", "text": "from outside the root"}}}},
	)

	sessionsDir := filepath.Join(profileDir, "sessions")
	if err := os.MkdirAll(sessionsDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(sessionsDir, "-linked-alias")); err != nil {
		t.Fatal(err)
	}

	page, err := reader.Read("omp", testSessionID, "", 80)
	if err != nil {
		t.Fatal(err)
	}
	if !page.Available || page.Total != 1 || page.Entries[0].Text != "from outside the root" {
		t.Fatalf("page = %#v, want the symlinked omp project directory outside the root to be searched", page)
	}
}

// TestCodexConversationSearchesDayDirectorySymlinkedOutsideRoot covers the
// third enumerated layout: the rollout file's directory component comes from
// ReadDir of the root's date hierarchy, so containment there is against the day
// directory for the same reason. A day directory on another volume is how a
// large rollout archive gets moved off the boot disk.
func TestCodexConversationSearchesDayDirectorySymlinkedOutsideRoot(t *testing.T) {
	reader, home := testReader(t)

	outside := filepath.Join(t.TempDir(), "archive", "22")
	writeRows(t, filepath.Join(outside, "rollout-2026-08-22T10-00-00-"+testSessionID+".jsonl"),
		map[string]any{"timestamp": "2026-08-22T10:00:00Z", "type": "response_item", "payload": map[string]any{"type": "message", "role": "user", "content": []any{map[string]any{"type": "input_text", "text": "from outside the root"}}}},
	)

	monthDir := filepath.Join(home, ".codex", "sessions", "2026", "08")
	if err := os.MkdirAll(monthDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(monthDir, "22")); err != nil {
		t.Fatal(err)
	}

	page, err := reader.Read("codex", testSessionID, "", 80)
	if err != nil {
		t.Fatal(err)
	}
	if !page.Available || page.Total != 1 || page.Entries[0].Text != "from outside the root" {
		t.Fatalf("page = %#v, want the day directory symlinked outside the root to be searched", page)
	}
}

// TestOMPConversationRejectsDirectoryNamedLikeSessionFile pins the invariant
// the file filters lean on. They no longer stat each candidate before the name
// tests - the stat was redundant and ran ahead of two free string comparisons -
// so containedRegularFile's Mode().IsRegular() is the only thing standing
// between a directory that happens to be named session_<id>.jsonl and loadTail
// trying to read it.
func TestOMPConversationRejectsDirectoryNamedLikeSessionFile(t *testing.T) {
	_, home := testReader(t)
	profileDir := t.TempDir()
	t.Setenv(agentroots.OMPListEnv, profileDir)
	reader := NewReader(home)

	decoy := filepath.Join(profileDir, "sessions", "-work", "session_"+testSessionID+".jsonl")
	if err := os.MkdirAll(decoy, 0o700); err != nil {
		t.Fatal(err)
	}

	page, err := reader.Read("omp", testSessionID, "", 80)
	if err != nil {
		t.Fatalf("read a directory named like a session file: %v", err)
	}
	if page.Available {
		t.Fatalf("page = %#v, want a directory named like the session file to be rejected", page)
	}
}

func realPath(t *testing.T, path string) string {
	t.Helper()
	real, err := filepath.EvalSymlinks(path)
	if err != nil {
		t.Fatal(err)
	}
	return real
}

// TestLocateReportsTheFileReadServes covers every shape the reader resolves -
// the Claude project-directory scan, the Codex rollout tree, and both Pi/OMP
// forms (an absolute .jsonl path and a bare canonical UUID). Locate is the one
// implementation of that lookup and Read routes through it, so each case
// asserts both halves of the seam: the path Locate reports is the planted file
// with symlinks resolved, and the text Read serves is in the file Locate named.
// A branch that answered from somewhere else would satisfy neither.
func TestLocateReportsTheFileReadServes(t *testing.T) {
	reader, home := testReader(t)

	piPath := filepath.Join(home, ".pi", "agent", "sessions", "--work--", "session.jsonl")
	cases := []struct {
		name    string
		agent   string
		session string
		file    string
		root    string
		marker  string
		row     map[string]any
	}{
		{
			name:    "claude project directory",
			agent:   "claude",
			session: testSessionID,
			file:    filepath.Join(home, ".claude", "projects", "-work", testSessionID+".jsonl"),
			root:    filepath.Join(home, ".claude", "projects"),
			marker:  "claude turn",
			row:     map[string]any{"type": "user", "uuid": "u1", "timestamp": "2026-08-22T10:00:00Z", "message": map[string]any{"content": "claude turn"}},
		},
		{
			name:    "codex rollout tree",
			agent:   "codex",
			session: testSessionID,
			file:    filepath.Join(home, ".codex", "sessions", "2026", "08", "22", "rollout-2026-08-22T10-00-00-"+testSessionID+".jsonl"),
			root:    filepath.Join(home, ".codex", "sessions"),
			marker:  "codex turn",
			row:     map[string]any{"timestamp": "2026-08-22T10:00:00Z", "type": "response_item", "payload": map[string]any{"type": "message", "role": "user", "content": []any{map[string]any{"type": "input_text", "text": "codex turn"}}}},
		},
		{
			name:    "pi absolute path",
			agent:   "pi",
			session: piPath,
			file:    piPath,
			root:    filepath.Join(home, ".pi", "agent", "sessions"),
			marker:  "pi turn",
			row:     map[string]any{"type": "message", "id": "u1", "timestamp": "2026-08-22T10:00:00Z", "message": map[string]any{"role": "user", "content": []any{map[string]any{"type": "text", "text": "pi turn"}}}},
		},
		{
			name:    "omp bare canonical uuid",
			agent:   "omp",
			session: testSessionID,
			file:    filepath.Join(home, ".omp", "agent", "sessions", "-work", "session_"+testSessionID+".jsonl"),
			root:    filepath.Join(home, ".omp", "agent", "sessions"),
			marker:  "omp turn",
			row:     map[string]any{"type": "message", "id": "u1", "timestamp": "2026-08-22T10:00:00Z", "message": map[string]any{"role": "user", "content": []any{map[string]any{"type": "text", "text": "omp turn"}}}},
		},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			writeRows(t, testCase.file, testCase.row)

			path, root, ok := reader.Locate(testCase.agent, testCase.session)
			if !ok {
				t.Fatalf("Locate(%q, %q) reported no transcript", testCase.agent, testCase.session)
			}
			if want := realPath(t, testCase.file); path != want {
				t.Fatalf("path = %q, want %q", path, want)
			}
			if root != testCase.root {
				t.Fatalf("root = %q, want %q", root, testCase.root)
			}

			page, err := reader.Read(testCase.agent, testCase.session, "", 80)
			if err != nil {
				t.Fatal(err)
			}
			if !page.Available || page.Total != 1 || page.Entries[0].Text != testCase.marker {
				t.Fatalf("page = %#v, want the located transcript served", page)
			}
			data, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(string(data), testCase.marker) {
				t.Fatalf("Read served %q, which is not in the file Locate reported (%s)", testCase.marker, path)
			}
		})
	}

	// Read trims the reported session id before resolving, so Locate has to as
	// well or the two disagree about a whitespace-padded pane value.
	padded, _, ok := reader.Locate("claude", "  "+testSessionID+"  ")
	if !ok || padded != realPath(t, cases[0].file) {
		t.Fatalf("Locate with a padded id = %q ok=%v, want the same file Read serves", padded, ok)
	}
}

// TestLocateReportsTheRootThatAnswered pins the root return specifically, with
// the session in the LAST element of each root list and an existing but empty
// earlier root. Returning any fixed element - the first, most plausibly - looks
// right in a single-root fixture and is wrong here. The Codex case is the one
// with a consumer: the title lookup reads session_index.jsonl from
// filepath.Dir(root), so a root from the wrong home names a different index.
func TestLocateReportsTheRootThatAnswered(t *testing.T) {
	_, home := testReader(t)
	claudeFirst := t.TempDir()
	codexFirst := t.TempDir()
	t.Setenv(agentroots.ClaudeListEnv, claudeFirst)
	t.Setenv(agentroots.CodexListEnv, codexFirst)
	reader := NewReader(home)

	// Both configured roots exist and are searched; neither holds the session.
	if err := os.MkdirAll(filepath.Join(claudeFirst, "projects", "-work"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(codexFirst, "sessions", "2026", "08", "22"), 0o700); err != nil {
		t.Fatal(err)
	}
	if roots := agentroots.Claude(home); len(roots) != 2 || roots[0] != filepath.Join(claudeFirst, "projects") {
		t.Fatalf("claude roots = %v, want the configured root searched before the home default", roots)
	}
	if roots := agentroots.Codex(home); len(roots) != 2 || roots[0] != filepath.Join(codexFirst, "sessions") {
		t.Fatalf("codex roots = %v, want the configured root searched before the home default", roots)
	}

	claudeFile := filepath.Join(home, ".claude", "projects", "-work", testSessionID+".jsonl")
	writeRows(t, claudeFile,
		map[string]any{"type": "user", "uuid": "u1", "timestamp": "2026-08-22T10:00:00Z", "message": map[string]any{"content": "home default copy"}},
	)
	codexFile := filepath.Join(home, ".codex", "sessions", "2026", "08", "22", "rollout-2026-08-22T10-00-00-"+testSessionID+".jsonl")
	writeRows(t, codexFile,
		map[string]any{"timestamp": "2026-08-22T10:00:00Z", "type": "response_item", "payload": map[string]any{"type": "message", "role": "user", "content": []any{map[string]any{"type": "input_text", "text": "home default copy"}}}},
	)

	path, root, ok := reader.Locate("claude", testSessionID)
	if !ok || path != realPath(t, claudeFile) {
		t.Fatalf("claude path = %q ok=%v, want %q", path, ok, realPath(t, claudeFile))
	}
	if want := filepath.Join(home, ".claude", "projects"); root != want {
		t.Fatalf("claude root = %q, want the root that answered (%q)", root, want)
	}

	path, root, ok = reader.Locate("codex", testSessionID)
	if !ok || path != realPath(t, codexFile) {
		t.Fatalf("codex path = %q ok=%v, want %q", path, ok, realPath(t, codexFile))
	}
	if want := filepath.Join(home, ".codex", "sessions"); root != want {
		t.Fatalf("codex root = %q, want the root that answered (%q)", root, want)
	}
	if want := filepath.Join(home, ".codex"); filepath.Dir(root) != want {
		t.Fatalf("codex home = %q, want %q", filepath.Dir(root), want)
	}
}

// TestLocateRejectsWhatReadCannotServe walks the four ways ok is false. Read
// distinguishes them in its Reason string; Locate collapses them, so each case
// also asserts Read agrees that nothing is available - a caller gating a title
// on Locate must never be more generous than the transcript view.
func TestLocateRejectsWhatReadCannotServe(t *testing.T) {
	reader, home := testReader(t)
	writeRows(t, filepath.Join(home, ".claude", "projects", "-work", testSessionID+".jsonl"),
		map[string]any{"type": "user", "uuid": "u1", "timestamp": "2026-08-22T10:00:00Z", "message": map[string]any{"content": "planted"}},
	)

	cases := []struct {
		name    string
		agent   string
		session string
	}{
		{name: "unsupported agent", agent: "cursor", session: testSessionID},
		{name: "empty id", agent: "claude", session: ""},
		{name: "whitespace-only id", agent: "claude", session: "   "},
		{name: "path traversal id", agent: "claude", session: ".."},
		{name: "non-canonical codex id", agent: "codex", session: "work-session-1"},
		{name: "id no root holds", agent: "claude", session: secondSessionID},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			path, root, ok := reader.Locate(testCase.agent, testCase.session)
			if ok || path != "" || root != "" {
				t.Fatalf("Locate(%q, %q) = (%q, %q, %v), want no transcript", testCase.agent, testCase.session, path, root, ok)
			}
			page, err := reader.Read(testCase.agent, testCase.session, "", 80)
			if err != nil {
				t.Fatal(err)
			}
			if page.Available {
				t.Fatalf("page = %#v, want unavailable wherever Locate reports nothing", page)
			}
		})
	}
}
