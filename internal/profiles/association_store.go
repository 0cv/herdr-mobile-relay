package profiles

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

const associationStoreName = "pane-profile-associations.json"

type Observation struct {
	PaneID          string
	NativeSessionID string
}

type Association struct {
	PaneID          string `json:"pane_id"`
	NativeSessionID string `json:"native_session_id"`
	ProfileID       string `json:"profile_id"`
}

type associationStore struct {
	Version      int           `json:"version"`
	Associations []Association `json:"associations"`
}

type associationFile interface {
	Name() string
	Chmod(os.FileMode) error
	Write([]byte) (int, error)
	Sync() error
	Close() error
}

type associationDirectory interface {
	Sync() error
	Close() error
}

type associationStoreIO struct {
	lstat         func(string) (os.FileInfo, error)
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
		createTemp: func(directory, pattern string) (associationFile, error) { return os.CreateTemp(directory, pattern) },
		rename:     os.Rename,
		openDirectory: func(directory string) (associationDirectory, error) {
			return os.Open(directory)
		},
	}
}

func (r *Resolver) Reconcile(observed []Observation) error {
	profiles := r.Profiles()
	configured := make(map[string]bool, len(profiles))
	for _, profile := range profiles {
		configured[profile.ID] = true
	}
	current, err := validObservations(observed)
	if err != nil {
		return err
	}

	r.mu.Lock()
	defer r.mu.Unlock()
	if err := r.loadAssociationsLocked(); err != nil {
		return err
	}

	next := make(map[string]Association, len(current))
	for paneID, observation := range current {
		if saved, ok := r.associations[paneID]; ok &&
			saved.NativeSessionID == observation.NativeSessionID && configured[saved.ProfileID] {
			next[paneID] = saved
		}
		if pending := r.remembered[paneID]; pending != "" && configured[pending] {
			next[paneID] = Association{PaneID: paneID, NativeSessionID: observation.NativeSessionID, ProfileID: pending}
		}
	}
	if !sameAssociations(r.associations, next) && r.associationDir != "" {
		if err := writeAssociationStore(r.associationDir, next); err != nil {
			return fmt.Errorf("persist pane profile associations: %w", err)
		}
	}
	r.associations = next
	clear(r.remembered)
	for paneID, association := range next {
		r.remembered[paneID] = association.ProfileID
	}
	return nil
}

func (r *Resolver) loadAssociationsLocked() error {
	if r.associationsRead {
		return r.associationErr
	}
	r.associationsRead = true
	if r.associationDir == "" {
		return nil
	}
	path := filepath.Join(r.associationDir, associationStoreName)
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		r.associationErr = fmt.Errorf("read pane profile associations: %w", err)
		return r.associationErr
	}
	if !info.Mode().IsRegular() || info.Mode().Perm()&0o077 != 0 {
		r.associationErr = errors.New("pane profile association store must be a private regular file")
		return r.associationErr
	}
	if info.Size() > 1024*1024 {
		r.associationErr = errors.New("pane profile association store is too large")
		return r.associationErr
	}
	data, err := os.ReadFile(path)
	if err != nil {
		r.associationErr = fmt.Errorf("read pane profile associations: %w", err)
		return r.associationErr
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	var store associationStore
	if err := decoder.Decode(&store); err != nil {
		r.associationErr = fmt.Errorf("decode pane profile associations: %w", err)
		return r.associationErr
	}
	if err := requireJSONEOF(decoder); err != nil || store.Version != 1 {
		r.associationErr = errors.New("pane profile association store has an unsupported format")
		return r.associationErr
	}
	loaded := make(map[string]Association, len(store.Associations))
	native := make(map[string]string, len(store.Associations))
	for _, association := range store.Associations {
		if !validIdentifier(association.PaneID) || !validIdentifier(association.NativeSessionID) || !validIdentifier(association.ProfileID) {
			r.associationErr = errors.New("pane profile association store contains an invalid identifier")
			return r.associationErr
		}
		association.PaneID = normalizeIdentifier(association.PaneID)
		association.NativeSessionID = strings.TrimSpace(association.NativeSessionID)
		association.ProfileID = strings.ToLower(strings.TrimSpace(association.ProfileID))
		if !validIdentifier(association.PaneID) || !validIdentifier(association.NativeSessionID) || !validIdentifier(association.ProfileID) {
			r.associationErr = errors.New("pane profile association store contains an invalid identifier")
			return r.associationErr
		}
		if _, duplicate := loaded[association.PaneID]; duplicate || native[association.NativeSessionID] != "" {
			r.associationErr = errors.New("pane profile association store contains a duplicate association")
			return r.associationErr
		}
		loaded[association.PaneID] = association
		native[association.NativeSessionID] = association.PaneID
	}
	r.associations = loaded
	return nil
}

func validObservations(observed []Observation) (map[string]Observation, error) {
	result := make(map[string]Observation, len(observed))
	native := make(map[string]string, len(observed))
	for _, observation := range observed {
		if !validIdentifier(observation.PaneID) || !validIdentifier(observation.NativeSessionID) {
			return nil, errors.New("observed pane association has an invalid identifier")
		}
		observation.PaneID = normalizeIdentifier(observation.PaneID)
		observation.NativeSessionID = strings.TrimSpace(observation.NativeSessionID)
		if !validIdentifier(observation.PaneID) || !validIdentifier(observation.NativeSessionID) {
			return nil, errors.New("observed pane association has an invalid identifier")
		}
		if _, duplicate := result[observation.PaneID]; duplicate || native[observation.NativeSessionID] != "" {
			return nil, errors.New("observed pane associations are ambiguous")
		}
		result[observation.PaneID] = observation
		native[observation.NativeSessionID] = observation.PaneID
	}
	return result, nil
}

func normalizeIdentifier(value string) string {
	return strings.ToLower(strings.TrimSpace(value))
}

func validIdentifier(value string) bool {
	if value == "" || len(value) > 256 {
		return false
	}
	for _, character := range value {
		if character < 0x20 || character == 0x7f {
			return false
		}
	}
	return true
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

func writeAssociationStore(directory string, associations map[string]Association) error {
	return writeAssociationStoreWith(defaultAssociationStoreIO(), directory, associations)
}

func writeAssociationStoreWith(ops associationStoreIO, directory string, associations map[string]Association) error {
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
	data, err := ops.marshal(associationStore{Version: 1, Associations: values})
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
	if _, err := temporary.Write(data); err != nil {
		temporary.Close()
		return err
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
