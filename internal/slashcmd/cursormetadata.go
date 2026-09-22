package slashcmd

import (
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"go.yaml.in/yaml/v3"
)

const maxCursorFileSize = 1 << 20

func readCursorCommandFile(path string) ([]byte, bool) {
	file, err := os.Open(path)
	if err != nil {
		return nil, false
	}
	defer file.Close()
	return readCursorFile(file, maxCursorFileSize)
}

func readCursorSkillFile(root, skillDir string, project bool) ([]byte, bool) {
	skillFile := filepath.Join(skillDir, "SKILL.md")
	var file *os.File
	var err error
	if project {
		realRoot, err := filepath.EvalSymlinks(root)
		if err != nil {
			return nil, false
		}
		realFile, err := filepath.EvalSymlinks(skillFile)
		if err != nil {
			return nil, false
		}
		relative, err := filepath.Rel(realRoot, realFile)
		if err != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
			return nil, false
		}
		file, err = os.OpenInRoot(realRoot, relative)
		if err != nil {
			return nil, false
		}
	} else {
		file, err = os.Open(skillFile)
		if err != nil {
			return nil, false
		}
	}
	defer file.Close()
	return readCursorFile(file, maxCursorFileSize)
}

func readCursorFile(file *os.File, maxSize int64) ([]byte, bool) {
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Size() == 0 || info.Size() > maxSize {
		return nil, false
	}
	data, err := io.ReadAll(io.LimitReader(file, maxSize+1))
	if err != nil || len(data) == 0 || int64(len(data)) > maxSize {
		return nil, false
	}
	return data, true
}

func parseCursorSkillMetadata(data []byte) (map[string]string, bool) {
	metadata := make(map[string]string)
	content := strings.TrimPrefix(string(data), "\ufeff")
	lines := strings.Split(strings.ReplaceAll(content, "\r\n", "\n"), "\n")
	language, ok := strings.CutPrefix(lines[0], "---")
	if !ok || strings.HasPrefix(language, "-") {
		return metadata, true
	}
	end := len(lines)
	empty := true
	for i := 1; i < len(lines); i++ {
		if strings.HasPrefix(lines[i], "---") {
			end = i
			break
		}
		line := strings.TrimSpace(lines[i])
		if line != "" && !strings.HasPrefix(line, "#") {
			empty = false
		}
	}
	if empty {
		return metadata, true
	}
	frontmatter := []byte(strings.Join(lines[1:end], "\n"))
	language = strings.TrimSpace(language)
	var fields map[string]any
	var err error
	switch strings.ToLower(language) {
	case "", "yaml", "yml":
		var document yaml.Node
		if err := yaml.Unmarshal(frontmatter, &document); err != nil || !cursorYAMLTagsSupported(&document) {
			return nil, false
		}
		err = document.Decode(&fields)
	case "js", "javascript":
		return metadata, true
	default:
		if language != "json" {
			return nil, false
		}
		err = json.Unmarshal(frontmatter, &fields)
	}
	if err != nil {
		return nil, false
	}
	if !cursorSkillAllowsCLI(fields) {
		return nil, false
	}
	for key, value := range fields {
		if text, ok := value.(string); ok {
			metadata[key] = text
		}
	}
	if invocable, ok := fields["user-invocable"].(bool); ok && !invocable {
		metadata["user-invocable"] = "false"
	}
	return metadata, true
}

func cursorYAMLTagsSupported(node *yaml.Node) bool {
	switch node.Kind {
	case yaml.ScalarNode, yaml.SequenceNode, yaml.MappingNode:
		switch node.ShortTag() {
		case "!!str", "!!bool", "!!int", "!!float", "!!null", "!!timestamp", "!!binary", "!!map", "!!seq", "!!omap", "!!pairs", "!!set", "!!merge":
		default:
			return false
		}
	}
	for _, child := range node.Content {
		if !cursorYAMLTagsSupported(child) {
			return false
		}
	}
	return true
}

func cursorSkillAllowsCLI(fields map[string]any) bool {
	var rawSurfaces any
	switch metadata := fields["metadata"].(type) {
	case map[string]any:
		rawSurfaces = metadata["surfaces"]
	case map[any]any:
		rawSurfaces = metadata["surfaces"]
	}
	var surfaces []string
	switch value := rawSurfaces.(type) {
	case string:
		for _, surface := range strings.Split(value, ",") {
			if surface = strings.TrimSpace(surface); surface != "" {
				surfaces = append(surfaces, surface)
			}
		}
	case []any:
		for _, surface := range value {
			if text, ok := surface.(string); ok {
				surfaces = append(surfaces, text)
			}
		}
	}
	return len(surfaces) == 0 || slices.Contains(surfaces, "cli")
}
