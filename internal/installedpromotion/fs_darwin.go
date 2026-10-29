//go:build darwin

package installedpromotion

import (
	"github.com/0cv/herdr-mobile-relay/internal/darwinacl"
	"golang.org/x/sys/unix"
)

func selectionACL(a darwinacl.ACL, ancestor bool) bool {
	return a.FileOK() || (ancestor && a.DirectoryOK())
}
func selectionPlatform(fd int, ancestor bool) error {
	a, e := darwinacl.Inspect(fd)
	if e != nil || !selectionACL(a, ancestor) || darwinacl.Filesystem(fd) != nil {
		return errSelection
	}
	return nil
}
func selectionExclusive(parent int, from, to string) error {
	return unix.RenameatxNp(parent, from, parent, to, unix.RENAME_EXCL)
}
