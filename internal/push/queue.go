package push

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
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
	Key       PushEventKey `json:"key"`
	Payload   []byte       `json:"payload"`
	CreatedAt time.Time    `json:"created_at"`
	ExpiresAt time.Time    `json:"expires_at"`
	Retract   bool         `json:"retract,omitempty"`
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
}

type queueFile struct {
	Entries   map[string]queueEntry      `json:"entries"`
	Delivered map[string]deliveredRecord `json:"delivered"`
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
	id := deliveryID(event.Key, subscription.Endpoint)
	entry := queueEntry{ID: id, Event: event, Subscription: subscription, DueAt: dueAt}
	q.mu.Lock()
	defer q.mu.Unlock()
	previous, existed := q.state.Entries[id]
	q.state.Entries[id] = entry
	if err := q.persistLocked(); err != nil {
		if existed {
			q.state.Entries[id] = previous
		} else {
			delete(q.state.Entries, id)
		}
		return "", err
	}
	return id, nil
}

func (q *durableQueue) cancelKey(key PushEventKey) error {
	q.process.Lock()
	defer q.process.Unlock()
	q.mu.Lock()
	defer q.mu.Unlock()
	removed := make(map[string]queueEntry)
	for id, entry := range q.state.Entries {
		if entry.Event.Key == key {
			removed[id] = entry
			delete(q.state.Entries, id)
		}
	}
	if len(removed) == 0 {
		return nil
	}
	if err := q.persistLocked(); err != nil {
		for id, entry := range removed {
			q.state.Entries[id] = entry
		}
		return err
	}
	return nil
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
	previousEntries := cloneQueueEntries(q.state.Entries)
	previousDelivered := cloneDeliveredRecords(q.state.Delivered)
	for id, entry := range q.state.Entries {
		if entry.Subscription.DeviceID == deviceID && remove[entry.Subscription.Endpoint] {
			delete(q.state.Entries, id)
		}
	}
	for id, record := range q.state.Delivered {
		if record.Subscription.DeviceID == deviceID && remove[record.Subscription.Endpoint] {
			delete(q.state.Delivered, id)
		}
	}
	if err := q.persistLocked(); err != nil {
		q.state.Entries = previousEntries
		q.state.Delivered = previousDelivered
		return err
	}
	return nil
}
func (q *durableQueue) replaceSubscriptions(deviceID string, endpoints []string, replacement Subscription) error {
	q.process.Lock()
	defer q.process.Unlock()
	return q.replaceSubscriptionsWhileProcessing(deviceID, endpoints, replacement)
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
	previousEntries := cloneQueueEntries(q.state.Entries)
	previousDelivered := cloneDeliveredRecords(q.state.Delivered)
	for id, entry := range q.state.Entries {
		if entry.Subscription.DeviceID != deviceID || !replace[entry.Subscription.Endpoint] {
			continue
		}
		delete(q.state.Entries, id)
		entry.Subscription = replacement
		entry.ID = deliveryID(entry.Event.Key, replacement.Endpoint)
		if _, exists := q.state.Entries[entry.ID]; !exists {
			q.state.Entries[entry.ID] = entry
		}
	}
	for id, record := range q.state.Delivered {
		if record.Subscription.DeviceID != deviceID || !replace[record.Subscription.Endpoint] {
			continue
		}
		if record.Subscription.Endpoint == replacement.Endpoint {
			record.Subscription = replacement
			q.state.Delivered[id] = record
			continue
		}
		delete(q.state.Delivered, id)
	}
	if err := q.persistLocked(); err != nil {
		q.state.Entries = previousEntries
		q.state.Delivered = previousDelivered
		return err
	}
	return nil
}

func cloneQueueEntries(entries map[string]queueEntry) map[string]queueEntry {
	cloned := make(map[string]queueEntry, len(entries))
	for id, entry := range entries {
		cloned[id] = entry
	}
	return cloned
}

func cloneDeliveredRecords(records map[string]deliveredRecord) map[string]deliveredRecord {
	cloned := make(map[string]deliveredRecord, len(records))
	for id, record := range records {
		cloned[id] = record
	}
	return cloned
}

func (q *durableQueue) removeDevice(deviceID string) error {
	q.process.Lock()
	defer q.process.Unlock()
	q.mu.Lock()
	defer q.mu.Unlock()
	removedEntries := make(map[string]queueEntry)
	removedDelivered := make(map[string]deliveredRecord)
	for id, entry := range q.state.Entries {
		if entry.Subscription.DeviceID == deviceID {
			removedEntries[id] = entry
			delete(q.state.Entries, id)
		}
	}
	for id, record := range q.state.Delivered {
		if record.Subscription.DeviceID == deviceID {
			removedDelivered[id] = record
			delete(q.state.Delivered, id)
		}
	}
	if err := q.persistLocked(); err != nil {
		for id, entry := range removedEntries {
			q.state.Entries[id] = entry
		}
		for id, record := range removedDelivered {
			q.state.Delivered[id] = record
		}
		return err
	}
	return nil
}

func (q *durableQueue) activeKeys() []PushEventKey {
	q.mu.Lock()
	defer q.mu.Unlock()
	seen := make(map[PushEventKey]struct{}, len(q.state.Entries)+len(q.state.Delivered))
	for _, entry := range q.state.Entries {
		seen[entry.Event.Key] = struct{}{}
	}
	for _, record := range q.state.Delivered {
		seen[record.Key] = struct{}{}
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
	for id, record := range q.state.Delivered {
		if record.Key == key {
			delete(q.state.Delivered, id)
		}
	}
	return q.persistLocked()
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

type recoverPrunedFunc func([]DeliveryResult) (bool, error)

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
			if recovery.fallback == nil {
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

type currentEventFunc func(PushEventKey) bool

type queueSender func(context.Context, Subscription, []byte) error

func (q *durableQueue) processDue(
	ctx context.Context,
	now time.Time,
	current currentEventFunc,
	send queueSender,
	accepted func(PushEventKey, time.Time) error,
) ([]DeliveryResult, error) {
	return q.processDueWithRecovery(ctx, now, current, send, accepted, nil)
}

func (q *durableQueue) processDueWithRecovery(
	ctx context.Context,
	now time.Time,
	current currentEventFunc,
	send queueSender,
	accepted func(PushEventKey, time.Time) error,
	recoverPruned recoverPrunedFunc,
) ([]DeliveryResult, error) {
	q.process.Lock()
	defer q.process.Unlock()
	entries := q.dueEntries(now)
	results := make([]DeliveryResult, 0, len(entries))
	terminalSubscriptions := make(map[string]bool)
	dirty := false
	flush := func() error {
		if !dirty {
			return nil
		}
		q.mu.Lock()
		err := q.persistLocked()
		q.mu.Unlock()
		if err == nil {
			dirty = false
		}
		return err
	}
	finish := func(cause error) ([]DeliveryResult, error) {
		if recoverPruned != nil {
			recovered, err := recoverPruned(results)
			dirty = recovered || dirty
			if err != nil {
				dirty = q.restorePrunedWhileProcessing(results, now) || dirty
				if persistErr := flush(); persistErr != nil {
					return results, persistErr
				}
				return results, err
			}
		}
		if err := flush(); err != nil {
			return results, err
		}
		return results, cause
	}
	for _, snapshot := range entries {
		if ctx.Err() != nil {
			return finish(ctx.Err())
		}
		subscriptionKey := snapshot.Subscription.DeviceID + "\x00" + snapshot.Subscription.Endpoint
		if terminalSubscriptions[subscriptionKey] {
			dirty = q.finishInMemory(snapshot.ID, snapshot, DeliveryPruned, now) || dirty
			results = append(results, DeliveryResult{
				Key: snapshot.Event.Key, Endpoint: snapshot.Subscription.Endpoint,
				Disposition: DeliveryPruned, Attempts: snapshot.Attempts,
				Event: snapshot.Event, Subscription: snapshot.Subscription,
			})
			continue
		}
		if !now.Before(snapshot.Event.ExpiresAt) {
			dirty = q.finishInMemory(snapshot.ID, snapshot, DeliveryExpired, now) || dirty
			results = append(results, DeliveryResult{Key: snapshot.Event.Key, Endpoint: snapshot.Subscription.Endpoint, Disposition: DeliveryExpired, Attempts: snapshot.Attempts})
			continue
		}
		if current == nil || !current(snapshot.Event.Key) {
			dirty = q.finishInMemory(snapshot.ID, snapshot, DeliveryStale, now) || dirty
			results = append(results, DeliveryResult{Key: snapshot.Event.Key, Endpoint: snapshot.Subscription.Endpoint, Disposition: DeliveryStale, Attempts: snapshot.Attempts})
			continue
		}
		err := send(ctx, snapshot.Subscription, snapshot.Event.Payload)
		if err != nil && ctx.Err() != nil {
			return finish(ctx.Err())
		}
		attempts := snapshot.Attempts + 1
		if err == nil {
			dirty = q.finishInMemory(snapshot.ID, snapshot, DeliveryAccepted, now) || dirty
			if accepted != nil {
				if err := accepted(snapshot.Event.Key, now); err != nil {
					return finish(err)
				}
			}
			results = append(results, DeliveryResult{Key: snapshot.Event.Key, Endpoint: snapshot.Subscription.Endpoint, Disposition: DeliveryAccepted, Attempts: attempts})
			continue
		}
		if isTerminalError(err) {
			terminalSubscriptions[subscriptionKey] = true
			dirty = q.finishInMemory(snapshot.ID, snapshot, DeliveryPruned, now) || dirty
			results = append(results, DeliveryResult{
				Key: snapshot.Event.Key, Endpoint: snapshot.Subscription.Endpoint,
				Disposition: DeliveryPruned, Attempts: attempts,
				Event: snapshot.Event, Subscription: snapshot.Subscription,
			})
			continue
		}
		if retryablePushError(err) && attempts < maxDeliveryAttempts {
			next := now.Add(retryDelay(attempts))
			if next.Before(snapshot.Event.ExpiresAt) {
				dirty = q.rescheduleInMemory(snapshot.ID, snapshot, attempts, next) || dirty
				results = append(results, DeliveryResult{Key: snapshot.Event.Key, Endpoint: snapshot.Subscription.Endpoint, Disposition: DeliveryRetrying, Attempts: attempts, NextAttempt: next})
				continue
			}
		}
		dirty = q.finishInMemory(snapshot.ID, snapshot, DeliveryDropped, now) || dirty
		results = append(results, DeliveryResult{Key: snapshot.Event.Key, Endpoint: snapshot.Subscription.Endpoint, Disposition: DeliveryDropped, Attempts: attempts})
	}
	return finish(nil)
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
	if disposition == DeliveryAccepted && !snapshot.Event.Retract {
		q.state.Delivered[id] = deliveredRecord{
			Key: snapshot.Event.Key, Subscription: snapshot.Subscription,
			Tag: notificationTag(snapshot.Event.Key), AcceptedAt: now,
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
	if asPushError(err, &pe) {
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

func (q *durableQueue) persistLocked() error {
	if len(q.state.Entries)+len(q.state.Delivered) > maxQueueEntries {
		return errors.New("push_queue_limit")
	}
	data, err := json.MarshalIndent(q.state, "", "  ")
	if err != nil {
		return fmt.Errorf("encode push queue: %w", err)
	}
	if len(data)+1 > maxQueueBytes {
		return errors.New("push_queue_limit")
	}
	return atomicWrite(q.path, append(data, '\n'), 0o600)
}
