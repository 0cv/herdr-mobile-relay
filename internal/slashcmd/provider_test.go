package slashcmd

import (
	"os"
	"testing"
)

type versionCaptureProvider struct {
	version string
}

func (p *versionCaptureProvider) ID() string { return "version-capture" }
func (p *versionCaptureProvider) Discover(ctx DiscoverContext) ([]Command, bool) {
	p.version = ctx.AgentVersion
	return nil, false
}

func TestResolveProviderExact(t *testing.T) {
	for _, id := range []string{"claude", "codex", "qoder", "qodercli", "pi", "omp", "kimi", "opencode"} {
		if resolveProvider(id) == nil {
			t.Errorf("resolveProvider(%q) = nil", id)
		}
	}
}

func TestResolveProviderCaseInsensitive(t *testing.T) {
	if resolveProvider("Claude") == nil {
		t.Error("resolveProvider should be case-insensitive")
	}
	if resolveProvider("QODER") == nil {
		t.Error("resolveProvider should be case-insensitive")
	}
}

func TestResolveProviderUnknown(t *testing.T) {
	if resolveProvider("unknown-agent") != nil {
		t.Error("unknown profile should return nil")
	}
	if resolveProvider("") != nil {
		t.Error("empty profile should return nil")
	}
}

// cwd is t.TempDir() rather than a shared path like /tmp: claudeProvider and
// qoderProvider walk up from ctx.Cwd via findProjectDirs looking for a project
// .claude/.qoder directory, and piProvider/ompProvider do the same for
// .agents/.pi/.omp. A stray project file reachable from cwd - e.g. a
// .claude/settings.json with a skillOverrides entry set to "off" - can delete a
// builtin via claudeSkillOverrides, not just add alongside it. /tmp is
// world-writable on macOS, so it is not a safe stand-in for an isolated cwd.
// isolateAgentEnv additionally strips the agent env vars those providers also
// consult, so only the dispatch table below decides the outcome.
func TestCatalogForExactDispatch(t *testing.T) {
	isolateAgentEnv(t)
	cwd := t.TempDir()
	tests := []struct {
		agent       string
		wantBuiltin string
	}{
		{"claude", "/clear"},
		{"claude-code", "/clear"},
		{"claude code", "/clear"},
		{"codex", "/clear"},
		{"qoder", "/clear"},
		{"qodercli", "/clear"},
		{"pi", "/model"},
		{"pi-coding-agent", "/model"},
		{"omp", "/model"},
		{"oh my pi", "/model"},
		{"kimi", "/model"},
		{"kimi-code", "/model"},
		{"opencode", "/models"},
		{"open code", "/models"},
	}
	for _, tt := range tests {
		catalog := CatalogFor(tt.agent, cwd, "/nonexistent")
		if !hasCommand(catalog, tt.wantBuiltin) {
			t.Errorf("CatalogFor(%q) missing %q", tt.agent, tt.wantBuiltin)
		}
	}
}

// The relay passes an empty profileID whenever the agent's own binary is missing
// from the service PATH, which is the norm under launchd's minimal environment. The
// reportedAgent fallback is then the only thing that selects a provider, so it must
// cover every name the exact-dispatch table above covers. Asserting through
// CatalogFor alone cannot catch a gap here: that path supplies the profileID itself.
//
// Each wantBuiltin is a command unique to its provider (not shared with any other
// provider's builtin list), so a case that resolves to the wrong provider fails here
// even though most agents' catalogs still come back non-empty. /clear and /model, the
// commands this test used before, are not provider-unique - /clear is also a claude,
// codex, and qoder builtin, and /model is also a pi, omp, and kimi builtin - so 15 of
// 18 cases kept passing with profileIDForAgentName mapping an agent to the wrong
// provider entirely.
//
// cwd and home are each t.TempDir() rather than a shared path like /tmp: claudeProvider
// reads skillOverrides from <home>/.claude/settings.json and from
// findProjectDirs(cwd, ".claude")/settings*.json, and a stray file there would suppress
// a builtin and fail this test on an otherwise-correct mapping. /tmp is world-writable
// on macOS, so it is not a safe stand-in for an isolated cwd/home.
func TestCatalogForProfileFallbackCoversEveryAgentName(t *testing.T) {
	cwd := t.TempDir()
	home := t.TempDir()
	tests := []struct {
		agent       string
		wantBuiltin string
	}{
		{"claude", "/doctor"},
		{"claude-code", "/doctor"},
		{"claude code", "/doctor"},
		{"codex", "/apps"},
		{"qoder", "/cost"},
		{"qodercli", "/cost"},
		{"pi", "/scoped-models"},
		{"pi-coding-agent", "/scoped-models"},
		{"omp", "/vibe"},
		{"oh my pi", "/vibe"},
		{"oh-my-pi", "/vibe"},
		{"kimi", "/yolo"},
		{"kimi code", "/yolo"},
		{"kimi-code", "/yolo"},
		{"kimi-cli", "/yolo"},
		{"opencode", "/models"},
		{"open code", "/models"},
		{"open-code", "/models"},
	}
	for _, tt := range tests {
		catalog := CatalogForProfile("", tt.agent, cwd, home, nil, "", "")
		if len(catalog.Commands) == 0 {
			t.Errorf("CatalogForProfile(\"\", %q) returned an empty catalog", tt.agent)
			continue
		}
		if !hasCommand(catalog, tt.wantBuiltin) {
			t.Errorf("CatalogForProfile(\"\", %q) missing %q", tt.agent, tt.wantBuiltin)
		}
	}
}

func TestCatalogForNoSubstringMatch(t *testing.T) {
	catalog := CatalogFor("not-claude-at-all", "/tmp", "/nonexistent")
	if len(catalog.Commands) != 0 {
		t.Errorf("substring match should not trigger provider: %+v", catalog.Commands)
	}
}

func TestUnknownProfileFallsToGeneric(t *testing.T) {
	root := t.TempDir()
	skillDir := root + "/skills"
	mkdirAll(t, skillDir+"/deploy")
	writeTestFile(t, skillDir+"/deploy/SKILL.md", "---\nname: deploy\ndescription: Deploy\n---\n")

	catalog := CatalogForProfile("custom", "custom-agent", root, root, []string{skillDir}, "skill:{name}", "")
	if !hasCommand(catalog, "/skill:deploy") {
		t.Error("unknown profile with config should use generic provider")
	}
}

func TestUnknownProfileNoConfig(t *testing.T) {
	catalog := CatalogForProfile("custom", "custom-agent", "/tmp", "/tmp", nil, "", "")
	if len(catalog.Commands) != 0 {
		t.Errorf("unknown profile without config should be empty: %+v", catalog.Commands)
	}
}

func TestCatalogPassesAgentVersionToProvider(t *testing.T) {
	provider := &versionCaptureProvider{}
	providers[provider.ID()] = provider
	t.Cleanup(func() { delete(providers, provider.ID()) })
	CatalogForProfile(provider.ID(), "", "/tmp", "/tmp", nil, "", "1.2.3")
	if provider.version != "1.2.3" {
		t.Fatalf("provider version = %q, want 1.2.3", provider.version)
	}
}

func mkdirAll(t *testing.T, path string) {
	t.Helper()
	if err := os.MkdirAll(path, 0o755); err != nil {
		t.Fatal(err)
	}
}
