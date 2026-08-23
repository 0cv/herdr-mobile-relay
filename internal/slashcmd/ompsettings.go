package slashcmd

import (
	"path"
	"regexp"
	"strings"
)

// ompSkillSettings holds the subset of Oh My Pi's settings that decides which
// skills become /skill:<name> commands. Every boolean defaults to true, so an
// unreadable or unparsable config file yields the agent's own defaults.
type ompSkillSettings struct {
	enabled             bool
	enableSkillCommands bool
	enableCodexUser     bool
	enableClaudeUser    bool
	enableClaudeProject bool
	enablePiUser        bool
	enablePiProject     bool
	enableAgentsUser    bool
	enableAgentsProject bool
	customDirectories   []string
	ignoredSkills       []string
	includeSkills       []string
	// disabledSkills holds names from the top-level disabledExtensions list that
	// carried the "skill:" prefix.
	disabledSkills map[string]bool
}

// defaultOMPSkillSettings returns the all-enabled, no-bans default.
func defaultOMPSkillSettings() ompSkillSettings {
	return ompSkillSettings{
		enabled:             true,
		enableSkillCommands: true,
		enableCodexUser:     true,
		enableClaudeUser:    true,
		enableClaudeProject: true,
		enablePiUser:        true,
		enablePiProject:     true,
		enableAgentsUser:    true,
		enableAgentsProject: true,
		disabledSkills:      map[string]bool{},
	}
}

// sourceFallbackEnabled reports the gate omp applies to providers with no
// toggle of their own (github, opencode, plugins, and project-level codex).
func (s *ompSkillSettings) sourceFallbackEnabled() bool {
	return s.enableCodexUser || s.enableClaudeUser || s.enableClaudeProject ||
		s.enablePiUser || s.enablePiProject
}

// allows reports whether a skill name survives omp's ban filters, applied in
// omp's own order: disabledExtensions, then ignoredSkills, then includeSkills.
func (s *ompSkillSettings) allows(name string) bool {
	if s.disabledSkills[name] {
		return false
	}
	for _, pattern := range s.ignoredSkills {
		if matched, err := path.Match(pattern, name); err == nil && matched {
			return false
		}
	}
	if len(s.includeSkills) == 0 {
		return true
	}
	for _, pattern := range s.includeSkills {
		if matched, err := path.Match(pattern, name); err == nil && matched {
			return true
		}
	}
	return false
}

var ompYAMLKeyPattern = regexp.MustCompile(`^([A-Za-z0-9_.-]+):(?:[ \t]+(.*))?$`)

// parseOMPSkillSettings applies one config file's keys onto s. It understands
// the constrained YAML subset omp's config actually uses: top-level keys, one
// level of nesting under "skills", block and flow lists, and boolean scalars.
// Anything it does not understand leaves the affected key at its current value,
// so a parse gap can only show a command the user could have run, never hide
// one.
func parseOMPSkillSettings(data []byte, s *ompSkillSettings) {
	const (
		modeNone = iota
		modeSkills
		modeDisabled
	)
	mode := modeNone
	childIndent := -1
	var listTarget *[]string

	for _, raw := range strings.Split(strings.ReplaceAll(string(data), "\r\n", "\n"), "\n") {
		line := strings.TrimRight(raw, " \t")
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}
		leading := line[:len(line)-len(strings.TrimLeft(line, " \t"))]
		if strings.Contains(leading, "\t") {
			continue
		}
		indent := len(leading)

		if indent == 0 {
			mode = modeNone
			childIndent = -1
			listTarget = nil
			key, value, ok := splitOMPKey(trimmed)
			if !ok {
				continue
			}
			switch key {
			case "skills":
				if value == "" {
					mode = modeSkills
				}
			case "disabledExtensions":
				items, listed, block := ompListValue(value)
				if block {
					s.disabledSkills = map[string]bool{}
					mode = modeDisabled
					continue
				}
				if !listed {
					continue
				}
				s.disabledSkills = map[string]bool{}
				for _, item := range items {
					addDisabledSkill(s, item)
				}
			}
			continue
		}

		switch mode {
		case modeDisabled:
			if item, ok := ompListItem(trimmed); ok {
				addDisabledSkill(s, item)
			}
		case modeSkills:
			if item, ok := ompListItem(trimmed); ok {
				if listTarget != nil && item != "" {
					*listTarget = append(*listTarget, item)
				}
				continue
			}
			key, value, ok := splitOMPKey(trimmed)
			if !ok {
				continue
			}
			if childIndent == -1 {
				childIndent = indent
			}
			if indent > childIndent {
				// Deeper than a direct child of "skills:" - part of a construct
				// this parser does not consume.
				listTarget = nil
				continue
			}
			listTarget = nil
			switch key {
			case "enabled":
				setScalarBool(&s.enabled, value)
			case "enableSkillCommands":
				setScalarBool(&s.enableSkillCommands, value)
			case "enableCodexUser":
				setScalarBool(&s.enableCodexUser, value)
			case "enableClaudeUser":
				setScalarBool(&s.enableClaudeUser, value)
			case "enableClaudeProject":
				setScalarBool(&s.enableClaudeProject, value)
			case "enablePiUser":
				setScalarBool(&s.enablePiUser, value)
			case "enablePiProject":
				setScalarBool(&s.enablePiProject, value)
			case "enableAgentsUser":
				setScalarBool(&s.enableAgentsUser, value)
			case "enableAgentsProject":
				setScalarBool(&s.enableAgentsProject, value)
			case "customDirectories":
				listTarget = setOMPList(&s.customDirectories, value)
			case "ignoredSkills":
				listTarget = setOMPList(&s.ignoredSkills, value)
			case "includeSkills":
				listTarget = setOMPList(&s.includeSkills, value)
			}
		}
	}
}

func addDisabledSkill(s *ompSkillSettings, id string) {
	name, ok := strings.CutPrefix(id, "skill:")
	if !ok || name == "" {
		return
	}
	if s.disabledSkills == nil {
		s.disabledSkills = map[string]bool{}
	}
	s.disabledSkills[name] = true
}

// setOMPList assigns a list-valued key. List keys replace rather than append
// across config levels, matching omp's own merge semantics. A block-form key
// returns the slice so following "- item" lines extend it; a value the parser
// does not understand leaves the key untouched.
func setOMPList(target *[]string, value string) *[]string {
	items, listed, block := ompListValue(value)
	if block {
		*target = nil
		return target
	}
	if !listed {
		return nil
	}
	*target = items
	return nil
}

// ompListValue interprets the scalar part of a list-valued key. block reports
// the "key:" form whose items follow on later lines; listed reports a flow
// list; neither means the value was not understood.
func ompListValue(value string) (items []string, listed, block bool) {
	if value == "" {
		return nil, false, true
	}
	value = stripScalarComment(value)
	if !strings.HasPrefix(value, "[") || !strings.HasSuffix(value, "]") {
		return nil, false, false
	}
	inner := strings.TrimSpace(value[1 : len(value)-1])
	if inner == "" {
		return nil, true, false
	}
	for _, field := range strings.Split(inner, ",") {
		if item := unquoteScalar(field); item != "" {
			items = append(items, item)
		}
	}
	return items, true, false
}

func ompListItem(trimmed string) (string, bool) {
	rest, ok := strings.CutPrefix(trimmed, "-")
	if !ok {
		return "", false
	}
	if rest != "" && !strings.HasPrefix(rest, " ") && !strings.HasPrefix(rest, "\t") {
		return "", false
	}
	return unquoteScalar(stripScalarComment(strings.TrimSpace(rest))), true
}

func splitOMPKey(trimmed string) (key, value string, ok bool) {
	matches := ompYAMLKeyPattern.FindStringSubmatch(trimmed)
	if matches == nil {
		return "", "", false
	}
	return matches[1], strings.TrimSpace(matches[2]), true
}

// setScalarBool applies a boolean scalar, accepting true/false case
// insensitively and optionally quoted. Any other scalar leaves the target
// alone. Shared by the YAML and TOML settings parsers.
func setScalarBool(target *bool, value string) {
	switch strings.ToLower(unquoteScalar(stripScalarComment(value))) {
	case "true":
		*target = true
	case "false":
		*target = false
	}
}

// stripScalarComment removes a trailing comment from an unquoted scalar. Both
// YAML and TOML start a comment with "#".
func stripScalarComment(value string) string {
	if strings.HasPrefix(value, "\"") || strings.HasPrefix(value, "'") {
		return value
	}
	if index := strings.Index(value, " #"); index >= 0 {
		return strings.TrimSpace(value[:index])
	}
	return value
}

func unquoteScalar(value string) string {
	value = strings.TrimSpace(value)
	if len(value) >= 2 && value[0] == value[len(value)-1] && (value[0] == '\'' || value[0] == '"') {
		value = value[1 : len(value)-1]
	}
	return strings.TrimSpace(value)
}
