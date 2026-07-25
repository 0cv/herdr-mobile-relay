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

const codexFinalQuestionView = `
Question 2/2 (1 unanswered)
What parts of the pipeline should the plan cover?

› 1. check + release workflows only (Recommended)  Plan and edits limited to .github workflows and local release scripts used by these workflows.
  2. Full shipping pipeline including web deploy   Include Pages deploy, app bundle checks, and release orchestration end-to-end.
  3. Current + future guardrails                   Propose process upgrades too, like changelog, audit policy, branch/publish controls, and release
                                                   playbooks.
  4. None of the above                             Optionally, add details in notes (tab).

tab to add notes | enter to submit all | ←/→ to navigate questions | esc to interrupt
`

const codexSingleQuestionView = `
Question 1/1 (1 unanswered)
Which hardening priority should lead the implementation plan?

› 1. Security and supply-chain hardening (Recommended)  Add integrity/supply-chain checks, provenance, stricter artifact verification, and secure
                                                        release gating.
  2. Reliability/reproducibility hardening              Eliminate flakiness and strengthen deterministic build, verification, and environment
                                                        checks.
  3. Balance both with minimal extra runtime            Apply a small, practical set of changes across reliability and security with low CI
                                                        overhead.
  4. None of the above                                  Optionally, add details in notes (tab).

tab to add notes | enter to submit answer | esc to interrupt
`

const qoderQuestionView = `
Asking User · [1m[38;2;42;219;92m[48;2;36;74;50m Vibe [0m > Distance > Activities > Budget > Submit
────────────────────────────────────────────────────────────
What type of trip vibe are you going for?

❯ 1. Nature & outdoors
     Hiking, lakes, forests, national parks
  2. City & culture
     Museums, architecture, food scene, nightlife
  3. Relaxation
     Spa, slow pace, good food, no tight schedule
  4. Adventure
     Mix of activities, road trip, exploring new places
  5. Type Something

Tab/←→ switch · Enter select · Esc back
`

const qoderMultiQuestionView = `
Asking User · Vibe > Distance > [1m[38;2;42;219;92m[48;2;36;74;50m Activities [0m > Budget > Submit
────────────────────────────────────────────────────────────
Which activities interest you? (select all that apply)
(Select all that apply)

  1. [x] Food & wine
     Local restaurants, wine, breweries, markets
  2. [ ] Outdoor sports
     Trails, kayaking, cycling, climbing
❯ 3. [x] History & culture
     Castles, old towns, galleries, festivals
  4. [ ] Beach / water
     Swimming, sunbathing, boat trips
  5. [ ] All of the above
     Select all options
  6. Next →
     Continue to submit
  7. Type Something

Tab/←→ switch · Enter toggle · Esc back
`

const qoderReviewView = `
Asking User · Vibe > Distance > Activities > Budget > [1m[38;2;42;219;92m[48;2;36;74;50m Submit [0m
────────────────────────────────────────────────────────────
Review your answers:

Vibe → Relaxation
Distance → Open to flying
Activities → Food & wine
Budget → Mid-range

Ready to submit?

❯ Submit answers
  Cancel ask

Tab/←→ switch · Enter select · Esc cancel
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

func TestParseCodexFinalQuestionSubmitAll(t *testing.T) {
	interaction := Parse(codexFinalQuestionView, "codex")
	if interaction == nil {
		t.Fatal("question was not parsed")
	}
	if interaction.Kind != "single_select" ||
		interaction.Question != "What parts of the pipeline should the plan cover?" ||
		len(interaction.Options) != 3 ||
		interaction.Options[2].Label != "Current + future guardrails" ||
		interaction.Options[2].Description != "Propose process upgrades too, like changelog, audit policy, branch/publish controls, and release playbooks." ||
		interaction.Other.Label != "None of the above" ||
		interaction.SubmitLabel != "Submit" ||
		!interaction.CanGoBack ||
		interaction.QuestionIndex != 2 ||
		interaction.QuestionTotal != 2 ||
		interaction.AllOptionCount != 4 {
		t.Fatalf("interaction = %+v", interaction)
	}
}

func TestParseCodexSingleQuestionWithoutNavigationFooter(t *testing.T) {
	interaction := Parse(codexSingleQuestionView, "codex")
	if interaction == nil {
		t.Fatal("single question was not parsed")
	}
	if interaction.Kind != "single_select" ||
		interaction.Question != "Which hardening priority should lead the implementation plan?" ||
		len(interaction.Options) != 3 ||
		interaction.Options[0].Description != "Add integrity/supply-chain checks, provenance, stricter artifact verification, and secure release gating." ||
		interaction.Other.Label != "None of the above" ||
		interaction.SubmitLabel != "Submit" ||
		interaction.CanGoBack ||
		interaction.QuestionIndex != 1 ||
		interaction.QuestionTotal != 1 {
		t.Fatalf("interaction = %+v", interaction)
	}
}

func TestParseQoderQuestion(t *testing.T) {
	interaction := Parse(qoderQuestionView, "qodercli")
	if interaction == nil {
		t.Fatal("question was not parsed")
	}
	if interaction.Kind != "single_select" ||
		interaction.Question != "What type of trip vibe are you going for?" ||
		len(interaction.Options) != 4 ||
		interaction.Options[0].Label != "Nature & outdoors" ||
		interaction.Options[0].Description != "Hiking, lakes, forests, national parks" ||
		interaction.Other.Label != "Type Something" ||
		interaction.SubmitLabel != "Next" ||
		interaction.CanGoBack ||
		interaction.QuestionIndex != 1 ||
		interaction.QuestionTotal != 4 ||
		interaction.Agent != "qoder" {
		t.Fatalf("interaction = %+v", interaction)
	}
}

func TestParseQoderMultiQuestion(t *testing.T) {
	interaction := Parse(qoderMultiQuestionView, "qodercli")
	if interaction == nil {
		t.Fatal("question was not parsed")
	}
	if interaction.Kind != "multi_select" ||
		interaction.Question != "Which activities interest you? (select all that apply)" ||
		len(interaction.Options) != 5 ||
		!interaction.Options[0].Selected ||
		interaction.Options[1].Selected ||
		!interaction.Options[2].Selected ||
		interaction.Options[4].Label != "All of the above" ||
		interaction.Options[4].Description != "Select all options" ||
		interaction.Other.Label != "Type Something" ||
		interaction.Focus != (Focus{Kind: "option", Index: 2}) ||
		interaction.QuestionIndex != 3 ||
		interaction.QuestionTotal != 4 ||
		!interaction.CanGoBack {
		t.Fatalf("interaction = %+v", interaction)
	}
}

func TestParseQoderReview(t *testing.T) {
	interaction := Parse(qoderReviewView, "qodercli")
	if interaction == nil {
		t.Fatal("review was not parsed")
	}
	if interaction.Kind != "single_select" ||
		interaction.Question != "Review your answers and choose what to do" ||
		len(interaction.Options) != 2 ||
		interaction.Options[0].Label != "Submit answers" ||
		interaction.Options[1].Label != "Cancel ask" ||
		interaction.Options[0].Description != "Vibe: Relaxation · Distance: Open to flying · Activities: Food & wine · Budget: Mid-range" ||
		!interaction.Other.Hidden ||
		interaction.SubmitLabel != "Continue" ||
		!interaction.CanGoBack ||
		interaction.QuestionIndex != 5 ||
		interaction.QuestionTotal != 5 ||
		interaction.Focus != (Focus{Kind: "option", Index: 0}) {
		t.Fatalf("interaction = %+v", interaction)
	}
}

func TestQoderHistoricalQuestionIsNotLive(t *testing.T) {
	if LayoutHint(qoderQuestionView + "\nWorking on your itinerary.") {
		t.Fatal("historical Qoder question was treated as live")
	}
}

func TestQuestionSupportsKnownTerminalAdapters(t *testing.T) {
	for _, agent := range []string{"claude", "codex", "qoder", "qodercli"} {
		if !Supports(agent) {
			t.Errorf("%q is not supported", agent)
		}
	}
	if Supports("opencode") {
		t.Fatal("unknown keyboard protocol was treated as supported")
	}
}

func TestCodexFooterSubmitVariants(t *testing.T) {
	for _, submitText := range []string{"answer", "answers", "all"} {
		footer := "tab to add notes | enter to submit " + submitText + " | ←/→ to navigate questions"
		if !codexFooter(footer) {
			t.Errorf("footer with %q was not recognized", submitText)
		}
	}
	if codexFooter("enter to submit all") {
		t.Fatal("plain submit text was recognized as a question control footer")
	}
	if !codexFooter("tab to add notes | enter to submit answer | esc to interrupt") {
		t.Fatal("keyboard-driven question footer was not recognized")
	}
	withoutNavigation := strings.Replace(
		codexQuestionView,
		" | ←/→ to navigate questions",
		" | esc to interrupt",
		1,
	)
	if interaction := Parse(withoutNavigation, "codex"); interaction == nil ||
		interaction.QuestionTotal != 3 {
		t.Fatalf("multi-question keyboard form without navigation hint = %+v", interaction)
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
	if LayoutHint(codexSingleQuestionView + "\nPlan complete.") {
		t.Fatal("historical single Codex question was treated as live")
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

func TestLatestCompletedResponsePreservesFullAnswer(t *testing.T) {
	content := []string{
		"• Earlier answer.",
		"─ Worked for 2s ─",
		"",
		"› New question",
		"",
		"\x1b[32m• First line of the latest answer.\x1b[0m",
		"  Second line.",
		"  Third line.",
		"  Fourth line.",
		"  Fifth line.",
		"  Sixth line.",
		"  Seventh line.",
		"  Eighth line.",
		"  Ninth line.",
		"  Tenth line.",
		"  Eleventh line.",
		"  Twelfth line.",
		"  Thirteenth line.",
		"  Fourteenth line.",
		"",
		"─ Worked for 2m 19s ─",
		"",
		"› Next question",
	}
	want := strings.Join([]string{
		"First line of the latest answer.",
		"Second line.",
		"Third line.",
		"Fourth line.",
		"Fifth line.",
		"Sixth line.",
		"Seventh line.",
		"Eighth line.",
		"Ninth line.",
		"Tenth line.",
		"Eleventh line.",
		"Twelfth line.",
		"Thirteenth line.",
		"Fourteenth line.",
	}, "\n")
	if got := LatestCompletedResponse(strings.Join(content, "\n")); got != want {
		t.Fatalf("LatestCompletedResponse() = %q, want %q", got, want)
	}
}

func TestLatestCompletedResponseRequiresCompletedTurn(t *testing.T) {
	if got := LatestCompletedResponse("● Still working\n  More output"); got != "" {
		t.Fatalf("LatestCompletedResponse() = %q, want empty", got)
	}
	claude := "● The implementation is ready.\n  It works.\n\n✻ Crunched for 1m 49s\n❯ "
	if got := LatestCompletedResponse(claude); got != "The implementation is ready.\nIt works." {
		t.Fatalf("LatestCompletedResponse() = %q, want complete Claude response", got)
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
