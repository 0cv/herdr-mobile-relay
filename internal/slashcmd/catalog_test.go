package slashcmd

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func TestSlashCommandLimitsMatchContract(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("..", "..", "contracts", "fixtures", "slash_command_limits.json"))
	if err != nil {
		t.Fatal(err)
	}
	var limits struct {
		MaxCustomFiles int `json:"max_custom_files"`
		MaxEntries     int `json:"max_entries"`
	}
	if err := json.Unmarshal(data, &limits); err != nil {
		t.Fatal(err)
	}
	if limits.MaxCustomFiles != maxCustomFiles || limits.MaxEntries != maxEntries {
		t.Fatalf("slash-command limits = %+v, want custom files %d and entries %d", limits, maxCustomFiles, maxEntries)
	}
}

func TestClaudeBuiltins(t *testing.T) {
	catalog := catalogFor("claude-code", "/tmp", "/nonexistent-home")
	if len(catalog.Commands) == 0 {
		t.Fatal("no commands returned")
	}
	if catalog.Truncated {
		t.Error("should not be truncated for builtins only")
	}

	found := false
	for _, cmd := range catalog.Commands {
		if cmd.Command == "/clear" {
			found = true
			if cmd.Source != "builtin" {
				t.Errorf("/clear source = %q", cmd.Source)
			}
		}
	}
	if !found {
		t.Error("/clear not found in claude builtins")
	}
}

func TestCodexBuiltins(t *testing.T) {
	catalog := catalogFor("codex", "/tmp", "/nonexistent-home")
	if len(catalog.Commands) == 0 {
		t.Fatal("no commands returned")
	}

	found := false
	for _, cmd := range catalog.Commands {
		if cmd.Command == "/clear" {
			found = true
		}
	}
	if !found {
		t.Error("/clear not found in codex builtins")
	}
}

func TestDiscoverPersonalCommands(t *testing.T) {
	home := t.TempDir()
	cmdDir := filepath.Join(home, ".claude", "commands")
	os.MkdirAll(cmdDir, 0o755)
	os.WriteFile(filepath.Join(cmdDir, "deploy.md"), []byte("Deploy the app to production\n\nMore details here."), 0o644)
	os.WriteFile(filepath.Join(cmdDir, "not-a-command.txt"), []byte("ignored"), 0o644)

	catalog := catalogFor("claude", "/tmp", home)

	found := false
	for _, cmd := range catalog.Commands {
		if cmd.Command == "/deploy" {
			found = true
			if cmd.Source != "personal" {
				t.Errorf("source = %q, want personal", cmd.Source)
			}
			if cmd.Description != "Deploy the app to production" {
				t.Errorf("description = %q", cmd.Description)
			}
		}
	}
	if !found {
		t.Error("/deploy not discovered from personal commands")
	}
}

func TestDiscoverProjectCommands(t *testing.T) {
	home := t.TempDir()
	cwd := t.TempDir()
	cmdDir := filepath.Join(cwd, ".claude", "commands")
	os.MkdirAll(cmdDir, 0o755)
	os.WriteFile(filepath.Join(cmdDir, "test.md"), []byte("Run project tests"), 0o644)

	catalog := catalogFor("claude", cwd, home)

	found := false
	for _, cmd := range catalog.Commands {
		if cmd.Command == "/test" {
			found = true
			if cmd.Source != "project" {
				t.Errorf("source = %q, want project", cmd.Source)
			}
		}
	}
	if !found {
		t.Error("/test not discovered from project commands")
	}
}

func TestDiscoverSkills(t *testing.T) {
	home := t.TempDir()
	skillDir := filepath.Join(home, ".claude", "skills", "my-skill")
	os.MkdirAll(skillDir, 0o755)
	os.WriteFile(filepath.Join(skillDir, "SKILL.md"), []byte("---\nname: my-skill\n---\nA custom skill"), 0o644)

	catalog := catalogFor("claude", "/tmp", home)

	found := false
	for _, cmd := range catalog.Commands {
		if cmd.Command == "/my-skill" {
			found = true
			if cmd.Source != "personal" {
				t.Errorf("source = %q, want personal", cmd.Source)
			}
		}
	}
	if !found {
		t.Error("/my-skill not discovered from skills directory")
	}
}

func TestClaudeBuiltinCatalogIsComplete(t *testing.T) {
	catalog := catalogFor("claude", "/tmp", "/nonexistent")
	if len(catalog.Commands) != 51 {
		t.Fatalf("Claude builtins = %d, want 51", len(catalog.Commands))
	}
}

func TestCatalogOutputLimitAtBoundary(t *testing.T) {
	atLimit := finalizeCatalog(make([]Command, maxEntries), false)
	if len(atLimit.Commands) != maxEntries || atLimit.Truncated {
		t.Fatalf("catalog at output limit = len %d, truncated %v", len(atLimit.Commands), atLimit.Truncated)
	}

	overLimit := finalizeCatalog(make([]Command, maxEntries+1), false)
	if len(overLimit.Commands) != maxEntries || !overLimit.Truncated {
		t.Fatalf("catalog over output limit = len %d, truncated %v", len(overLimit.Commands), overLimit.Truncated)
	}
}

func TestGenericConfiguredSkills(t *testing.T) {
	root := t.TempDir()
	first := filepath.Join(root, "personal")
	second := filepath.Join(root, "project")
	for _, path := range []string{
		filepath.Join(first, "deploy"),
		filepath.Join(second, "deploy"),
		filepath.Join(second, "hidden"),
	} {
		if err := os.MkdirAll(path, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(
		filepath.Join(first, "deploy", "SKILL.md"),
		[]byte("---\nname: deploy\ndescription: Deploy safely\nargument-hint: target\n---\n"),
		0o600,
	); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(
		filepath.Join(second, "deploy", "SKILL.md"),
		[]byte("---\nname: deploy\ndescription: Must not override\n---\n"),
		0o600,
	); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(
		filepath.Join(second, "hidden", "SKILL.md"),
		[]byte("---\nname: hidden\nuser-invocable: false\n---\n"),
		0o600,
	); err != nil {
		t.Fatal(err)
	}

	catalog := CatalogFor("pi", "pi-coding-agent", DiscoverContext{
		Cwd:           root,
		Home:          root,
		SkillDirs:     []string{first, second},
		CommandFormat: "skill:{name}",
	})
	if len(catalog.Commands) != len(piBuiltins)+1 {
		t.Fatalf("command count = %d, want %d: %+v", len(catalog.Commands), len(piBuiltins)+1, catalog.Commands)
	}
	for _, command := range catalog.Commands {
		if command.Command != "/skill:deploy" {
			continue
		}
		if command.Description != "Deploy safely" || command.ArgumentHint != "target" ||
			command.Source != "personal" {
			t.Fatalf("command = %+v", command)
		}
		return
	}
	t.Fatal("/skill:deploy not found")
}

func TestUnknownProfileHasNoClaudeFallback(t *testing.T) {
	catalog := CatalogFor("custom", "custom", DiscoverContext{Cwd: t.TempDir(), Home: t.TempDir()})
	if len(catalog.Commands) != 0 {
		t.Fatalf("unexpected commands: %+v", catalog.Commands)
	}
}

func TestExplicitSuppressionSkipsNativeDiscovery(t *testing.T) {
	isolateAgentEnv(t)
	t.Setenv("XDG_CONFIG_HOME", "")
	skill := "---\nname: native-only\ndescription: Must stay suppressed\n---\n"
	cases := []struct {
		profile    string
		builtins   []Command
		nativeFile string
	}{
		{"claude", claudeBuiltins, filepath.Join(".claude", "skills", "native-only", "SKILL.md")},
		{"qoder", qoderBuiltins, filepath.Join(".qoder", "skills", "native-only", "SKILL.md")},
		{"pi", piBuiltins, filepath.Join(".pi", "agent", "skills", "native-only", "SKILL.md")},
		{"omp", ompBuiltins, filepath.Join(".omp", "agent", "skills", "native-only", "SKILL.md")},
		{"kimi", kimiBuiltins, filepath.Join(".kimi-code", "skills", "native-only", "SKILL.md")},
		{"hermes", hermesBuiltins, filepath.Join(".hermes", "skills", "native-only", "SKILL.md")},
		{"agy", agyBuiltins, filepath.Join(".gemini", "skills", "native-only", "SKILL.md")},
		{"opencode", openCodeBuiltins, filepath.Join(".config", "opencode", "commands", "native-only.md")},
		{"cursor", cursorBuiltins, filepath.Join(".cursor", "skills", "native-only", "SKILL.md")},
	}
	for _, tc := range cases {
		t.Run(tc.profile, func(t *testing.T) {
			home := t.TempDir()
			writeFile(t, filepath.Join(home, tc.nativeFile), skill)
			catalog := CatalogFor(tc.profile, tc.profile, DiscoverContext{Cwd: t.TempDir(), Home: home, SuppressNative: true})
			if !reflect.DeepEqual(catalog.Commands, tc.builtins) {
				t.Fatalf("commands = %d, want the %d builtins in order", len(catalog.Commands), len(tc.builtins))
			}
			if catalog.Truncated {
				t.Fatal("suppressed catalog reported truncation")
			}
		})
	}
}
