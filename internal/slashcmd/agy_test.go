package slashcmd

import (
	"os"
	"path/filepath"
	"testing"
)

func TestAgyCatalogListsBuiltinsAndUserSkills(t *testing.T) {
	home := t.TempDir()
	skill := filepath.Join(home, ".gemini", "skills", "paneagent")
	if err := os.MkdirAll(skill, 0o700); err != nil {
		t.Fatal(err)
	}
	body := "---\nname: paneagent\ndescription: Relay prompts to agent panes\n---\nBody\n"
	if err := os.WriteFile(filepath.Join(skill, "SKILL.md"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, agent := range []string{"agy", "antigravity", "Antigravity-CLI"} {
		catalog := CatalogFor(agent, t.TempDir(), home)
		if catalog.Status != "available" {
			t.Fatalf("%s status = %q", agent, catalog.Status)
		}
		for _, want := range []string{"/model", "/compact", "/resume", "/paneagent"} {
			if !containsCommand(catalog, want) {
				t.Errorf("%s catalog is missing %s", agent, want)
			}
		}
		if got := commandSource(catalog, "/paneagent"); got != "personal" {
			t.Errorf("%s /paneagent source = %q, want personal", agent, got)
		}
		if got := commandSource(catalog, "/model"); got != "builtin" {
			t.Errorf("%s /model source = %q, want builtin", agent, got)
		}
	}
}

func TestAgyUserSkillCannotShadowBuiltin(t *testing.T) {
	home := t.TempDir()
	skill := filepath.Join(home, ".gemini", "skills", "model")
	if err := os.MkdirAll(skill, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(skill, "SKILL.md"), []byte("---\nname: model\ndescription: shadow\n---\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := commandSource(CatalogFor("agy", t.TempDir(), home), "/model"); got != "builtin" {
		t.Fatalf("/model source = %q, want builtin", got)
	}
}
