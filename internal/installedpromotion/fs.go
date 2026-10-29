//go:build linux || darwin

package installedpromotion

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"unicode"
	"unicode/utf8"

	"golang.org/x/sys/unix"
)

var errSelection = errors.New("installed selection refused")
var errSelectionCleanup = errors.New("installed selection cleanup refused")

const selectionBytes = maxBytes
const selectionNames = 256
const selectionFlags = unix.O_RDONLY | unix.O_NOFOLLOW | unix.O_NONBLOCK | unix.O_CLOEXEC

type selectionIdentity struct {
	Dev, Ino       uint64
	Mode, UID, GID uint32
}

func selectionID(s unix.Stat_t) selectionIdentity {
	return selectionIdentity{uint64(s.Dev), uint64(s.Ino), uint32(s.Mode), s.Uid, s.Gid}
}
func (i selectionIdentity) key() string {
	return fmt.Sprintf("%x:%x:%x:%x:%x", i.Dev, i.Ino, i.Mode, i.UID, i.GID)
}
func selectionStat(fd int) (unix.Stat_t, error) {
	var s unix.Stat_t
	e := unix.Fstat(fd, &s)
	return s, e
}
func selectionMode(s unix.Stat_t, directory, ancestor bool) bool {
	if directory {
		if s.Mode&unix.S_IFMT != unix.S_IFDIR || s.Mode&06000 != 0 {
			return false
		}
		if ancestor {
			return (s.Uid == 0 || s.Uid == uint32(os.Geteuid())) && (s.Mode&0022 == 0 || (s.Uid == 0 && s.Mode&01000 != 0))
		}
		return s.Uid == uint32(os.Geteuid()) && s.Mode&07777 == 0700
	}
	return s.Mode&unix.S_IFMT == unix.S_IFREG && s.Uid == uint32(os.Geteuid()) && s.Mode&07777 == 0600 && s.Nlink == 1 && s.Size >= 0 && s.Size <= selectionBytes
}
func selectionCheck(fd int, directory, ancestor bool) (selectionIdentity, error) {
	s, e := selectionStat(fd)
	if e != nil || !selectionMode(s, directory, ancestor) || selectionPlatform(fd, ancestor) != nil {
		return selectionIdentity{}, errSelection
	}
	return selectionID(s), nil
}
func selectionPath(path string) bool {
	if !utf8.ValidString(path) || !filepath.IsAbs(path) || filepath.Clean(path) != path || path == "/" || len(path) > 4096 {
		return false
	}
	parts := strings.Split(path[1:], "/")
	if len(parts) > 32 {
		return false
	}
	for _, p := range parts {
		if len(p) == 0 || len(p) > 255 {
			return false
		}
		for _, r := range p {
			if unicode.IsControl(r) {
				return false
			}
		}
	}
	return true
}

// Walk retains stable identities, not mutable directory timestamps/link counts.
func selectionRoot(path string) (int, []selectionIdentity, error) {
	if !selectionPath(path) {
		return -1, nil, errSelection
	}
	fd, e := unix.Open("/", selectionFlags|unix.O_DIRECTORY, 0)
	if e != nil {
		return -1, nil, errSelection
	}
	parts := append([]string{""}, strings.Split(path[1:], "/")...)
	ids := make([]selectionIdentity, 0, len(parts))
	for n, p := range parts {
		if n > 0 {
			var before unix.Stat_t
			if unix.Fstatat(fd, p, &before, unix.AT_SYMLINK_NOFOLLOW) != nil || !selectionMode(before, true, true) {
				unix.Close(fd)
				return -1, nil, errSelection
			}
			next, e := unix.Openat(fd, p, selectionFlags|unix.O_DIRECTORY, 0)
			if e == nil {
				after, ae := selectionStat(next)
				if ae != nil || selectionID(before) != selectionID(after) {
					unix.Close(next)
					unix.Close(fd)
					return -1, nil, errSelection
				}
			}
			ce := unix.Close(fd)
			if e != nil {
				return -1, nil, errSelection
			}
			fd = next
			if ce != nil {
				unix.Close(fd)
				return -1, nil, errSelection
			}
		}
		id, e := selectionCheck(fd, true, true)
		if e != nil || (n == len(parts)-1 && id.UID != uint32(os.Geteuid())) {
			unix.Close(fd)
			return -1, nil, errSelection
		}
		ids = append(ids, id)
	}
	return fd, ids, nil
}
func selectionChild(parent int, name string, dir bool) (int, selectionIdentity, error) {
	return selectionChildObserved(parent, name, dir, nil)
}

func selectionChildObserved(parent int, name string, dir bool, beforeOpen func()) (int, selectionIdentity, error) {
	var before unix.Stat_t
	if e := unix.Fstatat(parent, name, &before, unix.AT_SYMLINK_NOFOLLOW); e != nil {
		return -1, selectionIdentity{}, e
	}
	ps, pe := selectionStat(parent)
	if pe != nil || !selectionMode(before, dir, false) || before.Dev != ps.Dev {
		return -1, selectionIdentity{}, errSelection
	}
	if beforeOpen != nil {
		beforeOpen()
	}
	flags := selectionFlags
	if dir {
		flags |= unix.O_DIRECTORY
	}
	fd, e := unix.Openat(parent, name, flags, 0)
	if e != nil {
		return -1, selectionIdentity{}, e
	}
	id, e := selectionCheck(fd, dir, false)
	var named unix.Stat_t
	if e != nil || selectionID(before) != id || pe != nil || id.Dev != uint64(ps.Dev) || unix.Fstatat(parent, name, &named, unix.AT_SYMLINK_NOFOLLOW) != nil || selectionID(named) != id {
		unix.Close(fd)
		return -1, selectionIdentity{}, errSelection
	}
	return fd, id, nil
}
func selectionNamed(parent int, name string, id selectionIdentity, dir bool) error {
	fd, got, e := selectionChild(parent, name, dir)
	if e != nil {
		return errSelection
	}
	ce := unix.Close(fd)
	if ce != nil || got != id {
		return errSelection
	}
	return nil
}

// A bounded name-only scan never reads or removes unknown objects. All namespace
// entries count; reserved staging siblings count even if their type is unknown.
func selectionCount(fd int, prefix string) (int, error) {
	dup, e := unix.Openat(fd, ".", selectionFlags|unix.O_DIRECTORY, 0)
	if e != nil {
		return 0, errSelection
	}
	f := os.NewFile(uintptr(dup), "private selection inventory")
	names, e := f.Readdirnames(selectionNames + 1)
	ce := f.Close()
	if (e != nil && e != io.EOF) || ce != nil || len(names) > selectionNames {
		return 0, errSelection
	}
	n := 0
	for _, name := range names {
		if prefix == "" || strings.HasPrefix(name, prefix) {
			n++
		}
	}
	return n, nil
}
func selectionRemove(parent int, name string, id selectionIdentity, dir bool) error {
	if selectionNamed(parent, name, id, dir) != nil {
		return errSelectionCleanup
	}
	flags := 0
	if dir {
		flags = unix.AT_REMOVEDIR
	}
	if unix.Unlinkat(parent, name, flags) != nil {
		return errSelectionCleanup
	}
	return nil
}
