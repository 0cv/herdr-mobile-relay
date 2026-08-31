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
	"log/slog"
	"math/big"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
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
	parsedPublic, err := parseVAPIDPublic(string(publicPEM))
	if err != nil {
		t.Fatalf("parse derived public key: %v", err)
	}
	if parsedPublic != wantPublic {
		t.Fatalf("persisted public key = %q, want %q", parsedPublic, wantPublic)
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

	t.Run("invalid private only", func(t *testing.T) {
		dir := t.TempDir()
		if err := os.WriteFile(filepath.Join(dir, "vapid_private.pem"), []byte("not-a-key\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := NewManager(dir, testLogger()); err == nil || !strings.Contains(err.Error(), "parse VAPID private key") {
			t.Fatalf("NewManager error = %v, want invalid-private error", err)
		}
		if _, err := os.Stat(filepath.Join(dir, "vapid_public.pem")); !os.IsNotExist(err) {
			t.Fatalf("public key created for invalid private key: %v", err)
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

	sub := Subscription{Endpoint: "https://fcm.googleapis.com/ep1"}
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
	sub2 := Subscription{Endpoint: "https://fcm.googleapis.com/ep1", NotifyFinished: true}
	sub2.Keys.P256dh = "key2"
	sub2.Keys.Auth = "auth2"
	m.Subscribe(sub2)

	subs = m.Subscriptions()
	if len(subs) != 1 {
		t.Fatalf("after update: subs = %d, want 1", len(subs))
	}
	if !subs[0].NotifyFinished {
		t.Error("NotifyFinished should be true after update")
	}

	// Unsubscribe
	m.Unsubscribe([]string{"https://fcm.googleapis.com/ep1"})
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
func TestRemoveDeviceClearsPushState(t *testing.T) {
	manager, err := NewManager(t.TempDir(), testLogger())
	if err != nil {
		t.Fatal(err)
	}
	subscription := Subscription{Endpoint: "https://fcm.googleapis.com/device", DeviceID: "device-a"}
	subscription.Keys.P256dh = "key"
	subscription.Keys.Auth = "auth"
	if err := manager.Subscribe(subscription); err != nil {
		t.Fatal(err)
	}
	policy := DefaultDevicePolicy("device-a", "en")
	policy.Categories[CategoryAttention] = false
	if err := manager.policy.Set(policy); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	key := PushEventKey{DeviceID: "device-a", EventID: "event-a", Category: CategoryTest}
	event := PushEvent{Key: key, Payload: []byte(`{\"type\":\"test\"}`), CreatedAt: now, ExpiresAt: now.Add(time.Minute)}
	if _, err := manager.queue.enqueue(event, subscription, now); err != nil {
		t.Fatal(err)
	}
	manager.active[key] = true

	if err := manager.RemoveDevice("device-a"); err != nil {
		t.Fatal(err)
	}
	if subscriptions := manager.Subscriptions(); len(subscriptions) != 0 {
		t.Fatalf("Subscriptions() = %#v", subscriptions)
	}
	if keys := manager.queue.activeKeys(); len(keys) != 0 {
		t.Fatalf("activeKeys() = %#v", keys)
	}
	if !manager.policy.Get("device-a", "en").Categories[CategoryAttention] {
		t.Fatal("device policy was not removed")
	}
	if manager.active[key] {
		t.Fatal("active push state was not removed")
	}
}

func TestBuildBlockedPayload(t *testing.T) {
	payload := BuildBlockedPayload("Claude", "my-project", "rm -rf build-cache", "evt-1", "pane-1", "myhost", true, 3)
	assertPayloadFixture(t, payload, "blocked.json")
	if len(payload) == 0 {
		t.Fatal("payload is empty")
	}
	s := string(payload)
	if !strings.Contains(s, `"title":"my-project blocked"`) {
		t.Errorf("expected blocked project title, payload = %s", s)
	}
	if !strings.Contains(s, "rm -rf build-cache · myhost") {
		t.Errorf("expected command+host in body, payload = %s", s)
	}
	if !strings.Contains(s, `"tag":"herdr-myhost-pane-1"`) {
		t.Errorf("expected stable host/pane tag, payload = %s", s)
	}
	if !strings.Contains(s, "Approve once") {
		t.Errorf("expected Approve once action, payload = %s", s)
	}
	if !strings.Contains(s, `%22total%22%3A3`) {
		t.Errorf("expected total from options in approve URL, payload = %s", s)
	}
}

func TestBuildQuestionPayloadMatchesPythonContract(t *testing.T) {
	payload := BuildBlockedPayload("Claude", "my-project", "Which database?", "evt-1", "pane-1", "myhost", false, 0)
	assertPayloadFixture(t, payload, "question.json")
}

func TestBuildUnknownAttentionPayloadHasNoActions(t *testing.T) {
	payload := BuildAttentionPayload(
		"OpenCode", "my-project", "Agent needs inspection",
		"evt-1", "pane-1", "myhost", "unknown", 3,
	)
	var decoded map[string]any
	if err := json.Unmarshal(payload, &decoded); err != nil {
		t.Fatal(err)
	}
	if decoded["title"] != "my-project needs inspection" {
		t.Fatalf("title = %v", decoded["title"])
	}
	if actions, ok := decoded["actions"].([]any); !ok || len(actions) != 0 {
		t.Fatalf("unknown attention actions = %#v", decoded["actions"])
	}
	actionURLs, ok := decoded["action_urls"].(map[string]any)
	if !ok || len(actionURLs) != 0 {
		t.Fatalf("unknown attention action URLs = %#v", decoded["action_urls"])
	}
}

func TestBuildFinishedPayload(t *testing.T) {
	payload := BuildFinishedPayload("Codex", "app", "pane-1", "myhost", "evt-finished-1")
	assertPayloadFixture(t, payload, "finished.json")
	s := string(payload)
	if !strings.Contains(s, `"title":"app finished"`) {
		t.Errorf("expected project as title, payload = %s", s)
	}
	if !strings.Contains(s, "Codex completed · myhost") {
		t.Errorf("expected agent finished on host in body, payload = %s", s)
	}
	if !strings.Contains(s, `"tag":"herdr-finished-myhost-pane-1"`) {
		t.Errorf("expected distinct finished host/pane tag, payload = %s", s)
	}
	if !strings.Contains(s, "evt-finished-1") {
		t.Errorf("expected event ID in payload = %s", s)
	}
}

func TestFinishedPayloadIsClassifiedForOptInFiltering(t *testing.T) {
	payload := BuildFinishedPayload("Codex", "app", "pane-1", "myhost", "evt-finished-1")
	if kind := payloadType(payload); kind != "finished" {
		t.Fatalf("finished payload type = %q, want finished so non-opted-in subscriptions are skipped", kind)
	}
}

func TestSendFinishedHonorsSubscriptionOptIn(t *testing.T) {
	manager, err := NewManager(t.TempDir(), testLogger())
	if err != nil {
		t.Fatal(err)
	}
	for _, sub := range []Subscription{
		{Endpoint: "https://fcm.googleapis.com/disabled"},
		{Endpoint: "https://fcm.googleapis.com/enabled", NotifyFinished: true},
	} {
		sub.Keys.P256dh = "test-p256dh"
		sub.Keys.Auth = "test-auth"
		if err := manager.Subscribe(sub); err != nil {
			t.Fatal(err)
		}
	}

	var sent []string
	manager.sendPush = func(_ context.Context, sub Subscription, _ []byte) error {
		sent = append(sent, sub.Endpoint)
		return nil
	}
	manager.Send(t.Context(), BuildFinishedPayload("Codex", "app", "pane-1", "myhost", "evt-finished-1"))

	if !reflect.DeepEqual(sent, []string{"https://fcm.googleapis.com/enabled"}) {
		t.Fatalf("finished push endpoints = %v, want only opted-in subscription", sent)
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

	if err := manager.sendOne(t.Context(), sub, []byte("test")); err != nil {
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

func assertPayloadFixture(t *testing.T, actual []byte, name string) {
	t.Helper()
	expected, err := os.ReadFile(filepath.Join("..", "..", "contracts", "fixtures", "push", name))
	if err != nil {
		t.Fatal(err)
	}
	var actualValue, expectedValue any
	if err := json.Unmarshal(actual, &actualValue); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(expected, &expectedValue); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(actualValue, expectedValue) {
		t.Fatalf("payload mismatch\nactual: %s\nexpected: %s", actual, expected)
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
	value, err := parseVAPIDPrivate(string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: encoded})))
	if err != nil {
		t.Fatal(err)
	}
	scalar, err := base64.RawURLEncoding.DecodeString(value)
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
	manager.sendPush = func(_ context.Context, _ Subscription, payload []byte) error {
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
	results, err := manager.RunOnce(t.Context(), now)
	if err != nil || len(results) != 1 || results[0].Disposition != DeliveryRetrying {
		t.Fatalf("first RunOnce() = %#v, %v", results, err)
	}
	results, err = manager.RunOnce(t.Context(), results[0].NextAttempt)
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
	manager.sendPush = func(_ context.Context, _ Subscription, payload []byte) error {
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
	if _, err := manager.RunOnce(t.Context(), now); err != nil {
		t.Fatal(err)
	}
	exactKey := typedTestKey("device-1")
	if err := manager.Resolve(t.Context(), exactKey); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.RunOnce(t.Context(), time.Now().UTC()); err != nil {
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
	recovered.sendPush = func(context.Context, Subscription, []byte) error {
		sends++
		return nil
	}
	if results, err := recovered.RunOnce(t.Context(), now); err != nil || len(results) != 0 || sends != 0 {
		t.Fatalf("pre-reconcile RunOnce() = %#v, %v, sends %d", results, err, sends)
	}
	current := typedTestKey("device-1")
	if err := recovered.Reconcile(t.Context(), []PushEventKey{current}); err != nil {
		t.Fatal(err)
	}
	if results, err := recovered.RunOnce(t.Context(), now); err != nil || len(results) != 1 || sends != 1 {
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

func TestQueueCancellationWaitsForInFlightDelivery(t *testing.T) {
	queue, err := newDurableQueue(filepath.Join(t.TempDir(), "queue.json"))
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, time.August, 31, 12, 0, 0, 0, time.UTC)
	key := typedTestKey("device-1")
	event := PushEvent{Key: key, Payload: []byte(`{"type":"question"}`), CreatedAt: now, ExpiresAt: now.Add(time.Minute)}
	subscription := typedTestSubscription("device-1", "https://fcm.googleapis.com/one")
	if _, err := queue.enqueue(event, subscription, now); err != nil {
		t.Fatal(err)
	}
	started := make(chan struct{})
	release := make(chan struct{})
	processed := make(chan error, 1)
	go func() {
		_, processErr := queue.processDue(t.Context(), now, func(PushEventKey) bool { return true }, func(context.Context, Subscription, []byte) error {
			close(started)
			<-release
			return nil
		}, nil, nil)
		processed <- processErr
	}()
	<-started
	cancelled := make(chan error, 1)
	go func() { cancelled <- queue.cancelKey(key) }()
	select {
	case err := <-cancelled:
		t.Fatalf("cancellation returned before delivery completed: %v", err)
	case <-time.After(20 * time.Millisecond):
	}
	close(release)
	if err := <-processed; err != nil {
		t.Fatal(err)
	}
	if err := <-cancelled; err != nil {
		t.Fatal(err)
	}
	if queue.hasEntriesFor(key) {
		t.Fatal("cancelled event retained queued deliveries")
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
	manager.sendPush = func(context.Context, Subscription, []byte) error {
		sends++
		return nil
	}
	results, err := manager.RunOnce(t.Context(), now.Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if sends != 0 || len(results) != 1 || results[0].Disposition != DeliveryStale {
		t.Fatalf("policy-rechecked delivery = %#v, sends %d", results, sends)
	}
}

func TestTerminalTestDeliveryRetiresActiveAndDeliveredState(t *testing.T) {
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
	manager.sendPush = func(context.Context, Subscription, []byte) error { return nil }
	now := time.Date(2026, time.August, 31, 12, 0, 0, 0, time.UTC)
	key := PushEventKey{DeviceID: "device-1", EventID: "test-event", Category: CategoryTest}
	if _, err := manager.Publish(t.Context(), PublishRequest{
		Key: key, Preview: PreviewHidden, CreatedAt: now, ExpiresAt: now.Add(time.Minute),
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.RunOnce(t.Context(), now); err != nil {
		t.Fatal(err)
	}
	if manager.active[key] || len(manager.queue.deliveredFor(key)) != 0 {
		t.Fatal("terminal test delivery retained active or delivered state")
	}
}
