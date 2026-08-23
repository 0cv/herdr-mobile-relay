package slashcmd

import (
	"os"
	"path/filepath"
	"strings"

	"github.com/0cv/herdr-mobile-relay/internal/agentroots"
)

// ompCommandFormat is the form Oh My Pi registers skills under. Verified against
// the omp 18.0.3 bundle, which builds `skill:${e.name}` and parses input with
// `t.startsWith("/skill:")`.
const ompCommandFormat = "skill:{name}"

// maxSettingsSize caps an agent settings file the relay parses. Generous enough
// that a real config.yml is never skipped, bounded so a runaway file is.
const maxSettingsSize = 256 * 1024

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

// Discover reproduces Oh My Pi's own skill resolution: every provider omp
// registers against the skills capability, gated by the toggles in omp's own
// config.yml, rendered as the /skill:<name> commands omp registers.
//
// Sources are scanned in descending omp priority - project scope before user
// scope within one source, and the innermost ancestor first - with first-wins
// dedupe, matching omp's own resolution directly. Scanning the winners first
// also spends the shared file budget on them, so exhaustion truncates the least
// important skills, never the highest-priority ones.
func (p *ompProvider) Discover(ctx DiscoverContext) ([]Command, bool) {
	if ctx.CommandFormat != "" {
		// The relay's INI configured this profile explicitly, which outranks
		// discovery.
		custom, truncated := discoverGenericSkills(ctx.SkillDirs, ctx.CommandFormat)
		return builtinsWithCustom(ompBuiltins, custom), truncated
	}

	settings := loadOMPSkillSettings(ctx)
	if !settings.enabled || !settings.enableSkillCommands {
		builtins := make([]Command, len(ompBuiltins))
		copy(builtins, ompBuiltins)
		return builtins, false
	}

	truncated := false
	budget := maxCustomFiles
	active := make(map[string]Command, len(ompBuiltins))
	order := make([]string, 0, len(ompBuiltins))
	apply := func(commands []Command) {
		for _, command := range commands {
			if _, exists := active[command.Command]; exists {
				continue
			}
			order = append(order, command.Command)
			active[command.Command] = command
		}
	}
	apply(ompBuiltins)

	// The ban lists apply to skills only; omp's filters run over discovered
	// skills, never over its own builtin commands.
	scan := func(scope string, dirs ...string) {
		for _, dir := range dirs {
			if dir == "" {
				continue
			}
			if !filepath.IsAbs(dir) {
				// ctx.Home is the *service's* os.UserHomeDir(), not the
				// pane's, and is empty whenever that lookup failed - the
				// relay runs headless under launchd/systemd with an
				// arbitrary working directory unrelated to any project, so a
				// relative dir here would silently scan whatever skills
				// happen to sit under the service's own cwd and label them
				// "personal" or "project" as if they were the user's.
				continue
			}
			cmds, trunc := scanSkillDirFormat(dir, scope, ompCommandFormat, &budget)
			allowed := cmds[:0]
			for _, command := range cmds {
				if settings.allows(ompSkillName(command.Command)) {
					allowed = append(allowed, command)
				}
			}
			apply(allowed)
			truncated = truncated || trunc
		}
	}
	// scanProject scans <ancestor>/<stem>/skills in descending precedence:
	// innermost ancestor first and, within one ancestor, stems in the order omp
	// lists them. findProjectDirs returns ancestors outermost first with stems
	// in argument order, so the stems are passed reversed and the flat list is
	// walked backwards.
	scanProject := func(stems ...string) {
		reversed := make([]string, len(stems))
		for i, stem := range stems {
			reversed[len(stems)-1-i] = stem
		}
		dirs := findProjectDirs(ctx.Cwd, reversed)
		for i := len(dirs) - 1; i >= 0; i-- {
			scan("project", filepath.Join(dirs[i], "skills"))
		}
	}

	fallback := settings.sourceFallbackEnabled()

	// 1. native (100)
	if settings.enablePiProject {
		scanProject(".omp")
	}
	if settings.enablePiUser {
		scan("personal", agentroots.OMPSkillDirs(ctx.Home)...)
	}

	// 2. claude (80)
	if settings.enableClaudeProject {
		scanProject(".claude")
	}
	if settings.enableClaudeUser {
		scan("personal", filepath.Join(ctx.Home, ".claude", "skills"))
	}

	// 3. codex (70). omp scans a project .codex/skills even though codex
	// itself has no such directory, and that project level has no toggle of
	// its own.
	if fallback {
		scanProject(".codex")
	}
	if settings.enableCodexUser {
		scan("personal", filepath.Join(ctx.Home, ".codex", "skills"))
	}

	// 4. agents (70)
	if settings.enableAgentsProject {
		scanProject(".agent", ".agents")
	}
	if settings.enableAgentsUser {
		scan("personal",
			filepath.Join(ctx.Home, ".agent", "skills"),
			filepath.Join(ctx.Home, ".agents", "skills"))
	}

	// 5. opencode (55)
	if fallback {
		scanProject(".opencode")
		scan("personal", filepath.Join(ctx.Home, ".config", "opencode", "skills"))
	}

	// 6. github (30)
	if fallback {
		scanProject(".github")
	}

	// 7. customDirectories - subject to the ban lists but not to source
	// toggles. Scanned after every toggled source, so an authored skill of the
	// same name wins the collision. A relative entry is far more likely to
	// mean "relative to my project" than to be a mistake - config.yml is
	// often itself project-scoped - so it resolves against ctx.Cwd (the
	// pane's directory), matching how kimi.go resolves a relative
	// extra_skill_dirs entry against the project root. Skip it when the
	// pane's cwd is unknown rather than falling through to scan's own guard,
	// which would otherwise resolve against the service's unrelated cwd.
	for _, dir := range settings.customDirectories {
		dir = expandTilde(dir, ctx.Home)
		if !filepath.IsAbs(dir) {
			if ctx.Cwd == "" {
				continue
			}
			dir = filepath.Join(ctx.Cwd, dir)
		}
		scan("personal", dir)
	}

	// 8. managed (priority 5) - always enabled, scanned last so it loses every
	// collision.
	scan("personal", agentroots.OMPManagedSkillDirs(ctx.Home)...)

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

// ompSkillName recovers the skill name from a rendered /skill:<name> command.
// A builtin has no such prefix and is returned unchanged, which the ban lists
// never match because omp bans skills, not builtins.
func ompSkillName(command string) string {
	return strings.TrimPrefix(command, "/skill:")
}

// loadOMPSkillSettings reads the first config.yml (or config.yaml) found across
// omp's agent directories, then overlays every project-level .omp config from
// the git root down to the cwd. Where several profiles exist the relay cannot
// know which one the pane runs, so first-found wins for the user level: the
// search stops at the first directory containing a config file even when that
// file cannot be read, because falling through would apply an unrelated
// profile's settings. The innermost repo scope is applied last, matching omp's
// precedence, and list-valued keys replace rather than append.
func loadOMPSkillSettings(ctx DiscoverContext) ompSkillSettings {
	settings := defaultOMPSkillSettings()
	for _, dir := range agentroots.OMPConfigDirs(ctx.Home) {
		data, found, ok := settingsFileIn(dir, "config.yml", "config.yaml")
		if !found {
			continue
		}
		if ok {
			parseOMPSkillSettings(data, &settings)
		}
		break
	}
	for _, dir := range findProjectDirs(ctx.Cwd, []string{".omp"}) {
		if data, ok := readSettingsFile(dir, "config.yml", "config.yaml"); ok {
			parseOMPSkillSettings(data, &settings)
		}
	}
	return settings
}

// readSettingsFile returns the contents of the first readable name in dir.
func readSettingsFile(dir string, names ...string) ([]byte, bool) {
	data, _, ok := settingsFileIn(dir, names...)
	return data, ok
}

// settingsFileIn reads the first usable candidate name in dir. found reports
// that dir contains any candidate at all, readable or not, which is what a
// first-found-wins search over agent directories must stop on; ok reports that
// data was actually read. A candidate is unusable when it is not a regular
// file, exceeds maxSettingsSize, or cannot be read.
func settingsFileIn(dir string, names ...string) ([]byte, bool, bool) {
	found := false
	for _, name := range names {
		path := filepath.Join(dir, name)
		info, err := os.Stat(path)
		if err != nil {
			continue
		}
		found = true
		if !info.Mode().IsRegular() || info.Size() > maxSettingsSize {
			continue
		}
		data, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		return data, true, true
	}
	return nil, found, false
}

// expandTilde resolves a leading "~" against home, matching how omp expands
// customDirectories.
func expandTilde(path, home string) string {
	if path == "~" {
		return home
	}
	if rest, ok := strings.CutPrefix(path, "~/"); ok {
		return filepath.Join(home, rest)
	}
	return path
}

func init() {
	registerProvider(&ompProvider{})
}
