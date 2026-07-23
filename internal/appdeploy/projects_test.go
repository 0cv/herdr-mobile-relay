package appdeploy

import (
	"strings"
	"testing"
)

func TestProjectSelection(t *testing.T) {
	projects, err := ParseProjects(strings.NewReader(`[
		{"Project Name":"one","Project Domains":"one.pages.dev, app.example.test"},
		{"Project Name":"two","Project Domains":"two.pages.dev"}
	]`))
	if err != nil {
		t.Fatal(err)
	}
	matches, err := MatchingProjects(projects, "https://app.example.test")
	if err != nil {
		t.Fatal(err)
	}
	if len(matches) != 1 || matches[0].Name != "one" {
		t.Fatalf("matches = %#v", matches)
	}
	if err := ValidateProject(projects, "two", "https://app.example.test"); err == nil {
		t.Fatal("project without origin was accepted")
	}
}
