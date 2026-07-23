package appdeploy

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestManagerRejectsPhoneOverrides(t *testing.T) {
	root := t.TempDir()
	nodeDir := filepath.Join(root, "node")
	webRoot := filepath.Join(root, "web")
	if err := os.MkdirAll(nodeDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(webRoot, 0o755); err != nil {
		t.Fatal(err)
	}
	npx := filepath.Join(root, "npx")
	for _, filename := range []string{npx, filepath.Join(nodeDir, "node")} {
		if err := os.WriteFile(filename, []byte("#!/bin/sh\n"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(webRoot, "version.json"), []byte(`{"release_version":"1.2.3","revision":"abc"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HERDR_APP_DEPLOY_ORIGIN", "https://app.example.test")
	t.Setenv("HERDR_CLOUDFLARE_PAGES_PROJECT", "relay-app")
	t.Setenv("HERDR_CLOUDFLARE_PAGES_BRANCH", "main")
	t.Setenv("HERDR_APP_DEPLOY_NPX", npx)
	t.Setenv("HERDR_APP_DEPLOY_NODE_DIR", nodeDir)
	manager := NewManager(root, webRoot, "1.2.3", "abc")
	manager.launch = func(context.Context, string) error { return nil }
	if !manager.State().Configured {
		t.Fatalf("state = %#v", manager.State())
	}
	if _, _, err := manager.Schedule(context.Background(), "1.2.3", "abc", "https://other.example.test"); err == nil {
		t.Fatal("phone origin override was accepted")
	}
}
