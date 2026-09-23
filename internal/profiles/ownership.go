package profiles

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"
)

const ownershipLimit = 1024

type PaneIdentity struct {
	PaneID      string `json:"pane_id"`
	TerminalID  string `json:"terminal_id"`
	TabID       string `json:"tab_id"`
	WorkspaceID string `json:"workspace_id"`
}

type profileOwnership struct {
	Target    PaneIdentity `json:"target"`
	ProfileID string       `json:"profile_id"`
	Pending   bool         `json:"pending,omitempty"`
}

func (r *Resolver) SetOwnershipPath(path string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.ownershipPath = path
	r.ownershipUnavailable = false
	r.ownership = make(map[string]profileOwnership)
	r.unobserved = make(map[string]bool)
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		r.ownershipUnavailable = true
		return err
	}
	stat, owned := info.Sys().(*syscall.Stat_t)
	if !owned || stat.Uid != uint32(os.Geteuid()) || stat.Nlink != 1 || !info.Mode().IsRegular() || info.Mode().Perm() != 0600 || info.Size() > 1024*1024 {
		r.ownershipUnavailable = true
		return errors.New("profile ownership file must be private, regular and bounded")
	}
	file, err := os.Open(path)
	if err != nil {
		r.ownershipUnavailable = true
		return err
	}
	defer file.Close()
	decoder := json.NewDecoder(io.LimitReader(file, 1024*1024+1))
	decoder.DisallowUnknownFields()
	var records []profileOwnership
	if err := decoder.Decode(&records); err != nil {
		r.ownershipUnavailable = true
		return fmt.Errorf("read profile ownership: %w", err)
	}
	var extra any
	if len(records) > ownershipLimit || decoder.Decode(&extra) != io.EOF {
		r.ownershipUnavailable = true
		return errors.New("invalid profile ownership store")
	}
	for _, record := range records {
		if !record.Target.valid() || record.ProfileID == "" {
			r.ownershipUnavailable = true
			return errors.New("invalid profile ownership identity")
		}
		key := ownershipKey(record.Target.PaneID)
		if _, exists := r.ownership[key]; exists {
			r.ownershipUnavailable = true
			return errors.New("duplicate profile ownership identity")
		}
		r.ownership[key] = record
	}
	r.ownershipUnavailable = false
	return nil
}

func ownershipKey(pane string) string   { return strings.TrimSpace(pane) }
func (target PaneIdentity) valid() bool { return target.PaneID != "" && target.TerminalID != "" }

func (r *Resolver) OwnsTarget(target PaneIdentity, profileID string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	record, exists := r.ownership[ownershipKey(target.PaneID)]
	return !r.ownershipUnavailable && exists && !record.Pending && target.valid() && record.Target == target && record.ProfileID == profileID
}

func (r *Resolver) RememberVerified(target PaneIdentity, profileID string) error {
	return r.rememberOwnership(target, profileID, false)
}

func (r *Resolver) BeginLaunchOwnership(target PaneIdentity, profileID string) error {
	return r.rememberOwnership(target, profileID, true)
}

func (r *Resolver) rememberOwnership(target PaneIdentity, profileID string, pending bool) error {
	if !target.valid() {
		return errors.New("launch terminal identity is unavailable")
	}
	if _, exists := r.Profile(profileID); !exists {
		return errors.New("launch profile is unavailable")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.ownershipUnavailable {
		return errors.New("profile ownership store requires recovery")
	}
	key := ownershipKey(target.PaneID)
	if _, exists := r.ownership[key]; !exists && len(r.ownership) >= ownershipLimit {
		return errors.New("profile ownership store is full")
	}
	previous, existed := r.ownership[key]
	r.ownership[key] = profileOwnership{Target: target, ProfileID: profileID, Pending: pending}
	delete(r.unobserved, key)
	if err := r.persistOwnershipLocked(); err != nil {
		if existed {
			r.ownership[key] = previous
		} else {
			delete(r.ownership, key)
		}
		return err
	}
	return nil
}

func (r *Resolver) ReconcileOwnership(targets []PaneIdentity) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.ownershipUnavailable {
		return errors.New("profile ownership store requires recovery")
	}
	r.observed = make(map[string]PaneIdentity, len(targets))
	for _, target := range targets {
		key := ownershipKey(target.PaneID)
		if _, duplicate := r.observed[key]; key == "" || duplicate {
			r.observed = make(map[string]PaneIdentity)
			return errors.New("profile ownership observation is incomplete")
		}
		r.observed[key] = target
	}
	changed := false
	for key, record := range r.ownership {
		target, exists := r.observed[key]
		if exists {
			delete(r.unobserved, key)
			if target.valid() && target != record.Target {
				delete(r.ownership, key)
				changed = true
			}
			continue
		}
		if !r.unobserved[key] {
			// A poll that started before the launch still reports the new pane
			// as absent, so one absence is not proof the pane is gone.
			r.unobserved[key] = true
			continue
		}
		delete(r.ownership, key)
		delete(r.unobserved, key)
		changed = true
	}
	if changed {
		if err := r.persistOwnershipLocked(); err != nil {
			r.ownershipUnavailable = true
			return err
		}
	}
	return nil
}

func (r *Resolver) persistOwnershipLocked() error {
	if r.ownershipPath == "" {
		return nil
	}
	records := make([]profileOwnership, 0, len(r.ownership))
	for _, record := range r.ownership {
		records = append(records, record)
	}
	data, err := json.Marshal(records)
	if err != nil {
		return err
	}
	dir := filepath.Dir(r.ownershipPath)
	if err := os.MkdirAll(dir, 0700); err != nil {
		return err
	}
	file, err := os.CreateTemp(dir, ".profile-ownership-*")
	if err != nil {
		return err
	}
	defer os.Remove(file.Name())
	if _, err := file.Write(data); err != nil {
		file.Close()
		return err
	}
	if err := file.Sync(); err != nil {
		file.Close()
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	if err := os.Rename(file.Name(), r.ownershipPath); err != nil {
		return err
	}
	directory, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer directory.Close()
	return directory.Sync()
}
