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
	for _, id := range []string{"claude", "codex", "qoder", "qodercli"} {
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

func TestCatalogForExactDispatch(t *testing.T) {
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
	}
	for _, tt := range tests {
		catalog := CatalogFor(tt.agent, "/tmp", "/nonexistent")
		if !hasCommand(catalog, tt.wantBuiltin) {
			t.Errorf("CatalogFor(%q) missing %q", tt.agent, tt.wantBuiltin)
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
