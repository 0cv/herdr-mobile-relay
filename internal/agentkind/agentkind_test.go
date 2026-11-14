package agentkind

import (
	"strings"
	"testing"
)

func TestOfResolvesEveryAlias(t *testing.T) {
	want := map[string][]string{
		Claude:   {"claude", "claudecode", "Claude Code", "claude-code", "CLAUDE_CODE"},
		Codex:    {"codex", "openaicodex", "codexcli", "OpenAI Codex", "Codex CLI"},
		Cursor:   {"cursor", "cursoragent", "Cursor Agent", "cursor-agent"},
		Grok:     {"grok", "grokbuild", "Grok Build", "grok-build"},
		Qoder:    {"qoder", "qodercli", "Qoder CLI"},
		Pi:       {"pi", "picodingagent", "pi-coding-agent", "PiCodingAgent"},
		OMP:      {"omp", "ohmypi", "ompquestion", "omp-question", "oh-my-pi", "Oh My Pi", "oh_my_pi"},
		Kimi:     {"kimi", "kimicode", "kimicli", "kimicodecli", "Kimi Code CLI", "Kimi Code", "kimi-code", "kimi-cli"},
		OpenCode: {"opencode", "Open Code", "open-code"},
		OMO:      {"omo", "ohmyopencode", "Oh My OpenCode", "oh-my-opencode"},
		Hermes:   {"hermes", "hermesagent", "Hermes Agent", "hermes-agent"},
		Agy:      {"agy", "antigravity", "antigravitycli", "Antigravity CLI", "antigravity-cli"},
	}
	listed := map[string]bool{}
	for kind, names := range want {
		for _, name := range names {
			if got := Of(name); got != kind {
				t.Errorf("Of(%q) = %q, want %q", name, got, kind)
			}
			listed[separators.Replace(strings.ToLower(name))] = true
		}
	}
	for alias := range aliases {
		if !listed[alias] {
			t.Errorf("alias %q is missing from the test list", alias)
		}
	}
}

func TestOfRejectsUnknownNames(t *testing.T) {
	for _, name := range []string{"", "  ", "shell", "claude-extra", "my-claude", "pi-extra", "devin"} {
		if got := Of(name); got != "" {
			t.Errorf("Of(%q) = %q, want empty", name, got)
		}
	}
}
