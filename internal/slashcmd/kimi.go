package slashcmd

import (
	"os"
	"path/filepath"
	"strings"
)

// kimiBuiltins mirrors the primary interactive TUI commands in standalone
// Kimi Code 0.36.1. AgentVersion remains available in DiscoverContext for a
// clean cutover when the command registry changes.
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

// Discover reproduces standalone Kimi Code's native skill roots, verified
// against MoonshotAI/kimi-code's packages/agent-core/src/skill/scanner.ts:
// project .kimi-code and .agents roots, the KIMI_CODE_HOME user root, the
// home .agents root, then additive extra_skill_dirs. Skills are rendered as the
// canonical /skill:<name> commands Kimi registers.
//
// Sources are scanned in descending Kimi precedence - Project > User > Extra -
// with first-wins de-duplication. Scanning the winners first also spends the
// shared file budget on them, so exhaustion truncates the least important
// skills, never the winners.
func (p *kimiProvider) Discover(ctx DiscoverContext) ([]Command, bool) {
	if ctx.SuppressNative {
		builtins := make([]Command, len(kimiBuiltins))
		copy(builtins, kimiBuiltins)
		return builtins, false
	}
	if ctx.CommandFormat != "" {
		// The relay's INI configured this profile explicitly, which outranks
		// discovery.
		custom, truncated := discoverGenericSkills(ctx.SkillDirs, ctx.CommandFormat)
		return builtinsWithCustom(kimiBuiltins, custom), truncated
	}

	configDir := filepath.Join(ctx.Home, ".kimi-code")
	if configured := expandTilde(strings.TrimSpace(os.Getenv("KIMI_CODE_HOME")), ctx.Home); configured != "" &&
		filepath.IsAbs(configured) {
		configDir = configured
	}
	settings := kimiSkillSettings{}
	if filepath.IsAbs(configDir) {
		if data, ok := readSettingsFile(configDir, "config.toml"); ok {
			parseKimiSkillSettings(data, &settings)
		}
	}

	truncated := false
	budget := maxCustomFiles
	active := make(map[string]Command, len(kimiBuiltins))
	order := make([]string, 0, len(kimiBuiltins))
	apply := func(commands []Command) {
		for _, command := range commands {
			key := strings.ToLower(command.Command)
			if _, exists := active[key]; exists {
				continue
			}
			order = append(order, key)
			active[key] = command
		}
	}
	apply(kimiBuiltins)
	seenFiles := make(map[string]bool)
	seenDirs := make(map[string]bool)

	scan := func(scope string, dirs ...string) {
		for _, dir := range dirs {
			if !filepath.IsAbs(dir) {
				// An empty home makes filepath.Join return a relative path.
				// Never scan the headless service's unrelated working directory.
				continue
			}
			cmds, trunc := scanPiSkillTree(dir, scope, nil, true, true, &budget, seenFiles, seenDirs)
			apply(cmds)
			truncated = truncated || trunc
		}
	}

	// Kimi resolves project roots from the nearest .git ancestor, falling back
	// to the working directory itself.
	projectRoot := findGitRoot(ctx.Cwd)
	if projectRoot == "" {
		projectRoot = ctx.Cwd
	}
	if filepath.IsAbs(projectRoot) {
		scan("project",
			filepath.Join(projectRoot, ".kimi-code", "skills"),
			filepath.Join(projectRoot, ".agents", "skills"),
		)
	}

	// KIMI_CODE_HOME relocates only Kimi's brand data. Generic skills remain
	// under the real user home so they can be shared with other agents.
	scan("personal",
		filepath.Join(configDir, "skills"),
		filepath.Join(ctx.Home, ".agents", "skills"),
	)

	// Extra roots are lower precedence than project and user roots. Relative
	// entries resolve from the project root; "~" resolves from the real home.
	for _, dir := range settings.extraSkillDirs {
		dir = expandTilde(dir, ctx.Home)
		if strings.HasPrefix(dir, "~") {
			continue
		}
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
