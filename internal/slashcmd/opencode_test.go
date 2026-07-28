package slashcmd

import "testing"

func TestOpenCodeBuiltinCatalog(t *testing.T) {
	catalog := CatalogForProfile("opencode", "opencode", "/tmp", "/nonexistent", nil, "", "1.18.5")
	if catalog.Truncated {
		t.Fatal("OpenCode builtins should not be truncated")
	}
	if len(catalog.Commands) != 17 {
		t.Fatalf("OpenCode builtins = %d, want 17", len(catalog.Commands))
	}
	for _, command := range []string{"/agents", "/diff", "/init", "/models", "/review", "/skills"} {
		if !hasCommand(catalog, command) {
			t.Errorf("OpenCode catalog missing %q", command)
		}
	}
}

func TestOpenCodeOmitsConditionalCommands(t *testing.T) {
	catalog := CatalogFor("open-code", "/tmp", "/nonexistent")
	for _, command := range []string{"/org", "/variants", "/warp", "/workspaces"} {
		if hasCommand(catalog, command) {
			t.Errorf("OpenCode catalog unexpectedly includes conditional command %q", command)
		}
	}
}
