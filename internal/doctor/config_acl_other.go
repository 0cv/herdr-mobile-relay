//go:build !darwin

package doctor

import "golang.org/x/sys/unix"

// Production non-Darwin paths never use the Darwin ctime comparison.
func sameConfigCTime(_, _ *unix.Stat_t) bool { return true }
