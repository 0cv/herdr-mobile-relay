package doctor

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/0cv/herdr-mobile-relay/internal/darwinacl"
	"golang.org/x/sys/unix"
)

func absentPackageACL() packageACLProbe {
	return packageACLProbe{inspect: func(int) (darwinacl.ACL, error) { return darwinacl.ACL{Absent: true}, nil }, filesystem: func(int) error { return nil }}
}

func packageFDMatches(t *testing.T, fd int, wanted unix.Stat_t) bool {
	t.Helper()
	var st unix.Stat_t
	if unix.Fstat(fd, &st) != nil {
		t.Fatal("probe did not receive a live descriptor")
	}
	return st.Dev == wanted.Dev && st.Ino == wanted.Ino
}

func TestPackageACLAdmissionSeam(t *testing.T) {
	for _, ancestor := range []bool{false, true} {
		for _, kind := range []string{"absent", "deny", "grant", "empty", "unknown", "error", "volume"} {
			calls := []int{}
			p := absentPackageACL()
			p.filesystem = func(fd int) error {
				calls = append(calls, fd)
				if kind == "volume" {
					return errors.New("non-local volume")
				}
				return nil
			}
			p.inspect = func(fd int) (darwinacl.ACL, error) {
				calls = append(calls, fd)
				return packageACLAnswer(kind)
			}
			err := p.objectSafe(731, ancestor)
			want := kind == "absent" || (kind == "deny" && ancestor)
			if (err == nil) != want || len(calls) == 0 || calls[0] != 731 || (kind != "volume" && (len(calls) != 2 || calls[1] != 731)) {
				t.Fatalf("%s ancestor=%t: admission or same-fd evidence lost", kind, ancestor)
			}
		}
	}
	if (packageACLProbe{}).objectSafe(-1, false) == nil {
		t.Fatal("missing probe admitted")
	}
}

func packageACLAnswer(kind string) (darwinacl.ACL, error) {
	switch kind {
	case "absent", "volume":
		return darwinacl.ACL{Absent: true}, nil
	case "deny":
		return darwinacl.ACL{DenyOnly: true, Entries: 1, Denies: 1}, nil
	case "grant":
		return darwinacl.ACL{Entries: 1}, nil
	case "error", "unknown":
		return darwinacl.ACL{}, errors.New("unverifiable or truncated ACL")
	default:
		return darwinacl.ACL{}, nil
	}
}

// Every cell uses the actual acquisition pipeline and real descriptors. Only
// ACL/volume answers are synthetic; the advertised build is deliberately Linux.
func TestDarwinPackageEvidenceMatrix(t *testing.T) {
	for _, reader := range []string{"metadata", "bundle"} {
		for _, object := range []string{"ancestor", "root", "manifest", "nested", "enumeration", "file"} {
			if reader == "metadata" && (object == "nested" || object == "enumeration" || object == "file") {
				continue
			}
			for _, kind := range []string{"absent", "deny", "grant", "empty", "unknown", "error", "volume"} {
				t.Run(reader+"/"+object+"/"+kind, func(t *testing.T) {
					f := newBundleFixture(t, "linux/amd64")
					path := f.root
					switch object {
					case "ancestor":
						path = filepath.Dir(f.root)
					case "manifest":
						path = filepath.Join(f.root, "release-manifest.json")
					case "nested":
						path = filepath.Join(f.root, "relay/pi-command-bridge")
					case "file":
						path = filepath.Join(f.root, "web/index.html")
					}
					wanted := bundleLstat(t, path)
					p := absentPackageACL()
					checks, reads, opens := 0, 0, 0
					volumeFD, aclFD, rootFD, volumeChecks := -1, -1, -1, 0
					p.filesystem = func(fd int) error {
						volumeFD = fd
						if kind == "volume" && packageFDMatches(t, fd, wanted) {
							volumeChecks++
							if object == "enumeration" && volumeChecks != 3 {
								return nil
							}
							if object == "enumeration" && fd == rootFD {
								t.Fatal("volume not checked on enumeration fd")
							}
							checks++
							return errors.New("non-local volume")
						}
						return nil
					}
					p.inspect = func(fd int) (darwinacl.ACL, error) {
						aclFD = fd
						if fd != volumeFD {
							t.Fatal("ACL not bound to volume descriptor")
						}
						if !packageFDMatches(t, fd, wanted) {
							return packageACLAnswer("absent")
						}
						checks++
						if object == "enumeration" {
							if checks == 1 {
								rootFD = fd
							}
							if checks == 3 && fd == rootFD {
								t.Fatal("enumeration was not checked on its own fd")
							}
						}
						// Root check 1 is root acquisition, 2 is inventory, 3 is
						// the separately opened fd actually used for ReadDir.
						if object == "enumeration" && checks != 3 {
							return packageACLAnswer("absent")
						}
						return packageACLAnswer(kind)
					}
					want := kind == "absent" || (kind == "deny" && object == "ancestor")
					if reader == "metadata" {
						got := readPackageIdentityWithACL(f.root, f.build.Version, f.build.Revision, f.build.Target, "darwin", openParent,
							func(fd int, name string) (int, error) { opens++; return openSelectedFile(fd, name) },
							func(r io.Reader) ([]byte, error) {
								if aclFD != int(r.(*os.File).Fd()) {
									t.Fatal("metadata read fd was not inspected")
								}
								reads++
								return io.ReadAll(r)
							}, p)
						if want && got != f.build {
							t.Fatal("proved metadata refused")
						}
						if !want {
							assertMetadataUnavailable(t, got)
						}
						if !want && object != "manifest" && opens != 0 {
							t.Fatal("unproved ancestry opened manifest")
						}
					} else {
						q, target := f.query()
						q.goos, q.acl = "darwin", &p
						q.hooks = &bundleHooks{event: func(stage, _ string, file *os.File) {
							if stage == "read" {
								if aclFD != int(file.Fd()) {
									t.Fatal("bundle read fd was not inspected")
								}
								reads++
							}
						}}
						state := Unknown
						if want {
							state = OK
						}
						assertBundleState(t, q.Bundle(context.Background(), target, f.build), state)
					}
					if checks == 0 || (!want && reads != 0) || (want && reads == 0) {
						t.Fatal("missing evidence or content read after preflight refusal")
					}
				})
			}
		}
	}
}

func TestDarwinBundleACLFlipsAndReplacements(t *testing.T) {
	for _, stage := range []string{"preflight", "open", "read", "final", "ancestor", "replace", "ctime", "read-replace"} {
		t.Run(stage, func(t *testing.T) {
			f, holder := newBundleAncestorFixture(t)
			q, target := f.query()
			p := absentPackageACL()
			q.goos, q.acl = "darwin", &p
			changed, refused, reads, afterRefusal, fileReads := false, false, 0, 0, 0
			wanted := bundleLstat(t, filepath.Join(f.root, "LICENSE"))
			p.inspect = func(fd int) (darwinacl.ACL, error) {
				if changed && stage != "ancestor" && stage != "replace" && stage != "ctime" && stage != "read-replace" && packageFDMatches(t, fd, wanted) {
					refused = true
					return packageACLAnswer("grant")
				}
				return packageACLAnswer("absent")
			}
			q.hooks = &bundleHooks{event: func(event, name string, _ *os.File) {
				if event == "read" {
					reads++
					if name == "LICENSE" {
						fileReads++
					}
					if refused {
						afterRefusal++
					}
				}
				if changed {
					return
				}
				if stage == "ancestor" && event == "component" && name == filepath.Base(holder) {
					changed = true
					if err := os.Rename(holder, holder+".old"); err != nil {
						t.Fatal(err)
					}
					if err := os.Mkdir(holder, 0o755); err != nil {
						t.Fatal(err)
					}
					if err := os.Rename(filepath.Join(holder+".old", filepath.Base(f.root)), f.root); err != nil {
						t.Fatal(err)
					}
				} else if (event == stage && (name == "LICENSE" || stage == "final")) || (stage == "replace" && event == "preflight" && name == "LICENSE") ||
					((stage == "ctime" || stage == "read-replace") && event == "read" && name == "LICENSE") {
					changed = true
					if stage == "ctime" {
						path := filepath.Join(f.root, "LICENSE")
						if err := os.Chmod(path, 0o600); err != nil {
							t.Fatal(err)
						}
						if err := os.Chmod(path, 0o644); err != nil {
							t.Fatal(err)
						}
					}
					if stage == "replace" || stage == "read-replace" {
						path := filepath.Join(f.root, "LICENSE")
						if err := os.Remove(path); err != nil {
							t.Fatal(err)
						}
						bundleWrite(t, f.root, "LICENSE", []byte("replacement"), 0o644)
					}
				}
			}}
			assertBundleState(t, q.Bundle(context.Background(), target, f.build), Unknown)
			if !changed || afterRefusal != 0 || (stage == "open" && fileReads != 0) ||
				((stage == "preflight" || stage == "ancestor" || stage == "replace") && reads != 0) {
				t.Fatal("flip/replacement lost, or content followed preflight refusal")
			}
		})
	}
}
