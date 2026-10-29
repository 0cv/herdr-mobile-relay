//go:build darwin

package localcontrol

import (
	"github.com/0cv/herdr-mobile-relay/internal/darwinacl"
	"testing"
)

func TestInstalledHandoffPreflightACL(t *testing.T) {
	for _, tc := range []struct {
		a              darwinacl.ACL
		ancestor, want bool
	}{
		{darwinacl.ACL{Absent: true}, false, true},
		{darwinacl.ACL{DenyOnly: true, Entries: 1, Denies: 1}, true, true},
		{darwinacl.ACL{DenyOnly: true, Entries: 1, Denies: 1}, false, false},
		{darwinacl.ACL{}, true, false},
		{darwinacl.ACL{Entries: 1}, true, false},
	} {
		if handoffACL(tc.a, tc.ancestor) != tc.want {
			t.Fatal("ACL policy")
		}
	}
}
