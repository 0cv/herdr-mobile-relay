package slashcmd

import (
	"regexp"
	"strings"
)

const (
	maxEntries      = 300
	maxCustomFiles  = 250
	maxMetadataSize = 64 * 1024
)

var commandNamePattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:-]{0,119}$`)

type Command struct {
	Command      string `json:"command"`
	Description  string `json:"description"`
	Source       string `json:"source"`
	ArgumentHint string `json:"argument_hint,omitempty"`
}

type Catalog struct {
	Commands  []Command `json:"commands"`
	Truncated bool      `json:"truncated"`
}

func CatalogFor(agent, cwd, home string) Catalog {
	agentLower := strings.ToLower(strings.TrimSpace(agent))
	var profileID string
	switch agentLower {
	case "claude", "claude-code", "claude code":
		profileID = "claude"
	case "codex":
		profileID = "codex"
	case "qoder", "qodercli":
		profileID = "qoder"
	}
	return CatalogForProfile(profileID, agent, cwd, home, nil, "")
}

func CatalogForProfile(profileID, reportedAgent, cwd, home string, skillDirs []string, commandFormat string) Catalog {
	var commands []Command
	var truncated bool

	ctx := DiscoverContext{
		Cwd:           cwd,
		Home:          home,
		SkillDirs:     skillDirs,
		CommandFormat: commandFormat,
	}

	if p := resolveProvider(profileID); p != nil {
		commands, truncated = p.Discover(ctx)
	} else {
		commands, truncated = discoverGenericSkills(skillDirs, commandFormat)
	}

	if len(commands) > maxEntries {
		commands = commands[:maxEntries]
		truncated = true
	}

	return Catalog{Commands: commands, Truncated: truncated}
}
