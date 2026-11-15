package question

import (
	"regexp"
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

	rows := lastRun(scanRows(lines, 0, footerIndex, rowScan{
		checkboxes:   true,
		restartAtOne: true,
		endRunOnGap:  true,
	}))
	compactRows(rows, true)
	for index := range rows {
		rows[index].focus = openCodeFocusPattern.MatchString(rawLines[rows[index].line])
	}
	if len(rows) < 2 || !otherPattern.MatchString(rows[len(rows)-1].text) {
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

	all, focus := optionsFromRows(lines, rows, footerIndex)
	options, otherItem := splitOther(all)
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
	if rows[0].checkbox {
		kind = "multi_select"
	}
	if current < 1 || total < current {
		current, total = 1, 1
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
		SubmitLabel:    nextOrSubmit(current, total),
		CanGoBack:      current > 1,
		QuestionIndex:  current,
		QuestionTotal:  total,
		Focus:          focus,
		AllOptionCount: len(all),
		Agent:          "opencode",
		NotesActive:    notesActive,
	}
	return finishInteraction(interaction)
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
	options := []Option{{
		Index:       0,
		Label:       "Submit answers",
		Description: summaryLines(summary, 1000),
		Summary:     summary,
	}}
	return reviewInteraction("opencode", options, Focus{Kind: "option"}, true, current, total)
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
