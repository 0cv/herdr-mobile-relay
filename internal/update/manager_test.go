package update

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

const (
	currentTestRevision = "0123456789abcdef0123456789abcdef01234567"
	nextTestRevision    = "89abcdef0123456789abcdef0123456789abcdef"
)

func TestNewerVersion(t *testing.T) {
	if !NewerVersion("1.2.4", "1.2.3") ||
		NewerVersion("1.2.3", "1.2.3") ||
		NewerVersion("latest", "1.2.3") {
		t.Fatal("semantic version comparison failed")
	}
}

func TestUpdateWorkerLaunchForwardsAppDeploymentConfiguration(t *testing.T) {
	values := map[string]string{
		"HERDR_APP_DEPLOY_ORIGIN":        "https://app.example.test",
		"HERDR_CLOUDFLARE_PAGES_PROJECT": "relay-app",
		"HERDR_CLOUDFLARE_PAGES_BRANCH":  "main",
		"HERDR_APP_DEPLOY_NPX":           "/opt/node/bin/npx",
		"HERDR_APP_DEPLOY_NODE_DIR":      "/opt/node/bin",
		"CLOUDFLARE_API_TOKEN":           "must-not-be-forwarded",
	}
	lookup := func(key string) (string, bool) {
		value, found := values[key]
		return value, found
	}
	assignments := []string{
		"HERDR_APP_DEPLOY_ORIGIN=https://app.example.test",
		"HERDR_CLOUDFLARE_PAGES_PROJECT=relay-app",
		"HERDR_CLOUDFLARE_PAGES_BRANCH=main",
		"HERDR_APP_DEPLOY_NPX=/opt/node/bin/npx",
		"HERDR_APP_DEPLOY_NODE_DIR=/opt/node/bin",
	}

	linux := updateWorkerLaunch("linux", "relay-update", "/opt/relay", "/tmp/job.json", lookup)
	linuxArgs := []string{"--user", "--collect", "--unit=relay-update"}
	for _, assignment := range assignments {
		linuxArgs = append(linuxArgs, "--setenv="+assignment)
	}
	linuxArgs = append(linuxArgs, "/opt/relay", "update-worker", "/tmp/job.json")
	if linux.application != "systemd-run" || !slices.Equal(linux.args, linuxArgs) {
		t.Fatalf("linux launch = %#v, want application systemd-run args %#v", linux, linuxArgs)
	}

	darwin := updateWorkerLaunch("darwin", "relay-update", "/opt/relay", "/tmp/job.json", lookup)
	darwinArgs := []string{"submit", "-l", "relay-update", "--", "/usr/bin/env"}
	darwinArgs = append(darwinArgs, assignments...)
	darwinArgs = append(darwinArgs, "/opt/relay", "update-worker", "/tmp/job.json")
	if darwin.application != "launchctl" || !slices.Equal(darwin.args, darwinArgs) {
		t.Fatalf("darwin launch = %#v, want application launchctl args %#v", darwin, darwinArgs)
	}
}

func TestManagerReconcilesStaleAvailableStateFromPreviousRuntime(t *testing.T) {
	root := t.TempDir()
	runtimeDir := filepath.Join(root, "runtime")
	if err := writeState(filepath.Join(runtimeDir, "update-state.json"), State{
		State:             "available",
		CurrentVersion:    "0.8.6",
		CurrentRevision:   currentTestRevision,
		AvailableVersion:  "0.10.1",
		AvailableRevision: nextTestRevision,
		UpstreamVersion:   "0.10.1",
		UpstreamRevision:  nextTestRevision,
		TargetVersion:     "0.10.1",
		TargetRevision:    nextTestRevision,
		Mode:              "plugin",
		Eligible:          true,
		CanInstall:        true,
	}); err != nil {
		t.Fatal(err)
	}

	manager := NewManager(
		filepath.Join(root, "installed"),
		runtimeDir,
		testHerdrBinary(t),
		"0.10.1",
		nextTestRevision,
		"http://127.0.0.1:8375/healthz",
	)
	state := manager.State()
	if state.State != "current" || state.CanInstall ||
		state.CurrentVersion != "0.10.1" ||
		state.AvailableVersion != "" || state.TargetRevision != "" {
		t.Fatalf("reconciled state = %#v", state)
	}
	if _, scheduled, err := manager.Schedule(
		context.Background(),
		"0.10.1",
		nextTestRevision,
		false,
		"",
	); err == nil || scheduled.State != "current" {
		t.Fatalf("same-version schedule result = %#v, error = %v", scheduled, err)
	}
}

func TestManagerEligibilityRequiresReleasedBuildAndHerdr(t *testing.T) {
	herdrBin := testHerdrBinary(t)
	manager := &Manager{
		herdrBin: herdrBin,
		version:  "1.2.3",
		revision: currentTestRevision,
	}
	if eligible, mode, reason := manager.eligibility(); !eligible || mode != "plugin" || reason != "" {
		t.Fatalf("eligible build rejected: eligible=%v mode=%q reason=%q", eligible, mode, reason)
	}

	manager.version = "v1.2.3-dev"
	if eligible, _, reason := manager.eligibility(); eligible || !strings.Contains(reason, "released") {
		t.Fatalf("development build accepted: eligible=%v reason=%q", eligible, reason)
	}

	manager.version = "1.2.3"
	manager.herdrBin = filepath.Join(t.TempDir(), "missing-herdr")
	if eligible, _, reason := manager.eligibility(); eligible || !strings.Contains(reason, "unavailable") {
		t.Fatalf("missing Herdr accepted: eligible=%v reason=%q", eligible, reason)
	}
}

func TestFetchReleaseRequiresExactTagCommit(t *testing.T) {
	mux := http.NewServeMux()
	server := httptest.NewServer(mux)
	defer server.Close()

	mux.HandleFunc("/releases/latest", func(writer http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(writer).Encode(map[string]any{
			"tag_name": "v1.2.4",
		})
	})
	mux.HandleFunc("/git/ref/tags/v1.2.4", func(writer http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(writer).Encode(map[string]any{
			"object": map[string]string{"type": "commit", "sha": nextTestRevision},
		})
	})
	manager := NewManager(
		t.TempDir(),
		t.TempDir(),
		testHerdrBinary(t),
		"1.2.3",
		currentTestRevision,
		"http://127.0.0.1:8375/healthz",
	)
	manager.apiBase = server.URL
	manager.client = server.Client()
	metadata, err := manager.fetchRelease(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if metadata.Version != "1.2.4" || metadata.Revision != nextTestRevision {
		t.Fatalf("metadata = %#v", metadata)
	}
}

func TestManagerSchedulesExactHerdrPluginJob(t *testing.T) {
	root := t.TempDir()
	releaseRoot := filepath.Join(root, "installed")
	runtimeDir := filepath.Join(root, "runtime")
	herdrBin := testHerdrBinary(t)
	manager := NewManager(
		releaseRoot,
		runtimeDir,
		herdrBin,
		"1.2.3",
		currentTestRevision,
		"http://127.0.0.1:8375/healthz",
	)
	manager.state = State{
		State:             "available",
		AvailableVersion:  "1.2.4",
		AvailableRevision: shortRevision(nextTestRevision),
		TargetVersion:     "1.2.4",
		TargetRevision:    nextTestRevision,
		Mode:              "plugin",
		Eligible:          true,
		CanInstall:        true,
	}
	manager.metadata = releaseMetadata{Version: "1.2.4", Revision: nextTestRevision}
	if err := writeState(manager.statePath(), manager.state); err != nil {
		t.Fatal(err)
	}
	var launchedPath string
	manager.launch = func(_ context.Context, jobPath string) error {
		launchedPath = jobPath
		return nil
	}

	jobID, state, err := manager.Schedule(
		context.Background(),
		"1.2.4",
		nextTestRevision,
		true,
		"https://app.example.test",
	)
	if err != nil {
		t.Fatal(err)
	}
	if state.State != "scheduled" || jobID != filepath.Base(launchedPath) {
		t.Fatalf("schedule result = %q, %#v", jobID, state)
	}
	job, err := loadJob(launchedPath)
	if err != nil {
		t.Fatal(err)
	}
	if job.HerdrBin != herdrBin ||
		job.TargetVersion != "1.2.4" ||
		job.TargetRevision != nextTestRevision ||
		job.ReleaseRoot != releaseRoot ||
		!job.DeployAppFirst ||
		job.ExpectedAppOrigin != "https://app.example.test" {
		t.Fatalf("job = %#v", job)
	}
}

func TestManagerRecoversOrphanedPluginUpdateState(t *testing.T) {
	for _, stateName := range []string{"scheduled", "installing", "restarting"} {
		t.Run(stateName, func(t *testing.T) {
			root := t.TempDir()
			releaseRoot := filepath.Join(root, "installed")
			runtimeDir := filepath.Join(root, "runtime")
			started := time.Now().Add(-updateStartupGrace - time.Second).UTC().Format(time.RFC3339)
			if err := writeState(filepath.Join(runtimeDir, "update-state.json"), State{
				State:          stateName,
				TargetVersion:  "1.2.4",
				TargetRevision: nextTestRevision,
				StartedAt:      started,
			}); err != nil {
				t.Fatal(err)
			}

			manager := NewManager(
				releaseRoot,
				runtimeDir,
				testHerdrBinary(t),
				"1.2.3",
				currentTestRevision,
				"http://127.0.0.1/healthz",
			)
			state := manager.State()
			if state.State != "failed" ||
				!strings.Contains(state.Error, "run the update again") ||
				state.FinishedAt == "" {
				t.Fatalf("state = %#v", state)
			}
		})
	}
}

func TestManagerPreservesUpdateStateOwnedByWorker(t *testing.T) {
	root := t.TempDir()
	releaseRoot := filepath.Join(root, "installed")
	runtimeDir := filepath.Join(root, "runtime")
	if err := os.MkdirAll(releaseRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	lock, err := acquireLock(filepath.Join(releaseRoot, "update.lock"))
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close()
	if err := writeState(filepath.Join(runtimeDir, "update-state.json"), State{
		State:          "installing",
		TargetVersion:  "1.2.4",
		TargetRevision: nextTestRevision,
		StartedAt:      time.Now().Add(-time.Minute).UTC().Format(time.RFC3339),
	}); err != nil {
		t.Fatal(err)
	}

	manager := NewManager(
		releaseRoot,
		runtimeDir,
		testHerdrBinary(t),
		"1.2.3",
		currentTestRevision,
		"http://127.0.0.1/healthz",
	)
	if state := manager.State(); state.State != "installing" || state.Error != "" {
		t.Fatalf("state = %#v", state)
	}
}

func TestManagerAllowsScheduledUpdateStartupGrace(t *testing.T) {
	root := t.TempDir()
	releaseRoot := filepath.Join(root, "installed")
	runtimeDir := filepath.Join(root, "runtime")
	if err := writeState(filepath.Join(runtimeDir, "update-state.json"), State{
		State:          "scheduled",
		TargetVersion:  "1.2.4",
		TargetRevision: nextTestRevision,
		StartedAt:      time.Now().UTC().Format(time.RFC3339),
	}); err != nil {
		t.Fatal(err)
	}

	manager := NewManager(
		releaseRoot,
		runtimeDir,
		testHerdrBinary(t),
		"1.2.3",
		currentTestRevision,
		"http://127.0.0.1/healthz",
	)
	if state := manager.State(); state.State != "scheduled" {
		t.Fatalf("state = %#v", state)
	}
}

func TestManagerReconcilesCompletedPluginUpdateAfterRestart(t *testing.T) {
	root := t.TempDir()
	runtimeDir := filepath.Join(root, "runtime")
	if err := writeState(filepath.Join(runtimeDir, "update-state.json"), State{
		State:          "restarting",
		TargetVersion:  "1.2.4",
		TargetRevision: nextTestRevision,
		StartedAt:      time.Now().Add(-time.Minute).UTC().Format(time.RFC3339),
	}); err != nil {
		t.Fatal(err)
	}

	manager := NewManager(
		filepath.Join(root, "installed"),
		runtimeDir,
		testHerdrBinary(t),
		"1.2.4",
		strings.ToUpper(nextTestRevision),
		"http://127.0.0.1/healthz",
	)
	state := manager.State()
	if state.State != "succeeded" ||
		state.CurrentVersion != "1.2.4" ||
		state.CurrentRevision != strings.ToUpper(nextTestRevision) ||
		state.FinishedAt == "" {
		t.Fatalf("state = %#v", state)
	}
}

type blockingRoundTripper struct {
	started chan struct{}
	release chan struct{}
}

func (b blockingRoundTripper) RoundTrip(*http.Request) (*http.Response, error) {
	close(b.started)
	<-b.release
	return nil, errors.New("injected network failure")
}

func TestManagerStateDoesNotBlockOnReleaseCheckNetwork(t *testing.T) {
	manager := NewManager(
		t.TempDir(),
		t.TempDir(),
		testHerdrBinary(t),
		"1.2.3",
		currentTestRevision,
		"http://127.0.0.1:8375/healthz",
	)
	transport := blockingRoundTripper{started: make(chan struct{}), release: make(chan struct{})}
	manager.client = &http.Client{Transport: transport}
	done := make(chan struct{})
	go func() {
		manager.Check(context.Background())
		close(done)
	}()
	<-transport.started

	stateDone := make(chan State, 1)
	go func() { stateDone <- manager.State() }()
	select {
	case state := <-stateDone:
		if state.State != "checking" {
			t.Fatalf("state = %+v", state)
		}
	case <-time.After(time.Second):
		t.Fatal("State blocked behind release network I/O")
	}
	close(transport.release)
	<-done
}

func testHerdrBinary(t *testing.T) string {
	t.Helper()
	filename := filepath.Join(t.TempDir(), "herdr")
	if err := os.WriteFile(filename, []byte("#!/bin/sh\nexit 0\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	return filename
}
