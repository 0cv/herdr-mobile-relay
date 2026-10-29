//go:build linux || darwin

package installedpromotion

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/0cv/herdr-mobile-relay/internal/config"
	"golang.org/x/sys/unix"
)

func TestSelectionFreshPublicationFaults(t *testing.T) {
	for _, point := range []string{"admission-lock", "admission-validation", "admission-inventory", "create", "write", "file-sync", "file-close", "rename", "directory-sync", "return", "cancel-admission", "ambiguous-namespace-rename", "capsule-rename-exists"} {
		t.Run(point, func(t *testing.T) {
			c, s := fixture(t)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			f := faults{point: func(p string) error {
				if point == "cancel-admission" && p == "admission-lock" {
					cancel()
				}
				if p == point {
					return ErrRefused
				}
				return nil
			}}
			if point == "capsule-rename-exists" {
				f.rename = func(int, string, string) error { return unix.EEXIST }
			}
			if point == "ambiguous-namespace-rename" {
				f.namespaceRename = func(fd int, a, b string) error {
					if e := selectionExclusive(fd, a, b); e != nil {
						return e
					}
					return unix.EIO
				}
			}
			r, e := prepare(ctx, s, c.ConfigHome, c.InstanceID, strings.Repeat("1", 32), f)
			if !errors.Is(e, ErrUncertain) || r.Outcome != Uncertain || !hexID(r.Reference.Digest, 64) {
				t.Fatal("publication knowledge lost")
			}
			entries, e := os.ReadDir(filepath.Join(c.ConfigHome, namespace))
			want := 1
			if point == "directory-sync" || point == "return" {
				want = 2
			}
			if e != nil || len(entries) != want {
				t.Fatal("namespace evidence/capsule cleanup mismatch")
			}
			if want == 2 {
				if _, e := Read(context.Background(), c.ConfigHome, c.InstanceID, r.Reference); e != nil {
					t.Fatal("retained capsule unreadable")
				}
			}
		})
	}
}

func TestSelectionReuseParentFence(t *testing.T) {
	for _, kind := range []string{"ordered", "sync-failure", "cancel-after-sync", "attribution-drift", "read-only"} {
		t.Run(kind, func(t *testing.T) {
			c, s := fixture(t)
			first, e := prepare(context.Background(), s, c.ConfigHome, c.InstanceID, strings.Repeat("2", 32), faults{syncParent: func(int) error { return unix.EIO }})
			if !errors.Is(e, ErrUncertain) || first.Outcome != Uncertain {
				t.Fatal("interruption not retained")
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			synced, created := false, false
			f := faults{syncParent: func(fd int) error {
				if kind == "sync-failure" {
					return unix.EIO
				}
				if e := unix.Fsync(fd); e != nil {
					return e
				}
				synced = true
				return nil
			}, point: func(p string) error {
				if p == "reuse-parent-sync-complete" {
					if kind == "cancel-after-sync" {
						cancel()
					}
					if kind == "attribution-drift" {
						return os.WriteFile(filepath.Join(c.ConfigHome, namespace, "owner"), []byte("changed"), 0600)
					}
				}
				if p == "create" {
					created = true
					if !synced {
						return ErrRefused
					}
				}
				return nil
			}}
			if kind == "read-only" {
				// Read observes retained bytes without repairing even a missing capsule.
				before, e := os.ReadDir(filepath.Join(c.ConfigHome, namespace))
				if e != nil {
					t.Fatal(e)
				}
				got, e := Read(ctx, c.ConfigHome, c.InstanceID, first.Reference)
				after, ae := os.ReadDir(filepath.Join(c.ConfigHome, namespace))
				if e == nil || ae != nil || !reflect.DeepEqual(got, config.InstalledConfig{}) || len(before) != len(after) {
					t.Fatal("read repaired interrupted initialization")
				}
				return
			}
			r, e := prepare(ctx, s, c.ConfigHome, c.InstanceID, strings.Repeat("3", 32), f)
			if kind == "ordered" {
				if e != nil || r.Outcome != Durable || !synced || !created {
					t.Fatal("parent fence did not precede capsule")
				}
				return
			}
			if !errors.Is(e, ErrUncertain) || r.Outcome != Uncertain || created {
				t.Fatal("failed parent fence admitted capsule")
			}
			entries, e := os.ReadDir(filepath.Join(c.ConfigHome, namespace))
			if e != nil || len(entries) != 1 {
				t.Fatal("fence failure changed inventory")
			}
		})
	}
}

func TestSelectionReadFinalBoundary(t *testing.T) {
	for _, kind := range []string{"alias-drift", "cancel", "refusal"} {
		t.Run(kind, func(t *testing.T) {
			c, _ := fixture(t)
			root := filepath.Dir(c.ConfigHome)
			a, b, alias := filepath.Join(root, "a"), filepath.Join(root, "b"), filepath.Join(root, "alias")
			for _, p := range []string{a, b, filepath.Join(a, "parent"), filepath.Join(b, "parent")} {
				if e := os.Mkdir(p, 0700); e != nil {
					t.Fatal(e)
				}
			}
			if e := os.Symlink(a, alias); e != nil {
				t.Fatal(e)
			}
			c.CacheDir = filepath.Join(alias, "parent", "cache")
			s, e := config.BindInstalledCLI(&c)
			if e != nil {
				t.Fatal(e)
			}
			r := save(t, c, s)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			reached := false
			got, e := read(ctx, c.ConfigHome, c.InstanceID, r.Reference, faults{point: func(p string) error {
				if p != "read-final-binding" {
					return nil
				}
				reached = true
				switch kind {
				case "alias-drift":
					if e := os.Remove(alias); e != nil {
						t.Fatal(e)
					}
					if e := os.Symlink(b, alias); e != nil {
						t.Fatal(e)
					}
				case "cancel":
					cancel()
				case "refusal":
					return ErrRefused
				}
				return nil
			}})
			if !reached || e == nil || !reflect.DeepEqual(got, config.InstalledConfig{}) {
				t.Fatal("late change released selection")
			}
		})
	}
}

func TestSelectionQueuedCancellation(t *testing.T) {
	for _, kind := range []string{"prepare-root", "prepare-namespace", "read-root", "read-namespace"} {
		t.Run(kind, func(t *testing.T) {
			c, s := fixture(t)
			r := save(t, c, s)
			before, e := os.ReadDir(filepath.Join(c.ConfigHome, namespace))
			if e != nil {
				t.Fatal(e)
			}
			path := c.ConfigHome
			if strings.HasSuffix(kind, "namespace") {
				path = filepath.Join(path, namespace)
			}
			fd, e := unix.Open(path, selectionFlags|unix.O_DIRECTORY, 0)
			if e != nil {
				t.Fatal(e)
			}
			if e = unix.Flock(fd, unix.LOCK_EX|unix.LOCK_NB); e != nil {
				unix.Close(fd)
				t.Fatal(e)
			}
			ctx, cancel := context.WithCancel(context.Background())
			entered, done := make(chan struct{}), make(chan struct{})
			var once sync.Once
			ctx = context.WithValue(ctx, lockObserverKey{}, func(int) { once.Do(func() { close(entered) }) })
			var err error
			var result Result
			var got config.InstalledConfig
			// Every failure path cancels, releases the holder, THEN joins the waiter.
			// Joining does not depend on entry observation or the cancellation assertion.
			defer func() { cancel(); unix.Close(fd); <-done }()
			go func() {
				defer close(done)
				if strings.HasPrefix(kind, "prepare") {
					result, err = Prepare(ctx, s, c.ConfigHome, c.InstanceID)
				} else {
					got, err = Read(ctx, c.ConfigHome, c.InstanceID, r.Reference)
				}
			}()
			timer := time.NewTimer(5 * time.Second)
			defer timer.Stop()
			select {
			case <-entered:
			case <-timer.C:
				t.Fatal("real contended entry not observed")
			}
			cancel()
			select {
			case <-done:
			case <-timer.C:
				t.Fatal("cancellation did not complete while holder held")
			}
			if err == nil || !reflect.DeepEqual(got, config.InstalledConfig{}) {
				t.Fatal("cancelled waiter admitted")
			}
			if strings.HasPrefix(kind, "prepare") && result.Outcome != Unpublished {
				t.Fatal("queue acquired publication effect")
			}
			after, e := os.ReadDir(filepath.Join(c.ConfigHome, namespace))
			if e != nil || !reflect.DeepEqual(before, after) {
				t.Fatal("queued operation mutated namespace")
			}
		})
	}
}
