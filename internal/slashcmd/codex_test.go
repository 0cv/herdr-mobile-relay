package slashcmd

import "testing"

func TestCodexBaseBuiltins(t *testing.T) {
	catalog := CatalogFor("codex", "/tmp", "/nonexistent")
	if len(catalog.Commands) == 0 {
		t.Fatal("no commands returned")
	}
	if len(catalog.Commands) != 50 || !hasCommand(catalog, "/clear") ||
		!hasCommand(catalog, "/apps") || !hasCommand(catalog, "/model") ||
		!hasCommand(catalog, "/status") {
		t.Errorf("missing expected codex builtins: %+v", catalog.Commands)
	}
}

func TestCodexNoFilesystemDiscovery(t *testing.T) {
	home := t.TempDir()
	cmdDir := home + "/.codex/commands"
	mkdirAll(t, cmdDir)
	writeTestFile(t, cmdDir+"/extra.md", "Should not appear")

	catalog := CatalogFor("codex", "/tmp", home)
	if hasCommand(catalog, "/extra") {
		t.Error("codex should not do filesystem discovery")
	}
}

func TestCodexModelHint(t *testing.T) {
	catalog := CatalogFor("codex", "/tmp", "/nonexistent")
	for _, cmd := range catalog.Commands {
		if cmd.Command == "/goal" {
			if cmd.ArgumentHint != "[objective|edit|pause|resume|clear]" {
				t.Errorf("/goal argument_hint = %q", cmd.ArgumentHint)
			}
			return
		}
	}
	t.Error("/goal not found")
}
