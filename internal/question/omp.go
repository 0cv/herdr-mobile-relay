package question

import (
	"regexp"
	"strconv"
	"strings"

	"github.com/0cv/herdr-mobile-relay/internal/textutil"
)

var (
	ompAskHeaderPattern = regexp.MustCompile(`(?i)^╭[─━═_—\s]*Ask(?:[─━═_—\s]|$)`)
	// Only live omp Ask dialogs render this footer. Enter shows as "Enter" (18.0),
	// ⏎, or the Nerd Font Enter icon U+F0311.
	ompFooterPattern       = regexp.MustCompile(`(?i)(?:enter|⏎|\x{F0311})\s+(?:select|submit|next)\b`)
	ompOptionPattern       = regexp.MustCompile(`^\s*([❯›>]?)\s*(☑|☐|◉|○|||||\[[xX ]\]|\([oO ]\))\s+(.+?)\s*$`)
	ompFrameMetaPattern    = regexp.MustCompile(`\[\s*([\p{L}\p{N}_-]+)\s*\]\s*[·•]\s*options:\s*\d+`)
	ompProgressPattern     = regexp.MustCompile(`\s+\((\d+)\s*/\s*(\d+)\)\s*$`)
	ompReviewSubmitPattern = regexp.MustCompile(`(?i)^\s*[❯›>]?\s*submit\s*$`)
	ompTabIDPattern        = regexp.MustCompile(`^[\p{L}\p{N}_.-]+$`)
	ompActiveTabPattern    = regexp.MustCompile(`\x1b\[1m(?:\x1b\[[0-9;:]*m)*\s*([\p{L}\p{N}_.-]+)`)
)

type ompOptionRow struct {
	line     int
	focus    bool
	marker   string
	label    string
	selected bool
}

func ompLayoutHint(p *pane) bool {
	lines := p.trimmed()
	start := lastLineMatching(lines, ompAskHeaderPattern)
	if start < 0 {
		return false
	}
	footer := -1
	options := 0
	review := false
	reviewSubmit := false
	for index := start + 1; index < len(lines); index++ {
		line := lines[index]
		if ompOptionPattern.MatchString(line) {
			options++
		}
		if strings.EqualFold(line, "review answers") {
			review = true
		}
		if review && ompReviewSubmitPattern.MatchString(line) {
			reviewSubmit = true
		}
		if ompFooterPattern.MatchString(line) {
			footer = index
		}
		// The footer must sit inside this Ask box: tool approvals render
		// "enter select" too, but below this border, so the break rejects it.
		if strings.HasPrefix(line, "╰") {
			break
		}
	}
	if footer < 0 || (options < 2 && !reviewSubmit) {
		return false
	}
	// omp's empty input box at the bottom means the question was already answered.
	// Other lines below the box are ignored.
	return !ruleFramedStatusPrompt(lines[footer+1:])
}

// ompCleanLines additionally strips the Ask frame's inner scrollbar column,
// which survives edge cleanup when the frame content overflows.
func ompCleanLines(p *pane) []string {
	trimmed := p.trimmed()
	lines := make([]string, len(trimmed))
	for index, line := range trimmed {
		lines[index] = strings.TrimRight(line, " \t│█▉▊▋▌▍▎▏▁▂▃▄▅▆▇▀")
	}
	return lines
}

func parseOMP(p *pane) *Interaction {
	lines := ompCleanLines(p)
	rawLines := p.raw
	start := lastLineMatching(lines, ompAskHeaderPattern)
	if start < 0 {
		return nil
	}

	end := len(lines)
	for index := start + 1; index < len(lines); index++ {
		if ompFooterPattern.MatchString(lines[index]) {
			end = index
			break
		}
	}
	var rows []ompOptionRow
	for index := start + 1; index < end; index++ {
		match := ompOptionPattern.FindStringSubmatch(lines[index])
		if match == nil {
			continue
		}
		rows = append(rows, ompOptionRow{
			line:     index,
			focus:    match[1] != "",
			marker:   match[2],
			label:    textutil.Compact(match[3], 500),
			selected: ompMarkerSelected(match[2]),
		})
	}
	if len(rows) < 2 {
		return parseOMPReview(lines, start, end)
	}

	question := ompQuestion(lines, start, rows[0].line)
	current, total := ompPosition(lines, rawLines, start, question)
	if match := ompProgressPattern.FindStringSubmatch(question); match != nil {
		current, _ = strconv.Atoi(match[1])
		total, _ = strconv.Atoi(match[2])
		question = strings.TrimSpace(ompProgressPattern.ReplaceAllString(question, ""))
	}

	kind := "single_select"
	if ompCheckboxMarker(rows[0].marker) {
		kind = "multi_select"
	}
	options := make([]Option, 0, len(rows)-1)
	other := Other{Hidden: true}
	focus := Focus{Kind: "option"}
	for rowIndex, row := range rows {
		label := row.label
		if strings.Contains(strings.ToLower(label), "done selecting") {
			if row.focus {
				focus = Focus{Kind: "submit"}
			}
			continue
		}
		if otherPattern.MatchString(label) {
			rowEnd := end
			if rowIndex+1 < len(rows) {
				rowEnd = rows[rowIndex+1].line
			}
			other = Other{
				Selected: row.selected,
				Label:    label,
				Text:     ompDescription(lines, row.line, rowEnd),
			}
			if row.focus {
				focus = Focus{Kind: "option", Index: len(options)}
			}
			continue
		}
		rowEnd := end
		if rowIndex+1 < len(rows) {
			rowEnd = rows[rowIndex+1].line
		}
		optionIndex := len(options)
		options = append(options, Option{
			Index:       optionIndex,
			Label:       label,
			Description: ompDescription(lines, row.line, rowEnd),
			Selected:    row.selected,
		})
		if row.focus {
			focus = Focus{Kind: "option", Index: optionIndex}
		}
	}
	if len(options) == 0 || (other.Hidden && len(options) < 2) {
		return nil
	}
	allOptions := len(options)
	if !other.Hidden {
		allOptions++
	}
	if total == 0 {
		current, total = 1, 1
	}
	interaction := &Interaction{
		Kind:           kind,
		Question:       question,
		Options:        options,
		Other:          other,
		SubmitLabel:    nextOrSubmit(current, total),
		CanGoBack:      current > 1,
		QuestionIndex:  current,
		QuestionTotal:  total,
		Focus:          focus,
		AllOptionCount: allOptions,
		Agent:          "omp",
	}
	return finishInteraction(interaction)
}

func parseOMPReview(lines []string, start, end int) *Interaction {
	review, submit := -1, -1
	for index := start + 1; index < end; index++ {
		line := strings.TrimSpace(lines[index])
		if strings.EqualFold(line, "review answers") {
			review = index
			continue
		}
		if review >= 0 && ompReviewSubmitPattern.MatchString(line) {
			submit = index
			break
		}
	}
	if review < 0 || submit < 0 {
		return nil
	}

	var summary []SummaryEntry
	for _, line := range lines[review+1 : submit] {
		if match := menuPattern.FindStringSubmatch(line); match != nil {
			summary = append(summary, splitSummaryEntry(match[3]))
		}
	}
	questionTotal := 1
	if ids, _ := ompTabIDs(lines, start); len(ids) > 0 {
		questionTotal = len(ids) + 1
	}
	interaction := &Interaction{
		Kind:     "single_select",
		Question: "Review answers",
		Options: []Option{{
			Index:       0,
			Label:       "Submit answers",
			Description: summaryLines(summary, 500),
			Selected:    true,
			Summary:     summary,
		}},
		Other:          Other{Hidden: true},
		SubmitLabel:    "Submit",
		CanGoBack:      questionTotal > 1,
		QuestionIndex:  questionTotal,
		QuestionTotal:  questionTotal,
		Focus:          Focus{Kind: "option"},
		AllOptionCount: 1,
		Agent:          "omp",
	}
	return finishInteraction(interaction)
}

func ompQuestion(lines []string, start, firstOption int) string {
	for index := firstOption - 1; index > start; index-- {
		line := lines[index]
		if line == "" || ompBorderLine(line) {
			continue
		}
		return textutil.Compact(line, 1000)
	}
	return "OMP needs an answer"
}

func ompDescription(lines []string, start, end int) string {
	var parts []string
	for _, line := range lines[start+1 : end] {
		if line == "" || ompBorderLine(line) || ompFooterPattern.MatchString(line) {
			continue
		}
		parts = append(parts, strings.TrimSpace(strings.TrimPrefix(line, "↳")))
	}
	return textutil.Compact(strings.Join(parts, " "), 500)
}

// ompTabIDs collects the question ids from the Ask header tab row, which
// wraps onto continuation lines on narrow panes; the Submit tab ends it.
func ompTabIDs(lines []string, askStart int) ([]string, int) {
	var ids []string
	for index := askStart + 1; index < len(lines) && index <= askStart+4; index++ {
		for _, field := range strings.Fields(lines[index]) {
			if strings.EqualFold(field, "submit") {
				return ids, index
			}
			if !ompTabIDPattern.MatchString(field) {
				return nil, -1
			}
			ids = append(ids, field)
		}
	}
	return nil, -1
}

// ompActiveTab finds the tab rendered with the bold active-tab style in the
// raw terminal output; cleaned lines cannot carry that distinction.
func ompActiveTab(rawLines []string, first, last int) string {
	for index := first; index <= last && index < len(rawLines); index++ {
		if match := ompActiveTabPattern.FindStringSubmatch(rawLines[index]); match != nil {
			return match[1]
		}
	}
	return ""
}

func ompPosition(lines, rawLines []string, askStart int, question string) (int, int) {
	ids, tabEnd := ompTabIDs(lines, askStart)
	if len(ids) > 0 {
		if active := ompActiveTab(rawLines, askStart+1, tabEnd); active != "" {
			for index, id := range ids {
				if strings.EqualFold(id, active) {
					return index + 1, len(ids)
				}
			}
		}
	}
	frameEnd := -1
	for index := askStart - 1; index >= 0; index-- {
		if strings.HasPrefix(lines[index], "╰") {
			frameEnd = index
			break
		}
	}
	frameStart := -1
	if frameEnd >= 0 {
		for index := frameEnd - 1; index >= 0; index-- {
			if strings.HasPrefix(lines[index], "╭") {
				frameStart = index
				break
			}
		}
	}

	var metaIDs, prompts []string
	if frameStart >= 0 {
		for index := frameStart + 1; index < frameEnd; index++ {
			match := ompFrameMetaPattern.FindStringSubmatch(lines[index])
			if match == nil {
				continue
			}
			prompt := ""
			for candidate := index + 1; candidate < frameEnd; candidate++ {
				if ompFrameMetaPattern.MatchString(lines[candidate]) {
					break
				}
				if lines[candidate] == "" || ompBorderLine(lines[candidate]) {
					continue
				}
				prompt = textutil.Compact(lines[candidate], 1000)
				break
			}
			metaIDs = append(metaIDs, match[1])
			prompts = append(prompts, prompt)
		}
	}

	currentID := ""
	for index, prompt := range prompts {
		if prompt == question {
			currentID = metaIDs[index]
			break
		}
	}
	if currentID != "" && len(ids) > 0 {
		for index, id := range ids {
			if strings.EqualFold(id, currentID) {
				return index + 1, len(ids)
			}
		}
	}
	for index, prompt := range prompts {
		if prompt == question {
			return index + 1, len(prompts)
		}
	}
	if len(ids) > 0 {
		return 0, len(ids)
	}
	return 0, len(prompts)
}

func ompMarkerSelected(marker string) bool {
	switch strings.ToLower(marker) {
	case "☑", "◉", "", "", "[x]", "(o)":
		return true
	default:
		return false
	}
}

func ompCheckboxMarker(marker string) bool {
	switch strings.ToLower(marker) {
	case "☑", "☐", "", "", "[x]", "[ ]":
		return true
	default:
		return false
	}
}

func ompBorderLine(line string) bool {
	return strings.Trim(line, " \t─━═_—│|├┤╭╮╰╯┬┴┼") == ""
}
