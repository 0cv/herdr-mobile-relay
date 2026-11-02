package slashcmd

import (
	"fmt"
	"maps"
	"path/filepath"
	"reflect"
	"slices"
	"testing"
)

func TestOMPSettingsCompatibilityMalformedSiblings(t *testing.T) {
	for _, tc := range []struct{ name, line string }{
		{"unclosed-flow", "  ignoredSkills: [unterminated\n"},
		{"tab-indented", "\tignoredSkills: [replacement]\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := defaultOMPSkillSettings()
			parseOMPSkillSettings([]byte("skills:\n  ignoredSkills: [personal]\n"), &s)
			before := s
			parseOMPSkillSettings([]byte("skills:\n  enablePiUser: false\n"+tc.line+"  enableClaudeUser: false\n"), &s)
			if !reflect.DeepEqual(s, before) {
				t.Fatalf("invalid YAML must leave previous settings unchanged: got %+v, want %+v", s, before)
			}
		})
	}
}

// One overlay matrix applies each form to all five list fields, as two files.
func TestOMPSettingsCompatibilityListOverlayPolicy(t *testing.T) {
	for _, tc := range []struct {
		name, top, child, disabled, want string
		present, applied, invalid        bool
	}{
		{name: "absent", want: "old"},
		{name: "empty-flow", top: " []", child: " []", disabled: " []", present: true, applied: true},
		{name: "bare", present: true, applied: true},
		{name: "null", top: " null", child: " null", disabled: " null", present: true, applied: true},
		{name: "tilde", top: " ~", child: " ~", disabled: " ~", present: true, applied: true},
		{name: "unsupported", top: " {nested: value}", child: " {nested: value}", disabled: " {nested: value}", want: "old", present: true, invalid: true},
		{name: "unclosed-flow", top: " [unclosed", child: " [unclosed", disabled: " [unclosed", want: "old", present: true, invalid: true},
		{name: "replacement-flow", top: " [new]", child: " [new]", disabled: " [skill:new, plugin:ignored]", want: "new", present: true, applied: true},
		{name: "replacement-block", top: "\n  - new", child: "\n    - new", disabled: "\n  - skill:new\n  - plugin:ignored", want: "new", present: true, applied: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := defaultOMPSkillSettings()
			parseOMPSkillSettings([]byte("extensions: [old]\ndisabledExtensions: [skill:old, plugin:ignored]\nskills:\n  customDirectories: [old]\n  ignoredSkills: [old]\n  includeSkills: [old]\n"), &s)
			if !s.customDirectoriesSet || !s.extensionDirectoriesSet {
				t.Fatal("personal lists must establish both flags")
			}
			// The loader resets per-file flags before reading the project overlay.
			s.customDirectoriesSet, s.extensionDirectoriesSet = false, false
			project := "skills:\n  enableAgentsProject: false\n"
			if tc.present {
				project = fmt.Sprintf("extensions:%s\ndisabledExtensions:%s\nskills:\n  enableAgentsProject: false\n  customDirectories:%s\n  ignoredSkills:%s\n  includeSkills:%s\n", tc.top, tc.disabled, tc.child, tc.child, tc.child)
			}
			parseOMPSkillSettings([]byte(project), &s)
			if s.enableAgentsProject != tc.invalid {
				t.Errorf("project sibling enabled = %v, want invalid-file status %v", s.enableAgentsProject, tc.invalid)
			}
			var want []string
			wantDisabled := map[string]bool{}
			if tc.want != "" {
				want = []string{tc.want}
				wantDisabled[tc.want] = true
			}
			for name, got := range map[string][]string{
				"customDirectories": s.customDirectories, "extensions": s.extensionDirectories,
				"ignoredSkills": s.ignoredSkills, "includeSkills": s.includeSkills,
			} {
				if !slices.Equal(got, want) {
					t.Errorf("%s = %q, want %q", name, got, want)
				}
			}
			if !maps.Equal(s.disabledSkills, wantDisabled) {
				t.Errorf("skill-only disabledExtensions = %v, want %v", s.disabledSkills, wantDisabled)
			}
			if s.customDirectoriesSet != tc.applied || s.extensionDirectoriesSet != tc.applied {
				t.Errorf("per-file flags = custom:%v extension:%v, want %v", s.customDirectoriesSet, s.extensionDirectoriesSet, tc.applied)
			}
		})
	}
}

func TestOMPSettingsCompatibilityBooleanPolicy(t *testing.T) {
	for _, tc := range []struct {
		name, value string
		want        int // -1 retains the previous value
	}{
		{"true", "true", 1}, {"title-true", "True", 1}, {"upper-true", "TRUE", 1},
		{"false", "false", 0}, {"title-false", "False", 0}, {"upper-false", "FALSE", 0},
		{"mixed-true", "TrUe", -1}, {"quoted-true", `"TrUe"`, -1},
		{"mixed-false", "FaLsE", -1}, {"quoted-false", `'FaLsE'`, -1},
		{"quoted-standard-true", `"true"`, -1}, {"quoted-standard-false", `'false'`, -1},
		{"null", "null", -1}, {"number", "1", -1}, {"list", "[]", -1}, {"mapping", "{}", -1},
		{"yes", "yes", -1}, {"no", "no", -1}, {"on", "on", -1}, {"off", "off", -1},
		{"unquoted-trailing-comment", "true # comment", 1},
		{"legacy-quoted-true-comment", `"true" # comment`, -1},
		{"legacy-quoted-false-comment", `'false' # comment`, -1},
	} {
		for _, initial := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/from-%v", tc.name, initial), func(t *testing.T) {
				s := defaultOMPSkillSettings()
				s.enabled = initial
				parseOMPSkillSettings([]byte("skills:\n  enabled: "+tc.value+"\n  enablePiUser: false\n"), &s)
				if s.enablePiUser {
					t.Fatal("a non-boolean value must not prevent a valid sibling from applying")
				}
				want := initial
				if tc.want != -1 {
					want = tc.want == 1
				}
				if s.enabled != want {
					t.Fatalf("enabled = %v, want %v", s.enabled, want)
				}
			})
		}
	}
}

func TestOMPSettingsCompatibilityOrderedDuplicates(t *testing.T) {
	for _, tc := range []struct{ name, document string }{
		{"extensions", "extensions: [first]\nextensions: [last]\n"},
		{"disabledExtensions", "disabledExtensions: [skill:first]\ndisabledExtensions: [skill:last]\n"},
		{"customDirectories", "skills:\n  customDirectories: [first]\n  customDirectories: [last]\n"},
		{"ignoredSkills", "skills:\n  ignoredSkills: [first]\n  ignoredSkills: [last]\n"},
		{"includeSkills", "skills:\n  includeSkills: [first]\n  includeSkills: [last]\n"},
		{"enabled", "skills:\n  enabled: true\n  enabled: false\n"},
		{"null", "extensions: [first]\nextensions: null\n"},
		{"skills", "skills: {enabled: false}\nskills: {enabled: true}\n"},
		{"other-section", "skills: {enabled: false}\nother: {key: first, key: last}\n"},
		{"other-skill-key", "skills:\n  enabled: false\n  other: {key: first, key: last}\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := defaultOMPSkillSettings()
			parseOMPSkillSettings([]byte("extensions: [personal]\ndisabledExtensions: [skill:personal]\nskills:\n  customDirectories: [personal]\n  ignoredSkills: [personal]\n"), &s)
			before := s
			parseOMPSkillSettings([]byte(tc.document), &s)
			if !reflect.DeepEqual(s, before) {
				t.Fatalf("duplicate mapping keys must leave previous settings unchanged: got %+v, want %+v", s, before)
			}
		})
	}
}

func TestOMPSettingsCompatibilityCatalogOverlays(t *testing.T) {
	for _, tc := range []struct {
		name, project, customSource, extensionSource string
		customSet, extensionSet                      bool
		personalCustom, personalExtension            bool
		projectCustom, projectExtension              bool
	}{
		{"both-absent", "skills:\n  enableClaudeUser: false\n", "personal", "personal", false, false, true, true, false, false},
		{"custom-empty", "skills:\n  customDirectories: []\n", "project", "personal", true, false, false, true, false, false},
		{"extension-empty", "extensions: []\n", "personal", "project", false, true, true, false, false, false},
		{"both-project", "extensions: [team-extension]\nskills:\n  customDirectories: [team-skills]\n", "project", "project", true, true, false, false, true, true},
		{"null-lists", "extensions: null\nskills:\n  customDirectories: ~\n", "project", "project", true, true, false, false, false, false},
		{"invalid-project", "extensions: []\nskills:\n  enabled: false\n  customDirectories: [unterminated\n", "personal", "personal", false, false, true, true, false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newOMPFixture(t)
			t.Setenv("HOME", f.home)
			f.gitRepo(t)
			custom := filepath.Join(f.home, "personal-custom")
			extension := filepath.Join(f.home, "personal-extension")
			writeSkill(t, custom, "personal-custom", "Personal custom")
			writeSkill(t, filepath.Join(extension, "skills"), "personal-extension", "Personal extension")
			writeSkill(t, filepath.Join(f.repo, "team-skills"), "project-custom", "Project custom")
			writeSkill(t, filepath.Join(f.repo, "team-extension", "skills"), "project-extension", "Project extension")
			writeSkill(t, filepath.Join(f.repo, ".omp", "skills"), "native", "Native control")
			writeFile(t, filepath.Join(f.home, ".omp", "agent", "config.yml"), fmt.Sprintf("extensions: [%q]\nskills:\n  customDirectories: [%q]\n", extension, custom))
			writeFile(t, filepath.Join(f.repo, ".omp", "config.yml"), tc.project)

			s := newCatalogReader().loadOMPSkillSettings(DiscoverContext{Home: f.home, Cwd: f.repo})
			if s.customDirectoriesSet != tc.customSet || s.extensionDirectoriesSet != tc.extensionSet ||
				s.customDirectorySource != tc.customSource || s.extensionDirectorySource != tc.extensionSource {
				t.Fatalf("overlay flags/sources: custom=%v/%s extension=%v/%s", s.customDirectoriesSet, s.customDirectorySource, s.extensionDirectoriesSet, s.extensionDirectorySource)
			}
			boundary := ""
			if tc.customSet {
				boundary = f.repo
			}
			if s.customDirectoryBoundary != boundary {
				t.Fatalf("custom boundary = %q, want %q", s.customDirectoryBoundary, boundary)
			}
			var wantCustom, wantExtension []string
			if tc.personalCustom {
				wantCustom = []string{custom}
			} else if tc.projectCustom {
				wantCustom = []string{"team-skills"}
			}
			if tc.personalExtension {
				wantExtension = []string{extension}
			} else if tc.projectExtension {
				wantExtension = []string{"team-extension"}
			}
			if !slices.Equal(s.customDirectories, wantCustom) || !slices.Equal(s.extensionDirectories, wantExtension) {
				t.Fatalf("loaded lists: custom=%q want=%q extension=%q want=%q", s.customDirectories, wantCustom, s.extensionDirectories, wantExtension)
			}
			catalog := f.catalog(f.repo)
			if catalog.Truncated {
				t.Fatal("fixture catalog unexpectedly truncated")
			}
			for _, check := range []struct {
				name, source string
				present      bool
			}{
				{"native", "project", true}, {"personal-custom", "personal", tc.personalCustom},
				{"personal-extension", "personal", tc.personalExtension}, {"project-custom", "project", tc.projectCustom},
				{"project-extension", "project", tc.projectExtension},
			} {
				cmd, ok := commandByName(catalog, "/skill:"+check.name)
				if ok != check.present || ok && cmd.Source != check.source {
					t.Errorf("%s = %+v, present=%v; want present=%v source=%s", check.name, cmd, ok, check.present, check.source)
				}
			}
		})
	}
}

func TestOMPSettingsYAMLComparison(t *testing.T) {
	for _, tc := range []struct {
		name, document string
		enabled        bool
		ignored        []string
	}{
		{"unclosed-sibling", "skills:\n  enablePiUser: false\n  ignoredSkills: [unterminated\n  enableClaudeUser: false\n", false, []string{"personal"}},
		{"tab-sibling", "skills:\n  enablePiUser: false\n\tignoredSkills: [replacement]\n  enableClaudeUser: false\n", false, []string{"personal"}},
		{"bare-list", "skills:\n  ignoredSkills:\n", false, nil},
		{"explicit-null", "skills:\n  ignoredSkills: null\n", false, nil},
		{"quoted-boolean-comment", "skills:\n  enabled: \"true\" # comment\n", false, []string{"personal"}},
		{"unquoted-boolean-comment", "skills:\n  enabled: true # comment\n", true, []string{"personal"}},
		{"quoted-comma", "skills:\n  ignoredSkills: [\"one,two\", plain]\n", false, []string{"one,two", "plain"}},
		{"quoted-hash", "skills:\n  ignoredSkills: [\"needs # review\", keep]\n", false, []string{"needs # review", "keep"}},
		{"escaped-strings", "skills:\n  ignoredSkills: [\"line\\nbreak\", 'it''s']\n", false, []string{"line\nbreak", "it's"}},
		{"empty-flow-item", "skills:\n  ignoredSkills: [one,,two]\n", false, []string{"personal"}},
		{"nested", "skills:\n  ignoredSkills: [one, [two, three], {name: four}]\n", false, []string{"personal"}},
		{"unquoted-star", "skills:\n  ignoredSkills: [*]\n", false, []string{"personal"}},
		{"two-documents", "skills:\n  ignoredSkills: [first]\n---\nskills:\n  ignoredSkills: [second]\n", false, []string{"personal"}},
		{"malformed-trailing-document", "skills: {enabled: true}\n...\n---\n[unterminated\n", false, []string{"personal"}},
		{"flow-mapping", "skills: {enabled: true, ignoredSkills: [flow]}\n", true, []string{"flow"}},
		{"aliases", "patterns: &patterns [one, two]\nskills:\n  enabled: true\n  ignoredSkills: *patterns\n", true, []string{"one", "two"}},
		{"quoted-whitespace", "skills:\n  ignoredSkills: [\" padded \", \"\"]\n", false, []string{" padded "}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := defaultOMPSkillSettings()
			s.enabled = false
			s.ignoredSkills = []string{"personal"}
			parseOMPSkillSettings([]byte(tc.document), &s)
			if s.enabled != tc.enabled || !slices.Equal(s.ignoredSkills, tc.ignored) || !s.enablePiUser || !s.enableClaudeUser {
				t.Fatalf("settings: enabled=%v pi=%v claude=%v ignored=%q; want enabled=%v ignored=%q", s.enabled, s.enablePiUser, s.enableClaudeUser, s.ignoredSkills, tc.enabled, tc.ignored)
			}
		})
	}
}
