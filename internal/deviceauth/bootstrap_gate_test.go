package deviceauth

import (
	"context"
	"encoding/base64"
	"errors"
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

func TestBootstrapGateRevokeWaitsForInFlightResolver(t *testing.T) {
	gate, store, selector := closedGateFixture()
	store.mu.Lock()
	store.state.Invitation.ExpiresAt = time.Unix(101, 0).UTC()
	if err := gate.Open(); err != nil {
		store.mu.Unlock()
		t.Fatal(err)
	}
	resolved := make(chan error, 1)
	go func() {
		_, err := gate.ResolveE2EESecret(context.Background(), selector)
		resolved <- err
	}()
	// The resolver holds the gate read lock while waiting for Store.mu. No
	// writer has been started yet, so TryLock failing proves that reader entered.
	deadline := time.Now().Add(time.Second)
	for {
		if !gate.mu.TryLock() {
			break
		}
		gate.mu.Unlock()
		if time.Now().After(deadline) {
			store.mu.Unlock()
			t.Fatal("resolver did not enter the gate")
		}
		time.Sleep(time.Millisecond)
	}
	revoked := make(chan struct{})
	go func() {
		gate.Revoke()
		close(revoked)
	}()
	select {
	case <-revoked:
		store.mu.Unlock()
		t.Fatal("Revoke returned while a resolver was still in flight")
	case <-time.After(10 * time.Millisecond):
	}
	store.mu.Unlock()
	<-resolved
	select {
	case <-revoked:
	case <-time.After(time.Second):
		t.Fatal("Revoke did not complete after the in-flight resolver left")
	}
	if err := gate.Open(); !errors.Is(err, ErrBootstrapGateClosed) {
		t.Fatalf("revoked gate reopened: %v", err)
	}
}
