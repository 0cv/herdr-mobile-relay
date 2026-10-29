//go:build darwin

package installedpromotion

import (
	"github.com/0cv/herdr-mobile-relay/internal/darwinacl"
	"testing"
)

func TestSelectionDarwinACL(t *testing.T) {
	for _, tc := range []struct {
		name           string
		acl            darwinacl.ACL
		ancestor, want bool
	}{
		{"absent", darwinacl.ACL{Absent: true}, false, true},
		{"deny-ancestor", darwinacl.ACL{DenyOnly: true, Entries: 1, Denies: 1}, true, true},
		{"deny-private", darwinacl.ACL{DenyOnly: true, Entries: 1, Denies: 1}, false, false},
		{"unknown", darwinacl.ACL{}, true, false},
		{"grant", darwinacl.ACL{Entries: 1}, true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if selectionACL(tc.acl, tc.ancestor) != tc.want {
				t.Fatal("ACL policy changed")
			}
		})
	}
}
