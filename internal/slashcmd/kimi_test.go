package slashcmd

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

func TestKimiBuiltinCatalog(t *testing.T) {
	isolateAgentEnv(t)
	catalog := CatalogForProfile("kimi", "kimi", t.TempDir(), "/nonexistent", nil, "", "0.36.1", "")
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
		name     string
		seedDirs []string
		data     string
		wantDirs []string
	}{
		{
			name:     "single line array",
			data:     "extra_skill_dirs = [\"~/a\", '/b/c']\n",
			wantDirs: []string{"~/a", "/b/c"},
		},
		{
			name:     "multiline array",
			seedDirs: []string{"~/keep"},
			data:     "extra_skill_dirs = [\n  \"~/a\",\n  \"/b#c\", # quoted hash\n]\n",
			wantDirs: []string{"~/a", "/b#c"},
		},
		{
			name:     "empty array clears existing value",
			seedDirs: []string{"~/keep"},
			data:     "extra_skill_dirs = []\n",
		},
		{
			name:     "quoted key",
			data:     `"extra_skill_dirs" = ["~/quoted-key"]` + "\n",
			wantDirs: []string{"~/quoted-key"},
		},
		{
			name:     "nested table key ignored",
			seedDirs: []string{"~/keep"},
			data:     "[agent]\nextra_skill_dirs = [\"~/ignored\"]\n",
			wantDirs: []string{"~/keep"},
		},
		{
			name:     "legacy setting ignored",
			seedDirs: []string{"~/keep"},
			data:     "merge_all_available_skills = false\n",
			wantDirs: []string{"~/keep"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			settings := kimiSkillSettings{extraSkillDirs: tc.seedDirs}
			parseKimiSkillSettings([]byte(tc.data), &settings)
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
	writeFile(t, filepath.Join(f.home, ".kimi-code", "config.toml"), content)
}

func (f kimiFixture) catalog(cwd string) Catalog {
	return CatalogForProfile("kimi", "kimi", cwd, f.home, nil, "", "0.36.1", "")
}

func TestKimiDiscoversProjectKimiSkills(t *testing.T) {
	f := newKimiFixture(t)
	f.gitRepo(t)
	writeSkill(t, filepath.Join(f.repo, ".kimi-code", "skills"), "deploy", "Ship the service")

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
	writeSkill(t, filepath.Join(f.repo, ".kimi-code", "skills"), "deploy", "Ship the service")
	nested := filepath.Join(f.repo, "services", "api")
	mkdirAll(t, nested)

	if _, ok := commandByName(f.catalog(nested), "/skill:deploy"); !ok {
		t.Fatal("walk-up from a subdirectory did not reach the project root's .kimi-code/skills")
	}
}

func TestKimiMergesProjectBrandAndGenericRoots(t *testing.T) {
	f := newKimiFixture(t)
	f.gitRepo(t)
	writeSkill(t, filepath.Join(f.repo, ".kimi-code", "skills"), "review", "Review a diff")
	writeSkill(t, filepath.Join(f.repo, ".agents", "skills"), "audit", "Audit dependencies")

	catalog := f.catalog(f.repo)
	for _, name := range []string{"/skill:review", "/skill:audit"} {
		command, ok := commandByName(catalog, name)
		if !ok {
			t.Fatalf("%s missing from standalone Kimi Code project roots", name)
		}
		if command.Source != "project" {
			t.Errorf("%s source = %q, want project", name, command.Source)
		}
	}
}

func TestKimiMergesUserBrandAndGenericRoots(t *testing.T) {
	f := newKimiFixture(t)
	writeSkill(t, filepath.Join(f.home, ".kimi-code", "skills"), "deploy", "Ship the service")
	writeSkill(t, filepath.Join(f.home, ".agents", "skills"), "review", "Review a diff")

	catalog := f.catalog(f.repo)
	for _, name := range []string{"/skill:deploy", "/skill:review"} {
		command, ok := commandByName(catalog, name)
		if !ok {
			t.Fatalf("%s missing from standalone Kimi Code user roots", name)
		}
		if command.Source != "personal" {
			t.Errorf("%s source = %q, want personal", name, command.Source)
		}
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
	writeSkill(t, filepath.Join(f.home, ".kimi-code", "skills"), "deploy", "User copy")
	writeSkill(t, filepath.Join(f.repo, ".kimi-code", "skills"), "deploy", "Project copy")

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

func TestKimiIgnoresLegacyBrandRoots(t *testing.T) {
	f := newKimiFixture(t)
	f.gitRepo(t)
	writeSkill(t, filepath.Join(f.repo, ".kimi-code", "skills"), "deploy", "Current copy")
	writeSkill(t, filepath.Join(f.repo, ".codex", "skills"), "legacy", "Legacy copy")
	writeSkill(t, filepath.Join(f.repo, ".kimi", "skills"), "legacy-kimi", "Legacy Kimi copy")
	writeSkill(t, filepath.Join(f.home, ".claude", "skills"), "legacy-user", "Legacy user copy")
	writeSkill(t, filepath.Join(f.home, ".kimi", "skills"), "legacy-kimi-user", "Legacy Kimi user copy")

	command, ok := commandByName(f.catalog(f.repo), "/skill:deploy")
	if !ok || command.Description != "Current copy" {
		t.Fatalf("standalone Kimi Code skill = %+v, present %v", command, ok)
	}
	for _, name := range []string{
		"/skill:legacy",
		"/skill:legacy-user",
		"/skill:legacy-kimi",
		"/skill:legacy-kimi-user",
	} {
		if _, ok := commandByName(f.catalog(f.repo), name); ok {
			t.Errorf("legacy brand root exposed %s", name)
		}
	}
}

func TestKimiDropsSkillWithoutDescription(t *testing.T) {
	f := newKimiFixture(t)
	f.gitRepo(t)
	writeSkill(t, filepath.Join(f.repo, ".kimi-code", "skills"), "deploy", "")

	if _, ok := commandByName(f.catalog(f.repo), "/skill:deploy"); ok {
		t.Error("a skill without a description must not be listed")
	}
}

func TestKimiINIConfiguredFormatSkipsNativeDiscovery(t *testing.T) {
	f := newKimiFixture(t)
	f.gitRepo(t)
	explicit := filepath.Join(f.home, "explicit", "skills")
	writeSkill(t, explicit, "explicit", "Configured by the relay")
	writeSkill(t, filepath.Join(f.repo, ".kimi-code", "skills"), "deploy", "Ship the service")

	catalog := CatalogForProfile("kimi", "kimi", f.repo, f.home,
		[]string{explicit}, "skill:{name}", "", "")
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

func TestKimiDiscoversHomeAgentsSkills(t *testing.T) {
	f := newKimiFixture(t)
	writeSkill(t, filepath.Join(f.home, ".agents", "skills"), "shared", "Shared generic skill")

	if _, ok := commandByName(f.catalog(f.repo), "/skill:shared"); !ok {
		t.Fatal("~/.agents/skills was not scanned")
	}
}

func TestKimiBrandRootBeatsGenericRoot(t *testing.T) {
	f := newKimiFixture(t)
	f.gitRepo(t)
	writeSkill(t, filepath.Join(f.home, ".agents", "skills"), "shared", "User generic copy")
	writeSkill(t, filepath.Join(f.home, ".kimi-code", "skills"), "shared", "User brand copy")
	writeSkill(t, filepath.Join(f.repo, ".agents", "skills"), "review", "Project generic copy")
	writeSkill(t, filepath.Join(f.repo, ".kimi-code", "skills"), "review", "Project brand copy")

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
	writeSkill(t, filepath.Join(f.repo, ".kimi-code", "skills"), "root-skill", "At the project root")
	nested := filepath.Join(f.repo, "services", "api")
	writeSkill(t, filepath.Join(nested, ".kimi-code", "skills"), "nested-skill", "In an intermediate dir")

	catalog := f.catalog(nested)
	if _, ok := commandByName(catalog, "/skill:root-skill"); !ok {
		t.Fatal("the project root's .kimi-code/skills must be scanned from a subdirectory")
	}
	if _, ok := commandByName(catalog, "/skill:nested-skill"); ok {
		t.Error("Kimi resolves project candidates against the project root only, not each ancestor")
	}
}

func TestKimiWithoutGitRootUsesWorkDirectory(t *testing.T) {
	f := newKimiFixture(t)
	writeSkill(t, filepath.Join(f.repo, ".kimi-code", "skills"), "deploy", "Ship the service")

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
	writeSkill(t, filepath.Join(f.home, ".kimi-code", "skills"), "deploy", "User copy")

	command, ok := commandByName(f.catalog(f.repo), "/skill:deploy")
	if !ok {
		t.Fatal("/skill:deploy missing")
	}
	if command.Description != "User copy" {
		t.Errorf("description = %q, want the user copy: User outranks Extra", command.Description)
	}
}

func TestKimiBudgetFavorsHighestPriorityRoot(t *testing.T) {
	f := newKimiFixture(t)
	writeSkill(t, filepath.Join(f.home, ".kimi-code", "skills"), "mine", "The Kimi skill")
	bulk := filepath.Join(f.home, ".agents", "skills")
	for i := range maxCustomFiles {
		writeSkill(t, bulk, fmt.Sprintf("bulk-%03d", i), "Filler")
	}

	catalog := f.catalog(f.repo)
	if !catalog.Truncated {
		t.Error("exhausting the file budget must mark the catalog truncated")
	}
	if _, ok := commandByName(catalog, "/skill:mine"); !ok {
		t.Fatal("KIMI_CODE_HOME/skills must be scanned before ~/.agents/skills spends the budget")
	}
}

// TestKimiEmptyHomeDoesNotScanServiceWorkingDirectory guards the
// os.UserHomeDir failure path: relative user roots must never resolve against
// the relay service's unrelated working directory.
func TestKimiEmptyHomeDoesNotScanServiceWorkingDirectory(t *testing.T) {
	isolateAgentEnv(t)
	repo := t.TempDir()
	scratch := t.TempDir()
	writeSkill(t, filepath.Join(scratch, ".kimi-code", "skills"), "leak-kimi", "Should never be discovered")
	t.Chdir(scratch)

	catalog := CatalogForProfile("kimi", "kimi", repo, "", nil, "", "0.36.1", "")
	if _, ok := commandByName(catalog, "/skill:leak-kimi"); ok {
		t.Fatal("empty ctx.Home must not make Kimi scan the service's own working directory")
	}
}

// TestKimiEmptyHomeDoesNotReadServiceWorkingDirectoryConfig guards the same
// failure for .kimi-code/config.toml: a relative default must not be read from
// the relay service's unrelated working directory.
func TestKimiEmptyHomeDoesNotReadServiceWorkingDirectoryConfig(t *testing.T) {
	isolateAgentEnv(t)
	repo := t.TempDir()
	scratch := t.TempDir()
	extra := filepath.Join(scratch, "extra-skills")
	writeSkill(t, extra, "leak-kimi-config", "Should never be discovered")
	writeFile(t, filepath.Join(scratch, ".kimi-code", "config.toml"),
		fmt.Sprintf("extra_skill_dirs = [%q]\n", extra))
	t.Chdir(scratch)

	catalog := CatalogForProfile("kimi", "kimi", repo, "", nil, "", "0.36.1", "")
	if _, ok := commandByName(catalog, "/skill:leak-kimi-config"); ok {
		t.Fatal("empty ctx.Home must not make Kimi read a config.toml from the service's own working directory")
	}
}

func TestKimiReadsRelocatedHomeConfigAndSkills(t *testing.T) {
	f := newKimiFixture(t)
	kimiHome := filepath.Join(f.home, "kimi-code-home")
	t.Setenv("KIMI_CODE_HOME", kimiHome)
	writeFile(t, filepath.Join(kimiHome, "config.toml"),
		`"extra_skill_dirs" = ["configured-skills"]`+"\n")
	writeSkill(t, filepath.Join(kimiHome, "skills"), "relocated", "From relocated Kimi home")
	writeSkill(t, filepath.Join(f.repo, "configured-skills"), "configured", "From relocated config")

	for _, name := range []string{"/skill:relocated", "/skill:configured"} {
		if _, ok := commandByName(f.catalog(f.repo), name); !ok {
			t.Fatalf("KIMI_CODE_HOME did not expose %s", name)
		}
	}
}

func TestKimiRejectsUnsupportedTildeUserSkillPath(t *testing.T) {
	f := newKimiFixture(t)
	f.config(t, `extra_skill_dirs = ["~another/skills"]`+"\n")
	writeSkill(t, filepath.Join(f.repo, "~another", "skills"), "tilde-user", "Must not resolve relatively")

	if _, ok := commandByName(f.catalog(f.repo), "/skill:tilde-user"); ok {
		t.Fatal("unsupported ~user path was treated as project-relative")
	}
}

func TestKimiDiscoversFlatSkillFiles(t *testing.T) {
	f := newKimiFixture(t)
	writeFile(t, filepath.Join(f.home, ".kimi-code", "skills", "flat.md"),
		"---\nname: flat\ndescription: Flat Kimi skill\n---\n")

	if _, ok := commandByName(f.catalog(f.repo), "/skill:flat"); !ok {
		t.Fatal("flat Kimi skill file was not discovered")
	}
}

func TestKimiCanonicalizesRootsBeforeSpendingBudget(t *testing.T) {
	f := newKimiFixture(t)
	brand := filepath.Join(f.home, ".kimi-code", "skills")
	for index := 0; index < maxCustomFiles-1; index++ {
		writeSkill(t, brand, fmt.Sprintf("bulk-%03d", index), "Filler")
	}
	duplicate := filepath.Join(f.home, "duplicate-brand")
	if err := os.Symlink(brand, duplicate); err != nil {
		t.Fatal(err)
	}
	last := filepath.Join(f.home, "last-extra")
	writeSkill(t, last, "last-extra", "Last available budget slot")
	f.config(t, fmt.Sprintf("extra_skill_dirs = [%q, %q]\n", duplicate, last))

	if _, ok := commandByName(f.catalog(f.repo), "/skill:last-extra"); !ok {
		t.Fatal("duplicate canonical Kimi root spent the remaining discovery budget")
	}
}

func TestKimiDeduplicatesSkillNamesCaseInsensitively(t *testing.T) {
	f := newKimiFixture(t)
	f.gitRepo(t)
	writeSkill(t, filepath.Join(f.repo, ".kimi-code", "skills"), "Deploy", "Project copy")
	writeSkill(t, filepath.Join(f.home, ".kimi-code", "skills"), "deploy", "User copy")

	catalog := f.catalog(f.repo)
	command, ok := commandByName(catalog, "/skill:Deploy")
	if !ok || command.Description != "Project copy" {
		t.Fatalf("case-insensitive winner = %+v, present %v", command, ok)
	}
	if _, ok := commandByName(catalog, "/skill:deploy"); ok {
		t.Fatal("case-only duplicate Kimi skill name was listed")
	}
}
