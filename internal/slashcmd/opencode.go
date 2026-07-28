package slashcmd

// openCodeBuiltins mirrors commands that are available in the stable OpenCode
// 1.18.5 terminal UI. Conditional commands for experimental workspaces, model
// variants, and multi-organization accounts are intentionally omitted.
var openCodeBuiltins = []Command{
	{"/agents", "Switch agent", "builtin", ""},
	{"/connect", "Connect provider", "builtin", ""},
	{"/debug", "View debug info", "builtin", ""},
	{"/diff", "Open diff viewer", "builtin", ""},
	{"/editor", "Open editor", "builtin", ""},
	{"/exit", "Exit the app", "builtin", ""},
	{"/help", "Help", "builtin", ""},
	{"/init", "Guided AGENTS.md setup", "builtin", "<focus>"},
	{"/mcps", "Toggle MCPs", "builtin", ""},
	{"/models", "Switch model", "builtin", ""},
	{"/move", "Move session to another project directory", "builtin", ""},
	{"/new", "New session", "builtin", ""},
	{"/review", "Review changes", "builtin", "[commit|branch|pr]"},
	{"/sessions", "Switch session", "builtin", ""},
	{"/skills", "Browse skills", "builtin", ""},
	{"/status", "View status", "builtin", ""},
	{"/themes", "Switch theme", "builtin", ""},
}

type openCodeProvider struct{}

func (p *openCodeProvider) ID() string { return "opencode" }

func (p *openCodeProvider) Discover(_ DiscoverContext) ([]Command, bool) {
	commands := make([]Command, len(openCodeBuiltins))
	copy(commands, openCodeBuiltins)
	return commands, false
}

func init() { registerProvider(&openCodeProvider{}) }
