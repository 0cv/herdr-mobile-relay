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

// profileIDForAgentName maps an agent name as herdr reports it onto a provider
// profile ID. Both entrypoints resolve through this one table on purpose: while
// they carried separate switches the two drifted, and a kimi or opencode pane
// whose binary was missing from the relay's PATH fell through to the generic
// path and got an empty palette - not even builtins.
func profileIDForAgentName(agent string) string {
	switch strings.ToLower(strings.TrimSpace(agent)) {
	case "claude", "claude-code", "claude code":
		return "claude"
	case "codex":
		return "codex"
	case "qoder", "qodercli":
		return "qoder"
	case "pi", "pi-coding-agent":
		return "pi"
	case "omp", "oh my pi", "oh-my-pi":
		return "omp"
	case "kimi", "kimi code", "kimi-code", "kimi-cli":
		return "kimi"
	case "opencode", "open code", "open-code":
		return "opencode"
	}
	return ""
}

func CatalogFor(agent, cwd, home string) Catalog {
	return CatalogForProfile(profileIDForAgentName(agent), agent, cwd, home, nil, "", "")
}

func CatalogForProfile(
	profileID, reportedAgent, cwd, home string,
	skillDirs []string,
	commandFormat, agentVersion string,
) Catalog {
	var commands []Command
	var truncated bool

	ctx := DiscoverContext{
		Cwd:           cwd,
		Home:          home,
		SkillDirs:     skillDirs,
		CommandFormat: commandFormat,
		AgentVersion:  agentVersion,
	}

	p := resolveProvider(profileID)
	if p == nil && reportedAgent != "" {
		p = resolveProvider(profileIDForAgentName(reportedAgent))
	}

	if p != nil {
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
