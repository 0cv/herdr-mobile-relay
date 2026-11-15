package activity

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/0cv/herdr-mobile-relay/internal/atomicfile"
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

var entryIDSequence atomic.Uint64

type Journal struct {
	mu      sync.RWMutex
	entries []Entry
	path    string
	dir     string
}

func OpenJournal(cacheDir string) (*Journal, error) {
	if err := os.MkdirAll(cacheDir, 0o700); err != nil {
		return nil, fmt.Errorf("create activity directory: %w", err)
	}
	if err := os.Chmod(cacheDir, 0o700); err != nil {
		return nil, fmt.Errorf("protect activity directory: %w", err)
	}
	journal := &Journal{
		path: filepath.Join(cacheDir, "activity.jsonl"),
		dir:  cacheDir,
	}
	if err := journal.load(); err != nil {
		return nil, err
	}
	return journal, nil
}

func (j *Journal) load() error {
	file, err := os.Open(j.path)
	if errors.Is(err, os.ErrNotExist) {
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
	needsCompact := info.Size() > maxBytes
	for scanner.Scan() {
		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}
		var entry Entry
		if err := json.Unmarshal(line, &entry); err != nil {
			continue
		}
		normalized := NormalizeEntry(entry)
		if normalized.Extract != entry.Extract {
			needsCompact = true
		}
		entries = append(entries, normalized)
	}
	if err := scanner.Err(); err != nil {
		return fmt.Errorf("scan activity journal: %w", err)
	}
	retained, retainedBytes, err := retainWithinLimits(entries)
	if err != nil {
		return fmt.Errorf("normalize activity journal: %w", err)
	}
	if len(retained) != len(entries) || retainedBytes != info.Size() {
		needsCompact = true
	}
	j.entries = retained
	if needsCompact {
		if err := j.compactLocked(); err != nil {
			return fmt.Errorf("compact activity journal: %w", err)
		}
	}
	return nil
}

func (j *Journal) Append(entry Entry) error {
	j.mu.Lock()
	defer j.mu.Unlock()
	entry = NormalizeEntry(entry)
	data, err := encodeEntry(entry)
	if err != nil {
		return err
	}
	if len(data) > maxBytes {
		return fmt.Errorf("activity entry exceeds %d byte journal limit", maxBytes)
	}
	candidate := append(append([]Entry(nil), j.entries...), entry)
	retained, retainedBytes, err := retainWithinLimits(candidate)
	if err != nil {
		return err
	}
	if len(retained) != len(candidate) || retainedBytes > maxBytes || len(candidate) > maxItems {
		return j.compactEntriesLocked(retained)
	}
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
	return nil
}

func NormalizeEntry(entry Entry) Entry {
	runes := []rune(entry.Extract)
	if len(runes) > maxExtractChars {
		entry.Extract = string(runes[:maxExtractChars])
	}
	return entry
}

func encodeEntry(entry Entry) ([]byte, error) {
	data, err := json.Marshal(entry)
	if err != nil {
		return nil, err
	}
	return append(data, '\n'), nil
}

func retainWithinLimits(entries []Entry) ([]Entry, int64, error) {
	if len(entries) > maxItems {
		entries = entries[len(entries)-maxItems:]
	}
	sizes := make([]int, len(entries))
	var total int64
	for i, entry := range entries {
		data, err := encodeEntry(entry)
		if err != nil {
			return nil, 0, err
		}
		sizes[i] = len(data)
		total += int64(len(data))
	}
	first := 0
	for total > maxBytes && first < len(entries)-1 {
		total -= int64(sizes[first])
		first++
	}
	if total > maxBytes {
		return nil, 0, fmt.Errorf("activity entry exceeds %d byte journal limit", maxBytes)
	}
	return append([]Entry(nil), entries[first:]...), total, nil
}

func (j *Journal) Clear() error {
	j.mu.Lock()
	defer j.mu.Unlock()
	if err := j.compactEntriesLocked(nil); err != nil {
		return fmt.Errorf("clear activity journal: %w", err)
	}
	return nil
}

// Discard durably removes a committed entry that became stale before it could
// be published.
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
	if err := j.compactEntriesLocked(retained); err != nil {
		return fmt.Errorf("discard stale activity: %w", err)
	}
	return nil
}

func (j *Journal) compactLocked() error {
	return j.compactEntriesLocked(j.entries)
}

func (j *Journal) compactEntriesLocked(entries []Entry) error {
	var data []byte
	for _, entry := range entries {
		line, err := encodeEntry(entry)
		if err != nil {
			return err
		}
		data = append(data, line...)
	}
	outcome, err := atomicfile.Replace(j.dir, j.path, ".activity.jsonl.*.tmp", data, 0o600)
	if err != nil && !(outcome == atomicfile.Renamed && unsupportedDirectorySync(err)) {
		return err
	}
	j.entries = append([]Entry(nil), entries...)
	return nil
}

func unsupportedDirectorySync(err error) bool {
	return errors.Is(err, syscall.EINVAL) || errors.Is(err, syscall.ENOTSUP) || errors.Is(err, syscall.ENOSYS)
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
		ID:        fmt.Sprintf("%d-%d-%s", now.UnixNano(), entryIDSequence.Add(1), paneID),
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
