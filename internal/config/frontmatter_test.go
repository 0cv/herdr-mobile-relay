package config

import (
	"strings"
	"testing"
)

func TestParseFrontmatter(t *testing.T) {
	input := `title: My Session
model: claude-4
created: 2026-07-22
`
	fm, err := ParseFrontmatter(strings.NewReader(input))
	if err != nil {
		t.Fatal(err)
	}
	if fm["title"] != "My Session" {
		t.Errorf("title = %q", fm["title"])
	}
	if fm["model"] != "claude-4" {
		t.Errorf("model = %q", fm["model"])
	}
}

func TestParseFrontmatterSkipsComments(t *testing.T) {
	input := `# comment
key: value
`
	fm, err := ParseFrontmatter(strings.NewReader(input))
	if err != nil {
		t.Fatal(err)
	}
	if len(fm) != 1 {
		t.Fatalf("len = %d, want 1", len(fm))
	}
}

func TestParseFrontmatterNoColon(t *testing.T) {
	input := `just text without colon
`
	_, err := ParseFrontmatter(strings.NewReader(input))
	if err == nil {
		t.Fatal("expected error")
	}
}
