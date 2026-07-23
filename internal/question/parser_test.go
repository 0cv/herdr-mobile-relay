package question

import (
	"reflect"
	"strings"
	"testing"
)

const multiQuestionView = `
Improvements  ✓ Submit →
Which further improvements should be included?
❯ 1. [✓] Remove duplicate embed
PJN_CarePlanTimeline.cmp embeds the updater twice.
2. [ ] Harden aura subscribe races
Store the subscribe promise synchronously.
3. [✓] Extend Case watch list
Add the program developer and record type fields.
4. [ ] Refresh old parent on reparent
Publish for the old care plan too.
5. [ ] Type something.
Submit
6. Chat about this
Enter to select · ↑/↓ to navigate · Esc to cancel
`

const claudeFirstQuestionView = `
[48;2;55;55;55m Reconnect [0m ☐ Offline ☐ Feedback ✓ Submit →
What should drive reconnect attempts?
❯ 1. Backoff + jitter
Reduce synchronized retries.
2. Fixed retry
Keep timing predictable.
3. Event-driven
Retry only after connectivity changes.
4. Type something.
5. Chat about this
Enter to select · ↑/↓ to navigate · Esc to cancel
`

const codexQuestionView = `
[48;2;240;240;240m  [2mQuestion 1/3 (3 unanswered)
[48;2;240;240;240m  [38;5;6mWhere should the reusable adapter boundary sit?
[48;2;240;240;240m
[48;2;240;240;240m  [1m› 1. Domain port (Recommended)  Define transport-agnostic contracts.
[48;2;240;240;240m    2. Protocol boundary           Keep domain logic relay-shaped.
[48;2;240;240;240m    3. Workflow adapter            Encapsulate the full workflow.
[48;2;240;240;240m    4. None of the above           Optionally, add details in notes (tab).
[48;2;240;240;240m
[48;2;240;240;240m  tab to add notes | enter to submit answer | ←/→ to navigate questions
`

func TestParseClaudeMultiQuestion(t *testing.T) {
	interaction := Parse(multiQuestionView, "claude")
	if interaction == nil {
		t.Fatal("question was not parsed")
	}
	if interaction.Kind != "multi_select" ||
		interaction.Question != "Which further improvements should be included?" ||
		len(interaction.Options) != 4 ||
		!interaction.Options[0].Selected ||
		interaction.Options[1].Description != "Store the subscribe promise synchronously." ||
		interaction.Other.Selected ||
		interaction.SubmitLabel != "Submit" ||
		!interaction.CanChat ||
		interaction.Focus != (Focus{Kind: "option", Index: 0}) {
		t.Fatalf("interaction = %+v", interaction)
	}
}

func TestParseClaudeSingleQuestionPosition(t *testing.T) {
	interaction := Parse(claudeFirstQuestionView, "claude")
	if interaction == nil {
		t.Fatal("question was not parsed")
	}
	if interaction.Kind != "single_select" || interaction.SubmitLabel != "Next" ||
		interaction.QuestionIndex != 1 || interaction.QuestionTotal != 3 ||
		len(interaction.Options) != 3 || interaction.CanChat != true {
		t.Fatalf("interaction = %+v", interaction)
	}
}

func TestParseCodexQuestion(t *testing.T) {
	interaction := Parse(codexQuestionView, "codex")
	if interaction == nil {
		t.Fatal("question was not parsed")
	}
	if interaction.Kind != "single_select" ||
		interaction.Question != "Where should the reusable adapter boundary sit?" ||
		len(interaction.Options) != 3 ||
		interaction.Options[0].Label != "Domain port (Recommended)" ||
		interaction.Options[0].Description != "Define transport-agnostic contracts." ||
		interaction.SubmitLabel != "Next" ||
		interaction.Other.Label != "None of the above" ||
		interaction.Agent != "codex" {
		t.Fatalf("interaction = %+v", interaction)
	}
}

func TestHistoricalQuestionIsNotLive(t *testing.T) {
	approval := `
Plan complete. Claude is ready to proceed.
Do you want to proceed?
❯ 1. Yes, clear context and auto-accept edits
2. Yes, auto-accept edits
3. Yes, manually approve edits
4. Type here to tell Claude what to change
`
	if LayoutHint(claudeFirstQuestionView + approval) {
		t.Fatal("historical question was treated as live")
	}
}

func TestQuestionIdentityIgnoresSelections(t *testing.T) {
	initial := Parse(multiQuestionView, "claude")
	selected := Parse(
		stringReplace(multiQuestionView, "1. [✓] Remove", "1. [ ] Remove"),
		"claude",
	)
	if initial == nil || selected == nil || initial.ID != selected.ID {
		t.Fatalf("ids differ: %v, %v", initial, selected)
	}
}

func TestApprovalDetailsUsesLastSequentialMenu(t *testing.T) {
	summary, command, options := ApprovalDetails(`
❯ rm -rf build-cache
1. Old yes
2. Old no

Do you want to proceed?
1. Yes, single permission
2. Trust, always allow
3. No (tab to edit)
`)
	if !strings.Contains(summary, "rm -rf build-cache") {
		t.Fatalf("summary = %q", summary)
	}
	if command != "rm -rf build-cache" {
		t.Fatalf("command = %q", command)
	}
	want := []string{"Yes, single permission", "Trust, always allow", "No (tab to edit)"}
	if !reflect.DeepEqual(options, want) {
		t.Fatalf("options = %#v, want %#v", options, want)
	}
}

func stringReplace(value, old, replacement string) string {
	for index := 0; index+len(old) <= len(value); index++ {
		if value[index:index+len(old)] == old {
			return value[:index] + replacement + value[index+len(old):]
		}
	}
	return value
}
