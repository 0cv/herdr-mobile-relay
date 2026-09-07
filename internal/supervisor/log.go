package supervisor

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"syscall"
)

type rotatingWriter struct {
	mu       sync.Mutex
	path     string
	maxBytes int64
	backups  int
	ops      logIO
}

type logFile interface {
	Chmod(os.FileMode) error
	Write([]byte) (int, error)
	Stat() (os.FileInfo, error)
	Close() error
}

type logIO struct {
	mkdirAll func(string, os.FileMode) error
	chmod    func(string, os.FileMode) error
	lstat    func(string) (os.FileInfo, error)
	openFile func(string, int, os.FileMode) (logFile, error)
	remove   func(string) error
	rename   func(string, string) error
}

func defaultLogIO() logIO {
	return logIO{
		mkdirAll: os.MkdirAll, chmod: os.Chmod, lstat: os.Lstat, remove: os.Remove, rename: os.Rename,
		openFile: func(path string, flag int, mode os.FileMode) (logFile, error) {
			return os.OpenFile(path, flag|syscall.O_NOFOLLOW, mode)
		},
	}
}

func newRotatingWriter(path string, maxBytes int64, backups int) (*rotatingWriter, error) {
	return newRotatingWriterWith(defaultLogIO(), path, maxBytes, backups)
}

func newRotatingWriterWith(ops logIO, path string, maxBytes int64, backups int) (*rotatingWriter, error) {
	if maxBytes < 1 || backups < 0 {
		return nil, errors.New("invalid log rotation limits")
	}
	if err := ops.mkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	if err := ops.chmod(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	directory, err := ops.lstat(filepath.Dir(path))
	if err != nil {
		return nil, err
	}
	if !directory.IsDir() || directory.Mode()&os.ModeSymlink != 0 {
		return nil, errors.New("log directory must be a real directory")
	}
	return &rotatingWriter{path: path, maxBytes: maxBytes, backups: backups, ops: ops}, nil
}

func (w *rotatingWriter) Write(data []byte) (int, error) {
	originalLength := len(data)
	if int64(len(data)) > w.maxBytes {
		data = data[len(data)-int(w.maxBytes):]
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	info, err := w.ops.lstat(w.path)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return 0, err
	}
	if err == nil && !info.Mode().IsRegular() {
		return 0, errors.New("log path must be a regular file")
	}
	if err == nil && info.Size()+int64(len(data)) > w.maxBytes {
		if err := w.rotate(); err != nil {
			return 0, err
		}
	}
	file, err := w.ops.openFile(w.path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return 0, err
	}
	if err := file.Chmod(0o600); err != nil {
		file.Close()
		return 0, err
	}
	opened, statErr := file.Stat()
	if statErr != nil || !opened.Mode().IsRegular() {
		file.Close()
		if statErr != nil {
			return 0, statErr
		}
		return 0, errors.New("opened log is not a regular file")
	}
	written, writeErr := file.Write(data)
	closeErr := file.Close()
	if writeErr != nil {
		return 0, writeErr
	}
	if written != len(data) {
		return 0, io.ErrShortWrite
	}
	if closeErr != nil {
		return 0, closeErr
	}
	return originalLength, nil
}

func (w *rotatingWriter) rotate() error {
	if w.backups == 0 {
		err := w.ops.remove(w.path)
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return err
	}
	if err := w.ops.remove(w.path + "." + strconv.Itoa(w.backups)); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	for index := w.backups - 1; index >= 1; index-- {
		oldPath := w.path + "." + strconv.Itoa(index)
		newPath := w.path + "." + strconv.Itoa(index+1)
		if err := w.ops.rename(oldPath, newPath); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	return w.ops.rename(w.path, w.path+".1")
}
