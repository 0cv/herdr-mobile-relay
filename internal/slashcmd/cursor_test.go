package slashcmd

import (
	"os"
	"path/filepath"
	"testing"
)

// cursorSkill writes <dir>/<folder>/SKILL.md. The frontmatter name is
// deliberately allowed to differ from the folder so tests can pin which one
// names the command.
func cursorSkill(t *testing.T, dir, folder, frontmatterName, description string) {
	t.Helper()
	body := "---\nname: " + frontmatterName + "\n"
	if description != "" {
		body += "description: " + description + "\n"
	}
	body += "---\n\nbody\n"
	writeFile(t, filepath.Join(dir, folder, "SKILL.md"), body)
}

func cursorCatalog(t *testing.T, cwd, home string) Catalog {
	t.Helper()
	isolateAgentEnv(t)
	return CatalogFor("cursor", cwd, home)
}

// Cursor derives a skill's command id from its directory name, not from the
// frontmatter name. A skill whose frontmatter name disagrees with its folder
// must still be published under the folder name, because that is the id
// Cursor's own palette and parser resolve.
func TestCursorSkillCommandUsesFolderNameNotFrontmatter(t *testing.T) {
	home := t.TempDir()
	skillDir := filepath.Join(home, ".cursor", "skills")
	cursorSkill(t, skillDir, "pdf-forms", "pdf form toolkit", "Fill PDF forms")

	catalog := cursorCatalog(t, t.TempDir(), home)
	if containsCommand(catalog, "/pdf form toolkit") {
		t.Error("frontmatter name leaked into the palette; cursor resolves folder names")
	}
	if !containsCommand(catalog, "/pdf-forms") {
		t.Errorf("folder-named skill missing; catalog=%v", commandNames(catalog))
	}
}

// The shared ~/.agents/skills root is read alongside ~/.cursor/skills.
func TestCursorReadsAgentsSharedRoot(t *testing.T) {
	home := t.TempDir()
	cursorSkill(t, filepath.Join(home, ".agents", "skills"), "caveman", "caveman", "Shared skill")

	catalog := cursorCatalog(t, t.TempDir(), home)
	if !containsCommand(catalog, "/caveman") {
		t.Errorf("shared .agents root not scanned; catalog=%v", commandNames(catalog))
	}
}

// Cursor's own built-in skills root and its cloud/plugin trees are excluded:
// skills-cursor is reserved and managed by Cursor, and its own documentation
// tells users never to create skills there.
func TestCursorIgnoresReservedInternalRoots(t *testing.T) {
	home := t.TempDir()
	for _, root := range []string{
		filepath.Join(home, ".cursor", "skills-cursor"),
		filepath.Join(home, ".cursor", "cloud-skills"),
		filepath.Join(home, ".cursor", "plugins"),
	} {
		cursorSkill(t, root, "internal-tool", "internal-tool", "Internal")
	}

	catalog := cursorCatalog(t, t.TempDir(), home)
	if containsCommand(catalog, "/internal-tool") {
		t.Errorf("reserved internal root leaked into the palette; catalog=%v", commandNames(catalog))
	}
}

// A personal skill wins over a project skill of the same folder name, matching
// the Claude provider's documented precedence.
func TestCursorPersonalSkillOverridesProject(t *testing.T) {
	home := t.TempDir()
	cursorSkill(t, filepath.Join(home, ".cursor", "skills"), "review", "review", "Personal review")

	repo := t.TempDir()
	writeFile(t, filepath.Join(repo, ".git"), "gitdir: elsewhere")
	cursorSkill(t, filepath.Join(repo, ".cursor", "skills"), "review", "review", "Project review")

	catalog := cursorCatalog(t, repo, home)
	got := commandSource(catalog, "/review")
	if got != "personal" {
		t.Errorf("/review source = %q, want personal", got)
	}
}

// A project skill is still discovered when no personal skill shares its name.
func TestCursorDiscoversProjectSkill(t *testing.T) {
	repo := t.TempDir()
	writeFile(t, filepath.Join(repo, ".git"), "gitdir: elsewhere")
	cursorSkill(t, filepath.Join(repo, ".cursor", "skills"), "repo-only", "repo-only", "Project only")

	catalog := cursorCatalog(t, repo, t.TempDir())
	if got := commandSource(catalog, "/repo-only"); got != "project" {
		t.Errorf("/repo-only source = %q, want project", got)
	}
}

// A builtin must survive even when a personal skill shares its name, and must
// come first because builtins are applied before the skill roots.
func TestCursorBuiltinsSurviveSkillCollision(t *testing.T) {
	home := t.TempDir()
	cursorSkill(t, filepath.Join(home, ".cursor", "skills"), "help", "help", "A skill named help")

	catalog := cursorCatalog(t, t.TempDir(), home)
	if got := commandSource(catalog, "/help"); got != "personal" {
		t.Errorf("/help source = %q, want the personal skill to win", got)
	}
	if len(catalog.Commands) == 0 || catalog.Commands[0].Command != "/add-dir" {
		t.Errorf("builtins should still lead the catalog, got %v", commandNames(catalog)[:1])
	}
}

// Every published builtin is unconditional in Cursor. The debug-gated and
// developer-only entries it hides behind its debug flag must not appear, since
// the phone would otherwise offer commands the pane does not have.
func TestCursorOmitsDebugAndDeveloperOnlyCommands(t *testing.T) {
	home := t.TempDir()
	catalog := cursorCatalog(t, t.TempDir(), home)

	for _, name := range []string{
		"/debug-test", "/throw", "/pq", "/static-indicator",
		"/dev:classify-conversation", "/dev:score-commit", "/dev:score-recent-commits",
	} {
		if containsCommand(catalog, name) {
			t.Errorf("%s is not unconditionally available and must not be published", name)
		}
	}
	if len(cursorBuiltins) == 0 {
		t.Fatal("cursorBuiltins is empty")
	}
	for _, builtin := range cursorBuiltins {
		if !commandNamePattern.MatchString(builtin.Command[1:]) {
			t.Errorf("builtin %q is not a legal command name", builtin.Command)
		}
	}
}

// A symlinked skill directory must be followed, exactly as Cursor follows it.
// The generic scanner previously gated on DirEntry.IsDir(), which is false for
// a symlink, so a linked skill was silently dropped.
func TestCursorFollowsSymlinkedSkillDir(t *testing.T) {
	home := t.TempDir()
	target := filepath.Join(home, "checkouts", "1password-op-cli")
	cursorSkill(t, filepath.Dir(target), "1password-op-cli", "1password-op-cli", "Linked skill")

	links := filepath.Join(home, ".cursor", "skills")
	mkdirAll(t, links)
	if err := os.Symlink(target, filepath.Join(links, "1password-op-cli")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}

	catalog := cursorCatalog(t, t.TempDir(), home)
	if !containsCommand(catalog, "/1password-op-cli") {
		t.Errorf("symlinked skill dir dropped; catalog=%v", commandNames(catalog))
	}
}

// A skill marked user-invocable: false stays out of the palette but is not
// allowed to break discovery of its siblings.
func TestCursorRespectsUserInvocableFalse(t *testing.T) {
	home := t.TempDir()
	skillDir := filepath.Join(home, ".cursor", "skills")
	writeFile(t, filepath.Join(skillDir, "hidden", "SKILL.md"),
		"---\nname: hidden\ndescription: Hidden\nuser-invocable: false\n---\n\nbody\n")
	cursorSkill(t, skillDir, "visible", "visible", "Visible")

	catalog := cursorCatalog(t, t.TempDir(), home)
	if containsCommand(catalog, "/hidden") {
		t.Error("user-invocable: false skill was published")
	}
	if !containsCommand(catalog, "/visible") {
		t.Error("sibling skill lost when a non-invocable skill was present")
	}
}

// Configured agent-profiles.ini skill dirs remain an escape hatch for cursor.
func TestCursorHonorsConfiguredSkillDirs(t *testing.T) {
	home := t.TempDir()
	extra := filepath.Join(home, "extra-skills")
	cursorSkill(t, extra, "custom-command", "custom-command", "Extra")

	catalog := CatalogForProfile(
		"cursor", "cursor", t.TempDir(), home,
		[]string{extra}, "/{name}", "", "",
	)
	if !containsCommand(catalog, "/custom-command") {
		t.Errorf("configured skill dir ignored; catalog=%v", commandNames(catalog))
	}
}

// SuppressNative (herdr_commands "off") yields builtins only, with no skills.
func TestCursorSuppressNativeYieldsBuiltinsOnly(t *testing.T) {
	home := t.TempDir()
	cursorSkill(t, filepath.Join(home, ".cursor", "skills"), "someskill", "someskill", "Skill")

	catalog := CatalogForProfileWithSuppression(
		"cursor", "cursor", t.TempDir(), home,
		nil, "/{name}", "", "", true,
	)
	if len(catalog.Commands) != len(cursorBuiltins) {
		t.Errorf("suppressed catalog has %d commands, want %d builtins",
			len(catalog.Commands), len(cursorBuiltins))
	}
	if containsCommand(catalog, "/someskill") {
		t.Error("suppressed catalog must not scan skills")
	}
}

// Every reported agent spelling resolves to the cursor provider.
func TestCursorAgentNameAliases(t *testing.T) {
	for _, name := range []string{"cursor", "Cursor", "cursor-agent", "cursor agent"} {
		if got := profileIDForAgentName(name); got != "cursor" {
			t.Errorf("profileIDForAgentName(%q) = %q, want cursor", name, got)
		}
	}
}

func commandNames(catalog Catalog) []string { return commandNamesIn(catalog.Commands) }

func commandNamesIn(commands []Command) []string {
	names := make([]string, 0, len(commands))
	for _, command := range commands {
		names = append(names, command.Command)
	}
	return names
}

func containsCommand(catalog Catalog, name string) bool {
	return commandInList(catalog.Commands, name)
}

func commandInList(commands []Command, name string) bool {
	for _, command := range commands {
		if command.Command == name {
			return true
		}
	}
	return false
}

func commandSource(catalog Catalog, name string) string {
	for _, command := range catalog.Commands {
		if command.Command == name {
			return command.Source
		}
	}
	return ""
}
