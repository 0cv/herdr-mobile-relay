//go:build darwin

package doctor

import (
	"runtime"
	"syscall"
	"unsafe"

	"golang.org/x/sys/unix"
)

// A configuration file's ctime moves on any chmod, chown, link or ACL change.
func sameConfigCTime(a, b *unix.Stat_t) bool { return a.Ctim == b.Ctim }

// Bounded, descriptor-bound fgetattrlist(2): no name lookup, shell or external
// command. An errno, a truncated, malformed or unsupported answer is NOT evidence.
func darwinGetattrlist(fd int, attrs *unix.Attrlist, buf []byte, options uintptr) error {
	_, _, errno := syscall.Syscall6(unix.SYS_FGETATTRLIST, uintptr(fd), uintptr(unsafe.Pointer(attrs)),
		uintptr(unsafe.Pointer(&buf[0])), uintptr(len(buf)), options, 0)
	runtime.KeepAlive(attrs)
	runtime.KeepAlive(buf)
	if errno != 0 {
		return errno
	}
	return nil
}

func darwinInspectACL(fd int) (darwinACL, error) {
	var buf [darwinACLBuffer]byte
	attrs := unix.Attrlist{Bitmapcount: unix.ATTR_BIT_MAP_COUNT, Commonattr: darwinReturnedAttrs | darwinExtendedSecurity}
	if err := darwinGetattrlist(fd, &attrs, buf[:], unix.FSOPT_REPORT_FULLSIZE); err != nil {
		return darwinACL{}, err
	}
	acl, err := parseDarwinSecurity(buf[:])
	if err != nil || !acl.absent {
		return acl, err
	}
	// An omitted security attribute means "no ACL" only on a volume that
	// advertises ACL support; elsewhere it is unsupported, hence unknown.
	var volume [darwinVolumeLength]byte
	attrs = unix.Attrlist{Bitmapcount: unix.ATTR_BIT_MAP_COUNT, Volattr: unix.ATTR_VOL_INFO | unix.ATTR_VOL_CAPABILITIES}
	if err := darwinGetattrlist(fd, &attrs, volume[:], 0); err != nil {
		return darwinACL{}, err
	}
	if !darwinVolumeHasACLs(volume[:]) {
		return darwinACL{}, unix.ENOTSUP
	}
	return acl, nil
}
