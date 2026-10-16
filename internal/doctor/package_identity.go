package doctor

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"unicode/utf8"

	"golang.org/x/sys/unix"
)

const maxPackageMetadata = 256 * 1024
const maxPackageFiles = 1024

// ReadPackageIdentity reads only release-manifest.json in the executing package
// root supplied by command dispatch. It compares compiled identity and the
// manifest's web-map digest; it does NOT hash listed files, verify the installed
// binary, or establish complete bundle integrity. Failure returns no raw data.
// Filesystem reads are bounded in bytes, not a hard noncooperative-OS deadline.
func ReadPackageIdentity(root, version, revision, target string) BuildIdentity {
	return readPackageIdentity(root, version, revision, target, func(r io.Reader) ([]byte, error) {
		return io.ReadAll(io.LimitReader(r, maxPackageMetadata+1))
	})
}

// Sealed (0444) and ordinary builder (0644) metadata are both readable. Reject
// foreign ownership, group/world writers, special bits and multiple links.
// Owner/root write access is not integrity proof against that owner/root.
func packageRegular(st *unix.Stat_t) bool {
	return st.Mode&unix.S_IFMT == unix.S_IFREG && (st.Uid == 0 || int(st.Uid) == os.Geteuid()) &&
		st.Mode&0o022 == 0 && st.Mode&0o7000 == 0 && st.Mode&0o400 != 0 && st.Nlink == 1 &&
		st.Size >= 0 && st.Size <= maxPackageMetadata
}

func samePackageStat(a, b *unix.Stat_t) bool {
	return a.Dev == b.Dev && a.Ino == b.Ino && a.Mode == b.Mode && a.Uid == b.Uid &&
		a.Gid == b.Gid && a.Nlink == b.Nlink && a.Size == b.Size
}

func readPackageIdentity(root, version, revision, target string, read func(io.Reader) ([]byte, error)) BuildIdentity {
	return readPackageIdentityWithHost(root, version, revision, target, runtime.GOOS, openParent, openSelectedFile, read)
}

func readPackageIdentityWithHost(root, version, revision, target, goos string,
	acquireParent func(string) (int, string, error), openContent func(int, string) (int, error),
	read func(io.Reader) ([]byte, error),
) BuildIdentity {
	if earlyReaderHostRefusesModeOnlyAcquisition(goos) {
		return BuildIdentity{}
	}
	// Do not even open metadata for an unqualified development build/target.
	expected := BuildIdentity{version, revision, strings.Repeat("0", 64), target}
	if !validBuild(expected) || !filepath.IsAbs(root) || filepath.Clean(root) != root || root == "/" {
		return BuildIdentity{}
	}
	filename := filepath.Join(root, "release-manifest.json")
	parent, base, err := acquireParent(filename)
	if err != nil {
		return BuildIdentity{}
	}
	defer unix.Close(parent)
	var before unix.Stat_t
	if unix.Fstatat(parent, base, &before, unix.AT_SYMLINK_NOFOLLOW) != nil || !packageRegular(&before) {
		return BuildIdentity{}
	}
	fd, err := openContent(parent, base)
	if err != nil {
		return BuildIdentity{}
	}
	file := os.NewFile(uintptr(fd), "packaged diagnostic metadata")
	defer file.Close()
	var opened unix.Stat_t
	if unix.Fstat(fd, &opened) != nil || !packageRegular(&opened) || !samePackageStat(&before, &opened) {
		return BuildIdentity{}
	}
	initial, err := file.Stat()
	if err != nil {
		return BuildIdentity{}
	}
	data, err := read(file)
	if err != nil || len(data) > maxPackageMetadata || int64(len(data)) != opened.Size {
		return BuildIdentity{}
	}
	final, err := file.Stat()
	if err != nil || !os.SameFile(initial, final) || initial.Size() != final.Size() || initial.Mode() != final.Mode() || !initial.ModTime().Equal(final.ModTime()) {
		return BuildIdentity{}
	}
	currentParent, currentBase, err := acquireParent(filename)
	if err != nil {
		return BuildIdentity{}
	}
	defer unix.Close(currentParent)
	var named, after unix.Stat_t
	if unix.Fstatat(currentParent, currentBase, &named, unix.AT_SYMLINK_NOFOLLOW) != nil || unix.Fstat(fd, &after) != nil ||
		!packageRegular(&named) || !packageRegular(&after) || !samePackageStat(&named, &after) || !samePackageStat(&opened, &after) {
		return BuildIdentity{}
	}
	last, err := file.Stat()
	if err != nil || !os.SameFile(initial, last) || initial.Size() != last.Size() || initial.Mode() != last.Mode() || !initial.ModTime().Equal(last.ModTime()) {
		return BuildIdentity{}
	}
	return packageIdentity(data, version, revision, target)
}

// Dedicated complete manifest projection. Bounds apply even to additive fields;
// duplicate keys at any depth are refused rather than silently last-wins.
func packageIdentity(data []byte, version, revision, target string) BuildIdentity {
	manifest, ok := projectPackageManifest(data, version, revision, target)
	if !ok {
		return BuildIdentity{}
	}
	return manifest.build
}

// Private, per-acquisition projection. It has no exported mutable map, paths in
// errors or serialization surface. Metadata recognition intentionally does NOT
// enforce the complete public-content policy used by the bundle verifier.
type packageManifest struct {
	build BuildIdentity
	files map[string]string
}

func (packageManifest) String() string   { return "private package metadata" }
func (packageManifest) GoString() string { return "private package metadata" }
func (packageManifest) MarshalJSON() ([]byte, error) {
	return nil, errors.New("diagnostic metadata is not serializable")
}

func packageObject(data []byte) (map[string]any, bool) {
	if len(data) > maxPackageMetadata || !utf8.Valid(data) {
		return nil, false
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	nodes := 0
	value, ok := packageValue(decoder, 0, &nodes)
	if !ok {
		return nil, false
	}
	if _, err := decoder.Token(); err != io.EOF {
		return nil, false
	}
	object, ok := value.(map[string]any)
	return object, ok
}

func projectPackageManifest(data []byte, version, revision, target string) (packageManifest, bool) {
	object, ok := packageObject(data)
	if !ok {
		return packageManifest{}, false
	}
	schema, ok := localInteger(object["schema"])
	if !ok || schema != 1 {
		return packageManifest{}, false
	}
	v, vOK := object["version"].(string)
	r, rOK := object["revision"].(string)
	t, tOK := object["target"].(string)
	web, wOK := object["web_hash"].(string)
	build := BuildIdentity{v, r, web, t}
	if !vOK || !rOK || !tOK || !wOK || !validBuild(build) || v != version || r != revision || t != target {
		return packageManifest{}, false
	}
	values, ok := object["files"].(map[string]any)
	if !ok || len(values) == 0 || len(values) > maxPackageFiles {
		return packageManifest{}, false
	}
	files := make(map[string]string, len(values))
	for name, value := range values {
		hash, ok := value.(string)
		if !ok || !packagePath(name) || !hexField(hash, 64) {
			return packageManifest{}, false
		}
		files[name] = hash
	}
	if files["herdr-mobile-relay"] == "" || files["web/index.html"] == "" || files["release-manifest.json"] != "" {
		return packageManifest{}, false
	}
	for _, key := range []string{"app_transports", "relay_transports"} {
		if value, present := object[key]; present {
			list, ok := value.([]any)
			if !ok || len(list) == 0 || len(list) > 16 {
				return packageManifest{}, false
			}
			seen := make(map[string]bool)
			for _, item := range list {
				text, ok := item.(string)
				if !ok || len(text) == 0 || len(text) > 128 || seen[text] {
					return packageManifest{}, false
				}
				for _, b := range []byte(text) {
					if b < 0x21 || b > 0x7e {
						return packageManifest{}, false
					}
				}
				seen[text] = true
			}
		}
	}
	if packageWebHash(files) != web {
		return packageManifest{}, false
	}
	return packageManifest{build, files}, true
}

// Matches release.hashFileMap's canonical sorted name/NUL/hash/newline encoding.
// The caller decides whether the hashes are metadata or actual acquired bytes.
func packageWebHash(files map[string]string) string {
	keys := make([]string, 0)
	for name := range files {
		if strings.HasPrefix(name, "web/") {
			keys = append(keys, name)
		}
	}
	if len(keys) == 0 {
		return ""
	}
	sort.Strings(keys)
	hasher := sha256.New()
	for _, name := range keys {
		fmt.Fprintf(hasher, "%s\x00%s\n", name, files[name])
	}
	return hex.EncodeToString(hasher.Sum(nil))
}

func packagePath(name string) bool {
	if len(name) == 0 || len(name) > 512 || path.IsAbs(name) || path.Clean(name) != name || name == "." || name == ".." || strings.HasPrefix(name, "../") {
		return false
	}
	for _, b := range []byte(name) {
		if (b >= 'a' && b <= 'z') || (b >= 'A' && b <= 'Z') || (b >= '0' && b <= '9') || strings.ContainsRune("/._@+-", rune(b)) {
			continue
		}
		return false
	}
	return true
}

func packageValue(d *json.Decoder, depth int, nodes *int) (any, bool) {
	*nodes = *nodes + 1
	if depth > 16 || *nodes > 8192 {
		return nil, false
	}
	token, err := d.Token()
	if err != nil {
		return nil, false
	}
	switch token := token.(type) {
	case string:
		return token, len(token) <= 2048
	case json.Number:
		return token, len(token) <= 128
	case json.Delim:
		switch token {
		case '{':
			object := make(map[string]any)
			for d.More() {
				key, err := d.Token()
				name, ok := key.(string)
				if err != nil || !ok || len(name) > 2048 || len(object) >= maxPackageFiles {
					return nil, false
				}
				if _, duplicate := object[name]; duplicate {
					return nil, false
				}
				value, ok := packageValue(d, depth+1, nodes)
				if !ok {
					return nil, false
				}
				object[name] = value
			}
			end, err := d.Token()
			return object, err == nil && end == json.Delim('}')
		case '[':
			items := make([]any, 0)
			for d.More() {
				if len(items) >= 64 {
					return nil, false
				}
				value, ok := packageValue(d, depth+1, nodes)
				if !ok {
					return nil, false
				}
				items = append(items, value)
			}
			end, err := d.Token()
			return items, err == nil && end == json.Delim(']')
		default:
			return nil, false
		}
	default:
		return token, true // required scalar types are checked in projection
	}
}
