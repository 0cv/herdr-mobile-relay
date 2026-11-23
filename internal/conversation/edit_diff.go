package conversation

import (
	"encoding/json/jsontext"
	"encoding/json/v2"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/0cv/herdr-mobile-relay/internal/agentkind"
)

const (
	editDiffRows  = 200
	editDiffBytes = 8192
	editDiffCells = 250_000
)

type EditDiff struct {
	Kind      string `json:"kind"`
	Path      string `json:"path"`
	Added     int    `json:"added"`
	Removed   int    `json:"removed"`
	Text      string `json:"text"`
	Truncated bool   `json:"truncated"`
}

type editInput struct {
	path, old, new, patch string
	list, write           bool
}

func editInputFor(agent, tool string) editInput {
	switch agent {
	case agentkind.Claude:
		switch tool {
		case "Edit", "MultiEdit":
			return editInput{path: "file_path", old: "old_string", new: "new_string", list: tool == "MultiEdit"}
		case "Write":
			return editInput{path: "file_path", write: true}
		}
	case agentkind.Pi, agentkind.OMP:
		if tool == "write" {
			return editInput{path: "path", write: true}
		}
		if agent == agentkind.Pi && tool == "edit" {
			return editInput{path: "path", old: "oldText", new: "newText"}
		}
	case agentkind.OpenCode:
		switch tool {
		case "edit":
			return editInput{path: "path", old: "oldString", new: "newString"}
		case "write":
			return editInput{path: "path", write: true}
		case "patch", "apply_patch":
			return editInput{patch: "patchText"}
		}
	case agentkind.Hermes:
		switch tool {
		case "write_file":
			return editInput{path: "path", write: true}
		case "patch":
			return editInput{path: "path", old: "old_string", new: "new_string", patch: "patch"}
		}
	case agentkind.Grok:
		if tool == "search_replace" {
			return editInput{path: "file_path", old: "old_string", new: "new_string"}
		}
	}
	return editInput{}
}

func validEditToolJSON(raw []byte) bool {
	return jsontext.Value(raw).IsValid()
}

func (tool ToolActivity) editDiff(agent string, remainingCells *int) *EditDiff {
	return buildEditDiff(agent, tool.Name, tool.Input, tool.Truncated || tool.ambiguousEditInput, remainingCells)
}

func buildEditDiff(agent, tool, input string, truncated bool, remainingCells *int) *EditDiff {
	if truncated || !utf8.ValidString(input) || len(input) > maxEntryBytes/2 {
		return nil
	}
	if agent == agentkind.Codex && tool == "apply_patch" {
		return patchEditDiff(input)
	}
	shape := editInputFor(agent, tool)
	if shape == (editInput{}) {
		return nil
	}
	var args map[string]any
	if json.Unmarshal([]byte(input), &args) != nil || args == nil || !singleReplacement(args) {
		return nil
	}
	if shape.patch != "" {
		mode, hasMode := args["mode"]
		if agent != agentkind.Hermes || mode == "patch" {
			patch, _ := args[shape.patch].(string)
			diff := patchEditDiff(patch)
			if diff == nil || !matchesEditPath(args, diff.Path) {
				return nil
			}
			return diff
		}
		if hasMode && mode != "replace" {
			return nil
		}
	}
	path, _ := args[shape.path].(string)
	if _, hasPath := args["path"]; agent == agentkind.OpenCode && !hasPath {
		path, _ = args["filePath"].(string)
	}
	if !validEditPath(path) || !matchesEditPath(args, path) {
		return nil
	}
	builder := editDiffBuilder{diff: EditDiff{Kind: "edit", Path: path}}
	if shape.write {
		content, ok := editLines(args["content"])
		if !ok {
			return nil
		}
		builder.diff.Kind = "write"
		builder.addLines('+', content)
		return builder.finish()
	}
	edits, hasEdits := args["edits"]
	if shape.list || (agent == agentkind.Pi && hasEdits) {
		list, ok := edits.([]any)
		_, hasOld := args[shape.old]
		_, hasNew := args[shape.new]
		if !ok || len(list) == 0 || hasOld || hasNew {
			return nil
		}
		return replacementEditDiff(builder, shape, list, remainingCells)
	}
	if hasEdits {
		return nil
	}
	return replacementEditDiff(builder, shape, []any{args}, remainingCells)
}

func singleReplacement(args map[string]any) bool {
	for _, key := range []string{"replace_all", "replaceAll"} {
		if value, exists := args[key]; exists && value != false {
			return false
		}
	}
	return true
}

func validEditPath(path string) bool {
	return len(path) <= 1024 && strings.TrimSpace(path) != "" && strings.IndexFunc(path, unicode.IsControl) < 0
}

func matchesEditPath(args map[string]any, path string) bool {
	for _, key := range []string{"path", "file_path", "filePath"} {
		if value, exists := args[key]; exists && value != path {
			return false
		}
	}
	return true
}

func editLines(value any) ([]string, bool) {
	text, ok := value.(string)
	if !ok {
		return nil, false
	}
	text = strings.ReplaceAll(text, "\r\n", "\n")
	if strings.IndexFunc(text, func(r rune) bool { return unicode.IsControl(r) && r != '\n' && r != '\t' }) >= 0 {
		return nil, false
	}
	if text == "" {
		return nil, true
	}
	return strings.Split(strings.TrimSuffix(text, "\n"), "\n"), true
}

type lineReplacement struct{ old, new []string }

func replacementEditDiff(builder editDiffBuilder, shape editInput, list []any, remainingCells *int) *EditDiff {
	pairs := make([]lineReplacement, 0, len(list))
	cells, limit := 0, min(editDiffCells, *remainingCells)
	for _, item := range list {
		pair, ok := item.(map[string]any)
		if !ok || !singleReplacement(pair) || !matchesEditPath(pair, builder.diff.Path) {
			return nil
		}
		old, okOld := editLines(pair[shape.old])
		new, okNew := editLines(pair[shape.new])
		if !okOld || !okNew {
			return nil
		}
		for len(old) > 0 && len(new) > 0 && old[0] == new[0] {
			old, new = old[1:], new[1:]
		}
		for len(old) > 0 && len(new) > 0 && old[len(old)-1] == new[len(new)-1] {
			old, new = old[:len(old)-1], new[:len(new)-1]
		}
		if len(old) != 0 && len(new) > (limit-cells)/len(old) {
			return nil
		}
		cells += len(old) * len(new)
		pairs = append(pairs, lineReplacement{old, new})
	}
	*remainingCells -= cells
	for _, pair := range pairs {
		builder.separate()
		builder.replace(pair.old, pair.new)
	}
	return builder.finish()
}

func (b *editDiffBuilder) replace(old, new []string) {
	if len(old) == 0 || len(new) == 0 {
		b.addLines('-', old)
		b.addLines('+', new)
		return
	}
	stride := len(new) + 1
	lcs := make([]uint16, (len(old)+1)*stride)
	for i := len(old) - 1; i >= 0; i-- {
		for j := len(new) - 1; j >= 0; j-- {
			k := i*stride + j
			if old[i] == new[j] {
				lcs[k] = 1 + lcs[k+stride+1]
			} else {
				lcs[k] = max(lcs[k+stride], lcs[k+1])
			}
		}
	}
	startOld, startNew := 0, 0
	for i, j := 0, 0; i < len(old) && j < len(new); {
		k := i*stride + j
		if old[i] == new[j] {
			b.addLines('-', old[startOld:i])
			b.addLines('+', new[startNew:j])
			b.separate()
			i, j = i+1, j+1
			startOld, startNew = i, j
		} else if lcs[k+stride] >= lcs[k+1] {
			i++
		} else {
			j++
		}
	}
	b.addLines('-', old[startOld:])
	b.addLines('+', new[startNew:])
}

func patchEditDiff(patch string) *EditDiff {
	lines, ok := editLines(patch)
	if !ok || len(lines) < 4 || lines[0] != "*** Begin Patch" || lines[len(lines)-1] != "*** End Patch" {
		return nil
	}
	path, add := strings.CutPrefix(lines[1], "*** Add File: ")
	if !add {
		path, _ = strings.CutPrefix(lines[1], "*** Update File: ")
		if path == lines[1] {
			return nil
		}
	}
	if !validEditPath(path) {
		return nil
	}
	builder := editDiffBuilder{diff: EditDiff{Kind: "edit", Path: path}}
	body := lines[2 : len(lines)-1]
	inHunk, hasBody := false, false
	for i, line := range body {
		switch {
		case strings.HasPrefix(line, "+"):
			builder.row('+', line[1:])
			hasBody = true
		case !add && strings.HasPrefix(line, "-"):
			builder.row('-', line[1:])
			hasBody = true
		case !add && strings.HasPrefix(line, " "):
			hasBody = true
		case !add && (line == "@@" || strings.HasPrefix(line, "@@ ")):
			if inHunk && !hasBody {
				return nil
			}
			builder.separate()
			inHunk, hasBody = true, false
		case !add && line == "*** End of File" && hasBody && i == len(body)-1:
		default:
			return nil
		}
	}
	if !hasBody {
		return nil
	}
	return builder.finish()
}

type editDiffBuilder struct {
	diff EditDiff
	text strings.Builder
	rows int
	gap  bool
}

func (b *editDiffBuilder) separate() { b.gap = b.rows > 0 }

func (b *editDiffBuilder) addLines(sign byte, lines []string) {
	for _, line := range lines {
		b.row(sign, line)
	}
}

func (b *editDiffBuilder) row(sign byte, line string) {
	if sign == '+' {
		b.diff.Added++
	} else {
		b.diff.Removed++
	}
	if b.diff.Truncated {
		return
	}
	prefix, rows := "", 1
	if b.gap {
		prefix, rows = "\n@@\n", 2
	} else if b.rows > 0 {
		prefix = "\n"
	}
	available := editDiffBytes - b.text.Len() - len(prefix) - 1
	if len(line) > available || b.rows+rows > editDiffRows {
		b.diff.Truncated = true
		if b.rows > 0 {
			return
		}
		line = utf8Prefix(line, available)
	}
	b.text.WriteString(prefix)
	b.text.WriteByte(sign)
	b.text.WriteString(line)
	b.rows += rows
	b.gap = false
}

func (b *editDiffBuilder) finish() *EditDiff {
	if b.diff.Kind == "edit" && b.diff.Added+b.diff.Removed == 0 {
		return nil
	}
	b.diff.Text = b.text.String()
	return &b.diff
}

func utf8Prefix(text string, n int) string {
	if n >= len(text) {
		return text
	}
	for n > 0 && !utf8.RuneStart(text[n]) {
		n--
	}
	return text[:n]
}
