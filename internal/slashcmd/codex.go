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
	{"/permissions", "Change approval and sandbox permissions", "builtin", ""},
	{"/ide", "Include available IDE context in the next prompt", "builtin", "[instructions]"},
	{"/keymap", "View or change terminal keyboard shortcuts", "builtin", ""},
	{"/vim", "Toggle Vim editing mode", "builtin", ""},
	{"/agent", "Switch to another agent thread", "builtin", ""},
	{"/subagents", "Switch to another agent thread", "builtin", ""},
	{"/apps", "Browse available apps and connectors", "builtin", ""},
	{"/plugins", "Browse and manage plugins", "builtin", ""},
	{"/hooks", "View and manage lifecycle hooks", "builtin", ""},
	{"/clear", "Clear the terminal and start a new task", "builtin", ""},
	{"/rename", "Rename the current task", "builtin", "[name]"},
	{"/archive", "Archive the current session and exit", "builtin", ""},
	{"/delete", "Permanently delete the current session and exit", "builtin", ""},
	{"/compact", "Summarize the conversation to free context", "builtin", ""},
	{"/copy", "Copy the latest completed response", "builtin", ""},
	{"/diff", "Show the current Git diff", "builtin", ""},
	{"/exit", "Exit Codex", "builtin", ""},
	{"/quit", "Exit Codex", "builtin", ""},
	{"/experimental", "Configure experimental features", "builtin", ""},
	{"/approve", "Retry a recent automatic-review denial", "builtin", ""},
	{"/memories", "Configure memory use and generation", "builtin", ""},
	{"/skills", "Browse and use available skills", "builtin", ""},
	{"/import", "Import supported Claude Code configuration", "builtin", ""},
	{"/feedback", "Send feedback and optional diagnostics", "builtin", ""},
	{"/init", "Create an AGENTS.md scaffold", "builtin", ""},
	{"/logout", "Sign out of Codex", "builtin", ""},
	{"/mcp", "Show configured MCP servers and tools", "builtin", "[verbose]"},
	{"/mention", "Attach a file or folder", "builtin", "[path]"},
	{"/model", "Choose the active model and reasoning effort", "builtin", ""},
	{"/fast", "Toggle the Fast service tier when available", "builtin", ""},
	{"/plan", "Switch to plan mode", "builtin", "[planning prompt]"},
	{"/goal", "Set or manage a persistent task goal", "builtin", "[objective|edit|pause|resume|clear]"},
	{"/personality", "Choose the response style", "builtin", ""},
	{"/ps", "Show background terminals", "builtin", ""},
	{"/stop", "Stop all background terminals", "builtin", ""},
	{"/fork", "Fork the current task", "builtin", ""},
	{"/side", "Start a temporary side conversation", "builtin", "[question]"},
	{"/btw", "Start a temporary side conversation", "builtin", "[question]"},
	{"/raw", "Toggle raw scrollback mode", "builtin", "[on|off]"},
	{"/resume", "Resume a saved conversation", "builtin", "[session]"},
	{"/new", "Start a new task", "builtin", ""},
	{"/review", "Review the working tree", "builtin", "[instructions]"},
	{"/status", "Show session configuration and context usage", "builtin", ""},
	{"/usage", "Show account token usage", "builtin", "[daily|weekly|cumulative]"},
	{"/debug-config", "Show configuration layer diagnostics", "builtin", ""},
	{"/statusline", "Configure terminal status-line fields", "builtin", ""},
	{"/title", "Configure the terminal title", "builtin", ""},
	{"/theme", "Choose a syntax-highlighting theme", "builtin", ""},
	{"/pets", "Choose or hide a terminal pet", "builtin", ""},
	{"/pet", "Choose or hide a terminal pet", "builtin", ""},
}

var codexBuiltinVersions = []versionedBuiltins{
	{MinVersion: "", Commands: codexBuiltinsBase},
}

func (p *codexProvider) Discover(ctx DiscoverContext) ([]Command, bool) {
	return codexBuiltinsForVersion(ctx.AgentVersion), false
}

func codexBuiltinsForVersion(version string) []Command {
	var selected []Command
	var selectedMinimum string
	for _, vb := range codexBuiltinVersions {
		if vb.MinVersion == "" {
			if selected == nil {
				selected = vb.Commands
			}
			continue
		}
		if semverAtLeast(version, vb.MinVersion) &&
			(selectedMinimum == "" || semverAtLeast(vb.MinVersion, selectedMinimum)) {
			selected = vb.Commands
			selectedMinimum = vb.MinVersion
		}
	}
	if selected != nil {
		return selected
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
