package push

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"log/slog"
	"math/big"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/0cv/herdr-mobile-relay/internal/protocol"
)

func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
}

func TestNewManagerGeneratesVAPIDKeys(t *testing.T) {
	dir := t.TempDir()
	m, err := NewManager(dir, testLogger())
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}

	if m.VAPIDPublicKey() == "" {
		t.Error("VAPID public key is empty")
	}

	// Keys should be persisted
	if _, err := os.Stat(filepath.Join(dir, "vapid_private.pem")); err != nil {
		t.Error("private key file not created")
	}
	if _, err := os.Stat(filepath.Join(dir, "vapid_public.pem")); err != nil {
		t.Error("public key file not created")
	}
	privatePEM, err := os.ReadFile(filepath.Join(dir, "vapid_private.pem"))
	if err != nil {
		t.Fatal(err)
	}
	block, _ := pem.Decode(privatePEM)
	if block == nil || block.Type != "PRIVATE KEY" {
		t.Fatalf("private key is not PKCS#8 PEM: %q", privatePEM)
	}
	if _, err := x509.ParsePKCS8PrivateKey(block.Bytes); err != nil {
		t.Fatalf("parse private key: %v", err)
	}
}

func TestNewManagerLoadsExistingKeys(t *testing.T) {
	dir := t.TempDir()
	m1, _ := NewManager(dir, testLogger())
	key1 := m1.VAPIDPublicKey()

	m2, _ := NewManager(dir, testLogger())
	key2 := m2.VAPIDPublicKey()

	if key1 != key2 {
		t.Error("keys should be stable across restarts")
	}
}

func TestNewManagerDerivesMissingPythonPublicKey(t *testing.T) {
	dir := t.TempDir()
	privatePath := filepath.Join(dir, "vapid_private.pem")
	publicPath := filepath.Join(dir, "vapid_public.pem")
	subscriptionsPath := filepath.Join(dir, "subscriptions.json")

	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	privateDER, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	privatePEM := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: privateDER})
	subscriptions := []byte(`{"subscriptions":[{"subscription":{"endpoint":"https://fcm.googleapis.com/legacy","keys":{"p256dh":"legacy-key","auth":"legacy-auth"}},"client_id":"phone","notify_finished":true}]}`)
	if err := os.WriteFile(privatePath, privatePEM, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(subscriptionsPath, subscriptions, 0o600); err != nil {
		t.Fatal(err)
	}

	manager, err := NewManager(dir, testLogger())
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	wantPublic := encodeVAPIDPublic(&key.PublicKey)
	if got := manager.VAPIDPublicKey(); got == "" || got != wantPublic {
		t.Fatalf("VAPID public key = %q, want derived key %q", got, wantPublic)
	}
	assertFileContents(t, privatePath, privatePEM)
	assertFileContents(t, subscriptionsPath, subscriptions)

	publicPEM, err := os.ReadFile(publicPath)
	if err != nil {
		t.Fatal(err)
	}
	parsedPublic, err := parseVAPIDPublicKey(string(publicPEM))
	if err != nil {
		t.Fatalf("parse derived public key: %v", err)
	}
	if encodeVAPIDPublic(parsedPublic) != wantPublic {
		t.Fatalf("persisted public key = %q, want %q", encodeVAPIDPublic(parsedPublic), wantPublic)
	}
	if info, err := os.Stat(privatePath); err != nil {
		t.Fatal(err)
	} else if info.Mode().Perm() != 0o600 {
		t.Fatalf("private key mode = %o, want 600", info.Mode().Perm())
	}
	if info, err := os.Stat(publicPath); err != nil {
		t.Fatal(err)
	} else if info.Mode().Perm() != 0o644 {
		t.Fatalf("public key mode = %o, want 644", info.Mode().Perm())
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), ".vapid_public.pem.") {
			t.Fatalf("atomic write left temporary file %q", entry.Name())
		}
	}

	restarted, err := NewManager(dir, testLogger())
	if err != nil {
		t.Fatalf("restart NewManager: %v", err)
	}
	if restarted.VAPIDPublicKey() != wantPublic {
		t.Fatalf("public key changed across restart: got %q, want %q", restarted.VAPIDPublicKey(), wantPublic)
	}
	assertFileContents(t, privatePath, privatePEM)
	assertFileContents(t, subscriptionsPath, subscriptions)
}

func TestNewManagerRejectsUnsafeVAPIDLayouts(t *testing.T) {
	t.Run("public only", func(t *testing.T) {
		dir := t.TempDir()
		_, publicPEM := testVAPIDKeyPair(t)
		if err := os.WriteFile(filepath.Join(dir, "vapid_public.pem"), publicPEM, 0o644); err != nil {
			t.Fatal(err)
		}
		if _, err := NewManager(dir, testLogger()); err == nil || !strings.Contains(err.Error(), "private key is missing") {
			t.Fatalf("NewManager error = %v, want missing-private error", err)
		}
	})

	t.Run("mismatched pair", func(t *testing.T) {
		dir := t.TempDir()
		privatePEM, _ := testVAPIDKeyPair(t)
		_, publicPEM := testVAPIDKeyPair(t)
		if err := os.WriteFile(filepath.Join(dir, "vapid_private.pem"), privatePEM, 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "vapid_public.pem"), publicPEM, 0o644); err != nil {
			t.Fatal(err)
		}
		if _, err := NewManager(dir, testLogger()); err == nil || !strings.Contains(err.Error(), "does not match") {
			t.Fatalf("NewManager error = %v, want mismatch error", err)
		}
	})
}

func testVAPIDKeyPair(t *testing.T) ([]byte, []byte) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	privateDER, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	publicDER, err := x509.MarshalPKIXPublicKey(&key.PublicKey)
	if err != nil {
		t.Fatal(err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: privateDER}),
		pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: publicDER})
}

func assertFileContents(t *testing.T, path string, want []byte) {
	t.Helper()
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("%s contents changed", filepath.Base(path))
	}
}

func TestSubscribeAndUnsubscribe(t *testing.T) {
	dir := t.TempDir()
	m, _ := NewManager(dir, testLogger())

	sub := Subscription{Endpoint: "https://fcm.googleapis.com/ep1", DeviceID: "device-a"}
	sub.Keys.P256dh = "key1"
	sub.Keys.Auth = "auth1"
	m.Subscribe(sub)

	subs := m.Subscriptions()
	if len(subs) != 1 {
		t.Fatalf("subs = %d, want 1", len(subs))
	}
	if subs[0].Endpoint != "https://fcm.googleapis.com/ep1" {
		t.Errorf("endpoint = %q", subs[0].Endpoint)
	}

	// Subscribe same endpoint updates
	sub2 := Subscription{Endpoint: "https://fcm.googleapis.com/ep1", DeviceID: "device-a"}
	sub2.Keys.P256dh = "key2"
	sub2.Keys.Auth = "auth2"
	m.Subscribe(sub2)

	subs = m.Subscriptions()
	if len(subs) != 1 {
		t.Fatalf("after update: subs = %d, want 1", len(subs))
	}
	if subs[0].Keys.P256dh != "key2" || subs[0].Keys.Auth != "auth2" {
		t.Fatalf("subscription keys were not updated: %+v", subs[0].Keys)
	}

	// Unsubscribe
	if err := m.UnsubscribeDevice("device-a", []string{"https://fcm.googleapis.com/ep1"}, ""); err != nil {
		t.Fatal(err)
	}
	subs = m.Subscriptions()
	if len(subs) != 0 {
		t.Fatalf("after unsubscribe: subs = %d, want 0", len(subs))
	}
}

func TestSubscribeRejectsNonPushEndpoints(t *testing.T) {
	manager, err := NewManager(t.TempDir(), testLogger())
	if err != nil {
		t.Fatal(err)
	}
	for _, endpoint := range []string{
		"http://fcm.googleapis.com/send",
		"https://127.0.0.1/push",
		"https://internal.example.test/push",
		"https://fcm.googleapis.com:8443/send",
	} {
		subscription := Subscription{Endpoint: endpoint}
		subscription.Keys.P256dh = "key"
		subscription.Keys.Auth = "auth"
		if err := manager.Subscribe(subscription); err == nil {
			t.Fatalf("Subscribe(%q) succeeded", endpoint)
		}
	}
}

func TestSubscriptionsPersist(t *testing.T) {
	dir := t.TempDir()
	m1, _ := NewManager(dir, testLogger())

	sub := Subscription{Endpoint: "https://fcm.googleapis.com/persist"}
	sub.Keys.P256dh = "k"
	sub.Keys.Auth = "a"
	m1.Subscribe(sub)

	m2, _ := NewManager(dir, testLogger())
	subs := m2.Subscriptions()
	if len(subs) != 1 || subs[0].Endpoint != "https://fcm.googleapis.com/persist" {
		t.Error("subscriptions should persist across restarts")
	}
}
func TestWaitingTopic(t *testing.T) {
	path := filepath.Join(t.TempDir(), "signer.key")
	secret, err := hex.DecodeString("000102030405060708090a0b0c0d0e0f101112131415161718191a1b1c1d1e1f")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, secret, 0o600); err != nil {
		t.Fatal(err)
	}
	signer, err := loadOrCreateReferenceSigner(path)
	if err != nil {
		t.Fatal(err)
	}
	key := PushEventKey{DeviceID: "device-1", ServerSessionID: "primary", PaneID: "pane-1", Category: CategoryAttention}
	topic := signer.waitingTopic(key)
	if topic != "bnVaZyJfb4BZzSyx372-lKi0q9qpIxQz" || !regexp.MustCompile(`^[A-Za-z0-9_-]{32}$`).MatchString(topic) {
		t.Fatalf("topic = %q", topic)
	}
	revision := key
	revision.EventID = "another-event"
	revision.InteractionRevision++
	revision.Category = CategoryQuestion
	if signer.waitingTopic(revision) != topic {
		t.Fatal("event, revision or waiting category changed the topic")
	}
	device, session, pane := key, key, key
	device.DeviceID = "another-device"
	session.ServerSessionID = "another-session"
	pane.PaneID = "another-pane"
	for _, different := range []PushEventKey{device, session, pane} {
		if signer.waitingTopic(different) == topic {
			t.Fatalf("scope change did not change topic: %#v", different)
		}
	}
	other := &ReferenceSigner{key: make([]byte, 32)}
	if other.waitingTopic(key) == topic {
		t.Fatal("different signer did not change the topic")
	}
	recovered, err := loadOrCreateReferenceSigner(path)
	if err != nil || recovered.waitingTopic(key) != topic {
		t.Fatalf("reloaded signer changed topic: %v", err)
	}
}

type pushTestTransport func(*http.Request) (*http.Response, error)

func (f pushTestTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	return f(r)
}

func TestWaitingDeliveryHTTPHeaders(t *testing.T) {
	manager, dir := newDeliveringManager(t, endpointOne)
	policy := manager.Policy("device-1", "en")
	for _, category := range []Category{CategoryFinished, CategoryBrief, CategoryUpdate, CategoryTest} {
		policy.Categories[category] = true
	}
	if err := manager.SetPolicy(policy); err != nil {
		t.Fatal(err)
	}
	sub := manager.Subscriptions()[0]
	keyPair, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	sub.Keys.P256dh = base64.RawURLEncoding.EncodeToString(elliptic.Marshal(elliptic.P256(), keyPair.X, keyPair.Y))
	sub.Keys.Auth = base64.RawURLEncoding.EncodeToString(make([]byte, 16))
	if err := manager.Subscribe(sub); err != nil {
		t.Fatal(err)
	}

	var requests []*http.Request
	client := &http.Client{Transport: pushTestTransport(func(r *http.Request) (*http.Response, error) {
		requests = append(requests, r.Clone(r.Context()))
		return &http.Response{StatusCode: http.StatusCreated, Body: http.NoBody}, nil
	})}
	now := deliveryNow.Add(10 * time.Second)
	setup := func(m *Manager) {
		m.now = func() time.Time { return now }
		m.httpClient = client
	}
	setup(manager)
	check := func(ttl, topic string, endpoints ...string) {
		t.Helper()
		if _, err := manager.RunOnce(t.Context()); err != nil {
			t.Fatal(err)
		}
		if len(requests) != len(endpoints) {
			t.Fatalf("HTTP calls = %d, want %d", len(requests), len(endpoints))
		}
		seen := make(map[string]bool)
		for _, r := range requests {
			seen[r.URL.String()] = true
			if r.Header.Get("TTL") != ttl || r.Header.Get("Topic") != topic || r.Header.Values("Urgency") != nil {
				t.Fatalf("TTL/Topic/Urgency = %q/%q/%q, want %q/%q/unset",
					r.Header.Get("TTL"), r.Header.Get("Topic"), r.Header.Get("Urgency"), ttl, topic)
			}
		}
		for _, endpoint := range endpoints {
			if !seen[endpoint] {
				t.Fatalf("missing HTTP call to %s", endpoint)
			}
		}
		requests = nil
	}

	a := exactKey("approval")
	a.Category = CategoryAttention
	applyWaiting(t, manager, a, deliveryNow)
	second := sub
	second.Endpoint = endpointTwo
	if err := manager.Subscribe(second); err != nil {
		t.Fatal(err)
	}
	entry := manager.queue.state.Entries[deliveryID(a, sub.Endpoint)]
	if _, err := manager.queue.enqueue(entry.Event, second, entry.DueAt); err != nil {
		t.Fatal(err)
	}
	topic := manager.signer.waitingTopic(a)
	check("86390", topic, sub.Endpoint, second.Endpoint)
	for range 2 {
		if err := manager.Resolve(t.Context(), a); err != nil {
			t.Fatal(err)
		}
	}
	check("300", topic, sub.Endpoint, second.Endpoint)
	check("", "")

	b := a
	b.Category, b.EventID = CategoryQuestion, "question"
	applyWaiting(t, manager, b, deliveryNow)
	check("86390", topic, second.Endpoint)
	manager, err = NewManager(dir, testLogger())
	if err != nil {
		t.Fatal(err)
	}
	setup(manager)
	if err := manager.Reconcile(t.Context(), func(PushEventKey) bool { return false }); err != nil {
		t.Fatal(err)
	}
	check("300", topic, second.Endpoint)
	b.InteractionRevision++
	applyWaiting(t, manager, b, now)
	check("86400", topic, second.Endpoint)

	unsent := exactKey("unsent")
	unsent.PaneID = "other-pane"
	applyWaiting(t, manager, unsent, now)
	if err := manager.Resolve(t.Context(), unsent); err != nil {
		t.Fatal(err)
	}
	check("", "")

	for _, category := range []Category{CategoryFinished, CategoryBrief, CategoryUpdate, CategoryTest} {
		key := exactKey(string(category))
		key.Category = category
		if result, err := manager.Publish(t.Context(), PublishRequest{Key: key, Preview: PreviewHidden}); err != nil || result.Queued != 1 {
			t.Fatalf("publish %s = %#v, %v", category, result, err)
		}
		check("300", "", second.Endpoint)
	}
}

func TestSendOneUsesRoutableVAPIDSubject(t *testing.T) {
	var authorization string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		authorization = r.Header.Get("Authorization")
		w.WriteHeader(http.StatusCreated)
	}))
	defer server.Close()

	manager, err := NewManager(t.TempDir(), testLogger())
	if err != nil {
		t.Fatal(err)
	}
	manager.httpClient = server.Client()

	subscriptionKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	authSecret := make([]byte, 16)
	if _, err := rand.Read(authSecret); err != nil {
		t.Fatal(err)
	}
	sub := Subscription{Endpoint: server.URL}
	sub.Keys.P256dh = base64.RawURLEncoding.EncodeToString(elliptic.Marshal(
		elliptic.P256(),
		subscriptionKey.X,
		subscriptionKey.Y,
	))
	sub.Keys.Auth = base64.RawURLEncoding.EncodeToString(authSecret)

	if err := manager.sendOne(t.Context(), sub, []byte("test"), sendOptions{TTL: 300}); err != nil {
		t.Fatal(err)
	}

	const prefix = "vapid t="
	if !strings.HasPrefix(authorization, prefix) {
		t.Fatalf("Authorization = %q, want VAPID token", authorization)
	}
	token := strings.TrimPrefix(strings.SplitN(authorization, ",", 2)[0], prefix)
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		t.Fatalf("VAPID token has %d parts, want 3", len(parts))
	}
	claimsJSON, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		t.Fatal(err)
	}
	var claims struct {
		Subject string `json:"sub"`
	}
	if err := json.Unmarshal(claimsJSON, &claims); err != nil {
		t.Fatal(err)
	}
	if want := "https://github.com/0cv/herdr-mobile-relay"; claims.Subject != want {
		t.Fatalf("VAPID subject = %q, want %q", claims.Subject, want)
	}
}

func TestIsTerminalError(t *testing.T) {
	if !isTerminalError(&pushError{statusCode: 410}) {
		t.Error("410 should be terminal")
	}
	if !isTerminalError(&pushError{statusCode: 404}) {
		t.Error("404 should be terminal")
	}
	if isTerminalError(&pushError{statusCode: 401}) {
		t.Error("401 should not be terminal")
	}
	if isTerminalError(&pushError{statusCode: 403}) {
		t.Error("403 should not be terminal")
	}
	if isTerminalError(&pushError{statusCode: 503}) {
		t.Error("503 should not be terminal")
	}
}

func TestParseVAPIDPrivatePadsScalarTo32Bytes(t *testing.T) {
	key := &ecdsa.PrivateKey{
		PublicKey: ecdsa.PublicKey{Curve: elliptic.P256()},
		D:         new(big.Int).SetInt64(1),
	}
	key.PublicKey.X, key.PublicKey.Y = key.Curve.ScalarBaseMult(key.D.Bytes())
	encoded, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := parseVAPIDPrivateKey(string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: encoded})))
	if err != nil {
		t.Fatal(err)
	}
	scalar, err := base64.RawURLEncoding.DecodeString(encodeVAPIDPrivate(parsed))
	if err != nil {
		t.Fatal(err)
	}
	if len(scalar) != 32 || scalar[31] != 1 {
		t.Fatalf("scalar = %x", scalar)
	}
}

func typedTestSubscription(deviceID, endpoint string) Subscription {
	var subscription Subscription
	subscription.DeviceID = deviceID
	subscription.Endpoint = endpoint
	subscription.Keys.P256dh = "p256dh"
	subscription.Keys.Auth = "auth"
	subscription.Locale = "en"
	subscription.Platform = PlatformOther
	return subscription
}

func typedTestKey(deviceID string) PushEventKey {
	return PushEventKey{
		DeviceID:            deviceID,
		ServerSessionID:     "primary",
		PaneID:              "pane-1",
		TerminalID:          "terminal-1",
		Generation:          0,
		EventID:             "event-1",
		InteractionRevision: 1,
		Category:            CategoryQuestion,
	}
}

func TestManagerPublishRetriesGenerationZero(t *testing.T) {
	manager, err := NewManager(t.TempDir(), testLogger())
	if err != nil {
		t.Fatal(err)
	}
	if err := manager.Subscribe(typedTestSubscription("device-1", "https://fcm.googleapis.com/one")); err != nil {
		t.Fatal(err)
	}
	policy := DefaultDevicePolicy("device-1", "en")
	policy.Settle = 0
	policy.Cooldown = 0
	if err := manager.SetPolicy(policy); err != nil {
		t.Fatal(err)
	}
	attempts := 0
	manager.sendPush = func(_ context.Context, _ Subscription, payload []byte, _ sendOptions) error {
		attempts++
		var typed Payload
		if err := json.Unmarshal(payload, &typed); err != nil || typed.Version != 1 || typed.Key.Generation != 0 {
			t.Fatalf("typed payload = %#v, error = %v", typed, err)
		}
		if attempts == 1 {
			return &pushError{statusCode: http.StatusServiceUnavailable}
		}
		return nil
	}
	now := time.Date(2026, time.August, 31, 12, 0, 0, 0, time.UTC)
	key := typedTestKey("")
	published, err := manager.Publish(t.Context(), PublishRequest{
		Key: key, Preview: PreviewQuestion, CreatedAt: now, ExpiresAt: now.Add(time.Minute),
	})
	if err != nil || published.Queued != 1 {
		t.Fatalf("Publish() = %#v, %v", published, err)
	}
	results, err := runAt(t.Context(), manager, now)
	if err != nil || len(results) != 1 || results[0].Disposition != DeliveryRetrying {
		t.Fatalf("first RunOnce() = %#v, %v", results, err)
	}
	results, err = runAt(t.Context(), manager, results[0].NextAttempt)
	if err != nil || len(results) != 1 || results[0].Disposition != DeliveryAccepted {
		t.Fatalf("second RunOnce() = %#v, %v", results, err)
	}
}

func TestManagerResolveRetractsExactAcceptedEvent(t *testing.T) {
	manager, err := NewManager(t.TempDir(), testLogger())
	if err != nil {
		t.Fatal(err)
	}
	if err := manager.Subscribe(typedTestSubscription("device-1", "https://fcm.googleapis.com/one")); err != nil {
		t.Fatal(err)
	}
	policy := DefaultDevicePolicy("device-1", "en")
	policy.Settle = 0
	if err := manager.SetPolicy(policy); err != nil {
		t.Fatal(err)
	}
	var sent []Payload
	manager.sendPush = func(_ context.Context, _ Subscription, payload []byte, _ sendOptions) error {
		var typed Payload
		if err := json.Unmarshal(payload, &typed); err != nil {
			return err
		}
		sent = append(sent, typed)
		return nil
	}
	now := time.Date(2026, time.August, 31, 12, 0, 0, 0, time.UTC)
	key := typedTestKey("")
	if _, err := manager.Publish(t.Context(), PublishRequest{
		Key: key, Preview: PreviewQuestion, CreatedAt: now, ExpiresAt: now.Add(time.Minute),
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := runAt(t.Context(), manager, now); err != nil {
		t.Fatal(err)
	}
	exactKey := typedTestKey("device-1")
	if err := manager.Resolve(t.Context(), exactKey); err != nil {
		t.Fatal(err)
	}
	if _, err := runAt(t.Context(), manager, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	if len(sent) != 2 || sent[0].Key != exactKey || sent[1].Key != exactKey || !sent[1].Retract {
		t.Fatalf("sent payloads = %#v", sent)
	}
}

func TestRecoveredManagerWaitsForAuthoritativeReconcile(t *testing.T) {
	dir := t.TempDir()
	first, err := NewManager(dir, testLogger())
	if err != nil {
		t.Fatal(err)
	}
	if err := first.Subscribe(typedTestSubscription("device-1", "https://fcm.googleapis.com/one")); err != nil {
		t.Fatal(err)
	}
	policy := DefaultDevicePolicy("device-1", "en")
	policy.Settle = 0
	if err := first.SetPolicy(policy); err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, time.August, 31, 12, 0, 0, 0, time.UTC)
	if _, err := first.Publish(t.Context(), PublishRequest{
		Key: typedTestKey(""), Preview: PreviewQuestion, CreatedAt: now, ExpiresAt: now.Add(time.Hour),
	}); err != nil {
		t.Fatal(err)
	}
	recovered, err := NewManager(dir, testLogger())
	if err != nil {
		t.Fatal(err)
	}
	sends := 0
	recovered.sendPush = func(context.Context, Subscription, []byte, sendOptions) error {
		sends++
		return nil
	}
	if results, err := runAt(t.Context(), recovered, now); err != nil || len(results) != 0 || sends != 0 {
		t.Fatalf("pre-reconcile RunOnce() = %#v, %v, sends %d", results, err, sends)
	}
	current := typedTestKey("device-1")
	if err := recovered.Reconcile(t.Context(), func(key PushEventKey) bool { return key == current }); err != nil {
		t.Fatal(err)
	}
	if results, err := runAt(t.Context(), recovered, now); err != nil || len(results) != 1 || sends != 1 {
		t.Fatalf("post-reconcile RunOnce() = %#v, %v, sends %d", results, err, sends)
	}
}

func TestTimedSnoozeExpiresWithoutClearingTheStoredPreference(t *testing.T) {
	engine, err := newPolicyEngine(filepath.Join(t.TempDir(), "policy.json"))
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, time.August, 31, 12, 0, 0, 0, time.UTC)
	policy := DefaultDevicePolicy("device-1", "en")
	policy.Snoozed = true
	policy.SnoozeUntil = now.Add(time.Minute)
	if err := engine.Set(policy); err != nil {
		t.Fatal(err)
	}
	key := typedTestKey("device-1")
	if decision := engine.Decide(key, "en", now); decision.Code != "push_snoozed" {
		t.Fatalf("active snooze decision = %#v", decision)
	}
	if decision := engine.Decide(key, "en", now.Add(time.Minute)); !decision.Deliver {
		t.Fatalf("expired snooze decision = %#v", decision)
	}
}

func TestRunOnceRechecksLivePolicyBeforeDelivery(t *testing.T) {
	manager, err := NewManager(t.TempDir(), testLogger())
	if err != nil {
		t.Fatal(err)
	}
	if err := manager.Subscribe(typedTestSubscription("device-1", "https://fcm.googleapis.com/one")); err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, time.August, 31, 12, 0, 0, 0, time.UTC)
	policy := DefaultDevicePolicy("device-1", "en")
	policy.Settle = time.Minute
	if err := manager.SetPolicy(policy); err != nil {
		t.Fatal(err)
	}
	key := typedTestKey("")
	if _, err := manager.Publish(t.Context(), PublishRequest{
		Key: key, Preview: PreviewQuestion, CreatedAt: now, ExpiresAt: now.Add(time.Hour),
	}); err != nil {
		t.Fatal(err)
	}
	policy.Snoozed = true
	policy.SnoozeUntil = now.Add(time.Hour)
	if err := manager.SetPolicy(policy); err != nil {
		t.Fatal(err)
	}
	sends := 0
	manager.sendPush = func(context.Context, Subscription, []byte, sendOptions) error {
		sends++
		return nil
	}
	results, err := runAt(t.Context(), manager, now.Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if sends != 0 || len(results) != 1 || results[0].Disposition != DeliveryStale {
		t.Fatalf("policy-rechecked delivery = %#v, sends %d", results, sends)
	}
}

func TestPushEventRejectsPayloadBeyondServiceLimit(t *testing.T) {
	now := time.Date(2026, time.September, 2, 13, 0, 0, 0, time.UTC)
	event := PushEvent{
		Key: typedTestKey("device-1"), Payload: make([]byte, maxPushPayloadBytes),
		CreatedAt: now, ExpiresAt: now.Add(time.Minute),
	}
	if err := event.Validate(now); err != nil {
		t.Fatalf("maximum push payload rejected: %v", err)
	}
	event.Payload = append(event.Payload, 0)
	if err := event.Validate(now); err == nil {
		t.Fatal("oversized push payload was accepted")
	}
}

func TestRemoveDeviceKeepsMemoryWhenPersistenceFails(t *testing.T) {
	manager, err := NewManager(t.TempDir(), testLogger())
	if err != nil {
		t.Fatal(err)
	}
	if err := manager.Subscribe(typedTestSubscription("device-1", "https://fcm.googleapis.com/one")); err != nil {
		t.Fatal(err)
	}
	manager.path = filepath.Join(t.TempDir(), "missing", "subscriptions.json")
	if err := manager.RemoveDevice("device-1"); err == nil {
		t.Fatal("RemoveDevice succeeded with unavailable persistence path")
	}
	if subscriptions := manager.Subscriptions(); len(subscriptions) != 1 || subscriptions[0].DeviceID != "device-1" {
		t.Fatalf("subscriptions after failed removal = %#v", subscriptions)
	}
}

func TestQueueRejectsEntriesPastBound(t *testing.T) {
	queue, err := newDurableQueue(filepath.Join(t.TempDir(), "queue.json"))
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, time.September, 2, 12, 0, 0, 0, time.UTC)
	for index := range maxQueueEntries {
		queue.state.Entries[fmt.Sprintf("existing-%d", index)] = queueEntry{
			ID: fmt.Sprintf("existing-%d", index),
		}
	}
	key := typedTestKey("device-1")
	event := PushEvent{Key: key, Payload: []byte(`{"type":"question"}`), CreatedAt: now, ExpiresAt: now.Add(time.Minute)}
	if _, err := queue.enqueue(event, typedTestSubscription("device-1", "https://fcm.googleapis.com/one"), now); err == nil || err.Error() != "push_queue_limit" {
		t.Fatalf("enqueue error = %v", err)
	}
	if len(queue.state.Entries) != maxQueueEntries {
		t.Fatalf("queue grew to %d entries", len(queue.state.Entries))
	}
}

func TestPushEventKeyBoundsOwnedIdentifiersOnly(t *testing.T) {
	valid := typedTestKey("device-1")
	if err := valid.Validate(); err != nil {
		t.Fatal(err)
	}
	oversized := valid
	oversized.EventID = strings.Repeat("x", 257)
	if err := oversized.Validate(); err == nil {
		t.Fatal("oversized event id was accepted")
	}
	for name, mutate := range map[string]func(*PushEventKey){
		"server session": func(key *PushEventKey) { key.ServerSessionID = strings.Repeat("s", 257) },
		"pane":           func(key *PushEventKey) { key.PaneID = strings.Repeat("p", 257) },
		"terminal":       func(key *PushEventKey) { key.TerminalID = strings.Repeat("t", 257) },
		"agent session":  func(key *PushEventKey) { key.AgentSessionID = strings.Repeat("a", 257) },
	} {
		t.Run(name, func(t *testing.T) {
			longTarget := valid
			mutate(&longTarget)
			if err := longTarget.Validate(); err != nil {
				t.Fatalf("opaque target identifier was rejected: %v", err)
			}
		})
	}
}

func TestFailedPersistLeavesPolicyAndQueueUnchanged(t *testing.T) {
	dir := t.TempDir()
	engine, err := newPolicyEngine(filepath.Join(dir, "policy.json"))
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, time.August, 31, 12, 0, 0, 0, time.UTC)
	key := typedTestKey("device-1")
	snoozed := DefaultDevicePolicy("device-1", "en")
	snoozed.Snoozed = true
	if err := engine.Set(snoozed); err != nil {
		t.Fatal(err)
	}
	if err := engine.MarkAccepted(key, now); err != nil {
		t.Fatal(err)
	}
	target := protocol.TargetRef{ServerSessionID: "session", PaneID: "pane", TerminalID: "terminal"}
	engine.SetViewedPane("device-1", &target)
	engine.path = filepath.Join(dir, "missing", "policy.json")

	if err := engine.Set(DefaultDevicePolicy("device-1", "en")); err == nil {
		t.Fatal("Set() succeeded with a broken path")
	}
	if err := engine.Set(DefaultDevicePolicy("device-2", "en")); err == nil {
		t.Fatal("Set() succeeded with a broken path")
	}
	if err := engine.MarkAccepted(key, now.Add(time.Hour)); err == nil {
		t.Fatal("MarkAccepted() succeeded with a broken path")
	}
	if err := engine.RemoveDevice("device-1"); err == nil {
		t.Fatal("RemoveDevice() succeeded with a broken path")
	}
	if !engine.Get("device-1", "en").Snoozed {
		t.Fatal("failed writes changed the stored policy")
	}
	if got := engine.state.LastAccepted[policySlot(key)]; !got.Equal(now) {
		t.Fatalf("last accepted = %s, want %s", got, now)
	}
	if _, ok := engine.state.Policies["device-2"]; ok {
		t.Fatal("failed Set() stored a new policy")
	}
	if got := engine.viewedPanes["device-1"]; got != target {
		t.Fatalf("viewed pane = %#v, want %#v", got, target)
	}

	queue, err := newDurableQueue(filepath.Join(dir, "queue.json"))
	if err != nil {
		t.Fatal(err)
	}
	event := PushEvent{Key: key, Payload: []byte(`{"type":"question"}`), CreatedAt: now, ExpiresAt: now.Add(time.Minute)}
	subscription := typedTestSubscription("device-1", "https://fcm.googleapis.com/one")
	if _, err := queue.enqueue(event, subscription, now); err != nil {
		t.Fatal(err)
	}
	queue.state.Delivered["delivered"] = deliveredRecord{Key: key, Subscription: subscription}
	queue.path = filepath.Join(dir, "missing", "queue.json")

	other := typedTestKey("device-1")
	other.EventID = "other-event"
	otherEvent := event
	otherEvent.Key = other
	if _, err := queue.enqueue(otherEvent, subscription, now); err == nil {
		t.Fatal("enqueue() succeeded with a broken path")
	}
	next := queue.state.clone()
	next.cancelAlerts(key)
	if err := queue.commitLocked(next); err == nil {
		t.Fatal("committing cancellation succeeded with a broken path")
	}
	if err := queue.removeSubscriptions("device-1", []string{subscription.Endpoint}); err == nil {
		t.Fatal("removeSubscriptions() succeeded with a broken path")
	}
	replacement := typedTestSubscription("device-1", "https://fcm.googleapis.com/two")
	if err := queue.replaceSubscriptionsWhileProcessing("device-1", []string{subscription.Endpoint}, replacement); err == nil {
		t.Fatal("replaceSubscriptionsWhileProcessing() succeeded with a broken path")
	}
	if err := queue.forgetDelivered(key); err == nil {
		t.Fatal("forgetDelivered() succeeded with a broken path")
	}
	if err := queue.removeDevice("device-1"); err == nil {
		t.Fatal("removeDevice() succeeded with a broken path")
	}
	if len(queue.state.Entries) != 1 || len(queue.state.Delivered) != 1 {
		t.Fatalf("failed writes changed the queue: %#v", queue.state)
	}
	for _, entry := range queue.state.Entries {
		if entry.Subscription.Endpoint != subscription.Endpoint || entry.Event.Key != key {
			t.Fatalf("queued entry changed: %#v", entry)
		}
	}
}
