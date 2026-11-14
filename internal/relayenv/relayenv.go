package relayenv

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

var errReference = errors.New("value refers to another variable with $; write the literal value")

// Path returns the relay.env location, or "" when neither variable is set.
func Path() string {
	if filename := strings.TrimSpace(os.Getenv("HERDR_RELAY_ENV")); filename != "" {
		return filename
	}
	if directory := strings.TrimSpace(os.Getenv("HERDR_PLUGIN_CONFIG_DIR")); directory != "" {
		return filepath.Join(directory, "relay.env")
	}
	return ""
}

// Read returns the literal values of the requested keys found in the file at
// Path. A missing or unreadable file yields no values. The last assignment of a
// key wins.
func Read(keys ...string) (map[string]string, error) {
	values := make(map[string]string, len(keys))
	filename := Path()
	if filename == "" {
		return values, nil
	}
	data, err := os.ReadFile(filename)
	if err != nil {
		return values, nil
	}
	raw := make(map[string]string, len(keys))
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		line = strings.TrimSpace(strings.TrimPrefix(line, "export "))
		key, value, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		raw[strings.TrimSpace(key)] = strings.TrimSpace(value)
	}
	for _, key := range keys {
		value, present := raw[key]
		if !present {
			continue
		}
		parsed, err := literalValue(value)
		if err != nil {
			return nil, fmt.Errorf("%s in %s: %w", key, filename, err)
		}
		values[key] = parsed
	}
	return values, nil
}

func literalValue(value string) (string, error) {
	if value == "" {
		return "", nil
	}
	quote := value[0]
	if quote != '\'' && quote != '"' {
		value, _, _ = strings.Cut(value, " #")
		value, _, _ = strings.Cut(value, "\t#")
		value = strings.TrimSpace(value)
		if strings.Contains(value, "$") {
			return "", errReference
		}
		return value, nil
	}
	closing := strings.IndexByte(value[1:], quote)
	if closing < 0 {
		return "", fmt.Errorf("unterminated %c quote", quote)
	}
	literal := value[1 : closing+1]
	rest := strings.TrimSpace(value[closing+2:])
	if rest != "" && !strings.HasPrefix(rest, "#") {
		return "", fmt.Errorf("unexpected text after the closing %c quote", quote)
	}
	if quote == '"' && strings.Contains(literal, "$") {
		return "", errReference
	}
	return literal, nil
}
