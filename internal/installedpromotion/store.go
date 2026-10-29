//go:build linux || darwin

package installedpromotion

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"time"

	"github.com/0cv/herdr-mobile-relay/internal/config"
	"golang.org/x/sys/unix"
)

// A shared fixed child of the XDG config root, NOT the application's runtime.
// Installation names are SHA-256 prefixes, never unescaped installation IDs.
const namespace = ".herdr-installed-selections-v1"
const artifactLimit = 64

type Outcome string

const (
	Unpublished Outcome = "unpublished"
	Durable     Outcome = "durable"
	Uncertain   Outcome = "uncertain"
)

// Result must be retained on every error: effects can precede cancellation,
// directory sync or close failure. No operation automatically retries publication.
type Result struct {
	Reference Reference
	Outcome   Outcome
}

func (Result) String() string     { return "installed selection result [redacted]" }
func (r Result) GoString() string { return r.String() }

type store struct {
	anchor    string
	root, dir int
	reserved  int
	published bool // sticky knowledge of this call's namespace publication
	ids       []selectionIdentity
	id        selectionIdentity
}

func anchorOK(anchor, installation string) bool {
	if !selectionPath(anchor) || installation == "" || len(installation) > 128 {
		return false
	}
	for _, c := range installation {
		if c < 0x21 || c > 0x7e || c == '/' || c == '\\' {
			return false
		}
	}
	return true
}
func overlap(a, b string) bool {
	return a == b || strings.HasPrefix(a, b+"/") || strings.HasPrefix(b, a+"/")
}
func selectionOK(c config.Config, anchor, installation string) bool {
	if c.ConfigHome != anchor || c.InstanceID != installation {
		return false
	}
	root := filepath.Join(anchor, namespace)
	for _, p := range []string{c.RuntimeDir, c.CacheDir, c.ReleaseRoot, c.TailscaleCLIStateRoot, c.TailscaleCLICoordinationRoot} {
		// Binding checks canonical aliases; resolve the existing ancestors for overlap.
		q := p
		var suffix []string
		for {
			_, e := os.Lstat(q)
			if !errors.Is(e, os.ErrNotExist) {
				break
			}
			suffix = append(suffix, filepath.Base(q))
			parent := filepath.Dir(q)
			if parent == q {
				return false
			}
			q = parent
		}
		q, e := filepath.EvalSymlinks(q)
		if e != nil {
			return false
		}
		for i := len(suffix) - 1; i >= 0; i-- {
			q = filepath.Join(q, suffix[i])
		}
		if overlap(root, q) {
			return false
		}
	}
	return true
}
func (s *store) anchorUnchanged() bool {
	fd, ids, e := selectionRoot(s.anchor)
	if e != nil {
		return false
	}
	ce := unix.Close(fd)
	return ce == nil && reflect.DeepEqual(ids, s.ids)
}
func (s *store) validate() error {
	fd, ids, e := selectionRoot(s.anchor)
	if e != nil {
		return ErrRefused
	}
	ce := unix.Close(fd)
	if ce != nil || !reflect.DeepEqual(ids, s.ids) || selectionNamed(s.root, namespace, s.id, true) != nil || s.owner() != nil {
		return ErrRefused
	}
	return nil
}
func (s *store) ownerBytes() []byte {
	return []byte("installed-selections-v1\n" + s.ids[len(s.ids)-1].key() + "\n" + s.id.key() + "\n")
}
func (s *store) owner() error {
	fd, id, e := selectionChild(s.dir, "owner", false)
	if e != nil {
		return ErrRefused
	}
	file := os.NewFile(uintptr(fd), "selection namespace attribution")
	data, e := io.ReadAll(io.LimitReader(file, 513))
	ce := file.Close()
	if e != nil || ce != nil || string(data) != string(s.ownerBytes()) || selectionNamed(s.dir, "owner", id, false) != nil {
		return ErrRefused
	}
	return nil
}
func (s *store) close() error {
	var e error
	for _, fd := range []int{s.dir, s.root} {
		if fd >= 0 && unix.Close(fd) != nil {
			e = ErrRefused
		}
	}
	s.dir = -1
	s.root = -1
	return e
}

// Private per-call observer, used only after a real nonblocking contention result.
type lockObserverKey struct{}

func lock(ctx context.Context, fd int) error {
	for {
		if ctx.Err() != nil {
			return ErrRefused
		}
		e := unix.Flock(fd, unix.LOCK_EX|unix.LOCK_NB)
		if e == nil {
			return nil
		}
		if e != unix.EWOULDBLOCK && e != unix.EAGAIN {
			return ErrRefused
		}
		if observe, ok := ctx.Value(lockObserverKey{}).(func(int)); ok {
			observe(fd)
		}
		timer := time.NewTimer(10 * time.Millisecond)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ErrRefused
		case <-timer.C:
		}
	}
}
func openStore(ctx context.Context, anchor string, create bool) (*store, error) {
	return openStoreFaults(ctx, anchor, create, faults{}, "")
}
func openStoreFaults(ctx context.Context, anchor string, create bool, f faults, generation string) (*store, error) {
	fd, ids, e := selectionRoot(anchor)
	if e != nil {
		return nil, ErrRefused
	}
	s := &store{anchor: anchor, root: fd, dir: -1, ids: ids}
	fail := func(err error) (*store, error) {
		if s.close() != nil || s.published {
			err = ErrUncertain
		}
		return nil, err
	}
	// Lock the existing anchor, then its child, in one fixed order. No lock file,
	// lock reclamation, PID inference, detached waiter or mutable pointer exists.
	if lock(ctx, fd) != nil {
		return fail(ErrRefused)
	}
	stages, e := selectionCount(fd, namespace+"-")
	if e != nil || stages*2+2 > artifactLimit {
		return fail(ErrRefused)
	}
	s.reserved = stages * 2
	s.dir, s.id, e = selectionChild(fd, namespace, true)
	if errors.Is(e, unix.ENOENT) && create {
		if !hexID(generation, 32) {
			return fail(ErrRefused)
		}
		if e = s.initialize(ctx, generation, f); e != nil {
			return fail(e)
		}
	} else if e != nil {
		return fail(ErrRefused)
	}
	if f.check(ctx, "admission-lock") != nil || lock(ctx, s.dir) != nil || f.check(ctx, "admission-validation") != nil || s.validate() != nil || ctx.Err() != nil {
		return fail(ErrRefused)
	}
	if create && !s.published {
		// Recognition is not a durability acknowledgement. Fence the original
		// retained root/namespace attribution before permitting a new capsule.
		if s.validate() != nil || f.parentSync(ctx, s.root, "reuse-parent-sync") != nil || s.validate() != nil {
			return fail(ErrUncertain)
		}
	}
	if f.check(ctx, "admission-inventory") != nil {
		return fail(ErrRefused)
	}
	n, e := selectionCount(s.dir, "")
	if e != nil || n+stages*2 > artifactLimit {
		return fail(ErrRefused)
	}
	return s, nil
}

// initialize publishes only a completely synced, attributed namespace. On an
// entered rename or sync/close ambiguity it retains evidence; known unpublished
// identities alone are eligible for nonrecursive cleanup.
func (s *store) initialize(ctx context.Context, generation string, f faults) (result error) {
	stage := namespace + "-" + generation
	var ownerID selectionIdentity
	entered := false
	defer func() {
		if result == nil || entered {
			return
		}
		safe := s.anchorUnchanged()
		if s.dir >= 0 {
			safe = safe && selectionNamed(s.root, stage, s.id, true) == nil
			if safe && ownerID != (selectionIdentity{}) {
				safe = selectionRemove(s.dir, "owner", ownerID, false) == nil
			}
			if safe {
				safe = unix.Fsync(s.dir) == nil && selectionRemove(s.root, stage, s.id, true) == nil && unix.Fsync(s.root) == nil
			}
		}
		if !safe {
			result = ErrUncertain
		}
	}()
	if f.check(ctx, "namespace-create") != nil {
		return ErrRefused
	}
	if unix.Mkdirat(s.root, stage, 0700) != nil {
		return ErrRefused
	}
	s.dir, s.id, result = selectionChild(s.root, stage, true)
	if result != nil {
		return ErrUncertain
	}
	ownerFD, e := unix.Openat(s.dir, "owner", unix.O_WRONLY|unix.O_CREAT|unix.O_EXCL|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0600)
	if e != nil {
		return ErrRefused
	}
	owner := os.NewFile(uintptr(ownerFD), "selection namespace attribution")
	ownerID, e = selectionCheck(ownerFD, false, false)
	if e == nil {
		data := s.ownerBytes()
		n, we := owner.Write(data)
		if we != nil || n != len(data) {
			e = ErrRefused
		}
	}
	if e == nil {
		e = f.check(ctx, "owner-sync")
		if e == nil {
			e = owner.Sync()
		}
	}
	ce := owner.Close()
	if e != nil || ce != nil {
		return ErrRefused
	}
	if f.check(ctx, "namespace-sync") != nil || unix.Fsync(s.dir) != nil || !s.anchorUnchanged() || selectionNamed(s.root, stage, s.id, true) != nil || s.owner() != nil || f.check(ctx, "namespace-rename") != nil {
		return ErrRefused
	}
	entered = true
	s.published = true
	rename := f.namespaceRename
	if rename == nil {
		rename = selectionExclusive
	}
	if e = rename(s.root, stage, namespace); e != nil {
		if errors.Is(e, unix.EEXIST) {
			entered = false
			s.published = false
			return ErrRefused
		}
		return ErrUncertain
	}
	if f.parentSync(ctx, s.root, "namespace-parent-sync") != nil || s.validate() != nil || ctx.Err() != nil {
		return ErrUncertain
	}
	return nil
}
func matches(parent int, leaf string, expected selectionIdentity, hash string) bool {
	fd, id, e := selectionChild(parent, leaf, false)
	if e != nil {
		return false
	}
	file := os.NewFile(uintptr(fd), "private selection verification")
	before, be := selectionStat(fd)
	data, re := io.ReadAll(io.LimitReader(file, maxBytes+1))
	after, ae := selectionStat(fd)
	_, check := selectionCheck(fd, false, false)
	ce := file.Close()
	return be == nil && re == nil && ae == nil && check == nil && ce == nil && id == expected && selectionID(before) == selectionID(after) && before.Size == after.Size && before.Mtim == after.Mtim && before.Ctim == after.Ctim && before.Nlink == after.Nlink && len(data) <= maxBytes && digest(data) == hash && selectionNamed(parent, leaf, id, false) == nil
}
func name(installation string, r Reference) string {
	return "i" + digest([]byte(installation)) + "-g" + r.Generation
}

type faults struct {
	point           func(string) error
	rename          func(int, string, string) error
	namespaceRename func(int, string, string) error
	syncParent      func(int) error
}

func (f faults) parentSync(ctx context.Context, fd int, point string) error {
	if f.check(ctx, point) != nil {
		return ErrRefused
	}
	sync := f.syncParent
	if sync == nil {
		sync = unix.Fsync
	}
	if sync(fd) != nil {
		return ErrRefused
	}
	return f.check(ctx, point+"-complete")
}

func (f faults) check(ctx context.Context, p string) error {
	if ctx.Err() != nil {
		return ErrRefused
	}
	if f.point != nil && f.point(p) != nil {
		return ErrRefused
	}
	if ctx.Err() != nil {
		return ErrRefused
	}
	return nil
}

// Prepare saves a fresh immutable full snapshot. The caller supplies the ORIGINAL
// retained binding and original ConfigHome/installation independently. It creates
// no application roots and does not publish routes or confer any authority.
func Prepare(ctx context.Context, selected config.InstalledConfig, anchor, installation string) (Result, error) {
	var entropy [16]byte
	if _, e := rand.Read(entropy[:]); e != nil {
		return Result{}, ErrRefused
	}
	return prepare(ctx, selected, anchor, installation, hex.EncodeToString(entropy[:]), faults{})
}
func prepare(ctx context.Context, selected config.InstalledConfig, anchor, installation, generation string, f faults) (result Result, err error) {
	result.Outcome = Unpublished
	if ctx == nil || ctx.Err() != nil || !anchorOK(anchor, installation) {
		return result, ErrRefused
	}
	c := selected.Snapshot()
	if !bounded(c) {
		return result, ErrRefused
	}
	pin, e := selected.Binding().Fingerprint()
	if e != nil || !selectionOK(c, anchor, installation) {
		return result, ErrRefused
	}
	data, e := encode(c, generation, hex.EncodeToString(pin[:]))
	if e != nil {
		return result, ErrRefused
	}
	result.Reference = Reference{generation, digest(data)}
	s, e := openStoreFaults(ctx, anchor, true, f, generation)
	if e != nil {
		if errors.Is(e, ErrUncertain) {
			result.Outcome = Uncertain
		}
		return result, e
	}
	temp := "t" + name(installation, result.Reference)
	final := name(installation, result.Reference)
	var captured selectionIdentity
	entered := false
	defer func() {
		if err != nil && !entered && captured != (selectionIdentity{}) {
			if s.validate() != nil || selectionRemove(s.dir, temp, captured, false) != nil || unix.Fsync(s.dir) != nil {
				result.Outcome = Uncertain
				err = ErrUncertain
			}
		}
		if s.close() != nil {
			result.Outcome = Uncertain
			err = ErrUncertain
		}
		if !s.anchorUnchanged() || selected.Validate(&c) != nil || f.check(ctx, "return") != nil {
			if entered || s.published || result.Outcome == Uncertain {
				result.Outcome = Uncertain
				err = ErrUncertain
			} else {
				err = ErrRefused
			}
		}
		if err != nil && s.published {
			result.Outcome = Uncertain
			err = ErrUncertain
		}
	}()
	n, e := selectionCount(s.dir, "")
	if e != nil || n+s.reserved >= artifactLimit || f.check(ctx, "create") != nil {
		return result, ErrRefused
	}
	var existing unix.Stat_t
	if e = unix.Fstatat(s.dir, final, &existing, unix.AT_SYMLINK_NOFOLLOW); e != unix.ENOENT {
		return result, ErrRefused
	}
	fd, e := unix.Openat(s.dir, temp, unix.O_WRONLY|unix.O_CREAT|unix.O_EXCL|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0600)
	if e != nil {
		return result, ErrRefused
	}
	file := os.NewFile(uintptr(fd), "private selection output")
	captured, e = selectionCheck(fd, false, false)
	if e != nil || selectionNamed(s.dir, temp, captured, false) != nil {
		file.Close()
		result.Outcome = Uncertain
		return result, ErrUncertain
	}
	writeErr := f.check(ctx, "write")
	if writeErr == nil {
		n, e := file.Write(data)
		if e != nil || n != len(data) {
			writeErr = ErrRefused
		}
	}
	if writeErr == nil && (f.check(ctx, "file-sync") != nil || file.Sync() != nil) {
		writeErr = ErrRefused
	}
	if file.Close() != nil || f.check(ctx, "file-close") != nil {
		writeErr = ErrRefused
	}
	if writeErr != nil {
		return result, ErrRefused
	}
	if s.validate() != nil || selected.Validate(&c) != nil || !matches(s.dir, temp, captured, result.Reference.Digest) || f.check(ctx, "rename") != nil {
		return result, ErrRefused
	}
	entered = true
	result.Outcome = Uncertain
	rename := f.rename
	if rename == nil {
		rename = selectionExclusive
	}
	if e = rename(s.dir, temp, final); e != nil {
		if errors.Is(e, unix.EEXIST) {
			entered = false
			result.Outcome = Unpublished
			return result, ErrRefused
		}
		return result, ErrUncertain
	}
	if f.check(ctx, "directory-sync") != nil || unix.Fsync(s.dir) != nil || s.validate() != nil || !matches(s.dir, final, captured, result.Reference.Digest) {
		return result, ErrUncertain
	}
	result.Outcome = Durable
	return result, nil
}

// Read accepts only an independently retained reference, not a current pointer
// or an in-file self-reported digest. Every failure returns a zero selection.
func Read(ctx context.Context, anchor, installation string, r Reference) (config.InstalledConfig, error) {
	return read(ctx, anchor, installation, r, faults{})
}

func read(ctx context.Context, anchor, installation string, r Reference, f faults) (selected config.InstalledConfig, err error) {
	if ctx == nil || ctx.Err() != nil || !anchorOK(anchor, installation) || !hexID(r.Generation, 32) || !hexID(r.Digest, 64) {
		return selected, ErrRefused
	}
	s, e := openStore(ctx, anchor, false)
	if e != nil {
		return selected, ErrRefused
	}
	defer func() {
		if s.validate() != nil {
			err = ErrRefused
		}
		if s.close() != nil {
			err = ErrRefused
		}
		if !s.anchorUnchanged() || f.check(ctx, "read-final-binding") != nil {
			err = ErrRefused
		}
		if err == nil {
			c := selected.Snapshot()
			if selected.Validate(&c) != nil {
				err = ErrRefused
			}
		}
		if ctx.Err() != nil {
			err = ErrRefused
		}
		if err != nil {
			selected = config.InstalledConfig{}
		}
	}()
	leaf := name(installation, r)
	fd, id, e := selectionChild(s.dir, leaf, false)
	if e != nil {
		return selected, ErrRefused
	}
	file := os.NewFile(uintptr(fd), "private selection input")
	before, e := selectionStat(fd)
	if e != nil {
		file.Close()
		return selected, ErrRefused
	}
	data, e := io.ReadAll(io.LimitReader(file, maxBytes+1))
	after, ae := selectionStat(fd)
	_, check := selectionCheck(fd, false, false)
	ce := file.Close()
	if e != nil || ae != nil || check != nil || ce != nil || selectionID(before) != selectionID(after) || before.Size != after.Size || before.Mtim != after.Mtim || before.Ctim != after.Ctim || before.Nlink != after.Nlink || digest(data) != r.Digest || selectionNamed(s.dir, leaf, id, false) != nil {
		return selected, ErrRefused
	}
	c, w, e := decode(data)
	if e != nil || w.Generation != r.Generation || w.Anchor != anchor || w.Installation != installation || !selectionOK(c, anchor, installation) {
		return selected, ErrRefused
	}
	selected, e = config.BindInstalledCLI(&c)
	if e != nil {
		return config.InstalledConfig{}, ErrRefused
	}
	pin, e := selected.Binding().Fingerprint()
	if e != nil || hex.EncodeToString(pin[:]) != w.Pin || s.validate() != nil || ctx.Err() != nil {
		return config.InstalledConfig{}, ErrRefused
	}
	return selected, nil
}
