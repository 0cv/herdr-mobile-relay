package slashcmd

import (
	"os"
	"path/filepath"
	"testing"
)

func TestQoderBuiltins(t *testing.T) {
	catalog := CatalogFor("qoder", "/tmp", "/nonexistent")
	if len(catalog.Commands) == 0 {
		t.Fatal("no commands returned")
	}
	for _, expected := range []string{"/clear", "/help", "/status", "/compact", "/model"} {
		if !hasCommand(catalog, expected) {
			t.Errorf("missing qoder builtin %q", expected)
		}
	}
}

func TestQoderPersonalCommands(t *testing.T) {
	home := t.TempDir()
	cmdDir := filepath.Join(home, ".qoder", "commands")
	os.MkdirAll(cmdDir, 0o755)
	os.WriteFile(filepath.Join(cmdDir, "deploy.md"), []byte("Deploy app"), 0o644)

	catalog := CatalogFor("qoder", "/tmp", home)
	if !hasCommand(catalog, "/deploy") {
		t.Error("missing /deploy from personal commands")
	}
}

func TestQoderProjectCommands(t *testing.T) {
	root := t.TempDir()
	os.MkdirAll(filepath.Join(root, ".git"), 0o755)
	cmdDir := filepath.Join(root, ".qoder", "commands")
	os.MkdirAll(cmdDir, 0o755)
	os.WriteFile(filepath.Join(cmdDir, "test.md"), []byte("Run tests"), 0o644)

	sub := filepath.Join(root, "src")
	os.MkdirAll(sub, 0o755)

	home := t.TempDir()
	catalog := CatalogFor("qoder", sub, home)
	if !hasCommand(catalog, "/test") {
		t.Error("missing /test from project commands via git root")
	}
}

func TestQoderRecursiveNamespace(t *testing.T) {
	home := t.TempDir()
	cmdDir := filepath.Join(home, ".qoder", "commands", "git")
	os.MkdirAll(cmdDir, 0o755)
	os.WriteFile(filepath.Join(cmdDir, "commit.md"), []byte("Commit"), 0o644)

	catalog := CatalogFor("qoder", "/tmp", home)
	if !hasCommand(catalog, "/git:commit") {
		t.Error("missing /git:commit")
	}
}

func TestQoderSkills(t *testing.T) {
	home := t.TempDir()
	skillDir := filepath.Join(home, ".qoder", "skills", "my-skill")
	os.MkdirAll(skillDir, 0o755)
	os.WriteFile(filepath.Join(skillDir, "SKILL.md"), []byte("---\nname: my-skill\ndescription: Does things\n---\n"), 0o644)

	catalog := CatalogFor("qoder", "/tmp", home)
	if !hasCommand(catalog, "/my-skill") {
		t.Error("missing /my-skill from personal skills")
	}
}

func TestQoderSameNameCommandsCoexist(t *testing.T) {
	home := t.TempDir()
	personalDir := filepath.Join(home, ".qoder", "commands")
	os.MkdirAll(personalDir, 0o755)
	os.WriteFile(filepath.Join(personalDir, "deploy.md"), []byte("Personal"), 0o644)

	cwd := t.TempDir()
	projectDir := filepath.Join(cwd, ".qoder", "commands")
	os.MkdirAll(projectDir, 0o755)
	os.WriteFile(filepath.Join(projectDir, "deploy.md"), []byte("Project"), 0o644)

	catalog := CatalogFor("qoder", cwd, home)
	var sources []string
	for _, cmd := range catalog.Commands {
		if cmd.Command == "/deploy" {
			sources = append(sources, cmd.Source)
		}
	}
	if len(sources) != 2 {
		t.Fatalf("expected 2 /deploy entries (personal + project), got %d: %v", len(sources), sources)
	}
	if sources[0] != "personal" || sources[1] != "project" {
		t.Errorf("sources = %v, want [personal, project]", sources)
	}
}

func TestQodercliProfileID(t *testing.T) {
	catalog := CatalogForProfile("qodercli", "qodercli", "/tmp", "/nonexistent", nil, "", "")
	if !hasCommand(catalog, "/clear") {
		t.Error("qodercli profile should resolve to qoder provider")
	}
}

func TestQoderDoesNotUseClaudeDirectories(t *testing.T) {
	home := t.TempDir()
	claudeDir := filepath.Join(home, ".claude", "commands")
	os.MkdirAll(claudeDir, 0o755)
	os.WriteFile(filepath.Join(claudeDir, "claude-only.md"), []byte("Claude"), 0o644)

	catalog := CatalogFor("qoder", "/tmp", home)
	if hasCommand(catalog, "/claude-only") {
		t.Error("qoder should not discover from .claude/ directories")
	}
}

func TestQoderNonInvocableCommandSuppressesBuiltin(t *testing.T) {
	home := t.TempDir()
	cmdDir := filepath.Join(home, ".qoder", "commands")
	os.MkdirAll(cmdDir, 0o755)
	os.WriteFile(
		filepath.Join(cmdDir, "clear.md"),
		[]byte("---\nuser-invocable: false\n---\n"),
		0o644,
	)

	catalog := CatalogFor("qoder", "/tmp", home)
	if hasCommand(catalog, "/clear") {
		t.Error("non-invocable personal command did not suppress builtin /clear")
	}
}

func TestQoderProjectSuppressionHidesLowerScopeCommand(t *testing.T) {
	home := t.TempDir()
	personalDir := filepath.Join(home, ".qoder", "commands")
	os.MkdirAll(personalDir, 0o755)
	os.WriteFile(filepath.Join(personalDir, "deploy.md"), []byte("Personal"), 0o644)

	cwd := t.TempDir()
	projectDir := filepath.Join(cwd, ".qoder", "commands")
	os.MkdirAll(projectDir, 0o755)
	os.WriteFile(
		filepath.Join(projectDir, "deploy.md"),
		[]byte("---\nuser-invocable: false\n---\n"),
		0o644,
	)

	catalog := CatalogFor("qoder", cwd, home)
	if hasCommand(catalog, "/deploy") {
		t.Error("project suppression did not hide lower-scope /deploy")
	}
}

func TestQoderNearProjectSuppressionHidesOuterProjectCommand(t *testing.T) {
	root := t.TempDir()
	os.MkdirAll(filepath.Join(root, ".git"), 0o755)
	outerCommands := filepath.Join(root, ".qoder", "commands")
	os.MkdirAll(outerCommands, 0o755)
	os.WriteFile(filepath.Join(outerCommands, "deploy.md"), []byte("Outer project"), 0o644)

	cwd := filepath.Join(root, "packages", "mobile")
	nearCommands := filepath.Join(cwd, ".qoder", "commands")
	os.MkdirAll(nearCommands, 0o755)
	os.WriteFile(
		filepath.Join(nearCommands, "deploy.md"),
		[]byte("---\nuser-invocable: false\n---\n"),
		0o644,
	)

	catalog := CatalogFor("qoder", cwd, t.TempDir())
	if hasCommand(catalog, "/deploy") {
		t.Error("near project suppression did not hide outer project /deploy")
	}
}
