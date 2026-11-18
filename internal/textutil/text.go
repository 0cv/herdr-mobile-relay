package textutil

import (
	"regexp"
	"strings"
)

var ansiPattern = regexp.MustCompile(`\x1b\[[0-?]*[ -/]*[@-~]`)

func StripANSI(value string) string {
	return ansiPattern.ReplaceAllString(value, "")
}

func Compact(value string, limit int) string {
	value = strings.Join(strings.Fields(value), " ")
	runes := []rune(value)
	if len(runes) > limit {
		return string(runes[:limit])
	}
	return value
}
