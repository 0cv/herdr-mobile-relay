package slashcmd

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
)

type claudeProvider struct{}

func (p *claudeProvider) ID() string { return "claude" }

var claudeBuiltins = []Command{
	{"/add-dir", "Add another working directory", "builtin", "<path>"},
	{"/agents", "Manage agent configurations", "builtin", ""},
	{"/batch", "Run independent work in parallel worktrees", "builtin", "[task]"},
	{"/background", "Move the current session to the background", "builtin", ""},
	{"/branch", "Fork an earlier conversation", "builtin", "[session]"},
	{"/clear", "Start a fresh conversation", "builtin", ""},
	{"/compact", "Summarize the conversation to free context", "builtin", "[instructions]"},
	{"/copy", "Copy Claude's last response to clipboard (or /copy N for the Nth-latest)", "builtin", "[N]"},
	{"/config", "Open Claude Code settings", "builtin", ""},
	{"/context", "Show context-window usage", "builtin", ""},
	{"/debug", "Troubleshoot the current Claude Code session", "builtin", "[description]"},
	{"/diff", "Show changes in the working tree", "builtin", ""},
	{"/doctor", "Check the Claude Code installation", "builtin", ""},
	{"/effort", "Change the reasoning effort", "builtin", ""},
	{"/exit", "Exit Claude Code", "builtin", ""},
	{"/export", "Export the current conversation", "builtin", "[path]"},
	{"/extra-usage", "Configure extra usage", "builtin", ""},
	{"/feedback", "Report an issue with session context", "builtin", ""},
	{"/fork", "Fork the current conversation", "builtin", ""},
	{"/goal", "Set or clear a persistent goal", "builtin", "[condition|clear]"},
	{"/help", "Show help and available commands", "builtin", ""},
	{"/hooks", "View hook configuration", "builtin", ""},
	{"/ide", "Manage IDE integrations", "builtin", ""},
	{"/init", "Create a CLAUDE.md project guide", "builtin", ""},
	{"/insights", "Analyze Claude Code session patterns", "builtin", ""},
	{"/login", "Sign in to Claude Code", "builtin", ""},
	{"/logout", "Sign out of Claude Code", "builtin", ""},
	{"/mcp", "Manage MCP servers", "builtin", ""},
	{"/memory", "View or edit project memory", "builtin", ""},
	{"/mobile", "Show the Claude mobile app QR code", "builtin", ""},
	{"/model", "Choose the active Claude model", "builtin", ""},
	{"/permissions", "View or change permission rules", "builtin", ""},
	{"/plan", "Enter plan mode", "builtin", "[planning prompt]"},
	{"/plugin", "Browse and manage plugins", "builtin", ""},
	{"/reload-plugins", "Reload installed plugins", "builtin", ""},
	{"/remote-control", "Continue this session from another device", "builtin", "[name]"},
	{"/rename", "Rename the current session", "builtin", "[name]"},
	{"/resume", "Resume a saved conversation", "builtin", "[session]"},
	{"/review", "Review a pull request", "builtin", "[PR]"},
	{"/rewind", "Return to an earlier checkpoint", "builtin", ""},
	{"/security-review", "Review the current branch for security issues", "builtin", ""},
	{"/simplify", "Review recent changes for reusable improvements", "builtin", ""},
	{"/skills", "Browse available skills", "builtin", ""},
	{"/stats", "Show account usage statistics", "builtin", ""},
	{"/status", "Show version, model, account, and connectivity", "builtin", ""},
	{"/tasks", "Show background tasks", "builtin", ""},
	{"/teleport", "Pull a web session into this terminal", "builtin", "[session]"},
	{"/theme", "Choose the terminal theme", "builtin", ""},
	{"/usage", "Show plan and usage information", "builtin", ""},
	{"/verify", "Build and observe the application to verify changes", "builtin", "[instructions]"},
	{"/voice", "Configure voice dictation", "builtin", "[hold|tap|off]"},
}

func (p *claudeProvider) Discover(ctx DiscoverContext) ([]Command, bool) {
	truncated := false
	budget := maxCustomFiles
	active := make(map[string]Command, len(claudeBuiltins))
	order := make([]string, 0, len(claudeBuiltins))
	apply := func(commands []Command, suppressed []string) {
		for _, name := range suppressed {
			if _, exists := active[name]; exists {
				delete(active, name)
				order = removeCommandName(order, name)
			}
		}
		for _, command := range commands {
			if _, exists := active[command.Command]; !exists {
				order = append(order, command.Command)
			}
			active[command.Command] = command
		}
	}
	apply(claudeBuiltins, nil)

	if ctx.Cwd != "" {
		projectDirs := findProjectDirs(ctx.Cwd, []string{".claude"})
		for _, dir := range projectDirs {
			cmdDir := filepath.Join(dir, "commands")
			cmds, supp, trunc := walkCommandDirBudget(cmdDir, "project", &budget)
			apply(cmds, supp)
			truncated = truncated || trunc

			skillDir := filepath.Join(dir, "skills")
			cmds, supp, trunc = scanSkillDirBudget(skillDir, "project", &budget)
			apply(cmds, supp)
			truncated = truncated || trunc
		}
	}

	personalCmds := filepath.Join(ctx.Home, ".claude", "commands")
	cmds, supp, trunc := walkCommandDirBudget(personalCmds, "personal", &budget)
	apply(cmds, supp)
	truncated = truncated || trunc

	personalSkills := filepath.Join(ctx.Home, ".claude", "skills")
	cmds, supp, trunc = scanSkillDirBudget(personalSkills, "personal", &budget)
	apply(cmds, supp)
	truncated = truncated || trunc

	apply(nil, claudeSkillOverrides(ctx))
	commands := make([]Command, 0, len(order))
	for _, name := range order {
		if command, exists := active[name]; exists {
			commands = append(commands, command)
		}
	}

	if budget <= 0 {
		truncated = true
	}
	return commands, truncated
}

func removeCommandName(values []string, name string) []string {
	for index, value := range values {
		if value == name {
			return append(values[:index], values[index+1:]...)
		}
	}
	return values
}

// claudeSkillOverrides reads skillOverrides from Claude's settings files.
// Entries mapped to "off", "hidden", or "disabled" suppress the matching command.
func claudeSkillOverrides(ctx DiscoverContext) []string {
	values := make(map[string]string)
	paths := []string{
		filepath.Join(ctx.Home, ".claude", "settings.json"),
	}
	if ctx.Cwd != "" {
		for _, dir := range findProjectDirs(ctx.Cwd, []string{".claude"}) {
			paths = append(paths,
				filepath.Join(dir, "settings.json"),
				filepath.Join(dir, "settings.local.json"),
			)
		}
	}
	for _, path := range paths {
		data, err := os.ReadFile(path)
		if err != nil || len(data) > maxMetadataSize {
			continue
		}
		var settings struct {
			SkillOverrides map[string]string `json:"skillOverrides"`
		}
		if json.Unmarshal(data, &settings) != nil {
			continue
		}
		for name, value := range settings.SkillOverrides {
			values[name] = value
		}
	}
	var overridden []string
	for name, value := range values {
		if !strings.EqualFold(strings.TrimSpace(value), "off") {
			continue
		}
		if len(name) > 0 && name[0] != '/' {
			name = "/" + name
		}
		overridden = append(overridden, name)
	}
	return overridden
}

// dedupClaudePrecedence implements Python's precedence:
// - personal overrides project (same name)
// - nearest project overrides outer project (last in slice wins)
// - any custom overrides builtin
// - suppressed names (user-invocable: false) remove matching entries entirely
func dedupClaudePrecedence(commands []Command, builtinCount, personalCount int, suppressed []string) []Command {
	suppressedSet := make(map[string]bool, len(suppressed))
	for _, name := range suppressed {
		suppressedSet[name] = true
	}

	personalEnd := builtinCount + personalCount

	// Determine the winner for each command name.
	// Priority: personal > nearest project (last) > outer project > builtin.
	winners := make(map[string]Command, len(commands))
	for i, cmd := range commands {
		if suppressedSet[cmd.Command] {
			continue
		}
		existing, seen := winners[cmd.Command]
		if !seen {
			winners[cmd.Command] = cmd
			continue
		}
		isPersonal := i >= builtinCount && i < personalEnd
		isProject := i >= personalEnd
		existingIsBuiltin := existing.Source == "builtin"
		existingIsPersonal := existing.Source == "personal"

		if isPersonal {
			// Personal always wins over builtin and project.
			winners[cmd.Command] = cmd
		} else if isProject {
			if existingIsBuiltin || existingIsPersonal == false && existing.Source == "project" {
				// Nearest project (later in slice) wins over outer project and builtin.
				winners[cmd.Command] = cmd
			}
			// Project does NOT win over personal.
			_ = existingIsPersonal
		}
	}

	// Remove suppressed names entirely.
	for name := range suppressedSet {
		delete(winners, name)
	}

	// Emit in wire order, skipping duplicates and suppressed entries.
	seen := make(map[string]bool, len(winners))
	result := make([]Command, 0, len(winners))
	for _, cmd := range commands {
		if suppressedSet[cmd.Command] {
			continue
		}
		if seen[cmd.Command] {
			continue
		}
		seen[cmd.Command] = true
		if winner, ok := winners[cmd.Command]; ok {
			result = append(result, winner)
		}
	}
	return result
}

func init() {
	registerProvider(&claudeProvider{})
}
