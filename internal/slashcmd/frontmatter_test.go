package slashcmd

import (
	"strings"
	"testing"
)

func TestFrontmatterBasic(t *testing.T) {
	data := []byte("---\nname: deploy\ndescription: Deploy the app\nargument-hint: target\n---\nBody content")
	fm, ok := parseFrontmatterBytes(data)
	if !ok {
		t.Fatal("expected ok")
	}
	if fm["name"] != "deploy" {
		t.Errorf("name = %q", fm["name"])
	}
	if fm["description"] != "Deploy the app" {
		t.Errorf("description = %q", fm["description"])
	}
	if fm["argument-hint"] != "target" {
		t.Errorf("argument-hint = %q", fm["argument-hint"])
	}
}

func TestFrontmatterQuotedValues(t *testing.T) {
	data := []byte("---\nname: 'my-skill'\ndescription: \"A quoted description\"\n---\n")
	fm, ok := parseFrontmatterBytes(data)
	if !ok {
		t.Fatal("expected ok")
	}
	if fm["name"] != "my-skill" {
		t.Errorf("name = %q", fm["name"])
	}
	if fm["description"] != "A quoted description" {
		t.Errorf("description = %q", fm["description"])
	}
}

func TestFrontmatterMalformedLinesSkipped(t *testing.T) {
	data := []byte("---\nname: valid\nthis line has no colon separator\nanother bad line\ndescription: also valid\n---\n")
	fm, ok := parseFrontmatterBytes(data)
	if !ok {
		t.Fatal("expected ok")
	}
	if fm["name"] != "valid" {
		t.Errorf("name = %q", fm["name"])
	}
	if fm["description"] != "also valid" {
		t.Errorf("description = %q", fm["description"])
	}
	if len(fm) != 2 {
		t.Errorf("expected 2 keys, got %d: %v", len(fm), fm)
	}
}

func TestFrontmatterKeyLowercased(t *testing.T) {
	data := []byte("---\nName: Foo\nDESCRIPTION: Bar\n---\n")
	fm, _ := parseFrontmatterBytes(data)
	if fm["name"] != "Foo" {
		t.Errorf("name = %q", fm["name"])
	}
	if fm["description"] != "Bar" {
		t.Errorf("description = %q", fm["description"])
	}
}

func TestFrontmatterNoFence(t *testing.T) {
	data := []byte("Just a regular markdown file\nWith content")
	fm, ok := parseFrontmatterBytes(data)
	if !ok {
		t.Fatal("expected ok for no-fence file")
	}
	if len(fm) != 0 {
		t.Errorf("expected empty map, got %v", fm)
	}
}

func TestFrontmatterNoClosingFence(t *testing.T) {
	data := []byte("---\nname: unclosed\ndescription: no end fence\n")
	fm, ok := parseFrontmatterBytes(data)
	if !ok {
		t.Fatal("expected ok")
	}
	if fm["name"] != "unclosed" {
		t.Errorf("name = %q", fm["name"])
	}
}

func TestFrontmatterCRLF(t *testing.T) {
	data := []byte("---\r\nname: crlf-test\r\ndescription: Windows line endings\r\n---\r\nBody")
	fm, ok := parseFrontmatterBytes(data)
	if !ok {
		t.Fatal("expected ok")
	}
	if fm["name"] != "crlf-test" {
		t.Errorf("name = %q", fm["name"])
	}
}

func TestFrontmatterEmptyInput(t *testing.T) {
	fm, ok := parseFrontmatterBytes([]byte(""))
	if !ok {
		t.Fatal("expected ok for empty input")
	}
	if len(fm) != 0 {
		t.Errorf("expected empty map, got %v", fm)
	}
}

func TestReadSkillMetadataOversized(t *testing.T) {
	dir := t.TempDir()
	path := dir + "/SKILL.md"
	big := strings.Repeat("x", maxMetadataSize+1)
	writeTestFile(t, path, big)

	_, ok := readSkillMetadata(path)
	if ok {
		t.Error("oversized file should return false")
	}
}
