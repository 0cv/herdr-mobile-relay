package question

import (
	"reflect"
	"testing"
)

const codexApprovalView = `
• I need to run the checks.
─ Worked for 2s ─

Would you like to run this command?
$ make check
❯ 1. Approve
  2. Reject
Enter to select · Esc to cancel
`

const claudeApprovalView = `
Do you want to proceed?
Bash command
$ npm test
❯ 1. Yes
  2. Yes, and remember this choice
  3. No
Esc to cancel · Enter to confirm
`

const claudePermissionView = `
Claude needs your permission to use Bash.
$ npm test
❯ 1. Allow once
  2. Reject
Esc to cancel · Enter to confirm
`

const codexSubagentApprovalView = `
Approve all pending agents?
❯ 1. Approve all pending
  2. Configure individually
  3. Exit (cancel subagents)
`

const qoderApprovalView = `
Allow this command?
$ go test ./...
❯ 1. Allow
  2. Reject
Tab/←→ switch · Enter select · Esc cancel
`

func TestClassifyLiveApprovalsByAgent(t *testing.T) {
	tests := []struct {
		name    string
		agent   string
		content string
		want    []string
	}{
		{"codex tool", "codex", codexApprovalView, []string{"Approve", "Reject"}},
		{"codex subagents", "codex", codexSubagentApprovalView, []string{"Approve all pending", "Configure individually", "Exit (cancel subagents)"}},
		{"claude proceed", "claude", claudeApprovalView, []string{"Yes", "Yes, and remember this choice", "No"}},
		{"claude permission", "claude", claudePermissionView, []string{"Allow once", "Reject"}},
		{"qoder allow", "qodercli", qoderApprovalView, []string{"Allow", "Reject"}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got := Classify(test.content, test.agent)
			if got.Kind != AttentionApproval || !reflect.DeepEqual(got.Options, test.want) {
				t.Fatalf("classification = %+v, want approval options %#v", got, test.want)
			}
		})
	}
}

func TestClassifyStructuredQuestionsBeforeApprovalMenus(t *testing.T) {
	for _, test := range []struct {
		agent   string
		content string
	}{
		{"codex", codexQuestionView},
		{"claude", claudeFirstQuestionView},
		{"qoder", qoderQuestionView},
		{"qoder", qoderReviewView},
	} {
		got := Classify(test.content, test.agent)
		if got.Kind != AttentionQuestion || got.Interaction == nil || len(got.Options) != 0 {
			t.Fatalf("%s classification = %+v, want structured question", test.agent, got)
		}
	}
}

func TestClassifyOrdinaryQuestionsAndNumberedPlansAsChat(t *testing.T) {
	content := `
• Hello! What would you like to work on next?

  1. Add parser fixtures.
  2. Verify the backend transition.
  3. Run the browser tests.

─ Worked for 8s ─

›
gpt-5.6-sol xhigh · ~/project · main · Context 30% used
`
	got := Classify(content, "codex")
	if got.Kind != AttentionChat || len(got.Options) != 0 || got.Interaction != nil {
		t.Fatalf("classification = %+v, want chat without controls", got)
	}
}

func TestClassifyHistoricalAndSupersededApprovalsSafely(t *testing.T) {
	historical := codexApprovalView + `
• The checks passed. What would you like to do next?
─ Worked for 4s ─
›
`
	if got := Classify(historical, "codex"); got.Kind != AttentionChat || len(got.Options) != 0 {
		t.Fatalf("historical approval classification = %+v, want chat", got)
	}
	if got := Classify(codexApprovalView+"\nNewer output is still arriving.", "codex"); got.Kind != AttentionUnknown {
		t.Fatalf("superseded approval classification = %+v, want unknown", got)
	}
}

func TestClassifyUnknownAgentsWithoutFabricatedControls(t *testing.T) {
	for _, content := range []string{
		codexApprovalView,
		codexQuestionView,
		"Hello! What would you like to work on next?\n❯",
		"1. First test\n2. Second test",
	} {
		got := Classify(content, "opencode")
		if got.Kind != AttentionUnknown || len(got.Options) != 0 || got.Interaction != nil {
			t.Fatalf("unknown-agent classification = %+v", got)
		}
	}
	_, _, options := ApprovalDetails("")
	if len(options) != 0 {
		t.Fatalf("empty pane fabricated approval options: %#v", options)
	}
}
