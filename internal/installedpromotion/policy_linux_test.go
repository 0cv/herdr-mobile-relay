//go:build linux

package installedpromotion

import "testing"

func TestSelectionLinuxFilesystem(t *testing.T) {
	for _, tc := range []struct {
		name string
		kind uint64
		want bool
	}{
		{"ext", 0xef53, true}, {"tmpfs", 0x01021994, true}, {"overlay", 0x794c7630, false}, {"unknown", 0, false}, {"nfs", 0x6969, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if selectionLinuxFilesystem(tc.kind) != tc.want {
				t.Fatal("filesystem policy changed")
			}
		})
	}
}
