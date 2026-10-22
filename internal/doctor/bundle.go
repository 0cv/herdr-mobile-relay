package doctor

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/0cv/herdr-mobile-relay/internal/release"
	"golang.org/x/sys/unix"
)

const (
	maxBundleEntries     = 1280 // includes manifest and directories, not just files
	maxBundleDirs        = 128
	maxBundleDepth       = 5
	maxBundleName        = 128
	maxBundleFileBytes   = 32 * 1024 * 1024
	maxBundleBinaryBytes = 128 * 1024 * 1024
	maxBundleBytes       = 256 * 1024 * 1024
	maxBundleEntryBytes  = 1024 * 1024
	bundleBufferBytes    = 32 * 1024
)

// PackageQueries adds only installed byte consistency to the genuine local
// adapter. Dispatch supplies the OS-resolved physical executing package root;
// there is no CLI/config/ambient root selector, filesystem executor or cache.
// Package consistency is not publisher authentication or runtime qualification.
type PackageQueries struct {
	*LocalQueries
	root   string
	target LocalTarget
	build  BuildIdentity
	bound  bool
	hooks  *bundleHooks // package-private synchronized fixtures only; nil in production
	goos   string       // empty selects the actual host; private fixture override
	acl    *packageACLProbe
}
type bundleHooks struct {
	// Stages: component (root walk, between a component's named stat and open;
	// name is the component, file nil), inventory, before_open, open, read, final.
	event          func(stage, name string, file *os.File)
	filesystemType func(fd int) (uint64, error) // owned fixtures; production uses Fstatfs
}

func NewPackageQueries(root string, cfg Inspection, build BuildIdentity) *PackageQueries {
	return &PackageQueries{LocalQueries: NewLocalQueries(cfg, build), root: root,
		target: selectedLocalTarget(cfg, build), build: build,
		bound: cfg.Code() == ConfigValid && cfg.Mode() != ModeUnknown && validBuild(build) && bundleRootPath(root)}
}
func (PackageQueries) String() string   { return "private package diagnostic queries" }
func (PackageQueries) GoString() string { return "private package diagnostic queries" }
func (PackageQueries) MarshalJSON() ([]byte, error) {
	return nil, errors.New("diagnostic queries are not serializable")
}

var _ HostQueries = (*PackageQueries)(nil)
var _ TransportQueries = (*PackageQueries)(nil)

// Finite matrix: complete stable hashes/inventory/descriptor agreement => OK;
// complete stable hash/web-map/reference/SRI contradictions, a readable regular
// relay binary without execute bits, or an unlisted approved public file => Error.
// Missing required/listed content or descriptor, invalid metadata/descriptor
// projection, unsupported/private names, unsafe or unestablished writer safety,
// unreadable objects, changing evidence, caps and cancellation => Unknown.
// Error is withheld until the same final snapshot checks required for OK pass.
// This detects cooperative changes, NOT an atomic hostile-same-owner snapshot:
// an adversary can edit/restore between observations or replace all metadata.
func (q *PackageQueries) Bundle(parent context.Context, target LocalTarget, build BuildIdentity) Datum {
	if q == nil || !q.bound || target != q.target || build != q.build || absent(parent) || contextEnded(parent) {
		return localUnknown()
	}
	// Permission policy follows the host, never manifest target/config.
	if !bundleWriterSafetySupported(q.packageHost()) {
		return localUnknown()
	}
	ctx, cancel := context.WithTimeout(parent, GlobalBudget) // runner passes its remaining global budget
	defer cancel()
	state, evidence := q.inspectBundle(ctx)
	if contextEnded(ctx) {
		return localDatum(Unknown, EvidenceCancelled)
	}
	return localDatum(state, evidence)
}

// A supported host still needs per-object writer evidence before acquisition.
func bundleWriterSafetySupported(goos string) bool { return goos == "linux" || goos == "darwin" }

func (q *PackageQueries) packageHost() string {
	if q.goos != "" {
		return q.goos
	}
	return runtime.GOOS
}

func (q *PackageQueries) bundleObjectSafe(fd int, ancestor bool) bool {
	if q.packageHost() != "darwin" {
		return q.bundleFilesystem(fd)
	}
	probe := productionPackageACLProbe
	if q.acl != nil {
		probe = *q.acl
	} else if runtime.GOOS != "darwin" {
		return false // a simulated Darwin host needs explicitly injected evidence
	}
	if ancestor {
		return probe.objectSafe(fd, true) == nil
	}
	return probe.objectSafe(fd, false) == nil
}

// Reuse Herdr's finite Linux POSIX ACL-mask filesystem policy, not its
// socket/private-parent shape checks. Bundle's own owner/mode checks deny
// foreign writes (including named ACL grants through the group-class mask).
// Remote, FUSE, rich-ACL, unknown and unestablished overlay models stay unknown.
// This assumes the trusted kernel/root model, not hostile privileged mounts.
func (q *PackageQueries) bundleFilesystem(fd int) bool {
	if q.hooks == nil || q.hooks.filesystemType == nil {
		return herdrFilesystem(fd)
	}
	kind, err := q.hooks.filesystemType(fd)
	return err == nil && herdrFilesystemType(kind)
}

// A regular-file mount could otherwise introduce an unchecked filesystem
// beneath an admitted directory. Refuse cross-device files before opening
// content; mounted directories get their own descriptor-bound model check.
func bundleFileFilesystem(file, parent *unix.Stat_t) bool { return file.Dev == parent.Dev }

func (q *PackageQueries) event(stage, name string, file *os.File) {
	if q.hooks != nil && q.hooks.event != nil {
		q.hooks.event(stage, name, file)
	}
}

// This list is the explicit package-release.sh wrapper topology, not a general
// .sh allowlist. Adding shipped content requires a separately reviewed policy.
var bundleWrappers = []string{
	"common.sh", "native-install-transaction.sh", "herdr-mobile-relay-service.sh",
	"install-service.sh", "install-systemd-user-service.sh", "plugin-on-event.sh", "plugin-status.sh",
	"pi-commands.sh", "service.sh", "setup-link.sh", "setup.sh", "speech-voices.sh", "stable-setup.sh",
	"stable-teardown.sh", "start.sh", "tailscale.sh", "tailscale-external.sh", "tailscale-cli.sh",
	"tailscale-cli-service.sh", "transport-switch-locked.sh", "uninstall.sh", "uninstall-service.sh", "uninstall-systemd-user-service.sh",
}
var bundleWebPublic = []string{
	"_headers", "_redirects", "index.html", "herdr-bootstrap.js", "manifest-loader.js", "manifest.webmanifest",
	"notification-icons.js", "release.json", "setup.webmanifest", "sw.js", "version.json",
	"fonts/nerd-symbols-mono-v3.4.0.license.txt", "fonts/nerd-symbols-mono-v3.4.0.woff2",
	"icons/apple-touch-icon.png", "icons/icon-192.png", "icons/icon-512.png", "icons/icon-maskable-512.png",
	"icons/icon.svg", "icons/notification-badge.png",
}
var bundleBuildName = regexp.MustCompile(`^[0-9]+\.[0-9]+\.[0-9]+-[0-9]+-[a-f0-9]{16,64}$`)
var bundleAssetName = regexp.MustCompile(`^(?:app-[a-f0-9]{64}\.(?:js|css)|(?:ConversationHistory|ResumeTimingSettings|workspace-tools)-[0-9]+\.js|attachment-hash\.worker-[A-Za-z0-9_-]{8,64}\.js)$`)

func bundleContentPath(name string) bool {
	if !packagePath(name) {
		return false
	}
	switch name {
	case "herdr-mobile-relay", "LICENSE", "README.md", ".env.example", "relay/pi-command-bridge/bridge.mjs", "relay/pi-command-bridge/index.ts":
		return true
	}
	for _, wrapper := range bundleWrappers {
		if name == "relay/"+wrapper {
			return true
		}
	}
	if !strings.HasPrefix(name, "web/") {
		return false
	}
	web := strings.TrimSuffix(strings.TrimPrefix(name, "web/"), ".br")
	for _, public := range bundleWebPublic {
		if web == public {
			return true
		}
	}
	parts := strings.Split(web, "/")
	if len(parts) == 2 && parts[0] == "assets" {
		return bundleAssetName.MatchString(parts[1])
	}
	return len(parts) == 3 && parts[0] == "builds" && bundleBuildName.MatchString(parts[1]) && parts[2] == "index.html"
}
func bundleDirectoryPath(name string) bool {
	switch name {
	case ".", "relay", "relay/pi-command-bridge", "web", "web/assets", "web/builds", "web/fonts", "web/icons":
		return true
	}
	parts := strings.Split(name, "/")
	return len(parts) == 3 && parts[0] == "web" && parts[1] == "builds" && bundleBuildName.MatchString(parts[2])
}
func bundleRequiredFiles() []string {
	out := []string{"herdr-mobile-relay", "LICENSE", "README.md", ".env.example", "relay/pi-command-bridge/bridge.mjs", "relay/pi-command-bridge/index.ts"}
	for _, name := range bundleWrappers {
		out = append(out, "relay/"+name)
	}
	for _, name := range bundleWebPublic {
		out = append(out, "web/"+name)
	}
	return out
}
func bundleFileLimit(name string) int64 {
	switch name {
	case "herdr-mobile-relay":
		return maxBundleBinaryBytes
	case "release-manifest.json", "web/release.json":
		return maxPackageMetadata
	case "web/index.html":
		return maxBundleEntryBytes
	}
	if strings.HasPrefix(name, "web/builds/") && strings.HasSuffix(name, "/index.html") {
		return maxBundleEntryBytes
	}
	return maxBundleFileBytes
}
func bundleOwner(st *unix.Stat_t) bool { return st.Uid == 0 || int(st.Uid) == os.Geteuid() }
func bundleDirectory(st *unix.Stat_t, ancestor bool) bool {
	if st.Mode&unix.S_IFMT != unix.S_IFDIR || !bundleOwner(st) || st.Mode&0o400 == 0 || st.Mode&0o100 == 0 || st.Mode&0o6000 != 0 {
		return false
	}
	if st.Mode&0o022 == 0 {
		return st.Mode&0o1000 == 0
	}
	// Only root-owned sticky temporary ancestors may have other writers. The
	// selected root and ALL package subdirectories must never have them.
	return ancestor && st.Uid == 0 && st.Mode&0o1000 != 0
}
func bundleRegular(st *unix.Stat_t, name string) bool {
	return st.Mode&unix.S_IFMT == unix.S_IFREG && bundleOwner(st) && st.Mode&0o022 == 0 &&
		st.Mode&0o7000 == 0 && st.Mode&0o400 != 0 && st.Nlink == 1 && st.Size >= 0 && st.Size <= bundleFileLimit(name)
}
func sameBundleStat(a, b *unix.Stat_t) bool {
	return samePackageStat(a, b) && a.Mtim == b.Mtim && a.Ctim == b.Ctim
}

// Ancestors of the package root compare by identity only. Their link count, size
// and times change whenever any entry in them is created or removed (shared temp
// or $HOME) and say nothing about the package; the next component is resolved
// no-follow from the held descriptor, re-checked, and the chain rewalked at the end.
func sameBundleAncestor(a, b *unix.Stat_t) bool {
	return a.Dev == b.Dev && a.Ino == b.Ino && a.Mode == b.Mode && a.Uid == b.Uid && a.Gid == b.Gid
}
func bundleRootPath(root string) bool {
	return len(root) > 0 && len(root) <= 4096 && filepath.IsAbs(root) && filepath.Clean(root) == root && root != "/" &&
		len(strings.Split(root, "/")) <= 64 && utf8.ValidString(root) && strings.IndexFunc(root, unicode.IsControl) < 0
}

const bundleDirFlags = unix.O_RDONLY | unix.O_DIRECTORY | unix.O_NOFOLLOW | unix.O_CLOEXEC | unix.O_NONBLOCK

// No-follow absolute ancestry is validated at acquisition and rewalked at the
// end. An ancestor changing inode/mode/owner is unknown, even if the old root fd
// remains readable. Ancestor timestamps are not proof of package content, so
// ancestors compare by identity only; the root itself keeps the full comparison.
func (q *PackageQueries) openBundleRoot(ctx context.Context, root string) (int, []unix.Stat_t, bool) {
	if !bundleRootPath(root) || contextEnded(ctx) {
		return -1, nil, false
	}
	fd, err := unix.Open("/", bundleDirFlags, 0)
	if err != nil {
		return -1, nil, false
	}
	chain := make([]unix.Stat_t, 0)
	parts := strings.Split(strings.TrimPrefix(root, "/"), "/")
	for i, part := range parts {
		ancestor := i != len(parts)-1 // the last component is the package root itself
		var parent, named, opened unix.Stat_t
		if contextEnded(ctx) || unix.Fstat(fd, &parent) != nil || !bundleDirectory(&parent, true) || !q.bundleObjectSafe(fd, true) ||
			unix.Fstatat(fd, part, &named, unix.AT_SYMLINK_NOFOLLOW) != nil || !bundleDirectory(&named, ancestor) {
			unix.Close(fd)
			return -1, nil, false
		}
		chain = append(chain, parent)
		// Fixture seam between the named stat and the open; no-op without hooks.
		q.event("component", part, nil)
		next, err := unix.Openat(fd, part, bundleDirFlags, 0)
		unix.Close(fd)
		if err != nil {
			return -1, nil, false
		}
		fd = next
		if contextEnded(ctx) || unix.Fstat(fd, &opened) != nil ||
			(ancestor && !sameBundleAncestor(&named, &opened)) || (!ancestor && !sameBundleStat(&named, &opened)) || !q.bundleObjectSafe(fd, ancestor) {
			unix.Close(fd)
			return -1, nil, false
		}
	}
	return fd, chain, true
}

type bundleNode struct {
	stat unix.Stat_t
	dir  bool
}
type bundleInventory map[string]bundleNode

// Inventory reads directory entries and no-follow stat metadata only. Content
// policy/special-file/link/ownership/size refusals occur BEFORE any content open.
func (q *PackageQueries) bundleInventoryAt(ctx context.Context, root int) (bundleInventory, bool) {
	nodes := make(bundleInventory)
	entries, directories := 0, 0
	var total int64
	var walk func(int, string, int) bool
	walk = func(fd int, name string, depth int) (good bool) {
		var start unix.Stat_t
		if contextEnded(ctx) || depth > maxBundleDepth || unix.Fstat(fd, &start) != nil || !bundleDirectory(&start, false) || !q.bundleObjectSafe(fd, false) {
			return false
		}
		directories++
		if directories > maxBundleDirs {
			return false
		}
		nodes[name] = bundleNode{start, true}
		dup, err := unix.Openat(fd, ".", bundleDirFlags, 0)
		if err != nil {
			return false
		}
		var opened unix.Stat_t
		if contextEnded(ctx) || unix.Fstat(dup, &opened) != nil || !sameBundleStat(&start, &opened) || !q.bundleObjectSafe(dup, false) {
			unix.Close(dup)
			return false
		}
		file := os.NewFile(uintptr(dup), "diagnostic package directory")
		defer func() {
			if file.Close() != nil {
				good = false
			}
		}()
		for {
			if contextEnded(ctx) || (q.packageHost() == "darwin" && !q.bundleObjectSafe(dup, false)) {
				return false
			}
			batch, err := file.ReadDir(32)
			if (err != nil && err != io.EOF) || (q.packageHost() == "darwin" && !q.bundleObjectSafe(dup, false)) {
				return false
			}
			for _, entry := range batch {
				entries++
				if contextEnded(ctx) || entries > maxBundleEntries || len(entry.Name()) > maxBundleName || !packagePath(entry.Name()) || strings.Contains(entry.Name(), "/") {
					return false
				}
				relative := entry.Name()
				if name != "." {
					relative = name + "/" + relative
				}
				if !packagePath(relative) {
					return false
				}
				var st unix.Stat_t
				if unix.Fstatat(fd, entry.Name(), &st, unix.AT_SYMLINK_NOFOLLOW) != nil {
					return false
				}
				if st.Mode&unix.S_IFMT == unix.S_IFDIR {
					if !bundleDirectoryPath(relative) || !bundleDirectory(&st, false) {
						return false
					}
					child, err := unix.Openat(fd, entry.Name(), bundleDirFlags, 0)
					if err != nil {
						return false
					}
					var opened unix.Stat_t
					ok := unix.Fstat(child, &opened) == nil && sameBundleStat(&st, &opened) &&
						(q.packageHost() != "darwin" || opened.Dev == start.Dev) && walk(child, relative, depth+1)
					if unix.Close(child) != nil || !ok {
						return false
					}
				} else {
					if (relative != "release-manifest.json" && !bundleContentPath(relative)) || !bundleRegular(&st, relative) || !bundleFileFilesystem(&st, &start) {
						return false
					}
					total += st.Size
					if total > maxBundleBytes {
						return false
					}
					nodes[relative] = bundleNode{st, false}
				}
			}
			if err == io.EOF {
				break
			}
		}
		var final unix.Stat_t
		return !contextEnded(ctx) && unix.Fstat(fd, &final) == nil && sameBundleStat(&start, &final) &&
			(q.packageHost() != "darwin" || (q.bundleObjectSafe(fd, false) && q.bundleObjectSafe(dup, false)))
	}
	ok := walk(root, ".", 0)
	return nodes, ok
}

func (q *PackageQueries) bundleParentAt(ctx context.Context, root int, name string, inventory bundleInventory) (int, string, bool) {
	if !packagePath(name) || contextEnded(ctx) {
		return -1, "", false
	}
	fd, err := unix.Openat(root, ".", bundleDirFlags, 0)
	if err != nil {
		return -1, "", false
	}
	expected, exists := inventory["."]
	var opened unix.Stat_t
	if !exists || !expected.dir || unix.Fstat(fd, &opened) != nil || !sameBundleStat(&expected.stat, &opened) || !q.bundleObjectSafe(fd, false) {
		unix.Close(fd)
		return -1, "", false
	}
	parts := strings.Split(name, "/")
	dir := ""
	for _, component := range parts[:len(parts)-1] {
		if dir == "" {
			dir = component
		} else {
			dir += "/" + component
		}
		expected, exists := inventory[dir]
		var named, opened unix.Stat_t
		if !exists || !expected.dir || contextEnded(ctx) || unix.Fstatat(fd, component, &named, unix.AT_SYMLINK_NOFOLLOW) != nil || !sameBundleStat(&expected.stat, &named) {
			unix.Close(fd)
			return -1, "", false
		}
		next, err := unix.Openat(fd, component, bundleDirFlags, 0)
		unix.Close(fd)
		if err != nil {
			return -1, "", false
		}
		fd = next
		if contextEnded(ctx) || unix.Fstat(fd, &opened) != nil || !sameBundleStat(&expected.stat, &opened) || !q.bundleObjectSafe(fd, false) {
			unix.Close(fd)
			return -1, "", false
		}
	}
	return fd, parts[len(parts)-1], true
}

// Fixed streaming buffer, complete EOF/size check, descriptor/named identity
// before/after read, and context between every chunk. Capture is only requested
// for strict metadata and the descriptor's three already-approved public files.
func (q *PackageQueries) readBundleFile(ctx context.Context, root int, name string, inventory bundleInventory, capture bool, readBytes *int64) (string, []byte, bool) {
	return q.acquireBundleFile(ctx, root, name, inventory, capture, readBytes, false)
}

func (q *PackageQueries) acquireBundleFile(ctx context.Context, root int, name string, inventory bundleInventory, capture bool, readBytes *int64, preflight bool) (digest string, data []byte, ok bool) {
	node, exists := inventory[name]
	if !exists || node.dir || (name != "release-manifest.json" && !bundleContentPath(name)) || !bundleRegular(&node.stat, name) {
		return "", nil, false
	}
	if !preflight {
		q.event("before_open", name, nil)
	}
	parent, base, ready := q.bundleParentAt(ctx, root, name, inventory)
	if !ready {
		return "", nil, false
	}
	defer func() {
		if unix.Close(parent) != nil {
			ok = false
		}
	}()
	var named, parentStat unix.Stat_t
	if contextEnded(ctx) || unix.Fstat(parent, &parentStat) != nil || unix.Fstatat(parent, base, &named, unix.AT_SYMLINK_NOFOLLOW) != nil || !sameBundleStat(&node.stat, &named) || !bundleFileFilesystem(&named, &parentStat) {
		return "", nil, false
	}
	fd, err := unix.Openat(parent, base, unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_NONBLOCK|unix.O_CLOEXEC, 0)
	if err != nil {
		return "", nil, false
	}
	file := os.NewFile(uintptr(fd), "diagnostic package content")
	defer func() {
		if file.Close() != nil {
			ok = false
		}
	}()
	if preflight {
		q.event("preflight", name, file)
	} else {
		q.event("open", name, file)
	}
	var opened unix.Stat_t
	if contextEnded(ctx) || unix.Fstat(fd, &opened) != nil || !sameBundleStat(&node.stat, &opened) || !bundleRegular(&opened, name) || !q.bundleObjectSafe(fd, false) {
		return "", nil, false
	}
	if preflight {
		return "", nil, true
	}
	hasher := sha256.New()
	buffer := make([]byte, bundleBufferBytes)
	var count int64
	for {
		if contextEnded(ctx) || (q.packageHost() == "darwin" && !q.bundleObjectSafe(fd, false)) {
			return "", nil, false
		}
		// Read one extra byte, never a silently accepted prefix.
		limit := min(int64(len(buffer)), opened.Size+1-count)
		if limit <= 0 {
			return "", nil, false
		}
		n, err := file.Read(buffer[:int(limit)])
		count += int64(n)
		*readBytes += int64(n)
		q.event("read", name, file)
		if contextEnded(ctx) || count > opened.Size || *readBytes > maxBundleBytes+2*maxPackageMetadata ||
			(q.packageHost() == "darwin" && !q.bundleObjectSafe(fd, false)) {
			return "", nil, false
		}
		if n > 0 {
			hasher.Write(buffer[:n])
			if capture {
				data = append(data, buffer[:n]...)
			}
		}
		if err == io.EOF {
			break
		}
		if err != nil || n == 0 {
			return "", nil, false
		}
	}
	var after unix.Stat_t
	if count != opened.Size || unix.Fstat(fd, &after) != nil || !sameBundleStat(&opened, &after) ||
		unix.Fstatat(parent, base, &named, unix.AT_SYMLINK_NOFOLLOW) != nil || !sameBundleStat(&opened, &named) || contextEnded(ctx) {
		return "", nil, false
	}
	return hex.EncodeToString(hasher.Sum(nil)), data, true
}

// Prevalidate the descriptor using the same strict bounded JSON grammar. The
// semantic verifier receives only captured approved bytes, NEVER an os.DirFS
// or a path from descriptor/config as filesystem authority.
func bundleDescriptor(data []byte) (release.WebDescriptor, bool) {
	object, ok := packageObject(data)
	if !ok {
		return release.WebDescriptor{}, false
	}
	schema, sOK := localInteger(object["schema"])
	assets, aOK := localInteger(object["assets"])
	_, vOK := object["version"].(string)
	_, bOK := object["build"].(string)
	_, eOK := object["entry"].(string)
	files, fOK := object["files"].(map[string]any)
	if !sOK || schema != 1 || !aOK || assets < 0 || !vOK || !bOK || !eOK || !fOK || len(files) != 3 {
		return release.WebDescriptor{}, false
	}
	descriptor := release.WebDescriptor{Schema: int(schema), Assets: int(assets), Version: object["version"].(string),
		Build: object["build"].(string), Entry: object["entry"].(string), Files: make(map[string]release.WebDescriptorFile, 3)}
	for _, key := range []string{"entry", "javascript", "stylesheet"} {
		file, ok := files[key].(map[string]any)
		if !ok {
			return release.WebDescriptor{}, false
		}
		for _, field := range []string{"path", "sha256", "integrity"} {
			if _, ok := file[field].(string); !ok {
				return release.WebDescriptor{}, false
			}
		}
		for _, field := range []string{"brotli_sha256", "brotli_integrity"} {
			if value, exists := file[field]; exists {
				if _, ok := value.(string); !ok {
					return release.WebDescriptor{}, false
				}
			}
		}
		name := file["path"].(string)
		if !bundleContentPath("web/"+name) || !packagePath(name) {
			return release.WebDescriptor{}, false
		}
		brHash, _ := file["brotli_sha256"].(string)
		brSRI, _ := file["brotli_integrity"].(string)
		descriptor.Files[key] = release.WebDescriptorFile{Path: name, SHA256: file["sha256"].(string), Integrity: file["integrity"].(string),
			BrotliSHA256: brHash, BrotliIntegrity: brSRI}
	}
	return descriptor, true
}

// The shared release semantics see only four bounded in-memory public entries.
// Opens and reads remain cancellation-aware; there is no ambient filesystem.
// A verifier reading more than its finite contract gets incomplete evidence.
type bundleDescriptorFS struct {
	ctx        context.Context
	files      map[string][]byte
	read       int64
	incomplete bool
}
type bundleMemoryFile struct {
	owner  *bundleDescriptorFS
	reader *bytes.Reader
	size   int64
	closed bool
}

func (m *bundleDescriptorFS) Open(name string) (fs.File, error) {
	data, ok := m.files[name]
	if !ok || !fs.ValidPath(name) || (name != "release.json" && !bundleContentPath("web/"+name)) {
		m.incomplete = true
		return nil, fs.ErrNotExist
	}
	if contextEnded(m.ctx) || int64(len(data)) > bundleFileLimit("web/"+name) {
		m.incomplete = true
		return nil, fs.ErrPermission
	}
	return &bundleMemoryFile{owner: m, reader: bytes.NewReader(data), size: int64(len(data))}, nil
}
func (f *bundleMemoryFile) Read(p []byte) (int, error) {
	if f.closed {
		return 0, fs.ErrClosed
	}
	if contextEnded(f.owner.ctx) {
		f.owner.incomplete = true
		return 0, fs.ErrPermission
	}
	if len(p) > bundleBufferBytes {
		p = p[:bundleBufferBytes]
	}
	n, err := f.reader.Read(p)
	f.owner.read += int64(n)
	if f.owner.read > 4*maxBundleFileBytes+maxPackageMetadata || contextEnded(f.owner.ctx) {
		f.owner.incomplete = true
		return 0, fs.ErrPermission
	}
	return n, err
}
func (f *bundleMemoryFile) Close() error { f.closed = true; return nil }
func (f *bundleMemoryFile) Stat() (fs.FileInfo, error) {
	if f.closed {
		return nil, fs.ErrClosed
	}
	return bundleMemoryInfo{f.size}, nil
}

type bundleMemoryInfo struct{ size int64 }

func (bundleMemoryInfo) Name() string       { return "diagnostic public web content" }
func (i bundleMemoryInfo) Size() int64      { return i.size }
func (bundleMemoryInfo) Mode() fs.FileMode  { return 0o444 }
func (bundleMemoryInfo) ModTime() time.Time { return time.Time{} }
func (bundleMemoryInfo) IsDir() bool        { return false }
func (bundleMemoryInfo) Sys() any           { return nil }

func sameBundleInventory(a, b bundleInventory) bool {
	if len(a) != len(b) {
		return false
	}
	for name, node := range a {
		next, ok := b[name]
		if !ok || node.dir != next.dir || !sameBundleStat(&node.stat, &next.stat) {
			return false
		}
	}
	return true
}

// All approved content is opened no-follow and checked before the first byte.
// The same admission runs again on each actual read fd and final reacquisition.
func (q *PackageQueries) preflightBundleFiles(ctx context.Context, root int, inventory bundleInventory) bool {
	if q.packageHost() != "darwin" {
		return true // preserve the Linux acquisition sequence
	}
	var unused int64
	for name, node := range inventory {
		if !node.dir {
			if _, _, ok := q.acquireBundleFile(ctx, root, name, inventory, false, &unused, true); !ok {
				return false
			}
		}
	}
	return true
}

func (q *PackageQueries) inspectBundle(ctx context.Context) (state State, evidence Evidence) {
	root, chain, ok := q.openBundleRoot(ctx, q.root)
	if !ok {
		return Unknown, EvidenceUnavailable
	}
	defer func() {
		if unix.Close(root) != nil {
			state, evidence = Unknown, EvidenceUnavailable
		}
	}()
	inventory, ok := q.bundleInventoryAt(ctx, root)
	if !ok {
		return Unknown, EvidenceUnavailable
	}
	q.event("inventory", "", nil)
	if !q.preflightBundleFiles(ctx, root, inventory) {
		return Unknown, EvidenceUnavailable
	}
	var readBytes int64
	_, manifestData, ok := q.readBundleFile(ctx, root, "release-manifest.json", inventory, true, &readBytes)
	if !ok {
		return Unknown, EvidenceUnavailable
	}
	manifest, ok := projectPackageManifest(manifestData, q.build.Version, q.build.Revision, q.build.Target)
	if !ok || manifest.build != q.build {
		return Unknown, EvidenceMalformed
	}
	for name := range manifest.files {
		if !bundleContentPath(name) {
			return Unknown, EvidenceUnavailable
		}
		if node, exists := inventory[name]; !exists || node.dir {
			return Unknown, EvidenceUnavailable
		}
	}
	for _, name := range bundleRequiredFiles() {
		if manifest.files[name] == "" {
			return Unknown, EvidenceUnavailable
		}
	}
	// A safely acquired readable binary without execute bits is a package
	// shape contradiction, not unavailable evidence. Withhold the failure
	// until complete hashing and the same final snapshot checks as success.
	contradiction := inventory["herdr-mobile-relay"].stat.Mode&0o111 == 0
	for name, node := range inventory {
		if !node.dir && name != "release-manifest.json" && manifest.files[name] == "" {
			contradiction = true
		}
	}
	actual := make(map[string]string, len(manifest.files))
	descriptorHash, descriptorData, ok := q.readBundleFile(ctx, root, "web/release.json", inventory, true, &readBytes)
	if !ok {
		return Unknown, EvidenceUnavailable
	}
	actual["web/release.json"] = descriptorHash
	descriptor, ok := bundleDescriptor(descriptorData)
	if !ok {
		return Unknown, EvidenceMalformed
	}
	capture := make(map[string]bool)
	for _, file := range descriptor.Files {
		name := "web/" + file.Path
		if manifest.files[name] == "" {
			return Unknown, EvidenceUnavailable
		}
		capture[name] = true
		if file.BrotliSHA256 != "" || file.BrotliIntegrity != "" {
			if manifest.files[name+".br"] == "" {
				return Unknown, EvidenceUnavailable
			}
		}
	}
	keys := make([]string, 0, len(manifest.files))
	for name := range manifest.files {
		keys = append(keys, name)
	}
	sort.Strings(keys)
	captured := make(map[string][]byte, 3)
	for _, name := range keys {
		if contextEnded(ctx) {
			return Unknown, EvidenceCancelled
		}
		if name != "web/release.json" {
			hash, data, ok := q.readBundleFile(ctx, root, name, inventory, capture[name], &readBytes)
			if !ok {
				return Unknown, EvidenceUnavailable
			}
			actual[name] = hash
			if capture[name] {
				captured[strings.TrimPrefix(name, "web/")] = data
			}
		}
		contradiction = contradiction || actual[name] != manifest.files[name]
	}
	contradiction = contradiction || packageWebHash(actual) != q.build.WebHash
	// Shared current entry/JS/style/version/build/SRI/reference semantics over
	// already captured bounded public bytes. No disk walks/loads or raw errors.
	// Canonicalize only the strictly projected fields before the shared typed
	// decoder: encoding/json's case-insensitive struct matching must not let
	// additive Schema/Files/Path fields override the validated exact keys.
	semanticData, err := json.Marshal(descriptor)
	if err != nil {
		return Unknown, EvidenceMalformed
	}
	captured["release.json"] = semanticData
	semanticFS := &bundleDescriptorFS{ctx: ctx, files: captured}
	if _, err := release.VerifyWebDescriptor(semanticFS, q.build.Version); err != nil {
		contradiction = true
	}
	if semanticFS.incomplete || contextEnded(ctx) {
		return Unknown, EvidenceUnavailable
	}
	for _, file := range descriptor.Files {
		if file.BrotliSHA256 != "" || file.BrotliIntegrity != "" {
			contradiction = contradiction || actual["web/"+file.Path+".br"] != file.BrotliSHA256
		}
	}
	q.event("final", "", nil)
	// Reacquire the manifest, check byte-for-byte (including additive fields),
	// then rescan complete inventory. Retain no success from an earlier call.
	_, finalManifest, ok := q.readBundleFile(ctx, root, "release-manifest.json", inventory, true, &readBytes)
	if !ok || !bytes.Equal(manifestData, finalManifest) {
		return Unknown, EvidenceUnavailable
	}
	finalInventory, ok := q.bundleInventoryAt(ctx, root)
	if !ok || !sameBundleInventory(inventory, finalInventory) || !q.preflightBundleFiles(ctx, root, finalInventory) {
		return Unknown, EvidenceUnavailable
	}
	current, currentChain, ok := q.openBundleRoot(ctx, q.root)
	if !ok {
		return Unknown, EvidenceUnavailable
	}
	defer func() {
		if unix.Close(current) != nil {
			state, evidence = Unknown, EvidenceUnavailable
		}
	}()
	var original, named unix.Stat_t
	rootNode := inventory["."]
	if len(chain) != len(currentChain) || unix.Fstat(root, &original) != nil || unix.Fstat(current, &named) != nil ||
		!sameBundleStat(&rootNode.stat, &original) || !sameBundleStat(&original, &named) {
		return Unknown, EvidenceUnavailable
	}
	for i, old := range chain {
		now := currentChain[i]
		if !sameBundleAncestor(&old, &now) {
			return Unknown, EvidenceUnavailable
		}
	}
	if contextEnded(ctx) {
		return Unknown, EvidenceCancelled
	}
	if contradiction {
		return Error, EvidenceFailed
	}
	return OK, EvidenceHealthy
}
