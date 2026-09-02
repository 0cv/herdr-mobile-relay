package deviceauth

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"testing"
	"time"

	"github.com/0cv/herdr-mobile-relay/internal/transport"
)

func testCredential(deviceID, credentialID string, role Role) credentialRecord {
	return credentialRecord{
		Credential: Credential{
			DeviceID: deviceID, CredentialID: credentialID, Name: deviceID,
			Role: role, Locale: "en", PairedAt: time.Unix(1, 0).UTC(), Version: 1,
		},
		Secret: base64.RawURLEncoding.EncodeToString(bytes.Repeat([]byte{1}, secretBytes)),
	}
}

func TestRevokeCredentialPreservesLastController(t *testing.T) {
	store, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	store.state.Credentials = []credentialRecord{testCredential("device-one", "credential-one", RoleController)}
	if _, err := store.RevokeCredential("credential-one"); !errors.Is(err, ErrLastController) {
		t.Fatalf("RevokeCredential() error = %v, want %v", err, ErrLastController)
	}
	store.state.Credentials = append(store.state.Credentials, testCredential("device-two", "credential-two", RoleController))
	credential, err := store.RevokeCredential("credential-one")
	if err != nil {
		t.Fatal(err)
	}
	if !credential.Revoked || credential.Version != 2 {
		t.Fatalf("RevokeCredential() = %#v", credential)
	}
}

func TestArmBootstrapInvitationKeepsEnrolledDevices(t *testing.T) {
	store, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	secret := bytes.Repeat([]byte{9}, secretBytes)
	if err := store.EnsureBootstrapInvitation(secret, "relay", "en"); err != nil {
		t.Fatal(err)
	}
	// The first phone consumes the bootstrap; a stable install then refuses to
	// re-arm it on its own.
	if _, err := store.CompleteE2EEAuth(context.Background(), transport.E2EEAuthSelector{
		Kind: transport.E2EEAuthInvitation, ID: bootstrapInvitationID, Version: 1,
	}, true); err != nil {
		t.Fatal(err)
	}
	if err := store.EnsureBootstrapInvitation(secret, "relay", "en"); err != nil {
		t.Fatal(err)
	}
	if store.state.Invitation != nil {
		t.Fatal("stable install re-armed the consumed bootstrap by itself")
	}

	if err := store.ArmBootstrapInvitation(secret, "relay", "en"); err != nil {
		t.Fatal(err)
	}
	if store.state.Invitation == nil || store.state.Invitation.InvitationID != bootstrapInvitationID {
		t.Fatalf("bootstrap not re-armed: %#v", store.state.Invitation)
	}
	if got := len(store.ListCredentials("")); got != 1 {
		t.Fatalf("enrolled devices after re-arm = %d, want 1 kept", got)
	}
	if _, err := store.CompleteE2EEAuth(context.Background(), transport.E2EEAuthSelector{
		Kind: transport.E2EEAuthInvitation, ID: bootstrapInvitationID, Version: 1,
	}, true); err != nil {
		t.Fatalf("second phone could not pair with the re-armed bootstrap: %v", err)
	}
	if got := len(store.ListCredentials("")); got != 2 {
		t.Fatalf("enrolled devices after second pairing = %d, want 2", got)
	}
}

func TestResetWithBootstrapAtomicallyReplacesCredentials(t *testing.T) {
	dir := t.TempDir()
	store, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	store.state.Credentials = []credentialRecord{
		testCredential("device-one", "credential-one", RoleController),
		testCredential("device-two", "credential-two", RoleReader),
	}
	secret := bytes.Repeat([]byte{7}, secretBytes)
	if err := store.ResetWithBootstrap(secret, "relay", "en"); err != nil {
		t.Fatal(err)
	}
	if credentials := store.ListCredentials(""); len(credentials) != 0 {
		t.Fatalf("ListCredentials() = %#v, want none", credentials)
	}
	resolved, err := store.ResolveE2EESecret(context.Background(), transport.E2EEAuthSelector{
		Kind: transport.E2EEAuthInvitation, ID: bootstrapInvitationID, Version: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(resolved, secret) {
		t.Fatalf("ResolveE2EESecret() returned the wrong bootstrap secret")
	}
	reopened, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	invitation := reopened.state.Invitation
	if invitation == nil || invitation.InvitationID != bootstrapInvitationID || invitation.Role != RoleController {
		t.Fatalf("persisted invitation = %#v", invitation)
	}
}

func TestBootstrapInvitationRefreshesWhenFirstUsedAfterExpiry(t *testing.T) {
	store, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	now := time.Unix(1_700_000_000, 0).UTC()
	store.now = func() time.Time { return now }
	secret := bytes.Repeat([]byte{9}, secretBytes)
	if err := store.EnsureBootstrapInvitation(secret, "relay", "en"); err != nil {
		t.Fatal(err)
	}
	firstExpiry := store.state.Invitation.ExpiresAt
	now = firstExpiry
	resolved, err := store.ResolveE2EESecret(context.Background(), transport.E2EEAuthSelector{
		Kind: transport.E2EEAuthInvitation, ID: bootstrapInvitationID, Version: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(resolved, secret) {
		t.Fatal("refreshed bootstrap secret changed")
	}
	if !store.state.Invitation.ExpiresAt.Equal(now.Add(invitationLifetime)) {
		t.Fatalf("refreshed expiry = %s", store.state.Invitation.ExpiresAt)
	}
}

func TestBootstrapInvitationDoesNotCountFailedProofs(t *testing.T) {
	store, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	secret := bytes.Repeat([]byte{9}, secretBytes)
	if err := store.EnsureBootstrapInvitation(secret, "relay", "en"); err != nil {
		t.Fatal(err)
	}
	selector := transport.E2EEAuthSelector{
		Kind: transport.E2EEAuthInvitation, ID: bootstrapInvitationID, Version: 1,
	}
	for range maxInviteAttempts + 1 {
		if _, err := store.CompleteE2EEAuth(context.Background(), selector, false); !errors.Is(err, ErrAuthentication) {
			t.Fatalf("failed proof error = %v", err)
		}
	}
	if store.state.Invitation == nil || store.state.Invitation.FailedAttempts != 0 {
		t.Fatalf("bootstrap invitation after failed proofs = %#v", store.state.Invitation)
	}
	resolved, err := store.ResolveE2EESecret(context.Background(), selector)
	if err != nil || !bytes.Equal(resolved, secret) {
		t.Fatalf("ResolveE2EESecret() = %x, %v", resolved, err)
	}
}

func TestAuthorizeCredentialRejectsRevokedAndReplacedVersions(t *testing.T) {
	store, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	record := testCredential("device-one", "credential-one", RoleReader)
	store.state.Credentials = []credentialRecord{record}
	if credential, ok := store.AuthorizeCredential(record.CredentialID, 1); !ok || credential.DeviceID != record.DeviceID {
		t.Fatalf("live credential authorization = %#v, %v", credential, ok)
	}
	store.state.Credentials[0].Version = 2
	if _, ok := store.AuthorizeCredential(record.CredentialID, 1); ok {
		t.Fatal("replaced credential version remained authorized")
	}
	store.state.Credentials[0].Revoked = true
	if _, ok := store.AuthorizeCredential(record.CredentialID, 2); ok {
		t.Fatal("revoked credential remained authorized")
	}
}

func TestAuthenticatedLocaleIsNormalizedAndPersisted(t *testing.T) {
	store, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	secret := bytes.Repeat([]byte{4}, secretBytes)
	if err := store.EnsureBootstrapInvitation(secret, "tablet", "en"); err != nil {
		t.Fatal(err)
	}
	result, err := store.CompleteE2EEAuth(context.Background(), transport.E2EEAuthSelector{
		Kind: transport.E2EEAuthInvitation, ID: bootstrapInvitationID, Version: 1, Locale: "zh-cn",
	}, true)
	if err != nil {
		t.Fatal(err)
	}
	if result.Identity.Locale != "zh-CN" {
		t.Fatalf("redeemed locale = %q", result.Identity.Locale)
	}
	credentials := store.ListCredentials(result.Identity.CredentialID)
	if len(credentials) != 1 || credentials[0].Locale != "zh-CN" {
		t.Fatalf("persisted credential = %#v", credentials)
	}
}

func TestBootstrapStaysConsumedForStableInstalls(t *testing.T) {
	store, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	store.state.Credentials = []credentialRecord{testCredential("device-one", "credential-one", RoleController)}
	if err := store.EnsureBootstrapInvitation(bytes.Repeat([]byte{5}, secretBytes), "relay", "en"); err != nil {
		t.Fatal(err)
	}
	invitation := store.state.Invitation
	if invitation != nil {
		t.Fatalf("consumed bootstrap re-armed without reenrollment: %#v", invitation)
	}
}

func TestRearmedBootstrapEnrollsAReplacementDeviceEachLaunch(t *testing.T) {
	dir := t.TempDir()
	store, err := Open(dir, WithBootstrapReenrollment())
	if err != nil {
		t.Fatal(err)
	}
	store.state.Credentials = []credentialRecord{testCredential("device-one", "credential-one", RoleController)}
	secret := bytes.Repeat([]byte{6}, secretBytes)
	if err := store.EnsureBootstrapInvitation(secret, "relay", "en"); err != nil {
		t.Fatal(err)
	}
	invitation := store.state.Invitation
	if invitation == nil || invitation.InvitationID != bootstrapInvitationID {
		t.Fatalf("re-armed invitation = %#v", invitation)
	}
	result, err := store.CompleteE2EEAuth(context.Background(), transport.E2EEAuthSelector{
		Kind: transport.E2EEAuthInvitation, ID: bootstrapInvitationID, Version: 1, Locale: "en",
	}, true)
	if err != nil {
		t.Fatal(err)
	}
	credentials := store.ListCredentials(result.Identity.CredentialID)
	if len(credentials) != 2 {
		t.Fatalf("credentials after reenrollment = %#v", credentials)
	}
	for _, credential := range credentials {
		if credential.DeviceID == "device-one" && credential.Revoked {
			t.Fatal("reenrollment revoked the previously paired device")
		}
	}
}

func TestRearmedBootstrapRefreshesExpiryWithEnrolledDevices(t *testing.T) {
	store, err := Open(t.TempDir(), WithBootstrapReenrollment())
	if err != nil {
		t.Fatal(err)
	}
	now := time.Unix(1_700_000_000, 0).UTC()
	store.now = func() time.Time { return now }
	store.state.Credentials = []credentialRecord{testCredential("device-one", "credential-one", RoleController)}
	secret := bytes.Repeat([]byte{8}, secretBytes)
	if err := store.EnsureBootstrapInvitation(secret, "relay", "en"); err != nil {
		t.Fatal(err)
	}
	now = now.Add(invitationLifetime)
	resolved, err := store.ResolveE2EESecret(context.Background(), transport.E2EEAuthSelector{
		Kind: transport.E2EEAuthInvitation, ID: bootstrapInvitationID, Version: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(resolved, secret) {
		t.Fatal("refreshed re-armed bootstrap returned the wrong secret")
	}
}
