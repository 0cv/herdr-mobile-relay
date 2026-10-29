//go:build linux || darwin

package installedpromotion

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/sys/unix"
)

func TestSelectionMetadataPolicy(t *testing.T) {
	base := unix.Stat_t{Mode: unix.S_IFREG | 0600, Uid: uint32(os.Geteuid()), Nlink: 1, Size: 12}
	for _, tc := range []struct {
		name   string
		change func(*unix.Stat_t)
	}{
		{"fifo", func(s *unix.Stat_t) { s.Mode = unix.S_IFIFO | 0600 }},
		{"device", func(s *unix.Stat_t) { s.Mode = unix.S_IFCHR | 0600 }},
		{"socket", func(s *unix.Stat_t) { s.Mode = unix.S_IFSOCK | 0600 }},
		{"foreign", func(s *unix.Stat_t) { s.Uid++ }},
		{"hardlink", func(s *unix.Stat_t) { s.Nlink++ }},
		{"setid", func(s *unix.Stat_t) { s.Mode |= 04000 }},
		{"writers", func(s *unix.Stat_t) { s.Mode |= 0020 }},
		{"oversize", func(s *unix.Stat_t) { s.Size = maxBytes + 1 }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := base
			tc.change(&s)
			if selectionMode(s, false, false) {
				t.Fatal("unsafe pre-open metadata accepted")
			}
		})
	}
	if !selectionMode(base, false, false) {
		t.Fatal("regular file refused")
	}
}
func TestSelectionCleanupReplacement(t *testing.T) {
	c, s := fixture(t)
	gen := strings.Repeat("d", 32)
	temp := filepath.Join(c.ConfigHome, namespace, "t"+name(c.InstanceID, Reference{Generation: gen}))
	r, e := prepare(context.Background(), s, c.ConfigHome, c.InstanceID, gen, faults{point: func(p string) error {
		if p != "rename" {
			return nil
		}
		if e := os.Rename(temp, temp+"-captured"); e != nil {
			t.Fatal(e)
		}
		if e := os.WriteFile(temp, []byte("bystander"), 0600); e != nil {
			t.Fatal(e)
		}
		return ErrRefused
	}})
	if e == nil || r.Outcome != Uncertain {
		t.Fatal("cleanup uncertainty lost")
	}
	bytes, e := os.ReadFile(temp)
	if e != nil || string(bytes) != "bystander" {
		t.Fatal("replacement removed")
	}
}
func TestSelectionInitializationFaults(t *testing.T) {
	for _, point := range []string{"namespace-create", "owner-sync", "namespace-sync", "namespace-rename", "namespace-parent-sync"} {
		t.Run(point, func(t *testing.T) {
			c, s := fixture(t)
			r, e := prepare(context.Background(), s, c.ConfigHome, c.InstanceID, strings.Repeat("e", 32), faults{point: func(p string) error {
				if p == point {
					return ErrRefused
				}
				return nil
			}})
			if e == nil {
				t.Fatal("initialization fault ignored")
			}
			want := Unpublished
			if point == "namespace-parent-sync" {
				want = Uncertain
			}
			if r.Outcome != want {
				t.Fatal("initialization uncertainty erased")
			}
			if want == Unpublished {
				entries, e := os.ReadDir(c.ConfigHome)
				if e != nil || len(entries) != 0 {
					t.Fatal("unpublished namespace not cleaned")
				}
			}
			if want == Uncertain {
				if _, e := os.Lstat(filepath.Join(c.ConfigHome, namespace)); e != nil {
					t.Fatal("initialization evidence removed")
				}
			}
		})
	}
}
func TestSelectionInterruptedQuota(t *testing.T) {
	c, s := fixture(t)
	for i := 0; i < artifactLimit/2; i++ {
		p := filepath.Join(c.ConfigHome, namespace+"-"+strings.Repeat("x", i+1))
		if e := os.Mkdir(p, 0700); e != nil {
			t.Fatal(e)
		}
	}
	if _, e := Prepare(context.Background(), s, c.ConfigHome, c.InstanceID); e == nil {
		t.Fatal("interrupted artifact quota ignored")
	}
	entries, e := os.ReadDir(c.ConfigHome)
	if e != nil || len(entries) != artifactLimit/2 {
		t.Fatal("unknown artifacts reclaimed")
	}
}
func TestSelectionLockCancellation(t *testing.T) {
	c, s := fixture(t)
	save(t, c, s)
	holder, e := openStore(context.Background(), c.ConfigHome, false)
	if e != nil {
		t.Fatal(e)
	}
	defer holder.close() // unconditional release before fixture/global restoration
	fd, e := unix.Open(filepath.Join(c.ConfigHome, namespace), selectionFlags|unix.O_DIRECTORY, 0)
	if e != nil {
		t.Fatal(e)
	}
	defer unix.Close(fd)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	cancel()
	if lock(ctx, fd) == nil {
		t.Fatal("cancelled waiter admitted")
	}
}
