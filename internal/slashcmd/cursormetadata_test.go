package slashcmd

import (
	"path/filepath"
	"strings"
	"testing"
)

func TestCursorSkillMetadataParsing(t *testing.T) {
	for _, newline := range []string{"\n", "\r\n"} {
		data := strings.Join([]string{
			"---",
			"name: display name",
			"description: |",
			"  First paragraph",
			"  ---",
			"  Second paragraph",
			"argument-hint: '<path>'",
			"user-invocable: false",
			"metadata:",
			"  surfaces: [cli]",
			"---",
			"description: Not frontmatter",
		}, newline)
		metadata, ok := parseCursorSkillMetadata([]byte(data))
		if !ok {
			t.Fatalf("metadata rejected for newline %q", newline)
		}
		for key, want := range map[string]string{
			"name":           "display name",
			"description":    "First paragraph\n---\nSecond paragraph\n",
			"argument-hint":  "<path>",
			"user-invocable": "false",
		} {
			if got := metadata[key]; got != want {
				t.Errorf("%s = %q, want %q", key, got, want)
			}
		}
	}
}

func TestCursorSkillMetadataMalformed(t *testing.T) {
	if _, ok := parseCursorSkillMetadata([]byte("---\nmetadata: [invalid\n---\n")); ok {
		t.Error("invalid YAML accepted")
	}
}

func TestCursorSkillMetadataWithoutFrontmatter(t *testing.T) {
	metadata, ok := parseCursorSkillMetadata([]byte("# Skill\n\nmetadata:\n  surfaces: [ide]\n"))
	if !ok || len(metadata) != 0 {
		t.Errorf("body was parsed as frontmatter: %v, %v", metadata, ok)
	}
}

func TestCursorSkillMetadataReadLimit(t *testing.T) {
	for _, project := range []bool{false, true} {
		root := t.TempDir()
		skillDir := filepath.Join(root, "review")
		writeFile(t, filepath.Join(skillDir, "SKILL.md"), "---\nname: review\n---\n"+strings.Repeat("x", maxMetadataSize))
		if _, ok := readCursorSkillMetadata(root, skillDir, project); ok {
			t.Errorf("oversized skill accepted: project=%v", project)
		}
	}
}
