package slashcmd

import (
	"path/filepath"
)

type qoderProvider struct{}

func (p *qoderProvider) ID() string { return "qoder" }

type qoderProjectScope struct {
	commands   []Command
	skills     []Command
	suppressed []string
}

var qoderBuiltins = []Command{
	{"/clear", "Start a fresh conversation", "builtin", ""},
	{"/compact", "Summarize and compact conversation history", "builtin", "[instructions]"},
	{"/copy", "Copy the last assistant response to clipboard, or /copy N for the Nth-latest", "builtin", "[N]"},
	{"/config", "View or modify configuration", "builtin", "[key] [value]"},
	{"/cost", "Show token usage and cost", "builtin", ""},
	{"/help", "Show available commands", "builtin", ""},
	{"/model", "Switch the AI model", "builtin", "[model-name]"},
	{"/permissions", "View or modify permissions", "builtin", ""},
	{"/status", "Show session status", "builtin", ""},
}

func (p *qoderProvider) Discover(ctx DiscoverContext) ([]Command, bool) {
	truncated := false
	budget := maxCustomFiles

	var personalCmds, personalSkills []Command
	var personalSuppressed []string
	var projectScopes []qoderProjectScope

	if ctx.Cwd != "" {
		projectDirs := findProjectDirs(ctx.Cwd, []string{".qoder"})
		for _, dir := range projectDirs {
			cmdDir := filepath.Join(dir, "commands")
			projectCmds, commandSuppressions, trunc := walkCommandDirBudget(cmdDir, "project", &budget)
			truncated = truncated || trunc

			skillDir := filepath.Join(dir, "skills")
			projectSkills, skillSuppressions, trunc := scanSkillDirBudget(skillDir, "project", &budget)
			truncated = truncated || trunc
			projectScopes = append(projectScopes, qoderProjectScope{
				commands:   projectCmds,
				skills:     projectSkills,
				suppressed: append(commandSuppressions, skillSuppressions...),
			})
		}
	}

	personalCmdDir := filepath.Join(ctx.Home, ".qoder", "commands")
	cmds, suppressed, trunc := walkCommandDirBudget(personalCmdDir, "personal", &budget)
	personalCmds = append(personalCmds, cmds...)
	personalSuppressed = append(personalSuppressed, suppressed...)
	truncated = truncated || trunc

	personalSkillDir := filepath.Join(ctx.Home, ".qoder", "skills")
	cmds, suppressed, trunc = scanSkillDirBudget(personalSkillDir, "personal", &budget)
	personalSkills = append(personalSkills, cmds...)
	personalSuppressed = append(personalSuppressed, suppressed...)
	truncated = truncated || trunc

	commands := dedupQoder(
		qoderBuiltins,
		personalCmds,
		personalSkills,
		projectScopes,
		personalSuppressed,
	)

	if budget <= 0 {
		truncated = true
	}
	return commands, truncated
}

// dedupQoder implements Qoder precedence:
//   - Custom commands override builtins with the same name.
//   - Same-name commands from personal and project scopes coexist.
//   - Personal skills override project skills with the same name.
//   - Custom skills override builtins with the same name.
func dedupQoder(
	builtins, personalCmds, personalSkills []Command,
	projectScopes []qoderProjectScope,
	personalSuppressed []string,
) []Command {
	personalSuppressions := commandSet(personalSuppressed)
	projectSuppressions := make(map[string]bool)
	var projectCmds, projectSkills []Command
	for _, scope := range projectScopes {
		scopeSuppressions := commandSet(scope.suppressed)
		projectCmds = filterQoderCommands(projectCmds, scopeSuppressions)
		projectSkills = filterQoderCommands(projectSkills, scopeSuppressions)
		for name := range scopeSuppressions {
			projectSuppressions[name] = true
		}
		for _, cmd := range scope.commands {
			delete(projectSuppressions, cmd.Command)
			projectCmds = append(projectCmds, cmd)
		}
		for _, cmd := range scope.skills {
			delete(projectSuppressions, cmd.Command)
			projectSkills = append(projectSkills, cmd)
		}
	}

	// Collect personal skill names (personal overrides project for skills).
	personalSkillNames := make(map[string]bool, len(personalSkills))
	for _, cmd := range personalSkills {
		personalSkillNames[cmd.Command] = true
	}

	// Collect all custom names to override builtins.
	customNames := make(map[string]bool)
	for _, cmd := range personalCmds {
		customNames[cmd.Command] = true
	}
	for _, cmd := range personalSkills {
		customNames[cmd.Command] = true
	}
	for _, cmd := range projectCmds {
		customNames[cmd.Command] = true
	}
	for _, cmd := range projectSkills {
		customNames[cmd.Command] = true
	}
	for name := range personalSuppressions {
		customNames[name] = true
	}
	for name := range projectSuppressions {
		customNames[name] = true
	}

	var result []Command

	// Builtins: skip if a custom command/skill has the same name.
	for _, cmd := range builtins {
		if !customNames[cmd.Command] {
			result = append(result, cmd)
		}
	}

	// Personal commands (coexist with project commands of same name).
	for _, cmd := range personalCmds {
		if !projectSuppressions[cmd.Command] {
			result = append(result, cmd)
		}
	}

	// Personal skills.
	for _, cmd := range personalSkills {
		if !projectSuppressions[cmd.Command] {
			result = append(result, cmd)
		}
	}

	// Project commands (coexist with personal commands of same name).
	result = append(result, projectCmds...)

	// Project skills: skip if personal has same name.
	for _, cmd := range projectSkills {
		if !personalSkillNames[cmd.Command] {
			result = append(result, cmd)
		}
	}

	return result
}

func commandSet(names []string) map[string]bool {
	result := make(map[string]bool, len(names))
	for _, name := range names {
		result[name] = true
	}
	return result
}

func filterQoderCommands(commands []Command, suppressed map[string]bool) []Command {
	filtered := commands[:0]
	for _, command := range commands {
		if !suppressed[command.Command] {
			filtered = append(filtered, command)
		}
	}
	return filtered
}

func init() {
	registerProvider(&qoderProvider{})
	providers["qodercli"] = &qoderProvider{}
}
