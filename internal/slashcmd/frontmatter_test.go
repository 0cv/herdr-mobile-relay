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

// TestFrontmatterLiteralBlockScalar pins the real-world shape that used to yield
// the literal description "|" in the palette.
func TestFrontmatterLiteralBlockScalar(t *testing.T) {
	data := []byte(`---
name: archon
description: |
  Use when: User wants to run Archon workflows.
  Triggers: "use archon to", "run archon".
argument-hint: "[workflow] [message]"
---
Body text that must not leak into the description.
`)
	fm, ok := parseFrontmatterBytes(data)
	if !ok {
		t.Fatal("expected ok")
	}
	if fm["description"] == "|" {
		t.Fatal(`description = "|": block scalar header was taken as the value`)
	}
	want := "Use when: User wants to run Archon workflows.\nTriggers: \"use archon to\", \"run archon\"."
	if fm["description"] != want {
		t.Errorf("description = %q, want %q", fm["description"], want)
	}
	wantCompact := `Use when: User wants to run Archon workflows. Triggers: "use archon to", "run archon".`
	if got := compact(fm["description"], 240); got != wantCompact {
		t.Errorf("compact(description) = %q, want %q", got, wantCompact)
	}
	if fm["name"] != "archon" {
		t.Errorf("name = %q", fm["name"])
	}
	if fm["argument-hint"] != "[workflow] [message]" {
		t.Errorf("argument-hint = %q: key after the block was not parsed", fm["argument-hint"])
	}
}

func TestFrontmatterFoldedBlockScalar(t *testing.T) {
	data := []byte(`---
name: manage-run
description: >-
  Use when: User wants to INSPECT runs
  driven through the archon CLI.
---
`)
	fm, ok := parseFrontmatterBytes(data)
	if !ok {
		t.Fatal("expected ok")
	}
	if fm["description"] == ">-" {
		t.Fatal(`description = ">-": block scalar header was taken as the value`)
	}
	want := "Use when: User wants to INSPECT runs driven through the archon CLI."
	if fm["description"] != want {
		t.Errorf("description = %q, want %q", fm["description"], want)
	}
}

func TestFrontmatterBlockScalarHeaderVariants(t *testing.T) {
	cases := []struct {
		header string
		want   string
	}{
		{"|", "alpha\nbeta"},
		{"|-", "alpha\nbeta"},
		{"|+", "alpha\nbeta"},
		{"|2-", "alpha\nbeta"},
		{">", "alpha beta"},
		{">-", "alpha beta"},
		{">+", "alpha beta"},
	}
	for _, tc := range cases {
		t.Run(tc.header, func(t *testing.T) {
			data := []byte("---\ndescription: " + tc.header + "\n  alpha\n  beta\n---\n")
			fm, ok := parseFrontmatterBytes(data)
			if !ok {
				t.Fatal("expected ok")
			}
			if fm["description"] != tc.want {
				t.Errorf("description = %q, want %q", fm["description"], tc.want)
			}
		})
	}
}

func TestFrontmatterBlockScalarKeepsRelativeIndent(t *testing.T) {
	data := []byte("---\ndescription: |\n  line one\n    indented two\n  line three\n---\n")
	fm, _ := parseFrontmatterBytes(data)
	want := "line one\n  indented two\nline three"
	if fm["description"] != want {
		t.Errorf("description = %q, want %q", fm["description"], want)
	}
}

func TestFrontmatterBlockScalarKeepsBlankLines(t *testing.T) {
	data := []byte("---\ndescription: |\n  para one\n\n  para two\n\n---\n")
	fm, _ := parseFrontmatterBytes(data)
	want := "para one\n\npara two"
	if fm["description"] != want {
		t.Errorf("description = %q, want %q", fm["description"], want)
	}
}

func TestFrontmatterBlockScalarEndsAtNextKey(t *testing.T) {
	data := []byte("---\ndescription: |\n  first line\nname: after-block\n---\n")
	fm, _ := parseFrontmatterBytes(data)
	if fm["description"] != "first line" {
		t.Errorf("description = %q", fm["description"])
	}
	if fm["name"] != "after-block" {
		t.Errorf("name = %q: block swallowed the following key", fm["name"])
	}
}

func TestFrontmatterBlockScalarEndsAtFence(t *testing.T) {
	data := []byte("---\ndescription: |\n  first line\n---\nother: body line\n")
	fm, _ := parseFrontmatterBytes(data)
	if fm["description"] != "first line" {
		t.Errorf("description = %q", fm["description"])
	}
	if _, exists := fm["other"]; exists {
		t.Errorf("body key leaked into frontmatter: %v", fm)
	}
}

// TestFrontmatterEmptyBlockScalarFailsOpen covers the malformed case: an empty
// description drops the skill, which is safer than a one-character bogus one.
func TestFrontmatterEmptyBlockScalarFailsOpen(t *testing.T) {
	for _, data := range [][]byte{
		[]byte("---\nname: broken\ndescription: |\n---\n"),
		[]byte("---\nname: broken\ndescription: >-\n"),
		[]byte("---\nname: broken\ndescription: |\nother: value\n---\n"),
	} {
		fm, ok := parseFrontmatterBytes(data)
		if !ok {
			t.Fatalf("expected ok for %q", data)
		}
		if fm["description"] != "" {
			t.Errorf("description = %q for %q, want empty", fm["description"], data)
		}
	}
}

func TestFrontmatterPlainScalarStartingWithPipe(t *testing.T) {
	data := []byte("---\ndescription: |not-a-header\nname: keeps-parsing\n---\n")
	fm, _ := parseFrontmatterBytes(data)
	if fm["description"] != "|not-a-header" {
		t.Errorf("description = %q, want the plain scalar verbatim", fm["description"])
	}
	if fm["name"] != "keeps-parsing" {
		t.Errorf("name = %q", fm["name"])
	}
}

func TestReadSkillMetadataBlockScalarFromFile(t *testing.T) {
	dir := t.TempDir()
	path := dir + "/SKILL.md"
	writeTestFile(t, path, "---\nname: deploy\ndescription: |\n  Ship the app\n  to production.\n---\nbody\n")

	fm, ok := readSkillMetadata(path)
	if !ok {
		t.Fatal("expected ok")
	}
	if got := compact(fm["description"], 240); got != "Ship the app to production." {
		t.Errorf("description = %q", got)
	}
}

func TestReadSkillMetadataOversizedBlockScalar(t *testing.T) {
	dir := t.TempDir()
	path := dir + "/SKILL.md"
	body := strings.Repeat("  filler line\n", maxMetadataSize/8)
	writeTestFile(t, path, "---\nname: huge\ndescription: |\n"+body+"---\n")

	if _, ok := readSkillMetadata(path); ok {
		t.Error("oversized block scalar should still be rejected by maxMetadataSize")
	}
}
