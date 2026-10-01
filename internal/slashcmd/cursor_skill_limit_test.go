package slashcmd

import (
	"fmt"
	"path/filepath"
	"strings"
	"testing"
)

func TestCursorSkillIDsAtFileBudget(t *testing.T) {
	for _, scope := range []string{"project", "personal"} {
		for _, remaining := range []int{1, 2} {
			t.Run(fmt.Sprintf("%s/remaining=%d", scope, remaining), func(t *testing.T) {
				home, cwd := t.TempDir(), t.TempDir()
				commandCount := maxCustomFiles - remaining
				for i := 0; i < commandCount; i++ {
					writeFile(t, filepath.Join(home, ".cursor", "commands", fmt.Sprintf("cmd-%04d.md", i)), "Command\n")
				}
				root := home
				if scope == "project" {
					root = cwd
				}
				root = filepath.Join(root, ".cursor", "skills")
				cursorSkill(t, filepath.Join(root, "a"), "review", "ignored", "First review")
				cursorSkill(t, filepath.Join(root, "z"), "review", "ignored", "Second review")

				catalog := cursorCatalog(t, cwd, home)
				if !catalog.Truncated {
					t.Error("exhausted file budget did not mark catalog truncated")
				}
				if containsCommand(catalog, "/review") {
					t.Error("published /review, but Cursor registers only /a-review and /z-review")
				}
				wantCount := len(cursorBuiltins) + commandCount
				if remaining == 2 {
					wantCount += 2
				}
				if len(catalog.Commands) != wantCount {
					t.Errorf("catalog has %d commands, want %d", len(catalog.Commands), wantCount)
				}
				for _, name := range []string{"/a-review", "/z-review"} {
					if got, want := containsCommand(catalog, name), remaining == 2; got != want {
						t.Errorf("%s present = %v, want %v", name, got, want)
					}
				}
				for _, builtin := range cursorBuiltins {
					if commandSource(catalog, builtin.Command) != "builtin" {
						t.Errorf("builtin %s lost", builtin.Command)
					}
				}
			})
		}
	}
}

func TestCursorDirectoryBudgetOmitsIncompleteSkillRoot(t *testing.T) {
	for _, scope := range []string{"project", "personal"} {
		t.Run(scope, func(t *testing.T) {
			home, cwd := t.TempDir(), t.TempDir()
			root := home
			if scope == "project" {
				root = cwd
			}
			cursorRoot := filepath.Join(root, ".cursor", "skills")
			cursorSkill(t, filepath.Join(cursorRoot, "a"), "review", "ignored", "First review")
			for i := 0; i < maxEntries-3; i++ {
				mkdirAll(t, filepath.Join(cursorRoot, fmt.Sprintf("m-%04d", i)))
			}
			cursorSkill(t, filepath.Join(cursorRoot, "z"), "review", "ignored", "Second review")
			cursorSkill(t, filepath.Join(root, ".agents", "skills"), "complete-root", "ignored", "Complete root")

			catalog := cursorCatalog(t, cwd, home)
			if !catalog.Truncated {
				t.Error("exhausted directory budget did not mark catalog truncated")
			}
			assertCursorCustomCommands(t, catalog, Command{"/complete-root", "Complete root", scope, ""})
		})
	}
}

func TestCursorSkillSizePreservesCollisionIDs(t *testing.T) {
	for _, scope := range []string{"project", "personal"} {
		for _, size := range []int{64*1024 + 1, 1 << 20, 1<<20 + 1} {
			t.Run(fmt.Sprintf("%s/size=%d", scope, size), func(t *testing.T) {
				home, cwd := t.TempDir(), t.TempDir()
				root := home
				if scope == "project" {
					root = cwd
				}
				root = filepath.Join(root, ".cursor", "skills")
				body := "---\ndescription: Nested\nargument-hint: <nested>\n---\n"
				body += strings.Repeat("x", size-len(body))
				writeFile(t, filepath.Join(root, "a", "b", "a-b", "SKILL.md"), body)
				cursorSkill(t, filepath.Join(root, "a"), "b", "ignored", "Parent")
				cursorSkill(t, filepath.Join(root, "z"), "b", "ignored", "Sibling")

				catalog := cursorCatalog(t, cwd, home)
				want := []Command{{"/z-b", "Sibling", scope, ""}}
				if size <= 1<<20 {
					want = append(want, Command{"/a-b", "Nested", scope, "<nested>"}, Command{"/a-b-2", "Parent", scope, ""})
				} else {
					want = append(want, Command{"/a-b", "Parent", scope, ""})
				}
				assertCursorCustomCommands(t, catalog, want...)
				if catalog.Truncated {
					t.Error("complete discovery marked truncated")
				}
			})
		}
	}
}
