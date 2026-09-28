package conversation

import (
	"os"
	"path/filepath"
	"strings"
)

// cursorProjectSlug mirrors Cursor's on-disk project directory naming under
// ~/.cursor/projects: strip a leading slash, drop '.' characters, then replace
// remaining path separators with '-'. Observed for both normal repos and
// ~/.herdr/worktrees/... layouts.
func cursorProjectSlug(cwd string) string {
	cwd = strings.TrimSpace(cwd)
	if cwd == "" {
		return ""
	}
	cwd = filepath.Clean(cwd)
	if !filepath.IsAbs(cwd) {
		return ""
	}
	trimmed := strings.TrimPrefix(cwd, string(filepath.Separator))
	trimmed = strings.ReplaceAll(trimmed, ".", "")
	return strings.ReplaceAll(trimmed, string(filepath.Separator), "-")
}

func cursorTranscriptRel(sessionID string) string {
	id := strings.ToLower(strings.TrimSpace(sessionID))
	return filepath.Join("agent-transcripts", id, id+".jsonl")
}

func findCursorSession(roots []string, project ProjectContext, sessionID string) Location {
	if !canonicalSessionID.MatchString(sessionID) {
		return Location{}
	}
	rel := cursorTranscriptRel(sessionID)
	candidates := projectDirectoriesForContext(project)
	for _, root := range roots {
		seen := make(map[string]bool)
		for _, cwd := range candidates {
			slug := cursorProjectSlug(cwd)
			if slug == "" || seen[slug] {
				continue
			}
			seen[slug] = true
			path := filepath.Join(root, slug, rel)
			if found := containedRegularFile(path, root); found != "" {
				return Location{Path: found, Root: root}
			}
		}
		if location := scanCursorSession(root, rel); location.Path != "" {
			return location
		}
	}
	return Location{}
}

func scanCursorSession(root, rel string) Location {
	entries, err := os.ReadDir(root)
	if err != nil {
		return Location{}
	}
	for _, entry := range entries {
		projectDir := filepath.Join(root, entry.Name())
		if !isDir(projectDir) {
			continue
		}
		path := filepath.Join(projectDir, rel)
		if found := containedRegularFile(path, root); found != "" {
			return Location{Path: found, Root: root}
		}
	}
	return Location{}
}

func parseCursorRecord(record map[string]any) (string, string) {
	role := strings.ToLower(strings.TrimSpace(stringValue(record["role"])))
	if role != "user" && role != "assistant" {
		return "", ""
	}
	message, ok := record["message"].(map[string]any)
	if !ok {
		return "", ""
	}
	content := message["content"]
	if raw, ok := content.(string); ok {
		if role == "user" {
			return role, humanCursorText(raw)
		}
		return role, raw
	}
	if role == "user" {
		blocks := textBlockList(content)
		kept := make([]string, 0, len(blocks))
		for _, block := range blocks {
			if text := humanCursorText(block); strings.TrimSpace(text) != "" {
				kept = append(kept, text)
			}
		}
		if len(kept) == 0 {
			return "", ""
		}
		return role, strings.Join(kept, "\n")
	}
	return role, textBlocks(content)
}

func humanCursorText(raw string) string {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return ""
	}
	if query := innerTag(trimmed, "user_query"); query != "" {
		return query
	}
	return trimmed
}
