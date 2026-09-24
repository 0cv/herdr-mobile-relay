package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/0cv/herdr-mobile-relay/internal/config"
	"github.com/0cv/herdr-mobile-relay/internal/deviceauth"
	"github.com/0cv/herdr-mobile-relay/internal/transport"
)

func TestTailscaleS2RejectedStartupPreservesCredentials(t *testing.T) {
	root := t.TempDir()
	// Clear inherited application settings before setting only private paths.
	for _, entry := range os.Environ() {
		key, _, _ := strings.Cut(entry, "=")
		if strings.HasPrefix(key, "HERDR_") || strings.HasPrefix(key, "XDG_") {
			t.Setenv(key, "")
		}
	}
	for key, value := range map[string]string{
		"HOME": root, "XDG_CONFIG_HOME": filepath.Join(root, "config"), "XDG_DATA_HOME": filepath.Join(root, "data"), "XDG_CACHE_HOME": filepath.Join(root, "cache"), "XDG_RUNTIME_DIR": filepath.Join(root, "runtime"),
		"HERDR_RELAY_ENV": filepath.Join(root, "runtime", "relay.env"), "HERDR_RELEASE_ROOT": filepath.Join(root, "release"), "HERDR_WEB_ROOT": filepath.Join(root, "web"), "HERDR_BIN": filepath.Join(root, "nonexistent-herdr"), "HERDR_SOCKET_PATH": filepath.Join(root, "nonexistent.sock"),
		"HERDR_RELAY_TRANSPORT": "tailscale", "HERDR_RELAY_HOST": "127.0.0.1", "HERDR_RELAY_TOKEN": strings.Repeat("k", 32), "HERDR_RELAY_REARM_BOOTSTRAP": "false", "HERDR_TAILSCALE_ORIGIN": "https://fixture.example.ts.net", "HERDR_RELAY_INSTANCE_ID": "s2-instance", "HERDR_RELAY_RUN_ID": "s2-run", "HERDR_RELAY_PAIRING_SOCKET": filepath.Join(root, "pairing.sock"),
	} {
		t.Setenv(key, value)
	}
	cfg, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.RearmBootstrap || len(cfg.Token) != 32 {
		t.Fatal("otherwise-valid fixture not established")
	}
	dir := filepath.Join(cfg.RuntimeDir, "device-auth")
	store, err := deviceauth.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	var originals []transport.E2EEAuthResult
	for _, name := range []string{"first", "second"} {
		invitation, err := store.CreateInvitation(name, deviceauth.RoleController, "en")
		if err != nil {
			t.Fatal(err)
		}
		selector := transport.E2EEAuthSelector{Kind: transport.E2EEAuthInvitation, ID: invitation.InvitationID, Version: invitation.Version}
		if _, err := store.ResolveE2EESecret(context.Background(), selector); err != nil {
			t.Fatal(err)
		}
		result, err := store.CompleteE2EEAuth(context.Background(), selector, true)
		if err != nil {
			t.Fatal(err)
		}
		selector = transport.E2EEAuthSelector{Kind: transport.E2EEAuthCredential, ID: result.Identity.CredentialID, Version: result.Identity.CredentialVersion}
		if _, err := store.CompleteE2EEAuth(context.Background(), selector, true); err != nil {
			t.Fatal(err)
		}
		originals = append(originals, result)
	}
	if originals[0].Identity.DeviceID == originals[1].Identity.DeviceID || originals[0].Identity.CredentialID == originals[1].Identity.CredentialID || bytes.Equal(originals[0].CredentialSecret, originals[1].CredentialSecret) {
		t.Fatal("fixtures not distinct")
	}
	cases := []struct{ name, key, reset, refusal string }{
		{"empty", "", "false", "tailscale transport requires a relay key of exactly 32 bytes"},
		{"short", strings.Repeat("k", 31), "false", "relay key must be exactly 32 bytes"},
		{"long", strings.Repeat("k", 33), "false", "relay key must be exactly 32 bytes"},
	}
	for _, reset := range []string{"1", "t", "T", "TRUE", "True", "true"} {
		cases = append(cases, struct{ name, key, reset, refusal string }{"reset-" + reset, strings.Repeat("k", 32), reset, "tailscale transport refuses HERDR_RELAY_REARM_BOOTSTRAP; it would reset device credentials"})
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("HERDR_RELAY_TOKEN", tc.key)
			t.Setenv("HERDR_RELAY_REARM_BOOTSTRAP", tc.reset)
			// Mandatory guard: never call runServe if frozen admission is not certain.
			if _, err := config.Load(); err == nil || err.Error() != tc.refusal {
				t.Fatal("expected static config refusal absent; runServe not executed")
			}
			path := filepath.Join(dir, "devices.json")
			before, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			info, err := os.Stat(path)
			if err != nil {
				t.Fatal(err)
			}
			code, err := runServe()
			if code != 1 || err == nil || err.Error() != tc.refusal {
				t.Fatal("entrypoint did not return exact admission refusal")
			}
			after, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			afterInfo, err := os.Stat(path)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(before, after) || !os.SameFile(info, afterInfo) || info.Mode() != afterInfo.Mode() {
				t.Fatal("rejected startup changed store bytes/identity/mode")
			}
			// Open persists: verify untouched disk above before this operation.
			reopened, err := deviceauth.Open(dir)
			if err != nil {
				t.Fatal(err)
			}
			for _, original := range originals {
				id := original.Identity
				c, ok := reopened.AuthorizeCredential(id.CredentialID, id.CredentialVersion)
				if !ok || c.DeviceID != id.DeviceID || string(c.Role) != id.Role {
					t.Fatal("credential authorization changed")
				}
				secret, err := reopened.ResolveE2EESecret(context.Background(), transport.E2EEAuthSelector{Kind: transport.E2EEAuthCredential, ID: id.CredentialID, Version: id.CredentialVersion})
				if err != nil || !bytes.Equal(secret, original.CredentialSecret) {
					t.Fatal("credential resolution changed")
				}
			}
		})
	}
}
