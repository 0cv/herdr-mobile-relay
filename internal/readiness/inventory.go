package readiness

import (
	"bytes"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
)

const maxManifestBytes = 64 * 1024

type State string

const (
	StateReady                      State = "ready"
	StateAcknowledgedEmpty          State = "acknowledged_empty"
	StateUnexpectedEmpty            State = "unexpected_empty"
	StateInventoryMismatch          State = "inventory_mismatch"
	StateGenerationMismatch         State = "generation_mismatch"
	StateInvalidManifest            State = "invalid_manifest"
	StateInvalidInventory           State = "invalid_inventory"
	StateInvalidActiveRuntime       State = "invalid_active_runtime"
	StateActiveRuntimeMismatch      State = "active_runtime_mismatch"
	StateTopologyTransactionPending State = "topology_transaction_pending"
	StateUnavailable                State = "unavailable"
)

type Pane struct {
	PaneID          string `json:"pane_id"`
	NativeSessionID string `json:"native_session_id"`
	ProfileID       string `json:"profile_id"`
}

type manifest struct {
	Version           int    `json:"version"`
	Generation        string `json:"generation"`
	AcknowledgedEmpty bool   `json:"acknowledged_empty"`
	Panes             []Pane `json:"panes"`
}

type manifestFile interface {
	io.Reader
	Stat() (os.FileInfo, error)
	Close() error
}

type checkIO struct {
	lstat func(string) (os.FileInfo, error)
	open  func(string) (manifestFile, error)
}

type publishFile interface {
	io.Writer
	Stat() (os.FileInfo, error)
	Sync() error
	Close() error
}

type publishDirectory interface {
	Sync() error
	Close() error
}

type publishRoot interface {
	OpenFile(string, int, os.FileMode) (publishFile, error)
	OpenDirectory(string) (publishDirectory, error)
	Lstat(string) (os.FileInfo, error)
	Stat(string) (os.FileInfo, error)
	Rename(string, string) error
	Remove(string) error
	Close() error
}

type osPublishRoot struct {
	root *os.Root
}

func (r *osPublishRoot) OpenFile(name string, flag int, perm os.FileMode) (publishFile, error) {
	return r.root.OpenFile(name, flag, perm)
}

func (r *osPublishRoot) OpenDirectory(name string) (publishDirectory, error) {
	return r.root.Open(name)
}

func (r *osPublishRoot) Lstat(name string) (os.FileInfo, error) { return r.root.Lstat(name) }
func (r *osPublishRoot) Stat(name string) (os.FileInfo, error)  { return r.root.Stat(name) }
func (r *osPublishRoot) Rename(oldName, newName string) error {
	return r.root.Rename(oldName, newName)
}
func (r *osPublishRoot) Remove(name string) error { return r.root.Remove(name) }
func (r *osPublishRoot) Close() error             { return r.root.Close() }

type publishIO struct {
	lstat      func(string) (os.FileInfo, error)
	openRoot   func(string) (publishRoot, error)
	marshal    func(any) ([]byte, error)
	randomName func() string
}

func defaultPublishIO() publishIO {
	return publishIO{
		lstat: os.Lstat,
		openRoot: func(path string) (publishRoot, error) {
			root, err := os.OpenRoot(path)
			if err != nil {
				return nil, err
			}
			return &osPublishRoot{root: root}, nil
		},
		marshal:    json.Marshal,
		randomName: rand.Text,
	}
}

func defaultCheckIO() checkIO {
	return checkIO{
		lstat: os.Lstat,
		open: func(path string) (manifestFile, error) {
			return os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW, 0)
		},
	}
}

type Result struct {
	Ready      bool   `json:"ready"`
	State      State  `json:"state"`
	Generation string `json:"generation,omitempty"`
	Expected   int    `json:"expected"`
	Observed   int    `json:"observed"`
}

func Check(path, activeGeneration string, observed []Pane) Result {
	return checkWith(defaultCheckIO(), path, activeGeneration, observed)
}

// Publish replaces the expected inventory through a directory handle pinned to
// the active generation. The file and directory syncs make a successful return
// a crash-durable commit rather than merely a visible rename.
func Publish(path, generation string, panes []Pane, acknowledgedEmpty bool) error {
	return publishWith(defaultPublishIO(), path, generation, panes, acknowledgedEmpty)
}

func publishWith(ops publishIO, path, generation string, panes []Pane, acknowledgedEmpty bool) error {
	if !filepath.IsAbs(path) || filepath.Clean(path) != path || filepath.Base(path) != "expected-inventory.json" {
		return errors.New("expected inventory path must be an absolute normalized expected-inventory.json path")
	}
	if strings.TrimSpace(generation) != generation || !validValue(generation) {
		return errors.New("expected inventory generation is invalid")
	}
	canonical := make([]Pane, len(panes))
	for index, pane := range panes {
		canonical[index] = Pane{
			PaneID:          strings.TrimSpace(pane.PaneID),
			NativeSessionID: strings.TrimSpace(pane.NativeSessionID),
			ProfileID:       strings.TrimSpace(pane.ProfileID),
		}
	}
	if _, valid := paneSet(canonical); !valid || acknowledgedEmpty && len(canonical) != 0 {
		return errors.New("expected inventory panes are invalid")
	}
	sort.Slice(canonical, func(left, right int) bool { return canonical[left].PaneID < canonical[right].PaneID })
	data, err := ops.marshal(manifest{Version: 1, Generation: generation, AcknowledgedEmpty: acknowledgedEmpty, Panes: canonical})
	if err != nil {
		return fmt.Errorf("encode expected inventory: %w", err)
	}
	data = append(data, '\n')
	if len(data) > maxManifestBytes {
		return errors.New("expected inventory exceeds size limit")
	}

	directoryPath := filepath.Dir(path)
	directoryBefore, err := ops.lstat(directoryPath)
	if err != nil {
		return fmt.Errorf("inspect expected inventory directory: %w", err)
	}
	if !validPublishDirectory(directoryBefore) {
		return errors.New("expected inventory directory must be an owned physical 0700 directory")
	}
	directory, err := ops.openRoot(directoryPath)
	if err != nil {
		return fmt.Errorf("open expected inventory directory: %w", err)
	}
	defer directory.Close()
	directoryAfter, err := directory.Stat(".")
	if err != nil || !os.SameFile(directoryBefore, directoryAfter) || !validPublishDirectory(directoryAfter) {
		return errors.New("expected inventory directory changed or became unsafe while opening")
	}

	name := filepath.Base(path)
	if current, statErr := directory.Lstat(name); statErr == nil {
		if !validPublishManifestFile(current) {
			return errors.New("existing expected inventory must be an owned regular 0600 file with one link")
		}
	} else if !errors.Is(statErr, os.ErrNotExist) {
		return fmt.Errorf("inspect existing expected inventory: %w", statErr)
	}

	temporaryName := ".expected-inventory." + ops.randomName()
	temporary, err := directory.OpenFile(temporaryName, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return fmt.Errorf("create expected inventory temporary file: %w", err)
	}
	temporaryOpen := true
	defer func() {
		if temporaryOpen {
			_ = temporary.Close()
		}
		_ = directory.Remove(temporaryName)
	}()
	if info, statErr := temporary.Stat(); statErr != nil || !validPublishManifestFile(info) {
		return errors.New("expected inventory temporary file is unsafe")
	}
	written, err := temporary.Write(data)
	if err != nil {
		return fmt.Errorf("write expected inventory: %w", err)
	}
	if written != len(data) {
		return io.ErrShortWrite
	}
	if err := temporary.Sync(); err != nil {
		return fmt.Errorf("sync expected inventory: %w", err)
	}
	if err := temporary.Close(); err != nil {
		temporaryOpen = false
		return fmt.Errorf("close expected inventory: %w", err)
	}
	temporaryOpen = false
	if err := directory.Rename(temporaryName, name); err != nil {
		return fmt.Errorf("publish expected inventory: %w", err)
	}
	if err := syncRoot(directory); err != nil {
		return fmt.Errorf("sync expected inventory directory: %w", err)
	}
	return nil
}

func validPublishDirectory(info os.FileInfo) bool {
	stat, ok := info.Sys().(*syscall.Stat_t)
	return info.IsDir() && info.Mode().Perm() == 0o700 && ok && stat.Uid == uint32(os.Getuid())
}

func validPublishManifestFile(info os.FileInfo) bool {
	stat, ok := info.Sys().(*syscall.Stat_t)
	return info.Mode().IsRegular() && info.Mode().Perm() == 0o600 && info.Size() <= maxManifestBytes && ok && stat.Nlink == 1 && stat.Uid == uint32(os.Getuid())
}

func syncRoot(root publishRoot) error {
	directory, err := root.OpenDirectory(".")
	if err != nil {
		return err
	}
	syncErr := directory.Sync()
	return errors.Join(syncErr, directory.Close())
}

func checkWith(ops checkIO, path, activeGeneration string, observed []Pane) Result {
	result := Result{State: StateUnavailable, Observed: len(observed)}
	before, err := ops.lstat(path)
	if err != nil {
		return result
	}
	if !validManifestFile(before) {
		result.State = StateInvalidManifest
		return result
	}
	file, err := ops.open(path)
	if err != nil {
		return result
	}
	after, statErr := file.Stat()
	if statErr != nil {
		_ = file.Close()
		return result
	}
	if !validManifestFile(after) || !os.SameFile(before, after) {
		_ = file.Close()
		result.State = StateInvalidManifest
		return result
	}
	data, readErr := io.ReadAll(io.LimitReader(file, maxManifestBytes+1))
	closeErr := file.Close()
	if readErr != nil || closeErr != nil {
		return result
	}
	if len(data) > maxManifestBytes {
		result.State = StateInvalidManifest
		return result
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	var expected manifest
	if decoder.Decode(&expected) != nil || trailingJSON(decoder) != nil || expected.Version != 1 {
		result.State = StateInvalidManifest
		return result
	}
	result.Generation = expected.Generation
	result.Expected = len(expected.Panes)
	if !validValue(expected.Generation) || expected.Generation != strings.TrimSpace(activeGeneration) {
		result.State = StateGenerationMismatch
		return result
	}
	expectedSet, valid := paneSet(expected.Panes)
	if !valid || expected.AcknowledgedEmpty && len(expected.Panes) != 0 {
		result.State = StateInvalidManifest
		return result
	}
	observedSet, valid := paneSet(observed)
	if !valid {
		result.State = StateInvalidInventory
		return result
	}
	if len(expectedSet) == 0 && len(observedSet) == 0 {
		if expected.AcknowledgedEmpty {
			result.Ready = true
			result.State = StateAcknowledgedEmpty
		} else {
			result.State = StateUnexpectedEmpty
		}
		return result
	}
	if len(observedSet) == 0 {
		result.State = StateUnexpectedEmpty
		return result
	}
	if !samePaneSet(expectedSet, observedSet) {
		result.State = StateInventoryMismatch
		return result
	}
	result.Ready = true
	result.State = StateReady
	return result
}

func validManifestFile(info os.FileInfo) bool {
	stat, ok := info.Sys().(*syscall.Stat_t)
	return info.Mode().IsRegular() && info.Mode().Perm()&0o077 == 0 && info.Size() <= maxManifestBytes && ok && stat.Nlink == 1
}

func paneSet(panes []Pane) (map[string]Pane, bool) {
	result := make(map[string]Pane, len(panes))
	nativeSessions := make(map[string]bool, len(panes))
	for _, pane := range panes {
		pane.PaneID = strings.TrimSpace(pane.PaneID)
		pane.NativeSessionID = strings.TrimSpace(pane.NativeSessionID)
		pane.ProfileID = strings.TrimSpace(pane.ProfileID)
		if !validValue(pane.PaneID) || !validValue(pane.NativeSessionID) || !validValue(pane.ProfileID) {
			return nil, false
		}
		if _, duplicate := result[pane.PaneID]; duplicate || nativeSessions[pane.NativeSessionID] {
			return nil, false
		}
		result[pane.PaneID] = pane
		nativeSessions[pane.NativeSessionID] = true
	}
	return result, true
}

func samePaneSet(expected, observed map[string]Pane) bool {
	if len(expected) != len(observed) {
		return false
	}
	for paneID, pane := range expected {
		if observed[paneID] != pane {
			return false
		}
	}
	return true
}

func validValue(value string) bool {
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

func trailingJSON(decoder *json.Decoder) error {
	var value any
	err := decoder.Decode(&value)
	if errors.Is(err, io.EOF) {
		return nil
	}
	if err == nil {
		return errors.New("trailing JSON")
	}
	return err
}
