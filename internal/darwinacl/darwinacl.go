// Package darwinacl is the one bounded, descriptor-bound ACL inspector for macOS,
// shared by the installed package and support-snapshot readers.
// Mode bits cannot rule out ACL grants, so a reader proves privacy from the
// opened descriptor. Nothing here returns a GUID, a right, an identity or a
// path: results are counts, and every unknown, truncated, malformed or
// unsupported answer is an error, never evidence.
//
// The layouts come from the public XNU headers and were confirmed against the
// bytes a real macOS kernel returns:
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
package darwinacl

import (
	"encoding/binary"
	"errors"
)

const (
	returnedAttrs    = 0x80000000 // ATTR_CMN_RETURNED_ATTRS
	extendedSecurity = 0x00400000 // ATTR_CMN_EXTENDED_SECURITY
	filesecMagic     = 0x012cc16d // KAUTH_FILESEC_MAGIC
	filesecHeader    = 44
	aceSize          = 24
	maxACEs          = 128   // KAUTH_ACL_MAX_ENTRIES
	aceKindMask      = 0xf   // KAUTH_ACE_KINDMASK
	aceDeny          = 2     // KAUTH_ACE_DENY
	aceInherit       = 0x1f0 // KAUTH_ACE_INHERITED through KAUTH_ACE_ONLY_INHERIT
	aclBuffer        = 4096  // length, returned set, reference and 128 entries
	volumeLength     = 36    // u32 length, vol_capabilities_attr_t
	volumeInterfaces = 8     // capabilities[VOL_CAPABILITIES_INTERFACES]
	volumeValid      = 24    // valid[VOL_CAPABILITIES_INTERFACES]
	volumeACLBit     = 0x400 // VOL_CAP_INT_EXTENDED_SECURITY
)

// ACL keeps nothing but counts: never a GUID, a right or an identity.
type ACL struct {
	Absent   bool // the kernel omitted the security attribute
	DenyOnly bool // 1..128 entries, every one a DENY entry, no unknown flags
	Entries  int
	Denies   int
}

// FileOK: a regular file (and a private directory) must have no ACL at all.
func (a ACL) FileOK() bool { return a.Absent }

// DirectoryOK: an ancestor directory may also carry a fully parsed deny-only
// ACL: deny entries remove rights and can grant none.
func (a ACL) DirectoryOK() bool { return a.Absent || a.DenyOnly }

func parse(buf []byte) (ACL, error) {
	bad := errors.New("unverifiable descriptor ACL") // never includes private metadata
	if len(buf) < 32 {
		return ACL{}, bad
	}
	length := int(binary.NativeEndian.Uint32(buf))
	if length < 24 || length > len(buf) { // FSOPT_REPORT_FULLSIZE reports the untruncated length
		return ACL{}, bad
	}
	for i := 8; i < 24; i += 4 { // volume, directory, file and fork groups were not requested
		if binary.NativeEndian.Uint32(buf[i:]) != 0 {
			return ACL{}, bad
		}
	}
	switch binary.NativeEndian.Uint32(buf[4:]) {
	case returnedAttrs: // no security object: nothing, or an all-zero reference, follows
		if (length != 24 && length != 32) ||
			(length == 32 && (binary.NativeEndian.Uint32(buf[24:]) != 0 || binary.NativeEndian.Uint32(buf[28:]) != 0)) {
			return ACL{}, bad
		}
		return ACL{Absent: true}, nil
	case returnedAttrs | extendedSecurity:
	default:
		return ACL{}, bad
	}
	// The reference is relative to itself; every real answer puts the object
	// right behind it (offset 8), so any other offset is noncanonical.
	offset := int(int32(binary.NativeEndian.Uint32(buf[24:])))
	refLen := int(binary.NativeEndian.Uint32(buf[28:]))
	start := 24 + offset
	if offset != 8 || refLen < filesecHeader ||
		(refLen-filesecHeader)%aceSize != 0 || start+refLen != length {
		return ACL{}, bad
	}
	entries := (refLen - filesecHeader) / aceSize
	sec := buf[start:length]
	// The stored count must equal the size-derived count, so NOACL never matches.
	if entries > maxACEs || binary.NativeEndian.Uint32(sec) != filesecMagic ||
		int(binary.NativeEndian.Uint32(sec[36:])) != entries {
		return ACL{}, bad
	}
	acl := ACL{Entries: entries, DenyOnly: entries > 0 && binary.NativeEndian.Uint32(sec[40:]) == 0}
	for i := 0; i < entries; i++ {
		flags := binary.NativeEndian.Uint32(sec[filesecHeader+aceSize*i+16:])
		if flags&aceKindMask == aceDeny {
			acl.Denies++
		}
		if flags&aceKindMask != aceDeny || flags&^(aceKindMask|aceInherit) != 0 {
			acl.DenyOnly = false // permit, audit, alarm, unknown kinds or flags
		}
	}
	return acl, nil
}

// volumeHasACLs reads ATTR_VOL_CAPABILITIES: the volume must advertise, as
// valid, that it implements extended security (ACLs).
func volumeHasACLs(buf []byte) bool {
	return len(buf) >= volumeLength && binary.NativeEndian.Uint32(buf) == volumeLength &&
		binary.NativeEndian.Uint32(buf[volumeInterfaces:])&binary.NativeEndian.Uint32(buf[volumeValid:])&volumeACLBit != 0
}

// knownFilesystem is the filesystem model: only the local volumes whose
// ownership, mode and ACL semantics were checked against real kernel answers,
// and only while the VFS honours ownership. A volume mounted with
// MNT_IGNORE_OWNERSHIP (sys/mount.h; "Ignore ownership on this volume") reports
// every object as owned by the caller, so uid and mode checks prove nothing there.
// Everything else (network, FUSE, FAT, disk images of other kinds, unknown) is
// unsupported, hence unavailable, never inferred from mode bits.
func knownFilesystem(typeName string, ignoresOwnership bool) bool {
	return (typeName == "apfs" || typeName == "hfs") && !ignoresOwnership
}
