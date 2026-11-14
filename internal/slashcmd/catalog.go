package slashcmd

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"regexp"

	"github.com/0cv/herdr-mobile-relay/internal/agentkind"
)

const (
	// maxCustomFiles is the per-request discovery budget for custom command and
	// skill files. It bounds filesystem work independently of the serialized
	// catalog size.
	maxCustomFiles     = 2000
	maxEntries         = 4096
	maxMetadataSize    = 64 * 1024
	maxCommandFileSize = 1 << 20
	maxCatalogReadSize = 16 << 20
)

var commandNamePattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:-]{0,119}$`)

type Command struct {
	Command      string `json:"command"`
	Description  string `json:"description"`
	Source       string `json:"source"`
	ArgumentHint string `json:"argument_hint,omitempty"`
}

type Provenance struct {
	Path    string `json:"path"`
	Source  string `json:"source"`
	Scope   string `json:"scope"`
	Origin  string `json:"origin"`
	BaseDir string `json:"base_dir,omitempty"`
}

type Metadata struct {
	Kind       string      `json:"kind"`
	Provenance *Provenance `json:"provenance,omitempty"`
}

type RuntimeCommand struct {
	Command
	Metadata
}

type Catalog struct {
	Commands  []Command           `json:"commands"`
	Truncated bool                `json:"truncated"`
	Status    string              `json:"status,omitempty"`
	Revision  string              `json:"revision,omitempty"`
	Metadata  map[string]Metadata `json:"metadata,omitempty"`
}

func Revise(catalog Catalog) Catalog {
	catalog.Revision = ""
	data, _ := json.Marshal(catalog)
	hash := sha256.Sum256(data)
	catalog.Revision = hex.EncodeToString(hash[:])
	return catalog
}

func IsPi(profile, agent string) bool {
	provider := resolveProvider(profile)
	if provider == nil {
		provider = resolveProvider(agentkind.Of(agent))
	}
	return provider != nil && provider.ID() == "pi"
}

func MergePiRuntime(entries []RuntimeCommand, status string, truncated bool) Catalog {
	commands := append([]Command(nil), piBuiltins...)
	metadata := make(map[string]Metadata)
	seen := make(map[string]bool)
	for _, command := range commands {
		seen[command.Command] = true
		metadata[command.Command] = Metadata{Kind: "builtin"}
	}
	for _, entry := range entries {
		if seen[entry.Command.Command] {
			continue
		}
		seen[entry.Command.Command] = true
		if len(commands) == maxEntries {
			truncated = true
			continue
		}
		commands = append(commands, entry.Command)
		metadata[entry.Command.Command] = entry.Metadata
	}
	return Revise(Catalog{Commands: commands, Metadata: metadata, Status: status, Truncated: truncated})
}

func CatalogFor(agent, cwd, home string) Catalog {
	return CatalogForProfile(agentkind.Of(agent), agent, cwd, home, nil, "", "")
}

func CatalogForProfile(
	profileID, reportedAgent, cwd, home string,
	skillDirs []string,
	commandFormat, agentDir string,
) Catalog {
	return CatalogForProfileWithSuppression(
		profileID, reportedAgent, cwd, home,
		skillDirs, commandFormat, agentDir, false,
	)
}

func CatalogForProfileWithSuppression(
	profileID, reportedAgent, cwd, home string,
	skillDirs []string,
	commandFormat, agentDir string,
	suppressNative bool,
) Catalog {
	var commands []Command
	var truncated bool

	ctx := DiscoverContext{
		AgentDir:       agentDir,
		Cwd:            cwd,
		Home:           home,
		SkillDirs:      skillDirs,
		CommandFormat:  commandFormat,
		SuppressNative: suppressNative,
	}

	p := resolveProvider(profileID)
	if p == nil && reportedAgent != "" {
		p = resolveProvider(agentkind.Of(reportedAgent))
	}

	if p != nil {
		commands, truncated = p.Discover(ctx)
	} else {
		commands, truncated = discoverGenericSkills(skillDirs, commandFormat)
	}

	return finalizeCatalog(commands, truncated)
}

func finalizeCatalog(commands []Command, truncated bool) Catalog {
	if len(commands) > maxEntries {
		commands = commands[:maxEntries]
		truncated = true
	}
	return Revise(Catalog{Commands: commands, Truncated: truncated, Status: "available"})
}
