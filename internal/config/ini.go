package config

import (
	"bufio"
	"fmt"
	"io"
	"strings"
)

type INI struct {
	Sections map[string]map[string]string
}

func ParseINI(r io.Reader) (*INI, error) {
	ini := &INI{Sections: make(map[string]map[string]string)}
	current := ""
	ini.Sections[""] = make(map[string]string)

	scanner := bufio.NewScanner(r)
	lineNo := 0
	for scanner.Scan() {
		lineNo++
		line := scanner.Text()

		trimmed := strings.TrimSpace(line)
		if trimmed == "" {
			continue
		}
		if trimmed[0] == '#' || trimmed[0] == ';' {
			continue
		}

		if trimmed[0] == '[' {
			end := strings.IndexByte(trimmed, ']')
			if end < 0 {
				return nil, fmt.Errorf("line %d: unterminated section header", lineNo)
			}
			current = strings.ToLower(strings.TrimSpace(trimmed[1:end]))
			if _, ok := ini.Sections[current]; !ok {
				ini.Sections[current] = make(map[string]string)
			}
			continue
		}

		key, value, found := splitKeyValue(trimmed)
		if !found {
			return nil, fmt.Errorf("line %d: no delimiter found", lineNo)
		}

		key = strings.ToLower(strings.TrimSpace(key))
		value = strings.TrimSpace(value)

		if _, exists := ini.Sections[current][key]; exists {
			return nil, fmt.Errorf("line %d: duplicate key %q in section [%s]", lineNo, key, current)
		}
		ini.Sections[current][key] = value
	}

	return ini, scanner.Err()
}

func splitKeyValue(line string) (key, value string, found bool) {
	for i, ch := range line {
		if ch == '=' || ch == ':' {
			return line[:i], line[i+1:], true
		}
	}
	return "", "", false
}

func (ini *INI) Get(section, key string) (string, bool) {
	sec, ok := ini.Sections[strings.ToLower(section)]
	if !ok {
		return "", false
	}
	v, ok := sec[strings.ToLower(key)]
	return v, ok
}

func (ini *INI) SectionNames() []string {
	names := make([]string, 0, len(ini.Sections))
	for name := range ini.Sections {
		if name != "" {
			names = append(names, name)
		}
	}
	return names
}
