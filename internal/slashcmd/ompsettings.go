package slashcmd

import (
	"bytes"
	"io"
	"path"
	"slices"
	"strings"

	"go.yaml.in/yaml/v3"
)

// ompSkillSettings holds the subset of Oh My Pi's settings that decides which
// skills become /skill:<name> commands. Every boolean defaults to true, so an
// unreadable or unparsable config file yields the agent's own defaults.
type ompSkillSettings struct {
	enabled                  bool
	enableSkillCommands      bool
	enableCodexUser          bool
	enableClaudeUser         bool
	enableClaudeProject      bool
	enablePiUser             bool
	enablePiProject          bool
	enableAgentsUser         bool
	enableAgentsProject      bool
	customDirectories        []string
	customDirectoriesSet     bool
	customDirectorySource    string
	customDirectoryBoundary  string
	extensionDirectories     []string
	extensionDirectoriesSet  bool
	extensionDirectorySource string
	ignoredSkills            []string
	includeSkills            []string
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

// parseOMPSkillSettings applies one valid YAML file's present keys onto s.
func parseOMPSkillSettings(data []byte, s *ompSkillSettings) {
	var config struct {
		Extensions         yaml.Node `yaml:"extensions"`
		DisabledExtensions yaml.Node `yaml:"disabledExtensions"`
		Skills             struct {
			Enabled             any            `yaml:"enabled"`
			EnableSkillCommands any            `yaml:"enableSkillCommands"`
			EnableCodexUser     any            `yaml:"enableCodexUser"`
			EnableClaudeUser    any            `yaml:"enableClaudeUser"`
			EnableClaudeProject any            `yaml:"enableClaudeProject"`
			EnablePiUser        any            `yaml:"enablePiUser"`
			EnablePiProject     any            `yaml:"enablePiProject"`
			EnableAgentsUser    any            `yaml:"enableAgentsUser"`
			EnableAgentsProject any            `yaml:"enableAgentsProject"`
			CustomDirectories   yaml.Node      `yaml:"customDirectories"`
			IgnoredSkills       yaml.Node      `yaml:"ignoredSkills"`
			IncludeSkills       yaml.Node      `yaml:"includeSkills"`
			Other               map[string]any `yaml:",inline"`
		} `yaml:"skills"`
		Other map[string]any `yaml:",inline"`
	}
	decoder := yaml.NewDecoder(bytes.NewReader(data))
	if decoder.Decode(&config) != nil || decoder.Decode(&yaml.Node{}) != io.EOF {
		return
	}

	next := *s
	// Only YAML booleans override toggles; strings such as "false" do not.
	for target, value := range map[*bool]any{
		&next.enabled:             config.Skills.Enabled,
		&next.enableSkillCommands: config.Skills.EnableSkillCommands,
		&next.enableCodexUser:     config.Skills.EnableCodexUser,
		&next.enableClaudeUser:    config.Skills.EnableClaudeUser,
		&next.enableClaudeProject: config.Skills.EnableClaudeProject,
		&next.enablePiUser:        config.Skills.EnablePiUser,
		&next.enablePiProject:     config.Skills.EnablePiProject,
		&next.enableAgentsUser:    config.Skills.EnableAgentsUser,
		&next.enableAgentsProject: config.Skills.EnableAgentsProject,
	} {
		if value, ok := value.(bool); ok {
			*target = value
		}
	}

	var disabled []string
	for _, field := range []struct {
		node   *yaml.Node
		target *[]string
	}{
		{&config.Extensions, &next.extensionDirectories},
		{&config.DisabledExtensions, &disabled},
		{&config.Skills.CustomDirectories, &next.customDirectories},
		{&config.Skills.IgnoredSkills, &next.ignoredSkills},
		{&config.Skills.IncludeSkills, &next.includeSkills},
	} {
		if field.node.Kind == 0 {
			continue
		}
		var items []string
		if field.node.Decode(&items) != nil {
			return
		}
		*field.target = slices.DeleteFunc(items, func(item string) bool { return item == "" })
	}
	if config.DisabledExtensions.Kind != 0 {
		next.disabledSkills = map[string]bool{}
		for _, id := range disabled {
			addDisabledSkill(&next, id)
		}
	}
	next.customDirectoriesSet = next.customDirectoriesSet || config.Skills.CustomDirectories.Kind != 0
	next.extensionDirectoriesSet = next.extensionDirectoriesSet || config.Extensions.Kind != 0
	*s = next
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
