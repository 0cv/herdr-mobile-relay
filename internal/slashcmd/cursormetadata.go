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
		err = yaml.Unmarshal(frontmatter, &fields)
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

func cursorSkillAllowsCLI(fields map[string]any) bool {
	metadata, _ := fields["metadata"].(map[string]any)
	var surfaces []string
	switch value := metadata["surfaces"].(type) {
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
