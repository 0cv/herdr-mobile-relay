package config

import (
	"strings"
	"testing"
)

func TestParseINIBasic(t *testing.T) {
	input := `
[profiles]
claude = Claude Code
codex = Codex

[aliases]
fix = claude --fix
`
	ini, err := ParseINI(strings.NewReader(input))
	if err != nil {
		t.Fatal(err)
	}

	v, ok := ini.Get("profiles", "claude")
	if !ok || v != "Claude Code" {
		t.Errorf("profiles.claude = %q, %v", v, ok)
	}

	v, ok = ini.Get("aliases", "fix")
	if !ok || v != "claude --fix" {
		t.Errorf("aliases.fix = %q, %v", v, ok)
	}
}

func TestParseINIColonDelimiter(t *testing.T) {
	input := `[section]
key: value
`
	ini, err := ParseINI(strings.NewReader(input))
	if err != nil {
		t.Fatal(err)
	}
	v, ok := ini.Get("section", "key")
	if !ok || v != "value" {
		t.Errorf("section.key = %q, %v", v, ok)
	}
}

func TestParseINIKeysLowercased(t *testing.T) {
	input := `[Section]
MyKey = MyValue
`
	ini, err := ParseINI(strings.NewReader(input))
	if err != nil {
		t.Fatal(err)
	}
	v, ok := ini.Get("section", "mykey")
	if !ok || v != "MyValue" {
		t.Errorf("section.mykey = %q, %v", v, ok)
	}
}

func TestParseINIComments(t *testing.T) {
	input := `# full line comment
; another comment
[section]
key = value # inline preserved
`
	ini, err := ParseINI(strings.NewReader(input))
	if err != nil {
		t.Fatal(err)
	}
	v, _ := ini.Get("section", "key")
	if v != "value # inline preserved" {
		t.Errorf("inline comment not preserved: %q", v)
	}
}

func TestParseINIDuplicateKeyError(t *testing.T) {
	input := `[section]
key = one
key = two
`
	_, err := ParseINI(strings.NewReader(input))
	if err == nil {
		t.Fatal("expected error for duplicate key")
	}
	if !strings.Contains(err.Error(), "duplicate") {
		t.Errorf("error = %q, want mention of duplicate", err.Error())
	}
}

func TestParseINIUnterminatedSection(t *testing.T) {
	input := `[broken
key = value
`
	_, err := ParseINI(strings.NewReader(input))
	if err == nil {
		t.Fatal("expected error for unterminated section")
	}
}

func TestParseININoDelimiter(t *testing.T) {
	input := `[section]
justtext
`
	_, err := ParseINI(strings.NewReader(input))
	if err == nil {
		t.Fatal("expected error for missing delimiter")
	}
}

func TestSectionNames(t *testing.T) {
	input := `[alpha]
a = 1
[beta]
b = 2
`
	ini, err := ParseINI(strings.NewReader(input))
	if err != nil {
		t.Fatal(err)
	}
	names := ini.SectionNames()
	if len(names) != 2 {
		t.Fatalf("sections = %v, want 2", names)
	}
}
