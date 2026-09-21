package slashcmd

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

type cursorProvider struct{}

func (p *cursorProvider) ID() string { return "cursor" }

// cursorBuiltins mirrors the commands Cursor's terminal UI registers for
// itself. Cursor builds this table at runtime and gates a few entries behind
// its debug flag, so those are omitted here: /debug-test, /throw, /pq and
// /static-indicator exist only when debug logging is enabled, and the
// /dev:* entries are documented by Cursor as developer-only (they classify
// conversations and score commits for its own telemetry) with their own
// separate visibility gate. Publishing either group would offer the phone
// commands the pane does not have. Everything else is unconditional.
var cursorBuiltins = []Command{
	{"/add-dir", "Add directory", "builtin", "<path>"},
	{"/ask", "Ask Mode", "builtin", ""},
	{"/bedrock", "Bedrock", "builtin", "<subcommand> <options>"},
	{"/btw", "Side question", "builtin", "<question>"},
	{"/changes", "View changes", "builtin", ""},
	{"/clear", "Clear", "builtin", ""},
	{"/command", "Commands", "builtin", ""},
	{"/commit", "Commit", "builtin", ""},
	{"/config", "Config", "builtin", ""},
	{"/context", "Context", "builtin", ""},
	{"/copy", "Copy message", "builtin", ""},
	{"/copy-conversation-id", "Copy Conversation ID", "builtin", ""},
	{"/copy-request-id", "Copy Request ID", "builtin", ""},
	{"/debug", "Debug Mode", "builtin", "[<prompt>]"},
	{"/detach", "Detach", "builtin", ""},
	{"/exit", "Exit", "builtin", ""},
	{"/fast", "Fast Mode", "builtin", ""},
	{"/feedback", "Feedback", "builtin", "[message]"},
	{"/fork", "Fork Chat", "builtin", ""},
	{"/full-conversation", "Full Conversation", "builtin", ""},
	{"/goal", "Goal", "builtin", "<objective>"},
	{"/help", "Help", "builtin", "<command>"},
	{"/jobs", "Tasks", "builtin", ""},
	{"/line-numbers", "Line Numbers", "builtin", ""},
	{"/load-workspace", "Load workspace", "builtin", "<name>"},
	{"/logout", "Logout", "builtin", ""},
	{"/logs", "Logs", "builtin", ""},
	{"/max-mode", "Max Mode", "builtin", ""},
	{"/mcp", "MCP", "builtin", "[list|list-tools] [<identifier>]"},
	{"/model", "Model", "builtin", "<filter>"},
	{"/open-in-prompt-quality", "Open in Prompt Quality", "builtin", ""},
	{"/plan", "Plan Mode", "builtin", "[<prompt>]"},
	{"/plugin", "Plugin", "builtin", "[list|marketplace list|marketplace add <git-url>]"},
	{"/quit", "Quit", "builtin", ""},
	{"/rename", "Rename Chat", "builtin", "<name>"},
	{"/resume", "Resume Chat", "builtin", ""},
	{"/rewind", "Rewind", "builtin", ""},
	{"/rule", "Rules", "builtin", ""},
	{"/save-workspace", "Save workspace", "builtin", "<name>"},
	{"/show-thinking", "Show Thinking", "builtin", ""},
	{"/skills", "Skills", "builtin", ""},
	{"/status-indicators", "Status Indicators", "builtin", ""},
	{"/summarize", "Summarize", "builtin", ""},
	{"/sync-theme", "Sync Theme", "builtin", ""},
	{"/usage", "Usage", "builtin", ""},
	{"/vim", "Vim Mode", "builtin", ""},
	{"/zen-mode", "Zen Mode", "builtin", ""},
}

// cursorSkillRoots reports the directories Cursor scans for personal skills.
//
// Cursor resolves a skill's slash-command id from the name of the directory
// holding SKILL.md, not from the frontmatter "name" field - see
// getSkillIdForPath in its bundled skill loader, which returns the parent
// segment of the SKILL.md path. A skill in ~/.cursor/skills/pdf-forms is
// therefore /pdf-forms even when its frontmatter says name: pdf form toolkit.
// That is the opposite of the generic and Hermes paths, so Cursor needs its
// own scanner rather than discoverGenericSkills.
//
// The roots are deliberately narrower than Cursor's full internal list.
// Cursor also probes ~/.cursor/skills-cursor (reserved for its own built-in
// skills and managed automatically - its own documentation tells users never
// to create skills there), ~/.cursor/cloud-skills and ~/.cursor/plugins, as
// well as the ~/.claude, ~/.codex and ~/.grok trees. Those either duplicate
// another agent's catalog in a Cursor pane or expose internal state that is
// not a user-authored skill. ~/.agents/skills is included because it is the
// cross-agent shared root Cursor reads alongside its own.
func cursorSkillRoots(home string) []string {
	return []string{
		filepath.Join(home, ".cursor", "skills"),
		filepath.Join(home, ".agents", "skills"),
	}
}

// scanCursorSkillDirBudget scans dir for <entry>/SKILL.md and names each
// command after the entry directory. It follows symlinked skill directories
// through entryIsDir, matching scanSkillDirBudget: a personal root may link a
// skill to a checkout elsewhere, and Cursor itself resolves those links.
func scanCursorSkillDirBudget(dir, source string, budget *int) ([]Command, bool) {
	if dir == "" || *budget <= 0 {
		return nil, *budget <= 0
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, false
	}
	sort.Slice(entries, func(i, j int) bool {
		left, right := strings.ToLower(entries[i].Name()), strings.ToLower(entries[j].Name())
		if left == right {
			return entries[i].Name() < entries[j].Name()
		}
		return left < right
	})
	var commands []Command
	seen := make(map[string]bool, len(entries))
	truncated := false
	for _, entry := range entries {
		if *budget <= 0 {
			truncated = true
			break
		}
		if strings.HasPrefix(entry.Name(), ".") {
			continue
		}
		child := filepath.Join(dir, entry.Name())
		if !entryIsDir(entry, child) {
			continue
		}
		metadata, resolved, ok := scopedSkillMetadata(dir, entry.Name(), source, "")
		if !ok || seen[resolved] {
			continue
		}
		seen[resolved] = true
		*budget--
		if !userInvocable(metadata) {
			continue
		}
		name := entry.Name()
		if !commandNamePattern.MatchString(name) {
			continue
		}
		description := metadata["description"]
		if description == "" {
			description = strings.ToUpper(name[:1]) + name[1:] + " skill"
		}
		commands = append(commands, Command{
			Command:      "/" + name,
			Description:  compact(description, 240),
			Source:       source,
			ArgumentHint: compact(metadata["argument-hint"], 120),
		})
	}
	return commands, truncated
}

// projectAncestors returns the ancestor directories Cursor searches for
// workspace-scoped config, from the repository root down to cwd, or from the
// filesystem root when cwd is not in a repository. Cursor passes every
// workspace folder to its loader, so a nested cwd sees outer workspace roots
// as well.
func projectAncestors(cwd string) []string {
	if cwd == "" {
		return nil
	}
	stop := findGitRoot(cwd)
	if stop == "" {
		stop = filepath.VolumeName(cwd) + string(filepath.Separator)
	}
	var reversed []string
	dir := cwd
	for range maxGitWalkDepth {
		reversed = append(reversed, dir)
		if dir == stop {
			break
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}
	ancestors := make([]string, 0, len(reversed))
	for i := len(reversed) - 1; i >= 0; i-- {
		ancestors = append(ancestors, reversed[i])
	}
	return ancestors
}

// uniqueCommandName mirrors Cursor's getSkillIdForPath collision handling: the
// first use of a name keeps it, and each later duplicate gains a -2, -3 ...
// suffix. Cursor lists both skills rather than letting one replace the other,
// so the palette does the same.
func uniqueCommandName(name string, taken map[string]bool) string {
	if !taken[name] {
		taken[name] = true
		return name
	}
	for suffix := 2; ; suffix++ {
		candidate := fmt.Sprintf("%s-%d", name, suffix)
		if !taken[candidate] {
			taken[candidate] = true
			return candidate
		}
	}
}

// projectCommandRoots returns every ancestor's flat command directories in the
// order Cursor reads them for a workspace: outer scopes first, so the nearest
// workspace wins a name collision the same way a later root does.
func projectCommandRoots(cwd string) []string {
	var roots []string
	for _, ancestor := range projectAncestors(cwd) {
		roots = append(roots, cursorCommandRoots(ancestor)...)
	}
	return roots
}

// cursorCommandRoots reports the flat *.md command directories Cursor reads for
// one scope. Unlike skills these are not nested a directory per command: the
// id is the filename without its .md suffix, and Cursor does not recurse.
//
// Both trees are read at each scope. Cursor registers them in this order
// within a scope and loadCommandsFromDirectory assigns each id unconditionally,
// so the later .cursor tree wins a name collision. The .claude tree is in
// Cursor's own read path, not something HeRDR adds.
func cursorCommandRoots(dir string) []string {
	return []string{
		filepath.Join(dir, ".claude", "commands"),
		filepath.Join(dir, ".cursor", "commands"),
	}
}

// scanCursorCommandDirBudget scans dir for flat *.md command files and names
// each command after the file. It walks the top level only, matching Cursor,
// and skips anything that is not a regular file: a FIFO or socket named *.md
// would block fileFrontmatter forever.
func scanCursorCommandDirBudget(dir, source string, budget *int) ([]Command, bool) {
	if dir == "" || *budget <= 0 {
		return nil, *budget <= 0
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, false
	}
	sort.Slice(entries, func(i, j int) bool {
		left, right := strings.ToLower(entries[i].Name()), strings.ToLower(entries[j].Name())
		if left == right {
			return entries[i].Name() < entries[j].Name()
		}
		return left < right
	})
	var commands []Command
	seen := make(map[string]bool, len(entries))
	truncated := false
	for _, entry := range entries {
		if *budget <= 0 {
			truncated = true
			break
		}
		name := entry.Name()
		if strings.HasPrefix(name, ".") || !strings.HasSuffix(name, ".md") {
			continue
		}
		if !entry.Type().IsRegular() {
			continue
		}
		cmdName := strings.TrimSuffix(name, ".md")
		if !commandNamePattern.MatchString(cmdName) || seen[cmdName] {
			continue
		}
		seen[cmdName] = true
		*budget--
		path := filepath.Join(dir, name)
		fm := fileFrontmatter(path)
		if isHidden(fm) || !userInvocable(fm) {
			continue
		}
		commands = append(commands, Command{
			Command:      "/" + cmdName,
			Description:  descriptionFrom(fm, path),
			Source:       source,
			ArgumentHint: compact(fm["argument-hint"], 120),
		})
	}
	return commands, truncated
}

func (p *cursorProvider) Discover(ctx DiscoverContext) ([]Command, bool) {
	if ctx.SuppressNative {
		builtins := make([]Command, len(cursorBuiltins))
		copy(builtins, cursorBuiltins)
		return builtins, false
	}

	budget := maxCustomFiles
	truncated := false
	order := make([]string, 0, len(cursorBuiltins))
	active := make(map[string]Command, len(cursorBuiltins))
	add := func(command Command) {
		if _, exists := active[command.Command]; !exists {
			order = append(order, command.Command)
		}
		active[command.Command] = command
	}

	// Cursor resolves its builtins from a reserved registry rather than the
	// markdown command map, so a file reusing a builtin name would otherwise
	// publish a command the pane does not run. Reserve those names: a colliding
	// file is suffixed beside the builtin.
	reserved := make(map[string]bool, len(cursorBuiltins))
	for _, builtin := range cursorBuiltins {
		reserved[builtin.Command] = true
		add(builtin)
	}

	// Cursor's command map assigns ids unconditionally
	// (loadCommandsFromDirectory calls commands.set for every file), so a later
	// root replaces an earlier one of the same name. The user roots load last,
	// so a personal command file wins over a workspace file of the same name.
	scanCommands := func(root, source string) {
		cmds, trunc := scanCursorCommandDirBudget(root, source, &budget)
		truncated = truncated || trunc
		for _, command := range cmds {
			if reserved[command.Command] {
				command.Command = uniqueCommandName(command.Command, reserved)
			}
			add(command)
		}
	}
	for _, root := range projectCommandRoots(ctx.Cwd) {
		scanCommands(root, "project")
	}
	for _, root := range cursorCommandRoots(ctx.Home) {
		scanCommands(root, "personal")
	}

	// Skills collide differently. getSkillIdForPath keeps a set of ids already
	// handed out and appends -2, -3 ... to a later duplicate, so one folder name
	// under two roots lists both entries instead of one replacing the other.
	// Personal roots load first, matching Cursor's loadSkillRoots order, so the
	// personal entry keeps the bare name.
	taken := make(map[string]bool, len(active))
	for name := range active {
		taken[name] = true
	}
	scanSkills := func(root, source string) {
		cmds, trunc := scanCursorSkillDirBudget(root, source, &budget)
		truncated = truncated || trunc
		for _, command := range cmds {
			command.Command = uniqueCommandName(command.Command, taken)
			add(command)
		}
	}
	for _, root := range cursorSkillRoots(ctx.Home) {
		scanSkills(root, "personal")
	}
	for _, ancestor := range projectAncestors(ctx.Cwd) {
		for _, stem := range []string{".cursor", ".agents"} {
			scanSkills(filepath.Join(ancestor, stem, "skills"), "project")
		}
	}

	// Additional configured skill dirs from agent-profiles.ini. Cursor's own
	// command tree has no configured analogue, so this stays the documented
	// escape hatch for pointing the palette at a directory outside the roots
	// above.
	if len(ctx.SkillDirs) > 0 {
		format := ctx.CommandFormat
		if format == "" {
			format = "/{name}"
		}
		custom, trunc := discoverGenericSkills(ctx.SkillDirs, format)
		for _, command := range custom {
			if _, exists := active[command.Command]; !exists {
				order = append(order, command.Command)
			}
			active[command.Command] = command
		}
		truncated = truncated || trunc
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

func init() {
	registerProvider(&cursorProvider{})
}
