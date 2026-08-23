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
		want  func(home string) []string
	}{
		{
			name:  "default only",
			setup: func(t *testing.T) {},
			want: func(home string) []string {
				return []string{filepath.Join(home, ".codex")}
			},
		},
		{
			name: "CODEX_HOME override",
			setup: func(t *testing.T) {
				t.Setenv("CODEX_HOME", "/c")
			},
			want: func(home string) []string {
				return []string{"/c", filepath.Join(home, ".codex")}
			},
		},
		{
			name: "list env adds and comes first",
			setup: func(t *testing.T) {
				t.Setenv(CodexListEnv, "/c1")
				t.Setenv("CODEX_HOME", "/c2")
			},
			want: func(home string) []string {
				return []string{"/c1", "/c2", filepath.Join(home, ".codex")}
			},
		},
	}
	for _, scenario := range scenarios {
		t.Run(scenario.name, func(t *testing.T) {
			clearAllEnv(t)
			home := t.TempDir()
			scenario.setup(t)

			homes := CodexHomes(home)
			wantHomes := scenario.want(home)
			// The ordering itself is the thing under test - CodexHomes must
			// reproduce the configured precedence (list env, then the single
			// var, then the home default) exactly, not merely contain the
			// same elements. A prior version of this assertion only checked
			// len(sessions) == len(homes) and the zip relationship below,
			// both of which stay true under a reordering bug (e.g. adding
			// the single-env entry before the list-env loop), so it never
			// caught one.
			if !slices.Equal(homes, wantHomes) {
				t.Fatalf("CodexHomes(%q) = %v, want %v", home, homes, wantHomes)
			}

			sessions := Codex(home)
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

// A named profile makes Oh My Pi ignore PI_CODING_AGENT_DIR entirely, so a
// profile's sessions are reachable no other way than by knowing the layout.
// Discovering <config root>/profiles/<name>/agent is what makes the common
// one-profile-per-herdr-setup case work with no configuration at all.
func TestOMPDiscoversNamedProfileAgentDirs(t *testing.T) {
	clearAllEnv(t)
	home := t.TempDir()
	for _, name := range []string{"personal", "work"} {
		if err := os.MkdirAll(filepath.Join(home, ".omp", "profiles", name, "agent", "sessions"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	// A stray file under profiles/ must not become a root.
	if err := os.WriteFile(filepath.Join(home, ".omp", "profiles", "notes.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	got := OMP(home)
	want := []string{
		filepath.Join(home, ".omp", "profiles", "personal", "agent", "sessions"),
		filepath.Join(home, ".omp", "profiles", "work", "agent", "sessions"),
		filepath.Join(home, ".omp", "agent", "sessions"),
	}
	if !slices.Equal(got, want) {
		t.Fatalf("OMP(home) = %v, want discovered profiles then the home default %v", got, want)
	}
}

// Discovery must not disturb configuration precedence: an explicitly configured
// directory still comes first, and the home default still comes last.
func TestOMPConfigurationOutranksDiscoveredProfiles(t *testing.T) {
	clearAllEnv(t)
	home := t.TempDir()
	explicit := t.TempDir()
	if err := os.MkdirAll(filepath.Join(home, ".omp", "profiles", "personal", "agent"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv(OMPListEnv, explicit)

	got := OMP(home)
	want := []string{
		filepath.Join(explicit, "sessions"),
		filepath.Join(home, ".omp", "profiles", "personal", "agent", "sessions"),
		filepath.Join(home, ".omp", "agent", "sessions"),
	}
	if !slices.Equal(got, want) {
		t.Fatalf("OMP(home) = %v, want %v", got, want)
	}
}

// Discovery is additive: with no profiles directory the result is exactly what
// it was before discovery existed, so a default install is unaffected.
func TestDiscoveryIsANoOpWithoutProfiles(t *testing.T) {
	clearAllEnv(t)
	home := t.TempDir()
	for _, c := range []struct {
		name string
		got  []string
		want []string
	}{
		{"OMP", OMP(home), []string{filepath.Join(home, ".omp", "agent", "sessions")}},
		{"Pi", Pi(home), []string{filepath.Join(home, ".pi", "agent", "sessions")}},
	} {
		if !slices.Equal(c.got, c.want) {
			t.Errorf("%s(home) = %v, want %v", c.name, c.got, c.want)
		}
	}
}

// An explicitly configured entry is an agent directory, not a config root, so
// its sibling profiles/ must never be expanded.
func TestDiscoveryDoesNotExpandConfiguredEntries(t *testing.T) {
	clearAllEnv(t)
	home := t.TempDir()
	explicit := t.TempDir()
	if err := os.MkdirAll(filepath.Join(explicit, "profiles", "sneaky", "agent"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv(OMPListEnv, filepath.Join(explicit, "agent"))

	roots := OMP(home)
	for _, root := range roots {
		if strings.Contains(root, "sneaky") {
			t.Fatalf("expanded a configured entry's profiles/: %v", roots)
		}
	}
}

// Pi gets the same treatment as OMP, under its own config root, and the two
// must not bleed into each other.
func TestPiDiscoversNamedProfileAgentDirs(t *testing.T) {
	clearAllEnv(t)
	home := t.TempDir()
	if err := os.MkdirAll(filepath.Join(home, ".pi", "profiles", "alt", "agent"), 0o755); err != nil {
		t.Fatal(err)
	}

	got := Pi(home)
	want := []string{
		filepath.Join(home, ".pi", "profiles", "alt", "agent", "sessions"),
		filepath.Join(home, ".pi", "agent", "sessions"),
	}
	if !slices.Equal(got, want) {
		t.Fatalf("Pi(home) = %v, want %v", got, want)
	}
	for _, root := range OMP(home) {
		if strings.Contains(root, ".pi") {
			t.Fatalf("OMP root list contains a Pi path: %v", OMP(home))
		}
	}
}

// The seam classifies profile entries with os.Stat, not DirEntry.IsDir, because
// DirEntry reports the entry's own type bits (ModeSymlink for a symlink), so a
// profile reached through a symlink used to be silently skipped even though omp
// itself joins and follows that path. The discovered root uses the lexical path
// through the symlink - containment resolution happens later, not here.
func TestOMPDiscoversProfileReachedThroughASymlink(t *testing.T) {
	clearAllEnv(t)
	home := t.TempDir()
	if err := os.MkdirAll(filepath.Join(home, ".omp", "profiles", "personal", "agent"), 0o755); err != nil {
		t.Fatal(err)
	}
	target := t.TempDir()
	link := filepath.Join(home, ".omp", "profiles", "linked")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}

	got := OMP(home)
	want := []string{
		// "linked" sorts before "personal" in os.ReadDir's order.
		filepath.Join(home, ".omp", "profiles", "linked", "agent", "sessions"),
		filepath.Join(home, ".omp", "profiles", "personal", "agent", "sessions"),
		filepath.Join(home, ".omp", "agent", "sessions"),
	}
	if !slices.Equal(got, want) {
		t.Fatalf("OMP(home) = %v, want the symlinked profile (lexical path, unresolved) and the real "+
			"profile in os.ReadDir order, then the home default: %v", got, want)
	}
}

// A dangling symlink or a plain file directly under profiles/ must not become
// a root: os.Stat on a dangling symlink errors, and a regular file is not a
// directory either way.
func TestDiscoverySkipsBrokenSymlinksAndFiles(t *testing.T) {
	clearAllEnv(t)
	home := t.TempDir()
	profiles := filepath.Join(home, ".omp", "profiles")
	if err := os.MkdirAll(profiles, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(home, "nowhere"), filepath.Join(profiles, "broken")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(profiles, "afile.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	got := OMP(home)
	want := []string{filepath.Join(home, ".omp", "agent", "sessions")}
	if !slices.Equal(got, want) {
		t.Fatalf("OMP(home) = %v, want exactly the home default (broken symlink and file must be skipped): %v", got, want)
	}
}

// A relative config root reaches this package only when os.UserHomeDir()
// failed and home is "". profileAgentDirs refuses to scan it and add() refuses
// to keep it, so with home == "" nothing is returned at all - not the
// discovered profiles, and not the relative home default ".omp/agent/sessions"
// either. The working directory here deliberately contains a profiles/ layout
// that a relative scan would find, so the assertion is non-vacuous: drop
// either guard and one of those trees shows up in the result.
func TestDiscoveryIgnoresRelativeConfigRoot(t *testing.T) {
	clearAllEnv(t)
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, ".omp", "profiles", "x", "agent"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dir, ".pi", "profiles", "y", "agent"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Chdir(dir)

	for _, c := range []struct {
		name string
		got  []string
	}{
		{"OMP", OMP("")},
		{"Pi", Pi("")},
	} {
		if len(c.got) != 0 {
			t.Fatalf("%s(\"\") = %v, want no roots at all: every base derived from an empty home is relative", c.name, c.got)
		}
	}
}

// A discovered profile whose agent directory is also named explicitly in
// HERDR_OMP_CONFIG_DIRS must not appear twice: resolve's de-duplication keys
// on the final joined root, and the configured occurrence - added first -
// must be the one that wins the position.
func TestDiscoveredProfileDedupesAgainstAnIdenticalConfiguredEntry(t *testing.T) {
	clearAllEnv(t)
	home := t.TempDir()
	profileAgent := filepath.Join(home, ".omp", "profiles", "personal", "agent")
	if err := os.MkdirAll(profileAgent, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv(OMPListEnv, profileAgent)

	got := OMP(home)
	root := filepath.Join(profileAgent, "sessions")
	count := 0
	for _, r := range got {
		if r == root {
			count++
		}
	}
	if count != 1 {
		t.Fatalf("OMP(home) = %v contains %d occurrences of %q, want exactly 1", got, count, root)
	}
	if len(got) == 0 || got[0] != root {
		t.Fatalf("OMP(home) = %v, want the configured entry %q first", got, root)
	}
	want := []string{root, filepath.Join(home, ".omp", "agent", "sessions")}
	if !slices.Equal(got, want) {
		t.Fatalf("OMP(home) = %v, want %v", got, want)
	}
}

// A profiles/ directory that cannot be read (permissions, not absence) must
// degrade to the home default rather than panicking or propagating an error.
func TestDiscoveryUnreadableProfilesDirIsANoOp(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("running as root: chmod 0o000 does not block directory reads for root")
	}
	clearAllEnv(t)
	home := t.TempDir()
	profiles := filepath.Join(home, ".omp", "profiles")
	if err := os.MkdirAll(profiles, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(profiles, 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.Chmod(profiles, 0o755); err != nil {
			t.Fatal(err)
		}
	})

	got := OMP(home)
	want := []string{filepath.Join(home, ".omp", "agent", "sessions")}
	if !slices.Equal(got, want) {
		t.Fatalf("OMP(home) = %v, want exactly the home default when profiles/ is unreadable: %v", got, want)
	}
}

// The README's only worked example configures a value as "~/agents/claude-
// work"; a leading "~/" has to expand against home or that documented syntax
// never actually finds anything.
func TestConfiguredEntryExpandsTilde(t *testing.T) {
	clearAllEnv(t)
	home := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", "~/work/claude")
	want := []string{
		filepath.Join(home, "work", "claude", "projects"),
		filepath.Join(home, ".claude", "projects"),
	}
	if got := Claude(home); !slices.Equal(got, want) {
		t.Fatalf("Claude(%q) = %v, want %v (a leading ~/ must expand against home)", home, got, want)
	}
}

// A bare "~" (no trailing slash) means home itself, not "home joined with the
// empty string" by accident of string handling.
func TestConfiguredEntryExpandsBareTilde(t *testing.T) {
	clearAllEnv(t)
	home := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", "~")
	want := []string{
		filepath.Join(home, "projects"),
		filepath.Join(home, ".claude", "projects"),
	}
	if got := Claude(home); !slices.Equal(got, want) {
		t.Fatalf("Claude(%q) = %v, want %v (a bare ~ must expand to home itself)", home, got, want)
	}
}

// A relative entry - one that never started with "~" at all - must be
// dropped rather than silently resolved against the relay's working
// directory. This is the same hazard profileAgentDirs already guards against
// for a relative config root; add() now closes the matching half of it for a
// configured entry.
func TestConfiguredRelativeEntryIsSkipped(t *testing.T) {
	clearAllEnv(t)
	home := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", "relative/path")
	want := []string{filepath.Join(home, ".claude", "projects")}
	if got := Claude(home); !slices.Equal(got, want) {
		t.Fatalf("Claude(%q) = %v, want %v (a relative entry must be dropped, not resolved against the working directory)", home, got, want)
	}
}

// "~otheruser/..." names another account's home directory. This package has
// no way to resolve that and must not guess: it is left untouched by
// expandTilde and then rejected by the same absolute-path guard as any other
// relative string, rather than being joined onto the caller's own home.
func TestConfiguredTildeUserFormIsSkipped(t *testing.T) {
	clearAllEnv(t)
	home := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", "~otheruser/claude")
	want := []string{filepath.Join(home, ".claude", "projects")}
	if got := Claude(home); !slices.Equal(got, want) {
		t.Fatalf("Claude(%q) = %v, want %v (a ~user form must be dropped, not guessed at)", home, got, want)
	}
}

// Tilde expansion has to happen before de-duplication, not after: "~/x" and
// "<home>/x" name the same directory, and resolve's seen-set keys on the
// final joined root, so if expansion ran after de-duplication (or not at
// all) these would compare unequal and both would survive.
func TestTildeExpansionDedupesAgainstEquivalentAbsoluteEntry(t *testing.T) {
	clearAllEnv(t)
	home := t.TempDir()
	t.Setenv(ClaudeListEnv, strings.Join([]string{"~/x", filepath.Join(home, "x")}, string(os.PathListSeparator)))
	want := []string{
		filepath.Join(home, "x", "projects"),
		filepath.Join(home, ".claude", "projects"),
	}
	if got := Claude(home); !slices.Equal(got, want) {
		t.Fatalf("Claude(%q) = %v, want %v (tilde expansion must happen before de-duplication so ~/x and <home>/x collapse to one root)", home, got, want)
	}
}

// De-duplication keys on the *joined* root, and filepath.Join cleans the path
// it builds, so several spellings of one directory collapse into a single
// entry. The list is edited by hand in relay.env, where a trailing separator
// or a "/./" is a spelling and not a second root; a surviving duplicate would
// make every consumer walk the same tree twice. This pins the normalisation
// form rather than just the behaviour: de-duplicating the raw base before the
// leaf is joined, or building the root by string concatenation instead of
// filepath.Join, leaves these spellings distinct and fails here.
func TestDeduplicationNormalizesEquivalentSpellings(t *testing.T) {
	clearAllEnv(t)
	home := t.TempDir()
	shared := filepath.Join(home, "shared")
	sep := string(os.PathSeparator)
	// Built by concatenation on purpose: filepath.Join would clean these
	// spellings away before resolve ever saw them.
	t.Setenv(ClaudeListEnv, strings.Join([]string{
		shared,
		shared + sep,
		shared + sep + "." + sep,
		shared + sep + sep,
		shared + sep + "sub" + sep + "..",
	}, string(os.PathListSeparator)))

	want := []string{
		filepath.Join(shared, "projects"),
		filepath.Join(home, ".claude", "projects"),
	}
	if got := Claude(home); !slices.Equal(got, want) {
		t.Fatalf("Claude(%q) = %v, want %v (equivalent spellings of one directory must collapse to a single root)", home, got, want)
	}
}
