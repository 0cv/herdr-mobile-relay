package slashcmd

import (
	"regexp"
	"strconv"
	"strings"
)

type CopyProfile struct {
	PreSubmission       *regexp.Regexp
	PostSubmission      *regexp.Regexp
	Confirmation        *regexp.Regexp
	Composer            *regexp.Regexp
	ComposerPlaceholder *regexp.Regexp
	ComposerOptional    bool
	IdleLayout          *regexp.Regexp
	SecondaryPath       string
}

var copyProfiles = map[string]CopyProfile{
	"claude": {
		Confirmation: regexp.MustCompile(`(?im)copied\s+to\s+clipboard\s*\((?P<chars>[0-9]+)\s+characters?,\s*(?P<lines>[0-9]+)\s+lines?\)`),
		Composer:     regexp.MustCompile(`(?m)^\s*❯\s*(?P<text>.*?)\s*$`),
	},
	"codex": {
		Confirmation:        regexp.MustCompile(`(?im)copied\s+last\s+message\s+to\s+clipboard`),
		Composer:            regexp.MustCompile(`(?m)^\s*›\s*(?P<text>.*?)\s*$`),
		ComposerPlaceholder: regexp.MustCompile(`(?m)^\s*›\s*Use\s+/skills\b.*$`),
	},
	"kimi": {
		Confirmation: regexp.MustCompile(`(?im)copied\s+(?:to\s+clipboard\s*\(|via\s+terminal\s+escape\s+sequence\s+\(unverified,\s*)(?P<chars>[0-9]+)\s+characters?\)?\.?`),
		Composer:     regexp.MustCompile(`(?m)^\s*│\s*>\s*(?P<text>.*?)\s*│\s*$`),
	},
	// omp draws its composer as a bare line between two full-width rules; the
	// slash palette opens under the lower rule while /copy sits in the composer.
	"omp": {
		PreSubmission:  regexp.MustCompile(`(?ims)^[ \t]*/copy[ \t]*\r?\n[ \t]*─{20,}[ \t]*\r?\n(?:.*\r?\n){0,2}[^\r\n]*\bcopy\s+Pick\s+text\s+or\s+code\b`),
		PostSubmission: regexp.MustCompile(`(?m)^\s*╭─\s+Copy to clipboard\b.*$`),
		Confirmation:   regexp.MustCompile(`(?im)copied\s+last\s+message\s+to\s+clipboard`),
		Composer: regexp.MustCompile(
			`(?m)^[ \t]*─{20,}[ \t]*\r?\n[ \t]*(?P<text>(?:/[^\r\n]*?)?)[ \t]*\r?\n[ \t]*─{20,}[ \t]*$`,
		),
	},
	"pi": {
		Confirmation:     regexp.MustCompile(`(?im)copied\s+last\s+agent\s+message\s+to\s+clipboard`),
		Composer:         regexp.MustCompile(`(?m)^\s*❯\s*(?P<text>(?:/[^\r\n]*)?)\s*$`),
		ComposerOptional: true,
		IdleLayout: regexp.MustCompile(
			`(?m)^[ \t]*─{20,}[ \t]*\r?\n[ \t]*\r?\n[ \t]*─{20,}[ \t]*\r?\n[ \t]*(?:~|/)[^\r\n]*\r?\n[ \t]*↑[0-9][^\r\n]*•[ \t]+\S+[ \t]*$`,
		),
	},
	"qoder": {
		PreSubmission:       regexp.MustCompile(`(?ims)^\s*[❯›>]\s*/copy\b[^\r\n]*\r?\n(?:.*\r?\n){0,2}\s*(?:[|│❯›>→]\s*)*copy\b[^\r\n]*(?:clipboard|last\s+assistant)`),
		Confirmation:        regexp.MustCompile(`(?im)copied\s+to\s+clipboard\s*\((?P<chars>[0-9]+)\s+characters?,\s*(?P<lines>[0-9]+)\s+lines?\)`),
		Composer:            regexp.MustCompile(`(?m)^\s*>\s*(?P<text>.*?)\s*$`),
		ComposerPlaceholder: regexp.MustCompile(`(?m)^\s*>\s*Type your message or @path/to/file\s*$`),
		SecondaryPath:       "/tmp/qoder-copy/response.md",
	},
}

var copyANSI = regexp.MustCompile(`\x1b\[[0-?]*[ -/]*[@-~]`)

func CopyProfileFor(profileID, agent string) (CopyProfile, bool) {
	for _, value := range []string{profileID, agent} {
		key := strings.ToLower(strings.TrimSpace(value))
		key = strings.ReplaceAll(key, " ", "")
		key = strings.ReplaceAll(key, "-", "")
		switch key {
		case "claude", "claudecode":
			profile, ok := copyProfiles["claude"]
			return profile, ok
		case "codex", "openaicodex":
			profile, ok := copyProfiles["codex"]
			return profile, ok
		case "kimi", "kimicode":
			profile, ok := copyProfiles["kimi"]
			return profile, ok
		case "omp", "ohmypi":
			profile, ok := copyProfiles["omp"]
			return profile, ok
		case "pi", "picodingagent":
			profile, ok := copyProfiles["pi"]
			return profile, ok
		case "qoder", "qodercli":
			profile, ok := copyProfiles["qoder"]
			return profile, ok
		}
	}
	return CopyProfile{}, false
}

func (p CopyProfile) CleanSnapshot(content string) string {
	return copyANSI.ReplaceAllString(strings.ReplaceAll(content, "\r\n", "\n"), "")
}

func (p CopyProfile) MenuOpen(content string) bool {
	return p.overlayOpen(p.PreSubmission, content)
}

func (p CopyProfile) PickerOpen(content string) bool {
	return p.overlayOpen(p.PostSubmission, content)
}

func (p CopyProfile) overlayOpen(pattern *regexp.Regexp, content string) bool {
	if pattern == nil {
		return false
	}
	clean := p.CleanSnapshot(content)
	matches := pattern.FindAllStringIndex(clean, -1)
	if len(matches) == 0 {
		return false
	}
	if p.Composer == nil {
		return true
	}
	composers := p.Composer.FindAllStringIndex(clean, -1)
	if len(composers) == 0 {
		return true
	}
	return matches[len(matches)-1][1] > composers[len(composers)-1][1]
}

func (p CopyProfile) ConfirmationCounts(content string) (chars, lines int, ok bool) {
	if p.Confirmation == nil {
		return 0, 0, false
	}
	matches := p.Confirmation.FindAllStringSubmatch(p.CleanSnapshot(content), -1)
	if len(matches) == 0 {
		return 0, 0, false
	}
	match := matches[len(matches)-1]
	chars, lines = -1, -1
	if index := p.Confirmation.SubexpIndex("chars"); index >= 0 && index < len(match) {
		parsed, err := strconv.Atoi(match[index])
		if err != nil || parsed < 0 {
			return 0, 0, false
		}
		chars = parsed
	}
	if index := p.Confirmation.SubexpIndex("lines"); index >= 0 && index < len(match) {
		parsed, err := strconv.Atoi(match[index])
		if err != nil || parsed < 0 {
			return 0, 0, false
		}
		lines = parsed
	}
	return chars, lines, true
}

func (p CopyProfile) ComposerText(content string) (string, bool) {
	if p.Composer == nil {
		return "", false
	}
	matches := p.Composer.FindAllStringSubmatch(p.CleanSnapshot(content), -1)
	if len(matches) == 0 {
		return "", false
	}
	match := matches[len(matches)-1]
	if p.ComposerPlaceholder != nil && p.ComposerPlaceholder.MatchString(match[0]) {
		return "", true
	}
	index := p.Composer.SubexpIndex("text")
	if index < 0 || index >= len(match) {
		return strings.TrimSpace(match[0]), true
	}
	return strings.TrimSpace(match[index]), true
}

func (p CopyProfile) ComposerReady(content string) bool {
	clean := p.CleanSnapshot(content)
	if p.IdleLayout != nil && !p.IdleLayout.MatchString(clean) {
		return false
	}
	composer, found := p.ComposerText(clean)
	return (found && composer == "") || (!found && p.ComposerOptional)
}
