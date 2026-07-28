package slashcmd

type piProvider struct{}

func (p *piProvider) ID() string { return "pi" }

// piBuiltins mirrors the primary interactive commands in Pi 0.82.1. Keep this
// list version-aware if a future Pi release removes or renames a command.
var piBuiltins = []Command{
	{"/settings", "Open settings menu", "builtin", ""},
	{"/model", "Select the active model", "builtin", "<provider/model>"},
	{"/scoped-models", "Choose models for keyboard cycling", "builtin", ""},
	{"/export", "Export the current session", "builtin", "[file]"},
	{"/import", "Import and resume a JSONL session", "builtin", "<file>"},
	{"/share", "Share the session as a secret GitHub gist", "builtin", ""},
	{"/copy", "Copy the last agent message to the clipboard", "builtin", ""},
	{"/name", "Set the session display name", "builtin", "<name>"},
	{"/session", "Show session information and statistics", "builtin", ""},
	{"/changelog", "Show changelog entries", "builtin", ""},
	{"/hotkeys", "Show all keyboard shortcuts", "builtin", ""},
	{"/fork", "Create a fork from a previous user message", "builtin", ""},
	{"/clone", "Duplicate the current session at its current position", "builtin", ""},
	{"/tree", "Navigate the session tree", "builtin", ""},
	{"/trust", "Save the project trust decision for future sessions", "builtin", ""},
	{"/login", "Configure provider authentication", "builtin", "[provider]"},
	{"/logout", "Remove provider authentication", "builtin", "[provider]"},
	{"/new", "Start a new session", "builtin", ""},
	{"/compact", "Manually compact the session context", "builtin", "[instructions]"},
	{"/resume", "Resume a different session", "builtin", "[session]"},
	{"/reload", "Reload keybindings, extensions, skills, prompts, themes, and context files", "builtin", ""},
	{"/quit", "Quit Pi", "builtin", ""},
}

func (p *piProvider) Discover(ctx DiscoverContext) ([]Command, bool) {
	return builtinsWithGenericSkills(piBuiltins, ctx)
}

func builtinsWithGenericSkills(builtins []Command, ctx DiscoverContext) ([]Command, bool) {
	custom, truncated := discoverGenericSkills(ctx.SkillDirs, ctx.CommandFormat)
	if len(custom) == 0 {
		return builtins, truncated
	}

	commands := make([]Command, 0, len(builtins)+len(custom))
	commands = append(commands, builtins...)
	seen := make(map[string]bool, len(builtins)+len(custom))
	for _, command := range builtins {
		seen[command.Command] = true
	}
	for _, command := range custom {
		if seen[command.Command] {
			continue
		}
		seen[command.Command] = true
		commands = append(commands, command)
	}
	return commands, truncated
}

func init() {
	registerProvider(&piProvider{})
}
