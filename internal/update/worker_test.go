package update

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	relayrelease "github.com/0cv/herdr-mobile-relay/internal/release"
)

func TestInstallPluginPinsExactCommitAndSuppressesSetup(t *testing.T) {
	root := t.TempDir()
	argsPath := filepath.Join(root, "args")
	envPath := filepath.Join(root, "env")
	herdrBin := filepath.Join(root, "herdr")
	script := `#!/bin/sh
printf '%s\n' "$@" > "$HERDR_TEST_ARGS"
printf '%s\n' "$HERDR_MOBILE_RELAY_NO_AUTO_SETUP" > "$HERDR_TEST_ENV"
`
	if err := os.WriteFile(herdrBin, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HERDR_TEST_ARGS", argsPath)
	t.Setenv("HERDR_TEST_ENV", envPath)
	t.Setenv("HERDR_MOBILE_RELAY_NO_AUTO_SETUP", "0")

	job := Job{HerdrBin: herdrBin, TargetRevision: strings.ToUpper(nextTestRevision)}
	if err := installPlugin(t.Context(), job); err != nil {
		t.Fatal(err)
	}
	args, err := os.ReadFile(argsPath)
	if err != nil {
		t.Fatal(err)
	}
	wantArgs := "plugin\ninstall\n0cv/herdr-mobile-relay\n--ref\n" + nextTestRevision + "\n--yes\n"
	if string(args) != wantArgs {
		t.Fatalf("Herdr arguments = %q, want %q", args, wantArgs)
	}
	value, err := os.ReadFile(envPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(value) != "1\n" {
		t.Fatalf("HERDR_MOBILE_RELAY_NO_AUTO_SETUP = %q", value)
	}
}

func TestWorkerRunsPluginInstallAndPersistsSuccess(t *testing.T) {
	jobPath, job := writeWorkerTestJob(t)
	var calls []string
	worker := Worker{
		Install: func(_ context.Context, got Job) error {
			calls = append(calls, "install:"+got.TargetRevision)
			return nil
		},
		Verify: func(_ context.Context, healthURL string, manifest relayrelease.Manifest) error {
			calls = append(calls, "verify:"+manifest.Version)
			if healthURL != job.HealthURL ||
				manifest.Version != job.TargetVersion ||
				manifest.Revision != job.TargetRevision {
				t.Fatalf("verification request = %q, %#v", healthURL, manifest)
			}
			return nil
		},
	}
	if err := worker.Run(t.Context(), jobPath); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(calls, []string{
		"install:" + nextTestRevision,
		"verify:1.2.4",
	}) {
		t.Fatalf("worker calls = %v", calls)
	}
	state, err := readState(job.StatePath)
	if err != nil {
		t.Fatal(err)
	}
	if state.State != "succeeded" ||
		state.CurrentVersion != job.TargetVersion ||
		state.CurrentRevision != job.TargetRevision ||
		state.FinishedAt == "" {
		t.Fatalf("state = %#v", state)
	}
	if _, err := os.Stat(jobPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("completed job still exists: %v", err)
	}
}

func TestWorkerInstallFailureIsRetryable(t *testing.T) {
	jobPath, job := writeWorkerTestJob(t)
	worker := Worker{
		Install: func(context.Context, Job) error {
			return errors.New("injected plugin install failure")
		},
	}
	err := worker.Run(t.Context(), jobPath)
	if err == nil || !strings.Contains(err.Error(), "injected plugin install failure") {
		t.Fatalf("worker error = %v", err)
	}
	state, readErr := readState(job.StatePath)
	if readErr != nil {
		t.Fatal(readErr)
	}
	if state.State != "failed" ||
		!strings.Contains(state.Error, "injected plugin install failure") ||
		state.FinishedAt == "" {
		t.Fatalf("state = %#v", state)
	}
	if _, err := os.Stat(jobPath); err != nil {
		t.Fatalf("failed job was removed: %v", err)
	}
}

func TestWorkerStartupFailureDoesNotLeaveUpdateScheduled(t *testing.T) {
	root := t.TempDir()
	releaseRoot := filepath.Join(root, "installed")
	if err := os.WriteFile(releaseRoot, []byte("not a directory"), 0o600); err != nil {
		t.Fatal(err)
	}
	statePath := filepath.Join(root, "runtime", "update-state.json")
	job := Job{
		ReleaseRoot:    releaseRoot,
		HerdrBin:       testHerdrBinary(t),
		TargetVersion:  "1.2.4",
		TargetRevision: nextTestRevision,
		StatePath:      statePath,
		HealthURL:      "http://127.0.0.1/healthz",
	}
	jobPath := filepath.Join(root, "update-job.json")
	if err := writeJSONAtomic(jobPath, job); err != nil {
		t.Fatal(err)
	}
	if err := writeState(statePath, State{
		State:          "scheduled",
		TargetVersion:  job.TargetVersion,
		TargetRevision: job.TargetRevision,
	}); err != nil {
		t.Fatal(err)
	}

	if err := Run(t.Context(), jobPath); err == nil {
		t.Fatal("worker startup unexpectedly succeeded")
	}
	data, err := os.ReadFile(statePath)
	if err != nil {
		t.Fatal(err)
	}
	var state State
	if err := json.Unmarshal(data, &state); err != nil {
		t.Fatal(err)
	}
	if state.State != "failed" || state.Error == "" || state.FinishedAt == "" {
		t.Fatalf("state = %#v", state)
	}
}

func TestVerifyHealthRequiresExactInstalledIdentity(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Path != "/healthz" {
			http.NotFound(writer, request)
			return
		}
		_ = json.NewEncoder(writer).Encode(map[string]string{
			"status":          "ok",
			"release_version": "1.2.4",
			"revision":        strings.ToUpper(nextTestRevision),
		})
	}))
	defer server.Close()

	expected := relayrelease.Manifest{Version: "1.2.4", Revision: nextTestRevision}
	if err := verifyHealth(t.Context(), server.URL+"/healthz", expected); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 50*time.Millisecond)
	defer cancel()
	expected.Revision = currentTestRevision
	if err := verifyHealth(ctx, server.URL+"/healthz", expected); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("mismatched identity error = %v", err)
	}
}

func TestActivateKeepsCompleteReleaseTarget(t *testing.T) {
	root := t.TempDir()
	releaseDir := filepath.Join(root, "releases", "one")
	if err := os.MkdirAll(releaseDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := Activate(root, releaseDir); err != nil {
		t.Fatal(err)
	}
	target, err := os.Readlink(filepath.Join(root, "current"))
	if err != nil {
		t.Fatal(err)
	}
	if target != filepath.Join("releases", "one") {
		t.Fatalf("target = %q", target)
	}
}

func TestPruneOldReleasesKeepsCurrentAndRollbackOnly(t *testing.T) {
	root := filepath.Join(t.TempDir(), "installed")
	releases := filepath.Join(root, "releases")
	current := filepath.Join(releases, "current-release")
	previous := filepath.Join(releases, "previous-release")
	old := filepath.Join(releases, "old-release")
	inflight := filepath.Join(releases, ".update-inflight")
	for _, directory := range []string{current, previous, old, inflight} {
		if err := os.MkdirAll(directory, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	for _, directory := range []string{current, previous, old} {
		writeWorkerTestRelease(t, directory, filepath.Base(directory), filepath.Base(directory)+"-revision")
	}
	if err := relayrelease.Seal(old); err != nil {
		t.Fatal(err)
	}
	if err := PruneOldReleases(root, current, previous); err != nil {
		t.Fatal(err)
	}
	for _, kept := range []string{current, previous, inflight} {
		if _, err := os.Stat(kept); err != nil {
			t.Fatalf("kept release %s: %v", kept, err)
		}
	}
	if _, err := os.Stat(old); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("old release was not pruned: %v", err)
	}
}

func writeWorkerTestJob(t *testing.T) (string, Job) {
	t.Helper()
	root := t.TempDir()
	job := Job{
		ReleaseRoot:    filepath.Join(root, "installed"),
		HerdrBin:       testHerdrBinary(t),
		TargetVersion:  "1.2.4",
		TargetRevision: nextTestRevision,
		StatePath:      filepath.Join(root, "runtime", "update-state.json"),
		HealthURL:      "http://127.0.0.1:18375/healthz",
	}
	jobPath := filepath.Join(root, "runtime", "update-job.json")
	if err := writeJSONAtomic(jobPath, job); err != nil {
		t.Fatal(err)
	}
	return jobPath, job
}

func writeWorkerTestRelease(t *testing.T, root, version, revision string) {
	t.Helper()
	files := []string{
		"herdr-mobile-relay",
		"web/index.html",
		"LICENSE",
		"README.md",
		"relay/common.sh",
		"relay/herdr-mobile-relay-service.sh",
		"relay/plugin-on-event.sh",
		"relay/setup-link.sh",
		"relay/stable-setup.sh",
		"relay/stable-teardown.sh",
		"relay/start.sh",
	}
	for _, name := range files {
		filename := filepath.Join(root, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(filename), 0o755); err != nil {
			t.Fatal(err)
		}
		mode := os.FileMode(0o644)
		if name == "herdr-mobile-relay" {
			mode = 0o755
		}
		if err := os.WriteFile(filename, []byte(name+"\n"), mode); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := relayrelease.Build(root, version, revision, relayrelease.CurrentTarget()); err != nil {
		t.Fatal(err)
	}
}
