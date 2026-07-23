package slashcmd

import (
	"encoding/json"
	"os"
	"path/filepath"
)

type claudeProvider struct{}

func (p *claudeProvider) ID() string { return "claude" }

var claudeBuiltins = []Command{
	{"/clear", "Start a fresh conversation", "builtin", ""},
	{"/compact", "Summarize and compact conversation history", "builtin", "[instructions]"},
	{"/config", "View or modify configuration", "builtin", "[key] [value]"},
	{"/cost", "Show token usage and cost", "builtin", ""},
	{"/doctor", "Check system health and configuration", "builtin", ""},
	{"/help", "Show available commands", "builtin", ""},
	{"/init", "Initialize a new CLAUDE.md project file", "builtin", ""},
	{"/login", "Switch Anthropic accounts", "builtin", ""},
	{"/logout", "Sign out of current account", "builtin", ""},
	{"/mcp", "Manage MCP server connections", "builtin", "[subcommand]"},
	{"/memory", "Edit CLAUDE.md memory files", "builtin", "[path]"},
	{"/model", "Switch the AI model", "builtin", "[model-name]"},
	{"/permissions", "View or modify permissions", "builtin", ""},
	{"/pr-comments", "View PR comments", "builtin", "[pr-url]"},
	{"/review", "Review a pull request", "builtin", "[pr-url]"},
	{"/status", "Show session status", "builtin", ""},
	{"/terminal-setup", "Install Shift+Enter key binding", "builtin", ""},
	{"/vim", "Toggle vim mode", "builtin", ""},
}

func (p *claudeProvider) Discover(ctx DiscoverContext) ([]Command, bool) {
	var personal, project []Command
	var suppressed []string
	truncated := false
	budget := maxCustomFiles

	if ctx.Cwd != "" {
		projectDirs := findClaudeProjectDirs(ctx.Cwd)
		for _, dir := range projectDirs {
			cmdDir := filepath.Join(dir, "commands")
			cmds, supp, trunc := walkCommandDirBudget(cmdDir, "project", &budget)
			project = append(project, cmds...)
			suppressed = append(suppressed, supp...)
			truncated = truncated || trunc

			skillDir := filepath.Join(dir, "skills")
			cmds, trunc = scanSkillDirBudget(skillDir, "project", &budget)
			project = append(project, cmds...)
			truncated = truncated || trunc
		}
	}

	personalCmds := filepath.Join(ctx.Home, ".claude", "commands")
	cmds, supp, trunc := walkCommandDirBudget(personalCmds, "personal", &budget)
	personal = append(personal, cmds...)
	suppressed = append(suppressed, supp...)
	truncated = truncated || trunc

	personalSkills := filepath.Join(ctx.Home, ".claude", "skills")
	cmds, trunc = scanSkillDirBudget(personalSkills, "personal", &budget)
	personal = append(personal, cmds...)
	truncated = truncated || trunc

	suppressed = append(suppressed, claudeSkillOverrides(ctx)...)

	commands := make([]Command, 0, len(claudeBuiltins)+len(personal)+len(project))
	commands = append(commands, claudeBuiltins...)
	commands = append(commands, personal...)
	commands = append(commands, project...)

	commands = dedupClaudePrecedence(commands, len(claudeBuiltins), len(personal), suppressed)

	if budget <= 0 {
		truncated = true
	}
	return commands, truncated
}

// claudeSkillOverrides reads skillOverrides from Claude's settings files.
// Entries mapped to "off", "hidden", or "disabled" suppress the matching command.
func claudeSkillOverrides(ctx DiscoverContext) []string {
	var overridden []string
	paths := []string{
		filepath.Join(ctx.Home, ".claude", "settings.json"),
	}
	if ctx.Cwd != "" {
		for _, dir := range findClaudeProjectDirs(ctx.Cwd) {
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
			switch value {
			case "off", "hidden", "disabled":
				if len(name) > 0 && name[0] != '/' {
					name = "/" + name
				}
				overridden = append(overridden, name)
			}
		}
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

// findClaudeProjectDirs returns all .claude directories from git root through cwd.
// Without a git root, only cwd/.claude is checked.
func findClaudeProjectDirs(cwd string) []string {
	var dirs []string
	seen := make(map[string]bool)

	gitRoot := findGitRoot(cwd)
	if gitRoot == "" {
		candidate := filepath.Join(cwd, ".claude")
		if info, err := os.Stat(candidate); err == nil && info.IsDir() {
			dirs = append(dirs, candidate)
		}
		return dirs
	}

	candidate := filepath.Join(gitRoot, ".claude")
	if info, err := os.Stat(candidate); err == nil && info.IsDir() {
		dirs = append(dirs, candidate)
		seen[candidate] = true
	}

	// Walk from cwd upward to git root, collecting .claude dirs.
	var chain []string
	dir := cwd
	for depth := 0; depth < maxGitWalkDepth; depth++ {
		candidate := filepath.Join(dir, ".claude")
		if info, err := os.Stat(candidate); err == nil && info.IsDir() && !seen[candidate] {
			chain = append(chain, candidate)
			seen[candidate] = true
		}
		if dir == gitRoot {
			break
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}
	// Reverse chain so outermost scope comes first (git root direction).
	for i := len(chain) - 1; i >= 0; i-- {
		dirs = append(dirs, chain[i])
	}
	return dirs
}

func init() {
	registerProvider(&claudeProvider{})
}
