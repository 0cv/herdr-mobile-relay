package conversation

import (
	"net/url"
	"os"
	"path/filepath"
	"strings"
)

// grokTranscriptName is the per-session transcript Grok CLI appends to under
// <root>/<url-escaped cwd>/<session id>/.
const grokTranscriptName = "chat_history.jsonl"

// findGrokSession tries the project directory for each known cwd first, then
// scans every project directory, because the session id alone is unique and
// Grok's cwd escaping is not guaranteed to match url.PathEscape exactly.
func findGrokSession(roots []string, project ProjectContext, sessionID string) Location {
	for _, root := range roots {
		seen := make(map[string]bool)
		try := func(projectDir string) Location {
			projectDir = filepath.Clean(projectDir)
			if seen[projectDir] {
				return Location{}
			}
			seen[projectDir] = true
			if path := containedRegularFile(filepath.Join(projectDir, sessionID, grokTranscriptName), root); path != "" {
				return Location{Path: path, Root: root}
			}
			return Location{}
		}
		for _, cwd := range projectDirectoriesForContext(project) {
			if strings.TrimSpace(cwd) == "" {
				continue
			}
			if location := try(filepath.Join(root, url.PathEscape(cwd))); location.Path != "" {
				return location
			}
		}
		entries, err := os.ReadDir(root)
		if err != nil {
			continue
		}
		for _, entry := range entries {
			projectDir := filepath.Join(root, entry.Name())
			if !isDir(projectDir) {
				continue
			}
			if location := try(projectDir); location.Path != "" {
				return location
			}
		}
	}
	return Location{}
}

// parseGrokRecord keeps real user prompts and assistant replies. Grok marks
// injected user turns (skill lists, task notifications) with synthetic_reason,
// and its leading environment turn carries no prompt_index.
func parseGrokRecord(record map[string]any) (string, string) {
	switch stringValue(record["type"]) {
	case "user":
		if _, prompt := record["prompt_index"]; !prompt || stringValue(record["synthetic_reason"]) != "" {
			return "", ""
		}
		text := textBlocks(record["content"])
		if query := innerTag(text, "user_query"); query != "" {
			return "user", query
		}
		return "user", text
	case "assistant":
		return "assistant", textBlocks(record["content"])
	default:
		return "", ""
	}
}

func grokToolActivity(record map[string]any) ([]ToolActivity, []toolResult) {
	switch stringValue(record["type"]) {
	case "assistant":
		rawCalls, _ := record["tool_calls"].([]any)
		calls := make([]ToolActivity, 0, len(rawCalls))
		for _, raw := range rawCalls {
			call, ok := raw.(map[string]any)
			if !ok {
				continue
			}
			calls = append(calls, newToolActivity(
				firstString(call, "id", "call_id"),
				firstString(call, "name"),
				firstValue(call, "arguments", "input"),
			))
		}
		return calls, nil
	case "tool_result":
		return nil, []toolResult{{
			id:     strings.TrimSpace(firstString(record, "tool_call_id", "id")),
			output: textValue(record["content"]),
			failed: record["is_error"] == true,
		}}
	default:
		return nil, nil
	}
}
