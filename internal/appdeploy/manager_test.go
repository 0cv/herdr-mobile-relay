package appdeploy

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/0cv/herdr-mobile-relay/internal/release"
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
	webHash, err := release.WebHashFS(os.DirFS(webRoot))
	if err != nil {
		t.Fatal(err)
	}
	manifest, err := json.Marshal(release.Manifest{
		Schema:   release.ManifestSchema,
		Version:  "1.2.3",
		Revision: "abc",
		Target:   release.CurrentTarget(),
		WebHash:  webHash,
		Files:    map[string]string{"web/version.json": "unused"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, release.ManifestName), manifest, 0o644); err != nil {
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
	if !manager.Required() {
		t.Fatal("configured app deployment was not marked as required")
	}
	if err := manager.ValidateOrigin("https://app.example.test"); err != nil {
		t.Fatal(err)
	}
	if err := manager.ValidateOrigin("https://other.example.test"); err == nil {
		t.Fatal("phone origin override passed validation")
	}
	if _, _, err := manager.Schedule(context.Background(), "1.2.3", "abc", "https://other.example.test"); err == nil {
		t.Fatal("phone origin override was accepted")
	}
}

func TestAppDeployWorkerLaunchForwardsRelayEnvironmentPaths(t *testing.T) {
	values := map[string]string{
		"HERDR_RELAY_ENV":         "/home/cv/.config/herdr-mobile-relay/relay.env",
		"HERDR_PLUGIN_CONFIG_DIR": "/home/cv/.config/herdr-mobile-relay",
	}
	lookup := func(key string) (string, bool) {
		value, found := values[key]
		return value, found
	}
	assignments := []string{
		"HERDR_RELAY_ENV=/home/cv/.config/herdr-mobile-relay/relay.env",
		"HERDR_PLUGIN_CONFIG_DIR=/home/cv/.config/herdr-mobile-relay",
	}

	linux := appDeployWorkerLaunch("linux", "app-deploy", "/opt/relay", "/tmp/job.json", lookup)
	linuxArgs := []string{"--user", "--collect", "--unit=app-deploy"}
	for _, assignment := range assignments {
		linuxArgs = append(linuxArgs, "--setenv="+assignment)
	}
	linuxArgs = append(linuxArgs, "/opt/relay", "app-deploy-worker", "/tmp/job.json")
	if linux.application != "systemd-run" || !slices.Equal(linux.args, linuxArgs) {
		t.Fatalf("linux launch = %#v, want application systemd-run args %#v", linux, linuxArgs)
	}

	darwin := appDeployWorkerLaunch("darwin", "app-deploy", "/opt/relay", "/tmp/job.json", lookup)
	darwinArgs := []string{"submit", "-l", "app-deploy", "--", "/usr/bin/env"}
	darwinArgs = append(darwinArgs, assignments...)
	darwinArgs = append(darwinArgs, "/opt/relay", "app-deploy-worker", "/tmp/job.json")
	if darwin.application != "launchctl" || !slices.Equal(darwin.args, darwinArgs) {
		t.Fatalf("darwin launch = %#v, want application launchctl args %#v", darwin, darwinArgs)
	}
}

func TestManagerRecoversOrphanedDeploymentState(t *testing.T) {
	for _, stateName := range []string{"scheduled", "deploying"} {
		t.Run(stateName, func(t *testing.T) {
			root := t.TempDir()
			if err := writeState(filepath.Join(root, "app-deploy-state.json"), State{
				State:          stateName,
				TargetVersion:  "1.2.3",
				TargetRevision: "abc",
			}); err != nil {
				t.Fatal(err)
			}

			manager := NewManager(root, filepath.Join(root, "web"), "1.2.3", "abc")
			state := manager.State()
			if state.State != "failed" || state.Error != "App deployment worker stopped before completion" {
				t.Fatalf("state = %#v", state)
			}
		})
	}
}

func TestManagerPreservesStateOwnedByWorker(t *testing.T) {
	root := t.TempDir()
	lock, err := lockFile(filepath.Join(root, "app-deploy.lock"))
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close()
	if err := writeState(filepath.Join(root, "app-deploy-state.json"), State{
		State:          "deploying",
		TargetVersion:  "1.2.3",
		TargetRevision: "abc",
	}); err != nil {
		t.Fatal(err)
	}

	manager := NewManager(root, filepath.Join(root, "web"), "1.2.3", "abc")
	if state := manager.State(); state.State != "deploying" {
		t.Fatalf("state = %#v", state)
	}
}

func TestManagerAllowsScheduledWorkerStartupGracePeriod(t *testing.T) {
	root := t.TempDir()
	if err := writeState(filepath.Join(root, "app-deploy-state.json"), State{
		State:          "scheduled",
		TargetVersion:  "1.2.3",
		TargetRevision: "abc",
		StartedAt:      time.Now().UTC().Format(time.RFC3339),
	}); err != nil {
		t.Fatal(err)
	}

	manager := NewManager(root, filepath.Join(root, "web"), "1.2.3", "abc")
	if state := manager.State(); state.State != "scheduled" {
		t.Fatalf("state = %#v", state)
	}
}
