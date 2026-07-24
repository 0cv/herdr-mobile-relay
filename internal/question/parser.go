package question

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"regexp"
	"strconv"
	"strings"
)

type Option struct {
	Index       int    `json:"index"`
	Label       string `json:"label"`
	Description string `json:"description"`
	Selected    bool   `json:"selected"`
}

type Other struct {
	Selected    bool   `json:"selected"`
	Text        string `json:"text"`
	Label       string `json:"label,omitempty"`
	Placeholder string `json:"placeholder,omitempty"`
	AllowEmpty  bool   `json:"allow_empty,omitempty"`
}

type Focus struct {
	Kind  string `json:"-"`
	Index int    `json:"-"`
}

type Interaction struct {
	ID            string   `json:"id"`
	Kind          string   `json:"kind"`
	Question      string   `json:"question"`
	Options       []Option `json:"options"`
	Other         Other    `json:"other"`
	SubmitLabel   string   `json:"submit_label"`
	CanChat       bool     `json:"can_chat"`
	CanGoBack     bool     `json:"can_go_back"`
	QuestionIndex int      `json:"question_index,omitempty"`
	QuestionTotal int      `json:"question_total,omitempty"`

	Focus          Focus  `json:"-"`
	AllOptionCount int    `json:"-"`
	Agent          string `json:"-"`
	NotesActive    bool   `json:"-"`
}

type codexRow struct {
	line   int
	focus  bool
	prefix int
	body   string
}

var (
	ansiPattern        = regexp.MustCompile(`\x1b\[[0-?]*[ -/]*[@-~]`)
	edgePattern        = regexp.MustCompile(`^[│|]\s*|\s*[│|]$`)
	checkboxPattern    = regexp.MustCompile(`^\s*([❯›]?)\s*(\d+)\.\s*\[([^\]]*)\]\s*(.*?)\s*$`)
	menuPattern        = regexp.MustCompile(`^\s*([❯›]?)\s*(\d+)\.\s+(.*?)\s*$`)
	submitPattern      = regexp.MustCompile(`(?i)^\s*([❯›]?)\s*(?:\d+\.\s*)?(submit|next)\s*$`)
	chatPattern        = regexp.MustCompile(`(?i)^\s*([❯›]?)\s*(?:\d+\.\s*)?chat about this\s*$`)
	codexHeaderPattern = regexp.MustCompile(`(?i)^\s*question\s+(\d+)\s*/\s*(\d+)`)
	codexSubmitPattern = regexp.MustCompile(`(?i)\benter\s+to\s+submit\s+(answer|answers|all)\b`)
	otherPattern       = regexp.MustCompile(`(?i)^(?:type something\.?|none of the above|other)\b`)
	selectedPattern    = regexp.MustCompile(`\s*[✓✔]\s*$`)
	chromePattern      = regexp.MustCompile(`(?i)^(?:[\s─━═_—│|◔◑◕●]+|.*\besc to cancel\b|.*\btype to queue\b|[◔◑◕●]\s+(?:shell|bash).*)$`)
	promptSkipPattern  = regexp.MustCompile(`(?i)^(?:bash command|do you want to proceed\??|would you like to run\b.*|environment:\s*\w+|press enter to confirm\b.*|esc to cancel\b.*)$`)
	commandPattern     = regexp.MustCompile(`^\s*[$>❯›]\s+(.+?)\s*$`)
)

var (
	toolOptions     = []string{"yes, single permission", "trust, always allow", "no (tab to edit)"}
	subagentOptions = []string{"approve all pending", "configure individually", "exit (cancel subagents)"}
)

func Supports(agent string) bool {
	agent = strings.ToLower(agent)
	return strings.Contains(agent, "claude") || strings.Contains(agent, "codex")
}

func Parse(text, agent string) *Interaction {
	if !LayoutHint(text) {
		return nil
	}
	normalized := strings.ToLower(agent)
	if strings.Contains(normalized, "codex") {
		return parseCodex(text)
	}
	if strings.Contains(normalized, "claude") {
		return parseClaude(text)
	}
	if interaction := parseCodex(text); interaction != nil {
		return interaction
	}
	return parseClaude(text)
}

func LayoutHint(text string) bool {
	lines := cleanLines(text)
	hasCheckbox, hasSubmit, hasChat := false, false, false
	hasCodexHeader, hasCodexFooter := false, false
	singleCodexQuestion := false
	lastControl := -1
	for index, line := range lines {
		switch {
		case checkboxPattern.MatchString(line):
			hasCheckbox = true
		case submitPattern.MatchString(line):
			hasSubmit = true
			lastControl = index
		case chatPattern.MatchString(line):
			hasChat = true
			lastControl = index
		}
		if match := codexHeaderPattern.FindStringSubmatch(line); match != nil {
			hasCodexHeader = true
			current, _ := strconv.Atoi(match[1])
			total, _ := strconv.Atoi(match[2])
			singleCodexQuestion = current == 1 && total == 1
		}
		if codexFooter(line) || (singleCodexQuestion && codexSingleFooter(line)) {
			hasCodexFooter = true
			lastControl = index
		}
		if strings.Contains(strings.ToLower(line), "enter to select") &&
			strings.Contains(line, "↑/↓") {
			lastControl = index
		}
		if strings.Contains(strings.ToLower(line), "review your answers") ||
			strings.Contains(strings.ToLower(line), "submit answers") {
			lastControl = index
		}
	}
	hasLayout := (hasCheckbox && (hasSubmit || hasChat)) || hasChat ||
		(hasCodexHeader && hasCodexFooter) ||
		strings.Contains(strings.ToLower(strings.Join(lines, "\n")), "review your answers")
	if !hasLayout || lastControl < 0 {
		return false
	}
	for _, line := range lines[lastControl+1:] {
		if line != "" && strings.Trim(line, "─━═_—│| ") != "" {
			return false
		}
	}
	return true
}

// ApprovalDetails extracts the display summary, command context, and final
// sequential menu from a non-question approval pane.
func ApprovalDetails(text string) (string, string, []string) {
	summaryLines := paneSummaryLines(text)
	summary := strings.Join(summaryLines, "\n")
	options := approvalOptions(cleanLines(text))
	command := approvalCommand(summaryLines)
	return compact(summary, 500), compact(command, 240), options
}

func PaneSummary(text string) string {
	return strings.Join(paneSummaryLines(text), "\n")
}

func paneSummaryLines(text string) []string {
	lines := cleanLines(text)
	var summaryLines []string
	for _, line := range lines {
		if line == "" || chromePattern.MatchString(line) || promptSkipPattern.MatchString(line) {
			continue
		}
		summaryLines = append(summaryLines, line)
	}
	if len(summaryLines) > 12 {
		summaryLines = summaryLines[len(summaryLines)-12:]
	}
	return summaryLines
}

func approvalOptions(lines []string) []string {
	var runs [][]string
	var current []string
	expected := 1
	for _, line := range lines {
		match := menuPattern.FindStringSubmatch(line)
		if match == nil {
			if len(current) > 0 {
				runs = append(runs, current)
				current = nil
				expected = 1
			}
			continue
		}
		number, _ := strconv.Atoi(match[2])
		label := strings.TrimSpace(match[3])
		switch {
		case number == 1:
			if len(current) > 0 {
				runs = append(runs, current)
			}
			current = []string{label}
			expected = 2
		case len(current) > 0 && number == expected:
			current = append(current, label)
			expected++
		default:
			if len(current) > 0 {
				runs = append(runs, current)
			}
			current = nil
			expected = 1
		}
	}
	if len(current) > 0 {
		runs = append(runs, current)
	}
	for index := len(runs) - 1; index >= 0; index-- {
		if len(runs[index]) >= 2 {
			return append([]string(nil), runs[index]...)
		}
	}
	lower := strings.ToLower(strings.Join(lines, "\n"))
	if strings.Contains(lower, "yes, single permission") {
		return append([]string(nil), toolOptions...)
	}
	if strings.Contains(lower, "approve all pending") {
		return append([]string(nil), subagentOptions...)
	}
	return append([]string(nil), toolOptions...)
}

func approvalCommand(lines []string) string {
	command, fallback := "", ""
	for _, line := range lines {
		if line == "" || menuPattern.MatchString(line) || chromePattern.MatchString(line) ||
			promptSkipPattern.MatchString(line) {
			continue
		}
		if match := commandPattern.FindStringSubmatch(line); match != nil {
			command = strings.TrimSpace(match[1])
			continue
		}
		fallback = line
	}
	if command != "" {
		return command
	}
	return fallback
}

func parseClaude(text string) *Interaction {
	lines := cleanLines(text)
	type row struct {
		line     int
		number   int
		focus    bool
		mark     string
		label    string
		selected bool
	}
	var checkboxRows []row
	for index, line := range lines {
		match := checkboxPattern.FindStringSubmatch(line)
		if match == nil {
			continue
		}
		number, _ := strconv.Atoi(match[2])
		checkboxRows = append(checkboxRows, row{
			line:     index,
			number:   number,
			focus:    match[1] != "",
			mark:     strings.TrimSpace(match[3]),
			label:    compact(match[4], 500),
			selected: strings.TrimSpace(match[3]) != "",
		})
	}
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
		all := make([]Option, 0, len(checkboxRows))
		focus := Focus{Kind: "option"}
		for index, item := range checkboxRows {
			rowEnd := end
			if index+1 < len(checkboxRows) {
				rowEnd = checkboxRows[index+1].line
			}
			all = append(all, Option{
				Index:       index,
				Label:       strings.TrimSpace(selectedPattern.ReplaceAllString(item.label, "")),
				Description: description(lines, item.line, rowEnd),
				Selected:    item.selected || selectedPattern.MatchString(item.label),
			})
			if item.focus {
				focus = Focus{Kind: "option", Index: index}
			}
		}
		if submitFocus {
			focus = Focus{Kind: "submit"}
		}
		if chatFocus {
			focus = Focus{Kind: "chat"}
		}
		otherItem := all[len(all)-1]
		options := all[:len(all)-1]
		otherText := ""
		if !otherPattern.MatchString(otherItem.Label) {
			otherText = otherItem.Label
		}
		question := prompt(lines, checkboxRows[0].line)
		current, total := claudePosition(text)
		if submitLabel == "Submit" && current > 0 && current < total {
			submitLabel = "Next"
		}
		interaction := &Interaction{
			Kind:           "multi_select",
			Question:       question,
			Options:        options,
			Other:          Other{Selected: otherItem.Selected, Text: otherText},
			SubmitLabel:    defaultString(submitLabel, "Submit"),
			CanChat:        chatIndex >= 0,
			CanGoBack:      current > 1,
			QuestionIndex:  current,
			QuestionTotal:  total,
			Focus:          focus,
			AllOptionCount: len(all),
			Agent:          "claude",
		}
		interaction.ID = interactionID(interaction)
		return interaction
	}

	if chatIndex < 0 {
		return nil
	}
	var rows []row
	expected := 1
	for index, line := range lines[:chatIndex] {
		match := menuPattern.FindStringSubmatch(line)
		if match == nil {
			continue
		}
		number, _ := strconv.Atoi(match[2])
		if number == 1 {
			rows = nil
			expected = 1
		}
		if number != expected {
			continue
		}
		label := compact(match[3], 500)
		rows = append(rows, row{
			line:     index,
			number:   number,
			focus:    match[1] != "",
			label:    strings.TrimSpace(selectedPattern.ReplaceAllString(label, "")),
			selected: selectedPattern.MatchString(label),
		})
		expected++
	}
	if len(rows) < 3 {
		return nil
	}
	all := make([]Option, 0, len(rows))
	focus := Focus{Kind: "option"}
	for index, item := range rows {
		rowEnd := chatIndex
		if index+1 < len(rows) {
			rowEnd = rows[index+1].line
		}
		all = append(all, Option{
			Index:       index,
			Label:       item.label,
			Description: description(lines, item.line, rowEnd),
			Selected:    item.selected,
		})
		if item.focus {
			focus = Focus{Kind: "option", Index: index}
		}
	}
	if chatFocus {
		focus = Focus{Kind: "chat"}
	}
	otherItem := all[len(all)-1]
	options := all[:len(all)-1]
	otherText := ""
	if !otherPattern.MatchString(otherItem.Label) {
		otherText = otherItem.Label
	}
	current, total := claudePosition(text)
	submitLabel = "Submit"
	if current > 0 && current < total {
		submitLabel = "Next"
	}
	interaction := &Interaction{
		Kind:           "single_select",
		Question:       prompt(lines, rows[0].line),
		Options:        options,
		Other:          Other{Selected: otherItem.Selected, Text: otherText},
		SubmitLabel:    submitLabel,
		CanChat:        true,
		CanGoBack:      current > 1,
		QuestionIndex:  current,
		QuestionTotal:  total,
		Focus:          focus,
		AllOptionCount: len(all),
		Agent:          "claude",
	}
	interaction.ID = interactionID(interaction)
	return interaction
}

func parseCodex(text string) *Interaction {
	rawLines := strings.Split(strings.ReplaceAll(text, "\r\n", "\n"), "\n")
	lines := make([]string, len(rawLines))
	headerIndex, current, total := -1, 0, 0
	for index, raw := range rawLines {
		lines[index] = cleanCodexLine(raw)
		if match := codexHeaderPattern.FindStringSubmatch(lines[index]); match != nil {
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
		if codexFooter(lines[index]) ||
			(current == 1 && total == 1 && codexSingleFooter(lines[index])) {
			footerIndex = index
			break
		}
	}
	if footerIndex < 0 {
		return nil
	}
	var rows []codexRow
	expected := 1
	for index := headerIndex + 1; index < footerIndex; index++ {
		match := menuPattern.FindStringSubmatch(lines[index])
		if match == nil {
			continue
		}
		number, _ := strconv.Atoi(match[2])
		if number != expected {
			continue
		}
		prefix := strings.Index(lines[index], match[3])
		rows = append(rows, codexRow{line: index, focus: match[1] != "", prefix: prefix, body: match[3]})
		expected++
	}
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
	questionText := compact(strings.Join(questionParts, " "), 1000)
	if questionText == "" {
		return nil
	}
	descriptionColumn := codexDescriptionColumn(lines, rows)
	all := make([]Option, 0, len(rows))
	focus := Focus{Kind: "option"}
	for index, item := range rows {
		end := footerIndex
		if index+1 < len(rows) {
			end = rows[index+1].line
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
	otherItem := all[len(all)-1]
	options := all[:len(all)-1]
	notes := ""
	notesActive := false
	for _, line := range lines[rows[len(rows)-1].line+1 : footerIndex] {
		trimmed := strings.TrimSpace(strings.TrimPrefix(line, "›"))
		if trimmed != "" && !strings.Contains(strings.ToLower(trimmed), "tab ") {
			notes = compact(trimmed, 20000)
			notesActive = true
		}
	}
	if notesActive {
		focus = Focus{Kind: "option", Index: len(all) - 1}
	}
	if strings.Contains(text, "\x1b[") &&
		!strings.Contains(strings.Join(rawLines[headerIndex+1:firstOption], "\n"), "\x1b[38;5;6m") &&
		focus.Kind == "option" {
		if focus.Index < len(options) {
			options[focus.Index].Selected = true
		} else {
			otherItem.Selected = true
		}
	}
	submitLabel := "Submit"
	if current < total {
		submitLabel = "Next"
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
		SubmitLabel:    submitLabel,
		CanChat:        false,
		CanGoBack:      current > 1,
		QuestionIndex:  current,
		QuestionTotal:  total,
		Focus:          focus,
		AllOptionCount: len(all),
		Agent:          "codex",
		NotesActive:    notesActive,
	}
	interaction.ID = interactionID(interaction)
	return interaction
}

func cleanLines(text string) []string {
	raw := strings.Split(strings.ReplaceAll(text, "\r\n", "\n"), "\n")
	result := make([]string, len(raw))
	for index, line := range raw {
		result[index] = cleanLine(line)
	}
	return result
}

func cleanLine(line string) string {
	line = strings.TrimSpace(ansiPattern.ReplaceAllString(strings.ReplaceAll(line, "\r", ""), ""))
	for {
		next := strings.TrimSpace(edgePattern.ReplaceAllString(line, ""))
		if next == line {
			return line
		}
		line = next
	}
}

func cleanCodexLine(line string) string {
	line = strings.TrimRight(
		ansiPattern.ReplaceAllString(strings.ReplaceAll(line, "\r", ""), ""),
		" \t",
	)
	for {
		next := strings.TrimRight(edgePattern.ReplaceAllString(line, ""), " \t")
		if next == line {
			return line
		}
		line = next
	}
}

func description(lines []string, start, end int) string {
	var parts []string
	for _, line := range lines[start+1 : end] {
		if line == "" || checkboxPattern.MatchString(line) || menuPattern.MatchString(line) ||
			submitPattern.MatchString(line) || chatPattern.MatchString(line) ||
			strings.Contains(strings.ToLower(line), "enter to select") {
			continue
		}
		parts = append(parts, line)
	}
	return compact(strings.Join(parts, " "), 500)
}

func prompt(lines []string, firstOption int) string {
	for index := firstOption - 1; index >= 0; index-- {
		line := lines[index]
		lower := strings.ToLower(line)
		if line == "" || submitPattern.MatchString(line) || chatPattern.MatchString(line) ||
			strings.Contains(lower, "enter to select") ||
			(strings.Contains(line, "Submit") && strings.Contains(line, "→")) {
			continue
		}
		return compact(line, 1000)
	}
	return "Claude Code needs an answer"
}

func claudePosition(text string) (int, int) {
	for _, raw := range strings.Split(text, "\n") {
		clean := cleanLine(raw)
		if !strings.Contains(clean, "→") || !strings.Contains(strings.ToLower(clean), "submit") {
			continue
		}
		active := regexp.MustCompile(`\x1b\[[^m]*48[^m]*m`).FindStringIndex(raw)
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

func codexFooter(line string) bool {
	lower := strings.ToLower(line)
	return codexSubmitPattern.MatchString(line) &&
		strings.Contains(lower, "navigate questions")
}

func codexSingleFooter(line string) bool {
	lower := strings.ToLower(line)
	return codexSubmitPattern.MatchString(line) &&
		strings.Contains(lower, "tab to add notes")
}

func codexDescriptionColumn(lines []string, rows []codexRow) int {
	counts := make(map[int]int)
	for _, item := range rows {
		bodyStart := strings.Index(lines[item.line], item.body)
		if bodyStart < 0 {
			continue
		}
		if gap := regexp.MustCompile(`\s{2,}\S`).FindStringIndex(item.body); gap != nil {
			counts[bodyStart+gap[1]-1]++
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

func codexParts(lines []string, item codexRow, end, descriptionColumn int) (string, string) {
	var labels, descriptions []string
	for index := item.line; index < end; index++ {
		line := lines[index]
		if line == "" {
			continue
		}
		if index == item.line {
			left, right := item.body, ""
			if gap := regexp.MustCompile(`\s{2,}`).FindStringIndex(item.body); gap != nil {
				left, right = item.body[:gap[0]], item.body[gap[1]:]
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
			if len(line) < descriptionColumn {
				left = line
			} else {
				left, right = line[:descriptionColumn], line[descriptionColumn:]
			}
		}
		if value := strings.TrimSpace(left); value != "" {
			labels = append(labels, value)
		}
		if value := strings.TrimSpace(right); value != "" {
			descriptions = append(descriptions, value)
		}
	}
	return compact(strings.Join(labels, " "), 500), compact(strings.Join(descriptions, " "), 500)
}

func interactionID(interaction *Interaction) string {
	value := struct {
		Kind        string   `json:"kind"`
		Question    string   `json:"question"`
		Options     []string `json:"options"`
		SubmitLabel string   `json:"submit_label"`
		Position    []int    `json:"position,omitempty"`
	}{
		Kind:        interaction.Kind,
		Question:    interaction.Question,
		SubmitLabel: interaction.SubmitLabel,
	}
	for _, option := range interaction.Options {
		value.Options = append(value.Options, option.Label)
	}
	if interaction.QuestionIndex > 0 && interaction.QuestionTotal > 0 {
		value.Position = []int{interaction.QuestionIndex, interaction.QuestionTotal}
	}
	data, _ := json.Marshal(value)
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])[:20]
}

func compact(value string, limit int) string {
	value = strings.Join(strings.Fields(value), " ")
	runes := []rune(value)
	if len(runes) > limit {
		return string(runes[:limit])
	}
	return value
}

func title(value string) string {
	value = strings.ToLower(value)
	if value == "" {
		return ""
	}
	return strings.ToUpper(value[:1]) + value[1:]
}

func defaultString(value, fallback string) string {
	if value == "" {
		return fallback
	}
	return value
}
