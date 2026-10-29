//go:build linux

package localcontrol

import (
	"golang.org/x/sys/unix"
	"testing"
)

func TestInstalledHandoffSyntheticFilesystem(t *testing.T) {
	for _, tc := range []struct {
		name string
		kind uint64
		want bool
	}{
		{"ext", 0xef53, true}, {"xfs", 0x58465342, true}, {"btrfs", 0x9123683e, true}, {"tmpfs", 0x01021994, true}, {"ramfs", 0x858458f6, true}, {"overlay", 0x794c7630, false}, {"unknown", 0, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if handoffLinuxFilesystem(tc.kind) != tc.want {
				t.Fatal("synthetic filesystem policy")
			}
		})
	}
}

func handoffTestMode(s *unix.Stat_t, m uint32) { s.Mode = uint32(m) }
func handoffTestLinks(s *unix.Stat_t, n uint64) {
	s.Nlink = 0
	for i := uint64(0); i < n; i++ {
		s.Nlink++
	}
}
