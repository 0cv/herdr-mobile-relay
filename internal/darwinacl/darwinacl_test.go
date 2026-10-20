package darwinacl

import (
	"encoding/binary"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"golang.org/x/sys/unix"
)

func TestParseFailsClosed(t *testing.T) {
	put := func(b []byte, offset int, value uint32) { binary.NativeEndian.PutUint32(b[offset:], value) }
	// Real bytes: a security object is a 32-byte header, a 44-byte kauth_filesec
	// and 24 bytes per entry, in a larger fixed buffer.
	filesec := func(count, flags uint32, aces ...uint32) []byte {
		b := make([]byte, aclBuffer)
		size := filesecHeader + aceSize*len(aces)
		put(b, 0, uint32(32+size))
		put(b, 4, returnedAttrs|extendedSecurity)
		put(b, 24, 8)
		put(b, 28, uint32(size))
		put(b, 32, filesecMagic)
		put(b, 68, count)
		put(b, 72, flags)
		for i, ace := range aces {
			put(b, 76+24*i+16, ace)
		}
		return b
	}
	omitted := func(length, reference uint32) []byte { // security attribute not returned
		b := make([]byte, aclBuffer)
		put(b, 0, length)
		put(b, 4, returnedAttrs)
		put(b, 28, reference)
		return b
	}
	modify := func(b []byte, offset int, value uint32) []byte { put(b, offset, value); return b }
	many := func(n int, flags uint32) []uint32 {
		aces := make([]uint32, n)
		for i := range aces {
			aces[i] = flags
		}
		return aces
	}
	const deny, permit = aceDeny, 1
	padded := func() []byte { // a well-formed object, but 4 bytes behind the reference
		b := filesec(1, 0, deny)
		copy(b[36:], b[32:100])
		put(b, 32, 0)
		put(b, 24, 12)
		put(b, 0, 104)
		return b
	}
	for _, tc := range []struct {
		name string
		blob []byte
		want ACL
		fail bool
	}{
		{"no ACL, reference placeholder", omitted(32, 0), ACL{Absent: true}, false},
		{"no ACL, no placeholder", omitted(24, 0), ACL{Absent: true}, false},
		{"home folder deny entry", filesec(1, 0, deny), ACL{DenyOnly: true, Entries: 1, Denies: 1}, false},
		{"deny entries with inheritance bits", filesec(2, 0, deny|aceInherit, deny|0x10), ACL{DenyOnly: true, Entries: 2, Denies: 2}, false},
		{"maximum entries", filesec(128, 0, many(128, deny)...), ACL{DenyOnly: true, Entries: 128, Denies: 128}, false},
		{"too many entries", filesec(129, 0, many(129, deny)...), ACL{}, true},
		{"permit entry", filesec(1, 0, permit), ACL{Entries: 1}, false},
		{"deny then permit", filesec(2, 0, deny, permit), ACL{Entries: 2, Denies: 1}, false},
		{"audit entry", filesec(1, 0, 3), ACL{Entries: 1}, false},
		{"alarm entry", filesec(1, 0, 4), ACL{Entries: 1}, false},
		{"unknown kind", filesec(1, 0, 5), ACL{Entries: 1}, false},
		{"deny with audit flag", filesec(1, 0, deny|0x200), ACL{Entries: 1, Denies: 1}, false},
		{"ACL flags", filesec(1, 1<<16, deny), ACL{Entries: 1, Denies: 1}, false},
		{"empty ACL", filesec(0, 0), ACL{}, false},
		{"NOACL sentinel is unverified", filesec(^uint32(0), 0), ACL{}, true},
		{"count differs from size", filesec(2, 0, deny), ACL{}, true},
		{"truncated", modify(filesec(1, 0, deny), 0, aclBuffer+1), ACL{}, true},
		{"short buffer", filesec(1, 0, deny)[:60], ACL{}, true},
		{"bad magic", modify(filesec(1, 0, deny), 32, 0), ACL{}, true},
		{"size not a whole number of entries", modify(modify(filesec(1, 0, deny), 28, 50), 0, 82), ACL{}, true},
		{"reference past the response", modify(filesec(1, 0, deny), 24, 4000), ACL{}, true},
		{"negative offset", modify(filesec(1, 0, deny), 24, ^uint32(0)), ACL{}, true},
		{"padded reference offset", padded(), ACL{}, true},
		{"security attribute only", modify(filesec(1, 0, deny), 4, extendedSecurity), ACL{}, true},
		{"nothing returned", modify(filesec(1, 0, deny), 4, 0), ACL{}, true},
		{"another attribute group", modify(filesec(1, 0, deny), 12, 1), ACL{}, true},
		{"omitted with a placeholder offset", modify(omitted(32, 0), 24, 8), ACL{}, true},
		{"omitted with an offset and no length", modify(omitted(32, 0), 24, ^uint32(0)), ACL{}, true},
		{"omitted with data", omitted(32, 4), ACL{}, true},
		{"omitted with offset and data", modify(omitted(32, 4), 24, 8), ACL{}, true},
		{"omitted with odd length", omitted(28, 0), ACL{}, true},
	} {
		got, err := parse(tc.blob)
		if (err != nil) != tc.fail || got != tc.want {
			t.Fatalf("%s: descriptor ACL response verdict changed", tc.name)
		}
		if err != nil && err.Error() != "unverifiable descriptor ACL" {
			t.Fatalf("%s: error was not the fixed label", tc.name)
		}
	}
	for _, tc := range []struct {
		acl        ACL
		file, dirs bool
	}{
		{ACL{Absent: true}, true, true},
		{ACL{DenyOnly: true, Entries: 1, Denies: 1}, false, true},
		{ACL{Entries: 1}, false, false},
		{ACL{}, false, false},
	} {
		if tc.acl.FileOK() != tc.file || tc.acl.DirectoryOK() != tc.dirs {
			t.Fatal("file and directory ACL policy changed")
		}
	}
}

func TestVolumeMustAdvertiseACLs(t *testing.T) {
	volume := func(length, capabilities, valid uint32) []byte {
		b := make([]byte, volumeLength)
		binary.NativeEndian.PutUint32(b, length)
		binary.NativeEndian.PutUint32(b[volumeInterfaces:], capabilities)
		binary.NativeEndian.PutUint32(b[volumeValid:], valid)
		return b
	}
	for name, tc := range map[string]struct {
		blob []byte
		want bool
	}{
		"advertised":            {volume(36, 0x1af6fc7, 0x1ffffff), true},
		"not advertised":        {volume(36, 0x1af6bc7, 0x1ffffff), false},
		"advertised, not valid": {volume(36, 0x1af6fc7, 0x1fffbff), false},
		"wrong length":          {volume(40, 0x1af6fc7, 0x1ffffff), false},
		"short":                 {volume(36, 0x1af6fc7, 0x1ffffff)[:20], false},
	} {
		if volumeHasACLs(tc.blob) != tc.want {
			t.Fatalf("%s: volume ACL capability verdict changed", name)
		}
	}
}

func TestFilesystemModelAdmitsOnlyApfsAndHfs(t *testing.T) {
	for _, name := range []string{"apfs", "hfs"} {
		if !knownFilesystem(name, false) {
			t.Fatalf("%s: a verified local volume was refused", name)
		}
		if knownFilesystem(name, true) {
			t.Fatalf("%s: a volume that ignores ownership was admitted", name)
		}
	}
	for _, name := range []string{"", "APFS", "hfs+", "apfs\x00", "msdos", "exfat", "nfs", "smbfs", "afpfs", "webdav", "ufs", "devfs", "autofs", "tmpfs", "ext4", "fuse"} {
		if knownFilesystem(name, false) || knownFilesystem(name, true) {
			t.Fatalf("%q: an unverified filesystem model was admitted", name)
		}
	}
}

func TestInspectorsRefuseInvalidDescriptor(t *testing.T) {
	_, inspectErr := Inspect(-1)
	filesystemErr := Filesystem(-1)
	if inspectErr == nil || filesystemErr == nil {
		t.Fatal("an invalid descriptor produced evidence")
	}
	if runtime.GOOS != "darwin" && (!errors.Is(inspectErr, unix.ENOTSUP) || !errors.Is(filesystemErr, unix.ENOTSUP)) {
		t.Fatal("a host without the descriptor route produced evidence")
	}
}

// Plain private objects: the real descriptor routes, with no ACL operation.
func TestRealPlainObjectsHaveNoACLOnAnAllowedVolume(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("descriptor ACL evidence is Darwin only")
	}
	root := t.TempDir()
	path := filepath.Join(root, "plain")
	if err := os.WriteFile(path, []byte("{}"), 0o600); err != nil {
		t.Fatal("plain fixture could not be written")
	}
	for _, name := range []string{root, path} {
		fd, err := unix.Open(name, unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_NONBLOCK|unix.O_CLOEXEC, 0)
		if err != nil {
			t.Fatal("plain fixture could not be opened")
		}
		acl, inspectErr := Inspect(fd)
		filesystemErr := Filesystem(fd)
		_ = unix.Close(fd)
		if inspectErr != nil || acl != (ACL{Absent: true}) {
			t.Fatal("plain private object did not report absent ACL evidence")
		}
		if filesystemErr != nil {
			t.Fatal("the scratch volume was not an admitted ACL-capable filesystem")
		}
	}
}
