package question

import (
	"regexp"
	"strings"

	"github.com/0cv/herdr-mobile-relay/internal/agentkind"
	"github.com/0cv/herdr-mobile-relay/internal/textutil"
)

var (
	edgePattern      = regexp.MustCompile(`^[│|]\s*|\s*[│|]$`)
	checkboxPattern  = regexp.MustCompile(`^\s*([❯›]?)\s*(\d+)\.\s*\[([^\]]*)\]\s*(.*?)\s*$`)
	menuPattern      = regexp.MustCompile(`^\s*([❯›]?)\s*(\d+)\.\s+(.*?)\s*$`)
	submitPattern    = regexp.MustCompile(`(?i)^\s*([❯›]?)\s*(?:\d+\.\s*)?(submit|next)\s*$`)
	chatPattern      = regexp.MustCompile(`(?i)^\s*([❯›]?)\s*(?:\d+\.\s*)?chat about this\s*$`)
	otherPattern     = regexp.MustCompile(`(?i)^(?:type something\.?|type your own answer|none of the above|other)\b`)
	selectedPattern  = regexp.MustCompile(`\s*[✓✔]\s*$`)
	columnGapPattern = regexp.MustCompile(`\s{2,}`)
)

func Supports(agent string) bool {
	return supportsKind(agentkind.Of(agent))
}

func supportsKind(kind string) bool {
	switch kind {
	case agentkind.Claude, agentkind.Codex, agentkind.OMP, agentkind.Pi, agentkind.OpenCode,
		agentkind.Qoder, agentkind.Hermes, agentkind.Agy:
		return true
	}
	return false
}

func Parse(text, agent string) *Interaction {
	return parse(newPane(text), agent)
}

func parse(p *pane, agent string) *Interaction {
	if !layoutHint(p) {
		return nil
	}
	kind := agentkind.Of(agent)
	switch kind {
	case agentkind.OMP, agentkind.Pi:
		return parseOMP(p)
	case agentkind.Codex:
		return parseCodex(p)
	case agentkind.Claude:
		return parseClaude(p)
	case agentkind.Qoder:
		return parseQoder(p)
	case agentkind.OpenCode:
		return parseOpenCode(p)
	}
	if interaction := parseCodex(p); interaction != nil {
		return interaction
	}
	if interaction := parseClaude(p); interaction != nil {
		return interaction
	}
	return parseQoder(p)
}

func LayoutHint(text string) bool {
	return layoutHint(newPane(text))
}

func layoutHint(p *pane) bool {
	if openCodeLayoutHint(p) || ompLayoutHint(p) {
		return true
	}
	lines := p.trimmed()
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
		// A named Claude session ends its input rule with the name, and its task panel follows.
		if claudeSessionRulePattern.MatchString(line) || claudeTaskPanelPattern.MatchString(line) {
			continue
		}
		if line != "" && strings.Trim(line, "─━═_—│| ") != "" {
			return false
		}
	}
	return true
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
	return textutil.Compact(strings.Join(parts, " "), 500)
}
