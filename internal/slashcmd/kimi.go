package slashcmd

import (
	"os"
	"path/filepath"
)

// kimiBuiltins mirrors the primary interactive TUI commands in Kimi Code
// 0.29.2. AgentVersion is available in DiscoverContext for a clean version
// cutover when the command registry changes.
var kimiBuiltins = []Command{
	{"/yolo", "Toggle YOLO mode: auto-approve tool actions, but the agent may still ask questions", "builtin", ""},
	{"/auto", "Toggle Auto mode: fully autonomous, agent decides everything without asking", "builtin", ""},
	{"/permission", "Select permission mode", "builtin", ""},
	{"/settings", "Open TUI settings", "builtin", ""},
	{"/plan", "Toggle plan mode", "builtin", ""},
	{"/swarm", "Toggle swarm mode or run one task in swarm mode", "builtin", "[on|off] | <task>"},
	{"/model", "Switch LLM model", "builtin", ""},
	{"/effort", "Switch thinking effort", "builtin", ""},
	{"/provider", "Manage AI providers (add / delete / refresh)", "builtin", ""},
	{"/btw", "Ask a forked side agent a question", "builtin", ""},
	{"/help", "Show available commands and shortcuts", "builtin", ""},
	{"/new", "Start a fresh session in the current workspace", "builtin", ""},
	{"/sessions", "Browse and resume sessions", "builtin", ""},
	{"/tasks", "Browse background tasks", "builtin", ""},
	{"/mcp", "Show MCP server status", "builtin", ""},
	{"/plugins", "Manage plugins", "builtin", ""},
	{"/add-dir", "Add or list an additional workspace directory", "builtin", "[list] | <path>"},
	{"/experiments", "Manage experimental features", "builtin", ""},
	{"/reload", "Reload session and apply config.toml settings plus tui.toml UI preferences", "builtin", ""},
	{"/reload-tui", "Reload only tui.toml UI preferences", "builtin", ""},
	{"/compact", "Compact the conversation context", "builtin", "<instruction>"},
	{"/goal", "Start or manage an autonomous goal", "builtin", "[status|pause|resume|cancel|replace|next] | <objective>"},
	{"/init", "Analyze the codebase and generate AGENTS.md", "builtin", ""},
	{"/fork", "Fork the current session", "builtin", ""},
	{"/title", "Set or show session title", "builtin", "<title>"},
	{"/usage", "Show session tokens, context window, and plan quotas", "builtin", ""},
	{"/status", "Show current session and runtime status", "builtin", ""},
	{"/feedback", "Send feedback to make Kimi Code better", "builtin", ""},
	{"/undo", "Withdraw the last prompt from the transcript", "builtin", ""},
	{"/editor", "Set the external editor for Ctrl-G", "builtin", ""},
	{"/theme", "Set the terminal UI theme", "builtin", ""},
	{"/logout", "Log out of a configured provider", "builtin", ""},
	{"/login", "Select a platform and authenticate", "builtin", ""},
	{"/export-md", "Export current session as a Markdown file", "builtin", ""},
	{"/export-debug-zip", "Export current session as a debug ZIP archive", "builtin", ""},
	{"/copy", "Copy the last assistant message to the clipboard", "builtin", ""},
	{"/web", "Open the current session in the Web UI by starting a new server", "builtin", ""},
	{"/exit", "Exit the application", "builtin", ""},
	{"/version", "Show version information", "builtin", ""},
}

type kimiProvider struct{}

func (p *kimiProvider) ID() string { return "kimi" }

// Discover reproduces Kimi Code's own skill resolution, verified against
// MoonshotAI/kimi-cli docs/en/customization/skills.md: the brand and generic
// candidate groups at user and project scope plus the additive
// extra_skill_dirs, rendered as the /skill:<name> commands Kimi registers. Kimi
// also accepts a /<name> shorthand for the same skill; only the canonical form
// is listed.
//
// Sources are scanned in descending Kimi precedence - Project > User > Extra -
// with first-wins dedupe, and within one scope the brand group before the
// generic group, because a name defined in both groups resolves to the brand
// copy. Scanning the winners first also spends the shared file budget on them,
// so exhaustion truncates the least important skills, never the winners.
//
// merge_all_available_skills governs the brand group only: the generic group is
// always mutually exclusive, taking its first existing candidate.
func (p *kimiProvider) Discover(ctx DiscoverContext) ([]Command, bool) {
	if ctx.CommandFormat != "" {
		// The relay's INI configured this profile explicitly, which outranks
		// discovery.
		custom, truncated := discoverGenericSkills(ctx.SkillDirs, ctx.CommandFormat)
		return builtinsWithCustom(kimiBuiltins, custom), truncated
	}

	settings := defaultKimiSkillSettings()
	if data, ok := readSettingsFile(filepath.Join(ctx.Home, ".kimi"), "config.toml"); ok {
		parseKimiSkillSettings(data, &settings)
	}

	truncated := false
	budget := maxCustomFiles
	active := make(map[string]Command, len(kimiBuiltins))
	order := make([]string, 0, len(kimiBuiltins))
	apply := func(commands []Command) {
		for _, command := range commands {
			if _, exists := active[command.Command]; exists {
				continue
			}
			order = append(order, command.Command)
			active[command.Command] = command
		}
	}
	apply(kimiBuiltins)

	scan := func(scope string, dirs ...string) {
		for _, dir := range dirs {
			if dir == "" {
				continue
			}
			cmds, trunc := scanSkillDirFormat(dir, scope, ompCommandFormat, &budget)
			apply(cmds)
			truncated = truncated || trunc
		}
	}
	// firstExisting applies only the first candidate directory that exists. This
	// is what a mutually exclusive group does: always for the generic group, and
	// for the brand group when merge_all_available_skills is false.
	firstExisting := func(scope string, candidates []string) {
		for _, dir := range candidates {
			if info, err := os.Stat(dir); err == nil && info.IsDir() {
				scan(scope, dir)
				return
			}
		}
	}
	// brandGroup applies a candidate group in Kimi's documented priority order,
	// highest first: under first-wins the highest-priority directory registers
	// a name first, and the budget is spent there first.
	brandGroup := func(scope string, candidates []string) {
		if !settings.mergeAllAvailableSkills {
			firstExisting(scope, candidates)
			return
		}
		for _, dir := range candidates {
			scan(scope, dir)
		}
	}

	// projectRoot is Kimi's project scope: the nearest .git ancestor of the work
	// directory, falling back to the work directory itself. Kimi resolves every
	// project candidate against it, so an intermediate ancestor between the work
	// directory and the project root contributes nothing.
	projectRoot := findGitRoot(ctx.Cwd)
	if projectRoot == "" {
		projectRoot = ctx.Cwd
	}
	projectSkillDirs := func(stems ...string) []string {
		if projectRoot == "" {
			return nil
		}
		dirs := make([]string, 0, len(stems))
		for _, stem := range stems {
			dirs = append(dirs, filepath.Join(projectRoot, stem, "skills"))
		}
		return dirs
	}

	// 1. project brand group (kimi > claude > codex), then 2. project generic
	// group.
	brandGroup("project", projectSkillDirs(".kimi", ".claude", ".codex"))
	firstExisting("project", projectSkillDirs(".agents"))

	// 3. user brand group, then 4. user generic group.
	brandGroup("personal", []string{
		filepath.Join(ctx.Home, ".kimi", "skills"),
		filepath.Join(ctx.Home, ".claude", "skills"),
		filepath.Join(ctx.Home, ".codex", "skills"),
	})
	firstExisting("personal", []string{
		filepath.Join(ctx.Home, ".config", "agents", "skills"),
		filepath.Join(ctx.Home, ".agents", "skills"),
	})

	// 5. extra_skill_dirs are additive user scope, scanned last: a brand or
	// generic copy of the same name wins the collision. "~" resolves against
	// home and a relative entry against the project root, as Kimi documents.
	for _, dir := range settings.extraSkillDirs {
		dir = expandTilde(dir, ctx.Home)
		if !filepath.IsAbs(dir) {
			if projectRoot == "" {
				continue
			}
			dir = filepath.Join(projectRoot, dir)
		}
		scan("personal", dir)
	}

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

func init() { registerProvider(&kimiProvider{}) }
