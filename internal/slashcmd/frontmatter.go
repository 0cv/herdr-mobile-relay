package slashcmd

import (
	"os"
	"regexp"
	"strings"
)

var frontmatterKeyPattern = regexp.MustCompile(`^([A-Za-z0-9_-]+):\s*(.*?)\s*$`)

func parseFrontmatterBytes(data []byte) (map[string]string, bool) {
	lines := strings.Split(strings.ReplaceAll(string(data), "\r\n", "\n"), "\n")
	if len(lines) == 0 || strings.TrimSpace(lines[0]) != "---" {
		return map[string]string{}, true
	}
	result := make(map[string]string)
	for _, line := range lines[1:] {
		if strings.TrimSpace(line) == "---" {
			return result, true
		}
		matches := frontmatterKeyPattern.FindStringSubmatch(line)
		if matches == nil {
			continue
		}
		key := strings.ToLower(matches[1])
		value := matches[2]
		if len(value) >= 2 && value[0] == value[len(value)-1] && (value[0] == '\'' || value[0] == '"') {
			value = value[1 : len(value)-1]
		}
		result[key] = value
	}
	return result, true
}

func readSkillMetadata(path string) (map[string]string, bool) {
	data, err := os.ReadFile(path)
	if err != nil || len(data) > maxMetadataSize {
		return nil, false
	}
	return parseFrontmatterBytes(data)
}
