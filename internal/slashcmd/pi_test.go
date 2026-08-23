package slashcmd

import (
	"path/filepath"
	"strings"
	"testing"
)

func TestPiBuiltinCatalog(t *testing.T) {
	isolateAgentEnv(t)
	catalog := CatalogForProfile("pi", "pi", t.TempDir(), "/nonexistent", nil, "", "0.82.1")
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
	return fixture
}

func (f piFixture) gitRepo(t *testing.T) {
	t.Helper()
	mkdirAll(t, filepath.Join(f.repo, ".git"))
}

func (f piFixture) catalog(cwd string) Catalog {
	return CatalogForProfile("pi", "pi", cwd, f.home, nil, "", "0.82.1")
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

func TestPiDiscoversProjectSkillsFromSubdirectory(t *testing.T) {
	f := newPiFixture(t)
	f.gitRepo(t)
	writeSkill(t, filepath.Join(f.repo, ".pi", "skills"), "deploy", "Ship the service")
	nested := filepath.Join(f.repo, "services", "api")
	mkdirAll(t, nested)

	if _, ok := commandByName(f.catalog(nested), "/skill:deploy"); !ok {
		t.Fatal("walk-up from a subdirectory did not reach the git root's .pi/skills")
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

func TestPiDiscoversUserSkillsFromProfileAgentDir(t *testing.T) {
	f := newPiFixture(t)
	writeSkill(t, filepath.Join(f.home, ".pi", "profiles", "personal", "agent", "skills"),
		"prof", "From a named profile")

	command, ok := commandByName(f.catalog(f.repo), "/skill:prof")
	if !ok {
		t.Fatal("a named profile's skills must be discovered")
	}
	if command.Source != "personal" {
		t.Errorf("source = %q, want personal", command.Source)
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
		[]string{explicit}, "skill:{name}", "0.82.1")
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

func TestPiUnusableSettingsStopsAtFirstConfigDir(t *testing.T) {
	f := newPiFixture(t)
	f.gitRepo(t)
	writeSkill(t, filepath.Join(f.repo, ".pi", "skills"), "deploy", "Ship the service")
	writeFile(t, filepath.Join(f.home, ".pi", "profiles", "personal", "agent", "settings.json"),
		strings.Repeat(" ", maxSettingsSize+1))
	writeFile(t, filepath.Join(f.home, ".pi", "agent", "settings.json"),
		"{\"enableSkillCommands\": false}\n")

	if _, ok := commandByName(f.catalog(f.repo), "/skill:deploy"); !ok {
		t.Fatal("first-found wins: an unusable settings.json in the first agent directory must stop the search and fail open, not fall through to another profile's settings")
	}
}
