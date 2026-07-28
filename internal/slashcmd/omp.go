package slashcmd

type ompProvider struct{}

func (p *ompProvider) ID() string { return "omp" }

// ompBuiltins mirrors the primary TUI commands in OMP 17.1.7. AgentVersion is
// available in DiscoverContext for adding a clean version cutover when needed.
var ompBuiltins = []Command{
	{"/settings", "Open settings menu", "builtin", ""},
	{"/setup", "Open provider setup", "builtin", "[providers]"},
	{"/plan", "Toggle plan mode", "builtin", "[prompt]"},
	{"/plan-review", "Reopen the latest plan review", "builtin", ""},
	{"/vibe", "Toggle persistent fast-worker mode", "builtin", "[prompt]"},
	{"/goal", "Manage the persistent autonomous goal", "builtin", "[objective]"},
	{"/guided-goal", "Interview and refine a goal before enabling it", "builtin", "[rough objective]"},
	{"/loop", "Repeat the next prompt after every yield", "builtin", "[count|duration] [prompt]"},
	{"/queue", "Queue a message for after the agent yields", "builtin", "<message>"},
	{"/model", "Switch the model for this session", "builtin", "[model]"},
	{"/switch", "Switch the model for this session", "builtin", "[model]"},
	{"/fast", "Toggle priority service tier", "builtin", "[on|off|status]"},
	{"/computer", "Toggle the native computer-use tool", "builtin", "[on|off|status]"},
	{"/vision", "Control the inspect_image delegation tool", "builtin", "[on|off|auto|status]"},
	{"/prewalk", "Switch to a fast model at the next action", "builtin", ""},
	{"/advisor", "Manage the second-model advisor", "builtin", "[on|off|status|dump|configure]"},
	{"/export", "Export the session to HTML", "builtin", "[--themes] [path]"},
	{"/dump", "Copy the transcript and write request JSON", "builtin", ""},
	{"/share", "Share the session through an encrypted link", "builtin", ""},
	{"/collab", "Share this session live through a relay", "builtin", "[start|view|stop|status] [relay URL]"},
	{"/join", "Join a shared collaboration session", "builtin", "<link>"},
	{"/leave", "Leave the collaboration session", "builtin", ""},
	{"/browser", "Toggle browser headless or visible mode", "builtin", "[headless|visible]"},
	{"/copy", "Pick conversation text or code to copy", "builtin", ""},
	{"/todo", "View or modify the agent todo list", "builtin", "<subcommand>"},
	{"/session", "Manage the current session", "builtin", "[info|delete|pin]"},
	{"/jobs", "Show background job status", "builtin", ""},
	{"/usage", "Show provider usage and limits", "builtin", "[show|reset]"},
	{"/stats", "Launch the local statistics dashboard", "builtin", "[--port <port>]"},
	{"/changelog", "Show changelog entries", "builtin", "[full]"},
	{"/hotkeys", "Show all keyboard shortcuts", "builtin", ""},
	{"/tools", "Show tools visible to the agent", "builtin", ""},
	{"/context", "Show estimated context usage", "builtin", ""},
	{"/extensions", "Open the Extension Control Center", "builtin", ""},
	{"/agents", "Open the Agent Control Center", "builtin", ""},
	{"/branch", "Create a branch from a previous message", "builtin", ""},
	{"/fork", "Create a fork from a previous message", "builtin", ""},
	{"/tree", "Navigate the session tree", "builtin", ""},
	{"/login", "Log in with an OAuth provider", "builtin", "[provider|redirect URL]"},
	{"/logout", "Log out from an OAuth provider", "builtin", "[provider]"},
	{"/mcp", "Manage MCP servers", "builtin", "<subcommand>"},
	{"/ssh", "Manage SSH hosts", "builtin", "<subcommand>"},
	{"/new", "Start a new session", "builtin", ""},
	{"/fresh", "Reset provider state without changing the transcript", "builtin", ""},
	{"/drop", "Delete the current session and start a new one", "builtin", ""},
	{"/compact", "Manually compact the session context", "builtin", "[mode] [focus]"},
	{"/shake", "Drop heavy content from context", "builtin", "[elide|images]"},
	{"/handoff", "Hand off context to a new session", "builtin", "[focus instructions]"},
	{"/resume", "Resume a different session", "builtin", "[session ID]"},
	{"/btw", "Ask an ephemeral question using current context", "builtin", "<question>"},
	{"/tan", "Run a background agent on tangential work", "builtin", "<work>"},
	{"/omfg", "Forge a rule from a recurring-behavior complaint", "builtin", "<complaint>"},
	{"/retry", "Retry the last failed agent turn", "builtin", ""},
	{"/debug", "Open the debug tools selector", "builtin", ""},
	{"/memory", "Inspect and maintain memory", "builtin", "<subcommand>"},
	{"/rename", "Rename the current session", "builtin", "<title>"},
	{"/move", "Move the session to another directory", "builtin", "[path]"},
	{"/add-dir", "Add a workspace directory", "builtin", "<path>"},
	{"/remove-dir", "Remove a workspace directory", "builtin", "<path>"},
	{"/dirs", "List workspace directories", "builtin", ""},
	{"/exit", "Exit OMP", "builtin", ""},
	{"/marketplace", "Manage plugin marketplaces", "builtin", "<subcommand>"},
	{"/plugins", "View and manage installed plugins", "builtin", "[list|enable|disable]"},
	{"/reload-plugins", "Reload plugins, skills, commands, hooks, tools, agents, and MCP", "builtin", ""},
	{"/force", "Force the next turn to use a specific tool", "builtin", "<tool-name> [prompt]"},
	{"/live", "Start Codex-backed realtime voice mode", "builtin", ""},
	{"/pause", "Freeze all agents until resumed", "builtin", ""},
	{"/quit", "Quit OMP", "builtin", ""},
}

func (p *ompProvider) Discover(ctx DiscoverContext) ([]Command, bool) {
	return builtinsWithGenericSkills(ompBuiltins, ctx)
}

func init() {
	registerProvider(&ompProvider{})
}
