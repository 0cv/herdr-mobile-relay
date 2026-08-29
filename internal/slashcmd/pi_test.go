package slashcmd

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestPiBuiltinCatalog(t *testing.T) {
	isolateAgentEnv(t)
	catalog := CatalogForProfile("pi", "pi", t.TempDir(), "/nonexistent", nil, "", "0.82.1", "")
	if catalog.Truncated {
		t.Fatal("builtins-only catalog is truncated")
	}
	if len(catalog.Commands) != 22 {
		t.Fatalf("Pi builtins = %d, want 22", len(catalog.Commands))
	}
	for _, name := range []string{"/settings", "/model", "/resume", "/compact", "/quit"} {
		if !hasCommand(catalog, name) {
			t.Errorf("Pi catalog missing %s", name)
		}
	}
	for _, command := range catalog.Commands {
		if command.Source != "builtin" {
			t.Errorf("%s source = %q, want builtin", command.Command, command.Source)
		}
		if command.Description == "" {
			t.Errorf("%s has no description", command.Command)
		}
	}
}

// piFixture builds an isolated home + repo pair for Pi discovery tests.
type piFixture struct {
	home string
	repo string
}

func newPiFixture(t *testing.T) piFixture {
	t.Helper()
	isolateAgentEnv(t)
	root := t.TempDir()
	fixture := piFixture{
		home: filepath.Join(root, "home"),
		repo: filepath.Join(root, "repo"),
	}
	mkdirAll(t, filepath.Join(fixture.home, ".pi", "agent"))
	mkdirAll(t, fixture.repo)
	writeFile(t, filepath.Join(fixture.home, ".pi", "agent", "trust.json"),
		fmt.Sprintf("{%q:true}", fixture.repo))
	return fixture
}

func (f piFixture) gitRepo(t *testing.T) {
	t.Helper()
	mkdirAll(t, filepath.Join(f.repo, ".git"))
}

func (f piFixture) catalog(cwd string) Catalog {
	return CatalogForProfile("pi", "pi", cwd, f.home, nil, "", "0.82.1", "")
}

func TestPiDiscoversProjectPiSkills(t *testing.T) {
	f := newPiFixture(t)
	f.gitRepo(t)
	writeSkill(t, filepath.Join(f.repo, ".pi", "skills"), "deploy", "Ship the service")

	command, ok := commandByName(f.catalog(f.repo), "/skill:deploy")
	if !ok {
		t.Fatal("/skill:deploy missing from the Pi catalog")
	}
	if command.Source != "project" {
		t.Errorf("source = %q, want project", command.Source)
	}
	if command.Description != "Ship the service" {
		t.Errorf("description = %q", command.Description)
	}
}

func TestPiDoesNotInheritAncestorPiSkills(t *testing.T) {
	f := newPiFixture(t)
	f.gitRepo(t)
	writeSkill(t, filepath.Join(f.repo, ".pi", "skills"), "deploy", "Ship the service")
	nested := filepath.Join(f.repo, "services", "api")
	mkdirAll(t, nested)

	if _, ok := commandByName(f.catalog(nested), "/skill:deploy"); ok {
		t.Fatal("Pi must scan .pi/skills only in the current working directory")
	}
}

func TestPiDiscoversProjectAgentsSkills(t *testing.T) {
	f := newPiFixture(t)
	f.gitRepo(t)
	writeSkill(t, filepath.Join(f.repo, ".agents", "skills"), "shared", "Shared across agents")

	command, ok := commandByName(f.catalog(f.repo), "/skill:shared")
	if !ok {
		t.Fatal("Pi must discover a project .agents/skills directory")
	}
	if command.Source != "project" {
		t.Errorf("source = %q, want project", command.Source)
	}
}

func TestPiDiscoversUserSkillsFromAgentDir(t *testing.T) {
	f := newPiFixture(t)
	writeSkill(t, filepath.Join(f.home, ".pi", "agent", "skills"), "mine", "My own skill")

	command, ok := commandByName(f.catalog(f.repo), "/skill:mine")
	if !ok {
		t.Fatal("Pi must discover its own agent skills directory")
	}
	if command.Source != "personal" {
		t.Errorf("source = %q, want personal", command.Source)
	}
}

func TestPiDiscoversUserAgentsSkills(t *testing.T) {
	f := newPiFixture(t)
	writeSkill(t, filepath.Join(f.home, ".agents", "skills"), "generic", "Generic user skill")

	command, ok := commandByName(f.catalog(f.repo), "/skill:generic")
	if !ok {
		t.Fatal("Pi must discover ~/.agents/skills")
	}
	if command.Source != "personal" {
		t.Errorf("source = %q, want personal", command.Source)
	}
}

func TestPiDiscoversUserSkillsFromActiveProfileAgentDir(t *testing.T) {
	f := newPiFixture(t)
	agentDir := filepath.Join(f.home, ".pi", "profiles", "personal", "agent")
	writeSkill(t, filepath.Join(agentDir, "skills"), "prof", "From a named profile")
	writeSkill(t, filepath.Join(f.home, ".pi", "profiles", "other", "agent", "skills"),
		"other", "From another named profile")

	catalog := CatalogForProfile("pi", "pi", f.repo, f.home, nil, "", "0.82.1", agentDir)
	command, ok := commandByName(catalog, "/skill:prof")
	if !ok {
		t.Fatal("the active named profile's skills must be discovered")
	}
	if command.Source != "personal" {
		t.Errorf("source = %q, want personal", command.Source)
	}
	if _, ok := commandByName(catalog, "/skill:other"); ok {
		t.Fatal("another named profile's skill leaked into the active catalog")
	}
}

func TestPiSkillCommandsDisabledYieldsBuiltinsOnly(t *testing.T) {
	f := newPiFixture(t)
	f.gitRepo(t)
	writeSkill(t, filepath.Join(f.repo, ".pi", "skills"), "deploy", "Ship the service")
	writeFile(t, filepath.Join(f.home, ".pi", "agent", "settings.json"),
		"{\"enableSkillCommands\": false}\n")

	catalog := f.catalog(f.repo)
	if len(catalog.Commands) != len(piBuiltins) {
		t.Fatalf("commands = %d, want %d builtins", len(catalog.Commands), len(piBuiltins))
	}
	if countSource(catalog, "builtin") != len(piBuiltins) {
		t.Error("every command should be a builtin")
	}
}

func TestPiMalformedSettingsFailsOpen(t *testing.T) {
	f := newPiFixture(t)
	f.gitRepo(t)
	writeSkill(t, filepath.Join(f.repo, ".pi", "skills"), "deploy", "Ship the service")
	writeFile(t, filepath.Join(f.home, ".pi", "agent", "settings.json"), "{not json")

	if _, ok := commandByName(f.catalog(f.repo), "/skill:deploy"); !ok {
		t.Fatal("an unparsable settings.json must leave skill commands registered")
	}
}

func TestPiAbsentSettingsRegistersSkills(t *testing.T) {
	f := newPiFixture(t)
	f.gitRepo(t)
	writeSkill(t, filepath.Join(f.repo, ".pi", "skills"), "deploy", "Ship the service")

	if _, ok := commandByName(f.catalog(f.repo), "/skill:deploy"); !ok {
		t.Fatal("without settings.json Pi's documented default registers skill commands")
	}
}

func TestPiProjectSkillBeatsUserSkill(t *testing.T) {
	f := newPiFixture(t)
	f.gitRepo(t)
	writeSkill(t, filepath.Join(f.home, ".pi", "agent", "skills"), "deploy", "user description")
	writeSkill(t, filepath.Join(f.repo, ".pi", "skills"), "deploy", "project description")

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
	if command.Description != "project description" || command.Source != "project" {
		t.Fatalf("project scope should win, got %+v", command)
	}
}

func TestPiDropsSkillWithoutDescription(t *testing.T) {
	f := newPiFixture(t)
	f.gitRepo(t)
	writeSkill(t, filepath.Join(f.repo, ".pi", "skills"), "nodesc", "")

	if _, ok := commandByName(f.catalog(f.repo), "/skill:nodesc"); ok {
		t.Fatal("a skill without a description must not be listed")
	}
}

func TestPiINIConfiguredFormatSkipsNativeDiscovery(t *testing.T) {
	f := newPiFixture(t)
	f.gitRepo(t)
	writeSkill(t, filepath.Join(f.repo, ".pi", "skills"), "deploy", "Ship the service")
	explicit := filepath.Join(f.home, "explicit", "skills")
	writeSkill(t, explicit, "explicit-one", "Explicitly configured")

	catalog := CatalogForProfile("pi", "pi", f.repo, f.home,
		[]string{explicit}, "skill:{name}", "0.82.1", "")
	if _, ok := commandByName(catalog, "/skill:explicit-one"); !ok {
		t.Error("the INI-configured directory must be scanned")
	}
	if _, ok := commandByName(catalog, "/skill:deploy"); ok {
		t.Error("explicit configuration outranks discovery, so native discovery is skipped")
	}
}

func TestPiBrandBeatsGenericAtUserScope(t *testing.T) {
	f := newPiFixture(t)
	writeSkill(t, filepath.Join(f.home, ".pi", "agent", "skills"), "deploy", "Brand copy")
	writeSkill(t, filepath.Join(f.home, ".agents", "skills"), "deploy", "Generic copy")

	command, ok := commandByName(f.catalog(f.repo), "/skill:deploy")
	if !ok {
		t.Fatal("/skill:deploy missing")
	}
	if command.Description != "Brand copy" {
		t.Errorf("description = %q, want the brand copy: Pi's own agent directories outrank ~/.agents",
			command.Description)
	}
}

func TestPiBrandBeatsGenericAtProjectScope(t *testing.T) {
	f := newPiFixture(t)
	f.gitRepo(t)
	writeSkill(t, filepath.Join(f.repo, ".pi", "skills"), "deploy", "Brand copy")
	writeSkill(t, filepath.Join(f.repo, ".agents", "skills"), "deploy", "Generic copy")

	command, ok := commandByName(f.catalog(f.repo), "/skill:deploy")
	if !ok {
		t.Fatal("/skill:deploy missing")
	}
	if command.Description != "Brand copy" {
		t.Errorf("description = %q, want the brand copy: .pi outranks .agents within one ancestor",
			command.Description)
	}
}

func TestPiInactiveProfileSettingsDoNotOverrideDefault(t *testing.T) {
	f := newPiFixture(t)
	f.gitRepo(t)
	writeSkill(t, filepath.Join(f.repo, ".pi", "skills"), "deploy", "Ship the service")
	writeFile(t, filepath.Join(f.home, ".pi", "profiles", "personal", "agent", "settings.json"),
		strings.Repeat(" ", maxSettingsSize+1))
	writeFile(t, filepath.Join(f.home, ".pi", "agent", "settings.json"),
		"{\"enableSkillCommands\": false}\n")

	if _, ok := commandByName(f.catalog(f.repo), "/skill:deploy"); ok {
		t.Fatal("an inactive profile must not make the default profile ignore its own settings")
	}
}

// TestPiEmptyHomeDoesNotScanServiceWorkingDirectory guards the
// os.UserHomeDir() failure path in server.go: when ctx.Home is "" the
// ~/.agents/skills join becomes relative, and a relative dir must never
// resolve against the relay service's own working directory - which is
// arbitrary and unrelated to any project - instead of simply contributing
// nothing.
func TestPiEmptyHomeDoesNotScanServiceWorkingDirectory(t *testing.T) {
	isolateAgentEnv(t)
	repo := t.TempDir()
	scratch := t.TempDir()
	writeSkill(t, filepath.Join(scratch, ".agents", "skills"), "leak-pi", "Should never be discovered")
	t.Chdir(scratch)

	catalog := CatalogForProfile("pi", "pi", repo, "", nil, "", "0.82.1", "")
	if _, ok := commandByName(catalog, "/skill:leak-pi"); ok {
		t.Fatal("empty ctx.Home must not make Pi scan the service's own working directory")
	}
}

func TestPiInheritsAgentsSkillsFromGitAncestor(t *testing.T) {
	f := newPiFixture(t)
	f.gitRepo(t)
	writeSkill(t, filepath.Join(f.repo, ".agents", "skills"), "ancestor-agent", "Inherited generic skill")
	nested := filepath.Join(f.repo, "services", "api")
	mkdirAll(t, nested)

	if _, ok := commandByName(f.catalog(nested), "/skill:ancestor-agent"); !ok {
		t.Fatal("Pi did not inherit .agents/skills from the git root")
	}
}

func TestPiInheritsAgentsSkillsOutsideGit(t *testing.T) {
	f := newPiFixture(t)
	parent := filepath.Join(f.repo, "workspace")
	nested := filepath.Join(parent, "services", "api")
	mkdirAll(t, nested)
	writeSkill(t, filepath.Join(parent, ".agents", "skills"), "outside-git", "Filesystem ancestor")

	if _, ok := commandByName(f.catalog(nested), "/skill:outside-git"); !ok {
		t.Fatal("Pi did not inherit .agents/skills from a filesystem ancestor")
	}
}

func TestPiDiscoversGlobalSettingsSkillArray(t *testing.T) {
	f := newPiFixture(t)
	configured := filepath.Join(f.home, ".pi", "agent", "configured")
	writeSkill(t, configured, "from-settings", "Configured globally")
	writeFile(t, filepath.Join(f.home, ".pi", "agent", "settings.json"),
		`{"skills":["configured"]}`)

	if _, ok := commandByName(f.catalog(f.repo), "/skill:from-settings"); !ok {
		t.Fatal("global Pi settings skills array was not discovered")
	}
}

func TestPiTrustedProjectSettingsAppendGlobalSettings(t *testing.T) {
	f := newPiFixture(t)
	global := filepath.Join(f.home, ".pi", "agent", "global")
	project := filepath.Join(f.repo, ".pi", "project")
	writeSkill(t, global, "global-setting", "Global setting")
	writeSkill(t, project, "project-setting", "Project setting")
	writeFile(t, filepath.Join(f.home, ".pi", "agent", "settings.json"),
		`{"enableSkillCommands":false,"skills":["global"]}`)
	writeFile(t, filepath.Join(f.home, ".pi", "agent", "trust.json"),
		fmt.Sprintf("{%q:true}", f.repo))
	writeFile(t, filepath.Join(f.repo, ".pi", "settings.json"),
		`{"enableSkillCommands":true,"skills":["project"]}`)

	catalog := f.catalog(f.repo)
	if _, ok := commandByName(catalog, "/skill:project-setting"); !ok {
		t.Fatal("trusted project Pi settings did not enable project skill paths")
	}
	if _, ok := commandByName(catalog, "/skill:global-setting"); !ok {
		t.Fatal("project Pi settings skills must append to the global skills")
	}
}

func TestPiUntrustedProjectSettingsDoNotOverrideGlobal(t *testing.T) {
	f := newPiFixture(t)
	writeSkill(t, filepath.Join(f.home, ".pi", "agent", "skills"), "personal", "Personal skill")
	writeSkill(t, filepath.Join(f.repo, ".pi", "skills"), "untrusted-project", "Untrusted project")
	writeFile(t, filepath.Join(f.home, ".pi", "agent", "trust.json"),
		fmt.Sprintf("{%q:false}", f.repo))
	writeFile(t, filepath.Join(f.repo, ".pi", "settings.json"),
		`{"enableSkillCommands":false}`)

	catalog := f.catalog(f.repo)
	if _, ok := commandByName(catalog, "/skill:personal"); !ok {
		t.Fatal("untrusted project settings overrode the global defaults")
	}
	if _, ok := commandByName(catalog, "/skill:untrusted-project"); ok {
		t.Fatal("untrusted project skills were discovered")
	}
}

func TestPiSupportsLegacyNestedSkillCommandSetting(t *testing.T) {
	f := newPiFixture(t)
	writeSkill(t, filepath.Join(f.home, ".pi", "agent", "skills"), "personal", "Personal skill")
	writeFile(t, filepath.Join(f.home, ".pi", "agent", "settings.json"),
		`{"skills":{"enableSkillCommands":false}}`)

	if _, ok := commandByName(f.catalog(f.repo), "/skill:personal"); ok {
		t.Fatal("legacy nested skills.enableSkillCommands=false was ignored")
	}
}

func TestPiDefaultProjectTrustAlwaysEnablesProjectSettings(t *testing.T) {
	f := newPiFixture(t)
	writeFile(t, filepath.Join(f.home, ".pi", "agent", "trust.json"), `{}`)
	writeFile(t, filepath.Join(f.home, ".pi", "agent", "settings.json"),
		`{"defaultProjectTrust":"always"}`)
	writeSkill(t, filepath.Join(f.repo, ".pi", "skills"), "default-trusted", "Default trusted project")

	if _, ok := commandByName(f.catalog(f.repo), "/skill:default-trusted"); !ok {
		t.Fatal("defaultProjectTrust=always did not enable project resources")
	}
}

func TestPiDiscoversNestedAndFlatSkills(t *testing.T) {
	f := newPiFixture(t)
	root := filepath.Join(f.home, ".pi", "agent", "skills")
	writeSkill(t, filepath.Join(root, "group"), "nested", "Nested skill")
	writeFile(t, filepath.Join(root, "flat.md"), "---\nname: flat\ndescription: Flat skill\n---\n")

	catalog := f.catalog(f.repo)
	for _, name := range []string{"/skill:nested", "/skill:flat"} {
		if _, ok := commandByName(catalog, name); !ok {
			t.Errorf("%s missing from recursive Pi discovery", name)
		}
	}
}

func TestPiInterpretsSkillIncludeExcludePatterns(t *testing.T) {
	f := newPiFixture(t)
	root := filepath.Join(f.home, ".pi", "agent", "configured")
	writeSkill(t, root, "included", "Included skill")
	writeSkill(t, root, "excluded", "Excluded skill")
	writeFile(t, filepath.Join(f.home, ".pi", "agent", "settings.json"),
		`{"skills":["configured","!configured/**","+configured/included/**"]}`)

	catalog := f.catalog(f.repo)
	if _, ok := commandByName(catalog, "/skill:included"); !ok {
		t.Fatal("+ pattern did not restore the included Pi skill")
	}
	if _, ok := commandByName(catalog, "/skill:excluded"); ok {
		t.Fatal("! pattern did not exclude the matching Pi skill")
	}
}

func TestPiTrustUsesCanonicalUncappedAncestors(t *testing.T) {
	f := newPiFixture(t)
	real := filepath.Join(f.repo, "real")
	deep := real
	for index := 0; index < 40; index++ {
		deep = filepath.Join(deep, fmt.Sprintf("d%02d", index))
	}
	mkdirAll(t, deep)
	writeFile(t, filepath.Join(f.home, ".pi", "agent", "trust.json"),
		fmt.Sprintf("{%q:true}", real))
	link := filepath.Join(f.repo, "linked")
	if err := os.Symlink(deep, link); err != nil {
		t.Fatal(err)
	}

	if !piProjectTrusted(filepath.Join(f.home, ".pi", "agent"), link, "") {
		t.Fatal("Pi trust did not canonicalize cwd or search beyond 32 ancestors")
	}
}

func TestPiMalformedTrustStoreFailsClosed(t *testing.T) {
	f := newPiFixture(t)
	writeFile(t, filepath.Join(f.home, ".pi", "agent", "settings.json"),
		`{"defaultProjectTrust":"always"}`)
	writeFile(t, filepath.Join(f.home, ".pi", "agent", "trust.json"), `{malformed`)
	writeSkill(t, filepath.Join(f.repo, ".pi", "skills"), "must-stay-untrusted", "Project skill")

	if _, ok := commandByName(f.catalog(f.repo), "/skill:must-stay-untrusted"); ok {
		t.Fatal("malformed Pi trust store fell through to defaultProjectTrust=always")
	}
}
