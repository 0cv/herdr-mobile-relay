package conversation

// Root-resolution behaviour for the multi-root reader. Kept out of
// reader_test.go, which covers transcript parsing: the two concerns change for
// different reasons, and sharing one file's tail made this branch collide with
// unrelated parsing work.

import (
	"os"
	"path/filepath"
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

// TestClaudeConversationSearchesSymlinkedProjectDirectory guards against
// classifying project directories with DirEntry.IsDir, which is false for a
// symlink to a directory. The session file lives two levels below the
// projects root (storage/-real-work), reachable only through the top-level
// "-linked-alias" symlink; the top-level "storage" directory itself never
// directly holds the file, so a real (non-symlink) match is impossible and
// the test fails whenever the symlinked entry is skipped.
func TestClaudeConversationSearchesSymlinkedProjectDirectory(t *testing.T) {
	reader, home := testReader(t)

	projectsDir := filepath.Join(home, ".claude", "projects")
	realProjectDir := filepath.Join(projectsDir, "storage", "-real-work")
	writeRows(t, filepath.Join(realProjectDir, testSessionID+".jsonl"),
		map[string]any{"type": "user", "uuid": "u1", "timestamp": "2026-08-12T10:00:00Z", "message": map[string]any{"content": "via symlinked project directory"}},
	)

	link := filepath.Join(projectsDir, "-linked-alias")
	if err := os.Symlink(realProjectDir, link); err != nil {
		t.Fatal(err)
	}

	page, err := reader.Read("claude", testSessionID, "", 80)
	if err != nil {
		t.Fatal(err)
	}
	if !page.Available || page.Total != 1 || page.Entries[0].Text != "via symlinked project directory" {
		t.Fatalf("page = %#v, want the symlinked project directory to be searched", page)
	}
}
