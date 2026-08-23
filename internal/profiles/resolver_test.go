package profiles

import (
	"os"
	"path/filepath"
	"testing"
)

func TestDefaultCandidatesFiltered(t *testing.T) {
	// With a fake config home and no binaries on PATH, no profiles should be found
	dir := t.TempDir()
	r := NewResolver(dir, nil)
	profiles := r.Profiles()

	// codex/claude/opencode likely not installed in test env, so expect empty or few
	for _, p := range profiles {
		if p.ID == "" || p.Label == "" {
			t.Errorf("profile with empty field: %+v", p)
		}
	}
}

func TestDefaultCandidatesIncludePiOhMyPiAndKimi(t *testing.T) {
	binDir := t.TempDir()
	for _, name := range []string{"pi", "omp", "kimi"} {
		if err := os.WriteFile(filepath.Join(binDir, name), []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("PATH", binDir)

	profiles := NewResolver(t.TempDir(), nil).Profiles()
	for _, want := range []Profile{
		{ID: "pi", Label: "Pi", Kind: "pi"},
		{ID: "omp", Label: "Oh My Pi", Kind: "omp"},
		{ID: "kimi", Label: "Kimi", Kind: "kimi"},
	} {
		var got *Profile
		for index := range profiles {
			if profiles[index].ID == want.ID {
				got = &profiles[index]
				break
			}
		}
		if got == nil {
			t.Errorf("profile %q was not detected: %+v", want.ID, profiles)
			continue
		}
		if got.Label != want.Label || got.Kind != want.Kind || len(got.Argv) != 1 {
			t.Errorf("profile %q = %+v, want label %q, kind %q, and one executable", want.ID, *got, want.Label, want.Kind)
		}
	}
}

func TestAgentVersionUsesResolvedProfileExecutable(t *testing.T) {
	binDir := t.TempDir()
	codex := filepath.Join(binDir, "codex")
	if err := os.WriteFile(codex, []byte("#!/bin/sh\necho 'codex-cli 1.2.3'\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", binDir)
	resolver := NewResolver(t.TempDir(), nil)
	if version := resolver.AgentVersion("codex"); version != "1.2.3" {
		t.Fatalf("AgentVersion(codex) = %q, want 1.2.3", version)
	}
}

func TestINIProfiles(t *testing.T) {
	dir := t.TempDir()
	herdrDir := filepath.Join(dir, "herdr")
	os.MkdirAll(herdrDir, 0o755)

	ini := "[profiles]\nmyagent = My Custom Agent\n"
	os.WriteFile(filepath.Join(herdrDir, "agent-profiles.ini"), []byte(ini), 0o644)

	r := NewResolver(dir, nil)
	profiles := r.Profiles()

	// myagent won't pass binaryExists check, so it won't appear
	// But the INI parsing should work without error
	_ = profiles
}

func TestProfileIDForAgent(t *testing.T) {
	dir := t.TempDir()
	binDir := t.TempDir()
	for _, name := range []string{"claude", "pi"} {
		if err := os.WriteFile(filepath.Join(binDir, name), []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("PATH", binDir)
	herdrDir := filepath.Join(dir, "herdr")
	if err := os.MkdirAll(herdrDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(
		filepath.Join(herdrDir, "agent-profiles.ini"),
		[]byte("[profiles]\npi = Pi\n"),
		0o600,
	); err != nil {
		t.Fatal(err)
	}
	r := NewResolver(dir, nil)

	tests := []struct {
		agent, want string
	}{
		{"claude-code", "claude"},
		{"pi-coding-agent", "pi"},
		{"Claude Code", "claude"},
		{"unknown-thing", ""},
	}
	for _, tt := range tests {
		got := r.ProfileIDForAgent(tt.agent)
		if got != tt.want {
			t.Errorf("ProfileIDForAgent(%q) = %q, want %q", tt.agent, got, tt.want)
		}
	}
}

func TestProfilesCached(t *testing.T) {
	dir := t.TempDir()
	r := NewResolver(dir, nil)

	p1 := r.Profiles()
	p2 := r.Profiles()

	if len(p1) != len(p2) {
		t.Error("cached profiles should be stable")
	}
}

func TestINIProfilesMergeReplaceAliasesAndReload(t *testing.T) {
	dir := t.TempDir()
	binDir := t.TempDir()
	for _, name := range []string{"codex", "myagent", "other"} {
		path := filepath.Join(binDir, name)
		if err := os.WriteFile(path, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("PATH", binDir)
	herdrDir := filepath.Join(dir, "herdr")
	if err := os.MkdirAll(herdrDir, 0o755); err != nil {
		t.Fatal(err)
	}
	iniPath := filepath.Join(herdrDir, "agent-profiles.ini")
	merged := "[profiles]\ncodex = Custom Codex\nmyagent = My Agent\n[aliases]\nreported-agent = myagent\n"
	if err := os.WriteFile(iniPath, []byte(merged), 0o600); err != nil {
		t.Fatal(err)
	}

	resolver := NewResolver(dir, nil)
	profiles := resolver.Profiles()
	if got := profileLabel(profiles, "codex"); got != "Custom Codex" {
		t.Fatalf("codex label = %q", got)
	}
	if got := profileLabel(profiles, "myagent"); got != "My Agent" {
		t.Fatalf("myagent label = %q", got)
	}
	if got := resolver.ProfileIDForAgent("reported-agent"); got != "myagent" {
		t.Fatalf("alias resolved to %q", got)
	}

	replaced := "[config]\nreplace_profiles = true\n[profiles]\nother = Other Agent\n"
	if err := os.WriteFile(iniPath, []byte(replaced), 0o600); err != nil {
		t.Fatal(err)
	}
	resolver.Reload()
	profiles = resolver.Profiles()
	if len(profiles) != 1 || profiles[0].ID != "other" {
		t.Fatalf("replacement profiles = %+v", profiles)
	}
	if got := resolver.ProfileIDForAgent("reported-agent"); got != "" {
		t.Fatalf("stale alias resolved to %q", got)
	}
}

func TestCommandConfig(t *testing.T) {
	dir := t.TempDir()
	herdrDir := filepath.Join(dir, "herdr")
	if err := os.MkdirAll(herdrDir, 0o755); err != nil {
		t.Fatal(err)
	}
	skills := filepath.Join(dir, "skills")
	ini := "[skills]\npi = " + skills + "\n[commands]\npi = skill:{name}\nbad = {name}:{other}\n"
	if err := os.WriteFile(filepath.Join(herdrDir, "agent-profiles.ini"), []byte(ini), 0o600); err != nil {
		t.Fatal(err)
	}
	resolver := NewResolver(dir, nil)
	dirs, format, ok := resolver.CommandConfig("pi")
	if !ok || format != "skill:{name}" || len(dirs) != 1 || dirs[0] != skills {
		t.Fatalf("command config = %q, %q, %v", dirs, format, ok)
	}
	if _, _, ok := resolver.CommandConfig("bad"); ok {
		t.Fatal("invalid command format was accepted")
	}
}

func profileLabel(profiles []Profile, id string) string {
	for _, profile := range profiles {
		if profile.ID == id {
			return profile.Label
		}
	}
	return ""
}
