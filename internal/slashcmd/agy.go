package slashcmd

import (
	"os"
	"path/filepath"
)

type agyProvider struct{}

func (p *agyProvider) ID() string { return "agy" }

// agyBuiltins mirrors the commands Antigravity CLI 1.4.7 lists under
// /help > commands before any user skill or workflow. Where the CLI shows only
// the command name and a truncated description, the description here is a
// short neutral summary.
var agyBuiltins = []Command{
	{"/add-dir", "Add a directory to the workspace", "builtin", "<path>"},
	{"/agents", "List available agents", "builtin", ""},
	{"/artifact", "View an artifact", "builtin", ""},
	{"/btw", "Ask a side question without interrupting the current turn", "builtin", "<question>"},
	{"/changelog", "Show release notes and changes", "builtin", ""},
	{"/clear", "Clear the conversation and start a new one", "builtin", ""},
	{"/new", "Clear the conversation and start a new one", "builtin", ""},
	{"/codesearch", "Search the codebase", "builtin", "<query>"},
	{"/cs", "Search the codebase", "builtin", "<query>"},
	{"/search", "Search the codebase", "builtin", "<query>"},
	{"/config", "Open settings", "builtin", ""},
	{"/settings", "Open settings", "builtin", ""},
	{"/context", "Visualize context window usage", "builtin", ""},
	{"/copy", "Copy the most recent response", "builtin", ""},
	{"/credits", "Show remaining credits and purchase link", "builtin", ""},
	{"/diff", "View uncommitted changes and per-turn diffs", "builtin", ""},
	{"/effort", "Set the reasoning effort", "builtin", "[low|medium|high|xhigh|max]"},
	{"/exit", "Exit Antigravity CLI", "builtin", ""},
	{"/quit", "Exit Antigravity CLI", "builtin", ""},
	{"/feedback", "Submit feedback", "builtin", ""},
	{"/fork", "Create a branch of the current conversation", "builtin", ""},
	{"/branch", "Create a branch of the current conversation", "builtin", ""},
	{"/help", "Show help and available commands", "builtin", ""},
	{"/hooks", "Manage hooks", "builtin", ""},
	{"/keybindings", "Set custom keybindings", "builtin", ""},
	{"/logout", "Log out", "builtin", ""},
	{"/mcp", "Manage MCP servers", "builtin", ""},
	{"/model", "Set the model", "builtin", "[model]"},
	{"/open", "Open a file or folder", "builtin", "<path>"},
	{"/permissions", "Manage permissions", "builtin", ""},
	{"/plugin", "Manage plugins", "builtin", ""},
	{"/remote-control", "Turn remote control on or off", "builtin", ""},
	{"/rename", "Rename the conversation", "builtin", "<title>"},
	{"/resume", "Browse and resume previous conversations", "builtin", ""},
	{"/switch", "Browse and resume previous conversations", "builtin", ""},
	{"/rewind", "Rewind the conversation to an earlier turn", "builtin", ""},
	{"/undo", "Rewind the conversation to an earlier turn", "builtin", ""},
	{"/skills", "List available skills", "builtin", ""},
	{"/statusline", "Toggle the statusline", "builtin", ""},
	{"/tasks", "View background tasks", "builtin", ""},
	{"/title", "Toggle the terminal title", "builtin", ""},
	{"/usage", "View model usage and quota", "builtin", ""},
	{"/quota", "View model usage and quota", "builtin", ""},
	{"/voice", "Dictate a prompt by voice", "builtin", ""},
	{"/record", "Dictate a prompt by voice", "builtin", ""},
	{"/goal", "Run until a goal is met", "builtin", "<goal>"},
	{"/compact", "Compact the conversation context", "builtin", ""},
	{"/schedule", "Run a prompt on a schedule", "builtin", "<prompt>"},
	{"/plan", "Plan changes before editing", "builtin", "<task>"},
}

// agySkillRoots are the user directories Antigravity CLI scans for
// SKILL.md skills, relative to the home directory.
var agySkillRoots = []string{filepath.Join(".gemini", "skills")}

func (p *agyProvider) Discover(ctx DiscoverContext) ([]Command, bool) {
	reader := newCatalogReader()
	commands := make([]Command, len(agyBuiltins))
	copy(commands, agyBuiltins)
	if ctx.SuppressNative {
		return commands, reader.truncated
	}
	seen := make(map[string]bool, len(commands))
	for _, command := range commands {
		seen[command.Command] = true
	}

	var dirs []string
	for _, root := range agySkillRoots {
		if ctx.Home == "" {
			break
		}
		dir := filepath.Join(ctx.Home, root)
		if info, err := os.Stat(dir); err == nil && info.IsDir() {
			dirs = append(dirs, dir)
		}
	}
	dirs = append(dirs, ctx.SkillDirs...)
	if len(dirs) == 0 {
		return commands, reader.truncated
	}
	format := ctx.CommandFormat
	if format == "" {
		format = "/{name}"
	}
	custom, truncated := reader.discoverGenericSkills(dirs, format)
	for _, command := range custom {
		if !seen[command.Command] {
			seen[command.Command] = true
			commands = append(commands, command)
		}
	}
	return commands, truncated
}

func init() {
	registerProvider(&agyProvider{})
}
