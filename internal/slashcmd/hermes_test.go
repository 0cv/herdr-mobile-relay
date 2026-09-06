package slashcmd

import (
	"os"
	"path/filepath"
	"testing"
)

func TestHermesBuiltinCatalog(t *testing.T) {
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
	for _, name := range []string{"hermes", "hermes-agent", "hermes agent"} {
		catalog := CatalogFor(name, "/tmp", "/nonexistent")
		if !hasCommand(catalog, "/model") {
			t.Errorf("CatalogFor(%q) missing /model", name)
		}
	}
}

func TestHermesSkillDiscovery(t *testing.T) {
	tempDir, err := os.MkdirTemp("", "hermes-skill-test-*")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(tempDir)

	skillDir := filepath.Join(tempDir, ".hermes", "skills", "weather")
	if err := os.MkdirAll(skillDir, 0755); err != nil {
		t.Fatal(err)
	}
	skillContent := `---
name: weather
description: Check local weather forecast
argument-hint: <city>
---
# Weather skill
`
	if err := os.WriteFile(filepath.Join(skillDir, "SKILL.md"), []byte(skillContent), 0644); err != nil {
		t.Fatal(err)
	}

	catalog := CatalogForProfile("hermes", "hermes", tempDir, "/nonexistent", nil, "", "", "")
	if !hasCommand(catalog, "/weather") {
		t.Errorf("Hermes skill discovery failed to find /weather")
	}
}
