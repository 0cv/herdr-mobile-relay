package slashcmd

import (
	"path/filepath"
	"strings"
	"testing"
)

func TestCursorBuiltinAliasesCannotBecomeCustomCommands(t *testing.T) {
	aliases := map[string][]string{
		"/about":          {"/whoami", "/account"},
		"/auto-review":    {"/smart-auto"},
		"/clear":          {"/new", "/new-chat", "/newchat"},
		"/command":        {"/commands"},
		"/config":         {"/settings", "/preferences", "/cli-config"},
		"/copy":           {"/clipboard", "/paste"},
		"/fork":           {"/duplicate", "/clone", "/branch"},
		"/line-numbers":   {"/lines", "/numbers"},
		"/open":           {"/cursor"},
		"/rename":         {"/name", "/title"},
		"/resume":         {"/continue", "/recent", "/history"},
		"/rewind":         {"/restore", "/undo"},
		"/run-everything": {"/auto-run"},
		"/shell":          {"/sh", "/run"},
		"/show-thinking":  {"/thoughts", "/thinking", "/thinking-blocks"},
		"/summarize":      {"/compress", "/compact"},
	}
	for _, scope := range []string{"project", "personal", "configured"} {
		for _, kind := range []string{"command", "skill"} {
			if scope == "configured" && kind == "command" {
				continue
			}
			for _, uppercase := range []bool{false, true} {
				name := scope + "/" + kind
				if uppercase {
					name += "/uppercase"
				}
				t.Run(name, func(t *testing.T) {
					home, cwd, extra := t.TempDir(), t.TempDir(), t.TempDir()
					root := home
					if scope == "project" {
						root = cwd
					}
					skillRoot := filepath.Join(root, ".cursor", "skills")
					var skillDirs []string
					if scope == "configured" {
						skillRoot = extra
						skillDirs = []string{extra}
					}
					for _, names := range aliases {
						for _, alias := range names {
							name := strings.TrimPrefix(alias, "/")
							if uppercase {
								name = strings.ToUpper(name)
							}
							if kind == "command" {
								writeFile(t, filepath.Join(root, ".cursor", "commands", name+".md"), "Custom command\n")
								continue
							}
							cursorSkill(t, skillRoot, name, name, "Custom skill")
						}
					}
					catalog := CatalogForProfile("cursor", "cursor", cwd, home, skillDirs, "/{name}", "", "")
					assertCursorCustomCommands(t, catalog)
					for canonical := range aliases {
						if commandSource(catalog, canonical) != "builtin" {
							t.Errorf("canonical builtin %s is missing", canonical)
						}
					}
				})
			}
		}
	}
}

func TestCursorAdditionalUnconditionalBuiltins(t *testing.T) {
	home, cwd := t.TempDir(), t.TempDir()
	for _, name := range []string{"about", "shell", "sandbox", "open", "cursor", "update", "run-everything", "auto-review"} {
		writeFile(t, filepath.Join(home, ".cursor", "commands", name+".md"), "Custom command\n")
		cursorSkill(t, filepath.Join(cwd, ".cursor", "skills"), name, name, "Custom skill")
	}
	catalog := cursorCatalog(t, cwd, home)
	assertCursorCustomCommands(t, catalog)
	for _, name := range []string{"/about", "/shell", "/sandbox", "/open", "/cursor", "/update", "/run-everything", "/auto-review"} {
		if commandSource(catalog, name) != "builtin" {
			t.Errorf("unconditional builtin %s is missing", name)
		}
	}
}

func TestCursorOmitsPromptQualityBuiltin(t *testing.T) {
	for _, suppressed := range []bool{false, true} {
		home, cwd := t.TempDir(), t.TempDir()
		catalog := CatalogForProfileWithSuppression("cursor", "cursor", cwd, home, nil, "", "", "", suppressed)
		if containsCommand(catalog, "/open-in-prompt-quality") {
			t.Errorf("development-only builtin published with suppression=%v", suppressed)
		}
	}
}

func TestCursorPromptQualityNameIsNotReserved(t *testing.T) {
	home := t.TempDir()
	writeFile(t, filepath.Join(home, ".cursor", "commands", "open-in-prompt-quality.md"), "User command\n")
	catalog := cursorCatalog(t, t.TempDir(), home)
	assertCursorCustomCommands(t, catalog, Command{"/open-in-prompt-quality", "User command", "personal", ""})
}
