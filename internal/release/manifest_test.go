package release

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func testRelease(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	for name, contents := range map[string]string{
		"herdr-mobile-relay":                  "binary",
		"web/index.html":                      "<html></html>",
		"LICENSE":                             "license",
		"README.md":                           "readme",
		"relay/common.sh":                     "#!/bin/sh\n",
		"relay/herdr-mobile-relay-service.sh": "#!/bin/sh\n",
		"relay/plugin-on-event.sh":            "#!/bin/sh\n",
		"relay/setup-link.sh":                 "#!/bin/sh\n",
		"relay/stable-setup.sh":               "#!/bin/sh\n",
		"relay/stable-teardown.sh":            "#!/bin/sh\n",
		"relay/start.sh":                      "#!/bin/sh\n",
	} {
		path := filepath.Join(root, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		mode := os.FileMode(0o644)
		if name == "herdr-mobile-relay" || strings.HasSuffix(name, ".sh") {
			mode = 0o755
		}
		if err := os.WriteFile(path, []byte(contents), mode); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

func TestBuildAndVerifyManifest(t *testing.T) {
	root := testRelease(t)
	manifest, err := Build(root, "1.2.3", "abc123", "linux/amd64")
	if err != nil {
		t.Fatal(err)
	}
	if manifest.WebHash == "" {
		t.Fatal("web hash is empty")
	}
	verified, err := Verify(root, "linux/amd64")
	if err != nil {
		t.Fatal(err)
	}
	if verified.Version != "1.2.3" {
		t.Fatalf("version = %q", verified.Version)
	}
}

func TestVerifyRejectsTamperingAndUnlistedFiles(t *testing.T) {
	root := testRelease(t)
	if _, err := Build(root, "1.2.3", "abc123", "linux/amd64"); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "web", "index.html"), []byte("changed"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Verify(root, "linux/amd64"); err == nil || !strings.Contains(err.Error(), "hash mismatch") {
		t.Fatalf("tamper error = %v", err)
	}

	root = testRelease(t)
	if _, err := Build(root, "1.2.3", "abc123", "linux/amd64"); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "extra"), []byte("no"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Verify(root, "linux/amd64"); err == nil || !strings.Contains(err.Error(), "not listed") {
		t.Fatalf("unlisted error = %v", err)
	}
}

func TestVerifyRejectsWrongTargetAndSymlink(t *testing.T) {
	root := testRelease(t)
	if _, err := Build(root, "1.2.3", "abc123", "linux/amd64"); err != nil {
		t.Fatal(err)
	}
	if _, err := Verify(root, "darwin/arm64"); err == nil {
		t.Fatal("wrong target was accepted")
	}
	if err := os.Symlink("index.html", filepath.Join(root, "web", "linked")); err != nil {
		t.Fatal(err)
	}
	if _, err := Verify(root, "linux/amd64"); err == nil || !strings.Contains(err.Error(), "symlink") {
		t.Fatalf("symlink error = %v", err)
	}
}

func TestVerifyRejectsInvalidWebHashAndNonExecutableBinary(t *testing.T) {
	root := testRelease(t)
	if _, err := Build(root, "1.2.3", "abc123", "linux/amd64"); err != nil {
		t.Fatal(err)
	}
	manifestPath := filepath.Join(root, ManifestName)
	data, err := os.ReadFile(manifestPath)
	if err != nil {
		t.Fatal(err)
	}
	data = []byte(strings.Replace(string(data), `"web_hash": "`, `"web_hash": "00`, 1))
	if err := os.WriteFile(manifestPath, data, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Verify(root, "linux/amd64"); err == nil || !strings.Contains(err.Error(), "web hash") {
		t.Fatalf("web hash error = %v", err)
	}

	root = testRelease(t)
	if _, err := Build(root, "1.2.3", "abc123", "linux/amd64"); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(filepath.Join(root, "herdr-mobile-relay"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Verify(root, "linux/amd64"); err == nil || !strings.Contains(err.Error(), "not executable") {
		t.Fatalf("executable error = %v", err)
	}
}
