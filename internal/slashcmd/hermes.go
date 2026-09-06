package slashcmd

import (
	"path/filepath"
)

type hermesProvider struct{}

func (p *hermesProvider) ID() string { return "hermes" }

// hermesBuiltins mirrors commands available in the Hermes Agent CLI.
var hermesBuiltins = []Command{
	{"/model", "Switch model or view active configuration", "builtin", "[model] [--provider name]"},
	{"/usage", "Show session tokens, context window, and costs", "builtin", ""},
	{"/clear", "Clear screen and start a new session", "builtin", ""},
	{"/new", "Start a new session (fresh session ID + history)", "builtin", "[name]"},
	{"/reset", "Reset session history", "builtin", ""},
	{"/sessions", "Browse and resume previous sessions", "builtin", ""},
	{"/resume", "Resume a previously-named session", "builtin", "[name]"},
	{"/skills", "Search, install, inspect, or manage skills", "builtin", "[search|inspect|install]"},
	{"/tools", "Manage tools and view tool definitions", "builtin", "[list|enable|disable]"},
	{"/compress", "Compress conversation context", "builtin", "[focus topic]"},
	{"/branch", "Branch the current session to explore alternatives", "builtin", "[name]"},
	{"/fork", "Fork the current session", "builtin", "[name]"},
	{"/undo", "Back up N user turns and re-prompt", "builtin", "[N]"},
	{"/retry", "Retry the last message (resend to agent)", "builtin", ""},
	{"/status", "Show session, model, token, and context info", "builtin", ""},
	{"/copy", "Copy the last assistant response to clipboard", "builtin", "[number]"},
	{"/fast", "Toggle fast mode / priority processing", "builtin", "[normal|fast|status]"},
	{"/reasoning", "Manage reasoning effort and display", "builtin", "[none|low|medium|high]"},
	{"/yolo", "Toggle YOLO mode (skip dangerous command approvals)", "builtin", ""},
	{"/goal", "Set a standing goal across turns until achieved", "builtin", "[objective]"},
	{"/help", "Show available interactive commands", "builtin", ""},
	{"/exit", "Exit the session", "builtin", ""},
	{"/quit", "Quit Hermes", "builtin", ""},
}

func (p *hermesProvider) Discover(ctx DiscoverContext) ([]Command, bool) {
	commands := make([]Command, 0, len(hermesBuiltins))
	commands = append(commands, hermesBuiltins...)
	seen := make(map[string]bool, len(hermesBuiltins))
	for _, cmd := range hermesBuiltins {
		seen[cmd.Command] = true
	}

	truncated := false
	budget := maxWalkFiles

	// 1. Scan project-level skills: .hermes/skills/
	if ctx.Cwd != "" {
		projectSkills := filepath.Join(ctx.Cwd, ".hermes", "skills")
		cmds, _, trunc := scanSkillDirBudget(projectSkills, "project", &budget)
		for _, cmd := range cmds {
			if !seen[cmd.Command] {
				seen[cmd.Command] = true
				commands = append(commands, cmd)
			}
		}
		truncated = truncated || trunc
	}

	// 2. Scan user-level skills: ~/.hermes/skills/
	if ctx.Home != "" {
		personalSkills := filepath.Join(ctx.Home, ".hermes", "skills")
		cmds, _, trunc := scanSkillDirBudget(personalSkills, "personal", &budget)
		for _, cmd := range cmds {
			if !seen[cmd.Command] {
				seen[cmd.Command] = true
				commands = append(commands, cmd)
			}
		}
		truncated = truncated || trunc
	}

	// 3. Any additional configured skill dirs from agent-profiles.ini
	if len(ctx.SkillDirs) > 0 {
		format := ctx.CommandFormat
		if format == "" {
			format = "/{name}"
		}
		custom, trunc := discoverGenericSkills(ctx.SkillDirs, format)
		for _, cmd := range custom {
			if !seen[cmd.Command] {
				seen[cmd.Command] = true
				commands = append(commands, cmd)
			}
		}
		truncated = truncated || trunc
	}

	return commands, truncated
}

func init() {
	registerProvider(&hermesProvider{})
}
