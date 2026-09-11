package conversation

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"sync"
	"time"
)

const recentProjectionSchema = 1

var errRecentProjectionChanged = errors.New("conversation source changed during recent projection")

type recentProjectionKey struct {
	Schema         int
	Scope          string
	Provider       string
	SourcePath     string
	SourceRoot     string
	Revision       string
	RangeStart     int64
	RangeEnd       int64
	RangeDigest    string
	MaxRecordBytes int64
	OMO            bool
}

func (key recentProjectionKey) string() string {
	encoded, _ := json.Marshal(key)
	return string(encoded)
}

type recentProjection struct {
	Entries      []projectedEntry
	Diagnostics  BrowseDiagnostics
	Plan         *OMOTodoState
	accountedFor int64
}

type recentProjectionCacheEntry struct {
	value    recentProjection
	bytes    int64
	lastUsed time.Time
}

type recentProjectionCache struct {
	mu          sync.Mutex
	entries     map[string]recentProjectionCacheEntry
	totalBytes  int64
	maxEntries  int
	itemBytes   int64
	totalLimit  int64
	idleTTL     time.Duration
	hits        uint64
	misses      uint64
	projections uint64
}

func newRecentProjectionCache(options BrowserOptions) *recentProjectionCache {
	return &recentProjectionCache{
		entries:    make(map[string]recentProjectionCacheEntry),
		maxEntries: options.RecentCacheEntries,
		itemBytes:  options.RecentCacheItemBytes,
		totalLimit: options.RecentCacheBytes,
		idleTTL:    options.RecentCacheTTL,
	}
}

// get only holds the mutex for the map lookup and bookkeeping. The returned
// value is immutable and cloned by the caller after the lock is released.
func (cache *recentProjectionCache) get(key string, now time.Time) (recentProjection, bool) {
	if cache == nil {
		return recentProjection{}, false
	}
	cache.mu.Lock()
	defer cache.mu.Unlock()
	item, ok := cache.entries[key]
	if !ok {
		cache.misses++
		return recentProjection{}, false
	}
	if cache.idleTTL > 0 && now.Sub(item.lastUsed) >= cache.idleTTL {
		cache.deleteLocked(key)
		cache.misses++
		return recentProjection{}, false
	}
	cache.hits++
	item.lastUsed = now
	cache.entries[key] = item
	return cloneRecentProjection(item.value), true
}

func (cache *recentProjectionCache) put(key string, value recentProjection, now time.Time) bool {
	if cache == nil || key == "" {
		return false
	}
	copyValue := cloneRecentProjection(value)
	bytes := recentProjectionBytes(copyValue)
	if bytes < 1 || (cache.itemBytes > 0 && bytes > cache.itemBytes) || (cache.totalLimit > 0 && bytes > cache.totalLimit) {
		return false
	}
	copyValue.accountedFor = bytes
	cache.mu.Lock()
	defer cache.mu.Unlock()
	if previous, ok := cache.entries[key]; ok {
		cache.totalBytes -= previous.bytes
	}
	cache.entries[key] = recentProjectionCacheEntry{value: copyValue, bytes: bytes, lastUsed: now}
	cache.totalBytes += bytes
	cache.evictLocked(now)
	_, retained := cache.entries[key]
	return retained
}

func (cache *recentProjectionCache) recordProjection() {
	if cache == nil {
		return
	}
	cache.mu.Lock()
	cache.projections++
	cache.mu.Unlock()
}

func (cache *recentProjectionCache) stats() (hits, misses, projections uint64) {
	if cache == nil {
		return 0, 0, 0
	}
	cache.mu.Lock()
	defer cache.mu.Unlock()
	return cache.hits, cache.misses, cache.projections
}

func (cache *recentProjectionCache) clear() {
	if cache == nil {
		return
	}
	cache.mu.Lock()
	cache.entries = make(map[string]recentProjectionCacheEntry)
	cache.totalBytes = 0
	cache.mu.Unlock()
}

func (cache *recentProjectionCache) deleteLocked(key string) {
	if item, ok := cache.entries[key]; ok {
		cache.totalBytes -= item.bytes
		delete(cache.entries, key)
	}
}

func (cache *recentProjectionCache) evictLocked(now time.Time) {
	for key, item := range cache.entries {
		if cache.idleTTL > 0 && now.Sub(item.lastUsed) >= cache.idleTTL {
			cache.deleteLocked(key)
		}
	}
	for len(cache.entries) > cache.maxEntries || cache.totalLimit > 0 && cache.totalBytes > cache.totalLimit {
		oldestKey := ""
		var oldest time.Time
		for key, item := range cache.entries {
			if oldestKey == "" || item.lastUsed.Before(oldest) {
				oldestKey, oldest = key, item.lastUsed
			}
		}
		if oldestKey == "" {
			return
		}
		cache.deleteLocked(oldestKey)
	}
}

func cloneRecentProjection(value recentProjection) recentProjection {
	copyValue := recentProjection{
		Entries:      make([]projectedEntry, len(value.Entries)),
		Diagnostics:  value.Diagnostics,
		Plan:         cloneTodo(value.Plan),
		accountedFor: value.accountedFor,
	}
	for index, entry := range value.Entries {
		copyValue.Entries[index] = projectedEntry{Offset: entry.Offset, Entry: cloneConversationEntry(entry.Entry)}
	}
	return copyValue
}

func cloneConversationEntry(entry Entry) Entry {
	copyEntry := entry
	if entry.Tools != nil {
		copyEntry.Tools = append([]ToolActivity(nil), entry.Tools...)
	}
	return copyEntry
}

func (b *Browser) recentProjectionKey(scope BrowseScope, source fileSource, start, end int64, digest string, omo bool) string {
	return (recentProjectionKey{
		Schema: recentProjectionSchema, Scope: browseScopeID(scope), Provider: normalizedAgent(scope.Provider),
		SourcePath: source.path, SourceRoot: source.location.Root, Revision: source.revision,
		RangeStart: start, RangeEnd: end, RangeDigest: digest, MaxRecordBytes: b.options.MaxRecordBytes, OMO: omo,
	}).string()
}

// projectRecentRange hashes through the same JSONL reader that supplies the
// records to the projector. The digest check therefore covers blank, corrupt,
// oversized, and range-boundary bytes as well as visible records.
func (b *Browser) projectRecentRange(
	ctx context.Context,
	scope BrowseScope,
	source fileSource,
	start, end int64,
	digest string,
	omo bool,
) (recentProjection, error) {
	key := b.recentProjectionKey(scope, source, start, end, digest, omo)
	if cached, ok := b.recentCache.get(key, time.Now()); ok {
		return cloneRecentProjection(cached), nil
	}
	reader, err := NewJSONLRecordReader(ctx, source.file, start, end, b.options.MaxRecordBytes, nil)
	if err != nil {
		return recentProjection{}, err
	}
	rangeHash := sha256.New()
	reader.SetDigestWriter(rangeHash)
	b.recentCache.recordProjection()
	projector := newMemoryProjector(scope.Provider, source.revision)
	projection := recentProjection{}
	for {
		record, nextErr := reader.Next()
		if errors.Is(nextErr, io.EOF) {
			break
		}
		if nextErr != nil {
			return recentProjection{}, nextErr
		}
		if record.Oversized {
			projection.Diagnostics.OversizedRecords++
			continue
		}
		result := projector.apply(record)
		projection.Diagnostics.CorruptRecords += result.Diagnostics.CorruptRecords
		projection.Diagnostics.OmittedTools += result.Diagnostics.OmittedTools
		projection.Diagnostics.OmittedPayloads += result.Diagnostics.OmittedPayloads
		projection.Diagnostics.PlanCorrupt = projection.Diagnostics.PlanCorrupt || result.Diagnostics.PlanCorrupt
	}
	if hex.EncodeToString(rangeHash.Sum(nil)) != digest {
		return recentProjection{}, errRecentProjectionChanged
	}
	projection.Entries = append([]projectedEntry(nil), projector.entries...)
	if omo && projector.todoSeen && !projector.todoValid {
		projection.Diagnostics.CorruptRecords++
		projection.Diagnostics.PlanCorrupt = true
	}
	if omo && projector.plan != nil {
		projection.Plan = cloneTodo(projector.plan)
		projection.Plan.SessionID = scope.SessionID
		projection.Plan.Available = true
	}
	b.recentCache.put(key, projection, time.Now())
	return cloneRecentProjection(projection), nil
}

func recentProjectionBytes(value recentProjection) int64 {
	var total int64 = 128
	for _, entry := range value.Entries {
		total += int64(96 + len(entry.Entry.ID)*2 + len(entry.Entry.Timestamp)*2 + len(entry.Entry.Role)*2 + len(entry.Entry.Text)*2)
		for _, tool := range entry.Entry.Tools {
			total += int64(64 + len(tool.ID)*2 + len(tool.Name)*2 + len(tool.Input)*2 + len(tool.Output)*2)
		}
	}
	if value.Plan != nil {
		if encoded, err := json.Marshal(value.Plan); err == nil {
			total += int64(len(encoded) * 2)
		}
	}
	return total
}
