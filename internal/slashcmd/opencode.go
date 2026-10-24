package slashcmd

import (
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

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

func (p *openCodeProvider) Discover(ctx DiscoverContext) ([]Command, bool) {
	reader := newCatalogReader()
	commands := append([]Command(nil), openCodeBuiltins...)
	if ctx.SuppressNative {
		return commands, reader.truncated
	}
	config := os.Getenv("XDG_CONFIG_HOME")
	if !filepath.IsAbs(config) {
		config = filepath.Join(ctx.Home, ".config")
	}
	roots := []string{filepath.Join(config, "opencode")}
	roots = append(roots, findProjectDirs(ctx.Cwd, []string{".opencode"})...)
	indices := make(map[string]int)
	for i, command := range commands {
		indices[command.Command] = i
	}
	budget, visits := maxCustomFiles, maxEntries
	truncated := false
	for i, root := range roots {
		source := "project"
		if i == 0 {
			source = "personal"
		}
		for _, leaf := range []string{"command", "commands"} {
			found, clipped := reader.openCodeCommandDir(filepath.Join(root, leaf), source, &budget, &visits)
			truncated = truncated || clipped
			for _, command := range found {
				index, exists := indices[command.Command]
				if exists {
					if commands[index].Source != "builtin" {
						commands[index] = command
					}
					continue
				}
				indices[command.Command] = len(commands)
				commands = append(commands, command)
			}
		}
	}
	return commands, truncated || reader.truncated
}

var openCodeCommandName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:-]*(/[A-Za-z0-9][A-Za-z0-9._:-]*)*$`)

func (reader *catalogReader) openCodeCommandDir(dir, source string, budget, visits *int) ([]Command, bool) {
	root, err := os.OpenRoot(dir)
	if err != nil {
		return nil, false
	}
	defer root.Close()
	var commands []Command
	truncated := false
	var walk func(string, int)
	walk = func(relative string, depth int) {
		if depth > maxGitWalkDepth || *budget <= 0 || reader.truncated || *visits <= 0 {
			truncated = true
			return
		}
		directory, err := root.Open(relative)
		if err != nil {
			return
		}
		entries, err := directory.ReadDir(*visits + 1)
		directory.Close()
		if err != nil && err != io.EOF {
			return
		}
		sort.Slice(entries, func(i, j int) bool { return entries[i].Name() < entries[j].Name() })
		for _, entry := range entries {
			if *budget <= 0 || reader.truncated || *visits <= 0 {
				truncated = true
				return
			}
			*visits--
			name := filepath.Join(relative, entry.Name())
			if entry.IsDir() {
				walk(name, depth+1)
				continue
			}
			if !entry.Type().IsRegular() || !strings.HasSuffix(name, ".md") {
				continue
			}
			*budget--
			command := filepath.ToSlash(strings.TrimSuffix(name, ".md"))
			if len(command) > 120 || !openCodeCommandName.MatchString(command) {
				continue
			}
			file, err := root.Open(name)
			if err != nil {
				continue
			}
			data, ok := reader.readBoundedCommandFile(file)
			file.Close()
			if !ok {
				continue
			}
			metadata, ok := parseFrontmatterBytes(data)
			if !ok {
				continue
			}
			description := compact(metadata["description"], 240)
			if description == "" {
				description = "Custom OpenCode command"
			}
			commands = append(commands, Command{Command: "/" + command, Description: description, Source: source})
		}
	}
	walk(".", 0)
	return commands, truncated
}

func init() { registerProvider(&openCodeProvider{}) }
