package slashcmd

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

func (p *kimiProvider) Discover(_ DiscoverContext) ([]Command, bool) {
	commands := make([]Command, len(kimiBuiltins))
	copy(commands, kimiBuiltins)
	return commands, false
}

func init() { registerProvider(&kimiProvider{}) }
