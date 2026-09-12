package conversation

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	bolt "go.etcd.io/bbolt"
)

var (
	metaBucket         = []byte("meta")
	entriesBucket      = []byte("entries")
	pendingBucket      = []byte("pending_tools")
	metaValueKey       = []byte("snapshot")
	errSnapshotCorrupt = errors.New("conversation snapshot is corrupt")
)

type snapshotMetadata struct {
	Schema           int           `json:"schema"`
	State            string        `json:"state"`
	SnapshotID       string        `json:"snapshot_id"`
	SourceRevision   string        `json:"source_revision"`
	SourceDigest     string        `json:"source_digest"`
	EndOffset        int64         `json:"end_offset"`
	Boundary         int64         `json:"boundary"`
	VisibleEntries   int           `json:"visible_entries"`
	OversizedRecords int           `json:"oversized_records"`
	CorruptRecords   int           `json:"corrupt_records"`
	OmittedTools     int           `json:"omitted_tools,omitempty"`
	OmittedPayloads  int           `json:"omitted_payloads,omitempty"`
	PlanCorrupt      bool          `json:"plan_corrupt,omitempty"`
	CreatedAt        time.Time     `json:"created_at"`
	UpdatedAt        time.Time     `json:"updated_at"`
	Plan             *OMOTodoState `json:"omo_plan,omitempty"`
}

type indexedEntry struct {
	Offset int64  `json:"offset"`
	End    int64  `json:"end"`
	Digest string `json:"digest"`
	Entry  Entry  `json:"entry"`
}

type pendingIndexedTool struct {
	ID         string `json:"id"`
	EntryStart int64  `json:"entry_start"`
	ToolIndex  int    `json:"tool_index"`
}

type snapshotIndex struct {
	mu   sync.RWMutex
	path string
	db   *bolt.DB
}

func openSnapshotIndex(root, snapshotID string, metadata snapshotMetadata) (*snapshotIndex, error) {
	if root == "" || snapshotID == "" || filepath.Base(snapshotID) != snapshotID {
		return nil, errors.New("invalid conversation snapshot id")
	}
	path := filepath.Join(root, "snapshot-"+snapshotID+".db")
	if info, err := os.Lstat(path); err == nil {
		if !info.Mode().IsRegular() {
			return nil, errors.New("conversation snapshot is not a regular file")
		}
		if err := os.Chmod(path, 0o600); err != nil {
			return nil, err
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	db, err := bolt.Open(path, 0o600, &bolt.Options{Timeout: time.Second})
	if err == nil {
		if chmodErr := os.Chmod(path, 0o600); chmodErr != nil {
			_ = db.Close()
			return nil, chmodErr
		}
	}
	if err != nil {
		return nil, err
	}
	index := &snapshotIndex{path: path, db: db}
	err = db.Update(func(tx *bolt.Tx) error {
		for _, bucket := range [][]byte{metaBucket, entriesBucket, pendingBucket} {
			if _, err := tx.CreateBucketIfNotExists(bucket); err != nil {
				return err
			}
		}
		return putSnapshotMetadata(tx, metadata)
	})
	if err != nil {
		_ = db.Close()
		_ = os.Remove(path)
		return nil, err
	}
	return index, nil
}

func (s *snapshotIndex) close() error {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.db == nil {
		return nil
	}
	err := s.db.Close()
	s.db = nil
	return err
}

func (s *snapshotIndex) remove() error {
	if s == nil {
		return nil
	}
	if err := s.close(); err != nil {
		return err
	}
	return os.Remove(s.path)
}

func (s *snapshotIndex) size() int64 {
	if s == nil || s.path == "" {
		return 0
	}
	info, err := os.Stat(s.path)
	if err != nil {
		return 0
	}
	return info.Size()
}

func (s *snapshotIndex) metadata() (snapshotMetadata, error) {
	if s == nil {
		return snapshotMetadata{}, errors.New("conversation snapshot is closed")
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.db == nil {
		return snapshotMetadata{}, errors.New("conversation snapshot is closed")
	}
	var metadata snapshotMetadata
	err := s.db.View(func(tx *bolt.Tx) error {
		bucket := tx.Bucket(metaBucket)
		if bucket == nil {
			return errSnapshotCorrupt
		}
		value := bucket.Get(metaValueKey)
		if len(value) == 0 || json.Unmarshal(value, &metadata) != nil {
			return errSnapshotCorrupt
		}
		return nil
	})
	return metadata, err
}

func (s *snapshotIndex) setMetadata(metadata snapshotMetadata) error {
	if s == nil {
		return errors.New("conversation snapshot is closed")
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.db == nil {
		return errors.New("conversation snapshot is closed")
	}
	return s.db.Update(func(tx *bolt.Tx) error { return putSnapshotMetadata(tx, metadata) })
}

func putSnapshotMetadata(tx *bolt.Tx, metadata snapshotMetadata) error {
	bucket := tx.Bucket(metaBucket)
	if bucket == nil {
		return errSnapshotCorrupt
	}
	value, err := json.Marshal(metadata)
	if err != nil {
		return err
	}
	return bucket.Put(metaValueKey, value)
}

func entryOffsetKey(offset int64) []byte {
	var key [8]byte
	binary.BigEndian.PutUint64(key[:], uint64(offset))
	return key[:]
}

func toolPendingKey(agent, id string) []byte {
	return []byte(toolAssociationKey(agent, id))
}

func recordDigest(raw []byte) string {
	digest := sha256.Sum256(raw)
	return hex.EncodeToString(digest[:])
}

type preparedIndexedRecord struct {
	result      projectionResult
	record      JSONLRecord
	entry       *projectedEntry
	toolResults []toolResult
	writes      bool
}

func (s *snapshotIndex) applyRecord(agent, revision string, record JSONLRecord) (projectionResult, error) {
	results, err := s.applyRecords(agent, revision, []JSONLRecord{record})
	if len(results) == 0 {
		return projectionResult{}, err
	}
	return results[0], err
}

func (s *snapshotIndex) applyRecords(agent, revision string, records []JSONLRecord) ([]projectionResult, error) {
	if s == nil {
		return nil, errors.New("conversation snapshot is closed")
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.db == nil {
		return nil, errors.New("conversation snapshot is closed")
	}
	prepared := make([]preparedIndexedRecord, len(records))
	results := make([]projectionResult, len(records))
	writes := false
	for index, record := range records {
		item := &prepared[index]
		item.record = record
		item.result = projectionResult{}
		if record.Oversized {
			item.result.Diagnostics.OversizedRecords = 1
			results[index] = item.result
			continue
		}
		if !record.Complete {
			results[index] = item.result
			continue
		}
		var raw map[string]any
		if err := json.Unmarshal(record.Raw, &raw); err != nil {
			item.result.Diagnostics.CorruptRecords = 1
			results[index] = item.result
			continue
		}
		item.result.Plan, item.result.TodoSeen, item.result.TodoInvalid = observeOMORecord(raw, agent)
		if item.result.TodoInvalid {
			item.result.Diagnostics.PlanCorrupt = true
		}

		calls, toolResults := parseToolActivity(normalizedAgent(agent), raw)
		toolEntry := Entry{Tools: calls}
		omittedTools, omittedPayloads := normalizeEntryTools(&toolEntry)
		calls = toolEntry.Tools
		item.result.Diagnostics.OmittedTools += omittedTools
		item.result.Diagnostics.OmittedPayloads += omittedPayloads
		item.toolResults = toolResults
		role, timestamp, body := visibleRecord(agent, raw)
		body = sanitizeText(body)
		if role == "" && len(calls) > 0 {
			role = "assistant"
		}
		visible := role != "" && (body != "" || len(calls) > 0)
		if visible {
			body, truncated := clampText(body, maxEntryBytes)
			value := projectedEntry{
				Offset: record.Start,
				Entry: Entry{
					ID: browserEntryID(revision, record.Start, record.Raw), Timestamp: timestamp,
					Role: role, Text: body, Tools: calls,
					Truncated: truncated || omittedTools > 0 || omittedPayloads > 0,
				},
			}
			item.entry = &value
		}
		item.writes = item.entry != nil || len(item.toolResults) > 0
		item.result.Entry = item.entry
		results[index] = item.result
		writes = writes || item.writes
	}
	if !writes {
		return results, nil
	}
	if err := s.db.Update(func(tx *bolt.Tx) error {
		entries := tx.Bucket(entriesBucket)
		pending := tx.Bucket(pendingBucket)
		if entries == nil || pending == nil {
			return errSnapshotCorrupt
		}
		for _, item := range prepared {
			for _, toolResult := range item.toolResults {
				key := toolPendingKey(agent, toolResult.id)
				value := pending.Get(key)
				if len(value) == 0 {
					continue
				}
				var pointer pendingIndexedTool
				if json.Unmarshal(value, &pointer) != nil || pointer.ID != toolResult.id {
					continue
				}
				entryValue := entries.Get(entryOffsetKey(pointer.EntryStart))
				if len(entryValue) == 0 {
					_ = pending.Delete(key)
					continue
				}
				var stored indexedEntry
				if json.Unmarshal(entryValue, &stored) != nil || pointer.ToolIndex < 0 || pointer.ToolIndex >= len(stored.Entry.Tools) {
					return errSnapshotCorrupt
				}
				tool := &stored.Entry.Tools[pointer.ToolIndex]
				output, truncated := clampText(sanitizeText(toolResult.output), maxEntryBytes)
				tool.Output = output
				tool.Error = toolResult.failed
				tool.Truncated = tool.Truncated || truncated
				encoded, err := json.Marshal(stored)
				if err != nil {
					return err
				}
				if err := entries.Put(entryOffsetKey(pointer.EntryStart), encoded); err != nil {
					return err
				}
				if err := pending.Delete(key); err != nil {
					return err
				}
			}
			if item.entry == nil {
				continue
			}
			stored := indexedEntry{
				Offset: item.entry.Offset, End: item.record.End, Digest: recordDigest(item.record.Raw), Entry: item.entry.Entry,
			}
			encoded, err := json.Marshal(stored)
			if err != nil {
				return err
			}
			if err := entries.Put(entryOffsetKey(item.entry.Offset), encoded); err != nil {
				return err
			}
			for toolIndex := range item.entry.Entry.Tools {
				id := toolAssociationID(item.entry.Entry.Tools[toolIndex])
				if id == "" {
					continue
				}
				pointer := pendingIndexedTool{ID: id, EntryStart: item.entry.Offset, ToolIndex: toolIndex}
				encoded, err := json.Marshal(pointer)
				if err != nil {
					return err
				}
				if err := pending.Put(toolPendingKey(agent, id), encoded); err != nil {
					return err
				}
			}
		}
		return nil
	}); err != nil {
		return nil, err
	}
	return results, nil
}

func (s *snapshotIndex) entriesBefore(boundary int64, limit int) ([]projectedEntry, bool, error) {
	if s == nil {
		return nil, false, errors.New("conversation snapshot is closed")
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.db == nil {
		return nil, false, errors.New("conversation snapshot is closed")
	}
	if limit < 1 {
		return []projectedEntry{}, false, nil
	}
	result := make([]projectedEntry, 0, limit)
	hasMore := false
	err := s.db.View(func(tx *bolt.Tx) error {
		bucket := tx.Bucket(entriesBucket)
		if bucket == nil {
			return errSnapshotCorrupt
		}
		cursor := bucket.Cursor()
		boundaryKey := entryOffsetKey(boundary)
		key, value := cursor.Seek(boundaryKey)
		if key == nil {
			key, value = cursor.Last()
		}
		if key != nil && bytes.Compare(key, boundaryKey) >= 0 {
			key, value = cursor.Prev()
		}
		for key != nil && len(result) < limit {
			offset := int64(binary.BigEndian.Uint64(key))
			var stored indexedEntry
			if offset >= boundary || json.Unmarshal(value, &stored) != nil || stored.Offset != offset {
				return errSnapshotCorrupt
			}
			result = append(result, projectedEntry{Offset: stored.Offset, Entry: stored.Entry})
			key, value = cursor.Prev()
		}
		return nil
	})
	if err != nil {
		return nil, false, err
	}
	for left, right := 0, len(result)-1; left < right; left, right = left+1, right-1 {
		result[left], result[right] = result[right], result[left]
	}
	checkBefore := boundary
	if len(result) > 0 {
		checkBefore = result[0].Offset
	}
	err = s.db.View(func(tx *bolt.Tx) error {
		bucket := tx.Bucket(entriesBucket)
		if bucket == nil {
			return errSnapshotCorrupt
		}
		cursor := bucket.Cursor()
		boundaryKey := entryOffsetKey(checkBefore)
		key, _ := cursor.Seek(boundaryKey)
		if key == nil {
			key, _ = cursor.Last()
		}
		if key != nil && bytes.Compare(key, boundaryKey) >= 0 {
			key, _ = cursor.Prev()
		}
		more := key != nil
		if len(result) == 0 {
			more = key != nil
		}
		hasMore = more
		return nil
	})
	if err != nil {
		return nil, false, err
	}
	return result, hasMore, nil
}

func (s *snapshotIndex) count() (int, error) {
	return s.countWithCheckpoint(nil)
}

func (s *snapshotIndex) countWithCheckpoint(checkpoint func() error) (int, error) {
	if s == nil {
		return 0, errors.New("conversation snapshot is closed")
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.db == nil {
		return 0, errors.New("conversation snapshot is closed")
	}
	count := 0
	err := s.db.View(func(tx *bolt.Tx) error {
		bucket := tx.Bucket(entriesBucket)
		if bucket == nil {
			return errSnapshotCorrupt
		}
		return bucket.ForEach(func(key, value []byte) error {
			if checkpoint != nil {
				if err := checkpoint(); err != nil {
					return err
				}
			}
			if len(key) != 8 || len(value) == 0 {
				return errSnapshotCorrupt
			}
			var entry indexedEntry
			if err := json.Unmarshal(value, &entry); err != nil || entry.Offset != int64(binary.BigEndian.Uint64(key)) {
				return errSnapshotCorrupt
			}
			count++
			return nil
		})
	})
	return count, err
}

func (s *snapshotIndex) sync() error {
	if s == nil {
		return errors.New("conversation snapshot is closed")
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.db == nil {
		return errors.New("conversation snapshot is closed")
	}
	return s.db.Sync()
}

func (s *snapshotIndex) String() string {
	if s == nil {
		return ""
	}
	return fmt.Sprintf("%s", filepath.Base(s.path))
}
