package slashcmd

import (
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"go.yaml.in/yaml/v3"
)

func readCursorSkillMetadata(root, skillDir string, project bool) (map[string]string, bool) {
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
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() {
		return nil, false
	}
	data, err := io.ReadAll(io.LimitReader(file, maxMetadataSize+1))
	if err != nil || len(data) > maxMetadataSize {
		return nil, false
	}
	return parseCursorSkillMetadata(data)
}

func parseCursorSkillMetadata(data []byte) (map[string]string, bool) {
	metadata := make(map[string]string)
	lines := strings.Split(strings.ReplaceAll(string(data), "\r\n", "\n"), "\n")
	if len(lines) == 0 || strings.TrimSpace(lines[0]) != "---" {
		return metadata, true
	}
	end := len(lines)
	for i := 1; i < len(lines); i++ {
		if lines[i] == "---" {
			end = i
			break
		}
	}
	var fields map[string]any
	if err := yaml.Unmarshal([]byte(strings.Join(lines[1:end], "\n")), &fields); err != nil {
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
