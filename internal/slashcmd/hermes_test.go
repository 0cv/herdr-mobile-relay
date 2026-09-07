package slashcmd

import (
	"path/filepath"
	"testing"
)

func TestHermesBuiltinCatalog(t *testing.T) {
	isolateAgentEnv(t)
	catalog := CatalogForProfile("hermes", "hermes", "/tmp", "/nonexistent", nil, "", "", "")
	if catalog.Truncated {
		t.Fatal("Hermes builtins should not be truncated")
	}
	if len(catalog.Commands) < 20 {
		t.Fatalf("Hermes builtins = %d, want at least 20", len(catalog.Commands))
	}
	expected := []string{"/model", "/usage", "/clear", "/sessions", "/skills", "/tools", "/compress", "/branch", "/undo", "/yolo", "/fast", "/reasoning"}
	for _, command := range expected {
		if !hasCommand(catalog, command) {
			t.Errorf("Hermes catalog missing %q", command)
		}
	}
}

func TestHermesCatalogForAlias(t *testing.T) {
	isolateAgentEnv(t)
	for _, name := range []string{"hermes", "hermes-agent", "hermes agent"} {
		catalog := CatalogFor(name, "/tmp", "/nonexistent")
		if !hasCommand(catalog, "/model") {
			t.Errorf("CatalogFor(%q) missing /model", name)
		}
	}
}

func TestHermesSkillDiscovery(t *testing.T) {
	isolateAgentEnv(t)
	tempDir := t.TempDir()

	writeSkill(t, filepath.Join(tempDir, ".hermes", "skills"), "weather", "Check local weather forecast")
	writeSkill(t, filepath.Join(tempDir, ".hermes", "skills", "devops"), "nested", "Nested skill")

	catalog := CatalogForProfile("hermes", "hermes", tempDir, "/nonexistent", nil, "", "", "")
	if !hasCommand(catalog, "/weather") {
		t.Errorf("Hermes skill discovery failed to find /weather")
	}
	if !hasCommand(catalog, "/nested") {
		t.Errorf("Hermes skill discovery failed to find category-nested /nested")
	}
}

func TestHermesUsesActiveProfileHome(t *testing.T) {
	t.Run("explicit Hermes home", func(t *testing.T) {
		home := t.TempDir()
		profileHome := filepath.Join(home, ".hermes", "profiles", "coder")
		writeSkill(t, filepath.Join(home, ".hermes", "skills"), "defaultonly", "Default skill")
		writeSkill(t, filepath.Join(profileHome, "skills"), "coderonly", "Coder skill")
		t.Setenv("HERMES_HOME", profileHome)

		catalog := CatalogForProfile("hermes", "hermes", t.TempDir(), home, nil, "", "", "")
		if !hasCommand(catalog, "/coderonly") {
			t.Fatal("explicit Hermes home skill was not discovered")
		}
		if hasCommand(catalog, "/defaultonly") {
			t.Fatal("default Hermes skills leaked into an explicit profile")
		}
	})

	t.Run("sticky active profile", func(t *testing.T) {
		home := t.TempDir()
		defaultHome := filepath.Join(home, ".hermes")
		profileHome := filepath.Join(defaultHome, "profiles", "coder")
		writeFile(t, filepath.Join(defaultHome, "active_profile"), "coder\n")
		writeSkill(t, filepath.Join(defaultHome, "skills"), "defaultonly", "Default skill")
		writeSkill(t, filepath.Join(profileHome, "skills"), "coderonly", "Coder skill")
		t.Setenv("HERMES_HOME", "")

		catalog := CatalogForProfile("hermes", "hermes", t.TempDir(), home, nil, "", "", "")
		if !hasCommand(catalog, "/coderonly") {
			t.Fatal("sticky Hermes profile skill was not discovered")
		}
		if hasCommand(catalog, "/defaultonly") {
			t.Fatal("default Hermes skills leaked into the active profile")
		}
	})
}

func TestHermesProjectSkillsUseGitRoot(t *testing.T) {
	root := t.TempDir()
	subdir := filepath.Join(root, "packages", "app")
	mkdirAll(t, filepath.Join(root, ".git"))
	mkdirAll(t, subdir)
	writeSkill(t, filepath.Join(root, ".hermes", "skills"), "rootonly", "Root skill")
	writeSkill(t, filepath.Join(subdir, ".hermes", "skills"), "subonly", "Subdirectory skill")
	t.Setenv("HERMES_HOME", "")

	catalog := CatalogForProfile("hermes", "hermes", subdir, t.TempDir(), nil, "", "", "")
	if !hasCommand(catalog, "/rootonly") {
		t.Fatal("Git-root Hermes skill was not discovered from a subdirectory")
	}
	if hasCommand(catalog, "/subonly") {
		t.Fatal("Hermes discovered a subdirectory skill instead of the Git-root tree")
	}
}
