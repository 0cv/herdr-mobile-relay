package appdeploy

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestValidateRejectsOverridesAndUnpinnedIdentity(t *testing.T) {
	root := t.TempDir()
	nodeDir := filepath.Join(root, "node")
	if err := os.MkdirAll(nodeDir, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{filepath.Join(root, "npx"), filepath.Join(nodeDir, "node")} {
		if err := os.WriteFile(name, []byte("#!/bin/sh\n"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	web := filepath.Join(root, "web")
	if err := os.MkdirAll(web, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(web, "version.json"), []byte(`{"release_version":"1.2.3","revision":"abc"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	job := Job{
		RuntimeDir: root,
		WebRoot:    web,
		Origin:     "https://example.test",
		Project:    "relay-app",
		Branch:     "main",
		Version:    "1.2.3",
		Revision:   "abc",
		NPXPath:    filepath.Join(root, "npx"),
		NodeDir:    nodeDir,
	}
	if err := validate(job); err != nil {
		t.Fatal(err)
	}
	job.Origin = "https://example.test/override"
	if err := validate(job); err == nil {
		t.Fatal("origin with path accepted")
	}
	job.Origin = "https://example.test"
	job.Branch = "../preview"
	if err := validate(job); err == nil {
		t.Fatal("unsafe branch accepted")
	}
}

func TestCommandEnvironmentPinsOneNodeFirstPath(t *testing.T) {
	environment := commandEnvironment("/opt/pinned-node", []string{
		"HOME=/tmp/home",
		"PATH=/usr/local/bin:/usr/bin",
		"TOKEN=secret",
	})
	pathCount := 0
	for _, value := range environment {
		if strings.HasPrefix(value, "PATH=") {
			pathCount++
			if value != "PATH=/opt/pinned-node"+string(os.PathListSeparator)+"/usr/local/bin:/usr/bin" {
				t.Fatalf("PATH = %q", value)
			}
		}
	}
	if pathCount != 1 {
		t.Fatalf("PATH entries = %d, want 1", pathCount)
	}
}
