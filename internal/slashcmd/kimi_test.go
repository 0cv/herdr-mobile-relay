package slashcmd

import (
	"fmt"
	"path/filepath"
	"testing"
)

func TestKimiBuiltinCatalog(t *testing.T) {
	isolateAgentEnv(t)
	catalog := CatalogForProfile("kimi", "kimi", t.TempDir(), "/nonexistent", nil, "", "0.29.2")
	if catalog.Truncated {
		t.Fatal("Kimi builtins should not be truncated")
	}
	if len(catalog.Commands) != 39 {
		t.Fatalf("Kimi builtins = %d, want 39", len(catalog.Commands))
	}
	for _, command := range []string{"/model", "/permission", "/swarm", "/goal", "/export-md"} {
		if !hasCommand(catalog, command) {
			t.Errorf("Kimi catalog missing %q", command)
		}
	}
}

func TestKimiCommandHints(t *testing.T) {
	isolateAgentEnv(t)
	catalog := CatalogFor("kimi-code", t.TempDir(), "/nonexistent")
	for _, command := range catalog.Commands {
		if command.Command != "/goal" {
			continue
		}
		if command.ArgumentHint != "[status|pause|resume|cancel|replace|next] | <objective>" {
			t.Fatalf("/goal hint = %q", command.ArgumentHint)
		}
		return
	}
	t.Fatal("/goal not found")
}

func TestParseKimiSkillSettings(t *testing.T) {
	cases := []struct {
		name      string
		seedDirs  []string
		data      string
		wantMerge bool
		wantDirs  []string
	}{
		{
			name:      "bare false",
			data:      "merge_all_available_skills = false\n",
			wantMerge: false,
		},
		{
			name:      "bare true",
			data:      "merge_all_available_skills = true\n",
			wantMerge: true,
		},
		{
			name:      "case insensitive",
			data:      "merge_all_available_skills = FALSE\n",
			wantMerge: false,
		},
		{
			name:      "quoted boolean",
			data:      "merge_all_available_skills = \"false\"\n",
			wantMerge: false,
		},
		{
			name:      "unrecognised scalar leaves default",
			data:      "merge_all_available_skills = 0\n",
			wantMerge: true,
		},
		{
			name:      "trailing comment stripped",
			data:      "merge_all_available_skills = false # keep the old behaviour\n",
			wantMerge: false,
		},
		{
			name:      "comment line ignored",
			data:      "# merge_all_available_skills = false\n",
			wantMerge: true,
		},
		{
			name:      "indented key still parsed",
			data:      "  merge_all_available_skills = false\n",
			wantMerge: false,
		},
		{
			name:      "single line array",
			data:      "extra_skill_dirs = [\"~/a\", '/b/c']\n",
			wantMerge: true,
			wantDirs:  []string{"~/a", "/b/c"},
		},
		{
			name:      "array with trailing comment",
			data:      "extra_skill_dirs = [\"~/a\"] # team shared\n",
			wantMerge: true,
			wantDirs:  []string{"~/a"},
		},
		{
			name:      "hash inside a quoted item survives",
			data:      "extra_skill_dirs = [\"~/a#b\"]\n",
			wantMerge: true,
			wantDirs:  []string{"~/a#b"},
		},
		{
			name:      "empty items dropped",
			data:      "extra_skill_dirs = [\"\", \"~/a\", ]\n",
			wantMerge: true,
			wantDirs:  []string{"~/a"},
		},
		{
			name:      "empty array clears the key",
			seedDirs:  []string{"~/keep"},
			data:      "extra_skill_dirs = []\n",
			wantMerge: true,
			wantDirs:  nil,
		},
		{
			name:      "multi line array leaves the key untouched",
			seedDirs:  []string{"~/keep"},
			data:      "extra_skill_dirs = [\n  \"~/a\",\n]\n",
			wantMerge: true,
			wantDirs:  []string{"~/keep"},
		},
		{
			name:      "later file replaces the list",
			seedDirs:  []string{"~/keep"},
			data:      "extra_skill_dirs = [\"~/a\"]\n",
			wantMerge: true,
			wantDirs:  []string{"~/a"},
		},
		{
			name:      "table header isolates a later key",
			data:      "extra_skill_dirs = [\"~/a\"]\n\n[agent]\nmerge_all_available_skills = false\n",
			wantMerge: true,
			wantDirs:  []string{"~/a"},
		},
		{
			name:      "root keys before a table header are consumed",
			data:      "merge_all_available_skills = false\n[tui]\ntheme = \"dark\"\n",
			wantMerge: false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			settings := defaultKimiSkillSettings()
			settings.extraSkillDirs = tc.seedDirs
			parseKimiSkillSettings([]byte(tc.data), &settings)

			if settings.mergeAllAvailableSkills != tc.wantMerge {
				t.Errorf("mergeAllAvailableSkills = %v, want %v",
					settings.mergeAllAvailableSkills, tc.wantMerge)
			}
			if len(settings.extraSkillDirs) != len(tc.wantDirs) {
				t.Fatalf("extraSkillDirs = %q, want %q", settings.extraSkillDirs, tc.wantDirs)
			}
			for i, want := range tc.wantDirs {
				if settings.extraSkillDirs[i] != want {
					t.Errorf("extraSkillDirs[%d] = %q, want %q", i, settings.extraSkillDirs[i], want)
				}
			}
		})
	}
}

// kimiFixture builds an isolated home + repo pair for Kimi discovery tests.
type kimiFixture struct {
	home string
	repo string
}

func newKimiFixture(t *testing.T) kimiFixture {
	t.Helper()
	isolateAgentEnv(t)
	root := t.TempDir()
	fixture := kimiFixture{
		home: filepath.Join(root, "home"),
		repo: filepath.Join(root, "repo"),
	}
	mkdirAll(t, fixture.home)
	mkdirAll(t, fixture.repo)
	return fixture
}

func (f kimiFixture) gitRepo(t *testing.T) {
	t.Helper()
	mkdirAll(t, filepath.Join(f.repo, ".git"))
}

func (f kimiFixture) config(t *testing.T, content string) {
	t.Helper()
	writeFile(t, filepath.Join(f.home, ".kimi", "config.toml"), content)
}

func (f kimiFixture) catalog(cwd string) Catalog {
	return CatalogForProfile("kimi", "kimi", cwd, f.home, nil, "", "0.29.2")
}

func TestKimiDiscoversProjectKimiSkills(t *testing.T) {
	f := newKimiFixture(t)
	f.gitRepo(t)
	writeSkill(t, filepath.Join(f.repo, ".kimi", "skills"), "deploy", "Ship the service")

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

func TestKimiDiscoversProjectSkillsFromSubdirectory(t *testing.T) {
	f := newKimiFixture(t)
	f.gitRepo(t)
	writeSkill(t, filepath.Join(f.repo, ".kimi", "skills"), "deploy", "Ship the service")
	nested := filepath.Join(f.repo, "services", "api")
	mkdirAll(t, nested)

	if _, ok := commandByName(f.catalog(nested), "/skill:deploy"); !ok {
		t.Fatal("walk-up from a subdirectory did not reach the project root's .kimi/skills")
	}
}

func TestKimiMergesProjectBrandAndGenericGroups(t *testing.T) {
	f := newKimiFixture(t)
	f.gitRepo(t)
	writeSkill(t, filepath.Join(f.repo, ".claude", "skills"), "review", "Review a diff")
	writeSkill(t, filepath.Join(f.repo, ".agents", "skills"), "audit", "Audit dependencies")

	catalog := f.catalog(f.repo)
	for _, name := range []string{"/skill:review", "/skill:audit"} {
		command, ok := commandByName(catalog, name)
		if !ok {
			t.Fatalf("%s missing; merge_all_available_skills defaults to true", name)
		}
		if command.Source != "project" {
			t.Errorf("%s source = %q, want project", name, command.Source)
		}
	}
}

func TestKimiMergesUserBrandDirsByDefault(t *testing.T) {
	f := newKimiFixture(t)
	writeSkill(t, filepath.Join(f.home, ".kimi", "skills"), "deploy", "Ship the service")
	writeSkill(t, filepath.Join(f.home, ".claude", "skills"), "review", "Review a diff")

	catalog := f.catalog(f.repo)
	for _, name := range []string{"/skill:deploy", "/skill:review"} {
		command, ok := commandByName(catalog, name)
		if !ok {
			t.Fatalf("%s missing; every existing brand directory is merged by default", name)
		}
		if command.Source != "personal" {
			t.Errorf("%s source = %q, want personal", name, command.Source)
		}
	}
}

func TestKimiFirstExistingUserBrandDirWinsWhenMergeDisabled(t *testing.T) {
	f := newKimiFixture(t)
	f.config(t, "merge_all_available_skills = false\n")
	writeSkill(t, filepath.Join(f.home, ".kimi", "skills"), "deploy", "Ship the service")
	writeSkill(t, filepath.Join(f.home, ".claude", "skills"), "review", "Review a diff")

	catalog := f.catalog(f.repo)
	command, ok := commandByName(catalog, "/skill:deploy")
	if !ok {
		t.Fatal("~/.kimi/skills is the first existing brand directory and must be used")
	}
	if command.Source != "personal" {
		t.Errorf("source = %q, want personal", command.Source)
	}
	if _, ok := commandByName(catalog, "/skill:review"); ok {
		t.Error("merge_all_available_skills = false must use only the first existing brand directory")
	}
}

func TestKimiFallsBackToNextUserBrandDirWhenMergeDisabled(t *testing.T) {
	f := newKimiFixture(t)
	f.config(t, "merge_all_available_skills = false\n")
	writeSkill(t, filepath.Join(f.home, ".claude", "skills"), "review", "Review a diff")

	if _, ok := commandByName(f.catalog(f.repo), "/skill:review"); !ok {
		t.Fatal("~/.claude/skills must be used when ~/.kimi/skills does not exist")
	}
}

func TestKimiDiscoversExtraSkillDirs(t *testing.T) {
	f := newKimiFixture(t)
	f.config(t, "extra_skill_dirs = [\"~/elsewhere/skills\"]\n")
	writeSkill(t, filepath.Join(f.home, "elsewhere", "skills"), "brief", "Write a brief")

	command, ok := commandByName(f.catalog(f.repo), "/skill:brief")
	if !ok {
		t.Fatal("extra_skill_dirs entry was not scanned")
	}
	if command.Source != "personal" {
		t.Errorf("source = %q, want personal", command.Source)
	}
}

func TestKimiProjectSkillBeatsUserSkill(t *testing.T) {
	f := newKimiFixture(t)
	f.gitRepo(t)
	writeSkill(t, filepath.Join(f.home, ".kimi", "skills"), "deploy", "User copy")
	writeSkill(t, filepath.Join(f.repo, ".kimi", "skills"), "deploy", "Project copy")

	catalog := f.catalog(f.repo)
	matches := 0
	for _, command := range catalog.Commands {
		if command.Command == "/skill:deploy" {
			matches++
		}
	}
	if matches != 1 {
		t.Fatalf("/skill:deploy appears %d times, want 1", matches)
	}
	command, _ := commandByName(catalog, "/skill:deploy")
	if command.Description != "Project copy" {
		t.Errorf("description = %q, want the project copy", command.Description)
	}
	if command.Source != "project" {
		t.Errorf("source = %q, want project", command.Source)
	}
}

func TestKimiBrandPriorityWithinOneProjectAncestor(t *testing.T) {
	f := newKimiFixture(t)
	f.gitRepo(t)
	writeSkill(t, filepath.Join(f.repo, ".kimi", "skills"), "deploy", "Kimi copy")
	writeSkill(t, filepath.Join(f.repo, ".codex", "skills"), "deploy", "Codex copy")

	command, ok := commandByName(f.catalog(f.repo), "/skill:deploy")
	if !ok {
		t.Fatal("/skill:deploy missing")
	}
	if command.Description != "Kimi copy" {
		t.Errorf("description = %q, want the .kimi copy: brand priority is kimi > claude > codex",
			command.Description)
	}
}

func TestKimiDropsSkillWithoutDescription(t *testing.T) {
	f := newKimiFixture(t)
	f.gitRepo(t)
	writeSkill(t, filepath.Join(f.repo, ".kimi", "skills"), "deploy", "")

	if _, ok := commandByName(f.catalog(f.repo), "/skill:deploy"); ok {
		t.Error("a skill without a description must not be listed")
	}
}

func TestKimiINIConfiguredFormatSkipsNativeDiscovery(t *testing.T) {
	f := newKimiFixture(t)
	f.gitRepo(t)
	explicit := filepath.Join(f.home, "explicit", "skills")
	writeSkill(t, explicit, "explicit", "Configured by the relay")
	writeSkill(t, filepath.Join(f.repo, ".kimi", "skills"), "deploy", "Ship the service")

	catalog := CatalogForProfile("kimi", "kimi", f.repo, f.home,
		[]string{explicit}, "skill:{name}", "")
	if _, ok := commandByName(catalog, "/skill:explicit"); !ok {
		t.Fatal("an INI-configured skill directory must be scanned")
	}
	if _, ok := commandByName(catalog, "/skill:deploy"); ok {
		t.Error("an INI-configured format must skip native discovery")
	}
	if countSource(catalog, "project") != 0 {
		t.Errorf("project sources = %d, want 0", countSource(catalog, "project"))
	}
}

func TestKimiGenericGroupIsMutuallyExclusive(t *testing.T) {
	f := newKimiFixture(t)
	writeSkill(t, filepath.Join(f.home, ".config", "agents", "skills"), "preferred", "From the recommended dir")
	writeSkill(t, filepath.Join(f.home, ".agents", "skills"), "secondary", "From the fallback dir")

	catalog := f.catalog(f.repo)
	if _, ok := commandByName(catalog, "/skill:preferred"); !ok {
		t.Fatal("~/.config/agents/skills is the first existing generic candidate")
	}
	if _, ok := commandByName(catalog, "/skill:secondary"); ok {
		t.Error("the generic group is mutually exclusive: merge_all_available_skills does not merge it")
	}
}

func TestKimiGenericGroupFallsBackToAgentsDir(t *testing.T) {
	f := newKimiFixture(t)
	writeSkill(t, filepath.Join(f.home, ".agents", "skills"), "secondary", "From the fallback dir")

	if _, ok := commandByName(f.catalog(f.repo), "/skill:secondary"); !ok {
		t.Fatal("~/.agents/skills must be used when ~/.config/agents/skills does not exist")
	}
}

func TestKimiBrandGroupBeatsGenericGroup(t *testing.T) {
	f := newKimiFixture(t)
	f.gitRepo(t)
	writeSkill(t, filepath.Join(f.home, ".agents", "skills"), "shared", "User generic copy")
	writeSkill(t, filepath.Join(f.home, ".kimi", "skills"), "shared", "User brand copy")
	writeSkill(t, filepath.Join(f.repo, ".agents", "skills"), "review", "Project generic copy")
	writeSkill(t, filepath.Join(f.repo, ".claude", "skills"), "review", "Project brand copy")

	catalog := f.catalog(f.repo)
	user, ok := commandByName(catalog, "/skill:shared")
	if !ok {
		t.Fatal("/skill:shared missing")
	}
	if user.Description != "User brand copy" {
		t.Errorf("user description = %q, want the brand copy", user.Description)
	}
	project, ok := commandByName(catalog, "/skill:review")
	if !ok {
		t.Fatal("/skill:review missing")
	}
	if project.Description != "Project brand copy" {
		t.Errorf("project description = %q, want the brand copy", project.Description)
	}
}

func TestKimiProjectCandidatesResolveAtProjectRootOnly(t *testing.T) {
	f := newKimiFixture(t)
	f.gitRepo(t)
	writeSkill(t, filepath.Join(f.repo, ".kimi", "skills"), "root-skill", "At the project root")
	nested := filepath.Join(f.repo, "services", "api")
	writeSkill(t, filepath.Join(nested, ".kimi", "skills"), "nested-skill", "In an intermediate dir")

	catalog := f.catalog(nested)
	if _, ok := commandByName(catalog, "/skill:root-skill"); !ok {
		t.Fatal("the project root's .kimi/skills must be scanned from a subdirectory")
	}
	if _, ok := commandByName(catalog, "/skill:nested-skill"); ok {
		t.Error("Kimi resolves project candidates against the project root only, not each ancestor")
	}
}

func TestKimiWithoutGitRootUsesWorkDirectory(t *testing.T) {
	f := newKimiFixture(t)
	writeSkill(t, filepath.Join(f.repo, ".kimi", "skills"), "deploy", "Ship the service")

	if _, ok := commandByName(f.catalog(f.repo), "/skill:deploy"); !ok {
		t.Fatal("without a .git marker the work directory is the project root")
	}
}

func TestKimiExtraSkillDirsRelativeToProjectRoot(t *testing.T) {
	f := newKimiFixture(t)
	f.gitRepo(t)
	f.config(t, "extra_skill_dirs = [\"vendor/skills\"]\n")
	writeSkill(t, filepath.Join(f.repo, "vendor", "skills"), "vendored", "From a relative entry")
	nested := filepath.Join(f.repo, "services", "api")
	mkdirAll(t, nested)

	command, ok := commandByName(f.catalog(nested), "/skill:vendored")
	if !ok {
		t.Fatal("a relative extra_skill_dirs entry resolves against the project root")
	}
	if command.Source != "personal" {
		t.Errorf("source = %q, want personal", command.Source)
	}
}

func TestKimiExtraSkillDirsLoseNameCollisions(t *testing.T) {
	f := newKimiFixture(t)
	f.config(t, "extra_skill_dirs = [\"~/elsewhere/skills\"]\n")
	writeSkill(t, filepath.Join(f.home, "elsewhere", "skills"), "deploy", "Extra copy")
	writeSkill(t, filepath.Join(f.home, ".kimi", "skills"), "deploy", "User copy")

	command, ok := commandByName(f.catalog(f.repo), "/skill:deploy")
	if !ok {
		t.Fatal("/skill:deploy missing")
	}
	if command.Description != "User copy" {
		t.Errorf("description = %q, want the user copy: User outranks Extra", command.Description)
	}
}

func TestKimiBudgetFavorsHighestPriorityBrandDir(t *testing.T) {
	f := newKimiFixture(t)
	writeSkill(t, filepath.Join(f.home, ".kimi", "skills"), "mine", "The kimi skill")
	bulk := filepath.Join(f.home, ".codex", "skills")
	for i := range maxCustomFiles {
		writeSkill(t, bulk, fmt.Sprintf("bulk-%03d", i), "Filler")
	}

	catalog := f.catalog(f.repo)
	if !catalog.Truncated {
		t.Error("exhausting the file budget must mark the catalog truncated")
	}
	if _, ok := commandByName(catalog, "/skill:mine"); !ok {
		t.Fatal("~/.kimi/skills outranks ~/.codex/skills and must be scanned before the budget is spent")
	}
}

// TestKimiEmptyHomeDoesNotScanServiceWorkingDirectory guards the
// os.UserHomeDir() failure path in server.go: when ctx.Home is "" every
// user-scope join (.kimi/skills, .claude/skills, ...) becomes relative, and
// a relative dir must never resolve against the relay service's own working
// directory - which is arbitrary and unrelated to any project - instead of
// simply contributing nothing.
func TestKimiEmptyHomeDoesNotScanServiceWorkingDirectory(t *testing.T) {
	isolateAgentEnv(t)
	repo := t.TempDir()
	scratch := t.TempDir()
	writeSkill(t, filepath.Join(scratch, ".kimi", "skills"), "leak-kimi", "Should never be discovered")
	t.Chdir(scratch)

	catalog := CatalogForProfile("kimi", "kimi", repo, "", nil, "", "0.29.2")
	if _, ok := commandByName(catalog, "/skill:leak-kimi"); ok {
		t.Fatal("empty ctx.Home must not make Kimi scan the service's own working directory")
	}
}

// TestKimiEmptyHomeDoesNotReadServiceWorkingDirectoryConfig guards
// kimi.go's own config.toml read against the same failure: with ctx.Home
// empty, filepath.Join(ctx.Home, ".kimi") is relative, and must not resolve
// to a stray config.toml sitting in the service's own working directory. An
// extra_skill_dirs entry in that stray config would otherwise be honoured
// even though it names an otherwise-legitimate absolute path, because the
// leak is in reading the file at all, not in resolving the paths inside it.
func TestKimiEmptyHomeDoesNotReadServiceWorkingDirectoryConfig(t *testing.T) {
	isolateAgentEnv(t)
	repo := t.TempDir()
	scratch := t.TempDir()
	extra := filepath.Join(scratch, "extra-skills")
	writeSkill(t, extra, "leak-kimi-config", "Should never be discovered")
	writeFile(t, filepath.Join(scratch, ".kimi", "config.toml"),
		fmt.Sprintf("extra_skill_dirs = [%q]\n", extra))
	t.Chdir(scratch)

	catalog := CatalogForProfile("kimi", "kimi", repo, "", nil, "", "0.29.2")
	if _, ok := commandByName(catalog, "/skill:leak-kimi-config"); ok {
		t.Fatal("empty ctx.Home must not make Kimi read a config.toml from the service's own working directory")
	}
}
