package doctor

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"golang.org/x/sys/unix"
)

const (
	maxHerdrSocketPath = 103 // common Linux/Darwin sockaddr bound, excluding NUL
	maxHerdrParents = 32
	maxHerdrFrame = 64 * 1024 // INCLUDING exactly one newline terminator
	herdrReadBuffer = 4096
	herdrPingID = "herdr-doctor-ping-v1"
	herdrPingRequest = "{\"id\":\"herdr-doctor-ping-v1\",\"method\":\"ping\",\"params\":{}}\n"
)

// Only a selected literal can supply a path. No environment/default discovery,
// abstract/TCP address, symlink resolution, truncation or authority override.
func validHerdrSocketPath(path string) bool {
	if len(path) == 0 || len(path) > maxHerdrSocketPath || !utf8.ValidString(path) ||
		!filepath.IsAbs(path) || filepath.Clean(path) != path || path == "/" { return false }
	if len(strings.Split(strings.TrimPrefix(path, "/"), "/")) > maxHerdrParents { return false }
	return strings.IndexFunc(path, unicode.IsControl) < 0
}

// Private owned fixtures only; production constructors leave this nil. No
// public executor, method/params selector, peer override or permission bypass.
type herdrHooks struct {
	event func(stage string, conn *net.UnixConn)
	fdEvent func(stage string, fd int)
	dial func(context.Context, string) (net.Conn, error)
}
func (h *herdrHooks) call(stage string, conn *net.UnixConn) { if h != nil && h.event != nil { h.event(stage, conn) } }
func (h *herdrHooks) fd(stage string, fd int) { if h != nil && h.fdEvent != nil { h.fdEvent(stage, fd) } }
func herdrClose(fd int, hooks *herdrHooks) bool { err := unix.Close(fd); hooks.fd("close", fd); return err == nil }

func (q *LocalQueries) observeHerdr(parent context.Context, target LocalTarget) (datum Datum) {
	if q == nil || !q.bound || target != q.target || absent(parent) { return localUnknown() }
	if contextEnded(parent) { return localDatum(Unknown, EvidenceCancelled) }
	if (target.mode != ModeCloudflare && target.mode != ModeGateway && target.mode != ModeBYO) ||
		!validHerdrSocketPath(target.herdrSocket) || !herdrPathSafetyAvailable() { return localUnknown() }
	ctx, cancel := context.WithTimeout(parent, CallBudget)
	defer func() { if contextEnded(ctx) { datum = localDatum(Unknown, EvidenceCancelled) }; cancel() }()
	state, ok := herdrPath(ctx, target.herdrSocket, q.herdrFixture)
	if !ok { return localUnknown() }
	defer func() { if !herdrClose(state.parent, q.herdrFixture) { datum = localUnknown() } }()
	q.herdrFixture.call("before-dial", nil)
	if !state.agrees(ctx, target.herdrSocket, q.herdrFixture) || contextEnded(ctx) { return localUnknown() }
	dialer := net.Dialer{Timeout: CallBudget, KeepAlive: -1}
	var conn net.Conn
	var err error
	if q.herdrFixture != nil && q.herdrFixture.dial != nil { conn, err = q.herdrFixture.dial(ctx, target.herdrSocket) } else {
		conn, err = dialer.DialContext(ctx, "unix", target.herdrSocket)
	}
	if !absent(conn) { defer func() { if conn.Close() != nil { datum = localUnknown() } }() }
	if err != nil || absent(conn) { return localUnknown() }
	unixConn, ok := conn.(*net.UnixConn)
	if !ok { return localUnknown() }
	deadline, bounded := ctx.Deadline()
	if !bounded || conn.SetDeadline(deadline) != nil { return localUnknown() }
	// An owned cancellation callback interrupts I/O, is stopped on completion,
	// and is JOINED if already running. No abandoned goroutine/read or retry.
	stopped := make(chan struct{})
	stop := context.AfterFunc(ctx, func() { defer close(stopped); _ = conn.SetDeadline(time.Now()) })
	defer func() { if !stop() { <-stopped } }()
	q.herdrFixture.call("connected", unixConn)
	if contextEnded(ctx) || !herdrPeer(unixConn) || !state.agrees(ctx, target.herdrSocket, q.herdrFixture) { return localUnknown() }
	q.herdrFixture.call("before-send", unixConn)
	if contextEnded(ctx) || !state.agrees(ctx, target.herdrSocket, q.herdrFixture) { return localUnknown() }
	evidence := herdrExchange(ctx, conn)
	if evidence == EvidenceHerdrPing { evidence = herdrBufferedEvidence(unixConn) }
	q.herdrFixture.call("before-final", unixConn)
	if !state.agrees(ctx, target.herdrSocket, q.herdrFixture) || contextEnded(ctx) { return localUnknown() }
	return localDatum(Unknown, evidence)
}

// Linux POSIX ACL named-user/group rights are bounded by the ACL mask in the
// group-class mode bits. The selected parent is owner-only, the socket denies
// foreign writes/connects, and ancestors deny foreign writers except trusted
// root-owned sticky temporary directories (kernel sticky deletion protection).
// Trust kernel/root/privileged actors; this is cooperative local attribution,
// NOT protection from malicious same-user servers or an atomic pathname connect.
// Darwin never reaches this policy: mode bits cannot establish Darwin ACL safety.
func herdrDirectory(st *unix.Stat_t, private bool) bool {
	uid := uint32(os.Geteuid())
	if st.Mode&unix.S_IFMT != unix.S_IFDIR || st.Mode&0o6000 != 0 || (st.Uid != 0 && st.Uid != uid) { return false }
	if private { return st.Uid == uid && st.Mode&0o0077 == 0 && st.Mode&0o0500 == 0o0500 && st.Mode&0o1000 == 0 }
	return st.Mode&0o0022 == 0 || (st.Uid == 0 && st.Mode&unix.S_ISVTX != 0)
}
func herdrSocket(st *unix.Stat_t) bool {
	return st.Mode&unix.S_IFMT == unix.S_IFSOCK && st.Uid == uint32(os.Geteuid()) && st.Nlink == 1 && st.Mode&0o7022 == 0 && st.Mode&0o0200 != 0
}
func herdrFilesystem(fd int) bool {
	var st unix.Statfs_t
	if unix.Fstatfs(fd, &st) != nil { return false }
	return herdrFilesystemType(uint64(st.Type))
}
func herdrFilesystemType(kind uint64) bool {
	// Local Linux POSIX permission/ACL-mask filesystems only. Remote/FUSE/rich
	// or unknown models (including unestablished overlay backing) are unavailable,
	// not inferred safe from uid/mode.
	switch kind { case 0xef53, 0x58465342, 0x9123683e, 0x01021994, 0x858458f6: return true }
	return false
}
func herdrIdentity(a, b *unix.Stat_t) bool { return a.Dev == b.Dev && a.Ino == b.Ino && a.Mode == b.Mode && a.Uid == b.Uid && a.Gid == b.Gid }
func herdrStable(a, b *unix.Stat_t) bool { return herdrIdentity(a, b) && a.Nlink == b.Nlink && a.Size == b.Size && a.Mtim == b.Mtim && a.Ctim == b.Ctim }
func herdrPeerUIDAllowed(uid uint32) bool { return uid == uint32(os.Geteuid()) }

type herdrPathState struct { parent int; socket unix.Stat_t; ancestors []unix.Stat_t }
// A finite no-follow walk holds only the current parent descriptor, never one
// fd per entry. Revalidation peaks at three metadata fds (held parent plus
// current/next walk directory), and one owned connection. No content files or
// adjacent directories are inventoried/read.
// Filesystem syscalls are cooperative limits, not an OS hard deadline promise.
func herdrPath(ctx context.Context, path string, hooks *herdrHooks) (*herdrPathState, bool) {
	if contextEnded(ctx) || !validHerdrSocketPath(path) || !herdrPathSafetyAvailable() { return nil, false }
	flags := unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_NONBLOCK|unix.O_CLOEXEC
	fd, err := unix.Open("/", flags, 0)
	if err != nil { return nil, false }; hooks.fd("open", fd)
	parts := []string{""}; dir := filepath.Dir(path)
	if dir != "/" { parts = append(parts, strings.Split(strings.TrimPrefix(dir, "/"), "/")...) }
	ancestors := []unix.Stat_t{}
	for i, part := range parts {
		if contextEnded(ctx) { herdrClose(fd, hooks); return nil, false }
		if i > 0 {
			next, err := unix.Openat(fd, part, flags, 0)
			closed := herdrClose(fd, hooks)
			if err != nil { return nil, false }; fd = next; hooks.fd("open", fd)
			if !closed { herdrClose(fd, hooks); return nil, false }
		}
		var st unix.Stat_t
		if unix.Fstat(fd, &st) != nil || !herdrDirectory(&st, i == len(parts)-1) || !herdrFilesystem(fd) { herdrClose(fd, hooks); return nil, false }
		ancestors = append(ancestors, st)
	}
	var socket unix.Stat_t
	if unix.Fstatat(fd, filepath.Base(path), &socket, unix.AT_SYMLINK_NOFOLLOW) != nil || !herdrSocket(&socket) || contextEnded(ctx) { herdrClose(fd, hooks); return nil, false }
	return &herdrPathState{fd, socket, ancestors}, true
}
func (s *herdrPathState) agrees(ctx context.Context, path string, hooks *herdrHooks) (ok bool) {
	if contextEnded(ctx) { return false }
	current, valid := herdrPath(ctx, path, hooks)
	if !valid { return false }
	defer func() { if !herdrClose(current.parent, hooks) { ok = false } }()
	if len(current.ancestors) != len(s.ancestors) || !herdrStable(&s.socket, &current.socket) { return false }
	for i := range s.ancestors { if !herdrIdentity(&s.ancestors[i], &current.ancestors[i]) { return false } }
	var parent, socket unix.Stat_t
	return !contextEnded(ctx) && unix.Fstat(s.parent, &parent) == nil && herdrStable(&s.ancestors[len(s.ancestors)-1], &parent) &&
		herdrStable(&parent, &current.ancestors[len(current.ancestors)-1]) &&
		unix.Fstatat(s.parent, filepath.Base(path), &socket, unix.AT_SYMLINK_NOFOLLOW) == nil && herdrStable(&s.socket, &socket)
}

// One exact fixed request, no second feature probe, no retry after partial
// writes. Acquire at most 64 KiB INCLUDING framing; filling that budget without
// a terminator is invalid, not a success prefix. Bytes
// already acquired AFTER the newline (even whitespace/another frame) are invalid.
// Do not wait for indefinite EOF, or claim absence of future unacquired frames.
func herdrExchange(ctx context.Context, conn net.Conn) Evidence {
	if contextEnded(ctx) { return EvidenceCancelled }
	written, err := conn.Write([]byte(herdrPingRequest))
	if err != nil || written != len(herdrPingRequest) { return EvidenceUnavailable }
	body := make([]byte, 0, maxHerdrFrame)
	buffer := make([]byte, herdrReadBuffer)
	for len(body) < maxHerdrFrame {
		if contextEnded(ctx) { return EvidenceCancelled }
		n, err := conn.Read(buffer[:min(len(buffer), maxHerdrFrame-len(body))])
		if err != nil && err != io.EOF { return EvidenceUnavailable }
		if n > 0 {
			body = append(body, buffer[:n]...)
			if len(body) > maxHerdrFrame { return EvidenceMalformed }
			if newline := bytes.IndexByte(buffer[:n], '\n'); newline >= 0 {
				if newline != n-1 || contextEnded(ctx) { return EvidenceMalformed }
				return herdrPong(body)
			}
		}
		if err == io.EOF { if len(body) == 0 { return EvidenceUnavailable }; return EvidenceMalformed }
		if err != nil || n == 0 { return EvidenceUnavailable }
	}
	return EvidenceMalformed
}

var herdrVersion = regexp.MustCompile(`^[0-9]{1,4}\.[0-9]{1,4}\.[0-9]{1,4}(-[0-9A-Za-z-]+(\.[0-9A-Za-z-]+)*)?(\+[0-9A-Za-z-]+(\.[0-9A-Za-z-]+)*)?$`)
func herdrPong(frame []byte) Evidence {
	if len(frame) == 0 || len(frame) > maxHerdrFrame || frame[len(frame)-1] != '\n' || bytes.Count(frame, []byte{'\n'}) != 1 || !herdrJSONEncoding(frame) { return EvidenceMalformed }
	// Genuine existing strict local projection: 64 KiB, depth 16, 128/object
	// members and array items, 2048 nodes/string bytes, duplicate keys at ALL
	// levels and complete EOF. Add a finite number-token bound below.
	object, ok := localObject(frame)
	if !ok || !herdrNumbers(object) { return EvidenceMalformed }
	id, ok := object["id"].(string)
	if !ok || id != herdrPingID { return EvidenceMalformed }
	resultValue, hasResult := object["result"]
	errorValue, hasError := object["error"]
	if hasResult == hasError { return EvidenceMalformed }
	if hasError {
		failure, ok := errorValue.(map[string]any)
		if !ok { return EvidenceMalformed }
		if _, ok := failure["code"].(string); !ok { return EvidenceMalformed }
		if _, ok := failure["message"].(string); !ok { return EvidenceMalformed }
		return EvidenceUnavailable // no arbitrary error text or fabricated failure verdict
	}
	result, ok := resultValue.(map[string]any)
	if !ok || result["type"] != "pong" { return EvidenceMalformed }
	version, ok := result["version"].(string)
	if !ok || len(version) == 0 || len(version) > 64 || !herdrVersion.MatchString(version) { return EvidenceMalformed }
	protocol, ok := localInteger(result["protocol"])
	if !ok || protocol <= 0 || protocol > 65535 { return EvidenceMalformed }
	// This is HERDR'S protocol, never relay protocol.Version or inferred
	// feature compatibility. Optional advertisements are typed, not authority.
	if raw, present := result["capabilities"]; present {
		capabilities, ok := raw.(map[string]any); if !ok { return EvidenceMalformed }
		for _, name := range []string{"surface_interest", "health_check"} {
			if value, present := capabilities[name]; present && value != nil { if _, ok := value.(bool); !ok { return EvidenceMalformed } }
		}
		if value, present := capabilities["endpoint_protocol_generation"]; present && value != nil {
			generation, ok := localInteger(value); if !ok || generation < 0 || generation > 65535 { return EvidenceMalformed }
		}
	}
	return EvidenceHerdrPing // no version, feature payload or identifier leaves this function
}
func herdrNumbers(value any) bool {
	switch v := value.(type) {
	case json.Number: return len(v) <= 128
	case map[string]any: for _, item := range v { if !herdrNumbers(item) { return false } }
	case []any: for _, item := range v { if !herdrNumbers(item) { return false } }
	}
	return true
}
// Reject invalid literal UTF-8 AND unpaired escaped UTF-16, which encoding/json
// otherwise silently replaces. Bounded input; no retained secondary raw schema.
func herdrJSONEncoding(data []byte) bool {
	if !utf8.Valid(data) { return false }
	quoted := false
	for i := 0; i < len(data); i++ {
		if data[i] == '"' { quoted = !quoted; continue }
		if !quoted || data[i] != '\\' { continue }
		if i+1 >= len(data) { return false }
		if data[i+1] != 'u' { i++; continue }
		if i+5 >= len(data) { return false }
		code, ok := herdrHex4(data[i+2:i+6]); if !ok { return false }
		if code >= 0xdc00 && code <= 0xdfff { return false }
		if code >= 0xd800 && code <= 0xdbff {
			if i+11 >= len(data) || data[i+6] != '\\' || data[i+7] != 'u' { return false }
			low, ok := herdrHex4(data[i+8:i+12]); if !ok || low < 0xdc00 || low > 0xdfff { return false }; i += 11
		} else { i += 5 }
	}
	return true
}
func herdrHex4(data []byte) (uint16, bool) {
	var value uint16
	for _, c := range data {
		value <<= 4
		switch { case c >= '0' && c <= '9': value |= uint16(c-'0'); case c >= 'a' && c <= 'f': value |= uint16(c-'a'+10); case c >= 'A' && c <= 'F': value |= uint16(c-'A'+10); default: return 0, false }
	}
	return value, true
}
