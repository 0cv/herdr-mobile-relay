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
	for i := 1; i < len(lines); i++ {
		line := lines[i]
		if strings.TrimSpace(line) == "---" {
			return result, true
		}
		matches := frontmatterKeyPattern.FindStringSubmatch(line)
		if matches == nil {
			continue
		}
		key := strings.ToLower(matches[1])
		value := matches[2]
		if folded, isBlock := blockScalarHeader(value); isBlock {
			consumed := 0
			value, consumed = foldBlockScalar(lines[i+1:], folded)
			i += consumed
		} else if len(value) >= 2 && value[0] == value[len(value)-1] && (value[0] == '\'' || value[0] == '"') {
			value = value[1 : len(value)-1]
		}
		result[key] = value
	}
	return result, true
}

// blockScalarHeader reports whether value is a YAML block-scalar header such as
// "|", "|-", ">-" or "|2+", and whether that header selects the folded style.
// The indentation indicator is accepted but ignored: foldBlockScalar always
// strips the block's own common indentation instead. Anything else - including a
// plain scalar that merely starts with a pipe - is not a header.
func blockScalarHeader(value string) (folded, ok bool) {
	if value == "" {
		return false, false
	}
	switch value[0] {
	case '|':
	case '>':
		folded = true
	default:
		return false, false
	}
	for _, r := range value[1:] {
		if r != '+' && r != '-' && (r < '1' || r > '9') {
			return false, false
		}
	}
	return folded, true
}

// foldBlockScalar joins the indented continuation lines of one block scalar and
// returns the value plus the number of lines it consumed. The common indentation
// is stripped, deeper relative indentation is preserved, literal blocks keep
// their line breaks and folded blocks join lines with a space. The block ends at
// the first non-empty line that is not indented past the key, at a "---" fence,
// or at the end of the input; a block with no continuation lines yields "", so a
// malformed one drops the skill rather than surfacing a bogus description.
//
// Chomping indicators are deliberately not honoured: every consumer runs the
// value through compact, which collapses all whitespace runs anyway.
func foldBlockScalar(rest []string, folded bool) (string, int) {
	indent := -1
	consumed := 0
	var parts []string
	for _, line := range rest {
		trimmed := strings.TrimSpace(line)
		if trimmed == "---" {
			break
		}
		lineIndent := len(line) - len(strings.TrimLeft(line, " \t"))
		if trimmed != "" && lineIndent == 0 {
			break
		}
		consumed++
		if trimmed == "" {
			parts = append(parts, "")
			continue
		}
		if indent < 0 || lineIndent < indent {
			indent = lineIndent
		}
		parts = append(parts, line)
	}
	for len(parts) > 0 && parts[len(parts)-1] == "" {
		parts = parts[:len(parts)-1]
	}
	if len(parts) == 0 {
		return "", consumed
	}
	for i, part := range parts {
		if part != "" {
			parts[i] = part[indent:]
		}
	}
	separator := "\n"
	if folded {
		separator = " "
	}
	return strings.Join(parts, separator), consumed
}

func readSkillMetadata(path string) (map[string]string, bool) {
	data, err := os.ReadFile(path)
	if err != nil || len(data) > maxMetadataSize {
		return nil, false
	}
	return parseFrontmatterBytes(data)
}
