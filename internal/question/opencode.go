package question

import (
	"regexp"
	"strconv"
	"strings"

	"github.com/0cv/herdr-mobile-relay/internal/textutil"
)

var (
	openCodeFocusPattern  = regexp.MustCompile(`\x1b\[[^m]*48(?:;|:)2(?:;|:)30(?:;|:)30(?:;|:)30m`)
	openCodeActivePattern = regexp.MustCompile(
		`\x1b\[[^m]*48(?:;|:)2(?:;|:)157(?:;|:)124(?:;|:)216m([^\x1b]*)`,
	)
	openCodeColumnPattern = regexp.MustCompile(`\s{20,}`)
)

func parseOpenCode(text string) *Interaction {
	rawLines := strings.Split(strings.ReplaceAll(text, "\r\n", "\n"), "\n")
	lines := make([]string, len(rawLines))
	footerIndex := -1
	for index, raw := range rawLines {
		lines[index] = cleanOpenCodeLine(raw)
		if openCodeFooter(lines[index]) {
			footerIndex = index
		}
	}
	if footerIndex < 0 || !openCodeTailIsEmpty(lines, footerIndex) {
		return nil
	}

	current, total := openCodePosition(rawLines, lines, footerIndex)
	if interaction := parseOpenCodeReview(lines, footerIndex, current, total); interaction != nil {
		return interaction
	}

	type row struct {
		line     int
		focus    bool
		label    string
		selected bool
	}
	var runs [][]row
	var currentRun []row
	expected := 1
	flush := func() {
		if len(currentRun) > 0 {
			runs = append(runs, currentRun)
		}
		currentRun = nil
		expected = 1
	}
	for index := 0; index < footerIndex; index++ {
		match := checkboxPattern.FindStringSubmatch(lines[index])
		checkbox := match != nil
		if match == nil {
			match = menuPattern.FindStringSubmatch(lines[index])
		}
		if match == nil {
			continue
		}
		number, _ := strconv.Atoi(match[2])
		if number == 1 {
			flush()
		}
		if number != expected {
			flush()
			continue
		}
		labelIndex := 3
		selected := false
		if checkbox {
			labelIndex = 4
			selected = strings.TrimSpace(match[3]) != ""
		}
		label := textutil.Compact(match[labelIndex], 500)
		if selectedPattern.MatchString(label) {
			selected = true
			label = strings.TrimSpace(selectedPattern.ReplaceAllString(label, ""))
		}
		currentRun = append(currentRun, row{
			line:     index,
			focus:    openCodeFocusPattern.MatchString(rawLines[index]),
			label:    label,
			selected: selected,
		})
		expected++
	}
	flush()
	if len(runs) == 0 {
		return nil
	}
	rows := runs[len(runs)-1]
	if len(rows) < 2 || !otherPattern.MatchString(rows[len(rows)-1].label) {
		return nil
	}

	firstOption := rows[0].line
	questionText := ""
	for index := firstOption - 1; index >= 0; index-- {
		candidate := strings.TrimSpace(lines[index])
		if candidate == "" {
			continue
		}
		questionText = textutil.Compact(candidate, 1000)
		break
	}
	if questionText == "" {
		return nil
	}

	all := make([]Option, 0, len(rows))
	focus := Focus{Kind: "option"}
	for index, item := range rows {
		end := footerIndex
		if index+1 < len(rows) {
			end = rows[index+1].line
		}
		all = append(all, Option{
			Index:       index,
			Label:       item.label,
			Description: description(lines, item.line, end),
			Selected:    item.selected,
		})
		if item.focus {
			focus = Focus{Kind: "option", Index: index}
		}
	}
	otherItem := all[len(all)-1]
	options := all[:len(all)-1]
	otherText := otherItem.Description
	if strings.EqualFold(otherText, otherItem.Label) {
		otherText = ""
	}
	otherItem.Description = ""
	notesActive := focus == (Focus{Kind: "option", Index: len(all) - 1}) &&
		!otherItem.Selected
	if notesActive {
		focus = Focus{Kind: "other"}
	}

	kind := "single_select"
	if checkboxPattern.MatchString(lines[rows[0].line]) {
		kind = "multi_select"
	}
	if current < 1 || total < current {
		current, total = 1, 1
	}
	submitLabel := "Next"
	if current == total {
		submitLabel = "Submit"
	}
	interaction := &Interaction{
		Kind:     kind,
		Question: questionText,
		Options:  options,
		Other: Other{
			Selected:    otherItem.Selected,
			Text:        otherText,
			Label:       otherItem.Label,
			Placeholder: "Type your own answer",
		},
		SubmitLabel:    submitLabel,
		CanGoBack:      current > 1,
		QuestionIndex:  current,
		QuestionTotal:  total,
		Focus:          focus,
		AllOptionCount: len(all),
		Agent:          "opencode",
		NotesActive:    notesActive,
	}
	interaction.ID = interactionID(interaction)
	return interaction
}

func parseOpenCodeReview(
	lines []string,
	footerIndex, current, total int,
) *Interaction {
	reviewIndex := -1
	for index := footerIndex - 1; index >= 0; index-- {
		if strings.EqualFold(strings.TrimSpace(lines[index]), "Review") {
			reviewIndex = index
			break
		}
	}
	if reviewIndex < 0 {
		return nil
	}

	var summary []SummaryEntry
	for _, line := range lines[reviewIndex+1 : footerIndex] {
		line = strings.TrimSpace(line)
		if line == "" || !strings.Contains(line, ":") {
			continue
		}
		summary = append(summary, splitSummaryEntry(line))
	}
	if len(summary) == 0 {
		return nil
	}
	if total < 1 {
		total = len(summary)
	}
	current = total + 1
	total = current
	interaction := &Interaction{
		Kind:     "single_select",
		Question: "Review your answers and choose what to do",
		Options: []Option{{
			Index:       0,
			Label:       "Submit answers",
			Description: summaryLines(summary, 1000),
			Summary:     summary,
		}},
		Other:          Other{Hidden: true},
		SubmitLabel:    "Continue",
		CanGoBack:      true,
		QuestionIndex:  current,
		QuestionTotal:  total,
		Focus:          Focus{Kind: "option"},
		AllOptionCount: 1,
		Agent:          "opencode",
	}
	interaction.ID = interactionID(interaction)
	return interaction
}

func openCodeLayoutHint(text string) bool {
	rawLines := strings.Split(strings.ReplaceAll(text, "\r\n", "\n"), "\n")
	lines := make([]string, len(rawLines))
	footerIndex := -1
	for index, raw := range rawLines {
		lines[index] = cleanOpenCodeLine(raw)
		if openCodeFooter(lines[index]) {
			footerIndex = index
		}
	}
	return footerIndex >= 0 && openCodeTailIsEmpty(lines, footerIndex)
}

func openCodeTailIsEmpty(lines []string, footerIndex int) bool {
	for _, line := range lines[footerIndex+1:] {
		if strings.TrimSpace(line) != "" {
			return false
		}
	}
	return true
}

func openCodeFooter(line string) bool {
	lower := strings.ToLower(line)
	if !strings.Contains(lower, "esc dismiss") {
		return false
	}
	if strings.Contains(lower, "enter submit") {
		return true
	}
	return strings.Contains(lower, "↑↓ select") &&
		(strings.Contains(lower, "enter confirm") ||
			strings.Contains(lower, "enter toggle"))
}

func openCodePosition(rawLines, lines []string, footerIndex int) (int, int) {
	for index := footerIndex - 1; index >= 0; index-- {
		if !strings.Contains(strings.ToLower(lines[index]), "confirm") {
			continue
		}
		labels := openCodeTabs(lines[index])
		if len(labels) < 2 {
			continue
		}
		active := openCodeActiveTab(rawLines[index])
		if active == "" {
			return 0, len(labels) - 1
		}
		for tabIndex, label := range labels {
			if strings.EqualFold(active, label) {
				if strings.EqualFold(label, "confirm") {
					return len(labels), len(labels) - 1
				}
				return tabIndex + 1, len(labels) - 1
			}
		}
	}
	return 0, 0
}

func openCodeTabs(line string) []string {
	var labels []string
	for _, label := range columnGapPattern.Split(strings.TrimSpace(line), -1) {
		label = strings.TrimSpace(label)
		if label != "" {
			labels = append(labels, label)
		}
	}
	return labels
}

func openCodeActiveTab(raw string) string {
	var parts []string
	for _, match := range openCodeActivePattern.FindAllStringSubmatch(raw, -1) {
		if len(match) > 1 {
			parts = append(parts, match[1])
		}
	}
	return textutil.Compact(strings.Join(parts, ""), 500)
}

func cleanOpenCodeLine(line string) string {
	line = strings.TrimRight(
		textutil.StripANSI(strings.ReplaceAll(line, "\r", "")),
		" \t",
	)
	trimmed := strings.TrimLeft(line, " \t")
	if !strings.HasPrefix(trimmed, "┃") {
		return ""
	}
	trimmed = strings.TrimPrefix(trimmed, "┃")
	if gap := openCodeColumnPattern.FindStringIndex(trimmed); gap != nil {
		trimmed = trimmed[:gap[0]]
	}
	return strings.TrimSpace(trimmed)
}
