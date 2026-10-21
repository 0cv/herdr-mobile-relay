package coordinator

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/0cv/herdr-mobile-relay/internal/herdr"
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
	home := t.TempDir()
	for _, name := range names {
		// The profile needs an identity marker: hermesprofiles mirrors hermes'
		// named_profile_is_live, so a bare directory is a ghost, not a profile.
		dir := filepath.Join(home, ".hermes", "profiles", name)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatalf("create profile %q: %v", name, err)
		}
		if err := os.WriteFile(filepath.Join(dir, "config.yaml"), []byte("{}"), 0o644); err != nil {
			t.Fatalf("write marker for profile %q: %v", name, err)
		}
	}
	if err := os.MkdirAll(filepath.Join(home, "project"), 0o755); err != nil {
		t.Fatal(err)
	}
	binDir := filepath.Join(home, "bin")
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, binary := range []string{"hermes", "codex"} {
		writeScript(t, binDir, binary, "#!/bin/sh\nexit 0\n")
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	return &Lifecycle{
		profiles: profiles.NewResolver(filepath.Join(home, "config"), nil),
		home:     home,
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
			dir := t.TempDir()
			writeScript(t, dir, "hermes", "#!/bin/sh\nexit 0\n")
			t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
			home := filepath.Join(dir, "home")
			for _, profile := range []string{"apollo"} {
				profileDir := filepath.Join(home, ".hermes", "profiles", profile)
				if err := os.MkdirAll(profileDir, 0o755); err != nil {
					t.Fatal(err)
				}
				// hermesprofiles only reports a profile with an identity marker,
				// matching hermes' named_profile_is_live.
				if err := os.WriteFile(filepath.Join(profileDir, "config.yaml"), []byte("{}"), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			cwd := filepath.Join(home, "project")
			if err := os.MkdirAll(cwd, 0o755); err != nil {
				t.Fatal(err)
			}
			record := filepath.Join(dir, "invocations.log")
			bin := writeScript(t, dir, "herdr", "#!/bin/sh\n"+
				"printf '%s\\n' \"$*\" >> \""+record+"\"\n"+
				"case \"$1 $2\" in\n"+
				"  'pane list') printf '%s\\n' '{\"result\":{\"panes\":[]}}' ;;\n"+
				"  'workspace list') printf '%s\\n' '{\"result\":{\"workspaces\":[]}}' ;;\n"+
				"  'workspace create') printf '%s\\n' '{\"result\":{\"type\":\"workspace_created\",\"workspace\":{\"workspace_id\":\"workspace-new\",\"label\":\"project\"},\"tab\":{\"tab_id\":\"tab-new\",\"workspace_id\":\"workspace-new\",\"label\":\"project\"},\"root_pane\":{\"pane_id\":\"pane-new\",\"tab_id\":\"tab-new\",\"workspace_id\":\"workspace-new\"}}}' ;;\n"+
				"  'tab rename') printf '%s\\n' '{\"result\":{\"tab_id\":\"tab-new\"}}' ;;\n"+
				"  'agent start') printf '%s\\n' '{\"result\":{\"type\":\"agent_started\",\"agent\":{\"pane_id\":\"pane-new\",\"agent\":\"hermes\",\"name\":\"project-hermes\"}}}' ;;\n"+
				"  *) exit 2 ;;\n"+
				"esac\n")

			socketPath := filepath.Join(dir, "herdr.sock")
			startInventorySocket(t, socketPath, nil, herdr.Pane{ID: "pane-new", TerminalID: "terminal-new", TabID: "tab-new", WorkspaceID: "workspace-new"})
			lifecycle := &Lifecycle{
				herdr:    herdr.NewClient(bin, socketPath),
				profiles: profiles.NewResolver(filepath.Join(dir, "config"), nil),
				home:     home,
			}
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()

			if _, err := lifecycle.Start(ctx, profiles.Profile{ID: "hermes", Kind: "hermes"}, StartRequest{
				ProfileID:     "hermes",
				Name:          "project-hermes",
				Cwd:           cwd,
				HermesProfile: name,
			}); err != nil {
				t.Fatalf("Start() error = %v", err)
			}

			invocations, err := os.ReadFile(record)
			if err != nil {
				t.Fatalf("read invocations: %v", err)
			}
			want := "agent start project-hermes --kind hermes --pane pane-new --timeout "
			if !strings.Contains(string(invocations), want) {
				t.Fatalf("agent start not recorded as expected:\n%s", invocations)
			}
			if !strings.Contains(string(invocations), "-- -p "+name) {
				t.Fatalf("selected profile missing from argv:\n%s", invocations)
			}
		})
	}
}

// TestLifecycleOmitsProfileFlagWhenUnset keeps the pre-existing argv for a
// Hermes launch that never named a profile.
func TestLifecycleOmitsProfileFlagWhenUnset(t *testing.T) {
	dir := t.TempDir()
	writeScript(t, dir, "hermes", "#!/bin/sh\nexit 0\n")
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	home := filepath.Join(dir, "home")
	cwd := filepath.Join(home, "project")
	if err := os.MkdirAll(cwd, 0o755); err != nil {
		t.Fatal(err)
	}
	record := filepath.Join(dir, "invocations.log")
	bin := writeScript(t, dir, "herdr", "#!/bin/sh\n"+
		"printf '%s\\n' \"$*\" >> \""+record+"\"\n"+
		"case \"$1 $2\" in\n"+
		"  'pane list') printf '%s\\n' '{\"result\":{\"panes\":[]}}' ;;\n"+
		"  'workspace list') printf '%s\\n' '{\"result\":{\"workspaces\":[]}}' ;;\n"+
		"  'workspace create') printf '%s\\n' '{\"result\":{\"type\":\"workspace_created\",\"workspace\":{\"workspace_id\":\"workspace-new\",\"label\":\"project\"},\"tab\":{\"tab_id\":\"tab-new\",\"workspace_id\":\"workspace-new\"},\"root_pane\":{\"pane_id\":\"pane-new\",\"tab_id\":\"tab-new\",\"workspace_id\":\"workspace-new\"}}}' ;;\n"+
		"  'tab rename') printf '%s\\n' '{\"result\":{\"tab_id\":\"tab-new\"}}' ;;\n"+
		"  'agent start') printf '%s\\n' '{\"result\":{\"type\":\"agent_started\",\"agent\":{\"pane_id\":\"pane-new\",\"agent\":\"hermes\"}}}' ;;\n"+
		"  *) exit 2 ;;\n"+
		"esac\n")
	socketPath := filepath.Join(dir, "herdr.sock")
	startInventorySocket(t, socketPath, nil, herdr.Pane{ID: "pane-new", TerminalID: "terminal-new", TabID: "tab-new", WorkspaceID: "workspace-new"})
	lifecycle := &Lifecycle{
		herdr:    herdr.NewClient(bin, socketPath),
		profiles: profiles.NewResolver(filepath.Join(dir, "config"), nil),
		home:     home,
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	if _, err := lifecycle.Start(ctx, profiles.Profile{ID: "hermes", Kind: "hermes"}, StartRequest{
		ProfileID: "hermes",
		Name:      "project-hermes",
		Cwd:       cwd,
	}); err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	invocations, err := os.ReadFile(record)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(invocations), " -- -p ") {
		t.Fatalf("unset profile must not add -p:\n%s", invocations)
	}
}
