package uninstallmount

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

const fixtureInventory = "1 99 8:1 / / rw - ext4 /dev/fixture rw\n"

func TestInventoryStrictness(t *testing.T) {
	for name, data := range map[string]string{
		"truncated":            strings.TrimSuffix(fixtureInventory, "\n"),
		"empty":                "",
		"unknown escape":       fixtureInventory + "2 1 8:1 / /bad\\777 rw - ext4 /dev/fixture rw\n",
		"duplicate mountpoint": fixtureInventory + "2 1 8:1 / / rw - ext4 /dev/fixture rw\n",
		"duplicate id":         fixtureInventory + "1 99 8:1 / /home rw - ext4 /dev/fixture rw\n",
		"missing parent":       fixtureInventory + "2 88 8:1 / /home rw - ext4 /dev/fixture rw\n",
		"bad device":           "1 99 bad / / rw - ext4 /dev/fixture rw\n",
		"automount trigger":    fixtureInventory + "2 1 0:5 / /home/user/cache rw - autofs systemd-1 rw\n",
		"missing separator":    "1 99 8:1 / / rw ext4 /dev/fixture rw\n",
		"excess separator":     "1 99 8:1 / / rw - ext4 /dev/fixture - rw\n",
		"incomplete header":    "1 99 8:1 / / rw - ext4 /dev/fixture\n",
		"embedded NUL":         fixtureInventory + "\x00",
		"overlong":             fixtureInventory + strings.Repeat("x", 4097) + "\n",
	} {
		t.Run(name, func(t *testing.T) {
			if _, ok := parse([]byte(data)); ok {
				t.Fatal("accepted ambiguous mount inventory")
			}
		})
	}
}

func TestInventoryRootsAndDescriptorCorrespondence(t *testing.T) {
	roots := []string{"/home/user/releases", "/home/user/config", "/home/user/cache"}
	base, ok := parse([]byte(fixtureInventory))
	if !ok || !base.isolated("/home/user", roots) {
		t.Fatal("ordinary disjoint home roots rejected")
	}
	st := &unix.Statx_t{Mask: unix.STATX_MNT_ID, Mnt_id: 1, Dev_major: 8, Dev_minor: 1}
	if !base.matches(roots[0]+"/file", st) {
		t.Fatal("matching descriptor refused")
	}
	for name, candidate := range map[string]*unix.Statx_t{
		"missing mount ID": {Dev_major: 8, Dev_minor: 1, Mnt_id: 1},
		"aliased mount ID": {Mask: unix.STATX_MNT_ID, Mnt_id: 2, Dev_major: 8, Dev_minor: 1},
		"different device": {Mask: unix.STATX_MNT_ID, Mnt_id: 1, Dev_major: 8, Dev_minor: 2},
	} {
		t.Run(name, func(t *testing.T) {
			if base.matches(roots[0]+"/file", candidate) {
				t.Fatal("accepted unproven descriptor")
			}
		})
	}
	for _, root := range roots {
		for name, point := range map[string]string{"root": root, "nested": root + "/other"} {
			t.Run(root+name, func(t *testing.T) {
				line := "2 1 8:1 /outside " + point + " rw - ext4 /dev/fixture rw\n"
				inv, ok := parse([]byte(fixtureInventory + line))
				if !ok || inv.isolated("/home/user", roots) {
					t.Fatal("accepted mounted root or nested bind alias")
				}
				// Even if the inventory claimed an ordinary mount, the opened
				// descriptor must identify the precise visible mount, not st_dev.
				line = "2 1 8:1 / " + point + " rw - ext4 /dev/fixture rw\n"
				inv, ok = parse([]byte(fixtureInventory + line))
				if !ok || inv.matches(point, st) {
					t.Fatal("accepted changed mount ID")
				}
			})
		}
	}
	for name, line := range map[string]string{
		"whole filesystem alias": "2 1 8:1 / /elsewhere rw - ext4 /dev/fixture rw\n",
		"selected subtree alias": "2 1 8:1 /home/user/config /elsewhere rw - ext4 /dev/fixture rw\n",
		"ancestor subtree alias": "2 1 8:1 /home/user /elsewhere rw - ext4 /dev/fixture rw\n",
		"mounted home":           "2 1 9:1 / /home/user rw - ext4 /dev/other rw\n",
		"mounted ancestor":       "2 1 9:1 / /home rw - ext4 /dev/other rw\n",
	} {
		t.Run(name, func(t *testing.T) {
			alias, ok := parse([]byte(fixtureInventory + line))
			if !ok || alias.isolated("/home/user", roots) {
				t.Fatal("accepted root alias or mounted ancestor")
			}
		})
	}
	if base.isolated("/home/user", []string{roots[0], roots[0] + "/child", roots[2]}) {
		t.Fatal("accepted overlapping deletions")
	}
	if !validPath("/home/user/a b") || validPath("/home/../other") || validPath("/home//user") {
		t.Fatal("path normalization ambiguity")
	}
}

// Fixture topology is measured without invoking Check or isolated: the
// descriptor IDs, complete parsed inventory, mount intersections and static
// aliases all have to agree before the ordinary positive is required.
func baseMountFixture(dir string, roots []string) bool {
	physical, err := filepath.EvalSymlinks(dir)
	if err != nil || physical != dir {
		return false
	}
	root, err := unix.Open("/", unix.O_PATH|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		return false
	}
	defer unix.Close(root)
	selected, err := unix.Open(dir, unix.O_PATH|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		return false
	}
	defer unix.Close(selected)
	var base, actual unix.Statx_t
	mask := unix.STATX_BASIC_STATS | unix.STATX_MNT_ID
	flags := unix.AT_EMPTY_PATH | unix.AT_NO_AUTOMOUNT
	if unix.Statx(root, "", flags, mask, &base) != nil ||
		unix.Statx(selected, "", flags, mask, &actual) != nil ||
		base.Mask&unix.STATX_MNT_ID == 0 || actual.Mask&unix.STATX_MNT_ID == 0 ||
		base.Mnt_id != actual.Mnt_id || base.Dev_major != actual.Dev_major ||
		base.Dev_minor != actual.Dev_minor {
		return false
	}
	f, err := os.Open("/proc/self/mountinfo")
	if err != nil {
		return false
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, maxInventory+1))
	if err != nil || len(data) == 0 || len(data) > maxInventory ||
		data[len(data)-1] != '\n' || bytes.Contains(data, []byte(" - autofs ")) {
		return false
	}
	inv, ok := parse(data) // validate the complete inventory, not Check's verdict
	if !ok {
		return false
	}
	baseMount := inv.byPath["/"]
	if baseMount.root != "/" || baseMount.id != base.Mnt_id ||
		baseMount.major != uint64(base.Dev_major) || baseMount.minor != uint64(base.Dev_minor) {
		return false
	}
	under := func(child, parent string) bool {
		return child == parent || parent == "/" && strings.HasPrefix(child, "/") ||
			strings.HasPrefix(child, parent+"/")
	}
	for _, root := range roots {
		if !under(root, dir) || root == dir {
			return false
		}
		for _, m := range inv.byID {
			if m.id == baseMount.id {
				continue
			}
			if under(root, m.point) || under(m.point, root) ||
				m.major == baseMount.major && m.minor == baseMount.minor &&
					(under(root, m.root) || under(m.root, root)) {
				return false
			}
		}
	}
	return true
}

func TestPrivatePathRefusals(t *testing.T) {
	home := t.TempDir()
	roots := []string{filepath.Join(home, "release"), filepath.Join(home, "config"), filepath.Join(home, "cache")}
	for _, root := range roots {
		if err := os.Mkdir(root, 0700); err != nil {
			t.Fatal(err)
		}
	}
	// If an ancestor symlink were followed, this child would be present.
	if err := os.Mkdir(filepath.Join(roots[1], "child"), 0700); err != nil {
		t.Fatal(err)
	}
	canary := filepath.Join(roots[1], "keep")
	if err := os.WriteFile(canary, []byte("fixture-private-canary\n"), 0600); err != nil {
		t.Fatal(err)
	}
	// Inspect atime with Lstat, not a second directory walk that could itself
	// update atime under strictatime. Earlier roots are scanned before refusal.
	accessPaths := []string{home, roots[0], roots[1], roots[2], filepath.Join(roots[1], "child")}
	atimes := func() []syscall.Timespec {
		result := make([]syscall.Timespec, len(accessPaths))
		for i, p := range accessPaths {
			info, err := os.Lstat(p)
			if err != nil {
				t.Fatal(err)
			}
			result[i] = info.Sys().(*syscall.Stat_t).Atim
		}
		return result
	}
	assertAtimes := func(before []syscall.Timespec) {
		for i, current := range atimes() {
			if current != before[i] {
				t.Fatal("mount guard changed directory access time")
			}
		}
	}
	beforeAccess := atimes()
	if baseMountFixture(home, roots) {
		// Make atime eligible for a relatime update if ReadDir loses O_NOATIME.
		// This changes only private fixture directories, before the observation.
		for _, p := range accessPaths {
			info, err := os.Lstat(p)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.Chtimes(p, time.Unix(1, 0), info.ModTime()); err != nil {
				t.Fatal(err)
			}
		}
		beforeAccess = atimes()
		if err := Check(home, roots[0], roots[1], roots[2]); err != nil {
			t.Fatal("supported base-mount roots refused")
		}
		assertAtimes(beforeAccess)
	} else {
		if err := Check(home, roots[0], roots[1], roots[2]); err != errRefused {
			t.Fatal("unsupported topology or inventory did not return the fixed refusal")
		}
		return // unsupported host refusal was asserted, not counted as a positive
	}
	if err := os.Remove(roots[2]); err != nil {
		t.Fatal(err)
	}
	if err := Check(home, roots[0], roots[1], roots[2]); err != errRefused {
		t.Fatal("missing selected root did not return the fixed refusal")
	}
	if err := Check(home, roots[0], roots[1], filepath.Join(roots[2], "child")); err != errRefused {
		t.Fatal("missing ancestor did not return the fixed refusal")
	}
	if err := os.Symlink(roots[1], roots[2]); err != nil {
		t.Fatal(err)
	}
	if err := Check(home, roots[0], roots[1], filepath.Join(roots[2], "child")); err != errRefused {
		t.Fatal("symlink ancestor did not return the fixed refusal")
	}
	// Snapshot inode, mode, content, timestamps and every tree entry directly
	// before refusal. A recreated link with the same target is not preserved.
	snapshot := func() (map[string]os.FileInfo, string, []byte) {
		files := make(map[string]os.FileInfo)
		err := filepath.WalkDir(home, func(p string, _ os.DirEntry, err error) error {
			if err != nil {
				return err
			}
			files[p], err = os.Lstat(p)
			return err
		})
		if err != nil {
			t.Fatal(err)
		}
		link, err := os.Readlink(roots[2])
		if err != nil {
			t.Fatal(err)
		}
		content, err := os.ReadFile(canary)
		if err != nil {
			t.Fatal(err)
		}
		return files, link, content
	}
	before, linkBefore, dataBefore := snapshot()
	beforeAccess = atimes()
	if err := Check(home, roots[0], roots[1], roots[2]); err != errRefused {
		t.Fatal("final selected-root symlink did not return the fixed refusal")
	}
	assertAtimes(beforeAccess)
	after, linkAfter, dataAfter := snapshot()
	if len(before) != len(after) || linkBefore != linkAfter || !bytes.Equal(dataBefore, dataAfter) {
		t.Fatal("selected-symlink refusal changed private fixture")
	}
	for p, first := range before {
		last, exists := after[p]
		if !exists || !os.SameFile(first, last) || first.Mode() != last.Mode() ||
			first.Size() != last.Size() || !first.ModTime().Equal(last.ModTime()) ||
			first.Sys().(*syscall.Stat_t).Ctim != last.Sys().(*syscall.Stat_t).Ctim ||
			first.Sys().(*syscall.Stat_t).Nlink != last.Sys().(*syscall.Stat_t).Nlink {
			t.Fatal("selected-symlink refusal changed a private inode")
		}
	}
}

func TestEscapedAndChangedInventory(t *testing.T) {
	escaped := fixtureInventory + "2 1 8:1 / /home/user/a\\040b rw - ext4 /dev/fixture rw\n"
	inv, ok := parse([]byte(escaped))
	if !ok || inv.isolated("/home/user", []string{"/home/user/a b", "/home/user/config", "/home/user/cache"}) {
		t.Fatal("escaped mountpoint did not block deletion")
	}
	if !inventoryStable([]byte(fixtureInventory), []byte(fixtureInventory)) ||
		inventoryStable([]byte(fixtureInventory), []byte(escaped)) ||
		inventoryStable([]byte(fixtureInventory), []byte(strings.TrimSuffix(fixtureInventory, "\n"))) {
		t.Fatal("guard accepted changed or truncated second inventory")
	}
	if len([]byte(strings.Repeat("x", maxInventory+1))) <= maxInventory {
		t.Fatal("test inventory bound unavailable")
	}
	if _, ok := parse([]byte(strings.Repeat("x", maxInventory+1))); ok {
		t.Fatal("accepted oversized inventory")
	}
}
