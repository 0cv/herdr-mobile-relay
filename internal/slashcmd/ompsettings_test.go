package slashcmd

import "testing"

func TestDefaultOMPSkillSettingsEnablesEverything(t *testing.T) {
	s := defaultOMPSkillSettings()
	if !s.enabled || !s.enableSkillCommands || !s.enableCodexUser || !s.enableClaudeUser ||
		!s.enableClaudeProject || !s.enablePiUser || !s.enablePiProject ||
		!s.enableAgentsUser || !s.enableAgentsProject {
		t.Fatalf("expected every toggle enabled by default, got %+v", s)
	}
	if len(s.customDirectories) != 0 || len(s.ignoredSkills) != 0 || len(s.includeSkills) != 0 {
		t.Fatalf("expected empty lists by default, got %+v", s)
	}
	if !s.allows("anything") {
		t.Fatal("default settings must allow every skill name")
	}
}

func TestParseOMPSkillSettingsBooleans(t *testing.T) {
	s := defaultOMPSkillSettings()
	parseOMPSkillSettings([]byte(`skills:
  enableClaudeUser: false
  enableCodexUser: "FALSE"
  enableAgentsUser: no
  enablePiProject: TRUE
`), &s)
	if s.enableClaudeUser {
		t.Error("enableClaudeUser should be false")
	}
	if s.enableCodexUser {
		t.Error("quoted FALSE should disable enableCodexUser")
	}
	if !s.enableAgentsUser {
		t.Error("an unrecognised scalar must leave the key at its default")
	}
	if !s.enablePiProject {
		t.Error("TRUE should enable enablePiProject")
	}
}

func TestParseOMPSkillSettingsIgnoresOtherSections(t *testing.T) {
	s := defaultOMPSkillSettings()
	parseOMPSkillSettings([]byte(`commands:
  enableClaudeUser: false
  enableClaudeProject: false
retry:
  fallbackChains:
    default:
      - anthropic/claude-fable-5
skills:
  enableClaudeProject: false
`), &s)
	if !s.enableClaudeUser {
		t.Error("commands.enableClaudeUser must not affect skills settings")
	}
	if s.enableClaudeProject {
		t.Error("skills.enableClaudeProject should be false")
	}
}

func TestParseOMPSkillSettingsBlockLists(t *testing.T) {
	s := defaultOMPSkillSettings()
	parseOMPSkillSettings([]byte(`skills:
  customDirectories:
    - ~/.omp/profiles/personal/agent/skills
    - "/opt/skills"
  ignoredSkills:
    - noisy-*
  enablePiUser: false
`), &s)
	if len(s.customDirectories) != 2 ||
		s.customDirectories[0] != "~/.omp/profiles/personal/agent/skills" ||
		s.customDirectories[1] != "/opt/skills" {
		t.Fatalf("unexpected customDirectories: %#v", s.customDirectories)
	}
	if len(s.ignoredSkills) != 1 || s.ignoredSkills[0] != "noisy-*" {
		t.Fatalf("unexpected ignoredSkills: %#v", s.ignoredSkills)
	}
	if s.enablePiUser {
		t.Error("a key after a block list must still be parsed")
	}
	if s.allows("noisy-thing") {
		t.Error("ignoredSkills glob should ban noisy-thing")
	}
	if !s.allows("quiet-thing") {
		t.Error("unmatched name should remain allowed")
	}
}

func TestParseOMPSkillSettingsFlowLists(t *testing.T) {
	s := defaultOMPSkillSettings()
	parseOMPSkillSettings([]byte(`skills:
  includeSkills: [deploy, "review"]
  customDirectories: []
`), &s)
	if len(s.includeSkills) != 2 || s.includeSkills[0] != "deploy" || s.includeSkills[1] != "review" {
		t.Fatalf("unexpected includeSkills: %#v", s.includeSkills)
	}
	if len(s.customDirectories) != 0 {
		t.Fatalf("empty flow list should clear the key: %#v", s.customDirectories)
	}
	if !s.allows("deploy") || s.allows("other") {
		t.Error("includeSkills should act as an allow list")
	}
}

func TestParseOMPSkillSettingsListsReplaceAcrossFiles(t *testing.T) {
	s := defaultOMPSkillSettings()
	parseOMPSkillSettings([]byte("skills:\n  ignoredSkills:\n    - a\n"), &s)
	parseOMPSkillSettings([]byte("skills:\n  ignoredSkills:\n    - b\n"), &s)
	if len(s.ignoredSkills) != 1 || s.ignoredSkills[0] != "b" {
		t.Fatalf("later file must replace the list, got %#v", s.ignoredSkills)
	}
}

func TestParseOMPSkillSettingsDisabledExtensions(t *testing.T) {
	s := defaultOMPSkillSettings()
	parseOMPSkillSettings([]byte(`disabledExtensions:
  - skill:deploy
  - plugin:foo
  - agent:bar
skills:
  enabled: true
`), &s)
	if !s.disabledSkills["deploy"] {
		t.Error("skill:deploy should be banned")
	}
	if len(s.disabledSkills) != 1 {
		t.Fatalf("only skill: entries count, got %#v", s.disabledSkills)
	}
	if s.allows("deploy") {
		t.Error("banned skill must not be allowed")
	}
	if !s.allows("foo") {
		t.Error("plugin:foo must not ban a skill named foo")
	}
}

func TestParseOMPSkillSettingsDisabledExtensionsFlowForm(t *testing.T) {
	s := defaultOMPSkillSettings()
	parseOMPSkillSettings([]byte("disabledExtensions: [skill:one, skill:two]\n"), &s)
	if !s.disabledSkills["one"] || !s.disabledSkills["two"] {
		t.Fatalf("flow form should be parsed, got %#v", s.disabledSkills)
	}
}

func TestParseOMPSkillSettingsDisabledExtensionsUnderSkillsIsIgnored(t *testing.T) {
	s := defaultOMPSkillSettings()
	parseOMPSkillSettings([]byte("skills:\n  disabledExtensions:\n    - skill:deploy\n"), &s)
	if len(s.disabledSkills) != 0 {
		t.Fatalf("disabledExtensions is top level only, got %#v", s.disabledSkills)
	}
}

func TestParseOMPSkillSettingsTabIndentationSkipped(t *testing.T) {
	s := defaultOMPSkillSettings()
	parseOMPSkillSettings([]byte("skills:\n\tenableClaudeUser: false\n"), &s)
	if !s.enableClaudeUser {
		t.Error("a tab-indented line must be skipped, leaving the default")
	}
}

func TestParseOMPSkillSettingsTrailingCommentsAndSpaces(t *testing.T) {
	s := defaultOMPSkillSettings()
	parseOMPSkillSettings([]byte("skills: \n  enableClaudeUser: false # off on purpose\n"), &s)
	if s.enableClaudeUser {
		t.Error("trailing comment should not defeat boolean parsing")
	}
}

func TestSourceFallbackEnabled(t *testing.T) {
	s := defaultOMPSkillSettings()
	if !s.sourceFallbackEnabled() {
		t.Error("defaults should enable the fall-through gate")
	}
	s.enableCodexUser = false
	s.enableClaudeUser = false
	s.enableClaudeProject = false
	s.enablePiUser = false
	s.enablePiProject = false
	if s.sourceFallbackEnabled() {
		t.Error("all five toggles off should close the fall-through gate")
	}
	s.enableAgentsProject = true
	if s.sourceFallbackEnabled() {
		t.Error("agents toggles must not open the fall-through gate")
	}
}
