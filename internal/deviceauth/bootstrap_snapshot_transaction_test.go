package deviceauth

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/0cv/herdr-mobile-relay/internal/transport"
)

// SYNTHETIC final admission only; this proves no HTTPS qualification.
func TestBootstrapSnapshotSyntheticTransactionPreservesDevices(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "devices")
	if err := os.Mkdir(dir, 0700); err != nil {
		t.Fatal(err)
	}
	credentials := []credentialRecord{}
	for _, role := range []Role{RoleController, RoleReader} {
		credentials = append(credentials, credentialRecord{Credential: Credential{
			DeviceID: string(role) + "-device", CredentialID: string(role) + "-credential", Name: "fixture", Role: role, Locale: "en", Version: 1, PairedAt: time.Unix(100, 0).UTC(),
		}, Secret: encodeSecret(bytes.Repeat([]byte(string(role)[:1]), 32))})
	}
	data, err := json.Marshal(diskState{SchemaVersion: storeSchemaVersion, Credentials: credentials})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, storeFilename), data, 0600); err != nil {
		t.Fatal(err)
	}
	s, err := OpenDeferred(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, raw := range []string{"synthetic-bootstrap-secret-00001", "synthetic-bootstrap-secret-00002"} {
		if err := s.ArmBootstrapInvitationTransactional([]byte(raw), "fixture", "en", func() error { return nil }); err != nil {
			t.Fatal(err)
		}
		inv, err := s.CurrentBootstrapInvitation(context.Background())
		if err != nil || inv.Secret != encodeSecret([]byte(raw)) || !reflect.DeepEqual(credentials, s.state.Credentials) {
			t.Fatal("transaction did not preserve actual secret/devices")
		}
	}
	selector := transport.E2EEAuthSelector{Kind: transport.E2EEAuthInvitation, ID: "bootstrap", Version: 1}
	result, err := s.CompleteE2EEAuth(context.Background(), selector, true)
	if err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(s.path)
	if err != nil {
		t.Fatal(err)
	}
	if inv, err := s.CurrentBootstrapInvitation(context.Background()); err == nil || inv.Secret != "" {
		t.Fatal("pending invitation was reprinted")
	}
	retry, err := s.CompleteE2EEAuth(context.Background(), selector, true)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(result, retry) {
		t.Fatal("pending retry minted different credential")
	}
	after, err := os.ReadFile(s.path)
	if err != nil || !bytes.Equal(before, after) {
		t.Fatal("pending read/retry wrote")
	}
	// Consumption is driven by the existing credential handshake, never by read.
	id := s.state.Invitation.PendingCredentialID
	if _, err := s.CompleteE2EEAuth(context.Background(), transport.E2EEAuthSelector{Kind: transport.E2EEAuthCredential, ID: id, Version: 1}, true); err != nil {
		t.Fatal(err)
	}
	if inv, err := s.CurrentBootstrapInvitation(context.Background()); err == nil || inv.Secret != "" {
		t.Fatal("consumed invitation reprinted")
	}
}

func TestBootstrapSnapshotUnresolvedTransactionLatch(t *testing.T) {
	s := snapshotFixture(t)
	s.managedArmFault = func(stage string) error {
		if stage != "after-rename" {
			return nil
		}
		// Replace the transaction's inode: rollback must preserve foreign bytes.
		other := filepath.Join(s.dir, "foreign")
		if err := os.WriteFile(other, []byte("synthetic replacement"), 0600); err != nil {
			return err
		}
		if err := os.Rename(other, s.path); err != nil {
			return err
		}
		return errors.New("synthetic failure")
	}
	err := s.ArmBootstrapInvitationTransactional([]byte("synthetic-bootstrap-secret-00002"), "fixture", "en")
	if !errors.Is(err, ErrManagedArmRecovery) || !s.managedUncertain {
		t.Fatal("uncertainty not retained")
	}
	if inv, err := s.CurrentBootstrapInvitation(context.Background()); !errors.Is(err, ErrManagedArmRecovery) || inv.Secret != "" {
		t.Fatal("unresolved read succeeded")
	}
	if err := s.ArmBootstrapInvitationTransactional([]byte("synthetic-bootstrap-secret-00003"), "fixture", "en"); !errors.Is(err, ErrManagedArmRecovery) {
		t.Fatal("automatic uncertainty erasure")
	}
}

func TestBootstrapSnapshotGateRevokedDuringAcquisition(t *testing.T) {
	for _, mode := range []string{"revoke", "suspend", "cancel"} {
		t.Run(mode, func(t *testing.T) {
			s := snapshotFixture(t)
			g := NewBootstrapGate()
			if err := g.Attach(s); err != nil {
				t.Fatal(err)
			}
			if err := g.Open(); err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			s.now = func() time.Time {
				switch mode {
				case "revoke":
					g.Revoke()
				case "suspend":
					g.SuspendAdmission()
				case "cancel":
					cancel()
				}
				return time.Now()
			}
			if inv, err := g.CurrentBootstrapInvitation(ctx); err == nil || inv.Secret != "" {
				t.Fatal("late guard leaked invitation")
			}
		})
	}
}
