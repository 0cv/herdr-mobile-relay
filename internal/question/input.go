package question

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
		return []InputStep{{Keys: []string{"Left"}}}
	case "next":
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
	if interaction.NotesActive {
		keys = nil
	}
	if intent.OtherText == "" {
		return []InputStep{{Keys: append(keys, "Enter")}}
	}
	if !interaction.NotesActive {
		keys = append(keys, "Tab")
	}
	keys = append(keys, "Ctrl+U")
	return []InputStep{
		{Keys: keys},
		{Text: intent.OtherText},
		{Keys: []string{"Enter"}},
	}
}

func planQoderInput(interaction *Interaction, intent InputIntent) []InputStep {
	if interaction.Kind == "single_select" {
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
	if intent.OtherSelected {
		keys := append(qoderNavigationKeys(&current, otherTarget), "Enter", "Ctrl+U")
		steps = append(steps, InputStep{Keys: keys})
		if intent.OtherText != "" {
			steps = append(steps, InputStep{Text: intent.OtherText})
		}
		steps = append(steps, InputStep{Keys: []string{"Enter"}})
		current.Focus = otherTarget
	}
	submit := Focus{Kind: "submit"}
	return append(steps, InputStep{Keys: append(qoderNavigationKeys(&current, submit), "Enter")})
}

func planClaudeInput(interaction *Interaction, intent InputIntent) []InputStep {
	if interaction.Kind == "single_select" {
		if len(intent.Selected) > 0 {
			target := Focus{Kind: "option", Index: intent.Selected[0]}
			return []InputStep{{Keys: append(navigationKeys(interaction, target), "Enter")}}
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

func containsInt(values []int, target int) bool {
	for _, value := range values {
		if value == target {
			return true
		}
	}
	return false
}
