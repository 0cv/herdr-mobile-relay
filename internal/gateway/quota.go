package gateway

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/0cv/herdr-mobile-relay/internal/gatewaywire"
)

// stateVersion is the on-disk counter format. A file from a newer gateway is an
// error rather than silently discarded state, because discarding counters would
// silently reset every quota.
const stateVersion = 1

// counter is one relay's relayed-byte usage for one calendar month (UTC).
type counter struct {
	Month        string `json:"month"`
	RelayedBytes uint64 `json:"relayed_bytes"`
	WarnSent     bool   `json:"warn_sent"`
	ExceededSent bool   `json:"exceeded_sent"`
}

// persistedState is the entire contents of the optional state file. Counters and
// nothing else: no relay keys, no client addresses, no traffic.
type persistedState struct {
	Version int                 `json:"version"`
	Relays  map[string]*counter `json:"relays"`
}

// quotaStore tracks relayed bytes per relay per month and decides when a relay
// should be warned or cut off from new connections.
type quotaStore struct {
	path        string
	limit       int64
	warnPercent int
	warnBytes   uint64
	logger      *slog.Logger

	mu     sync.Mutex
	relays map[string]*counter
	dirty  bool
}

func newQuotaStore(path string, limit int64, warnPercent int, logger *slog.Logger) *quotaStore {
	q := &quotaStore{
		path:        path,
		limit:       limit,
		warnPercent: warnPercent,
		logger:      logger,
		relays:      make(map[string]*counter),
	}
	if limit > 0 && warnPercent > 0 {
		q.warnBytes = scalePercent(uint64(limit), uint64(warnPercent))
	}
	return q
}

// scalePercent computes floor(value*percent/100) without overflowing.
func scalePercent(value, percent uint64) uint64 {
	return value/100*percent + (value%100)*percent/100
}

// enabled reports whether quotas are enforced at all.
func (q *quotaStore) enabled() bool { return q.limit >= 0 }

// add charges relayed bytes against a relay and returns the advisory notices the
// relay should receive now. Each notice is sent at most once per relay per month.
func (q *quotaStore) add(relayID string, n uint64, now time.Time) []gatewaywire.NoticePayload {
	if !q.enabled() || n == 0 {
		return nil
	}
	q.mu.Lock()
	defer q.mu.Unlock()

	entry := q.counterLocked(relayID, now)
	entry.RelayedBytes += n
	q.dirty = true

	var notices []gatewaywire.NoticePayload
	if !entry.WarnSent && q.warnBytes > 0 && entry.RelayedBytes >= q.warnBytes {
		entry.WarnSent = true
		notices = append(notices, q.noticeLocked(entry, gatewaywire.NoticeQuotaWarning))
	}
	if !entry.ExceededSent && entry.RelayedBytes >= uint64(q.limit) {
		entry.ExceededSent = true
		notices = append(notices, q.noticeLocked(entry, gatewaywire.NoticeQuotaExceeded))
	}
	return notices
}

// exceeded reports whether a relay has consumed its monthly quota, and returns
// the exceeded notice if the relay has not been told yet. Connections already
// established are never severed; only new ones are refused.
func (q *quotaStore) exceeded(relayID string, now time.Time) (bool, []gatewaywire.NoticePayload) {
	if !q.enabled() {
		return false, nil
	}
	q.mu.Lock()
	defer q.mu.Unlock()

	entry := q.counterLocked(relayID, now)
	if entry.RelayedBytes < uint64(q.limit) {
		return false, nil
	}
	if entry.ExceededSent {
		return true, nil
	}
	entry.ExceededSent = true
	q.dirty = true
	return true, []gatewaywire.NoticePayload{q.noticeLocked(entry, gatewaywire.NoticeQuotaExceeded)}
}

// counterLocked returns the relay's counter for the current month, resetting it
// on a UTC month boundary.
func (q *quotaStore) counterLocked(relayID string, now time.Time) *counter {
	month := monthKey(now)
	entry := q.relays[relayID]
	if entry == nil {
		entry = &counter{Month: month}
		q.relays[relayID] = entry
		return entry
	}
	if entry.Month != month {
		*entry = counter{Month: month}
	}
	return entry
}

func (q *quotaStore) noticeLocked(entry *counter, kind string) gatewaywire.NoticePayload {
	notice := gatewaywire.NoticePayload{
		Kind:         kind,
		RelayedBytes: entry.RelayedBytes,
		QuotaBytes:   uint64(q.limit),
	}
	if kind == gatewaywire.NoticeQuotaWarning {
		notice.Message = "relayed traffic reached this gateway's monthly warning threshold; a direct connection or a self-hosted gateway avoids the limit"
		return notice
	}
	notice.Message = "monthly relayed-byte quota is exhausted; new gateway connections are refused until the next calendar month"
	return notice
}

// prune drops counters from previous months so the table cannot grow without
// bound on a long-lived gateway.
func (q *quotaStore) prune(now time.Time) {
	month := monthKey(now)
	q.mu.Lock()
	defer q.mu.Unlock()
	for relayID, entry := range q.relays {
		if entry.Month != month {
			delete(q.relays, relayID)
			q.dirty = true
		}
	}
}

// load reads the counter file if one is configured. A missing file is a fresh
// gateway, not an error.
func (q *quotaStore) load(now time.Time) error {
	if q.path == "" {
		return nil
	}
	data, err := os.ReadFile(q.path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("gateway: read state %s: %w", q.path, err)
	}
	var state persistedState
	if err := json.Unmarshal(data, &state); err != nil {
		return fmt.Errorf("gateway: parse state %s: %w", q.path, err)
	}
	if state.Version != stateVersion {
		return fmt.Errorf("gateway: state %s has unsupported version %d", q.path, state.Version)
	}

	month := monthKey(now)
	q.mu.Lock()
	defer q.mu.Unlock()
	for relayID, entry := range state.Relays {
		if entry == nil || entry.Month != month {
			continue
		}
		q.relays[relayID] = entry
	}
	q.logger.Info("gateway state loaded", "relays", len(q.relays))
	return nil
}

// save rewrites the counter file atomically. Nothing but counters is persisted.
func (q *quotaStore) save(now time.Time) error {
	if q.path == "" {
		return nil
	}
	q.mu.Lock()
	if !q.dirty {
		q.mu.Unlock()
		return nil
	}
	state := persistedState{Version: stateVersion, Relays: make(map[string]*counter, len(q.relays))}
	month := monthKey(now)
	for relayID, entry := range q.relays {
		if entry.Month != month {
			continue
		}
		snapshot := *entry
		state.Relays[relayID] = &snapshot
	}
	q.dirty = false
	q.mu.Unlock()

	data, err := json.Marshal(state)
	if err != nil {
		return fmt.Errorf("gateway: encode state: %w", err)
	}
	return writeFileAtomic(q.path, data)
}

// writeFileAtomic replaces path with data via a same-directory temp file so a
// crash mid-write cannot truncate the counters.
func writeFileAtomic(path string, data []byte) error {
	dir := filepath.Dir(path)
	temp, err := os.CreateTemp(dir, ".herdr-gateway-state-*")
	if err != nil {
		return fmt.Errorf("gateway: create temp state in %s: %w", dir, err)
	}
	name := temp.Name()
	if err := temp.Chmod(0o600); err != nil {
		temp.Close()
		os.Remove(name)
		return fmt.Errorf("gateway: chmod temp state: %w", err)
	}
	if _, err := temp.Write(data); err != nil {
		temp.Close()
		os.Remove(name)
		return fmt.Errorf("gateway: write temp state: %w", err)
	}
	if err := temp.Sync(); err != nil {
		temp.Close()
		os.Remove(name)
		return fmt.Errorf("gateway: sync temp state: %w", err)
	}
	if err := temp.Close(); err != nil {
		os.Remove(name)
		return fmt.Errorf("gateway: close temp state: %w", err)
	}
	if err := os.Rename(name, path); err != nil {
		os.Remove(name)
		return fmt.Errorf("gateway: replace state %s: %w", path, err)
	}
	return nil
}

func monthKey(now time.Time) string { return now.UTC().Format("2006-01") }
