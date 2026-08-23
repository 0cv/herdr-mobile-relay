package slashcmd

import (
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
	return CatalogForProfile("omp", "omp", cwd, f.home, nil, "", "18.0.3")
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

func TestOMPDropsSkillWithoutDescription(t *testing.T) {
	f := newOMPFixture(t)
	f.gitRepo(t)
	writeSkill(t, filepath.Join(f.repo, ".omp", "skills"), "nodesc", "")

	if _, ok := commandByName(f.catalog(f.repo), "/skill:nodesc"); ok {
		t.Fatal("a skill without a description must not be listed")
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

func TestOMPDiscoversUserSkillsFromProfileAgentDir(t *testing.T) {
	f := newOMPFixture(t)
	writeSkill(t, filepath.Join(f.home, ".omp", "profiles", "personal", "agent", "skills"),
		"profile-skill", "From a named profile")

	command, ok := commandByName(f.catalog(f.repo), "/skill:profile-skill")
	if !ok {
		t.Fatal("a named profile's skills must be discovered")
	}
	if command.Source != "personal" {
		t.Errorf("source = %q, want personal", command.Source)
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

func TestOMPFollowsSymlinkedSkillDirectory(t *testing.T) {
	f := newOMPFixture(t)
	f.gitRepo(t)
	real := filepath.Join(f.home, "dotfiles", "deploy")
	writeFile(t, filepath.Join(real, "SKILL.md"), "---\nname: deploy\ndescription: Ship it\n---\n")
	skillsDir := filepath.Join(f.repo, ".omp", "skills")
	mkdirAll(t, skillsDir)
	if err := os.Symlink(real, filepath.Join(skillsDir, "deploy")); err != nil {
		t.Fatalf("symlink: %v", err)
	}

	if _, ok := commandByName(f.catalog(f.repo), "/skill:deploy"); !ok {
		t.Fatal("a symlinked skill directory must be followed")
	}
}

func TestOMPINIConfiguredFormatSkipsNativeDiscovery(t *testing.T) {
	f := newOMPFixture(t)
	f.gitRepo(t)
	writeSkill(t, filepath.Join(f.repo, ".omp", "skills"), "deploy", "Ship the service")
	explicit := filepath.Join(f.home, "explicit", "skills")
	writeSkill(t, explicit, "explicit-one", "Explicitly configured")

	catalog := CatalogForProfile("omp", "omp", f.repo, f.home,
		[]string{explicit}, "skill:{name}", "18.0.3")
	if _, ok := commandByName(catalog, "/skill:explicit-one"); !ok {
		t.Error("the INI-configured directory must be scanned")
	}
	if _, ok := commandByName(catalog, "/skill:deploy"); ok {
		t.Error("explicit configuration outranks discovery, so native discovery is skipped")
	}
}

func TestOMPUnusableConfigStopsAtFirstConfigDir(t *testing.T) {
	f := newOMPFixture(t)
	f.gitRepo(t)
	writeSkill(t, filepath.Join(f.repo, ".omp", "skills"), "deploy", "Ship the service")
	writeFile(t, filepath.Join(f.home, ".omp", "profiles", "personal", "agent", "config.yml"),
		strings.Repeat(" ", maxSettingsSize+1))
	writeFile(t, filepath.Join(f.home, ".omp", "agent", "config.yml"),
		"disabledExtensions:\n  - skill:deploy\n")

	if _, ok := commandByName(f.catalog(f.repo), "/skill:deploy"); !ok {
		t.Fatal("first-found wins: an unusable config.yml in the first agent directory must stop the search and fail open, not fall through to another profile's bans")
	}
}

func TestOMPProfileConfigGovernsDiscovery(t *testing.T) {
	f := newOMPFixture(t)
	f.gitRepo(t)
	writeSkill(t, filepath.Join(f.repo, ".omp", "skills"), "deploy", "Ship the service")
	writeSkill(t, filepath.Join(f.repo, ".omp", "skills"), "audit", "Audit the tree")
	writeFile(t, filepath.Join(f.home, ".omp", "profiles", "personal", "agent", "config.yml"),
		"disabledExtensions:\n  - skill:deploy\n")

	catalog := f.catalog(f.repo)
	if _, ok := commandByName(catalog, "/skill:deploy"); ok {
		t.Error("a named profile's config.yml must be found and its bans applied")
	}
	if _, ok := commandByName(catalog, "/skill:audit"); !ok {
		t.Error("discovery must stay on for skills the profile does not ban")
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
