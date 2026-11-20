package question

import (
	"regexp"
	"strings"

	"github.com/0cv/herdr-mobile-relay/internal/agentkind"
	"github.com/0cv/herdr-mobile-relay/internal/textutil"
)

const VisibleRows = 80

const claudeApprovalHint = "Esc to cancel · Tab to amend"

var claudeQuestionHints = []string{
	"Enter to select · ↑/↓ to navigate · Esc to cancel",
	"Enter to select · Tab/Arrow keys to navigate · Esc to cancel",
	"Enter to select · ↑/↓ to navigate · ctrl+g to edit in VS Code · Esc to cancel",
}

var (
	pureRule          = regexp.MustCompile(`^[─━═]{3,}$`)
	claudeTab         = regexp.MustCompile(`^(?:←\s+)?[☐☒☑]\s+\S.*$`)
	claudeTaskCount   = regexp.MustCompile(`^\d+ tasks? \(\d+ done, (?:\d+ in progress, )?\d+ open\)$`)
	claudeTaskItem    = regexp.MustCompile(`^[◻◼✔] \S.*$`)
	claudeTaskSummary = regexp.MustCompile(`^… \+\d+ (?:in progress|pending|completed)(?:, \d+ (?:pending|completed))*$`)
	ompAskTitle       = regexp.MustCompile(`^╭─+ Ask ─+╮$`)
	ompToolTitle      = regexp.MustCompile(`^╭─+ Allow tool: [\w.-]+ ─+╮$`)
	ompClose          = regexp.MustCompile(`^╰─+╯$`)
	ompDivider        = regexp.MustCompile(`^├[─┴]+┤$`)
	ompAskHint        = regexp.MustCompile(`^(?:Enter|⏎|\x{F0311}) (?:select|submit|next) · n note · ↑/↓ move · (?:Tab/←/→ · )?(?:⎋ cancel|Esc cancel)(?: · Alt\+H hide)?$`)
	ompUsage          = regexp.MustCompile(`^[A-Z] \d+%·\w+ \d+%·\w+ · [A-Z] \d+%·\w+ · drain [A-Z]→[A-Z]$`)
	ompModel          = regexp.MustCompile(`^[⠁-⣿] \d+[hms] · ⬢ [^·]+ · ◒ \w+ · 📁 [^·]+ · ⑂ [^·]+ · ◫ \d+(?:\.\d+)?%/\d+[KkMm](?: ⟲)? · \$\d+\.\d+(?:\s+📱 RELAY: .+)?$`)
	ompQuota          = regexp.MustCompile(`^[A-Z] \d+h \d+% w \d+%(?:→\w+)? f \d+%(?:→\w+)?(?: · [A-Z] \d+h \d+% w \d+%(?:→\w+)? f \d+%(?:→\w+)?)*\s+│ codexP \d+h \d+% w \d+% · codexS \d+h \d+% w \d+%\s+│\s+cache till \d+:\d\d(?:am|pm)$`)
	codexLiveHeader   = regexp.MustCompile(`(?i)^question \d+/\d+ \(\d+ unanswered\)$`)
	codexLiveHint     = regexp.MustCompile(`^(?:tab to (?:add|edit) notes \| )?enter to submit answer \| (?:←/→ to navigate questions \| )?esc to interrupt$`)
	qoderLiveHeader   = regexp.MustCompile(`^Asking User(?: ·\s+\S.* >\s+Submit)?$`)
	qoderLiveHint     = regexp.MustCompile(`^(?:Tab/←→ switch · |↑↓ navigate · )?Enter (?:toggle|select|submit) · Esc (?:back|cancel)$`)
)

// ClassifyLive requires a complete control on the visible screen. The bool
// distinguishes a reliable screen (including a miss) from an unusable read.
func ClassifyLive(text, agent string) (Classification, bool) {
	p := newPane(text)
	if len(p.raw) > VisibleRows {
		p = newPane(strings.Join(p.raw[len(p.raw)-VisibleRows:], "\n"))
	}
	for _, r := range strings.Join(mapLines(p.raw, textutil.StripANSI), "\n") {
		if r < ' ' && r != '\r' && r != '\t' && r != '\n' {
			return Classification{}, false
		}
	}
	if text == "" {
		return Classification{}, false
	}
	var result Classification
	switch agentkind.Of(agent) {
	case agentkind.Claude:
		result = liveClaude(p, agent)
	case agentkind.OMP, agentkind.Pi:
		result = liveOMP(p, agent)
	case agentkind.Codex, agentkind.Qoder:
		result = liveNumbered(p, agent)
	}
	switch result.Kind {
	case AttentionQuestion:
		if result.Interaction != nil && result.Interaction.ID != "" && result.Prompt != "" {
			return result, true
		}
	case AttentionApproval:
		if result.Prompt != "" && result.Command != "" && ApprovalFingerprint(result) != "" {
			return result, true
		}
	}
	return Classification{}, true
}

func beforeBlanks(lines []string, end int) int {
	for end > 0 && lines[end-1] == "" {
		end--
	}
	return end
}

func beforeRules(lines []string, end int) int {
	end = beforeBlanks(lines, end)
	for end > 0 && pureRule.MatchString(lines[end-1]) {
		end = beforeBlanks(lines, end-1)
	}
	return end
}

func claudeLiveEnd(lines []string) int {
	end := beforeRules(lines, len(lines))
	i := end
	if i > 0 && claudeTaskSummary.MatchString(lines[i-1]) {
		i--
	}
	itemsEnd := i
	for i > 0 && claudeTaskItem.MatchString(lines[i-1]) {
		i--
	}
	if i < itemsEnd && i > 0 && claudeTaskCount.MatchString(lines[i-1]) {
		i = beforeBlanks(lines, i-1)
		if i > 0 && claudeSessionRulePattern.MatchString(lines[i-1]) {
			return beforeBlanks(lines, i-1)
		}
	}
	if end > 0 && claudeSessionRulePattern.MatchString(lines[end-1]) {
		return beforeBlanks(lines, end-1)
	}
	return end
}

func knownFooter(lines []string, hints []string) (int, string) {
	start, hint := -1, ""
	for n := 1; n <= 3 && n <= len(lines); n++ {
		i := len(lines) - n
		if lines[i] == "" || n > 1 && pureRule.MatchString(lines[i]) {
			break
		}
		pieces := lines[i:]
		candidate := strings.Join(pieces, " ")
		for _, full := range hints {
			if candidate != full {
				continue
			}
			for j := 1; j < n; j++ {
				prefix := strings.Join(pieces[:j], " ")
				for _, complete := range hints {
					if prefix == complete {
						return -1, ""
					}
				}
			}
			if start >= 0 {
				return -1, ""
			}
			start, hint = i, full
		}
	}
	return start, hint
}

func liveClaude(p *pane, agent string) Classification {
	lines := p.trimmed()
	end := claudeLiveEnd(lines)
	if end == 0 {
		return Classification{}
	}
	if footer, hint := knownFooter(lines[:end], claudeQuestionHints); footer > 0 {
		chat := beforeBlanks(lines, footer) - 1
		if chat < 0 || !chatPattern.MatchString(lines[chat]) {
			return Classification{}
		}
		start := lastLineMatching(lines[:chat], claudeTab)
		if start > 0 && pureRule.MatchString(lines[start-1]) {
			return liveSpan(p, start, footer, hint, agent, AttentionQuestion)
		}
	}
	if footer, hint := knownFooter(lines[:end], []string{claudeApprovalHint}); footer > 0 {
		menu := latestApprovalMenu(p.indented()[:footer])
		if len(menu) >= 2 && menu[len(menu)-1].line == beforeBlanks(lines, footer)-1 {
			first := menu[0].line
			header := beforeBlanks(lines, first) - 1
			if header >= 0 && approvalHeader(agentkind.Claude, lines[header]) {
				start := lastLineMatching(lines[:header], pureRule)
				if start >= 0 {
					return liveSpan(p, start+1, footer, hint, agent, AttentionApproval)
				}
			}
		}
	}
	if !claudeReviewPattern.MatchString(lines[end-1]) {
		return Classification{}
	}
	i := beforeBlanks(lines, end-1) - 1
	if i < 0 || !claudeReviewPattern.MatchString(lines[i]) {
		return Classification{}
	}
	ready := beforeBlanks(lines, i) - 1
	if ready < 0 || lines[ready] != "Ready to submit your answers?" {
		return Classification{}
	}
	for i := ready - 1; i > 0; i-- {
		if lines[i] == "Review your answers" {
			start := beforeBlanks(lines, i) - 1
			if start > 0 && claudeTab.MatchString(lines[start]) && pureRule.MatchString(lines[start-1]) {
				result := liveSpan(p, start, end, "", agent, AttentionQuestion)
				if result.Interaction != nil && len(result.Interaction.Options) == 2 && len(result.Interaction.Options[0].Summary) > 0 {
					return result
				}
			}
			break
		}
	}
	return Classification{}
}

func liveSpan(p *pane, start, end int, hint, agent string, kind AttentionKind) Classification {
	span := strings.Join(p.raw[start:end], "\n")
	if hint != "" {
		span += "\n" + hint
	}
	result := Classify(span, agent)
	if result.Kind != kind {
		return Classification{}
	}
	if kind == AttentionApproval {
		rows := newPane(span).trimmed()
		result.Prompt = textutil.Compact(paneSummary(newPane(stableApprovalSource(strings.Join(rows, "\n")))), 500)
		result.ApprovalIdentity = ApprovalFingerprint(result)
	}
	return result
}

func liveOMP(p *pane, agent string) Classification {
	lines := ompCleanLines(p)
	end := beforeBlanks(lines, len(lines))
	if end == 0 {
		return Classification{}
	}
	switch {
	case ompQuota.MatchString(lines[end-1]):
		end--
	case end >= 5 && lines[end-1] == "⟲ history" && lines[end-2] == "cache ✓" && lines[end-3] == "omp-cache-keepalive: armed" && ompModel.MatchString(lines[end-4]) && ompUsage.MatchString(lines[end-5]):
		end -= 5
	}
	end = beforeBlanks(lines, end)
	if end == 0 || !ompClose.MatchString(lines[end-1]) {
		return Classification{}
	}
	for start := end - 2; start >= 0; start-- {
		line := lines[start]
		if strings.HasPrefix(line, "╰") {
			break
		}
		if !strings.HasPrefix(line, "╭") {
			continue
		}
		if !strings.HasSuffix(line, "╮") || len([]rune(line)) != len([]rune(lines[end-1])) {
			break
		}
		for _, raw := range p.raw[start+1 : end-1] {
			row := strings.TrimSpace(textutil.StripANSI(raw))
			if !(strings.HasPrefix(row, "│") && strings.HasSuffix(row, "│") || ompDivider.MatchString(row)) {
				return Classification{}
			}
		}
		footer := beforeBlanks(lines, end-1) - 1
		if footer <= start {
			break
		}
		switch {
		case ompAskTitle.MatchString(line) && ompAskHint.MatchString(lines[footer]) && ompDivider.MatchString(lines[footer-1]):
			return liveSpan(p, start, end, "", agent, AttentionQuestion)
		case ompToolTitle.MatchString(line) && lines[footer] == "up/down navigate  enter select  esc cancel":
			result := liveSpan(p, start, end, "", agent, AttentionApproval)
			if last := beforeBlanks(lines, footer) - 1; last > start && len(result.Options) >= 2 && strings.TrimSpace(ompPlanFocusPattern.ReplaceAllString(lines[last], "")) == result.Options[len(result.Options)-1] {
				return result
			}
		case ompPlanMenuPattern.MatchString(lines[start+1]) && lines[footer] == "↑↓ select · ⏎ confirm · ◂▸ model · c copy · tab regions · Ctrl+G editor · esc cancel" && ompDivider.MatchString(lines[footer-1]):
			return liveSpan(p, start+1, end, "", agent, AttentionApproval)
		}
		break
	}
	return Classification{}
}

func liveNumbered(p *pane, agent string) Classification {
	lines := p.trimmed()
	end := beforeRules(lines, len(lines))
	if end == 0 {
		return Classification{}
	}
	hint := lines[end-1]
	header := codexLiveHeader
	if agentkind.Of(agent) == agentkind.Qoder {
		if !qoderLiveHint.MatchString(hint) {
			return Classification{}
		}
		header = qoderLiveHeader
	} else if !codexLiveHint.MatchString(hint) {
		if hint == "Press enter to confirm or esc to go back" {
			rows := latestApprovalMenu(p.indented()[:end-1])
			if len(rows) >= 2 && rows[len(rows)-1].line == beforeBlanks(lines, end-1)-1 {
				start := beforeBlanks(lines, rows[0].line) - 1
				if start >= 0 && lines[start] == "Implement this plan?" {
					return liveSpan(p, start, end, "", agent, AttentionApproval)
				}
			}
		}
		return Classification{}
	}
	if start := lastLineMatching(lines[:end-1], header); start >= 0 {
		return liveSpan(p, start, end, "", agent, AttentionQuestion)
	}
	return Classification{}
}
