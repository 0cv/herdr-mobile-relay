//go:build darwin

package darwinacl

import (
	"runtime"
	"syscall"
	"unsafe"

	"golang.org/x/sys/unix"
)

// Bounded, descriptor-bound fgetattrlist(2): no name lookup, shell or external
// command. An errno, a truncated, malformed or unsupported answer is NOT evidence.
func getattrlist(fd int, attrs *unix.Attrlist, buf []byte, options uintptr) error {
	_, _, errno := syscall.Syscall6(unix.SYS_FGETATTRLIST, uintptr(fd), uintptr(unsafe.Pointer(attrs)),
		uintptr(unsafe.Pointer(&buf[0])), uintptr(len(buf)), options, 0)
	runtime.KeepAlive(attrs)
	runtime.KeepAlive(buf)
	if errno != 0 {
		return errno
	}
	return nil
}

// volumeACLs: the descriptor's volume must advertise ACL support.
func volumeACLs(fd int) error {
	var volume [volumeLength]byte
	attrs := unix.Attrlist{Bitmapcount: unix.ATTR_BIT_MAP_COUNT, Volattr: unix.ATTR_VOL_INFO | unix.ATTR_VOL_CAPABILITIES}
	if err := getattrlist(fd, &attrs, volume[:], 0); err != nil {
		return err
	}
	if !volumeHasACLs(volume[:]) {
		return unix.ENOTSUP
	}
	return nil
}

// Inspect reports the ACL evidence of an opened file or directory descriptor.
func Inspect(fd int) (ACL, error) {
	var buf [aclBuffer]byte
	attrs := unix.Attrlist{Bitmapcount: unix.ATTR_BIT_MAP_COUNT, Commonattr: returnedAttrs | extendedSecurity}
	if err := getattrlist(fd, &attrs, buf[:], unix.FSOPT_REPORT_FULLSIZE); err != nil {
		return ACL{}, err
	}
	acl, err := parse(buf[:])
	if err != nil || !acl.Absent {
		return acl, err
	}
	// An omitted security attribute means "no ACL" only on a volume that
	// advertises ACL support; elsewhere it is unsupported, hence unknown.
	if err := volumeACLs(fd); err != nil {
		return ACL{}, err
	}
	return acl, nil
}

// Filesystem is nil only when the descriptor lives on an apfs or hfs volume
// (fstatfs f_fstypename) that honours ownership (f_flags without
// MNT_IGNORE_OWNERSHIP) and advertises ACL support.
func Filesystem(fd int) error {
	var st unix.Statfs_t
	if err := unix.Fstatfs(fd, &st); err != nil {
		return err
	}
	if !knownFilesystem(unix.ByteSliceToString(st.Fstypename[:]), st.Flags&unix.MNT_IGNORE_OWNERSHIP != 0) {
		return unix.ENOTSUP
	}
	return volumeACLs(fd)
}
