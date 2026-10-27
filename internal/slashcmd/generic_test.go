package slashcmd

import (
	"os"
	"path/filepath"
	"testing"
)

// A symlinked skill directory must be followed on the generic path. It gated
// on DirEntry.IsDir(), which is false for a symlink, so a linked skill was
// silently dropped from the palette even though the native skill scanners all
// follow links through entryIsDir. walk.go documents that skill directories
// "carry no namespace and are de-duplicated by resolved path, so
// scanSkillDirBudget can and does follow them"; this keeps the generic path
// consistent with that.
func TestGenericDiscoveryFollowsSymlinkedSkillDir(t *testing.T) {
	home := t.TempDir()
	target := filepath.Join(home, "checkouts", "linked-skill")
	writeSkillAt(t, filepath.Dir(target), "linked-skill", "Linked")

	links := filepath.Join(home, "generic-skills")
	mkdirAll(t, links)
	if err := os.Symlink(target, filepath.Join(links, "linked-skill")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}

	commands, _ := discoverGenericSkills([]string{links}, "/{name}")
	if !commandInList(commands, "/linked-skill") {
		t.Errorf("generic scanner dropped a symlinked skill dir; got %v", commandNamesIn(commands))
	}
}

// A symlink whose target has no SKILL.md is still ignored, and a plain file in
// the same directory must not be mistaken for a skill directory.
func TestGenericDiscoveryIgnoresNonSkillEntries(t *testing.T) {
	home := t.TempDir()
	links := filepath.Join(home, "generic-skills")
	mkdirAll(t, links)
	writeFile(t, filepath.Join(links, "loose.md"), "not a skill dir")
	mkdirAll(t, filepath.Join(links, "empty-dir"))
	writeSkillAt(t, links, "real-skill", "Real")

	commands, _ := discoverGenericSkills([]string{links}, "/{name}")
	if !commandInList(commands, "/real-skill") {
		t.Errorf("real skill missing; got %v", commandNamesIn(commands))
	}
	if len(commands) != 1 {
		t.Errorf("expected only the real skill, got %v", commandNamesIn(commands))
	}
}

// writeSkillAt creates <dir>/<folder>/SKILL.md with frontmatter.
func writeSkillAt(t *testing.T, dir, folder, description string) {
	t.Helper()
	body := "---\nname: " + folder + "\n"
	if description != "" {
		body += "description: " + description + "\n"
	}
	body += "---\n\nbody\n"
	writeFile(t, filepath.Join(dir, folder, "SKILL.md"), body)
}
