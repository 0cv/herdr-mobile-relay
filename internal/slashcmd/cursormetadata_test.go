package slashcmd

import (
	"fmt"
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

func TestCursorSkillFrontmatterBoundariesAndLanguages(t *testing.T) {
	for _, tc := range []struct {
		name, body, description, invocable string
		accepted                           bool
	}{
		{"leading-space", " ---\nuser-invocable: false\n---\nBody\n", "", "", true},
		{"leading-newline", "\n---\nuser-invocable: false\n---\nBody\n", "", "", true},
		{"horizontal-rule", "----\nuser-invocable: false\n---\nBody\n", "", "", true},
		{"closing-suffix", "---\ndescription: Before\n--- trailing content\ndescription: After\n", "Before", "", true},
		{"unclosed", "---yaml\ndescription: Unclosed\n", "Unclosed", "", true},
		{"empty", "---\n---\nBody\n", "", "", true},
		{"unknown-empty", "---toml\n# Comment\n---\nBody\n", "", "", true},
		{"unknown-content", "---toml\nuser-invocable = false\n---\nBody\n", "", "", false},
		{"javascript", "---javascript\nuser-invocable: false\n---\nBody\n", "", "", true},
		{"js", "---JS\nthrow new Error('must not execute')\n---\nBody\n", "", "", true},
		{"json", "---json\n{\"description\": \"JSON skill\", \"user-invocable\": false}\n---\nBody\n", "JSON skill", "false", true},
		{"json-ide-only", "---json\n{\"metadata\": {\"surfaces\": [\"ide\"]}}\n---\nBody\n", "", "", false},
		{"json-invalid", "---json\nuser-invocable: false\n---\nBody\n", "", "", false},
		{"json-uppercase", "---JSON\n{\"description\": \"JSON skill\"}\n---\nBody\n", "", "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			metadata, ok := parseCursorSkillMetadata([]byte(tc.body))
			if ok != tc.accepted {
				t.Fatalf("accepted = %v, want %v", ok, tc.accepted)
			}
			if !ok {
				return
			}
			if metadata["description"] != tc.description || metadata["user-invocable"] != tc.invocable {
				t.Errorf("metadata = %v, want description %q, invocable %q", metadata, tc.description, tc.invocable)
			}
		})
	}
}

func TestCursorSkillFileReadLimit(t *testing.T) {
	for _, project := range []bool{false, true} {
		for _, size := range []int{0, 1, 64*1024 + 1, 1 << 20, 1<<20 + 1} {
			t.Run(fmt.Sprintf("project=%v/size=%d", project, size), func(t *testing.T) {
				root := t.TempDir()
				skillDir := filepath.Join(root, "review")
				writeFile(t, filepath.Join(skillDir, "SKILL.md"), strings.Repeat("x", size))
				eligible := size > 0 && size <= 1<<20
				data, ok := readCursorSkillFile(root, skillDir, project)
				if ok != eligible || ok && len(data) != size {
					t.Errorf("read returned %d bytes, ok=%v; want size=%d, ok=%v", len(data), ok, size, eligible)
				}
			})
		}
	}
}
