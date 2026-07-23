package upload

import (
	"encoding/base64"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestStoreValidPNG(t *testing.T) {
	dir := t.TempDir()
	data := "data:image/png;base64," + base64.StdEncoding.EncodeToString([]byte("fake-png-data"))

	res := Store(dir, "screenshot.png", "image/png", data)
	if !res.OK {
		t.Fatalf("expected OK, got error: %s", res.Error)
	}
	if res.Path == "" {
		t.Fatal("path is empty")
	}
	if !strings.HasPrefix(res.Path, dir) {
		t.Errorf("path %q not under dir %q", res.Path, dir)
	}
	if !strings.HasSuffix(res.Path, ".png") {
		t.Errorf("path %q doesn't end in .png", res.Path)
	}

	content, err := os.ReadFile(res.Path)
	if err != nil {
		t.Fatalf("read stored file: %v", err)
	}
	if string(content) != "fake-png-data" {
		t.Errorf("content = %q", content)
	}

	info, _ := os.Stat(res.Path)
	if info.Mode().Perm() != 0o600 {
		t.Errorf("perm = %o, want 600", info.Mode().Perm())
	}
}

func TestStoreRejectsNonImageMime(t *testing.T) {
	dir := t.TempDir()
	res := Store(dir, "evil.exe", "application/octet-stream", "data:application/octet-stream;base64,AAAA")
	if res.OK {
		t.Error("should reject non-image mime")
	}
	if res.Error != "unsupported file type" {
		t.Errorf("error = %q", res.Error)
	}
}

func TestStoreRejectsInvalidBase64(t *testing.T) {
	dir := t.TempDir()
	res := Store(dir, "img.png", "image/png", "data:image/png;base64,not-valid-base64!!!")
	if res.OK {
		t.Error("should reject invalid base64")
	}
}

func TestStoreRejectsOversized(t *testing.T) {
	dir := t.TempDir()
	big := make([]byte, MaxBytes+1)
	data := "data:image/png;base64," + base64.StdEncoding.EncodeToString(big)

	res := Store(dir, "big.png", "image/png", data)
	if res.OK {
		t.Error("should reject oversized image")
	}
	if !strings.Contains(res.Error, "larger than") {
		t.Errorf("error = %q", res.Error)
	}
}

func TestStoreRejectsMissingBase64Marker(t *testing.T) {
	dir := t.TempDir()
	res := Store(dir, "img.png", "image/png", "data:image/png,AAAA")
	if res.OK {
		t.Error("should reject data URL without ;base64 marker")
	}
}

func TestStoreRawBase64WithoutDataURL(t *testing.T) {
	dir := t.TempDir()
	raw := base64.StdEncoding.EncodeToString([]byte("raw-data"))
	res := Store(dir, "img.jpg", "image/jpeg", raw)
	if !res.OK {
		t.Fatalf("raw base64 should work: %s", res.Error)
	}
	if !strings.HasSuffix(res.Path, ".jpg") {
		t.Errorf("path = %q", res.Path)
	}
}

func TestSafeStemSanitization(t *testing.T) {
	dir := t.TempDir()
	data := base64.StdEncoding.EncodeToString([]byte("x"))
	res := Store(dir, "../../etc/passwd.png", "image/png", data)
	if !res.OK {
		t.Fatalf("store failed: %s", res.Error)
	}
	// Path must stay inside dir — no traversal
	if !strings.HasPrefix(res.Path, dir) {
		t.Errorf("path traversal detected: %q", res.Path)
	}
	base := filepath.Base(res.Path)
	if strings.Contains(base, "..") || strings.Contains(base, "/") {
		t.Errorf("unsafe filename: %q", base)
	}
}

func TestPrune(t *testing.T) {
	dir := t.TempDir()

	// Create an old file
	old := filepath.Join(dir, "old-file.png")
	os.WriteFile(old, []byte("old"), 0o600)
	oldTime := time.Now().AddDate(0, 0, -10)
	os.Chtimes(old, oldTime, oldTime)

	// Create a recent file
	recent := filepath.Join(dir, "recent-file.png")
	os.WriteFile(recent, []byte("new"), 0o600)

	removed := Prune(dir)
	if removed != 1 {
		t.Errorf("removed = %d, want 1", removed)
	}
	if _, err := os.Stat(old); !os.IsNotExist(err) {
		t.Error("old file should be deleted")
	}
	if _, err := os.Stat(recent); err != nil {
		t.Error("recent file should survive")
	}
}

func TestExtensionFallback(t *testing.T) {
	dir := t.TempDir()
	data := base64.StdEncoding.EncodeToString([]byte("x"))

	res := Store(dir, "photo.unknown", "image/webp", data)
	if !res.OK {
		t.Fatal(res.Error)
	}
	if !strings.HasSuffix(res.Path, ".webp") {
		t.Errorf("mime should win: %q", res.Path)
	}
}
