package slashcmd

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func assertCursorCustomCommands(t *testing.T, catalog Catalog, want ...Command) {
	t.Helper()
	var got []Command
	for _, command := range catalog.Commands {
		if command.Source != "builtin" {
			got = append(got, command)
		}
	}
	compare := func(a, b Command) int { return strings.Compare(a.Command, b.Command) }
	slices.SortFunc(got, compare)
	slices.SortFunc(want, compare)
	if !slices.Equal(got, want) {
		t.Errorf("custom commands = %+v, want %+v", got, want)
	}
}

func TestCursorBuiltinCollisionsIgnoreCase(t *testing.T) {
	for _, scope := range []string{"project", "personal", "configured"} {
		t.Run(scope, func(t *testing.T) {
			home, cwd := t.TempDir(), t.TempDir()
			root := home
			if scope == "project" {
				root = cwd
			}
			writeFile(t, filepath.Join(root, ".cursor", "commands", "Clear.md"), "Custom clear\n")
			skillRoot := filepath.Join(root, ".cursor", "skills")
			var skillDirs []string
			if scope == "configured" {
				skillRoot = t.TempDir()
				skillDirs = []string{skillRoot}
			}
			cursorSkill(t, skillRoot, "HeLP", "HeLP", "Custom help")
			cursorSkill(t, skillRoot, "clear-cache", "clear-cache", "Clear cache")

			catalog := CatalogForProfile("cursor", "cursor", cwd, home, skillDirs, "/{name}", "", "")
			for _, name := range []string{"/clear", "/help"} {
				if commandSource(catalog, name) != "builtin" {
					t.Errorf("%s lost its builtin", name)
				}
			}
			source := scope
			if source == "configured" {
				source = "personal"
			}
			assertCursorCustomCommands(t, catalog, Command{"/clear-cache", "Clear cache", source, ""})
		})
	}
}

func TestCursorCommandSkillCollisionsIgnoreCase(t *testing.T) {
	home, cwd, extra := t.TempDir(), t.TempDir(), t.TempDir()
	writeFile(t, filepath.Join(cwd, ".cursor", "commands", "Review.md"), "Command review\n")
	cursorSkill(t, filepath.Join(home, ".cursor", "skills"), "review", "review", "Native skill")
	cursorSkill(t, extra, "REVIEW", "REVIEW", "Configured skill")
	catalog := CatalogForProfile("cursor", "cursor", cwd, home, []string{extra}, "/{name}", "", "")
	assertCursorCustomCommands(t, catalog, Command{"/Review", "Command review", "project", ""})
}

func TestCursorCaseVariantsKeepFirstRegisteredCommand(t *testing.T) {
	home, cwd := t.TempDir(), t.TempDir()
	writeFile(t, filepath.Join(cwd, ".cursor", "commands", "Review.md"), "Project review\n")
	writeFile(t, filepath.Join(home, ".cursor", "commands", "review.md"), "Personal review\n")
	catalog := cursorCatalog(t, cwd, home)
	assertCursorCustomCommands(t, catalog, Command{"/Review", "Project review", "project", ""})

	writeFile(t, filepath.Join(home, ".claude", "commands", "Review.md"), "Exact replacement\n")
	catalog = cursorCatalog(t, cwd, home)
	assertCursorCustomCommands(t, catalog, Command{"/Review", "Exact replacement", "personal", ""})
}

func TestCursorCommandFrontmatterDoesNotSuppressRegistration(t *testing.T) {
	for _, stem := range []string{".cursor", ".claude"} {
		for _, field := range []string{"hidden: true", "user-invocable: false"} {
			for _, scope := range []string{"personal", "project"} {
				t.Run(stem+"/"+field+"/"+scope, func(t *testing.T) {
					home, cwd := t.TempDir(), t.TempDir()
					root := home
					if scope == "project" {
						root = cwd
					}
					writeFile(t, filepath.Join(root, stem, "commands", "review.md"),
						"---\ndescription: Command review\nargument-hint: <command>\n"+field+"\n---\nReview instructions\n")
					cursorSkill(t, filepath.Join(home, ".cursor", "skills"), "review", "review", "Skill review")
					catalog := cursorCatalog(t, cwd, home)
					assertCursorCustomCommands(t, catalog, Command{"/review", "Command review", scope, "<command>"})
				})
			}
		}
	}
}

func TestCursorNestedSkillIDsUseFullPathOrder(t *testing.T) {
	for _, parentInvocable := range []bool{false, true} {
		t.Run(fmt.Sprintf("parent-invocable=%v", parentInvocable), func(t *testing.T) {
			home := t.TempDir()
			root := filepath.Join(home, ".cursor", "skills")
			writeFile(t, filepath.Join(root, "a", "b", "SKILL.md"),
				fmt.Sprintf("---\ndescription: Parent\nuser-invocable: %v\n---\nParent instructions\n", parentInvocable))
			cursorSkill(t, filepath.Join(root, "a", "b"), "a-b", "ignored", "Nested")
			cursorSkill(t, filepath.Join(root, "z"), "b", "ignored", "Sibling")

			catalog := cursorCatalog(t, t.TempDir(), home)
			want := []Command{{"/a-b", "Nested", "personal", ""}, {"/z-b", "Sibling", "personal", ""}}
			if parentInvocable {
				want = append(want, Command{"/a-b-2", "Parent", "personal", ""})
			}
			assertCursorCustomCommands(t, catalog, want...)
		})
	}
}

func TestCursorSkillWalkUsesLocalePathOrder(t *testing.T) {
	root := filepath.Join(t.TempDir(), "skills")
	names := []string{"a_b", "a-b", "a:b", "a.b", "alpha1", "Alpha2"}
	var want []string
	for _, name := range names {
		cursorSkill(t, root, name, "ignored", name)
		want = append(want, filepath.Join(root, name))
	}
	budget := maxCustomFiles
	got, truncated := cursorSkillWalk(root, false, &budget, make(map[string]bool))
	if !slices.Equal(got, want) || truncated {
		t.Errorf("skill paths = %v, truncated = %v, want %v", got, truncated, want)
	}
}

func TestCursorSkillAliasSelectionMatchesLoaderOrder(t *testing.T) {
	for _, project := range []bool{false, true} {
		for _, linkFile := range []bool{false, true} {
			t.Run(fmt.Sprintf("project=%v/file-link=%v", project, linkFile), func(t *testing.T) {
				home, cwd := t.TempDir(), t.TempDir()
				root, source := home, "personal"
				if project {
					root, source = cwd, "project"
				}
				root = filepath.Join(root, ".cursor", "skills")
				cursorSkill(t, root, "review", "ignored", "Review instructions")
				target, link := filepath.Join(root, "review"), filepath.Join(root, "Z-alias")
				name := "/Z-alias"
				if linkFile {
					target, link = filepath.Join(target, "SKILL.md"), filepath.Join(link, "SKILL.md")
					mkdirAll(t, filepath.Dir(link))
					name = "/review"
				}
				if err := os.Symlink(target, link); err != nil {
					t.Skipf("symlinks unavailable: %v", err)
				}
				catalog := cursorCatalog(t, cwd, home)
				assertCursorCustomCommands(t, catalog, Command{name, "Review instructions", source, ""})
			})
		}
	}
}

func TestCursorSkipsEmptyFiles(t *testing.T) {
	for _, scope := range []string{"project", "personal"} {
		t.Run(scope, func(t *testing.T) {
			home, cwd := t.TempDir(), t.TempDir()
			root := home
			if scope == "project" {
				root = cwd
			}
			for _, stem := range []string{".cursor", ".claude"} {
				writeFile(t, filepath.Join(root, stem, "commands", "empty-command.md"), "")
			}
			skillRoot := filepath.Join(root, ".cursor", "skills")
			writeFile(t, filepath.Join(skillRoot, "empty-skill", "SKILL.md"), "")
			cursorSkill(t, skillRoot, "sibling", "ignored", "Sibling")
			catalog := cursorCatalog(t, cwd, home)
			assertCursorCustomCommands(t, catalog, Command{"/sibling", "Sibling", scope, ""})
		})
	}
}

func TestCursorEmptySkillDoesNotReserveAnID(t *testing.T) {
	home := t.TempDir()
	root := filepath.Join(home, ".cursor", "skills")
	writeFile(t, filepath.Join(root, "x", "y", "SKILL.md"), "")
	cursorSkill(t, filepath.Join(root, "x", "y"), "x-y", "ignored", "Nested")
	cursorSkill(t, filepath.Join(root, "z"), "y", "ignored", "Sibling")
	catalog := cursorCatalog(t, t.TempDir(), home)
	assertCursorCustomCommands(t, catalog,
		Command{"/x-y", "Nested", "personal", ""},
		Command{"/z-y", "Sibling", "personal", ""},
	)
}

func TestCursorEmptyCommandDoesNotOverrideSkill(t *testing.T) {
	home, cwd := t.TempDir(), t.TempDir()
	writeFile(t, filepath.Join(home, ".cursor", "commands", "review.md"), "")
	cursorSkill(t, filepath.Join(home, ".cursor", "skills"), "review", "ignored", "Skill review")
	catalog := cursorCatalog(t, cwd, home)
	assertCursorCustomCommands(t, catalog, Command{"/review", "Skill review", "personal", ""})

	writeFile(t, filepath.Join(cwd, ".cursor", "commands", "review.md"), "Project review\n")
	catalog = cursorCatalog(t, cwd, home)
	assertCursorCustomCommands(t, catalog, Command{"/review", "Project review", "project", ""})
}
