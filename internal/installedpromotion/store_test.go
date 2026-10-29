//go:build linux || darwin

package installedpromotion

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/0cv/herdr-mobile-relay/internal/config"
	"golang.org/x/sys/unix"
)

func fixture(t *testing.T) (config.Config, config.InstalledConfig) {
	t.Helper()
	root, e := os.MkdirTemp("/tmp", "sp-")
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() { os.RemoveAll(root) }) // fixture-owned only, never production cleanup
	root, e = filepath.EvalSymlinks(root)
	if e != nil {
		t.Fatal(e)
	}
	t.Chdir(root)
	for _, key := range []string{"HOME", "XDG_CONFIG_HOME", "XDG_CACHE_HOME", "XDG_DATA_HOME", "XDG_RUNTIME_DIR", "TMPDIR", "PATH", "HERDR_SOCKET_PATH", "HERDR_BIN", "HERDR_RELAY_ENV", "HERDR_PLUGIN_CONFIG_DIR"} {
		t.Setenv(key, filepath.Join(root, "absent-"+key))
	}
	c := config.Config{Host: "127.0.0.1", Port: 8375, PluginPort: 8376, Token: "01234567890123456789012345678901", InstanceID: "..", Transport: config.TransportTailscaleCLI, TailscaleCLIScope: "production", TailscaleCLIBin: filepath.Join(root, "absent-cli"), TailscaleCLIOrigin: "https://relay.example.invalid", PhoneAppOrigin: "https://app.example.invalid", ConfigHome: filepath.Join(root, "config"), RuntimeDir: filepath.Join(root, "config", "herdr-mobile-relay"), CacheDir: filepath.Join(root, "cache"), ReleaseRoot: filepath.Join(root, "release"), TailscaleCLIStateRoot: filepath.Join(root, "registration"), TailscaleCLICoordinationRoot: filepath.Join(root, "coordination"), AllowedOrigins: []string{"https://app.example.invalid"}, GatewayURLs: []string{}, SocketPath: filepath.Join(root, "absent-herdr.sock"), HerdrBin: filepath.Join(root, "absent-herdr"), ServiceName: "private-service", PollInterval: 2.25, WebRoot: filepath.Join(root, "absent-web"), DataHome: filepath.Join(root, "data")}
	if e = os.Mkdir(c.ConfigHome, 0700); e != nil {
		t.Fatal(e)
	}
	selected, e := config.BindInstalledCLI(&c)
	if e != nil {
		t.Fatal("binding refused")
	}
	return c, selected
}
func save(t *testing.T, c config.Config, s config.InstalledConfig) Result {
	t.Helper()
	r, e := Prepare(context.Background(), s, c.ConfigHome, c.InstanceID)
	if e != nil || r.Outcome != Durable {
		t.Fatal("private filesystem preparation refused (supported filesystem required)")
	}
	return r
}
func TestSelectionRoundTrip(t *testing.T) {
	c, s := fixture(t)
	r := save(t, c, s)
	t.Setenv("HERDR_RELAY_TOKEN", "ambient-never-selected")
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(filepath.Dir(c.ConfigHome), "other"))
	got, e := Read(context.Background(), c.ConfigHome, c.InstanceID, r.Reference)
	if e != nil || !reflect.DeepEqual(got.Snapshot(), c) {
		t.Fatal("full snapshot not reproduced")
	}
	copy := got.Snapshot()
	copy.AllowedOrigins[0] = "changed"
	if !reflect.DeepEqual(got.Snapshot(), c) {
		t.Fatal("snapshot alias")
	}
	for _, p := range []string{c.RuntimeDir, c.CacheDir, c.ReleaseRoot, c.TailscaleCLIStateRoot, c.TailscaleCLICoordinationRoot} {
		if _, e := os.Lstat(p); !os.IsNotExist(e) {
			t.Fatal("application root created")
		}
	}
	for _, tc := range []struct {
		name, anchor, id string
		ref              Reference
	}{
		{"anchor", filepath.Dir(c.ConfigHome), c.InstanceID, r.Reference},
		{"installation", c.ConfigHome, ".", r.Reference},
		{"digest", c.ConfigHome, c.InstanceID, Reference{r.Reference.Generation, strings.Repeat("0", 64)}},
		{"generation", c.ConfigHome, c.InstanceID, Reference{strings.Repeat("0", 32), r.Reference.Digest}},
		{"path-reference", c.ConfigHome, c.InstanceID, Reference{"../escape", r.Reference.Digest}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, e := Read(context.Background(), tc.anchor, tc.id, tc.ref)
			if e == nil || !reflect.DeepEqual(got, config.InstalledConfig{}) {
				t.Fatal("redirect accepted")
			}
		})
	}
	t.Run("exclusive", func(t *testing.T) {
		again, e := prepare(context.Background(), s, c.ConfigHome, c.InstanceID, r.Reference.Generation, faults{})
		if e == nil || again.Outcome != Unpublished {
			t.Fatal("generation overwritten")
		}
		if _, e := Read(context.Background(), c.ConfigHome, c.InstanceID, r.Reference); e != nil {
			t.Fatal("old generation changed")
		}
	})
	t.Run("multiple-installations", func(t *testing.T) {
		other := c
		other.InstanceID = "."
		bound, e := config.BindInstalledCLI(&other)
		if e != nil {
			t.Fatal(e)
		}
		r2 := save(t, other, bound)
		if _, e := Read(context.Background(), c.ConfigHome, c.InstanceID, r2.Reference); e == nil {
			t.Fatal("other namespace adopted")
		}
	})
}
func TestSelectionFaults(t *testing.T) {
	for _, point := range []string{"create", "write", "file-sync", "file-close", "rename", "directory-sync", "return"} {
		t.Run(point, func(t *testing.T) {
			c, s := fixture(t)
			prior := save(t, c, s) // explicitly acknowledged namespace and capsule
			r, e := prepare(context.Background(), s, c.ConfigHome, c.InstanceID, strings.Repeat("a", 32), faults{point: func(p string) error {
				if p == point {
					return ErrRefused
				}
				return nil
			}})
			if e == nil {
				t.Fatal("fault ignored")
			}
			want := Unpublished
			if point == "directory-sync" || point == "return" {
				want = Uncertain
			}
			if r.Outcome != want {
				t.Fatal("wrong effect classification")
			}
			entries, e := os.ReadDir(filepath.Join(c.ConfigHome, namespace))
			if e != nil {
				t.Fatal(e)
			}
			if _, e := Read(context.Background(), c.ConfigHome, c.InstanceID, prior.Reference); e != nil {
				t.Fatal("acknowledged capsule changed")
			}
			if want == Unpublished && len(entries) != 2 {
				t.Fatal("owned unpublished artifact leaked")
			}
			if want == Uncertain && len(entries) != 3 {
				t.Fatal("uncertain evidence removed")
			}
		})
	}
	t.Run("ambiguous-rename", func(t *testing.T) {
		c, s := fixture(t)
		r, e := prepare(context.Background(), s, c.ConfigHome, c.InstanceID, strings.Repeat("b", 32), faults{rename: func(fd int, from, to string) error {
			if e := selectionExclusive(fd, from, to); e != nil {
				return e
			}
			return unix.EIO
		}})
		if !errors.Is(e, ErrUncertain) || r.Outcome != Uncertain {
			t.Fatal("ambiguity erased")
		}
		if _, e := Read(context.Background(), c.ConfigHome, c.InstanceID, r.Reference); e != nil {
			t.Fatal("retained reference unusable")
		}
	})
	t.Run("cancel-before", func(t *testing.T) {
		c, s := fixture(t)
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		if _, e := Prepare(ctx, s, c.ConfigHome, c.InstanceID); e == nil {
			t.Fatal("cancellation ignored")
		}
		if _, e := os.Lstat(filepath.Join(c.ConfigHome, namespace)); !os.IsNotExist(e) {
			t.Fatal("cancelled creation")
		}
	})
	t.Run("cancel-after-rename", func(t *testing.T) {
		c, s := fixture(t)
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		r, e := prepare(ctx, s, c.ConfigHome, c.InstanceID, strings.Repeat("c", 32), faults{point: func(p string) error {
			if p == "directory-sync" {
				cancel()
			}
			return nil
		}})
		if e == nil || r.Outcome != Uncertain {
			t.Fatal("cancellation erased effect")
		}
	})
	t.Run("quota", func(t *testing.T) {
		c, s := fixture(t)
		save(t, c, s)
		for i := 0; i < artifactLimit-2; i++ {
			if e := os.WriteFile(filepath.Join(c.ConfigHome, namespace, fmt.Sprintf("unknown-%d", i)), nil, 0600); e != nil {
				t.Fatal(e)
			}
		}
		if _, e := Prepare(context.Background(), s, c.ConfigHome, c.InstanceID); e == nil {
			t.Fatal("quota ignored")
		}
	})
}
func TestSelectionFilesystemRefusal(t *testing.T) {
	for _, kind := range []string{"symlink", "hardlink", "mode", "replacement", "oversize"} {
		t.Run(kind, func(t *testing.T) {
			c, s := fixture(t)
			r := save(t, c, s)
			p := filepath.Join(c.ConfigHome, namespace, name(c.InstanceID, r.Reference))
			switch kind {
			case "symlink":
				if e := os.Rename(p, p+"-old"); e != nil {
					t.Fatal(e)
				}
				if e := os.Symlink(p+"-old", p); e != nil {
					t.Fatal(e)
				}
			case "hardlink":
				if e := os.Link(p, p+"-link"); e != nil {
					t.Fatal(e)
				}
			case "mode":
				if e := os.Chmod(p, 0644); e != nil {
					t.Fatal(e)
				}
			case "replacement":
				if e := os.WriteFile(p, []byte("replacement"), 0600); e != nil {
					t.Fatal(e)
				}
			case "oversize":
				if e := os.WriteFile(p, []byte(strings.Repeat("x", maxBytes+1)), 0600); e != nil {
					t.Fatal(e)
				}
			}
			if got, e := Read(context.Background(), c.ConfigHome, c.InstanceID, r.Reference); e == nil || !reflect.DeepEqual(got, config.InstalledConfig{}) {
				t.Fatal("unsafe payload released")
			}
		})
	}
	t.Run("unknown-namespace", func(t *testing.T) {
		c, s := fixture(t)
		if e := os.Mkdir(filepath.Join(c.ConfigHome, namespace), 0700); e != nil {
			t.Fatal(e)
		}
		if _, e := Prepare(context.Background(), s, c.ConfigHome, c.InstanceID); e == nil {
			t.Fatal("unknown namespace adopted")
		}
	})
	t.Run("overlap", func(t *testing.T) {
		c, _ := fixture(t)
		c.RuntimeDir = filepath.Join(c.ConfigHome, namespace)
		s, e := config.BindInstalledCLI(&c)
		if e != nil {
			t.Fatal(e)
		}
		if _, e := Prepare(context.Background(), s, c.ConfigHome, c.InstanceID); e == nil {
			t.Fatal("runtime overlap accepted")
		}
	})
	t.Run("missing-anchor", func(t *testing.T) {
		c, _ := fixture(t)
		if e := os.Remove(c.ConfigHome); e != nil {
			t.Fatal(e)
		}
		s, e := config.BindInstalledCLI(&c)
		if e != nil {
			t.Fatal(e)
		}
		if _, e := Prepare(context.Background(), s, c.ConfigHome, c.InstanceID); e == nil {
			t.Fatal("missing anchor created")
		}
		if _, e := os.Lstat(c.ConfigHome); !os.IsNotExist(e) {
			t.Fatal("anchor created")
		}
	})
	t.Run("canonical-drift", func(t *testing.T) {
		c, _ := fixture(t)
		root := filepath.Dir(c.ConfigHome)
		a, b := filepath.Join(root, "a"), filepath.Join(root, "b")
		for _, p := range []string{a, b, filepath.Join(a, "parent"), filepath.Join(b, "parent")} {
			if e := os.Mkdir(p, 0700); e != nil {
				t.Fatal(e)
			}
		}
		alias := filepath.Join(root, "alias")
		if e := os.Symlink(a, alias); e != nil {
			t.Fatal(e)
		}
		c.CacheDir = filepath.Join(alias, "parent", "cache")
		s, e := config.BindInstalledCLI(&c)
		if e != nil {
			t.Fatal(e)
		}
		r := save(t, c, s)
		if e := os.Remove(alias); e != nil {
			t.Fatal(e)
		}
		if e := os.Symlink(b, alias); e != nil {
			t.Fatal(e)
		}
		if _, e := config.BindInstalledCLI(&c); e != nil {
			t.Fatal("fresh binding must demonstrate alias rebinding")
		}
		if _, e := Read(context.Background(), c.ConfigHome, c.InstanceID, r.Reference); e == nil {
			t.Fatal("original pin lost")
		}
	})
}
