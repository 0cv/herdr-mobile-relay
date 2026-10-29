//go:build linux || darwin

package localcontrol

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/0cv/herdr-mobile-relay/internal/config"
	"golang.org/x/sys/unix"
)

// Parent execution must supply the existing short, sterile private fixture
// environment. No workers, AfterFuncs, sockets, devices or production writers.
func preflightNamespace(t *testing.T, selected config.InstalledConfig, root string) {
	t.Helper()
	if e := os.Mkdir(root+"/ic", 0700); e != nil {
		t.Fatal(e)
	}
	fd, ids, e := handoffRoot(root)
	if e != nil {
		t.Fatal("fixture root")
	}
	ns, id, e := handoffChild(fd, "ic", true)
	ce := unix.Close(fd)
	if e != nil || ce != nil {
		if ns >= 0 {
			_ = unix.Close(ns)
		}
		t.Fatal("fixture namespace")
	}
	if unix.Close(ns) != nil {
		t.Fatal("fixture close")
	}
	owner := handoffOwner{"installed-handoff-v1", selected.Snapshot().InstanceID, ids[len(ids)-1].key(), id.key()}
	data, e := json.Marshal(owner)
	if e != nil {
		t.Fatal(e)
	}
	handoffPut(t, root+"/ic/owner", data)
}

func preflightExpect(t *testing.T, selected config.InstalledConfig, want InstalledHandoffPresence) {
	t.Helper()
	got, e := InspectInstalledHandoff(context.Background(), selected)
	if want == "" {
		if e == nil || got != (InstalledHandoffObservation{}) {
			t.Fatal("expected zero refusal")
		}
	} else if e != nil || got.Presence() != want {
		t.Fatal("unexpected observation")
	}
}

func TestInstalledHandoffPreflightStates(t *testing.T) {
	for _, name := range []string{"missing-runtime", "missing-nested", "absent-namespace", "attributed", "current", "unknown-namespace", "bad-current", "foreign-current", "credential-unread", "private-leaf"} {
		t.Run(name, func(t *testing.T) {
			selected, c, root := handoffFixture(t)
			want := InstalledHandoffAttributedNamespace
			switch name {
			case "missing-runtime", "missing-nested":
				if e := os.Remove(root); e != nil {
					t.Fatal(e)
				}
				if name == "missing-nested" {
					cfg := selected.Snapshot()
					cfg.RuntimeDir = root + "/nested"
					var e error
					selected, e = config.BindInstalledCLI(&cfg)
					if e != nil {
						t.Fatal(e)
					}
				}
				want = InstalledHandoffMissingRuntimeDir
			case "absent-namespace":
				want = InstalledHandoffAbsentNamespace
			case "private-leaf":
				if e := os.Chmod(root, 0755); e != nil {
					t.Fatal(e)
				}
				want = ""
			default:
				preflightNamespace(t, selected, root)
				switch name {
				case "unknown-namespace":
					handoffPut(t, root+"/ic/owner", []byte(`{}`))
					want = ""
				case "bad-current":
					handoffPut(t, root+"/ic/current", []byte(`{"run":"bad"}`))
					want = ""
				case "current", "foreign-current", "credential-unread":
					installation := c.Installation
					if name == "foreign-current" {
						installation = "foreign"
						want = ""
					}
					data, _ := json.Marshal(handoffLocator{InstalledProtocol, installation, c.Run})
					handoffPut(t, root+"/ic/current", data)
					if name == "credential-unread" {
						// Deliberately unusable generation: inspection must not open it or c.
						handoffPut(t, root+"/ic/g"+c.Run, []byte("not a directory or credential"))
					}
				}
			}
			before := preflightTree(t, filepath.Dir(root))
			preflightExpect(t, selected, want)
			if after := preflightTree(t, filepath.Dir(root)); after != before {
				t.Fatal("inspection mutated fixture")
			}
		})
	}
}

// Test-only snapshot reads regular fixture bytes; production preflight does not.
func preflightTree(t *testing.T, root string) string {
	t.Helper()
	var b strings.Builder
	e := filepath.Walk(root, func(path string, info os.FileInfo, e error) error {
		if e != nil {
			return e
		}
		fmt.Fprintf(&b, "%s:%v:", path, info.Mode())
		if info.Mode().IsRegular() {
			data, e := os.ReadFile(path)
			if e != nil {
				return e
			}
			b.Write(data)
		}
		if info.Mode()&os.ModeSymlink != 0 {
			target, e := os.Readlink(path)
			if e != nil {
				return e
			}
			b.WriteString(target)
		}
		return nil
	})
	if e != nil {
		t.Fatal(e)
	}
	return b.String()
}

func TestInstalledHandoffPreflightBudget(t *testing.T) {
	for _, name := range []string{"absent-fit", "absent-full", "known-fit", "known-full", "replacement-fit", "replacement-full", "scan-limit", "scan-overflow", "interrupted"} {
		t.Run(name, func(t *testing.T) {
			selected, _, root := handoffFixture(t)
			known := strings.HasPrefix(name, "known") || strings.HasPrefix(name, "replacement")
			n := 13
			prefix := root + "/.ic-"
			want := InstalledHandoffAbsentNamespace
			if known {
				preflightNamespace(t, selected, root)
				want = InstalledHandoffAttributedNamespace
				n = 13
				prefix = root + "/ic/unknown-" // owner + 13 = 14 four-slot units
				if strings.HasPrefix(name, "replacement") {
					data, _ := json.Marshal(handoffLocator{InstalledProtocol, selected.Snapshot().InstanceID, strings.Repeat("a", 32)})
					handoffPut(t, root+"/ic/current", data)
					n--
				}
			}
			if strings.HasSuffix(name, "full") {
				n++
				want = ""
			}
			if name == "scan-limit" {
				n = 256
				prefix = root + "/unrelated-"
			}
			if name == "scan-overflow" {
				n = 257
				prefix = root + "/unrelated-"
				want = ""
			}
			if name == "interrupted" {
				n = 14
				want = ""
			}
			for i := 0; i < n; i++ {
				handoffPut(t, fmt.Sprintf("%s%d", prefix, i), []byte("unknown retained"))
			}
			before := preflightTree(t, root)
			preflightExpect(t, selected, want)
			if preflightTree(t, root) != before {
				t.Fatal("inventory changed")
			}
		})
	}
}

func TestInstalledHandoffPreflightFinalBoundary(t *testing.T) {
	for _, name := range []string{"nil", "zero", "cancelled", "read-cancel", "closed-cancel", "read-error", "closed-error", "root-replacement", "owner-replacement", "current-appeared", "closed-namespace", "closed-owner", "closed-current"} {
		t.Run(name, func(t *testing.T) {
			selected, _, root := handoffFixture(t)
			preflightNamespace(t, selected, root)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			var use context.Context = ctx
			switch name {
			case "nil":
				use = nil
			case "zero":
				selected = config.InstalledConfig{}
			case "cancelled":
				cancel()
			}
			got, e := inspectInstalledHandoff(use, selected, func(stage string) error {
				if name == stage+"-cancel" {
					cancel()
				}
				if name == stage+"-error" {
					return errHandoff
				}
				if name == "root-replacement" && stage == "closed" {
					if e := os.Rename(root, root+"-old"); e != nil {
						t.Fatal(e)
					}
					if e := os.Mkdir(root, 0700); e != nil {
						t.Fatal(e)
					}
				}
				if name == "closed-namespace" && stage == "closed" {
					if e := os.Rename(root+"/ic", root+"/old"); e != nil {
						t.Fatal(e)
					}
					preflightNamespace(t, selected, root)
				}
				if name == "closed-owner" && stage == "closed" {
					handoffPut(t, root+"/ic/owner", []byte(`{}`))
				}
				if name == "closed-current" && stage == "closed" {
					handoffPut(t, root+"/ic/current", []byte(`{}`))
				}
				if name == "owner-replacement" && stage == "read" {
					handoffPut(t, root+"/ic/owner", []byte(`{}`))
				}
				if name == "current-appeared" && stage == "read" {
					handoffPut(t, root+"/ic/current", []byte(`{}`))
				}
				return nil
			})
			if e == nil || got != (InstalledHandoffObservation{}) {
				t.Fatal("late failure exposed observation")
			}
		})
	}
}

func TestInstalledHandoffPreflightPaths(t *testing.T) {
	for _, name := range []string{"103", "104", "alias", "alias-expanded-104", "alias-drift", "missing-alias", "leaf-alias"} {
		t.Run(name, func(t *testing.T) {
			selected, _, root := handoffFixture(t)
			cfg := selected.Snapshot()
			parent := filepath.Dir(root)
			want := InstalledHandoffMissingRuntimeDir
			switch name {
			case "103", "104", "alias-expanded-104":
				total := 103
				if name != "103" {
					total = 104
					want = ""
				}
				overhead := len(filepath.Join(parent, "ic", "g"+strings.Repeat("0", 32), "s")) + 1
				cfg.RuntimeDir = parent + "/" + strings.Repeat("r", total-overhead)
				if name == "alias-expanded-104" {
					cfg.RuntimeDir = cfg.RuntimeDir[:len(cfg.RuntimeDir)-2] // /r restores exactly 104 bytes
					if e := os.Mkdir(cfg.RuntimeDir, 0700); e != nil {
						t.Fatal(e)
					}
					// Expand an intermediate alias, rather than a forbidden final symlink.
					target := cfg.RuntimeDir
					if e := os.Mkdir(target+"/r", 0700); e != nil {
						t.Fatal(e)
					}
					if e := os.Symlink(target, parent+"/a"); e != nil {
						t.Fatal(e)
					}
					cfg.RuntimeDir = parent + "/a/r"
				}
			default:
				if e := os.Symlink(parent, parent+"/a"); e != nil {
					t.Fatal(e)
				}
				cfg.RuntimeDir = parent + "/a/r"
				want = InstalledHandoffAbsentNamespace
				if name == "missing-alias" {
					cfg.RuntimeDir += "/missing"
					want = InstalledHandoffMissingRuntimeDir
				}
				if name == "leaf-alias" {
					cfg.RuntimeDir = parent + "/a"
				}
			}
			var e error
			selected, e = config.BindInstalledCLI(&cfg)
			if name == "leaf-alias" {
				if e == nil {
					t.Fatal("binding alias policy changed")
				}
				return
			}
			if e != nil {
				t.Fatal("fixture binding")
			}
			if name == "alias-drift" {
				if e := os.Mkdir(parent+"/other", 0700); e != nil {
					t.Fatal(e)
				}
				if e := os.Remove(parent + "/a"); e != nil {
					t.Fatal(e)
				}
				if e := os.Symlink(parent+"/other", parent+"/a"); e != nil {
					t.Fatal(e)
				}
				want = ""
			}
			preflightExpect(t, selected, want)
		})
	}
}

func TestInstalledHandoffPreflightDataAndPathPolicy(t *testing.T) {
	if (InstalledHandoffObservation{}).Presence() != "" {
		t.Fatal("zero admitted")
	}
	for _, path := range []string{"", "/", "relative", "/a/../b", "/a/\n", "/" + strings.Repeat("x", 256), "/" + strings.Repeat("a/", 33) + "b", string([]byte{'/', 255})} {
		if handoffPath(path) {
			t.Fatal("unbounded path admitted")
		}
	}
	o := InstalledHandoffObservation{InstalledHandoffAttributedNamespace}
	if fmt.Sprintf("%v %+v %#v", o, o, o) != strings.TrimSpace(strings.Repeat("installed handoff observation [redacted] ", 3)) {
		t.Fatal("redaction")
	}
}
