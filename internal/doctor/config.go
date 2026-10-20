// Package doctor implements read-only diagnostic policy, not startup authority.
package doctor

import (
	"encoding/binary"
	"errors"
	"io"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/0cv/herdr-mobile-relay/internal/setuphelper"
	"golang.org/x/sys/unix"
)

const (
	MaxConfigBytes = 64 * 1024
	MaxConfigLine  = 4096
	MaxConfigValue = 2048
	MaxConfigKeys  = 128
	MaxGateways    = 8
)

type Mode string

const (
	ModeUnknown    Mode = "unknown"
	ModeCloudflare Mode = "cloudflare"
	ModeGateway    Mode = "gateway"
	ModeManaged    Mode = "tailscale"
	ModeBYO        Mode = "tailscale-external"
	ModeCLI        Mode = "tailscale-cli"
)

type ConfigCode string

const (
	ConfigValid       ConfigCode = "config_valid"
	ConfigInvalid     ConfigCode = "config_invalid"
	ConfigUnavailable ConfigCode = "config_unavailable"
	ConfigUnsafe      ConfigCode = "config_unsafe"
)

// Inspection deliberately contains no token, unknown key values or ambient
// configuration. Expected instance and routing are private input to adapters,
// never a report. CLI recognition grants no inspection/startup authority.
type Inspection struct {
	code         ConfigCode
	mode         Mode
	tokenPresent bool
	tokenValid   bool
	port         int
	pluginPort   int
	gateways     []string
	appOrigin    string
	relayOrigin  string
	instance     string
	herdrSocket  string // only validated explicit literal selection; never ambient discovery
}

func (c Inspection) Code() ConfigCode {
	switch c.code {
	case ConfigValid, ConfigInvalid, ConfigUnavailable, ConfigUnsafe:
		return c.code
	default:
		return ConfigUnavailable
	}
}
func (c Inspection) Mode() Mode {
	if c.Code() != ConfigValid {
		return ModeUnknown
	}
	switch c.mode {
	case ModeCloudflare, ModeGateway, ModeManaged, ModeBYO, ModeCLI:
		return c.mode
	default:
		return ModeUnknown
	}
}
func (Inspection) String() string   { return "private diagnostic configuration" }
func (Inspection) GoString() string { return "private diagnostic configuration" }
func (Inspection) MarshalJSON() ([]byte, error) {
	return nil, errors.New("diagnostic configuration is not serializable")
}

func refusedConfig(code ConfigCode) Inspection {
	return Inspection{code: code, mode: ModeUnknown}
}

// ReadConfig reads only the explicitly selected file. No HOME/env merge,
// directory creation, chmod, sourcing, token-file dereference or store opening.
// Directory traversal is no-follow, and special files are refused before read.
// On Darwin, mode bits alone are insufficient: the opened descriptors' ACL
// evidence must prove that nothing but the owner and root can write the file or
// any directory above it, before and after the read. OS file reads can still
// stall on a noncooperative filesystem; this is a byte and scheduling bound,
// not a hard filesystem wall-clock bound.
func ReadConfig(path string) Inspection {
	return readConfig(path, os.Geteuid(), func(r io.Reader) ([]byte, error) {
		return io.ReadAll(io.LimitReader(r, MaxConfigBytes+1))
	})
}

func openParent(path string) (int, string, error) { return openParentWalk(path, nil) }

// openParentWalk is openParent plus an optional inspection of every directory
// descriptor on the no-follow walk (the root, each ancestor and the parent),
// made on the opened descriptor before the walk descends further.
func openParentWalk(path string, visit func(int) error) (int, string, error) {
	if !filepath.IsAbs(path) || filepath.Clean(path) != path || len(path) > 4096 {
		return -1, "", errors.New("invalid selection")
	}
	parts := strings.Split(strings.TrimPrefix(path, "/"), "/")
	if len(parts) < 1 || len(parts) > 64 || parts[len(parts)-1] == "" {
		return -1, "", errors.New("invalid selection")
	}
	flags := unix.O_RDONLY | unix.O_DIRECTORY | unix.O_NOFOLLOW | unix.O_CLOEXEC | unix.O_NONBLOCK
	fd, err := unix.Open("/", flags, 0)
	if err != nil {
		return -1, "", err
	}
	if visit != nil {
		if err := visit(fd); err != nil {
			_ = unix.Close(fd)
			return -1, "", err
		}
	}
	for _, component := range parts[:len(parts)-1] {
		next, err := unix.Openat(fd, component, flags, 0)
		_ = unix.Close(fd)
		if err != nil {
			return -1, "", err
		}
		fd = next
		if visit != nil {
			if err := visit(fd); err != nil {
				_ = unix.Close(fd)
				return -1, "", err
			}
		}
	}
	return fd, parts[len(parts)-1], nil
}

func privateRegular(st *unix.Stat_t, uid int) bool {
	return st.Mode&unix.S_IFMT == unix.S_IFREG && int(st.Uid) == uid &&
		st.Mode&0o077 == 0 && st.Mode&0o7000 == 0 && st.Mode&0o400 != 0 && st.Nlink == 1
}

// Darwin descriptor ACL evidence. The layouts come from the public XNU headers
// and were confirmed against the bytes a real macOS kernel returns:
//   - sys/attr.h: fgetattrlist(2) with ATTR_CMN_RETURNED_ATTRS and
//     ATTR_CMN_EXTENDED_SECURITY returns a u32 length, a 20-byte attribute_set_t
//     and, only when the object has an ACL, an attrreference (int32 offset from
//     the reference, u32 length) to a kauth_filesec;
//   - sys/kauth.h: kauth_filesec is the magic, owner and group GUIDs, entrycount
//     and flags (44 bytes) followed by entrycount 24-byte kauth_ace (GUID, flags,
//     rights).
//
// An object without an ACL has the security attribute omitted from the returned
// set; that proves "no ACL" only on a volume that advertises
// VOL_CAP_INT_EXTENDED_SECURITY. The NOACL entrycount sentinel was never
// observed on this route and stays unverifiable. A zero-entry ACL (what
// chmod -a leaves behind) is refused: sys/kauth.h notes that Windows reads an
// empty ACL as "deny all", so its meaning is not assumed.
const (
	darwinReturnedAttrs    = 0x80000000 // ATTR_CMN_RETURNED_ATTRS
	darwinExtendedSecurity = 0x00400000 // ATTR_CMN_EXTENDED_SECURITY
	darwinFilesecMagic     = 0x012cc16d // KAUTH_FILESEC_MAGIC
	darwinFilesecHeader    = 44
	darwinACESize          = 24
	darwinMaxACEs          = 128   // KAUTH_ACL_MAX_ENTRIES
	darwinACEKindMask      = 0xf   // KAUTH_ACE_KINDMASK
	darwinACEDeny          = 2     // KAUTH_ACE_DENY
	darwinACEInherit       = 0x1f0 // KAUTH_ACE_INHERITED through KAUTH_ACE_ONLY_INHERIT
	darwinACLBuffer        = 4096  // length, returned set, reference and 128 entries
	darwinVolumeLength     = 36    // u32 length, vol_capabilities_attr_t
	darwinVolumeInterfaces = 8     // capabilities[VOL_CAPABILITIES_INTERFACES]
	darwinVolumeValid      = 24    // valid[VOL_CAPABILITIES_INTERFACES]
	darwinVolumeACLBit     = 0x400 // VOL_CAP_INT_EXTENDED_SECURITY
)

// darwinACL keeps nothing but counts: never a GUID, a right or an identity.
type darwinACL struct {
	absent   bool // the kernel omitted the security attribute
	denyOnly bool // 1..128 entries, every one a DENY entry, no unknown flags
	entries  int
	denies   int
}

// A regular file must have no ACL at all. A directory may also carry a fully
// parsed deny-only ACL: deny entries remove rights and can grant none.
func (a darwinACL) fileOK() bool      { return a.absent }
func (a darwinACL) directoryOK() bool { return a.absent || a.denyOnly }

func parseDarwinSecurity(buf []byte) (darwinACL, error) {
	bad := errors.New("unverifiable descriptor ACL") // never includes private metadata
	if len(buf) < 32 {
		return darwinACL{}, bad
	}
	length := int(binary.NativeEndian.Uint32(buf))
	if length < 24 || length > len(buf) { // FSOPT_REPORT_FULLSIZE reports the untruncated length
		return darwinACL{}, bad
	}
	for i := 8; i < 24; i += 4 { // volume, directory, file and fork groups were not requested
		if binary.NativeEndian.Uint32(buf[i:]) != 0 {
			return darwinACL{}, bad
		}
	}
	switch binary.NativeEndian.Uint32(buf[4:]) {
	case darwinReturnedAttrs: // no security object: nothing, or an all-zero reference, follows
		if (length != 24 && length != 32) ||
			(length == 32 && (binary.NativeEndian.Uint32(buf[24:]) != 0 || binary.NativeEndian.Uint32(buf[28:]) != 0)) {
			return darwinACL{}, bad
		}
		return darwinACL{absent: true}, nil
	case darwinReturnedAttrs | darwinExtendedSecurity:
	default:
		return darwinACL{}, bad
	}
	// The reference is relative to itself; every real answer puts the object
	// right behind it (offset 8), so any other offset is noncanonical.
	offset := int(int32(binary.NativeEndian.Uint32(buf[24:])))
	refLen := int(binary.NativeEndian.Uint32(buf[28:]))
	start := 24 + offset
	if offset != 8 || refLen < darwinFilesecHeader ||
		(refLen-darwinFilesecHeader)%darwinACESize != 0 || start+refLen != length {
		return darwinACL{}, bad
	}
	entries := (refLen - darwinFilesecHeader) / darwinACESize
	sec := buf[start:length]
	// The stored count must equal the size-derived count, so NOACL never matches.
	if entries > darwinMaxACEs || binary.NativeEndian.Uint32(sec) != darwinFilesecMagic ||
		int(binary.NativeEndian.Uint32(sec[36:])) != entries {
		return darwinACL{}, bad
	}
	acl := darwinACL{entries: entries, denyOnly: entries > 0 && binary.NativeEndian.Uint32(sec[40:]) == 0}
	for i := 0; i < entries; i++ {
		flags := binary.NativeEndian.Uint32(sec[darwinFilesecHeader+darwinACESize*i+16:])
		if flags&darwinACEKindMask == darwinACEDeny {
			acl.denies++
		}
		if flags&darwinACEKindMask != darwinACEDeny || flags&^(darwinACEKindMask|darwinACEInherit) != 0 {
			acl.denyOnly = false // permit, audit, alarm, unknown kinds or flags
		}
	}
	return acl, nil
}

// darwinVolumeHasACLs reads ATTR_VOL_CAPABILITIES: the volume must advertise,
// as valid, that it implements extended security (ACLs).
func darwinVolumeHasACLs(buf []byte) bool {
	return len(buf) >= darwinVolumeLength && binary.NativeEndian.Uint32(buf) == darwinVolumeLength &&
		binary.NativeEndian.Uint32(buf[darwinVolumeInterfaces:])&binary.NativeEndian.Uint32(buf[darwinVolumeValid:])&darwinVolumeACLBit != 0
}

// Package-metadata acquisition (package_identity.go) still has no ACL evidence,
// so it keeps refusing every Darwin host. Only the selected configuration
// reader has a descriptor ACL route (configHostCannotProveACL).
func earlyReaderHostRefusesModeOnlyAcquisition(goos string) bool { return goos == "darwin" }

// A simulated Darwin host cannot supply real descriptor ACL evidence.
func configHostCannotProveACL(goos string) bool { return goos == "darwin" && runtime.GOOS != "darwin" }

var (
	errAncestorUnsafe  = errors.New("unsafe configuration ancestor")
	errAncestorUnknown = errors.New("unverifiable configuration ancestor")
)

// configAncestor mirrors bundleDirectory(st, ancestor=true) for every directory
// above a selected file, with the caller's uid: owned by root or the user, no
// setuid/setgid, no group or other writers except a root-owned sticky directory
// such as /private/tmp.
func configAncestor(st *unix.Stat_t, uid int) bool {
	if st.Mode&unix.S_IFMT != unix.S_IFDIR || (st.Uid != 0 && int(st.Uid) != uid) ||
		st.Mode&0o400 == 0 || st.Mode&0o100 == 0 || st.Mode&0o6000 != 0 {
		return false
	}
	if st.Mode&0o022 == 0 {
		return st.Mode&0o1000 == 0
	}
	return st.Uid == 0 && st.Mode&0o1000 != 0
}

// ancestorChain inspects every directory descriptor of one no-follow walk (the
// root, each ancestor and the parent) as the walk opens it, before it descends
// further: ownership and mode policy, then ACL evidence. The directories checked
// are exactly the directories traversed. Their identities are kept so the final
// rewalk can prove none was replaced.
type ancestorChain struct {
	uid     int
	inspect func(int) (darwinACL, error)
	dirs    []unix.Stat_t
}

func (c *ancestorChain) visit(fd int) error {
	var st unix.Stat_t
	if unix.Fstat(fd, &st) != nil {
		return errAncestorUnknown
	}
	if !configAncestor(&st, c.uid) {
		return errAncestorUnsafe
	}
	acl, err := c.inspect(fd)
	if err != nil {
		return errAncestorUnknown
	}
	if !acl.directoryOK() {
		return errAncestorUnsafe
	}
	c.dirs = append(c.dirs, st)
	return nil
}

// Ancestors compare by identity only (as sameBundleAncestor does): their times
// change whenever any entry is created or removed and say nothing about writers.
// An empty chain proves nothing and never matches.
func (c *ancestorChain) same(o *ancestorChain) bool {
	if len(c.dirs) == 0 || len(c.dirs) != len(o.dirs) {
		return false
	}
	for i := range c.dirs {
		if !sameBundleAncestor(&c.dirs[i], &o.dirs[i]) {
			return false
		}
	}
	return true
}

func refusedParent(err error) Inspection {
	if errors.Is(err, unix.ENOENT) || errors.Is(err, unix.EACCES) || errors.Is(err, errAncestorUnknown) {
		return refusedConfig(ConfigUnavailable)
	}
	return refusedConfig(ConfigUnsafe)
}

// fileACLCode is ConfigValid only when the selected file's descriptor has no ACL.
func fileACLCode(fd int, inspect func(int) (darwinACL, error)) ConfigCode {
	acl, err := inspect(fd)
	if err != nil {
		return ConfigUnavailable
	}
	if !acl.fileOK() {
		return ConfigUnsafe
	}
	return ConfigValid
}

func openSelectedFile(parent int, base string) (int, error) {
	return unix.Openat(parent, base, unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_NONBLOCK|unix.O_CLOEXEC, 0)
}

func readConfig(path string, uid int, read func(io.Reader) ([]byte, error)) Inspection {
	return readConfigWithHost(path, uid, runtime.GOOS, openParentWalk, openSelectedFile, read)
}

func readConfigWithHost(path string, uid int, goos string,
	acquireParent func(string, func(int) error) (int, string, error), openContent func(int, string) (int, error),
	read func(io.Reader) ([]byte, error),
) Inspection {
	if configHostCannotProveACL(goos) {
		return refusedConfig(ConfigUnavailable)
	}
	return readConfigWithACL(path, uid, goos, acquireParent, openContent, read, darwinInspectACL)
}

func readConfigWithACL(path string, uid int, goos string,
	acquireParent func(string, func(int) error) (int, string, error), openContent func(int, string) (int, error),
	read func(io.Reader) ([]byte, error), inspect func(int) (darwinACL, error),
) Inspection {
	var chain *ancestorChain // nil off Darwin
	var visit func(int) error
	if goos == "darwin" {
		chain = &ancestorChain{uid: uid, inspect: inspect}
		visit = chain.visit
	}
	parent, base, err := acquireParent(path, visit)
	if err != nil {
		return refusedParent(err)
	}
	defer unix.Close(parent)
	if chain != nil && len(chain.dirs) == 0 { // the walk must have been inspected
		return refusedConfig(ConfigUnsafe)
	}
	var before unix.Stat_t
	if err := unix.Fstatat(parent, base, &before, unix.AT_SYMLINK_NOFOLLOW); err != nil {
		return refusedConfig(ConfigUnavailable)
	}
	if !privateRegular(&before, uid) || before.Size > MaxConfigBytes {
		return refusedConfig(ConfigUnsafe)
	}
	fd, err := openContent(parent, base)
	if err != nil {
		return refusedConfig(ConfigUnavailable)
	}
	file := os.NewFile(uintptr(fd), "selected diagnostic configuration")
	defer file.Close()
	var opened unix.Stat_t
	if err := unix.Fstat(fd, &opened); err != nil || !privateRegular(&opened, uid) ||
		opened.Dev != before.Dev || opened.Ino != before.Ino {
		return refusedConfig(ConfigUnsafe)
	}
	if goos == "darwin" {
		if code := fileACLCode(fd, inspect); code != ConfigValid {
			return refusedConfig(code)
		}
	}
	initial, err := file.Stat()
	if err != nil {
		return refusedConfig(ConfigUnavailable)
	}
	data, err := read(file)
	if err != nil {
		return refusedConfig(ConfigUnavailable)
	}
	final, err := file.Stat()
	if err != nil || !os.SameFile(initial, final) || initial.Size() != final.Size() ||
		initial.Mode() != final.Mode() || !initial.ModTime().Equal(final.ModTime()) {
		return refusedConfig(ConfigUnsafe)
	}
	if goos == "darwin" {
		if code := fileACLCode(fd, inspect); code != ConfigValid {
			return refusedConfig(code)
		}
	}
	// Rewalk the named path without following symlinks, detecting replacements
	// of either the file or a parent while the selected descriptor was read.
	var again *ancestorChain
	visit = nil
	if chain != nil {
		again = &ancestorChain{uid: uid, inspect: inspect}
		visit = again.visit
	}
	currentParent, currentBase, err := acquireParent(path, visit)
	if err != nil {
		return refusedConfig(ConfigUnsafe)
	}
	defer unix.Close(currentParent)
	var named, after unix.Stat_t
	if unix.Fstatat(currentParent, currentBase, &named, unix.AT_SYMLINK_NOFOLLOW) != nil ||
		unix.Fstat(fd, &after) != nil || !privateRegular(&named, uid) || !privateRegular(&after, uid) ||
		named.Dev != opened.Dev || named.Ino != opened.Ino || named.Size != after.Size ||
		after.Dev != opened.Dev || after.Ino != opened.Ino ||
		(chain != nil && (!sameConfigCTime(&opened, &after) || !sameConfigCTime(&named, &after) || !chain.same(again))) {
		return refusedConfig(ConfigUnsafe) // Darwin: every directory is re-proven and none was replaced
	}
	return ParseConfig(strings.NewReader(string(data)))
}

// ParseConfig parses a complete bounded literal assignment format. It is not a
// shell interpreter. Supported legacy values are simple unquoted literals and
// non-expanding double quotes. Generated single-quote concatenation (a closing
// quote, a backslash-escaped apostrophe, then a reopening quote) is supported
// exactly; no multiline or executable suffixes.
func ParseConfig(r io.Reader) Inspection {
	if absent(r) {
		return refusedConfig(ConfigInvalid)
	}
	data, err := io.ReadAll(io.LimitReader(r, MaxConfigBytes+1))
	if err != nil || len(data) > MaxConfigBytes || !utf8.Valid(data) {
		return refusedConfig(ConfigInvalid)
	}
	for _, r := range string(data) {
		if unicode.IsControl(r) && r != '\n' {
			return refusedConfig(ConfigInvalid)
		}
	}
	values := make(map[string]string)
	seen := make(map[string]bool)
	for _, line := range strings.Split(string(data), "\n") {
		if len(line) > MaxConfigLine {
			return refusedConfig(ConfigInvalid)
		}
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		line = strings.TrimPrefix(line, "export ")
		key, raw, found := strings.Cut(line, "=")
		if !found || !validKey(key) || seen[key] || len(seen) >= MaxConfigKeys {
			return refusedConfig(ConfigInvalid)
		}
		value, ok := literal(raw)
		if !ok || len(value) > MaxConfigValue || strings.IndexFunc(value, unicode.IsControl) >= 0 {
			return refusedConfig(ConfigInvalid)
		}
		seen[key] = true
		// Unknown keys are checked for syntax but never retained or exported.
		switch key {
		case "HERDR_RELAY_TRANSPORT", "HERDR_GATEWAY_URL", "HERDR_PHONE_APP_URL",
			"HERDR_EXTERNAL_HTTPS_ORIGIN", "HERDR_TAILSCALE_ORIGIN", "HERDR_TAILSCALE_CLI_ORIGIN",
			"HERDR_RELAY_PORT", "HERDR_RELAY_PLUGIN_PORT", "HERDR_RELAY_TOKEN", "HERDR_RELAY_INSTANCE_ID", "HERDR_SOCKET_PATH":
			values[key] = value
		}
	}
	c := Inspection{code: ConfigValid, mode: ModeUnknown, port: 8375, pluginPort: 8376}
	c.tokenPresent = values["HERDR_RELAY_TOKEN"] != ""
	c.tokenValid = len(values["HERDR_RELAY_TOKEN"]) == 32
	delete(values, "HERDR_RELAY_TOKEN")
	if c.tokenPresent && !c.tokenValid {
		return refusedConfig(ConfigInvalid)
	}
	c.instance = values["HERDR_RELAY_INSTANCE_ID"]
	// Missing identity prevents exact local observation, not literal inspection.
	if c.instance != "" && !validInstance(c.instance) {
		return refusedConfig(ConfigInvalid)
	}
	// Unsupported/missing socket selectors do not invalidate otherwise valid
	// routing or select a fallback. They grant no Herdr query capability.
	if path := values["HERDR_SOCKET_PATH"]; validHerdrSocketPath(path) {
		c.herdrSocket = path
	}
	for _, field := range []struct {
		key    string
		target *int
	}{
		{"HERDR_RELAY_PORT", &c.port}, {"HERDR_RELAY_PLUGIN_PORT", &c.pluginPort},
	} {
		if value := values[field.key]; value != "" {
			n, err := strconv.Atoi(value)
			if err != nil || n < 1 || n > 65535 || strconv.Itoa(n) != value {
				return refusedConfig(ConfigInvalid)
			}
			*field.target = n
		}
	}
	for _, value := range strings.Split(values["HERDR_GATEWAY_URL"], ",") {
		value = strings.TrimSpace(value)
		if value == "" {
			continue
		}
		origin, ok := gatewayOrigin(value)
		if !ok || len(c.gateways) >= MaxGateways {
			return refusedConfig(ConfigInvalid)
		}
		for _, prior := range c.gateways {
			if prior == origin {
				return refusedConfig(ConfigInvalid)
			}
		}
		c.gateways = append(c.gateways, origin)
	}
	mode := strings.ToLower(strings.TrimSpace(values["HERDR_RELAY_TRANSPORT"]))
	if mode == "" {
		mode = string(ModeCloudflare)
		if len(c.gateways) != 0 {
			mode = string(ModeGateway)
		}
	}
	switch Mode(mode) {
	case ModeCloudflare, ModeGateway, ModeManaged, ModeBYO, ModeCLI:
		c.mode = Mode(mode)
	default:
		return refusedConfig(ConfigInvalid)
	}
	if (c.mode == ModeGateway) != (len(c.gateways) != 0) {
		return refusedConfig(ConfigInvalid)
	}
	if value := values["HERDR_PHONE_APP_URL"]; value != "" {
		origin, ok := httpsOrigin(value)
		if !ok {
			return refusedConfig(ConfigInvalid)
		}
		c.appOrigin = origin
	}
	for _, field := range []struct {
		key  string
		mode Mode
	}{
		{"HERDR_EXTERNAL_HTTPS_ORIGIN", ModeBYO}, {"HERDR_TAILSCALE_ORIGIN", ModeManaged},
		{"HERDR_TAILSCALE_CLI_ORIGIN", ModeCLI},
	} {
		if value := values[field.key]; value != "" {
			origin, ok := httpsOrigin(value)
			if !ok || c.mode != field.mode {
				return refusedConfig(ConfigInvalid)
			}
			c.relayOrigin = origin
		}
	}
	// Completeness for startup/admission is intentionally not inferred here.
	return c
}

// Matches config.safeRunID's existing installation identity grammar, without
// importing the startup loader or reflecting an invalid private value.
func validInstance(value string) bool {
	if len(value) == 0 || len(value) > 128 {
		return false
	}
	for _, r := range value {
		if r < 0x21 || r > 0x7e || r == '/' || r == '\\' {
			return false
		}
	}
	return true
}

func validKey(key string) bool {
	if len(key) == 0 || len(key) > 64 || (key[0] != '_' && (key[0] < 'A' || key[0] > 'Z')) {
		return false
	}
	for _, b := range []byte(key) {
		if b != '_' && (b < 'A' || b > 'Z') && (b < '0' || b > '9') {
			return false
		}
	}
	return true
}

func literal(raw string) (string, bool) {
	if raw == "" {
		return "", true
	}
	if raw[0] == '\'' {
		var out strings.Builder
		for len(raw) > 0 {
			if raw[0] != '\'' {
				return "", false
			}
			end := strings.IndexByte(raw[1:], '\'')
			if end < 0 {
				return "", false
			}
			out.WriteString(raw[1 : end+1])
			raw = raw[end+2:]
			if strings.HasPrefix(raw, "\\'") {
				out.WriteByte('\'')
				raw = raw[2:]
				if raw == "" {
					return "", false
				}
			}
		}
		return out.String(), true
	}
	if raw[0] == '"' {
		if len(raw) < 2 || raw[len(raw)-1] != '"' {
			return "", false
		}
		value := raw[1 : len(raw)-1]
		return value, !strings.ContainsAny(value, "\"$`\\")
	}
	if strings.ContainsAny(raw, " \t\r\n'\"\\$`;&|<>(){}!#") {
		return "", false
	}
	return raw, true
}

func httpsOrigin(value string) (string, bool) {
	if strings.ContainsAny(value, "?#\\") || strings.TrimSpace(value) != value {
		return "", false
	}
	value = strings.TrimSuffix(value, "/")
	origin, err := setuphelper.NormalizeExternalHTTPSOrigin(value)
	return origin, err == nil
}

func gatewayOrigin(value string) (string, bool) {
	if strings.ContainsAny(value, "?#\\") {
		return "", false
	}
	u, err := url.Parse(value)
	if err != nil || u.User != nil || u.Host == "" || u.Opaque != "" || (u.Path != "" && u.Path != "/") {
		return "", false
	}
	if u.Scheme == "wss" {
		origin, ok := httpsOrigin("https://" + u.Host + u.Path)
		return strings.Replace(origin, "https://", "wss://", 1), ok
	}
	if u.Scheme != "ws" || (u.Hostname() != "localhost" && !net.ParseIP(u.Hostname()).IsLoopback()) {
		return "", false
	}
	origin, err := setuphelper.NormalizeOrigin("http://"+u.Host+u.Path, true)
	return strings.Replace(origin, "http://", "ws://", 1), err == nil
}
