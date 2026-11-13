package push

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"log/slog"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestLoadedVAPIDKeyUsesPKCS8OnDisk(t *testing.T) {
	privatePEM, publicPEM := testVAPIDKeyPair(t)
	key, err := parseVAPIDPrivateKey(string(privatePEM))
	if err != nil {
		t.Fatal(err)
	}
	sec1, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	for name, data := range map[string][]byte{
		"scalar": []byte(encodeVAPIDPrivate(key)),
		"SEC1":   pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: sec1}),
		"PKCS8":  privatePEM,
	} {
		for _, mode := range []os.FileMode{0o600, 0o400} {
			t.Run(name+"/"+mode.String(), func(t *testing.T) {
				dir := t.TempDir()
				privatePath := filepath.Join(dir, "vapid_private.pem")
				if err := os.WriteFile(privatePath, data, mode); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(dir, "vapid_public.pem"), publicPEM, 0o644); err != nil {
					t.Fatal(err)
				}
				before, err := os.Stat(privatePath)
				if err != nil {
					t.Fatal(err)
				}
				manager, err := NewManager(dir, testLogger())
				if err != nil {
					t.Fatal(err)
				}
				if manager.vapidPrivate != encodeVAPIDPrivate(key) || manager.VAPIDPublicKey() != encodeVAPIDPublic(&key.PublicKey) {
					t.Fatal("loaded VAPID key changed")
				}
				rewritten, err := os.ReadFile(privatePath)
				if err != nil {
					t.Fatal(err)
				}
				block, _ := pem.Decode(rewritten)
				if block == nil || block.Type != "PRIVATE KEY" {
					t.Fatalf("private key is not PKCS8 PEM: %q", rewritten)
				}
				parsed, err := x509.ParsePKCS8PrivateKey(block.Bytes)
				if err != nil {
					t.Fatal(err)
				}
				if parsedKey, ok := parsed.(*ecdsa.PrivateKey); !ok || !parsedKey.Equal(key) {
					t.Fatal("rewritten private key changed")
				}
				after, err := os.Stat(privatePath)
				if err != nil {
					t.Fatal(err)
				}
				if after.Mode().Perm() != mode || os.SameFile(before, after) {
					t.Fatalf("private key was not atomically replaced with mode %o: %v", mode, after.Mode())
				}
			})
		}
	}
}

func TestLoadedFlatSubscriptionsUseWrapperOnDisk(t *testing.T) {
	var subscription Subscription
	if err := json.Unmarshal([]byte(`{"endpoint":"https://fcm.googleapis.com/phone","keys":{"p256dh":"key","auth":"auth"},"device_id":"device-a","locale":"fr","platform":"android","user_agent":"phone","notify_finished":true,"client_id":"client-a"}`), &subscription); err != nil {
		t.Fatal(err)
	}
	for name, subscriptions := range map[string][]Subscription{
		"empty": {},
		"phone": {subscription},
	} {
		for _, mode := range []os.FileMode{0o600, 0o640, 0o400} {
			t.Run(name+"/"+mode.String(), func(t *testing.T) {
				dir := t.TempDir()
				path := filepath.Join(dir, "subscriptions.json")
				data, err := json.Marshal(subscriptions)
				if err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(path, data, mode); err != nil {
					t.Fatal(err)
				}
				before, err := os.Stat(path)
				if err != nil {
					t.Fatal(err)
				}
				manager, err := NewManager(dir, testLogger())
				if err != nil {
					t.Fatal(err)
				}
				if !reflect.DeepEqual(manager.Subscriptions(), subscriptions) {
					t.Fatalf("loaded subscriptions = %#v, want %#v", manager.Subscriptions(), subscriptions)
				}
				rewritten, err := os.ReadFile(path)
				if err != nil {
					t.Fatal(err)
				}
				var wrapper pythonFile
				if err := json.Unmarshal(rewritten, &wrapper); err != nil || wrapper.Subscriptions == nil || len(wrapper.Subscriptions) != len(subscriptions) {
					t.Fatalf("subscriptions are not wrapped: %s (%v)", rewritten, err)
				}
				after, err := os.Stat(path)
				if err != nil {
					t.Fatal(err)
				}
				if after.Mode().Perm() != mode || os.SameFile(before, after) {
					t.Fatalf("subscriptions were not atomically replaced with mode %o: %v", mode, after.Mode())
				}
				restarted, err := NewManager(dir, testLogger())
				if err != nil {
					t.Fatal(err)
				}
				if !reflect.DeepEqual(restarted.Subscriptions(), subscriptions) {
					t.Fatalf("restarted subscriptions = %#v, want %#v", restarted.Subscriptions(), subscriptions)
				}
			})
		}
	}
}

func TestLoadedPushFilesRemainUsableWhenRewriteFails(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("requires file permission enforcement")
	}
	dir := t.TempDir()
	privatePEM, publicPEM := testVAPIDKeyPair(t)
	key, err := parseVAPIDPrivateKey(string(privatePEM))
	if err != nil {
		t.Fatal(err)
	}
	privateData := []byte(encodeVAPIDPrivate(key))
	privatePath := filepath.Join(dir, "vapid_private.pem")
	publicPath := filepath.Join(dir, "vapid_public.pem")
	subscriptionsPath := filepath.Join(dir, "subscriptions.json")
	subscriptionsData := []byte(`[{"endpoint":"https://fcm.googleapis.com/phone","keys":{"p256dh":"key","auth":"auth"}}]`)
	for path, data := range map[string][]byte{
		privatePath:       privateData,
		publicPath:        publicPEM,
		subscriptionsPath: subscriptionsData,
	} {
		if err := os.WriteFile(path, data, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.Chmod(dir, 0o700); err != nil {
			t.Error(err)
		}
	})
	var logs bytes.Buffer
	manager := &Manager{path: subscriptionsPath, logger: slog.New(slog.NewTextHandler(&logs, nil))}
	if err := manager.load(); err != nil {
		t.Fatal(err)
	}
	if err := manager.loadOrGenerateVAPIDKeys(dir); err != nil {
		t.Fatal(err)
	}
	if len(manager.Subscriptions()) != 1 || manager.Subscriptions()[0].Endpoint != "https://fcm.googleapis.com/phone" {
		t.Fatalf("loaded subscriptions = %#v", manager.Subscriptions())
	}
	if manager.vapidPrivate != string(privateData) || manager.VAPIDPublicKey() != encodeVAPIDPublic(&key.PublicKey) {
		t.Fatal("loaded VAPID key changed")
	}
	assertFileContents(t, privatePath, privateData)
	assertFileContents(t, subscriptionsPath, subscriptionsData)
	if strings.Count(logs.String(), "rewrite loaded push file") != 2 {
		t.Fatalf("rewrite failures were not logged: %s", logs.String())
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
