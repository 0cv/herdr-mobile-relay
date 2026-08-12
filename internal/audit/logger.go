package audit

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

const (
	maxAuditBytes = 5 * 1024 * 1024
	maxRotations  = 3
)

type Record struct {
	Timestamp    string         `json:"timestamp"`
	Stage        string         `json:"stage"`
	Action       string         `json:"action"`
	RequestID    string         `json:"request_id,omitempty"`
	ClientID     string         `json:"client_id"`
	ConnectionID string         `json:"connection_id,omitempty"`
	PaneID       string         `json:"pane_id,omitempty"`
	Agent        string         `json:"agent,omitempty"`
	Project      string         `json:"project,omitempty"`
	Session      string         `json:"session,omitempty"`
	Host         string         `json:"host,omitempty"`
	OK           *bool          `json:"ok,omitempty"`
	Phase        string         `json:"phase,omitempty"`
	Error        string         `json:"error,omitempty"`
	Details      map[string]any `json:"details,omitempty"`
}

type Logger struct {
	mu   sync.Mutex
	dir  string
	path string
}

func Open(cacheDir string) (*Logger, error) {
	dir := filepath.Join(cacheDir, "audit")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("create audit directory: %w", err)
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		return nil, fmt.Errorf("protect audit directory: %w", err)
	}
	logger := &Logger{dir: dir, path: filepath.Join(dir, "remote-writes.jsonl")}
	if info, err := os.Lstat(logger.path); err == nil {
		if !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
			return nil, errors.New("audit path is not a regular file")
		}
		if err := os.Chmod(logger.path, 0o600); err != nil {
			return nil, fmt.Errorf("protect audit file: %w", err)
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("inspect audit file: %w", err)
	}
	return logger, nil
}

func (l *Logger) Append(record Record) error {
	if l == nil {
		return nil
	}
	record.Timestamp = time.Now().UTC().Format(time.RFC3339Nano)
	record.Stage = clamp(record.Stage, 32)
	record.Action = clamp(record.Action, 80)
	record.RequestID = clamp(record.RequestID, 160)
	record.ClientID = clamp(record.ClientID, 160)
	record.ConnectionID = clamp(record.ConnectionID, 160)
	record.PaneID = clamp(record.PaneID, 160)
	record.Agent = clamp(record.Agent, 160)
	record.Project = clamp(record.Project, 512)
	record.Session = clamp(record.Session, 512)
	record.Host = clamp(record.Host, 255)
	record.Phase = clamp(record.Phase, 80)
	record.Error = clamp(record.Error, 1000)
	line, err := json.Marshal(record)
	if err != nil {
		return fmt.Errorf("encode audit record: %w", err)
	}
	line = append(line, '\n')
	if len(line) > 128*1024 {
		return errors.New("audit record exceeds size limit")
	}

	l.mu.Lock()
	defer l.mu.Unlock()
	if err := l.rotateIfNeeded(int64(len(line))); err != nil {
		return err
	}
	file, err := l.openAppendFile()
	if err != nil {
		return err
	}
	written, writeErr := file.Write(line)
	if writeErr == nil && written != len(line) {
		writeErr = io.ErrShortWrite
	}
	syncErr := file.Sync()
	closeErr := file.Close()
	if writeErr != nil || syncErr != nil || closeErr != nil {
		return fmt.Errorf("append audit record: %w", errors.Join(writeErr, syncErr, closeErr))
	}
	return nil
}

func (l *Logger) openAppendFile() (*os.File, error) {
	for attempt := 0; attempt < 3; attempt++ {
		info, err := os.Lstat(l.path)
		if errors.Is(err, os.ErrNotExist) {
			file, createErr := os.OpenFile(l.path, os.O_CREATE|os.O_EXCL|os.O_APPEND|os.O_WRONLY, 0o600)
			if errors.Is(createErr, os.ErrExist) {
				continue
			}
			if createErr != nil {
				return nil, fmt.Errorf("create audit file: %w", createErr)
			}
			opened, statErr := file.Stat()
			if statErr != nil {
				_ = file.Close()
				return nil, fmt.Errorf("validate new audit file: %w", statErr)
			}
			if !opened.Mode().IsRegular() {
				_ = file.Close()
				return nil, errors.New("new audit path is not a regular file")
			}
			if chmodErr := file.Chmod(0o600); chmodErr != nil {
				_ = file.Close()
				return nil, fmt.Errorf("protect audit file: %w", chmodErr)
			}
			return file, nil
		}
		if err != nil {
			return nil, fmt.Errorf("inspect audit file: %w", err)
		}
		if !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
			return nil, errors.New("audit path is not a regular file")
		}
		file, openErr := os.OpenFile(l.path, os.O_APPEND|os.O_WRONLY, 0o600)
		if errors.Is(openErr, os.ErrNotExist) {
			continue
		}
		if openErr != nil {
			return nil, fmt.Errorf("open audit file: %w", openErr)
		}
		opened, statErr := file.Stat()
		if statErr != nil {
			_ = file.Close()
			return nil, fmt.Errorf("validate audit file: %w", statErr)
		}
		if !opened.Mode().IsRegular() || !os.SameFile(info, opened) {
			_ = file.Close()
			return nil, errors.New("audit file changed while it was opening")
		}
		if chmodErr := file.Chmod(0o600); chmodErr != nil {
			_ = file.Close()
			return nil, fmt.Errorf("protect audit file: %w", chmodErr)
		}
		return file, nil
	}
	return nil, errors.New("audit file changed repeatedly while it was opening")
}

func (l *Logger) rotateIfNeeded(nextBytes int64) error {
	info, err := os.Lstat(l.path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("inspect audit file: %w", err)
	}
	if !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
		return errors.New("audit path is not a regular file")
	}
	if info.Size()+nextBytes <= maxAuditBytes {
		return nil
	}
	for index := maxRotations; index >= 1; index-- {
		oldPath := l.path
		if index > 1 {
			oldPath = fmt.Sprintf("%s.%d", l.path, index-1)
		}
		newPath := fmt.Sprintf("%s.%d", l.path, index)
		if index == maxRotations {
			if err := os.Remove(newPath); err != nil && !errors.Is(err, os.ErrNotExist) {
				return fmt.Errorf("remove oldest audit rotation: %w", err)
			}
		}
		if err := os.Rename(oldPath, newPath); err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("rotate audit file: %w", err)
		}
	}
	directory, err := os.Open(l.dir)
	if err != nil {
		return fmt.Errorf("open audit directory: %w", err)
	}
	syncErr := directory.Sync()
	closeErr := directory.Close()
	if syncErr != nil || closeErr != nil {
		return fmt.Errorf("sync audit rotation: %w", errors.Join(syncErr, closeErr))
	}
	return nil
}

func clamp(value string, limit int) string {
	value = strings.TrimSpace(value)
	runes := []rune(value)
	if len(runes) <= limit {
		return value
	}
	return string(runes[:limit])
}
