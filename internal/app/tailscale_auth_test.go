package app

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

func TestTailscaleS2BootstrapPreservesCredentials(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "store")
	store, err := deviceauth.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{Transport: config.TransportTailscale, Token: strings.Repeat("k", 32)}
	server := &Server{cfg: cfg, deviceAuth: store, hostname: "fixture", ready: true}
	var originals []transport.E2EEAuthResult
	for _, role := range []deviceauth.Role{deviceauth.RoleController, deviceauth.RoleReader} {
		invitation, err := store.CreateInvitation(string(role), role, "en")
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
	verify := func(s *deviceauth.Store) {
		t.Helper()
		for _, original := range originals {
			id := original.Identity
			credential, ok := s.AuthorizeCredential(id.CredentialID, id.CredentialVersion)
			if !ok || credential.DeviceID != id.DeviceID || string(credential.Role) != id.Role {
				t.Fatal("prior credential authorization lost")
			}
			secret, err := s.ResolveE2EESecret(context.Background(), transport.E2EEAuthSelector{Kind: transport.E2EEAuthCredential, ID: id.CredentialID, Version: id.CredentialVersion})
			if err != nil || !bytes.Equal(secret, original.CredentialSecret) {
				t.Fatal("prior credential secret lost")
			}
		}
	}
	if err := armBootstrap(store, cfg, "fixture"); err != nil {
		t.Fatal(err)
	}
	if store.BootstrapStatus().Armed {
		t.Fatal("stable path resurrected invitation")
	}
	verify(store)
	status, err := server.armBootstrapForControl()
	if err != nil || !status.InvitationArmed || status.InvitationExpiresAt == "" {
		t.Fatal("explicit arm not acknowledged")
	}
	verify(store)
	reopened, err := deviceauth.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	verify(reopened)
	if !reopened.BootstrapStatus().Armed {
		t.Fatal("acknowledged invitation not persisted")
	}
	before, err := os.ReadFile(filepath.Join(dir, "devices.json"))
	if err != nil {
		t.Fatal(err)
	}
	prior := store.BootstrapStatus()
	// Replace only the private directory path with a file; CreateTemp must fail
	// before publication, regardless of user privileges. Restore before reopen.
	moved := dir + "-saved"
	if err := os.Rename(dir, moved); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(dir, []byte("obstacle"), 0600); err != nil {
		t.Fatal(err)
	}
	outcome := server.armBootstrapInvitation()
	if err := os.Remove(dir); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(moved, dir); err != nil {
		t.Fatal(err)
	}
	if outcome == "armed for one more device" || !strings.Contains(outcome, "persist bootstrap invitation") {
		t.Fatal("app reported successful arm on persistence failure")
	}
	if store.BootstrapStatus() != prior {
		t.Fatal("failed app arm changed invitation")
	}
	after, err := os.ReadFile(filepath.Join(dir, "devices.json"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) {
		t.Fatal("failed arm changed disk")
	}
	verify(store)
	reopened, err = deviceauth.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	verify(reopened)
	if server.armBootstrapInvitation() != "armed for one more device" {
		t.Fatal("recovered app arm failed")
	}
	// Explicit local Cloudflare control preserves intentional ephemeral reset.
	cloudflare := &config.Config{Transport: config.TransportCloudflare, Token: cfg.Token, RearmBootstrap: true}
	if err := armBootstrap(store, cloudflare, "ephemeral"); err != nil {
		t.Fatal(err)
	}
	if len(store.ListCredentials("")) != 0 || !store.BootstrapStatus().Armed {
		t.Fatal("Cloudflare reset behavior changed")
	}
	reopened, err = deviceauth.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(reopened.ListCredentials("")) != 0 || !reopened.BootstrapStatus().Armed {
		t.Fatal("Cloudflare reset not persisted")
	}
}
