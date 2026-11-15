package question

import (
	"regexp"
	"strconv"
	"strings"

	"github.com/0cv/herdr-mobile-relay/internal/textutil"
)

var (
	codexHeaderPattern = regexp.MustCompile(`(?i)^\s*question\s+(\d+)\s*/\s*(\d+)`)
	codexSubmitPattern = regexp.MustCompile(`(?i)\benter\s+to\s+submit\s+(answer|answers|all)\b`)
)

func parseCodex(p *pane) *Interaction {
	rawLines := p.raw
	lines := p.indented()
	headerIndex, current, total := -1, 0, 0
	for index, line := range lines {
		if match := codexHeaderPattern.FindStringSubmatch(line); match != nil {
			headerIndex = index
			current, _ = strconv.Atoi(match[1])
			total, _ = strconv.Atoi(match[2])
		}
	}
	if headerIndex < 0 {
		return nil
	}
	footerIndex := -1
	for index := headerIndex + 1; index < len(lines); index++ {
		if codexFooter(lines[index]) {
			footerIndex = index
			break
		}
	}
	if footerIndex < 0 {
		return nil
	}
	rows := lastRun(scanRows(lines, headerIndex+1, footerIndex, rowScan{}))
	if len(rows) < 3 {
		return nil
	}
	firstOption := rows[0].line
	var questionParts []string
	for _, line := range lines[headerIndex+1 : firstOption] {
		if line != "" {
			questionParts = append(questionParts, line)
		}
	}
	questionText := textutil.Compact(strings.Join(questionParts, " "), 1000)
	if questionText == "" {
		return nil
	}
	notesStart := footerIndex
	notesActive := strings.Contains(
		strings.ToLower(lines[footerIndex]),
		"clear notes",
	)
	if notesActive {
		for index := rows[len(rows)-1].line + 1; index < footerIndex; index++ {
			if strings.TrimSpace(lines[index]) != "" {
				notesStart = index
				break
			}
		}
	}
	descriptionColumn := codexDescriptionColumn(lines, rows)
	all := make([]Option, 0, len(rows))
	focus := Focus{Kind: "option"}
	for index, item := range rows {
		end := footerIndex
		if index+1 < len(rows) {
			end = rows[index+1].line
		} else if notesStart < footerIndex {
			end = notesStart
		}
		label, desc := codexParts(lines, item, end, descriptionColumn)
		if label == "" {
			return nil
		}
		all = append(all, Option{Index: index, Label: label, Description: desc})
		if item.focus {
			focus = Focus{Kind: "option", Index: index}
		}
	}
	if len(all) == 0 || !otherPattern.MatchString(all[len(all)-1].Label) {
		return nil
	}
	options, otherItem := splitOther(all)
	notes := ""
	for _, line := range lines[notesStart:footerIndex] {
		trimmed := strings.TrimSpace(line)
		trimmed = strings.TrimSpace(strings.TrimPrefix(trimmed, "›"))
		if trimmed != "" {
			notes = textutil.Compact(trimmed, 20000)
		}
	}
	if notesActive {
		focus = Focus{Kind: "option", Index: len(all) - 1}
	}
	if strings.Contains(p.text, "\x1b[") &&
		!strings.Contains(strings.Join(rawLines[headerIndex+1:firstOption], "\n"), "\x1b[38;5;6m") &&
		focus.Kind == "option" {
		if focus.Index < len(options) {
			options[focus.Index].Selected = true
		} else {
			otherItem.Selected = true
		}
	}
	interaction := &Interaction{
		Kind:     "single_select",
		Question: questionText,
		Options:  options,
		Other: Other{
			Selected:    otherItem.Selected || notesActive,
			Text:        notes,
			Label:       otherItem.Label,
			Placeholder: "Optional notes",
			AllowEmpty:  true,
		},
		SubmitLabel:    nextOrSubmit(current, total),
		CanChat:        false,
		CanGoBack:      current > 1,
		QuestionIndex:  current,
		QuestionTotal:  total,
		Focus:          focus,
		AllOptionCount: len(all),
		Agent:          "codex",
		NotesActive:    notesActive,
	}
	return finishInteraction(interaction)
}

func codexFooter(line string) bool {
	lower := strings.ToLower(line)
	return codexSubmitPattern.MatchString(line) &&
		(strings.Contains(lower, "navigate questions") ||
			strings.Contains(lower, "tab to add notes") ||
			strings.Contains(lower, "tab or esc to clear notes"))
}

func codexDescriptionColumn(lines []string, rows []optionRow) int {
	counts := make(map[int]int)
	for _, item := range rows {
		bodyStart := strings.Index(lines[item.line], item.text)
		if bodyStart < 0 {
			continue
		}
		if gap := columnGapPattern.FindStringIndex(item.text); gap != nil {
			prefixColumn := len([]rune(lines[item.line][:bodyStart]))
			descriptionColumn := prefixColumn + len([]rune(item.text[:gap[1]]))
			counts[descriptionColumn]++
		}
	}
	best, bestCount := -1, 0
	for column, count := range counts {
		if count > bestCount || count == bestCount && (best < 0 || column < best) {
			best, bestCount = column, count
		}
	}
	return best
}

func codexParts(lines []string, item optionRow, end, descriptionColumn int) (string, string) {
	var labels, descriptions []string
	for index := item.line; index < end; index++ {
		line := lines[index]
		if line == "" {
			continue
		}
		if index == item.line {
			left, right := item.text, ""
			if gap := columnGapPattern.FindStringIndex(item.text); gap != nil {
				left, right = item.text[:gap[0]], item.text[gap[1]:]
			}
			if value := strings.TrimSpace(left); value != "" {
				labels = append(labels, value)
			}
			if value := strings.TrimSpace(right); value != "" {
				descriptions = append(descriptions, value)
			}
			continue
		}
		left, right := line, ""
		if descriptionColumn >= 0 {
			runes := []rune(line)
			if len(runes) < descriptionColumn {
				left = line
			} else {
				left, right = string(runes[:descriptionColumn]), string(runes[descriptionColumn:])
			}
		}
		if value := strings.TrimSpace(left); value != "" {
			labels = append(labels, value)
		}
		if value := strings.TrimSpace(right); value != "" {
			descriptions = append(descriptions, value)
		}
	}
	return textutil.Compact(strings.Join(labels, " "), 500), textutil.Compact(strings.Join(descriptions, " "), 500)
}
