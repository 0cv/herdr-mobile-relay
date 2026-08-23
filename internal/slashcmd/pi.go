package slashcmd

import (
	"encoding/json"
	"path/filepath"

	"github.com/0cv/herdr-mobile-relay/internal/agentroots"
)

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

// Discover reproduces Pi's own skill resolution: the skill directory of every
// Pi agent profile plus ~/.agents/skills at user scope, and .pi/skills and
// .agents/skills from the git root down to the cwd at project scope, rendered
// as the /skill:<name> commands Pi registers.
//
// Sources are scanned in descending precedence with first-wins dedupe: project
// scope before user scope, so the more specific scope wins a name collision,
// and within each scope the brand directories (.pi, Pi's own agent dirs) before
// the generic .agents ones, matching Pi's own resolution. Scanning the winners
// first also spends the shared file budget on them, so exhaustion truncates the
// least important skills.
//
// Pi's frontmatter disable-model-invocation suppresses model auto-invocation
// only and explicitly keeps /skill:<name> working, so it is not a palette hide
// and nothing here filters on it.
func (p *piProvider) Discover(ctx DiscoverContext) ([]Command, bool) {
	if ctx.CommandFormat != "" {
		// The relay's INI configured this profile explicitly, which outranks
		// discovery.
		custom, truncated := discoverGenericSkills(ctx.SkillDirs, ctx.CommandFormat)
		return builtinsWithCustom(piBuiltins, custom), truncated
	}

	if !piSkillCommandsEnabled(ctx.Home) {
		builtins := make([]Command, len(piBuiltins))
		copy(builtins, piBuiltins)
		return builtins, false
	}

	truncated := false
	budget := maxCustomFiles
	active := make(map[string]Command, len(piBuiltins))
	order := make([]string, 0, len(piBuiltins))
	apply := func(commands []Command) {
		for _, command := range commands {
			if _, exists := active[command.Command]; exists {
				continue
			}
			order = append(order, command.Command)
			active[command.Command] = command
		}
	}
	apply(piBuiltins)

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

	// findProjectDirs lists ancestors outermost first with stems in argument
	// order, so the stems are passed lowest precedence first and the flat list
	// is walked backwards: innermost ancestor first, .pi before .agents within
	// one ancestor.
	projectDirs := findProjectDirs(ctx.Cwd, []string{".agents", ".pi"})
	for i := len(projectDirs) - 1; i >= 0; i-- {
		scan("project", filepath.Join(projectDirs[i], "skills"))
	}

	scan("personal", agentroots.PiSkillDirs(ctx.Home)...)
	scan("personal", filepath.Join(ctx.Home, ".agents", "skills"))

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

// piSkillSettings is the slice of Pi's settings.json the palette depends on. The
// pointer distinguishes an absent key from an explicit false.
type piSkillSettings struct {
	EnableSkillCommands *bool `json:"enableSkillCommands"`
}

// piSkillCommandsEnabled reports whether Pi registers skills as /skill:<name>
// commands. The first settings.json found across Pi's agent directories
// decides; where several profiles exist the relay cannot know which one the
// pane runs, so first-found wins - the search stops at the first directory
// containing a settings.json even when that file cannot be read, because
// falling through would apply an unrelated profile's settings. An absent,
// unreadable or unparsable file leaves Pi's documented default of true, because
// failing open can only show a command the user could have run, never hide one.
func piSkillCommandsEnabled(home string) bool {
	for _, dir := range agentroots.PiConfigDirs(home) {
		data, found, ok := settingsFileIn(dir, "settings.json")
		if !found {
			continue
		}
		if !ok {
			return true
		}
		var settings piSkillSettings
		if err := json.Unmarshal(data, &settings); err != nil {
			return true
		}
		if settings.EnableSkillCommands != nil {
			return *settings.EnableSkillCommands
		}
		return true
	}
	return true
}

// builtinsWithCustom appends custom commands to builtins, keeping the builtin on
// a name collision. Used for the INI-configured escape hatch, where the relay's
// own configuration named the skill directories explicitly.
func builtinsWithCustom(builtins, custom []Command) []Command {
	if len(custom) == 0 {
		return builtins
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
	return commands
}

func init() {
	registerProvider(&piProvider{})
}
