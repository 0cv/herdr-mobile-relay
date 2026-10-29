//go:build linux

package localcontrol

import "testing"

func TestInstalledHandoffPreflightFilesystem(t *testing.T) {
	for _, tc := range []struct {
		kind uint64
		want bool
	}{
		{0xef53, true}, {0x58465342, true}, {0x9123683e, true}, {0x01021994, true}, {0x858458f6, true},
		{0x794c7630, false}, {0, false},
	} {
		if handoffLinuxFilesystem(tc.kind) != tc.want {
			t.Fatal("filesystem policy")
		}
	}
}
