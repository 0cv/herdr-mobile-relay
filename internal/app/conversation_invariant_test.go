package app

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/0cv/herdr-mobile-relay/internal/agentroots"
	"github.com/0cv/herdr-mobile-relay/internal/conversation"
	"github.com/0cv/herdr-mobile-relay/internal/session"
)

// The bug this package's conversation plumbing exists to avoid: the pane title
// was read out of one directory tree while the transcript was looked up in
// another, so the header showed a correct title next to "No conversation log is
// available for this session."
//
// session.Resolver and conversation.Reader live in different packages and
// neither can see the other's fields, so an assertion inside either package can
// only restate its own construction. The invariant is a BEHAVIOURAL one and has
// to be tested where both types meet, which is here: for the same agent, cwd and
// session id, the title and the transcript must either both resolve or both
// fail. Never one without the other.
func writeClaudeTranscript(t *testing.T, path, title string) {
	t.Helper()
	writeClaudeTranscriptAnswering(t, path, title, "the answer")
}

// writeClaudeTranscriptAnswering gives the assistant turn distinguishable
// text, so a test that puts the same session id in two roots can tell which
// root the reader actually read. An empty title writes no summary row at all:
// that is a session Claude has not summarised yet, the normal state of one
// just started.
func writeClaudeTranscriptAnswering(t *testing.T, path, title, answer string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	// The user prompt is a bare string, not a block array, on purpose. This file
	// tests PATH RESOLUTION - which tree a transcript is found in - and must not
	// depend on how Claude content shapes are parsed. A string prompt parses
	// identically before and after the array-content fix, so these tests stay
	// green at every commit in the series.
	var rows []map[string]any
	if title != "" {
		rows = append(rows, map[string]any{"type": "summary", "summary": title})
	}
	rows = append(rows,
		map[string]any{"type": "user", "uuid": "u1", "timestamp": "2026-08-12T10:00:00Z",
			"message": map[string]any{"content": "the prompt"}},
		map[string]any{"type": "assistant", "uuid": "a1", "timestamp": "2026-08-12T10:00:01Z",
			"message": map[string]any{"content": []any{map[string]any{"type": "text", "text": answer}}}},
	)
	var buf []byte
	for _, row := range rows {
		encoded, err := json.Marshal(row)
		if err != nil {
			t.Fatal(err)
		}
		buf = append(buf, encoded...)
		buf = append(buf, '\n')
	}
	if err := os.WriteFile(path, buf, 0o644); err != nil {
		t.Fatal(err)
	}
}

func clearAgentEnv(t *testing.T) {
	t.Helper()
	for _, name := range []string{
		"CLAUDE_CONFIG_DIR", "CODEX_HOME", "PI_CODING_AGENT_DIR",
		agentroots.ClaudeListEnv, agentroots.QoderListEnv, agentroots.CodexListEnv,
		agentroots.PiListEnv, agentroots.OMPListEnv,
	} {
		t.Setenv(name, "")
	}
}

const invariantSession = "123e4567-e89b-12d3-a456-426614174321"

// Both halves resolve for a session reachable only through a non-default
// profile. Before the fix the title resolved and the transcript did not, which
// is exactly the reported symptom.
func TestTitleAndTranscriptAgreeForANonDefaultProfile(t *testing.T) {
	clearAgentEnv(t)
	home := t.TempDir()
	profile := t.TempDir()
	t.Setenv(agentroots.ClaudeListEnv, profile)

	const cwd = "/work/app"
	writeClaudeTranscript(t, filepath.Join(profile, "projects", "-work-app", invariantSession+".jsonl"), "Profile Title")

	title := session.NewResolver(home).SessionName("claude", cwd, invariantSession)
	page, err := conversation.NewReader(home).Read("claude", invariantSession, "", 80)
	if err != nil {
		t.Fatal(err)
	}

	if title == "" {
		t.Errorf("title did not resolve from the configured profile")
	}
	if !page.Available {
		t.Errorf("transcript did not resolve from the configured profile: reason=%q", page.Reason)
	}
	if title != "" && !page.Available {
		t.Fatalf("INVARIANT BROKEN: title %q resolved while the transcript reported %q", title, page.Reason)
	}
	if page.Available && page.Total != 2 {
		t.Errorf("page total = %d, want the prompt and the answer", page.Total)
	}
}

// The same agreement must hold for the home default with no configuration at
// all, so the fix is a no-op for a single-profile install.
func TestTitleAndTranscriptAgreeForTheHomeDefault(t *testing.T) {
	clearAgentEnv(t)
	home := t.TempDir()

	const cwd = "/work/app"
	writeClaudeTranscript(t, filepath.Join(home, ".claude", "projects", "-work-app", invariantSession+".jsonl"), "Home Title")

	title := session.NewResolver(home).SessionName("claude", cwd, invariantSession)
	page, err := conversation.NewReader(home).Read("claude", invariantSession, "", 80)
	if err != nil {
		t.Fatal(err)
	}
	if title == "" || !page.Available {
		t.Fatalf("INVARIANT BROKEN: title=%q available=%v reason=%q", title, page.Available, page.Reason)
	}
}

// And they must fail TOGETHER when the session exists in no configured root,
// with the reason string that distinguishes a path-resolution failure from a
// missing session id. A title without a transcript here would be the original
// bug reappearing.
func TestTitleAndTranscriptFailTogetherWhenNoRootHasTheSession(t *testing.T) {
	clearAgentEnv(t)
	home := t.TempDir()
	profile := t.TempDir()
	t.Setenv(agentroots.ClaudeListEnv, profile)

	// A project directory exists in both roots, but neither holds this session.
	for _, root := range []string{filepath.Join(profile, "projects"), filepath.Join(home, ".claude", "projects")} {
		if err := os.MkdirAll(filepath.Join(root, "-work-app"), 0o755); err != nil {
			t.Fatal(err)
		}
	}

	title := session.NewResolver(home).SessionName("claude", "/work/app", invariantSession)
	page, err := conversation.NewReader(home).Read("claude", invariantSession, "", 80)
	if err != nil {
		t.Fatal(err)
	}
	if title != "" {
		t.Errorf("title = %q, want empty when no root holds the session", title)
	}
	if page.Available {
		t.Errorf("transcript resolved from nowhere: %#v", page.Entries)
	}
	if page.Reason != "No conversation log is available for this session." {
		t.Errorf("reason = %q, want the path-resolution reason verbatim", page.Reason)
	}
}

// The exact production shape of the reported bug: the pane's agent used a
// non-default config directory, so the relay's CLAUDE_CONFIG_DIR names a
// profile, while the transcript the title is read from sits under ~/.claude.
// Before the fix the reader honoured CLAUDE_CONFIG_DIR and searched only the
// profile while the resolver ignored it and searched only ~/.claude, so the
// title resolved and the transcript did not. This file cannot be compiled
// against v0.17.5 to demonstrate that - it imports internal/agentroots, which
// the fix introduces - so what it guards is the fixed behaviour: the two
// halves agree once the variable is honoured on both paths.
func TestLegacyConfigDirDoesNotSplitTitleFromTranscript(t *testing.T) {
	clearAgentEnv(t)
	home := t.TempDir()
	profile := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", profile)
	if err := os.MkdirAll(filepath.Join(profile, "projects"), 0o755); err != nil {
		t.Fatal(err)
	}

	writeClaudeTranscript(t, filepath.Join(home, ".claude", "projects", "-work-app", invariantSession+".jsonl"), "Home Title")

	title := session.NewResolver(home).SessionName("claude", "/work/app", invariantSession)
	page, err := conversation.NewReader(home).Read("claude", invariantSession, "", 80)
	if err != nil {
		t.Fatal(err)
	}
	if title != "" && !page.Available {
		t.Fatalf("INVARIANT BROKEN, the reported bug: title %q resolved while the transcript reported %q",
			title, page.Reason)
	}
	if title == "" || !page.Available {
		t.Fatalf("both halves should resolve: title=%q available=%v reason=%q", title, page.Available, page.Reason)
	}
}

// A missing session id is a different failure from a path that cannot be
// resolved, and the two reason strings are the only signal that tells an
// operator which one happened. Pin both.
func TestMissingSessionIDKeepsItsOwnReason(t *testing.T) {
	clearAgentEnv(t)
	page, err := conversation.NewReader(t.TempDir()).Read("claude", "", "", 80)
	if err != nil {
		t.Fatal(err)
	}
	if page.Available || page.Reason != "This agent has not reported a conversation session yet." {
		t.Fatalf("page = %#v, want the missing-session-id reason verbatim", page)
	}
}

// The same session id in two roots, which is what a copied or restored
// transcript looks like. conversation.Reader takes the transcript from the
// first root that HOLDS the file; the title must come from that same root even
// though the first root's copy has no summary record yet and the second root's
// does. Answering with the first NON-EMPTY title across the roots pairs root
// 2's title with root 1's transcript: a correct-looking header over someone
// else's conversation, which is the original bug in a different disguise.
func TestTitleAndTranscriptComeFromTheSameRootForADuplicateSessionID(t *testing.T) {
	clearAgentEnv(t)
	home := t.TempDir()
	profile := t.TempDir()
	t.Setenv(agentroots.ClaudeListEnv, profile)

	writeClaudeTranscriptAnswering(t,
		filepath.Join(profile, "projects", "-work-app", invariantSession+".jsonl"), "", "first root answer")
	writeClaudeTranscriptAnswering(t,
		filepath.Join(home, ".claude", "projects", "-work-app", invariantSession+".jsonl"),
		"Second Root Title", "second root answer")

	title := session.NewResolver(home).SessionName("claude", "/work/app", invariantSession)
	page, err := conversation.NewReader(home).Read("claude", invariantSession, "", 80)
	if err != nil {
		t.Fatal(err)
	}
	if !page.Available {
		t.Fatalf("transcript did not resolve at all: reason=%q", page.Reason)
	}

	var answers []string
	for _, entry := range page.Entries {
		if entry.Role == "assistant" {
			answers = append(answers, entry.Text)
		}
	}
	if len(answers) != 1 || answers[0] != "first root answer" {
		t.Fatalf("transcript answers = %q, want the first root's %q", answers, "first root answer")
	}
	if title == "Second Root Title" {
		t.Fatalf("INVARIANT BROKEN: the title came from the second root while the transcript came from the first")
	}
	if title != "" {
		t.Fatalf("title = %q, want the first root's own title - it has no summary record, so it is empty", title)
	}
}

// writeCodexIndexEntry appends one session_index.jsonl record to codexHome -
// the file both session.codexSessionName and conversation's Codex Locate
// path key off id, keyed by the config directory (not the "sessions" leaf)
// per agentroots.CodexHomes.
func writeCodexIndexEntry(t *testing.T, codexHome, sessionID, threadName string) {
	t.Helper()
	if err := os.MkdirAll(codexHome, 0o755); err != nil {
		t.Fatal(err)
	}
	entry, err := json.Marshal(map[string]any{"id": sessionID, "thread_name": threadName})
	if err != nil {
		t.Fatal(err)
	}
	f, err := os.OpenFile(filepath.Join(codexHome, "session_index.jsonl"), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if _, err := f.Write(append(entry, '\n')); err != nil {
		t.Fatal(err)
	}
}

// writeCodexRollout writes a minimal rollout file under codexHome/sessions -
// one user turn and one assistant turn carrying answer, so a test that puts
// the same session id in two roots can tell which root the reader actually
// read. Serves the same purpose writeClaudeTranscriptAnswering does for
// Claude. day is the two-digit day-of-month component; the fixture is
// otherwise fixed at 2026-08 so callers only vary what distinguishes roots.
func writeCodexRollout(t *testing.T, codexHome, day, sessionID, answer string) {
	t.Helper()
	dir := filepath.Join(codexHome, "sessions", "2026", "08", day)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	rows := []map[string]any{
		{"timestamp": "2026-08-" + day + "T10:00:00Z", "type": "response_item",
			"payload": map[string]any{"type": "message", "role": "user",
				"content": []any{map[string]any{"type": "input_text", "text": "the prompt"}}}},
		{"timestamp": "2026-08-" + day + "T10:00:01Z", "type": "response_item",
			"payload": map[string]any{"type": "message", "role": "assistant",
				"content": []any{map[string]any{"type": "output_text", "text": answer}}}},
	}
	var buf []byte
	for _, row := range rows {
		encoded, err := json.Marshal(row)
		if err != nil {
			t.Fatal(err)
		}
		buf = append(buf, encoded...)
		buf = append(buf, '\n')
	}
	path := filepath.Join(dir, "rollout-2026-08-"+day+"T10-00-00-"+sessionID+".jsonl")
	if err := os.WriteFile(path, buf, 0o644); err != nil {
		t.Fatal(err)
	}
}

const codexInvariantSession = "223e4567-e89b-12d3-a456-426614174321"

// The baseline agreement must hold for Codex too, mirroring
// TestTitleAndTranscriptAgreeForANonDefaultProfile: a session reachable only
// through a non-default config directory must title and transcript together.
func TestCodexTitleAndTranscriptAgreeForANonDefaultConfigDir(t *testing.T) {
	clearAgentEnv(t)
	home := t.TempDir()
	work := t.TempDir()
	t.Setenv(agentroots.CodexListEnv, work)

	writeCodexIndexEntry(t, work, codexInvariantSession, "Work Session")
	writeCodexRollout(t, work, "22", codexInvariantSession, "the answer")

	title := session.NewResolver(home).SessionName("codex", "/work/app", codexInvariantSession)
	page, err := conversation.NewReader(home).Read("codex", codexInvariantSession, "", 80)
	if err != nil {
		t.Fatal(err)
	}
	if title == "" {
		t.Errorf("title did not resolve from the configured codex home")
	}
	if !page.Available {
		t.Errorf("transcript did not resolve from the configured codex home: reason=%q", page.Reason)
	}
}

// The round-3 blocker, reproduced verbatim from two independent gate
// reviewers: session.codexSessionName answered with the first NON-EMPTY
// thread_name across codexHomes while conversation.findCodexSession answered
// with the first home whose rollout tree HOLDS the session - different
// search key and a different stopping rule. Codex writes the index record
// before it names the thread, so an empty thread_name in the transcript's
// own root is the ordinary fresh-session state, not a corner case; falling
// through to a later root's title pairs it with a transcript that root never
// actually served.
func TestCodexTitleComesFromTheSameRootAsTheTranscriptForADuplicateSessionID(t *testing.T) {
	clearAgentEnv(t)
	home := t.TempDir()
	work := t.TempDir()
	t.Setenv(agentroots.CodexListEnv, work)

	writeCodexIndexEntry(t, work, codexInvariantSession, "")
	writeCodexRollout(t, work, "22", codexInvariantSession, "work conversation")

	homeCodex := filepath.Join(home, ".codex")
	writeCodexIndexEntry(t, homeCodex, codexInvariantSession, "Deploy hotfix")
	writeCodexRollout(t, homeCodex, "21", codexInvariantSession, "home conversation")

	title := session.NewResolver(home).SessionName("codex", "/work/app", codexInvariantSession)
	page, err := conversation.NewReader(home).Read("codex", codexInvariantSession, "", 80)
	if err != nil {
		t.Fatal(err)
	}
	if !page.Available {
		t.Fatalf("transcript did not resolve at all: reason=%q", page.Reason)
	}

	var answers []string
	for _, entry := range page.Entries {
		if entry.Role == "assistant" {
			answers = append(answers, entry.Text)
		}
	}
	if len(answers) != 1 || answers[0] != "work conversation" {
		t.Fatalf("transcript answers = %q, want the work root's %q (earliest configured root wins)",
			answers, "work conversation")
	}
	if title == "Deploy hotfix" {
		t.Fatalf("INVARIANT BROKEN: title came from the home root while the transcript came from the work root")
	}
	if title != "" {
		t.Fatalf("title = %q, want empty - the work root's own index entry has no thread name yet", title)
	}
}

// Codex titles with no transcript at all: before the fix the title was read
// straight from session_index.jsonl with no check that the rollout the
// reader will serve exists, so a pruned or unsynced rollout put a confident
// name over "No conversation log is available for this session."
func TestCodexTitleRequiresAReachableTranscript(t *testing.T) {
	clearAgentEnv(t)
	home := t.TempDir()
	writeCodexIndexEntry(t, filepath.Join(home, ".codex"), codexInvariantSession, "Ghost Title")
	// Deliberately no rollout file anywhere - a pruned or unsynced transcript.

	title := session.NewResolver(home).SessionName("codex", "/work/app", codexInvariantSession)
	page, err := conversation.NewReader(home).Read("codex", codexInvariantSession, "", 80)
	if err != nil {
		t.Fatal(err)
	}
	if page.Available {
		t.Fatalf("transcript resolved from nowhere: %#v", page.Entries)
	}
	if title != "" {
		t.Fatalf("INVARIANT BROKEN: title %q resolved over a session with no reachable transcript", title)
	}
}

// Id validation asymmetry: the resolver used to title any validSessionID
// while the reader requires a canonical UUID for Codex. A non-canonical id
// must be refused identically on both sides even when an index entry and a
// same-shaped rollout both exist for it.
func TestCodexNonCanonicalSessionIDIsRefusedOnBothSides(t *testing.T) {
	clearAgentEnv(t)
	home := t.TempDir()
	const nonCanonical = "sess-456"
	codexHome := filepath.Join(home, ".codex")
	writeCodexIndexEntry(t, codexHome, nonCanonical, "Should Never Title")
	writeCodexRollout(t, codexHome, "22", nonCanonical, "should never be readable")

	title := session.NewResolver(home).SessionName("codex", "/work/app", nonCanonical)
	page, err := conversation.NewReader(home).Read("codex", nonCanonical, "", 80)
	if err != nil {
		t.Fatal(err)
	}
	if page.Available {
		t.Fatalf("INVARIANT BROKEN: reader served a transcript for a non-canonical id: %#v", page.Entries)
	}
	if title != "" {
		t.Fatalf("INVARIANT BROKEN: resolver titled a non-canonical id: %q", title)
	}
}
