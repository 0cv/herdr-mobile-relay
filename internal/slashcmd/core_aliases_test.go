package slashcmd

import "testing"

func TestCoreAgentAliases(t *testing.T) {
	for name, want := range map[string]string{"qodercli": "qoder", "kimi-code": "kimi", "grok-build": "grok", "grok": "grok"} {
		if got := ProfileIDForAgentName(name); got != want {
			t.Errorf("%s: got %q, want %q", name, got, want)
		}
	}
}
