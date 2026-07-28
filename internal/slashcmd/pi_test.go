package slashcmd

import "testing"

func TestPiBuiltinCatalog(t *testing.T) {
	catalog := CatalogForProfile("pi", "pi", "/tmp", "/nonexistent", nil, "", "0.82.1")
	if catalog.Truncated {
		t.Fatal("builtins-only catalog is truncated")
	}
	if len(catalog.Commands) != 22 {
		t.Fatalf("Pi builtins = %d, want 22", len(catalog.Commands))
	}
	for _, name := range []string{"/settings", "/model", "/resume", "/compact", "/quit"} {
		if !hasCommand(catalog, name) {
			t.Errorf("Pi catalog missing %s", name)
		}
	}
	for _, command := range catalog.Commands {
		if command.Source != "builtin" {
			t.Errorf("%s source = %q, want builtin", command.Command, command.Source)
		}
		if command.Description == "" {
			t.Errorf("%s has no description", command.Command)
		}
	}
}
