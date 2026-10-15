package p1fixture

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestOfflineControlFileLimitUnchanged(t *testing.T) {
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(root, 0700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, "identity.json")
	payload := bytes.Repeat([]byte("x"), 512*1024)
	if err := os.WriteFile(path, payload, 0600); err != nil {
		t.Fatal(err)
	}
	if data, err := ReadPrivate(path, os.Getuid()); err != nil || !bytes.Equal(data, payload) {
		t.Fatalf("control boundary rejected: %v", err)
	}
	if err := os.WriteFile(path, append(payload, 'x'), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadPrivate(path, os.Getuid()); !errors.Is(err, ErrContract) {
		t.Fatalf("oversized control read admitted: %v", err)
	}
	if _, err := Validate(strings.Repeat(" ", 512*1024+1), func(string) string {
		t.Fatal("oversized control input reached environment validation")
		return ""
	}); !errors.Is(err, ErrContract) {
		t.Fatalf("oversized control input admitted: %v", err)
	}
}
