package coordinator

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/0cv/herdr-mobile-relay/internal/hermesprofiles"
	"github.com/0cv/herdr-mobile-relay/internal/profiles"
)

// TestHermesProfileArgs covers the last frame before the picker's value becomes
// argv: an empty choice adds no -p (hermes keeps its sticky active_profile), an
// explicit name travels as `-p <name>`, and a name that cannot be a profile is an
// error instead of a silent omission that would start a different agent.
func TestHermesProfileArgs(t *testing.T) {
	cases := []struct {
		name    string
		profile string
		want    []string
		wantErr bool
	}{
		{name: "unset", profile: ""},
		{name: "named", profile: "apollo", want: []string{"-p", "apollo"}},
		{name: "default", profile: hermesprofiles.Default, want: []string{"-p", hermesprofiles.Default}},
		{name: "malformed", profile: "Bad Name", wantErr: true},
		// 65 characters: one past the longest name NamePattern accepts.
		{name: "too long", profile: "a" + strings.Repeat("b", 64), wantErr: true},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			args, err := hermesProfileArgs(test.profile)
			if test.wantErr {
				if err == nil {
					t.Fatalf("hermesProfileArgs(%q) error = nil, want a rejection", test.profile)
				}
				if args != nil {
					t.Fatalf("hermesProfileArgs(%q) args = %#v, want nil", test.profile, args)
				}
				return
			}
			if err != nil {
				t.Fatalf("hermesProfileArgs(%q) error = %v", test.profile, err)
			}
			if !reflect.DeepEqual(args, test.want) {
				t.Fatalf("hermesProfileArgs(%q) = %#v, want %#v", test.profile, args, test.want)
			}
		})
	}
}

// hermesLifecycle builds a Lifecycle whose home carries the given profile
// directories, so Exists answers against the test's own home rather than the
// machine running the suite.
//
// The resolver only reports a profile whose executable is on PATH, so the test
// provides stub `hermes` and `codex` binaries: without them the kind check that
// rejects a stray hermes_profile is masked by "profile_id is not available" on
// a host that does not have every agent installed.
func hermesLifecycle(t *testing.T, names ...string) *Lifecycle {
	t.Helper()
	t.Setenv("HERMES_HOME", "")
	t.Setenv("HERDR_HERMES_DATA_DIRS", "")
	env := newLaunchEnv(t, "hermes", "codex")
	for _, name := range names {
		// The profile needs an identity marker: hermesprofiles mirrors hermes'
		// named_profile_is_live, so a bare directory is a ghost, not a profile.
		dir := filepath.Join(env.home, ".hermes", "profiles", name)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatalf("create profile %q: %v", name, err)
		}
		if err := os.WriteFile(filepath.Join(dir, "config.yaml"), []byte("{}"), 0o644); err != nil {
			t.Fatalf("write marker for profile %q: %v", name, err)
		}
	}
	return &Lifecycle{
		profiles: profiles.NewResolver(env.config, nil),
		home:     env.home,
	}
}

func TestValidateStartAcceptsHermesProfile(t *testing.T) {
	lifecycle := hermesLifecycle(t, "apollo")
	cwd := filepath.Join(lifecycle.home, "project")

	for _, name := range []string{"", "apollo", hermesprofiles.Default} {
		t.Run("profile="+name, func(t *testing.T) {
			profile, request, err := lifecycle.ValidateStart(StartRequest{
				ProfileID:     "hermes",
				Name:          "project-hermes",
				Cwd:           cwd,
				HermesProfile: name,
			})
			if err != nil {
				t.Fatalf("ValidateStart() error = %v", err)
			}
			if profile.Kind != "hermes" {
				t.Fatalf("profile.Kind = %q, want hermes", profile.Kind)
			}
			if request.HermesProfile != name {
				t.Fatalf("request.HermesProfile = %q, want %q", request.HermesProfile, name)
			}
		})
	}
}

func TestValidateStartRejectsHermesProfileForOtherAgents(t *testing.T) {
	lifecycle := hermesLifecycle(t, "apollo")
	_, _, err := lifecycle.ValidateStart(StartRequest{
		ProfileID:     "codex",
		Name:          "project-codex",
		Cwd:           filepath.Join(lifecycle.home, "project"),
		HermesProfile: "apollo",
	})
	if err == nil || !strings.Contains(err.Error(), "only valid for Hermes agents") {
		t.Fatalf("ValidateStart() error = %v, want the wrong-kind rejection", err)
	}
}

func TestValidateStartRejectsMalformedHermesProfile(t *testing.T) {
	lifecycle := hermesLifecycle(t, "apollo")
	_, _, err := lifecycle.ValidateStart(StartRequest{
		ProfileID:     "hermes",
		Name:          "project-hermes",
		Cwd:           filepath.Join(lifecycle.home, "project"),
		HermesProfile: "Bad Name",
	})
	if err == nil || !strings.Contains(err.Error(), "hermes_profile must match") {
		t.Fatalf("ValidateStart() error = %v, want the name-shape rejection", err)
	}
}

func TestValidateStartRejectsUninstalledHermesProfile(t *testing.T) {
	lifecycle := hermesLifecycle(t, "apollo")
	_, _, err := lifecycle.ValidateStart(StartRequest{
		ProfileID:     "hermes",
		Name:          "project-hermes",
		Cwd:           filepath.Join(lifecycle.home, "project"),
		HermesProfile: "nope",
	})
	if err == nil || !strings.Contains(err.Error(), `hermes profile "nope" is not installed`) {
		t.Fatalf("ValidateStart() error = %v, want the missing-profile rejection", err)
	}
}

// TestLifecycleStartsHermesWithSelectedProfile proves the picked profile reaches
// argv, and that `default` is sent when it was picked explicitly.
func TestLifecycleStartsHermesWithSelectedProfile(t *testing.T) {
	for _, name := range []string{"apollo", hermesprofiles.Default} {
		t.Run("profile="+name, func(t *testing.T) {
			lifecycle := hermesLifecycle(t, "apollo")
			client := newLaunchFake()
			lifecycle.herdr = client
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()

			result, err := lifecycle.Start(ctx, profiles.Profile{ID: "hermes", Kind: "hermes"}, StartRequest{
				ProfileID:     "hermes",
				Name:          "project-hermes",
				Cwd:           filepath.Join(lifecycle.home, "project"),
				HermesProfile: name,
			})
			if err != nil {
				t.Fatalf("Start() error = %v", err)
			}
			if result.PaneID != "pane-new" {
				t.Fatalf("Start() pane_id = %q, want pane-new", result.PaneID)
			}

			starts := client.callsTo("StartAgent")
			if len(starts) != 1 {
				t.Fatalf("agent start not recorded as expected: %+v", client.recorded())
			}
			if !reflect.DeepEqual(starts[0].Args[:3], []any{"project-hermes", "hermes", "pane-new"}) {
				t.Fatalf("agent start arguments = %+v", starts[0].Args)
			}
			if got := starts[0].Args[4]; !reflect.DeepEqual(got, []string{"-p", name}) {
				t.Fatalf("selected profile missing from argv: %#v", got)
			}
		})
	}
}

// TestLifecycleOmitsProfileFlagWhenUnset keeps the pre-existing argv for a
// Hermes launch that never named a profile.
func TestLifecycleOmitsProfileFlagWhenUnset(t *testing.T) {
	lifecycle := hermesLifecycle(t)
	client := newLaunchFake()
	lifecycle.herdr = client
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	result, err := lifecycle.Start(ctx, profiles.Profile{ID: "hermes", Kind: "hermes"}, StartRequest{
		ProfileID: "hermes",
		Name:      "project-hermes",
		Cwd:       filepath.Join(lifecycle.home, "project"),
	})
	if err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	if result.PaneID != "pane-new" {
		t.Fatalf("Start() pane_id = %q, want pane-new", result.PaneID)
	}
	starts := client.callsTo("StartAgent")
	if len(starts) != 1 {
		t.Fatalf("agent start not recorded as expected: %+v", client.recorded())
	}
	if got := starts[0].Args[4]; len(got.([]string)) != 0 {
		t.Fatalf("unset profile must not add -p: %#v", got)
	}
}
