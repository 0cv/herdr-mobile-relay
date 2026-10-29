//go:build linux

package installedpromotion

import "golang.org/x/sys/unix"

// POSIX ACL masks are reflected in group bits. Overlay, remote and FUSE
// filesystems are deliberately NOT accepted for this writable private lifecycle.
func selectionLinuxFilesystem(kind uint64) bool {
	switch kind {
	case 0xef53, 0x58465342, 0x9123683e, 0x01021994, 0x858458f6:
		return true
	}
	return false
}
func selectionPlatform(fd int, ancestor bool) error {
	var s unix.Statfs_t
	if unix.Fstatfs(fd, &s) != nil || !selectionLinuxFilesystem(uint64(s.Type)) {
		return errSelection
	}
	return nil
}
func selectionExclusive(parent int, from, to string) error {
	return unix.Renameat2(parent, from, parent, to, unix.RENAME_NOREPLACE)
}
