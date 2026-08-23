package slashcmd

import "testing"

func TestOMPBuiltinCatalog(t *testing.T) {
	isolateAgentEnv(t)
	catalog := CatalogForProfile("omp", "omp", t.TempDir(), "/nonexistent", nil, "", "17.1.7")
	if catalog.Truncated {
		t.Fatal("builtins-only catalog is truncated")
	}
	if len(catalog.Commands) != 68 {
		t.Fatalf("OMP builtins = %d, want 68", len(catalog.Commands))
	}
	for _, name := range []string{
		"/settings", "/plan", "/model", "/advisor", "/todo", "/compact", "/plugins", "/quit",
	} {
		if !hasCommand(catalog, name) {
			t.Errorf("OMP catalog missing %s", name)
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
