package question

import (
	"regexp"
	"strings"

	"github.com/0cv/herdr-mobile-relay/internal/textutil"
)

var (
	qoderActivePattern = regexp.MustCompile(`\x1b\[[^m]*48(?:;|:)[^m]*m\s*([^\x1b]+)`)
	qoderReviewPattern = regexp.MustCompile(`(?i)^\s*([❯›]?)\s*(submit answers|cancel ask)\s*$`)
)

func parseQoder(text string) *Interaction {
	rawLines := strings.Split(strings.ReplaceAll(text, "\r\n", "\n"), "\n")
	lines := make([]string, len(rawLines))
	headerIndex, footerIndex := -1, -1
	current, total := 0, 0
	for index, raw := range rawLines {
		lines[index] = cleanLine(raw)
		if qoderHeader(lines[index]) {
			headerIndex = index
			current, total = qoderPosition(raw)
			if current == 0 && total == 0 {
				current, total = 1, 1
			}
		}
		if headerIndex >= 0 && qoderFooter(lines[index]) {
			footerIndex = index
		}
	}
	if headerIndex < 0 || footerIndex <= headerIndex || current < 1 || total < current {
		if headerIndex < 0 || footerIndex <= headerIndex {
			return nil
		}
		return parseQoderReview(lines, headerIndex, footerIndex, total)
	}

	scanned := lastRun(scanRows(lines, headerIndex+1, footerIndex, rowScan{checkboxes: true}))
	compactRows(scanned, false)
	var checkboxRows, menuRows []optionRow
	for _, item := range scanned {
		if item.checkbox {
			checkboxRows = append(checkboxRows, item)
		} else {
			menuRows = append(menuRows, item)
		}
	}

	kind := "single_select"
	rows := menuRows
	submitRow := optionRow{}
	if len(checkboxRows) >= 2 {
		kind = "multi_select"
		rows = checkboxRows
		for _, item := range menuRows {
			label := strings.TrimSpace(strings.TrimSuffix(item.text, "→"))
			if strings.EqualFold(label, "next") || strings.EqualFold(label, "submit") {
				submitRow = item
			}
		}
	}
	var otherRow optionRow
	for _, item := range menuRows {
		if otherPattern.MatchString(item.text) {
			otherRow = item
		}
	}
	if len(rows) < 2 || otherRow.line == 0 {
		return nil
	}
	if kind == "single_select" {
		if !otherPattern.MatchString(rows[len(rows)-1].text) {
			return nil
		}
		rows = rows[:len(rows)-1]
	} else if submitRow.line == 0 {
		return nil
	}

	firstOption := rows[0].line
	questionText := ""
	for index := firstOption - 1; index > headerIndex; index-- {
		candidate := strings.TrimSpace(lines[index])
		if candidate == "" || strings.Trim(candidate, "─━═_—│| ") == "" {
			continue
		}
		lower := strings.ToLower(candidate)
		if strings.HasPrefix(candidate, "(") &&
			strings.Contains(lower, "select all") {
			continue
		}
		questionText = textutil.Compact(candidate, 1000)
		break
	}
	if questionText == "" {
		return nil
	}

	end := otherRow.line
	if submitRow.line > 0 {
		end = submitRow.line
	}
	options, focus := optionsFromRows(lines, rows, end)
	if submitRow.focus {
		focus = Focus{Kind: "submit"}
	}
	if otherRow.focus {
		focus = Focus{Kind: "other"}
	}
	notes := ""
	notesActive := otherRow.focus &&
		strings.Contains(strings.ToLower(lines[footerIndex]), "enter submit")
	if notesActive {
		var noteLines []string
		for _, line := range lines[otherRow.line+1 : footerIndex] {
			if line != "" {
				noteLines = append(noteLines, strings.TrimSpace(line))
			}
		}
		notes = textutil.Compact(strings.Join(noteLines, " "), 20000)
	}
	interaction := &Interaction{
		Kind:     kind,
		Question: questionText,
		Options:  options,
		Other: Other{
			Selected:    otherRow.checked || notesActive,
			Text:        notes,
			Label:       otherRow.text,
			Placeholder: "Type an answer",
		},
		SubmitLabel:    nextOrSubmit(current, total),
		CanGoBack:      current > 1,
		QuestionIndex:  current,
		QuestionTotal:  total,
		Focus:          focus,
		AllOptionCount: len(options) + 1,
		Agent:          "qoder",
		NotesActive:    notesActive,
	}
	return finishInteraction(interaction)
}

func parseQoderReview(lines []string, headerIndex, footerIndex, questionTotal int) *Interaction {
	reviewIndex := -1
	for index := headerIndex + 1; index < footerIndex; index++ {
		if strings.EqualFold(strings.TrimSpace(lines[index]), "Review your answers:") {
			reviewIndex = index
			break
		}
	}
	if reviewIndex < 0 {
		return nil
	}

	var summary []SummaryEntry
	var options []Option
	focus := Focus{Kind: "option"}
	for index := reviewIndex + 1; index < footerIndex; index++ {
		line := strings.TrimSpace(lines[index])
		if match := qoderReviewPattern.FindStringSubmatch(line); match != nil {
			optionIndex := len(options)
			options = append(options, Option{Index: optionIndex, Label: title(match[2])})
			if match[1] != "" {
				focus = Focus{Kind: "option", Index: optionIndex}
			}
			continue
		}
		if strings.Contains(line, "→") {
			parts := strings.SplitN(line, "→", 2)
			summary = append(summary, SummaryEntry{
				Question: strings.TrimSpace(parts[0]),
				Answer:   strings.TrimSpace(parts[1]),
			})
		}
	}
	if len(options) != 2 {
		return nil
	}
	options[0].Description = summaryLines(summary, 1000)
	options[0].Summary = summary
	step := questionTotal + 1
	return reviewInteraction("qoder", options, focus, true, step, step)
}

func qoderHeader(line string) bool {
	lower := strings.ToLower(line)
	if strings.EqualFold(strings.TrimSpace(line), "Asking User") {
		return true
	}
	return strings.Contains(lower, "asking user") &&
		strings.Contains(line, "·") &&
		strings.Contains(lower, "submit")
}

func qoderFooter(line string) bool {
	lower := strings.ToLower(line)
	if strings.Contains(lower, "esc back") &&
		strings.Contains(lower, "enter") &&
		(strings.Contains(lower, "navigate") ||
			strings.Contains(lower, "enter submit")) {
		return true
	}
	return strings.Contains(lower, "switch") &&
		(strings.Contains(lower, "enter select") ||
			strings.Contains(lower, "enter toggle") ||
			strings.Contains(lower, "enter submit")) &&
		(strings.Contains(lower, "esc back") || strings.Contains(lower, "esc cancel")) &&
		(strings.Contains(line, "←") || strings.Contains(lower, "tab/"))
}

func qoderPosition(raw string) (int, int) {
	clean := cleanLine(raw)
	dot := strings.Index(clean, "·")
	if dot < 0 {
		return 0, 0
	}
	parts := strings.Split(clean[dot+len("·"):], ">")
	var tabs []string
	for _, part := range parts {
		label := strings.TrimSpace(part)
		if label == "" || strings.EqualFold(label, "submit") {
			continue
		}
		tabs = append(tabs, label)
	}
	active := qoderActivePattern.FindStringSubmatch(raw)
	if len(active) < 2 {
		return 0, len(tabs)
	}
	activeLabel := strings.TrimSpace(cleanLine(active[1]))
	for index, label := range tabs {
		if label == activeLabel {
			return index + 1, len(tabs)
		}
	}
	return 0, len(tabs)
}
