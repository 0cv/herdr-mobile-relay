package question

import (
	"reflect"
	"strings"
	"testing"
)

// Captured from Antigravity CLI 1.4.7 with a shell command awaiting approval.
const agyApprovalView = `
 ❯ agy
      ▄▀▀▄        Antigravity CLI 1.4.7
     ▀▀▀▀▀▀       user@example.com (Google AI Pro)
    ▀▀▀▀▀▀▀▀      Gemini 4 Argon (Medium)
────────────────────────────────────────────────────────────
> run the shell command: touch /tmp/agy-test/approval-probe.txt
● Ran (touch /tmp/agy-test/approval-probe.txt) (ctrl+o to expand)
Command
────────────────────────────────────────────────────────────
Requesting permission for:
   touch /tmp/agy-test/approval-probe.txt
Run this command?
> 1. Yes, run command
  2. Yes, and always allow in this conversation for commands that start with 'touch'
  3. Yes, and always allow for commands that start with 'touch' (Persist to settings.json)
  4. No, cancel

  ↑/↓ Navigate · tab Amend · ctrl+g edit/expand command
esc to cancel
Context [░░░░░░░░] 11.1k/1M (1%) │ 5h [░░░░░░░░] 0% (4h59m) │ Weekly [░░░░░░░░] 0% (6d23h)
`

var agyApprovalOptions = []string{
	"Yes, run command",
	"Yes, and always allow in this conversation for commands that start with 'touch'",
	"Yes, and always allow for commands that start with 'touch' (Persist to settings.json)",
	"No, cancel",
}

func TestClassifyAgyPermissionDialog(t *testing.T) {
	for _, agent := range []string{"agy", "antigravity", "Antigravity-CLI"} {
		got := Classify(agyApprovalView, agent)
		if got.Kind != AttentionApproval || !reflect.DeepEqual(got.Options, agyApprovalOptions) || got.ApprovalFocus != 0 {
			t.Fatalf("%s classification = %+v, want approval with four options focused on the first", agent, got)
		}
		if !strings.Contains(got.Command, "touch /tmp/agy-test/approval-probe.txt") {
			t.Fatalf("%s command = %q", agent, got.Command)
		}
	}
}

func TestClassifyAgyFocusFollowsMenuSelection(t *testing.T) {
	moved := strings.Replace(agyApprovalView, "> 1. Yes, run command", "  1. Yes, run command", 1)
	moved = strings.Replace(moved, "  4. No, cancel", "> 4. No, cancel", 1)
	got := Classify(moved, "agy")
	if got.Kind != AttentionApproval || got.ApprovalFocus != 3 {
		t.Fatalf("classification = %+v, want approval focused on option 4", got)
	}
}

func TestClassifyAgyIgnoresUnrelatedNumberedList(t *testing.T) {
	text := "● Here are the steps:\n> 1. Yes, install it\n  2. No, cancel\n\n> \n? for shortcuts\n"
	if got := Classify(text, "agy"); got.Kind == AttentionApproval {
		t.Fatalf("classification = %+v, want no approval for a plain numbered list", got)
	}
}
