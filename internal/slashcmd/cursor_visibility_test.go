package slashcmd

import (
	"fmt"
	"path/filepath"
	"strings"
	"testing"
)

var cursorConditionalBuiltinNames = []string{
	"detach", "goal", "max-mode", "usage", "zen-mode", "zen",
}

func TestCursorOmitsConditionalBuiltins(t *testing.T) {
	for _, suppressed := range []bool{false, true} {
		t.Run(fmt.Sprintf("suppressed=%v", suppressed), func(t *testing.T) {
			t.Setenv("CURSOR_AGENT_PERSIST_SESSION", "relay-session")
			catalog := CatalogForProfileWithSuppression("cursor", "cursor", t.TempDir(), t.TempDir(), nil, "", "", "", suppressed)
			for _, name := range cursorConditionalBuiltinNames {
				if containsCommand(catalog, "/"+name) {
					t.Errorf("conditional builtin /%s was published", name)
				}
			}
		})
	}
}

func TestCursorConditionalBuiltinNamesAreNotReserved(t *testing.T) {
	for _, scope := range []string{"project", "personal", "configured"} {
		for _, kind := range []string{"command", "skill"} {
			if scope == "configured" && kind == "command" {
				continue
			}
			t.Run(scope+"/"+kind, func(t *testing.T) {
				home, cwd, extra := t.TempDir(), t.TempDir(), t.TempDir()
				root, source := home, scope
				if scope == "project" {
					root = cwd
				}
				skillRoot := filepath.Join(root, ".cursor", "skills")
				var skillDirs []string
				if scope == "configured" {
					skillRoot, source = extra, "personal"
					skillDirs = []string{extra}
				}
				var want []Command
				for _, name := range cursorConditionalBuiltinNames {
					if kind == "command" {
						writeFile(t, filepath.Join(root, ".cursor", "commands", name+".md"), "Custom command\n")
					} else {
						cursorSkill(t, skillRoot, name, name, "Custom command")
					}
					want = append(want, Command{"/" + name, "Custom command", source, ""})
				}
				catalog := CatalogForProfile("cursor", "cursor", cwd, home, skillDirs, "/{name}", "", "")
				assertCursorCustomCommands(t, catalog, want...)
			})
		}
	}
}

func TestCursorSkillFrontmatterMarkers(t *testing.T) {
	for _, marker := range []string{"---", "---yaml", "--- yml", "--- YAML", "\ufeff---", "\ufeff---yaml"} {
		for _, newline := range []string{"\n", "\r\n"} {
			for _, scope := range []string{"project", "personal"} {
				t.Run(fmt.Sprintf("%q/%q/%s", marker, newline, scope), func(t *testing.T) {
					home, cwd := t.TempDir(), t.TempDir()
					root := home
					if scope == "project" {
						root = cwd
					}
					root = filepath.Join(root, ".cursor", "skills")
					for _, skill := range []struct {
						name, fields string
					}{
						{"visible", "metadata: {surfaces: [cli]}"},
						{"hidden", "user-invocable: false"},
						{"ide-only", "metadata: {surfaces: [ide]}"},
					} {
						body := marker + "\ndescription: Skill description\nargument-hint: <path>\n" + skill.fields + "\n---\nBody\n"
						writeFile(t, filepath.Join(root, skill.name, "SKILL.md"), strings.ReplaceAll(body, "\n", newline))
					}
					catalog := cursorCatalog(t, cwd, home)
					assertCursorCustomCommands(t, catalog, Command{"/visible", "Skill description", scope, "<path>"})
				})
			}
		}
	}
}

func TestCursorSkillUserInvocableValues(t *testing.T) {
	for _, scope := range []string{"project", "personal"} {
		for _, tc := range []struct {
			value   string
			visible bool
		}{
			{"false", false},
			{"'false'", false},
			{"' FALSE '", false},
			{"true", true},
			{"no", true},
			{"off", true},
			{"0", true},
			{"'0'", true},
			{"' NO '", true},
			{"' Off '", true},
			{"null", true},
		} {
			t.Run(scope+"/"+tc.value, func(t *testing.T) {
				home, cwd := t.TempDir(), t.TempDir()
				root := home
				if scope == "project" {
					root = cwd
				}
				writeFile(t, filepath.Join(root, ".cursor", "skills", "example", "SKILL.md"),
					"---\ndescription: Example\nuser-invocable: "+tc.value+"\n---\nBody\n")
				catalog := cursorCatalog(t, cwd, home)
				var want []Command
				if tc.visible {
					want = append(want, Command{"/example", "Example", scope, ""})
				}
				assertCursorCustomCommands(t, catalog, want...)
			})
		}
	}
}
