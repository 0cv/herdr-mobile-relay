package question

import "slices"

// InputIntent is the provider-neutral action requested by the phone.
type InputIntent struct {
	Navigation    string
	Clarify       bool
	Selected      []int
	OtherSelected bool
	OtherText     string
}

// InputStep is one ordered terminal operation. Keeping text separate from key
// presses lets the coordinator preserve the dispatch uncertainty boundary.
type InputStep struct {
	Keys []string
	Text string
}

// PlanInput translates the shared question protocol into the keyboard contract
// of the detected terminal form.
func PlanInput(interaction *Interaction, intent InputIntent) []InputStep {
	switch intent.Navigation {
	case "previous":
		if interaction.Agent == "opencode" {
			return []InputStep{{Keys: []string{"Shift+Tab"}}}
		}
		return []InputStep{{Keys: []string{"Left"}}}
	case "next":
		if interaction.Agent == "opencode" {
			return []InputStep{{Keys: []string{"Tab"}}}
		}
		return []InputStep{{Keys: []string{"Right"}}}
	}
	if intent.Clarify {
		return []InputStep{{Keys: append(navigationKeys(interaction, Focus{Kind: "chat"}), "Enter")}}
	}

	switch interaction.Agent {
	case "codex":
		return planCodexInput(interaction, intent)
	case "qoder":
		return planQoderInput(interaction, intent)
	case "opencode":
		return planOpenCodeInput(interaction, intent)
	case "omp":
		return planOMPInput(interaction, intent)
	default:
		return planClaudeInput(interaction, intent)
	}
}

func planCodexInput(interaction *Interaction, intent InputIntent) []InputStep {
	if len(intent.Selected) > 0 {
		target := Focus{Kind: "option", Index: intent.Selected[0]}
		return []InputStep{{Keys: append(navigationKeys(interaction, target), "Enter")}}
	}
	target := Focus{Kind: "option", Index: interaction.AllOptionCount - 1}
	keys := navigationKeys(interaction, target)
	if intent.OtherText == "" {
		return []InputStep{{Keys: append(keys, "Enter")}}
	}
	if interaction.NotesActive {
		keys = []string{"Ctrl+U"}
	} else {
		keys = append(keys, "Tab")
	}
	steps := make([]InputStep, 0, 3)
	if len(keys) > 0 {
		steps = append(steps, InputStep{Keys: keys})
	}
	steps = append(steps,
		InputStep{Text: intent.OtherText},
		InputStep{Keys: []string{"Enter"}},
	)
	return steps
}

func planQoderInput(interaction *Interaction, intent InputIntent) []InputStep {
	if interaction.Kind == "single_select" {
		if interaction.NotesActive {
			steps := []InputStep{{Keys: []string{"Ctrl+U"}}}
			if intent.OtherSelected && intent.OtherText != "" {
				steps = append(steps, InputStep{Text: intent.OtherText})
			}
			steps = append(steps, InputStep{Keys: []string{"Enter"}})
			if intent.OtherSelected {
				return steps
			}
			current := *interaction
			current.Focus = Focus{Kind: "other"}
			target := Focus{Kind: "option", Index: intent.Selected[0]}
			return append(steps, InputStep{Keys: append(qoderNavigationKeys(&current, target), "Enter")})
		}
		if len(intent.Selected) > 0 {
			target := Focus{Kind: "option", Index: intent.Selected[0]}
			return []InputStep{{Keys: append(qoderNavigationKeys(interaction, target), "Enter")}}
		}
		target := Focus{Kind: "other"}
		keys := append(qoderNavigationKeys(interaction, target), "Enter", "Ctrl+U")
		steps := []InputStep{{Keys: keys}}
		if intent.OtherText != "" {
			steps = append(steps, InputStep{Text: intent.OtherText})
		}
		return append(steps, InputStep{Keys: []string{"Enter"}})
	}
	return planQoderMultiInput(interaction, intent)
}

func planQoderMultiInput(interaction *Interaction, intent InputIntent) []InputStep {
	current := *interaction
	var steps []InputStep
	notesHandled := false
	if current.NotesActive {
		steps = append(steps, InputStep{Keys: []string{"Ctrl+U"}})
		if intent.OtherSelected && intent.OtherText != "" {
			steps = append(steps, InputStep{Text: intent.OtherText})
		}
		steps = append(steps, InputStep{Keys: []string{"Enter"}})
		current.Focus = Focus{Kind: "other"}
		current.NotesActive = false
		current.Other.Selected = true
		current.Other.Text = intent.OtherText
		notesHandled = true
	}
	for index, option := range current.Options {
		desired := containsInt(intent.Selected, index)
		if option.Selected == desired {
			continue
		}
		target := Focus{Kind: "option", Index: index}
		steps = append(steps, InputStep{Keys: append(qoderNavigationKeys(&current, target), "Enter")})
		current.Focus = target
		current.Options[index].Selected = desired
	}
	otherTarget := Focus{Kind: "other"}
	switch {
	case intent.OtherSelected && !notesHandled:
		keys := append(qoderNavigationKeys(&current, otherTarget), "Enter", "Ctrl+U")
		steps = append(steps, InputStep{Keys: keys})
		if intent.OtherText != "" {
			steps = append(steps, InputStep{Text: intent.OtherText})
		}
		steps = append(steps, InputStep{Keys: []string{"Enter"}})
		current.Focus = otherTarget
	case !intent.OtherSelected && current.Other.Selected:
		steps = append(steps, InputStep{
			Keys: append(qoderNavigationKeys(&current, otherTarget), "Enter"),
		})
		current.Focus = otherTarget
	}
	submit := Focus{Kind: "submit"}
	return append(steps, InputStep{Keys: append(qoderNavigationKeys(&current, submit), "Enter")})
}

func planOpenCodeInput(interaction *Interaction, intent InputIntent) []InputStep {
	if interaction.Other.Hidden {
		return []InputStep{{Keys: []string{"Enter"}}}
	}
	if interaction.Kind == "multi_select" {
		return planOpenCodeMultiInput(interaction, intent)
	}
	if interaction.NotesActive {
		if intent.OtherSelected {
			steps := []InputStep{{Keys: []string{"Ctrl+U"}}}
			if intent.OtherText != "" {
				steps = append(steps, InputStep{Text: intent.OtherText})
			}
			return append(steps,
				InputStep{Keys: []string{"Enter"}},
				InputStep{Keys: []string{"Enter"}},
			)
		}
		current := *interaction
		current.Focus = Focus{Kind: "other"}
		target := Focus{Kind: "option", Index: intent.Selected[0]}
		return []InputStep{
			{Keys: []string{"Escape"}},
			{Keys: append(openCodeNavigationKeys(&current, target), "Enter")},
		}
	}
	if len(intent.Selected) > 0 {
		target := Focus{Kind: "option", Index: intent.Selected[0]}
		return []InputStep{{Keys: append(openCodeNavigationKeys(interaction, target), "Enter")}}
	}
	target := Focus{Kind: "other"}
	keys := append(openCodeNavigationKeys(interaction, target), "Enter", "Ctrl+U")
	steps := []InputStep{{Keys: keys}}
	if intent.OtherText != "" {
		steps = append(steps, InputStep{Text: intent.OtherText})
	}
	return append(steps,
		InputStep{Keys: []string{"Enter"}},
		InputStep{Keys: []string{"Enter"}},
	)
}

func planOpenCodeMultiInput(interaction *Interaction, intent InputIntent) []InputStep {
	current := *interaction
	var steps []InputStep
	if current.NotesActive {
		if intent.OtherSelected {
			steps = append(steps, InputStep{Keys: []string{"Ctrl+U"}})
			if intent.OtherText != "" {
				steps = append(steps, InputStep{Text: intent.OtherText})
			}
			steps = append(steps, InputStep{Keys: []string{"Enter"}})
			current.Other.Selected = true
			current.Other.Text = intent.OtherText
		} else {
			steps = append(steps, InputStep{Keys: []string{"Escape"}})
		}
		current.Focus = Focus{Kind: "other"}
		current.NotesActive = false
	}
	for index, option := range current.Options {
		desired := containsInt(intent.Selected, index)
		if option.Selected == desired {
			continue
		}
		target := Focus{Kind: "option", Index: index}
		steps = append(steps, InputStep{Keys: append(openCodeNavigationKeys(&current, target), "Enter")})
		current.Focus = target
		current.Options[index].Selected = desired
	}

	otherTarget := Focus{Kind: "other"}
	switch {
	case intent.OtherSelected &&
		(!current.Other.Selected || current.Other.Text != intent.OtherText):
		keys := append(openCodeNavigationKeys(&current, otherTarget), "Enter", "Ctrl+U")
		steps = append(steps, InputStep{Keys: keys})
		if intent.OtherText != "" {
			steps = append(steps, InputStep{Text: intent.OtherText})
		}
		steps = append(steps, InputStep{Keys: []string{"Enter"}})
		current.Focus = otherTarget
	case !intent.OtherSelected && current.Other.Selected:
		steps = append(steps, InputStep{
			Keys: append(openCodeNavigationKeys(&current, otherTarget), "Enter"),
		})
		current.Focus = otherTarget
	}
	return append(steps, InputStep{Keys: []string{"Tab"}})
}

func planOMPInput(interaction *Interaction, intent InputIntent) []InputStep {
	if interaction.Kind == "single_select" {
		if len(intent.Selected) > 0 {
			target := Focus{Kind: "option", Index: intent.Selected[0]}
			return []InputStep{{Keys: append(navigationKeys(interaction, target), "Enter")}}
		}
		target := Focus{Kind: "option", Index: interaction.AllOptionCount - 1}
		steps := []InputStep{{Keys: append(navigationKeys(interaction, target), "Enter", "Ctrl+U")}}
		if intent.OtherText != "" {
			steps = append(steps, InputStep{Text: intent.OtherText})
		}
		return append(steps, InputStep{Keys: []string{"Enter"}})
	}

	current := *interaction
	var steps []InputStep
	for index, option := range current.Options {
		desired := containsInt(intent.Selected, index)
		if option.Selected == desired {
			continue
		}
		target := Focus{Kind: "option", Index: index}
		steps = append(steps, InputStep{Keys: append(navigationKeys(&current, target), "Enter")})
		current.Focus = target
	}
	if intent.OtherSelected {
		target := Focus{Kind: "option", Index: current.AllOptionCount - 1}
		steps = append(steps, InputStep{Keys: append(navigationKeys(&current, target), "Enter", "Ctrl+U")})
		if intent.OtherText != "" {
			steps = append(steps, InputStep{Text: intent.OtherText})
		}
		return append(steps, InputStep{Keys: []string{"Enter"}})
	}
	if current.QuestionTotal > 1 {
		return append(steps, InputStep{Keys: []string{"Right"}})
	}
	if len(intent.Selected) == 0 {
		return steps
	}
	distance := len(current.Options) - current.Focus.Index
	keys := make([]string, distance)
	for index := range keys {
		keys[index] = "Down"
	}
	return append(steps, InputStep{Keys: append(keys, "Enter")})
}

func planClaudeInput(interaction *Interaction, intent InputIntent) []InputStep {
	if interaction.Kind == "single_select" {
		if len(intent.Selected) > 0 {
			current := *interaction
			var steps []InputStep
			if !current.Other.Hidden && current.Other.Text != "" && intent.OtherText == "" {
				otherTarget := Focus{Kind: "option", Index: current.AllOptionCount - 1}
				steps = append(steps, InputStep{Keys: append(navigationKeys(&current, otherTarget), "Ctrl+U")})
				current.Focus = otherTarget
			}
			target := Focus{Kind: "option", Index: intent.Selected[0]}
			return append(steps, InputStep{Keys: append(navigationKeys(&current, target), "Enter")})
		}
		target := Focus{Kind: "option", Index: interaction.AllOptionCount - 1}
		keys := append(navigationKeys(interaction, target), "Ctrl+U")
		steps := []InputStep{{Keys: keys}}
		if intent.OtherText != "" {
			steps = append(steps, InputStep{Text: intent.OtherText})
		}
		return append(steps, InputStep{Keys: []string{"Enter"}})
	}

	current := *interaction
	var steps []InputStep
	for index, option := range current.Options {
		desired := containsInt(intent.Selected, index)
		if option.Selected == desired {
			continue
		}
		target := Focus{Kind: "option", Index: index}
		steps = append(steps, InputStep{Keys: append(navigationKeys(&current, target), "Enter")})
		current.Focus = target
		current.Options[index].Selected = desired
	}
	otherTarget := Focus{Kind: "option", Index: current.AllOptionCount - 1}
	if current.Other.Text != intent.OtherText {
		steps = append(steps, InputStep{Keys: append(navigationKeys(&current, otherTarget), "Ctrl+U")})
		current.Focus = otherTarget
		if intent.OtherText != "" {
			steps = append(steps, InputStep{Text: intent.OtherText})
		}
	}
	if current.Other.Selected != intent.OtherSelected {
		steps = append(steps, InputStep{Keys: append(navigationKeys(&current, otherTarget), "Enter")})
		current.Focus = otherTarget
	}
	submit := Focus{Kind: "submit"}
	return append(steps, InputStep{Keys: append(navigationKeys(&current, submit), "Enter")})
}

func navigationKeys(interaction *Interaction, target Focus) []string {
	position := func(focus Focus) int {
		switch focus.Kind {
		case "option":
			return focus.Index
		case "submit":
			if interaction.Kind == "multi_select" {
				return interaction.AllOptionCount
			}
		case "chat":
			position := interaction.AllOptionCount
			if interaction.Kind == "multi_select" {
				position++
			}
			return position
		}
		return 0
	}
	distance := position(target) - position(interaction.Focus)
	key := "Down"
	if distance < 0 {
		key = "Up"
		distance = -distance
	}
	keys := make([]string, distance)
	for index := range keys {
		keys[index] = key
	}
	return keys
}

func qoderNavigationKeys(interaction *Interaction, target Focus) []string {
	position := func(focus Focus) int {
		switch focus.Kind {
		case "option":
			return focus.Index
		case "submit":
			return len(interaction.Options)
		case "other":
			position := len(interaction.Options)
			if interaction.Kind == "multi_select" {
				position++
			}
			return position
		}
		return 0
	}
	distance := position(target) - position(interaction.Focus)
	key := "Down"
	if distance < 0 {
		key = "Up"
		distance = -distance
	}
	keys := make([]string, distance)
	for index := range keys {
		keys[index] = key
	}
	return keys
}

func openCodeNavigationKeys(interaction *Interaction, target Focus) []string {
	position := func(focus Focus) int {
		if focus.Kind == "other" {
			return len(interaction.Options)
		}
		return focus.Index
	}
	distance := position(target) - position(interaction.Focus)
	key := "Down"
	if distance < 0 {
		key = "Up"
		distance = -distance
	}
	keys := make([]string, distance)
	for index := range keys {
		keys[index] = key
	}
	return keys
}

func containsInt(values []int, target int) bool {
	return slices.Contains(values, target)
}
