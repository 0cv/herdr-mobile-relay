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

	"github.com/0cv/herdr-mobile-relay/internal/hermesprofiles"
)

const ownershipLimit = 1024

type PaneIdentity struct {
	PaneID      string `json:"pane_id"`
	TerminalID  string `json:"terminal_id"`
	TabID       string `json:"tab_id"`
	WorkspaceID string `json:"workspace_id"`
}

// profileOwnership is decoded by every relay version with DisallowUnknownFields,
// so it must never gain a field: the Hermes profile lives in hermesOwnership.
type profileOwnership struct {
	Target    PaneIdentity `json:"target"`
	ProfileID string       `json:"profile_id"`
	Pending   bool         `json:"pending,omitempty"`
}

// hermesOwnership is the `hermes -p` profile a verified pane really started with
// ("" for a Hermes launch without `-p`); a pane with no record is unknown.
type hermesOwnership struct {
	Target        PaneIdentity `json:"target"`
	HermesProfile string       `json:"hermes_profile"`
}

func hermesStorePath(ownershipPath string) string {
	return filepath.Join(filepath.Dir(ownershipPath), "hermes-"+filepath.Base(ownershipPath))
}

func (r *Resolver) SetOwnershipPath(path string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.hermes = make(map[string]hermesOwnership)
	if err := r.loadOwnershipLocked(path); err != nil {
		return err
	}
	if err := r.loadHermesLocked(); err != nil {
		r.ownershipUnavailable = true
		return err
	}
	return nil
}

func (r *Resolver) loadOwnershipLocked(path string) error {
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

// loadHermesLocked applies the ownership file's policy to the Hermes file. A record
// whose ownership entry is gone, pending or for another target is stale (for
// example after a rollback in between): it is dropped and the file rewritten.
func (r *Resolver) loadHermesLocked() error {
	if r.ownershipPath == "" {
		return nil
	}
	path := hermesStorePath(r.ownershipPath)
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	stat, owned := info.Sys().(*syscall.Stat_t)
	if !owned || stat.Uid != uint32(os.Geteuid()) || stat.Nlink != 1 || !info.Mode().IsRegular() || info.Mode().Perm() != 0600 || info.Size() > 1024*1024 {
		return errors.New("hermes profile file must be private, regular and bounded")
	}
	file, err := os.Open(path)
	if err != nil {
		return err
	}
	defer file.Close()
	decoder := json.NewDecoder(io.LimitReader(file, 1024*1024+1))
	decoder.DisallowUnknownFields()
	var records []hermesOwnership
	if err := decoder.Decode(&records); err != nil {
		return fmt.Errorf("read hermes profile records: %w", err)
	}
	var extra any
	if len(records) > ownershipLimit || decoder.Decode(&extra) != io.EOF {
		return errors.New("invalid hermes profile store")
	}
	dropped := false
	for _, record := range records {
		key := ownershipKey(record.Target.PaneID)
		if _, duplicate := r.hermes[key]; duplicate || !record.Target.valid() || (record.HermesProfile != "" && !hermesprofiles.Valid(record.HermesProfile)) {
			return errors.New("invalid hermes profile record")
		}
		if owner, exists := r.ownership[key]; !exists || owner.Pending || owner.Target != record.Target {
			dropped = true
			continue
		}
		r.hermes[key] = record
	}
	if dropped {
		return r.persistHermesLocked()
	}
	return nil
}

func ownershipKey(pane string) string   { return strings.TrimSpace(pane) }
func (target PaneIdentity) valid() bool { return target.PaneID != "" && target.TerminalID != "" }

func (r *Resolver) OwnsTarget(target PaneIdentity, profileID string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.ownsTargetLocked(target, profileID)
}

func (r *Resolver) ownsTargetLocked(target PaneIdentity, profileID string) bool {
	record, exists := r.ownership[ownershipKey(target.PaneID)]
	return !r.ownershipUnavailable && exists && !record.Pending && target.valid() && record.Target == target && record.ProfileID == profileID
}

// OwnedHermesProfile reports, in one locked step, whether the pane is a verified
// launch of profileID, and its recorded Hermes profile. known is false when no
// record exists (an older relay's pane, or a kind that records none): "" then means
// unknown, unlike a recorded launch without `-p`, which is known and "". owned is
// false while the store requires recovery: an unreadable record never matches.
func (r *Resolver) OwnedHermesProfile(target PaneIdentity, profileID string) (owned bool, hermesProfile string, known bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if !r.ownsTargetLocked(target, profileID) {
		return false, "", false
	}
	if record := r.hermes[ownershipKey(target.PaneID)]; record.Target == target {
		return true, record.HermesProfile, true
	}
	return true, "", false
}

func (r *Resolver) RememberVerified(target PaneIdentity, profileID string) error {
	return r.rememberOwnership(target, profileID, nil, false)
}

// RememberVerifiedHermes verifies a Hermes launch and records its profile in one
// step ("" records a launch without `-p`).
func (r *Resolver) RememberVerifiedHermes(target PaneIdentity, profileID, hermesProfile string) error {
	return r.rememberOwnership(target, profileID, &hermesProfile, false)
}

func (r *Resolver) BeginLaunchOwnership(target PaneIdentity, profileID string) error {
	return r.rememberOwnership(target, profileID, nil, true)
}

func (r *Resolver) rememberOwnership(target PaneIdentity, profileID string, hermes *string, pending bool) error {
	if !target.valid() {
		return errors.New("launch terminal identity is unavailable")
	}
	if _, exists := r.Profile(profileID); !exists {
		return errors.New("launch profile is unavailable")
	}
	if hermes != nil && (pending || (*hermes != "" && !hermesprofiles.Valid(*hermes))) {
		return errors.New("hermes profile cannot be recorded for this launch")
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
	previousHermes, hadHermes := r.hermes[key]
	r.ownership[key] = profileOwnership{Target: target, ProfileID: profileID, Pending: pending}
	delete(r.unobserved, key)
	delete(r.hermes, key)
	if hermes != nil {
		r.hermes[key] = hermesOwnership{Target: target, HermesProfile: *hermes}
	}
	hermesChanged := hadHermes || hermes != nil
	if err := r.persistReleaseLocked(hermesChanged); err != nil {
		if existed {
			r.ownership[key] = previous
		} else {
			delete(r.ownership, key)
		}
		delete(r.hermes, key)
		if hadHermes {
			r.hermes[key] = previousHermes
		}
		if hermesChanged {
			_ = r.persistHermesLocked() // best effort: bring the disk back to memory
		}
		return err
	}
	return nil
}

func (r *Resolver) dropHermesLocked(key string) bool {
	_, had := r.hermes[key]
	delete(r.hermes, key)
	return had
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
	changed, hermesChanged := false, false
	for key, record := range r.ownership {
		target, exists := r.observed[key]
		if exists {
			delete(r.unobserved, key)
			if target.valid() && target != record.Target {
				delete(r.ownership, key)
				hermesChanged = r.dropHermesLocked(key) || hermesChanged
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
		hermesChanged = r.dropHermesLocked(key) || hermesChanged
		changed = true
	}
	if changed {
		if err := r.persistReleaseLocked(hermesChanged); err != nil {
			r.ownershipUnavailable = true
			return err
		}
	}
	return nil
}

// persistReleaseLocked writes the Hermes file first: a crash between the writes
// leaves a verified pane with an unknown profile, never a profile without its record.
func (r *Resolver) persistReleaseLocked(hermesChanged bool) error {
	if hermesChanged {
		if err := r.persistHermesLocked(); err != nil {
			return err
		}
	}
	return r.persistOwnershipLocked()
}

func (r *Resolver) persistHermesLocked() error {
	if r.ownershipPath == "" {
		return nil
	}
	records := make([]hermesOwnership, 0, len(r.hermes))
	for _, record := range r.hermes {
		records = append(records, record)
	}
	data, err := json.Marshal(records)
	if err != nil {
		return err
	}
	return writePrivateFile(hermesStorePath(r.ownershipPath), ".hermes-profile-*", data)
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
	return writePrivateFile(r.ownershipPath, ".profile-ownership-*", data)
}

func writePrivateFile(path, pattern string, data []byte) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0700); err != nil {
		return err
	}
	file, err := os.CreateTemp(dir, pattern)
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
	if err := os.Rename(file.Name(), path); err != nil {
		return err
	}
	directory, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer directory.Close()
	return directory.Sync()
}
