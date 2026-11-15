package question

import (
	"regexp"
	"strconv"
	"strings"

	"github.com/0cv/herdr-mobile-relay/internal/textutil"
)

var (
	claudeReviewPattern      = regexp.MustCompile(`(?i)^\s*([❯›]?)\s*(\d+)\.\s*(submit answers|cancel)\s*$`)
	claudeActiveTabPattern   = regexp.MustCompile(`\x1b\[[^m]*48[^m]*m`)
	claudeSessionRulePattern = regexp.MustCompile(`^─{3,} \S.* ─$`)
	claudeTaskPanelPattern   = regexp.MustCompile(
		`^(?:\d+ tasks? \(\d+ done, (?:\d+ in progress, )?\d+ open\)|[◻◼✔] \S.*|… \+\d+ (?:in progress|pending|completed)(?:, \d+ (?:pending|completed))*)$`,
	)
)

func parseClaude(text string) *Interaction {
	lines := cleanLines(text)
	var checkboxRows []optionRow
	for index, line := range lines {
		match := checkboxPattern.FindStringSubmatch(line)
		if match == nil {
			continue
		}
		checkboxRows = append(checkboxRows, optionRow{
			line:    index,
			focus:   match[1] != "",
			checked: strings.TrimSpace(match[3]) != "",
			text:    match[4],
		})
	}
	compactClaudeRows(checkboxRows)
	submitIndex, submitFocus, submitLabel := -1, false, ""
	chatIndex, chatFocus := -1, false
	for index, line := range lines {
		if match := submitPattern.FindStringSubmatch(line); match != nil {
			submitIndex, submitFocus, submitLabel = index, match[1] != "", title(match[2])
		}
		if match := chatPattern.FindStringSubmatch(line); match != nil {
			chatIndex, chatFocus = index, match[1] != ""
		}
	}
	if len(checkboxRows) >= 2 && submitIndex >= 0 {
		end := submitIndex
		if chatIndex >= 0 && chatIndex < end {
			end = chatIndex
		}
		all, focus := optionsFromRows(lines, checkboxRows, end)
		if submitFocus {
			focus = Focus{Kind: "submit"}
		}
		if chatFocus {
			focus = Focus{Kind: "chat"}
		}
		options, otherItem := splitOther(all)
		question := prompt(lines, checkboxRows[0].line)
		current, total := claudePosition(text)
		if submitLabel == "Submit" && current > 0 && current < total {
			submitLabel = "Next"
		}
		interaction := &Interaction{
			Kind:           "multi_select",
			Question:       question,
			Options:        options,
			Other:          claudeOther(otherItem),
			SubmitLabel:    submitLabel,
			CanChat:        chatIndex >= 0,
			CanGoBack:      current > 1,
			QuestionIndex:  current,
			QuestionTotal:  total,
			Focus:          focus,
			AllOptionCount: len(all),
			Agent:          "claude",
		}
		return finishInteraction(interaction)
	}

	if chatIndex < 0 {
		return parseClaudeReview(text, lines)
	}
	rows := lastRun(scanRows(lines, 0, chatIndex, rowScan{restartAtOne: true}))
	if len(rows) < 3 {
		return nil
	}
	compactClaudeRows(rows)
	all, focus := optionsFromRows(lines, rows, chatIndex)
	if chatFocus {
		focus = Focus{Kind: "chat"}
	}
	options, otherItem := splitOther(all)
	current, total := claudePosition(text)
	interaction := &Interaction{
		Kind:           "single_select",
		Question:       prompt(lines, rows[0].line),
		Options:        options,
		Other:          claudeOther(otherItem),
		SubmitLabel:    nextOrSubmit(current, total),
		CanChat:        true,
		CanGoBack:      current > 1,
		QuestionIndex:  current,
		QuestionTotal:  total,
		Focus:          focus,
		AllOptionCount: len(all),
		Agent:          "claude",
	}
	// Leftover typed text only marks the custom answer as chosen while no
	// option row carries the confirmed selection; otherwise a stale note from
	// an earlier visit would override the real answer.
	if interaction.Other.Text != "" && !interaction.Other.Selected {
		selectedElsewhere := false
		for _, option := range options {
			if option.Selected {
				selectedElsewhere = true
				break
			}
		}
		if !selectedElsewhere {
			interaction.Other.Selected = true
		}
	}
	return finishInteraction(interaction)
}

func claudeOther(item Option) Other {
	if otherPattern.MatchString(item.Label) {
		return Other{Selected: item.Selected}
	}
	return Other{Selected: item.Selected, Text: item.Label}
}

func parseClaudeReview(text string, lines []string) *Interaction {
	reviewIndex := -1
	for index := len(lines) - 1; index >= 0; index-- {
		if strings.EqualFold(strings.TrimSpace(lines[index]), "Review your answers") {
			reviewIndex = index
			break
		}
	}
	if reviewIndex < 0 {
		return nil
	}

	var options []Option
	focus := Focus{Kind: "option"}
	for index := reviewIndex + 1; index < len(lines); index++ {
		match := claudeReviewPattern.FindStringSubmatch(lines[index])
		if match == nil {
			continue
		}
		number, _ := strconv.Atoi(match[2])
		if number != len(options)+1 {
			return nil
		}
		optionIndex := len(options)
		options = append(options, Option{Index: optionIndex, Label: textutil.Compact(match[3], 500)})
		if match[1] != "" {
			focus = Focus{Kind: "option", Index: optionIndex}
		}
	}
	if len(options) != 2 ||
		!strings.EqualFold(options[0].Label, "Submit answers") ||
		!strings.EqualFold(options[1].Label, "Cancel") {
		return nil
	}

	var summary []SummaryEntry
	prompt := ""
	answerOpen := false
	for _, line := range lines[reviewIndex+1:] {
		trimmed := strings.TrimSpace(line)
		switch {
		case strings.HasPrefix(trimmed, "●"):
			prompt = strings.TrimSpace(strings.TrimPrefix(trimmed, "●"))
			answerOpen = false
		case strings.HasPrefix(trimmed, "→") && prompt != "":
			answer := strings.TrimSpace(strings.TrimPrefix(trimmed, "→"))
			if answer == "__other__" {
				answer = CustomAnswerPlaceholder
			}
			summary = append(summary, SummaryEntry{Question: strings.TrimSuffix(prompt, "?"), Answer: answer})
			prompt = ""
			answerOpen = true
		case trimmed == "" || claudeReviewPattern.MatchString(line):
			prompt = ""
			answerOpen = false
		case prompt != "":
			// Long prompts wrap onto continuation lines in the terminal.
			prompt += " " + trimmed
		case answerOpen && len(summary) > 0:
			summary[len(summary)-1].Answer += " " + trimmed
		}
	}
	options[0].Description = summaryLines(summary, 1000)
	options[0].Summary = summary

	current, total := claudePosition(text)
	if current < 1 || total < current {
		current = len(summary) + 1
		total = current
	}
	return reviewInteraction("claude", options, focus, strings.Contains(text, "←"), current, total)
}

func prompt(lines []string, firstOption int) string {
	start, end := -1, -1
	for index := firstOption - 1; index >= 0; index-- {
		line := lines[index]
		lower := strings.ToLower(line)
		boundary := line == "" || submitPattern.MatchString(line) || chatPattern.MatchString(line) ||
			strings.Contains(lower, "enter to select") ||
			(strings.Contains(line, "Submit") && strings.Contains(line, "→"))
		if boundary {
			if end >= 0 {
				break
			}
			continue
		}
		if end < 0 {
			end = index
		}
		start = index
	}
	if end < 0 {
		return "Claude Code needs an answer"
	}
	return textutil.Compact(strings.Join(lines[start:end+1], " "), 1000)
}

func claudePosition(text string) (int, int) {
	for _, raw := range strings.Split(text, "\n") {
		clean := cleanLine(raw)
		if !strings.Contains(clean, "→") || !strings.Contains(strings.ToLower(clean), "submit") {
			continue
		}
		active := claudeActiveTabPattern.FindStringIndex(raw)
		if active == nil {
			return 0, 0
		}
		prefix := cleanLine(raw[:active[0]])
		current := countMarks(prefix) + 1
		beforeSubmit := clean
		if index := strings.Index(strings.ToLower(beforeSubmit), "submit"); index >= 0 {
			beforeSubmit = beforeSubmit[:index]
		}
		total := countMarks(beforeSubmit)
		activeText := cleanLine(raw[active[1]:])
		if !strings.ContainsAny(activeText, "☐☒☑✓✔") || total < current {
			total++
		}
		if current >= 1 && total >= current {
			return current, total
		}
	}
	return 0, 0
}

func countMarks(value string) int {
	count := 0
	for _, char := range value {
		if strings.ContainsRune("☐☒☑✓✔", char) {
			count++
		}
	}
	return count
}

func compactClaudeRows(rows []optionRow) {
	compactRows(rows, true)
	for index := range rows {
		rows[index].text = strings.TrimSpace(rows[index].text)
	}
}
