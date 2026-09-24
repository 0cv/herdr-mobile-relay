package deviceauth

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

var managedIdentityValidStore = []byte("{\"schema_version\":1,\"credentials\":[]}\n")

func managedIdentityStoreDir(t *testing.T) (string, string) {
	t.Helper()
	root := t.TempDir()
	dir := filepath.Join(root, "device-auth")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatalf("create store directory: %v", err)
	}
	return dir, filepath.Join(dir, "devices.json")
}

func TestDeviceAuthRejectsSymlinkedStoreDirectory(t *testing.T) {
	root := t.TempDir()
	target := filepath.Join(root, "missing-target")
	link := filepath.Join(root, "device-auth")
	if err := os.Symlink(target, link); err != nil {
		t.Fatalf("create directory symlink: %v", err)
	}

	if _, err := Open(link); err == nil {
		t.Fatal("Open accepted a symlinked store directory")
	}
	if _, err := os.Lstat(target); !os.IsNotExist(err) {
		t.Fatalf("symlink target was created: %v", err)
	}
}

func TestDeviceAuthRejectsInsecureStoreDirectoryMode(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "device-auth")
	if err := os.Mkdir(dir, 0o755); err != nil {
		t.Fatalf("create store directory: %v", err)
	}
	if err := os.Chmod(dir, 0o755); err != nil {
		t.Fatalf("chmod store directory: %v", err)
	}

	if _, err := Open(dir); err == nil {
		t.Fatal("Open accepted a 0755 store directory")
	}
	if _, err := os.Lstat(filepath.Join(dir, "devices.json")); !os.IsNotExist(err) {
		t.Fatalf("Open created devices.json in the refused directory: %v", err)
	}
	info, err := os.Lstat(dir)
	if err != nil {
		t.Fatalf("inspect refused directory: %v", err)
	}
	if info.Mode().Perm() != 0o755 {
		t.Fatalf("refused directory was silently chmodded to %04o", info.Mode().Perm())
	}
}

func TestDeviceAuthRejectsSymlinkedDeviceFile(t *testing.T) {
	dir, recordPath := managedIdentityStoreDir(t)
	target := filepath.Join(t.TempDir(), "real-devices.json")
	if err := os.WriteFile(target, managedIdentityValidStore, 0o600); err != nil {
		t.Fatalf("write symlink target: %v", err)
	}
	if err := os.Symlink(target, recordPath); err != nil {
		t.Fatalf("create file symlink: %v", err)
	}

	if _, err := Open(dir); err == nil {
		t.Fatal("Open accepted a symlinked devices.json")
	}
	info, err := os.Lstat(recordPath)
	if err != nil {
		t.Fatalf("symlink was removed: %v", err)
	}
	if info.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("record path is no longer a symlink: %v", info.Mode())
	}
	after, err := os.ReadFile(target)
	if err != nil {
		t.Fatalf("symlink target was removed: %v", err)
	}
	if !bytes.Equal(after, managedIdentityValidStore) {
		t.Fatal("symlink target bytes changed")
	}
}

func TestDeviceAuthRejectsHardLinkedDeviceFile(t *testing.T) {
	dir, recordPath := managedIdentityStoreDir(t)
	if err := os.WriteFile(recordPath, managedIdentityValidStore, 0o600); err != nil {
		t.Fatalf("write device store: %v", err)
	}
	if err := os.Chmod(recordPath, 0o600); err != nil {
		t.Fatalf("chmod device store: %v", err)
	}
	linkPath := filepath.Join(dir, "devices-link.json")
	if err := os.Link(recordPath, linkPath); err != nil {
		t.Fatalf("create hard link: %v", err)
	}
	original, err := os.Lstat(recordPath)
	if err != nil {
		t.Fatalf("inspect original: %v", err)
	}
	linked, err := os.Lstat(linkPath)
	if err != nil {
		t.Fatalf("inspect hard link: %v", err)
	}
	if !os.SameFile(original, linked) {
		t.Fatal("fixture is not a hard link")
	}

	if _, err := Open(dir); err == nil {
		t.Fatal("Open accepted a hard-linked devices.json")
	}
	for _, path := range []string{recordPath, linkPath} {
		info, err := os.Lstat(path)
		if err != nil {
			t.Fatalf("hard-linked name %s was removed: %v", path, err)
		}
		if !info.Mode().IsRegular() {
			t.Fatalf("hard-linked name %s changed type: %v", path, info.Mode())
		}
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read hard-linked name %s: %v", path, err)
		}
		if !bytes.Equal(data, managedIdentityValidStore) {
			t.Fatalf("hard-linked name %s bytes changed", path)
		}
	}
}

func TestDeviceAuthRejectsInsecureDeviceFileMode(t *testing.T) {
	dir, recordPath := managedIdentityStoreDir(t)
	if err := os.WriteFile(recordPath, managedIdentityValidStore, 0o644); err != nil {
		t.Fatalf("write device store: %v", err)
	}
	if err := os.Chmod(recordPath, 0o644); err != nil {
		t.Fatalf("chmod device store: %v", err)
	}

	if _, err := Open(dir); err == nil {
		t.Fatal("Open accepted a 0644 devices.json")
	}
	info, err := os.Lstat(recordPath)
	if err != nil {
		t.Fatalf("device store was removed: %v", err)
	}
	if info.Mode().Perm() != 0o644 {
		t.Fatalf("device store was silently chmodded to %04o", info.Mode().Perm())
	}
	after, err := os.ReadFile(recordPath)
	if err != nil {
		t.Fatalf("read device store: %v", err)
	}
	if !bytes.Equal(after, managedIdentityValidStore) {
		t.Fatal("refused device store was rewritten")
	}
}
