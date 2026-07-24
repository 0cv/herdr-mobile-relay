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
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	webpush "github.com/SherClockHolmes/webpush-go"
)

type Subscription struct {
	Endpoint string `json:"endpoint"`
	Keys     struct {
		P256dh string `json:"p256dh"`
		Auth   string `json:"auth"`
	} `json:"keys"`
	UserAgent      string `json:"user_agent,omitempty"`
	NotifyFinished bool   `json:"notify_finished,omitempty"`
	ClientID       string `json:"client_id,omitempty"`
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
	ClientID       string `json:"client_id"`
	UserAgent      string `json:"user_agent"`
	NotifyFinished bool   `json:"notify_finished"`
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
}

func NewManager(pushDir string, logger *slog.Logger) (*Manager, error) {
	if err := os.MkdirAll(pushDir, 0o700); err != nil {
		return nil, fmt.Errorf("create push dir: %w", err)
	}
	if err := os.Chmod(pushDir, 0o700); err != nil {
		return nil, fmt.Errorf("protect push dir: %w", err)
	}

	m := &Manager{
		path:       filepath.Join(pushDir, "subscriptions.json"),
		logger:     logger,
		httpClient: &http.Client{Timeout: 10 * time.Second},
	}

	if err := m.load(); err != nil {
		return nil, err
	}

	if err := m.loadOrGenerateVAPIDKeys(pushDir); err != nil {
		return nil, err
	}
	m.sendPush = m.sendOne

	return m, nil
}

func (m *Manager) loadOrGenerateVAPIDKeys(pushDir string) error {
	privPath := filepath.Join(pushDir, "vapid_private.pem")
	pubPath := filepath.Join(pushDir, "vapid_public.pem")

	privData, privErr := os.ReadFile(privPath)
	pubData, pubErr := os.ReadFile(pubPath)

	if privErr == nil && pubErr == nil {
		privKey, err := parseVAPIDPrivate(string(privData))
		if err != nil {
			return fmt.Errorf("parse VAPID private key: %w", err)
		}
		pubKey, err := parseVAPIDPublic(string(pubData))
		if err != nil {
			return fmt.Errorf("parse VAPID public key: %w", err)
		}
		m.vapidPrivate = privKey
		m.vapidPublic = pubKey
		if err := os.Chmod(privPath, 0o600); err != nil {
			return err
		}
		return nil
	}
	if !os.IsNotExist(privErr) || !os.IsNotExist(pubErr) {
		return fmt.Errorf("VAPID key pair is incomplete or unreadable")
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

	privateScalar := make([]byte, 32)
	key.D.FillBytes(privateScalar)
	m.vapidPrivate = base64.RawURLEncoding.EncodeToString(privateScalar)
	m.vapidPublic = base64.RawURLEncoding.EncodeToString(elliptic.Marshal(key.Curve, key.X, key.Y))
	m.logger.Info("generated new VAPID key pair")
	return nil
}

// parseVAPIDPrivate handles both PEM-encoded EC private keys (Python format)
// and raw base64url scalars (webpush-go format).
func parseVAPIDPrivate(data string) (string, error) {
	data = strings.TrimSpace(data)
	if !strings.HasPrefix(data, "-----BEGIN") {
		return data, nil
	}

	block, _ := pem.Decode([]byte(data))
	if block == nil {
		return "", fmt.Errorf("invalid PEM block")
	}

	key, err := x509.ParseECPrivateKey(block.Bytes)
	if err != nil {
		pkcs8Key, err2 := x509.ParsePKCS8PrivateKey(block.Bytes)
		if err2 != nil {
			return "", fmt.Errorf("parse EC key: %v (pkcs8: %v)", err, err2)
		}
		ecKey, ok := pkcs8Key.(*ecdsa.PrivateKey)
		if !ok {
			return "", fmt.Errorf("not an EC private key")
		}
		key = ecKey
	}

	privateScalar := make([]byte, 32)
	key.D.FillBytes(privateScalar)
	return base64.RawURLEncoding.EncodeToString(privateScalar), nil
}

// parseVAPIDPublic handles both PEM-encoded EC public keys (Python format)
// and raw base64url points (webpush-go format).
func parseVAPIDPublic(data string) (string, error) {
	data = strings.TrimSpace(data)
	if !strings.HasPrefix(data, "-----BEGIN") {
		return data, nil
	}

	block, _ := pem.Decode([]byte(data))
	if block == nil {
		return "", fmt.Errorf("invalid PEM block")
	}

	pub, err := x509.ParsePKIXPublicKey(block.Bytes)
	if err != nil {
		return "", fmt.Errorf("parse public key: %w", err)
	}

	ecPub, ok := pub.(*ecdsa.PublicKey)
	if !ok {
		return "", fmt.Errorf("not an EC public key")
	}

	point := elliptic.Marshal(ecPub.Curve, ecPub.X, ecPub.Y)
	return base64.RawURLEncoding.EncodeToString(point), nil
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
			sub.UserAgent = ps.UserAgent
			sub.NotifyFinished = ps.NotifyFinished
			sub.ClientID = ps.ClientID
			m.subscriptions = append(m.subscriptions, sub)
		}
		return nil
	}

	// Fallback: flat array (legacy Go format)
	return json.Unmarshal(data, &m.subscriptions)
}

func (m *Manager) persist(subscriptions []Subscription) error {
	pf := pythonFile{Subscriptions: make([]pythonSubscription, 0, len(subscriptions))}
	for _, sub := range subscriptions {
		var ps pythonSubscription
		ps.Subscription.Endpoint = sub.Endpoint
		ps.Subscription.Keys.P256dh = sub.Keys.P256dh
		ps.Subscription.Keys.Auth = sub.Keys.Auth
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
	if sub.Endpoint == "" || sub.Keys.P256dh == "" || sub.Keys.Auth == "" {
		return fmt.Errorf("subscription endpoint and keys are required")
	}
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
		if replace[existing.Endpoint] && existing.Endpoint != sub.Endpoint {
			continue
		}
		filtered = append(filtered, existing)
	}
	for i, existing := range filtered {
		if existing.Endpoint == sub.Endpoint {
			filtered[i] = sub
			if err := m.persist(filtered); err != nil {
				return err
			}
			m.subscriptions = filtered
			return nil
		}
	}
	filtered = append(filtered, sub)
	if err := m.persist(filtered); err != nil {
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
		Subscriber:      "relay@herdr.local",
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
	notifyURL := notificationURL(host, paneID, eventID, "", 0, 0)

	if project == "" {
		project = agent
	}
	if project == "" {
		project = "agent"
	}
	if command == "" {
		command = "Agent needs approval"
	}
	title := fmt.Sprintf("%s blocked", project)
	body := fmt.Sprintf("%s · %s", command, host)
	if !hasApproval {
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
	if hasApproval {
		total := approvalTotal
		if total < 2 {
			total = 2
		}
		payload["actions"] = []map[string]any{
			{"action": "approve", "title": "Approve once"},
		}
		payload["action_urls"] = map[string]string{
			"approve": notificationURL(host, paneID, eventID, "approve", 0, total),
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
