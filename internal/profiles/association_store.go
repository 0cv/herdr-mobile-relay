package profiles

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"syscall"
)

const (
	associationStoreName     = "pane-profile-associations.json"
	maxAssociationStoreBytes = 1024 * 1024
)

type Observation struct {
	PaneID          string
	NativeSessionID string
}

type Association struct {
	PaneID          string `json:"pane_id"`
	NativeSessionID string `json:"native_session_id"`
	ProfileID       string `json:"profile_id"`
}

type pendingAssociation struct {
	PaneID    string `json:"pane_id"`
	ProfileID string `json:"profile_id"`
}

type rejectedAssociation struct {
	PaneID          string `json:"pane_id"`
	NativeSessionID string `json:"native_session_id"`
}

type associationStore struct {
	Version      int                   `json:"version"`
	Associations []Association         `json:"associations"`
	Pending      []pendingAssociation  `json:"pending,omitempty"`
	Rejected     []rejectedAssociation `json:"rejected,omitempty"`
}

type associationFile interface {
	Name() string
	Chmod(os.FileMode) error
	Write([]byte) (int, error)
	Sync() error
	Close() error
}

type associationReadFile interface {
	io.Reader
	Stat() (os.FileInfo, error)
	Close() error
}

type associationDirectory interface {
	Sync() error
	Close() error
}

type associationStoreIO struct {
	lstat         func(string) (os.FileInfo, error)
	open          func(string) (associationReadFile, error)
	mkdirAll      func(string, os.FileMode) error
	chmod         func(string, os.FileMode) error
	marshal       func(any) ([]byte, error)
	createTemp    func(string, string) (associationFile, error)
	rename        func(string, string) error
	openDirectory func(string) (associationDirectory, error)
}

func defaultAssociationStoreIO() associationStoreIO {
	return associationStoreIO{
		lstat: os.Lstat, mkdirAll: os.MkdirAll, chmod: os.Chmod, marshal: json.Marshal,
		open: func(path string) (associationReadFile, error) {
			return os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW, 0)
		},
		createTemp: func(directory, pattern string) (associationFile, error) { return os.CreateTemp(directory, pattern) },
		rename:     os.Rename,
		openDirectory: func(directory string) (associationDirectory, error) {
			return os.Open(directory)
		},
	}
}

func (r *Resolver) Reconcile(observed []Observation) (resultErr error) {
	profiles, err := r.ProfilesWithError()
	if err != nil {
		r.Invalidate()
		return fmt.Errorf("discover profiles: %w", err)
	}
	configured := make(map[string]bool, len(profiles))
	for _, profile := range profiles {
		configured[profile.ID] = true
	}
	r.mu.Lock()
	for profileID := range r.knownProfileIDs {
		configured[profileID] = true
	}
	r.mu.Unlock()
	current, err := validObservations(observed)
	if err != nil {
		r.Invalidate()
		return err
	}

	r.mu.Lock()
	defer r.mu.Unlock()
	if err := r.loadAssociationsLocked(); err != nil {
		r.invalidateLocked()
		return err
	}

	next := make(map[string]Association, len(r.associations))
	verified := make(map[string]string, len(current))
	untrusted := make(map[string]bool)
	rejected := make(map[string]string)
	pending := make(map[string]string, len(r.pending)+len(r.remembered))
	for paneID, profileID := range r.pending {
		if configured[profileID] {
			pending[paneID] = profileID
		}
	}
	for paneID, profileID := range r.remembered {
		if configured[profileID] {
			pending[paneID] = profileID
		}
	}
	for paneID, observation := range current {
		if r.forgotten[paneID] {
			continue
		}
		if saved, ok := r.associations[paneID]; ok && configured[saved.ProfileID] {
			if observation.NativeSessionID == "" {
				next[paneID] = saved
			} else if saved.NativeSessionID == observation.NativeSessionID {
				next[paneID] = saved
				verified[paneID] = saved.ProfileID
			} else {
				untrusted[paneID] = true
				rejected[paneID] = observation.NativeSessionID
			}
		}
		if rejectedIdentity, wasRejected := r.rejected[paneID]; wasRejected {
			untrusted[paneID] = true
			rejected[paneID] = observation.NativeSessionID
			if rejected[paneID] == "" {
				rejected[paneID] = rejectedIdentity
			}
		}
		if profileID := pending[paneID]; profileID != "" && r.remembered[paneID] == profileID && configured[profileID] && observation.NativeSessionID != "" {
			next[paneID] = Association{PaneID: paneID, NativeSessionID: observation.NativeSessionID, ProfileID: profileID}
			verified[paneID] = profileID
			delete(pending, paneID)
			delete(untrusted, paneID)
			delete(rejected, paneID)
		}
	}
	if (!sameAssociations(r.associations, next) || !samePending(r.pending, pending) || !samePending(r.rejected, rejected)) && r.associationDir != "" {
		if err := writeAssociationState(r.associationDir, next, pending, rejected); err != nil {
			r.verified = make(map[string]string)
			r.untrusted = untrusted
			return fmt.Errorf("persist pane profile associations: %w", err)
		}
	}
	remembered := make(map[string]string, len(r.remembered))
	for paneID, profileID := range r.remembered {
		if pending[paneID] == profileID {
			remembered[paneID] = profileID
		}
	}
	r.associations = next
	r.pending = pending
	r.rejected = rejected
	r.verified = verified
	r.untrusted = untrusted
	r.remembered = remembered
	clear(r.forgotten)
	return nil
}

func (r *Resolver) loadAssociationsLocked() error {
	if r.associationsRead {
		return nil
	}
	if r.associationDir == "" {
		r.associationsRead = true
		r.associationErr = nil
		return nil
	}
	fail := func(err error) error {
		r.associationsRead = false
		r.associationErr = err
		r.verified = make(map[string]string)
		return err
	}
	path := filepath.Join(r.associationDir, associationStoreName)
	data, err := readAssociationStoreWith(defaultAssociationStoreIO(), path)
	if errors.Is(err, os.ErrNotExist) {
		r.associationsRead = true
		r.associationErr = nil
		return nil
	}
	if err != nil {
		return fail(fmt.Errorf("read pane profile associations: %w", err))
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	var store associationStore
	if err := decoder.Decode(&store); err != nil {
		return fail(fmt.Errorf("decode pane profile associations: %w", err))
	}
	if err := requireJSONEOF(decoder); err != nil || store.Version != 1 {
		return fail(errors.New("pane profile association store has an unsupported format"))
	}
	loaded := make(map[string]Association, len(store.Associations))
	native := make(map[string]string, len(store.Associations))
	for _, association := range store.Associations {
		if !validIdentifier(association.PaneID) || !validIdentifier(association.NativeSessionID) || !validIdentifier(association.ProfileID) {
			return fail(errors.New("pane profile association store contains an invalid identifier"))
		}
		association.PaneID = normalizeIdentifier(association.PaneID)
		association.NativeSessionID = strings.TrimSpace(association.NativeSessionID)
		association.ProfileID = strings.ToLower(strings.TrimSpace(association.ProfileID))
		if !validIdentifier(association.PaneID) || !validIdentifier(association.NativeSessionID) || !validIdentifier(association.ProfileID) {
			return fail(errors.New("pane profile association store contains an invalid identifier"))
		}
		if _, duplicate := loaded[association.PaneID]; duplicate || native[association.NativeSessionID] != "" {
			return fail(errors.New("pane profile association store contains a duplicate association"))
		}
		loaded[association.PaneID] = association
		native[association.NativeSessionID] = association.PaneID
	}
	pending := make(map[string]string, len(store.Pending))
	for _, association := range store.Pending {
		if !validIdentifier(association.PaneID) || !validIdentifier(association.ProfileID) {
			return fail(errors.New("pane profile association store contains an invalid pending identifier"))
		}
		association.PaneID = normalizeIdentifier(association.PaneID)
		association.ProfileID = normalizeIdentifier(association.ProfileID)
		if !validIdentifier(association.PaneID) || !validIdentifier(association.ProfileID) {
			return fail(errors.New("pane profile association store contains an invalid pending identifier"))
		}
		if _, duplicate := pending[association.PaneID]; duplicate {
			return fail(errors.New("pane profile association store contains a duplicate pending association"))
		}
		pending[association.PaneID] = association.ProfileID
	}
	rejected := make(map[string]string, len(store.Rejected))
	for _, association := range store.Rejected {
		if !validIdentifier(association.PaneID) || !validIdentifier(association.NativeSessionID) {
			return fail(errors.New("pane profile association store contains an invalid rejected identity"))
		}
		association.PaneID = normalizeIdentifier(association.PaneID)
		association.NativeSessionID = strings.TrimSpace(association.NativeSessionID)
		if !validIdentifier(association.PaneID) || !validIdentifier(association.NativeSessionID) {
			return fail(errors.New("pane profile association store contains an invalid rejected identity"))
		}
		if _, duplicate := rejected[association.PaneID]; duplicate {
			return fail(errors.New("pane profile association store contains a duplicate rejected identity"))
		}
		rejected[association.PaneID] = association.NativeSessionID
	}
	r.associations = loaded
	r.pending = pending
	r.rejected = rejected
	r.associationsRead = true
	r.associationErr = nil
	return nil
}

func readAssociationStoreWith(ops associationStoreIO, path string) ([]byte, error) {
	before, err := ops.lstat(path)
	if err != nil {
		return nil, err
	}
	if !validAssociationStoreFile(before) {
		return nil, errors.New("pane profile association store must be a private bounded regular file with one link")
	}
	file, err := ops.open(path)
	if err != nil {
		return nil, err
	}
	after, statErr := file.Stat()
	if statErr != nil {
		_ = file.Close()
		return nil, statErr
	}
	if !validAssociationStoreFile(after) || !os.SameFile(before, after) {
		_ = file.Close()
		return nil, errors.New("pane profile association store changed or became unsafe while opening")
	}
	data, readErr := io.ReadAll(io.LimitReader(file, maxAssociationStoreBytes+1))
	closeErr := file.Close()
	if readErr != nil {
		return nil, readErr
	}
	if closeErr != nil {
		return nil, closeErr
	}
	if len(data) > maxAssociationStoreBytes {
		return nil, errors.New("pane profile association store is too large")
	}
	return data, nil
}

func validAssociationStoreFile(info os.FileInfo) bool {
	stat, ok := info.Sys().(*syscall.Stat_t)
	return info.Mode().IsRegular() && info.Mode().Perm()&0o077 == 0 && info.Size() <= maxAssociationStoreBytes && ok && stat.Nlink == 1
}

func validObservations(observed []Observation) (map[string]Observation, error) {
	result := make(map[string]Observation, len(observed))
	native := make(map[string]string, len(observed))
	for _, observation := range observed {
		if !validIdentifier(observation.PaneID) {
			return nil, errors.New("observed pane association has an invalid identifier")
		}
		observation.PaneID = normalizeIdentifier(observation.PaneID)
		observation.NativeSessionID = strings.TrimSpace(observation.NativeSessionID)
		if !validIdentifier(observation.PaneID) || observation.NativeSessionID != "" && !validIdentifier(observation.NativeSessionID) {
			return nil, errors.New("observed pane association has an invalid identifier")
		}
		if _, duplicate := result[observation.PaneID]; duplicate || observation.NativeSessionID != "" && native[observation.NativeSessionID] != "" {
			return nil, errors.New("observed pane associations are ambiguous")
		}
		result[observation.PaneID] = observation
		if observation.NativeSessionID != "" {
			native[observation.NativeSessionID] = observation.PaneID
		}
	}
	return result, nil
}

func normalizeIdentifier(value string) string {
	return strings.ToLower(strings.TrimSpace(value))
}

func validIdentifier(value string) bool {
	if len(value) > 256 {
		return false
	}
	for _, character := range value {
		if character < 0x20 || character == 0x7f {
			return false
		}
	}
	return value != ""
}

func requireJSONEOF(decoder *json.Decoder) error {
	var trailing any
	err := decoder.Decode(&trailing)
	if errors.Is(err, io.EOF) {
		return nil
	}
	if err == nil {
		return errors.New("unexpected trailing JSON")
	}
	return err
}

func sameAssociations(left, right map[string]Association) bool {
	if len(left) != len(right) {
		return false
	}
	for paneID, association := range left {
		if right[paneID] != association {
			return false
		}
	}
	return true
}

func cloneAssociations(source map[string]Association) map[string]Association {
	cloned := make(map[string]Association, len(source))
	for paneID, association := range source {
		cloned[paneID] = association
	}
	return cloned
}

func clonePending(source map[string]string) map[string]string {
	cloned := make(map[string]string, len(source))
	for paneID, profileID := range source {
		cloned[paneID] = profileID
	}
	return cloned
}

func samePending(left, right map[string]string) bool {
	if len(left) != len(right) {
		return false
	}
	for paneID, profileID := range left {
		if right[paneID] != profileID {
			return false
		}
	}
	return true
}

func writeAssociationStore(directory string, associations map[string]Association) error {
	return writeAssociationStoreWith(defaultAssociationStoreIO(), directory, associations)
}

func writeAssociationStoreWith(ops associationStoreIO, directory string, associations map[string]Association) error {
	return writeAssociationStateWith(ops, directory, associations, nil, nil)
}

func writeAssociationState(directory string, associations map[string]Association, pending, rejected map[string]string) error {
	return writeAssociationStateWith(defaultAssociationStoreIO(), directory, associations, pending, rejected)
}

func writeAssociationStateWith(ops associationStoreIO, directory string, associations map[string]Association, pending, rejected map[string]string) error {
	if info, err := ops.lstat(directory); err == nil {
		if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			return errors.New("pane profile association directory must be a directory")
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if err := ops.mkdirAll(directory, 0o700); err != nil {
		return err
	}
	if err := ops.chmod(directory, 0o700); err != nil {
		return err
	}
	values := make([]Association, 0, len(associations))
	for _, association := range associations {
		values = append(values, association)
	}
	sort.Slice(values, func(i, j int) bool { return values[i].PaneID < values[j].PaneID })
	pendingValues := make([]pendingAssociation, 0, len(pending))
	for paneID, profileID := range pending {
		pendingValues = append(pendingValues, pendingAssociation{PaneID: paneID, ProfileID: profileID})
	}
	sort.Slice(pendingValues, func(i, j int) bool { return pendingValues[i].PaneID < pendingValues[j].PaneID })
	rejectedValues := make([]rejectedAssociation, 0, len(rejected))
	for paneID, nativeSessionID := range rejected {
		rejectedValues = append(rejectedValues, rejectedAssociation{PaneID: paneID, NativeSessionID: nativeSessionID})
	}
	slices.SortFunc(rejectedValues, func(left, right rejectedAssociation) int {
		return strings.Compare(left.PaneID, right.PaneID)
	})
	data, err := ops.marshal(associationStore{Version: 1, Associations: values, Pending: pendingValues, Rejected: rejectedValues})
	if err != nil {
		return err
	}
	data = append(data, '\n')
	temporary, err := ops.createTemp(directory, ".pane-profile-associations.*")
	if err != nil {
		return err
	}
	temporaryPath := temporary.Name()
	defer os.Remove(temporaryPath)
	if err := temporary.Chmod(0o600); err != nil {
		temporary.Close()
		return err
	}
	written, err := temporary.Write(data)
	if err != nil {
		temporary.Close()
		return err
	}
	if written != len(data) {
		temporary.Close()
		return io.ErrShortWrite
	}
	if err := temporary.Sync(); err != nil {
		temporary.Close()
		return err
	}
	if err := temporary.Close(); err != nil {
		return err
	}
	if err := ops.rename(temporaryPath, filepath.Join(directory, associationStoreName)); err != nil {
		return err
	}
	directoryHandle, err := ops.openDirectory(directory)
	if err != nil {
		return err
	}
	err = directoryHandle.Sync()
	closeErr := directoryHandle.Close()
	if err != nil {
		return err
	}
	return closeErr
}
