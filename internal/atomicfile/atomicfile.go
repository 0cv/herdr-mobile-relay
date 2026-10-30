// Package atomicfile provides a narrow byte-replacement operation.
package atomicfile

import (
	"io"
	"os"
)

// Outcome records whether Rename returned success, not whether data is durable.
type Outcome uint8

const (
	// RenameNotConfirmed means no successful Rename return was observed. It
	// always accompanies an error, but a rename error can still mean published.
	RenameNotConfirmed Outcome = iota
	// Renamed means Rename returned success; later replacement is still possible.
	Renamed
)

type tempFile interface {
	Name() string
	Chmod(os.FileMode) error
	Write([]byte) (int, error)
	Sync() error
	Close() error
}

type directoryFile interface {
	Sync() error
	Close() error
}

type operations struct {
	createTemp func(string, string) (tempFile, error)
	rename     func(string, string) error
	open       func(string) (directoryFile, error)
	remove     func(string) error
}

// Replace writes data to a temporary name, renames it to target, then syncs
// directory. It passes all paths and the pattern unchanged. Callers must keep
// path resolution (including the working directory) stable; same-parent and
// directory-sync claims require directory to be target's actual parent. All
// possible temporary names must be distinct from target, including aliases.
// Cleanup removes only the temporary name, not explicitly target. No hostile
// path-race, containment, physical durability or writer-ordering guarantee is
// made. Directory-open or sync errors after a successful rename return Renamed.
func Replace(directory, target, pattern string, data []byte, mode os.FileMode) (Outcome, error) {
	return replace(directory, target, pattern, data, mode, operations{
		createTemp: func(dir, pattern string) (tempFile, error) { return os.CreateTemp(dir, pattern) },
		rename:     os.Rename,
		open:       func(dir string) (directoryFile, error) { return os.Open(dir) },
		remove:     os.Remove,
	})
}

func replace(directory, target, pattern string, data []byte, mode os.FileMode, ops operations) (Outcome, error) {
	temp, err := ops.createTemp(directory, pattern)
	if err != nil {
		return RenameNotConfirmed, err
	}
	tempPath := temp.Name()
	defer ops.remove(tempPath)
	if err := temp.Chmod(mode); err != nil {
		_ = temp.Close()
		return RenameNotConfirmed, err
	}
	written, writeErr := temp.Write(data)
	if writeErr == nil && written != len(data) {
		writeErr = io.ErrShortWrite
	}
	if writeErr == nil {
		writeErr = temp.Sync()
	}
	closeErr := temp.Close()
	if writeErr != nil {
		return RenameNotConfirmed, writeErr
	}
	if closeErr != nil {
		return RenameNotConfirmed, closeErr
	}
	if err := ops.rename(tempPath, target); err != nil {
		return RenameNotConfirmed, err
	}
	dir, err := ops.open(directory)
	if err != nil {
		return Renamed, err
	}
	err = dir.Sync()
	_ = dir.Close()
	return Renamed, err
}
