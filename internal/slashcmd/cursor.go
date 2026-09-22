package slashcmd

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"golang.org/x/text/collate"
	"golang.org/x/text/language"
)

type cursorProvider struct{}

func (p *cursorProvider) ID() string { return "cursor" }

var cursorBuiltins = []Command{
	{"/about", "About", "builtin", ""},
	{"/add-dir", "Add directory", "builtin", "<path>"},
	{"/ask", "Ask Mode", "builtin", ""},
	{"/auto-review", "Auto-review", "builtin", ""},
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
	{"/cursor", "Open in Cursor", "builtin", ""},
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
	{"/open", "Open in Cursor", "builtin", ""},
	{"/plan", "Plan Mode", "builtin", "[<prompt>]"},
	{"/plugin", "Plugin", "builtin", "[list|marketplace list|marketplace add <git-url>]"},
	{"/quit", "Quit", "builtin", ""},
	{"/rename", "Rename Chat", "builtin", "<name>"},
	{"/resume", "Resume Chat", "builtin", ""},
	{"/rewind", "Rewind", "builtin", ""},
	{"/rule", "Rules", "builtin", ""},
	{"/run-everything", "Run Everything", "builtin", ""},
	{"/sandbox", "Sandbox", "builtin", ""},
	{"/save-workspace", "Save workspace", "builtin", "<name>"},
	{"/shell", "Shell Mode", "builtin", "[<command>]"},
	{"/show-thinking", "Show Thinking", "builtin", ""},
	{"/skills", "Skills", "builtin", ""},
	{"/status-indicators", "Status Indicators", "builtin", ""},
	{"/summarize", "Summarize", "builtin", ""},
	{"/sync-theme", "Sync Theme", "builtin", ""},
	{"/update", "Update", "builtin", ""},
	{"/usage", "Usage", "builtin", ""},
	{"/vim", "Vim Mode", "builtin", ""},
	{"/zen-mode", "Zen Mode", "builtin", ""},
}

var cursorBuiltinAliases = map[string][]string{
	"/about":          {"/whoami", "/account"},
	"/auto-review":    {"/smart-auto"},
	"/clear":          {"/new", "/new-chat", "/newchat"},
	"/command":        {"/commands"},
	"/config":         {"/settings", "/preferences", "/cli-config"},
	"/copy":           {"/clipboard", "/paste"},
	"/fork":           {"/duplicate", "/clone", "/branch"},
	"/line-numbers":   {"/lines", "/numbers"},
	"/open":           {"/cursor"},
	"/rename":         {"/name", "/title"},
	"/resume":         {"/continue", "/recent", "/history"},
	"/rewind":         {"/restore", "/undo"},
	"/run-everything": {"/auto-run"},
	"/shell":          {"/sh", "/run"},
	"/show-thinking":  {"/thoughts", "/thinking", "/thinking-blocks"},
	"/summarize":      {"/compress", "/compact"},
	"/zen-mode":       {"/zen"},
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

func cursorSkillWalk(root string, project bool, budget *int, seenFiles map[string]bool) ([]string, bool) {
	const maxDepth = 10
	if root == "" {
		return nil, false
	}
	realRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		return nil, false
	}

	var files []string
	fileIndices := make(map[string]int)
	pathOrder := collate.New(language.Und)
	seenDirs := make(map[string]bool)
	visited := 0
	truncated := false

	var walk func(dir string, depth int, linkedPersonalDir bool)
	walk = func(dir string, depth int, linkedPersonalDir bool) {
		if depth > maxDepth {
			return
		}
		if *budget <= 0 {
			truncated = true
			return
		}
		resolved, err := filepath.EvalSymlinks(dir)
		if err != nil || seenDirs[resolved] || !linkedPersonalDir && !pathWithin(resolved, realRoot) {
			return
		}
		seenDirs[resolved] = true
		visited++
		if visited > maxEntries {
			truncated = true
			return
		}

		entries, err := os.ReadDir(dir)
		if err != nil {
			return
		}
		for _, entry := range entries {
			if *budget <= 0 {
				truncated = true
				return
			}
			child := filepath.Join(dir, entry.Name())
			isSymlink := entry.Type()&os.ModeSymlink != 0
			if entryIsDir(entry, child) {
				if linkedPersonalDir || cursorSkillDirIgnored(entry.Name()) {
					continue
				}
				walk(child, depth+1, !project && isSymlink)
				continue
			}
			if entry.Name() != "SKILL.md" || !regularFile(child) {
				continue
			}
			resolved, err := filepath.EvalSymlinks(child)
			if err != nil || seenFiles[resolved] {
				continue
			}
			if (!linkedPersonalDir || isSymlink) && !pathWithin(resolved, realRoot) {
				continue
			}
			if index, exists := fileIndices[resolved]; exists {
				if pathOrder.CompareString(child, files[index]) < 0 {
					files[index] = child
				}
				continue
			}
			fileIndices[resolved] = len(files)
			files = append(files, child)
			*budget--
		}
	}
	walk(root, 0, false)

	sort.SliceStable(files, func(i, j int) bool {
		return pathOrder.CompareString(files[i], files[j]) < 0
	})
	for resolved := range fileIndices {
		seenFiles[resolved] = true
	}
	dirs := make([]string, 0, len(files))
	for _, file := range files {
		dirs = append(dirs, filepath.Dir(file))
	}
	return dirs, truncated
}

func cursorSkillDirIgnored(name string) bool {
	switch name {
	case "node_modules", "__pycache__", "dist", "build":
		return true
	default:
		return strings.HasPrefix(name, ".")
	}
}

func regularFile(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.Mode().IsRegular()
}

// duplicateSkillFolderNames reports the folder names that appear more than once
// within one root. Cursor computes this per root, before assigning any id, and
// uses it to decide which skills get a relative-path id.
func duplicateSkillFolderNames(dirs []string) map[string]bool {
	counts := make(map[string]int, len(dirs))
	for _, dir := range dirs {
		counts[filepath.Base(dir)]++
	}
	duplicates := make(map[string]bool)
	for name, count := range counts {
		if count > 1 {
			duplicates[name] = true
		}
	}
	return duplicates
}

// relativeSkillID mirrors getRelativeSkillId: a skill folder's path relative to
// the root, with the separators joined by "-". It reports false when the path
// does not sit below the root, where Cursor falls back to the bare name.
func relativeSkillID(root, skillDir string) (string, bool) {
	relative, err := filepath.Rel(root, skillDir)
	if err != nil || relative == "" || relative == "." || filepath.IsAbs(relative) {
		return "", false
	}
	if relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		return "", false
	}
	parts := strings.Split(relative, string(filepath.Separator))
	kept := make([]string, 0, len(parts))
	for _, part := range parts {
		if part != "" && part != "." {
			kept = append(kept, part)
		}
	}
	if len(kept) == 0 {
		return "", false
	}
	return strings.Join(kept, "-"), true
}

// cursorSkillID mirrors getSkillIdForPath: the id is the skill folder's name,
// except that a folder name duplicated within the same root instead uses the
// root-relative path, and any id already handed out for this root gains a -2,
// -3 ... suffix.
//
// taken is deliberately scoped to ONE root. Cursor builds a fresh id set inside
// each loadSkillsFromDirectory call, so duplicates across roots do not suffix
// - the later root simply overwrites the earlier id in the skills map.
func cursorSkillID(root, skillDir string, duplicates, taken map[string]bool) string {
	id := filepath.Base(skillDir)
	if duplicates[id] {
		if relative, ok := relativeSkillID(root, skillDir); ok {
			id = relative
		}
	}
	return uniqueCommandName(id, taken)
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
// would block discovery forever.
func scanCursorCommandDirBudget(dir, source string, budget *int) ([]Command, bool) {
	if dir == "" || *budget <= 0 {
		return nil, *budget <= 0
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, false
	}
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
		path := filepath.Join(dir, name)
		info, err := os.Stat(path)
		if err != nil || !info.Mode().IsRegular() || info.Size() == 0 || info.Size() > maxCursorCommandSize {
			continue
		}
		cmdName := strings.TrimSuffix(name, ".md")
		if !commandNamePattern.MatchString(cmdName) || seen[cmdName] {
			continue
		}
		seen[cmdName] = true
		*budget--
		data, ok := readCursorCommandFile(path)
		if !ok {
			continue
		}
		fm, _ := parseFrontmatterBytes(data)
		description := fm["description"]
		if description == "" {
			description = extractFirstLineBytes(data)
		}
		commands = append(commands, Command{
			Command:      "/" + cmdName,
			Description:  compact(description, 120),
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

	// Cursor resolves its builtins from a separate registry, not the markdown
	// command map or the skills map. A file or skill reusing a builtin name is
	// therefore not separately invocable - typing that name runs the builtin -
	// and the pane does not register any alias for it. Publishing a fabricated
	// suffix like /clear-2 would offer the phone a command nothing resolves, so
	// a colliding entry is dropped and the builtin keeps its name.
	reserved := make(map[string]bool, len(cursorBuiltins))
	for _, builtin := range cursorBuiltins {
		reserved[strings.ToLower(builtin.Command)] = true
		for _, alias := range cursorBuiltinAliases[builtin.Command] {
			reserved[strings.ToLower(alias)] = true
		}
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
			if reserved[strings.ToLower(command.Command)] {
				continue
			}
			add(command)
		}
	}
	if ctx.Cwd != "" {
		for _, root := range cursorCommandRoots(ctx.Cwd) {
			scanCommands(root, "project")
		}
	}
	for _, root := range cursorCommandRoots(ctx.Home) {
		scanCommands(root, "personal")
	}
	for name := range active {
		reserved[strings.ToLower(name)] = true
	}

	// Skills follow getSkillIdForPath instead: the id is the skill folder's
	// name, or the root-relative path when that folder name repeats within the
	// same root, and any repeat within the root gains a -2, -3 ... suffix.
	//
	// The id set is built fresh per root. Cursor's loadSkillsFromDirectory
	// creates its own set inside each call, so a duplicate across roots is not
	// suffixed - the later root overwrites the earlier id in the skills map.
	// That makes root order decide collisions, and Cursor's loadSkillRoots
	// passes workspace roots before personal roots, so a personal skill replaces
	// a project one with the same id.
	seenSkillFiles := make(map[string]bool)
	scanSkills := func(root, source string, project bool) {
		dirs, trunc := cursorSkillWalk(root, project, &budget, seenSkillFiles)
		truncated = truncated || trunc
		if len(dirs) == 0 {
			return
		}
		duplicates := duplicateSkillFolderNames(dirs)
		taken := make(map[string]bool, len(dirs))
		for _, skillDir := range dirs {
			data, ok := readCursorSkillFile(root, skillDir, project)
			if !ok {
				continue
			}
			name := cursorSkillID(root, skillDir, duplicates, taken)
			if !commandNamePattern.MatchString(name) {
				continue
			}
			metadata, ok := parseCursorSkillMetadata(data)
			if !ok || !userInvocable(metadata) {
				continue
			}
			command := "/" + name
			if reserved[strings.ToLower(command)] {
				continue
			}
			description := metadata["description"]
			if description == "" {
				description = strings.ToUpper(name[:1]) + name[1:] + " skill"
			}
			add(Command{
				Command:      command,
				Description:  compact(description, 240),
				Source:       source,
				ArgumentHint: compact(metadata["argument-hint"], 120),
			})
		}
	}
	if ctx.Cwd != "" {
		for _, root := range cursorSkillRoots(ctx.Cwd) {
			scanSkills(root, "project", true)
		}
	}
	for _, root := range cursorSkillRoots(ctx.Home) {
		scanSkills(root, "personal", false)
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
			if reserved[strings.ToLower(command.Command)] {
				continue
			}
			add(command)
		}
		truncated = truncated || trunc
	}

	commands := make([]Command, 0, len(order))
	seen := make(map[string]bool, len(order))
	for _, name := range order {
		key := strings.ToLower(name)
		if seen[key] {
			continue
		}
		seen[key] = true
		commands = append(commands, active[name])
	}
	if budget <= 0 {
		truncated = true
	}
	return commands, truncated
}

func init() {
	registerProvider(&cursorProvider{})
}
