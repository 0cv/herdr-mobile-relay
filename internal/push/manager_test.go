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
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
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
	subscriptions := []byte(`{"subscriptions":[{"subscription":{"endpoint":"https://push.example.test/legacy","keys":{"p256dh":"legacy-key","auth":"legacy-auth"}},"client_id":"phone","notify_finished":true}]}`)
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

	sub := Subscription{Endpoint: "https://push.example.com/ep1"}
	sub.Keys.P256dh = "key1"
	sub.Keys.Auth = "auth1"
	m.Subscribe(sub)

	subs := m.Subscriptions()
	if len(subs) != 1 {
		t.Fatalf("subs = %d, want 1", len(subs))
	}
	if subs[0].Endpoint != "https://push.example.com/ep1" {
		t.Errorf("endpoint = %q", subs[0].Endpoint)
	}

	// Subscribe same endpoint updates
	sub2 := Subscription{Endpoint: "https://push.example.com/ep1", NotifyFinished: true}
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
	m.Unsubscribe([]string{"https://push.example.com/ep1"})
	subs = m.Subscriptions()
	if len(subs) != 0 {
		t.Fatalf("after unsubscribe: subs = %d, want 0", len(subs))
	}
}

func TestSubscriptionsPersist(t *testing.T) {
	dir := t.TempDir()
	m1, _ := NewManager(dir, testLogger())

	sub := Subscription{Endpoint: "https://push.example.com/persist"}
	sub.Keys.P256dh = "k"
	sub.Keys.Auth = "a"
	m1.Subscribe(sub)

	m2, _ := NewManager(dir, testLogger())
	subs := m2.Subscriptions()
	if len(subs) != 1 || subs[0].Endpoint != "https://push.example.com/persist" {
		t.Error("subscriptions should persist across restarts")
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
		{Endpoint: "https://push.example.com/disabled"},
		{Endpoint: "https://push.example.com/enabled", NotifyFinished: true},
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

	if !reflect.DeepEqual(sent, []string{"https://push.example.com/enabled"}) {
		t.Fatalf("finished push endpoints = %v, want only opted-in subscription", sent)
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
