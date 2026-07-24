package slashcmd

import (
	"os"
	"path/filepath"
	"testing"
)

func TestWalkFlat(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "deploy.md"), []byte("Deploy the app"), 0o644)
	os.WriteFile(filepath.Join(dir, "test.md"), []byte("Run tests"), 0o644)
	os.WriteFile(filepath.Join(dir, "notes.txt"), []byte("ignored"), 0o644)

	commands := walkCommandDir(dir, "personal")
	if len(commands) != 2 {
		t.Fatalf("got %d commands, want 2: %+v", len(commands), commands)
	}
	names := map[string]bool{}
	for _, cmd := range commands {
		names[cmd.Command] = true
		if cmd.Source != "personal" {
			t.Errorf("source = %q", cmd.Source)
		}
	}
	if !names["/deploy"] || !names["/test"] {
		t.Errorf("missing expected commands: %v", names)
	}
}

func TestWalkRecursive(t *testing.T) {
	dir := t.TempDir()
	os.MkdirAll(filepath.Join(dir, "git"), 0o755)
	os.WriteFile(filepath.Join(dir, "git", "commit.md"), []byte("Commit changes"), 0o644)
	os.MkdirAll(filepath.Join(dir, "a", "b"), 0o755)
	os.WriteFile(filepath.Join(dir, "a", "b", "c.md"), []byte("Deep command"), 0o644)

	commands := walkCommandDir(dir, "project")
	names := map[string]bool{}
	for _, cmd := range commands {
		names[cmd.Command] = true
	}
	if !names["/git:commit"] {
		t.Error("missing /git:commit")
	}
	if !names["/a:b:c"] {
		t.Error("missing /a:b:c")
	}
}

func TestWalkHasNoArbitraryDepthLimit(t *testing.T) {
	dir := t.TempDir()
	deep := dir
	for i := 0; i < 7; i++ {
		deep = filepath.Join(deep, "level")
		os.MkdirAll(deep, 0o755)
	}
	os.WriteFile(filepath.Join(deep, "too-deep.md"), []byte("Should not appear"), 0o644)

	// Also create one at depth 5 (should appear)
	at5 := dir
	for i := 0; i < 5; i++ {
		at5 = filepath.Join(at5, "sub")
	}
	os.MkdirAll(at5, 0o755)
	os.WriteFile(filepath.Join(at5, "ok.md"), []byte("At depth 5"), 0o644)

	commands := walkCommandDir(dir, "personal")
	if !commandSliceHas(commands, "/level:level:level:level:level:level:level:too-deep") {
		t.Error("deep command was omitted")
	}
}

func commandSliceHas(commands []Command, name string) bool {
	for _, command := range commands {
		if command.Command == name {
			return true
		}
	}
	return false
}

func TestWalkFileLimit(t *testing.T) {
	dir := t.TempDir()
	for i := 0; i < 260; i++ {
		name := filepath.Join(dir, "cmd"+string(rune('a'+i%26))+string(rune('0'+i/26))+".md")
		os.WriteFile(name, []byte("cmd"), 0o644)
	}
	commands := walkCommandDir(dir, "personal")
	if len(commands) > maxWalkFiles {
		t.Errorf("got %d commands, limit is %d", len(commands), maxWalkFiles)
	}
}

func TestWalkSymlinkDirSkipped(t *testing.T) {
	dir := t.TempDir()
	real := filepath.Join(dir, "real")
	os.MkdirAll(real, 0o755)
	os.WriteFile(filepath.Join(real, "inside.md"), []byte("Inside"), 0o644)
	os.Symlink(real, filepath.Join(dir, "linked"))

	commands := walkCommandDir(dir, "personal")
	for _, cmd := range commands {
		if cmd.Command == "/linked:inside" {
			t.Error("symlinked directory was followed")
		}
	}
}

func TestWalkSymlinkFileSkipped(t *testing.T) {
	dir := t.TempDir()
	real := filepath.Join(dir, "real.md")
	os.WriteFile(real, []byte("Real"), 0o644)
	os.Symlink(real, filepath.Join(dir, "link.md"))

	commands := walkCommandDir(dir, "personal")
	for _, cmd := range commands {
		if cmd.Command == "/link" {
			t.Error("symlinked file was included")
		}
	}
}

func TestWalkHiddenSkipped(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, ".hidden.md"), []byte("Hidden"), 0o644)
	os.MkdirAll(filepath.Join(dir, ".hiddendir"), 0o755)
	os.WriteFile(filepath.Join(dir, ".hiddendir", "inside.md"), []byte("Inside hidden"), 0o644)
	os.WriteFile(filepath.Join(dir, "visible.md"), []byte("Visible"), 0o644)

	commands := walkCommandDir(dir, "personal")
	if len(commands) != 1 || commands[0].Command != "/visible" {
		t.Errorf("expected only /visible, got %+v", commands)
	}
}

func TestWalkInvalidNameSkipped(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, " leading-space.md"), []byte("Bad"), 0o644)
	os.WriteFile(filepath.Join(dir, "good.md"), []byte("Good"), 0o644)

	commands := walkCommandDir(dir, "personal")
	if len(commands) != 1 || commands[0].Command != "/good" {
		t.Errorf("expected only /good, got %+v", commands)
	}
}

func TestWalkSkillDirInCommands(t *testing.T) {
	dir := t.TempDir()
	skillSub := filepath.Join(dir, "my-skill")
	os.MkdirAll(skillSub, 0o755)
	os.WriteFile(filepath.Join(skillSub, "SKILL.md"), []byte("---\nname: my-skill\ndescription: A skill\n---\n"), 0o644)
	os.WriteFile(filepath.Join(skillSub, "sibling.md"), []byte("Should be ignored"), 0o644)

	commands := walkCommandDir(dir, "personal")
	if len(commands) != 1 {
		t.Fatalf("expected 1 command, got %+v", commands)
	}
	if commands[0].Command != "/my-skill" {
		t.Errorf("command = %q", commands[0].Command)
	}
	if commands[0].Description != "A skill" {
		t.Errorf("description = %q", commands[0].Description)
	}
}

func TestWalkHiddenFrontmatter(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "secret.md"), []byte("---\nhidden: true\n---\nSecret command"), 0o644)
	os.WriteFile(filepath.Join(dir, "public.md"), []byte("Public command"), 0o644)

	commands := walkCommandDir(dir, "personal")
	if len(commands) != 1 || commands[0].Command != "/public" {
		t.Errorf("expected only /public, got %+v", commands)
	}
}

func TestFindGitRoot(t *testing.T) {
	root := t.TempDir()
	os.MkdirAll(filepath.Join(root, ".git"), 0o755)
	sub := filepath.Join(root, "a", "b", "c")
	os.MkdirAll(sub, 0o755)

	got := findGitRoot(sub)
	if got != root {
		t.Errorf("findGitRoot = %q, want %q", got, root)
	}
}

func TestFindGitRootWorktree(t *testing.T) {
	root := t.TempDir()
	os.WriteFile(filepath.Join(root, ".git"), []byte("gitdir: /other/.git/worktrees/x"), 0o644)
	sub := filepath.Join(root, "src")
	os.MkdirAll(sub, 0o755)

	got := findGitRoot(sub)
	if got != root {
		t.Errorf("findGitRoot = %q, want %q", got, root)
	}
}
