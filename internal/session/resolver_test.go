package session

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

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

func TestCodexSessionName(t *testing.T) {
	home := t.TempDir()
	codexDir := filepath.Join(home, ".codex")
	sessionID := "0194f6c8-1111-7222-8333-123456789abc"
	writeCodexRollout(t, codexDir, sessionID)

	entry := map[string]any{"id": sessionID, "thread_name": "Build API endpoint"}
	data, _ := json.Marshal(entry)
	os.WriteFile(filepath.Join(codexDir, "session_index.jsonl"), append(data, '\n'), 0o644)

	r := NewResolver(home)
	name := r.SessionName("codex", "/tmp", sessionID)
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
	expireTitleCache(resolver, "omp|/home/user/app|"+sessionPath)
	if got := resolver.SessionName("omp", "/home/user/app", sessionPath); got != "session_fix_renamed" {
		t.Fatalf("updated session name = %q, want %q after cache expiry", got, "session_fix_renamed")
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

	// The title cache is intentionally consulted before any filesystem work.
	entry2 := map[string]any{"type": "summary", "title": "Second Title"}
	data2, _ := json.Marshal(entry2)
	os.WriteFile(filepath.Join(projDir, "s1.jsonl"), append(data2, '\n'), 0o644)
	if got := r.SessionName("qoder", "/home/user/proj", "s1"); got != "First Title" {
		t.Fatalf("cached call = %q, want stable title before TTL expiry", got)
	}
	expireTitleCache(r, "qoder|/home/user/proj|s1")
	if got := r.SessionName("qoder", "/home/user/proj", "s1"); got != "Second Title" {
		t.Errorf("expired call = %q, want 'Second Title'", got)
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
	sessionID := "0194f6c8-2222-7333-8444-abcdef123456"
	writeCodexRollout(t, second, sessionID)

	data, err := json.Marshal(map[string]any{"id": sessionID, "thread_name": "Second home thread"})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(second, "session_index.jsonl"), append(data, '\n'), 0o644); err != nil {
		t.Fatal(err)
	}

	if got := NewResolver(home).SessionName("codex", "/tmp", sessionID); got != "Second home thread" {
		t.Fatalf("session name = %q, want %q from the home containing the rollout", got, "Second home thread")
	}
}

func TestTitleCacheTTLWithNonDefaultClaudeRoot(t *testing.T) {
	clearAgentRootEnv(t)
	home := t.TempDir()
	first := t.TempDir()
	second := t.TempDir()
	t.Setenv(agentroots.ClaudeListEnv, first+string(os.PathListSeparator)+second)

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
	if got := r.SessionName("claude", "/home/user/proj", "s1"); got != "First Title" {
		t.Fatalf("cached call = %q, want stable title before TTL expiry", got)
	}
	expireTitleCache(r, "claude|/home/user/proj|s1")
	if got := r.SessionName("claude", "/home/user/proj", "s1"); got != "Second Title After Rename" {
		t.Fatalf("expired call = %q, want %q", got, "Second Title After Rename")
	}
}

func TestTitleCacheKeepsSelectedRootUntilExpiry(t *testing.T) {
	clearAgentRootEnv(t)
	home := t.TempDir()
	first := t.TempDir()
	second := t.TempDir()
	t.Setenv(agentroots.ClaudeListEnv, first+string(os.PathListSeparator)+second)

	writeTitleFile(t, filepath.Join(first, "projects", "home-user-app", "other.jsonl"), "Unrelated")
	target := filepath.Join(second, "projects", "home-user-app", "wanted.jsonl")
	writeTitleFile(t, target, "First Title")

	r := NewResolver(home)
	if got := r.SessionName("claude", "/home/user/app", "wanted"); got != "First Title" {
		t.Fatalf("first call = %q, want %q from the second root", got, "First Title")
	}

	writeTitleFile(t, target, "Renamed Title")
	if got := r.SessionName("claude", "/home/user/app", "wanted"); got != "First Title" {
		t.Fatalf("cached call = %q, want stable title before TTL expiry", got)
	}
	expireTitleCache(r, "claude|/home/user/app|wanted")
	if got := r.SessionName("claude", "/home/user/app", "wanted"); got != "Renamed Title" {
		t.Fatalf("expired call = %q, want %q", got, "Renamed Title")
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

func expireTitleCache(r *Resolver, key string) {
	r.mu.Lock()
	entry := r.cache[key]
	entry.expires = time.Time{}
	r.cache[key] = entry
	r.mu.Unlock()
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
		{"piRoots", r.piRoots(), []string{
			filepath.Join(profile, "sessions"),
			filepath.Join(home, ".pi", "agent", "sessions"),
		}},
		{"ompRoots", r.ompRoots(), []string{
			filepath.Join(profile, "sessions"),
			filepath.Join(home, ".omp", "agent", "sessions"),
		}},
	} {
		if !slices.Equal(c.got, c.want) {
			t.Errorf("%s = %v, want %v", c.name, c.got, c.want)
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
// is false for a symlink to a directory. Classifying with it drops a project
// directory symlinked in from a dotfiles repo, which is a mainstream way to
// keep agent configuration. Both of findProjectDir's loops have to stat.
func TestSymlinkedProjectDirectoryIsFoundByItsEncodedName(t *testing.T) {
	clearAgentRootEnv(t)
	home := t.TempDir()
	projects := filepath.Join(home, ".claude", "projects")
	target := filepath.Join(projects, ".store", "dotfiles-app")
	writeTitleFile(t, filepath.Join(target, "sess-link.jsonl"), "Symlinked project title")
	if err := os.Symlink(target, filepath.Join(projects, "home-user-app")); err != nil {
		t.Fatal(err)
	}

	if got := NewResolver(home).SessionName("claude", "/home/user/app", "sess-link"); got != "Symlinked project title" {
		t.Fatalf("session name = %q, want %q from a contained symlinked project directory", got, "Symlinked project title")
	}
}

// The same hazard in the second loop, the one that identifies a project
// directory by the cwd file inside it rather than by its encoded name.
func TestSymlinkedProjectDirectoryIsFoundByItsCwdFile(t *testing.T) {
	clearAgentRootEnv(t)
	home := t.TempDir()
	projects := filepath.Join(home, ".claude", "projects")
	target := filepath.Join(projects, ".store", "dotfiles-app")
	writeTitleFile(t, filepath.Join(target, "sess-link.jsonl"), "Cwd-matched symlink title")
	if err := os.WriteFile(filepath.Join(target, "cwd"), []byte("/home/user/app\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, filepath.Join(projects, "not-the-encoded-name")); err != nil {
		t.Fatal(err)
	}

	if got := NewResolver(home).SessionName("claude", "/home/user/app", "sess-link"); got != "Cwd-matched symlink title" {
		t.Fatalf("session name = %q, want %q from a contained symlink matched by cwd", got, "Cwd-matched symlink title")
	}
}

func TestCodexIndexWithoutRolloutHasNoTitle(t *testing.T) {
	home := t.TempDir()
	codexDir := filepath.Join(home, ".codex")
	if err := os.MkdirAll(codexDir, 0o755); err != nil {
		t.Fatal(err)
	}
	sessionID := "0194f6c8-3333-7444-8555-fedcba987654"
	data, _ := json.Marshal(map[string]any{"id": sessionID, "thread_name": "Orphan title"})
	if err := os.WriteFile(filepath.Join(codexDir, "session_index.jsonl"), append(data, '\n'), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := NewResolver(home).SessionName("codex", "/tmp", sessionID); got != "" {
		t.Fatalf("session name = %q, want empty without a readable rollout", got)
	}
}

func TestProjectSessionSymlinkEscapingRootHasNoTitle(t *testing.T) {
	home := t.TempDir()
	projects := filepath.Join(home, ".claude", "projects")
	target := filepath.Join(t.TempDir(), "external")
	writeTitleFile(t, filepath.Join(target, "session.jsonl"), "External title")
	if err := os.MkdirAll(projects, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, filepath.Join(projects, "home-user-app")); err != nil {
		t.Fatal(err)
	}
	if got := NewResolver(home).SessionName("claude", "/home/user/app", "session"); got != "" {
		t.Fatalf("session name = %q, want empty for project symlink escaping the transcript root", got)
	}
}

func TestClaudeTitleSelectsCurrentProjectOnDuplicateID(t *testing.T) {
	home := t.TempDir()
	projects := filepath.Join(home, ".claude", "projects")
	writeTitleFile(t, filepath.Join(projects, "-old", "duplicate.jsonl"), "Old project title")
	writeTitleFile(t, filepath.Join(projects, "-work", "duplicate.jsonl"), "Current project title")

	if got := NewResolver(home).SessionName("claude", "/work", "duplicate"); got != "Current project title" {
		t.Fatalf("session name = %q, want title from the current project's transcript", got)
	}
}

func TestTitleCacheEntryMustMatchLocatedTranscript(t *testing.T) {
	home := t.TempDir()
	writeTitleFile(t, filepath.Join(home, ".claude", "projects", "-work", "session.jsonl"), "Current title")
	resolver := NewResolver(home)
	key := "claude|/work|session"
	resolver.cache[key] = cacheEntry{
		name:     "Stale title from another transcript",
		location: conversation.Location{Path: filepath.Join(home, "other.jsonl"), Root: filepath.Join(home, "other")},
		expires:  time.Now().Add(time.Hour),
	}

	if got := resolver.SessionName("claude", "/work", "session"); got != "Current title" {
		t.Fatalf("session name = %q, want title bound to the currently located transcript", got)
	}
}

func writeCodexRollout(t *testing.T, codexDir, sessionID string) {
	t.Helper()
	path := filepath.Join(codexDir, "sessions", "2026", "08", "24", "rollout-2026-08-24T00-00-00-"+sessionID+".jsonl")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("{}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
}
