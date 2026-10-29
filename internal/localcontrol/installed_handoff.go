//go:build linux || darwin

package localcontrol

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/0cv/herdr-mobile-relay/internal/config"
	"golang.org/x/sys/unix"
)

// InstalledHandoffOutcome describes completed filesystem operations, not service
// ownership, listener readiness or a hardware power-loss guarantee.
type InstalledHandoffOutcome string

const (
	InstalledHandoffUnpublished InstalledHandoffOutcome = "unpublished"
	InstalledHandoffPublished   InstalledHandoffOutcome = "published"
	InstalledHandoffUncertain   InstalledHandoffOutcome = "uncertain"
)

var errHandoffUncertain = errors.New("installed handoff durability uncertain")

type handoffOwner struct {
	Format       string `json:"format"`
	Installation string `json:"installation"`
	Root         string `json:"root"`
	Namespace    string `json:"namespace"`
}
type handoffLocator struct {
	Protocol     string `json:"protocol"`
	Installation string `json:"installation"`
	Run          string `json:"run"`
}
type handoffCredential struct {
	Protocol     string `json:"protocol"`
	Installation string `json:"installation"`
	Run          string `json:"run"`
	Capability   string `json:"capability"`
}

func (handoffCredential) String() string     { return "handoff record [redacted]" }
func (r handoffCredential) GoString() string { return r.String() }

// InstalledHandoff copies share retirement. Only future concrete owned endpoint
// composition may publish, AFTER binding. This helper grants no service authority.
type InstalledHandoff struct{ state *handoffState }

func (InstalledHandoff) String() string     { return "installed handoff [redacted]" }
func (h InstalledHandoff) GoString() string { return h.String() }

type InstalledHandoffSnapshot struct {
	credentials InstalledCredentials
	socket      string
}

func (InstalledHandoffSnapshot) String() string                      { return "installed handoff snapshot [redacted]" }
func (s InstalledHandoffSnapshot) GoString() string                  { return s.String() }
func (s InstalledHandoffSnapshot) Credentials() InstalledCredentials { return s.credentials }
func (s InstalledHandoffSnapshot) SocketPath() string                { return s.socket }

type handoffState struct {
	selected                                              config.InstalledConfig
	credentials                                           InstalledCredentials
	path                                                  string
	ancestors                                             []handoffIdentity
	root, ns, gen                                         int
	nsID, genID, ownerID, credentialID, currentID, tempID handoffIdentity
	nsName, tempName                                      string
	owner                                                 handoffOwner
	gate                                                  chan struct{}
	retired                                               atomic.Bool
	done                                                  chan struct{}
	once                                                  sync.Once
	closeErr                                              error
	// Per-instance private fault seam, never an exported policy/fixture bypass.
	fault            func(string) error
	prepared         bool
	recognized       bool
	publication      atomic.Uint32
	initAmbiguous    bool
	publishAmbiguous bool
	rename           func(int, string, string, bool) error
	queued           func()
}

func handoffHex(s string, n int) bool {
	if len(s) != n {
		return false
	}
	for _, c := range s {
		if !(c >= '0' && c <= '9') && !(c >= 'a' && c <= 'f') {
			return false
		}
	}
	return true
}
func handoffCredentials(c InstalledCredentials) bool {
	return validIdentity(c.Installation) && handoffHex(c.Run, 32) && handoffHex(c.Capability, 64)
}
func (s *handoffState) point(ctx context.Context, name string) error {
	if ctx == nil || ctx.Err() != nil || s.retired.Load() {
		return errHandoff
	}
	if s.fault != nil && s.fault(name) != nil {
		return errHandoff
	}
	if ctx.Err() != nil || s.retired.Load() {
		return errHandoff
	}
	return nil
}
func (s *handoffState) binding() error {
	cfg := s.selected.Snapshot()
	if s.selected.Validate(&cfg) != nil || cfg.InstanceID != s.credentials.Installation {
		return errHandoff
	}
	canonical, e := filepath.EvalSymlinks(cfg.RuntimeDir)
	if e != nil || canonical != s.path {
		return errHandoff
	}
	fd, ids, e := handoffRoot(s.path)
	if e != nil {
		return errHandoff
	}
	ce := unix.Close(fd)
	if ce != nil || !reflect.DeepEqual(ids, s.ancestors) || s.selected.Validate(&cfg) != nil {
		return errHandoff
	}
	return nil
}
func (s *handoffState) validate() error {
	if s.binding() != nil || handoffNamed(s.root, s.nsName, s.nsID, true) != nil {
		return errHandoff
	}
	var owner handoffOwner
	id, e := handoffRead(s.ns, "owner", &owner)
	if e != nil || id != s.ownerID || owner != s.owner {
		return errHandoff
	}
	if s.gen >= 0 {
		if handoffNamed(s.ns, "g"+s.credentials.Run, s.genID, true) != nil {
			return errHandoff
		}
		var c handoffCredential
		id, e := handoffRead(s.gen, "c", &c)
		if e != nil || id != s.credentialID || c != s.record() {
			return errHandoff
		}
	}
	return nil
}
func (s *handoffState) record() handoffCredential {
	c := s.credentials
	return handoffCredential{InstalledProtocol, c.Installation, c.Run, c.Capability}
}
func (s *handoffState) write(ctx context.Context, parent int, name string, value any, stage string) (id handoffIdentity, result error) {
	if s.point(ctx, stage+"-before-create") != nil {
		return id, errHandoff
	}
	data, e := json.Marshal(value)
	if e != nil || len(data) > handoffBytes {
		return id, errHandoff
	}
	fd, e := unix.Openat(parent, name, unix.O_WRONLY|unix.O_CREAT|unix.O_EXCL|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0600)
	if e != nil {
		return id, errHandoff
	}
	f := os.NewFile(uintptr(fd), "private handoff output")
	defer func() {
		if f.Close() != nil {
			result = errors.Join(result, errHandoff)
		}
		if s.point(ctx, stage+"-closed") != nil {
			result = errors.Join(result, errHandoff)
		}
	}()
	id, e = handoffCheck(fd, false, false)
	ps, pe := handoffStat(parent)
	if e != nil || pe != nil || id.Dev != uint64(ps.Dev) || handoffNamed(parent, name, id, false) != nil {
		return id, errHandoff
	}
	if s.point(ctx, stage+"-write") != nil {
		return id, errHandoff
	}
	if n, e := f.Write(data); e != nil || n != len(data) {
		return id, errHandoff
	}
	if s.point(ctx, stage+"-sync") != nil || f.Sync() != nil || handoffNamed(parent, name, id, false) != nil {
		return id, errHandoff
	}
	return id, nil
}

// PrepareInstalledHandoff requires the ORIGINAL retained selection and actual
// core credentials. RuntimeDir must already exist. On error a nonzero handle
// still owns captured artifacts and MUST be closed; cleanup errors are separate.
// Established namespaces survive Close. No prior generation is adopted/deleted.
func PrepareInstalledHandoff(ctx context.Context, selected config.InstalledConfig, c InstalledCredentials) (InstalledHandoff, error) {
	return prepareInstalledHandoff(ctx, selected, c, nil)
}
func prepareInstalledHandoff(ctx context.Context, selected config.InstalledConfig, c InstalledCredentials, fault func(string) error) (h InstalledHandoff, result error) {
	return prepareHandoffRename(ctx, selected, c, fault, nil)
}
func prepareHandoffRename(ctx context.Context, selected config.InstalledConfig, c InstalledCredentials, fault func(string) error, rename func(int, string, string, bool) error) (h InstalledHandoff, result error) {
	cfg := selected.Snapshot()
	if ctx == nil || ctx.Err() != nil || !handoffCredentials(c) || cfg.InstanceID != c.Installation || selected.Validate(&cfg) != nil {
		return h, errHandoff
	}
	path, e := filepath.EvalSymlinks(cfg.RuntimeDir)
	if e != nil || !handoffPath(path) || len(filepath.Join(path, "ic", "g"+c.Run, "s")) > 103 || selected.Validate(&cfg) != nil {
		return h, errHandoff
	}
	root, ids, e := handoffRoot(path)
	if e != nil {
		return h, errHandoff
	}
	s := &handoffState{selected: selected, credentials: c, path: path, ancestors: ids, root: root, ns: -1, gen: -1, nsName: "ic", gate: make(chan struct{}, 1), done: make(chan struct{}), fault: fault, rename: rename}
	h.state = s
	if s.binding() != nil || s.point(ctx, "prepare") != nil {
		return h, errHandoff
	}
	s.ns, s.nsID, e = handoffChild(root, "ic", true)
	if errors.Is(e, unix.ENOENT) {
		if handoffBudget(root, -1, 8) != nil {
			return h, errHandoff
		}
		s.nsName = ".ic-" + c.Run
		if unix.Mkdirat(root, s.nsName, 0700) != nil {
			return h, errHandoff
		}
		s.ns, s.nsID, e = handoffChild(root, s.nsName, true)
		if e != nil {
			return h, errHandoff
		}
		s.recognized = true // exclusively created and captured staging only
		s.owner = handoffOwner{"installed-handoff-v1", c.Installation, ids[len(ids)-1].key(), s.nsID.key()}
		s.ownerID, e = s.write(ctx, s.ns, "owner", s.owner, "owner")
		if e != nil {
			return h, e
		}
		if s.point(ctx, "init-directory-sync") != nil || unix.Fsync(s.ns) != nil || s.validate() != nil || s.point(ctx, "init-rename") != nil {
			return h, errHandoff
		}
		if e := s.renameInto(root, s.nsName, "ic", true); e != nil {
			if errors.Is(e, unix.EEXIST) {
				return h, errHandoff
			}
			s.initAmbiguous = true
			return h, errHandoffUncertain
		}
		s.nsName = "ic"
		if s.point(ctx, "init-parent-sync") != nil || unix.Fsync(root) != nil || s.point(ctx, "init-complete") != nil {
			return h, errHandoffUncertain
		}
	} else if e != nil {
		return h, errHandoff
	} else {
		s.ownerID, e = handoffRead(s.ns, "owner", &s.owner)
		expected := handoffOwner{"installed-handoff-v1", c.Installation, ids[len(ids)-1].key(), s.nsID.key()}
		if e != nil || s.owner != expected {
			return h, errHandoff
		}
		s.recognized = true
		if s.validate() != nil || s.point(ctx, "reuse-parent-sync") != nil || unix.Fsync(root) != nil || s.point(ctx, "reuse-parent-synced") != nil {
			return h, errHandoffUncertain
		}
	}
	if s.validate() != nil || handoffBudget(root, s.ns, 4) != nil || s.point(ctx, "generation-create") != nil {
		return h, errHandoff
	}
	if unix.Mkdirat(s.ns, "g"+c.Run, 0700) != nil {
		return h, errHandoff
	}
	s.gen, s.genID, e = handoffChild(s.ns, "g"+c.Run, true)
	if e != nil {
		return h, errHandoff
	}
	s.credentialID, e = s.write(ctx, s.gen, "c", s.record(), "credential")
	if e != nil {
		return h, e
	}
	if s.point(ctx, "generation-sync") != nil || unix.Fsync(s.gen) != nil || unix.Fsync(s.ns) != nil || s.validate() != nil || s.point(ctx, "prepared") != nil {
		return h, errHandoff
	}
	s.prepared = true
	return h, nil
}
func (h InstalledHandoff) enter(ctx context.Context) (*handoffState, error) {
	s := h.state
	if s == nil || ctx == nil || s.retired.Load() {
		return nil, errHandoff
	}
	select {
	case s.gate <- struct{}{}:
	default:
		if s.queued != nil {
			s.queued()
		}
		select {
		case s.gate <- struct{}{}:
		case <-ctx.Done():
			return nil, errHandoff
		case <-s.done:
			return nil, errHandoff
		}
	}
	if s.point(ctx, "operation") != nil || !s.prepared || s.gen < 0 || s.validate() != nil {
		<-s.gate
		return nil, errHandoff
	}
	return s, nil
}
func (h InstalledHandoff) SocketPath(ctx context.Context) (string, error) {
	s, e := h.enter(ctx)
	if e != nil {
		return "", e
	}
	defer func() { <-s.gate }()
	path := filepath.Join(s.path, "ic", "g"+s.credentials.Run, "s")
	if s.point(ctx, "socket-path") != nil {
		return "", errHandoff
	}
	return path, nil
}

func (s *handoffState) renameInto(fd int, from, to string, exclusive bool) error {
	if s.rename != nil {
		return s.rename(fd, from, to, exclusive)
	}
	if exclusive {
		return handoffExclusive(fd, from, to)
	}
	return unix.Renameat(fd, from, fd, to)
}
func (h InstalledHandoff) retainedOutcome() InstalledHandoffOutcome {
	if h.state != nil && h.state.publication.Load() != 0 {
		return InstalledHandoffUncertain
	}
	return InstalledHandoffUnpublished
}

// Published acknowledges this call only. Refusals retain uncertainty after any
// possible publication, including retirement. Entered rename is recorded before
// dispatch; only a known exclusive collision clears provisional uncertainty.
// PublishDiscovery must be called only by the future concrete owned endpoint,
// after bind. Valid old discovery is data, NEVER evidence its service is dead.
func (h InstalledHandoff) PublishDiscovery(ctx context.Context) (InstalledHandoffOutcome, error) {
	s, e := h.enter(ctx)
	if e != nil {
		return h.retainedOutcome(), e
	}
	defer func() { <-s.gate }()
	outcome := InstalledHandoffUnpublished
	if s.publication.Load() != 0 {
		return InstalledHandoffUncertain, errHandoff
	}
	var old handoffLocator
	oldID, e := handoffRead(s.ns, "current", &old)
	absent := errors.Is(e, unix.ENOENT)
	if !absent && (e != nil || !handoffLocatorOK(old, s.credentials.Installation)) {
		return outcome, errHandoff
	}
	if handoffBudget(s.root, s.ns, 4) != nil || s.tempID != (handoffIdentity{}) {
		return outcome, errHandoff
	}
	s.tempName = "t" + s.credentials.Run
	s.tempID, e = s.write(ctx, s.ns, s.tempName, handoffLocator{InstalledProtocol, s.credentials.Installation, s.credentials.Run}, "current")
	if e != nil {
		return outcome, e
	}
	if s.validate() != nil || s.point(ctx, "current-rename") != nil {
		return outcome, errHandoff
	}
	var staged handoffLocator
	stagedID, stagedErr := handoffRead(s.ns, s.tempName, &staged)
	if stagedErr != nil || stagedID != s.tempID || staged != (handoffLocator{InstalledProtocol, s.credentials.Installation, s.credentials.Run}) {
		return outcome, errHandoff
	}
	if absent {
		s.publication.Store(1)
		e = s.renameInto(s.ns, s.tempName, "current", true)
	} else {
		var again handoffLocator
		id, re := handoffRead(s.ns, "current", &again)
		if re != nil || id != oldID || again != old {
			return outcome, errHandoff
		}
		s.publication.Store(1)
		e = s.renameInto(s.ns, s.tempName, "current", false)
	}
	if e != nil {
		if absent && errors.Is(e, unix.EEXIST) {
			s.publication.Store(0)
			return outcome, errHandoff
		}
		s.publishAmbiguous = true
		return InstalledHandoffUncertain, errHandoffUncertain
	}
	s.currentID = s.tempID
	s.tempID = handoffIdentity{}
	// After rename every failure is uncertain, including cancellation and close
	// errors from the final descriptor-based validation; never report no effect.
	if s.point(ctx, "current-directory-sync") != nil || unix.Fsync(s.ns) != nil || s.validate() != nil || s.point(ctx, "current-complete") != nil {
		return InstalledHandoffUncertain, errHandoffUncertain
	}
	var visible handoffLocator
	visibleID, visibleErr := handoffRead(s.ns, "current", &visible)
	if visibleErr != nil || visibleID != s.currentID || visible != staged || s.point(ctx, "publication-return") != nil {
		return InstalledHandoffUncertain, errHandoffUncertain
	}
	s.publication.Store(2)
	return InstalledHandoffPublished, nil
}
func handoffLocatorOK(l handoffLocator, installation string) bool {
	return l.Protocol == InstalledProtocol && l.Installation == installation && handoffHex(l.Run, 32)
}

// Close retires aliases, waits for the whole serialized filesystem operation,
// then removes captured identities only. Unknown sockets block empty-dir removal.
// No recursive deletion, stale adoption, PID checks or detached workers exist.
func (h InstalledHandoff) Close() error {
	s := h.state
	if s == nil {
		return nil
	}
	s.once.Do(func() {
		s.retired.Store(true)
		close(s.done)
		s.gate <- struct{}{}
		defer func() { <-s.gate }()
		add := func(e error) {
			if e != nil {
				s.closeErr = errors.Join(s.closeErr, errHandoffCleanup)
			}
		}
		// Resolve only captured identities, never adopt an unrelated destination.
		if s.initAmbiguous && handoffNamed(s.root, "ic", s.nsID, true) == nil {
			s.nsName = "ic"
		}
		if s.publishAmbiguous && handoffNamed(s.ns, "current", s.tempID, false) == nil {
			s.currentID = s.tempID
			s.tempID = handoffIdentity{}
		}
		safe := s.binding() == nil && (s.ns < 0 || s.recognized)
		if safe && s.ns >= 0 {
			safe = handoffNamed(s.root, s.nsName, s.nsID, true) == nil
		}
		if safe && s.ns >= 0 && s.nsName == "ic" {
			var owner handoffOwner
			id, e := handoffRead(s.ns, "owner", &owner)
			safe = e == nil && id == s.ownerID && owner == s.owner
		}
		if !safe {
			add(errHandoffCleanup)
		} else if s.ns >= 0 {
			if s.currentID != (handoffIdentity{}) {
				add(handoffRemove(s.ns, "current", s.currentID, false))
			}
			if s.tempID != (handoffIdentity{}) {
				add(handoffRemove(s.ns, s.tempName, s.tempID, false))
			}
			if s.gen >= 0 {
				if handoffNamed(s.ns, "g"+s.credentials.Run, s.genID, true) != nil {
					add(errHandoffCleanup)
				} else {
					if s.credentialID != (handoffIdentity{}) {
						add(handoffRemove(s.gen, "c", s.credentialID, false))
					}
					add(unix.Fsync(s.gen))
					add(handoffRemove(s.ns, "g"+s.credentials.Run, s.genID, true))
				}
			}
			if strings.HasPrefix(s.nsName, ".ic-") {
				if s.ownerID != (handoffIdentity{}) {
					add(handoffRemove(s.ns, "owner", s.ownerID, false))
				}
				add(unix.Fsync(s.ns))
				add(handoffRemove(s.root, s.nsName, s.nsID, true))
				add(unix.Fsync(s.root))
			} else {
				add(unix.Fsync(s.ns))
			}
		}
		for _, fd := range []int{s.gen, s.ns, s.root} {
			if fd >= 0 {
				add(unix.Close(fd))
			}
		}
		s.credentials = InstalledCredentials{}
	})
	return s.closeErr
}

// ReadInstalledHandoff independently selects and validates installation/config.
// No returned bytes authorize service ownership or assert endpoint readiness.
func ReadInstalledHandoff(ctx context.Context, selected config.InstalledConfig) (snapshot InstalledHandoffSnapshot, result error) {
	return readInstalledHandoff(ctx, selected, nil)
}

func readInstalledHandoff(ctx context.Context, selected config.InstalledConfig, boundary func(string, []int)) (snapshot InstalledHandoffSnapshot, result error) {
	cfg := selected.Snapshot()
	if ctx == nil || ctx.Err() != nil || selected.Validate(&cfg) != nil {
		return snapshot, errHandoff
	}
	path, e := filepath.EvalSymlinks(cfg.RuntimeDir)
	if e != nil || !handoffPath(path) || len(filepath.Join(path, "ic", "g"+strings.Repeat("0", 32), "s")) > 103 {
		return snapshot, errHandoff
	}
	root, ids, e := handoffRoot(path)
	if e != nil {
		return snapshot, errHandoff
	}
	s := &handoffState{selected: selected, credentials: InstalledCredentials{Installation: cfg.InstanceID}, path: path, ancestors: ids, root: root, ns: -1, gen: -1, nsName: "ic"}
	defer func() {
		for _, fd := range []int{s.gen, s.ns, s.root} {
			if fd >= 0 && unix.Close(fd) != nil {
				result = errHandoff
			}
		}
		if boundary != nil {
			boundary("closed", []int{s.gen, s.ns, s.root})
		}
		// binding includes its own descriptor closes; cancellation is checked last.
		if s.binding() != nil || ctx.Err() != nil {
			result = errHandoff
		}
		if result != nil {
			snapshot = InstalledHandoffSnapshot{}
		}
	}()
	s.ns, s.nsID, e = handoffChild(root, "ic", true)
	if e != nil {
		return snapshot, errHandoff
	}
	s.ownerID, e = handoffRead(s.ns, "owner", &s.owner)
	if e != nil || s.owner != (handoffOwner{"installed-handoff-v1", cfg.InstanceID, ids[len(ids)-1].key(), s.nsID.key()}) {
		return snapshot, errHandoff
	}
	var locator handoffLocator
	current, e := handoffRead(s.ns, "current", &locator)
	if e != nil || !handoffLocatorOK(locator, cfg.InstanceID) {
		return snapshot, errHandoff
	}
	s.gen, s.genID, e = handoffChild(s.ns, "g"+locator.Run, true)
	if e != nil {
		return snapshot, errHandoff
	}
	var record handoffCredential
	s.credentialID, e = handoffRead(s.gen, "c", &record)
	s.credentials = InstalledCredentials{record.Run, record.Installation, record.Capability}
	if e != nil || !handoffCredentials(s.credentials) || record.Protocol != InstalledProtocol || record.Installation != cfg.InstanceID || record.Run != locator.Run {
		return snapshot, errHandoff
	}
	var again handoffLocator
	id, e := handoffRead(s.ns, "current", &again)
	if e != nil || id != current || again != locator || s.validate() != nil || ctx.Err() != nil {
		return snapshot, errHandoff
	}
	if boundary != nil {
		boundary("validated", nil)
	}
	return InstalledHandoffSnapshot{s.credentials, filepath.Join(path, "ic", "g"+record.Run, "s")}, nil
}
