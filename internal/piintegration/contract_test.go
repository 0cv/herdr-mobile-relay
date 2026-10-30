package piintegration

import (
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"
)

func TestShellRefusalContract(t *testing.T) {
	for _, kind := range []string{"extensions", "target", "index.ts", "bridge.mjs", "activity.mjs", ".relay-owned", "unowned", "wrong marker", "directory file"} {
		t.Run(kind, func(t *testing.T) {
			shell, _ := filepath.Abs("../../relay/pi-commands.sh")
			for _, implementation := range []string{"go", "shell"} {
				dir, outside := t.TempDir(), t.TempDir()
				put(t, filepath.Join(outside, "untouched"), "keep")
				m := New(bundle(t), func() []string { return []string{dir} })
				m.Set(true)
				path := filepath.Join(target(dir), kind)
				switch kind {
				case "extensions":
					path = filepath.Join(dir, "extensions")
				case "target":
					path = target(dir)
				case "unowned", "wrong marker":
					path = filepath.Join(target(dir), ".relay-owned")
				case "directory file":
					path = filepath.Join(target(dir), "index.ts")
				}
				if err := os.RemoveAll(path); err != nil {
					t.Fatal(err)
				}
				switch kind {
				case "unowned":
				case "wrong marker":
					put(t, path, "someone else")
				case "directory file":
					if err := os.Mkdir(path, 0700); err != nil {
						t.Fatal(err)
					}
				default:
					destination := filepath.Join(outside, "untouched")
					if kind == "extensions" || kind == "target" {
						destination = outside
					}
					if err := os.Symlink(destination, path); err != nil {
						t.Fatal(err)
					}
				}
				for _, action := range []string{"install", "remove"} {
					if implementation == "go" {
						if m.apply(action).Profiles[0].Status != "refused" {
							t.Fatal("Go accepted", kind, action)
						}
					} else if out, err := exec.Command("/bin/bash", "--noprofile", "--norc", shell, action, dir).CombinedOutput(); err == nil {
						t.Fatalf("shell accepted %s %s: %s", kind, action, out)
					}
				}
				bytes, _ := os.ReadFile(filepath.Join(outside, "untouched"))
				if string(bytes) != "keep" {
					t.Fatal("changed symlink destination")
				}
			}
		})
	}
}

func TestSpecialFilesAreRefusedWithoutBlocking(t *testing.T) {
	for _, implementation := range []string{"go", "shell"} {
		for _, name := range Files {
			t.Run(implementation+"/"+name, func(t *testing.T) {
				dir := t.TempDir()
				m := New(bundle(t), func() []string { return []string{dir} })
				m.Set(true)
				file := filepath.Join(target(dir), name)
				if err := os.Remove(file); err != nil {
					t.Fatal(err)
				}
				if err := syscall.Mkfifo(file, 0600); err != nil {
					t.Fatal(err)
				}
				for _, action := range []string{"install", "remove"} {
					if implementation == "go" {
						if m.apply(action).Profiles[0].Status != "refused" {
							t.Fatal("accepted special file", action)
						}
						continue
					}
					shell, _ := filepath.Abs("../../relay/pi-commands.sh")
					if out, err := exec.Command("/bin/bash", "--noprofile", "--norc", shell, action, dir).CombinedOutput(); err == nil {
						t.Fatalf("shell accepted special file: %s", out)
					}
				}
				info, err := os.Lstat(file)
				if err != nil || info.Mode()&os.ModeNamedPipe == 0 {
					t.Fatal("special file changed", err)
				}
			})
		}
	}
}

func TestRefreshDoesNotAdoptNewProfiles(t *testing.T) {
	first, second := t.TempDir(), t.TempDir()
	dirs := []string{first}
	m := New(bundle(t), func() []string { return dirs })
	m.Set(true)
	dirs = append(dirs, second)
	state := m.Refresh()
	if !state.Enabled || !state.Profiles[0].Installed || state.Profiles[1].Installed {
		t.Fatalf("%+v", state)
	}
	if _, err := os.Lstat(target(second)); !os.IsNotExist(err) {
		t.Fatal("startup adopted a new profile", err)
	}
	if !m.Set(true).Profiles[1].Installed {
		t.Fatal("explicit enable did not install the new profile")
	}
}

func TestMissingBundleDoesNotCreateOrHideInstallation(t *testing.T) {
	dir := t.TempDir()
	missing := New(os.DirFS(filepath.Join(dir, "missing-bundle")), func() []string { return []string{dir} })
	if p := missing.Set(true).Profiles[0]; p.Status != "refused" || p.Installed {
		t.Fatalf("%+v", p)
	}
	if _, err := os.Lstat(target(dir)); !os.IsNotExist(err) {
		t.Fatal("created an unowned directory without a bundle")
	}
	installed := New(bundle(t), func() []string { return []string{dir} })
	installed.Set(true)
	if !missing.Status().Enabled {
		t.Fatal("missing bundle hid marker-owned disk state")
	}
	if missing.Instance() == installed.Instance() || missing.Status().Instance != missing.Instance() {
		t.Fatal("relay instance identity is not exact")
	}
	if missing.Set(false).Enabled {
		t.Fatal("missing bundle prevented removal")
	}
	if missing.Set(false).Enabled {
		t.Fatal("repeated remove is not idempotent")
	}
}
