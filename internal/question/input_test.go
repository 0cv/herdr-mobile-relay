package question

import (
	"reflect"
	"testing"
)

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
