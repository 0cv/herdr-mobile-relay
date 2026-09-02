package push

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"log/slog"
	"math/big"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/0cv/herdr-mobile-relay/internal/protocol"
	webpush "github.com/SherClockHolmes/webpush-go"
)

type Subscription struct {
	Endpoint string `json:"endpoint"`
	Keys     struct {
		P256dh string `json:"p256dh"`
		Auth   string `json:"auth"`
	} `json:"keys"`
	DeviceID       string   `json:"device_id,omitempty"`
	Locale         string   `json:"locale,omitempty"`
	Platform       Platform `json:"platform,omitempty"`
	UserAgent      string   `json:"user_agent,omitempty"`
	NotifyFinished bool     `json:"notify_finished,omitempty"`
	ClientID       string   `json:"client_id,omitempty"`
}

// pythonSubscription matches the Python relay's on-disk format:
// {"subscriptions":[{"subscription":{endpoint,keys},"client_id":"...","user_agent":"...","notify_finished":bool}]}
type pythonSubscription struct {
	Subscription struct {
		Endpoint string `json:"endpoint"`
		Keys     struct {
			P256dh string `json:"p256dh"`
			Auth   string `json:"auth"`
		} `json:"keys"`
	} `json:"subscription"`
	DeviceID       string   `json:"device_id,omitempty"`
	Locale         string   `json:"locale,omitempty"`
	Platform       Platform `json:"platform,omitempty"`
	ClientID       string   `json:"client_id"`
	UserAgent      string   `json:"user_agent"`
	NotifyFinished bool     `json:"notify_finished"`
}

type pythonFile struct {
	Subscriptions []pythonSubscription `json:"subscriptions"`
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

func (m *Manager) loadOrGenerateVAPIDKeys(pushDir string) error {
	privPath := filepath.Join(pushDir, "vapid_private.pem")
	pubPath := filepath.Join(pushDir, "vapid_public.pem")

	privData, privErr := os.ReadFile(privPath)
	pubData, pubErr := os.ReadFile(pubPath)

	if privErr != nil && !os.IsNotExist(privErr) {
		return fmt.Errorf("read VAPID private key: %w", privErr)
	}
	if pubErr != nil && !os.IsNotExist(pubErr) {
		return fmt.Errorf("read VAPID public key: %w", pubErr)
	}

	privMissing := os.IsNotExist(privErr)
	pubMissing := os.IsNotExist(pubErr)
	if privMissing && !pubMissing {
		return fmt.Errorf("VAPID private key is missing while public key exists")
	}
	if !privMissing {
		privateKey, err := parseVAPIDPrivateKey(string(privData))
		if err != nil {
			return fmt.Errorf("parse VAPID private key: %w", err)
		}
		derivedPublic := deriveVAPIDPublic(privateKey)

		if pubMissing {
			publicDER, err := x509.MarshalPKIXPublicKey(derivedPublic)
			if err != nil {
				return fmt.Errorf("encode derived VAPID public key: %w", err)
			}
			publicPEM := pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: publicDER})
			if err := atomicWrite(pubPath, publicPEM, 0o644); err != nil {
				return fmt.Errorf("write derived VAPID public key: %w", err)
			}
			m.logger.Info("derived missing VAPID public key from existing private key")
		} else {
			publicKey, err := parseVAPIDPublicKey(string(pubData))
			if err != nil {
				return fmt.Errorf("parse VAPID public key: %w", err)
			}
			if publicKey.X.Cmp(derivedPublic.X) != 0 || publicKey.Y.Cmp(derivedPublic.Y) != 0 {
				return fmt.Errorf("VAPID public key does not match private key")
			}
		}

		if err := os.Chmod(privPath, 0o600); err != nil {
			return fmt.Errorf("protect VAPID private key: %w", err)
		}
		m.vapidPrivate = encodeVAPIDPrivate(privateKey)
		m.vapidPublic = encodeVAPIDPublic(derivedPublic)
		return nil
	}

	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return fmt.Errorf("generate VAPID keys: %w", err)
	}
	privateDER, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return fmt.Errorf("encode VAPID private key: %w", err)
	}
	publicDER, err := x509.MarshalPKIXPublicKey(&key.PublicKey)
	if err != nil {
		return fmt.Errorf("encode VAPID public key: %w", err)
	}
	privatePEM := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: privateDER})
	publicPEM := pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: publicDER})
	if err := atomicWrite(privPath, privatePEM, 0o600); err != nil {
		return fmt.Errorf("write VAPID private key: %w", err)
	}
	if err := atomicWrite(pubPath, publicPEM, 0o644); err != nil {
		return fmt.Errorf("write VAPID public key: %w", err)
	}

	m.vapidPrivate = encodeVAPIDPrivate(key)
	m.vapidPublic = encodeVAPIDPublic(&key.PublicKey)
	m.logger.Info("generated new VAPID key pair")
	return nil
}

// parseVAPIDPrivate handles both PEM-encoded EC private keys (Python format)
// and raw base64url scalars (webpush-go format).
func parseVAPIDPrivate(data string) (string, error) {
	key, err := parseVAPIDPrivateKey(data)
	if err != nil {
		return "", err
	}
	return encodeVAPIDPrivate(key), nil
}

func parseVAPIDPrivateKey(data string) (*ecdsa.PrivateKey, error) {
	data = strings.TrimSpace(data)
	if !strings.HasPrefix(data, "-----BEGIN") {
		scalar, err := base64.RawURLEncoding.DecodeString(data)
		if err != nil {
			return nil, fmt.Errorf("decode private scalar: %w", err)
		}
		curve := elliptic.P256()
		d := new(big.Int).SetBytes(scalar)
		if d.Sign() <= 0 || d.Cmp(curve.Params().N) >= 0 {
			return nil, fmt.Errorf("private scalar is outside the P-256 range")
		}
		x, y := curve.ScalarBaseMult(d.Bytes())
		return &ecdsa.PrivateKey{
			PublicKey: ecdsa.PublicKey{Curve: curve, X: x, Y: y},
			D:         d,
		}, nil
	}

	block, _ := pem.Decode([]byte(data))
	if block == nil {
		return nil, fmt.Errorf("invalid PEM block")
	}

	key, err := x509.ParseECPrivateKey(block.Bytes)
	if err != nil {
		pkcs8Key, err2 := x509.ParsePKCS8PrivateKey(block.Bytes)
		if err2 != nil {
			return nil, fmt.Errorf("parse EC key: %v (pkcs8: %v)", err, err2)
		}
		ecKey, ok := pkcs8Key.(*ecdsa.PrivateKey)
		if !ok {
			return nil, fmt.Errorf("not an EC private key")
		}
		key = ecKey
	}
	if key.Curve != elliptic.P256() {
		return nil, fmt.Errorf("VAPID private key must use P-256")
	}
	return key, nil
}

// parseVAPIDPublic handles both PEM-encoded EC public keys (Python format)
// and raw base64url points (webpush-go format).
func parseVAPIDPublic(data string) (string, error) {
	key, err := parseVAPIDPublicKey(data)
	if err != nil {
		return "", err
	}
	return encodeVAPIDPublic(key), nil
}

func parseVAPIDPublicKey(data string) (*ecdsa.PublicKey, error) {
	data = strings.TrimSpace(data)
	if !strings.HasPrefix(data, "-----BEGIN") {
		point, err := base64.RawURLEncoding.DecodeString(data)
		if err != nil {
			return nil, fmt.Errorf("decode public point: %w", err)
		}
		x, y := elliptic.Unmarshal(elliptic.P256(), point)
		if x == nil || y == nil {
			return nil, fmt.Errorf("invalid P-256 public point")
		}
		return &ecdsa.PublicKey{Curve: elliptic.P256(), X: x, Y: y}, nil
	}

	block, _ := pem.Decode([]byte(data))
	if block == nil {
		return nil, fmt.Errorf("invalid PEM block")
	}

	pub, err := x509.ParsePKIXPublicKey(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("parse public key: %w", err)
	}

	ecPub, ok := pub.(*ecdsa.PublicKey)
	if !ok {
		return nil, fmt.Errorf("not an EC public key")
	}
	if ecPub.Curve != elliptic.P256() {
		return nil, fmt.Errorf("VAPID public key must use P-256")
	}
	return ecPub, nil
}

func deriveVAPIDPublic(key *ecdsa.PrivateKey) *ecdsa.PublicKey {
	x, y := elliptic.P256().ScalarBaseMult(key.D.Bytes())
	return &ecdsa.PublicKey{Curve: elliptic.P256(), X: x, Y: y}
}

func encodeVAPIDPrivate(key *ecdsa.PrivateKey) string {
	privateScalar := make([]byte, 32)
	key.D.FillBytes(privateScalar)
	return base64.RawURLEncoding.EncodeToString(privateScalar)
}

func encodeVAPIDPublic(key *ecdsa.PublicKey) string {
	return base64.RawURLEncoding.EncodeToString(elliptic.Marshal(elliptic.P256(), key.X, key.Y))
}

func validPushEndpoint(raw string) bool {
	endpoint, err := url.Parse(raw)
	if err != nil || endpoint.Scheme != "https" || endpoint.User != nil ||
		endpoint.Hostname() == "" || endpoint.Fragment != "" {
		return false
	}
	if port := endpoint.Port(); port != "" && port != "443" {
		return false
	}
	host := strings.ToLower(strings.TrimSuffix(endpoint.Hostname(), "."))
	switch host {
	case "fcm.googleapis.com", "android.googleapis.com", "updates.push.services.mozilla.com",
		"push.services.mozilla.com", "web.push.apple.com":
		return true
	default:
		return host == "notify.windows.com" || strings.HasSuffix(host, ".notify.windows.com")
	}
}

func keepValidPushSubscriptions(subscriptions []Subscription) []Subscription {
	valid := subscriptions[:0]
	for _, subscription := range subscriptions {
		if validPushEndpoint(subscription.Endpoint) {
			valid = append(valid, subscription)
		}
	}
	return valid
}

func (m *Manager) load() error {
	data, err := os.ReadFile(m.path)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("read subscriptions: %w", err)
	}

	// Try the Python wrapper format first: {"subscriptions":[...]}
	var pf pythonFile
	if err := json.Unmarshal(data, &pf); err == nil && pf.Subscriptions != nil {
		m.subscriptions = make([]Subscription, 0, len(pf.Subscriptions))
		for _, ps := range pf.Subscriptions {
			var sub Subscription
			sub.Endpoint = ps.Subscription.Endpoint
			sub.Keys.P256dh = ps.Subscription.Keys.P256dh
			sub.Keys.Auth = ps.Subscription.Keys.Auth
			sub.DeviceID = ps.DeviceID
			sub.Locale = ps.Locale
			sub.Platform = ps.Platform
			sub.UserAgent = ps.UserAgent
			sub.NotifyFinished = ps.NotifyFinished
			sub.ClientID = ps.ClientID
			m.subscriptions = append(m.subscriptions, sub)
		}
		m.subscriptions = keepValidPushSubscriptions(m.subscriptions)
		return nil
	}

	// Fallback: flat array (legacy Go format)
	if err := json.Unmarshal(data, &m.subscriptions); err != nil {
		return err
	}
	m.subscriptions = keepValidPushSubscriptions(m.subscriptions)
	return nil
}

func (m *Manager) persist(subscriptions []Subscription) error {
	pf := pythonFile{Subscriptions: make([]pythonSubscription, 0, len(subscriptions))}
	for _, sub := range subscriptions {
		var ps pythonSubscription
		ps.Subscription.Endpoint = sub.Endpoint
		ps.Subscription.Keys.P256dh = sub.Keys.P256dh
		ps.Subscription.Keys.Auth = sub.Keys.Auth
		ps.DeviceID = sub.DeviceID
		ps.Locale = sub.Locale
		ps.Platform = sub.Platform
		ps.UserAgent = sub.UserAgent
		ps.NotifyFinished = sub.NotifyFinished
		ps.ClientID = sub.ClientID
		pf.Subscriptions = append(pf.Subscriptions, ps)
	}

	data, err := json.MarshalIndent(pf, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	return atomicWrite(m.path, data, 0o600)
}

func (m *Manager) Subscribe(sub Subscription, replacementSets ...[]string) error {
	if sub.Keys.P256dh == "" || sub.Keys.Auth == "" {
		return fmt.Errorf("subscription endpoint and keys are required")
	}
	if !validPushEndpoint(sub.Endpoint) {
		return errors.New("push_subscription_endpoint_not_allowed")
	}
	m.queue.process.Lock()
	defer m.queue.process.Unlock()
	m.mu.Lock()
	defer m.mu.Unlock()

	var replaceEndpoints []string
	if len(replacementSets) > 0 {
		replaceEndpoints = replacementSets[0]
	}
	replace := make(map[string]bool, len(replaceEndpoints))
	for _, endpoint := range replaceEndpoints {
		if endpoint != "" {
			replace[endpoint] = true
		}
	}
	filtered := make([]Subscription, 0, len(m.subscriptions)+1)
	for _, existing := range m.subscriptions {
		if replace[existing.Endpoint] && existing.Endpoint != sub.Endpoint && existing.DeviceID == sub.DeviceID {
			continue
		}
		filtered = append(filtered, existing)
	}
	for i, existing := range filtered {
		if existing.Endpoint != sub.Endpoint {
			continue
		}
		if existing.DeviceID != sub.DeviceID {
			return errors.New("push_subscription_device_mismatch")
		}
		filtered[i] = sub
		if err := m.persist(filtered); err != nil {
			return err
		}
		replacements := append(append([]string(nil), replaceEndpoints...), sub.Endpoint)
		if err := m.queue.replaceSubscriptionsWhileProcessing(sub.DeviceID, replacements, sub); err != nil {
			_ = m.persist(m.subscriptions)
			return err
		}
		m.subscriptions = filtered
		return nil
	}
	filtered = append(filtered, sub)
	if err := m.persist(filtered); err != nil {
		return err
	}
	if err := m.queue.replaceSubscriptionsWhileProcessing(sub.DeviceID, replaceEndpoints, sub); err != nil {
		_ = m.persist(m.subscriptions)
		return err
	}
	m.subscriptions = filtered
	return nil
}

func (m *Manager) Unsubscribe(endpoints []string, clientIDs ...string) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	clientID := ""
	if len(clientIDs) > 0 {
		clientID = clientIDs[0]
	}
	remove := make(map[string]bool, len(endpoints))
	for _, e := range endpoints {
		remove[e] = true
	}

	var filtered []Subscription
	for _, sub := range m.subscriptions {
		matchesClient := clientID != "" && sub.ClientID == clientID
		if !remove[sub.Endpoint] && !matchesClient {
			filtered = append(filtered, sub)
		}
	}
	if err := m.persist(filtered); err != nil {
		return err
	}
	m.subscriptions = filtered
	return nil
}

func (m *Manager) UnsubscribeDevice(deviceID string, endpoints []string, clientID string) error {
	if strings.TrimSpace(deviceID) == "" {
		return errors.New("push_device_required")
	}
	remove := make(map[string]bool, len(endpoints))
	for _, endpoint := range endpoints {
		if endpoint != "" {
			remove[endpoint] = true
		}
	}
	m.mu.Lock()
	filtered := make([]Subscription, 0, len(m.subscriptions))
	removed := make([]Subscription, 0)
	for _, subscription := range m.subscriptions {
		match := subscription.DeviceID == deviceID &&
			(remove[subscription.Endpoint] || (clientID != "" && subscription.ClientID == clientID))
		if match {
			removed = append(removed, subscription)
			continue
		}
		filtered = append(filtered, subscription)
	}
	if err := m.persist(filtered); err != nil {
		m.mu.Unlock()
		return err
	}
	m.subscriptions = filtered
	m.mu.Unlock()
	if len(removed) == 0 {
		return nil
	}
	removedEndpoints := make([]string, 0, len(removed))
	for _, subscription := range removed {
		removedEndpoints = append(removedEndpoints, subscription.Endpoint)
	}
	return m.queue.removeSubscriptions(deviceID, removedEndpoints)
}
func (m *Manager) RemoveDevice(deviceID string) error {
	if strings.TrimSpace(deviceID) == "" {
		return errors.New("push device is required")
	}
	m.mu.Lock()
	filtered := make([]Subscription, 0, len(m.subscriptions))
	for _, subscription := range m.subscriptions {
		if subscription.DeviceID != deviceID {
			filtered = append(filtered, subscription)
		}
	}
	if err := m.persist(filtered); err != nil {
		m.mu.Unlock()
		return err
	}
	m.subscriptions = filtered
	for key := range m.active {
		if key.DeviceID == deviceID {
			delete(m.active, key)
		}
	}
	for key := range m.retracting {
		if key.DeviceID == deviceID {
			delete(m.retracting, key)
		}
	}
	m.mu.Unlock()
	if err := m.queue.removeDevice(deviceID); err != nil {
		return err
	}
	return m.policy.RemoveDevice(deviceID)
}

func (m *Manager) Subscriptions() []Subscription {
	m.mu.Lock()
	defer m.mu.Unlock()
	result := make([]Subscription, len(m.subscriptions))
	copy(result, m.subscriptions)
	return result
}

func (m *Manager) VAPIDPublicKey() string {
	return m.vapidPublic
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

func (m *Manager) ResolvePane(ctx context.Context, target protocol.TargetRef, exceptEventID string) error {
	m.mu.Lock()
	keys := make([]PushEventKey, 0, len(m.active))
	for key := range m.active {
		if key.EventID != exceptEventID && sameTarget(key.Target(), target) {
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

func (m *Manager) Send(ctx context.Context, payload []byte) {
	subs := m.Subscriptions()
	if len(subs) == 0 {
		return
	}

	finished := payloadType(payload) == "finished"
	var toRemove []string
	for _, sub := range subs {
		if finished && !sub.NotifyFinished {
			continue
		}
		if err := m.sendPush(ctx, sub, payload); err != nil {
			m.logger.Warn("push send failed", "endpoint", truncateEndpoint(sub.Endpoint), "error", err)
			if isTerminalError(err) {
				toRemove = append(toRemove, sub.Endpoint)
			}
		}
	}

	if len(toRemove) > 0 {
		if err := m.Unsubscribe(toRemove, ""); err != nil {
			m.logger.Warn("failed to persist pruned push subscriptions", "error", err)
		}
		m.logger.Info("pruned dead push subscriptions", "count", len(toRemove))
	}
}

func payloadType(payload []byte) string {
	var envelope struct {
		Tag  string `json:"tag"`
		Data struct {
			Type string `json:"type"`
		} `json:"data"`
	}
	_ = json.Unmarshal(payload, &envelope)
	if envelope.Data.Type == "" && strings.HasPrefix(envelope.Tag, "herdr-finished-") {
		return "finished"
	}
	return envelope.Data.Type
}

func atomicWrite(filename string, data []byte, mode os.FileMode) error {
	directory := filepath.Dir(filename)
	temp, err := os.CreateTemp(directory, "."+filepath.Base(filename)+".")
	if err != nil {
		return err
	}
	tempName := temp.Name()
	defer os.Remove(tempName)
	if err := temp.Chmod(mode); err != nil {
		temp.Close()
		return err
	}
	if _, err := temp.Write(data); err != nil {
		temp.Close()
		return err
	}
	if err := temp.Sync(); err != nil {
		temp.Close()
		return err
	}
	if err := temp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tempName, filename); err != nil {
		return err
	}
	parent, err := os.Open(directory)
	if err == nil {
		err = parent.Sync()
		_ = parent.Close()
	}
	return err
}

func (m *Manager) sendOne(ctx context.Context, sub Subscription, payload []byte) error {
	wpSub := &webpush.Subscription{
		Endpoint: sub.Endpoint,
		Keys: webpush.Keys{
			P256dh: sub.Keys.P256dh,
			Auth:   sub.Keys.Auth,
		},
	}

	resp, err := webpush.SendNotificationWithContext(ctx, payload, wpSub, &webpush.Options{
		HTTPClient:      m.httpClient,
		Subscriber:      "https://github.com/0cv/herdr-mobile-relay",
		VAPIDPublicKey:  m.vapidPublic,
		VAPIDPrivateKey: m.vapidPrivate,
		TTL:             300,
	})
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return nil
	}

	return &pushError{statusCode: resp.StatusCode}
}

type pushError struct {
	statusCode int
}

func (e *pushError) Error() string {
	return fmt.Sprintf("push service returned %d", e.statusCode)
}

// isTerminalError returns true only when the push endpoint is permanently gone.
// Authentication failures can be caused by relay-side VAPID configuration and
// must not destroy an otherwise valid subscription.
func isTerminalError(err error) bool {
	var pe *pushError
	if ok := asPushError(err, &pe); ok {
		switch pe.statusCode {
		case http.StatusNotFound, http.StatusGone:
			return true
		}
	}
	return false
}

func asPushError(err error, target **pushError) bool {
	for err != nil {
		if pe, ok := err.(*pushError); ok {
			*target = pe
			return true
		}
		type unwrapper interface{ Unwrap() error }
		u, ok := err.(unwrapper)
		if !ok {
			return false
		}
		err = u.Unwrap()
	}
	return false
}

func truncateEndpoint(endpoint string) string {
	if len(endpoint) > 60 {
		return endpoint[:60] + "..."
	}
	return endpoint
}

func BuildBlockedPayload(agent, project, command, eventID, paneID, host string, hasApproval bool, approvalTotal int) []byte {
	kind := "question"
	if hasApproval {
		kind = "approval"
	}
	return BuildAttentionPayload(agent, project, command, eventID, paneID, host, kind, approvalTotal)
}

func BuildAttentionPayload(
	agent, project, command, eventID, paneID, host, kind string,
	approvalTotal int,
) []byte {
	notifyURL := notificationURL(host, paneID, eventID, "", 0, 0)

	if project == "" {
		project = agent
	}
	if project == "" {
		project = "agent"
	}
	if command == "" {
		command = "Agent needs inspection"
	}
	title := fmt.Sprintf("%s needs inspection", project)
	body := fmt.Sprintf("%s · %s", command, host)
	if kind == "approval" && approvalTotal >= 2 {
		title = fmt.Sprintf("%s blocked", project)
	} else if kind == "question" {
		title = fmt.Sprintf("%s needs answers", project)
	}

	payload := map[string]any{
		"title":       title,
		"body":        body,
		"tag":         fmt.Sprintf("herdr-%s-%s", host, paneID),
		"url":         notifyURL,
		"actions":     []any{},
		"action_urls": map[string]string{},
	}
	if kind == "approval" && approvalTotal >= 2 {
		payload["actions"] = []map[string]any{
			{"action": "approve", "title": "Approve once"},
		}
		payload["action_urls"] = map[string]string{
			"approve": notificationURL(host, paneID, eventID, "approve", 0, approvalTotal),
		}
	}
	data, _ := json.Marshal(payload)
	return data
}

func BuildFinishedPayload(agent, project, paneID, host, eventID string) []byte {
	notifyURL := notificationURL(host, paneID, eventID, "", 0, 0)

	if project == "" {
		project = agent
	}
	if project == "" {
		project = "Agent"
	}
	if agent == "" {
		agent = "Agent"
	}
	payload := map[string]any{
		"title":       fmt.Sprintf("%s finished", project),
		"body":        fmt.Sprintf("%s completed · %s", agent, host),
		"tag":         fmt.Sprintf("herdr-finished-%s-%s", host, paneID),
		"url":         notifyURL,
		"actions":     []any{},
		"action_urls": map[string]string{},
	}
	data, _ := json.Marshal(payload)
	return data
}

func notificationURL(host, paneID, eventID, action string, index, total int) string {
	target := struct {
		Host           string `json:"host"`
		PaneID         string `json:"pane_id"`
		NotificationID string `json:"notification_id"`
		Action         string `json:"action,omitempty"`
		Index          *int   `json:"index,omitempty"`
		Total          *int   `json:"total,omitempty"`
	}{
		Host:           host,
		PaneID:         paneID,
		NotificationID: eventID,
		Action:         action,
	}
	if action != "" {
		target.Index = &index
		target.Total = &total
	}
	encoded, _ := json.Marshal(target)
	return "./#notify=" + url.QueryEscape(string(encoded))
}
