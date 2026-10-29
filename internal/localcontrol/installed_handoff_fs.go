//go:build linux || darwin

package localcontrol

import (
	"bytes"
	"encoding/json"
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

var errHandoff = errors.New("installed handoff refused")
var errHandoffCleanup = errors.New("installed handoff cleanup refused")

const handoffBytes = 2048
const handoffArtifacts = 64
const handoffNames = 256
const handoffFlags = unix.O_RDONLY | unix.O_NOFOLLOW | unix.O_NONBLOCK | unix.O_CLOEXEC

type handoffIdentity struct {
	Dev, Ino       uint64
	Mode, UID, GID uint32
}

func handoffID(s unix.Stat_t) handoffIdentity {
	return handoffIdentity{uint64(s.Dev), uint64(s.Ino), uint32(s.Mode), s.Uid, s.Gid}
}
func (i handoffIdentity) key() string {
	return fmt.Sprintf("%x:%x:%x:%x:%x", i.Dev, i.Ino, i.Mode, i.UID, i.GID)
}
func handoffStat(fd int) (unix.Stat_t, error) {
	var s unix.Stat_t
	e := unix.Fstat(fd, &s)
	return s, e
}
func handoffMode(s unix.Stat_t, directory, ancestor bool) bool {
	if directory {
		if s.Mode&unix.S_IFMT != unix.S_IFDIR || s.Mode&06000 != 0 {
			return false
		}
		if ancestor {
			return (s.Uid == 0 || s.Uid == uint32(os.Geteuid())) && (s.Mode&0022 == 0 || (s.Uid == 0 && s.Mode&01000 != 0))
		}
		return s.Uid == uint32(os.Geteuid()) && s.Mode&07777 == 0700
	}
	return s.Mode&unix.S_IFMT == unix.S_IFREG && s.Uid == uint32(os.Geteuid()) && s.Mode&07777 == 0600 && s.Nlink == 1 && s.Size >= 0 && s.Size <= handoffBytes
}
func handoffCheck(fd int, directory, ancestor bool) (handoffIdentity, error) {
	s, e := handoffStat(fd)
	if e != nil || !handoffMode(s, directory, ancestor) || handoffPlatform(fd, ancestor) != nil {
		return handoffIdentity{}, errHandoff
	}
	return handoffID(s), nil
}
func handoffPath(path string) bool {
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
func handoffRoot(path string) (int, []handoffIdentity, error) {
	if !handoffPath(path) {
		return -1, nil, errHandoff
	}
	fd, e := unix.Open("/", handoffFlags|unix.O_DIRECTORY, 0)
	if e != nil {
		return -1, nil, errHandoff
	}
	parts := append([]string{""}, strings.Split(path[1:], "/")...)
	ids := make([]handoffIdentity, 0, len(parts))
	for n, p := range parts {
		if n > 0 {
			next, e := unix.Openat(fd, p, handoffFlags|unix.O_DIRECTORY, 0)
			ce := unix.Close(fd)
			if e != nil {
				return -1, nil, errHandoff
			}
			fd = next
			if ce != nil {
				unix.Close(fd)
				return -1, nil, errHandoff
			}
		}
		id, e := handoffCheck(fd, true, n < len(parts)-1)
		if e != nil {
			unix.Close(fd)
			return -1, nil, e
		}
		ids = append(ids, id)
	}
	return fd, ids, nil
}
func handoffChild(parent int, name string, dir bool) (int, handoffIdentity, error) {
	return handoffChildObserved(parent, name, dir, nil)
}

func handoffChildObserved(parent int, name string, dir bool, beforeOpen func()) (int, handoffIdentity, error) {
	var before unix.Stat_t
	if e := unix.Fstatat(parent, name, &before, unix.AT_SYMLINK_NOFOLLOW); e != nil {
		return -1, handoffIdentity{}, e
	}
	ps, pe := handoffStat(parent)
	if pe != nil || !handoffMode(before, dir, false) || before.Dev != ps.Dev {
		return -1, handoffIdentity{}, errHandoff
	}
	if beforeOpen != nil {
		beforeOpen()
	}
	flags := handoffFlags
	if dir {
		flags |= unix.O_DIRECTORY
	}
	fd, e := unix.Openat(parent, name, flags, 0)
	if e != nil {
		return -1, handoffIdentity{}, e
	}
	id, e := handoffCheck(fd, dir, false)
	var named unix.Stat_t
	if e != nil || handoffID(before) != id || pe != nil || id.Dev != uint64(ps.Dev) || unix.Fstatat(parent, name, &named, unix.AT_SYMLINK_NOFOLLOW) != nil || handoffID(named) != id {
		unix.Close(fd)
		return -1, handoffIdentity{}, errHandoff
	}
	return fd, id, nil
}
func handoffNamed(parent int, name string, id handoffIdentity, dir bool) error {
	fd, got, e := handoffChild(parent, name, dir)
	if e != nil {
		return errHandoff
	}
	ce := unix.Close(fd)
	if ce != nil || got != id {
		return errHandoff
	}
	return nil
}
func handoffDecode(data []byte, out any) error {
	if len(data) == 0 || len(data) > handoffBytes || decodeControlJSON(data, out) != nil {
		return errHandoff
	}
	var a, b map[string]json.RawMessage
	canonical, e := json.Marshal(out)
	if e != nil || json.Unmarshal(data, &a) != nil || json.Unmarshal(canonical, &b) != nil || a == nil || len(a) != len(b) {
		return errHandoff
	}
	for k, v := range a {
		if _, ok := b[k]; !ok || bytes.Equal(bytes.TrimSpace(v), []byte("null")) {
			return errHandoff
		}
	}
	return nil
}
func handoffRead(parent int, name string, out any) (id handoffIdentity, result error) {
	fd, id, e := handoffChild(parent, name, false)
	if e != nil {
		return id, e
	}
	f := os.NewFile(uintptr(fd), "private handoff")
	defer func() {
		if f.Close() != nil {
			result = errHandoff
		}
	}()
	before, e := handoffStat(fd)
	if e != nil {
		return id, errHandoff
	}
	data, e := io.ReadAll(io.LimitReader(f, handoffBytes+1))
	if e != nil || handoffDecode(data, out) != nil {
		return id, errHandoff
	}
	after, e := handoffStat(fd)
	if e != nil || handoffID(before) != handoffID(after) || before.Size != after.Size || before.Nlink != after.Nlink || before.Mtim != after.Mtim || before.Ctim != after.Ctim || handoffNamed(parent, name, id, false) != nil {
		return id, errHandoff
	}
	return id, nil
}

// A bounded name-only scan never reads or removes unknown objects. All namespace
// entries count; reserved staging siblings count even if their type is unknown.
func handoffCount(fd int, prefix string) (int, error) {
	dup, e := unix.Openat(fd, ".", handoffFlags|unix.O_DIRECTORY, 0)
	if e != nil {
		return 0, errHandoff
	}
	f := os.NewFile(uintptr(dup), "private handoff inventory")
	names, e := f.Readdirnames(handoffNames + 1)
	ce := f.Close()
	if (e != nil && e != io.EOF) || ce != nil || len(names) > handoffNames {
		return 0, errHandoff
	}
	n := 0
	for _, name := range names {
		if prefix == "" || strings.HasPrefix(name, prefix) {
			n++
		}
	}
	return n, nil
}
func handoffBudget(root, ns int, need int) error {
	a, e := handoffCount(root, ".ic-")
	if e != nil {
		return e
	}
	b := 0
	if ns >= 0 {
		b, e = handoffCount(ns, "")
		if e != nil {
			return e
		}
	}
	// Each generation/staging reserves its maximum bounded three leaves as well
	// as the directory. Unknown reserved entries cost the same conservative unit.
	if (a+b)*4+need > handoffArtifacts {
		return errHandoff
	}
	return nil
}
func handoffRemove(parent int, name string, id handoffIdentity, dir bool) error {
	if handoffNamed(parent, name, id, dir) != nil {
		return errHandoffCleanup
	}
	flags := 0
	if dir {
		flags = unix.AT_REMOVEDIR
	}
	if unix.Unlinkat(parent, name, flags) != nil {
		return errHandoffCleanup
	}
	return nil
}
