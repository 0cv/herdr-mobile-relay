package agentroots

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// clearAllEnv resets every environment variable agentroots reads, so a
// developer's shell exports (or a previous subtest) cannot leak into the
// next scenario.
func clearAllEnv(t *testing.T) {
	t.Helper()
	for _, name := range []string{
		"CLAUDE_CONFIG_DIR", "CODEX_HOME", "PI_CODING_AGENT_DIR",
		ClaudeListEnv, QoderListEnv, CodexListEnv, PiListEnv, OMPListEnv,
	} {
		t.Setenv(name, "")
	}
}

func TestClaudeRoots(t *testing.T) {
	t.Run("home default only", func(t *testing.T) {
		clearAllEnv(t)
		home := t.TempDir()
		want := []string{filepath.Join(home, ".claude", "projects")}
		if got := Claude(home); !slices.Equal(got, want) {
			t.Fatalf("Claude(%q) = %v, want %v", home, got, want)
		}
	})

	t.Run("single env var still honoured", func(t *testing.T) {
		clearAllEnv(t)
		home := t.TempDir()
		t.Setenv("CLAUDE_CONFIG_DIR", "/a")
		want := []string{
			filepath.Join("/a", "projects"),
			filepath.Join(home, ".claude", "projects"),
		}
		if got := Claude(home); !slices.Equal(got, want) {
			t.Fatalf("Claude(%q) = %v, want %v", home, got, want)
		}
	})

	t.Run("list env adds and comes first", func(t *testing.T) {
		clearAllEnv(t)
		home := t.TempDir()
		t.Setenv(ClaudeListEnv, "/x")
		t.Setenv("CLAUDE_CONFIG_DIR", "/a")
		want := []string{
			filepath.Join("/x", "projects"),
			filepath.Join("/a", "projects"),
			filepath.Join(home, ".claude", "projects"),
		}
		if got := Claude(home); !slices.Equal(got, want) {
			t.Fatalf("Claude(%q) = %v, want %v", home, got, want)
		}
	})

	t.Run("multi entry list keeps order", func(t *testing.T) {
		clearAllEnv(t)
		home := t.TempDir()
		t.Setenv(ClaudeListEnv, strings.Join([]string{"/x", "/y"}, string(os.PathListSeparator)))
		want := []string{
			filepath.Join("/x", "projects"),
			filepath.Join("/y", "projects"),
			filepath.Join(home, ".claude", "projects"),
		}
		if got := Claude(home); !slices.Equal(got, want) {
			t.Fatalf("Claude(%q) = %v, want %v", home, got, want)
		}
	})

	t.Run("de-duplication of list entry against single var", func(t *testing.T) {
		clearAllEnv(t)
		home := t.TempDir()
		t.Setenv(ClaudeListEnv, "/a")
		t.Setenv("CLAUDE_CONFIG_DIR", "/a")
		want := []string{
			filepath.Join("/a", "projects"),
			filepath.Join(home, ".claude", "projects"),
		}
		if got := Claude(home); !slices.Equal(got, want) {
			t.Fatalf("Claude(%q) = %v, want %v", home, got, want)
		}
	})

	t.Run("de-duplication of list entry against home base", func(t *testing.T) {
		clearAllEnv(t)
		home := t.TempDir()
		t.Setenv(ClaudeListEnv, filepath.Join(home, ".claude"))
		want := []string{filepath.Join(home, ".claude", "projects")}
		if got := Claude(home); !slices.Equal(got, want) {
			t.Fatalf("Claude(%q) = %v, want %v", home, got, want)
		}
	})

	t.Run("blank and whitespace-only entries are skipped", func(t *testing.T) {
		clearAllEnv(t)
		home := t.TempDir()
		t.Setenv(ClaudeListEnv, strings.Join([]string{"/x", "", "  ", "/y"}, string(os.PathListSeparator)))
		want := []string{
			filepath.Join("/x", "projects"),
			filepath.Join("/y", "projects"),
			filepath.Join(home, ".claude", "projects"),
		}
		if got := Claude(home); !slices.Equal(got, want) {
			t.Fatalf("Claude(%q) = %v, want %v", home, got, want)
		}
	})
}

func TestQoderRoots(t *testing.T) {
	t.Run("no single var override", func(t *testing.T) {
		clearAllEnv(t)
		home := t.TempDir()
		t.Setenv("CLAUDE_CONFIG_DIR", "/should-not-affect-qoder")
		t.Setenv("CODEX_HOME", "/also-should-not-affect-qoder")
		t.Setenv("PI_CODING_AGENT_DIR", "/nor-this")
		want := []string{filepath.Join(home, ".qoder", "projects")}
		if got := Qoder(home); !slices.Equal(got, want) {
			t.Fatalf("Qoder(%q) = %v, want %v (single-dir env vars must not affect Qoder)", home, got, want)
		}
	})

	t.Run("home default and list env", func(t *testing.T) {
		clearAllEnv(t)
		home := t.TempDir()
		t.Setenv(QoderListEnv, "/q")
		want := []string{
			filepath.Join("/q", "projects"),
			filepath.Join(home, ".qoder", "projects"),
		}
		if got := Qoder(home); !slices.Equal(got, want) {
			t.Fatalf("Qoder(%q) = %v, want %v", home, got, want)
		}
	})
}

func TestCodexRootsAndHomes(t *testing.T) {
	scenarios := []struct {
		name  string
		setup func(t *testing.T)
	}{
		{name: "default only", setup: func(t *testing.T) {}},
		{name: "CODEX_HOME override", setup: func(t *testing.T) {
			t.Setenv("CODEX_HOME", "/c")
		}},
		{name: "list env adds and comes first", setup: func(t *testing.T) {
			t.Setenv(CodexListEnv, "/c1")
			t.Setenv("CODEX_HOME", "/c2")
		}},
	}
	for _, scenario := range scenarios {
		t.Run(scenario.name, func(t *testing.T) {
			clearAllEnv(t)
			home := t.TempDir()
			scenario.setup(t)

			sessions := Codex(home)
			homes := CodexHomes(home)
			if len(sessions) != len(homes) {
				t.Fatalf("len(Codex(home)) = %d, len(CodexHomes(home)) = %d, want equal (%v vs %v)", len(sessions), len(homes), sessions, homes)
			}
			for index := range homes {
				want := filepath.Join(homes[index], "sessions")
				if sessions[index] != want {
					t.Fatalf("Codex(home)[%d] = %q, want %q (= filepath.Join(CodexHomes(home)[%d], \"sessions\"))", index, sessions[index], want, index)
				}
			}
		})
	}
}

func TestOMPAndPiRoots(t *testing.T) {
	t.Run("distinct home defaults", func(t *testing.T) {
		clearAllEnv(t)
		home := t.TempDir()
		wantPi := []string{filepath.Join(home, ".pi", "agent", "sessions")}
		wantOMP := []string{filepath.Join(home, ".omp", "agent", "sessions")}
		if got := Pi(home); !slices.Equal(got, wantPi) {
			t.Fatalf("Pi(%q) = %v, want %v", home, got, wantPi)
		}
		if got := OMP(home); !slices.Equal(got, wantOMP) {
			t.Fatalf("OMP(%q) = %v, want %v", home, got, wantOMP)
		}
	})

	t.Run("shared single var, distinct base", func(t *testing.T) {
		clearAllEnv(t)
		home := t.TempDir()
		t.Setenv("PI_CODING_AGENT_DIR", "/shared")
		wantPi := []string{
			filepath.Join("/shared", "sessions"),
			filepath.Join(home, ".pi", "agent", "sessions"),
		}
		wantOMP := []string{
			filepath.Join("/shared", "sessions"),
			filepath.Join(home, ".omp", "agent", "sessions"),
		}
		if got := Pi(home); !slices.Equal(got, wantPi) {
			t.Fatalf("Pi(%q) = %v, want %v", home, got, wantPi)
		}
		if got := OMP(home); !slices.Equal(got, wantOMP) {
			t.Fatalf("OMP(%q) = %v, want %v", home, got, wantOMP)
		}
	})

	t.Run("independent list vars", func(t *testing.T) {
		clearAllEnv(t)
		home := t.TempDir()
		t.Setenv(PiListEnv, "/pi-profile")
		t.Setenv(OMPListEnv, "/omp-profile")
		wantPi := []string{
			filepath.Join("/pi-profile", "sessions"),
			filepath.Join(home, ".pi", "agent", "sessions"),
		}
		wantOMP := []string{
			filepath.Join("/omp-profile", "sessions"),
			filepath.Join(home, ".omp", "agent", "sessions"),
		}
		if got := Pi(home); !slices.Equal(got, wantPi) {
			t.Fatalf("Pi(%q) = %v, want %v (PiListEnv must not leak into OMP)", home, got, wantPi)
		}
		if got := OMP(home); !slices.Equal(got, wantOMP) {
			t.Fatalf("OMP(%q) = %v, want %v (OMPListEnv must not leak into Pi)", home, got, wantOMP)
		}
	})
}
