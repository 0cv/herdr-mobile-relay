package deviceauth

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Private fixture files only; no resolver, sockets or external state.
func snapshotFixture(t *testing.T) *Store {
	t.Helper()
	s, err := OpenDeferred(filepath.Join(t.TempDir(), "devices"))
	if err != nil {
		t.Fatal(err)
	}
	if err := s.ArmBootstrapInvitationTransactional([]byte("synthetic-bootstrap-secret-00001"), "fixture", "en"); err != nil {
		t.Fatal(err)
	}
	return s
}

func TestBootstrapSnapshotActualSecretReadOnlyRestart(t *testing.T) {
	s := snapshotFixture(t)
	before, err := os.ReadFile(s.path)
	if err != nil {
		t.Fatal(err)
	}
	restarted, err := OpenDeferred(s.dir)
	if err != nil {
		t.Fatal(err)
	}
	got, err := restarted.CurrentBootstrapInvitation(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if got.Secret != encodeSecret([]byte("synthetic-bootstrap-secret-00001")) || got.InvitationID != "bootstrap" || got.Version != 1 {
		t.Fatal("snapshot differs")
	}
	got.Secret = "changed copy"
	again, err := restarted.CurrentBootstrapInvitation(context.Background())
	if err != nil || again.Secret == got.Secret {
		t.Fatal("alias or failed reread")
	}
	after, err := os.ReadFile(s.path)
	if err != nil || !bytes.Equal(before, after) {
		t.Fatal("read changed store")
	}
	for _, formatted := range []string{fmt.Sprint(again), fmt.Sprintf("%#v", again), fmt.Sprintf("%+v", again)} {
		if strings.Contains(formatted, again.Secret) {
			t.Fatal("secret formatting")
		}
	}
}

func TestBootstrapSnapshotRefusalsPreserveState(t *testing.T) {
	for _, name := range []string{"missing", "expired", "burned", "rate-limited", "pending", "version", "secret", "uncertain", "changed-disk", "cancelled"} {
		t.Run(name, func(t *testing.T) {
			s := snapshotFixture(t)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			switch name {
			case "missing":
				s.state.Invitation = nil
			case "expired":
				s.state.Invitation.ExpiresAt = time.Now().Add(-time.Second)
			case "burned":
				s.state.Invitation.FailedAttempts = maxInviteAttempts
			case "rate-limited":
				s.state.Invitation.NextAttemptAt = time.Now().Add(time.Hour)
			case "pending":
				s.state.Invitation.PendingCredentialID = "synthetic-pending"
			case "version":
				s.state.Invitation.Version = 2
			case "secret":
				s.state.Invitation.Secret = "invalid"
			case "uncertain":
				s.managedUncertain = true
			case "changed-disk":
				if err := os.WriteFile(s.path, []byte("foreign synthetic bytes"), 0600); err != nil {
					t.Fatal(err)
				}
			case "cancelled":
				cancel()
			}
			before, err := os.ReadFile(s.path)
			if err != nil {
				t.Fatal(err)
			}
			prior := s.state.Invitation
			got, err := s.CurrentBootstrapInvitation(ctx)
			if err == nil || got.Secret != "" {
				t.Fatal("refusal exposed invitation")
			}
			after, readErr := os.ReadFile(s.path)
			if readErr != nil || !bytes.Equal(before, after) || prior != s.state.Invitation {
				t.Fatal("refusal mutated state")
			}
		})
	}
}

func TestBootstrapSnapshotGateCancellationRevocationSuspension(t *testing.T) {
	for _, state := range []string{"open", "closed", "revoked", "suspended", "cancelled"} {
		t.Run(state, func(t *testing.T) {
			s := snapshotFixture(t)
			g := NewBootstrapGate()
			if err := g.Attach(s); err != nil {
				t.Fatal(err)
			}
			if state != "closed" {
				if err := g.Open(); err != nil {
					t.Fatal(err)
				}
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			switch state {
			case "revoked":
				g.Revoke()
			case "suspended":
				g.SuspendAdmission()
			case "cancelled":
				cancel()
			}
			before, err := os.ReadFile(s.path)
			if err != nil {
				t.Fatal(err)
			}
			value, err := g.CurrentBootstrapInvitation(ctx)
			if state == "open" {
				if err != nil || value.Secret == "" {
					t.Fatal("open read refused")
				}
			} else if err == nil || value.Secret != "" {
				t.Fatal("gate leaked")
			}
			after, readErr := os.ReadFile(s.path)
			if readErr != nil || !bytes.Equal(before, after) {
				t.Fatal("gate read wrote")
			}
		})
	}
}
