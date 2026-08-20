// Package noecho recognizes terminal prompts that read a secret with echo
// disabled — sudo, ssh, gpg and friends. The phone needs the signal because
// such a prompt must never reach the draft store, the activity journal or the
// audit payload hash, and because the generic composer stays locked on a pane
// the attention classifier cannot read.
package noecho

import (
	"regexp"
	"strings"
	"unicode/utf8"

	"github.com/0cv/herdr-mobile-relay/internal/history"
)

// MaxPromptChars bounds both the accepted and the reported prompt line. A real
// noecho prompt is short; anything longer is prose that merely mentions a
// password.
const MaxPromptChars = 120

var promptPatterns = []*regexp.Regexp{
	regexp.MustCompile(`(?i)^\[sudo\] password for [^\s:]{1,64}\s*:$`),
	regexp.MustCompile(`(?i)^\S+@\S+'s password\s*:$`),
	regexp.MustCompile(`(?i)^password\s*:$`),
	regexp.MustCompile(`(?i)^password for [^:]{1,80}:$`),
	regexp.MustCompile(`(?i)^enter passphrase\s*:$`),
	regexp.MustCompile(`(?i)^enter passphrase for key '[^']{1,96}'\s*:$`),
	regexp.MustCompile(`(?i)^enter pin(?: for [^:]{1,80})?\s*:$`),
	regexp.MustCompile(`(?i)^(?:repeat|verify|confirm) password\s*:$`),
}

// rejectPattern covers shapes that read like a question about a password
// rather than a request for one. A yes/no affordance in particular means the
// pane wants a keystroke, not a secret.
var rejectPattern = regexp.MustCompile(`(?i)y\s*/\s*n|password policy|password manager`)

// Match reports a recognized no-echo secret prompt at the tail of pane content.
// prompt is the matched line, trimmed and capped at 120 chars.
func Match(content string) (prompt string, ok bool) {
	line, found := lastNonEmptyLine(content)
	if !found {
		return "", false
	}
	if utf8.RuneCountInString(line) > MaxPromptChars {
		return "", false
	}
	if strings.HasSuffix(line, "?") || rejectPattern.MatchString(line) {
		return "", false
	}
	for _, pattern := range promptPatterns {
		if pattern.MatchString(line) {
			return line, true
		}
	}
	return "", false
}

func lastNonEmptyLine(content string) (string, bool) {
	for rest := content; rest != ""; {
		index := strings.LastIndexByte(rest, '\n')
		line := strings.TrimSpace(history.NormalizeLine(rest[index+1:]))
		if line != "" {
			return line, true
		}
		if index < 0 {
			return "", false
		}
		rest = rest[:index]
	}
	return "", false
}
