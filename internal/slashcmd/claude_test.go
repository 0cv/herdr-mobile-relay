package slashcmd

import (
	"os"
	"path/filepath"
	"testing"
)

func TestClaudeRecursiveNamespace(t *testing.T) {
	home := t.TempDir()
	cmdDir := filepath.Join(home, ".claude", "commands", "git")
	os.MkdirAll(cmdDir, 0o755)
	os.WriteFile(filepath.Join(cmdDir, "commit.md"), []byte("Commit changes"), 0o644)

	catalog := CatalogFor("claude", "/tmp", home)
	if !hasCommand(catalog, "/git:commit") {
		t.Error("missing /git:commit")
	}
}

func TestClaudeDeepNesting(t *testing.T) {
	home := t.TempDir()
	deep := filepath.Join(home, ".claude", "commands", "a", "b")
	os.MkdirAll(deep, 0o755)
	os.WriteFile(filepath.Join(deep, "c.md"), []byte("Deep"), 0o644)

	catalog := CatalogFor("claude", "/tmp", home)
	if !hasCommand(catalog, "/a:b:c") {
		t.Error("missing /a:b:c")
	}
}

func TestClaudeGitRoot(t *testing.T) {
	root := t.TempDir()
	os.MkdirAll(filepath.Join(root, ".git"), 0o755)
	cmdDir := filepath.Join(root, ".claude", "commands")
	os.MkdirAll(cmdDir, 0o755)
	os.WriteFile(filepath.Join(cmdDir, "build.md"), []byte("Build project"), 0o644)

	sub := filepath.Join(root, "src", "pkg")
	os.MkdirAll(sub, 0o755)

	home := t.TempDir()
	catalog := CatalogFor("claude", sub, home)
	if !hasCommand(catalog, "/build") {
		t.Error("project command not found from subdirectory via git root")
	}
}

func TestClaudeGitRootFallback(t *testing.T) {
	// With a .git directory at root, commands in root/.claude are found from a subdirectory.
	root := t.TempDir()
	os.MkdirAll(filepath.Join(root, ".git"), 0o755)
	cmdDir := filepath.Join(root, ".claude", "commands")
	os.MkdirAll(cmdDir, 0o755)
	os.WriteFile(filepath.Join(cmdDir, "lint.md"), []byte("Run linter"), 0o644)

	sub := filepath.Join(root, "src")
	os.MkdirAll(sub, 0o755)

	home := t.TempDir()
	catalog := CatalogFor("claude", sub, home)
	if !hasCommand(catalog, "/lint") {
		t.Error("project command not found via upward walk from git root")
	}
}

func TestClaudeFrontmatterDescription(t *testing.T) {
	home := t.TempDir()
	cmdDir := filepath.Join(home, ".claude", "commands")
	os.MkdirAll(cmdDir, 0o755)
	os.WriteFile(filepath.Join(cmdDir, "deploy.md"), []byte("---\ndescription: Custom deploy desc\n---\nBody text here"), 0o644)

	catalog := CatalogFor("claude", "/tmp", home)
	for _, cmd := range catalog.Commands {
		if cmd.Command == "/deploy" {
			if cmd.Description != "Custom deploy desc" {
				t.Errorf("description = %q, want frontmatter value", cmd.Description)
			}
			return
		}
	}
	t.Error("/deploy not found")
}

func TestClaudePersonalOverridesProject(t *testing.T) {
	home := t.TempDir()
	personalDir := filepath.Join(home, ".claude", "commands")
	os.MkdirAll(personalDir, 0o755)
	os.WriteFile(filepath.Join(personalDir, "deploy.md"), []byte("Personal deploy"), 0o644)

	cwd := t.TempDir()
	projectDir := filepath.Join(cwd, ".claude", "commands")
	os.MkdirAll(projectDir, 0o755)
	os.WriteFile(filepath.Join(projectDir, "deploy.md"), []byte("Project deploy"), 0o644)

	catalog := CatalogFor("claude", cwd, home)
	count := 0
	for _, cmd := range catalog.Commands {
		if cmd.Command == "/deploy" {
			count++
			if cmd.Source != "personal" {
				t.Errorf("source = %q, want personal (personal has final precedence)", cmd.Source)
			}
		}
	}
	if count != 1 {
		t.Errorf("expected exactly 1 /deploy, got %d", count)
	}
}

func TestClaudeHiddenCommandSkipped(t *testing.T) {
	home := t.TempDir()
	cmdDir := filepath.Join(home, ".claude", "commands")
	os.MkdirAll(cmdDir, 0o755)
	os.WriteFile(filepath.Join(cmdDir, "secret.md"), []byte("---\nhidden: true\n---\nSecret"), 0o644)
	os.WriteFile(filepath.Join(cmdDir, "public.md"), []byte("Public"), 0o644)

	catalog := CatalogFor("claude", "/tmp", home)
	if hasCommand(catalog, "/secret") {
		t.Error("hidden command should not appear")
	}
	if !hasCommand(catalog, "/public") {
		t.Error("public command should appear")
	}
}

func TestClaudeSkillUserInvocableFalse(t *testing.T) {
	home := t.TempDir()
	skillDir := filepath.Join(home, ".claude", "skills", "internal-only")
	os.MkdirAll(skillDir, 0o755)
	os.WriteFile(filepath.Join(skillDir, "SKILL.md"), []byte("---\nname: internal-only\nuser-invocable: false\n---\n"), 0o644)

	catalog := CatalogFor("claude", "/tmp", home)
	if hasCommand(catalog, "/internal-only") {
		t.Error("user-invocable: false skill should not appear")
	}
}

func TestClaudeBuiltinArgumentHints(t *testing.T) {
	catalog := CatalogFor("claude", "/tmp", "/nonexistent")
	for _, cmd := range catalog.Commands {
		if cmd.Command == "/remote-control" {
			if cmd.ArgumentHint != "[name]" {
				t.Errorf("/remote-control argument_hint = %q", cmd.ArgumentHint)
			}
			return
		}
	}
	t.Error("/remote-control not found")
}

func TestClaudeLaterScopeRestoresSuppressedCommand(t *testing.T) {
	home := t.TempDir()
	cwd := t.TempDir()
	if err := os.MkdirAll(filepath.Join(cwd, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	projectSkill := filepath.Join(cwd, ".claude", "skills", "deploy")
	personalSkill := filepath.Join(home, ".claude", "skills", "deploy")
	if err := os.MkdirAll(projectSkill, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(personalSkill, 0o755); err != nil {
		t.Fatal(err)
	}
	writeTestFile(t, filepath.Join(projectSkill, "SKILL.md"), "---\nname: deploy\nuser-invocable: false\n---\n")
	writeTestFile(t, filepath.Join(personalSkill, "SKILL.md"), "---\nname: deploy\ndescription: Personal deploy\n---\n")

	catalog := CatalogFor("claude", cwd, home)
	if !hasCommand(catalog, "/deploy") {
		t.Fatal("later personal skill did not restore a project-suppressed command")
	}
}

func TestClaudeLaterSettingsReenableEarlierOff(t *testing.T) {
	home := t.TempDir()
	cwd := t.TempDir()
	if err := os.MkdirAll(filepath.Join(cwd, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(home, ".claude"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(cwd, ".claude"), 0o755); err != nil {
		t.Fatal(err)
	}
	writeTestFile(t, filepath.Join(home, ".claude", "settings.json"), `{"skillOverrides":{"review":"off"}}`)
	writeTestFile(t, filepath.Join(cwd, ".claude", "settings.json"), `{"skillOverrides":{"review":"on"}}`)

	catalog := CatalogFor("claude", cwd, home)
	if !hasCommand(catalog, "/review") {
		t.Fatal("later project setting did not re-enable /review")
	}
}

func hasCommand(catalog Catalog, name string) bool {
	for _, cmd := range catalog.Commands {
		if cmd.Command == name {
			return true
		}
	}
	return false
}

func writeTestFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}
