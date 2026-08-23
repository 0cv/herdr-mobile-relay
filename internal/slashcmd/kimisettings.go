package slashcmd

import (
	"regexp"
	"strings"
)

// kimiSkillSettings holds the subset of Kimi Code's config.toml that decides
// which skill directories become /skill:<name> commands. Kimi 0.29.2 documents
// no per-skill disable and no switch for the skill commands themselves, so
// there is no ban list here.
type kimiSkillSettings struct {
	// mergeAllAvailableSkills selects a candidate group's semantics: true uses
	// every directory in the group, false only the first that exists.
	mergeAllAvailableSkills bool
	extraSkillDirs          []string
}

// defaultKimiSkillSettings returns Kimi's own defaults: merge everything, no
// extra directories.
func defaultKimiSkillSettings() kimiSkillSettings {
	return kimiSkillSettings{mergeAllAvailableSkills: true}
}

var kimiTOMLKeyPattern = regexp.MustCompile(`^([A-Za-z0-9_.-]+)[ \t]*=[ \t]*(.*)$`)

// parseKimiSkillSettings applies one config.toml's keys onto s. It understands
// the constrained TOML subset Kimi's skill settings actually use: flat
// "key = value" and single-line "key = [a, b]" in the root table, plus the
// "[table]" headers that end it. Anything it does not understand leaves the
// affected key at its current value, so a parse gap can only show a command the
// user could have run, never hide one.
func parseKimiSkillSettings(data []byte, s *kimiSkillSettings) {
	root := true

	for _, raw := range strings.Split(strings.ReplaceAll(string(data), "\r\n", "\n"), "\n") {
		trimmed := strings.TrimSpace(raw)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}
		if strings.HasPrefix(trimmed, "[") {
			// A [table] or [[array of tables]] header. Only root-table keys are
			// consumed, so a same-named key under another table is ignored.
			root = false
			continue
		}
		if !root {
			continue
		}
		matches := kimiTOMLKeyPattern.FindStringSubmatch(trimmed)
		if matches == nil {
			continue
		}
		value := strings.TrimSpace(matches[2])
		switch matches[1] {
		case "merge_all_available_skills":
			setScalarBool(&s.mergeAllAvailableSkills, value)
		case "extra_skill_dirs":
			setKimiList(&s.extraSkillDirs, value)
		}
	}
}

// setKimiList assigns a list-valued key from a single-line TOML array. List
// keys replace rather than append across config files. A multi-line array, or
// any other value, leaves the key untouched: half-parsing a construct this
// parser does not understand could hide a command.
func setKimiList(target *[]string, value string) {
	if items, ok := kimiArrayValue(value); ok {
		*target = items
	}
}

// kimiArrayValue unquotes and trims the items of a single-line TOML array,
// dropping empty ones. An array that does not close on this line is not
// understood; an array that closes with nothing in it clears the key.
func kimiArrayValue(value string) ([]string, bool) {
	if !strings.HasPrefix(value, "[") {
		return nil, false
	}
	end := kimiArrayEnd(value)
	if end < 0 {
		return nil, false
	}
	inner := strings.TrimSpace(value[1:end])
	if inner == "" {
		return []string{}, true
	}
	items := make([]string, 0, strings.Count(inner, ",")+1)
	for _, field := range strings.Split(inner, ",") {
		if item := unquoteScalar(field); item != "" {
			items = append(items, item)
		}
	}
	return items, true
}

// kimiArrayEnd returns the index of the array's closing bracket, ignoring
// brackets inside quoted items and any trailing comment, or -1 when the array
// does not close on this line.
func kimiArrayEnd(value string) int {
	quote := byte(0)
	for i := 1; i < len(value); i++ {
		switch c := value[i]; {
		case quote != 0:
			if c == quote {
				quote = 0
			}
		case c == '\'' || c == '"':
			quote = c
		case c == ']':
			return i
		}
	}
	return -1
}
