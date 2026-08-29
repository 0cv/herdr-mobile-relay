package slashcmd

import (
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
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
	if ctx.SuppressNative {
		builtins := make([]Command, len(piBuiltins))
		copy(builtins, piBuiltins)
		return builtins, false
	}
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

	seenFiles := make(map[string]bool)
	seenDirs := make(map[string]bool)
	scanScope := func(source string, roots []string, scope piSkillScope) {
		commands, trunc := scanPiSkillRoots(roots, scope.paths, scope.patterns, source, &budget, seenFiles, seenDirs)
		apply(commands)
		truncated = truncated || trunc
	}

	if settings.projectTrusted {
		projectRoots := []string{filepath.Join(ctx.Cwd, ".pi", "skills")}
		agentDirs := findProjectDirs(ctx.Cwd, []string{".agents"})
		for index := len(agentDirs) - 1; index >= 0; index-- {
			projectRoots = append(projectRoots, filepath.Join(agentDirs[index], "skills"))
		}
		scanScope("project", projectRoots, settings.project)
	}

	agentDir := selectedAgentDir(ctx, ".pi", "HERDR_PI_CONFIG_DIRS", "PI_CODING_AGENT_DIR")
	scanScope("personal", []string{
		filepath.Join(agentDir, "skills"),
		filepath.Join(ctx.Home, ".agents", "skills"),
	}, settings.global)

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

type piSkillScope struct {
	paths    []piConfiguredSkillPath
	patterns []string
}

type piSkillSettings struct {
	enableSkillCommands bool
	global              piSkillScope
	project             piSkillScope
	projectTrusted      bool
}

type piSettingsValues struct {
	enableSkillCommands *bool
	skillPaths          []string
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
			result.global = resolvePiSkillScope(global.skillPaths, agentDir, ctx.Home)
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
	result.project = resolvePiSkillScope(project.skillPaths, projectSettingsDir, ctx.Home)
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

func resolvePiSkillScope(entries []string, base, home string) piSkillScope {
	var scope piSkillScope
	for _, entry := range entries {
		entry = strings.TrimSpace(entry)
		if entry == "" {
			continue
		}
		prefix := byte(0)
		if strings.ContainsRune("!+-", rune(entry[0])) {
			prefix = entry[0]
			entry = strings.TrimSpace(entry[1:])
		}
		expanded := expandTilde(entry, home)
		if expanded == "" || strings.HasPrefix(expanded, "~") {
			continue
		}
		if !filepath.IsAbs(expanded) {
			expanded = filepath.Join(base, expanded)
		}
		expanded = filepath.Clean(expanded)
		if prefix != 0 || strings.ContainsAny(entry, "*?[]") {
			if prefix != 0 {
				expanded = string(prefix) + expanded
			}
			scope.patterns = append(scope.patterns, expanded)
			continue
		}
		scope.paths = append(scope.paths, piConfiguredSkillPath{path: expanded})
	}
	return scope
}

func piProjectTrusted(agentDir, cwd, defaultTrust string) bool {
	if cwd == "" {
		return false
	}
	var decisions map[string]*bool
	if filepath.IsAbs(agentDir) {
		if data, found, ok := settingsFileIn(agentDir, "trust.json"); found {
			if !ok || json.Unmarshal(data, &decisions) != nil {
				return false
			}
		}
	}
	current, err := filepath.Abs(cwd)
	if err != nil {
		return false
	}
	if resolved, err := filepath.EvalSymlinks(current); err == nil {
		current = resolved
	}
	current = filepath.Clean(current)
	for {
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

func scanPiSkillRoots(paths []string, configured []piConfiguredSkillPath, patterns []string, source string, budget *int, seenFiles, seenDirs map[string]bool) ([]Command, bool) {
	var commands []Command
	truncated := false
	for _, entry := range configured {
		found, trunc := scanPiSkillTree(entry.path, source, patterns, false, true, budget, seenFiles, seenDirs)
		commands = append(commands, found...)
		truncated = truncated || trunc
	}
	for _, path := range paths {
		found, trunc := scanPiSkillTree(path, source, patterns, true, true, budget, seenFiles, seenDirs)
		commands = append(commands, found...)
		truncated = truncated || trunc
	}
	return commands, truncated
}

func scanPiSkillTree(path, source string, patterns []string, auto, includeFlat bool, budget *int, seenFiles, seenDirs map[string]bool) ([]Command, bool) {
	if !filepath.IsAbs(path) {
		return nil, false
	}
	if *budget <= 0 {
		return nil, true
	}
	info, err := os.Stat(path)
	if err != nil {
		return nil, false
	}
	real := path
	if resolved, err := filepath.EvalSymlinks(path); err == nil {
		real = resolved
	}
	real = filepath.Clean(real)
	if info.Mode().IsRegular() {
		if !strings.EqualFold(filepath.Ext(real), ".md") || !piSkillAllowed(real, patterns, auto) || seenFiles[real] {
			return nil, false
		}
		seenFiles[real] = true
		command := skillCommandFromFile(real, strings.TrimSuffix(filepath.Base(real), filepath.Ext(real)), source, ompCommandFormat, budget, skillScanOptions{requireDescription: true})
		if command == nil {
			return nil, false
		}
		return []Command{*command}, false
	}
	if !info.IsDir() || seenDirs[real] {
		return nil, false
	}
	seenDirs[real] = true

	skillFile := filepath.Join(real, "SKILL.md")
	if skillInfo, err := os.Stat(skillFile); err == nil && skillInfo.Mode().IsRegular() {
		if !piSkillAllowed(skillFile, patterns, auto) || seenFiles[skillFile] {
			return nil, false
		}
		seenFiles[skillFile] = true
		command := skillCommandFromFile(skillFile, filepath.Base(real), source, ompCommandFormat, budget, skillScanOptions{requireDescription: true})
		if command == nil {
			return nil, false
		}
		return []Command{*command}, false
	}

	entries, err := os.ReadDir(real)
	if err != nil {
		return nil, false
	}
	var commands []Command
	for _, entry := range entries {
		if *budget <= 0 {
			return commands, true
		}
		name := entry.Name()
		if strings.HasPrefix(name, ".") || name == "node_modules" {
			continue
		}
		child := filepath.Join(real, name)
		childInfo, err := os.Stat(child)
		if err != nil {
			continue
		}
		if childInfo.IsDir() {
			found, truncated := scanPiSkillTree(child, source, patterns, auto, false, budget, seenFiles, seenDirs)
			commands = append(commands, found...)
			if truncated {
				return commands, true
			}
			continue
		}
		if !includeFlat || !childInfo.Mode().IsRegular() || !strings.EqualFold(filepath.Ext(name), ".md") || strings.EqualFold(name, "SKILL.md") {
			continue
		}
		if !piSkillAllowed(child, patterns, auto) {
			continue
		}
		realFile := child
		if resolved, err := filepath.EvalSymlinks(child); err == nil {
			realFile = resolved
		}
		if seenFiles[realFile] {
			continue
		}
		seenFiles[realFile] = true
		if command := skillCommandFromFile(realFile, strings.TrimSuffix(name, filepath.Ext(name)), source, ompCommandFormat, budget, skillScanOptions{requireDescription: true}); command != nil {
			commands = append(commands, *command)
		}
	}
	return commands, false
}

func piSkillAllowed(path string, patterns []string, auto bool) bool {
	allowed := true
	hasPlainIncludes := false
	if !auto {
		for _, pattern := range patterns {
			if pattern != "" && !strings.ContainsRune("!+-", rune(pattern[0])) {
				hasPlainIncludes = true
				break
			}
		}
		if hasPlainIncludes {
			allowed = false
		}
	}
	dir := filepath.Dir(path)
	for _, raw := range patterns {
		if raw == "" {
			continue
		}
		prefix := byte(0)
		if strings.ContainsRune("!+-", rune(raw[0])) {
			prefix = raw[0]
			raw = raw[1:]
		} else if auto {
			continue
		}
		if !piGlobMatch(raw, path) && !piGlobMatch(raw, dir) {
			continue
		}
		allowed = prefix == '+' || prefix == 0
	}
	return allowed
}

func piGlobMatch(pattern, path string) bool {
	pattern = filepath.ToSlash(filepath.Clean(pattern))
	path = filepath.ToSlash(filepath.Clean(path))
	var expression strings.Builder
	expression.WriteByte('^')
	for index := 0; index < len(pattern); index++ {
		switch pattern[index] {
		case '*':
			if index+1 < len(pattern) && pattern[index+1] == '*' {
				index++
				if index+1 < len(pattern) && pattern[index+1] == '/' {
					index++
					expression.WriteString("(?:.*/)?")
				} else {
					expression.WriteString(".*")
				}
			} else {
				expression.WriteString("[^/]*")
			}
		case '?':
			expression.WriteString("[^/]")
		default:
			expression.WriteString(regexp.QuoteMeta(string(pattern[index])))
		}
	}
	expression.WriteByte('$')
	matched, err := regexp.MatchString(expression.String(), path)
	return err == nil && matched
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
