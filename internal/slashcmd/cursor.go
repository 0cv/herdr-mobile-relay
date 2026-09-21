package slashcmd

import (
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

func (p *cursorProvider) Discover(ctx DiscoverContext) ([]Command, bool) {
	if ctx.SuppressNative {
		builtins := make([]Command, len(cursorBuiltins))
		copy(builtins, cursorBuiltins)
		return builtins, false
	}

	budget := maxCustomFiles
	truncated := false
	active := make(map[string]Command, len(cursorBuiltins))
	order := make([]string, 0, len(cursorBuiltins))
	apply := func(commands []Command) {
		for _, command := range commands {
			if _, exists := active[command.Command]; !exists {
				order = append(order, command.Command)
			}
			active[command.Command] = command
		}
	}

	// Precedence follows the Claude provider: a personal skill wins over a
	// project skill of the same name, because personal roots are applied last.
	// Cursor's loader sets each skill by id as it walks its roots, so the last
	// root to define a name is what its palette resolves; this repo's Claude
	// provider documents the same personal-over-project rule, and matching it
	// keeps one mental model for both. Builtins are applied first so any skill
	// of the same name overrides them.
	apply(cursorBuiltins)

	if ctx.Cwd != "" {
		for _, dir := range findProjectDirs(ctx.Cwd, []string{".cursor", ".agents"}) {
			skillsDir := filepath.Join(dir, "skills")
			cmds, trunc := scanCursorSkillDirBudget(skillsDir, "project", &budget)
			apply(cmds)
			truncated = truncated || trunc
		}
	}

	for _, root := range cursorSkillRoots(ctx.Home) {
		cmds, trunc := scanCursorSkillDirBudget(root, "personal", &budget)
		apply(cmds)
		truncated = truncated || trunc
	}

	// Additional configured skill dirs from agent-profiles.ini. Cursor has no
	// project command tree of its own, so this stays the documented escape
	// hatch for pointing the palette at a directory outside the roots above.
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
