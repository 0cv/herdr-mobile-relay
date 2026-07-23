package slashcmd

import (
	"strconv"
	"strings"
)

type codexProvider struct{}

func (p *codexProvider) ID() string { return "codex" }

type versionedBuiltins struct {
	MinVersion string
	Commands   []Command
}

var codexBuiltinsBase = []Command{
	{"/clear", "Start a fresh conversation", "builtin", ""},
	{"/help", "Show available commands", "builtin", ""},
	{"/model", "Switch the AI model", "builtin", "[model-name]"},
	{"/status", "Show session status", "builtin", ""},
}

var codexBuiltinVersions = []versionedBuiltins{
	{MinVersion: "", Commands: codexBuiltinsBase},
}

func (p *codexProvider) Discover(ctx DiscoverContext) ([]Command, bool) {
	return codexBuiltinsForVersion(ctx.AgentVersion), false
}

func codexBuiltinsForVersion(version string) []Command {
	for _, vb := range codexBuiltinVersions {
		if vb.MinVersion == "" || semverAtLeast(version, vb.MinVersion) {
			return vb.Commands
		}
	}
	return codexBuiltinsBase
}

func semverAtLeast(reported, minimum string) bool {
	r := parseVersionParts(reported)
	m := parseVersionParts(minimum)
	for i := 0; i < 3; i++ {
		if r[i] > m[i] {
			return true
		}
		if r[i] < m[i] {
			return false
		}
	}
	return true
}

func parseVersionParts(v string) [3]int {
	var parts [3]int
	segments := strings.SplitN(strings.TrimSpace(v), ".", 3)
	for i := 0; i < len(segments) && i < 3; i++ {
		num := ""
		for _, ch := range segments[i] {
			if ch >= '0' && ch <= '9' {
				num += string(ch)
			} else {
				break
			}
		}
		if num != "" {
			parts[i], _ = strconv.Atoi(num)
		}
	}
	return parts
}

func init() {
	registerProvider(&codexProvider{})
}
