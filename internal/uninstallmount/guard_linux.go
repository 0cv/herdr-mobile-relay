package uninstallmount

import (
	"bytes"
	"errors"
	"io"
	"os"
	"path"
	"strconv"
	"strings"
	"syscall"

	"golang.org/x/sys/unix"
)

// Check and CheckRoots are read-only, conservative observations in the caller's
// mount namespace. Neither authorizes a later pathname deletion: the shell
// uninstaller repeats the observation immediately before each destructive step,
// and a concurrent replacement in between still needs its own policy. Never
// return an underlying error (which may contain a private path).
var errRefused = errors.New("uninstall mount guard refused")

const maxInventory = 4 << 20

type mount struct {
	id, parent, major, minor uint64
	root, point, fstype      string
}

type inventory struct {
	byID   map[uint64]mount
	byPath map[string]mount
}

func validPath(p string) bool {
	return strings.HasPrefix(p, "/") && path.Clean(p) == p && !strings.ContainsAny(p, "\x00\n\r\t\\")
}

// Kernel mountinfo escapes space, tab, newline and backslash as octal. Reject
// unknown escapes rather than silently treating an ambiguous name as literal.
func unescape(s string) (string, bool) {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] != '\\' {
			b.WriteByte(s[i])
			continue
		}
		if i+3 >= len(s) {
			return "", false
		}
		switch s[i+1 : i+4] {
		case "040":
			b.WriteByte(' ')
		case "011":
			b.WriteByte('\t')
		case "012":
			b.WriteByte('\n')
		case "134":
			b.WriteByte('\\')
		default:
			return "", false
		}
		i += 3
	}
	return b.String(), true
}

func decimal(s string) (uint64, bool) {
	if s == "" || strings.Trim(s, "0123456789") != "" {
		return 0, false
	}
	n, err := strconv.ParseUint(s, 10, 64)
	return n, err == nil
}

func parse(data []byte) (inventory, bool) {
	inv := inventory{make(map[uint64]mount), make(map[string]mount)}
	if len(data) == 0 || len(data) > maxInventory || data[len(data)-1] != '\n' || bytes.IndexByte(data, 0) >= 0 {
		return inv, false
	}
	for _, line := range strings.Split(strings.TrimSuffix(string(data), "\n"), "\n") {
		if len(line) == 0 || len(line) > 4096 || len(inv.byID) >= 16384 {
			return inv, false
		}
		fields := strings.Fields(line)
		sep := -1
		for i, field := range fields {
			if field == "-" {
				if sep != -1 {
					return inv, false
				}
				sep = i
			}
			if strings.Contains(field, "\\") {
				if _, ok := unescape(field); !ok {
					return inv, false
				}
			}
		}
		if sep < 6 || len(fields)-sep != 4 || strings.Join(fields, " ") != line {
			return inv, false
		}
		for _, tag := range fields[6:sep] {
			if tag == "unbindable" {
				continue
			}
			prefix, number, ok := strings.Cut(tag, ":")
			if !ok || prefix != "shared" && prefix != "master" && prefix != "propagate_from" {
				return inv, false
			}
			id, valid := decimal(number)
			if !valid || id == 0 {
				return inv, false
			}
		}
		id, a := decimal(fields[0])
		parent, b := decimal(fields[1])
		dev := strings.Split(fields[2], ":")
		if len(dev) != 2 {
			return inv, false
		}
		major, c := decimal(dev[0])
		minor, d := decimal(dev[1])
		root, e := unescape(fields[3])
		point, f := unescape(fields[4])
		if !a || !b || !c || !d || !e || !f || id == 0 || parent == id ||
			!validPath(root) || !validPath(point) || fields[5] == "" ||
			fields[sep+1] == "" || fields[sep+2] == "" || fields[sep+3] == "" {
			return inv, false
		}
		if _, exists := inv.byID[id]; exists {
			return inv, false
		}
		inv.byID[id] = mount{id, parent, major, minor, root, point, fields[sep+1]}
	}
	// Same-point stacking is ambiguous, except for one autofs trigger and the
	// single filesystem it triggered (systemd's binfmt_misc is the common case):
	// the trigger is then covered and the child is the visible mount. Whether an
	// autofs mount matters at all is decided by isolated, by location.
	covered := make(map[uint64]bool)
	for _, m := range inv.byID {
		if m.point == "/" {
			continue
		}
		p, ok := inv.byID[m.parent]
		if !ok || !within(m.point, p.point) {
			return inv, false
		}
		if m.point == p.point {
			if p.fstype != "autofs" || m.fstype == "autofs" || covered[p.id] {
				return inv, false
			}
			covered[p.id] = true
		}
	}
	for _, m := range inv.byID {
		if covered[m.id] {
			continue
		}
		if _, exists := inv.byPath[m.point]; exists {
			return inv, false
		}
		inv.byPath[m.point] = m
	}
	if _, ok := inv.byPath["/"]; !ok {
		return inv, false
	}
	return inv, true
}

func within(child, parent string) bool {
	return parent == "/" && strings.HasPrefix(child, "/") || child == parent || strings.HasPrefix(child, parent+"/")
}

// Every opened descriptor must agree with the unique visible mountinfo entry
// for its lexical name, not merely with st_dev (bind aliases share st_dev).
func (inv inventory) matches(p string, st *unix.Statx_t) bool {
	for at := p; ; at = path.Dir(at) {
		if m, ok := inv.byPath[at]; ok {
			return m.root == "/" && st.Mask&unix.STATX_MNT_ID != 0 &&
				st.Mnt_id == m.id && uint64(st.Dev_major) == m.major && uint64(st.Dev_minor) == m.minor
		}
		if at == "/" {
			return false
		}
	}
}

// An autofs trigger may mount a filesystem on the first lookup of its path, so
// it is refused wherever this guard (or the deletion it precedes) can look: on
// the ancestry of HOME or a selected root, or at or beneath HOME, which holds
// every root. An autofs elsewhere is only a pathname this guard never opens and
// cannot place content under a root: a mount is visible at its own mountpoint
// alone, and a mount at or beneath a root is refused below.
func (inv inventory) autofsClear(home string, roots []string) bool {
	for _, m := range inv.byID {
		if m.fstype != "autofs" {
			continue
		}
		for _, p := range append([]string{home}, roots...) {
			if within(p, m.point) || within(m.point, p) {
				return false
			}
		}
	}
	return true
}

func (inv inventory) isolated(home string, roots []string) bool {
	base := inv.byPath["/"]
	if base.root != "/" || !inv.autofsClear(home, roots) {
		return false
	}
	for i, root := range roots {
		if !within(root, home) {
			return false
		}
		for _, other := range roots[:i] {
			if within(root, other) || within(other, root) {
				return false
			}
		}
		for _, m := range inv.byID {
			if m.id == base.id {
				continue
			}
			// No separate mount on HOME/target ancestry or beneath a root,
			// even if it has the same device number as the parent mount.
			if within(root, m.point) || within(m.point, root) {
				return false
			}
			// With no covering mount besides /, root is its filesystem-
			// relative path. A bind of that subtree (or an ancestor) to
			// an unrelated mountpoint is still an alias of selected state.
			if m.major == base.major && m.minor == base.minor &&
				(within(m.root, root) || within(root, m.root)) {
				return false
			}
		}
	}
	return true
}

func inventoryBytes() ([]byte, error) {
	f, err := os.Open("/proc/self/mountinfo")
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return io.ReadAll(io.LimitReader(f, maxInventory+1))
}

func statFD(fd int, p string, inv inventory) (unix.Statx_t, bool) {
	var st unix.Statx_t
	err := unix.Statx(fd, "", unix.AT_EMPTY_PATH|unix.AT_SYMLINK_NOFOLLOW|unix.AT_NO_AUTOMOUNT,
		unix.STATX_BASIC_STATS|unix.STATX_MNT_ID, &st)
	return st, err == nil && inv.matches(p, &st)
}

// What a selected root turned out to be. Only a directory is opened for scanning.
type rootKind int

const (
	rootDirectory rootKind = iota
	rootSymlink
	rootAbsent
)

// Walk names from / rather than resolving an untrusted pathname. The inventory
// has already refused every autofs trigger this walk could reach. Ancestors use
// O_PATH; selected roots require O_NOATIME or refuse without enumeration.
// Missing entries and symlinks refuse without following them, except that a
// tolerant walk reports a missing entry, or a final symlink, without following
// it (the shell removes such a root as a link or not at all, never traversing it).
func openRoot(p string, inv inventory, tolerant bool) (int, rootKind, error) {
	fd, err := unix.Open("/", unix.O_PATH|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return -1, rootDirectory, errRefused
	}
	if _, ok := statFD(fd, "/", inv); !ok {
		unix.Close(fd)
		return -1, rootDirectory, errRefused
	}
	current := ""
	for _, part := range strings.Split(strings.TrimPrefix(p, "/"), "/") {
		current += "/" + part
		// O_PATH|O_NOFOLLOW gets the trigger inode without automounting it.
		// Verify its namespace identity before opening a readable directory.
		item, openErr := unix.Openat(fd, part, unix.O_PATH|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
		if openErr != nil {
			unix.Close(fd)
			if tolerant && errors.Is(openErr, unix.ENOENT) {
				return -1, rootAbsent, nil
			}
			return -1, rootDirectory, errRefused
		}
		st, ok := statFD(item, current, inv)
		if ok && tolerant && current == p && st.Mode&syscall.S_IFMT == syscall.S_IFLNK {
			unix.Close(item)
			unix.Close(fd)
			return -1, rootSymlink, nil
		}
		if !ok || st.Mode&syscall.S_IFMT != syscall.S_IFDIR {
			unix.Close(item)
			unix.Close(fd)
			return -1, rootDirectory, errRefused
		}
		flags := unix.O_PATH | unix.O_DIRECTORY | unix.O_NOFOLLOW | unix.O_CLOEXEC
		if current == p {
			flags = unix.O_RDONLY | unix.O_NOATIME | unix.O_DIRECTORY | unix.O_NOFOLLOW | unix.O_CLOEXEC
		}
		next, openErr := unix.Openat(item, ".", flags, 0)
		unix.Close(item)
		unix.Close(fd)
		if openErr != nil {
			return -1, rootDirectory, errRefused
		}
		fd = next
		if _, ok := statFD(fd, current, inv); !ok {
			unix.Close(fd)
			return -1, rootDirectory, errRefused
		}
	}
	return fd, rootDirectory, nil
}

// Directory enumeration is descriptor-relative and bounded; a root or nested
// mount with the same device is still rejected by its distinct mount ID.
func scan(fd int, p string, inv inventory, depth int, count *int) bool {
	if depth > 64 {
		unix.Close(fd)
		return false
	}
	file := os.NewFile(uintptr(fd), "")
	defer file.Close()
	for {
		entries, err := file.ReadDir(128)
		if err != nil && err != io.EOF {
			return false
		}
		for _, entry := range entries {
			*count++
			if *count > 100000 {
				return false
			}
			name := entry.Name()
			child := path.Join(p, name)
			item, openErr := unix.Openat(fd, name, unix.O_PATH|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
			if openErr != nil {
				return false
			}
			st, ok := statFD(item, child, inv)
			if !ok || st.Mode&syscall.S_IFMT == syscall.S_IFREG && st.Nlink != 1 {
				unix.Close(item)
				return false
			}
			if st.Mode&syscall.S_IFMT == syscall.S_IFDIR {
				sub, openErr := unix.Openat(item, ".", unix.O_RDONLY|unix.O_NOATIME|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
				unix.Close(item)
				if openErr != nil {
					return false
				}
				if _, ok := statFD(sub, child, inv); !ok {
					unix.Close(sub)
					return false
				}
				if !scan(sub, child, inv, depth+1, count) {
					return false
				}
			} else {
				unix.Close(item)
				if st.Mode&syscall.S_IFMT != syscall.S_IFREG && st.Mode&syscall.S_IFMT != syscall.S_IFLNK {
					return false
				}
			}
		}
		if err == io.EOF {
			return true
		}
	}
}

func inventoryStable(before, after []byte) bool {
	return len(after) != 0 && len(after) <= maxInventory && bytes.Equal(before, after)
}

// Check is the strict three-root form: all of the selected release, config and
// cache roots must exist as real directories.
func Check(home, release, config, cache string) error {
	return check(home, []string{release, config, cache}, false)
}

// CheckRoots proves one to three selected roots. Each is a real directory
// (proved and scanned as in Check), absent (nothing to prove), or a final
// symlink (not traversed). One root is the recheck made immediately before a
// destructive step: the same proofs for that root and its ancestry, from a
// freshly read inventory.
func CheckRoots(home string, roots ...string) error {
	if len(roots) < 1 || len(roots) > 3 {
		return errRefused
	}
	return check(home, roots, true)
}

func check(home string, roots []string, tolerant bool) error {
	if !validPath(home) || home == "/" {
		return errRefused
	}
	for _, root := range roots {
		if !validPath(root) || root == home || !within(root, home) {
			return errRefused
		}
	}
	before, err := inventoryBytes()
	if err != nil {
		return errRefused
	}
	inv, ok := parse(before)
	if !ok || !inv.isolated(home, roots) {
		return errRefused
	}
	homeFD, _, err := openRoot(home, inv, false)
	if err != nil {
		return errRefused
	}
	unix.Close(homeFD)
	for _, root := range roots {
		fd, kind, err := openRoot(root, inv, tolerant)
		if err != nil {
			return errRefused
		}
		if kind != rootDirectory {
			continue
		}
		count := 0
		if !scan(fd, root, inv, 0, &count) {
			return errRefused
		}
	}
	after, err := inventoryBytes()
	if err != nil || !inventoryStable(before, after) {
		return errRefused
	}
	return nil
}
