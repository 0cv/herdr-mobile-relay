package appdeploy

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/0cv/herdr-mobile-relay/internal/release"
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
	webHash, err := release.WebHashFS(os.DirFS(web))
	if err != nil {
		t.Fatal(err)
	}
	job.WebHash = webHash
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

func TestRunRejectsWebBundleThatDoesNotMatchReleaseManifest(t *testing.T) {
	root := t.TempDir()
	nodeDir := filepath.Join(root, "node")
	web := filepath.Join(root, "web")
	if err := os.MkdirAll(nodeDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(web, 0o755); err != nil {
		t.Fatal(err)
	}
	npx := filepath.Join(root, "npx")
	for _, name := range []string{npx, filepath.Join(nodeDir, "node")} {
		if err := os.WriteFile(name, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(web, "version.json"), []byte(`{"release_version":"1.2.3","revision":"abc"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	webHash, err := release.WebHashFS(os.DirFS(web))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(web, "index.html"), []byte("tampered"), 0o644); err != nil {
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
		WebHash:    webHash,
		NPXPath:    npx,
		NodeDir:    nodeDir,
	}
	jobPath := filepath.Join(root, "job.json")
	if err := writeManagerJSON(jobPath, job); err != nil {
		t.Fatal(err)
	}
	if err := writeState(filepath.Join(root, "app-deploy-state.json"), State{
		State:          "scheduled",
		TargetVersion:  job.Version,
		TargetRevision: job.Revision,
	}); err != nil {
		t.Fatal(err)
	}
	err = Run(t.Context(), jobPath)
	if err == nil || !strings.Contains(err.Error(), "verified release manifest") {
		t.Fatalf("Run() error = %v", err)
	}
	data, readErr := os.ReadFile(filepath.Join(root, "app-deploy-state.json"))
	if readErr != nil {
		t.Fatal(readErr)
	}
	var state State
	if err := json.Unmarshal(data, &state); err != nil {
		t.Fatal(err)
	}
	if state.State != "failed" || state.FinishedAt == "" || !strings.Contains(state.Error, "verified release manifest") {
		t.Fatalf("state = %#v", state)
	}
}

func TestRunDoesNotOverwriteStateOwnedByAnotherWorker(t *testing.T) {
	root := t.TempDir()
	jobPath := filepath.Join(root, "job.json")
	if err := writeManagerJSON(jobPath, Job{RuntimeDir: root}); err != nil {
		t.Fatal(err)
	}
	if err := writeState(filepath.Join(root, "app-deploy-state.json"), State{
		State:          "deploying",
		TargetVersion:  "1.2.3",
		TargetRevision: "abc",
	}); err != nil {
		t.Fatal(err)
	}
	lock, err := lockFile(filepath.Join(root, "app-deploy.lock"))
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close()

	if err := Run(t.Context(), jobPath); !errors.Is(err, errDeployLocked) {
		t.Fatalf("Run() error = %v", err)
	}
	data, err := os.ReadFile(filepath.Join(root, "app-deploy-state.json"))
	if err != nil {
		t.Fatal(err)
	}
	var state State
	if err := json.Unmarshal(data, &state); err != nil {
		t.Fatal(err)
	}
	if state.State != "deploying" || state.Error != "" {
		t.Fatalf("state = %#v", state)
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

func TestVerifyPublicRetriesUntilExpectedIdentityIsPublished(t *testing.T) {
	var requests atomic.Int32
	cacheBusters := make(chan string, 2)
	cacheControls := make(chan string, 2)
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		attempt := requests.Add(1)
		cacheBusters <- request.URL.Query().Get("herdr_deploy_check")
		cacheControls <- request.Header.Get("Cache-Control")
		writer.Header().Set("Content-Type", "application/json")
		if attempt == 1 {
			_, _ = writer.Write([]byte(`{"release_version":"1.2.2","revision":"old"}`))
			return
		}
		_, _ = writer.Write([]byte(`{"release_version":"1.2.3","revision":"abc"}`))
	}))
	defer server.Close()

	job := Job{Origin: server.URL, Version: "1.2.3", Revision: "abc"}
	err := verifyPublicWith(t.Context(), job, server.Client(), func(int) time.Duration { return 0 })
	if err != nil {
		t.Fatal(err)
	}
	if requests.Load() != 2 {
		t.Fatalf("requests = %d, want 2", requests.Load())
	}
	firstCacheBust, secondCacheBust := <-cacheBusters, <-cacheBusters
	if firstCacheBust == "" || secondCacheBust == "" || firstCacheBust == secondCacheBust {
		t.Fatalf("cache busters = %q, %q", firstCacheBust, secondCacheBust)
	}
	for range 2 {
		if cacheControl := <-cacheControls; cacheControl != "no-cache, no-store" {
			t.Fatalf("Cache-Control = %q", cacheControl)
		}
	}
}

func TestVerifyPublicTimesOutWithLastObservedIdentity(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		writer.Header().Set("Content-Type", "application/json")
		_, _ = writer.Write([]byte(`{"release_version":"1.2.2","revision":"old"}`))
	}))
	defer server.Close()
	ctx, cancel := context.WithTimeout(t.Context(), 50*time.Millisecond)
	defer cancel()

	job := Job{Origin: server.URL, Version: "1.2.3", Revision: "abc"}
	err := verifyPublicWith(ctx, job, server.Client(), func(int) time.Duration { return time.Minute })
	if err == nil || !strings.Contains(err.Error(), "before timeout") ||
		!strings.Contains(err.Error(), "got 1.2.2 (old)") {
		t.Fatalf("verifyPublicWith() error = %v", err)
	}
}

func TestVerifyPublicDoesNotRetryPermanentHTTPFailure(t *testing.T) {
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		requests.Add(1)
		http.Error(writer, "forbidden", http.StatusForbidden)
	}))
	defer server.Close()

	job := Job{Origin: server.URL, Version: "1.2.3", Revision: "abc"}
	err := verifyPublicWith(t.Context(), job, server.Client(), func(int) time.Duration {
		t.Fatal("permanent failure was retried")
		return 0
	})
	if err == nil || !strings.Contains(err.Error(), "HTTP 403") {
		t.Fatalf("verifyPublicWith() error = %v", err)
	}
	if requests.Load() != 1 {
		t.Fatalf("requests = %d, want 1", requests.Load())
	}
}
