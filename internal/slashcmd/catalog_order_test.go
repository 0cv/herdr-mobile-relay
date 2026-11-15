package slashcmd

import (
	"path/filepath"
	"reflect"
	"testing"
)

func catalogPairs(catalog Catalog) []string {
	pairs := make([]string, 0, len(catalog.Commands))
	for _, command := range catalog.Commands {
		pairs = append(pairs, command.Command+"|"+command.Source)
	}
	return pairs
}

// wantOrdered lists the builtins in order, with the source of a builtin replaced
// by overrides[command] or the builtin dropped when that value is empty, then
// the tail of commands that were added after the builtins.
func wantOrdered(builtins []Command, overrides map[string]string, tail ...string) []string {
	want := make([]string, 0, len(builtins)+len(tail))
	for _, builtin := range builtins {
		source, overridden := overrides[builtin.Command]
		if !overridden {
			source = builtin.Source
		}
		if source != "" {
			want = append(want, builtin.Command+"|"+source)
		}
	}
	return append(want, tail...)
}

func assertOrdered(t *testing.T, catalog Catalog, want []string) {
	t.Helper()
	if got := catalogPairs(catalog); !reflect.DeepEqual(got, want) {
		t.Fatalf("catalog order:\n got %q\nwant %q", got, want)
	}
}

func writeCommand(t *testing.T, path, description string) {
	t.Helper()
	writeFile(t, path, "---\ndescription: "+description+"\n---\nBody\n")
}

func TestClaudeCatalogOrder(t *testing.T) {
	isolateAgentEnv(t)
	home, repo := t.TempDir(), t.TempDir()
	cwd := filepath.Join(repo, "sub")
	mkdirAll(t, filepath.Join(repo, ".git"))
	mkdirAll(t, cwd)

	outer := filepath.Join(repo, ".claude")
	writeCommand(t, filepath.Join(outer, "commands", "help.md"), "Outer help")
	writeCommand(t, filepath.Join(outer, "commands", "deploy.md"), "Outer deploy")
	writeFile(t, filepath.Join(outer, "commands", "review.md"), "---\nuser-invocable: false\n---\nBody\n")
	writeSkill(t, filepath.Join(outer, "skills"), "alpha", "Outer alpha")
	writeFile(t, filepath.Join(outer, "settings.json"), `{"skillOverrides":{"stats":"on"}}`)

	inner := filepath.Join(cwd, ".claude")
	writeCommand(t, filepath.Join(inner, "commands", "deploy.md"), "Inner deploy")
	writeCommand(t, filepath.Join(inner, "commands", "zeta.md"), "Inner zeta")

	personal := filepath.Join(home, ".claude")
	writeCommand(t, filepath.Join(personal, "commands", "help.md"), "Personal help")
	writeCommand(t, filepath.Join(personal, "commands", "deploy.md"), "Personal deploy")
	writeCommand(t, filepath.Join(personal, "commands", "beta.md"), "Personal beta")
	writeSkill(t, filepath.Join(personal, "skills"), "gamma", "Personal gamma")
	writeSkill(t, filepath.Join(personal, "skills"), "review", "Personal review")
	writeFile(t, filepath.Join(personal, "settings.json"),
		`{"skillOverrides":{"plan":"off","stats":"off","gamma":"off"}}`)

	catalog := catalogFor("claude", cwd, home)
	assertOrdered(t, catalog, wantOrdered(claudeBuiltins,
		map[string]string{"/help": "personal", "/review": "", "/plan": ""},
		"/deploy|personal", "/alpha|project", "/zeta|project", "/beta|personal", "/review|personal",
	))
}

func TestQoderCatalogOrder(t *testing.T) {
	isolateAgentEnv(t)
	home, repo := t.TempDir(), t.TempDir()
	cwd := filepath.Join(repo, "sub")
	mkdirAll(t, filepath.Join(repo, ".git"))
	mkdirAll(t, cwd)

	outer := filepath.Join(repo, ".qoder")
	writeCommand(t, filepath.Join(outer, "commands", "help.md"), "Outer help")
	writeCommand(t, filepath.Join(outer, "commands", "deploy.md"), "Outer deploy")
	writeFile(t, filepath.Join(outer, "commands", "cost.md"), "---\nuser-invocable: false\n---\nBody\n")
	writeSkill(t, filepath.Join(outer, "skills"), "alpha", "Outer alpha")
	writeSkill(t, filepath.Join(outer, "skills"), "shared", "Outer shared")

	inner := filepath.Join(cwd, ".qoder")
	writeCommand(t, filepath.Join(inner, "commands", "deploy.md"), "Inner deploy")
	writeSkill(t, filepath.Join(inner, "skills"), "shared", "Inner shared")

	personal := filepath.Join(home, ".qoder")
	writeCommand(t, filepath.Join(personal, "commands", "help.md"), "Personal help")
	writeCommand(t, filepath.Join(personal, "commands", "deploy.md"), "Personal deploy")
	writeFile(t, filepath.Join(personal, "commands", "copy.md"), "---\nuser-invocable: false\n---\nBody\n")
	writeSkill(t, filepath.Join(personal, "skills"), "shared", "Personal shared")
	writeSkill(t, filepath.Join(personal, "skills"), "status", "Personal status")

	catalog := catalogFor("qoder", cwd, home)
	assertOrdered(t, catalog, wantOrdered(qoderBuiltins,
		map[string]string{"/help": "", "/cost": "", "/copy": "", "/status": ""},
		"/deploy|personal", "/help|personal", "/shared|personal", "/status|personal",
		"/deploy|project", "/help|project", "/deploy|project", "/alpha|project",
	))
}

func TestKimiCatalogOrder(t *testing.T) {
	f := newKimiFixture(t)
	f.gitRepo(t)
	project := f.repo
	writeSkill(t, filepath.Join(project, ".kimi-code", "skills"), "Deploy", "Project Deploy")
	writeSkill(t, filepath.Join(project, ".kimi-code", "skills"), "build", "Project build")
	writeSkill(t, filepath.Join(project, ".agents", "skills"), "build", "Agents build")
	writeSkill(t, filepath.Join(project, ".agents", "skills"), "agents-only", "Agents only")
	writeSkill(t, filepath.Join(f.home, ".kimi-code", "skills"), "deploy", "Personal deploy")
	writeSkill(t, filepath.Join(f.home, ".kimi-code", "skills"), "tools", "Personal tools")
	writeSkill(t, filepath.Join(f.home, ".agents", "skills"), "help", "Personal help")
	writeSkill(t, filepath.Join(f.home, ".agents", "skills"), "TOOLS", "Agents tools")

	assertOrdered(t, f.catalog(f.repo), wantOrdered(kimiBuiltins, nil,
		"/skill:Deploy|project", "/skill:build|project", "/skill:agents-only|project",
		"/skill:tools|personal", "/skill:help|personal",
	))
}

func TestPiCatalogOrder(t *testing.T) {
	f := newPiFixture(t)
	f.gitRepo(t)
	writeSkill(t, filepath.Join(f.repo, ".pi", "skills"), "deploy", "Project deploy")
	writeSkill(t, filepath.Join(f.repo, ".pi", "skills"), "build", "Project build")
	writeSkill(t, filepath.Join(f.repo, ".agents", "skills"), "build", "Agents build")
	writeSkill(t, filepath.Join(f.repo, ".agents", "skills"), "agents-only", "Agents only")
	writeSkill(t, filepath.Join(f.home, ".pi", "agent", "skills"), "deploy", "Personal deploy")
	writeSkill(t, filepath.Join(f.home, ".pi", "agent", "skills"), "tools", "Personal tools")
	writeSkill(t, filepath.Join(f.home, ".agents", "skills"), "help", "Personal help")
	writeSkill(t, filepath.Join(f.home, ".agents", "skills"), "tools", "Agents tools")

	assertOrdered(t, f.catalog(f.repo), wantOrdered(piBuiltins, nil,
		"/skill:build|project", "/skill:deploy|project", "/skill:agents-only|project",
		"/skill:tools|personal", "/skill:help|personal",
	))
}

func TestOMPCatalogOrder(t *testing.T) {
	f := newOMPFixture(t)
	f.gitRepo(t)
	writeSkill(t, filepath.Join(f.repo, ".omp", "skills"), "alpha", "Project alpha")
	writeSkill(t, filepath.Join(f.repo, ".omp", "skills"), "build", "Project build")
	writeSkill(t, filepath.Join(f.repo, ".claude", "skills"), "build", "Claude build")
	writeSkill(t, filepath.Join(f.repo, ".claude", "skills"), "claude-only", "Claude only")
	agent := filepath.Join(f.home, ".omp", "agent")
	writeSkill(t, filepath.Join(agent, "skills"), "deploy", "Personal deploy")
	writeSkill(t, filepath.Join(agent, "skills"), "tools", "Personal tools")
	writeSkill(t, filepath.Join(f.home, ".claude", "skills"), "help", "Claude help")
	writeSkill(t, filepath.Join(f.home, ".claude", "skills"), "tools", "Claude tools")
	custom := filepath.Join(f.repo, "custom-skills")
	writeSkill(t, custom, "deploy", "Custom deploy")
	writeSkill(t, custom, "tools", "Custom tools")
	writeSkill(t, custom, "custom-only", "Custom only")
	writeFile(t, filepath.Join(f.repo, ".omp", "config.yml"), "skills:\n  customDirectories:\n    - custom-skills\n")
	writeSkill(t, filepath.Join(agent, "managed-skills"), "deploy", "Managed deploy")
	writeSkill(t, filepath.Join(agent, "managed-skills"), "managed-only", "Managed only")

	assertOrdered(t, f.catalog(f.repo), wantOrdered(ompBuiltins, nil,
		"/skill:alpha|project", "/skill:build|project", "/skill:deploy|project", "/skill:tools|project",
		"/skill:claude-only|project", "/skill:help|personal", "/skill:custom-only|project",
		"/skill:managed-only|personal",
	))
}

func TestHermesCatalogOrder(t *testing.T) {
	isolateAgentEnv(t)
	home, repo, configured := t.TempDir(), t.TempDir(), t.TempDir()
	mkdirAll(t, filepath.Join(repo, ".git"))
	for _, scope := range []struct{ dir, only string }{
		{filepath.Join(repo, ".hermes", "skills"), "project-only"},
		{filepath.Join(home, ".hermes", "skills"), "personal-only"},
		{configured, "configured-only"},
	} {
		writeSkill(t, scope.dir, "help", "Help skill")
		writeSkill(t, scope.dir, "deploy", "Deploy skill")
		writeSkill(t, scope.dir, scope.only, "Only skill")
	}

	catalog := CatalogFor("hermes", "hermes", DiscoverContext{
		Cwd:           repo,
		Home:          home,
		SkillDirs:     []string{configured},
		CommandFormat: "/{name}",
	})
	assertOrdered(t, catalog, wantOrdered(hermesBuiltins, nil,
		"/deploy|project", "/project-only|project", "/personal-only|personal", "/configured-only|personal",
	))
}

func TestCursorCatalogOrder(t *testing.T) {
	isolateAgentEnv(t)
	home, repo := t.TempDir(), t.TempDir()
	writeCommand(t, filepath.Join(repo, ".claude", "commands", "help.md"), "Project help")
	writeCommand(t, filepath.Join(repo, ".claude", "commands", "new.md"), "Project new")
	writeCommand(t, filepath.Join(repo, ".claude", "commands", "deploy.md"), "Claude deploy")
	writeCommand(t, filepath.Join(repo, ".cursor", "commands", "deploy.md"), "Cursor deploy")
	writeCommand(t, filepath.Join(repo, ".cursor", "commands", "alpha.md"), "Cursor alpha")
	writeCommand(t, filepath.Join(home, ".cursor", "commands", "Deploy.md"), "Personal Deploy")
	writeCommand(t, filepath.Join(home, ".cursor", "commands", "Plan.md"), "Personal Plan")
	writeCommand(t, filepath.Join(home, ".claude", "commands", "beta.md"), "Personal beta")
	writeSkill(t, filepath.Join(repo, ".cursor", "skills"), "deploy", "Project deploy skill")
	writeSkill(t, filepath.Join(repo, ".cursor", "skills"), "shared", "Project shared")
	writeSkill(t, filepath.Join(home, ".cursor", "skills"), "shared", "Personal shared")
	writeSkill(t, filepath.Join(home, ".cursor", "skills"), "Mixed-Case", "Personal mixed")
	writeSkill(t, filepath.Join(home, ".agents", "skills"), "mixed-case", "Agents mixed")

	catalog := catalogFor("cursor", repo, home)
	assertOrdered(t, catalog, wantOrdered(cursorBuiltins, nil,
		"/deploy|project", "/alpha|project", "/beta|personal", "/shared|personal", "/Mixed-Case|personal",
	))
}

func TestOpenCodeCatalogOrder(t *testing.T) {
	home, repo := t.TempDir(), t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", "")
	mkdirAll(t, filepath.Join(repo, ".git"))
	cwd := filepath.Join(repo, "sub")
	mkdirAll(t, cwd)
	writeCommand(t, filepath.Join(home, ".config", "opencode", "commands", "help.md"), "Personal help")
	writeCommand(t, filepath.Join(home, ".config", "opencode", "commands", "deploy.md"), "Personal deploy")
	writeCommand(t, filepath.Join(home, ".config", "opencode", "command", "personal-only.md"), "Personal only")
	writeCommand(t, filepath.Join(repo, ".opencode", "commands", "deploy.md"), "Outer deploy")
	writeCommand(t, filepath.Join(repo, ".opencode", "commands", "help.md"), "Outer help")
	writeCommand(t, filepath.Join(repo, ".opencode", "command", "outer-only.md"), "Outer only")
	writeCommand(t, filepath.Join(cwd, ".opencode", "commands", "deploy.md"), "Inner deploy")
	writeCommand(t, filepath.Join(cwd, ".opencode", "commands", "git", "review.md"), "Inner review")

	catalog := catalogFor("opencode", cwd, home)
	assertOrdered(t, catalog, wantOrdered(openCodeBuiltins, nil,
		"/personal-only|personal", "/deploy|project", "/outer-only|project", "/git/review|project",
	))
}

func TestAgyCatalogOrder(t *testing.T) {
	isolateAgentEnv(t)
	home, configured := t.TempDir(), t.TempDir()
	for _, scope := range []struct{ dir, only string }{
		{filepath.Join(home, ".gemini", "skills"), "personal-only"},
		{configured, "configured-only"},
	} {
		writeSkill(t, scope.dir, "help", "Help skill")
		writeSkill(t, scope.dir, "deploy", "Deploy skill")
		writeSkill(t, scope.dir, scope.only, "Only skill")
	}

	catalog := CatalogFor("agy", "agy", DiscoverContext{Cwd: t.TempDir(), Home: home, SkillDirs: []string{configured}})
	assertOrdered(t, catalog, wantOrdered(agyBuiltins, nil,
		"/deploy|personal", "/personal-only|personal", "/configured-only|project",
	))
}

func TestConfiguredFormatKeepsBuiltinsFirst(t *testing.T) {
	cases := []struct {
		profile  string
		builtins []Command
	}{
		{"kimi", kimiBuiltins},
		{"pi", piBuiltins},
		{"omp", ompBuiltins},
	}
	for _, tc := range cases {
		t.Run(tc.profile, func(t *testing.T) {
			isolateAgentEnv(t)
			home, cwd := t.TempDir(), t.TempDir()
			personal, project := t.TempDir(), t.TempDir()
			builtinName := tc.builtins[0].Command[1:]
			writeSkill(t, personal, "zeta", "Personal zeta")
			writeSkill(t, personal, builtinName, "Must not replace the builtin")
			writeSkill(t, personal, "alpha", "Personal alpha")
			writeSkill(t, project, "beta", "Project beta")

			catalog := CatalogFor(tc.profile, tc.profile, DiscoverContext{
				Cwd:           cwd,
				Home:          home,
				SkillDirs:     []string{personal, project},
				CommandFormat: "/{name}",
			})
			assertOrdered(t, catalog, wantOrdered(tc.builtins, nil,
				"/alpha|personal", "/zeta|personal", "/beta|project",
			))

			catalog = CatalogFor(tc.profile, tc.profile, DiscoverContext{Cwd: cwd, Home: home, CommandFormat: "/{name}"})
			assertOrdered(t, catalog, wantOrdered(tc.builtins, nil))
		})
	}
}
