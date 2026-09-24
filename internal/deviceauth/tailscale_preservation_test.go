package deviceauth

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/0cv/herdr-mobile-relay/internal/transport"
)

func s2Enroll(t *testing.T, s *Store, selector transport.E2EEAuthSelector) transport.E2EEAuthResult {
	t.Helper()
	if _, err := s.ResolveE2EESecret(context.Background(), selector); err != nil {
		t.Fatal(err)
	}
	result, err := s.CompleteE2EEAuth(context.Background(), selector, true)
	if err != nil {
		t.Fatal(err)
	}
	credential := transport.E2EEAuthSelector{Kind: transport.E2EEAuthCredential, ID: result.Identity.CredentialID, Version: result.Identity.CredentialVersion}
	if _, err := s.CompleteE2EEAuth(context.Background(), credential, true); err != nil {
		t.Fatal(err)
	}
	return result
}

func s2Seed(t *testing.T) (*Store, []transport.E2EEAuthResult) {
	t.Helper()
	s, err := Open(testDeviceStoreDir(t))
	if err != nil {
		t.Fatal(err)
	}
	key := []byte(strings.Repeat("b", 32))
	if err := s.EnsureBootstrapInvitation(key, "first", "en"); err != nil {
		t.Fatal(err)
	}
	first := s2Enroll(t, s, transport.E2EEAuthSelector{Kind: transport.E2EEAuthInvitation, ID: "bootstrap", Version: 1})
	invitation, err := s.CreateInvitation("second", RoleReader, "en")
	if err != nil {
		t.Fatal(err)
	}
	second := s2Enroll(t, s, transport.E2EEAuthSelector{Kind: transport.E2EEAuthInvitation, ID: invitation.InvitationID, Version: invitation.Version})
	if first.Identity.DeviceID == second.Identity.DeviceID || first.Identity.CredentialID == second.Identity.CredentialID || bytes.Equal(first.CredentialSecret, second.CredentialSecret) {
		t.Fatal("fixtures are not distinct")
	}
	return s, []transport.E2EEAuthResult{first, second}
}

func s2Credentials(t *testing.T, s *Store, originals []transport.E2EEAuthResult) {
	t.Helper()
	for _, original := range originals {
		id := original.Identity
		c, ok := s.AuthorizeCredential(id.CredentialID, id.CredentialVersion)
		if !ok || c.DeviceID != id.DeviceID || string(c.Role) != id.Role {
			t.Fatal("prior credential identity/version/role lost")
		}
		secret, err := s.ResolveE2EESecret(context.Background(), transport.E2EEAuthSelector{Kind: transport.E2EEAuthCredential, ID: id.CredentialID, Version: id.CredentialVersion})
		if err != nil || !bytes.Equal(secret, original.CredentialSecret) {
			t.Fatal("prior credential secret no longer resolves")
		}
	}
}

func TestTailscaleS2ArmPreservesCredentials(t *testing.T) {
	s, originals := s2Seed(t)
	key := []byte(strings.Repeat("b", 32))
	if err := s.EnsureBootstrapInvitation(key, "stable", "en"); err != nil {
		t.Fatal(err)
	}
	if s.BootstrapStatus().Armed {
		t.Fatal("stable ensure resurrected consumed invitation")
	}
	before := s.ListCredentials("")
	if err := s.ArmBootstrapInvitation(key, "third", "en"); err != nil {
		t.Fatal(err)
	}
	s2Credentials(t, s, originals)
	if !reflect.DeepEqual(before, s.ListCredentials("")) {
		t.Fatal("arm changed credential records")
	}
	reopened, err := Open(s.dir)
	if err != nil {
		t.Fatal(err)
	}
	if !reopened.BootstrapStatus().Armed {
		t.Fatal("invitation not persisted")
	}
	s2Credentials(t, reopened, originals)
	selector := transport.E2EEAuthSelector{Kind: transport.E2EEAuthInvitation, ID: "bootstrap", Version: 1}
	s2Enroll(t, reopened, selector)
	if _, err := reopened.CompleteE2EEAuth(context.Background(), selector, true); err == nil {
		t.Fatal("consumed invitation reused")
	}
	s2Credentials(t, reopened, originals)
	again, err := Open(s.dir)
	if err != nil {
		t.Fatal(err)
	}
	s2Credentials(t, again, originals)
}

func TestTailscaleS2ArmPersistenceFailure(t *testing.T) {
	s, originals := s2Seed(t)
	key := []byte(strings.Repeat("b", 32))
	if err := s.ArmBootstrapInvitation(key, "prior", "en"); err != nil {
		t.Fatal(err)
	}
	prior := *s.state.Invitation
	disk, err := os.ReadFile(s.path)
	if err != nil {
		t.Fatal(err)
	}
	// CreateTemp fails with ENOTDIR before any publication, even as root.
	// This does not establish post-rename/fsync or crash rollback.
	obstacle := filepath.Join(t.TempDir(), "not-a-directory")
	if err := os.WriteFile(obstacle, []byte("fixture"), 0600); err != nil {
		t.Fatal(err)
	}
	dir := s.dir
	s.dir = obstacle
	err = s.ArmBootstrapInvitation(key, "replacement", "en")
	s.dir = dir
	if err == nil {
		t.Fatal("failed persistence reported success")
	}
	if !reflect.DeepEqual(prior, *s.state.Invitation) {
		t.Fatal("prior invitation not restored")
	}
	after, err := os.ReadFile(s.path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(disk, after) {
		t.Fatal("failed persistence changed disk")
	}
	s2Credentials(t, s, originals)
	for _, invalid := range []struct {
		key  []byte
		name string
	}{{nil, "valid"}, {key, ""}} {
		if err := s.ArmBootstrapInvitation(invalid.key, invalid.name, "en"); err == nil {
			t.Fatal("invalid input accepted")
		}
		if !reflect.DeepEqual(prior, *s.state.Invitation) {
			t.Fatal("invalid input mutated invitation")
		}
	}
	if err := s.ArmBootstrapInvitation(key, "recovered", "en"); err != nil {
		t.Fatal(err)
	}
	reopened, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if !reopened.BootstrapStatus().Armed || reopened.state.Invitation.Name != "recovered" {
		t.Fatal("recovered arm not persisted")
	}
	s2Credentials(t, reopened, originals)
}
