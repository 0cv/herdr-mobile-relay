package slashcmd

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
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

var (
	hermesProfileNamePattern  = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,63}$`)
	hermesSkillInvalidPattern = regexp.MustCompile(`[^a-z0-9-]`)
	hermesSkillHyphenPattern  = regexp.MustCompile(`-{2,}`)
)

func hermesSkillSlug(name string) string {
	name = strings.ReplaceAll(strings.ReplaceAll(strings.ToLower(name), " ", "-"), "_", "-")
	name = hermesSkillInvalidPattern.ReplaceAllString(name, "")
	name = hermesSkillHyphenPattern.ReplaceAllString(name, "-")
	return strings.Trim(name, "-")
}

func parseHermesSkillMetadata(metadata map[string]string, dirName, source string) *Command {
	name := metadata["name"]
	if name == "" {
		name = dirName
	}
	slug := hermesSkillSlug(name)
	if slug == "" || !commandNamePattern.MatchString(slug) || !userInvocable(metadata) {
		return nil
	}
	description := metadata["description"]
	if description == "" {
		description = strings.ToUpper(slug[:1]) + slug[1:] + " skill"
	}
	return &Command{
		Command:      "/" + slug,
		Description:  compact(description, 240),
		Source:       source,
		ArgumentHint: compact(metadata["argument-hint"], 120),
	}
}

func (p *hermesProvider) Discover(ctx DiscoverContext) ([]Command, bool) {
	if ctx.SuppressNative {
		builtins := make([]Command, len(hermesBuiltins))
		copy(builtins, hermesBuiltins)
		return builtins, false
	}

	commands := make([]Command, 0, len(hermesBuiltins))
	commands = append(commands, hermesBuiltins...)
	seen := make(map[string]bool, len(hermesBuiltins))
	for _, cmd := range hermesBuiltins {
		seen[cmd.Command] = true
	}

	truncated := false
	budget := maxWalkFiles

	// Hermes resolves project skills from the nearest trusted Git root, not
	// from the process working directory.
	if ctx.Cwd != "" {
		projectRoot := ctx.Cwd
		if gitRoot := findGitRoot(ctx.Cwd); gitRoot != "" {
			projectRoot = gitRoot
		}
		projectSkills := filepath.Join(projectRoot, ".hermes", "skills")
		cmds, trunc := scanHermesSkillDirBudget(projectSkills, "project", projectRoot, &budget)
		for _, cmd := range cmds {
			if !seen[cmd.Command] {
				seen[cmd.Command] = true
				commands = append(commands, cmd)
			}
		}
		truncated = truncated || trunc
	}

	// Hermes keeps the active profile's skills under its Hermes home. An
	// explicit HERMES_HOME wins; otherwise honor the sticky profile selection.
	if home := hermesHome(ctx); home != "" {
		personalSkills := filepath.Join(home, "skills")
		cmds, trunc := scanHermesSkillDirBudget(personalSkills, "personal", "", &budget)
		for _, cmd := range cmds {
			if !seen[cmd.Command] {
				seen[cmd.Command] = true
				commands = append(commands, cmd)
			}
		}
		truncated = truncated || trunc
	}

	// Additional configured skill dirs from agent-profiles.ini.
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

func hermesHome(ctx DiscoverContext) string {
	envHome := strings.TrimSpace(os.Getenv("HERMES_HOME"))
	if ctx.Home == "" && envHome == "" {
		return ""
	}
	defaultHome := filepath.Join(ctx.Home, ".hermes")
	if envHome != "" {
		envHome = expandTilde(envHome, ctx.Home)
		if filepath.IsAbs(envHome) {
			return filepath.Clean(envHome)
		}
	}
	if ctx.Home == "" || !filepath.IsAbs(defaultHome) {
		return ""
	}

	data, err := os.ReadFile(filepath.Join(defaultHome, "active_profile"))
	if err != nil {
		return defaultHome
	}
	profile := strings.ToLower(strings.TrimSpace(string(data)))
	if profile == "" || profile == "default" || !hermesProfileNamePattern.MatchString(profile) {
		return defaultHome
	}
	profileHome := filepath.Join(defaultHome, "profiles", profile)
	info, err := os.Stat(profileHome)
	if err != nil || !info.IsDir() {
		return defaultHome
	}
	return profileHome
}

func scanHermesSkillDirBudget(root, source, boundary string, budget *int) ([]Command, bool) {
	if root == "" || *budget <= 0 {
		return nil, *budget <= 0
	}
	var commands []Command
	seenFiles := make(map[string]bool)
	seenDirs := make(map[string]bool)
	truncated := false

	var scan func(string)
	scan = func(dir string) {
		if *budget <= 0 {
			truncated = true
			return
		}
		info, err := os.Stat(dir)
		if err != nil || !info.IsDir() {
			return
		}
		realDir := dir
		if resolved, err := filepath.EvalSymlinks(dir); err == nil {
			realDir = resolved
		}
		realDir = filepath.Clean(realDir)
		if boundary != "" && !pathWithin(realDir, boundary) {
			return
		}
		if seenDirs[realDir] {
			return
		}
		seenDirs[realDir] = true

		skillFile := filepath.Join(dir, "SKILL.md")
		if skillInfo, err := os.Stat(skillFile); err == nil && skillInfo.Mode().IsRegular() {
			metadata, resolved, ok := scopedSkillMetadata(
				filepath.Dir(dir), filepath.Base(dir), source, boundary,
			)
			if !ok || seenFiles[resolved] {
				return
			}
			seenFiles[resolved] = true
			*budget--
			if cmd := parseHermesSkillMetadata(metadata, filepath.Base(dir), source); cmd != nil {
				commands = append(commands, *cmd)
			}
			return
		}

		entries, err := os.ReadDir(dir)
		if err != nil {
			return
		}
		for _, entry := range entries {
			if *budget <= 0 {
				truncated = true
				return
			}
			if strings.HasPrefix(entry.Name(), ".") || entry.Name() == "node_modules" {
				continue
			}
			child := filepath.Join(dir, entry.Name())
			if entryIsDir(entry, child) {
				scan(child)
			}
		}
	}

	scan(root)
	return commands, truncated
}

func init() {
	registerProvider(&hermesProvider{})
}
