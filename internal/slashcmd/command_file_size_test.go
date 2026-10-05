package slashcmd

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func paddedFile(t *testing.T, path, content string, size int) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	writeTestFile(t, path, content+strings.Repeat("x", size-len(content)))
}

func findCatalogCommand(catalog Catalog, name string) (Command, bool) {
	for _, command := range catalog.Commands {
		if command.Command == name {
			return command, true
		}
	}
	return Command{}, false
}

func TestClaudeSkillsWithLargeBodies(t *testing.T) {
	home, cwd := t.TempDir(), t.TempDir()
	if err := os.MkdirAll(filepath.Join(cwd, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	skill := func(root, name string, size int) {
		paddedFile(t, filepath.Join(root, ".claude", "skills", name, "SKILL.md"),
			"---\nname: "+name+"\ndescription: "+name+" skill\n---\n", size)
	}
	skill(home, "personal-large", 100*1024)
	skill(home, "personal-at-limit", maxCommandFileSize)
	skill(home, "personal-oversized", maxCommandFileSize+1)
	skill(cwd, "project-large", 100*1024)
	skill(cwd, "project-oversized", maxCommandFileSize+1)

	catalog := CatalogFor("claude", cwd, home)
	for _, name := range []string{"/personal-large", "/personal-at-limit", "/project-large"} {
		command, ok := findCatalogCommand(catalog, name)
		if !ok {
			t.Errorf("%s missing", name)
			continue
		}
		if want := strings.TrimPrefix(name, "/") + " skill"; command.Description != want {
			t.Errorf("%s description = %q, want %q", name, command.Description, want)
		}
	}
	for _, name := range []string{"/personal-oversized", "/project-oversized"} {
		if hasCommand(catalog, name) {
			t.Errorf("%s exceeds the file size limit but was published", name)
		}
	}
	if catalog.Truncated {
		t.Error("skipping an oversized file marked the catalog truncated")
	}
}

func TestClaudeCommandsWithLargeBodies(t *testing.T) {
	home := t.TempDir()
	commands := filepath.Join(home, ".claude", "commands")
	paddedFile(t, filepath.Join(commands, "described.md"),
		"---\ndescription: Large command\nargument-hint: <target>\n---\n", 100*1024)
	paddedFile(t, filepath.Join(commands, "first-line.md"), "Uses the first line\n", 100*1024)
	paddedFile(t, filepath.Join(commands, "hidden.md"), "---\nhidden: true\n---\n", 100*1024)
	paddedFile(t, filepath.Join(commands, "internal.md"), "---\nuser-invocable: false\n---\n", 100*1024)
	paddedFile(t, filepath.Join(commands, "oversized.md"), "---\nhidden: true\n---\n", maxCommandFileSize+1)

	catalog := CatalogFor("claude", t.TempDir(), home)
	described, ok := findCatalogCommand(catalog, "/described")
	if !ok {
		t.Fatal("/described missing")
	}
	if described.Description != "Large command" || described.ArgumentHint != "<target>" {
		t.Errorf("/described = %+v", described)
	}
	firstLine, ok := findCatalogCommand(catalog, "/first-line")
	if !ok {
		t.Fatal("/first-line missing")
	}
	if firstLine.Description != "Uses the first line" {
		t.Errorf("/first-line description = %q", firstLine.Description)
	}
	for _, name := range []string{"/hidden", "/internal", "/oversized"} {
		if hasCommand(catalog, name) {
			t.Errorf("%s should not be published", name)
		}
	}
	if catalog.Truncated {
		t.Error("skipping an oversized file marked the catalog truncated")
	}
}

func TestQoderSkillWithLargeBody(t *testing.T) {
	home := t.TempDir()
	paddedFile(t, filepath.Join(home, ".qoder", "skills", "large", "SKILL.md"),
		"---\nname: large\ndescription: Large skill\n---\n", 100*1024)

	catalog := CatalogFor("qoder", t.TempDir(), home)
	command, ok := findCatalogCommand(catalog, "/large")
	if !ok {
		t.Fatal("/large missing")
	}
	if command.Description != "Large skill" {
		t.Errorf("/large description = %q", command.Description)
	}
}
