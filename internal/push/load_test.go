package push

import (
	"bytes"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestNewManagerReplacesUnreadableVAPIDKeys(t *testing.T) {
	privatePEM, publicPEM := testVAPIDKeyPair(t)
	key, err := parseVAPIDPrivateKey(string(privatePEM))
	if err != nil {
		t.Fatal(err)
	}
	sec1, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	for name, files := range map[string]map[string][]byte{
		"private scalar": {
			"vapid_private.pem": []byte(encodeVAPIDPrivate(key)),
			"vapid_public.pem":  publicPEM,
		},
		"SEC1 private": {
			"vapid_private.pem": pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: sec1}),
			"vapid_public.pem":  publicPEM,
		},
		"public point": {
			"vapid_private.pem": privatePEM,
			"vapid_public.pem":  []byte(encodeVAPIDPublic(&key.PublicKey)),
		},
		"invalid private only": {
			"vapid_private.pem": []byte("not-a-key\n"),
		},
		"invalid public": {
			"vapid_private.pem": privatePEM,
			"vapid_public.pem":  []byte("not-a-key\n"),
		},
		"invalid private DER": {
			"vapid_private.pem": pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: []byte("invalid")}),
			"vapid_public.pem":  publicPEM,
		},
		"invalid public DER": {
			"vapid_private.pem": privatePEM,
			"vapid_public.pem":  pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: []byte("invalid")}),
		},
	} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			for filename, data := range files {
				if err := os.WriteFile(filepath.Join(dir, filename), data, 0o600); err != nil {
					t.Fatal(err)
				}
			}
			var logs bytes.Buffer
			manager, err := NewManager(dir, slog.New(slog.NewTextHandler(&logs, nil)))
			if err != nil {
				t.Fatal(err)
			}
			if manager.vapidPrivate == "" || manager.VAPIDPublicKey() == "" || manager.VAPIDPublicKey() == encodeVAPIDPublic(&key.PublicKey) {
				t.Fatal("unreadable key did not produce a new key pair")
			}
			if !strings.Contains(logs.String(), "level=WARN") || !strings.Contains(logs.String(), "generating new key pair") {
				t.Fatalf("key recovery was not logged: %s", logs.String())
			}
			restarted, err := NewManager(dir, testLogger())
			if err != nil {
				t.Fatal(err)
			}
			if restarted.vapidPrivate != manager.vapidPrivate || restarted.VAPIDPublicKey() != manager.VAPIDPublicKey() {
				t.Fatal("replacement key pair changed across restart")
			}
		})
	}
}

func TestNewManagerStartsWithoutUnreadableSubscriptions(t *testing.T) {
	for name, data := range map[string]string{
		"empty":             "",
		"invalid JSON":      "{",
		"invalid wrapper":   `{"subscriptions":false}`,
		"flat empty":        "[]",
		"flat subscription": `[{"endpoint":"https://fcm.googleapis.com/phone","keys":{"p256dh":"key","auth":"auth"}}]`,
	} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "subscriptions.json")
			if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
				t.Fatal(err)
			}
			var logs bytes.Buffer
			manager, err := NewManager(dir, slog.New(slog.NewTextHandler(&logs, nil)))
			if err != nil {
				t.Fatal(err)
			}
			if len(manager.Subscriptions()) != 0 {
				t.Fatalf("loaded unreadable subscriptions: %#v", manager.Subscriptions())
			}
			if !strings.Contains(logs.String(), "level=WARN") || !strings.Contains(logs.String(), "starting with no subscriptions") {
				t.Fatalf("subscription recovery was not logged: %s", logs.String())
			}
			assertFileContents(t, path, []byte(data))
			subscription := typedTestSubscription("device-a", "https://fcm.googleapis.com/phone")
			if err := manager.Subscribe(subscription); err != nil {
				t.Fatal(err)
			}
			restarted, err := NewManager(dir, testLogger())
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(restarted.Subscriptions(), []Subscription{subscription}) {
				t.Fatalf("restarted subscriptions = %#v", restarted.Subscriptions())
			}
		})
	}
}

func TestNewManagerDoesNotRewriteLoadedPushFiles(t *testing.T) {
	dir := t.TempDir()
	manager, err := NewManager(dir, testLogger())
	if err != nil {
		t.Fatal(err)
	}
	subscription := typedTestSubscription("device-a", "https://fcm.googleapis.com/phone")
	if err := manager.Subscribe(subscription); err != nil {
		t.Fatal(err)
	}
	before := make(map[string]os.FileInfo)
	for _, filename := range []string{"vapid_private.pem", "vapid_public.pem", "subscriptions.json"} {
		path := filepath.Join(dir, filename)
		if err := os.Chmod(path, 0o400); err != nil {
			t.Fatal(err)
		}
		before[filename], err = os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
	}
	restarted, err := NewManager(dir, testLogger())
	if err != nil {
		t.Fatal(err)
	}
	if restarted.vapidPrivate != manager.vapidPrivate || restarted.VAPIDPublicKey() != manager.VAPIDPublicKey() {
		t.Fatal("loaded key pair changed")
	}
	if !reflect.DeepEqual(restarted.Subscriptions(), []Subscription{subscription}) {
		t.Fatalf("loaded subscriptions = %#v", restarted.Subscriptions())
	}
	for filename, info := range before {
		after, err := os.Stat(filepath.Join(dir, filename))
		if err != nil {
			t.Fatal(err)
		}
		if !os.SameFile(info, after) || !info.ModTime().Equal(after.ModTime()) || info.Mode() != after.Mode() {
			t.Fatalf("%s changed on load", filename)
		}
	}
}

func TestNewManagerKeepsPushFileReadErrorsFatal(t *testing.T) {
	for _, filename := range []string{"vapid_private.pem", "vapid_public.pem", "subscriptions.json"} {
		t.Run(filename+"/directory", func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, filename)
			if err := os.Mkdir(path, 0o700); err != nil {
				t.Fatal(err)
			}
			if _, err := NewManager(dir, testLogger()); err == nil {
				t.Fatal("file read error did not stop startup")
			}
		})
		t.Run(filename+"/permission", func(t *testing.T) {
			if os.Geteuid() == 0 {
				t.Skip("requires file permission enforcement")
			}
			dir := t.TempDir()
			path := filepath.Join(dir, filename)
			if err := os.WriteFile(path, []byte("unreadable"), 0o000); err != nil {
				t.Fatal(err)
			}
			if _, err := NewManager(dir, testLogger()); !errors.Is(err, os.ErrPermission) {
				t.Fatalf("NewManager error = %v, want permission error", err)
			}
		})
	}
}

func TestStoredPolicyUpdateOnce(t *testing.T) {
	for name, field := range map[string]string{
		"omitted": "",
		"false":   `,"update_once":false`,
		"true":    `,"update_once":true`,
	} {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "policies.json")
			if err := os.WriteFile(path, []byte(`{"policies":{"device-a":{"device_id":"device-a"`+field+`}}}`), 0o600); err != nil {
				t.Fatal(err)
			}
			engine, err := newPolicyEngine(path)
			if err != nil {
				t.Fatal(err)
			}
			if got := engine.Get("device-a", "en").UpdateOnce; got != (name == "true") {
				t.Fatalf("update once = %v", got)
			}
			if !engine.Get("new-device", "en").UpdateOnce {
				t.Fatal("new device no longer gets the default policy")
			}
		})
	}
}
