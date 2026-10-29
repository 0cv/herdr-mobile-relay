//go:build darwin

package localcontrol

import (
	"github.com/0cv/herdr-mobile-relay/internal/darwinacl"
	"golang.org/x/sys/unix"
)

func handoffACL(a darwinacl.ACL, ancestor bool) bool {
	return a.FileOK() || (ancestor && a.DirectoryOK())
}
func handoffPlatform(fd int, ancestor bool) error {
	a, e := darwinacl.Inspect(fd)
	if e != nil || !handoffACL(a, ancestor) || darwinacl.Filesystem(fd) != nil {
		return errHandoff
	}
	return nil
}
func handoffExclusive(parent int, from, to string) error {
	return unix.RenameatxNp(parent, from, parent, to, unix.RENAME_EXCL)
}
