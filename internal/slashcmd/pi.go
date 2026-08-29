package slashcmd

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
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

// Discover reproduces Pi's skill resolution: the active agent directory and
// ~/.agents/skills at user scope, cwd/.pi/skills plus inherited .agents/skills
// at trusted project scope, and the skills array from global or trusted project
// settings, rendered as the /skill:<name> commands Pi registers.
//
// Sources are scanned in descending precedence with first-wins dedupe: project
// scope before user scope, and Pi's own directories before generic .agents
// directories. Scanning the winners first also spends the shared file budget on
// them, so exhaustion truncates the least important skills.
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

	settings := loadPiSkillSettings(ctx)
	if !settings.enableSkillCommands {
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
			if dir == "" || !filepath.IsAbs(dir) {
				continue
			}
			cmds, trunc := scanSkillDirFormat(dir, scope, ompCommandFormat, &budget)
			apply(cmds)
			truncated = truncated || trunc
		}
	}
	scanConfigured := func(paths []piConfiguredSkillPath, source string) {
		for _, configured := range paths {
			cmds, trunc := scanPiConfiguredSkillPath(configured.path, source, &budget)
			apply(cmds)
			truncated = truncated || trunc
		}
	}

	if settings.projectTrusted {
		if settings.skillPathSource == "project" {
			scanConfigured(settings.skillPaths, "project")
		}
		// Pi only scans cwd/.pi/skills. Generic .agents/skills are inherited
		// from cwd through the git root, or through the filesystem root outside
		// a repository.
		scan("project", filepath.Join(ctx.Cwd, ".pi", "skills"))
		agentDirs := findProjectDirs(ctx.Cwd, []string{".agents"})
		for i := len(agentDirs) - 1; i >= 0; i-- {
			scan("project", filepath.Join(agentDirs[i], "skills"))
		}
	}

	if settings.skillPathSource != "project" {
		scanConfigured(settings.skillPaths, "personal")
	}
	agentDir := selectedAgentDir(ctx, ".pi", "HERDR_PI_CONFIG_DIRS", "PI_CODING_AGENT_DIR")
	scan("personal", filepath.Join(agentDir, "skills"))
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

type piConfiguredSkillPath struct {
	path string
}

type piSkillSettings struct {
	enableSkillCommands bool
	skillPaths          []piConfiguredSkillPath
	skillPathSource     string
	projectTrusted      bool
}

type piSettingsValues struct {
	enableSkillCommands *bool
	skillPaths          []string
	hasSkillPaths       bool
	defaultProjectTrust string
}

func loadPiSkillSettings(ctx DiscoverContext) piSkillSettings {
	agentDir := selectedAgentDir(ctx, ".pi", "HERDR_PI_CONFIG_DIRS", "PI_CODING_AGENT_DIR")
	result := piSkillSettings{enableSkillCommands: true}
	var global piSettingsValues
	if filepath.IsAbs(agentDir) {
		if data, found, ok := settingsFileIn(agentDir, "settings.json"); found && ok {
			global = parsePiSettings(data)
			if global.enableSkillCommands != nil {
				result.enableSkillCommands = *global.enableSkillCommands
			}
			result.skillPaths = resolvePiSkillPaths(global.skillPaths, agentDir, ctx.Home)
		}
	}

	result.projectTrusted = piProjectTrusted(agentDir, ctx.Cwd, global.defaultProjectTrust)
	if !result.projectTrusted || !filepath.IsAbs(ctx.Cwd) {
		return result
	}
	projectSettingsDir := filepath.Join(ctx.Cwd, ".pi")
	data, found, ok := settingsFileIn(projectSettingsDir, "settings.json")
	if !found || !ok {
		return result
	}
	project := parsePiSettings(data)
	if project.enableSkillCommands != nil {
		result.enableSkillCommands = *project.enableSkillCommands
	}
	if project.hasSkillPaths {
		result.skillPaths = resolvePiSkillPaths(project.skillPaths, projectSettingsDir, ctx.Home)
		result.skillPathSource = "project"
	}
	return result
}

func parsePiSettings(data []byte) piSettingsValues {
	var raw map[string]json.RawMessage
	if json.Unmarshal(data, &raw) != nil {
		return piSettingsValues{}
	}
	var values piSettingsValues
	if encoded, ok := raw["enableSkillCommands"]; ok {
		var enabled bool
		if json.Unmarshal(encoded, &enabled) == nil {
			values.enableSkillCommands = &enabled
		}
	}
	if encoded, ok := raw["defaultProjectTrust"]; ok {
		_ = json.Unmarshal(encoded, &values.defaultProjectTrust)
	}
	if encoded, ok := raw["skills"]; ok {
		var paths []string
		if json.Unmarshal(encoded, &paths) == nil {
			values.skillPaths = paths
			values.hasSkillPaths = true
		} else {
			var legacy struct {
				EnableSkillCommands *bool `json:"enableSkillCommands"`
			}
			if json.Unmarshal(encoded, &legacy) == nil &&
				values.enableSkillCommands == nil && legacy.EnableSkillCommands != nil {
				values.enableSkillCommands = legacy.EnableSkillCommands
			}
		}
	}
	return values
}

func resolvePiSkillPaths(paths []string, base, home string) []piConfiguredSkillPath {
	resolved := make([]piConfiguredSkillPath, 0, len(paths))
	for _, configured := range paths {
		configured = expandTilde(strings.TrimSpace(configured), home)
		if configured == "" || strings.HasPrefix(configured, "~") {
			continue
		}
		if !filepath.IsAbs(configured) {
			configured = filepath.Join(base, configured)
		}
		resolved = append(resolved, piConfiguredSkillPath{path: filepath.Clean(configured)})
	}
	return resolved
}

func piProjectTrusted(agentDir, cwd, defaultTrust string) bool {
	if cwd == "" {
		return false
	}
	var decisions map[string]*bool
	if filepath.IsAbs(agentDir) {
		if data, found, ok := settingsFileIn(agentDir, "trust.json"); found && ok {
			_ = json.Unmarshal(data, &decisions)
		}
	}
	current := filepath.Clean(cwd)
	for range maxGitWalkDepth {
		if decision, ok := decisions[current]; ok && decision != nil {
			return *decision
		}
		parent := filepath.Dir(current)
		if parent == current {
			break
		}
		current = parent
	}
	return strings.EqualFold(strings.TrimSpace(defaultTrust), "always")
}

func scanPiConfiguredSkillPath(path, source string, budget *int) ([]Command, bool) {
	if *budget <= 0 {
		return nil, true
	}
	info, err := os.Stat(path)
	if err != nil {
		return nil, false
	}
	if info.IsDir() {
		skillFile := filepath.Join(path, "SKILL.md")
		if skillInfo, err := os.Stat(skillFile); err == nil && skillInfo.Mode().IsRegular() {
			metadata, ok := readSkillMetadata(skillFile)
			if !ok || metadata["description"] == "" {
				return nil, false
			}
			*budget--
			name := metadata["name"]
			if name == "" {
				name = filepath.Base(path)
			}
			if !commandNamePattern.MatchString(name) {
				return nil, false
			}
			return []Command{{
				Command:      "/skill:" + name,
				Description:  compact(metadata["description"], 240),
				Source:       source,
				ArgumentHint: compact(metadata["argument-hint"], 120),
			}}, false
		}
		return scanSkillDirFormat(path, source, ompCommandFormat, budget)
	}
	if !info.Mode().IsRegular() {
		return nil, false
	}
	metadata, ok := readSkillMetadata(path)
	if !ok || metadata["description"] == "" {
		return nil, false
	}
	*budget--
	name := metadata["name"]
	if name == "" {
		name = strings.TrimSuffix(filepath.Base(path), filepath.Ext(path))
	}
	if !commandNamePattern.MatchString(name) {
		return nil, false
	}
	return []Command{{
		Command:      "/skill:" + name,
		Description:  compact(metadata["description"], 240),
		Source:       source,
		ArgumentHint: compact(metadata["argument-hint"], 120),
	}}, false
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
