package activeruntime

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"
)

const maxSnapshotBytes = 64 * 1024

type Snapshot struct {
	SchemaVersion         int    `json:"schemaVersion"`
	Generation            string `json:"generation"`
	SessionName           string `json:"sessionName"`
	SocketPath            string `json:"socketPath"`
	ExpectedInventoryPath string `json:"expectedInventoryPath"`
}

type snapshotFile interface {
	io.Reader
	Stat() (os.FileInfo, error)
	Close() error
}

type loadIO struct {
	lstat func(string) (os.FileInfo, error)
	open  func(string) (snapshotFile, error)
}

func defaultLoadIO() loadIO {
	return loadIO{
		lstat: os.Lstat,
		open: func(path string) (snapshotFile, error) {
			return os.Open(path)
		},
	}
}

func Load(path string) (Snapshot, error) {
	return loadWith(defaultLoadIO(), path)
}

func loadWith(ops loadIO, path string) (Snapshot, error) {
	before, err := ops.lstat(path)
	if err != nil {
		return Snapshot{}, err
	}
	if err := validateSnapshotFile(before); err != nil {
		return Snapshot{}, err
	}
	file, err := ops.open(path)
	if err != nil {
		return Snapshot{}, err
	}
	after, statErr := file.Stat()
	if statErr != nil {
		_ = file.Close()
		return Snapshot{}, statErr
	}
	if err := validateSnapshotFile(after); err != nil || !os.SameFile(before, after) {
		_ = file.Close()
		return Snapshot{}, errors.New("active runtime changed or became unsafe while opening")
	}
	data, readErr := io.ReadAll(io.LimitReader(file, maxSnapshotBytes+1))
	closeErr := file.Close()
	if readErr != nil {
		return Snapshot{}, readErr
	}
	if closeErr != nil {
		return Snapshot{}, closeErr
	}
	if len(data) > maxSnapshotBytes {
		return Snapshot{}, errors.New("active runtime exceeds size limit")
	}

	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	var snapshot Snapshot
	if err := decoder.Decode(&snapshot); err != nil {
		return Snapshot{}, fmt.Errorf("decode active runtime: %w", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return Snapshot{}, errors.New("active runtime has trailing content")
	}
	if err := validateSnapshot(path, snapshot); err != nil {
		return Snapshot{}, err
	}
	return snapshot, nil
}

func validateSnapshotFile(info os.FileInfo) error {
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !info.Mode().IsRegular() || info.Mode().Perm()&0o077 != 0 || info.Size() > maxSnapshotBytes || !ok || stat.Nlink != 1 {
		return errors.New("active runtime must be a private bounded regular file with one link")
	}
	return nil
}

func validateSnapshot(path string, snapshot Snapshot) error {
	if snapshot.SchemaVersion != 1 || !validName(snapshot.Generation) || snapshot.SessionName != snapshot.Generation {
		return errors.New("active runtime has an unsupported identity")
	}
	root := filepath.Dir(path)
	sessionRoot := filepath.Join(root, "sessions", snapshot.Generation)
	wantSocket := filepath.Join(sessionRoot, "herdr.sock")
	wantInventory := filepath.Join(sessionRoot, "expected-inventory.json")
	if !normalizedAbsolute(snapshot.SocketPath) || snapshot.SocketPath != wantSocket {
		return errors.New("active runtime socket path is outside its generation")
	}
	if !normalizedAbsolute(snapshot.ExpectedInventoryPath) || snapshot.ExpectedInventoryPath != wantInventory {
		return errors.New("active runtime inventory path is outside its generation")
	}
	return nil
}

func validName(value string) bool {
	if value == "" || len(value) > 256 || strings.TrimSpace(value) != value {
		return false
	}
	for _, character := range value {
		if character < 0x20 || character == 0x7f || character == '/' || character == '\\' {
			return false
		}
	}
	return true
}

func normalizedAbsolute(path string) bool {
	return filepath.IsAbs(path) && filepath.Clean(path) == path
}
