//go:build linux || darwin

package localcontrol

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"golang.org/x/sys/unix"
)

func TestInstalledHandoffPreflightMetadata(t *testing.T) {
	for _, name := range []string{"namespace-mode", "namespace-symlink", "namespace-file", "owner-mode", "owner-hardlink", "owner-symlink", "owner-directory", "owner-oversize", "current-mode", "current-directory", "ancestor-mode"} {
		t.Run(name, func(t *testing.T) {
			selected, _, root := handoffFixture(t)
			preflightNamespace(t, selected, root)
			owner := root + "/ic/owner"
			switch name {
			case "namespace-mode":
				if e := os.Chmod(root+"/ic", 0755); e != nil {
					t.Fatal(e)
				}
			case "namespace-symlink", "namespace-file":
				if e := os.Rename(root+"/ic", root+"/old"); e != nil {
					t.Fatal(e)
				}
				if name == "namespace-symlink" {
					if e := os.Symlink("old", root+"/ic"); e != nil {
						t.Fatal(e)
					}
				} else {
					handoffPut(t, root+"/ic", nil)
				}
			case "owner-mode":
				if e := os.Chmod(owner, 0640); e != nil {
					t.Fatal(e)
				}
			case "owner-hardlink":
				if e := os.Link(owner, root+"/copy"); e != nil {
					t.Fatal(e)
				}
			case "owner-symlink", "owner-directory":
				if e := os.Rename(owner, owner+"-old"); e != nil {
					t.Fatal(e)
				}
				if name == "owner-symlink" {
					if e := os.Symlink("owner-old", owner); e != nil {
						t.Fatal(e)
					}
				} else {
					if e := os.Mkdir(owner, 0700); e != nil {
						t.Fatal(e)
					}
				}
			case "owner-oversize":
				handoffPut(t, owner, make([]byte, handoffBytes+1))
			case "current-mode":
				handoffPut(t, root+"/ic/current", nil)
				if e := os.Chmod(root+"/ic/current", 0640); e != nil {
					t.Fatal(e)
				}
			case "current-directory":
				if e := os.Mkdir(root+"/ic/current", 0700); e != nil {
					t.Fatal(e)
				}
			case "ancestor-mode":
				if e := os.Chmod(filepath.Dir(root), 0777); e != nil {
					t.Fatal(e)
				}
			}
			before := preflightTree(t, filepath.Dir(root))
			preflightExpect(t, selected, "")
			if preflightTree(t, filepath.Dir(root)) != before {
				t.Fatal("unsafe metadata repaired")
			}
		})
	}
}

func TestInstalledHandoffPreflightWalk(t *testing.T) {
	for _, name := range []string{"missing-appeared", "ancestor-replaced", "close-error", "private-root-preserved", "ancestor-not-private"} {
		t.Run(name, func(t *testing.T) {
			_, _, root := handoffFixture(t)
			path := root
			if name == "missing-appeared" || name == "ancestor-not-private" {
				path += "/missing"
			}
			if name == "ancestor-not-private" {
				if e := os.Chmod(root, 0755); e != nil {
					t.Fatal(e)
				}
			}
			if name == "private-root-preserved" {
				if e := os.Chmod(root, 0755); e != nil {
					t.Fatal(e)
				}
				w, e := preflightOpen(root)
				if e == nil {
					_ = w.close()
					t.Fatal("private root weakened")
				}
				return
			}
			w, e := preflightOpen(path)
			if e != nil {
				t.Fatal("walk")
			}
			defer func() {
				if e := w.close(); e != nil {
					t.Error("walk close")
				}
			}()
			switch name {
			case "missing-appeared":
				if e := os.Mkdir(path, 0700); e != nil {
					t.Fatal(e)
				}
			case "ancestor-replaced":
				if e := os.Rename(root, root+"-old"); e != nil {
					t.Fatal(e)
				}
				if e := os.Mkdir(root, 0700); e != nil {
					t.Fatal(e)
				}
			case "close-error":
				// Transfer ownership out of the close set BEFORE the one real close.
				// -1 deterministically models EBADF without re-closing a released
				// number that another thread could reuse. This is not a native fault.
				last := len(w.fds) - 1
				fd := w.fds[last]
				fds := append([]int(nil), w.fds[:last]...)
				w.fds[last] = -1
				if unix.Close(fd) != nil {
					t.Error("fault setup close") // Never retry an ownership-ambiguous close.
				}
				if w.close() == nil {
					t.Error("lost close failure")
				}
				for _, fd := range fds {
					var st unix.Stat_t
					if unix.Fstat(fd, &st) != unix.EBADF {
						t.Error("descriptor leaked or number reused")
					}
				}
				if len(w.fds) != 0 {
					t.Error("close retained descriptors")
				}
				return
			case "ancestor-not-private":
				if !w.missing || w.validate() != nil {
					t.Fatal("ancestor incorrectly private")
				}
				return
			}
			if w.validate() == nil {
				t.Fatal("drift accepted")
			}
		})
	}
}

func TestInstalledHandoffPreflightLateAlias(t *testing.T) {
	selected, _, root := handoffFixture(t)
	preflightNamespace(t, selected, root)
	cfg := selected.Snapshot()
	// Change a different original binding root after all preflight reads/closes.
	got, e := inspectInstalledHandoff(context.Background(), selected, func(stage string) error {
		if stage != "closed" {
			return nil
		}
		if e := os.Symlink(root, cfg.CacheDir); e != nil {
			t.Fatal(e)
		}
		return nil
	})
	if e == nil || got != (InstalledHandoffObservation{}) {
		t.Fatal("final binding drift accepted")
	}
}

func TestInstalledHandoffPreflightSyntheticSpecial(t *testing.T) {
	// Never instantiate or open a device, FIFO or socket, even on regression.
	for _, mode := range []uint32{unix.S_IFCHR, unix.S_IFBLK, unix.S_IFIFO, unix.S_IFSOCK, unix.S_IFLNK} {
		var st unix.Stat_t
		handoffTestMode(&st, mode|0600)
		st.Uid = uint32(os.Geteuid())
		handoffTestLinks(&st, 1)
		if handoffMode(st, false, false) || handoffMode(st, true, true) {
			t.Fatal("special metadata admitted")
		}
	}
}
