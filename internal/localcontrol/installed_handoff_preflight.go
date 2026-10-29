//go:build linux || darwin

package localcontrol

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"

	"github.com/0cv/herdr-mobile-relay/internal/config"
	"golang.org/x/sys/unix"
)

// preflightPath mirrors the original binding's alias policy: the last existing
// selected component must itself be a directory, not a symlink. Intermediate
// aliases are resolved, never used to reselect a different binding.
func preflightPath(path string) (string, error) {
	if !handoffPath(path) {
		return "", errHandoff
	}
	ancestor := path
	var suffix []string
	for {
		info, e := os.Lstat(ancestor)
		if errors.Is(e, os.ErrNotExist) {
			suffix = append(suffix, filepath.Base(ancestor))
			ancestor = filepath.Dir(ancestor)
			continue
		}
		if e != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			return "", errHandoff
		}
		resolved, e := filepath.EvalSymlinks(ancestor)
		if e != nil {
			return "", errHandoff
		}
		for i := len(suffix) - 1; i >= 0; i-- {
			resolved = filepath.Join(resolved, suffix[i])
		}
		if !handoffPath(resolved) || len(filepath.Join(resolved, "ic", "g"+strings.Repeat("0", 32), "s")) > 103 {
			return "", errHandoff
		}
		return resolved, nil
	}
}

type preflightWalk struct {
	fds     []int
	ids     []handoffIdentity
	parts   []string
	missing bool
}

func (w *preflightWalk) close() error {
	var result error
	for i := len(w.fds) - 1; i >= 0; i-- {
		if unix.Close(w.fds[i]) != nil {
			result = errors.Join(result, errHandoff)
		}
	}
	w.fds = nil
	return result
}

// All existing descriptors are retained. A missing leaf is checked as an
// ancestor, never subjected to (or confused with) private runtime admission.
func preflightOpen(path string) (w preflightWalk, result error) {
	w.parts = strings.Split(path[1:], "/")
	defer func() {
		if result != nil {
			result = errors.Join(result, w.close())
		}
	}()
	fd, e := unix.Open("/", handoffFlags|unix.O_DIRECTORY, 0)
	if e != nil {
		return w, errHandoff
	}
	w.fds = append(w.fds, fd)
	id, e := handoffCheck(fd, true, true)
	if e != nil {
		return w, errHandoff
	}
	w.ids = append(w.ids, id)
	for i, p := range w.parts {
		var named unix.Stat_t
		e := unix.Fstatat(fd, p, &named, unix.AT_SYMLINK_NOFOLLOW)
		if errors.Is(e, unix.ENOENT) {
			w.missing = true
			return w, nil
		}
		ancestor := i < len(w.parts)-1
		if e != nil || !handoffMode(named, true, ancestor) {
			return w, errHandoff
		}
		next, e := unix.Openat(fd, p, handoffFlags|unix.O_DIRECTORY, 0)
		if e != nil {
			return w, errHandoff
		}
		w.fds = append(w.fds, next)
		id, e := handoffCheck(next, true, ancestor)
		if e != nil || id != handoffID(named) {
			return w, errHandoff
		}
		w.ids = append(w.ids, id)
		fd = next
	}
	return w, nil
}

func (w *preflightWalk) validate() error {
	for i, fd := range w.fds {
		ancestor := w.missing || i < len(w.fds)-1
		id, e := handoffCheck(fd, true, ancestor)
		if e != nil || id != w.ids[i] {
			return errHandoff
		}
		if i > 0 {
			var named unix.Stat_t
			if unix.Fstatat(w.fds[i-1], w.parts[i-1], &named, unix.AT_SYMLINK_NOFOLLOW) != nil || handoffID(named) != id {
				return errHandoff
			}
		}
	}
	if w.missing {
		var named unix.Stat_t
		if !errors.Is(unix.Fstatat(w.fds[len(w.fds)-1], w.parts[len(w.fds)-1], &named, unix.AT_SYMLINK_NOFOLLOW), unix.ENOENT) {
			return errHandoff
		}
	}
	return nil
}

// InspectInstalledHandoff performs no preparation, credential reads or effects.
// Full existing-root observation is required again after owned preparation.
// Prepare and PublishDiscovery still independently validate their own budgets.
func InspectInstalledHandoff(ctx context.Context, selected config.InstalledConfig) (InstalledHandoffObservation, error) {
	return inspectInstalledHandoff(ctx, selected, nil)
}

// boundary is a synchronous per-call test seam, not a policy override.
func inspectInstalledHandoff(ctx context.Context, selected config.InstalledConfig, boundary func(string) error) (observation InstalledHandoffObservation, result error) {
	cfg := selected.Snapshot()
	if ctx == nil || ctx.Err() != nil || !handoffPath(cfg.RuntimeDir) || selected.Validate(&cfg) != nil {
		return observation, errHandoff
	}
	path, e := preflightPath(cfg.RuntimeDir)
	if e != nil {
		return observation, errHandoff
	}
	w, e := preflightOpen(path)
	if e != nil {
		return observation, errHandoff
	}
	ns := -1
	var nsID, ownerID, currentID handoffIdentity
	var owner handoffOwner
	var current handoffLocator
	absent := false
	defer func() {
		if ns >= 0 && unix.Close(ns) != nil {
			result = errors.Join(result, errHandoff)
		}
		result = errors.Join(result, w.close())
		if boundary != nil {
			result = errors.Join(result, boundary("closed"))
		}
		// Reacquire ancestry after all metadata reads and closes. Both the original
		// canonical path and retained identities must still match; never rebind.
		again, pe := preflightPath(cfg.RuntimeDir)
		if pe != nil || again != path {
			result = errors.Join(result, errHandoff)
		} else {
			final, fe := preflightOpen(path)
			if fe != nil {
				result = errors.Join(result, errHandoff)
			} else {
				if final.missing != w.missing || !reflect.DeepEqual(final.ids, w.ids) || final.validate() != nil {
					result = errors.Join(result, errHandoff)
				}
				if result == nil && !final.missing {
					result = preflightFinalNamespace(final.fds[len(final.fds)-1], observation.state, nsID, ownerID, currentID, owner, current, absent)
				}
				result = errors.Join(result, final.validate(), final.close())
			}
		}
		// Binding validation may itself block. Context is deliberately checked last.
		if selected.Validate(&cfg) != nil || ctx.Err() != nil {
			result = errors.Join(result, errHandoff)
		}
		if result != nil {
			observation = InstalledHandoffObservation{}
			result = errHandoff
		}
	}()
	if w.validate() != nil {
		return observation, errHandoff
	}
	if w.missing {
		return InstalledHandoffObservation{InstalledHandoffMissingRuntimeDir}, nil
	}
	root := w.fds[len(w.fds)-1]
	ns, nsID, e = handoffChild(root, "ic", true)
	if errors.Is(e, unix.ENOENT) {
		// Initial Prepare adds owner and generation; Publish adds current staging.
		// Three four-slot units, not merely Prepare's initial eight-slot check.
		if handoffBudget(root, -1, 12) != nil || w.validate() != nil {
			return observation, errHandoff
		}
		var st unix.Stat_t
		if !errors.Is(unix.Fstatat(root, "ic", &st, unix.AT_SYMLINK_NOFOLLOW), unix.ENOENT) {
			return observation, errHandoff
		}
		return InstalledHandoffObservation{InstalledHandoffAbsentNamespace}, nil
	}
	if e != nil {
		return observation, errHandoff
	}
	ownerID, e = handoffRead(ns, "owner", &owner)
	expected := handoffOwner{"installed-handoff-v1", cfg.InstanceID, w.ids[len(w.ids)-1].key(), nsID.key()}
	if e != nil || owner != expected {
		return observation, errHandoff
	}
	currentID, e = handoffRead(ns, "current", &current)
	absent = errors.Is(e, unix.ENOENT)
	if !absent && (e != nil || !handoffLocatorOK(current, cfg.InstanceID)) {
		return observation, errHandoff
	}
	// Reuse adds generation, then current staging even when replacing current.
	if handoffBudget(root, ns, 8) != nil {
		return observation, errHandoff
	}
	if boundary != nil && boundary("read") != nil {
		return observation, errHandoff
	}
	var ownerAgain handoffOwner
	oid, e := handoffRead(ns, "owner", &ownerAgain)
	if e != nil || oid != ownerID || ownerAgain != owner {
		return observation, errHandoff
	}
	var currentAgain handoffLocator
	cid, e := handoffRead(ns, "current", &currentAgain)
	if absent {
		if !errors.Is(e, unix.ENOENT) {
			return observation, errHandoff
		}
	} else if e != nil || cid != currentID || currentAgain != current {
		return observation, errHandoff
	}
	if handoffNamed(root, "ic", nsID, true) != nil || w.validate() != nil {
		return observation, errHandoff
	}
	return InstalledHandoffObservation{InstalledHandoffAttributedNamespace}, nil
}

// Reobserve non-secret namespace data on the final retained ancestry. This
// helper closes its descriptor before the caller's final binding/context check.
func preflightFinalNamespace(root int, state InstalledHandoffPresence, nsID, ownerID, currentID handoffIdentity, owner handoffOwner, current handoffLocator, absent bool) (result error) {
	if state == InstalledHandoffAbsentNamespace {
		var st unix.Stat_t
		if !errors.Is(unix.Fstatat(root, "ic", &st, unix.AT_SYMLINK_NOFOLLOW), unix.ENOENT) {
			return errHandoff
		}
		return handoffBudget(root, -1, 12)
	}
	fd, id, e := handoffChild(root, "ic", true)
	if e != nil {
		return errHandoff
	}
	defer func() {
		if unix.Close(fd) != nil {
			result = errors.Join(result, errHandoff)
		}
	}()
	if id != nsID {
		return errHandoff
	}
	var o handoffOwner
	oid, e := handoffRead(fd, "owner", &o)
	if e != nil || oid != ownerID || o != owner {
		return errHandoff
	}
	var c handoffLocator
	cid, e := handoffRead(fd, "current", &c)
	if absent {
		if !errors.Is(e, unix.ENOENT) {
			return errHandoff
		}
	} else if e != nil || cid != currentID || c != current {
		return errHandoff
	}
	if handoffBudget(root, fd, 8) != nil || handoffNamed(root, "ic", nsID, true) != nil {
		return errHandoff
	}
	return nil
}
