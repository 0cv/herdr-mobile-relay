package slashcmd

import (
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

type budgetProviderFixture struct {
	home, cwd string
	roots     []string
	profile   string
	format    string
	prefix    string
}

func newBudgetProviderFixture(t *testing.T, profile string) budgetProviderFixture {
	t.Helper()
	isolateAgentEnv(t)
	f := budgetProviderFixture{home: t.TempDir(), cwd: t.TempDir(), profile: profile, prefix: "/"}
	if err := os.MkdirAll(filepath.Join(f.cwd, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	switch profile {
	case "claude", "qoder", "cursor":
		f.roots = []string{filepath.Join(f.cwd, "."+profile, "commands"), filepath.Join(f.home, "."+profile, "commands")}
	case "opencode":
		t.Setenv("XDG_CONFIG_HOME", filepath.Join(f.home, ".config"))
		f.roots = []string{filepath.Join(f.home, ".config", "opencode", "commands"), filepath.Join(f.cwd, ".opencode", "commands")}
	case "omp", "pi":
		f.prefix = "/skill:"
		f.roots = []string{filepath.Join(f.cwd, "."+profile, "skills"), filepath.Join(f.home, "."+profile, "agent", "skills")}
		if profile == "pi" {
			path := filepath.Join(f.home, ".pi", "agent", "settings.json")
			if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
				t.Fatal(err)
			}
			writeTestFile(t, path, `{"defaultProjectTrust":"always"}`)
		}
	case "kimi":
		f.prefix = "/skill:"
		f.roots = []string{filepath.Join(f.cwd, ".kimi-code", "skills"), filepath.Join(f.home, ".kimi-code", "skills")}
	case "hermes":
		f.roots = []string{filepath.Join(f.cwd, ".hermes", "skills"), filepath.Join(f.home, "configured")}
		f.format = "/{name}"
	case "generic":
		f.roots = []string{filepath.Join(f.home, "skills"), filepath.Join(f.cwd, "skills")}
		f.format = "/{name}"
	}
	return f
}

func (f budgetProviderFixture) catalog() Catalog {
	var dirs []string
	if f.profile == "generic" {
		dirs = f.roots
	} else if f.profile == "hermes" {
		dirs = f.roots[1:]
	}
	return CatalogForProfile(f.profile, "", f.cwd, f.home, dirs, f.format, "", "")
}

func (f budgetProviderFixture) add(t *testing.T, root int, name, metadata string, size int) {
	t.Helper()
	path := filepath.Join(f.roots[root], name, "SKILL.md")
	if f.profile == "claude" || f.profile == "qoder" || f.profile == "cursor" || f.profile == "opencode" {
		path = filepath.Join(f.roots[root], name+".md")
	}
	paddedFile(t, path, metadata, size)
}

func TestCatalogMetadataBudget(t *testing.T) {
	for _, profile := range []string{"claude", "cursor", "qoder", "omp", "pi", "kimi", "hermes", "opencode", "generic"} {
		t.Run(profile, func(t *testing.T) {
			f := newBudgetProviderFixture(t, profile)
			for root := range f.roots {
				for i := range 150 {
					name := fmt.Sprintf("budget-%d-%03d", root, i)
					f.add(t, root, name, "---\nname: "+name+"\ndescription: Budget entry\n---\n", 100*1024)
				}
			}
			catalog := f.catalog()
			if !catalog.Truncated {
				t.Error("cumulative reads exceeding 16 MiB did not truncate discovery")
			}
			custom := 0
			for _, command := range catalog.Commands {
				if strings.HasPrefix(command.Command, f.prefix+"budget-") {
					custom++
				}
			}
			if custom < 250 || custom >= 300 {
				t.Errorf("published %d of 300 entries, want a bounded prefix near 16 MiB", custom)
			}
			if !hasCommand(catalog, f.prefix+"budget-0-000") || hasCommand(catalog, f.prefix+"budget-1-149") {
				t.Error("discovery did not preserve the ordered prefix across roots")
			}
			if repeated := f.catalog(); !reflect.DeepEqual(catalog, repeated) {
				t.Error("budget-limited discovery is not deterministic")
			}
		})
	}
}

func TestCatalogMetadataPrefixSemantics(t *testing.T) {
	for _, profile := range []string{"claude", "cursor", "qoder", "omp", "pi", "kimi", "hermes", "opencode", "generic"} {
		t.Run(profile, func(t *testing.T) {
			f := newBudgetProviderFixture(t, profile)
			metadata := "---\nname: large\ndescription: Large body metadata\nargument-hint: <target>\n---\n"
			f.add(t, 0, "large", metadata, maxCommandFileSize)
			f.add(t, 0, "oversized", metadata, maxCommandFileSize+1)
			f.add(t, 0, "huge-frontmatter", "---\nname: huge-frontmatter\ndescription: Huge\n"+strings.Repeat("  filler\n", maxCommandFileSize/8)+"---\n", 2*maxCommandFileSize)
			catalog := f.catalog()
			command, ok := findCatalogCommand(catalog, f.prefix+"large")
			if !ok || command.Description != "Large body metadata" {
				t.Errorf("large body command = %+v, present = %v", command, ok)
			}
			if profile != "opencode" && command.ArgumentHint != "<target>" {
				t.Errorf("argument hint = %q", command.ArgumentHint)
			}
			for _, name := range []string{"oversized", "huge-frontmatter"} {
				if hasCommand(catalog, f.prefix+name) {
					t.Errorf("oversized file %q was published", name)
				}
			}
			if catalog.Truncated {
				t.Error("invalid/oversized files marked catalog truncated")
			}
		})
	}
}

func TestCommandDescriptionBodyPrefix(t *testing.T) {
	for _, profile := range []string{"claude", "qoder", "cursor"} {
		t.Run(profile, func(t *testing.T) {
			f := newBudgetProviderFixture(t, profile)
			f.add(t, 0, "fallback", "# Heading\n\nBody description\n", maxCommandFileSize)
			command, ok := findCatalogCommand(f.catalog(), "/fallback")
			if !ok || command.Description != "Body description" {
				t.Errorf("body fallback = %+v, present = %v", command, ok)
			}
		})
	}
}
