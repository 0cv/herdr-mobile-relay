package slashcmd

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

func TestPiTrustUsesOnlyCanonicalDecisionKeys(t *testing.T) {
	for _, decision := range []string{"false", "true", "null", ""} {
		t.Run("canonical="+decision, func(t *testing.T) {
			f := newPiFixture(t)
			canonical, err := filepath.EvalSymlinks(f.repo)
			if err != nil {
				t.Fatal(err)
			}
			alias := filepath.Join(f.home, "project-alias")
			if err := os.Symlink(canonical, alias); err != nil {
				t.Fatal(err)
			}
			writeSkill(t, filepath.Join(canonical, ".pi", "skills"), "project", "Project skill")
			aliasDecision := "true"
			if decision == "true" {
				aliasDecision = "false"
			}
			registry := fmt.Sprintf("{%q:%s", alias, aliasDecision)
			if decision != "" {
				registry += fmt.Sprintf(",%q:%s", canonical, decision)
			}
			registry += "}"
			writeFile(t, filepath.Join(f.home, ".pi", "agent", "trust.json"), registry)
			for attempt := range 32 {
				for _, cwd := range []string{canonical, alias} {
					_, found := commandByName(f.catalog(cwd), "/skill:project")
					if found != (decision == "true") {
						t.Fatalf("attempt %d cwd %q: project discovered = %v, canonical decision = %q", attempt, cwd, found, decision)
					}
				}
			}
		})
	}
}
