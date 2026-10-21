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

// systemd's binfmt_misc: an autofs trigger plus the filesystem it triggered,
// stacked at one mountpoint (the child's parent is the trigger).
const (
	autofsLine = "2 1 0:5 / /proc/sys/fs/binfmt_misc rw - autofs systemd-1 rw,direct\n"
	binfmtLine = "3 2 0:6 / /proc/sys/fs/binfmt_misc rw - binfmt_misc binfmt_misc rw\n"
	autofsPair = autofsLine + binfmtLine
)

func TestInventoryStrictness(t *testing.T) {
	for name, data := range map[string]string{
		"truncated":            strings.TrimSuffix(fixtureInventory, "\n"),
		"empty":                "",
		"unknown escape":       fixtureInventory + "2 1 8:1 / /bad\\777 rw - ext4 /dev/fixture rw\n",
		"duplicate mountpoint": fixtureInventory + "2 1 8:1 / / rw - ext4 /dev/fixture rw\n",
		"duplicate id":         fixtureInventory + "1 99 8:1 / /home rw - ext4 /dev/fixture rw\n",
		"missing parent":       fixtureInventory + "2 88 8:1 / /home rw - ext4 /dev/fixture rw\n",
		"bad device":           "1 99 bad / / rw - ext4 /dev/fixture rw\n",
		"stacked ordinary":     fixtureInventory + "2 1 8:2 / /data rw - ext4 /dev/a rw\n3 2 8:3 / /data rw - ext4 /dev/b rw\n",
		"stacked at base":      fixtureInventory + "2 1 8:2 / / rw - ext4 /dev/a rw\n",
		"double trigger child": fixtureInventory + autofsPair + "4 3 0:7 / /proc/sys/fs/binfmt_misc rw - binfmt_misc binfmt_misc rw\n",
		"autofs over autofs":   fixtureInventory + autofsLine + "3 2 0:6 / /proc/sys/fs/binfmt_misc rw - autofs systemd-1 rw\n",
		"two trigger children": fixtureInventory + autofsPair + "4 2 0:7 / /proc/sys/fs/binfmt_misc rw - binfmt_misc binfmt_misc rw\n",
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
	if err != nil || len(data) == 0 || len(data) > maxInventory || data[len(data)-1] != '\n' {
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
	for _, m := range inv.byID {
		// Measured independently: an autofs trigger only matters on HOME's
		// ancestry or at or beneath HOME (which holds every selected root).
		if m.fstype == "autofs" && (under(dir, m.point) || under(m.point, dir)) {
			return false
		}
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

func TestAutofsScope(t *testing.T) {
	roots := []string{"/home/user/releases", "/home/user/config", "/home/user/cache"}
	for name, extra := range map[string]string{
		"trigger elsewhere":                autofsLine,
		"triggered pair elsewhere":         autofsPair,
		"indirect map outside home":        "2 1 0:5 / /mnt/net rw - autofs auto.net rw,indirect\n",
		"sibling of home, not an ancestor": "2 1 0:5 / /home/other rw - autofs auto.home rw,indirect\n",
	} {
		t.Run(name, func(t *testing.T) {
			inv, ok := parse([]byte(fixtureInventory + extra))
			if !ok || !inv.isolated("/home/user", roots) {
				t.Fatal("refused an autofs mount that no lookup of this guard can reach")
			}
		})
	}
	if inv, ok := parse([]byte(fixtureInventory + autofsPair)); !ok || inv.byPath["/proc/sys/fs/binfmt_misc"].id != 3 || len(inv.byID) != 3 {
		t.Fatal("the triggered filesystem is not the visible mount of a stacked autofs pair")
	}
	for name, extra := range map[string]string{
		"ancestor of home":           "2 1 0:5 / /home rw - autofs auto.home rw,indirect\n",
		"home":                       "2 1 0:5 / /home/user rw - autofs auto.home rw,indirect\n",
		"selected root":              "2 1 0:5 / /home/user/config rw - autofs x rw,direct\n",
		"beneath selected root":      "2 1 0:5 / /home/user/config/auto rw - autofs x rw,direct\n",
		"beneath home, beside roots": "2 1 0:5 / /home/user/nfs rw - autofs x rw,direct\n",
		"triggered pair at a root":   "2 1 0:5 / /home/user/cache rw - autofs x rw,direct\n3 2 0:6 / /home/user/cache rw - nfs host:/x rw\n",
	} {
		t.Run(name, func(t *testing.T) {
			inv, ok := parse([]byte(fixtureInventory + extra))
			if !ok {
				t.Fatal("well-formed autofs inventory not parsed")
			}
			if inv.isolated("/home/user", roots) || inv.isolated("/home/user", roots[1:2]) {
				t.Fatal("accepted an autofs trigger a lookup could fire")
			}
		})
	}
	if inv, ok := parse([]byte("1 99 0:5 / / rw - autofs x rw,direct\n")); !ok || inv.isolated("/home/user", roots) {
		t.Fatal("accepted an autofs base mount")
	}
}

// The recheck is one root's proof from a fresh inventory: a mount on another
// selected root does not refuse it, a mount on its own ancestry or beneath it does.
func TestSingleRootRecheckInventory(t *testing.T) {
	roots := []string{"/home/user/releases", "/home/user/config", "/home/user/cache"}
	for name, point := range map[string]string{"config": roots[1], "nested config": roots[1] + "/n"} {
		t.Run(name, func(t *testing.T) {
			inv, ok := parse([]byte(fixtureInventory + "2 1 9:1 / " + point + " rw - ext4 /dev/other rw\n"))
			if !ok || !inv.isolated("/home/user", roots[:1]) || !inv.isolated("/home/user", roots[2:]) {
				t.Fatal("a mount on another root refused an independent single-root recheck")
			}
			if inv.isolated("/home/user", roots[1:2]) || inv.isolated("/home/user", roots) {
				t.Fatal("single-root or three-root proof accepted a mounted root")
			}
		})
	}
	inv, ok := parse([]byte(fixtureInventory + "2 1 9:1 / /home rw - ext4 /dev/other rw\n"))
	if !ok || inv.isolated("/home/user", roots[:1]) {
		t.Fatal("single-root recheck accepted a mounted ancestor")
	}
}

func TestCheckRootsKindsAndRecheck(t *testing.T) {
	home := t.TempDir()
	roots := []string{filepath.Join(home, "release"), filepath.Join(home, "config"), filepath.Join(home, "cache")}
	for _, root := range roots {
		if err := os.Mkdir(root, 0700); err != nil {
			t.Fatal(err)
		}
	}
	if !baseMountFixture(home, roots) {
		for _, args := range [][]string{roots, roots[:1]} {
			if err := CheckRoots(home, args...); err != errRefused {
				t.Fatal("unsupported topology did not return the fixed refusal")
			}
		}
		return // unsupported host refusal was asserted, not counted as a positive
	}
	mustRefuse := func(what string, args ...string) {
		t.Helper()
		if err := CheckRoots(home, args...); err != errRefused {
			t.Fatal(what + ": not the fixed refusal")
		}
	}
	mustAllow := func(what string, args ...string) {
		t.Helper()
		if err := CheckRoots(home, args...); err != nil {
			t.Fatal(what + ": refused")
		}
	}
	mustAllow("three directories", roots...)
	mustAllow("single directory", roots[1])
	// The recheck of one root does not depend on another root's contents.
	fifo := filepath.Join(roots[1], "fifo")
	if err := unix.Mkfifo(fifo, 0600); err != nil {
		t.Fatal(err)
	}
	mustAllow("recheck of an unaffected root", roots[0])
	mustRefuse("recheck of an affected root", roots[1])
	mustRefuse("three-root proof with an affected root", roots...)
	if err := os.Remove(fifo); err != nil {
		t.Fatal(err)
	}
	// Absent: nothing to check (also beneath a missing ancestor), never for the strict form.
	if err := os.Remove(roots[2]); err != nil {
		t.Fatal(err)
	}
	mustAllow("absent root", roots...)
	mustAllow("absent root alone", roots[2])
	mustAllow("absent root beneath a missing ancestor", filepath.Join(roots[2], "child", "deeper"))
	if err := Check(home, roots[0], roots[1], roots[2]); err != errRefused {
		t.Fatal("strict form admitted an absent root")
	}
	// Symlink: not traversed, so a refusable object behind it does not matter;
	// the other roots are still proved, and a name beneath the link is refused.
	outside := filepath.Join(home, "outside")
	if err := os.Mkdir(outside, 0700); err != nil {
		t.Fatal(err)
	}
	if err := unix.Mkfifo(filepath.Join(outside, "fifo"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, roots[2]); err != nil {
		t.Fatal(err)
	}
	mustAllow("symlink root", roots...)
	mustAllow("symlink root alone", roots[2])
	mustRefuse("name beneath a symlink", filepath.Join(roots[2], "child"))
	if err := Check(home, roots[0], roots[1], roots[2]); err != errRefused {
		t.Fatal("strict form admitted a symlink root")
	}
	if err := os.Remove(roots[2]); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(roots[2], []byte("file"), 0600); err != nil {
		t.Fatal(err)
	}
	mustRefuse("regular file root", roots...)
	mustRefuse("no root")
	mustRefuse("four roots", append(append([]string{}, roots...), roots[0]+"/x")...)
	mustRefuse("repeated root", roots[0], roots[0])
	mustRefuse("nested roots", roots[0], roots[0]+"/child")
	mustRefuse("root outside HOME", filepath.Dir(home))
	mustRefuse("relative root", "release")
}
