package slashcmd

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// ompFixture builds an isolated home + repo pair for omp discovery tests.
type ompFixture struct {
	home string
	repo string
}

func newOMPFixture(t *testing.T) ompFixture {
	t.Helper()
	isolateAgentEnv(t)
	root := t.TempDir()
	fixture := ompFixture{
		home: filepath.Join(root, "home"),
		repo: filepath.Join(root, "repo"),
	}
	mkdirAll(t, filepath.Join(fixture.home, ".omp", "agent"))
	mkdirAll(t, fixture.repo)
	return fixture
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	mkdirAll(t, filepath.Dir(path))
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

// writeSkill creates <dir>/<name>/SKILL.md with frontmatter.
func writeSkill(t *testing.T, dir, name, description string) {
	t.Helper()
	body := "---\nname: " + name + "\n"
	if description != "" {
		body += "description: " + description + "\n"
	}
	body += "---\n\nbody\n"
	writeFile(t, filepath.Join(dir, name, "SKILL.md"), body)
}

func (f ompFixture) gitRepo(t *testing.T) {
	t.Helper()
	mkdirAll(t, filepath.Join(f.repo, ".git"))
}

func (f ompFixture) catalog(cwd string) Catalog {
	return CatalogForProfile("omp", "omp", cwd, f.home, nil, "", "18.0.3", "")
}

func commandByName(catalog Catalog, name string) (Command, bool) {
	for _, command := range catalog.Commands {
		if command.Command == name {
			return command, true
		}
	}
	return Command{}, false
}

func countSource(catalog Catalog, source string) int {
	total := 0
	for _, command := range catalog.Commands {
		if command.Source == source {
			total++
		}
	}
	return total
}

func TestOMPDiscoversProjectOMPSkills(t *testing.T) {
	f := newOMPFixture(t)
	f.gitRepo(t)
	writeSkill(t, filepath.Join(f.repo, ".omp", "skills"), "deploy", "Ship the service")

	catalog := f.catalog(f.repo)
	command, ok := commandByName(catalog, "/skill:deploy")
	if !ok {
		t.Fatalf("/skill:deploy missing from %d commands", len(catalog.Commands))
	}
	if command.Source != "project" {
		t.Errorf("source = %q, want project", command.Source)
	}
	if command.Description != "Ship the service" {
		t.Errorf("description = %q", command.Description)
	}
}

func TestOMPDiscoversProjectSkillsFromSubdirectory(t *testing.T) {
	f := newOMPFixture(t)
	f.gitRepo(t)
	writeSkill(t, filepath.Join(f.repo, ".omp", "skills"), "deploy", "Ship the service")
	nested := filepath.Join(f.repo, "services", "api")
	mkdirAll(t, nested)

	if _, ok := commandByName(f.catalog(nested), "/skill:deploy"); !ok {
		t.Fatal("walk-up from a subdirectory did not reach the git root's .omp/skills")
	}
}

func TestOMPDiscoversClaudeProjectSkills(t *testing.T) {
	f := newOMPFixture(t)
	f.gitRepo(t)
	writeSkill(t, filepath.Join(f.repo, ".claude", "skills"), "review", "Review a diff")

	command, ok := commandByName(f.catalog(f.repo), "/skill:review")
	if !ok {
		t.Fatal("omp must discover .claude/skills")
	}
	if command.Source != "project" {
		t.Errorf("source = %q, want project", command.Source)
	}
}

func TestOMPHonoursEnableClaudeProjectFalse(t *testing.T) {
	f := newOMPFixture(t)
	f.gitRepo(t)
	writeSkill(t, filepath.Join(f.repo, ".omp", "skills"), "deploy", "Ship the service")
	writeSkill(t, filepath.Join(f.repo, ".claude", "skills"), "review", "Review a diff")
	writeFile(t, filepath.Join(f.home, ".omp", "agent", "config.yml"),
		"skills:\n  enableClaudeProject: false\n")

	catalog := f.catalog(f.repo)
	if _, ok := commandByName(catalog, "/skill:review"); ok {
		t.Error("enableClaudeProject: false must hide .claude/skills")
	}
	if _, ok := commandByName(catalog, "/skill:deploy"); !ok {
		t.Error(".omp/skills must remain visible")
	}
}

func TestOMPHonoursDisabledExtensions(t *testing.T) {
	f := newOMPFixture(t)
	f.gitRepo(t)
	writeSkill(t, filepath.Join(f.repo, ".omp", "skills"), "deploy", "Ship the service")
	writeSkill(t, filepath.Join(f.repo, ".omp", "skills"), "audit", "Audit the tree")
	writeFile(t, filepath.Join(f.home, ".omp", "agent", "config.yml"),
		"disabledExtensions:\n  - skill:deploy\n  - plugin:foo\n")

	catalog := f.catalog(f.repo)
	if _, ok := commandByName(catalog, "/skill:deploy"); ok {
		t.Error("skill:deploy must be banned")
	}
	if _, ok := commandByName(catalog, "/skill:audit"); !ok {
		t.Error("plugin:foo must not affect skills")
	}
}

func TestOMPSkillCommandsDisabledYieldsBuiltinsOnly(t *testing.T) {
	f := newOMPFixture(t)
	f.gitRepo(t)
	writeSkill(t, filepath.Join(f.repo, ".omp", "skills"), "deploy", "Ship the service")
	writeFile(t, filepath.Join(f.home, ".omp", "agent", "config.yml"),
		"skills:\n  enableSkillCommands: false\n")

	catalog := f.catalog(f.repo)
	if len(catalog.Commands) != len(ompBuiltins) {
		t.Fatalf("commands = %d, want %d builtins", len(catalog.Commands), len(ompBuiltins))
	}
	if countSource(catalog, "builtin") != len(ompBuiltins) {
		t.Error("every command should be a builtin")
	}
}

func TestOMPSkillsDisabledYieldsBuiltinsOnly(t *testing.T) {
	f := newOMPFixture(t)
	f.gitRepo(t)
	writeSkill(t, filepath.Join(f.repo, ".omp", "skills"), "deploy", "Ship the service")
	writeFile(t, filepath.Join(f.home, ".omp", "agent", "config.yml"),
		"skills:\n  enabled: false\n")

	if len(f.catalog(f.repo).Commands) != len(ompBuiltins) {
		t.Fatal("skills.enabled: false must yield builtins only")
	}
}

func TestOMPListsSkillWithoutDescription(t *testing.T) {
	f := newOMPFixture(t)
	f.gitRepo(t)
	writeSkill(t, filepath.Join(f.repo, ".omp", "skills"), "nodesc", "")

	command, ok := commandByName(f.catalog(f.repo), "/skill:nodesc")
	if !ok {
		t.Fatal("OMP compatibility skills do not require a description")
	}
	if command.Description != "Nodesc skill" {
		t.Fatalf("fallback description = %q", command.Description)
	}
}

func TestOMPNativeBeatsClaudeOnNameCollision(t *testing.T) {
	f := newOMPFixture(t)
	f.gitRepo(t)
	writeSkill(t, filepath.Join(f.repo, ".omp", "skills"), "deploy", "native description")
	writeSkill(t, filepath.Join(f.repo, ".claude", "skills"), "deploy", "claude description")

	catalog := f.catalog(f.repo)
	seen := 0
	for _, command := range catalog.Commands {
		if command.Command == "/skill:deploy" {
			seen++
		}
	}
	if seen != 1 {
		t.Fatalf("/skill:deploy appears %d times, want 1", seen)
	}
	command, _ := commandByName(catalog, "/skill:deploy")
	if command.Description != "native description" {
		t.Fatalf("description = %q, want the .omp one", command.Description)
	}
}

func TestOMPProjectSkillBeatsUserSkill(t *testing.T) {
	f := newOMPFixture(t)
	f.gitRepo(t)
	writeSkill(t, filepath.Join(f.home, ".omp", "agent", "skills"), "deploy", "user description")
	writeSkill(t, filepath.Join(f.repo, ".omp", "skills"), "deploy", "project description")

	command, ok := commandByName(f.catalog(f.repo), "/skill:deploy")
	if !ok {
		t.Fatal("/skill:deploy missing")
	}
	if command.Description != "project description" || command.Source != "project" {
		t.Fatalf("project scope should win, got %+v", command)
	}
}

func TestOMPDiscoversUserSkillsFromActiveProfileAgentDir(t *testing.T) {
	f := newOMPFixture(t)
	agentDir := filepath.Join(f.home, ".omp", "profiles", "personal", "agent")
	writeSkill(t, filepath.Join(agentDir, "skills"), "profile-skill", "From a named profile")
	writeSkill(t, filepath.Join(f.home, ".omp", "profiles", "other", "agent", "skills"),
		"other-skill", "From another profile")

	catalog := CatalogForProfile("omp", "omp", f.repo, f.home, nil, "", "18.0.3", agentDir)
	command, ok := commandByName(catalog, "/skill:profile-skill")
	if !ok {
		t.Fatal("the active named profile's skills must be discovered")
	}
	if command.Source != "personal" {
		t.Errorf("source = %q, want personal", command.Source)
	}
	if _, ok := commandByName(catalog, "/skill:other-skill"); ok {
		t.Fatal("another named profile's skill leaked into the active catalog")
	}
}

func TestOMPDiscoversCustomDirectories(t *testing.T) {
	f := newOMPFixture(t)
	custom := filepath.Join(f.home, "elsewhere", "skills")
	writeSkill(t, custom, "custom-one", "From a custom directory")
	writeFile(t, filepath.Join(f.home, ".omp", "agent", "config.yml"),
		"skills:\n  enablePiUser: false\n  customDirectories:\n    - ~/elsewhere/skills\n")

	command, ok := commandByName(f.catalog(f.repo), "/skill:custom-one")
	if !ok {
		t.Fatal("customDirectories must be scanned even with source toggles off")
	}
	if command.Source != "personal" {
		t.Errorf("source = %q, want personal", command.Source)
	}
}

func TestOMPHonoursProjectConfigOverlay(t *testing.T) {
	f := newOMPFixture(t)
	f.gitRepo(t)
	writeSkill(t, filepath.Join(f.repo, ".omp", "skills"), "deploy", "Ship the service")
	writeSkill(t, filepath.Join(f.repo, ".claude", "skills"), "review", "Review a diff")
	writeFile(t, filepath.Join(f.repo, ".omp", "config.yml"),
		"skills:\n  enableClaudeProject: false\n")

	catalog := f.catalog(f.repo)
	if _, ok := commandByName(catalog, "/skill:review"); ok {
		t.Error("a repo's own .omp/config.yml must be able to hide .claude/skills")
	}
	if _, ok := commandByName(catalog, "/skill:deploy"); !ok {
		t.Error(".omp/skills must remain visible")
	}
}

func TestOMPProjectConfigOverridesUserConfig(t *testing.T) {
	f := newOMPFixture(t)
	f.gitRepo(t)
	writeSkill(t, filepath.Join(f.repo, ".claude", "skills"), "review", "Review a diff")
	writeFile(t, filepath.Join(f.home, ".omp", "agent", "config.yml"),
		"skills:\n  enableClaudeProject: false\n")
	writeFile(t, filepath.Join(f.repo, ".omp", "config.yml"),
		"skills:\n  enableClaudeProject: true\n")

	if _, ok := commandByName(f.catalog(f.repo), "/skill:review"); !ok {
		t.Fatal("the repo config is applied last and must win")
	}
}

func TestOMPUserSkillsIgnoredWhenPiUserDisabled(t *testing.T) {
	f := newOMPFixture(t)
	writeSkill(t, filepath.Join(f.home, ".omp", "agent", "skills"), "user-skill", "A user skill")
	writeFile(t, filepath.Join(f.home, ".omp", "agent", "config.yml"),
		"skills:\n  enablePiUser: false\n")

	if _, ok := commandByName(f.catalog(f.repo), "/skill:user-skill"); ok {
		t.Fatal("enablePiUser: false must hide the native user skills")
	}
}

func TestOMPManagedSkillsAlwaysScanned(t *testing.T) {
	f := newOMPFixture(t)
	writeSkill(t, filepath.Join(f.home, ".omp", "agent", "managed-skills"), "managed-one", "A managed skill")
	writeFile(t, filepath.Join(f.home, ".omp", "agent", "config.yml"),
		"skills:\n  enablePiUser: false\n  enableClaudeUser: false\n  enableClaudeProject: false\n  enableCodexUser: false\n  enablePiProject: false\n")

	if _, ok := commandByName(f.catalog(f.repo), "/skill:managed-one"); !ok {
		t.Fatal("managed skills are always enabled")
	}
}

func TestOMPFallThroughGateClosesGitHubSource(t *testing.T) {
	f := newOMPFixture(t)
	f.gitRepo(t)
	writeSkill(t, filepath.Join(f.repo, ".github", "skills"), "gh-one", "A GitHub skill")

	if _, ok := commandByName(f.catalog(f.repo), "/skill:gh-one"); !ok {
		t.Fatal("the default fall-through gate is open, .github/skills should be listed")
	}

	writeFile(t, filepath.Join(f.home, ".omp", "agent", "config.yml"),
		"skills:\n  enableCodexUser: false\n  enableClaudeUser: false\n  enableClaudeProject: false\n  enablePiUser: false\n  enablePiProject: false\n")
	if _, ok := commandByName(f.catalog(f.repo), "/skill:gh-one"); ok {
		t.Fatal("all five toggles off must close the fall-through gate")
	}
}

func TestOMPIgnoredSkillsGlob(t *testing.T) {
	f := newOMPFixture(t)
	f.gitRepo(t)
	writeSkill(t, filepath.Join(f.repo, ".omp", "skills"), "noisy-one", "Noisy")
	writeSkill(t, filepath.Join(f.repo, ".omp", "skills"), "quiet", "Quiet")
	writeFile(t, filepath.Join(f.home, ".omp", "agent", "config.yml"),
		"skills:\n  ignoredSkills:\n    - noisy-*\n")

	catalog := f.catalog(f.repo)
	if _, ok := commandByName(catalog, "/skill:noisy-one"); ok {
		t.Error("ignoredSkills glob must ban noisy-one")
	}
	if _, ok := commandByName(catalog, "/skill:quiet"); !ok {
		t.Error("quiet must survive")
	}
}

func TestOMPIncludeSkillsActsAsAllowList(t *testing.T) {
	f := newOMPFixture(t)
	f.gitRepo(t)
	writeSkill(t, filepath.Join(f.repo, ".omp", "skills"), "wanted", "Wanted")
	writeSkill(t, filepath.Join(f.repo, ".omp", "skills"), "other", "Other")
	writeFile(t, filepath.Join(f.home, ".omp", "agent", "config.yml"),
		"skills:\n  includeSkills: [wanted]\n")

	catalog := f.catalog(f.repo)
	if _, ok := commandByName(catalog, "/skill:other"); ok {
		t.Error("includeSkills must exclude unlisted skills")
	}
	if _, ok := commandByName(catalog, "/skill:wanted"); !ok {
		t.Error("wanted must be listed")
	}
	if len(catalog.Commands) != len(ompBuiltins)+1 {
		t.Errorf("commands = %d, want %d", len(catalog.Commands), len(ompBuiltins)+1)
	}
}

func TestOMPFollowsContainedSymlinkedSkillDirectory(t *testing.T) {
	f := newOMPFixture(t)
	f.gitRepo(t)
	real := filepath.Join(f.repo, ".skill-store", "deploy")
	writeFile(t, filepath.Join(real, "SKILL.md"), "---\nname: deploy\ndescription: Ship it\n---\n")
	skillsDir := filepath.Join(f.repo, ".omp", "skills")
	mkdirAll(t, skillsDir)
	if err := os.Symlink(filepath.Join("..", "..", ".skill-store", "deploy"), filepath.Join(skillsDir, "deploy")); err != nil {
		t.Fatalf("symlink: %v", err)
	}

	if _, ok := commandByName(f.catalog(f.repo), "/skill:deploy"); !ok {
		t.Fatal("a skill directory symlinked within the project must be followed")
	}
}

func TestOMPINIConfiguredFormatSkipsNativeDiscovery(t *testing.T) {
	f := newOMPFixture(t)
	f.gitRepo(t)
	writeSkill(t, filepath.Join(f.repo, ".omp", "skills"), "deploy", "Ship the service")
	explicit := filepath.Join(f.home, "explicit", "skills")
	writeSkill(t, explicit, "explicit-one", "Explicitly configured")

	catalog := CatalogForProfile("omp", "omp", f.repo, f.home,
		[]string{explicit}, "skill:{name}", "18.0.3", "")
	if _, ok := commandByName(catalog, "/skill:explicit-one"); !ok {
		t.Error("the INI-configured directory must be scanned")
	}
	if _, ok := commandByName(catalog, "/skill:deploy"); ok {
		t.Error("explicit configuration outranks discovery, so native discovery is skipped")
	}
}

func TestOMPInactiveProfileConfigDoesNotOverrideDefault(t *testing.T) {
	f := newOMPFixture(t)
	f.gitRepo(t)
	writeSkill(t, filepath.Join(f.repo, ".omp", "skills"), "deploy", "Ship the service")
	writeFile(t, filepath.Join(f.home, ".omp", "profiles", "personal", "agent", "config.yml"),
		strings.Repeat(" ", maxSettingsSize+1))
	writeFile(t, filepath.Join(f.home, ".omp", "agent", "config.yml"),
		"disabledExtensions:\n  - skill:deploy\n")

	if _, ok := commandByName(f.catalog(f.repo), "/skill:deploy"); ok {
		t.Fatal("an inactive profile must not make the default profile ignore its own bans")
	}
}

func TestOMPActiveProfileConfigGovernsDiscovery(t *testing.T) {
	f := newOMPFixture(t)
	f.gitRepo(t)
	writeSkill(t, filepath.Join(f.repo, ".omp", "skills"), "deploy", "Ship the service")
	writeSkill(t, filepath.Join(f.repo, ".omp", "skills"), "audit", "Audit the tree")
	agentDir := filepath.Join(f.home, ".omp", "profiles", "personal", "agent")
	writeFile(t, filepath.Join(agentDir, "config.yml"), "disabledExtensions:\n  - skill:deploy\n")

	catalog := CatalogForProfile("omp", "omp", f.repo, f.home, nil, "", "18.0.3", agentDir)
	if _, ok := commandByName(catalog, "/skill:deploy"); ok {
		t.Error("the active profile's config.yml must apply its bans")
	}
	if _, ok := commandByName(catalog, "/skill:audit"); !ok {
		t.Error("discovery must stay on for skills the active profile does not ban")
	}
}

func TestOMPBudgetFavorsHighestPrioritySource(t *testing.T) {
	f := newOMPFixture(t)
	f.gitRepo(t)
	writeSkill(t, filepath.Join(f.repo, ".omp", "skills"), "mine", "The native skill")
	bulk := filepath.Join(f.repo, ".github", "skills")
	for i := range maxCustomFiles {
		writeSkill(t, bulk, fmt.Sprintf("bulk-%03d", i), "Filler")
	}

	catalog := f.catalog(f.repo)
	if !catalog.Truncated {
		t.Error("exhausting the file budget must mark the catalog truncated")
	}
	if _, ok := commandByName(catalog, "/skill:mine"); !ok {
		t.Fatal("the highest-priority source must be scanned before the budget is spent on low-priority ones")
	}
}

func TestFindProjectDirsWalksIntermediateAncestors(t *testing.T) {
	root := t.TempDir()
	mkdirAll(t, filepath.Join(root, ".git"))
	cwd := filepath.Join(root, "services", "api")
	outer := filepath.Join(root, ".omp")
	middle := filepath.Join(root, "services", ".omp")
	inner := filepath.Join(cwd, ".omp")
	for _, dir := range []string{outer, middle, inner} {
		mkdirAll(t, dir)
	}

	dirs := findProjectDirs(cwd, []string{".omp"})
	want := []string{outer, middle, inner}
	if len(dirs) != len(want) {
		t.Fatalf("dirs = %v, want %v", dirs, want)
	}
	for i := range want {
		if dirs[i] != want[i] {
			t.Fatalf("dirs = %v, want %v (outermost first, intermediate ancestors included)", dirs, want)
		}
	}
}

// TestOMPEmptyHomeDoesNotScanServiceWorkingDirectory guards the
// os.UserHomeDir() failure path in server.go: when ctx.Home is "" every
// user-scope join (.claude/skills, .codex/skills, ...) becomes relative, and
// a relative dir must never resolve against the relay service's own working
// directory - which is arbitrary and unrelated to any project - instead of
// simply contributing nothing.
func TestOMPEmptyHomeDoesNotScanServiceWorkingDirectory(t *testing.T) {
	isolateAgentEnv(t)
	repo := t.TempDir()
	scratch := t.TempDir()
	writeSkill(t, filepath.Join(scratch, ".claude", "skills"), "leak-omp", "Should never be discovered")
	t.Chdir(scratch)

	catalog := CatalogForProfile("omp", "omp", repo, "", nil, "", "18.0.3", "")
	if _, ok := commandByName(catalog, "/skill:leak-omp"); ok {
		t.Fatal("empty ctx.Home must not make omp scan the service's own working directory")
	}
}

// TestOMPCustomDirectoryResolvesAgainstCwd covers the other way a relative
// scan root reaches os.ReadDir: a relative customDirectories entry is legal
// in a project .omp/config.yml, and is far more likely to mean "relative to
// my project" than to be a mistake - it must resolve against the pane's cwd.
func TestOMPCustomDirectoryResolvesAgainstCwd(t *testing.T) {
	f := newOMPFixture(t)
	writeFile(t, filepath.Join(f.home, ".omp", "agent", "config.yml"),
		"skills:\n  customDirectories:\n    - relskills\n")
	writeSkill(t, filepath.Join(f.repo, "relskills"), "intended", "From the project")

	command, ok := commandByName(f.catalog(f.repo), "/skill:intended")
	if !ok {
		t.Fatal("a relative customDirectories entry must resolve against ctx.Cwd")
	}
	if command.Source != "personal" {
		t.Errorf("source = %q, want personal", command.Source)
	}
}

// TestOMPCustomDirectorySkippedWhenCwdUnknown covers the same relative
// customDirectories entry when the pane's cwd is unknown: it must be
// dropped, not fall through to scan's own guard and silently resolve against
// the service's unrelated working directory.
func TestOMPCustomDirectorySkippedWhenCwdUnknown(t *testing.T) {
	f := newOMPFixture(t)
	writeFile(t, filepath.Join(f.home, ".omp", "agent", "config.yml"),
		"skills:\n  customDirectories:\n    - relskills\n")
	scratch := t.TempDir()
	writeSkill(t, filepath.Join(scratch, "relskills"), "stray", "Should never be discovered")
	t.Chdir(scratch)

	catalog := CatalogForProfile("omp", "omp", "", f.home, nil, "", "18.0.3", "")
	if _, ok := commandByName(catalog, "/skill:stray"); ok {
		t.Fatal("a relative customDirectories entry must not resolve against the service's cwd when ctx.Cwd is empty")
	}
}

func TestOMPRejectsProjectSkillMetadataSymlinkEscapingRoot(t *testing.T) {
	f := newOMPFixture(t)
	f.gitRepo(t)
	external := filepath.Join(t.TempDir(), "SKILL.md")
	writeFile(t, external, "---\nname: leaked\ndescription: Outside the project\n---\n")
	skillDir := filepath.Join(f.repo, ".omp", "skills", "leaked")
	mkdirAll(t, skillDir)
	if err := os.Symlink(external, filepath.Join(skillDir, "SKILL.md")); err != nil {
		t.Fatal(err)
	}

	if _, ok := commandByName(f.catalog(f.repo), "/skill:leaked"); ok {
		t.Fatal("project skill metadata symlinked outside the project root was exposed")
	}
}

func TestOMPCustomDirectoryOverridesDefaultProvider(t *testing.T) {
	f := newOMPFixture(t)
	f.gitRepo(t)
	writeSkill(t, filepath.Join(f.repo, ".omp", "skills"), "deploy", "Native copy")
	custom := filepath.Join(f.home, "custom-skills")
	writeSkill(t, custom, "deploy", "Custom copy")
	writeFile(t, filepath.Join(f.home, ".omp", "agent", "config.yml"),
		fmt.Sprintf("skills:\n  customDirectories:\n    - %s\n", custom))

	command, ok := commandByName(f.catalog(f.repo), "/skill:deploy")
	if !ok {
		t.Fatal("/skill:deploy missing")
	}
	if command.Description != "Custom copy" {
		t.Fatalf("description = %q, want custom directory override", command.Description)
	}
}

func TestOMPDiscoversConfiguredExtensionPackageSkills(t *testing.T) {
	f := newOMPFixture(t)
	extension := filepath.Join(f.home, "extensions", "release-tools")
	writeSkill(t, filepath.Join(extension, "skills"), "release", "Release from extension")
	settings, err := json.Marshal(map[string]any{"extensions": []string{extension}})
	if err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(f.home, ".omp", "agent", "settings.json"), string(settings))

	command, ok := commandByName(f.catalog(f.repo), "/skill:release")
	if !ok {
		t.Fatal("an OMP extension package's skills were not discovered")
	}
	if command.Description != "Release from extension" {
		t.Fatalf("command = %+v", command)
	}
}

func TestOMPDiscoversInstalledExtensionDependencySkills(t *testing.T) {
	f := newOMPFixture(t)
	pluginRoot := filepath.Join(f.home, ".omp", "plugins")
	writeFile(t, filepath.Join(pluginRoot, "package.json"),
		`{"dependencies":{"@acme/release-tools":"1.0.0"}}`)
	extension := filepath.Join(pluginRoot, "node_modules", "@acme", "release-tools")
	writeFile(t, filepath.Join(extension, "package.json"), `{"omp":{}}`)
	writeSkill(t, filepath.Join(extension, "skills"), "publish", "Publish from installed extension")

	command, ok := commandByName(f.catalog(f.repo), "/skill:publish")
	if !ok {
		t.Fatal("an installed OMP extension dependency's skills were not discovered")
	}
	if command.Description != "Publish from installed extension" {
		t.Fatalf("command = %+v", command)
	}
}

func TestOMPProjectCustomDirectoryCannotEscapeProject(t *testing.T) {
	f := newOMPFixture(t)
	external := filepath.Join(f.home, "external-skills")
	writeSkill(t, external, "leaked", "Must remain private")
	writeFile(t, filepath.Join(f.repo, ".omp", "config.yml"),
		fmt.Sprintf("skills:\n  customDirectories: [%q]\n", external))

	if _, ok := commandByName(f.catalog(f.repo), "/skill:leaked"); ok {
		t.Fatal("project customDirectories exposed a skill outside the project boundary")
	}
}

func TestOMPProjectCustomDirectoryInsideProjectIsProjectScoped(t *testing.T) {
	f := newOMPFixture(t)
	writeSkill(t, filepath.Join(f.repo, "team-skills"), "inside", "Inside project")
	writeFile(t, filepath.Join(f.repo, ".omp", "config.yml"),
		"skills:\n  customDirectories: [team-skills]\n")

	command, ok := commandByName(f.catalog(f.repo), "/skill:inside")
	if !ok {
		t.Fatal("contained project customDirectories skill was not discovered")
	}
	if command.Source != "project" {
		t.Fatalf("source = %q, want project", command.Source)
	}
}

func TestOMPDiscoversClaudeMarketplacePluginSkills(t *testing.T) {
	f := newOMPFixture(t)
	install := filepath.Join(f.home, "marketplace", "release-tools")
	writeSkill(t, filepath.Join(install, "skills"), "market-release", "Marketplace release")
	writeSkill(t, filepath.Join(install, "team-skills"), "manifest-skill", "Manifest skill directory")
	writeFile(t, filepath.Join(install, ".claude-plugin", "plugin.json"),
		`{"skills":["team-skills"]}`)
	disabled := filepath.Join(f.home, "marketplace", "disabled-tools")
	writeSkill(t, filepath.Join(disabled, "skills"), "disabled-plugin", "Disabled plugin")
	registry, err := json.Marshal(map[string]any{
		"version": 2,
		"plugins": map[string]any{
			"release-tools@acme": []any{map[string]any{
				"installPath": install,
				"enabled":     true,
				"scope":       "user",
			}},
			"disabled-tools@acme": []any{map[string]any{
				"installPath": disabled,
				"enabled":     false,
				"scope":       "user",
			}},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(f.home, ".claude", "plugins", "installed_plugins.json"), string(registry))

	catalog := f.catalog(f.repo)
	for _, name := range []string{"/skill:market-release", "/skill:manifest-skill"} {
		if _, ok := commandByName(catalog, name); !ok {
			t.Fatalf("Claude marketplace plugin skill %s was not discovered", name)
		}
	}
	if _, ok := commandByName(catalog, "/skill:disabled-plugin"); ok {
		t.Fatal("disabled Claude marketplace plugin was discovered")
	}
}

func TestOMPAgentsSkillsBeatCodexAtEqualPriority(t *testing.T) {
	f := newOMPFixture(t)
	writeSkill(t, filepath.Join(f.home, ".agents", "skills"), "shared-tie", "Agents copy")
	writeSkill(t, filepath.Join(f.home, ".codex", "skills"), "shared-tie", "Codex copy")

	command, ok := commandByName(f.catalog(f.repo), "/skill:shared-tie")
	if !ok {
		t.Fatal("equal-priority compatibility skill missing")
	}
	if command.Description != "Agents copy" {
		t.Fatalf("description = %q, want Agents provider to win the tie", command.Description)
	}
}

func TestOMPSkipsFrontmatterDisabledSkill(t *testing.T) {
	f := newOMPFixture(t)
	writeFile(t, filepath.Join(f.repo, ".omp", "skills", "off", "SKILL.md"),
		"---\nname: off\ndescription: Disabled\nenabled: false\n---\n")

	if _, ok := commandByName(f.catalog(f.repo), "/skill:off"); ok {
		t.Fatal("frontmatter enabled:false skill was listed")
	}
}

func TestOMPProjectConfigDoesNotInheritFromAncestor(t *testing.T) {
	f := newOMPFixture(t)
	nested := filepath.Join(f.repo, "services", "api")
	mkdirAll(t, nested)
	writeSkill(t, filepath.Join(nested, ".omp", "skills"), "local", "Local skill")
	writeFile(t, filepath.Join(f.repo, ".omp", "config.yml"),
		"skills:\n  enableSkillCommands: false\n")

	if _, ok := commandByName(f.catalog(nested), "/skill:local"); !ok {
		t.Fatal("ancestor .omp/config.yml incorrectly disabled cwd skill discovery")
	}
}

func TestOMPWalksSkillAncestorsOutsideGit(t *testing.T) {
	f := newOMPFixture(t)
	parent := filepath.Join(f.repo, "workspace")
	nested := filepath.Join(parent, "services", "api")
	mkdirAll(t, nested)
	writeSkill(t, filepath.Join(parent, ".omp", "skills"), "ancestor", "Ancestor skill")

	if _, ok := commandByName(f.catalog(nested), "/skill:ancestor"); !ok {
		t.Fatal("non-git ancestor .omp/skills was not discovered")
	}
}

func TestOMPFlowListKeepsCommaInsideQuotedPath(t *testing.T) {
	f := newOMPFixture(t)
	custom := filepath.Join(f.home, "skills,shared")
	writeSkill(t, custom, "comma-path", "Quoted comma path")
	writeFile(t, filepath.Join(f.home, ".omp", "agent", "config.yml"),
		fmt.Sprintf("skills:\n  customDirectories: [%q]\n", custom))

	if _, ok := commandByName(f.catalog(f.repo), "/skill:comma-path"); !ok {
		t.Fatal("quoted YAML flow-list comma split one custom directory into two")
	}
}

func TestOMPDiscoversProjectPluginRootSkills(t *testing.T) {
	f := newOMPFixture(t)
	pluginRoot := filepath.Join(f.repo, ".omp", "plugins")
	writeFile(t, filepath.Join(pluginRoot, "package.json"),
		`{"dependencies":{"project-tools":"1.0.0"}}`)
	extension := filepath.Join(pluginRoot, "node_modules", "project-tools")
	writeFile(t, filepath.Join(extension, "package.json"), `{"omp":{}}`)
	writeSkill(t, filepath.Join(extension, "skills"), "project-plugin", "Project plugin")

	command, ok := commandByName(f.catalog(f.repo), "/skill:project-plugin")
	if !ok {
		t.Fatal("project .omp/plugins dependency skill was not discovered")
	}
	if command.Source != "project" {
		t.Fatalf("source = %q, want project", command.Source)
	}
}
