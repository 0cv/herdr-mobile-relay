package slashcmd

import (
	"os"
	"path/filepath"
)

type qoderProvider struct{}

func (p *qoderProvider) ID() string { return "qoder" }

var qoderBuiltins = []Command{
	{"/clear", "Start a fresh conversation", "builtin", ""},
	{"/compact", "Summarize and compact conversation history", "builtin", "[instructions]"},
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

	var personalCmds, personalSkills, projectCmds, projectSkills []Command
	var personalSuppressed, projectSuppressed []string

	if ctx.Cwd != "" {
		projectDirs := findQoderProjectDirs(ctx.Cwd)
		for _, dir := range projectDirs {
			cmdDir := filepath.Join(dir, "commands")
			cmds, suppressed, trunc := walkCommandDirBudget(cmdDir, "project", &budget)
			projectCmds = append(projectCmds, cmds...)
			projectSuppressed = append(projectSuppressed, suppressed...)
			truncated = truncated || trunc

			skillDir := filepath.Join(dir, "skills")
			cmds, suppressed, trunc = scanSkillDirBudget(skillDir, "project", &budget)
			projectSkills = append(projectSkills, cmds...)
			projectSuppressed = append(projectSuppressed, suppressed...)
			truncated = truncated || trunc
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
		projectCmds,
		projectSkills,
		personalSuppressed,
		projectSuppressed,
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
	builtins, personalCmds, personalSkills, projectCmds, projectSkills []Command,
	personalSuppressed, projectSuppressed []string,
) []Command {
	personalSuppressions := commandSet(personalSuppressed)
	projectSuppressions := commandSet(projectSuppressed)

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

// findQoderProjectDirs returns all .qoder directories from git root through cwd.
// Without a git root, only cwd/.qoder is checked.
func findQoderProjectDirs(cwd string) []string {
	var dirs []string
	seen := make(map[string]bool)

	gitRoot := findGitRoot(cwd)
	if gitRoot == "" {
		candidate := filepath.Join(cwd, ".qoder")
		if info, err := os.Stat(candidate); err == nil && info.IsDir() {
			dirs = append(dirs, candidate)
		}
		return dirs
	}

	candidate := filepath.Join(gitRoot, ".qoder")
	if info, err := os.Stat(candidate); err == nil && info.IsDir() {
		dirs = append(dirs, candidate)
		seen[candidate] = true
	}

	var chain []string
	dir := cwd
	for depth := 0; depth < maxGitWalkDepth; depth++ {
		candidate := filepath.Join(dir, ".qoder")
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
	for i := len(chain) - 1; i >= 0; i-- {
		dirs = append(dirs, chain[i])
	}
	return dirs
}

func init() {
	registerProvider(&qoderProvider{})
	providers["qodercli"] = &qoderProvider{}
}
