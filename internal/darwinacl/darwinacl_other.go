//go:build !darwin

package darwinacl

import "golang.org/x/sys/unix"

// Only macOS has this descriptor route: elsewhere there is no ACL evidence.
func Inspect(int) (ACL, error) { return ACL{}, unix.ENOTSUP }

// Only macOS has this filesystem model.
func Filesystem(int) error { return unix.ENOTSUP }
