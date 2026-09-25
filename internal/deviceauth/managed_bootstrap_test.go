package deviceauth

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func managedArmStoreFixture(t *testing.T, existing bool) (string, *Store, []byte) {
	t.Helper()
	root := t.TempDir()
	dir := filepath.Join(root, "device-auth")
	var prior []byte
	if existing {
		if err := os.Mkdir(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		prior, _ = json.Marshal(diskState{SchemaVersion: storeSchemaVersion, Credentials: []credentialRecord{{
			Credential: Credential{
				DeviceID: "device-1", CredentialID: "credential-1", Name: "existing", Role: RoleController,
				Locale: "en", PairedAt: time.Unix(100, 0).UTC(), Version: 1,
			},
			Secret: base64.RawURLEncoding.EncodeToString(bytes.Repeat([]byte("s"), secretBytes)),
		}}})
		prior = append(prior, '\n')
		if err := os.WriteFile(filepath.Join(dir, storeFilename), prior, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	store, err := OpenDeferred(dir)
	if err != nil {
		t.Fatalf("OpenDeferred: %v", err)
	}
	return dir, store, prior
}

func TestOpenDeferredPreservesAbsenceAndExistingDeviceState(t *testing.T) {
	for _, existing := range []bool{false, true} {
		t.Run(map[bool]string{false: "absent", true: "existing"}[existing], func(t *testing.T) {
			dir, store, before := managedArmStoreFixture(t, existing)
			if existing {
				after, err := os.ReadFile(filepath.Join(dir, storeFilename))
				if err != nil || !bytes.Equal(after, before) {
					t.Fatalf("read-only open changed file: bytes=%q err=%v", after, err)
				}
				info, err := os.Stat(dir)
				if err != nil || info.Mode().Perm() != 0o700 {
					t.Fatalf("directory mode after read-only open = %v, err=%v", info, err)
				}
				info, err = os.Stat(filepath.Join(dir, storeFilename))
				if err != nil || info.Mode().Perm() != 0o600 {
					t.Fatalf("file mode after read-only open = %v, err=%v", info, err)
				}
			} else if _, err := os.Lstat(dir); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("read-only open created device directory: %v", err)
			}
			if store == nil {
				t.Fatal("read-only open returned nil store")
			}
		})
	}
}

func TestManagedArmPersistenceFailureRestoresExactPriorState(t *testing.T) {
	for _, existing := range []bool{false, true} {
		t.Run(map[bool]string{false: "absent", true: "existing"}[existing], func(t *testing.T) {
			dir, store, prior := managedArmStoreFixture(t, existing)
			store.managedArmFault = func(stage string) error {
				if stage == "after-rename" {
					return errors.New("injected directory sync failure")
				}
				return nil
			}
			if err := store.ArmBootstrapInvitationTransactional(bytes.Repeat([]byte("k"), secretBytes), "relay", "en"); err == nil {
				t.Fatal("injected persistence failure was ignored")
			}
			if existing {
				after, err := os.ReadFile(filepath.Join(dir, storeFilename))
				if err != nil || !bytes.Equal(after, prior) {
					t.Fatalf("rollback did not restore exact bytes: got=%q err=%v", after, err)
				}
				dirInfo, dirErr := os.Stat(dir)
				fileInfo, fileErr := os.Stat(filepath.Join(dir, storeFilename))
				if dirErr != nil || fileErr != nil || dirInfo.Mode().Perm() != 0o700 || fileInfo.Mode().Perm() != 0o600 {
					t.Fatalf("rollback changed modes: dir=%v file=%v errors=%v/%v", dirInfo, fileInfo, dirErr, fileErr)
				}
			} else if _, err := os.Lstat(dir); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("rollback did not restore directory absence: %v", err)
			}
		})
	}
}

func TestManagedArmFinalAdmissionFailureRestoresExactPriorState(t *testing.T) {
	for _, existing := range []bool{false, true} {
		t.Run(map[bool]string{false: "absent", true: "existing"}[existing], func(t *testing.T) {
			dir, store, prior := managedArmStoreFixture(t, existing)
			admissionErr := errors.New("owner was revoked before gate linearization")
			err := store.ArmBootstrapInvitationTransactional(bytes.Repeat([]byte("k"), secretBytes), "relay", "en", func() error {
				return admissionErr
			})
			if !errors.Is(err, admissionErr) || errors.Is(err, ErrManagedArmRecovery) {
				t.Fatalf("final admission rollback error = %v", err)
			}
			if existing {
				after, readErr := os.ReadFile(filepath.Join(dir, storeFilename))
				if readErr != nil || !bytes.Equal(after, prior) {
					t.Fatalf("final admission failure changed existing bytes: got=%q err=%v", after, readErr)
				}
				fileInfo, fileErr := os.Stat(filepath.Join(dir, storeFilename))
				if fileErr != nil || fileInfo.Mode().Perm() != 0o600 {
					t.Fatalf("final admission failure changed existing mode: info=%v err=%v", fileInfo, fileErr)
				}
			} else if _, statErr := os.Lstat(dir); !errors.Is(statErr, os.ErrNotExist) {
				t.Fatalf("final admission failure did not restore absence: %v", statErr)
			}
		})
	}
}

func TestManagedArmRollbackPreservesReplacedTargetAndRequiresRecovery(t *testing.T) {
	dir, store, _ := managedArmStoreFixture(t, false)
	store.managedArmFault = func(stage string) error {
		if stage != "after-rename" {
			return nil
		}
		foreign := filepath.Join(dir, "foreign.tmp")
		if err := os.WriteFile(foreign, []byte("foreign replacement"), 0o600); err != nil {
			return err
		}
		if err := os.Rename(foreign, filepath.Join(dir, storeFilename)); err != nil {
			return err
		}
		return errors.New("injected failure after foreign replacement")
	}
	err := store.ArmBootstrapInvitationTransactional(bytes.Repeat([]byte("k"), secretBytes), "relay", "en")
	if !errors.Is(err, ErrManagedArmRecovery) {
		t.Fatalf("rollback error = %v, want explicit recovery error", err)
	}
	got, readErr := os.ReadFile(filepath.Join(dir, storeFilename))
	if readErr != nil || string(got) != "foreign replacement" {
		t.Fatalf("foreign replacement was not preserved: %q, %v", got, readErr)
	}
}

func TestBootstrapGateFinalAdmissionFailureKeepsGateClosed(t *testing.T) {
	dir, store, _ := managedArmStoreFixture(t, false)
	gate := NewBootstrapGate()
	if err := gate.Attach(store); err != nil {
		t.Fatal(err)
	}
	admissionErr := errors.New("arm context expired before commit")
	err := gate.ArmBootstrapInvitation(bytes.Repeat([]byte("k"), secretBytes), "relay", "en", nil, func() error {
		return admissionErr
	})
	if !errors.Is(err, admissionErr) || gate.OpenStatus() {
		t.Fatalf("final admission result = %v, open=%t", err, gate.OpenStatus())
	}
	if _, err := os.Lstat(dir); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("failed final admission left a device store behind: %v", err)
	}
}

func TestBootstrapGateArmCommitSerializesWithRevoke(t *testing.T) {
	dir, store, _ := managedArmStoreFixture(t, false)
	gate := NewBootstrapGate()
	if err := gate.Attach(store); err != nil {
		t.Fatal(err)
	}
	admitStarted := make(chan struct{})
	admitRelease := make(chan struct{})
	armDone := make(chan error, 1)
	go func() {
		armDone <- gate.ArmBootstrapInvitation(bytes.Repeat([]byte("k"), secretBytes), "relay", "en", func() error {
			close(admitStarted)
			<-admitRelease
			return nil
		})
	}()
	<-admitStarted
	revokeStarted := make(chan struct{})
	revokeDone := make(chan struct{})
	go func() {
		close(revokeStarted)
		gate.Revoke()
		close(revokeDone)
	}()
	<-revokeStarted
	select {
	case <-revokeDone:
		t.Fatal("Revoke crossed the serialized durable-arm operation")
	case <-time.After(10 * time.Millisecond):
	}
	close(admitRelease)
	if err := <-armDone; err != nil {
		t.Fatalf("arm: %v", err)
	}
	select {
	case <-revokeDone:
	case <-time.After(time.Second):
		t.Fatal("Revoke did not complete after arm linearized")
	}
	if gate.OpenStatus() {
		t.Fatal("revocation left the invitation gate open")
	}
	if _, err := os.Stat(filepath.Join(dir, storeFilename)); err != nil {
		t.Fatalf("successful arm was rolled back after revoke: %v", err)
	}
	if err := gate.ArmBootstrapInvitation(bytes.Repeat([]byte("n"), secretBytes), "relay", "en", nil); !errors.Is(err, ErrBootstrapGateClosed) {
		t.Fatalf("revoked gate admitted another arm: %v", err)
	}
}
