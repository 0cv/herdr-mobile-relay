package slashcmd

import "testing"

func TestCodexBaseBuiltins(t *testing.T) {
	catalog := CatalogFor("codex", "/tmp", "/nonexistent")
	if len(catalog.Commands) == 0 {
		t.Fatal("no commands returned")
	}
	if len(catalog.Commands) != 50 || !hasCommand(catalog, "/clear") ||
		!hasCommand(catalog, "/apps") || !hasCommand(catalog, "/model") ||
		!hasCommand(catalog, "/status") {
		t.Errorf("missing expected codex builtins: %+v", catalog.Commands)
	}
}

func TestCodexNoFilesystemDiscovery(t *testing.T) {
	home := t.TempDir()
	cmdDir := home + "/.codex/commands"
	mkdirAll(t, cmdDir)
	writeTestFile(t, cmdDir+"/extra.md", "Should not appear")

	catalog := CatalogFor("codex", "/tmp", home)
	if hasCommand(catalog, "/extra") {
		t.Error("codex should not do filesystem discovery")
	}
}

func TestCodexModelHint(t *testing.T) {
	catalog := CatalogFor("codex", "/tmp", "/nonexistent")
	for _, cmd := range catalog.Commands {
		if cmd.Command == "/goal" {
			if cmd.ArgumentHint != "[objective|edit|pause|resume|clear]" {
				t.Errorf("/goal argument_hint = %q", cmd.ArgumentHint)
			}
			return
		}
	}
	t.Error("/goal not found")
}

func TestSemverAtLeast(t *testing.T) {
	tests := []struct {
		reported, minimum string
		want              bool
	}{
		{"0.2.0", "0.2.0", true},
		{"0.2.1", "0.2.0", true},
		{"1.0.0", "0.2.0", true},
		{"0.1.9", "0.2.0", false},
		{"0.1.0", "0.2.0", false},
		{"", "0.2.0", false},
		{"garbage", "0.2.0", false},
		{"0.2.0-beta", "0.2.0", true},
	}
	for _, tt := range tests {
		got := semverAtLeast(tt.reported, tt.minimum)
		if got != tt.want {
			t.Errorf("semverAtLeast(%q, %q) = %v, want %v", tt.reported, tt.minimum, got, tt.want)
		}
	}
}

func TestParseVersionParts(t *testing.T) {
	tests := []struct {
		input string
		want  [3]int
	}{
		{"1.2.3", [3]int{1, 2, 3}},
		{"0.2.0", [3]int{0, 2, 0}},
		{"1.0", [3]int{1, 0, 0}},
		{"2", [3]int{2, 0, 0}},
		{"", [3]int{0, 0, 0}},
		{"1.2.3-beta", [3]int{1, 2, 3}},
	}
	for _, tt := range tests {
		got := parseVersionParts(tt.input)
		if got != tt.want {
			t.Errorf("parseVersionParts(%q) = %v, want %v", tt.input, got, tt.want)
		}
	}
}

func TestCodexVersionSpecificCatalogWinsOverBaseFallback(t *testing.T) {
	original := codexBuiltinVersions
	codexBuiltinVersions = []versionedBuiltins{
		{MinVersion: "", Commands: []Command{{Command: "/base"}}},
		{MinVersion: "1.2.0", Commands: []Command{{Command: "/new"}}},
	}
	defer func() { codexBuiltinVersions = original }()

	commands := codexBuiltinsForVersion("1.3.0")
	if len(commands) != 1 || commands[0].Command != "/new" {
		t.Fatalf("version-specific commands = %+v, want /new", commands)
	}
}
