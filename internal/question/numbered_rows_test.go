package question

import (
	"reflect"
	"strings"
	"testing"
)

func numberedRowsPane(agent string, rows []string) string {
	switch agent {
	case "claude":
		return "Pick one\n" + strings.Join(rows, "\n") + "\n9. Chat about this\nEnter to select · ↑/↓ to navigate · Esc to cancel\n"
	case "codex":
		var body []string
		for index, row := range rows {
			prefix := "  "
			if index == 0 {
				prefix = "› "
			}
			body = append(body, prefix+row+"  details")
		}
		return "Question 1/1 (1 unanswered)\nPick one\n\n" + strings.Join(body, "\n") +
			"\n\ntab to add notes | enter to submit answer | esc to interrupt\n"
	case "qoder":
		return "Asking User\n────────────────────────────────────────\nPick one\n\n" +
			strings.Join(rows, "\n") + "\n\nTab/←→ switch · Enter select · Esc back\n"
	}
	var body []string
	for _, row := range rows {
		body = append(body, "  ┃  "+row)
	}
	return "  ┃  Pick one\n" + strings.Join(body, "\n") + "\n  ┃  ↑↓ select  enter submit  esc dismiss\n"
}

func TestParseNumberedRows(t *testing.T) {
	sequences := []struct {
		name string
		rows []string
	}{
		{"second one after a list", []string{"1. Alpha", "2. Beta", "3. Type something.", "1. Gamma", "2. Delta", "3. Type something."}},
		{"out of order number between rows", []string{"1. Alpha", "5. Stray", "2. Beta", "3. Type something."}},
		{"missing number in the middle", []string{"1. Alpha", "2. Beta", "4. Delta", "5. Type something."}},
		{"missing number after the last row", []string{"1. Alpha", "2. Beta", "3. Type something.", "5. Omega"}},
	}
	type outcome struct {
		options []string
		other   string
	}
	want := map[string]*outcome{
		"claude/second one after a list":             {[]string{"Gamma", "Delta"}, ""},
		"codex/second one after a list":              {[]string{"Alpha", "Beta"}, "Type something. 1. Gamma 2. Delta 3. Type s"},
		"qoder/second one after a list":              {[]string{"Alpha", "Beta"}, "Type something."},
		"opencode/second one after a list":           {[]string{"Gamma", "Delta"}, "Type something."},
		"claude/out of order number between rows":    {[]string{"Alpha", "Beta"}, ""},
		"codex/out of order number between rows":     {[]string{"Alpha 5. Stray", "Beta"}, "Type something."},
		"qoder/out of order number between rows":     {[]string{"Alpha", "Beta"}, "Type something."},
		"opencode/out of order number between rows":  nil,
		"claude/missing number in the middle":        nil,
		"codex/missing number in the middle":         nil,
		"qoder/missing number in the middle":         nil,
		"opencode/missing number in the middle":      nil,
		"claude/missing number after the last row":   {[]string{"Alpha", "Beta"}, ""},
		"codex/missing number after the last row":    {[]string{"Alpha", "Beta"}, "Type something. 5. Omega"},
		"qoder/missing number after the last row":    {[]string{"Alpha", "Beta"}, "Type something."},
		"opencode/missing number after the last row": {[]string{"Alpha", "Beta"}, "Type something."},
	}
	for _, agent := range []string{"claude", "codex", "qoder", "opencode"} {
		for _, sequence := range sequences {
			name := agent + "/" + sequence.name
			t.Run(name, func(t *testing.T) {
				var got *outcome
				if interaction := Parse(numberedRowsPane(agent, sequence.rows), agent); interaction != nil {
					got = &outcome{other: interaction.Other.Label}
					for _, option := range interaction.Options {
						got.options = append(got.options, option.Label)
					}
				}
				if !reflect.DeepEqual(got, want[name]) {
					t.Fatalf("got %+v, want %+v", got, want[name])
				}
			})
		}
	}
}
