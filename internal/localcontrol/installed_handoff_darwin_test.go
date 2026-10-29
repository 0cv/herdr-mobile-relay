//go:build darwin

package localcontrol

import (
	"github.com/0cv/herdr-mobile-relay/internal/darwinacl"
	"golang.org/x/sys/unix"
	"testing"
)

// Synthetic inspector evidence only, not native ACL qualification.
func TestInstalledHandoffSyntheticACL(t *testing.T) {
	for _, tc := range []struct {
		name           string
		a              darwinacl.ACL
		ancestor, want bool
	}{
		{"absent-private", darwinacl.ACL{Absent: true}, false, true},
		{"deny-ancestor", darwinacl.ACL{DenyOnly: true, Entries: 1, Denies: 1}, true, true},
		{"deny-private", darwinacl.ACL{DenyOnly: true, Entries: 1, Denies: 1}, false, false},
		{"unknown", darwinacl.ACL{}, true, false},
		{"grant", darwinacl.ACL{Entries: 1}, true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if handoffACL(tc.a, tc.ancestor) != tc.want {
				t.Fatal("synthetic ACL policy")
			}
		})
	}
}

func handoffTestMode(s *unix.Stat_t, m uint32)  { s.Mode = uint16(m) }
func handoffTestLinks(s *unix.Stat_t, n uint64) { s.Nlink = uint16(n) }
