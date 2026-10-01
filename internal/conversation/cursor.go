package conversation

import (
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
)

var multiDash = regexp.MustCompile(`-+`)

// cursorProjectSlug mirrors Cursor's on-disk project directory naming under
// ~/.cursor/projects: strip a leading slash, replace '/' and '.' with '-',
// collapse repeated dashes, and trim edge dashes. Verified against Cursor
// Agent (e.g. /Users/christophe.vidal/Documents → Users-christophe-vidal-Documents).
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
	trimmed = strings.ReplaceAll(trimmed, ".", "-")
	trimmed = strings.ReplaceAll(trimmed, string(filepath.Separator), "-")
	trimmed = multiDash.ReplaceAllString(trimmed, "-")
	return strings.Trim(trimmed, "-")
}

func cursorTranscriptRels(sessionID string) []string {
	id := strings.ToLower(strings.TrimSpace(sessionID))
	return []string{
		filepath.Join("agent-transcripts", id, id+".jsonl"), // nested (current)
		filepath.Join("agent-transcripts", id+".jsonl"),     // flat (older)
	}
}

func findCursorSession(roots []string, project ProjectContext, sessionID string) Location {
	if !canonicalSessionID.MatchString(sessionID) {
		return Location{}
	}
	rels := cursorTranscriptRels(sessionID)
	candidates := projectDirectoriesForContext(project)

	// Try direct folder-name lookup first (fastest path).
	seenSlug := make(map[string]bool)
	for _, root := range roots {
		for _, cwd := range candidates {
			slug := cursorProjectSlug(cwd)
			if slug == "" || seenSlug[slug] {
				continue
			}
			seenSlug[slug] = true
			if location := cursorTranscriptInProject(root, slug, rels); location.Path != "" {
				return location
			}
		}
	}

	// Fall back to .workspace-trusted discovery if folder name didn't match.
	for _, root := range roots {
		for _, cwd := range candidates {
			if location := findCursorByWorkspaceTrusted(root, cwd, rels); location.Path != "" {
				return location
			}
		}
	}

	// Final fallback: scan all projects by UUID.
	for _, root := range roots {
		for _, rel := range rels {
			if location := scanCursorSession(root, rel); location.Path != "" {
				return location
			}
		}
	}
	return Location{}
}

func findCursorByWorkspaceTrusted(root, cwd string, rels []string) Location {
	cwd = strings.TrimSpace(cwd)
	if cwd == "" {
		return Location{}
	}

	// Resolve symlinks once for the target path.
	resolvedCWD, errCWD := filepath.EvalSymlinks(cwd)

	entries, err := os.ReadDir(root)
	if err != nil {
		return Location{}
	}
	for _, entry := range entries {
		projectDir := filepath.Join(root, entry.Name())
		if !isDir(projectDir) {
			continue
		}
		trustedPath := containedRegularFile(filepath.Join(projectDir, ".workspace-trusted"), root)
		if trustedPath == "" {
			continue
		}
		data, err := os.ReadFile(trustedPath)
		if err != nil {
			continue
		}
		var payload struct {
			WorkspacePath string `json:"workspacePath"`
		}
		if json.Unmarshal(data, &payload) != nil {
			continue
		}
		recorded := strings.TrimSpace(payload.WorkspacePath)
		if recorded == "" {
			continue
		}

		// Compare paths: direct match or resolved match.
		match := (cwd == recorded)
		if !match && errCWD == nil {
			resolvedRecorded, errRecorded := filepath.EvalSymlinks(recorded)
			match = (errRecorded == nil && resolvedCWD == resolvedRecorded)
		}

		if !match {
			continue
		}
		if location := cursorTranscriptInProject(root, entry.Name(), rels); location.Path != "" {
			return location
		}
	}
	return Location{}
}

func cursorTranscriptInProject(root, projectName string, rels []string) Location {
	for _, rel := range rels {
		path := filepath.Join(root, projectName, rel)
		if found := containedRegularFile(path, root); found != "" {
			return Location{Path: found, Root: root}
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

// humanCursorText keeps only explicit <user_query> bodies. User records without
// that envelope are harness-injected (subagent/system notifications) and must
// stay hidden, matching Claude/Codex filtering of their injected envelopes.
func humanCursorText(raw string) string {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return ""
	}
	return innerTag(trimmed, "user_query")
}

func cursorTimestampFromRecord(record map[string]any) string {
	role := strings.ToLower(strings.TrimSpace(stringValue(record["role"])))
	if role != "user" {
		return ""
	}
	message, ok := record["message"].(map[string]any)
	if !ok {
		return ""
	}
	content := message["content"]
	var raw string
	if s, ok := content.(string); ok {
		raw = innerTag(s, "timestamp")
	} else {
		for _, block := range textBlockList(content) {
			if stamp := innerTag(block, "timestamp"); stamp != "" {
				raw = stamp
				break
			}
		}
	}
	if raw == "" {
		return ""
	}
	return normalizeCursorTimestamp(raw)
}

// normalizeCursorTimestamp parses Cursor's <timestamp> format and returns RFC 3339 UTC,
// or empty string if parsing fails. Cursor emits "Weekday, Mon DD, YYYY, H:MM AM/PM (UTC±N)"
// but JavaScript Date() constructors ignore parenthesized timezone suffixes, so we must
// parse the offset explicitly and convert to UTC.
func normalizeCursorTimestamp(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}

	// Extract timezone offset from parentheses at end: (UTC) or (UTC+2) or (UTC-5:30)
	offsetStr := ""
	if idx := strings.LastIndex(raw, "("); idx >= 0 {
		if end := strings.Index(raw[idx:], ")"); end >= 0 {
			offsetStr = raw[idx+1 : idx+end]
			raw = strings.TrimSpace(raw[:idx])
		}
	}

	// Parse offset: "UTC", "UTC+2", "UTC-5:30", etc.
	offsetMinutes := 0
	if offsetStr != "" && strings.HasPrefix(offsetStr, "UTC") {
		offset := strings.TrimPrefix(offsetStr, "UTC")
		if offset != "" {
			sign := 1
			if strings.HasPrefix(offset, "-") {
				sign = -1
				offset = offset[1:]
			} else if strings.HasPrefix(offset, "+") {
				offset = offset[1:]
			}

			parts := strings.Split(offset, ":")
			if len(parts) > 0 {
				if hours, err := strconv.Atoi(parts[0]); err == nil {
					offsetMinutes = sign * hours * 60
					if len(parts) > 1 {
						if mins, err := strconv.Atoi(parts[1]); err == nil {
							offsetMinutes += sign * mins
						}
					}
				}
			}
		}
	}

	// Parse the date/time portion. Try common layouts.
	// Example: "Tuesday, Sep 22, 2026, 2:35 PM"
	layouts := []string{
		"Monday, Jan 2, 2006, 3:04 PM",
		"Monday, Jan 02, 2006, 3:04 PM",
		"Monday, Jan 2, 2006, 15:04",
		"Monday, Jan 02, 2006, 15:04",
	}

	var t time.Time
	var err error
	for _, layout := range layouts {
		if t, err = time.Parse(layout, raw); err == nil {
			break
		}
	}

	if err != nil {
		return ""
	}

	// Apply the offset by subtracting it (to get UTC)
	t = t.Add(-time.Duration(offsetMinutes) * time.Minute)

	// Return as RFC 3339 in UTC
	return t.UTC().Format(time.RFC3339)
}
