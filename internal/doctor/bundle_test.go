package doctor

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/0cv/herdr-mobile-relay/internal/release"
	"golang.org/x/sys/unix"
)

// Actual filesystem verifier fixtures. Build is used only to AUTHOR a synthetic
// public package, never as a verifier substitute. Nothing is executed here by
// the implementation worker; no file is an executable program or credential.
type bundleFixture struct {
	root       string
	build      BuildIdentity
	descriptor release.WebDescriptor
}

func bundleHash(data []byte) string { sum := sha256.Sum256(data); return hex.EncodeToString(sum[:]) }
func bundleSRI(hash string) string {
	decoded, _ := hex.DecodeString(hash)
	return "sha256-" + base64.StdEncoding.EncodeToString(decoded)
}
func bundleWrite(t *testing.T, root, name string, data []byte, mode os.FileMode) {
	t.Helper()
	filename := filepath.Join(root, filepath.FromSlash(name))
	if err := os.MkdirAll(filepath.Dir(filename), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filename, data, mode); err != nil {
		t.Fatal(err)
	}
}
func newBundleFixture(t *testing.T, target string) *bundleFixture {
	t.Helper()
	temporary, err := os.MkdirTemp("", "bd-") // short enough for a disposable Unix socket on Darwin
	if err != nil {
		t.Fatal(err)
	}
	root, err := filepath.EvalSymlinks(temporary)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(root) })
	f := &bundleFixture{root: root, build: BuildIdentity{"0.22.4", strings.Repeat("a", 40), "", target}}
	for _, name := range bundleRequiredFiles() {
		bundleWrite(t, root, name, []byte("synthetic public package fixture\n"), 0o644)
		web := strings.TrimPrefix(name, "web/")
		if strings.HasPrefix(name, "web/") && ((!strings.Contains(web, "/") && !strings.HasPrefix(web, "_")) || web == "icons/icon.svg") {
			bundleWrite(t, root, name+".br", []byte("synthetic compressed public file\n"), 0o644)
		}
	}
	if err := os.Chmod(filepath.Join(root, "herdr-mobile-relay"), 0o755); err != nil {
		t.Fatal(err)
	}
	js, css := []byte("/* synthetic JS, never executed */\n"), []byte("/* synthetic style */\n")
	jsHash, cssHash := bundleHash(js), bundleHash(css)
	build := strings.Repeat("b", 64)
	entryName := "builds/0.22.4-1-" + build[:16] + "/index.html"
	jsName, cssName := "assets/app-"+jsHash+".js", "assets/app-"+cssHash+".css"
	entry := []byte(`<link href="/` + cssName + `" integrity="` + bundleSRI(cssHash) + `"><script src="/` + jsName + `" integrity="` + bundleSRI(jsHash) + `"></script>`)
	f.descriptor = release.WebDescriptor{Schema: 1, Version: f.build.Version, Assets: 1, Build: build, Entry: "/" + entryName,
		Files: map[string]release.WebDescriptorFile{
			"entry":      {Path: entryName, SHA256: bundleHash(entry), Integrity: bundleSRI(bundleHash(entry))},
			"javascript": {Path: jsName, SHA256: jsHash, Integrity: bundleSRI(jsHash)},
			"stylesheet": {Path: cssName, SHA256: cssHash, Integrity: bundleSRI(cssHash)},
		}}
	for name, data := range map[string][]byte{entryName: entry, jsName: js, cssName: css} {
		bundleWrite(t, root, "web/"+name, data, 0o644)
		// Digest-only synthetic Brotli content; consistency does not decompress,
		// execute or qualify the browser. Every such byte must still be hashed.
		bundleWrite(t, root, "web/"+name+".br", []byte("synthetic compressed bytes "+name), 0o644)
	}
	for key, file := range f.descriptor.Files {
		br := []byte("synthetic compressed bytes " + file.Path)
		file.BrotliSHA256, file.BrotliIntegrity = bundleHash(br), bundleSRI(bundleHash(br))
		f.descriptor.Files[key] = file
	}
	for _, name := range []string{"ConversationHistory-1.js", "ResumeTimingSettings-1.js", "workspace-tools-1.js", "attachment-hash.worker-D-WkX_nj.js"} {
		bundleWrite(t, root, "web/assets/"+name, []byte("synthetic lazy/worker bytes\n"), 0o644)
		bundleWrite(t, root, "web/assets/"+name+".br", []byte("synthetic lazy compressed bytes\n"), 0o644)
	}
	// The current builder copies empty historical build directories too.
	if err := os.MkdirAll(filepath.Join(root, "web/builds/0.22.4-0-"+strings.Repeat("c", 16)), 0o755); err != nil {
		t.Fatal(err)
	}
	f.writeDescriptor(t)
	f.rebuild(t)
	return f
}
func (f *bundleFixture) writeDescriptor(t *testing.T) {
	t.Helper()
	data, err := json.Marshal(f.descriptor)
	if err != nil {
		t.Fatal(err)
	}
	bundleWrite(t, f.root, "web/release.json", data, 0o644)
}
func (f *bundleFixture) rebuild(t *testing.T) {
	t.Helper()
	manifest, err := release.Build(f.root, f.build.Version, f.build.Revision, f.build.Target)
	if err != nil {
		t.Fatal(err)
	}
	f.build.WebHash = manifest.WebHash
}
func (f *bundleFixture) query() (*PackageQueries, LocalTarget) {
	cfg := ParseConfig(strings.NewReader("HERDR_RELAY_TRANSPORT=cloudflare\n")) // no local instance: never network
	q := NewPackageQueries(f.root, cfg, f.build)
	if runtime.GOOS == "darwin" {
		probe := absentPackageACL() // portable pipeline tests, not native qualification
		q.acl = &probe
	}
	return q, selectedLocalTarget(cfg, f.build)
}
func (f *bundleFixture) observe() Datum {
	q, target := f.query()
	return q.Bundle(context.Background(), target, f.build)
}

// Target strings exercise metadata/layout shapes, not a host ACL attestation.
// These fixtures inject absent ACL + allowed-volume evidence on Darwin.
func bundleExpectedState(state State) State {
	if !bundleWriterSafetySupported(runtime.GOOS) {
		return Unknown
	}
	return state
}
func assertBundleState(t *testing.T, d Datum, state State) {
	t.Helper()
	if d.State != state || d.Source != SourceQuery || d.At.IsZero() {
		t.Fatalf("unexpected finite package observation: %s/%s", d.State, d.Evidence)
	}
	want := EvidenceUnavailable
	if state == OK {
		want = EvidenceHealthy
	}
	if state == Error {
		want = EvidenceFailed
	}
	if state != Unknown && d.Evidence != want {
		t.Fatal("package outcome evidence diverged")
	}
}
func assertBundleMetadataProjection(t *testing.T, f *bundleFixture) {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(f.root, "release-manifest.json"))
	if err != nil {
		t.Fatal(err)
	}
	projection, ok := projectPackageManifest(data, f.build.Version, f.build.Revision, f.build.Target)
	if !ok || projection.build != f.build {
		t.Fatal("platform-independent strict manifest projection changed")
	}
	want := f.build // plain metadata must prove ACL/volume safety on the actual host
	if got := ReadPackageIdentity(f.root, f.build.Version, f.build.Revision, f.build.Target); got != want {
		t.Fatal("public package metadata acquisition diverged from actual-host policy")
	}
}
func bundleInventoryFingerprint(t *testing.T, root string) string {
	t.Helper()
	var out strings.Builder
	err := filepath.WalkDir(root, func(name string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		fmt.Fprintf(&out, "%s %v %d %d\n", name, info.Mode(), info.Size(), info.ModTime().UnixNano())
		if info.Mode().IsRegular() {
			data, err := os.ReadFile(name)
			if err != nil {
				return err
			}
			fmt.Fprintln(&out, bundleHash(data))
		}
		if info.Mode()&os.ModeSymlink != 0 {
			link, err := os.Readlink(name)
			if err != nil {
				return err
			}
			fmt.Fprintln(&out, link)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return out.String()
}
func TestInstalledBundleCompleteBytesTargetsModesAndNonmutation(t *testing.T) {
	for _, target := range []string{"linux/amd64", "linux/arm64", "darwin/amd64", "darwin/arm64"} {
		for _, sealed := range []bool{false, true} {
			t.Run(fmt.Sprint(target, sealed), func(t *testing.T) {
				f := newBundleFixture(t, target)
				if sealed {
					err := filepath.WalkDir(f.root, func(name string, entry os.DirEntry, err error) error {
						if err != nil {
							return err
						}
						info, err := entry.Info()
						if err != nil {
							return err
						}
						return os.Chmod(name, info.Mode().Perm()&^0o222)
					})
					if err != nil {
						t.Fatal(err)
					}
					// Only this disposable test tree is restored for t.TempDir cleanup.
					t.Cleanup(func() {
						_ = filepath.WalkDir(f.root, func(name string, e os.DirEntry, err error) error {
							if err == nil && e.IsDir() {
								return os.Chmod(name, 0o755)
							}
							return err
						})
					})
				}
				before := bundleInventoryFingerprint(t, f.root)
				q, selected := f.query()
				opened := []*os.File{}
				events := []string{}
				q.hooks = &bundleHooks{event: func(stage, _ string, file *os.File) {
					events = append(events, stage)
					if stage == "open" {
						opened = append(opened, file)
					}
				}}
				if runtime.GOOS == "darwin" {
					q.hooks.filesystemType = func(int) (uint64, error) {
						t.Fatal("actual Darwin refusal reached filesystem classification")
						return 0, nil
					}
				}
				datum := q.Bundle(context.Background(), selected, f.build)
				assertBundleState(t, datum, bundleExpectedState(OK))
				if !bundleWriterSafetySupported(runtime.GOOS) && (datum.Evidence != EvidenceUnavailable || len(events) != 0) {
					t.Fatal("public unsupported-host refusal crossed bundle acquisition hooks")
				}
				manifest, err := release.Verify(f.root, target)
				if err != nil {
					t.Fatal("synthetic package diverged from the existing release contract")
				}
				wantOpens := len(manifest.Files) + 2
				if !bundleWriterSafetySupported(runtime.GOOS) {
					wantOpens = 0
				}
				if len(opened) != wantOpens {
					t.Fatal("complete content acquisition or fail-closed no-read policy diverged")
				}
				for _, file := range opened {
					if _, err := file.Stat(); err == nil {
						t.Fatal("content fd retained after observation")
					}
				}
				if bundleInventoryFingerprint(t, f.root) != before {
					t.Fatal("bundle observation mutated bytes/modes/inventory")
				}
				cfg := ParseConfig(strings.NewReader("HERDR_RELAY_TRANSPORT=cloudflare\n"))
				r := Run(context.Background(), cfg, f.build, Options{}, Dependencies{Host: q, Transport: q})
				if findCheck(t, r, CheckBundle).State != bundleExpectedState(OK) || r.ExitCode() != 3 {
					t.Fatal("package consistency claimed complete host/runtime health")
				}
				assertRenderPrivacy(t, r)
				for _, rendered := range []string{fmt.Sprintf("%v", q), fmt.Sprintf("%#v", q)} {
					if strings.Contains(rendered, f.root) {
						t.Fatal("private query root escaped formatting")
					}
				}
				raw, refusal := q.MarshalJSON()
				if len(raw) != 0 || refusal == nil || refusal.Error() != "diagnostic queries are not serializable" {
					t.Fatal("private package queries serializable")
				}
				encoded, err := json.Marshal(q)
				var wrapped *json.MarshalerError
				// encoding/json wraps MarshalJSON's refusal. Require that exact
				// refusal from this type, with no serialized bytes, not just any error.
				if len(encoded) != 0 || !errors.As(err, &wrapped) || wrapped.Type != reflect.TypeOf(q) || wrapped.Err == nil || wrapped.Err.Error() != refusal.Error() {
					t.Fatal("private package queries serialization refusal lost")
				}
			})
		}
	}
}
func TestInstalledBundleStableContradictionsAllPublicContent(t *testing.T) {
	for _, kind := range []string{"binary", "nonexecutable", "wrapper", "index", "javascript", "stylesheet", "brotli", "lazy", "font", "bridge", "license", "unlisted public", "descriptor reference", "descriptor SRI", "descriptor version", "descriptor build", "descriptor entry", "descriptor br", "descriptor script path"} {
		t.Run(kind, func(t *testing.T) {
			f := newBundleFixture(t, "linux/amd64")
			name := ""
			switch kind {
			case "binary":
				name = "herdr-mobile-relay"
			case "nonexecutable":
				if err := os.Chmod(filepath.Join(f.root, "herdr-mobile-relay"), 0o644); err != nil {
					t.Fatal(err)
				}
			case "wrapper":
				name = "relay/plugin-status.sh"
			case "index":
				name = "web/index.html"
			case "javascript":
				name = "web/" + f.descriptor.Files["javascript"].Path
			case "stylesheet":
				name = "web/" + f.descriptor.Files["stylesheet"].Path
			case "brotli":
				name = "web/" + f.descriptor.Files["entry"].Path + ".br"
			case "lazy":
				name = "web/assets/ConversationHistory-1.js.br"
			case "font":
				name = "web/fonts/nerd-symbols-mono-v3.4.0.woff2"
			case "bridge":
				name = "relay/pi-command-bridge/index.ts"
			case "license":
				name = "LICENSE"
			case "unlisted public":
				bundleWrite(t, f.root, "web/assets/ConversationHistory-2.js", []byte("unexpected approved public content"), 0o644)
			case "descriptor reference":
				file := f.descriptor.Files["entry"]
				data := []byte("<html>no exact JS/style references</html>")
				file.SHA256, file.Integrity = bundleHash(data), bundleSRI(bundleHash(data))
				f.descriptor.Files["entry"] = file
				bundleWrite(t, f.root, "web/"+file.Path, data, 0o644)
			case "descriptor SRI":
				file := f.descriptor.Files["javascript"]
				file.Integrity = "sha256-invalid"
				f.descriptor.Files["javascript"] = file
			case "descriptor version":
				f.descriptor.Version = "0.99.9"
			case "descriptor build":
				f.descriptor.Build = "private-error-canary"
			case "descriptor entry":
				f.descriptor.Entry = "https://node-canary.invalid/entry"
			case "descriptor br":
				file := f.descriptor.Files["javascript"]
				file.BrotliSHA256 = strings.Repeat("d", 64)
				file.BrotliIntegrity = bundleSRI(file.BrotliSHA256)
				f.descriptor.Files["javascript"] = file
			case "descriptor script path":
				file := f.descriptor.Files["javascript"]
				file.Path = "herdr-bootstrap.js"
				f.descriptor.Files["javascript"] = file
			}
			if name != "" {
				file, err := os.OpenFile(filepath.Join(f.root, name), os.O_WRONLY|os.O_APPEND, 0)
				if err != nil {
					t.Fatal(err)
				}
				if _, err := file.WriteString("tampered bytes"); err != nil {
					t.Fatal(err)
				}
				if err := file.Close(); err != nil {
					t.Fatal(err)
				}
			}
			if strings.HasPrefix(kind, "descriptor ") {
				f.writeDescriptor(t)
				f.rebuild(t)
			}
			// Self-consistent manifest metadata is still available despite bad
			// actual bytes or semantics. It cannot qualify the bundle itself.
			assertBundleMetadataProjection(t, f)
			before := bundleInventoryFingerprint(t, f.root)
			q, selected := f.query()
			events := []string{}
			q.hooks = &bundleHooks{event: func(stage, _ string, _ *os.File) { events = append(events, stage) }}
			if runtime.GOOS == "darwin" {
				q.hooks.filesystemType = func(int) (uint64, error) {
					t.Fatal("actual Darwin refusal reached filesystem classification")
					return 0, nil
				}
			}
			datum := q.Bundle(context.Background(), selected, f.build)
			assertBundleState(t, datum, bundleExpectedState(Error))
			if !bundleWriterSafetySupported(runtime.GOOS) && (datum.Evidence != EvidenceUnavailable || len(events) != 0) {
				t.Fatal("public unsupported-host refusal crossed bundle acquisition hooks")
			}
			if before != bundleInventoryFingerprint(t, f.root) {
				t.Fatal("failed consistency check repaired package")
			}
		})
	}
}

// F001: missing execute bits may be reported as failure only after the whole
// readable package snapshot has been acquired and revalidated. No binary runs.
func TestInstalledBundleNonexecutableRequiresCompleteStableSnapshot(t *testing.T) {
	for _, kind := range []string{"ordinary", "readonly", "missing content", "unsafe content", "cancel final", "changed mode", "replaced binary"} {
		t.Run(kind, func(t *testing.T) {
			f := newBundleFixture(t, "linux/amd64")
			binary := filepath.Join(f.root, "herdr-mobile-relay")
			mode := os.FileMode(0o644)
			if kind == "readonly" {
				mode = 0o444
			}
			if err := os.Chmod(binary, mode); err != nil {
				t.Fatal(err)
			}
			want := bundleExpectedState(Error)
			if kind == "missing content" {
				if err := os.Remove(filepath.Join(f.root, "LICENSE")); err != nil {
					t.Fatal(err)
				}
				want = Unknown
			}
			if kind == "unsafe content" {
				if err := os.Chmod(filepath.Join(f.root, "LICENSE"), 0o666); err != nil {
					t.Fatal(err)
				}
				want = Unknown
			}
			if kind == "cancel final" || kind == "changed mode" || kind == "replaced binary" {
				want = Unknown
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			assertBundleMetadataProjection(t, f)
			q, target := f.query()
			opened := []*os.File{}
			events := []string{}
			final := false
			q.hooks = &bundleHooks{event: func(stage, _ string, file *os.File) {
				events = append(events, stage)
				if stage == "open" {
					opened = append(opened, file)
				}
				if stage != "final" {
					return
				}
				final = true
				switch kind {
				case "cancel final":
					cancel()
				case "changed mode":
					if err := os.Chmod(binary, 0o755); err != nil {
						t.Fatal(err)
					}
				case "replaced binary":
					data, err := os.ReadFile(binary)
					if err != nil {
						t.Fatal(err)
					}
					if err := os.Remove(binary); err != nil {
						t.Fatal(err)
					}
					bundleWrite(t, f.root, "herdr-mobile-relay", data, mode)
				}
			}}
			if runtime.GOOS == "darwin" {
				q.hooks.filesystemType = func(int) (uint64, error) {
					t.Fatal("actual Darwin refusal reached filesystem classification")
					return 0, nil
				}
			}
			before := bundleInventoryFingerprint(t, f.root)
			datum := q.Bundle(ctx, target, f.build)
			assertBundleState(t, datum, want)
			for _, file := range opened {
				if _, err := file.Stat(); err == nil {
					t.Fatal("nonexecutable observation retained a content fd")
				}
			}
			if !bundleWriterSafetySupported(runtime.GOOS) {
				if datum.Evidence != EvidenceUnavailable {
					t.Fatal("Darwin refusal evidence was not unavailable")
				}
				info, err := os.Stat(binary)
				if err != nil || info.Mode().Perm()&0o111 != 0 || final || len(events) != 0 || len(opened) != 0 {
					t.Fatal("Darwin nonexecutable fixture was not refused before bundle acquisition")
				}
				if before != bundleInventoryFingerprint(t, f.root) {
					t.Fatal("Darwin bundle refusal mutated a nonexecutable fixture")
				}
			}
			if want == Error {
				manifest, err := release.Load(f.root)
				if err != nil {
					t.Fatal(err)
				}
				if !final || len(opened) != len(manifest.Files)+2 {
					t.Fatal("shape failure bypassed complete acquisition/revalidation")
				}
				if before != bundleInventoryFingerprint(t, f.root) {
					t.Fatal("shape observation repaired or mutated the package")
				}
				if ReadPackageIdentity(f.root, f.build.Version, f.build.Revision, f.build.Target) != f.build {
					t.Fatal("shape failure erased metadata availability")
				}
				if _, err := release.Verify(f.root, f.build.Target); err == nil {
					t.Fatal("release contract unexpectedly accepts a nonexecutable binary")
				}
				cfg := ParseConfig(strings.NewReader("HERDR_RELAY_TRANSPORT=cloudflare\n"))
				r := Run(context.Background(), cfg, f.build, Options{}, Dependencies{Host: q, Transport: q})
				if findCheck(t, r, CheckBundle).State != Error || r.ExitCode() != 1 {
					t.Fatal("observed shape failure downgraded to incomplete")
				}
				assertRenderPrivacy(t, r)
			}
		})
	}
}
func TestInstalledBundleUnavailableUnsafeAndNoPrivateContentOpen(t *testing.T) {
	for _, kind := range []string{"missing listed", "missing required", "missing descriptor", "file symlink", "parent symlink", "root symlink", "hardlink", "fifo", "socket", "directory", "writable file", "writable directory", "writable root", "no read", "unknown directory", "private config", "private token", "private auth", "private journal", "private research", "private web"} {
		t.Run(kind, func(t *testing.T) {
			f := newBundleFixture(t, "linux/amd64")
			root, filename := f.root, filepath.Join(f.root, "LICENSE")
			switch kind {
			case "missing listed", "missing required", "missing descriptor":
				if kind == "missing descriptor" {
					filename = filepath.Join(f.root, "web/release.json")
				}
				if err := os.Remove(filename); err != nil {
					t.Fatal(err)
				}
				if kind == "missing required" {
					f.rebuild(t)
				}
			case "file symlink":
				if err := os.Remove(filename); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink("README.md", filename); err != nil {
					t.Fatal(err)
				}
			case "parent symlink":
				if err := os.Rename(filepath.Join(root, "web/assets"), filepath.Join(root, "assets-original")); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink("../assets-original", filepath.Join(root, "web/assets")); err != nil {
					t.Fatal(err)
				}
			case "root symlink":
				root += "-link"
				if err := os.Symlink(f.root, root); err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { _ = os.Remove(root) })
			case "hardlink":
				if err := os.Link(filename, filepath.Join(root, "README-alias")); err != nil {
					t.Fatal(err)
				}
			case "fifo", "socket", "directory":
				if err := os.Remove(filename); err != nil {
					t.Fatal(err)
				}
				if kind == "fifo" {
					if err := unix.Mkfifo(filename, 0o600); err != nil {
						t.Fatal(err)
					}
				}
				if kind == "directory" {
					if err := os.Mkdir(filename, 0o700); err != nil {
						t.Fatal(err)
					}
				}
				if kind == "socket" {
					fd, err := unix.Socket(unix.AF_UNIX, unix.SOCK_STREAM, 0)
					if err != nil {
						t.Fatal(err)
					}
					defer unix.Close(fd)
					if err := unix.Bind(fd, &unix.SockaddrUnix{Name: filename}); err != nil {
						t.Fatal(err)
					}
				}
			case "writable file":
				if err := os.Chmod(filename, 0o666); err != nil {
					t.Fatal(err)
				}
			case "writable directory":
				if err := os.Chmod(filepath.Join(root, "web"), 0o777); err != nil {
					t.Fatal(err)
				}
			case "writable root":
				if err := os.Chmod(root, 0o777); err != nil {
					t.Fatal(err)
				}
			case "no read":
				if err := os.Chmod(filename, 0o200); err != nil {
					t.Fatal(err)
				}
			case "unknown directory":
				if err := os.Mkdir(filepath.Join(root, "research"), 0o700); err != nil {
					t.Fatal(err)
				}
			default:
				private := map[string]string{"private config": "relay/.env", "private token": "relay/token", "private auth": "device-auth.json", "private journal": "journal.json", "private research": "research/notes.txt", "private web": "web/assets/auth.json"}[kind]
				bundleWrite(t, root, private, []byte("secret-canary /private/path-canary node-canary\x1b"), 0o600)
				f.rebuild(t) // malicious but syntactically self-consistent manifest
			}
			q, selected := f.query()
			q.root = root
			opened := []string{}
			q.hooks = &bundleHooks{event: func(stage, name string, _ *os.File) {
				if stage == "open" {
					opened = append(opened, name)
				}
			}}
			assertBundleState(t, q.Bundle(context.Background(), selected, f.build), Unknown)
			for _, name := range opened {
				if name != "release-manifest.json" && !bundleContentPath(name) {
					t.Fatal("private/unapproved content was opened")
				}
			}
			if strings.HasPrefix(kind, "private") && len(opened) != 0 {
				t.Fatal("private inventory content opened before policy refusal")
			}
		})
	}
	// No privileged chown/mknod is needed to test these actual policy branches.
	for _, kind := range []string{"character", "block", "setuid", "setgid"} {
		st := unix.Stat_t{Mode: unix.S_IFREG | 0o644, Uid: uint32(os.Geteuid()), Nlink: 1, Size: 1}
		switch kind {
		case "character":
			st.Mode = unix.S_IFCHR | 0o644
		case "block":
			st.Mode = unix.S_IFBLK | 0o644
		case "setuid":
			st.Mode |= 0o4000
		case "setgid":
			st.Mode |= 0o2000
		}
		if bundleRegular(&st, "LICENSE") {
			t.Fatal("special/device-shaped metadata admitted")
		}
	}
	foreign := unix.Stat_t{Mode: unix.S_IFREG | 0o644, Uid: uint32(os.Geteuid() + 1), Nlink: 1, Size: 1}
	if bundleRegular(&foreign, "LICENSE") {
		t.Fatal("foreign owner admitted")
	}
}
func TestInstalledBundleBindingCancellationAndChangingEvidence(t *testing.T) {
	for _, kind := range []string{"before", "expired", "inventory cancel", "read cancel", "final cancel", "read failure", "grow", "truncate", "file replacement", "directory replacement", "manifest replacement", "root replacement", "final chmod", "late unlisted", "changed then restored"} {
		t.Run(kind, func(t *testing.T) {
			f := newBundleFixture(t, "linux/amd64")
			q, selected := f.query()
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			if kind == "before" {
				cancel()
			}
			if kind == "expired" {
				var stop context.CancelFunc
				ctx, stop = context.WithDeadline(ctx, time.Now().Add(-time.Second))
				defer stop()
			}
			changed := false
			opened := []*os.File{}
			q.hooks = &bundleHooks{event: func(stage, name string, file *os.File) {
				if stage == "open" {
					opened = append(opened, file)
				}
				if changed {
					return
				}
				if (kind == "inventory cancel" && stage == "inventory") || (kind == "read cancel" && stage == "read") || (kind == "final cancel" && stage == "final") {
					changed = true
					cancel()
					return
				}
				if stage == "read" && name == "README.md" && (kind == "read failure" || kind == "grow" || kind == "truncate") {
					changed = true
					if kind == "read failure" {
						_ = file.Close()
						return
					}
					data := []byte("changed")
					if kind == "grow" {
						data = []byte(strings.Repeat("grow", 30000))
					}
					bundleWrite(t, f.root, name, data, 0o644)
					return
				}
				if stage != "final" {
					return
				}
				changed = true
				switch kind {
				case "file replacement", "manifest replacement":
					name := "LICENSE"
					if kind == "manifest replacement" {
						name = "release-manifest.json"
					}
					data, err := os.ReadFile(filepath.Join(f.root, name))
					if err != nil {
						t.Fatal(err)
					}
					if err := os.Rename(filepath.Join(f.root, name), filepath.Join(f.root, name+"-old")); err != nil {
						t.Fatal(err)
					}
					bundleWrite(t, f.root, name, data, 0o644)
				case "directory replacement":
					if err := os.Rename(filepath.Join(f.root, "web/assets"), filepath.Join(f.root, "old-assets")); err != nil {
						t.Fatal(err)
					}
					if err := os.Mkdir(filepath.Join(f.root, "web/assets"), 0o755); err != nil {
						t.Fatal(err)
					}
				case "root replacement":
					if err := os.Rename(f.root, f.root+"-old"); err != nil {
						t.Fatal(err)
					}
					t.Cleanup(func() { _ = os.RemoveAll(f.root + "-old") })
					if err := os.Mkdir(f.root, 0o755); err != nil {
						t.Fatal(err)
					}
				case "final chmod":
					if err := os.Chmod(filepath.Join(f.root, "LICENSE"), 0o600); err != nil {
						t.Fatal(err)
					}
				case "late unlisted":
					bundleWrite(t, f.root, "web/assets/ConversationHistory-2.js", []byte("new public file"), 0o644)
				case "changed then restored":
					name := filepath.Join(f.root, "LICENSE")
					info, err := os.Stat(name)
					if err != nil {
						t.Fatal(err)
					}
					data, err := os.ReadFile(name)
					if err != nil {
						t.Fatal(err)
					}
					if err := os.WriteFile(name, []byte(strings.Repeat("x", len(data))), 0o644); err != nil {
						t.Fatal(err)
					}
					if err := os.WriteFile(name, data, 0o644); err != nil {
						t.Fatal(err)
					}
					if err := os.Chtimes(name, info.ModTime(), info.ModTime()); err != nil {
						t.Fatal(err)
					}
				}
			}}
			assertBundleState(t, q.Bundle(ctx, selected, f.build), Unknown)
			for _, file := range opened {
				if _, err := file.Stat(); err == nil {
					t.Fatal("fd retained on refusal/cancellation")
				}
			}
			if (kind == "before" || kind == "expired") && len(opened) != 0 {
				t.Fatal("expired query acquired content")
			}
		})
	}
	f := newBundleFixture(t, "linux/amd64")
	q, target := f.query()
	q.hooks = &bundleHooks{event: func(string, string, *os.File) { t.Fatal("stale binding performed I/O") }}
	for _, kind := range []string{"version", "revision", "web", "target", "selection"} {
		build, selected := f.build, target
		switch kind {
		case "version":
			build.Version = "0.99.9"
		case "revision":
			build.Revision = strings.Repeat("d", 40)
		case "web":
			build.WebHash = strings.Repeat("d", 64)
		case "target":
			build.Target = "darwin/arm64"
		case "selection":
			selected.pluginPort++
		}
		assertBundleState(t, q.Bundle(context.Background(), selected, build), Unknown)
	}
	// Reacquisition compares the WHOLE build, not just compiled version/revision.
	q.hooks = nil
	bundleWrite(t, f.root, "web/index.html", []byte("new build bytes"), 0o644)
	f.rebuild(t)
	assertBundleState(t, q.Bundle(context.Background(), target, q.build), Unknown)
}
func TestInstalledBundleCapsAndStrictProjection(t *testing.T) {
	for _, kind := range []string{"file bytes", "binary bytes", "total bytes", "directory count", "file count", "inventory count", "name bytes", "duplicate", "suffix", "UTF8", "descriptor duplicate", "descriptor type", "descriptor traversal", "descriptor missing file"} {
		t.Run(kind, func(t *testing.T) {
			f := newBundleFixture(t, "linux/amd64")
			switch kind {
			case "file bytes":
				if err := os.Truncate(filepath.Join(f.root, "LICENSE"), maxBundleFileBytes+1); err != nil {
					t.Fatal(err)
				}
			case "binary bytes":
				if err := os.Truncate(filepath.Join(f.root, "herdr-mobile-relay"), maxBundleBinaryBytes+1); err != nil {
					t.Fatal(err)
				}
			case "total bytes":
				if err := os.Truncate(filepath.Join(f.root, "herdr-mobile-relay"), maxBundleBinaryBytes); err != nil {
					t.Fatal(err)
				}
				for i := 0; i < 5; i++ {
					name := fmt.Sprintf("web/assets/ConversationHistory-%d.js", i+20)
					bundleWrite(t, f.root, name, nil, 0o644)
					if err := os.Truncate(filepath.Join(f.root, name), maxBundleFileBytes); err != nil {
						t.Fatal(err)
					}
				}
			case "directory count":
				for i := 0; i <= maxBundleDirs; i++ {
					if err := os.Mkdir(filepath.Join(f.root, fmt.Sprintf("web/builds/0.22.4-%d-%s", i+20, strings.Repeat("d", 16))), 0o755); err != nil {
						t.Fatal(err)
					}
				}
			case "inventory count":
				for i := 0; i <= maxBundleEntries; i++ {
					bundleWrite(t, f.root, fmt.Sprintf("web/assets/ConversationHistory-%d.js", i+20), nil, 0o644)
				}
			case "name bytes":
				bundleWrite(t, f.root, "web/assets/ConversationHistory-"+strings.Repeat("9", maxBundleName)+".js", nil, 0o644)
			case "file count":
				manifest, err := release.Load(f.root)
				if err != nil {
					t.Fatal(err)
				}
				for i := 0; i <= maxPackageFiles; i++ {
					name := fmt.Sprintf("web/assets/ConversationHistory-%d.js", i+20)
					bundleWrite(t, f.root, name, nil, 0o644)
					manifest.Files[name] = bundleHash(nil)
				}
				manifest.WebHash = packageWebHash(manifest.Files)
				data, err := json.Marshal(manifest)
				if err != nil {
					t.Fatal(err)
				}
				bundleWrite(t, f.root, "release-manifest.json", data, 0o644)
				f.build.WebHash = manifest.WebHash
			case "duplicate", "suffix", "UTF8":
				data, err := os.ReadFile(filepath.Join(f.root, "release-manifest.json"))
				if err != nil {
					t.Fatal(err)
				}
				if kind == "duplicate" {
					data = bytesWithDuplicate(data, "schema", "1")
				}
				if kind == "suffix" {
					data = append(data, []byte(" {}")...)
				}
				if kind == "UTF8" {
					data = []byte("{\"extra\":\"\xff\"}")
				}
				bundleWrite(t, f.root, "release-manifest.json", data, 0o644)
			case "descriptor duplicate", "descriptor type", "descriptor traversal", "descriptor missing file":
				data, err := os.ReadFile(filepath.Join(f.root, "web/release.json"))
				if err != nil {
					t.Fatal(err)
				}
				if kind == "descriptor duplicate" {
					data = bytesWithDuplicate(data, "schema", "1")
				}
				if kind == "descriptor type" {
					data = []byte(`{"schema":"1"}`)
				}
				if kind == "descriptor traversal" {
					file := f.descriptor.Files["entry"]
					file.Path = "../relay/.env"
					f.descriptor.Files["entry"] = file
					data, err = json.Marshal(f.descriptor)
					if err != nil {
						t.Fatal(err)
					}
				}
				if kind == "descriptor missing file" {
					file := f.descriptor.Files["javascript"]
					file.Path = "assets/app-" + strings.Repeat("f", 64) + ".js"
					f.descriptor.Files["javascript"] = file
					data, err = json.Marshal(f.descriptor)
					if err != nil {
						t.Fatal(err)
					}
				}
				bundleWrite(t, f.root, "web/release.json", data, 0o644)
				f.rebuild(t)
			}
			assertBundleState(t, f.observe(), Unknown)
		})
	}
	// Exact per-file boundaries and root-owned sealed metadata shapes, without
	// allocating sparse-file contents or requiring privileged filesystem writes.
	for _, name := range []string{"herdr-mobile-relay", "LICENSE", "web/release.json", "web/index.html"} {
		st := unix.Stat_t{Mode: unix.S_IFREG | 0o444, Uid: 0, Nlink: 1, Size: bundleFileLimit(name)}
		if !bundleRegular(&st, name) {
			t.Fatal("exact byte cap rejected")
		}
		st.Size++
		if bundleRegular(&st, name) {
			t.Fatal("byte cap prefix accepted")
		}
	}
	for _, name := range []string{"relay.env", "relay/.env", "relay/auth.sh", "relay/pi-command-bridge/config.json", "web/data.js", "web/fonts/token.woff2", "web/icons/device.png", "web/assets/research.js", "web/builds/0.22.4-1-" + strings.Repeat("a", 16) + "/journal.json", "web/assets/../index.html"} {
		if bundleContentPath(name) {
			t.Fatal("manifest authorized unshipped/private content")
		}
	}
	// Streaming is exercised over multiple fixed chunks, not a copied hash model.
	f := newBundleFixture(t, "linux/amd64")
	bundleWrite(t, f.root, "README.md", []byte(strings.Repeat("x", 2*bundleBufferBytes+1)), 0o644)
	f.rebuild(t)
	q, target := f.query()
	positions := []int64{}
	q.hooks = &bundleHooks{event: func(stage, name string, file *os.File) {
		if stage == "read" && name == "README.md" {
			pos, err := file.Seek(0, io.SeekCurrent)
			if err != nil {
				t.Fatal(err)
			}
			positions = append(positions, pos)
		}
	}}
	assertBundleState(t, q.Bundle(context.Background(), target, f.build), bundleExpectedState(OK))
	wantPositions := []int64{bundleBufferBytes, 2 * bundleBufferBytes, 2*bundleBufferBytes + 1, 2*bundleBufferBytes + 1}
	if !bundleWriterSafetySupported(runtime.GOOS) {
		wantPositions = []int64{}
	}
	if !reflect.DeepEqual(positions, wantPositions) {
		t.Fatal("read chunk/complete EOF or unsupported-host no-read bound changed")
	}
}
func bytesWithDuplicate(data []byte, key, value string) []byte {
	return []byte(`{"` + key + `":` + value + `,` + strings.TrimPrefix(strings.TrimSpace(string(data)), "{"))
}

// F003: conservative policy tests run everywhere. The actual ACL-grant cells
// run on authorized disposable hosted Darwin runners, with no required skip,
// and prove refusal before reads even when mode bits appear safe.
func TestInstalledBundleDarwinACLWriterSafetyRemainsUnknownBeforeReads(t *testing.T) {
	for _, goos := range []string{"linux", "darwin", "windows", "", "unrecognized"} {
		if bundleWriterSafetySupported(goos) != (goos == "linux" || goos == "darwin") {
			t.Fatal("unsupported writer safety became available")
		}
	}
	if runtime.GOOS != "darwin" {
		return
	} // actual Darwin ACL cells require the Darwin host matrix
	if os.Getenv("GITHUB_ACTIONS") != "true" || os.Getenv("RUNNER_ENVIRONMENT") != "github-hosted" {
		t.Fatal("Darwin ACL fixture requires an authorized disposable GitHub-hosted runner")
	}
	for _, kind := range []string{"file", "package root", "package directory", "ancestor"} {
		t.Run(kind, func(t *testing.T) {
			f := newBundleFixture(t, "darwin/arm64")
			parent, err := filepath.EvalSymlinks(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			root := filepath.Join(parent, "public-package")
			if err := os.Rename(f.root, root); err != nil {
				t.Fatal(err)
			}
			f.root = root
			path, grant := filepath.Join(root, "LICENSE"), "everyone allow write,append,writeattr,writeextattr"
			switch kind {
			case "package root":
				path = root
			case "package directory":
				path = filepath.Join(root, "web")
			case "ancestor":
				path = parent
			}
			if kind != "file" {
				grant = "everyone allow add_file,add_subdirectory,delete_child,writeattr,writeextattr"
			}
			beforeMode, err := os.Stat(path)
			if err != nil {
				t.Fatal(err)
			}
			// Test setup only, fixed literal argv on owned temporary paths. No
			// production executor, ACL mutation, privilege escalation or tools.
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			command := exec.CommandContext(ctx, "/bin/chmod", "+a", grant, path)
			command.Env = []string{"PATH=/usr/bin:/bin", "LANG=C", "HOME=" + parent}
			command.Stdout, command.Stderr = io.Discard, io.Discard
			err = command.Run()
			cancel()
			if err != nil {
				t.Fatal("disposable Darwin ACL grant unavailable")
			}
			afterMode, err := os.Stat(path)
			if err != nil {
				t.Fatal(err)
			}
			if afterMode.Mode().Perm() != beforeMode.Mode().Perm() || afterMode.Mode().Perm()&0o022 != 0 {
				t.Fatal("ACL fixture did not isolate writers hidden from mode bits")
			}
			before := bundleInventoryFingerprint(t, root)
			q, target := f.query()
			q.acl = nil // this cell must use the real production descriptor probe
			q.hooks = &bundleHooks{event: func(stage, _ string, _ *os.File) {
				if stage == "read" {
					t.Fatal("unestablished ACL safety acquired bundle content")
				}
			}}
			assertBundleState(t, q.Bundle(context.Background(), target, f.build), Unknown)
			cfg := ParseConfig(strings.NewReader("HERDR_RELAY_TRANSPORT=cloudflare\n"))
			r := Run(context.Background(), cfg, f.build, Options{}, Dependencies{Host: q, Transport: q})
			if check := findCheck(t, r, CheckBundle); check.State != Unknown || check.Evidence != EvidenceUnavailable || !check.Required || r.ExitCode() != 3 {
				t.Fatal("unknown ACL safety became a healthy or failed byte verdict")
			}
			if before != bundleInventoryFingerprint(t, root) {
				t.Fatal("ACL refusal changed package bytes/modes/inventory")
			}
			assertRenderPrivacy(t, r)
		})
	}
}

// Descriptor/permission-model fixtures are synthetic on every host: they test
// the real Bundle acquisition pipeline with injected Fstatfs classifications,
// not Linux runtime qualification. Separate Darwin probes exercise ACL/volume
// admission; these cells explicitly select the synthetic Linux policy.
func TestInstalledBundleFilesystemModelPolicy(t *testing.T) {
	for _, test := range []struct {
		name    string
		kind    uint64
		allowed bool
	}{
		{"ext", 0xef53, true}, {"xfs", 0x58465342, true}, {"btrfs", 0x9123683e, true},
		{"tmpfs", 0x01021994, true}, {"ramfs", 0x858458f6, true},
		{"unknown", 0, false}, {"nfs", 0x6969, false}, {"cifs", 0xff534d42, false},
		{"fuse", 0x65735546, false}, {"overlay", 0x794c7630, false},
		{"9p", 0x01021997, false}, {"zfs", 0x2fc12fc1, false}, {"unrecognized", 0xffff, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			q := &PackageQueries{hooks: &bundleHooks{filesystemType: func(int) (uint64, error) { return test.kind, nil }}}
			if q.bundleFilesystem(-1) != test.allowed || herdrFilesystemType(test.kind) != test.allowed {
				t.Fatal("bundle diverged from finite Herdr filesystem policy")
			}
		})
	}
	q := &PackageQueries{}
	if q.bundleFilesystem(-1) {
		t.Fatal("unavailable descriptor model admitted")
	}
	q.hooks = &bundleHooks{filesystemType: func(int) (uint64, error) { return 0xef53, fmt.Errorf("unavailable fixture model") }}
	if q.bundleFilesystem(-1) {
		t.Fatal("failed metadata acquisition admitted an allowlisted prefix")
	}
	parent := unix.Stat_t{Dev: 1}
	file := unix.Stat_t{Dev: 1}
	if !bundleFileFilesystem(&file, &parent) {
		t.Fatal("same admitted filesystem refused")
	}
	file.Dev = 2
	if bundleFileFilesystem(&file, &parent) {
		t.Fatal("unchecked regular-file filesystem mount admitted")
	}
}

func TestInstalledBundleFilesystemAdmissionBeforeContentOpen(t *testing.T) {
	for _, location := range []string{"ancestor", "root", "web", "nested directory"} {
		for _, kind := range []uint64{0, 0x6969, 0xff534d42, 0x65735546, 0x794c7630, 0xffff} {
			t.Run(fmt.Sprintf("%s-%x", location, kind), func(t *testing.T) {
				f := newBundleFixture(t, "linux/amd64")
				path := f.root
				switch location {
				case "ancestor":
					path = filepath.Dir(f.root)
				case "web":
					path = filepath.Join(f.root, "web")
				case "nested directory":
					path = filepath.Join(f.root, "relay/pi-command-bridge")
				}
				var blocked unix.Stat_t
				if err := unix.Lstat(path, &blocked); err != nil {
					t.Fatal(err)
				}
				q, _ := f.query()
				q.goos = "linux" // this fixture exercises Linux POSIX-mask volume policy
				checked := false
				descriptors := []int{}
				q.hooks = &bundleHooks{
					event: func(stage, _ string, _ *os.File) {
						if stage == "before_open" || stage == "open" || stage == "read" {
							t.Fatal("unsupported model reached content acquisition")
						}
					},
					filesystemType: func(fd int) (uint64, error) {
						descriptors = append(descriptors, fd)
						var st unix.Stat_t
						if unix.Fstat(fd, &st) != nil {
							t.Fatal("model lookup was not descriptor-bound")
						}
						if st.Dev == blocked.Dev && st.Ino == blocked.Ino {
							checked = true
							return kind, nil
						}
						return 0xef53, nil // synthetic admitted model for other directories
					},
				}
				before := bundleInventoryFingerprint(t, f.root)
				state, evidence := q.inspectBundle(context.Background())
				if !checked || state != Unknown || evidence != EvidenceUnavailable {
					t.Fatal("unsupported model fabricated a complete byte verdict")
				}
				for _, fd := range descriptors {
					var st unix.Stat_t
					if unix.Fstat(fd, &st) == nil {
						t.Fatal("model refusal retained a directory descriptor")
					}
				}
				if before != bundleInventoryFingerprint(t, f.root) {
					t.Fatal("model refusal mutated owned package")
				}
			})
		}
	}
}

func TestInstalledBundleFilesystemRevalidationAndUnavailableMetadata(t *testing.T) {
	for _, stage := range []string{"initial", "inventory", "before_open", "final"} {
		t.Run(stage, func(t *testing.T) {
			f := newBundleFixture(t, "linux/amd64")
			q, _ := f.query()
			q.goos = "linux" // synthetic Linux volume evidence on either host
			refuse := stage == "initial"
			refused := false
			opened := []*os.File{}
			readsAfterRefusal := 0
			q.hooks = &bundleHooks{
				event: func(event, _ string, file *os.File) {
					if event == stage {
						refuse = true
					}
					if event == "open" {
						opened = append(opened, file)
						if refused {
							t.Fatal("unavailable parent model opened content")
						}
					}
					if event == "read" && refused {
						readsAfterRefusal++
					}
				},
				filesystemType: func(int) (uint64, error) {
					if refuse {
						refused = true
						return 0xef53, fmt.Errorf("unavailable fixture metadata")
					}
					return 0xef53, nil
				},
			}
			state, evidence := q.inspectBundle(context.Background())
			if !refused || state != Unknown || evidence != EvidenceUnavailable || readsAfterRefusal != 0 {
				t.Fatalf("failed/revoked model produced a complete verdict or further reads: refused=%t state=%s evidence=%s reads=%d",
					refused, state, evidence, readsAfterRefusal)
			}
			if (stage == "initial" || stage == "inventory" || stage == "before_open") && len(opened) != 0 {
				t.Fatal("unavailable model opened content")
			}
			for _, file := range opened {
				if _, err := file.Stat(); err == nil {
					t.Fatal("revalidation refusal retained content fd")
				}
			}
		})
	}
	// Accepted classifications still reach the genuine full byte/descriptor
	// verifier, not an allowlist-only success, and F001 is not downgraded.
	for _, state := range []State{OK, Error} {
		f := newBundleFixture(t, "linux/amd64")
		if state == Error {
			if err := os.Chmod(filepath.Join(f.root, "herdr-mobile-relay"), 0o644); err != nil {
				t.Fatal(err)
			}
		}
		q, _ := f.query()
		q.goos = "linux"
		opened := 0
		q.hooks = &bundleHooks{filesystemType: func(int) (uint64, error) { return 0x01021994, nil }, event: func(stage, _ string, _ *os.File) {
			if stage == "open" {
				opened++
			}
		}}
		got, evidence := q.inspectBundle(context.Background())
		wantEvidence := EvidenceHealthy
		if state == Error {
			wantEvidence = EvidenceFailed
		}
		if got != state || evidence != wantEvidence || opened < len(bundleRequiredFiles()) {
			t.Fatal("admitted model bypassed complete hashing or stable nonexecutable contradiction")
		}
	}
}

// The package lives at <holder>/<unique name>: holder is a private ancestor
// whose entries the "component" seam changes between named stat and open.
func newBundleAncestorFixture(t *testing.T) (*bundleFixture, string) {
	t.Helper()
	f := newBundleFixture(t, "linux/amd64")
	holder := f.root + ".ancestor"
	t.Cleanup(func() { _ = os.RemoveAll(holder); _ = os.RemoveAll(holder + ".old") })
	nested := filepath.Join(holder, filepath.Base(f.root))
	if err := os.Mkdir(holder, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(holder, 0o755); err != nil { // independent of the umask
		t.Fatal(err)
	}
	if err := os.Rename(f.root, nested); err != nil {
		t.Fatal(err)
	}
	f.root = nested
	return f, holder
}

// An old mtime makes any later entry change alter Mtim, whatever the clock granularity.
func bundleAgeDirectory(t *testing.T, path string) {
	t.Helper()
	old := time.Unix(1_000_000_000, 0)
	if err := os.Chtimes(path, old, old); err != nil {
		t.Fatal(err)
	}
}
func bundleLstat(t *testing.T, path string) unix.Stat_t {
	t.Helper()
	var st unix.Stat_t
	if err := unix.Lstat(path, &st); err != nil {
		t.Fatal(err)
	}
	return st
}

// Runs the genuine inspector over an admitted synthetic model, calling mutate(n)
// at the nth "component" stage of the named component (1 acquisition walk, 2
// final rewalk). Returns the verdict and how often each stage fired.
func inspectWithComponentHook(t *testing.T, f *bundleFixture, component string, mutate func(call int)) (State, Evidence, map[string]int) {
	t.Helper()
	q, _ := f.query()
	q.goos = "linux" // component-race fixtures retain their synthetic Linux model
	stages, calls := map[string]int{}, 0
	q.hooks = &bundleHooks{
		filesystemType: func(int) (uint64, error) { return 0xef53, nil },
		event: func(stage, name string, _ *os.File) {
			stages[stage]++
			if stage == "component" && name == component {
				calls++
				mutate(calls)
			}
		},
	}
	state, evidence := q.inspectBundle(context.Background())
	return state, evidence, stages
}

func TestInstalledBundleAncestorEntryChangesAreNotUnknown(t *testing.T) {
	for _, kind := range []string{"create file", "create directory", "remove file", "remove directory"} {
		t.Run(kind, func(t *testing.T) {
			f, holder := newBundleAncestorFixture(t)
			sibling := func(n int) string { return filepath.Join(holder, fmt.Sprintf("sibling-%d", n)) }
			create := func(n int) error {
				if strings.HasSuffix(kind, "directory") {
					return os.Mkdir(sibling(n), 0o700)
				}
				return os.WriteFile(sibling(n), []byte("x"), 0o600)
			}
			removing := strings.HasPrefix(kind, "remove")
			for n := 1; removing && n <= 8; n++ {
				if err := create(n); err != nil {
					t.Fatal(err)
				}
			}
			bundleAgeDirectory(t, holder)
			calls := 0
			state, evidence, stages := inspectWithComponentHook(t, f, filepath.Base(holder), func(call int) {
				calls = call
				before := bundleLstat(t, holder)
				var err error
				if removing {
					err = os.Remove(sibling(call))
				} else {
					err = create(call)
				}
				if err != nil {
					t.Fatal(err)
				}
				// Prove the fixture really changes what the old full comparison
				// rejected (Mtim at least) while the ancestor identity is intact.
				after := bundleLstat(t, holder)
				if call == 1 && (sameBundleStat(&before, &after) || !sameBundleAncestor(&before, &after)) {
					t.Fatal("fixture did not make a benign ancestor-only metadata change")
				}
			})
			if state != OK || evidence != EvidenceHealthy || calls < 2 || stages["open"] < len(bundleRequiredFiles()) {
				t.Fatalf("ancestor %s: state=%s evidence=%s walks=%d stages=%v, want the complete healthy verdict", kind, state, evidence, calls, stages)
			}
		})
	}
}

func TestInstalledBundleAncestorIdentityChangeStillRefused(t *testing.T) {
	for _, kind := range []string{"replaced directory", "mode change"} {
		t.Run(kind, func(t *testing.T) {
			f, holder := newBundleAncestorFixture(t)
			state, evidence, stages := inspectWithComponentHook(t, f, filepath.Base(holder), func(call int) {
				if call != 1 {
					return
				}
				var err error
				if kind == "mode change" {
					err = os.Chmod(holder, 0o750) // still a permitted ancestor: only identity can refuse
				} else { // same path and intact valid package, but a different directory
					err = os.Rename(holder, holder+".old")
					if err == nil {
						err = os.Mkdir(holder, 0o755)
					}
					if err == nil {
						err = os.Chmod(holder, 0o755)
					}
					if err == nil {
						err = os.Rename(filepath.Join(holder+".old", filepath.Base(f.root)), f.root)
					}
				}
				if err != nil {
					t.Fatal(err)
				}
			})
			if state != Unknown || evidence != EvidenceUnavailable || stages["before_open"]+stages["open"]+stages["read"] != 0 {
				t.Fatalf("changed ancestor %s: state=%s evidence=%s stages=%v, want refusal before any content open", kind, state, evidence, stages)
			}
		})
	}
}

// The package root keeps the full comparison: a transient root entry leaves the
// inventory unchanged but alters timestamps, which must still refuse.
func TestInstalledBundleAncestorRootComponentKeepsFullComparison(t *testing.T) {
	f := newBundleFixture(t, "linux/amd64")
	bundleAgeDirectory(t, f.root)
	state, evidence, stages := inspectWithComponentHook(t, f, filepath.Base(f.root), func(call int) {
		if call != 1 {
			return
		}
		before, scratch := bundleLstat(t, f.root), filepath.Join(f.root, "scratch")
		if err := os.WriteFile(scratch, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Remove(scratch); err != nil {
			t.Fatal(err)
		}
		if after := bundleLstat(t, f.root); sameBundleStat(&before, &after) || !sameBundleAncestor(&before, &after) {
			t.Fatal("fixture did not make a timestamp-only root change")
		}
	})
	if state != Unknown || evidence != EvidenceUnavailable || stages["before_open"]+stages["open"]+stages["read"] != 0 {
		t.Fatalf("root timestamp change: state=%s evidence=%s stages=%v, want refusal before any content open", state, evidence, stages)
	}
}

// Ancestors: identity (device, inode, mode, owner, group) only. Link count, size
// and times never matter there, and the full comparison stays strict.
func TestInstalledBundleAncestorIdentityPredicate(t *testing.T) {
	base := unix.Stat_t{Dev: 1, Ino: 2, Mode: unix.S_IFDIR | 0o755, Uid: 3, Gid: 4}
	for _, test := range []struct {
		name     string
		change   func(*unix.Stat_t)
		identity bool
	}{
		{"device", func(s *unix.Stat_t) { s.Dev++ }, false},
		{"inode", func(s *unix.Stat_t) { s.Ino++ }, false},
		{"mode", func(s *unix.Stat_t) { s.Mode ^= 0o020 }, false},
		{"owner", func(s *unix.Stat_t) { s.Uid++ }, false},
		{"group", func(s *unix.Stat_t) { s.Gid++ }, false},
		{"link count", func(s *unix.Stat_t) { s.Nlink++ }, true},
		{"size", func(s *unix.Stat_t) { s.Size++ }, true},
		{"mtime", func(s *unix.Stat_t) { s.Mtim.Sec++ }, true},
		{"ctime", func(s *unix.Stat_t) { s.Ctim.Sec++ }, true},
	} {
		changed := base
		test.change(&changed)
		if sameBundleAncestor(&base, &changed) != test.identity || sameBundleStat(&base, &changed) || !sameBundleAncestor(&base, &base) || !sameBundleStat(&base, &base) {
			t.Fatalf("%s: ancestor identity/full comparison policy diverged", test.name)
		}
	}
}

func TestInstalledBundleDarwinFilesystemGateStillPrecedesAcquisition(t *testing.T) {
	if !bundleWriterSafetySupported("darwin") {
		t.Fatal("Darwin descriptor evidence path disabled")
	}
	f := newBundleFixture(t, "linux/amd64") // advertised target cannot override host policy
	q, target := f.query()
	probe := absentPackageACL()
	probe.filesystem = func(int) error { return errors.New("unsupported volume") }
	q.goos, q.acl = "darwin", &probe
	q.hooks = &bundleHooks{
		event: func(stage, _ string, _ *os.File) {
			if stage == "read" {
				t.Fatal("Darwin refusal reached bundle acquisition")
			}
		},
		filesystemType: func(int) (uint64, error) { t.Fatal("Darwin reached Linux filesystem policy"); return 0xef53, nil },
	}
	assertBundleState(t, q.Bundle(context.Background(), target, f.build), Unknown)
	cfg := ParseConfig(strings.NewReader("HERDR_RELAY_TRANSPORT=cloudflare\n"))
	r := Run(context.Background(), cfg, f.build, Options{}, Dependencies{Host: q, Transport: q})
	if check := findCheck(t, r, CheckBundle); check.State != Unknown || check.Evidence != EvidenceUnavailable || !check.Required || r.ExitCode() != 3 {
		t.Fatal("unestablished Darwin model became healthy")
	}
}

func TestInstalledBundleBuilderShapeAndCanonicalDescriptorBoundary(t *testing.T) {
	// Independent source topology comparison supplements actual fixture bytes;
	// it is NOT an extracted stamped package or publisher provenance check.
	builder, err := os.ReadFile("../../scripts/package-release.sh")
	if err != nil {
		t.Fatal(err)
	}
	for _, wrapper := range bundleWrappers {
		if !strings.Contains(string(builder), "        "+wrapper+" \\\n") && !strings.Contains(string(builder), "        "+wrapper+"; do") {
			t.Fatal("public wrapper policy diverged from package builder")
		}
	}
	if len(bundleWrappers) != 25 {
		t.Fatal("explicit wrapper inventory changed without a fixture update")
	}
	for _, required := range []string{"common.sh", "herdr-mobile-relay-service.sh", "plugin-on-event.sh", "setup-link.sh", "stable-setup.sh", "stable-teardown.sh", "start.sh", "transport-switch-locked.sh"} {
		found := false
		for _, name := range bundleWrappers {
			found = found || name == required
		}
		if !found {
			t.Fatal("existing release required wrapper omitted")
		}
	}
	f := newBundleFixture(t, "linux/amd64")
	data, err := os.ReadFile(filepath.Join(f.root, "web/release.json"))
	if err != nil {
		t.Fatal(err)
	}
	var projected map[string]any
	if json.Unmarshal(data, &projected) != nil {
		t.Fatal("invalid fixture descriptor")
	}
	projected["Schema"] = 99
	projected["Files"] = map[string]any{"entry": map[string]any{"Path": "../relay/.env", "SHA256": "secret-canary"}}
	data, err = json.Marshal(projected)
	if err != nil {
		t.Fatal(err)
	}
	bundleWrite(t, f.root, "web/release.json", data, 0o644)
	f.rebuild(t)
	// Exact lower-case projected keys govern; additive case-folded names never
	// overwrite them via encoding/json's struct decoder or grant private reads.
	assertBundleState(t, f.observe(), bundleExpectedState(OK))
	cfg := ParseConfig(strings.NewReader("HERDR_RELAY_TRANSPORT=cloudflare\n"))
	q := NewPackageQueries(f.root, cfg, f.build)
	r := Run(context.Background(), cfg, f.build, Options{}, Dependencies{Host: q, Transport: q})
	assertRenderPrivacy(t, r)
	ctx, cancel := context.WithCancel(context.Background())
	memory := &bundleDescriptorFS{ctx: ctx, files: map[string][]byte{"release.json": []byte("{}")}}
	file, err := memory.Open("release.json")
	if err != nil {
		t.Fatal(err)
	}
	cancel()
	if _, err := file.Read(make([]byte, 1)); err == nil || !memory.incomplete {
		t.Fatal("shared semantic reads ignored cancellation")
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := file.Stat(); err == nil {
		t.Fatal("semantic file remained open")
	}
}

func addHostedPackageGrant(t *testing.T, path string) {
	t.Helper()
	if !hostedDarwin() {
		t.Fatal("requires an authorized disposable GitHub-hosted runner")
	}
	before, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	grant := "group:everyone allow write,append,writeattr,writeextattr"
	if before.IsDir() {
		grant = "group:everyone allow add_file,add_subdirectory,delete_child,writeattr,writeextattr"
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	command := exec.CommandContext(ctx, "/bin/chmod", "+a", grant, path)
	command.Env = []string{"PATH=/usr/bin:/bin", "LANG=C"}
	command.Stdout, command.Stderr = io.Discard, io.Discard
	if command.Run() != nil {
		t.Fatal("hosted package ACL fixture creation failed")
	}
	after, err := os.Stat(path)
	if err != nil || before.Mode() != after.Mode() {
		t.Fatal("ACL fixture changed mode bits")
	}
}

func hostedPackageFixture(t *testing.T) *bundleFixture {
	t.Helper()
	if !hostedDarwin() {
		t.Fatal("requires an authorized disposable GitHub-hosted runner")
	}
	t.Setenv("TMPDIR", t.TempDir()) // all builder writes stay under disposable scratch
	return newBundleFixture(t, "darwin/arm64")
}

func TestDarwinHostedBundleACLFreePositive(t *testing.T) {
	if !hostedDarwin() {
		t.Skip("requires an authorized disposable GitHub-hosted runner")
	}
	f := hostedPackageFixture(t)
	q, target := f.query()
	q.acl = nil // genuine production ACL AND volume evidence, never fake admission
	assertBundleState(t, q.Bundle(context.Background(), target, f.build), OK)
}

func TestDarwinHostedBundleACLGrantRefusals(t *testing.T) {
	if !hostedDarwin() {
		t.Skip("requires an authorized disposable GitHub-hosted runner")
	}
	for _, object := range []string{"file", "root", "subdirectory", "ancestor"} {
		t.Run(object, func(t *testing.T) {
			f := hostedPackageFixture(t)
			path := f.root
			switch object {
			case "file":
				path = filepath.Join(f.root, "LICENSE")
			case "subdirectory":
				path = filepath.Join(f.root, "relay/pi-command-bridge")
			case "ancestor":
				path = filepath.Dir(f.root)
			}
			addHostedPackageGrant(t, path)
			q, target := f.query()
			q.acl = nil
			reads := 0
			q.hooks = &bundleHooks{event: func(stage, _ string, _ *os.File) {
				if stage == "read" {
					reads++
				}
			}}
			assertBundleState(t, q.Bundle(context.Background(), target, f.build), Unknown)
			cfg := ParseConfig(strings.NewReader("HERDR_RELAY_TRANSPORT=cloudflare\n"))
			r := Run(context.Background(), cfg, f.build, Options{}, Dependencies{Host: q, Transport: q})
			if check := findCheck(t, r, CheckBundle); !check.Required || check.State != Unknown || check.Evidence != EvidenceUnavailable || reads != 0 {
				t.Fatal("real ACL grant did not give required Unknown before reads")
			}
			assertRenderPrivacy(t, r)
		})
	}
}
