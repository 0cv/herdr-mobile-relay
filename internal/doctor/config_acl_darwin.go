//go:build darwin

package doctor

import "golang.org/x/sys/unix"

// A configuration file's ctime moves on any chmod, chown, link or ACL change.
func sameConfigCTime(a, b *unix.Stat_t) bool { return a.Ctim == b.Ctim }
