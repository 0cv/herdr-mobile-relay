package slashcmd

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"testing"
)

func TestCursorProjectRootsDoNotIncludeAncestors(t *testing.T) {
	for _, gitRepo := range []bool{false, true} {
		t.Run(fmt.Sprintf("git=%v", gitRepo), func(t *testing.T) {
			repo, home := t.TempDir(), t.TempDir()
			if gitRepo {
				writeFile(t, filepath.Join(repo, ".git"), "gitdir: elsewhere")
			}
			cwd := filepath.Join(repo, "app")
			for _, stem := range []string{".claude", ".cursor"} {
				writeFile(t, filepath.Join(repo, stem, "commands", "parent-command.md"), "Parent command\n")
				writeFile(t, filepath.Join(cwd, stem, "commands", "local-command.md"), "Local command\n")
			}
			for _, stem := range []string{".cursor", ".agents"} {
				cursorSkill(t, filepath.Join(repo, stem, "skills"), "parent-skill", "ignored", "Parent skill")
				cursorSkill(t, filepath.Join(cwd, stem, "skills"), "local-skill", "ignored", "Local skill")
			}

			catalog := cursorCatalog(t, cwd, home)
			for _, name := range []string{"/parent-command", "/parent-skill"} {
				if containsCommand(catalog, name) {
					t.Errorf("ancestor command %s was published", name)
				}
			}
			for _, name := range []string{"/local-command", "/local-skill"} {
				if commandSource(catalog, name) != "project" {
					t.Errorf("workspace command %s missing; catalog=%v", name, commandNames(catalog))
				}
			}
		})
	}
}

func TestCursorPersonalSkillFileSymlinkBoundary(t *testing.T) {
	for _, linkedDir := range []bool{false, true} {
		for _, insideRoot := range []bool{false, true} {
			t.Run(fmt.Sprintf("linked-dir=%v/inside-root=%v", linkedDir, insideRoot), func(t *testing.T) {
				home := t.TempDir()
				root := filepath.Join(home, ".cursor", "skills")
				targetRoot := filepath.Join(home, "dotfiles")
				if insideRoot {
					targetRoot = filepath.Join(root, ".targets")
				}
				cursorSkill(t, targetRoot, "target", "ignored", "Linked skill")
				skillDir := filepath.Join(root, "linked")
				if linkedDir {
					targetDir := filepath.Join(home, "dotfiles", "linked-directory")
					mkdirAll(t, targetDir)
					mkdirAll(t, root)
					if err := os.Symlink(targetDir, skillDir); err != nil {
						t.Skipf("symlinks unavailable: %v", err)
					}
				} else {
					mkdirAll(t, skillDir)
				}
				if err := os.Symlink(filepath.Join(targetRoot, "target", "SKILL.md"), filepath.Join(skillDir, "SKILL.md")); err != nil {
					t.Skipf("symlinks unavailable: %v", err)
				}
				cursorSkill(t, root, "sibling", "ignored", "Sibling skill")

				catalog := cursorCatalog(t, t.TempDir(), home)
				if got := containsCommand(catalog, "/linked"); got != insideRoot {
					t.Errorf("linked file published = %v, want %v", got, insideRoot)
				}
				if !containsCommand(catalog, "/sibling") {
					t.Error("sibling skill missing")
				}
			})
		}
	}
}

func TestCursorPersonalLinkedSkillDoesNotDescend(t *testing.T) {
	for _, insideRoot := range []bool{false, true} {
		t.Run(fmt.Sprintf("inside-root=%v", insideRoot), func(t *testing.T) {
			home := t.TempDir()
			root := filepath.Join(home, ".cursor", "skills")
			targetRoot := filepath.Join(home, "dotfiles")
			if insideRoot {
				targetRoot = filepath.Join(root, ".targets")
			}
			cursorSkill(t, targetRoot, "parent", "ignored", "Parent skill")
			cursorSkill(t, filepath.Join(targetRoot, "parent"), "nested", "ignored", "Nested skill")
			cursorSkill(t, filepath.Join(root, "tools"), "nested", "ignored", "Workspace nested skill")
			if err := os.Symlink(filepath.Join(targetRoot, "parent"), filepath.Join(root, "linked")); err != nil {
				t.Skipf("symlinks unavailable: %v", err)
			}

			catalog := cursorCatalog(t, t.TempDir(), home)
			if !containsCommand(catalog, "/linked") {
				t.Error("linked directory's own skill missing")
			}
			if !containsCommand(catalog, "/nested") {
				t.Errorf("linked descendants changed the sibling ID; catalog=%v", commandNames(catalog))
			}
			for _, name := range []string{"/linked-nested", "/tools-nested"} {
				if containsCommand(catalog, name) {
					t.Errorf("linked descendant introduced %s", name)
				}
			}
		})
	}
}

func TestCursorPersonalOutsideFileLinkDoesNotChangeSkillID(t *testing.T) {
	home := t.TempDir()
	root := filepath.Join(home, ".cursor", "skills")
	cursorSkill(t, filepath.Join(root, "tools"), "review", "ignored", "Real review")
	cursorSkill(t, home, "review", "ignored", "Outside review")
	link := filepath.Join(root, "other", "review", "SKILL.md")
	mkdirAll(t, filepath.Dir(link))
	if err := os.Symlink(filepath.Join(home, "review", "SKILL.md"), link); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}

	catalog := cursorCatalog(t, t.TempDir(), home)
	if !containsCommand(catalog, "/review") {
		t.Errorf("outside file link changed the valid skill ID; catalog=%v", commandNames(catalog))
	}
	for _, name := range []string{"/other-review", "/tools-review"} {
		if containsCommand(catalog, name) {
			t.Errorf("outside file link introduced %s", name)
		}
	}
}

func TestCursorCommandWinsSkillCollision(t *testing.T) {
	for _, commandScope := range []string{"project", "personal"} {
		for _, skillScope := range []string{"project", "personal", "configured"} {
			for _, stem := range []string{".claude", ".cursor"} {
				t.Run(commandScope+"-command/"+skillScope+"-skill/"+stem, func(t *testing.T) {
					repo, home := t.TempDir(), t.TempDir()
					commandRoot := repo
					if commandScope == "personal" {
						commandRoot = home
					}
					writeFile(t, filepath.Join(commandRoot, stem, "commands", "review.md"),
						"---\ndescription: Command review\nargument-hint: <command>\n---\nBody\n")
					skillRoot := filepath.Join(repo, ".cursor", "skills")
					var skillDirs []string
					switch skillScope {
					case "personal":
						skillRoot = filepath.Join(home, ".cursor", "skills")
					case "configured":
						skillRoot = filepath.Join(home, "extra-skills")
						skillDirs = []string{skillRoot}
					}
					writeFile(t, filepath.Join(skillRoot, "review", "SKILL.md"),
						"---\nname: review\ndescription: Skill review\nargument-hint: <skill>\n---\nBody\n")

					catalog := CatalogForProfile("cursor", "cursor", repo, home, skillDirs, "/{name}", "", "")
					want := Command{"/review", "Command review", commandScope, "<command>"}
					count := 0
					for _, command := range catalog.Commands {
						if command.Command != "/review" {
							continue
						}
						count++
						if command != want {
							t.Errorf("/review = %+v, want %+v", command, want)
						}
					}
					if count != 1 {
						t.Errorf("published %d /review entries, want 1", count)
					}
				})
			}
		}
	}
}

func TestCursorSkillWalkRootAndDepth(t *testing.T) {
	for _, project := range []bool{false, true} {
		t.Run(fmt.Sprintf("project=%v", project), func(t *testing.T) {
			root := filepath.Join(t.TempDir(), "skills")
			writeFile(t, filepath.Join(root, "SKILL.md"), "Root skill\n")
			cursorSkill(t, root, "parent", "ignored", "Parent skill")
			deep := filepath.Join(root, "parent")
			for depth := 2; depth <= 10; depth++ {
				deep = filepath.Join(deep, fmt.Sprintf("level-%d", depth))
			}
			writeFile(t, filepath.Join(deep, "SKILL.md"), "Depth 10\n")
			writeFile(t, filepath.Join(deep, "too-deep", "SKILL.md"), "Depth 11\n")
			budget := maxCustomFiles
			dirs, truncated := cursorSkillWalk(root, project, &budget, make(map[string]bool))
			want := []string{root, filepath.Join(root, "parent"), deep}
			slices.Sort(want)
			if !slices.Equal(dirs, want) {
				t.Errorf("skill directories = %v, want %v", dirs, want)
			}
			if truncated || budget != maxCustomFiles-len(want) {
				t.Errorf("truncated = %v, remaining budget = %d", truncated, budget)
			}
		})
	}
}

func TestCursorRootSkillChangesNestedDuplicateID(t *testing.T) {
	home := t.TempDir()
	root := filepath.Join(home, ".cursor", "skills")
	writeFile(t, filepath.Join(root, "SKILL.md"), "Root skill\n")
	cursorSkill(t, filepath.Join(root, "other"), "skills", "ignored", "Nested skill")

	catalog := cursorCatalog(t, t.TempDir(), home)
	if !containsCommand(catalog, "/other-skills") {
		t.Errorf("root skill missing from duplicate detection; catalog=%v", commandNames(catalog))
	}
	if commandSource(catalog, "/skills") != "builtin" {
		t.Error("root skill overrode the builtin")
	}
}
