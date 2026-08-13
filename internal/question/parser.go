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
	Index       int            `json:"index"`
	Label       string         `json:"label"`
	Description string         `json:"description"`
	Selected    bool           `json:"selected"`
	Summary     []SummaryEntry `json:"summary,omitempty"`
}

// SummaryEntry is one answered question in a final review option, kept
// structured so the phone can style questions and answers differently.
type SummaryEntry struct {
	Question string `json:"q"`
	Answer   string `json:"a"`
}

type Other struct {
	Selected    bool   `json:"selected"`
	Text        string `json:"text"`
	Label       string `json:"label,omitempty"`
	Placeholder string `json:"placeholder,omitempty"`
	AllowEmpty  bool   `json:"allow_empty,omitempty"`
	Hidden      bool   `json:"hidden,omitempty"`
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
	ansiPattern           = regexp.MustCompile(`\x1b\[[0-?]*[ -/]*[@-~]`)
	edgePattern           = regexp.MustCompile(`^[│|]\s*|\s*[│|]$`)
	checkboxPattern       = regexp.MustCompile(`^\s*([❯›]?)\s*(\d+)\.\s*\[([^\]]*)\]\s*(.*?)\s*$`)
	menuPattern           = regexp.MustCompile(`^\s*([❯›]?)\s*(\d+)\.\s+(.*?)\s*$`)
	submitPattern         = regexp.MustCompile(`(?i)^\s*([❯›]?)\s*(?:\d+\.\s*)?(submit|next)\s*$`)
	chatPattern           = regexp.MustCompile(`(?i)^\s*([❯›]?)\s*(?:\d+\.\s*)?chat about this\s*$`)
	codexHeaderPattern    = regexp.MustCompile(`(?i)^\s*question\s+(\d+)\s*/\s*(\d+)`)
	codexSubmitPattern    = regexp.MustCompile(`(?i)\benter\s+to\s+submit\s+(answer|answers|all)\b`)
	qoderActivePattern    = regexp.MustCompile(`\x1b\[[^m]*48(?:;|:)[^m]*m\s*([^\x1b]+)`)
	qoderReviewPattern    = regexp.MustCompile(`(?i)^\s*([❯›]?)\s*(submit answers|cancel ask)\s*$`)
	claudeReviewPattern   = regexp.MustCompile(`(?i)^\s*([❯›]?)\s*(\d+)\.\s*(submit answers|cancel)\s*$`)
	openCodeFocusPattern  = regexp.MustCompile(`\x1b\[[^m]*48(?:;|:)2(?:;|:)30(?:;|:)30(?:;|:)30m`)
	openCodeActivePattern = regexp.MustCompile(
		`\x1b\[[^m]*48(?:;|:)2(?:;|:)157(?:;|:)124(?:;|:)216m([^\x1b]*)`,
	)
	openCodeColumnPattern  = regexp.MustCompile(`\s{20,}`)
	ompAskHeaderPattern    = regexp.MustCompile(`(?i)^╭[─━═_—\s]*Ask(?:[─━═_—\s]|$)`)
	ompOptionPattern       = regexp.MustCompile(`^\s*([❯›>]?)\s*(☑|☐|◉|○|||||\[[xX ]\]|\([oO ]\))\s+(.+?)\s*$`)
	ompFrameMetaPattern    = regexp.MustCompile(`\[\s*([\p{L}\p{N}_-]+)\s*\]\s*[·•]\s*options:\s*\d+`)
	ompProgressPattern     = regexp.MustCompile(`\s+\((\d+)\s*/\s*(\d+)\)\s*$`)
	ompReviewSubmitPattern = regexp.MustCompile(`(?i)^\s*[❯›>]?\s*submit\s*$`)
	ompTabIDPattern        = regexp.MustCompile(`^[\p{L}\p{N}_.-]+$`)
	ompActiveTabPattern    = regexp.MustCompile(`\x1b\[1m(?:\x1b\[[0-9;:]*m)*\s*([\p{L}\p{N}_.-]+)`)
	otherPattern           = regexp.MustCompile(`(?i)^(?:type something\.?|type your own answer|none of the above|other)\b`)
	selectedPattern        = regexp.MustCompile(`\s*[✓✔]\s*$`)
	columnGapPattern       = regexp.MustCompile(`\s{2,}`)
	chromePattern          = regexp.MustCompile(`(?i)^(?:[\s─━═_—│|◔◑◕●]+|.*\besc to cancel\b|.*\btype to queue\b|[◔◑◕●]\s+(?:shell|bash).*)$`)
	promptSkipPattern      = regexp.MustCompile(`(?i)^(?:bash command|do you want to proceed\??|would you like to run\b.*|environment:\s*\w+|press enter to confirm\b.*|esc to cancel\b.*)$`)
	commandPattern         = regexp.MustCompile(`^\s*[$>❯›]\s+(.+?)\s*$`)
	turnDurationPattern    = regexp.MustCompile(
		`(?i)^[^\p{L}\p{N}]*\p{L}+(?:ed|ing)\s+for\s+(?:\d+h\s*)?(?:\d+m\s*)?\d+s\b`,
	)
	responseStartPattern  = regexp.MustCompile(`^\s*[•●]\s+\S`)
	responsePrefixPattern = regexp.MustCompile(`^\s*[•●]\s+`)
)

func Supports(agent string) bool {
	agent = strings.ToLower(agent)
	return strings.Contains(agent, "claude") ||
		strings.Contains(agent, "codex") ||
		ompAskAgent(agent) ||
		strings.Contains(agent, "opencode") ||
		strings.Contains(agent, "qoder")
}

func ompAskAgent(agent string) bool {
	agent = strings.ToLower(strings.TrimSpace(agent))
	return agent == "omp" || strings.HasPrefix(agent, "omp-") ||
		agent == "pi" || strings.HasPrefix(agent, "pi-") ||
		strings.Contains(agent, "oh-my-pi")
}

func Parse(text, agent string) *Interaction {
	if !LayoutHint(text) {
		return nil
	}
	normalized := strings.ToLower(agent)
	if ompAskAgent(normalized) {
		return parseOMP(text)
	}
	if strings.Contains(normalized, "codex") {
		return parseCodex(text)
	}
	if strings.Contains(normalized, "claude") {
		return parseClaude(text)
	}
	if strings.Contains(normalized, "qoder") {
		return parseQoder(text)
	}
	if strings.Contains(normalized, "opencode") {
		return parseOpenCode(text)
	}
	if interaction := parseCodex(text); interaction != nil {
		return interaction
	}
	if interaction := parseClaude(text); interaction != nil {
		return interaction
	}
	return parseQoder(text)
}

func LayoutHint(text string) bool {
	if openCodeLayoutHint(text) || ompLayoutHint(text) {
		return true
	}
	lines := cleanLines(text)
	hasCheckbox, hasSubmit, hasChat := false, false, false
	hasCodexHeader, hasCodexFooter, hasQoderHeader, hasQoderFooter := false, false, false, false
	hasReview := false
	lastControl := -1
	for index, line := range lines {
		lower := strings.ToLower(line)
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
		}
		if codexFooter(line) {
			hasCodexFooter = true
			lastControl = index
		}
		if qoderHeader(line) {
			hasQoderHeader = true
		}
		if qoderFooter(line) {
			hasQoderFooter = true
			lastControl = index
		}
		if strings.Contains(lower, "enter to select") &&
			(strings.Contains(line, "↑/↓") || strings.Contains(lower, "arrow keys")) {
			lastControl = index
		}
		if strings.Contains(lower, "review your answers") {
			hasReview = true
			lastControl = index
		}
		if hasReview && (qoderReviewPattern.MatchString(line) ||
			claudeReviewPattern.MatchString(line)) {
			lastControl = index
		}
	}
	hasLayout := (hasCheckbox && (hasSubmit || hasChat)) || hasChat ||
		(hasCodexHeader && hasCodexFooter) ||
		(hasQoderHeader && hasQoderFooter) ||
		hasReview
	if !hasLayout || lastControl < 0 {
		return false
	}
	for _, line := range lines[lastControl+1:] {
		if hasCodexHeader && strings.EqualFold(strings.TrimSpace(line), "esc to interrupt") {
			continue
		}
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

// LatestCompletedResponse returns the complete latest Codex or Claude response
// bounded by the agent's response marker and completed-turn duration line.
// Unlike PaneSummary, it intentionally does not impose a display-line limit;
// the activity journal applies its own persisted extract safety limit.
func LatestCompletedResponse(text string) string {
	rawLines := strings.Split(strings.ReplaceAll(text, "\r", ""), "\n")
	lines := make([]string, len(rawLines))
	for index, line := range rawLines {
		lines[index] = strings.TrimRight(ansiPattern.ReplaceAllString(line, ""), " \t")
	}

	end := -1
	for index := len(lines) - 1; index >= 0; index-- {
		if turnDurationPattern.MatchString(strings.TrimSpace(lines[index])) {
			end = index
			break
		}
	}
	if end < 0 {
		return ""
	}

	start := -1
	for index := end - 1; index >= 0; index-- {
		if responseStartPattern.MatchString(lines[index]) {
			start = index
			break
		}
		if turnDurationPattern.MatchString(strings.TrimSpace(lines[index])) {
			break
		}
	}
	if start < 0 {
		return ""
	}

	response := append([]string(nil), lines[start:end]...)
	response[0] = responsePrefixPattern.ReplaceAllString(response[0], "")
	for index := 1; index < len(response); index++ {
		response[index] = strings.TrimPrefix(response[index], "  ")
	}
	for len(response) > 0 && strings.TrimSpace(response[len(response)-1]) == "" {
		response = response[:len(response)-1]
	}
	return strings.TrimSpace(strings.Join(response, "\n"))
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
	return nil
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

type ompOptionRow struct {
	line     int
	focus    bool
	marker   string
	label    string
	selected bool
}

func ompLayoutHint(text string) bool {
	lines := cleanLines(text)
	start := -1
	for index, line := range lines {
		if ompAskHeaderPattern.MatchString(line) {
			start = index
		}
	}
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
		lower := strings.ToLower(line)
		if strings.Contains(lower, "enter select") || strings.Contains(lower, "enter submit") {
			footer = index
		}
	}
	if footer < 0 || (options < 2 && !reviewSubmit) {
		return false
	}
	for _, line := range lines[footer+1:] {
		if line != "" && !ompBorderLine(line) {
			return false
		}
	}
	return true
}

// ompCleanLines additionally strips the Ask frame's inner scrollbar column,
// which survives edge cleanup when the frame content overflows.
func ompCleanLines(text string) []string {
	lines := cleanLines(text)
	for index, line := range lines {
		lines[index] = strings.TrimRight(line, " \t│█▉▊▋▌▍▎▏▁▂▃▄▅▆▇▀")
	}
	return lines
}

func parseOMP(text string) *Interaction {
	lines := ompCleanLines(text)
	rawLines := strings.Split(strings.ReplaceAll(text, "\r\n", "\n"), "\n")
	start := -1
	for index, line := range lines {
		if ompAskHeaderPattern.MatchString(line) {
			start = index
		}
	}
	if start < 0 {
		return nil
	}

	end := len(lines)
	for index := start + 1; index < len(lines); index++ {
		lower := strings.ToLower(lines[index])
		if strings.Contains(lower, "enter select") || strings.Contains(lower, "enter submit") {
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
			label:    compact(match[3], 500),
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
		if strings.EqualFold(label, "done selecting") ||
			strings.Contains(strings.ToLower(label), "done selecting") {
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
	submitLabel := "Submit"
	if current < total {
		submitLabel = "Next"
	}
	interaction := &Interaction{
		Kind:           kind,
		Question:       question,
		Options:        options,
		Other:          other,
		SubmitLabel:    submitLabel,
		CanGoBack:      current > 1,
		QuestionIndex:  current,
		QuestionTotal:  total,
		Focus:          focus,
		AllOptionCount: allOptions,
		Agent:          "omp",
	}
	interaction.ID = interactionID(interaction)
	return interaction
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
	interaction.ID = interactionID(interaction)
	return interaction
}

func ompQuestion(lines []string, start, firstOption int) string {
	for index := firstOption - 1; index > start; index-- {
		line := lines[index]
		if line == "" || ompBorderLine(line) {
			continue
		}
		return compact(line, 1000)
	}
	return "OMP needs an answer"
}

func ompDescription(lines []string, start, end int) string {
	var parts []string
	for _, line := range lines[start+1 : end] {
		if line == "" || ompBorderLine(line) ||
			strings.Contains(strings.ToLower(line), "enter select") {
			continue
		}
		parts = append(parts, strings.TrimSpace(strings.TrimPrefix(line, "↳")))
	}
	return compact(strings.Join(parts, " "), 500)
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
				prompt = compact(lines[candidate], 1000)
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
		return parseClaudeReview(text, lines)
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
	// Leftover typed text only marks the custom answer as chosen while no
	// option row carries the confirmed selection; otherwise a stale note from
	// an earlier visit would override the real answer.
	if otherText != "" && !interaction.Other.Selected {
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
	interaction.ID = interactionID(interaction)
	return interaction
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
		options = append(options, Option{Index: optionIndex, Label: compact(match[3], 500)})
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
	interaction := &Interaction{
		Kind:           "single_select",
		Question:       "Review your answers and choose what to do",
		Options:        options,
		Other:          Other{Hidden: true},
		SubmitLabel:    "Continue",
		CanGoBack:      strings.Contains(text, "←"),
		QuestionIndex:  current,
		QuestionTotal:  total,
		Focus:          focus,
		AllOptionCount: len(options),
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
		if codexFooter(lines[index]) {
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
	otherItem := all[len(all)-1]
	options := all[:len(all)-1]
	notes := ""
	for _, line := range lines[notesStart:footerIndex] {
		trimmed := strings.TrimSpace(line)
		trimmed = strings.TrimSpace(strings.TrimPrefix(trimmed, "›"))
		if trimmed != "" {
			notes = compact(trimmed, 20000)
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

	type row struct {
		line     int
		number   int
		focus    bool
		label    string
		selected bool
	}
	var checkboxRows, menuRows []row
	expected := 1
	for index := headerIndex + 1; index < footerIndex; index++ {
		if match := checkboxPattern.FindStringSubmatch(lines[index]); match != nil {
			number, _ := strconv.Atoi(match[2])
			if number != expected {
				continue
			}
			checkboxRows = append(checkboxRows, row{
				line:     index,
				number:   number,
				focus:    match[1] != "",
				label:    compact(match[4], 500),
				selected: strings.TrimSpace(match[3]) != "",
			})
			expected++
			continue
		}
		match := menuPattern.FindStringSubmatch(lines[index])
		if match == nil {
			continue
		}
		number, _ := strconv.Atoi(match[2])
		if number != expected {
			continue
		}
		menuRows = append(menuRows, row{
			line:   index,
			number: number,
			focus:  match[1] != "",
			label:  compact(match[3], 500),
		})
		expected++
	}

	kind := "single_select"
	rows := menuRows
	submitRow := row{}
	if len(checkboxRows) >= 2 {
		kind = "multi_select"
		rows = checkboxRows
		for _, item := range menuRows {
			label := strings.TrimSpace(strings.TrimSuffix(item.label, "→"))
			if strings.EqualFold(label, "next") || strings.EqualFold(label, "submit") {
				submitRow = item
			}
		}
	}
	var otherRow row
	for _, item := range menuRows {
		if otherPattern.MatchString(item.label) {
			otherRow = item
		}
	}
	if len(rows) < 2 || otherRow.line == 0 {
		return nil
	}
	if kind == "single_select" {
		if !otherPattern.MatchString(rows[len(rows)-1].label) {
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
		questionText = compact(candidate, 1000)
		break
	}
	if questionText == "" {
		return nil
	}

	options := make([]Option, 0, len(rows))
	focus := Focus{Kind: "option"}
	for index, item := range rows {
		end := otherRow.line
		if index+1 < len(rows) {
			end = rows[index+1].line
		} else if submitRow.line > 0 {
			end = submitRow.line
		}
		options = append(options, Option{
			Index:       index,
			Label:       item.label,
			Description: description(lines, item.line, end),
			Selected:    item.selected,
		})
		if item.focus {
			focus = Focus{Kind: "option", Index: index}
		}
	}
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
		notes = compact(strings.Join(noteLines, " "), 20000)
	}
	interaction := &Interaction{
		Kind:     kind,
		Question: questionText,
		Options:  options,
		Other: Other{
			Selected:    otherRow.selected || notesActive,
			Text:        notes,
			Label:       otherRow.label,
			Placeholder: "Type an answer",
		},
		SubmitLabel:    "Next",
		CanGoBack:      current > 1,
		QuestionIndex:  current,
		QuestionTotal:  total,
		Focus:          focus,
		AllOptionCount: len(options) + 1,
		Agent:          "qoder",
		NotesActive:    notesActive,
	}
	if current == total {
		interaction.SubmitLabel = "Submit"
	}
	interaction.ID = interactionID(interaction)
	return interaction
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
	interaction := &Interaction{
		Kind:           "single_select",
		Question:       "Review your answers and choose what to do",
		Options:        options,
		Other:          Other{Hidden: true},
		SubmitLabel:    "Continue",
		CanGoBack:      true,
		QuestionIndex:  step,
		QuestionTotal:  step,
		Focus:          focus,
		AllOptionCount: len(options),
		Agent:          "qoder",
	}
	interaction.ID = interactionID(interaction)
	return interaction
}

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
		label := compact(match[labelIndex], 500)
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
		questionText = compact(candidate, 1000)
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
	for _, label := range regexp.MustCompile(`\s{2,}`).Split(strings.TrimSpace(line), -1) {
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
	return compact(strings.Join(parts, ""), 500)
}

func cleanOpenCodeLine(line string) string {
	line = strings.TrimRight(
		ansiPattern.ReplaceAllString(strings.ReplaceAll(line, "\r", ""), ""),
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
	return compact(strings.Join(lines[start:end+1], " "), 1000)
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
		(strings.Contains(lower, "navigate questions") ||
			strings.Contains(lower, "tab to add notes") ||
			strings.Contains(lower, "tab or esc to clear notes"))
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

func codexDescriptionColumn(lines []string, rows []codexRow) int {
	counts := make(map[int]int)
	for _, item := range rows {
		bodyStart := strings.Index(lines[item.line], item.body)
		if bodyStart < 0 {
			continue
		}
		if gap := columnGapPattern.FindStringIndex(item.body); gap != nil {
			prefixColumn := len([]rune(lines[item.line][:bodyStart]))
			descriptionColumn := prefixColumn + len([]rune(item.body[:gap[1]]))
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

func codexParts(lines []string, item codexRow, end, descriptionColumn int) (string, string) {
	var labels, descriptions []string
	for index := item.line; index < end; index++ {
		line := lines[index]
		if line == "" {
			continue
		}
		if index == item.line {
			left, right := item.body, ""
			if gap := columnGapPattern.FindStringIndex(item.body); gap != nil {
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

// summaryLines compacts each review answer separately and keeps one answer
// per line so the phone renders a readable summary instead of a paragraph.
// Entries stay structured so questions and answers can be styled apart.
func summaryLines(entries []SummaryEntry, limit int) string {
	parts := make([]string, 0, len(entries))
	for index := range entries {
		entries[index].Question = compact(entries[index].Question, limit)
		entries[index].Answer = compact(entries[index].Answer, limit)
		line := strconv.Itoa(index+1) + ". " + entries[index].Question
		if entries[index].Answer != "" {
			line += ": " + entries[index].Answer
		}
		parts = append(parts, line)
	}
	return strings.Join(parts, "\n")
}

// splitSummaryEntry separates a terminal "label: value" review line into its
// question and answer halves; the label side never contains a colon.
func splitSummaryEntry(line string) SummaryEntry {
	parts := strings.SplitN(line, ": ", 2)
	if len(parts) == 2 {
		return SummaryEntry{Question: parts[0], Answer: parts[1]}
	}
	return SummaryEntry{Question: line}
}

// CustomAnswerPlaceholder marks a review answer whose typed free text the
// terminal does not repeat.
const CustomAnswerPlaceholder = "custom answer"

// SummaryKey normalizes a question so review summary entries can be matched
// with the question views the free text was typed into.
func SummaryKey(value string) string {
	return strings.ToLower(compact(strings.TrimSuffix(strings.TrimSpace(value), "?"), 500))
}

// FillCustomAnswers replaces placeholder review answers with the recorded
// free-text answers and refreshes the plain-text summary fallback.
func FillCustomAnswers(interaction *Interaction, answers map[string]string) {
	if interaction == nil || len(answers) == 0 {
		return
	}
	for index := range interaction.Options {
		option := &interaction.Options[index]
		changed := false
		for i := range option.Summary {
			if option.Summary[i].Answer != CustomAnswerPlaceholder {
				continue
			}
			if text := answers[SummaryKey(option.Summary[i].Question)]; text != "" {
				option.Summary[i].Answer = text
				changed = true
			}
		}
		if changed {
			option.Description = summaryLines(option.Summary, 1000)
		}
	}
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
