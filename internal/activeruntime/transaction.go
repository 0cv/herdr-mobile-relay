package activeruntime

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"syscall"
)

const (
	TopologyTransactionName = "topology-transaction.json"
	maxTransactionBytes     = 64 * 1024
)

type TopologyPane struct {
	PaneID          string `json:"pane_id"`
	NativeSessionID string `json:"native_session_id"`
	ProfileID       string `json:"profile_id"`
	WorkspaceID     string `json:"workspace_id,omitempty"`
}

type TopologyTransactionRecord struct {
	SchemaVersion int            `json:"schema_version"`
	Generation    string         `json:"generation"`
	RequestID     string         `json:"request_id"`
	Action        string         `json:"action"`
	Target        string         `json:"target,omitempty"`
	Panes         []TopologyPane `json:"panes"`
}

type TopologyTransaction struct {
	root topologyRoot
	once sync.Once
	err  error
}

type topologyFile interface {
	io.Writer
	Stat() (os.FileInfo, error)
	Sync() error
	Close() error
}

type topologyDirectory interface {
	Sync() error
	Close() error
}

type topologyRoot interface {
	OpenFile(string, int, os.FileMode) (topologyFile, error)
	OpenDirectory(string) (topologyDirectory, error)
	Lstat(string) (os.FileInfo, error)
	Remove(string) error
	Stat(string) (os.FileInfo, error)
	Close() error
}

type osTopologyRoot struct {
	root *os.Root
}

func (r *osTopologyRoot) OpenFile(name string, flag int, perm os.FileMode) (topologyFile, error) {
	return r.root.OpenFile(name, flag, perm)
}

func (r *osTopologyRoot) OpenDirectory(name string) (topologyDirectory, error) {
	return r.root.Open(name)
}

func (r *osTopologyRoot) Lstat(name string) (os.FileInfo, error) { return r.root.Lstat(name) }
func (r *osTopologyRoot) Remove(name string) error               { return r.root.Remove(name) }
func (r *osTopologyRoot) Stat(name string) (os.FileInfo, error)  { return r.root.Stat(name) }
func (r *osTopologyRoot) Close() error                           { return r.root.Close() }

type topologyTransactionIO struct {
	load     func(string) (Snapshot, error)
	marshal  func(any) ([]byte, error)
	openRoot func(string) (topologyRoot, error)
}

func defaultTopologyTransactionIO() topologyTransactionIO {
	return topologyTransactionIO{load: Load, marshal: json.Marshal, openRoot: openRuntimeRoot}
}

func BeginTopologyTransaction(activeRuntimePath string, record TopologyTransactionRecord) (*TopologyTransaction, error) {
	return beginTopologyTransactionWith(defaultTopologyTransactionIO(), activeRuntimePath, record)
}

func beginTopologyTransactionWith(ops topologyTransactionIO, activeRuntimePath string, record TopologyTransactionRecord) (*TopologyTransaction, error) {
	if err := validateActiveRuntimePath(activeRuntimePath); err != nil {
		return nil, err
	}
	active, err := ops.load(activeRuntimePath)
	if err != nil {
		return nil, fmt.Errorf("load active runtime for topology transaction: %w", err)
	}
	if record.Generation != active.Generation {
		return nil, errors.New("topology transaction generation does not match the active runtime")
	}
	canonical, err := validateTopologyRecord(record)
	if err != nil {
		return nil, err
	}
	data, err := ops.marshal(canonical)
	if err != nil {
		return nil, fmt.Errorf("encode topology transaction: %w", err)
	}
	data = append(data, '\n')
	if len(data) > maxTransactionBytes {
		return nil, errors.New("topology transaction exceeds size limit")
	}

	root, err := ops.openRoot(activeRuntimePath)
	if err != nil {
		return nil, err
	}
	keepRoot := false
	defer func() {
		if !keepRoot {
			_ = root.Close()
		}
	}()
	marker, err := root.OpenFile(TopologyTransactionName, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return nil, fmt.Errorf("create topology transaction: %w", err)
	}
	markerOpen := true
	defer func() {
		if markerOpen {
			_ = marker.Close()
		}
	}()
	if info, statErr := marker.Stat(); statErr != nil || !validTransactionFile(info) {
		return nil, errors.New("topology transaction file is unsafe")
	}
	written, err := marker.Write(data)
	if err != nil {
		return nil, fmt.Errorf("write topology transaction: %w", err)
	}
	if written != len(data) {
		return nil, io.ErrShortWrite
	}
	if err := marker.Sync(); err != nil {
		return nil, fmt.Errorf("sync topology transaction: %w", err)
	}
	if err := marker.Close(); err != nil {
		markerOpen = false
		return nil, fmt.Errorf("close topology transaction: %w", err)
	}
	markerOpen = false
	if err := syncRuntimeRoot(root); err != nil {
		return nil, fmt.Errorf("sync topology transaction directory: %w", err)
	}
	keepRoot = true
	return &TopologyTransaction{root: root}, nil
}

func TopologyTransactionPending(activeRuntimePath string) (bool, error) {
	return topologyTransactionPendingWith(openRuntimeRoot, activeRuntimePath)
}

func topologyTransactionPendingWith(openRoot func(string) (topologyRoot, error), activeRuntimePath string) (bool, error) {
	if err := validateActiveRuntimePath(activeRuntimePath); err != nil {
		return true, err
	}
	root, err := openRoot(activeRuntimePath)
	if err != nil {
		return true, err
	}
	defer root.Close()
	_, err = root.Lstat(TopologyTransactionName)
	if err == nil {
		return true, nil
	}
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	return true, fmt.Errorf("inspect topology transaction: %w", err)
}

func (t *TopologyTransaction) Resolve() error {
	if t == nil {
		return nil
	}
	t.once.Do(func() {
		removeErr := t.root.Remove(TopologyTransactionName)
		if removeErr == nil {
			removeErr = syncRuntimeRoot(t.root)
		}
		t.err = errors.Join(removeErr, t.root.Close())
	})
	return t.err
}

func validateActiveRuntimePath(path string) error {
	if !normalizedAbsolute(path) || filepath.Base(path) != "active-runtime.json" {
		return errors.New("topology transaction requires an absolute normalized active-runtime.json path")
	}
	return nil
}

func validateTopologyRecord(record TopologyTransactionRecord) (TopologyTransactionRecord, error) {
	if !validTransactionValue(record.Generation, true) || !validTransactionValue(record.RequestID, true) || !validTransactionValue(record.Action, true) || !validTransactionValue(record.Target, false) {
		return TopologyTransactionRecord{}, errors.New("topology transaction identity is invalid")
	}
	record.SchemaVersion = 1
	record.Panes = append([]TopologyPane(nil), record.Panes...)
	paneIDs := make(map[string]bool, len(record.Panes))
	nativeSessionIDs := make(map[string]bool, len(record.Panes))
	for _, pane := range record.Panes {
		if !validTransactionValue(pane.PaneID, true) || !validTransactionValue(pane.NativeSessionID, true) || !validTransactionValue(pane.ProfileID, true) || !validTransactionValue(pane.WorkspaceID, false) || paneIDs[pane.PaneID] || nativeSessionIDs[pane.NativeSessionID] {
			return TopologyTransactionRecord{}, errors.New("topology transaction pane inventory is invalid")
		}
		paneIDs[pane.PaneID] = true
		nativeSessionIDs[pane.NativeSessionID] = true
	}
	sort.Slice(record.Panes, func(left, right int) bool { return record.Panes[left].PaneID < record.Panes[right].PaneID })
	return record, nil
}

func validTransactionValue(value string, required bool) bool {
	if value == "" {
		return !required
	}
	if len(value) > 512 || strings.TrimSpace(value) != value {
		return false
	}
	return strings.IndexFunc(value, func(character rune) bool { return character < 0x20 || character == 0x7f }) == -1
}

type runtimeRootIO struct {
	lstat func(string) (os.FileInfo, error)
	open  func(string) (topologyRoot, error)
}

func defaultRuntimeRootIO() runtimeRootIO {
	return runtimeRootIO{
		lstat: os.Lstat,
		open: func(path string) (topologyRoot, error) {
			root, err := os.OpenRoot(path)
			if err != nil {
				return nil, err
			}
			return &osTopologyRoot{root: root}, nil
		},
	}
}

func openRuntimeRoot(activeRuntimePath string) (topologyRoot, error) {
	return openRuntimeRootWith(defaultRuntimeRootIO(), activeRuntimePath)
}

func openRuntimeRootWith(ops runtimeRootIO, activeRuntimePath string) (topologyRoot, error) {
	path := filepath.Dir(activeRuntimePath)
	before, err := ops.lstat(path)
	if err != nil {
		return nil, fmt.Errorf("inspect active runtime directory: %w", err)
	}
	if !validRuntimeDirectory(before) {
		return nil, errors.New("active runtime directory must be an owned physical 0700 directory")
	}
	root, err := ops.open(path)
	if err != nil {
		return nil, fmt.Errorf("open active runtime directory: %w", err)
	}
	after, err := root.Stat(".")
	if err != nil || !os.SameFile(before, after) || !validRuntimeDirectory(after) {
		_ = root.Close()
		return nil, errors.New("active runtime directory changed or became unsafe while opening")
	}
	return root, nil
}

func validRuntimeDirectory(info os.FileInfo) bool {
	stat, ok := info.Sys().(*syscall.Stat_t)
	return info.IsDir() && info.Mode().Perm() == 0o700 && ok && stat.Uid == uint32(os.Getuid())
}

func validTransactionFile(info os.FileInfo) bool {
	stat, ok := info.Sys().(*syscall.Stat_t)
	return info.Mode().IsRegular() && info.Mode().Perm() == 0o600 && info.Size() <= maxTransactionBytes && ok && stat.Nlink == 1 && stat.Uid == uint32(os.Getuid())
}

func syncRuntimeRoot(root topologyRoot) error {
	directory, err := root.OpenDirectory(".")
	if err != nil {
		return err
	}
	return errors.Join(directory.Sync(), directory.Close())
}
