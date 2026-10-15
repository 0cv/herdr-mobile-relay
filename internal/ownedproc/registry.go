package ownedproc

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"time"
)

var ErrIncomplete = errors.New("owned process cleanup incomplete")

const MaxProcesses = 128
const MaxTable = 8192
const MaxDepth = 32
const InspectionBudget = 200 * time.Millisecond

type Process struct {
	PID    int
	Parent int
	Birth  string
	Exited bool
}

type Inspector interface {
	Lookup(context.Context, int) (Process, error)
	Table(context.Context) ([]Process, error)
	Signal(int, bool) error
}

type entry struct {
	Process
	ParentBirth string
	Depth       int
	Retired     bool
}

type Registry struct {
	Registered func(Process)
	inspector  Inspector
	root       int
	parent     Process
	entries    map[int]*entry
	issues     []error
}

func New(ctx context.Context, inspector Inspector, pid, parent int) (*Registry, error) {
	r := &Registry{inspector: inspector, root: pid, entries: make(map[int]*entry)}
	p, err := inspector.Lookup(ctx, parent)
	if err != nil || !valid(p) || p.Exited {
		return r, r.fail("spawn parent identity unavailable")
	}
	r.parent = p
	child, err := inspector.Lookup(ctx, pid)
	if err != nil || !valid(child) || child.Parent != parent {
		return r, r.fail("spawn identity unavailable")
	}
	again, err := inspector.Lookup(ctx, parent)
	if err != nil || again.Birth != p.Birth || again.Exited {
		return r, r.fail("spawn parent changed")
	}
	r.entries[pid] = &entry{Process: child, ParentBirth: p.Birth}
	return r, nil
}

func valid(p Process) bool { return p.PID > 0 && p.Birth != "" }

func (r *Registry) fail(message string) error {
	err := fmt.Errorf("%w: %s", ErrIncomplete, message)
	if len(r.issues) < MaxProcesses {
		r.issues = append(r.issues, err)
	}
	return err
}

func (r *Registry) Err() error { return errors.Join(r.issues...) }

func (r *Registry) IdentityFromRoot() (Process, bool) { return r.Identity(r.root) }

func (r *Registry) Identity(pid int) (Process, bool) {
	e, ok := r.entries[pid]
	if !ok {
		return Process{}, false
	}
	return e.Process, true
}

func (r *Registry) match(ctx context.Context, e *entry) (bool, error) {
	if e.Retired {
		return false, nil
	}
	p, err := r.inspector.Lookup(ctx, e.PID)
	if errors.Is(err, errors.ErrUnsupported) {
		return false, r.fail("inspection unsupported")
	}
	if errors.Is(err, ErrAbsent) {
		e.Retired = true
		return false, nil
	}
	if err != nil || !valid(p) {
		return false, r.fail("process identity unavailable")
	}
	if p.Birth != e.Birth {
		e.Retired = true
		return false, r.fail("process birth changed")
	}
	if p.Exited {
		e.Retired = true
		return false, nil
	}
	if p.Parent != e.Parent {
		return false, r.fail("process reparented")
	}
	if e.Depth == 0 {
		parent, err := r.inspector.Lookup(ctx, r.parent.PID)
		if err != nil || parent.Birth != e.ParentBirth || parent.Exited {
			return false, r.fail("root parent provenance unavailable")
		}
	} else {
		parent, ok := r.entries[e.Parent]
		if !ok || parent.Birth != e.ParentBirth {
			return false, r.fail("parent provenance unavailable")
		}
		live, err := r.match(ctx, parent)
		if err != nil {
			return false, err
		}
		if !live {
			return false, r.fail("owned parent exited")
		}
	}
	return true, nil
}

func (r *Registry) Sample(parent context.Context) error {
	if err := parent.Err(); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(parent, InspectionBudget)
	defer cancel()
	rows, err := r.inspector.Table(ctx)
	if err != nil {
		return r.fail("process table unavailable")
	}
	if len(rows) > MaxTable {
		return r.fail("process table limit")
	}
	for _, e := range r.entries {
		if _, err := r.match(ctx, e); err != nil {
			return err
		}
	}
	for _, p := range rows {
		e := r.entries[p.PID]
		owner := r.entries[p.Parent]
		if e != nil && e.Retired && e.Birth != p.Birth && owner != nil && !owner.Retired && !p.Exited {
			return r.fail("retired PID replaced under owned parent")
		}
	}
	for depth := 1; depth <= MaxDepth; depth++ {
		added := false
		for _, p := range rows {
			if err := ctx.Err(); err != nil {
				return r.fail("discovery deadline")
			}
			if _, known := r.entries[p.PID]; known || p.Exited {
				continue
			}
			owner, ok := r.entries[p.Parent]
			if !ok || owner.Retired || owner.Depth != depth-1 {
				continue
			}
			live, err := r.match(ctx, owner)
			if err != nil {
				return err
			}
			if !live {
				continue
			}
			if !valid(p) {
				return r.fail("descendant birth unavailable")
			}
			current, err := r.inspector.Lookup(ctx, p.PID)
			absent := errors.Is(err, ErrAbsent)
			if absent {
				current = p
			}
			if (!absent && err != nil) || current.Birth != p.Birth || current.Parent != p.Parent {
				return r.fail("descendant changed during discovery")
			}
			live, err = r.match(ctx, owner)
			if err != nil {
				return err
			}
			if !live {
				return r.fail("parent exited during discovery")
			}
			if len(r.entries) >= MaxProcesses {
				return r.fail("owned process limit")
			}
			r.entries[p.PID] = &entry{Process: current, ParentBirth: owner.Birth, Depth: depth, Retired: absent}
			if r.Registered != nil && !absent {
				r.Registered(current)
			}
			added = true
		}
		if !added {
			deeper := false
			for _, e := range r.entries {
				deeper = deeper || (!e.Retired && e.Depth == depth)
			}
			if !deeper {
				break
			}
		}
	}
	for _, p := range rows {
		if owner := r.entries[p.Parent]; owner != nil && owner.Retired && !p.Exited {
			if e := r.entries[p.PID]; e == nil || !e.Retired {
				return r.fail("unobservable descendant of exited parent")
			}
		}
		if owner := r.entries[p.Parent]; owner != nil && owner.Depth == MaxDepth && !p.Exited {
			if _, known := r.entries[p.PID]; !known {
				return r.fail("discovery depth limit")
			}
		}
	}
	return nil
}

func (r *Registry) Signal(ctx context.Context, pid int, force bool) error {
	e, ok := r.entries[pid]
	if !ok || pid <= 0 {
		return r.fail("unregistered signal refused")
	}
	live, err := r.match(ctx, e)
	if err != nil || !live {
		return err
	}
	p, err := r.inspector.Lookup(ctx, pid)
	if err != nil || !valid(p) || p.Exited || p.Birth != e.Birth || p.Parent != e.Parent {
		return r.fail("immediate signal identity check failed")
	}
	if err := ctx.Err(); err != nil {
		return r.fail("signal deadline")
	}
	if err := r.inspector.Signal(pid, force); err != nil {
		return r.fail("individual signal failed")
	}
	return nil
}

func (r *Registry) ordered() []*entry {
	entries := make([]*entry, 0, len(r.entries))
	for _, e := range r.entries {
		entries = append(entries, e)
	}
	sort.Slice(entries, func(i, j int) bool {
		if entries[i].Depth == entries[j].Depth {
			return entries[i].PID < entries[j].PID
		}
		return entries[i].Depth > entries[j].Depth
	})
	return entries
}

func (r *Registry) Finish(ctx context.Context) error {
	if err := r.Sample(ctx); err != nil {
		return err
	}
	for _, e := range r.entries {
		live, err := r.match(ctx, e)
		if err != nil {
			return err
		}
		if live {
			return r.fail("recorded incarnation remains alive")
		}
	}
	return r.Err()
}

func (r *Registry) Cancel(ctx context.Context, grace time.Duration) error {
	if err := r.Sample(ctx); err != nil {
		return errors.Join(err, r.fail("cleanup snapshot incomplete"))
	}
	for _, e := range r.ordered() {
		_ = r.Signal(ctx, e.PID, false)
	}
	timer := time.NewTimer(grace)
	defer timer.Stop()
	graceTick := time.NewTicker(10 * time.Millisecond)
	defer graceTick.Stop()
graceLoop:
	for {
		remaining := false
		for _, e := range r.entries {
			live, err := r.match(ctx, e)
			if err != nil {
				return err
			}
			remaining = remaining || live
		}
		if !remaining {
			return r.Err()
		}
		select {
		case <-ctx.Done():
			return r.fail("cleanup deadline")
		case <-timer.C:
			break graceLoop
		case <-graceTick.C:
		}
	}
	if err := r.Sample(ctx); err != nil {
		return err
	}
	for _, e := range r.ordered() {
		_ = r.Signal(ctx, e.PID, true)
	}
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for {
		remaining := false
		for _, e := range r.entries {
			live, err := r.match(ctx, e)
			if err != nil {
				return err
			}
			remaining = remaining || live
		}
		if !remaining {
			return r.Err()
		}
		select {
		case <-ctx.Done():
			return r.fail("survivor exit deadline")
		case <-ticker.C:
		}
	}
}

var ErrAbsent = errors.New("process incarnation absent")
