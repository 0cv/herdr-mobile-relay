package session

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/0cv/herdr-mobile-relay/internal/agentroots"
)

// TestMain clears every environment variable agentroots consults before any
// test in this package runs. Without this, a developer's exported
// CLAUDE_CONFIG_DIR / CODEX_HOME / PI_CODING_AGENT_DIR / HERDR_*_CONFIG_DIRS
// leaks into every test's Resolver and can override the fixture data it just
// wrote (see clearAgentRootEnv below for the per-test escape hatch used by
// tests that need to reassert a var mid-run). A single TestMain protects every
// test in the package, including ones added later that forget to call
// clearAgentRootEnv themselves - the property the per-test helper alone
// cannot guarantee.
func TestMain(m *testing.M) {
	for _, key := range []string{
		agentroots.ClaudeListEnv,
		agentroots.QoderListEnv,
		agentroots.CodexListEnv,
		agentroots.PiListEnv,
		agentroots.OMPListEnv,
		"CLAUDE_CONFIG_DIR",
		"CODEX_HOME",
		"PI_CODING_AGENT_DIR",
	} {
		os.Setenv(key, "")
	}
	os.Exit(m.Run())
}

func TestQoderSessionName(t *testing.T) {
	home := t.TempDir()
	projDir := filepath.Join(home, ".qoder", "projects", "home-user-myapp")
	os.MkdirAll(projDir, 0o755)

	// Write a session JSONL with a title
	entries := []map[string]any{
		{"type": "summary", "title": "Fix login bug"},
		{"type": "ai-title", "aiTitle": "Authentication fix"},
		{"type": "custom-title", "customTitle": "My Custom Title"},
	}
	var lines []byte
	for _, e := range entries {
		data, _ := json.Marshal(e)
		lines = append(lines, data...)
		lines = append(lines, '\n')
	}
	os.WriteFile(filepath.Join(projDir, "sess-123.jsonl"), lines, 0o644)

	r := NewResolver(home)
	name := r.SessionName("qoder", "/home/user/myapp", "sess-123")
	if name != "My Custom Title" {
		t.Errorf("session name = %q, want 'My Custom Title'", name)
	}
}

func TestQoderLeadingDashSessionName(t *testing.T) {
	home := t.TempDir()
	projDir := filepath.Join(home, ".qoder", "projects", "-home-user-myapp")
	if err := os.MkdirAll(projDir, 0o755); err != nil {
		t.Fatal(err)
	}
	entry := map[string]any{
		"type":        "custom-title",
		"sessionId":   "sess-renamed",
		"customTitle": "Renamed Qoder session",
	}
	data, err := json.Marshal(entry)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(projDir, "sess-renamed.jsonl"), append(data, '\n'), 0o644); err != nil {
		t.Fatal(err)
	}

	if got := NewResolver(home).SessionName("qodercli", "/home/user/myapp", "sess-renamed"); got != "Renamed Qoder session" {
		t.Fatalf("session name = %q, want %q", got, "Renamed Qoder session")
	}
}

func TestPiSessionName(t *testing.T) {
	home := t.TempDir()
	sessionDir := filepath.Join(home, ".pi", "agent", "sessions", "--home-user-app--")
	if err := os.MkdirAll(sessionDir, 0o755); err != nil {
		t.Fatal(err)
	}
	sessionPath := filepath.Join(sessionDir, "2026-08-01T09-26-04-302Z_session.jsonl")
	data := []byte(
		"{\"type\":\"session\",\"version\":3,\"cwd\":\"/home/user/app\"}\n" +
			"{\"type\":\"session_info\",\"name\":\"Pi initial\"}\n" +
			"{\"type\":\"session_info\",\"name\":\"Pi renamed\"}\n",
	)
	if err := os.WriteFile(sessionPath, data, 0o644); err != nil {
		t.Fatal(err)
	}

	for _, agent := range []string{"pi", "pi-coding-agent"} {
		if got := NewResolver(home).SessionName(agent, "/home/user/app", sessionPath); got != "Pi renamed" {
			t.Errorf("agent %q session name = %q, want %q", agent, got, "Pi renamed")
		}
	}
}

func TestClaudeSessionName(t *testing.T) {
	home := t.TempDir()
	projDir := filepath.Join(home, ".claude", "projects", "home-user-app")
	os.MkdirAll(projDir, 0o755)

	entry := map[string]any{"type": "summary", "summary": "Refactor database layer"}
	data, _ := json.Marshal(entry)
	os.WriteFile(filepath.Join(projDir, "abc.jsonl"), append(data, '\n'), 0o644)

	r := NewResolver(home)
	name := r.SessionName("claude-code", "/home/user/app", "abc")
	if name != "Refactor database layer" {
		t.Errorf("session name = %q, want 'Refactor database layer'", name)
	}
}

func TestCodexSessionName(t *testing.T) {
	home := t.TempDir()
	codexDir := filepath.Join(home, ".codex")
	os.MkdirAll(codexDir, 0o755)

	entry := map[string]any{"id": "sess-456", "thread_name": "Build API endpoint"}
	data, _ := json.Marshal(entry)
	os.WriteFile(filepath.Join(codexDir, "session_index.jsonl"), append(data, '\n'), 0o644)

	r := NewResolver(home)
	name := r.SessionName("codex", "/tmp", "sess-456")
	if name != "Build API endpoint" {
		t.Errorf("session name = %q, want 'Build API endpoint'", name)
	}
}

func TestOMPSessionName(t *testing.T) {
	home := t.TempDir()
	sessionDir := filepath.Join(home, ".omp", "agent", "sessions", "-home-user-app")
	if err := os.MkdirAll(sessionDir, 0o755); err != nil {
		t.Fatal(err)
	}
	sessionPath := filepath.Join(sessionDir, "2026-08-01T07-51-16-639Z_session.jsonl")
	data := []byte(
		"{\"type\":\"title\",\"title\":\"session_fix\"}\n" +
			"{\"type\":\"session\",\"version\":3,\"cwd\":\"/home/user/app\"}\n" +
			"{\"type\":\"title_change\",\"title\":\"historical_title\"}\n",
	)
	if err := os.WriteFile(sessionPath, data, 0o644); err != nil {
		t.Fatal(err)
	}

	resolver := NewResolver(home)
	if got := resolver.SessionName("omp", "/home/user/app", sessionPath); got != "session_fix" {
		t.Fatalf("session name = %q, want %q", got, "session_fix")
	}

	data = []byte(
		"{\"type\":\"title\",\"title\":\"session_fix_renamed\"}\n" +
			"{\"type\":\"session\",\"version\":3,\"cwd\":\"/home/user/app\"}\n" +
			"{\"type\":\"title_change\",\"title\":\"session_fix\"}\n",
	)
	if err := os.WriteFile(sessionPath, data, 0o644); err != nil {
		t.Fatal(err)
	}
	if got := resolver.SessionName("omp", "/home/user/app", sessionPath); got != "session_fix_renamed" {
		t.Fatalf("updated session name = %q, want %q", got, "session_fix_renamed")
	}
}

func TestOMPSessionNameFallsBackToHistoricalTitleChange(t *testing.T) {
	home := t.TempDir()
	sessionDir := filepath.Join(home, ".omp", "agent", "sessions", "-home-user-app")
	if err := os.MkdirAll(sessionDir, 0o755); err != nil {
		t.Fatal(err)
	}
	sessionPath := filepath.Join(sessionDir, "legacy.jsonl")
	data := []byte(
		"{\"type\":\"session\",\"version\":3,\"cwd\":\"/home/user/app\"}\n" +
			"{\"type\":\"title_change\",\"title\":\"legacy_title\"}\n",
	)
	if err := os.WriteFile(sessionPath, data, 0o644); err != nil {
		t.Fatal(err)
	}

	if got := NewResolver(home).SessionName("omp", "/home/user/app", sessionPath); got != "legacy_title" {
		t.Fatalf("legacy session name = %q, want %q", got, "legacy_title")
	}
}

func TestEmptySessionID(t *testing.T) {
	home := t.TempDir()
	r := NewResolver(home)
	name := r.SessionName("claude", "/tmp", "")
	if name != "" {
		t.Errorf("empty session ID should return empty, got %q", name)
	}
}

func TestUnknownAgent(t *testing.T) {
	home := t.TempDir()
	r := NewResolver(home)
	name := r.SessionName("unknown-agent", "/tmp", "sess-1")
	if name != "" {
		t.Errorf("unknown agent should return empty, got %q", name)
	}
}

func TestCacheTTL(t *testing.T) {
	home := t.TempDir()
	projDir := filepath.Join(home, ".qoder", "projects", "home-user-proj")
	os.MkdirAll(projDir, 0o755)

	entry := map[string]any{"type": "summary", "title": "First Title"}
	data, _ := json.Marshal(entry)
	os.WriteFile(filepath.Join(projDir, "s1.jsonl"), append(data, '\n'), 0o644)

	r := NewResolver(home)
	name1 := r.SessionName("qoder", "/home/user/proj", "s1")
	if name1 != "First Title" {
		t.Fatalf("first call = %q", name1)
	}

	// Overwrite file — the source signature invalidates the cache immediately.
	entry2 := map[string]any{"type": "summary", "title": "Second Title"}
	data2, _ := json.Marshal(entry2)
	os.WriteFile(filepath.Join(projDir, "s1.jsonl"), append(data2, '\n'), 0o644)

	name2 := r.SessionName("qoder", "/home/user/proj", "s1")
	if name2 != "Second Title" {
		t.Errorf("refreshed call = %q, want 'Second Title'", name2)
	}
}

func TestExactSessionIDDoesNotFallBackToOnlyOtherFile(t *testing.T) {
	home := t.TempDir()
	projDir := filepath.Join(home, ".claude", "projects", "home-user-app")
	if err := os.MkdirAll(projDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(projDir, "other.jsonl"), []byte("{\"type\":\"custom-title\",\"customTitle\":\"Wrong\"}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := NewResolver(home).SessionName("claude", "/home/user/app", "wanted"); got != "" {
		t.Fatalf("session name = %q, want empty exact-ID miss", got)
	}
}

// clearAgentRootEnv unsets every variable agentroots consults, so a developer's
// exported shell environment cannot add roots to a test and make it flaky.
func clearAgentRootEnv(t *testing.T) {
	t.Helper()
	for _, key := range []string{
		agentroots.ClaudeListEnv,
		agentroots.QoderListEnv,
		agentroots.CodexListEnv,
		agentroots.PiListEnv,
		agentroots.OMPListEnv,
		"CLAUDE_CONFIG_DIR",
		"CODEX_HOME",
		"PI_CODING_AGENT_DIR",
	} {
		t.Setenv(key, "")
	}
}

func writeTitleFile(t *testing.T, path, title string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	data, err := json.Marshal(map[string]any{"type": "custom-title", "customTitle": title})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, append(data, '\n'), 0o644); err != nil {
		t.Fatal(err)
	}
}

// Pins the architectural constraint whose absence produced the original bug:
// NewResolver must take every root from the agentroots seam instead of
// hand-rolling paths of its own. Reverting any field below to a
// filepath.Join(r.home, ...) fails this, because the seam also carries the
// configured list entry set up here.
//
// It deliberately does NOT claim to cover the title/transcript invariant.
// Comparing this package's fields against the seam can only restate this
// package's own construction, and conversation.Reader's roots are unexported
// and live in another package. The behavioural half - title and transcript
// resolving, or failing, together - is covered where both types are visible, in
// internal/app/conversation_invariant_test.go. Keep the two in step.
func TestResolverSourcesEveryRootFromTheSeam(t *testing.T) {
	clearAgentRootEnv(t)
	home := t.TempDir()
	t.Setenv(agentroots.ClaudeListEnv, t.TempDir())

	r := NewResolver(home)
	for _, c := range []struct {
		name string
		got  []string
		want []string
	}{
		{"claudeRoots", r.claudeRoots, agentroots.Claude(home)},
		{"qoderRoots", r.qoderRoots, agentroots.Qoder(home)},
		{"codexHomes", r.codexHomes, agentroots.CodexHomes(home)},
		{"piRoots", r.piRoots, agentroots.Pi(home)},
		{"ompRoots", r.ompRoots, agentroots.OMP(home)},
	} {
		if !slices.Equal(c.got, c.want) {
			t.Errorf("%s = %v, want the seam's %v", c.name, c.got, c.want)
		}
	}
}

// A root whose project directory exists but does not hold this session must not
// end the search: the session may live in another configured profile. Returning
// "" from the first root fails this test.
func TestClaudeTitleFromSecondRootWhenFirstRootLacksTheSession(t *testing.T) {
	clearAgentRootEnv(t)
	home := t.TempDir()
	profile := t.TempDir()
	t.Setenv(agentroots.ClaudeListEnv, profile)

	writeTitleFile(t, filepath.Join(profile, "projects", "home-user-app", "unrelated.jsonl"), "First root title")
	writeTitleFile(t, filepath.Join(home, ".claude", "projects", "home-user-app", "wanted.jsonl"), "Second root title")

	r := NewResolver(home)
	if len(r.claudeRoots) != 2 || r.claudeRoots[0] != filepath.Join(profile, "projects") {
		t.Fatalf("claudeRoots = %v, want the configured profile first and the home default kept", r.claudeRoots)
	}
	if got := r.SessionName("claude", "/home/user/app", "wanted"); got != "Second root title" {
		t.Fatalf("session name = %q, want %q", got, "Second root title")
	}
}

func TestOMPSessionPathAcceptedFromNonDefaultRoot(t *testing.T) {
	clearAgentRootEnv(t)
	home := t.TempDir()
	profile := t.TempDir()
	t.Setenv(agentroots.OMPListEnv, profile)

	sessionPath := filepath.Join(profile, "sessions", "-home-user-app", "s.jsonl")
	if err := os.MkdirAll(filepath.Dir(sessionPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(sessionPath, []byte("{\"type\":\"title\",\"title\":\"profile_omp\"}\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	if got := NewResolver(home).SessionName("omp", "/home/user/app", sessionPath); got != "profile_omp" {
		t.Fatalf("session name = %q, want %q", got, "profile_omp")
	}
}

// Containment regression guard: a path outside every configured root is
// rejected even though one configured root resolves fine.
func TestOMPSessionPathRejectedOutsideEveryRoot(t *testing.T) {
	clearAgentRootEnv(t)
	home := t.TempDir()
	profile := t.TempDir()
	outside := t.TempDir()
	t.Setenv(agentroots.OMPListEnv, profile)

	if err := os.MkdirAll(filepath.Join(profile, "sessions"), 0o755); err != nil {
		t.Fatal(err)
	}
	outsidePath := filepath.Join(outside, "escaped.jsonl")
	if err := os.WriteFile(outsidePath, []byte("{\"type\":\"title\",\"title\":\"escaped_title\"}\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	if got := NewResolver(home).SessionName("omp", "/home/user/app", outsidePath); got != "" {
		t.Fatalf("session name = %q, want empty for a path outside every root", got)
	}
}

// Containment regression guard: a symlink sitting inside a configured root but
// resolving outside every root is rejected, because the check evaluates
// symlinks before comparing.
func TestOMPSessionPathRejectedSymlinkEscapingEveryRoot(t *testing.T) {
	clearAgentRootEnv(t)
	home := t.TempDir()
	profile := t.TempDir()
	outside := t.TempDir()
	t.Setenv(agentroots.OMPListEnv, profile)

	target := filepath.Join(outside, "escaped.jsonl")
	if err := os.WriteFile(target, []byte("{\"type\":\"title\",\"title\":\"escaped_title\"}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(profile, "sessions", "-home-user-app", "link.jsonl")
	if err := os.MkdirAll(filepath.Dir(link), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}

	if got := NewResolver(home).SessionName("omp", "/home/user/app", link); got != "" {
		t.Fatalf("session name = %q, want empty for a symlink escaping every root", got)
	}
}

func TestCodexTitleFromSecondConfiguredHome(t *testing.T) {
	clearAgentRootEnv(t)
	home := t.TempDir()
	first := t.TempDir()
	second := t.TempDir()
	t.Setenv(agentroots.CodexListEnv, strings.Join([]string{first, second}, string(filepath.ListSeparator)))

	data, err := json.Marshal(map[string]any{"id": "sess-second", "thread_name": "Second home thread"})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(second, "session_index.jsonl"), append(data, '\n'), 0o644); err != nil {
		t.Fatal(err)
	}

	if got := NewResolver(home).SessionName("codex", "/tmp", "sess-second"); got != "Second home thread" {
		t.Fatalf("session name = %q, want %q; the first home has no index file and must not end the search", got, "Second home thread")
	}
}

// Mirrors TestCacheTTL, but for a session reachable only through a non-default
// root: sourceSignature has to sign the file the title actually came from.
func TestCacheInvalidationWithNonDefaultClaudeRoot(t *testing.T) {
	clearAgentRootEnv(t)
	home := t.TempDir()
	profile := t.TempDir()
	t.Setenv(agentroots.ClaudeListEnv, profile)

	sessionFile := filepath.Join(profile, "projects", "home-user-proj", "s1.jsonl")
	writeTitleFile(t, sessionFile, "First Title")

	r := NewResolver(home)
	if got := r.SessionName("claude", "/home/user/proj", "s1"); got != "First Title" {
		t.Fatalf("first call = %q, want %q", got, "First Title")
	}

	writeTitleFile(t, sessionFile, "Second Title After Rename")
	if got := r.SessionName("claude", "/home/user/proj", "s1"); got != "Second Title After Rename" {
		t.Fatalf("refreshed call = %q, want %q", got, "Second Title After Rename")
	}
}

// Regression guard for the title cache. projectSessionTitle keeps searching
// roots until one yields a non-empty title, so sourceSignature must sign the
// candidate in EVERY root. Signing only the first root that happens to have a
// project directory for this cwd - which it did - signs a file the title never
// came from, and the cached title then survives an edit to the file it really
// came from for the whole 60s TTL.
func TestTitleCacheTracksTheRootTheTitleActuallyCameFrom(t *testing.T) {
	clearAgentRootEnv(t)
	home := t.TempDir()
	first := t.TempDir()
	second := t.TempDir()
	t.Setenv(agentroots.ClaudeListEnv, first+string(os.PathListSeparator)+second)

	// The first root has a project directory for this cwd but NOT this session,
	// so the title has to come from the second root.
	writeTitleFile(t, filepath.Join(first, "projects", "home-user-app", "other.jsonl"), "Unrelated")
	target := filepath.Join(second, "projects", "home-user-app", "wanted.jsonl")
	writeTitleFile(t, target, "First Title")

	r := NewResolver(home)
	if got := r.SessionName("claude", "/home/user/app", "wanted"); got != "First Title" {
		t.Fatalf("first call = %q, want %q from the second root", got, "First Title")
	}

	writeTitleFile(t, target, "Renamed Title")
	if got := r.SessionName("claude", "/home/user/app", "wanted"); got != "Renamed Title" {
		t.Fatalf("second call = %q, want %q - the cache signed the wrong root", got, "Renamed Title")
	}
}

// The two paths this change unified did not agree before it: the conversation
// reader honoured CLAUDE_CONFIG_DIR and searched only that directory, while the
// resolver ignored the variable and searched only ~/.claude. Neither old
// behaviour can be reproduced byte-for-byte on both paths at once, because that
// disagreement was the bug. What must hold instead is that the change only ever
// WIDENS: every lookup that resolved at v0.17.5 still resolves, to the same
// value. This test pins that for the legacy variable - the home default stays
// searched (the old resolver behaviour) and the configured directory is now
// searched too (the old reader behaviour), with the configured one taking
// precedence in the order.
func TestLegacyClaudeConfigDirWidensWithoutDroppingTheHomeDefault(t *testing.T) {
	clearAgentRootEnv(t)
	home := t.TempDir()
	profile := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", profile)

	writeTitleFile(t, filepath.Join(home, ".claude", "projects", "home-user-app", "sess-home.jsonl"), "Home Title")
	writeTitleFile(t, filepath.Join(profile, "projects", "home-user-app", "sess-profile.jsonl"), "Profile Title")

	r := NewResolver(home)
	if got, want := r.claudeRoots, []string{
		filepath.Join(profile, "projects"),
		filepath.Join(home, ".claude", "projects"),
	}; !slices.Equal(got, want) {
		t.Fatalf("claudeRoots = %v, want the configured directory first then the home default %v", got, want)
	}

	// Old resolver behaviour: ~/.claude was always searched. Still true.
	if got := r.SessionName("claude", "/home/user/app", "sess-home"); got != "Home Title" {
		t.Errorf("home-default session title = %q, want %q - the home default must stay searched", got, "Home Title")
	}
	// Old reader behaviour: CLAUDE_CONFIG_DIR was honoured. Now titles honour it too.
	if got := r.SessionName("claude", "/home/user/app", "sess-profile"); got != "Profile Title" {
		t.Errorf("configured-directory session title = %q, want %q", got, "Profile Title")
	}
}

// Same widening guarantee for the agents whose roots used to be hardcoded with
// no override at all, and for Pi/OMP sharing PI_CODING_AGENT_DIR: setting the
// legacy variable must not cost either agent its own home default.
func TestLegacyPiCodingAgentDirKeepsBothHomeDefaults(t *testing.T) {
	clearAgentRootEnv(t)
	home := t.TempDir()
	profile := t.TempDir()
	t.Setenv("PI_CODING_AGENT_DIR", profile)

	r := NewResolver(home)
	for _, c := range []struct {
		name string
		got  []string
		want []string
	}{
		{"piRoots", r.piRoots, []string{
			filepath.Join(profile, "sessions"),
			filepath.Join(home, ".pi", "agent", "sessions"),
		}},
		{"ompRoots", r.ompRoots, []string{
			filepath.Join(profile, "sessions"),
			filepath.Join(home, ".omp", "agent", "sessions"),
		}},
	} {
		if !slices.Equal(c.got, c.want) {
			t.Errorf("%s = %v, want %v", c.name, c.got, c.want)
		}
	}
}
