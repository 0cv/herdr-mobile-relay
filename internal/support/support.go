package support

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"golang.org/x/sys/unix"
)

type Snapshot struct {
	GeneratedAt      string            `json:"generated_at"`
	Version          string            `json:"version"`
	Revision         string            `json:"revision"`
	Protocol         int               `json:"protocol"`
	ReleaseDirectory string            `json:"release_directory,omitempty"`
	WebHash          string            `json:"web_hash,omitempty"`
	WebVersion       string            `json:"web_version,omitempty"`
	Readiness        string            `json:"readiness"`
	Inventory        map[string]any    `json:"inventory"`
	Components       map[string]string `json:"components"`
	Scheduler        any               `json:"scheduler,omitempty"`
	Transport        any               `json:"transport,omitempty"`
	UDP              any               `json:"udp,omitempty"`
	ActivityFailures uint64            `json:"activity_failures"`
	TopologyRetries  uint64            `json:"topology_retries"`
	PollFailures     int               `json:"poll_failures"`
	RecentErrors     []string          `json:"recent_errors"`
}

// PublicSupport is a finite historical projection, never current health,
// process ownership, installed-package identity, or admission evidence. Only
// these typed fields may be serialized by the support command.
type PublicSupport struct {
	Schema            string  `json:"schema"`
	Source            string  `json:"source"`
	Meaning           string  `json:"meaning"`
	Protocol          *int    `json:"protocol,omitempty"`
	RecordedReadiness string  `json:"recorded_readiness"`
	RecordedInventory string  `json:"recorded_inventory"`
	ActivityFailures  *uint64 `json:"activity_failures,omitempty"`
	TopologyRetries   *uint64 `json:"topology_retries,omitempty"`
	PollFailures      *int    `json:"poll_failures,omitempty"`
	RecentErrorCount  *int    `json:"recent_error_count,omitempty"`
}

// PublicProjection does not traverse or encode opaque snapshot fields. A
// missing/invalid protocol cannot turn absent numeric data into asserted zeros.
// Counts beyond the exact JSON integer range are unknown, not rounded.
func PublicProjection(s Snapshot) PublicSupport {
	p := PublicSupport{
		Schema: "support-public-v1", Source: "persisted_snapshot",
		Meaning:           "historical_only_not_current_health_or_admission",
		RecordedReadiness: "unknown", RecordedInventory: "unknown",
	}
	if s.Protocol != 3 {
		return p
	}
	p.Protocol = &s.Protocol
	switch s.Readiness {
	case "starting", "ready":
		p.RecordedReadiness = s.Readiness
	}
	if state, ok := s.Inventory["state"].(string); ok {
		switch state {
		case "starting", "ready", "error":
			p.RecordedInventory = state
		}
	}
	const maxExactCount = 1<<53 - 1
	if s.ActivityFailures <= maxExactCount {
		p.ActivityFailures = &s.ActivityFailures
	}
	if s.TopologyRetries <= maxExactCount {
		p.TopologyRetries = &s.TopologyRetries
	}
	if s.PollFailures >= 0 && uint64(s.PollFailures) <= maxExactCount {
		p.PollFailures = &s.PollFailures
	}
	if s.RecentErrors != nil && len(s.RecentErrors) <= 256 {
		count := len(s.RecentErrors)
		p.RecentErrorCount = &count
	}
	return p
}

func Path(runtimeDir string) string {
	return filepath.Join(runtimeDir, "support-state.json")
}

func Write(runtimeDir string, snapshot Snapshot) error {
	if err := os.MkdirAll(runtimeDir, 0o700); err != nil {
		return err
	}
	snapshot.GeneratedAt = time.Now().UTC().Format(time.RFC3339)
	data, err := json.MarshalIndent(snapshot, "", "  ")
	if err != nil {
		return err
	}
	path := Path(runtimeDir)
	temp, err := os.CreateTemp(runtimeDir, ".support-state.*.tmp")
	if err != nil {
		return err
	}
	tempPath := temp.Name()
	defer os.Remove(tempPath)
	if err := temp.Chmod(0o600); err != nil {
		_ = temp.Close()
		return err
	}
	written, writeErr := temp.Write(data)
	if writeErr == nil && written != len(data) {
		writeErr = io.ErrShortWrite
	}
	if writeErr == nil {
		writeErr = temp.Sync()
	}
	closeErr := temp.Close()
	if writeErr != nil {
		return writeErr
	}
	if closeErr != nil {
		return closeErr
	}
	if err := os.Rename(tempPath, path); err != nil {
		return err
	}
	if directory, err := os.Open(runtimeDir); err == nil {
		_ = directory.Sync()
		_ = directory.Close()
	}
	return nil
}

// Load acquires only selected persisted historical support data. Returned
// Snapshot fields (especially operational metadata/RecentErrors) remain private,
// untrusted and NOT redacted or current health/admission authority. Public
// callers must use PublicProjection, never serialize the private Snapshot.
// Relative/unclean paths and unsupported permission models are unavailable or
// refused, never normalized into another selected root or repaired.
func Load(runtimeDir string) (Snapshot, error) {
	return loadSnapshot(runtimeDir, nil)
}

// Normal producer snapshots contain six aggregate inventory fields, 12 scheduler
// metrics, bounded transport counters, 3 UDP metrics and up to 20 recent errors
// (normally with at most 300-rune details). These are
// conservative compatibility budgets, not permission to truncate larger future
// snapshots. A complete <=256-KiB input is required before expansive decoding.
// JSON budgets cover the ENTIRE tree, including skipped additive fields: depth
// includes the root/value level; members are global plus per-object; arrays have
// per-array counts; strings/keys use decoded UTF-8 bytes, numbers token bytes.
const (
	maxSupportBytes   = 256 * 1024
	supportReadChunk  = 32 * 1024
	maxSupportPath    = 4096
	maxSupportParents = 64
	maxSupportDepth   = 16
	maxSupportMembers = 4096
	maxSupportObject  = 512
	maxSupportArray   = 256
	maxSupportNodes   = 8192
	maxSupportString  = 8192
	maxSupportKey     = 128
	maxSupportNumber  = 128
)

// Finite Load matrix: absent/inaccessible I/O, unsupported permission model or
// any acquisition/JSON budget => unavailable; unsafe selection/ownership/type
// or changed evidence => refused; malformed JSON/known types/timestamp => invalid.
// Every error returns a zero Snapshot and no raw cause. These are acquisition
// outcomes, not current-health states or a redaction of successful private data.
var (
	errSupportUnavailable = errors.New("support snapshot unavailable")
	errSupportRefused     = errors.New("support snapshot refused")
	errSupportInvalid     = errors.New("invalid support snapshot")
)

// Private deterministic fixture seam; production passes nil. No executor,
// filesystem override, permission-policy bypass, retry or abandoned goroutine.
type supportLoadHooks struct {
	event func(stage string, file *os.File)
}

func (h *supportLoadHooks) call(stage string, file *os.File) {
	if h != nil && h.event != nil {
		h.event(stage, file)
	}
}
func loadSnapshot(dir string, hooks *supportLoadHooks) (Snapshot, error) {
	// Darwin ACLs can grant access absent from mode bits. No bounded native
	// descriptor-safe ACL inspector is implemented here: refuse BEFORE reads.
	if runtime.GOOS != "linux" {
		return Snapshot{}, errSupportUnavailable
	}
	if !supportSelectedPath(dir) {
		return Snapshot{}, errSupportRefused
	}
	data, err := readSupport(dir, hooks)
	if err != nil {
		return Snapshot{}, err
	}
	return decodeSupport(data)
}
func supportSelectedPath(dir string) bool {
	if len(dir) == 0 || len(dir) > maxSupportPath || !utf8.ValidString(dir) || !filepath.IsAbs(dir) || filepath.Clean(dir) != dir || dir == "/" {
		return false
	}
	parts := strings.Split(strings.TrimPrefix(dir, "/"), "/")
	if len(parts) > maxSupportParents {
		return false
	}
	for _, part := range parts {
		if len(part) == 0 || len(part) > 255 {
			return false
		}
		for _, r := range part {
			if unicode.IsControl(r) {
				return false
			}
		}
	}
	return true
}
func supportIOError(err error) error {
	if errors.Is(err, unix.ELOOP) || errors.Is(err, unix.ENOTDIR) {
		return errSupportRefused
	}
	return errSupportUnavailable
}

// Linux POSIX access ACL named-user/group permissions are limited by the ACL
// mask, reflected in stat's group-class bits; other permissions are reflected
// in other-class bits. Private 0700/0500 directories and 0600/0400 files therefore
// deny foreign access even if an ineffective named ACL entry exists. Ancestors
// deny foreign writes, except trusted-root sticky temporary ancestors: kernel
// sticky deletion rules protect the owned selected directory. Trust kernel/root;
// privileged attackers and hostile same-owner edit/restore are NOT excluded.
// Do not assume these rules on arbitrary remote/FUSE/rich-ACL filesystems. Only
// the following local Linux POSIX filesystem types are admitted; all others are
// unavailable. This is a private finite reader policy, not a platform adapter.
func supportFilesystem(fd int) bool {
	var st unix.Statfs_t
	if unix.Fstatfs(fd, &st) != nil {
		return false
	}
	return supportFilesystemType(uint64(st.Type))
}
func supportFilesystemType(kind uint64) bool {
	switch kind {
	case 0xef53, 0x58465342, 0x9123683e, 0x01021994, 0x858458f6, 0x794c7630: // ext, XFS, Btrfs, tmpfs, ramfs, overlay
		return true
	}
	return false
}
func supportDirectory(st *unix.Stat_t, private bool) bool {
	uid := uint32(os.Geteuid())
	if st.Mode&unix.S_IFMT != unix.S_IFDIR || st.Mode&0o6000 != 0 || (st.Uid != 0 && st.Uid != uid) {
		return false
	}
	if private {
		return st.Uid == uid && st.Mode&0o0077 == 0 && st.Mode&0o0500 == 0o0500 && st.Mode&0o1000 == 0
	}
	return st.Mode&0o0022 == 0 || (st.Uid == 0 && st.Mode&unix.S_ISVTX != 0)
}
func supportRegular(st *unix.Stat_t) bool {
	return st.Mode&unix.S_IFMT == unix.S_IFREG && st.Uid == uint32(os.Geteuid()) && st.Nlink == 1 && st.Mode&0o7077 == 0 && st.Mode&0o0400 != 0 && st.Size >= 0
}
func supportIdentity(a, b *unix.Stat_t) bool {
	return a.Dev == b.Dev && a.Ino == b.Ino && a.Mode == b.Mode && a.Uid == b.Uid && a.Gid == b.Gid
}
func supportStable(a, b *unix.Stat_t) bool {
	return supportIdentity(a, b) && a.Nlink == b.Nlink && a.Size == b.Size && a.Mtim == b.Mtim && a.Ctim == b.Ctim
}

// Keep only the current directory fd, not one fd per ancestor. The finite stat
// records bind every named parent during the final no-follow walk. Ancestor
// siblings may change without invalidating identity/permission checks; changes
// in the private selected directory itself must invalidate acquisition.
func supportParent(dir string) (int, []unix.Stat_t, error) {
	flags := unix.O_RDONLY | unix.O_DIRECTORY | unix.O_NOFOLLOW | unix.O_NONBLOCK | unix.O_CLOEXEC
	fd, err := unix.Open("/", flags, 0)
	if err != nil {
		return -1, nil, supportIOError(err)
	}
	records := []unix.Stat_t{}
	parts := append([]string{""}, strings.Split(strings.TrimPrefix(dir, "/"), "/")...)
	for i, part := range parts {
		if i != 0 {
			next, err := unix.Openat(fd, part, flags, 0)
			closeErr := unix.Close(fd)
			if err != nil {
				return -1, nil, supportIOError(err)
			}
			fd = next
			if closeErr != nil {
				_ = unix.Close(fd)
				return -1, nil, errSupportUnavailable
			}
		}
		var st unix.Stat_t
		if unix.Fstat(fd, &st) != nil {
			_ = unix.Close(fd)
			return -1, nil, errSupportUnavailable
		}
		if !supportDirectory(&st, i == len(parts)-1) {
			_ = unix.Close(fd)
			return -1, nil, errSupportRefused
		}
		if !supportFilesystem(fd) {
			_ = unix.Close(fd)
			return -1, nil, errSupportUnavailable
		}
		records = append(records, st)
	}
	return fd, records, nil
}
func readSupport(dir string, hooks *supportLoadHooks) (data []byte, result error) {
	parent, ancestors, err := supportParent(dir)
	if err != nil {
		return nil, err
	}
	defer func() {
		if unix.Close(parent) != nil {
			data, result = nil, errSupportUnavailable
		}
	}()
	const base = "support-state.json"
	var named unix.Stat_t
	if err := unix.Fstatat(parent, base, &named, unix.AT_SYMLINK_NOFOLLOW); err != nil {
		return nil, supportIOError(err)
	}
	if !supportRegular(&named) {
		return nil, errSupportRefused
	}
	if named.Size > maxSupportBytes {
		return nil, errSupportUnavailable
	}
	hooks.call("before-open", nil)
	fd, err := unix.Openat(parent, base, unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_NONBLOCK|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, supportIOError(err)
	}
	file := os.NewFile(uintptr(fd), "private support input")
	if file == nil {
		_ = unix.Close(fd)
		return nil, errSupportUnavailable
	}
	defer func() {
		if file.Close() != nil {
			data, result = nil, errSupportUnavailable
		}
	}()
	var opened unix.Stat_t
	if unix.Fstat(fd, &opened) != nil || !supportRegular(&opened) || !supportStable(&named, &opened) {
		return nil, errSupportRefused
	}
	if !supportFilesystem(fd) {
		return nil, errSupportUnavailable
	}
	hooks.call("open", file)
	var buffer bytes.Buffer
	chunk := make([]byte, supportReadChunk)
	remaining := maxSupportBytes + 1
	for remaining > 0 {
		n, err := file.Read(chunk[:min(remaining, len(chunk))])
		remaining -= n
		buffer.Write(chunk[:n])
		hooks.call("read", file)
		if buffer.Len() > maxSupportBytes {
			return nil, errSupportUnavailable
		}
		if err == io.EOF {
			break
		}
		if err != nil || n == 0 {
			return nil, errSupportUnavailable
		}
	}
	if int64(buffer.Len()) != opened.Size {
		return nil, errSupportRefused
	}
	hooks.call("before-final", file)
	var after, currentNamed, currentParent unix.Stat_t
	if unix.Fstat(fd, &after) != nil || !supportStable(&opened, &after) || !supportRegular(&after) ||
		unix.Fstat(parent, &currentParent) != nil || !supportStable(&ancestors[len(ancestors)-1], &currentParent) ||
		unix.Fstatat(parent, base, &currentNamed, unix.AT_SYMLINK_NOFOLLOW) != nil || !supportStable(&opened, &currentNamed) {
		return nil, errSupportRefused
	}
	current, finalAncestors, err := supportParent(dir)
	if err != nil {
		return nil, err
	}
	defer func() {
		if unix.Close(current) != nil {
			data, result = nil, errSupportUnavailable
		}
	}()
	if len(ancestors) != len(finalAncestors) {
		return nil, errSupportRefused
	}
	for i := range ancestors {
		if !supportIdentity(&ancestors[i], &finalAncestors[i]) {
			return nil, errSupportRefused
		}
	}
	if !supportStable(&currentParent, &finalAncestors[len(finalAncestors)-1]) ||
		unix.Fstatat(current, base, &currentNamed, unix.AT_SYMLINK_NOFOLLOW) != nil || !supportStable(&opened, &currentNamed) {
		return nil, errSupportRefused
	}
	// Cooperative consistency only: no OS hard deadline or atomic hostile
	// snapshot guarantee. No successful result survives descriptor close errors.
	return buffer.Bytes(), nil
}

type supportJSONBudget struct {
	nodes, members int
	limited        bool
}

func (b *supportJSONBudget) limit() bool { b.limited = true; return false }
func (b *supportJSONBudget) value(dec *json.Decoder, token json.Token, depth int) bool {
	b.nodes++
	if depth > maxSupportDepth || b.nodes > maxSupportNodes {
		return b.limit()
	}
	switch v := token.(type) {
	case json.Delim:
		switch v {
		case '{':
			seen := map[string]bool{}
			for dec.More() {
				token, err := dec.Token()
				if err != nil {
					return false
				}
				key, ok := token.(string)
				b.members++
				if !ok || seen[key] {
					return false
				}
				if len(key) > maxSupportKey || len(seen) >= maxSupportObject || b.members > maxSupportMembers {
					return b.limit()
				}
				seen[key] = true
				token, err = dec.Token()
				if err != nil || !b.value(dec, token, depth+1) {
					return false
				}
			}
			token, err := dec.Token()
			return err == nil && token == json.Delim('}')
		case '[':
			count := 0
			for dec.More() {
				count++
				if count > maxSupportArray {
					return b.limit()
				}
				token, err := dec.Token()
				if err != nil || !b.value(dec, token, depth+1) {
					return false
				}
			}
			token, err := dec.Token()
			return err == nil && token == json.Delim(']')
		default:
			return false
		}
	case string:
		if len(v) > maxSupportString {
			return b.limit()
		}
		return true
	case json.Number:
		if len(v) > maxSupportNumber {
			return b.limit()
		}
		return true
	case nil, bool:
		return true
	default:
		return false
	}
}

// encoding/json replaces unpaired UTF-16 escapes with U+FFFD. A strict reader
// must not silently change input strings/keys; check escape pairs as well as
// literal UTF-8 before decoding. Grammar is still validated by Decoder below.
func supportStringEncoding(data []byte) bool {
	if !utf8.Valid(data) {
		return false
	}
	quoted := false
	for i := 0; i < len(data); i++ {
		if data[i] == '"' {
			quoted = !quoted
			continue
		}
		if !quoted || data[i] != '\\' {
			continue
		}
		if i+1 >= len(data) {
			return false
		}
		if data[i+1] != 'u' {
			i++
			continue
		}
		if i+5 >= len(data) {
			return false
		}
		code, ok := supportHex4(data[i+2 : i+6])
		if !ok {
			return false
		}
		if code >= 0xdc00 && code <= 0xdfff {
			return false
		}
		if code >= 0xd800 && code <= 0xdbff {
			if i+11 >= len(data) || data[i+6] != '\\' || data[i+7] != 'u' {
				return false
			}
			next, ok := supportHex4(data[i+8 : i+12])
			if !ok || next < 0xdc00 || next > 0xdfff {
				return false
			}
			i += 11
		} else {
			i += 5
		}
	}
	return true
}
func supportHex4(data []byte) (uint16, bool) {
	var value uint16
	for _, c := range data {
		value <<= 4
		switch {
		case c >= '0' && c <= '9':
			value |= uint16(c - '0')
		case c >= 'a' && c <= 'f':
			value |= uint16(c - 'a' + 10)
		case c >= 'A' && c <= 'F':
			value |= uint16(c - 'A' + 10)
		default:
			return 0, false
		}
	}
	return value, true
}
func decodeSupport(data []byte) (Snapshot, error) {
	if len(data) > maxSupportBytes {
		return Snapshot{}, errSupportUnavailable
	}
	if !supportStringEncoding(data) {
		return Snapshot{}, errSupportInvalid
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	first, err := dec.Token()
	budget := supportJSONBudget{}
	if err != nil || first != json.Delim('{') {
		return Snapshot{}, errSupportInvalid
	}
	if !budget.value(dec, first, 1) {
		if budget.limited {
			return Snapshot{}, errSupportUnavailable
		}
		return Snapshot{}, errSupportInvalid
	}
	if _, err := dec.Token(); err != io.EOF {
		return Snapshot{}, errSupportInvalid
	}
	var raw map[string]json.RawMessage
	if json.Unmarshal(data, &raw) != nil {
		return Snapshot{}, errSupportInvalid
	}
	var snapshot Snapshot
	// Project exact known keys only. Unmarshal directly into Snapshot would let
	// case-folded additive aliases overwrite exact validated keys. Unknown keys
	// were bounded/validated above and are safely discarded, never reserialized.
	fields := []struct {
		name     string
		value    any
		kind     byte
		required bool
	}{
		{"generated_at", &snapshot.GeneratedAt, 's', true}, {"version", &snapshot.Version, 's', true},
		{"revision", &snapshot.Revision, 's', true}, {"protocol", &snapshot.Protocol, 'n', true},
		{"release_directory", &snapshot.ReleaseDirectory, 's', false}, {"web_hash", &snapshot.WebHash, 's', false},
		{"web_version", &snapshot.WebVersion, 's', false}, {"readiness", &snapshot.Readiness, 's', true},
		{"inventory", &snapshot.Inventory, 'o', true}, {"components", &snapshot.Components, 'o', true},
		{"scheduler", &snapshot.Scheduler, 'x', false}, {"transport", &snapshot.Transport, 'x', false},
		{"udp", &snapshot.UDP, 'x', false}, {"activity_failures", &snapshot.ActivityFailures, 'n', true},
		{"topology_retries", &snapshot.TopologyRetries, 'n', true}, {"poll_failures", &snapshot.PollFailures, 'n', true},
		{"recent_errors", &snapshot.RecentErrors, 'a', true},
	}
	for _, field := range fields {
		value, present := raw[field.name]
		if !present {
			if field.required {
				return Snapshot{}, errSupportInvalid
			}
			continue
		}
		value = bytes.TrimSpace(value)
		isNull := bytes.Equal(value, []byte("null"))
		if len(value) == 0 || ((field.kind == 's' || field.kind == 'n') && isNull) ||
			(field.kind == 's' && value[0] != '"') || (field.kind == 'o' && !isNull && value[0] != '{') ||
			(field.kind == 'a' && !isNull && value[0] != '[') || json.Unmarshal(value, field.value) != nil {
			return Snapshot{}, errSupportInvalid
		}
		// encoding/json accepts null into string map/slice elements; these are
		// known strings, not optional opaque metrics, so reject that coercion.
		if field.name == "components" && !isNull {
			var entries map[string]json.RawMessage
			if json.Unmarshal(value, &entries) != nil {
				return Snapshot{}, errSupportInvalid
			}
			for _, item := range entries {
				item = bytes.TrimSpace(item)
				if len(item) == 0 || item[0] != '"' {
					return Snapshot{}, errSupportInvalid
				}
			}
		}
		if field.name == "recent_errors" && !isNull {
			var entries []json.RawMessage
			if json.Unmarshal(value, &entries) != nil {
				return Snapshot{}, errSupportInvalid
			}
			for _, item := range entries {
				item = bytes.TrimSpace(item)
				if len(item) == 0 || item[0] != '"' {
					return Snapshot{}, errSupportInvalid
				}
			}
		}
	}
	if _, err := time.Parse(time.RFC3339Nano, snapshot.GeneratedAt); err != nil {
		return Snapshot{}, errSupportInvalid
	}
	// Preserve the exact persisted timestamp, never refresh or relabel it.
	return snapshot, nil
}
