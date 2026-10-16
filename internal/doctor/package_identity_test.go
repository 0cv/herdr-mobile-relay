package doctor

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"sort"
	"strings"
	"testing"

	"golang.org/x/sys/unix"
)

func metadataFixture(t *testing.T) (map[string]any, BuildIdentity) {
	t.Helper()
	build := testBuild()
	files := map[string]string{"herdr-mobile-relay": strings.Repeat("a", 64), "web/index.html": strings.Repeat("b", 64), "web/assets/app.js": strings.Repeat("c", 64)}
	keys := []string{"web/index.html", "web/assets/app.js"}
	sort.Strings(keys)
	hash := sha256.New()
	for _, key := range keys {
		fmt.Fprintf(hash, "%s\x00%s\n", key, files[key])
	}
	build.WebHash = hex.EncodeToString(hash.Sum(nil))
	return map[string]any{"schema": 1, "version": build.Version, "revision": build.Revision, "target": build.Target,
		"web_hash": build.WebHash, "files": files, "app_transports": []string{"herdr-e2ee-v2"}}, build
}
func metadataEncode(t *testing.T, value any) []byte {
	t.Helper()
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return data
}
func metadataRoot(t *testing.T, data []byte) string {
	t.Helper()
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "release-manifest.json"), data, 0o644); err != nil {
		t.Fatal(err)
	}
	return root
}
func metadataRead(root string, build BuildIdentity) BuildIdentity {
	return readPackageIdentityWithHost(root, build.Version, build.Revision, build.Target, "linux", openParent, openSelectedFile, func(r io.Reader) ([]byte, error) {
		return io.ReadAll(io.LimitReader(r, maxPackageMetadata+1))
	})
}
func assertMetadataUnavailable(t *testing.T, got BuildIdentity) {
	t.Helper()
	if got != (BuildIdentity{}) {
		t.Fatal("unvalidated package metadata retained identity")
	}
}

func TestPackageIdentityDarwinRefusesBeforeFilesystemOpen(t *testing.T) {
	manifest, build := metadataFixture(t) // build target is linux/amd64, not the Darwin host
	manifest["private_fixture"] = "private-data-canary"
	data := metadataEncode(t, manifest)
	root := metadataRoot(t, data)
	filename := filepath.Join(root, "release-manifest.json")
	var beforeStat unix.Stat_t
	if err := unix.Stat(filename, &beforeStat); err != nil || !packageRegular(&beforeStat) {
		t.Fatal("metadata fixture was not apparently safe regular-file metadata")
	}
	beforeBytes, err := os.ReadFile(filename)
	if err != nil {
		t.Fatal(err)
	}
	beforeInfo, err := os.Stat(filename)
	if err != nil {
		t.Fatal(err)
	}
	beforeEntries, err := os.ReadDir(root)
	if err != nil {
		t.Fatal(err)
	}

	parentOpens, contentOpens, reads := 0, 0, 0
	got := readPackageIdentityWithHost(root, build.Version, build.Revision, build.Target, "darwin",
		func(path string) (int, string, error) { parentOpens++; return openParent(path) },
		func(parent int, base string) (int, error) { contentOpens++; return openSelectedFile(parent, base) },
		func(r io.Reader) ([]byte, error) { reads++; return io.ReadAll(r) })
	assertMetadataUnavailable(t, got)
	if parentOpens != 0 || contentOpens != 0 || reads != 0 {
		t.Fatal("Darwin metadata refusal crossed an acquisition boundary")
	}
	if runtime.GOOS == "darwin" && ReadPackageIdentity(root, build.Version, build.Revision, build.Target) != (BuildIdentity{}) {
		t.Fatal("production host policy admitted metadata based on its advertised Linux target")
	}
	afterBytes, err := os.ReadFile(filename)
	if err != nil {
		t.Fatal(err)
	}
	afterInfo, err := os.Stat(filename)
	if err != nil {
		t.Fatal(err)
	}
	afterEntries, err := os.ReadDir(root)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(beforeBytes, afterBytes) || !os.SameFile(beforeInfo, afterInfo) || beforeInfo.Mode() != afterInfo.Mode() ||
		!beforeInfo.ModTime().Equal(afterInfo.ModTime()) || !reflect.DeepEqual(beforeEntries, afterEntries) {
		t.Fatal("Darwin metadata refusal mutated fixture bytes, metadata or directory inventory")
	}
}

func TestPackageIdentityMetadataOnlySealedAndNonmutating(t *testing.T) {
	manifest, build := metadataFixture(t)
	data := metadataEncode(t, manifest)
	root := metadataRoot(t, data)
	filename := filepath.Join(root, "release-manifest.json")
	for _, mode := range []os.FileMode{0o644, 0o444} {
		if err := os.Chmod(filename, mode); err != nil {
			t.Fatal(err)
		}
		before, err := os.Stat(filename)
		if err != nil {
			t.Fatal(err)
		}
		entries, err := os.ReadDir(root)
		if err != nil {
			t.Fatal(err)
		}
		if metadataRead(root, build) != build {
			t.Fatal("complete compiled-bound metadata refused")
		}
		after, err := os.Stat(filename)
		if err != nil {
			t.Fatal(err)
		}
		actual, err := os.ReadFile(filename)
		if err != nil {
			t.Fatal(err)
		}
		finalEntries, err := os.ReadDir(root)
		if err != nil {
			t.Fatal(err)
		}
		if string(actual) != string(data) || before.Mode() != after.Mode() || !before.ModTime().Equal(after.ModTime()) || len(entries) != len(finalEntries) {
			t.Fatal("metadata inspection mutated package")
		}
	}
	// Listed web/binary files deliberately do not exist. This proves metadata
	// agreement is NOT file verification and must not mark CheckBundle healthy.
	cfg := ParseConfig(strings.NewReader("HERDR_RELAY_TRANSPORT=cloudflare\n"))
	q := NewLocalQueries(cfg, build)
	r := Run(context.Background(), cfg, build, Options{}, Dependencies{Host: q, Transport: q})
	if findCheck(t, r, CheckBuild).State != OK || findCheck(t, r, CheckBundle).State != Unknown || r.ExitCode() != 3 {
		t.Fatal("manifest-only identity qualified package/host")
	}
	assertRenderPrivacy(t, r)
}

func TestPackageIdentityCompiledBindingAndUnavailable(t *testing.T) {
	manifest, build := metadataFixture(t)
	root := metadataRoot(t, metadataEncode(t, manifest))
	for _, field := range []string{"version", "revision", "target", "dev", "invalid target"} {
		expected := build
		switch field {
		case "version":
			expected.Version = "0.99.9"
		case "revision":
			expected.Revision = strings.Repeat("d", 40)
		case "target":
			expected.Target = "darwin/arm64"
		case "dev":
			expected.Version = "dev"
		case "invalid target":
			expected.Target = "private-node-canary"
		}
		assertMetadataUnavailable(t, metadataRead(root, expected))
	}
	assertMetadataUnavailable(t, metadataRead(filepath.Join(root, "absent"), build))
	for _, root := range []string{"", ".", "/", root + "/../private", root + "/"} {
		assertMetadataUnavailable(t, metadataRead(root, build))
	}
	// Even a fully specified config cannot learn its expected build from a relay.
	cfg := ParseConfig(strings.NewReader("HERDR_RELAY_TRANSPORT=cloudflare\nHERDR_RELAY_INSTANCE_ID=private-instance-canary\n"))
	missing := BuildIdentity{}
	q := NewLocalQueries(cfg, missing)
	if q.bound {
		t.Fatal("missing expected package identity permitted local acquisition")
	}
	r := Run(context.Background(), cfg, missing, Options{}, Dependencies{Host: q, Transport: q})
	if r.ExitCode() != 3 || findCheck(t, r, CheckBuild).State != Unknown || findCheck(t, r, CheckRelay).State != Unknown {
		t.Fatal("responding relay could fill missing package authority")
	}
}

func TestPackageIdentityCompleteSchemaBounds(t *testing.T) {
	for _, test := range []struct {
		name string
		edit func(map[string]any)
	}{
		{"schema type", func(m map[string]any) { m["schema"] = "1" }},
		{"schema version", func(m map[string]any) { m["schema"] = 2 }},
		{"schema fractional", func(m map[string]any) { m["schema"] = 1.5 }},
		{"missing schema", func(m map[string]any) { delete(m, "schema") }},
		{"version type", func(m map[string]any) { m["version"] = true }},
		{"missing revision", func(m map[string]any) { delete(m, "revision") }},
		{"hash type", func(m map[string]any) { m["web_hash"] = 64 }},
		{"hash mismatch", func(m map[string]any) { m["web_hash"] = strings.Repeat("f", 64) }},
		{"files type", func(m map[string]any) { m["files"] = []string{"web/index.html"} }},
		{"file hash type", func(m map[string]any) {
			m["files"] = map[string]any{"herdr-mobile-relay": true, "web/index.html": strings.Repeat("a", 64)}
		}},
		{"empty files", func(m map[string]any) { m["files"] = map[string]string{} }},
		{"missing binary", func(m map[string]any) { delete(m["files"].(map[string]string), "herdr-mobile-relay") }},
		{"missing index", func(m map[string]any) { delete(m["files"].(map[string]string), "web/index.html") }},
		{"self listing", func(m map[string]any) {
			m["files"].(map[string]string)["release-manifest.json"] = strings.Repeat("a", 64)
		}},
		{"path traversal", func(m map[string]any) { m["files"].(map[string]string)["web/../private"] = strings.Repeat("a", 64) }},
		{"absolute path", func(m map[string]any) {
			m["files"].(map[string]string)["/private/path-canary"] = strings.Repeat("a", 64)
		}},
		{"unclean path", func(m map[string]any) { m["files"].(map[string]string)["web//private"] = strings.Repeat("a", 64) }},
		{"control path", func(m map[string]any) { m["files"].(map[string]string)["web/\x1bprivate"] = strings.Repeat("a", 64) }},
		{"invalid hash", func(m map[string]any) { m["files"].(map[string]string)["web/index.html"] = "secret-metadata-canary" }},
		{"uppercase hash", func(m map[string]any) { m["files"].(map[string]string)["web/index.html"] = strings.Repeat("A", 64) }},
		{"transports type", func(m map[string]any) { m["app_transports"] = "secret-metadata-canary" }},
		{"transport element type", func(m map[string]any) { m["relay_transports"] = []int{1} }},
		{"transport duplicates", func(m map[string]any) { m["relay_transports"] = []string{"v2", "v2"} }},
		{"control transport", func(m map[string]any) { m["relay_transports"] = []string{"\x1bprivate"} }},
	} {
		t.Run(test.name, func(t *testing.T) {
			m, b := metadataFixture(t)
			test.edit(m)
			assertMetadataUnavailable(t, metadataRead(metadataRoot(t, metadataEncode(t, m)), b))
		})
	}
	m, b := metadataFixture(t)
	good := string(metadataEncode(t, m))
	bad := []string{
		good + " {}", good + "!", good[:len(good)-1], `[]`, `null`, `<html>secret-metadata-canary</html>`,
		strings.TrimSuffix(good, "}") + `,"schema":1}`, strings.TrimSuffix(good, "}") + `,"extra":{"x":1,"x":2}}`,
		strings.TrimSuffix(good, "}") + `,"extra":"` + strings.Repeat("x", 2049) + `"}`,
		strings.TrimSuffix(good, "}") + `,"extra":` + strings.Repeat("[", 18) + "null" + strings.Repeat("]", 18) + `}`,
		strings.TrimSuffix(good, "}") + `,"extra":[` + strings.TrimSuffix(strings.Repeat("null,", 65), ",") + `]}`,
		"{\"extra\":\"\xff\"}",
	}
	var fields strings.Builder
	fields.WriteString("{")
	for i := 0; i <= maxPackageFiles; i++ {
		if i > 0 {
			fields.WriteString(",")
		}
		fmt.Fprintf(&fields, "\"k%d\":null", i)
	}
	fields.WriteString("}")
	bad = append(bad, strings.TrimSuffix(good, "}")+`,"extra":`+fields.String()+`}`)
	// Nodes are bounded independently of per-container bounds. Keep the valid
	// required manifest so refusal cannot be explained by missing identity.
	bad = append(bad, strings.TrimSuffix(good, "}")+`,"extra":[`+strings.TrimSuffix(strings.Repeat("["+strings.TrimSuffix(strings.Repeat("["+strings.TrimSuffix(strings.Repeat("null,", 64), ",")+"],", 64), ",")+"],", 3), ",")+`]}`)
	bad = append(bad, strings.TrimSuffix(good, "}")+`,"extra":`+strings.Repeat("1", 129)+`}`)
	counted, countedBuild := metadataFixture(t)
	files := counted["files"].(map[string]string)
	for i := len(files); i < maxPackageFiles; i++ {
		files[fmt.Sprintf("extras/file%04d", i)] = strings.Repeat("a", 64)
	}
	if metadataRead(metadataRoot(t, metadataEncode(t, counted)), countedBuild) != countedBuild {
		t.Fatal("exact bounded manifest file count refused")
	}
	files["extras/over-limit"] = strings.Repeat("a", 64)
	assertMetadataUnavailable(t, metadataRead(metadataRoot(t, metadataEncode(t, counted)), countedBuild))
	for _, data := range bad {
		assertMetadataUnavailable(t, metadataRead(metadataRoot(t, []byte(data)), b))
	}
	boundary := good + strings.Repeat(" ", maxPackageMetadata-len(good))
	if metadataRead(metadataRoot(t, []byte(boundary)), b) != b {
		t.Fatal("complete exact byte bound refused")
	}
	assertMetadataUnavailable(t, metadataRead(metadataRoot(t, []byte(boundary+" ")), b))
	assertMetadataUnavailable(t, metadataRead(metadataRoot(t, []byte(boundary+"!")), b))
	// A duplicate within the authoritative file map is also refused.
	duplicateFiles := strings.Replace(good, `"files":{`, `"files":{"web/index.html":"`+strings.Repeat("d", 64)+`",`, 1)
	assertMetadataUnavailable(t, metadataRead(metadataRoot(t, []byte(duplicateFiles)), b))
}

func TestPackageIdentityNoFollowSpecialAndReplacedFiles(t *testing.T) {
	m, b := metadataFixture(t)
	data := metadataEncode(t, m)
	for _, kind := range []string{"symlink", "parent symlink", "fifo", "directory", "hardlink", "group writable", "world writable"} {
		t.Run(kind, func(t *testing.T) {
			root := metadataRoot(t, data)
			file := filepath.Join(root, "release-manifest.json")
			switch kind {
			case "symlink":
				other := filepath.Join(root, "original")
				if err := os.Rename(file, other); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(other, file); err != nil {
					t.Fatal(err)
				}
			case "parent symlink":
				link := filepath.Join(filepath.Dir(root), filepath.Base(root)+"-link")
				if err := os.Symlink(root, link); err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { _ = os.Remove(link) })
				root = link
			case "fifo", "directory":
				if err := os.Remove(file); err != nil {
					t.Fatal(err)
				}
				if kind == "fifo" {
					if err := unix.Mkfifo(file, 0o600); err != nil {
						t.Fatal(err)
					}
				} else {
					if err := os.Mkdir(file, 0o700); err != nil {
						t.Fatal(err)
					}
				}
			case "hardlink":
				if err := os.Link(file, filepath.Join(root, "other")); err != nil {
					t.Fatal(err)
				}
			case "group writable":
				if err := os.Chmod(file, 0o664); err != nil {
					t.Fatal(err)
				}
			case "world writable":
				if err := os.Chmod(file, 0o646); err != nil {
					t.Fatal(err)
				}
			}
			assertMetadataUnavailable(t, metadataRead(root, b))
		})
	}
	for _, kind := range []string{"rename file", "replace parent", "chmod", "read error", "short read"} {
		t.Run(kind, func(t *testing.T) {
			root := metadataRoot(t, data)
			read := func(r io.Reader) ([]byte, error) {
				bytes, err := io.ReadAll(r)
				if err != nil {
					t.Fatal(err)
				}
				switch kind {
				case "rename file":
					if err := os.Rename(filepath.Join(root, "release-manifest.json"), filepath.Join(root, "old")); err != nil {
						t.Fatal(err)
					}
					if err := os.WriteFile(filepath.Join(root, "release-manifest.json"), data, 0o644); err != nil {
						t.Fatal(err)
					}
				case "replace parent":
					if err := os.Rename(root, root+"-old"); err != nil {
						t.Fatal(err)
					}
					t.Cleanup(func() { _ = os.RemoveAll(root + "-old") })
					if err := os.Mkdir(root, 0o700); err != nil {
						t.Fatal(err)
					}
					if err := os.WriteFile(filepath.Join(root, "release-manifest.json"), data, 0o644); err != nil {
						t.Fatal(err)
					}
				case "chmod":
					if err := os.Chmod(filepath.Join(root, "release-manifest.json"), 0o600); err != nil {
						t.Fatal(err)
					}
				case "read error":
					return nil, errors.New("private-error-canary")
				case "short read":
					return bytes[:len(bytes)-1], nil
				}
				return bytes, nil
			}
			assertMetadataUnavailable(t, readPackageIdentityWithHost(root, b.Version, b.Revision, b.Target, "linux", openParent, openSelectedFile, read))
		})
	}
	st := unix.Stat_t{Mode: unix.S_IFREG | 0o644, Uid: uint32(os.Geteuid() + 1), Nlink: 1, Size: 10}
	if st.Uid == 0 {
		t.Fatal("invalid synthetic ownership fixture")
	}
	if packageRegular(&st) {
		t.Fatal("foreign-owned metadata admitted")
	}
	// Dedicated path grammar never normalizes hostile map entries into trust.
	for _, path := range []string{".", "..", "../x", "web/../x", "web\\x", "/web/x", "web//x", "web/x/", "web/secret name", strings.Repeat("a", 513)} {
		if packagePath(path) {
			t.Fatal("invalid manifest path admitted")
		}
	}
	if !reflect.DeepEqual(ReadPackageIdentity("relative-private-canary", "dev", "unknown", "unknown"), BuildIdentity{}) {
		t.Fatal("invalid metadata leaked")
	}
}

func TestSharedManifestProjectionRemainsMetadataOnlyAndPrivate(t *testing.T) {
	manifest, build := metadataFixture(t)
	// This path is syntactically valid metadata but forbidden content authority.
	manifest["files"].(map[string]string)["relay/.env"] = strings.Repeat("d", 64)
	data := metadataEncode(t, manifest)
	projection, ok := projectPackageManifest(data, build.Version, build.Revision, build.Target)
	if !ok || projection.build != build || packageIdentity(data, build.Version, build.Revision, build.Target) != build {
		t.Fatal("metadata-only compatibility relaxed or conflated with bundle policy")
	}
	if bundleContentPath("relay/.env") {
		t.Fatal("metadata path became private-content authority")
	}
	for _, output := range []string{fmt.Sprintf("%v", projection), fmt.Sprintf("%#v", projection)} {
		if output != "private package metadata" {
			t.Fatal("private file map escaped formatting")
		}
	}
	direct, refusal := projection.MarshalJSON()
	encoded, err := json.Marshal(projection)
	var wrapped *json.MarshalerError
	if len(direct) != 0 || refusal == nil || refusal.Error() != "diagnostic metadata is not serializable" ||
		len(encoded) != 0 || err == nil || !errors.As(err, &wrapped) || wrapped.Type != reflect.TypeOf(&projection) ||
		wrapped.Err == nil || wrapped.Err.Error() != refusal.Error() {
		t.Fatalf("private projection serialization result: (%q, %v)", encoded, err)
	}
	other, ok := projectPackageManifest(data, build.Version, build.Revision, build.Target)
	if !ok {
		t.Fatal("independent projection refused")
	}
	projection.files["web/index.html"] = strings.Repeat("e", 64)
	if other.files["web/index.html"] == projection.files["web/index.html"] {
		t.Fatal("projection retained an alias/cache across acquisitions")
	}
	for _, bad := range [][]byte{append(append([]byte(nil), data...), []byte(" {}")...), bytesWithDuplicate(data, "schema", "1"), []byte(`{"schema":1,"extra":{"x":1,"x":2}}`)} {
		if _, ok := packageObject(bad); ok {
			t.Fatal("shared strict grammar accepted duplicate/suffix input")
		}
	}
}
