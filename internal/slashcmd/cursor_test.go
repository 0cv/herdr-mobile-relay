package slashcmd

import (
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"
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

// Cursor builds a fresh id set inside each loadSkillsFromDirectory call, so a
// duplicate folder name across two roots is NOT suffixed: the later root simply
// overwrites the earlier id in the skills map. loadSkillRoots passes the
// workspace roots first, so a personal skill of the same folder name replaces
// the project one and only a single /review exists.
func TestCursorSkillNameCollisionAcrossRootsReplaces(t *testing.T) {
	home := t.TempDir()
	writeFile(t, filepath.Join(home, ".cursor", "skills", "review", "SKILL.md"),
		"---\nname: review\ndescription: Personal review\nargument-hint: <personal>\n---\n")

	repo := t.TempDir()
	writeFile(t, filepath.Join(repo, ".git"), "gitdir: elsewhere")
	writeFile(t, filepath.Join(repo, ".cursor", "skills", "review", "SKILL.md"),
		"---\nname: review\ndescription: Project review\nargument-hint: <project>\n---\n")

	catalog := cursorCatalog(t, repo, home)
	if !containsCommand(catalog, "/review") {
		t.Errorf("collision dropped the skill entirely; catalog=%v", commandNames(catalog))
	}
	if containsCommand(catalog, "/review-2") {
		t.Errorf("cross-root duplicates must not be suffixed; catalog=%v", commandNames(catalog))
	}
	for _, command := range catalog.Commands {
		if command.Command == "/review" {
			want := Command{"/review", "Personal review", "personal", "<personal>"}
			if command != want {
				t.Errorf("/review = %+v, want %+v", command, want)
			}
		}
	}
}

// A folder name repeated WITHIN one root is what makes Cursor switch to a
// root-relative id: getRelativeSkillId joins the path below the root with "-",
// so skills/tools/review/SKILL.md and skills/other/review/SKILL.md resolve as
// /tools-review and /other-review rather than colliding on /review.
func TestCursorDuplicateFolderNamesWithinRootUseRelativeIDs(t *testing.T) {
	home := t.TempDir()
	root := filepath.Join(home, ".cursor", "skills")
	cursorSkill(t, filepath.Join(root, "tools"), "review", "review", "Tools review")
	cursorSkill(t, filepath.Join(root, "other"), "review", "review", "Other review")

	catalog := cursorCatalog(t, t.TempDir(), home)
	for _, name := range []string{"/tools-review", "/other-review"} {
		if !containsCommand(catalog, name) {
			t.Errorf("%s missing; catalog=%v", name, commandNames(catalog))
		}
	}
	if containsCommand(catalog, "/review") {
		t.Errorf("duplicated folder name should not keep a bare id; catalog=%v", commandNames(catalog))
	}
}

// Cursor's findSkillMarkdownFiles recurses, so a skill nested below the root is
// discovered under its own folder name. A scanner that only reads
// <root>/<entry>/SKILL.md silently drops it.
func TestCursorDiscoversNestedSkill(t *testing.T) {
	home := t.TempDir()
	root := filepath.Join(home, ".cursor", "skills")
	cursorSkill(t, filepath.Join(root, "tools"), "deep-tool", "deep-tool", "Nested skill")

	catalog := cursorCatalog(t, t.TempDir(), home)
	if !containsCommand(catalog, "/deep-tool") {
		t.Errorf("nested skill omitted; catalog=%v", commandNames(catalog))
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

// A user command file wins over a workspace file of the same name: Cursor
// loads the workspace roots first and the user roots last, and
// loadCommandsFromDirectory assigns each id unconditionally.
func TestCursorUserCommandBeatsWorkspaceCommand(t *testing.T) {
	home := t.TempDir()
	repo := t.TempDir()
	writeFile(t, filepath.Join(repo, ".git"), "gitdir: elsewhere")
	writeFile(t, filepath.Join(repo, ".cursor", "commands", "deploy.md"), "# Repo deploy\n")
	writeFile(t, filepath.Join(home, ".cursor", "commands", "deploy.md"), "# Personal deploy\n")

	catalog := cursorCatalog(t, repo, home)
	if got := commandSource(catalog, "/deploy"); got != "personal" {
		t.Errorf("/deploy source = %q, want personal to win the collision", got)
	}
	if containsCommand(catalog, "/project-deploy") {
		t.Error("command files are not prefixed by scope")
	}
}

// Commands live as flat *.md files named after the command, in both the .cursor
// and .claude trees Cursor reads, at workspace and user scope.
func TestCursorReadsCommandFiles(t *testing.T) {
	home := t.TempDir()
	repo := t.TempDir()
	writeFile(t, filepath.Join(repo, ".git"), "gitdir: elsewhere")
	writeFile(t, filepath.Join(repo, ".cursor", "commands", "ship.md"), "# Ship it\n")
	writeFile(t, filepath.Join(repo, ".claude", "commands", "audit.md"), "# Audit it\n")
	writeFile(t, filepath.Join(home, ".cursor", "commands", "personal.md"), "# Personal\n")
	writeFile(t, filepath.Join(home, ".claude", "commands", "claudeuser.md"), "# Claude user\n")

	catalog := cursorCatalog(t, repo, home)
	for _, name := range []string{"/ship", "/audit", "/personal", "/claudeuser"} {
		if !containsCommand(catalog, name) {
			t.Errorf("%s missing; catalog=%v", name, commandNames(catalog))
		}
	}
}

// A builtin keeps its reserved name and a colliding user file is dropped rather
// than suffixed. Cursor resolves builtins from its own registry and registers
// markdown commands under their unchanged filename stem, so nothing resolves a
// fabricated /clear-2 - publishing it would offer the phone a command that
// cannot invoke the file.
func TestCursorUserFileCannotTakeBuiltinName(t *testing.T) {
	home := t.TempDir()
	writeFile(t, filepath.Join(home, ".cursor", "commands", "clear.md"), "# My clear\n")

	catalog := cursorCatalog(t, t.TempDir(), home)
	if !containsCommand(catalog, "/clear") {
		t.Errorf("/clear missing; catalog=%v", commandNames(catalog))
	}
	if got := commandSource(catalog, "/clear"); got != "builtin" {
		t.Errorf("/clear source = %q, want the builtin to keep its reserved name", got)
	}
	if containsCommand(catalog, "/clear-2") {
		t.Errorf("a suffixed alias is not resolvable in Cursor and must not be published; catalog=%v", commandNames(catalog))
	}
}

// A skill named after a builtin is likewise dropped, leaving the builtin alone.
func TestCursorBuiltinSurvivesSkillCollision(t *testing.T) {
	home := t.TempDir()
	cursorSkill(t, filepath.Join(home, ".cursor", "skills"), "help", "help", "A skill named help")

	catalog := cursorCatalog(t, t.TempDir(), home)
	if got := commandSource(catalog, "/help"); got != "builtin" {
		t.Errorf("/help source = %q, want the builtin to keep the name", got)
	}
	if containsCommand(catalog, "/help-2") {
		t.Errorf("skill alias must not be invented; catalog=%v", commandNames(catalog))
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

// A non-regular *.md entry is skipped rather than read: fileFrontmatter would
// block forever on a FIFO with no writer, in a service that polls.
func TestCursorSkipsNonRegularCommandFile(t *testing.T) {
	home := t.TempDir()
	dir := filepath.Join(home, ".cursor", "commands")
	mkdirAll(t, dir)
	writeFile(t, filepath.Join(dir, "real.md"), "# Real\n")
	pipe := filepath.Join(dir, "pipe.md")
	if err := syscall.Mkfifo(pipe, 0o600); err != nil {
		t.Skipf("fifo unavailable: %v", err)
	}
	if err := os.Symlink(pipe, filepath.Join(dir, "linked-pipe.md")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}

	done := make(chan Catalog, 1)
	go func() { done <- cursorCatalog(t, t.TempDir(), home) }()
	select {
	case catalog := <-done:
		if !containsCommand(catalog, "/real") {
			t.Errorf("real command missing; catalog=%v", commandNames(catalog))
		}
		for _, command := range []string{"/pipe", "/linked-pipe"} {
			if containsCommand(catalog, command) {
				t.Errorf("a fifo must not become a command: %s", command)
			}
		}
	case <-time.After(10 * time.Second):
		t.Fatal("discovery hung on a fifo; non-regular files must be skipped")
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

func TestCursorNestedProjectSkillMetadata(t *testing.T) {
	for _, stem := range []string{".cursor", ".agents"} {
		t.Run(stem, func(t *testing.T) {
			repo := t.TempDir()
			writeFile(t, filepath.Join(repo, ".git"), "gitdir: elsewhere")
			root := filepath.Join(repo, stem, "skills")
			for _, skill := range []struct {
				path, description, hint string
			}{
				{"review", "Top-level review", "<top>"},
				{"tools/review", "Nested review", "<nested>"},
				{"tools/unique", "Unique nested skill", "<unique>"},
			} {
				writeFile(t, filepath.Join(root, skill.path, "SKILL.md"),
					"---\nname: ignored\ndescription: "+skill.description+"\nargument-hint: "+skill.hint+"\n---\n")
			}
			catalog := cursorCatalog(t, repo, t.TempDir())
			for _, want := range []Command{
				{"/review", "Top-level review", "project", "<top>"},
				{"/tools-review", "Nested review", "project", "<nested>"},
				{"/unique", "Unique nested skill", "project", "<unique>"},
			} {
				if !containsCommand(catalog, want.Command) {
					t.Errorf("missing %s", want.Command)
				}
				for _, got := range catalog.Commands {
					if got.Command == want.Command && got != want {
						t.Errorf("got %+v, want %+v", got, want)
					}
				}
			}
		})
	}
}

func TestCursorNestedProjectSkillSymlinkBoundary(t *testing.T) {
	for _, linkFile := range []bool{false, true} {
		for _, location := range []string{"skill-root", "repository", "outside"} {
			name := "directory-" + location
			if linkFile {
				name = "file-" + location
			}
			t.Run(name, func(t *testing.T) {
				repo := t.TempDir()
				writeFile(t, filepath.Join(repo, ".git"), "gitdir: elsewhere")
				targetRoot := filepath.Join(repo, ".cursor", "skills", "z-targets")
				switch location {
				case "repository":
					targetRoot = repo
				case "outside":
					targetRoot = t.TempDir()
				}
				cursorSkill(t, targetRoot, "shared", "ignored", "Linked metadata")
				target := filepath.Join(targetRoot, "shared")
				link := filepath.Join(repo, ".cursor", "skills", "tools", "linked")
				if linkFile {
					target = filepath.Join(target, "SKILL.md")
					link = filepath.Join(link, "SKILL.md")
				}
				mkdirAll(t, filepath.Dir(link))
				relativeTarget, err := filepath.Rel(filepath.Dir(link), target)
				if err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(relativeTarget, link); err != nil {
					t.Skipf("symlinks unavailable: %v", err)
				}
				catalog := cursorCatalog(t, repo, t.TempDir())
				if got, want := containsCommand(catalog, "/linked"), location == "skill-root"; got != want {
					t.Errorf("/linked present = %v, want %v", got, want)
				}
				for _, command := range catalog.Commands {
					if command.Command == "/linked" && (command.Description != "Linked metadata" || command.Source != "project") {
						t.Errorf("unexpected linked metadata: %+v", command)
					}
				}
			})
		}
	}
}

func TestCursorSkipsDependencyAndBuildDirectories(t *testing.T) {
	for _, ignored := range []string{"node_modules", "__pycache__", "dist", "build"} {
		t.Run(ignored, func(t *testing.T) {
			home := t.TempDir()
			root := filepath.Join(home, ".cursor", "skills")
			cursorSkill(t, filepath.Join(root, "tools"), "review", "review", "Real review")
			cursorSkill(t, filepath.Join(root, ignored), "review", "review", "Ignored review")
			cursorSkill(t, root, ignored, ignored, "Ignored skill")

			catalog := cursorCatalog(t, t.TempDir(), home)
			if !containsCommand(catalog, "/review") {
				t.Errorf("ignored directory changed skill ID; catalog=%v", commandNames(catalog))
			}
			for _, command := range catalog.Commands {
				if command.Source != "builtin" && command != (Command{"/review", "Real review", "personal", ""}) {
					t.Errorf("unexpected command: %+v", command)
				}
			}
		})
	}
}

func TestCursorDeduplicatesSkillRealpathsAcrossRoots(t *testing.T) {
	for _, duplicateName := range []bool{false, true} {
		name := "alias"
		if duplicateName {
			name = "alias-with-distinct-sibling"
		}
		t.Run(name, func(t *testing.T) {
			home := t.TempDir()
			cursorRoot := filepath.Join(home, ".cursor", "skills")
			agentsRoot := filepath.Join(home, ".agents", "skills")
			cursorSkill(t, cursorRoot, "original", "ignored", "Original skill")
			link := filepath.Join(agentsRoot, "tools", "alias")
			mkdirAll(t, filepath.Dir(link))
			if err := os.Symlink(filepath.Join(cursorRoot, "original"), link); err != nil {
				t.Skipf("symlinks unavailable: %v", err)
			}
			if duplicateName {
				cursorSkill(t, filepath.Join(agentsRoot, "other"), "alias", "ignored", "Distinct skill")
			}

			catalog := cursorCatalog(t, t.TempDir(), home)
			if !containsCommand(catalog, "/original") {
				t.Error("original skill missing")
			}
			if got := containsCommand(catalog, "/alias"); got != duplicateName {
				t.Errorf("/alias present = %v, want %v", got, duplicateName)
			}
			for _, command := range catalog.Commands {
				if command.Source == "builtin" || command.Command == "/original" {
					continue
				}
				if !duplicateName || command != (Command{"/alias", "Distinct skill", "personal", ""}) {
					t.Errorf("unexpected command: %+v", command)
				}
			}
		})
	}
}

func TestCursorFollowsRegularCommandSymlink(t *testing.T) {
	for _, stem := range []string{".cursor", ".claude"} {
		t.Run(stem, func(t *testing.T) {
			home := t.TempDir()
			target := filepath.Join(home, "dotfiles", "review.md")
			writeFile(t, target, "---\ndescription: Linked review\n---\nReview the code\n")
			link := filepath.Join(home, stem, "commands", "linked-review.md")
			mkdirAll(t, filepath.Dir(link))
			if err := os.Symlink(target, link); err != nil {
				t.Skipf("symlinks unavailable: %v", err)
			}
			if err := os.Symlink(filepath.Join(home, "missing"), filepath.Join(filepath.Dir(link), "broken.md")); err != nil {
				t.Fatal(err)
			}

			catalog := cursorCatalog(t, t.TempDir(), home)
			if !containsCommand(catalog, "/linked-review") {
				t.Error("symlinked command missing")
			}
			if containsCommand(catalog, "/broken") {
				t.Error("broken symlink became a command")
			}
		})
	}
}

func TestCursorSkillSurfaces(t *testing.T) {
	cases := []struct {
		name, metadata string
		visible        bool
	}{
		{"unset", "", true},
		{"ide-list", "metadata:\n  surfaces: [ide]\n", false},
		{"cli-list", "metadata:\n  surfaces: [ide, cli]\n", true},
		{"ide-block-list", "metadata:\n  surfaces:\n    - ide\n    - cloud\n", false},
		{"cli-block-list", "metadata:\n  surfaces:\n    - ide\n    - cli\n", true},
		{"indentless-list", "metadata:\n  surfaces:\n  - ide\n", false},
		{"ide-scalar", "metadata:\n  surfaces: ide, cloud\n", false},
		{"cli-scalar", "metadata:\n  surfaces: 'ide, cli'\n", true},
		{"folded-scalar", "metadata:\n  surfaces: >-\n    ide,\n    cloud\n", false},
		{"inline-map", "metadata: {surfaces: [ide]}\n", false},
		{"inline-map-cli", "metadata: {surfaces: [cli]}\n", true},
		{"case-sensitive", "metadata: {surfaces: [CLI]}\n", false},
		{"list-preserves-whitespace", "metadata: {surfaces: [' cli ']}\n", false},
		{"alias", "shared: &surfaces [ide]\nmetadata: {surfaces: *surfaces}\n", false},
		{"metadata-null", "metadata: null\n", true},
		{"metadata-scalar", "metadata: unrelated\n", true},
		{"empty-list", "metadata:\n  surfaces: []\n", true},
		{"empty-scalar", "metadata:\n  surfaces: ''\n", true},
		{"null", "metadata:\n  surfaces: null\n", true},
		{"nonstring-list", "metadata:\n  surfaces: [42, false]\n", true},
		{"mixed-list", "metadata:\n  surfaces: [42, ide]\n", false},
		{"unrelated-root-key", "surfaces: [ide]\n", true},
		{"unrelated-nested-key", "metadata:\n  other:\n    surfaces: [ide]\n", true},
	}
	for _, tc := range cases {
		for _, project := range []bool{false, true} {
			scope := "personal"
			if project {
				scope = "project"
			}
			t.Run(tc.name+"-"+scope, func(t *testing.T) {
				home, repo := t.TempDir(), t.TempDir()
				writeFile(t, filepath.Join(repo, ".git"), "gitdir: elsewhere")
				root := home
				if project {
					root = repo
				}
				root = filepath.Join(root, ".cursor", "skills")
				writeFile(t, filepath.Join(root, "surface-test", "SKILL.md"),
					"---\nname: ignored\ndescription: Surface test\n"+tc.metadata+"---\nBody\n")
				cursorSkill(t, root, "sibling", "sibling", "Sibling skill")
				catalog := cursorCatalog(t, repo, home)
				if got := containsCommand(catalog, "/surface-test"); got != tc.visible {
					t.Errorf("skill present = %v, want %v", got, tc.visible)
				}
				if !containsCommand(catalog, "/sibling") {
					t.Error("surface filtering removed sibling")
				}
			})
		}
	}
}

func TestCursorProjectOutsideLinkDoesNotChangeSkillID(t *testing.T) {
	for _, linkFile := range []bool{false, true} {
		name := "directory"
		if linkFile {
			name = "file"
		}
		t.Run(name, func(t *testing.T) {
			repo := t.TempDir()
			writeFile(t, filepath.Join(repo, ".git"), "gitdir: elsewhere")
			root := filepath.Join(repo, ".cursor", "skills")
			cursorSkill(t, filepath.Join(root, "tools"), "review", "review", "Real review")
			cursorSkill(t, repo, "review", "review", "Outside review")
			target := filepath.Join(repo, "review")
			link := filepath.Join(root, "other", "review")
			if linkFile {
				target = filepath.Join(target, "SKILL.md")
				link = filepath.Join(link, "SKILL.md")
			}
			mkdirAll(t, filepath.Dir(link))
			relativeTarget, err := filepath.Rel(filepath.Dir(link), target)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(relativeTarget, link); err != nil {
				t.Skipf("symlinks unavailable: %v", err)
			}

			catalog := cursorCatalog(t, repo, t.TempDir())
			if !containsCommand(catalog, "/review") {
				t.Error("outside link changed the valid skill ID")
			}
			for _, command := range catalog.Commands {
				if command.Source != "builtin" && command != (Command{"/review", "Real review", "project", ""}) {
					t.Errorf("unexpected command: %+v", command)
				}
			}
		})
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
