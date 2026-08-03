package slashcmd

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCopyProfileForVerifiedAgents(t *testing.T) {
	for _, agent := range []string{
		"claude", "claude-code", "codex", "kimi", "omp", "pi", "pi-coding-agent", "qoder", "qodercli",
	} {
		profile, ok := CopyProfileFor("", agent)
		if !ok || profile.Confirmation == nil || profile.Composer == nil {
			t.Fatalf("CopyProfileFor(%q) = %+v, %v; want a complete profile", agent, profile, ok)
		}
	}
}

func TestCopyProfileConfirmationCounts(t *testing.T) {
	profile, ok := CopyProfileFor("claude", "")
	if !ok {
		t.Fatal("missing Claude copy profile")
	}
	chars, lines, matched := profile.ConfirmationCounts("⎿ Copied to clipboard (44 characters, 1 lines)")
	if !matched || chars != 44 || lines != 1 {
		t.Fatalf("ConfirmationCounts() = (%d, %d, %v), want (44, 1, true)", chars, lines, matched)
	}
	if _, _, matched := profile.ConfirmationCounts("Copied to clipboard (not counted)"); matched {
		t.Fatal("ConfirmationCounts() accepted an uncounted confirmation")
	}

	kimi, ok := CopyProfileFor("kimi", "")
	if !ok {
		t.Fatal("missing Kimi copy profile")
	}
	chars, lines, matched = kimi.ConfirmationCounts("Copied to clipboard (28 characters).")
	if !matched || chars != 28 || lines != -1 {
		t.Fatalf("Kimi ConfirmationCounts() = (%d, %d, %v), want (28, -1, true)", chars, lines, matched)
	}

	codex, ok := CopyProfileFor("codex", "")
	if !ok {
		t.Fatal("missing Codex copy profile")
	}
	chars, lines, matched = codex.ConfirmationCounts("• Copied last message to clipboard")
	if !matched || chars != -1 || lines != -1 {
		t.Fatalf("Codex ConfirmationCounts() = (%d, %d, %v), want (-1, -1, true)", chars, lines, matched)
	}
}

func TestCopyProfilesMatchRecordedConfirmations(t *testing.T) {
	tests := []struct {
		name      string
		agent     string
		wantChars int
		wantLines int
		wantMenu  bool
	}{
		{name: "claude-direct-real.ansi", agent: "claude", wantChars: 2333, wantLines: 13},
		{name: "codex-direct-real.ansi", agent: "codex", wantChars: -1, wantLines: -1},
		{name: "omp-direct-real.ansi", agent: "omp", wantChars: -1, wantLines: -1},
		{name: "pi-direct-real.ansi", agent: "pi", wantChars: -1, wantLines: -1},
		{name: "qoder-direct-real.ansi", agent: "qoder", wantChars: 801, wantLines: 9},
	}
	for _, test := range tests {
		t.Run(test.agent, func(t *testing.T) {
			content, err := os.ReadFile(filepath.Join("..", "copyresponse", "testdata", test.name))
			if err != nil {
				t.Fatal(err)
			}
			profile, ok := CopyProfileFor(test.agent, "")
			if !ok {
				t.Fatalf("missing %s profile", test.agent)
			}
			if got := profile.MenuOpen(string(content)); got != test.wantMenu {
				t.Fatalf("MenuOpen() = %v, want %v", got, test.wantMenu)
			}
			chars, lines, matched := profile.ConfirmationCounts(string(content))
			if !matched || chars != test.wantChars || lines != test.wantLines {
				t.Fatalf("ConfirmationCounts() = (%d, %d, %v), want (%d, %d, true)", chars, lines, matched, test.wantChars, test.wantLines)
			}
		})
	}
}

func TestCopyProfilesMatchRecordedMenusAndPickers(t *testing.T) {
	tests := []struct {
		name   string
		agent  string
		menu   bool
		picker bool
	}{
		{name: "kimi-copy-menu-real.ansi", agent: "kimi", menu: false},
		{name: "omp-copy-menu-real.ansi", agent: "omp", menu: true},
		{name: "omp-picker-real.ansi", agent: "omp", picker: true},
		{name: "pi-direct-real.ansi", agent: "pi"},
		{name: "qoder-copy-menu-real.ansi", agent: "qoder", menu: true},
	}
	for _, test := range tests {
		t.Run(test.agent, func(t *testing.T) {
			content, err := os.ReadFile(filepath.Join("..", "copyresponse", "testdata", test.name))
			if err != nil {
				t.Fatal(err)
			}
			profile, ok := CopyProfileFor(test.agent, "")
			if !ok {
				t.Fatalf("missing %s profile", test.agent)
			}
			if got := profile.MenuOpen(string(content)); got != test.menu {
				t.Fatalf("MenuOpen() = %v, want %v", got, test.menu)
			}
			if got := profile.PickerOpen(string(content)); got != test.picker {
				t.Fatalf("PickerOpen() = %v, want %v", got, test.picker)
			}
		})
	}
}

func TestCopyProfileComposerRecognizesIdlePlaceholders(t *testing.T) {
	tests := []struct {
		name  string
		agent string
		want  string
		found bool
	}{
		{name: "codex-direct-real.ansi", agent: "codex", want: "", found: true},
		{name: "omp-direct-real.ansi", agent: "omp", want: "", found: true},
		{name: "pi-direct-real.ansi", agent: "pi", want: "", found: false},
	}
	for _, test := range tests {
		t.Run(test.agent, func(t *testing.T) {
			content, err := os.ReadFile(filepath.Join("..", "copyresponse", "testdata", test.name))
			if err != nil {
				t.Fatal(err)
			}
			profile, ok := CopyProfileFor(test.agent, "")
			if !ok {
				t.Fatalf("missing %s profile", test.agent)
			}
			got, found := profile.ComposerText(string(content))
			if got != test.want || found != test.found {
				t.Fatalf("ComposerText() = (%q, %v), want (%q, %v)", got, found, test.want, test.found)
			}
		})
	}

	kimi, ok := CopyProfileFor("kimi", "")
	if !ok {
		t.Fatal("missing Kimi copy profile")
	}
	content, err := os.ReadFile(filepath.Join("..", "copyresponse", "testdata", "kimi-copy-menu-real.ansi"))
	if err != nil {
		t.Fatal(err)
	}
	if got, found := kimi.ComposerText(string(content)); got != "/copy" || !found {
		t.Fatalf("Kimi ComposerText() = (%q, %v), want (/copy, true)", got, found)
	}
}

func TestQoderMenuOpenRequiresCurrentComposer(t *testing.T) {
	profile, ok := CopyProfileFor("qoder", "")

	if !ok {
		t.Fatal("missing Qoder copy profile")
	}
	active, err := os.ReadFile(filepath.Join("..", "copyresponse", "testdata", "qoder-copy-menu-real.ansi"))
	if err != nil {
		t.Fatal(err)
	}
	if !profile.MenuOpen(string(active)) {
		t.Fatal("MenuOpen() = false for active Qoder slash menu")
	}
	completed := string(active) + "\n > /copy\n ● Copied to clipboard (801 characters, 9 lines)\n >   Type your message or @path/to/file\n"
	if profile.MenuOpen(completed) {
		t.Fatal("MenuOpen() = true after Qoder returned to a blank composer")
	}
}
func TestPiCopyProfileRequiresVerifiedIdleLayout(t *testing.T) {
	content, err := os.ReadFile(filepath.Join("..", "copyresponse", "testdata", "pi-direct-real.ansi"))
	if err != nil {
		t.Fatal(err)
	}
	profile, ok := CopyProfileFor("pi", "")
	if !ok {
		t.Fatal("missing Pi copy profile")
	}
	if !profile.ComposerReady(string(content)) {
		t.Fatal("ComposerReady() rejected the recorded idle Pi layout")
	}
	lines := strings.Split(profile.CleanSnapshot(string(content)), "\n")
	typed := false
	for index := len(lines) - 1; index >= 2; index-- {
		if !strings.Contains(lines[index], "──") || strings.TrimSpace(lines[index-1]) != "" ||
			!strings.Contains(lines[index-2], "──") {
			continue
		}
		lines[index-1] = "typed prompt"
		typed = true
		break
	}
	if !typed {
		t.Fatal("recorded Pi fixture has no empty input region")
	}
	if profile.ComposerReady(strings.Join(lines, "\n")) {
		t.Fatal("ComposerReady() accepted Pi text typed in the input region")
	}
	if profile.ComposerReady("❯ \nCopied last agent message to clipboard\n") {
		t.Fatal("ComposerReady() accepted Pi output without its idle layout")
	}
}

func TestCopyProfileConfirmationCountsUsesLatestMatch(t *testing.T) {
	profile, ok := CopyProfileFor("claude", "")
	if !ok {
		t.Fatal("missing Claude copy profile")
	}
	chars, lines, matched := profile.ConfirmationCounts(
		"Copied to clipboard (5 characters, 1 line)\nCopied to clipboard (17 characters, 2 lines)",
	)
	if !matched || chars != 17 || lines != 2 {
		t.Fatalf("ConfirmationCounts() = (%d, %d, %v), want (17, 2, true)", chars, lines, matched)
	}
}
