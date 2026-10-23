package coordinator

import (
	"path/filepath"
	"testing"

	"github.com/0cv/herdr-mobile-relay/internal/profiles"
)

// The home directory itself is a valid launch directory for a terminal
// profile only. Agents still need a project folder below it.
func TestResolveCwdHomeDirectoryByProfile(t *testing.T) {
	home := t.TempDir()
	resolvedHome, err := filepath.EvalSymlinks(home)
	if err != nil {
		t.Fatal(err)
	}
	lifecycle := &Lifecycle{home: home}

	cases := []struct {
		name        string
		profile     profiles.Profile
		wantAllowed bool
	}{
		{name: "default rejects home", profile: profiles.Profile{ID: "claude"}},
		{name: "terminal accepts home", profile: profiles.Profile{ID: "terminal", IsTerminal: true}, wantAllowed: true},
		{name: "agent rejects home", profile: profiles.Profile{ID: "hermes", Kind: "hermes"}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			resolved, err := lifecycle.ResolveCwdForProfile(resolvedHome, tc.profile)
			if tc.wantAllowed {
				if err != nil {
					t.Fatalf("ResolveCwdForProfile(%q) error = %v, want nil", resolvedHome, err)
				}
				if resolved != resolvedHome {
					t.Fatalf("resolved cwd = %q, want %q", resolved, resolvedHome)
				}
				return
			}
			if err == nil {
				t.Fatalf("ResolveCwdForProfile(%q) = %q, want an error", resolvedHome, resolved)
			}
		})
	}

	if _, err := lifecycle.ResolveCwd(resolvedHome); err == nil {
		t.Fatalf("ResolveCwd(%q) = nil error, want the home directory rejected", resolvedHome)
	}
}
