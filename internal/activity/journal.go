package activity

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

const (
	maxItems        = 500
	maxBytes        = 2 * 1024 * 1024
	maxExtractChars = 100000
)

type MilliTimestamp int64

func (t MilliTimestamp) MarshalJSON() ([]byte, error) {
	return []byte(strconv.FormatInt(int64(t), 10)), nil
}

func (t *MilliTimestamp) UnmarshalJSON(data []byte) error {
	if len(data) > 0 && data[0] == '"' {
		var s string
		if err := json.Unmarshal(data, &s); err != nil {
			return err
		}
		parsed, err := time.Parse(time.RFC3339, s)
		if err != nil {
			return err
		}
		*t = MilliTimestamp(parsed.UnixMilli())
		return nil
	}
	var n int64
	if err := json.Unmarshal(data, &n); err != nil {
		return err
	}
	*t = MilliTimestamp(n)
	return nil
}

type Entry struct {
	ID        string         `json:"id"`
	Timestamp MilliTimestamp `json:"timestamp"`
	Kind      string         `json:"kind"`
	Status    string         `json:"status"`
	Summary   string         `json:"summary"`
	Host      string         `json:"host,omitempty"`
	PaneID    string         `json:"pane_id,omitempty"`
	Agent     string         `json:"agent,omitempty"`
	Project   string         `json:"project,omitempty"`
	RequestID string         `json:"request_id,omitempty"`
	Extract   string         `json:"extract,omitempty"`
	Session   string         `json:"session,omitempty"`
	Details   map[string]any `json:"details,omitempty"`
}

type Journal struct {
	mu            sync.RWMutex
	entries       []Entry
	path          string
	tombstonePath string
	dir           string
	bytes         int64
}

func OpenJournal(cacheDir string) (*Journal, error) {
	if err := os.MkdirAll(cacheDir, 0o700); err != nil {
		return nil, fmt.Errorf("create activity directory: %w", err)
	}
	if err := os.Chmod(cacheDir, 0o700); err != nil {
		return nil, fmt.Errorf("protect activity directory: %w", err)
	}
	journal := &Journal{
		path:          filepath.Join(cacheDir, "activity.jsonl"),
		tombstonePath: filepath.Join(cacheDir, "activity.tombstones"),
		dir:           cacheDir,
	}
	if err := journal.load(); err != nil {
		return nil, err
	}
	return journal, nil
}

func (j *Journal) load() error {
	tombstones, err := j.loadTombstones()
	if err != nil {
		return err
	}
	file, err := os.Open(j.path)
	if errors.Is(err, os.ErrNotExist) {
		if len(tombstones) > 0 {
			if err := j.writeTombstonesLocked(nil); err != nil {
				return fmt.Errorf("clean orphaned activity tombstones: %w", err)
			}
		}
		return nil
	}
	if err != nil {
		return fmt.Errorf("read activity journal: %w", err)
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return err
	}
	if info.Size() > maxBytes*4 {
		return fmt.Errorf("activity journal is corrupt or oversized: %d bytes", info.Size())
	}
	scanner := bufio.NewScanner(io.LimitReader(file, maxBytes*4+1))
	scanner.Buffer(make([]byte, 64*1024), maxBytes)
	var entries []Entry
	for scanner.Scan() {
		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}
		var entry Entry
		if err := json.Unmarshal(line, &entry); err != nil {
			continue
		}
		if _, discarded := tombstones[entry.ID]; !discarded {
			entries = append(entries, entry)
		}
	}
	if err := scanner.Err(); err != nil {
		return fmt.Errorf("scan activity journal: %w", err)
	}
	if len(entries) > maxItems {
		entries = entries[len(entries)-maxItems:]
	}
	j.entries = entries
	j.bytes = info.Size()
	if len(tombstones) > 0 {
		if err := j.compactLocked(); err != nil {
			return fmt.Errorf("recover tombstoned activity: %w", err)
		}
		if err := j.writeTombstonesLocked(nil); err != nil {
			return fmt.Errorf("clean recovered activity tombstones: %w", err)
		}
	}
	return nil
}

func (j *Journal) Append(entry Entry) error {
	j.mu.Lock()
	defer j.mu.Unlock()
	if len([]rune(entry.Extract)) > maxExtractChars {
		entry.Extract = string([]rune(entry.Extract)[:maxExtractChars])
	}
	data, err := json.Marshal(entry)
	if err != nil {
		return err
	}
	data = append(data, '\n')
	file, err := os.OpenFile(j.path, os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o600)
	if err != nil {
		return fmt.Errorf("open activity journal: %w", err)
	}
	if err := file.Chmod(0o600); err != nil {
		_ = file.Close()
		return fmt.Errorf("protect activity journal: %w", err)
	}
	written, writeErr := file.Write(data)
	if writeErr == nil && written != len(data) {
		writeErr = io.ErrShortWrite
	}
	if writeErr == nil {
		writeErr = file.Sync()
	}
	closeErr := file.Close()
	if writeErr != nil {
		return fmt.Errorf("append activity journal: %w", writeErr)
	}
	if closeErr != nil {
		return closeErr
	}
	j.entries = append(j.entries, entry)
	j.bytes += int64(len(data))
	if len(j.entries) > maxItems || j.bytes > maxBytes {
		if len(j.entries) > maxItems {
			j.entries = j.entries[len(j.entries)-maxItems:]
		}
		return j.compactLocked()
	}
	return nil
}

func (j *Journal) Clear() error {
	j.mu.Lock()
	defer j.mu.Unlock()

	previousTombstones, err := j.loadTombstones()
	if err != nil {
		return err
	}
	clearTombstones := cloneSet(previousTombstones)
	for _, entry := range j.entries {
		if entry.ID != "" {
			clearTombstones[entry.ID] = struct{}{}
		}
	}
	if err := j.writeTombstonesLocked(clearTombstones); err != nil {
		return fmt.Errorf("prepare activity clear: %w", err)
	}
	if err := j.compactEntriesLocked(nil); err != nil {
		restoreErr := j.writeTombstonesLocked(previousTombstones)
		if restoreErr != nil {
			return errors.Join(fmt.Errorf("clear activity journal: %w", err), fmt.Errorf("restore activity tombstones: %w", restoreErr))
		}
		return fmt.Errorf("clear activity journal: %w", err)
	}
	// The journal is durably empty. A leftover tombstone file is harmless and
	// will be cleaned during the next OpenJournal recovery.
	_ = j.writeTombstonesLocked(previousTombstones)
	return nil
}

// Discard durably removes a committed entry that became stale before it could
// be published. The write-ahead tombstone closes the crash window between
// deciding to discard and rewriting the JSONL journal.
func (j *Journal) Discard(id string) error {
	if id == "" {
		return nil
	}
	j.mu.Lock()
	defer j.mu.Unlock()

	retained := make([]Entry, 0, len(j.entries))
	found := false
	for _, entry := range j.entries {
		if entry.ID == id {
			found = true
			continue
		}
		retained = append(retained, entry)
	}
	if !found {
		return nil
	}
	previousTombstones, err := j.loadTombstones()
	if err != nil {
		return err
	}
	discardTombstones := cloneSet(previousTombstones)
	discardTombstones[id] = struct{}{}
	if err := j.writeTombstonesLocked(discardTombstones); err != nil {
		return fmt.Errorf("prepare stale activity discard: %w", err)
	}
	if err := j.compactEntriesLocked(retained); err != nil {
		restoreErr := j.writeTombstonesLocked(previousTombstones)
		if restoreErr != nil {
			return errors.Join(fmt.Errorf("discard stale activity: %w", err), fmt.Errorf("restore activity tombstones: %w", restoreErr))
		}
		return fmt.Errorf("discard stale activity: %w", err)
	}
	_ = j.writeTombstonesLocked(previousTombstones)
	return nil
}

func (j *Journal) compactLocked() error {
	return j.compactEntriesLocked(j.entries)
}

func (j *Journal) compactEntriesLocked(entries []Entry) error {
	temp, err := os.CreateTemp(j.dir, ".activity.jsonl.*.tmp")
	if err != nil {
		return err
	}
	tempPath := temp.Name()
	defer os.Remove(tempPath)
	if err := temp.Chmod(0o600); err != nil {
		_ = temp.Close()
		return err
	}
	var bytesWritten int64
	for _, entry := range entries {
		data, err := json.Marshal(entry)
		if err != nil {
			_ = temp.Close()
			return err
		}
		data = append(data, '\n')
		written, err := temp.Write(data)
		if err != nil {
			_ = temp.Close()
			return err
		}
		if written != len(data) {
			_ = temp.Close()
			return io.ErrShortWrite
		}
		bytesWritten += int64(written)
	}
	if err := temp.Sync(); err != nil {
		_ = temp.Close()
		return err
	}
	if err := temp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tempPath, j.path); err != nil {
		return err
	}
	if err := syncDirectory(j.dir); err != nil {
		return err
	}
	j.entries = append([]Entry(nil), entries...)
	j.bytes = bytesWritten
	return nil
}

func (j *Journal) loadTombstones() (map[string]struct{}, error) {
	tombstones := make(map[string]struct{})
	file, err := os.Open(j.tombstonePath)
	if errors.Is(err, os.ErrNotExist) {
		return tombstones, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read activity tombstones: %w", err)
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return nil, err
	}
	if info.Size() > maxBytes {
		return nil, fmt.Errorf("activity tombstones are corrupt or oversized: %d bytes", info.Size())
	}
	if err := file.Chmod(0o600); err != nil {
		return nil, fmt.Errorf("protect activity tombstones: %w", err)
	}
	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 1024), maxBytes)
	for scanner.Scan() {
		if id := strings.TrimSpace(scanner.Text()); id != "" {
			tombstones[id] = struct{}{}
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("scan activity tombstones: %w", err)
	}
	return tombstones, nil
}

func (j *Journal) writeTombstonesLocked(tombstones map[string]struct{}) error {
	if len(tombstones) == 0 {
		err := os.Remove(j.tombstonePath)
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		return syncDirectory(j.dir)
	}

	ids := make([]string, 0, len(tombstones))
	for id := range tombstones {
		if strings.TrimSpace(id) != "" {
			ids = append(ids, id)
		}
	}
	sort.Strings(ids)
	temp, err := os.CreateTemp(j.dir, ".activity.tombstones.*.tmp")
	if err != nil {
		return err
	}
	tempPath := temp.Name()
	defer os.Remove(tempPath)
	if err := temp.Chmod(0o600); err != nil {
		_ = temp.Close()
		return err
	}
	for _, id := range ids {
		line := id + "\n"
		written, err := io.WriteString(temp, line)
		if err != nil {
			_ = temp.Close()
			return err
		}
		if written != len(line) {
			_ = temp.Close()
			return io.ErrShortWrite
		}
	}
	if err := temp.Sync(); err != nil {
		_ = temp.Close()
		return err
	}
	if err := temp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tempPath, j.tombstonePath); err != nil {
		return err
	}
	return syncDirectory(j.dir)
}

func cloneSet(values map[string]struct{}) map[string]struct{} {
	result := make(map[string]struct{}, len(values))
	for value := range values {
		result[value] = struct{}{}
	}
	return result
}

func syncDirectory(path string) error {
	directory, err := os.Open(path)
	if err != nil {
		return err
	}
	defer directory.Close()
	err = directory.Sync()
	if errors.Is(err, syscall.EINVAL) || errors.Is(err, syscall.ENOTSUP) || errors.Is(err, syscall.ENOSYS) {
		return nil
	}
	return err
}

func (j *Journal) Recent(limit int) []Entry {
	j.mu.RLock()
	defer j.mu.RUnlock()
	if limit <= 0 || limit > len(j.entries) {
		limit = len(j.entries)
	}
	start := len(j.entries) - limit
	result := make([]Entry, limit)
	copy(result, j.entries[start:])
	return result
}

func NewEntry(kind, status, summary, paneID, agent, project, requestID string) Entry {
	now := time.Now().UTC()
	return Entry{
		ID:        fmt.Sprintf("%d-%s", now.UnixNano(), paneID),
		Timestamp: MilliTimestamp(now.UnixMilli()),
		Kind:      kind,
		Status:    status,
		Summary:   summary,
		PaneID:    paneID,
		Agent:     agent,
		Project:   project,
		RequestID: requestID,
	}
}
