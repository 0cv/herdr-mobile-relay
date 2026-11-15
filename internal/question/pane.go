package question

import (
	"regexp"
	"strings"

	"github.com/0cv/herdr-mobile-relay/internal/textutil"
)

var (
	chromePattern       = regexp.MustCompile(`(?i)^(?:[\s─━═_—│|◔◑◕●┃┆┊╭╮╯╰├┤┬┴┼┌┐└┘]+|.*\besc to cancel\b|.*\btype to queue\b|[◔◑◕●]\s+(?:shell|bash).*)$`)
	promptSkipPattern   = regexp.MustCompile(`(?i)^(?:bash command|do you want to proceed\??|would you like to run\b.*|environment:\s*\w+|press enter to confirm\b.*|esc to cancel\b.*)$`)
	turnDurationPattern = regexp.MustCompile(
		`(?i)^[^\p{L}\p{N}]*\p{L}+(?:ed|ing)\s+for\s+(?:\d+h\s*)?(?:\d+m\s*)?\d+s\b`,
	)
	responseStartPattern  = regexp.MustCompile(`^\s*[•●]\s+\S`)
	responsePrefixPattern = regexp.MustCompile(`^\s*[•●]\s+`)
)

type pane struct {
	text         string
	raw          []string
	trimmedLines []string
	indentedRows []string
	openCodeRows []string
}

func newPane(text string) *pane {
	return &pane{text: text, raw: strings.Split(strings.ReplaceAll(text, "\r\n", "\n"), "\n")}
}

// trimmed, indented and openCode are built on first use. Callers must not edit the returned lines.
func (p *pane) trimmed() []string {
	if p.trimmedLines == nil {
		p.trimmedLines = mapLines(p.raw, cleanLine)
	}
	return p.trimmedLines
}

func (p *pane) indented() []string {
	if p.indentedRows == nil {
		p.indentedRows = mapLines(p.raw, cleanIndentedLine)
	}
	return p.indentedRows
}

func (p *pane) openCode() []string {
	if p.openCodeRows == nil {
		p.openCodeRows = mapLines(p.raw, cleanOpenCodeLine)
	}
	return p.openCodeRows
}

func mapLines(lines []string, clean func(string) string) []string {
	result := make([]string, len(lines))
	for index, line := range lines {
		result[index] = clean(line)
	}
	return result
}

func PaneSummary(text string) string {
	return paneSummary(newPane(text))
}

func paneSummary(p *pane) string {
	return strings.Join(paneSummaryLines(p), "\n")
}

func latestCompletedTurnLine(lines []string) int {
	for index := len(lines) - 1; index >= 0; index-- {
		if turnDurationPattern.MatchString(strings.TrimSpace(lines[index])) {
			return index
		}
	}
	return -1
}

// LatestCompletedResponse returns the complete latest Codex or Claude response
// bounded by the agent's response marker and completed-turn duration line.
// Unlike PaneSummary, it intentionally does not impose a display-line limit;
// the activity journal applies its own persisted extract safety limit.
func LatestCompletedResponse(text string) string {
	rawLines := strings.Split(strings.ReplaceAll(text, "\r", ""), "\n")
	lines := make([]string, len(rawLines))
	for index, line := range rawLines {
		lines[index] = strings.TrimRight(textutil.StripANSI(line), " \t")
	}

	end := latestCompletedTurnLine(lines)
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

func paneSummaryLines(p *pane) []string {
	var summaryLines []string
	for _, line := range p.trimmed() {
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

func cleanLine(line string) string {
	line = strings.TrimSpace(textutil.StripANSI(strings.ReplaceAll(line, "\r", "")))
	for {
		next := strings.TrimSpace(edgePattern.ReplaceAllString(line, ""))
		if next == line {
			return line
		}
		line = next
	}
}

func cleanIndentedLine(line string) string {
	line = strings.TrimRight(
		textutil.StripANSI(strings.ReplaceAll(line, "\r", "")),
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
