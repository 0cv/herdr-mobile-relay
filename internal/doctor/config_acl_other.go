//go:build !darwin

package doctor

import "golang.org/x/sys/unix"

// A simulated Darwin host cannot supply descriptor ACL evidence.
func darwinInspectACL(int) (darwinACL, error) { return darwinACL{}, unix.ENOTSUP }

// Production non-Darwin paths never use the Darwin ctime comparison.
func sameConfigCTime(_, _ *unix.Stat_t) bool { return true }
