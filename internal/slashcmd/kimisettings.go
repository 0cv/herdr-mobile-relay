package slashcmd

import (
	"strconv"
	"strings"
)

// kimiSkillSettings holds the standalone Kimi Code config.toml field that
// changes which directories become /skill:<name> commands.
type kimiSkillSettings struct {
	extraSkillDirs []string
}

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
		key, value, ok := splitKimiAssignment(trimmed)
		if !ok {
			continue
		}
		if key != "extra_skill_dirs" {
			continue
		}
		for kimiArrayEnd(value) < 0 && index+1 < len(lines) {
			index++
			value += "\n" + lines[index]
		}
		setKimiList(&settings.extraSkillDirs, value)
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

// scanKimi walks value from index from, skipping comments, and calls visit for
// every other byte with whether it belongs to a quoted string. It stops when
// visit returns true and reports that index (or -1) and whether all quotes closed.
func scanKimi(value string, from int, visit func(index int, quoted bool) bool) (int, bool) {
	var quote byte
	escaped := false
	for index := from; index < len(value); index++ {
		char := value[index]
		if quote == 0 && char == '#' {
			newline := strings.IndexByte(value[index:], '\n')
			if newline < 0 {
				return -1, true
			}
			index += newline - 1
			continue
		}
		if visit(index, quote != 0 || char == '\'' || char == '"') {
			return index, true
		}
		switch {
		case escaped:
			escaped = false
		case quote == '"' && char == '\\':
			escaped = true
		case quote != 0 && char == quote:
			quote = 0
		case quote == 0 && (char == '\'' || char == '"'):
			quote = char
		}
	}
	return -1, quote == 0
}

// kimiArrayEnd returns the closing bracket outside strings and comments.
func kimiArrayEnd(value string) int {
	end, _ := scanKimi(value, 1, func(index int, quoted bool) bool {
		return !quoted && value[index] == ']'
	})
	return end
}

func splitKimiArray(value string) ([]string, bool) {
	var fields []string
	var field strings.Builder
	_, closed := scanKimi(value, 0, func(index int, quoted bool) bool {
		if quoted || value[index] != ',' {
			field.WriteByte(value[index])
			return false
		}
		fields = append(fields, field.String())
		field.Reset()
		return false
	})
	if !closed {
		return nil, false
	}
	return append(fields, field.String()), true
}

func splitKimiAssignment(line string) (key, value string, ok bool) {
	equals, _ := scanKimi(line, 0, func(index int, quoted bool) bool {
		return !quoted && line[index] == '='
	})
	if equals < 0 {
		return "", "", false
	}
	rawKey := strings.TrimSpace(line[:equals])
	value = strings.TrimSpace(line[equals+1:])
	if rawKey == "" {
		return "", "", false
	}
	if rawKey[0] == '\'' || rawKey[0] == '"' {
		key, ok = unquoteKimiString(rawKey)
		if !ok {
			return "", "", false
		}
		return key, value, true
	}
	for _, current := range rawKey {
		if !(current == '_' || current == '-' || current >= 'A' && current <= 'Z' ||
			current >= 'a' && current <= 'z' || current >= '0' && current <= '9') {
			return "", "", false
		}
	}
	return rawKey, value, true
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
	return unquoted, err == nil
}
