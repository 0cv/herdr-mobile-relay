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
	invitation, err := reopened.ActiveInvitation()
	if err != nil {
		t.Fatal(err)
	}
	if invitation == nil || invitation.InvitationID != bootstrapInvitationID || invitation.Role != RoleController {
		t.Fatalf("ActiveInvitation() = %#v", invitation)
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
