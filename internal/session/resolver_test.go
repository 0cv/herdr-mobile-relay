package session

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/0cv/herdr-mobile-relay/internal/agentroots"
	"github.com/0cv/herdr-mobile-relay/internal/conversation"
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

// A Codex session is titled from session_index.jsonl, but only once the
// rollout the conversation reader would serve exists beside it: the reader's
// Locate decides whether this pane has a transcript at all, and a title is
// only ever read from the home whose rollout answered.
func TestCodexSessionName(t *testing.T) {
	home := t.TempDir()
	const id = "3f9a1c2e-4b5d-4a6f-8c7d-9e0f1a2b3c4d"
	codexHome := filepath.Join(home, ".codex")
	writeCodexIndex(t, codexHome, id, "Build API endpoint")
	writeCodexRollout(t, codexHome, id, "2026/08/22")

	r := NewResolver(home)
	if name := r.SessionName("codex", "/tmp", id); name != "Build API endpoint" {
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
	if roots := r.claudeRoots(); len(roots) != 2 || roots[0] != filepath.Join(profile, "projects") {
		t.Fatalf("claudeRoots = %v, want the configured profile first and the home default kept", roots)
	}
	if got := r.SessionName("claude", "/home/user/app", "wanted"); got != "Second root title" {
		t.Fatalf("session name = %q, want %q", got, "Second root title")
	}
}

// Reader parity (Bug 1): the transcript reader stops at the first root that
// HOLDS the session file, regardless of its contents. The resolver must stop
// at that same root even when its copy has no title record yet - the normal
// state of a session Claude has not summarised - instead of answering with a
// later root's title. Otherwise the phone pairs root 2's title with root 1's
// transcript.
func TestClaudeUntitledSessionInFirstRootIsNotOverriddenBySecondRoot(t *testing.T) {
	clearAgentRootEnv(t)
	home := t.TempDir()
	profile := t.TempDir()
	t.Setenv(agentroots.ClaudeListEnv, profile)

	untitled := filepath.Join(profile, "projects", "home-user-app", "wanted.jsonl")
	if err := os.MkdirAll(filepath.Dir(untitled), 0o755); err != nil {
		t.Fatal(err)
	}
	// Transcript lines only, no title record - extractTitle returns "" here.
	if err := os.WriteFile(untitled, []byte("{\"type\":\"user\",\"text\":\"hi\"}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	writeTitleFile(t, filepath.Join(home, ".claude", "projects", "home-user-app", "wanted.jsonl"), "Second root title")

	if got := NewResolver(home).SessionName("claude", "/home/user/app", "wanted"); got != "" {
		t.Fatalf("session name = %q, want empty - the first root holds the session, so it must answer even untitled", got)
	}
}

// Bug 2: with no reported session id the sole-.jsonl fallback may only guess
// within the root that owns the cwd's project directory. Two candidates there
// is ambiguous - it must not fall through to a later root, whose sole
// transcript answers for a pane whose sessions live in the earlier root. The
// phone would then show a confidently wrong title while the conversation view
// shows nothing at all, because the reader rejects empty session ids outright.
// Pre-multi-root this was impossible: only ~/.claude was consulted.
func TestEmptySessionIDAmbiguousFirstRootDoesNotFallThroughToSecondRoot(t *testing.T) {
	clearAgentRootEnv(t)
	home := t.TempDir()
	profile := t.TempDir()
	t.Setenv(agentroots.ClaudeListEnv, profile)

	writeTitleFile(t, filepath.Join(profile, "projects", "home-user-app", "a.jsonl"), "Title A")
	writeTitleFile(t, filepath.Join(profile, "projects", "home-user-app", "b.jsonl"), "Title B")
	writeTitleFile(t, filepath.Join(home, ".claude", "projects", "home-user-app", "only.jsonl"), "Stale home title")

	if got := NewResolver(home).SessionName("claude", "/home/user/app", ""); got != "" {
		t.Fatalf("session name = %q, want empty - an ambiguous owning root is not a licence to guess from another tree", got)
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

	const id = "7c1d2e3f-0a1b-4c2d-8e3f-4a5b6c7d8e9f"
	writeCodexIndex(t, second, id, "Second home thread")
	writeCodexRollout(t, second, id, "2026/08/22")

	if got := NewResolver(home).SessionName("codex", "/tmp", id); got != "Second home thread" {
		t.Fatalf("session name = %q, want %q; the first home holds nothing for this session and must not end the search", got, "Second home thread")
	}
}

// Mirrors TestCacheTTL, but for a session reachable only through a non-default
// root: the cache signature has to sign the file the title actually came
// from. The fixture sits in the SECOND root on purpose - with it in the first
// root a signature that only ever signed roots[0] would pass, and the bug
// this test names would go unnoticed.
func TestCacheInvalidationWithNonDefaultClaudeRoot(t *testing.T) {
	clearAgentRootEnv(t)
	home := t.TempDir()
	first := t.TempDir()
	second := t.TempDir()
	t.Setenv(agentroots.ClaudeListEnv, first+string(os.PathListSeparator)+second)

	// The first root holds no project directory for this cwd at all.
	if err := os.MkdirAll(filepath.Join(first, "projects"), 0o755); err != nil {
		t.Fatal(err)
	}
	sessionFile := filepath.Join(second, "projects", "home-user-proj", "s1.jsonl")
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

// Regression guard for the title cache. The signature must sign the file the
// title was actually read from - the transcript Locate answered with,
// whichever root it sits in. Signing only the first root that happens to have
// a project directory for this cwd - which it once did - signs a file the
// title never came from, and the cached title then survives an edit to the
// file it really came from for the whole 60s TTL.
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
	if got, want := r.claudeRoots(), []string{
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

// Same widening guarantee for the agents whose roots used to be hardcoded
// with no override at all, and for Pi/OMP sharing PI_CODING_AGENT_DIR. The
// root lists themselves are pinned in agentroots_test.go ("shared single var,
// distinct base"); this asserts the behaviour those lists buy at the
// resolver: a transcript under the configured directory and one under each
// agent's own home default both resolve, so setting the legacy variable never
// costs an agent its home default.
func TestLegacyPiCodingAgentDirKeepsBothHomeDefaults(t *testing.T) {
	clearAgentRootEnv(t)
	home := t.TempDir()
	profile := t.TempDir()
	t.Setenv("PI_CODING_AGENT_DIR", profile)

	r := NewResolver(home)
	for _, c := range []struct {
		agent, record, want string
		paths               []string
	}{
		{"pi", "{\"type\":\"session_info\",\"name\":\"pi_title\"}\n", "pi_title", []string{
			filepath.Join(profile, "sessions", "-home-user-app", "pi-configured.jsonl"),
			filepath.Join(home, ".pi", "agent", "sessions", "-home-user-app", "pi-default.jsonl"),
		}},
		{"omp", "{\"type\":\"title\",\"title\":\"omp_title\"}\n", "omp_title", []string{
			filepath.Join(profile, "sessions", "-home-user-app", "omp-configured.jsonl"),
			filepath.Join(home, ".omp", "agent", "sessions", "-home-user-app", "omp-default.jsonl"),
		}},
	} {
		for _, sessionPath := range c.paths {
			if err := os.MkdirAll(filepath.Dir(sessionPath), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(sessionPath, []byte(c.record), 0o644); err != nil {
				t.Fatal(err)
			}
			if got := r.SessionName(c.agent, "/home/user/app", sessionPath); got != c.want {
				t.Errorf("%s title for %s = %q, want %q", c.agent, sessionPath, got, c.want)
			}
		}
	}
}

// Roots are resolved per call, never snapshotted in the constructor. The relay
// is a long-lived user service and `omp --profile work` creates its profile
// directory the first time it runs, which is normally long after the resolver
// was built. A resolver holding a snapshot never sees the profile, and every
// pane in it stays unresolvable until someone restarts the service.
func TestProfileCreatedAfterTheResolverWasBuiltIsStillResolved(t *testing.T) {
	clearAgentRootEnv(t)
	home := t.TempDir()

	r := NewResolver(home)

	for _, c := range []struct{ agent, configRoot, record string }{
		{"omp", ".omp", "{\"type\":\"title\",\"title\":\"late_profile\"}\n"},
		{"pi", ".pi", "{\"type\":\"session_info\",\"name\":\"late_profile\"}\n"},
	} {
		sessionPath := filepath.Join(home, c.configRoot, "profiles", "work", "agent", "sessions", "-home-user-app", "s.jsonl")
		if err := os.MkdirAll(filepath.Dir(sessionPath), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(sessionPath, []byte(c.record), 0o644); err != nil {
			t.Fatal(err)
		}
		if got := r.SessionName(c.agent, "/home/user/app", sessionPath); got != "late_profile" {
			t.Errorf("%s session name = %q, want %q from a profile created after NewResolver returned", c.agent, got, "late_profile")
		}
	}
}

// os.ReadDir reports the directory entry's own type bits, so DirEntry.IsDir()
// is false for a symlink to a directory. Classifying with it would drop a
// project directory symlinked in from a dotfiles repo, which is a mainstream
// way to keep agent configuration - so the shared session predicate has to
// stat through the link. The symlink target sits OUTSIDE the root on purpose:
// the reader accepts that (its containment is against the project directory,
// not the root), and the resolver has to accept it too or the two split.
func TestSymlinkedProjectDirectoryIsSearchedForTheSession(t *testing.T) {
	clearAgentRootEnv(t)
	home := t.TempDir()
	elsewhere := t.TempDir()

	target := filepath.Join(elsewhere, "dotfiles-app")
	writeTitleFile(t, filepath.Join(target, "sess-link.jsonl"), "Symlinked project title")
	projects := filepath.Join(home, ".claude", "projects")
	if err := os.MkdirAll(projects, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, filepath.Join(projects, "home-user-app")); err != nil {
		t.Fatal(err)
	}

	if got := NewResolver(home).SessionName("claude", "/home/user/app", "sess-link"); got != "Symlinked project title" {
		t.Fatalf("session name = %q, want %q from a symlinked project directory", got, "Symlinked project title")
	}
}

// The same hazard on the empty-session-id path, which still anchors on the
// cwd: findProjectDir identifies the project directory by the cwd file inside
// it when the encoded name does not match, and has to stat through the link
// to see a directory at all.
func TestSymlinkedProjectDirectoryIsFoundByItsCwdFile(t *testing.T) {
	clearAgentRootEnv(t)
	home := t.TempDir()
	elsewhere := t.TempDir()

	target := filepath.Join(elsewhere, "dotfiles-app")
	writeTitleFile(t, filepath.Join(target, "sess-link.jsonl"), "Cwd-matched symlink title")
	if err := os.WriteFile(filepath.Join(target, "cwd"), []byte("/home/user/app\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	projects := filepath.Join(home, ".claude", "projects")
	if err := os.MkdirAll(projects, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, filepath.Join(projects, "not-the-encoded-name")); err != nil {
		t.Fatal(err)
	}

	if got := NewResolver(home).SessionName("claude", "/home/user/app", ""); got != "Cwd-matched symlink title" {
		t.Fatalf("session name = %q, want %q from a symlinked project directory matched by its cwd file", got, "Cwd-matched symlink title")
	}
}

// GAP 1 regression guard: the reader's predicate has no cwd in it.
// conversation.Reader.Read locates the transcript by scanning EVERY project
// directory in each root for <sessionID>.jsonl and stops at the first root
// that holds it. The resolver once located the session through the
// cwd-encoded project directory instead, so a first root holding the session
// under an UNRELATED directory name answered for the reader but not for the
// resolver: title from the home default, transcript from the profile - the
// correct-looking-header-over-someone-else's-conversation split this seam
// exists to prevent. The title must come from the root the reader serves,
// whatever its project directory is called.
func TestTitleComesFromTheRootTheReaderServesEvenUnderAnotherProjectDirectory(t *testing.T) {
	clearAgentRootEnv(t)
	home := t.TempDir()
	profile := t.TempDir()
	t.Setenv(agentroots.ClaudeListEnv, profile)

	// First root: the session sits under a project directory that has nothing
	// to do with the pane's cwd (a checkout that moved, a copied tree).
	// Second root: the same id under the cwd-encoded directory name.
	writeTitleFile(t, filepath.Join(profile, "projects", "-some-other-checkout", "wanted.jsonl"), "Profile title")
	writeTitleFile(t, filepath.Join(home, ".claude", "projects", "-work-app", "wanted.jsonl"), "Home title")

	if got := NewResolver(home).SessionName("claude", "/work/app", "wanted"); got != "Profile title" {
		t.Fatalf("session name = %q, want %q - the first root holds the session, so it must answer no matter which project directory the transcript sits under", got, "Profile title")
	}
}

// The positive half of the empty-session-id heuristic; its ambiguity half is
// pinned above. A pane that reported no session id is still identified when
// the cwd's project directory holds exactly one transcript. Deleting the
// heuristic leaves every id-less pane titleless, which this catches.
func TestEmptySessionIDResolvesTheSoleTranscriptInTheOwningRoot(t *testing.T) {
	clearAgentRootEnv(t)
	home := t.TempDir()
	writeTitleFile(t, filepath.Join(home, ".claude", "projects", "home-user-app", "only.jsonl"), "Sole transcript title")

	if got := NewResolver(home).SessionName("claude", "/home/user/app", ""); got != "Sole transcript title" {
		t.Fatalf("session name = %q, want %q from the sole transcript in the cwd's project directory", got, "Sole transcript title")
	}
}

// The other half of the reader's acceptance: conversation.Reader runs every
// candidate through containedRegularFile against its project directory, so a
// session FILE symlinked out of its own project directory is skipped and the
// scan moves on. The resolver has to skip it too - answering with the escaped
// copy's title would pair it with the transcript the reader takes from the
// next root that genuinely holds the file.
func TestSessionFileSymlinkedOutOfItsProjectDirectoryIsSkipped(t *testing.T) {
	clearAgentRootEnv(t)
	home := t.TempDir()
	profile := t.TempDir()
	outside := t.TempDir()
	t.Setenv(agentroots.ClaudeListEnv, profile)

	escaped := filepath.Join(outside, "real.jsonl")
	writeTitleFile(t, escaped, "Escaped title")
	projectDir := filepath.Join(profile, "projects", "-home-user-app")
	if err := os.MkdirAll(projectDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(escaped, filepath.Join(projectDir, "wanted.jsonl")); err != nil {
		t.Fatal(err)
	}
	writeTitleFile(t, filepath.Join(home, ".claude", "projects", "-home-user-app", "wanted.jsonl"), "Genuine title")

	if got := NewResolver(home).SessionName("claude", "/home/user/app", "wanted"); got != "Genuine title" {
		t.Fatalf("session name = %q, want %q - the reader skips the escaped symlink and serves the next root's transcript", got, "Genuine title")
	}
}

// The resolver must refuse exactly the ids the reader refuses. "." passes a
// naive character check - the filename probed would be "..jsonl" - but
// conversation.SafeSessionID rejects the id outright, and the lookup now
// routes through the reader's Locate, so a title here would otherwise sit
// over a conversation view that says "not available".
func TestDotSessionIDIsRejectedLikeTheReaderRejectsIt(t *testing.T) {
	clearAgentRootEnv(t)
	home := t.TempDir()
	writeTitleFile(t, filepath.Join(home, ".claude", "projects", "home-user-app", "..jsonl"), "Dot title")

	if got := NewResolver(home).SessionName("claude", "/home/user/app", "."); got != "" {
		t.Fatalf("session name = %q, want empty for id %q, which the reader rejects", got, ".")
	}
}

// writeCodexIndex appends one session_index.jsonl record to a Codex home.
// threadName may be empty: Codex writes the index record before it names the
// thread, so an empty thread_name is the ordinary fresh-session state.
func writeCodexIndex(t *testing.T, codexHome, id, threadName string) {
	t.Helper()
	if err := os.MkdirAll(codexHome, 0o755); err != nil {
		t.Fatal(err)
	}
	record, err := json.Marshal(map[string]any{"id": id, "thread_name": threadName})
	if err != nil {
		t.Fatal(err)
	}
	f, err := os.OpenFile(filepath.Join(codexHome, "session_index.jsonl"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if _, err := f.Write(append(record, '\n')); err != nil {
		t.Fatal(err)
	}
}

// writeCodexRollout writes the rollout transcript conversation.Reader serves
// for a Codex session, under the dated tree its lookup scans. day is
// slash-separated ("2026/08/22").
func writeCodexRollout(t *testing.T, codexHome, id, day string) string {
	t.Helper()
	dir := filepath.Join(codexHome, "sessions", filepath.FromSlash(day))
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "rollout-"+strings.ReplaceAll(day, "/", "-")+"T10-00-00-"+id+".jsonl")
	if err := os.WriteFile(path, []byte("{\"type\":\"message\"}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

// The round-3 blocker. The work home holds this session's rollout - the
// transcript the reader serves - with a fresh, still-unnamed index record;
// the home default holds an index record with a confident name and its own,
// earlier rollout for the same id. The title must be the work home's (empty)
// thread_name, never "Deploy hotfix": pairing the home default's title with
// the work home's transcript is a correct-looking header over someone else's
// conversation.
func TestCodexTitleComesFromTheHomeWhoseRolloutTheReaderServes(t *testing.T) {
	clearAgentRootEnv(t)
	home := t.TempDir()
	work := t.TempDir()
	t.Setenv(agentroots.CodexListEnv, work)

	const id = "0199a213-81a0-7800-8000-1a2b3c4d5e6f"
	writeCodexIndex(t, work, id, "")
	writeCodexRollout(t, work, id, "2026/08/22")
	homeCodex := filepath.Join(home, ".codex")
	writeCodexIndex(t, homeCodex, id, "Deploy hotfix")
	writeCodexRollout(t, homeCodex, id, "2026/08/21")

	if got := NewResolver(home).SessionName("codex", "/tmp", id); got != "" {
		t.Fatalf("session name = %q, want empty - the work home's rollout answers and its thread is not named yet", got)
	}
}

// A pruned or unsynced rollout: the index still remembers the session, but
// the reader has no transcript to serve, so the pane must not be titled.
func TestCodexIndexEntryWithoutRolloutIsNotTitled(t *testing.T) {
	clearAgentRootEnv(t)
	home := t.TempDir()
	const id = "5b6c7d8e-9f0a-4b1c-8d2e-3f4a5b6c7d8e"
	writeCodexIndex(t, filepath.Join(home, ".codex"), id, "Ghost session")

	if got := NewResolver(home).SessionName("codex", "/tmp", id); got != "" {
		t.Fatalf("session name = %q, want empty - no rollout exists anywhere, so the reader serves no transcript", got)
	}
}

// The reader accepts either an absolute .jsonl path or a bare canonical UUID
// for Pi and Oh My Pi (scanning <projectdir>/*_<id>.jsonl); the resolver used
// to accept only the path form, so a bare-UUID pane got a transcript with no
// title. Both halves must resolve, to the same file.
func TestPiAndOMPBareSessionUUIDResolvesTitleAndTranscript(t *testing.T) {
	clearAgentRootEnv(t)
	home := t.TempDir()

	for _, c := range []struct{ agent, id, base, record, want string }{
		{"omp", "9e8d7c6b-5a49-4837-a6b5-c4d3e2f1a0b9",
			filepath.Join(home, ".omp", "agent", "sessions"),
			"{\"type\":\"title\",\"title\":\"omp_bare_uuid\"}\n", "omp_bare_uuid"},
		{"pi", "8d7c6b5a-4938-4726-b5a4-d3c2e1f0a9b8",
			filepath.Join(home, ".pi", "agent", "sessions"),
			"{\"type\":\"session_info\",\"name\":\"pi_bare_uuid\"}\n", "pi_bare_uuid"},
	} {
		sessionPath := filepath.Join(c.base, "-home-user-app", "2026-08-22T10-00-00-000Z_"+c.id+".jsonl")
		if err := os.MkdirAll(filepath.Dir(sessionPath), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(sessionPath, []byte(c.record), 0o644); err != nil {
			t.Fatal(err)
		}

		if got := NewResolver(home).SessionName(c.agent, "/home/user/app", c.id); got != c.want {
			t.Errorf("%s title for bare id %s = %q, want %q", c.agent, c.id, got, c.want)
		}
		wantPath, err := filepath.EvalSymlinks(sessionPath)
		if err != nil {
			t.Fatal(err)
		}
		if gotPath, _, ok := conversation.NewReader(home).Locate(c.agent, c.id); !ok || gotPath != wantPath {
			t.Errorf("%s transcript for bare id %s = %q (ok=%v), want %q", c.agent, c.id, gotPath, ok, wantPath)
		}
	}
}

// A ".." in a reported path after a symlinked directory: filepath.Clean
// resolves it lexically against the link's NAME while EvalSymlinks resolves
// it against what the link points at, so the two name different files. The
// reader evaluates the path as given (containedRegularFile), and the title
// must be read from the file the reader serves - a decoy sits at the
// lexically-cleaned spelling to catch a resolver that Cleans first.
func TestReportedPathWithDotDotAfterSymlinkResolvesLikeTheReader(t *testing.T) {
	clearAgentRootEnv(t)
	home := t.TempDir()
	base := filepath.Join(home, ".omp", "agent", "sessions")
	if err := os.MkdirAll(filepath.Join(base, "deep", "dir"), 0o755); err != nil {
		t.Fatal(err)
	}
	served := filepath.Join(base, "deep", "x.jsonl")
	if err := os.WriteFile(served, []byte("{\"type\":\"title\",\"title\":\"Right title\"}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// The decoy sits where Clean would land: Clean("link/../x.jsonl")
	// collapses to "x.jsonl" beside the link, ignoring what "link" points at.
	if err := os.WriteFile(filepath.Join(base, "x.jsonl"), []byte("{\"type\":\"title\",\"title\":\"Wrong title\"}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(base, "deep", "dir"), filepath.Join(base, "link")); err != nil {
		t.Fatal(err)
	}
	// Built by concatenation: filepath.Join would Clean the ".." away.
	sep := string(filepath.Separator)
	reported := filepath.Join(base, "link") + sep + ".." + sep + "x.jsonl"

	wantPath, err := filepath.EvalSymlinks(served)
	if err != nil {
		t.Fatal(err)
	}
	if gotPath, _, ok := conversation.NewReader(home).Locate("omp", reported); !ok || gotPath != wantPath {
		t.Fatalf("reader locates %q (ok=%v), want %q", gotPath, ok, wantPath)
	}
	if got := NewResolver(home).SessionName("omp", "/home/user/app", reported); got != "Right title" {
		t.Fatalf("session name = %q, want %q - the title must come from the file the reader serves, not the lexically-cleaned spelling", got, "Right title")
	}
}
