package question

import (
	"reflect"
	"strings"
	"testing"
)

func TestPlanOMPChoiceAndCustomAnswer(t *testing.T) {
	interaction := Parse(claudeAskQuestionView, "omp")
	if interaction == nil {
		t.Fatal("question was not parsed")
	}
	choice := PlanInput(interaction, InputIntent{Selected: []int{0}})
	wantChoice := []InputStep{{Keys: []string{"Up", "Enter"}}}
	if !reflect.DeepEqual(choice, wantChoice) {
		t.Fatalf("choice plan = %#v, want %#v", choice, wantChoice)
	}

	custom := PlanInput(interaction, InputIntent{OtherSelected: true, OtherText: "Quiet lakes"})
	wantCustom := []InputStep{
		{Keys: []string{"Down", "Down", "Enter", "Ctrl+U"}},
		{Text: "Quiet lakes"},
		{Keys: []string{"Enter"}},
	}
	if !reflect.DeepEqual(custom, wantCustom) {
		t.Fatalf("custom plan = %#v, want %#v", custom, wantCustom)
	}

	multiView := strings.ReplaceAll(claudeAskQuestionView, "○", "☐")
	multiView = strings.Replace(multiView, "☐ Budget", "☑ Budget", 1)
	multi := Parse(multiView, "omp")
	if multi == nil || multi.Kind != "multi_select" {
		t.Fatalf("multi-select interaction = %+v", multi)
	}
	multiChoice := PlanInput(multi, InputIntent{Selected: []int{0, 2}})
	wantMulti := []InputStep{
		{Keys: []string{"Down", "Enter"}},
		{Keys: []string{"Right"}},
	}
	if !reflect.DeepEqual(multiChoice, wantMulti) {
		t.Fatalf("multi-select plan = %#v, want %#v", multiChoice, wantMulti)
	}

	review := Parse(ompReviewView, "omp")
	if review == nil {
		t.Fatal("review was not parsed")
	}
	submit := PlanInput(review, InputIntent{Selected: []int{0}})
	if !reflect.DeepEqual(submit, []InputStep{{Keys: []string{"Enter"}}}) {
		t.Fatalf("review plan = %#v", submit)
	}
}

func TestPlanQoderChoiceAndNavigation(t *testing.T) {
	interaction := Parse(qoderQuestionView, "qodercli")
	if interaction == nil {
		t.Fatal("question was not parsed")
	}
	choice := PlanInput(interaction, InputIntent{Selected: []int{2}})
	wantChoice := []InputStep{{Keys: []string{"Down", "Down", "Enter"}}}
	if !reflect.DeepEqual(choice, wantChoice) {
		t.Fatalf("choice plan = %#v, want %#v", choice, wantChoice)
	}
	previous := PlanInput(interaction, InputIntent{Navigation: "previous"})
	if !reflect.DeepEqual(previous, []InputStep{{Keys: []string{"Left"}}}) {
		t.Fatalf("previous plan = %#v", previous)
	}
}

func TestPlanQoderCustomAnswerOpensInput(t *testing.T) {
	interaction := Parse(qoderQuestionView, "qodercli")
	if interaction == nil {
		t.Fatal("question was not parsed")
	}
	steps := PlanInput(interaction, InputIntent{OtherSelected: true, OtherText: "Surprise me"})
	want := []InputStep{
		{Keys: []string{"Down", "Down", "Down", "Down", "Enter", "Ctrl+U"}},
		{Text: "Surprise me"},
		{Keys: []string{"Enter"}},
	}
	if !reflect.DeepEqual(steps, want) {
		t.Fatalf("custom answer plan = %#v, want %#v", steps, want)
	}
}

func TestPlanQoderMultiSelectUsesNextControl(t *testing.T) {
	interaction := Parse(qoderMultiQuestionView, "qodercli")
	if interaction == nil {
		t.Fatal("question was not parsed")
	}
	steps := PlanInput(interaction, InputIntent{Selected: []int{0, 2}})
	want := []InputStep{{Keys: []string{"Down", "Down", "Down", "Enter"}}}
	if !reflect.DeepEqual(steps, want) {
		t.Fatalf("multi-select plan = %#v, want %#v", steps, want)
	}
}

func TestPlanQoderMultiSelectCustomAnswerReturnsToNext(t *testing.T) {
	interaction := Parse(qoderMultiQuestionView, "qodercli")
	if interaction == nil {
		t.Fatal("question was not parsed")
	}
	steps := PlanInput(interaction, InputIntent{
		Selected:      []int{0, 2},
		OtherSelected: true,
		OtherText:     "Live music",
	})
	want := []InputStep{
		{Keys: []string{"Down", "Down", "Down", "Down", "Enter", "Ctrl+U"}},
		{Text: "Live music"},
		{Keys: []string{"Enter"}},
		{Keys: []string{"Up", "Enter"}},
	}
	if !reflect.DeepEqual(steps, want) {
		t.Fatalf("multi-select custom plan = %#v, want %#v", steps, want)
	}
}

func TestPlanQoderReviewChoiceAndPrevious(t *testing.T) {
	interaction := Parse(qoderReviewView, "qodercli")
	if interaction == nil {
		t.Fatal("review was not parsed")
	}
	cancel := PlanInput(interaction, InputIntent{Selected: []int{1}})
	if !reflect.DeepEqual(cancel, []InputStep{{Keys: []string{"Down", "Enter"}}}) {
		t.Fatalf("cancel plan = %#v", cancel)
	}
	previous := PlanInput(interaction, InputIntent{Navigation: "previous"})
	if !reflect.DeepEqual(previous, []InputStep{{Keys: []string{"Left"}}}) {
		t.Fatalf("previous plan = %#v", previous)
	}
}

func TestPlanCodexActiveNotesReplacesTextInPlace(t *testing.T) {
	interaction := Parse(
		attentionFixture(t, "codex-first-question_with_notes.ansi"),
		"codex",
	)
	if interaction == nil || !interaction.NotesActive {
		t.Fatalf("interaction = %+v", interaction)
	}
	steps := PlanInput(interaction, InputIntent{
		OtherSelected: true,
		OtherText:     "Climb down instead",
	})
	want := []InputStep{
		{Keys: []string{"Ctrl+U"}},
		{Text: "Climb down instead"},
		{Keys: []string{"Enter"}},
	}
	if !reflect.DeepEqual(steps, want) {
		t.Fatalf("notes plan = %#v, want %#v", steps, want)
	}
}

func TestPlanQoderActiveNotesReplacesTextBeforeSubmitting(t *testing.T) {
	interaction := Parse(
		attentionFixture(t, "qodercli-multi-questions-and-notes.ansi"),
		"qodercli",
	)
	if interaction == nil || !interaction.NotesActive {
		t.Fatalf("interaction = %+v", interaction)
	}
	steps := PlanInput(interaction, InputIntent{
		OtherSelected: true,
		OtherText:     "Updated travel notes",
	})
	want := []InputStep{
		{Keys: []string{"Ctrl+U"}},
		{Text: "Updated travel notes"},
		{Keys: []string{"Enter"}},
		{Keys: []string{"Up", "Enter"}},
	}
	if !reflect.DeepEqual(steps, want) {
		t.Fatalf("notes plan = %#v, want %#v", steps, want)
	}
}

func TestPlanQoderActiveNotesCanBeRemoved(t *testing.T) {
	interaction := Parse(
		attentionFixture(t, "qodercli-multi-questions-and-notes.ansi"),
		"qodercli",
	)
	if interaction == nil || !interaction.NotesActive {
		t.Fatalf("interaction = %+v", interaction)
	}
	var selected []int
	for index, option := range interaction.Options {
		if option.Selected {
			selected = append(selected, index)
		}
	}
	steps := PlanInput(interaction, InputIntent{Selected: selected})
	want := []InputStep{
		{Keys: []string{"Ctrl+U"}},
		{Keys: []string{"Enter"}},
		{Keys: []string{"Enter"}},
		{Keys: []string{"Up", "Enter"}},
	}
	if !reflect.DeepEqual(steps, want) {
		t.Fatalf("notes removal plan = %#v, want %#v", steps, want)
	}
}

func TestPlanOpenCodeChoiceAndTabNavigation(t *testing.T) {
	interaction := Parse(
		attentionFixture(t, "opencode-single-question.ansi"),
		"opencode",
	)
	if interaction == nil {
		t.Fatal("question was not parsed")
	}
	choice := PlanInput(interaction, InputIntent{Selected: []int{0}})
	if !reflect.DeepEqual(choice, []InputStep{{
		Keys: []string{"Up", "Up", "Enter"},
	}}) {
		t.Fatalf("choice plan = %#v", choice)
	}
	previous := PlanInput(interaction, InputIntent{Navigation: "previous"})
	if !reflect.DeepEqual(previous, []InputStep{{Keys: []string{"Shift+Tab"}}}) {
		t.Fatalf("previous plan = %#v", previous)
	}
	next := PlanInput(interaction, InputIntent{Navigation: "next"})
	if !reflect.DeepEqual(next, []InputStep{{Keys: []string{"Tab"}}}) {
		t.Fatalf("next plan = %#v", next)
	}
}

func TestPlanOpenCodeMultiSelectAdvancesWithTab(t *testing.T) {
	interaction := Parse(
		attentionFixture(t, "opencode-questions-with-multiple-choice-answers.ansi"),
		"opencode",
	)
	if interaction == nil {
		t.Fatal("question was not parsed")
	}
	steps := PlanInput(interaction, InputIntent{Selected: []int{1, 2}})
	want := []InputStep{
		{Keys: []string{"Enter"}},
		{Keys: []string{"Tab"}},
	}
	if !reflect.DeepEqual(steps, want) {
		t.Fatalf("multi-select plan = %#v, want %#v", steps, want)
	}
}

func TestPlanOpenCodeActiveCustomAnswerAcceptsThenAdvances(t *testing.T) {
	interaction := Parse(
		attentionFixture(t, "opencode-questions-multiple-choice-with-free-text-edited.ansi"),
		"opencode",
	)
	if interaction == nil || !interaction.NotesActive {
		t.Fatalf("interaction = %+v", interaction)
	}
	steps := PlanInput(interaction, InputIntent{
		Selected:      []int{1, 2, 4},
		OtherSelected: true,
		OtherText:     "Updated custom answer",
	})
	want := []InputStep{
		{Keys: []string{"Ctrl+U"}},
		{Text: "Updated custom answer"},
		{Keys: []string{"Enter"}},
		{Keys: []string{"Tab"}},
	}
	if !reflect.DeepEqual(steps, want) {
		t.Fatalf("custom answer plan = %#v, want %#v", steps, want)
	}
}

func TestPlanOpenCodeReviewSubmitsWithEnter(t *testing.T) {
	interaction := Parse(
		attentionFixture(t, "opencode-many-questions-confirm.ansi"),
		"opencode",
	)
	if interaction == nil || !interaction.Other.Hidden {
		t.Fatalf("interaction = %+v", interaction)
	}
	steps := PlanInput(interaction, InputIntent{Selected: []int{0}})
	if !reflect.DeepEqual(steps, []InputStep{{Keys: []string{"Enter"}}}) {
		t.Fatalf("review plan = %#v", steps)
	}
}

func TestPlanClaudeCapturedCustomAnswerReplacesTextInPlace(t *testing.T) {
	interaction := Parse(
		attentionFixture(t, "claude-plan-one-question-notes.ansi"),
		"claude",
	)
	if interaction == nil || !interaction.Other.Selected {
		t.Fatalf("interaction = %+v", interaction)
	}
	steps := PlanInput(interaction, InputIntent{
		OtherSelected: true,
		OtherText:     "Surprise me",
	})
	want := []InputStep{
		{Keys: []string{"Ctrl+U"}},
		{Text: "Surprise me"},
		{Keys: []string{"Enter"}},
	}
	if !reflect.DeepEqual(steps, want) {
		t.Fatalf("custom answer plan = %#v, want %#v", steps, want)
	}
}

func TestPlanClaudeChoiceClearsStaleNote(t *testing.T) {
	interaction := Parse(claudeStaleNoteQuestionView, "claude")
	if interaction == nil || interaction.Other.Text != "Hhrr" {
		t.Fatalf("interaction = %+v", interaction)
	}
	steps := PlanInput(interaction, InputIntent{Selected: []int{2}})
	want := []InputStep{
		{Keys: []string{"Down", "Down", "Down", "Down", "Ctrl+U"}},
		{Keys: []string{"Up", "Up", "Enter"}},
	}
	if !reflect.DeepEqual(steps, want) {
		t.Fatalf("choice plan = %#v, want %#v", steps, want)
	}
}

func TestPlanClaudeCapturedReviewChoiceAndPrevious(t *testing.T) {
	interaction := Parse(
		attentionFixture(t, "claude-plan-submit-answers.ansi"),
		"claude",
	)
	if interaction == nil || !interaction.Other.Hidden {
		t.Fatalf("interaction = %+v", interaction)
	}
	cancel := PlanInput(interaction, InputIntent{Selected: []int{1}})
	if !reflect.DeepEqual(cancel, []InputStep{{Keys: []string{"Down", "Enter"}}}) {
		t.Fatalf("cancel plan = %#v", cancel)
	}
	previous := PlanInput(interaction, InputIntent{Navigation: "previous"})
	if !reflect.DeepEqual(previous, []InputStep{{Keys: []string{"Left"}}}) {
		t.Fatalf("previous plan = %#v", previous)
	}
}
