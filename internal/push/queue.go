package push

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"math"
	"net"
	"net/http"
	"os"
	"sort"
	"sync"
	"time"
)

const (
	maxDeliveryAttempts = 6
	maxQueueEntries     = 1024
	maxQueueBytes       = 4 * 1024 * 1024
	maxPushPayloadBytes = 3993
)

type PushEvent struct {
	Key        PushEventKey `json:"key"`
	Payload    []byte       `json:"payload"`
	CreatedAt  time.Time    `json:"created_at"`
	ExpiresAt  time.Time    `json:"expires_at"`
	Retract    bool         `json:"retract,omitempty"`
	WaitingSeq uint64       `json:"waiting_seq,omitempty"`
}

func (e PushEvent) Validate(now time.Time) error {
	if err := e.Key.Validate(); err != nil {
		return err
	}
	if len(e.Payload) == 0 || len(e.Payload) > maxPushPayloadBytes ||
		e.CreatedAt.IsZero() || e.ExpiresAt.IsZero() || !e.ExpiresAt.After(e.CreatedAt) {
		return errors.New("push_invalid_event")
	}
	if !now.Before(e.ExpiresAt) {
		return errors.New("push_event_expired")
	}
	return nil
}

type queueEntry struct {
	ID           string       `json:"id"`
	Event        PushEvent    `json:"event"`
	Subscription Subscription `json:"subscription"`
	DueAt        time.Time    `json:"due_at"`
	Attempts     int          `json:"attempts"`
}

type deliveredRecord struct {
	Key          PushEventKey `json:"key"`
	Subscription Subscription `json:"subscription"`
	Tag          string       `json:"tag"`
	AcceptedAt   time.Time    `json:"accepted_at"`
	WaitingSeq   uint64       `json:"waiting_seq,omitempty"`
}

type queueFile struct {
	WaitingSeq uint64                     `json:"waiting_seq,omitempty"`
	Entries    map[string]queueEntry      `json:"entries"`
	Delivered  map[string]deliveredRecord `json:"delivered"`
}

func (f queueFile) clone() queueFile {
	return queueFile{
		WaitingSeq: f.WaitingSeq,
		Entries:    maps.Clone(f.Entries),
		Delivered:  maps.Clone(f.Delivered),
	}
}

type durableQueue struct {
	mu      sync.Mutex
	process sync.Mutex
	path    string
	state   queueFile
}

func newDurableQueue(path string) (*durableQueue, error) {
	q := &durableQueue{
		path: path,
		state: queueFile{
			Entries:   make(map[string]queueEntry),
			Delivered: make(map[string]deliveredRecord),
		},
	}
	data, err := os.ReadFile(path)
	if err == nil {
		if err := json.Unmarshal(data, &q.state); err != nil {
			return nil, fmt.Errorf("decode push queue: %w", err)
		}
	} else if !os.IsNotExist(err) {
		return nil, fmt.Errorf("read push queue: %w", err)
	}
	if q.state.Entries == nil {
		q.state.Entries = make(map[string]queueEntry)
	}
	if q.state.Delivered == nil {
		q.state.Delivered = make(map[string]deliveredRecord)
	}
	return q, nil
}

func (q *durableQueue) enqueue(event PushEvent, subscription Subscription, dueAt time.Time) (string, error) {
	if subscription.DeviceID == "" || subscription.DeviceID != event.Key.DeviceID {
		return "", errors.New("push_subscription_device_mismatch")
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	next := q.state.clone()
	id := next.enqueue(event, subscription, dueAt)
	if err := q.commitLocked(next); err != nil {
		return "", err
	}
	return id, nil
}

func (f *queueFile) nextWaitingSeq() (uint64, error) {
	if f.WaitingSeq == math.MaxUint64 {
		return 0, errors.New("push_waiting_sequence_exhausted")
	}
	f.WaitingSeq++
	return f.WaitingSeq, nil
}

func (f *queueFile) enqueue(event PushEvent, subscription Subscription, dueAt time.Time) string {
	id := deliveryID(event.Key, subscription.Endpoint)
	f.Entries[id] = queueEntry{ID: id, Event: event, Subscription: subscription, DueAt: dueAt}
	return id
}

func (f *queueFile) cancelAlerts(key PushEventKey) {
	for id, entry := range f.Entries {
		if entry.Event.Key == key && !entry.Event.Retract {
			delete(f.Entries, id)
		}
	}
}

func (q *durableQueue) waitingRetractDeadline(entry queueEntry) time.Time {
	q.mu.Lock()
	defer q.mu.Unlock()
	key := entry.Event.Key
	var deadline time.Time
	for _, pending := range q.state.Entries {
		other := pending.Event.Key
		if pending.Event.Retract && other.Category.Waiting() &&
			pending.Subscription.Endpoint == entry.Subscription.Endpoint &&
			other.DeviceID == key.DeviceID && other.ServerSessionID == key.ServerSessionID && other.PaneID == key.PaneID &&
			pending.Event.ExpiresAt.After(deadline) {
			deadline = pending.Event.ExpiresAt
		}
	}
	return deadline
}

func (q *durableQueue) removeSubscriptions(deviceID string, endpoints []string) error {
	remove := make(map[string]bool, len(endpoints))
	for _, endpoint := range endpoints {
		remove[endpoint] = true
	}
	if len(remove) == 0 {
		return nil
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	next := q.state.clone()
	for id, entry := range next.Entries {
		if entry.Subscription.DeviceID == deviceID && remove[entry.Subscription.Endpoint] {
			delete(next.Entries, id)
		}
	}
	for id, record := range next.Delivered {
		if record.Subscription.DeviceID == deviceID && remove[record.Subscription.Endpoint] {
			delete(next.Delivered, id)
		}
	}
	return q.commitLocked(next)
}

func (q *durableQueue) replaceSubscriptionsWhileProcessing(deviceID string, endpoints []string, replacement Subscription) error {
	replace := make(map[string]bool, len(endpoints))
	for _, endpoint := range endpoints {
		if endpoint != "" {
			replace[endpoint] = true
		}
	}
	if len(replace) == 0 {
		return nil
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	next := q.state.clone()
	for id, entry := range q.state.Entries {
		if entry.Subscription.DeviceID != deviceID || !replace[entry.Subscription.Endpoint] {
			continue
		}
		delete(next.Entries, id)
		if entry.Event.Retract && entry.Event.Key.Category.Waiting() && entry.Subscription.Endpoint != replacement.Endpoint {
			continue
		}
		entry.Subscription = replacement
		entry.ID = deliveryID(entry.Event.Key, replacement.Endpoint)
		if _, exists := next.Entries[entry.ID]; !exists {
			next.Entries[entry.ID] = entry
		}
	}
	for id, record := range next.Delivered {
		if record.Subscription.DeviceID != deviceID || !replace[record.Subscription.Endpoint] {
			continue
		}
		if record.Subscription.Endpoint == replacement.Endpoint {
			record.Subscription = replacement
			next.Delivered[id] = record
			continue
		}
		delete(next.Delivered, id)
	}
	return q.commitLocked(next)
}

func (q *durableQueue) removeDevice(deviceID string) error {
	q.mu.Lock()
	defer q.mu.Unlock()
	next := q.state.clone()
	for id, entry := range next.Entries {
		if entry.Subscription.DeviceID == deviceID {
			delete(next.Entries, id)
		}
	}
	for id, record := range next.Delivered {
		if record.Subscription.DeviceID == deviceID {
			delete(next.Delivered, id)
		}
	}
	return q.commitLocked(next)
}

func (q *durableQueue) activeKeys() []PushEventKey {
	q.mu.Lock()
	defer q.mu.Unlock()
	seen := make(map[PushEventKey]struct{}, len(q.state.Entries)+len(q.state.Delivered))
	for _, entry := range q.state.Entries {
		if !entry.Event.Retract {
			seen[entry.Event.Key] = struct{}{}
		}
	}
	for id, record := range q.state.Delivered {
		if !q.state.Entries[id].Event.Retract {
			seen[record.Key] = struct{}{}
		}
	}
	keys := make([]PushEventKey, 0, len(seen))
	for key := range seen {
		keys = append(keys, key)
	}
	sort.Slice(keys, func(i, j int) bool {
		return notificationTag(keys[i]) < notificationTag(keys[j])
	})
	return keys
}

func (q *durableQueue) deliveredFor(key PushEventKey) []deliveredRecord {
	q.mu.Lock()
	defer q.mu.Unlock()
	var records []deliveredRecord
	for _, record := range q.state.Delivered {
		if record.Key == key {
			records = append(records, record)
		}
	}
	return records
}

func (q *durableQueue) forgetDelivered(key PushEventKey) error {
	q.mu.Lock()
	defer q.mu.Unlock()
	next := q.state.clone()
	for id, record := range next.Delivered {
		if record.Key == key {
			delete(next.Delivered, id)
		}
	}
	return q.commitLocked(next)
}

func (q *durableQueue) hasEntriesFor(key PushEventKey) bool {
	q.mu.Lock()
	defer q.mu.Unlock()
	for _, entry := range q.state.Entries {
		if entry.Event.Key == key {
			return true
		}
	}
	return false
}

type DeliveryDisposition string

const (
	DeliveryAccepted DeliveryDisposition = "service_accepted"
	DeliveryRetrying DeliveryDisposition = "queued_for_retry"
	DeliveryDropped  DeliveryDisposition = "dropped"
	DeliveryPruned   DeliveryDisposition = "subscription_pruned"
	DeliveryExpired  DeliveryDisposition = "expired"
	DeliveryStale    DeliveryDisposition = "stale"
)

type DeliveryResult struct {
	Key          PushEventKey        `json:"key"`
	Endpoint     string              `json:"-"`
	Disposition  DeliveryDisposition `json:"disposition"`
	Attempts     int                 `json:"attempts"`
	NextAttempt  time.Time           `json:"next_attempt,omitempty"`
	Event        PushEvent           `json:"-"`
	Subscription Subscription        `json:"-"`
}

type prunedRecovery struct {
	deviceID  string
	endpoints []string
	fallback  *Subscription
	results   []DeliveryResult
}

func (q *durableQueue) recoverPrunedWhileProcessing(recoveries []prunedRecovery, now time.Time) bool {
	q.mu.Lock()
	defer q.mu.Unlock()
	dirty := false
	for _, recovery := range recoveries {
		endpoints := make(map[string]bool, len(recovery.endpoints))
		for _, endpoint := range recovery.endpoints {
			if endpoint != "" {
				endpoints[endpoint] = true
			}
		}
		for id, entry := range q.state.Entries {
			if entry.Subscription.DeviceID != recovery.deviceID || !endpoints[entry.Subscription.Endpoint] {
				continue
			}
			delete(q.state.Entries, id)
			dirty = true
			if recovery.fallback == nil || entry.Event.Retract && entry.Event.Key.Category.Waiting() {
				continue
			}
			entry.Subscription = *recovery.fallback
			entry.ID = deliveryID(entry.Event.Key, recovery.fallback.Endpoint)
			if _, exists := q.state.Entries[entry.ID]; !exists {
				q.state.Entries[entry.ID] = entry
			}
		}
		for id, record := range q.state.Delivered {
			if record.Subscription.DeviceID == recovery.deviceID && endpoints[record.Subscription.Endpoint] {
				delete(q.state.Delivered, id)
				dirty = true
			}
		}
		if recovery.fallback == nil {
			continue
		}
		for _, result := range recovery.results {
			if result.Event.Retract && result.Key.Category.Waiting() {
				continue
			}
			id := deliveryID(result.Event.Key, recovery.fallback.Endpoint)
			if _, exists := q.state.Entries[id]; exists {
				continue
			}
			q.state.Entries[id] = queueEntry{
				ID: id, Event: result.Event, Subscription: *recovery.fallback, DueAt: now,
			}
			dirty = true
		}
	}
	return dirty
}

func (q *durableQueue) restorePrunedWhileProcessing(results []DeliveryResult, now time.Time) bool {
	q.mu.Lock()
	defer q.mu.Unlock()
	dirty := false
	for _, result := range results {
		if result.Disposition != DeliveryPruned {
			continue
		}
		id := deliveryID(result.Event.Key, result.Subscription.Endpoint)
		if _, exists := q.state.Entries[id]; exists {
			continue
		}
		dueAt := now.Add(retryDelay(result.Attempts))
		if !dueAt.Before(result.Event.ExpiresAt) {
			dueAt = now
		}
		q.state.Entries[id] = queueEntry{
			ID: id, Event: result.Event, Subscription: result.Subscription,
			DueAt: dueAt, Attempts: result.Attempts,
		}
		dirty = true
	}
	return dirty
}

func newDeliveryResult(entry queueEntry, disposition DeliveryDisposition, attempts int) DeliveryResult {
	return DeliveryResult{
		Key: entry.Event.Key, Endpoint: entry.Subscription.Endpoint,
		Disposition: disposition, Attempts: attempts,
		Event: entry.Event, Subscription: entry.Subscription,
	}
}

func (q *durableQueue) write() error {
	q.mu.Lock()
	defer q.mu.Unlock()
	return q.writeState(q.state)
}

func (q *durableQueue) dueEntries(now time.Time) []queueEntry {
	q.mu.Lock()
	defer q.mu.Unlock()
	entries := make([]queueEntry, 0, len(q.state.Entries))
	for _, entry := range q.state.Entries {
		if !entry.DueAt.After(now) {
			entries = append(entries, entry)
		}
	}
	sort.Slice(entries, func(i, j int) bool {
		if entries[i].DueAt.Equal(entries[j].DueAt) {
			return entries[i].ID < entries[j].ID
		}
		return entries[i].DueAt.Before(entries[j].DueAt)
	})
	return entries
}

func (q *durableQueue) rescheduleInMemory(id string, snapshot queueEntry, attempts int, next time.Time) bool {
	q.mu.Lock()
	defer q.mu.Unlock()
	current, ok := q.state.Entries[id]
	if !ok || !sameQueueEntry(current, snapshot) {
		return false
	}
	current.Attempts = attempts
	current.DueAt = next
	q.state.Entries[id] = current
	return true
}

func (q *durableQueue) finishInMemory(id string, snapshot queueEntry, disposition DeliveryDisposition, now time.Time) bool {
	q.mu.Lock()
	defer q.mu.Unlock()
	current, ok := q.state.Entries[id]
	if !ok || !sameQueueEntry(current, snapshot) {
		return false
	}
	delete(q.state.Entries, id)
	if snapshot.Event.Retract {
		delete(q.state.Delivered, id)
	} else if disposition == DeliveryAccepted {
		q.state.Delivered[id] = deliveredRecord{
			Key: snapshot.Event.Key, Subscription: snapshot.Subscription,
			Tag: notificationTag(snapshot.Event.Key), AcceptedAt: now, WaitingSeq: snapshot.Event.WaitingSeq,
		}
	}
	return true
}

func sameQueueEntry(left, right queueEntry) bool {
	return left.ID == right.ID &&
		left.Event.Key == right.Event.Key &&
		left.Event.CreatedAt.Equal(right.Event.CreatedAt) &&
		left.Event.ExpiresAt.Equal(right.Event.ExpiresAt) &&
		left.Event.Retract == right.Event.Retract &&
		left.Event.WaitingSeq == right.Event.WaitingSeq &&
		bytes.Equal(left.Event.Payload, right.Event.Payload) &&
		left.Subscription == right.Subscription &&
		left.DueAt.Equal(right.DueAt) &&
		left.Attempts == right.Attempts
}

func retryablePushError(err error) bool {
	if err == nil || errors.Is(err, context.Canceled) {
		return false
	}
	var pe *pushError
	if errors.As(err, &pe) {
		return pe.statusCode == http.StatusRequestTimeout || pe.statusCode == http.StatusTooManyRequests || pe.statusCode >= 500
	}
	var networkError net.Error
	return errors.As(err, &networkError)
}

func retryDelay(attempts int) time.Duration {
	if attempts < 1 {
		attempts = 1
	}
	delay := time.Second << (attempts - 1)
	if delay > time.Minute {
		return time.Minute
	}
	return delay
}

func deliveryID(key PushEventKey, endpoint string) string {
	data, _ := json.Marshal(struct {
		Key      PushEventKey `json:"key"`
		Endpoint string       `json:"endpoint"`
	}{Key: key, Endpoint: endpoint})
	digest := sha256.Sum256(data)
	return hex.EncodeToString(digest[:])
}

func (q *durableQueue) commitLocked(next queueFile) error {
	if err := q.writeState(next); err != nil {
		return err
	}
	q.state = next
	return nil
}

func (q *durableQueue) writeState(state queueFile) error {
	if len(state.Entries)+len(state.Delivered) > maxQueueEntries {
		return errors.New("push_queue_limit")
	}
	data, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return fmt.Errorf("encode push queue: %w", err)
	}
	if len(data)+1 > maxQueueBytes {
		return errors.New("push_queue_limit")
	}
	return atomicWrite(q.path, append(data, '\n'), 0o600)
}
