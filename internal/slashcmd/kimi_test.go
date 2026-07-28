package slashcmd

import "testing"

func TestKimiBuiltinCatalog(t *testing.T) {
	catalog := CatalogForProfile("kimi", "kimi", "/tmp", "/nonexistent", nil, "", "0.29.2")
	if catalog.Truncated {
		t.Fatal("Kimi builtins should not be truncated")
	}
	if len(catalog.Commands) != 39 {
		t.Fatalf("Kimi builtins = %d, want 39", len(catalog.Commands))
	}
	for _, command := range []string{"/model", "/permission", "/swarm", "/goal", "/export-md"} {
		if !hasCommand(catalog, command) {
			t.Errorf("Kimi catalog missing %q", command)
		}
	}
}

func TestKimiCommandHints(t *testing.T) {
	catalog := CatalogFor("kimi-code", "/tmp", "/nonexistent")
	for _, command := range catalog.Commands {
		if command.Command != "/goal" {
			continue
		}
		if command.ArgumentHint != "[status|pause|resume|cancel|replace|next] | <objective>" {
			t.Fatalf("/goal hint = %q", command.ArgumentHint)
		}
		return
	}
	t.Fatal("/goal not found")
}
