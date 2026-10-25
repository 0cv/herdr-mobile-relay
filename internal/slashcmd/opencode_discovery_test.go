package slashcmd

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeOpenCodeCommand(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0600); err != nil {
		t.Fatal(err)
	}
}

func TestOpenCodeNativeMarkdownCommands(t *testing.T) {
	home, config, project := t.TempDir(), t.TempDir(), t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", config)
	writeOpenCodeCommand(t, filepath.Join(config, "opencode", "command", "personal.md"), "---\ndescription: Personal command\n---\nNever execute this body.\n")
	writeOpenCodeCommand(t, filepath.Join(project, ".opencode", "commands", "git", "review.md"), "---\ndescription: Nested review\nhidden: true\nuser-invocable: false\n---\nNever execute this body.\n")
	writeOpenCodeCommand(t, filepath.Join(project, ".opencode", "command", "legacy.md"), "Legacy spelling")
	writeOpenCodeCommand(t, filepath.Join(project, ".opencode", "skills", "not-a-slash-command", "SKILL.md"), "---\nname: not-a-slash-command\ndescription: Native @ mention\n---\nSkill body")
	catalog := CatalogFor("opencode", project, home)
	for _, name := range []string{"/personal", "/git/review", "/legacy"} {
		if !hasCommand(catalog, name) {
			t.Errorf("missing native command %s", name)
		}
	}
	for _, cmd := range catalog.Commands {
		if cmd.Command == "/git/review" && (cmd.Description != "Nested review" || cmd.Source != "project") {
			t.Errorf("native command metadata: %+v", cmd)
		}
	}
	if catalog.Truncated || hasCommand(catalog, "/not-a-slash-command") {
		t.Fatalf("skills are @ mentions, not slash commands: %+v", catalog)
	}
}

func TestOpenCodeCommandPrecedenceAndSuppression(t *testing.T) {
	home, project := t.TempDir(), t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", "")
	writeOpenCodeCommand(t, filepath.Join(home, ".config", "opencode", "commands", "deploy.md"), "---\ndescription: Personal\n---\nBody")
	writeOpenCodeCommand(t, filepath.Join(project, ".opencode", "commands", "deploy.md"), "---\ndescription: Outer\n---\nBody")
	cwd := filepath.Join(project, "src")
	writeOpenCodeCommand(t, filepath.Join(cwd, ".opencode", "command", "deploy.md"), "---\ndescription: Inner\n---\nBody")
	writeOpenCodeCommand(t, filepath.Join(cwd, ".opencode", "commands", "help.md"), "---\ndescription: Must not override builtin\n---\nBody")
	catalog := CatalogFor("opencode", cwd, home)
	found := false
	for _, cmd := range catalog.Commands {
		if cmd.Command == "/deploy" {
			if found || cmd.Description != "Inner" || cmd.Source != "project" {
				t.Fatalf("precedence: %+v", catalog)
			}
			found = true
		}
		if cmd.Command == "/help" && cmd.Source != "builtin" {
			t.Fatal("custom command replaced builtin")
		}
	}
	if !found {
		t.Fatal("missing nearest project command")
	}
	commands, truncated := (&openCodeProvider{}).Discover(DiscoverContext{Home: home, Cwd: cwd, SuppressNative: true})
	if truncated || len(commands) != len(openCodeBuiltins) {
		t.Fatal("native suppression did not retain only builtins")
	}
}

func TestOpenCodeCommandBoundsAndReadOnly(t *testing.T) {
	home, config, project := t.TempDir(), t.TempDir(), t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", config)
	root := filepath.Join(project, ".opencode", "commands")
	outside := filepath.Join(home, "outside.md")
	writeOpenCodeCommand(t, outside, "Outside marker")
	writeOpenCodeCommand(t, filepath.Join(root, "oversized.md"), strings.Repeat("x", maxCommandFileSize+1))
	writeOpenCodeCommand(t, filepath.Join(root, "kept.md"), "---\ndescription: Metadata only\n---\nNever execute $(touch unexpected)")
	if err := os.Symlink(outside, filepath.Join(root, "escape.md")); err != nil {
		t.Fatal(err)
	}
	catalog := CatalogFor("opencode", project, home)
	if catalog.Truncated || !hasCommand(catalog, "/kept") || hasCommand(catalog, "/oversized") || hasCommand(catalog, "/escape") {
		t.Fatalf("bounded file handling: %+v", catalog)
	}
	data, err := os.ReadFile(outside)
	if err != nil || string(data) != "Outside marker" {
		t.Fatal("discovery changed source")
	}
	for i := 0; i <= maxCustomFiles; i++ {
		writeOpenCodeCommand(t, filepath.Join(root, fmt.Sprintf("budget%04d.md", i)), "Body")
	}
	catalog = CatalogFor("opencode", project, home)
	if !catalog.Truncated || len(catalog.Commands) != len(openCodeBuiltins)+maxCustomFiles {
		t.Fatalf("file budget: entries=%d truncated=%v", len(catalog.Commands), catalog.Truncated)
	}
}
