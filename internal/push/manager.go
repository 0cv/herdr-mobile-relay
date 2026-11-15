package push

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"github.com/0cv/herdr-mobile-relay/internal/atomicfile"
	"github.com/0cv/herdr-mobile-relay/internal/protocol"
	webpush "github.com/SherClockHolmes/webpush-go"
)

type Subscription struct {
	Endpoint string `json:"endpoint"`
	Keys     struct {
		P256dh string `json:"p256dh"`
		Auth   string `json:"auth"`
	} `json:"keys"`
	DeviceID  string   `json:"device_id,omitempty"`
	Locale    string   `json:"locale,omitempty"`
	Platform  Platform `json:"platform,omitempty"`
	UserAgent string   `json:"user_agent,omitempty"`
	ClientID  string   `json:"client_id,omitempty"`
}

type Manager struct {
	mu            sync.Mutex
	subscriptions []Subscription
	path          string
	logger        *slog.Logger
	vapidPublic   string
	vapidPrivate  string
	httpClient    webpush.HTTPClient
	sendPush      func(context.Context, Subscription, []byte) error
	queue         *durableQueue
	policy        *PolicyEngine
	signer        *ReferenceSigner
	active        map[PushEventKey]bool
	retracting    map[PushEventKey]bool
	reconciled    bool
	wake          chan struct{}
}

func NewManager(pushDir string, logger *slog.Logger) (*Manager, error) {
	if err := os.MkdirAll(pushDir, 0o700); err != nil {
		return nil, fmt.Errorf("create push dir: %w", err)
	}
	if err := os.Chmod(pushDir, 0o700); err != nil {
		return nil, fmt.Errorf("protect push dir: %w", err)
	}

	m := &Manager{
		path:   filepath.Join(pushDir, "subscriptions.json"),
		logger: logger,
		httpClient: &http.Client{
			Timeout: 10 * time.Second,
			CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
				return http.ErrUseLastResponse
			},
		},
		active:     make(map[PushEventKey]bool),
		retracting: make(map[PushEventKey]bool),
		wake:       make(chan struct{}, 1),
	}

	if err := m.load(); err != nil {
		return nil, err
	}

	if err := m.loadOrGenerateVAPIDKeys(pushDir); err != nil {
		return nil, err
	}
	m.sendPush = m.sendOne
	queue, err := newDurableQueue(filepath.Join(pushDir, "queue.json"))
	if err != nil {
		return nil, err
	}
	m.queue = queue
	policy, err := newPolicyEngine(filepath.Join(pushDir, "policies.json"))
	if err != nil {
		return nil, err
	}
	m.policy = policy
	signer, err := loadOrCreateReferenceSigner(filepath.Join(pushDir, "action_ref.key"))
	if err != nil {
		return nil, err
	}
	m.signer = signer
	recovered := m.queue.activeKeys()
	for _, key := range recovered {
		m.active[key] = true
	}
	m.reconciled = len(recovered) == 0

	return m, nil
}

type PublishRequest struct {
	Key       PushEventKey
	Preview   PreviewMode
	CreatedAt time.Time
	ExpiresAt time.Time
}

type PublishResult struct {
	Queued     int `json:"queued"`
	Suppressed int `json:"suppressed"`
}

func (m *Manager) Publish(ctx context.Context, request PublishRequest) (PublishResult, error) {
	var result PublishResult
	if err := ctx.Err(); err != nil {
		return result, err
	}
	now := request.CreatedAt
	if now.IsZero() {
		now = time.Now().UTC()
	}
	expiresAt := request.ExpiresAt
	if expiresAt.IsZero() {
		expiresAt = now.Add(5 * time.Minute)
	}
	m.queue.process.Lock()
	defer m.queue.process.Unlock()
	subscriptions := m.Subscriptions()
	selected := make([]Subscription, 0, len(subscriptions))
	seenDevices := make(map[string]bool, len(subscriptions))
	for index := len(subscriptions) - 1; index >= 0; index-- {
		subscription := subscriptions[index]
		if subscription.DeviceID == "" || seenDevices[subscription.DeviceID] ||
			(request.Key.DeviceID != "" && request.Key.DeviceID != subscription.DeviceID) {
			continue
		}
		seenDevices[subscription.DeviceID] = true
		selected = append(selected, subscription)
	}
	for _, subscription := range selected {
		key := request.Key
		key.DeviceID = subscription.DeviceID
		event := PushEvent{Key: key, CreatedAt: now, ExpiresAt: expiresAt}
		decision := m.policy.Decide(key, subscription.Locale, now)
		if !decision.Deliver {
			result.Suppressed++
			continue
		}
		payload, err := BuildPayload(PayloadRequest{
			Key:       key,
			Locale:    subscription.Locale,
			Preview:   request.Preview,
			ExpiresAt: expiresAt,
		}, m.signer)
		if err != nil {
			return result, err
		}
		event.Payload = payload
		if err := event.Validate(now); err != nil {
			return result, err
		}
		if _, err := m.queue.enqueue(event, subscription, decision.DueAt); err != nil {
			return result, err
		}
		m.mu.Lock()
		m.active[key] = true
		m.mu.Unlock()
		result.Queued++
	}
	if result.Queued > 0 {
		m.signal()
	}
	return result, nil
}

func (m *Manager) Resolve(ctx context.Context, key PushEventKey) error {
	if err := key.Validate(); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := m.queue.cancelKey(key); err != nil {
		return err
	}
	records := m.queue.deliveredFor(key)
	m.mu.Lock()
	delete(m.active, key)
	if len(records) > 0 {
		m.retracting[key] = true
	}
	m.mu.Unlock()
	if len(records) == 0 {
		return m.queue.forgetDelivered(key)
	}
	now := time.Now().UTC()
	expiresAt := now.Add(5 * time.Minute)
	for _, record := range records {
		payload, err := BuildPayload(PayloadRequest{
			Key:       key,
			Locale:    record.Subscription.Locale,
			Preview:   PreviewHidden,
			ExpiresAt: expiresAt,
			Retract:   true,
		}, m.signer)
		if err != nil {
			return err
		}
		event := PushEvent{
			Key: key, Payload: payload, CreatedAt: now, ExpiresAt: expiresAt, Retract: true,
		}
		if _, err := m.queue.enqueue(event, record.Subscription, now); err != nil {
			return err
		}
	}
	m.signal()
	return nil
}

func (m *Manager) ResolvePaneID(ctx context.Context, paneID, exceptEventID string) error {
	m.mu.Lock()
	keys := make([]PushEventKey, 0, len(m.active))
	for key := range m.active {
		if key.PaneID == paneID && key.EventID != exceptEventID {
			keys = append(keys, key)
		}
	}
	m.mu.Unlock()
	for _, key := range keys {
		if err := m.Resolve(ctx, key); err != nil {
			return err
		}
	}
	return nil
}

// Reconcile releases recovered deliveries only after the caller has supplied
// the first authoritative inventory. Recovered events absent from that exact
// inventory are retracted rather than retried against a guessed session.
func (m *Manager) Reconcile(ctx context.Context, current []PushEventKey) error {
	currentSet := make(map[PushEventKey]bool, len(current))
	for _, key := range current {
		if err := key.Validate(); err != nil {
			return err
		}
		currentSet[key] = true
	}
	m.mu.Lock()
	stale := make([]PushEventKey, 0)
	for key := range m.active {
		if key.Category != CategoryUpdate && key.Category != CategoryTest && !currentSet[key] {
			stale = append(stale, key)
		}
	}
	m.reconciled = false
	m.mu.Unlock()
	for _, key := range stale {
		if err := m.Resolve(ctx, key); err != nil {
			return err
		}
	}
	m.mu.Lock()
	m.reconciled = true
	m.mu.Unlock()
	m.signal()
	return nil
}

func (m *Manager) Run(ctx context.Context) {
	ticker := time.NewTicker(250 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		case <-m.wake:
		}
		if _, err := m.RunOnce(ctx, time.Now().UTC()); err != nil && !errors.Is(err, context.Canceled) {
			m.logger.Warn("push queue processing failed", "error", err)
		}
	}
}

func (m *Manager) RunOnce(ctx context.Context, now time.Time) ([]DeliveryResult, error) {
	m.mu.Lock()
	reconciled := m.reconciled
	m.mu.Unlock()
	if !reconciled {
		return nil, nil
	}
	results, err := m.queue.processDueWithRecovery(
		ctx,
		now,
		func(key PushEventKey) bool {
			m.mu.Lock()
			active := m.active[key]
			retracting := m.retracting[key]
			m.mu.Unlock()
			return retracting || active && m.policy.Allows(key, "en", now)
		},
		m.sendPush,
		func(key PushEventKey, acceptedAt time.Time) error {
			m.mu.Lock()
			retracting := m.retracting[key]
			m.mu.Unlock()
			if retracting {
				return nil
			}
			return m.policy.MarkAccepted(key, acceptedAt)
		},
		func(pruned []DeliveryResult) (bool, error) {
			return m.recoverPrunedSubscriptionsWhileProcessing(pruned, now)
		},
	)
	if err != nil {
		return results, err
	}
	seen := make(map[PushEventKey]bool)
	for _, result := range results {
		seen[result.Key] = true
	}
	for key := range seen {
		m.mu.Lock()
		retracting := m.retracting[key]
		m.mu.Unlock()
		if retracting && !m.queue.hasEntriesFor(key) {
			if err := m.queue.forgetDelivered(key); err != nil {
				return results, err
			}
			m.mu.Lock()
			delete(m.retracting, key)
			m.mu.Unlock()
			continue
		}
		if m.queue.hasEntriesFor(key) {
			continue
		}
		if key.Category == CategoryTest || key.Category == CategoryUpdate {
			if err := m.queue.forgetDelivered(key); err != nil {
				return results, err
			}
		}
		if key.Category == CategoryTest || key.Category == CategoryUpdate || len(m.queue.deliveredFor(key)) == 0 {
			m.mu.Lock()
			delete(m.active, key)
			m.mu.Unlock()
		}
	}
	return results, nil
}
func (m *Manager) recoverPrunedSubscriptionsWhileProcessing(results []DeliveryResult, now time.Time) (bool, error) {
	type prunedDevice struct {
		endpoints map[string]bool
		results   []DeliveryResult
	}
	pruned := make(map[string]*prunedDevice)
	for _, result := range results {
		if result.Disposition != DeliveryPruned {
			continue
		}
		deviceID := result.Subscription.DeviceID
		device := pruned[deviceID]
		if device == nil {
			device = &prunedDevice{endpoints: make(map[string]bool)}
			pruned[deviceID] = device
		}
		device.endpoints[result.Subscription.Endpoint] = true
		device.results = append(device.results, result)
	}
	if len(pruned) == 0 {
		return false, nil
	}

	m.mu.Lock()
	defer m.mu.Unlock()
	deviceIDs := make([]string, 0, len(pruned))
	for deviceID := range pruned {
		deviceIDs = append(deviceIDs, deviceID)
	}
	sort.Strings(deviceIDs)
	recoveries := make([]prunedRecovery, 0, len(deviceIDs))
	requeued := false
	for _, deviceID := range deviceIDs {
		device := pruned[deviceID]
		endpoints := make([]string, 0, len(device.endpoints))
		for endpoint := range device.endpoints {
			endpoints = append(endpoints, endpoint)
		}
		sort.Strings(endpoints)
		var fallback *Subscription
		for index := len(m.subscriptions) - 1; index >= 0; index-- {
			candidate := m.subscriptions[index]
			if candidate.DeviceID == deviceID && !device.endpoints[candidate.Endpoint] {
				copy := candidate
				fallback = &copy
				break
			}
		}
		recoveries = append(recoveries, prunedRecovery{
			deviceID: deviceID, endpoints: endpoints, fallback: fallback, results: device.results,
		})
		requeued = requeued || fallback != nil && len(device.results) > 0
	}

	filtered := make([]Subscription, 0, len(m.subscriptions))
	for _, subscription := range m.subscriptions {
		device := pruned[subscription.DeviceID]
		if device != nil && device.endpoints[subscription.Endpoint] {
			continue
		}
		filtered = append(filtered, subscription)
	}
	if err := m.persist(filtered); err != nil {
		return false, err
	}
	dirty := m.queue.recoverPrunedWhileProcessing(recoveries, now)
	m.subscriptions = filtered
	if requeued {
		m.signal()
	}
	return dirty, nil
}

func (m *Manager) Policy(deviceID, locale string) DevicePolicy {
	policy := m.policy.Get(deviceID, locale)
	if policy.Snoozed && !policy.SnoozeUntil.IsZero() && !time.Now().UTC().Before(policy.SnoozeUntil) {
		policy.Snoozed = false
		policy.SnoozeUntil = time.Time{}
	}
	return policy
}

func (m *Manager) SetPolicy(policy DevicePolicy) error {
	return m.policy.Set(policy)
}

func (m *Manager) SetViewedPane(deviceID string, target *protocol.TargetRef) {
	m.policy.SetViewedPane(deviceID, target)
}

func (m *Manager) RecoveredKeys() []PushEventKey {
	return m.queue.activeKeys()
}

func (m *Manager) VerifyEventReference(token string, now time.Time) (ReferenceClaims, error) {
	claims, err := m.signer.Verify(token, now)
	if err != nil {
		return ReferenceClaims{}, err
	}
	m.mu.Lock()
	current := m.reconciled && m.active[claims.Key]
	m.mu.Unlock()
	if !current {
		return ReferenceClaims{}, ErrStaleReference
	}
	return claims, nil
}

func (m *Manager) signal() {
	select {
	case m.wake <- struct{}{}:
	default:
	}
}

func atomicWrite(filename string, data []byte, mode os.FileMode) error {
	_, err := atomicfile.Replace(filepath.Dir(filename), filename, "."+filepath.Base(filename)+".", data, mode)
	return err
}
