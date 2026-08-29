package slashcmd

import (
	"regexp"
	"strconv"
	"strings"
)

// kimiSkillSettings holds the subset of Kimi Code's config.toml that decides
// which skill directories become /skill:<name> commands. Kimi 0.29.2 documents
// no per-skill disable and no switch for the skill commands themselves, so
// there is no ban list here.
type kimiSkillSettings struct {
	// mergeAllAvailableSkills selects a candidate group's semantics: true uses
	// every directory in the group, false only the first that exists.
	mergeAllAvailableSkills bool
	extraSkillDirs          []string
}

// defaultKimiSkillSettings returns Kimi's own defaults: merge everything, no
// extra directories.
func defaultKimiSkillSettings() kimiSkillSettings {
	return kimiSkillSettings{mergeAllAvailableSkills: true}
}

var kimiTOMLKeyPattern = regexp.MustCompile(`^([A-Za-z0-9_.-]+)[ \t]*=[ \t]*(.*)$`)

// parseKimiSkillSettings applies the root-table skill settings from config.toml.
// It accepts both compact and multiline arrays while ignoring comments and
// quoted brackets.
func parseKimiSkillSettings(data []byte, settings *kimiSkillSettings) {
	root := true
	lines := strings.Split(strings.ReplaceAll(string(data), "\r\n", "\n"), "\n")
	for index := 0; index < len(lines); index++ {
		trimmed := strings.TrimSpace(lines[index])
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}
		if strings.HasPrefix(trimmed, "[") {
			root = false
			continue
		}
		if !root {
			continue
		}
		matches := kimiTOMLKeyPattern.FindStringSubmatch(trimmed)
		if matches == nil {
			continue
		}
		value := strings.TrimSpace(matches[2])
		switch matches[1] {
		case "merge_all_available_skills":
			setScalarBool(&settings.mergeAllAvailableSkills, value)
		case "extra_skill_dirs":
			for kimiArrayEnd(value) < 0 && index+1 < len(lines) {
				index++
				value += "\n" + lines[index]
			}
			setKimiList(&settings.extraSkillDirs, value)
		}
	}
}

// setKimiList assigns a TOML array. List keys replace rather than append
// across config files; malformed or unterminated arrays leave the key intact.
func setKimiList(target *[]string, value string) {
	if items, ok := kimiArrayValue(value); ok {
		*target = items
	}
}

// kimiArrayValue unquotes and trims TOML string items, dropping empty ones.
// An empty array clears the key.
func kimiArrayValue(value string) ([]string, bool) {
	if !strings.HasPrefix(strings.TrimSpace(value), "[") {
		return nil, false
	}
	end := kimiArrayEnd(value)
	if end < 0 {
		return nil, false
	}
	inner := value[1:end]
	fields, ok := splitKimiArray(inner)
	if !ok {
		return nil, false
	}
	items := make([]string, 0, len(fields))
	for _, field := range fields {
		if item, ok := unquoteKimiString(field); ok && item != "" {
			items = append(items, item)
		} else if !ok {
			return nil, false
		}
	}
	return items, true
}

// kimiArrayEnd returns the closing bracket outside strings and comments.
func kimiArrayEnd(value string) int {
	quote := byte(0)
	for index := 1; index < len(value); index++ {
		char := value[index]
		if quote != 0 {
			if quote == '"' && char == '\\' && index+1 < len(value) {
				index++
				continue
			}
			if char == quote {
				quote = 0
			}
			continue
		}
		switch char {
		case '\'', '"':
			quote = char
		case '#':
			if newline := strings.IndexByte(value[index:], '\n'); newline >= 0 {
				index += newline
			} else {
				return -1
			}
		case ']':
			return index
		}
	}
	return -1
}

func splitKimiArray(value string) ([]string, bool) {
	var fields []string
	start := 0
	quote := byte(0)
	for index := 0; index < len(value); index++ {
		char := value[index]
		if quote != 0 {
			if quote == '"' && char == '\\' && index+1 < len(value) {
				index++
				continue
			}
			if char == quote {
				quote = 0
			}
			continue
		}
		switch char {
		case '\'', '"':
			quote = char
		case '#':
			end := strings.IndexByte(value[index:], '\n')
			if end < 0 {
				value = value[:index]
				index = len(value)
				continue
			}
			value = value[:index] + value[index+end:]
			index--
		case ',':
			fields = append(fields, value[start:index])
			start = index + 1
		}
	}
	if quote != 0 {
		return nil, false
	}
	fields = append(fields, value[start:])
	return fields, true
}

func unquoteKimiString(value string) (string, bool) {
	value = strings.TrimSpace(value)
	if value == "" {
		return "", true
	}
	if value[0] == '\'' {
		if len(value) < 2 || value[len(value)-1] != '\'' {
			return "", false
		}
		return value[1 : len(value)-1], true
	}
	if value[0] != '"' {
		return "", false
	}
	unquoted, err := strconv.Unquote(value)
	return strings.TrimSpace(unquoted), err == nil
}
