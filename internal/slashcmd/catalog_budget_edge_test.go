package slashcmd

import (
	"fmt"
	"path/filepath"
	"testing"
)

func TestCatalogBudgetEdge(t *testing.T) {
	cases := []struct {
		profile  string
		builtins []Command
		// A budget that ends at exactly zero is reported as truncated by every
		// provider except hermes, which only notices a file it had to skip.
		truncatedAtLimit bool
		add              func(t *testing.T, home string, index int)
	}{
		{"claude", claudeBuiltins, true, func(t *testing.T, home string, index int) {
			writeFile(t, filepath.Join(home, ".claude", "commands", fmt.Sprintf("edge-%04d.md", index)), "Edge")
		}},
		{"qoder", qoderBuiltins, true, func(t *testing.T, home string, index int) {
			writeFile(t, filepath.Join(home, ".qoder", "commands", fmt.Sprintf("edge-%04d.md", index)), "Edge")
		}},
		{"pi", piBuiltins, true, func(t *testing.T, home string, index int) {
			writeSkill(t, filepath.Join(home, ".pi", "agent", "skills"), fmt.Sprintf("edge-%04d", index), "Edge")
		}},
		{"hermes", hermesBuiltins, false, func(t *testing.T, home string, index int) {
			writeSkill(t, filepath.Join(home, ".hermes", "skills"), fmt.Sprintf("edge-%04d", index), "Edge")
		}},
	}
	for _, tc := range cases {
		for _, files := range []int{maxCustomFiles, maxCustomFiles + 1} {
			wantTruncated := files > maxCustomFiles || tc.truncatedAtLimit
			t.Run(fmt.Sprintf("%s/%d", tc.profile, files), func(t *testing.T) {
				isolateAgentEnv(t)
				home := t.TempDir()
				for index := range files {
					tc.add(t, home, index)
				}
				catalog := CatalogForProfile(tc.profile, tc.profile, t.TempDir(), home, nil, "", "")
				if catalog.Truncated != wantTruncated {
					t.Errorf("truncated = %v, want %v", catalog.Truncated, wantTruncated)
				}
				if custom := len(catalog.Commands) - len(tc.builtins); custom != maxCustomFiles {
					t.Errorf("custom commands = %d, want %d", custom, maxCustomFiles)
				}
			})
		}
	}
}
