package app

import (
	"bytes"
	"encoding/json"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/0cv/herdr-mobile-relay/internal/config"
	"github.com/0cv/herdr-mobile-relay/internal/managedstate"
)

func managedTestLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func managedTestRoot(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	if err := os.Chmod(root, 0o700); err != nil {
		t.Fatalf("chmod test root: %v", err)
	}
	return root
}

func managedTestConfig(root string) *config.Config {
	return &config.Config{
		Host:              "127.0.0.1",
		Port:              18991,
		SocketPath:        filepath.Join(root, "herdr.sock"),
		PollInterval:      3600,
		RuntimeDir:        root,
		CacheDir:          filepath.Join(root, "cache"),
		ConfigHome:        filepath.Join(root, "config"),
		Token:             strings.Repeat("k", 32),
		ManagedRunID:      "run-managed",
		PairingSocketPath: filepath.Join(root, "pairing.sock"),
	}
}

func TestManagedOwnerAcquireRefusesBusyRoot(t *testing.T) {
	root := managedTestRoot(t)
	heldRoot, err := managedstate.OpenExistingRoot(root)
	if err != nil {
		t.Fatalf("open existing root: %v", err)
	}
	heldOwner, err := heldRoot.TryAcquireOwner()
	if err != nil {
		t.Fatalf("pre-acquire owner: %v", err)
	}
	if err := heldOwner.PublishRecord(); err != nil {
		t.Fatalf("publish pre-acquired owner: %v", err)
	}
	defer heldOwner.Close()

	lockDir := filepath.Join(root, "owner.lock")
	recordPath := filepath.Join(lockDir, "owner.json")
	before, err := os.ReadFile(recordPath)
	if err != nil {
		t.Fatalf("read pre-acquired record: %v", err)
	}

	owner, err := AcquireManagedOwner(root)
	if err == nil {
		t.Fatalf("AcquireManagedOwner succeeded on a busy root: %+v", owner)
	}
	if !strings.Contains(err.Error(), "Busy") {
		t.Fatalf("busy refusal %q does not name Busy", err.Error())
	}

	after, err := os.ReadFile(recordPath)
	if err != nil {
		t.Fatalf("pre-existing record was removed: %v", err)
	}
	if !bytes.Equal(before, after) {
		t.Fatal("pre-existing owner record bytes changed")
	}
	info, err := os.Lstat(lockDir)
	if err != nil {
		t.Fatalf("pre-existing lock directory was removed: %v", err)
	}
	if !info.IsDir() || info.Mode().Perm() != 0o700 {
		t.Fatalf("pre-existing lock directory mutated: mode %v", info.Mode())
	}
}

func TestManagedOwnerAcquirePublishesAndValidates(t *testing.T) {
	root := managedTestRoot(t)
	owner, err := AcquireManagedOwner(root)
	if err != nil {
		t.Fatalf("acquire managed owner: %v", err)
	}
	want, err := filepath.EvalSymlinks(root)
	if err != nil {
		t.Fatalf("resolve canonical root: %v", err)
	}
	if got := owner.Directory(); got != want {
		t.Fatalf("Directory() = %q, want canonical %q", got, want)
	}
	if err := owner.Validate(); err != nil {
		t.Fatalf("Validate() = %v, want nil", err)
	}

	lockInfo, err := os.Lstat(filepath.Join(root, "owner.lock"))
	if err != nil {
		t.Fatalf("owner.lock missing: %v", err)
	}
	if !lockInfo.IsDir() || lockInfo.Mode().Perm() != 0o700 {
		t.Fatalf("owner.lock mode = %v, want 0700 directory", lockInfo.Mode())
	}
	recordInfo, err := os.Lstat(filepath.Join(root, "owner.lock", "owner.json"))
	if err != nil {
		t.Fatalf("owner.json missing: %v", err)
	}
	if !recordInfo.Mode().IsRegular() || recordInfo.Mode().Perm() != 0o600 {
		t.Fatalf("owner.json mode = %v, want 0600 regular file", recordInfo.Mode())
	}

	if err := owner.Close(); err != nil {
		t.Fatalf("first Close() = %v", err)
	}
	if err := owner.Close(); err != nil {
		t.Fatalf("second Close() = %v, want nil (idempotent)", err)
	}
}

func TestRetireManagedOwnerReleasesAndPreservesOnTamper(t *testing.T) {
	tamperedRoot := managedTestRoot(t)
	tampered, err := AcquireManagedOwner(tamperedRoot)
	if err != nil {
		t.Fatalf("acquire for tamper: %v", err)
	}
	recordPath := filepath.Join(tamperedRoot, "owner.lock", "owner.json")
	raw, err := os.ReadFile(recordPath)
	if err != nil {
		t.Fatalf("read record: %v", err)
	}
	var decoded map[string]any
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatalf("decode record: %v", err)
	}
	decoded["nonce"] = strings.Repeat("0", 64)
	tamperedBytes, err := json.Marshal(decoded)
	if err != nil {
		t.Fatalf("re-encode tampered record: %v", err)
	}
	if err := os.WriteFile(recordPath, tamperedBytes, 0o600); err != nil {
		t.Fatalf("write tampered record: %v", err)
	}

	if err := RetireManagedOwner(tampered, managedTestLogger()); err == nil {
		t.Fatal("RetireManagedOwner accepted a tampered record")
	}
	if _, err := os.Lstat(recordPath); err != nil {
		t.Fatalf("tampered record was removed: %v", err)
	}
	if _, err := os.Lstat(filepath.Join(tamperedRoot, "owner.lock")); err != nil {
		t.Fatalf("tampered lock directory was removed: %v", err)
	}

	cleanRoot := managedTestRoot(t)
	clean, err := AcquireManagedOwner(cleanRoot)
	if err != nil {
		t.Fatalf("clean acquire: %v", err)
	}
	if err := RetireManagedOwner(clean, managedTestLogger()); err != nil {
		t.Fatalf("clean retire: %v", err)
	}
	if _, err := os.Lstat(filepath.Join(cleanRoot, "owner.lock")); !os.IsNotExist(err) {
		t.Fatalf("clean retire left owner.lock: %v", err)
	}
	if _, err := os.Lstat(filepath.Join(cleanRoot, "generation")); err != nil {
		t.Fatalf("clean retire removed generation file: %v", err)
	}
}

func TestNewOwnedRefusesWithoutOwnerInManagedMode(t *testing.T) {
	root := managedTestRoot(t)
	cfg := managedTestConfig(root)

	srv, err := NewOwned(cfg, "0.9.0", "rev", managedTestLogger(), nil)
	if err == nil {
		t.Fatalf("NewOwned accepted a managed run without an owner: %+v", srv)
	}
	if srv != nil {
		t.Fatal("NewOwned returned a server alongside an error")
	}
	if _, statErr := os.Lstat(filepath.Join(root, "device-auth")); !os.IsNotExist(statErr) {
		t.Fatalf("refused admission created device-auth: %v", statErr)
	}
	if _, statErr := os.Lstat(cfg.PairingSocketPath); !os.IsNotExist(statErr) {
		t.Fatalf("refused admission created a control socket: %v", statErr)
	}
}

func TestNewOwnedRefusesForeignOwnerDirectory(t *testing.T) {
	rootA := managedTestRoot(t)
	rootB := managedTestRoot(t)
	owner, err := AcquireManagedOwner(rootB)
	if err != nil {
		t.Fatalf("acquire owner on B: %v", err)
	}
	cfg := managedTestConfig(rootA)

	srv, err := NewOwned(cfg, "0.9.0", "rev", managedTestLogger(), owner)
	if err == nil {
		t.Fatalf("NewOwned accepted a foreign owner: %+v", srv)
	}
	if srv != nil {
		t.Fatal("NewOwned returned a server alongside an error")
	}
	if _, statErr := os.Lstat(filepath.Join(rootA, "device-auth")); !os.IsNotExist(statErr) {
		t.Fatalf("refused admission created device-auth in root A: %v", statErr)
	}
	if err := owner.Validate(); err != nil {
		t.Fatalf("owner B was invalidated by the refusal: %v", err)
	}
}

func TestNewOwnedRefusesSocketOutsideAdmittedRoot(t *testing.T) {
	rootA := managedTestRoot(t)
	socketRoot := managedTestRoot(t)
	owner, err := AcquireManagedOwner(rootA)
	if err != nil {
		t.Fatalf("acquire owner on A: %v", err)
	}
	cfg := managedTestConfig(rootA)
	cfg.PairingSocketPath = filepath.Join(socketRoot, "pairing.sock")

	srv, err := NewOwned(cfg, "0.9.0", "rev", managedTestLogger(), owner)
	if err == nil {
		t.Fatalf("NewOwned accepted a socket outside the admitted root: %+v", srv)
	}
	if srv != nil {
		t.Fatal("NewOwned returned a server alongside an error")
	}
	if _, statErr := os.Lstat(filepath.Join(rootA, "device-auth")); !os.IsNotExist(statErr) {
		t.Fatalf("refused admission created device-auth in root A: %v", statErr)
	}
	if _, statErr := os.Lstat(cfg.PairingSocketPath); !os.IsNotExist(statErr) {
		t.Fatalf("refused admission created a control socket: %v", statErr)
	}
}

func TestNewOwnedManagedModeCreatesDeviceStoreAfterAdmission(t *testing.T) {
	root := managedTestRoot(t)
	owner, err := AcquireManagedOwner(root)
	if err != nil {
		t.Fatalf("acquire managed owner: %v", err)
	}
	cfg := managedTestConfig(root)

	srv, err := NewOwned(cfg, "0.9.0", "rev", managedTestLogger(), owner)
	if err != nil {
		t.Fatalf("NewOwned managed mode: %v", err)
	}
	if srv == nil {
		t.Fatal("NewOwned returned a nil server without error")
	}
	recordPath := filepath.Join(root, "device-auth", "devices.json")
	info, err := os.Lstat(recordPath)
	if err != nil {
		t.Fatalf("device store was not created after admission: %v", err)
	}
	if !info.Mode().IsRegular() || info.Mode().Perm() != 0o600 {
		t.Fatalf("devices.json mode = %v, want 0600 regular file", info.Mode())
	}
	if err := owner.Validate(); err != nil {
		t.Fatalf("owner invalidated after admission: %v", err)
	}

	second, err := AcquireManagedOwner(root)
	if err == nil {
		t.Fatalf("second acquisition succeeded while ownership is held: %+v", second)
	}
	if !strings.Contains(err.Error(), "Busy") {
		t.Fatalf("second-acquisition refusal %q does not name Busy", err.Error())
	}
}

func TestNewOwnedLegacyModeUnchanged(t *testing.T) {
	root := managedTestRoot(t)
	legacy := &config.Config{
		Host:         "127.0.0.1",
		Port:         18992,
		SocketPath:   filepath.Join(root, "herdr.sock"),
		PollInterval: 3600,
		RuntimeDir:   root,
		CacheDir:     filepath.Join(root, "cache"),
		ConfigHome:   filepath.Join(root, "config"),
		Token:        strings.Repeat("k", 32),
	}

	srv, err := NewOwned(legacy, "0.9.0", "rev", managedTestLogger(), nil)
	if err != nil {
		t.Fatalf("legacy NewOwned with nil owner: %v", err)
	}
	if srv == nil {
		t.Fatal("legacy NewOwned returned a nil server without error")
	}
	recordPath := filepath.Join(root, "device-auth", "devices.json")
	info, err := os.Lstat(recordPath)
	if err != nil {
		t.Fatalf("legacy device store was not created: %v", err)
	}
	if !info.Mode().IsRegular() || info.Mode().Perm() != 0o600 {
		t.Fatalf("legacy devices.json mode = %v, want 0600 regular file", info.Mode())
	}

	ownerRoot := managedTestRoot(t)
	owner, err := AcquireManagedOwner(ownerRoot)
	if err != nil {
		t.Fatalf("acquire owner for legacy misuse: %v", err)
	}
	if _, err := NewOwned(legacy, "0.9.0", "rev", managedTestLogger(), owner); err == nil {
		t.Fatal("legacy NewOwned accepted a non-nil owner")
	}
}
