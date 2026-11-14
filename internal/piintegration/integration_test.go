package piintegration

import (
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	herdrmobilerelay "github.com/0cv/herdr-mobile-relay"
	"github.com/0cv/herdr-mobile-relay/internal/agentroots"
)

func bundle(t *testing.T) fs.FS {
	t.Helper()
	return herdrmobilerelay.PiCommandBridge()
}

func put(t *testing.T, path, text string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(text), 0600); err != nil {
		t.Fatal(err)
	}
}

func target(dir string) string { return filepath.Join(dir, "extensions", Directory) }

func TestInstallRefreshRemove(t *testing.T) {
	dir := t.TempDir()
	m := New(bundle(t), func() []string { return []string{dir} })
	if m.Status().Enabled || m.Refresh().Enabled {
		t.Fatal("installed without opt-in")
	}
	if _, err := os.Stat(target(dir)); !os.IsNotExist(err) {
		t.Fatal("created files without opt-in")
	}
	put(t, filepath.Join(dir, "settings.json"), "user settings")
	put(t, filepath.Join(dir, "extensions", "herdr.ts"), "Herdr extension")
	if state := m.Set(true); !state.Enabled || state.Profiles[0].Status != "installed" {
		t.Fatalf("%+v", state)
	}
	if state := m.Set(true); state.Profiles[0].Status != "up to date" {
		t.Fatalf("%+v", state)
	}
	for _, name := range Files {
		info, err := os.Stat(filepath.Join(target(dir), name))
		if err != nil || info.Mode().Perm() != 0600 {
			t.Fatalf("%s permissions: %v %v", name, info, err)
		}
	}
	info, _ := os.Stat(target(dir))
	if info.Mode().Perm() != 0700 {
		t.Fatal("directory permissions")
	}
	if err := os.Chmod(target(dir), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(filepath.Join(target(dir), "bridge.mjs"), 0644); err != nil {
		t.Fatal(err)
	}
	if m.Refresh().Profiles[0].Status != "installed" {
		t.Fatal("did not repair permissions")
	}
	info, _ = os.Stat(target(dir))
	fileInfo, _ := os.Stat(filepath.Join(target(dir), "bridge.mjs"))
	if info.Mode().Perm() != 0700 || fileInfo.Mode().Perm() != 0600 {
		t.Fatal("unsafe refreshed permissions")
	}
	put(t, filepath.Join(target(dir), "index.ts"), "outdated")
	if m.Status().Profiles[0].Status != "outdated" {
		t.Fatal("missed outdated install")
	}
	if state := m.Refresh(); state.Profiles[0].Status != "installed" {
		t.Fatalf("%+v", state)
	}
	put(t, filepath.Join(target(dir), "unrelated"), "keep")
	if state := m.Set(false); state.Enabled || state.Profiles[0].Status != "removed" {
		t.Fatalf("%+v", state)
	}
	for _, name := range Files {
		if _, err := os.Stat(filepath.Join(target(dir), name)); !os.IsNotExist(err) {
			t.Fatal("not removed", name)
		}
	}
	for _, name := range []string{"settings.json", "extensions/herdr.ts", "extensions/" + Directory + "/unrelated"} {
		if _, err := os.Stat(filepath.Join(dir, name)); err != nil {
			t.Fatal("changed unrelated file", err)
		}
	}
	if m.Set(true).Profiles[0].Status != "refused" {
		t.Fatal("adopted leftover unowned directory")
	}
}

func TestRefuseUnsafeProfiles(t *testing.T) {
	for _, kind := range []string{"extensions", "target", "index.ts", "bridge.mjs", "activity.mjs", ".relay-owned", "unowned", "wrong marker", "directory file", "relative"} {
		t.Run(kind, func(t *testing.T) {
			dir, outside := t.TempDir(), t.TempDir()
			put(t, filepath.Join(outside, "untouched"), "original")
			m := New(bundle(t), func() []string { return []string{dir} })
			if kind == "relative" {
				m = New(bundle(t), func() []string { return []string{"relative"} })
			} else if kind == "extensions" || kind == "target" {
				path := filepath.Join(dir, "extensions")
				if kind == "target" {
					if err := os.MkdirAll(path, 0700); err != nil {
						t.Fatal(err)
					}
					path = target(dir)
				}
				if err := os.Symlink(outside, path); err != nil {
					t.Fatal(err)
				}
			} else {
				m.Set(true)
				name := kind
				switch kind {
				case "unowned":
					name = ".relay-owned"
				case "wrong marker":
					put(t, filepath.Join(target(dir), ".relay-owned"), "someone else")
					name = ""
				case "directory file":
					name = "index.ts"
				}
				if name != "" {
					if err := os.Remove(filepath.Join(target(dir), name)); err != nil {
						t.Fatal(err)
					}
					if kind == "directory file" {
						if err := os.Mkdir(filepath.Join(target(dir), name), 0700); err != nil {
							t.Fatal(err)
						}
					} else if kind != "unowned" {
						if err := os.Symlink(filepath.Join(outside, "untouched"), filepath.Join(target(dir), name)); err != nil {
							t.Fatal(err)
						}
					}
				}
			}
			for _, action := range []string{"status", "install", "refresh", "remove"} {
				state := m.apply(action)
				if state.Profiles[0].Status != "refused" || state.Profiles[0].Reason == "" {
					t.Fatalf("%s: %+v", action, state)
				}
			}
			bytes, _ := os.ReadFile(filepath.Join(outside, "untouched"))
			if string(bytes) != "original" {
				t.Fatal("modified symlink destination")
			}
		})
	}
}

func TestMultipleProfilesAndPartialFailure(t *testing.T) {
	home := t.TempDir()
	custom := filepath.Join(home, "custom")
	refused := filepath.Join(home, "refused")
	put(t, filepath.Join(refused, "extensions", Directory, "user.ts"), "keep")
	put(t, filepath.Join(home, ".pi/profiles/work/agent/settings.json"), "{}")
	t.Setenv("PI_CODING_AGENT_DIR", custom)
	t.Setenv("HERDR_PI_CONFIG_DIRS", strings.Join([]string{refused, custom}, string(os.PathListSeparator)))
	m := New(bundle(t), func() []string { return agentroots.PiConfigDirs(home) })
	state := m.Set(true)
	if !state.Enabled || len(state.Profiles) != 4 || state.Profiles[0].Status != "refused" {
		t.Fatalf("%+v", state)
	}
	for _, p := range state.Profiles[1:] {
		if !p.Installed {
			t.Fatalf("%+v", p)
		}
	}
	if state = m.Set(false); state.Enabled {
		t.Fatalf("%+v", state)
	}
}

func TestRunInstallsAndRemoves(t *testing.T) {
	dir := t.TempDir()
	var out strings.Builder
	if err := Run(bundle(t), []string{"install", dir}, &out); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), target(dir)) || !New(bundle(t), func() []string { return []string{dir} }).Status().Enabled {
		t.Fatal("not installed", out.String())
	}
	out.Reset()
	if err := Run(bundle(t), []string{"remove", dir}, &out); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(target(dir)); !os.IsNotExist(err) {
		t.Fatal("not removed", err)
	}
}

func TestRunRejectsBadArgumentsAndUnownedDirectories(t *testing.T) {
	dir := t.TempDir()
	put(t, filepath.Join(target(dir), "unrelated"), "keep")
	for _, args := range [][]string{nil, {"install"}, {"status", dir}, {"install", "relative"}} {
		if err := Run(bundle(t), args, io.Discard); !errors.Is(err, ErrUsage) {
			t.Fatal(args, err)
		}
	}
	if err := Run(bundle(t), []string{"install", dir}, io.Discard); err == nil || errors.Is(err, ErrUsage) {
		t.Fatal("overwrote an unowned directory", err)
	}
}
