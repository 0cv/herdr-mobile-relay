package slashcmd

import (
	"os"
	"path/filepath"
	"testing"
)

func TestClaudeBuiltins(t *testing.T) {
	catalog := CatalogFor("claude-code", "/tmp", "/nonexistent-home")
	if len(catalog.Commands) == 0 {
		t.Fatal("no commands returned")
	}
	if catalog.Truncated {
		t.Error("should not be truncated for builtins only")
	}

	found := false
	for _, cmd := range catalog.Commands {
		if cmd.Command == "/clear" {
			found = true
			if cmd.Source != "builtin" {
				t.Errorf("/clear source = %q", cmd.Source)
			}
		}
	}
	if !found {
		t.Error("/clear not found in claude builtins")
	}
}

func TestCodexBuiltins(t *testing.T) {
	catalog := CatalogFor("codex", "/tmp", "/nonexistent-home")
	if len(catalog.Commands) == 0 {
		t.Fatal("no commands returned")
	}

	found := false
	for _, cmd := range catalog.Commands {
		if cmd.Command == "/clear" {
			found = true
		}
	}
	if !found {
		t.Error("/clear not found in codex builtins")
	}
}

func TestDiscoverPersonalCommands(t *testing.T) {
	home := t.TempDir()
	cmdDir := filepath.Join(home, ".claude", "commands")
	os.MkdirAll(cmdDir, 0o755)
	os.WriteFile(filepath.Join(cmdDir, "deploy.md"), []byte("Deploy the app to production\n\nMore details here."), 0o644)
	os.WriteFile(filepath.Join(cmdDir, "not-a-command.txt"), []byte("ignored"), 0o644)

	catalog := CatalogFor("claude", "/tmp", home)

	found := false
	for _, cmd := range catalog.Commands {
		if cmd.Command == "/deploy" {
			found = true
			if cmd.Source != "personal" {
				t.Errorf("source = %q, want personal", cmd.Source)
			}
			if cmd.Description != "Deploy the app to production" {
				t.Errorf("description = %q", cmd.Description)
			}
		}
	}
	if !found {
		t.Error("/deploy not discovered from personal commands")
	}
}

func TestDiscoverProjectCommands(t *testing.T) {
	home := t.TempDir()
	cwd := t.TempDir()
	cmdDir := filepath.Join(cwd, ".claude", "commands")
	os.MkdirAll(cmdDir, 0o755)
	os.WriteFile(filepath.Join(cmdDir, "test.md"), []byte("Run project tests"), 0o644)

	catalog := CatalogFor("claude", cwd, home)

	found := false
	for _, cmd := range catalog.Commands {
		if cmd.Command == "/test" {
			found = true
			if cmd.Source != "project" {
				t.Errorf("source = %q, want project", cmd.Source)
			}
		}
	}
	if !found {
		t.Error("/test not discovered from project commands")
	}
}

func TestDiscoverSkills(t *testing.T) {
	home := t.TempDir()
	skillDir := filepath.Join(home, ".claude", "skills", "my-skill")
	os.MkdirAll(skillDir, 0o755)
	os.WriteFile(filepath.Join(skillDir, "SKILL.md"), []byte("---\nname: my-skill\n---\nA custom skill"), 0o644)

	catalog := CatalogFor("claude", "/tmp", home)

	found := false
	for _, cmd := range catalog.Commands {
		if cmd.Command == "/my-skill" {
			found = true
			if cmd.Source != "personal" {
				t.Errorf("source = %q, want personal", cmd.Source)
			}
		}
	}
	if !found {
		t.Error("/my-skill not discovered from skills directory")
	}
}

func TestClaudeBuiltinCatalogIsComplete(t *testing.T) {
	catalog := CatalogFor("claude", "/tmp", "/nonexistent")
	if len(catalog.Commands) != 51 {
		t.Fatalf("Claude builtins = %d, want 51", len(catalog.Commands))
	}
}

func TestGenericConfiguredSkills(t *testing.T) {
	root := t.TempDir()
	first := filepath.Join(root, "personal")
	second := filepath.Join(root, "project")
	for _, path := range []string{
		filepath.Join(first, "deploy"),
		filepath.Join(second, "deploy"),
		filepath.Join(second, "hidden"),
	} {
		if err := os.MkdirAll(path, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(
		filepath.Join(first, "deploy", "SKILL.md"),
		[]byte("---\nname: deploy\ndescription: Deploy safely\nargument-hint: target\n---\n"),
		0o600,
	); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(
		filepath.Join(second, "deploy", "SKILL.md"),
		[]byte("---\nname: deploy\ndescription: Must not override\n---\n"),
		0o600,
	); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(
		filepath.Join(second, "hidden", "SKILL.md"),
		[]byte("---\nname: hidden\nuser-invocable: false\n---\n"),
		0o600,
	); err != nil {
		t.Fatal(err)
	}

	catalog := CatalogForProfile("pi", "pi-coding-agent", root, root, []string{first, second}, "skill:{name}", "", "")
	if len(catalog.Commands) != len(piBuiltins)+1 {
		t.Fatalf("command count = %d, want %d: %+v", len(catalog.Commands), len(piBuiltins)+1, catalog.Commands)
	}
	for _, command := range catalog.Commands {
		if command.Command != "/skill:deploy" {
			continue
		}
		if command.Description != "Deploy safely" || command.ArgumentHint != "target" ||
			command.Source != "personal" {
			t.Fatalf("command = %+v", command)
		}
		return
	}
	t.Fatal("/skill:deploy not found")
}

func TestUnknownProfileHasNoClaudeFallback(t *testing.T) {
	catalog := CatalogForProfile("custom", "custom", t.TempDir(), t.TempDir(), nil, "", "", "")
	if len(catalog.Commands) != 0 {
		t.Fatalf("unexpected commands: %+v", catalog.Commands)
	}
}
