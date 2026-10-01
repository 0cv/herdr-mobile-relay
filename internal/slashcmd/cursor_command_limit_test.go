package slashcmd

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCursorCommandFileSizeLimit(t *testing.T) {
	for _, size := range []int{0, 1024, 64*1024 + 1, 1 << 20, 1<<20 + 1} {
		for _, linked := range []bool{false, true} {
			t.Run(fmt.Sprintf("size=%d/linked=%v", size, linked), func(t *testing.T) {
				home := t.TempDir()
				dir := filepath.Join(home, ".cursor", "commands")
				path := filepath.Join(dir, "review.md")
				target := path
				if linked {
					target = filepath.Join(home, "dotfiles", "review.md")
				}
				body := ""
				if size > 0 {
					body = "---\ndescription: Command review\nargument-hint: <path>\n---\n"
					body += strings.Repeat("x", size-len(body))
				}
				writeFile(t, target, body)
				if linked {
					mkdirAll(t, dir)
					if err := os.Symlink(target, path); err != nil {
						t.Skipf("symlinks unavailable: %v", err)
					}
				}
				eligible := size > 0 && size <= 1<<20
				data, ok := readCursorCommandFile(path)
				if ok != eligible || ok && len(data) != size {
					t.Errorf("read returned %d bytes, ok=%v; want size=%d, ok=%v", len(data), ok, size, eligible)
				}
				writeFile(t, filepath.Join(dir, "sibling.md"), "Sibling command\n")
				catalog := cursorCatalog(t, t.TempDir(), home)
				want := []Command{{"/sibling", "Sibling command", "personal", ""}}
				if eligible {
					want = append(want, Command{"/review", "Command review", "personal", "<path>"})
				}
				assertCursorCustomCommands(t, catalog, want...)
				if catalog.Truncated {
					t.Error("skipped oversized command marked the catalog truncated")
				}
			})
		}
	}
}

func TestCursorOversizedCommandDoesNotOverride(t *testing.T) {
	for _, fallback := range []string{"command", "skill"} {
		t.Run(fallback, func(t *testing.T) {
			home, cwd := t.TempDir(), t.TempDir()
			writeFile(t, filepath.Join(home, ".cursor", "commands", "review.md"), strings.Repeat("x", 1<<20+1))
			if fallback == "command" {
				writeFile(t, filepath.Join(cwd, ".cursor", "commands", "review.md"), "Project review\n")
			} else {
				cursorSkill(t, filepath.Join(cwd, ".cursor", "skills"), "review", "review", "Project review")
			}
			catalog := cursorCatalog(t, cwd, home)
			assertCursorCustomCommands(t, catalog, Command{"/review", "Project review", "project", ""})
		})
	}
}
