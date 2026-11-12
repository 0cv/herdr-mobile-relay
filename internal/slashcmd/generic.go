package slashcmd

import (
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/0cv/herdr-mobile-relay/internal/textutil"
)

func (reader *catalogReader) discoverGenericSkills(dirs []string, commandFormat string) ([]Command, bool) {
	if commandFormat == "" || strings.Count(commandFormat, "{name}") != 1 {
		return nil, false
	}
	var commands []Command
	seen := make(map[string]bool)
	scanned := 0
	truncated := false
	for index, dir := range dirs {
		source := "project"
		if index == 0 {
			source = "personal"
		}
		entries, err := os.ReadDir(dir)
		if err != nil {
			continue
		}
		sort.Slice(entries, func(i, j int) bool {
			left, right := strings.ToLower(entries[i].Name()), strings.ToLower(entries[j].Name())
			if left == right {
				return entries[i].Name() < entries[j].Name()
			}
			return left < right
		})
		for _, entry := range entries {
			if scanned >= maxCustomFiles || reader.truncated {
				truncated = true
				break
			}
			if !entryIsDir(entry, filepath.Join(dir, entry.Name())) || strings.HasPrefix(entry.Name(), ".") {
				continue
			}
			path := filepath.Join(dir, entry.Name(), "SKILL.md")
			info, err := os.Stat(path)
			if err != nil || !info.Mode().IsRegular() {
				continue
			}
			scanned++
			metadata, ok := reader.readSkillMetadata(path)
			if !ok {
				continue
			}
			name := metadata["name"]
			if !commandNamePattern.MatchString(name) || seen[name] || !userInvocable(metadata) {
				continue
			}
			seen[name] = true
			description := metadata["description"]
			if description == "" {
				description = strings.ToUpper(name[:1]) + name[1:] + " skill"
			}
			commands = append(commands, Command{
				Command:      "/" + strings.TrimPrefix(strings.Replace(commandFormat, "{name}", name, 1), "/"),
				Description:  textutil.Compact(description, 240),
				Source:       source,
				ArgumentHint: textutil.Compact(metadata["argument-hint"], 120),
			})
		}
		if truncated {
			break
		}
	}
	return commands, truncated || reader.truncated
}

func userInvocable(metadata map[string]string) bool {
	switch strings.ToLower(strings.TrimSpace(metadata["user-invocable"])) {
	case "false", "no", "off", "0":
		return false
	default:
		return true
	}
}
