package deviceauth

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/0cv/herdr-mobile-relay/internal/transport"
)

func closedGateFixture() (*BootstrapGate, *Store, transport.E2EEAuthSelector) {
	secret := base64.RawURLEncoding.EncodeToString([]byte(strings.Repeat("s", secretBytes)))
	store := &Store{
		now:    func() time.Time { return time.Unix(100, 0).UTC() },
		random: strings.NewReader(strings.Repeat("r", 256)),
		state: diskState{
			SchemaVersion: storeSchemaVersion,
			Invitation: &invitationRecord{
				InvitationID: bootstrapInvitationID,
				Version:      1,
				Secret:       secret,
				ExpiresAt:    time.Unix(99, 0).UTC(),
				Name:         "relay",
				Role:         RoleController,
				Locale:       "en",
			},
			Credentials: make([]credentialRecord, 0),
		},
	}
	gate := NewBootstrapGate()
	if err := gate.Attach(store); err != nil {
		panic(err)
	}
	selector := transport.E2EEAuthSelector{Kind: transport.E2EEAuthInvitation, ID: bootstrapInvitationID, Version: 1}
	return gate, store, selector
}

func TestBootstrapGateClosedRejectsInvitationBeforeStoreAccess(t *testing.T) {
	gate, store, selector := closedGateFixture()
	before := store.state.Invitation.ExpiresAt

	if _, err := gate.ResolveE2EESecret(context.Background(), selector); !errors.Is(err, ErrBootstrapGateClosed) {
		t.Fatalf("ResolveE2EESecret error = %v, want transient closed-gate error", err)
	}
	if gate.IsE2EEAuthRejected(ErrBootstrapGateClosed) {
		t.Fatal("closed gate was classified as permanently rejected authentication")
	}
	for _, authenticated := range []bool{false, true} {
		if _, err := gate.CompleteE2EEAuth(context.Background(), selector, authenticated); !errors.Is(err, ErrBootstrapGateClosed) {
			t.Fatalf("CompleteE2EEAuth(%t) error = %v, want closed-gate error", authenticated, err)
		}
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	if store.state.Invitation == nil || !store.state.Invitation.ExpiresAt.Equal(before) || store.state.Invitation.FailedAttempts != 0 || store.state.Invitation.PendingCredentialID != "" || len(store.state.Credentials) != 0 {
		t.Fatal("closed gate allowed expired-invitation refresh, consumption, or attempt accounting")
	}
}

func TestBootstrapGateRawWatchEOFDeniesResolveAndComplete(t *testing.T) {
	store, err := OpenDeferred(filepath.Join(t.TempDir(), "device-auth"))
	if err != nil {
		t.Fatal(err)
	}
	gate := NewBootstrapGate()
	gate.RequireAuthorityAdmission()
	watchEnded := make(chan struct{})
	authorityInvalidated := make(chan struct{})
	if err := gate.BindAuthorityAdmission(watchEnded, authorityInvalidated); err != nil {
		t.Fatal(err)
	}
	if err := gate.Attach(store); err != nil {
		t.Fatal(err)
	}
	if err := gate.ArmBootstrapInvitation(bytes.Repeat([]byte{7}, secretBytes), "relay", "en", nil); err != nil {
		t.Fatalf("arm with live authority channels: %v", err)
	}
	selector := transport.E2EEAuthSelector{Kind: transport.E2EEAuthInvitation, ID: bootstrapInvitationID, Version: 1, Locale: "en"}
	close(watchEnded)
	if gate.OpenStatus() {
		t.Fatal("watch EOF left admission logically open")
	}
	if _, err := gate.ResolveE2EESecret(context.Background(), selector); !errors.Is(err, ErrBootstrapGateClosed) {
		t.Fatalf("ResolveE2EESecret after watch EOF = %v", err)
	}
	if _, err := gate.CompleteE2EEAuth(context.Background(), selector, true); !errors.Is(err, ErrBootstrapGateClosed) {
		t.Fatalf("CompleteE2EEAuth after watch EOF = %v", err)
	}
	if credentials := store.ListCredentials(""); len(credentials) != 0 {
		t.Fatalf("watch EOF enrolled credentials: %+v", credentials)
	}
}

func TestBootstrapGateRevokeDoesNotWaitBehindAdmissionLock(t *testing.T) {
	gate, store, selector := closedGateFixture()
	store.state.Invitation.ExpiresAt = time.Unix(101, 0).UTC()
	if err := gate.Open(); err != nil {
		t.Fatal(err)
	}
	gate.mu.RLock()
	revoked := make(chan struct{})
	go func() {
		gate.Revoke()
		close(revoked)
	}()
	select {
	case <-revoked:
	case <-time.After(time.Second):
		gate.mu.RUnlock()
		t.Fatal("Revoke waited behind an admission lock")
	}
	if !gate.revoked.Load() {
		gate.mu.RUnlock()
		t.Fatal("Revoke did not latch denial while the admission lock was held")
	}
	gate.mu.RUnlock()
	if gate.OpenStatus() {
		t.Fatal("atomic revocation left the gate open")
	}
	if _, err := gate.ResolveE2EESecret(context.Background(), selector); !errors.Is(err, ErrBootstrapGateClosed) {
		t.Fatalf("resolver error after revocation = %v", err)
	}
	if err := gate.Open(); !errors.Is(err, ErrBootstrapGateClosed) {
		t.Fatalf("revoked gate reopened: %v", err)
	}
}
