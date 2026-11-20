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
	sendPush      func(context.Context, Subscription, []byte, sendOptions) error
	now           func() time.Time
	queue         *durableQueue
	policy        *PolicyEngine
	signer        *ReferenceSigner
	active        map[PushEventKey]bool
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
		now:    time.Now,
		httpClient: &http.Client{
			Timeout: 10 * time.Second,
			CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
				return http.ErrUseLastResponse
			},
		},
		active: make(map[PushEventKey]bool),
		wake:   make(chan struct{}, 1),
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
	m.reconciled = len(m.queue.state.Entries) == 0 && len(recovered) == 0

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
	m.queue.process.Lock()
	defer m.queue.process.Unlock()
	return m.updateQueue(ctx, &request, nil)
}

func (m *Manager) ApplyPaneTransition(ctx context.Context, paneID string, request *PublishRequest, current func() bool) (PublishResult, error) {
	m.queue.process.Lock()
	defer m.queue.process.Unlock()
	if !current() {
		return PublishResult{}, nil
	}
	return m.updateQueue(ctx, request, func(key PushEventKey) bool {
		if key.PaneID != paneID {
			return false
		}
		if request == nil {
			return true
		}
		if !key.Category.Waiting() {
			return key.EventID != request.Key.EventID
		}
		replacement := request.Key
		replacement.DeviceID = key.DeviceID
		return key != replacement
	})
}

func (m *Manager) updateQueue(ctx context.Context, request *PublishRequest, superseded func(PushEventKey) bool) (PublishResult, error) {
	if err := ctx.Err(); err != nil {
		return PublishResult{}, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.queue.mu.Lock()
	defer m.queue.mu.Unlock()

	next := m.queue.state.clone()
	var retire []PushEventKey
	if superseded != nil {
		now := m.now().UTC()
		for key := range m.active {
			if superseded(key) {
				if err := m.retract(&next, key, now); err != nil {
					return PublishResult{}, err
				}
				retire = append(retire, key)
			}
		}
	}
	var result PublishResult
	var added []PushEventKey
	if request != nil {
		var err error
		result, added, err = m.publish(&next, *request)
		if err != nil {
			return PublishResult{}, err
		}
	}
	if len(retire) == 0 && len(added) == 0 {
		return result, nil
	}
	if err := m.queue.commitLocked(next); err != nil {
		return PublishResult{}, err
	}
	for _, key := range retire {
		delete(m.active, key)
	}
	for _, key := range added {
		m.active[key] = true
	}
	m.signal()
	return result, nil
}

func (m *Manager) publish(next *queueFile, request PublishRequest) (PublishResult, []PushEventKey, error) {
	var result PublishResult
	now := request.CreatedAt
	if now.IsZero() {
		now = m.now().UTC()
	}
	expiresAt := request.ExpiresAt
	if expiresAt.IsZero() {
		expiresAt = now.Add(5 * time.Minute)
	}
	var added []PushEventKey
	seenDevices := make(map[string]bool, len(m.subscriptions))
	for index := len(m.subscriptions) - 1; index >= 0; index-- {
		subscription := m.subscriptions[index]
		key := request.Key
		if subscription.DeviceID == "" || seenDevices[subscription.DeviceID] ||
			(key.DeviceID != "" && key.DeviceID != subscription.DeviceID) {
			continue
		}
		seenDevices[subscription.DeviceID] = true
		key.DeviceID = subscription.DeviceID
		if key.Category.Waiting() && m.active[key] {
			continue
		}
		decision := m.policy.Decide(key, subscription.Locale, now)
		if !decision.Deliver {
			result.Suppressed++
			continue
		}
		refExpiry := expiresAt
		if key.Category.Waiting() {
			refExpiry = now.Add(48 * time.Hour)
		}
		payload, err := BuildPayload(PayloadRequest{
			Key:       key,
			Locale:    subscription.Locale,
			Preview:   request.Preview,
			ExpiresAt: refExpiry,
		}, m.signer)
		if err != nil {
			return result, nil, err
		}
		event := PushEvent{Key: key, Payload: payload, CreatedAt: now, ExpiresAt: expiresAt}
		if err := event.Validate(now); err != nil {
			return result, nil, err
		}
		next.enqueue(event, subscription, decision.DueAt)
		added = append(added, key)
		result.Queued++
	}
	return result, added, nil
}

func (m *Manager) Resolve(ctx context.Context, key PushEventKey) error {
	if err := key.Validate(); err != nil {
		return err
	}
	m.queue.process.Lock()
	defer m.queue.process.Unlock()
	_, err := m.updateQueue(ctx, nil, func(candidate PushEventKey) bool { return candidate == key })
	return err
}

func (m *Manager) retract(next *queueFile, key PushEventKey, now time.Time) error {
	next.cancelAlerts(key)
	expiresAt := now.Add(5 * time.Minute)
	for id, record := range next.Delivered {
		if record.Key != key {
			continue
		}
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
		if _, pending := next.Entries[id]; !pending {
			next.enqueue(event, record.Subscription, now)
		}
		delete(next.Delivered, id)
	}
	return nil
}

func (m *Manager) Reconcile(ctx context.Context, current func(PushEventKey) bool) error {
	m.queue.process.Lock()
	defer m.queue.process.Unlock()
	_, err := m.updateQueue(ctx, nil, func(key PushEventKey) bool {
		return key.Category != CategoryUpdate && key.Category != CategoryTest && !current(key)
	})
	if err != nil {
		return err
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
		if _, err := m.RunOnce(ctx); err != nil && !errors.Is(err, context.Canceled) {
			m.logger.Warn("push queue processing failed", "error", err)
		}
	}
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
