package question

import (
	"regexp"
	"strconv"
	"strings"
)

type AttentionKind string

const (
	AttentionApproval AttentionKind = "approval"
	AttentionQuestion AttentionKind = "question"
	AttentionChat     AttentionKind = "chat"
	AttentionUnknown  AttentionKind = "unknown"
)

type Classification struct {
	Kind           AttentionKind
	Prompt         string
	Command        string
	Options        []string
	ApprovalFocus  int
	Interaction    *Interaction
	QuestionLayout bool
}

type approvalMenuRow struct {
	line  int
	focus bool
	label string
}

var (
	approvalFooterPattern = regexp.MustCompile(
		`(?i)(?:enter\s+(?:to\s+)?(?:select|confirm)|` +
			`(?:esc|escape)\s+(?:to\s+)?(?:cancel|reject|deny|exit)|` +
			`(?:↑/↓|up/down).*(?:navigate|select)|tab\s+to\s+(?:edit|amend))`,
	)
	normalPromptPattern = regexp.MustCompile(`(?i)^\s*[❯›>]\s*(?:$|(?:ask|describe|type|send|use)\b.*)$`)
	statusFooterPattern = regexp.MustCompile(
		`(?i)(?:\bcontext\s+\d+%\s+used\b|\bctx\s*:?\s*(?:\d+%|-+)|` +
			`\?\s+for\s+shortcuts|\b(?:manual|plan)\s+mode\b|` +
			`\b(?:shift\+tab|ctrl\+|cmd\+)|\b\d+\s+agents?\b)`,
	)
	ompPlanMenuPattern         = regexp.MustCompile(`(?i)^plan mode\s*[-–—]\s*next step$`)
	ompPlanFocusPattern        = regexp.MustCompile(`^[❯›>\x{f054}]\s+`)
	ompInputHeaderPattern      = regexp.MustCompile(`^╭[─━═]{2}.*╮$`)
	ompInputFooterPattern      = regexp.MustCompile(`^╰[─━═].*[─━═]╯$`)
	openCodeInputPromptPattern = regexp.MustCompile(`(?i)\bask anything\.\.\.`)
	piInputStatusPattern       = regexp.MustCompile(`(?i)\d+(?:\.\d+)?%/\d+[km]\b`)
	terminalRulePattern        = regexp.MustCompile(`^[─━═_—]{8,}$`)
)

// Classify determines what, if anything, the live control region is asking
// the user to do. A blocked agent status is deliberately not an input.
func Classify(text, agent string) Classification {
	structuredControls := Supports(agent)
	inputReady := normalInputPrompt(text, agent)
	if !structuredControls && !inputReady {
		return Classification{
			Kind:   AttentionUnknown,
			Prompt: compact(PaneSummary(text), 500),
		}
	}
	if structuredControls {
		if interaction := Parse(text, agent); interaction != nil {
			return Classification{
				Kind:           AttentionQuestion,
				Prompt:         interaction.Question,
				Command:        interaction.Question,
				Interaction:    interaction,
				QuestionLayout: true,
			}
		}
		if options, focus := liveApprovalDetails(text, agent); len(options) > 0 {
			summaryLines := paneSummaryLines(text)
			return Classification{
				Kind:          AttentionApproval,
				Prompt:        compact(strings.Join(summaryLines, "\n"), 500),
				Command:       compact(approvalCommand(summaryLines), 240),
				Options:       options,
				ApprovalFocus: focus,
			}
		}
	}
	if inputReady {
		response := LatestCompletedResponse(text)
		if response == "" {
			response = PaneSummary(text)
		}
		return Classification{
			Kind:   AttentionChat,
			Prompt: response,
		}
	}
	return Classification{
		Kind:   AttentionUnknown,
		Prompt: compact(PaneSummary(text), 500),
	}
}

func liveApprovalDetails(text, agent string) ([]string, int) {
	normalized := strings.ToLower(agent)
	if strings.Contains(normalized, "opencode") {
		return nil, 0
	}
	lines := cleanLines(text)
	if ompAskAgent(normalized) {
		return ompPlanApprovalDetails(lines)
	}
	rawLines := strings.Split(strings.ReplaceAll(text, "\r\n", "\n"), "\n")
	menuLines := make([]string, len(rawLines))
	for index, line := range rawLines {
		menuLines[index] = cleanCodexLine(line)
	}
	rows := latestApprovalMenu(menuLines)
	if len(rows) < 2 || !approvalLabels(rows) {
		return nil, 0
	}

	latestCompleted := latestCompletedTurnLine(lines)
	if rows[0].line <= latestCompleted {
		return nil, 0
	}

	headerStart := latestCompleted + 1
	if candidate := rows[0].line - 16; candidate > headerStart {
		headerStart = candidate
	}
	header := strings.Join(lines[headerStart:rows[0].line], "\n")
	if !approvalHeader(normalized, header) {
		return nil, 0
	}
	if newerOutputAfterMenu(menuLines, rows[len(rows)-1].line, normalized) {
		return nil, 0
	}

	options := make([]string, 0, len(rows))
	focus := 0
	for index, row := range rows {
		options = append(options, row.label)
		if row.focus {
			focus = index
		}
	}
	return options, focus
}

// ompPlanApprovalDetails detects the OMP plan-review action menu, which uses
// unnumbered focus-marker rows instead of the shared numbered approval menus.
func ompPlanApprovalDetails(lines []string) ([]string, int) {
	header := -1
	for index, line := range lines {
		if ompPlanMenuPattern.MatchString(line) {
			header = index
		}
	}
	if header < 0 {
		return nil, 0
	}
	options := make([]string, 0, 4)
	focus := 0
	end := -1
	for index := header + 1; index < len(lines); index++ {
		line := lines[index]
		if line == "" || ompBorderLine(line) {
			end = index
			break
		}
		lower := strings.ToLower(line)
		if strings.HasPrefix(lower, "continue with") || strings.HasPrefix(line, "↳") {
			continue
		}
		if marker := ompPlanFocusPattern.FindString(line); marker != "" {
			focus = len(options)
			line = strings.TrimSpace(strings.TrimPrefix(line, marker))
		}
		options = append(options, compact(line, 500))
	}
	if end < 0 || len(options) < 2 {
		return nil, 0
	}
	for _, line := range lines[end:] {
		if line == "" || ompBorderLine(line) ||
			strings.ContainsRune(line, '·') ||
			strings.Contains(strings.ToLower(line), "scroll") {
			continue
		}
		return nil, 0
	}
	return options, focus
}

func latestApprovalMenu(lines []string) []approvalMenuRow {
	var runs [][]approvalMenuRow
	var current []approvalMenuRow
	expected := 1
	flush := func() {
		if len(current) > 0 {
			runs = append(runs, current)
		}
		current = nil
		expected = 1
	}
	for index, line := range lines {
		match := menuPattern.FindStringSubmatch(line)
		if match == nil {
			if len(current) > 0 && line != "" && !approvalContinuation(line) {
				flush()
			}
			continue
		}
		number, _ := strconv.Atoi(match[2])
		label := compact(match[3], 500)
		switch {
		case number == 1:
			flush()
			current = []approvalMenuRow{{line: index, focus: match[1] != "", label: label}}
			expected = 2
		case len(current) > 0 && number == expected:
			current = append(current, approvalMenuRow{line: index, focus: match[1] != "", label: label})
			expected++
		default:
			flush()
		}
	}
	flush()
	for index := len(runs) - 1; index >= 0; index-- {
		if len(runs[index]) < 2 {
			continue
		}
		focused := false
		for _, row := range runs[index] {
			focused = focused || row.focus
		}
		if focused {
			return runs[index]
		}
	}
	return nil
}

func approvalContinuation(line string) bool {
	lower := strings.ToLower(line)
	return strings.HasPrefix(line, " ") ||
		approvalFooterPattern.MatchString(line) ||
		strings.Contains(lower, "esc to cancel")
}

func approvalHeader(agent, header string) bool {
	lower := strings.ToLower(header)
	switch {
	case strings.Contains(agent, "codex"):
		return (strings.Contains(lower, "would you like to") ||
			strings.Contains(lower, "do you want to") ||
			strings.Contains(lower, "implement this plan") ||
			strings.Contains(lower, "approve all pending") ||
			strings.Contains(lower, "requested permission") ||
			strings.Contains(lower, "approve") &&
				(strings.Contains(lower, "subagent") ||
					strings.Contains(lower, "pending") ||
					strings.Contains(lower, "permission"))) &&
			(strings.Contains(lower, "run") ||
				strings.Contains(lower, "proceed") ||
				strings.Contains(lower, "permission") ||
				strings.Contains(lower, "subagent") ||
				strings.Contains(lower, "agent") ||
				strings.Contains(lower, "plan") ||
				strings.Contains(lower, "command") ||
				strings.Contains(lower, "tool"))
	case strings.Contains(agent, "claude"):
		return strings.Contains(lower, "do you want to proceed") ||
			strings.Contains(lower, "would you like to proceed") ||
			strings.Contains(lower, "do you want to") &&
				(strings.Contains(lower, "create") ||
					strings.Contains(lower, "edit") ||
					strings.Contains(lower, "delete") ||
					strings.Contains(lower, "run")) ||
			strings.Contains(lower, "allow") &&
				(strings.Contains(lower, "permission") ||
					strings.Contains(lower, "tool") ||
					strings.Contains(lower, "command") ||
					strings.Contains(lower, "action")) ||
			(strings.Contains(lower, "needs your permission") ||
				strings.Contains(lower, "requested permission")) &&
				(strings.Contains(lower, "tool") ||
					strings.Contains(lower, "bash") ||
					strings.Contains(lower, "command") ||
					strings.Contains(lower, "action"))
	case strings.Contains(agent, "qoder"):
		return strings.Contains(lower, "permission required") &&
			(strings.Contains(lower, "apply this change") ||
				strings.Contains(lower, "tool:") ||
				strings.Contains(lower, "file:")) ||
			strings.Contains(lower, "would you like to proceed") &&
				(strings.Contains(lower, "ready to execute") ||
					strings.Contains(lower, "plan approval")) ||
			strings.Contains(lower, "allow") &&
				(strings.Contains(lower, "action") ||
					strings.Contains(lower, "command") ||
					strings.Contains(lower, "tool"))
	default:
		return false
	}
}

func approvalLabels(rows []approvalMenuRow) bool {
	first := strings.ToLower(rows[0].label)
	last := strings.ToLower(rows[len(rows)-1].label)
	positive := regexp.MustCompile(`\b(?:yes|allow|approve|proceed|trust)\b`).MatchString(first)
	negative := regexp.MustCompile(`\b(?:no|deny|reject|cancel|exit)\b`).MatchString(last)
	return positive && negative
}

func latestCompletedTurnLine(lines []string) int {
	for index := len(lines) - 1; index >= 0; index-- {
		if turnDurationPattern.MatchString(lines[index]) {
			return index
		}
	}
	return -1
}

func newerOutputAfterMenu(lines []string, lastMenuLine int, agent string) bool {
	for _, line := range lines[lastMenuLine+1:] {
		if line == "" || chromePattern.MatchString(line) || approvalFooterPattern.MatchString(line) {
			continue
		}
		if strings.Contains(agent, "qoder") && qoderApprovalTailLine(line) {
			continue
		}
		return true
	}
	return false
}

func qoderApprovalTailLine(line string) bool {
	trimmed := strings.TrimSpace(line)
	if strings.EqualFold(trimmed, "Ctrl+X to edit plan") {
		return true
	}
	if !strings.HasPrefix(line, " ") || trimmed == "" {
		return false
	}
	lower := strings.ToLower(trimmed)
	return strings.HasPrefix(lower, "reject this plan") &&
		strings.Contains(lower, "without providing feedback")
}

func normalInputPrompt(text, agent string) bool {
	normalized := strings.ToLower(agent)
	if !Supports(normalized) && !strings.Contains(normalized, "kimi") {
		return false
	}
	lines := cleanLines(text)
	if ompInputFramePrompt(lines, normalized) ||
		piInputFramePrompt(lines, normalized) ||
		openCodeInputPrompt(lines, normalized) ||
		kimiInputFramePrompt(lines, normalized) {
		return true
	}
	for index := len(lines) - 1; index >= 0 && index >= len(lines)-10; index-- {
		if !normalPromptPattern.MatchString(lines[index]) {
			continue
		}
		validTail := true
		for _, line := range lines[index+1:] {
			if line == "" || chromePattern.MatchString(line) || statusFooterPattern.MatchString(line) {
				continue
			}
			validTail = false
			break
		}
		if validTail {
			return true
		}
	}
	return false
}

func ompInputFramePrompt(lines []string, agent string) bool {
	if !ompAskAgent(agent) {
		return false
	}
	last := len(lines) - 1
	for last >= 0 && lines[last] == "" {
		last--
	}
	if last < 1 || !ompInputFooterPattern.MatchString(lines[last]) {
		return false
	}
	previous := last - 1
	for previous >= 0 && lines[previous] == "" {
		previous--
	}
	return previous >= 0 && ompInputHeaderPattern.MatchString(lines[previous])
}

func piInputFramePrompt(lines []string, agent string) bool {
	agent = strings.ToLower(strings.TrimSpace(agent))
	if agent != "pi" && !strings.HasPrefix(agent, "pi-") {
		return false
	}
	last := len(lines) - 1
	for last >= 0 && lines[last] == "" {
		last--
	}
	if last < 1 || !piInputStatusPattern.MatchString(lines[last]) {
		return false
	}
	rules := 0
	for index := last - 1; index >= 0 && index >= last-6; index-- {
		if !terminalRulePattern.MatchString(lines[index]) {
			continue
		}
		rules++
		if rules == 2 {
			return true
		}
	}
	return false
}

func openCodeInputPrompt(lines []string, agent string) bool {
	if !strings.Contains(strings.ToLower(agent), "opencode") {
		return false
	}
	for index := len(lines) - 1; index >= 0 && index >= len(lines)-12; index-- {
		if openCodeInputPromptPattern.MatchString(lines[index]) {
			return true
		}
	}
	return false
}

func kimiInputFramePrompt(lines []string, agent string) bool {
	if !strings.Contains(strings.ToLower(agent), "kimi") {
		return false
	}
	for footer := len(lines) - 1; footer >= 2 && footer >= len(lines)-12; footer-- {
		if !ompInputFooterPattern.MatchString(lines[footer]) {
			continue
		}
		prompt := footer - 1
		for prompt >= 0 && lines[prompt] == "" {
			prompt--
		}
		if prompt < 1 || !normalPromptPattern.MatchString(lines[prompt]) {
			continue
		}
		header := prompt - 1
		for header >= 0 && lines[header] == "" {
			header--
		}
		if header >= 0 && ompInputHeaderPattern.MatchString(lines[header]) {
			return true
		}
	}
	return false
}
