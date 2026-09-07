package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"testing"

	"github.com/0cv/herdr-mobile-relay/internal/activeruntime"
	"github.com/0cv/herdr-mobile-relay/internal/release"
	"github.com/0cv/herdr-mobile-relay/internal/supervisor"
)

func TestVerifyReleaseIdentity(t *testing.T) {
	originalVersion, originalRevision := version, revision
	version, revision = "1.2.3", "candidate-revision"
	t.Cleanup(func() {
		version, revision = originalVersion, originalRevision
	})

	manifest := release.Manifest{
		Version:  "1.2.3",
		Revision: "candidate-revision",
		Target:   release.CurrentTarget(),
	}
	if err := verifyReleaseIdentity(manifest, "1.2.3", "candidate-revision", release.CurrentTarget(), false); err != nil {
		t.Fatalf("matching identity rejected: %v", err)
	}

	tests := []struct {
		name             string
		manifest         release.Manifest
		expectedVersion  string
		expectedRevision string
		expectedTarget   string
		allowCrossTarget bool
		errorPart        string
	}{
		{
			name:             "workflow version",
			manifest:         manifest,
			expectedVersion:  "1.2.4",
			expectedRevision: "candidate-revision",
			expectedTarget:   release.CurrentTarget(),
			errorPart:        "expected version",
		},
		{
			name:             "workflow revision",
			manifest:         manifest,
			expectedVersion:  "1.2.3",
			expectedRevision: "other-revision",
			expectedTarget:   release.CurrentTarget(),
			errorPart:        "expected revision",
		},
		{
			name:             "workflow target",
			manifest:         manifest,
			expectedVersion:  "1.2.3",
			expectedRevision: "candidate-revision",
			expectedTarget:   "other/target",
			errorPart:        "expected target",
		},
		{
			name: "binary version",
			manifest: release.Manifest{
				Version:  "1.2.4",
				Revision: "candidate-revision",
				Target:   release.CurrentTarget(),
			},
			expectedTarget: release.CurrentTarget(),
			errorPart:      "binary version",
		},
		{
			name: "binary revision",
			manifest: release.Manifest{
				Version:  "1.2.3",
				Revision: "other-revision",
				Target:   release.CurrentTarget(),
			},
			expectedTarget: release.CurrentTarget(),
			errorPart:      "binary revision",
		},
		{
			name: "binary target",
			manifest: release.Manifest{
				Version:  "1.2.3",
				Revision: "candidate-revision",
				Target:   "other/target",
			},
			expectedTarget: "other/target",
			errorPart:      "binary target",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := verifyReleaseIdentity(
				test.manifest,
				test.expectedVersion,
				test.expectedRevision,
				test.expectedTarget,
				test.allowCrossTarget,
			)
			if err == nil || !strings.Contains(err.Error(), test.errorPart) {
				t.Fatalf("identity error = %v, want %q", err, test.errorPart)
			}
		})
	}

	crossTarget := manifest
	crossTarget.Target = "other/target"
	if err := verifyReleaseIdentity(crossTarget, "", "", "other/target", true); err != nil {
		t.Fatalf("cross-target build-host verification rejected: %v", err)
	}
}

func TestBuildSupervisorConfigUsesSeparateChildEnvironments(t *testing.T) {
	root := t.TempDir()
	config, err := buildSupervisorConfig([]string{
		"--managed",
		"--relay", filepath.Join(root, "relay"),
		"--cloudflared", filepath.Join(root, "cloudflared"),
		"--cloudflared-config", filepath.Join(root, "cloudflared.yml"),
		"--active-runtime", filepath.Join(root, "active-runtime.json"),
		"--state", filepath.Join(root, "supervisor.json"),
		"--log-dir", filepath.Join(root, "logs"),
		"--release-root", filepath.Join(root, "release"),
		"--instance", "relay-personal",
		"--local-health", "http://127.0.0.1:8375/readyz",
		"--public-health", "https://relay.example/readyz",
	}, []string{
		"PATH=/managed/bin", "HOME=/managed/home", "HERDR_RELAY_TOKEN=relay-sentinel",
		"GH_TOKEN=gh-sentinel", "GITHUB_TOKEN=github-sentinel", "CLOUDFLARE_API_TOKEN=cloudflare-sentinel", "RANDOM=random-sentinel",
	})
	if err != nil {
		t.Fatal(err)
	}
	if !config.Managed {
		t.Fatal("managed supervisor flag was lost")
	}
	if !slices.Equal(config.Relay.Args, []string{"serve"}) || !slices.Equal(config.Tunnel.Args, []string{"tunnel", "--config", filepath.Join(root, "cloudflared.yml"), "run"}) {
		t.Fatalf("child args = relay %v, tunnel %v", config.Relay.Args, config.Tunnel.Args)
	}
	if !containsEnvironment(config.Relay.Env, "HERDR_RELAY_TOKEN=relay-sentinel") || containsAny(config.Relay.Env, "gh-sentinel", "github-sentinel", "cloudflare-sentinel", "random-sentinel") {
		t.Fatalf("relay environment = %v", config.Relay.Env)
	}
	if !containsEnvironment(config.Tunnel.Env, "PATH=/managed/bin") || containsAny(config.Tunnel.Env, "relay-sentinel", "gh-sentinel", "github-sentinel", "cloudflare-sentinel", "random-sentinel") {
		t.Fatalf("tunnel environment = %v", config.Tunnel.Env)
	}
}

func TestBuildSupervisorConfigRejectsParseTrailingModeAndPathErrors(t *testing.T) {
	root := t.TempDir()
	required := []string{
		"--relay", filepath.Join(root, "relay"),
		"--cloudflared", filepath.Join(root, "cloudflared"),
		"--cloudflared-config", filepath.Join(root, "cloudflared.yml"),
		"--state", filepath.Join(root, "state.json"),
		"--log-dir", filepath.Join(root, "logs"),
		"--release-root", filepath.Join(root, "release"),
		"--instance", "relay-personal",
		"--local-health", "http://127.0.0.1:8375/readyz",
		"--public-health", "https://relay.example/readyz",
	}
	tests := map[string][]string{
		"unknown flag":            {"--unknown"},
		"trailing argument":       append(append([]string{}, required...), "extra"),
		"ordinary active runtime": append(append([]string{}, required...), "--active-runtime", filepath.Join(root, "active.json")),
		"managed missing runtime": append([]string{"--managed"}, required...),
		"relative tunnel config":  append(append([]string{}, required[:4]...), append([]string{"--cloudflared-config", "relative.yml"}, required[6:]...)...),
	}
	for name, args := range tests {
		t.Run(name, func(t *testing.T) {
			if _, err := buildSupervisorConfig(args, nil); err == nil {
				t.Fatal("invalid supervisor arguments accepted")
			}
		})
	}
	if config, err := buildSupervisorConfig(required, nil); err != nil || config.Managed || config.ActiveRuntimePath != "" {
		t.Fatalf("ordinary config = %+v, %v", config, err)
	}
}

func TestSupervisorCommandsValidateArgumentsAndExposeTripReset(t *testing.T) {
	if code, err := run([]string{"supervise"}); code != 2 || err == nil {
		t.Fatalf("empty supervise = (%d, %v)", code, err)
	}
	if code, err := run([]string{"supervisor-status"}); code != 2 || err == nil {
		t.Fatalf("empty status = (%d, %v)", code, err)
	}
	if code, err := run([]string{"supervisor-reset"}); code != 2 || err == nil {
		t.Fatalf("empty reset = (%d, %v)", code, err)
	}
	if code, err := run([]string{"supervisor-record-failure"}); code != 2 || err == nil {
		t.Fatalf("empty record failure = (%d, %v)", code, err)
	}
	if code, err := run([]string{"supervisor-record-failure", filepath.Join(t.TempDir(), "state"), "bad-cap", "failure"}); code != 2 || err == nil {
		t.Fatalf("invalid record failure cap = (%d, %v)", code, err)
	}
	if code, err := run([]string{"supervisor-record-failure", filepath.Join(t.TempDir(), "state"), "0", "failure"}); code != 1 || err == nil {
		t.Fatalf("rejected record failure = (%d, %v)", code, err)
	}
	if code, err := run([]string{"supervisor-ready"}); code != 2 || err == nil {
		t.Fatalf("empty ready command = (%d, %v)", code, err)
	}
	if code, err := run([]string{"supervisor-status", filepath.Join(t.TempDir(), "missing")}); code != 1 || err == nil {
		t.Fatalf("missing status = (%d, %v)", code, err)
	}
	if code, err := run([]string{"supervisor-reset", filepath.Join(t.TempDir(), "missing")}); code != 1 || err == nil {
		t.Fatalf("missing reset = (%d, %v)", code, err)
	}
	recordedPath := filepath.Join(t.TempDir(), "recorded.json")
	if code, err := run([]string{"supervisor-record-failure", recordedPath, "2", "fixture bootstrap failure"}); code != 0 || err != nil {
		t.Fatalf("record failure = (%d, %v)", code, err)
	}
	recorded, err := supervisor.ReadState(recordedPath)
	if err != nil || recorded.Status != supervisor.StatusRetrying || recorded.Failures != 1 || recorded.Reason != "fixture bootstrap failure" {
		t.Fatalf("recorded failure = %+v, %v", recorded, err)
	}

	statePath := filepath.Join(t.TempDir(), "state.json")
	if err := writeSupervisorStateFixture(statePath, supervisor.State{Status: supervisor.StatusTripped, Failures: 5, Reason: "fixture"}); err != nil {
		t.Fatal(err)
	}
	if code, err := run([]string{"supervisor-status", statePath}); code != 0 || err != nil {
		t.Fatalf("status = (%d, %v)", code, err)
	}
	if code, err := run([]string{"supervisor-reset", statePath}); code != 0 || err != nil {
		t.Fatalf("reset = (%d, %v)", code, err)
	}
	state, err := supervisor.ReadState(statePath)
	if err != nil || state.Status != supervisor.StatusReset {
		t.Fatalf("reset state = %+v, %v", state, err)
	}
}

func TestRunSuperviseTreatsPersistedTripAsSuccessfulServiceExit(t *testing.T) {
	original := newServiceSupervisor
	t.Cleanup(func() { newServiceSupervisor = original })
	if _, err := original(supervisor.Config{}); err == nil {
		t.Fatal("default supervisor factory accepted invalid config")
	}
	newServiceSupervisor = func(supervisor.Config) (serviceSupervisor, error) {
		return fixtureServiceSupervisor{err: supervisor.ErrTripped}, nil
	}
	root := t.TempDir()
	args := []string{
		"--managed",
		"--relay", filepath.Join(root, "relay"), "--cloudflared", filepath.Join(root, "cloudflared"),
		"--cloudflared-config", filepath.Join(root, "cloudflared.yml"), "--active-runtime", filepath.Join(root, "active-runtime.json"),
		"--state", filepath.Join(root, "state.json"), "--log-dir", filepath.Join(root, "logs"),
		"--release-root", filepath.Join(root, "release"), "--instance", "relay-personal",
		"--local-health", "http://127.0.0.1:8375/readyz", "--public-health", "https://relay.example/readyz",
	}
	if code, err := runSupervise(args, nil); code != 0 || err != nil {
		t.Fatalf("tripped service exit = (%d, %v)", code, err)
	}
	newServiceSupervisor = func(supervisor.Config) (serviceSupervisor, error) {
		return fixtureServiceSupervisor{err: supervisor.ErrAlreadyRunning}, nil
	}
	if code, err := runSupervise(args, nil); code != 0 || err != nil {
		t.Fatalf("duplicate service exit = (%d, %v)", code, err)
	}
	newServiceSupervisor = func(supervisor.Config) (serviceSupervisor, error) { return nil, errors.New("fixture config") }
	if code, err := runSupervise(args, nil); code != 1 || err == nil {
		t.Fatalf("factory failure = (%d, %v)", code, err)
	}
	newServiceSupervisor = func(supervisor.Config) (serviceSupervisor, error) { return fixtureServiceSupervisor{}, nil }
	if code, err := runSupervise(args, nil); code != 0 || err != nil {
		t.Fatalf("clean service exit = (%d, %v)", code, err)
	}
	newServiceSupervisor = func(supervisor.Config) (serviceSupervisor, error) {
		return fixtureServiceSupervisor{err: errors.New("fixture run")}, nil
	}
	if code, err := runSupervise(args, nil); code != 1 || err == nil || err.Error() != "fixture run" {
		t.Fatalf("service failure = (%d, %v)", code, err)
	}
	newServiceSupervisor = func(supervisor.Config) (serviceSupervisor, error) {
		return fixtureServiceSupervisor{err: syscall.ENOSPC}, nil
	}
	if code, err := runSupervise(args, nil); code != 1 || !errors.Is(err, syscall.ENOSPC) {
		t.Fatalf("full-disk-like service failure = (%d, %v)", code, err)
	}
}

func TestRunSupervisePersistsAndCapsBootstrapValidationFailures(t *testing.T) {
	original := newServiceSupervisor
	t.Cleanup(func() { newServiceSupervisor = original })
	newServiceSupervisor = func(supervisor.Config) (serviceSupervisor, error) {
		return nil, errors.New("fixture invalid service configuration")
	}
	root := t.TempDir()
	statePath := filepath.Join(root, "state.json")
	args := []string{
		"--relay", filepath.Join(root, "relay"), "--cloudflared", filepath.Join(root, "cloudflared"),
		"--cloudflared-config", filepath.Join(root, "cloudflared.yml"), "--state", statePath,
		"--log-dir", filepath.Join(root, "logs"), "--release-root", filepath.Join(root, "release"), "--instance", "relay-personal",
		"--local-health", "http://127.0.0.1:8375/readyz",
		"--public-health", "https://relay.example/readyz",
	}
	for attempt := 1; attempt <= 5; attempt++ {
		code, err := runSupervise(args, nil)
		if attempt < 5 && (code != 1 || err == nil) {
			t.Fatalf("retry %d = (%d, %v)", attempt, code, err)
		}
		if attempt == 5 && (code != 0 || err != nil) {
			t.Fatalf("trip = (%d, %v)", code, err)
		}
		state, readErr := supervisor.ReadState(statePath)
		if readErr != nil || state.Failures != attempt {
			t.Fatalf("attempt %d state = %+v, %v", attempt, state, readErr)
		}
		want := supervisor.StatusRetrying
		if attempt == 5 {
			want = supervisor.StatusTripped
		}
		if state.Status != want {
			t.Fatalf("attempt %d status = %s, want %s", attempt, state.Status, want)
		}
	}
}

func TestRecordSuperviseBootstrapFailureReportsPersistenceFailure(t *testing.T) {
	statePath := t.TempDir()
	code, err := recordSuperviseBootstrapFailure(supervisor.Config{StatePath: statePath, MaxFailures: 5}, errors.New("invalid service configuration"), 2)
	if code != 2 || err == nil || !strings.Contains(err.Error(), "persist supervisor bootstrap failure") {
		t.Fatalf("persistence failure = (%d, %v)", code, err)
	}
}

func TestRunSupervisorReadyLoadsVerifiedReleaseAndActiveGeneration(t *testing.T) {
	originalReleaseVerifier := verifyReadinessRelease
	originalActiveLoader := loadReadinessActiveRuntime
	originalReadinessVerifier := verifyRunningReadiness
	originalVersion, originalRevision := version, revision
	t.Cleanup(func() {
		verifyReadinessRelease = originalReleaseVerifier
		loadReadinessActiveRuntime = originalActiveLoader
		verifyRunningReadiness = originalReadinessVerifier
		version, revision = originalVersion, originalRevision
	})
	version, revision = "1.2.3", "revision-1"
	root := t.TempDir()
	releaseRoot := filepath.Join(root, "release")
	activePath := filepath.Join(root, "active-runtime.json")
	statePath := filepath.Join(root, "supervisor.json")
	verifyReadinessRelease = func(path, target string) (release.Manifest, error) {
		if path != releaseRoot || target != release.CurrentTarget() {
			t.Fatalf("release verification = %q, %q", path, target)
		}
		return release.Manifest{Version: "1.2.3", Revision: "revision-1", Target: target, WebHash: "web-1"}, nil
	}
	loadReadinessActiveRuntime = func(path string) (activeruntime.Snapshot, error) {
		if path != activePath {
			t.Fatalf("active runtime path = %q", path)
		}
		return activeruntime.Snapshot{Generation: "generation-1"}, nil
	}
	var captured supervisor.ReadinessExpectation
	verifyRunningReadiness = func(_ context.Context, state, local, public string, expected supervisor.ReadinessExpectation) error {
		if state != statePath || local != "http://127.0.0.1:8375/readyz" || public != "https://relay.example/readyz" {
			t.Fatalf("readiness boundaries = %q, %q, %q", state, local, public)
		}
		captured = expected
		return nil
	}
	code, err := runSupervisorReady([]string{
		"--managed", "--active-runtime", activePath,
		"--state", statePath, "--release-root", releaseRoot, "--instance", "relay-personal",
		"--local-health", "http://127.0.0.1:8375/readyz", "--public-health", "https://relay.example/readyz",
	})
	if code != 0 || err != nil {
		t.Fatalf("ready command = (%d, %v)", code, err)
	}
	want := supervisor.ReadinessExpectation{Managed: true, Instance: "relay-personal", ReleaseVersion: "1.2.3", Revision: "revision-1", BundleHash: "web-1", Generation: "generation-1"}
	if captured != want {
		t.Fatalf("readiness expectation = %+v, want %+v", captured, want)
	}
	ordinary := []string{
		"--state", statePath, "--release-root", releaseRoot, "--instance", "relay-personal",
		"--local-health", "http://127.0.0.1:8375/readyz", "--public-health", "https://relay.example/readyz",
	}
	loadReadinessActiveRuntime = func(string) (activeruntime.Snapshot, error) {
		t.Fatal("ordinary readiness loaded a managed runtime")
		return activeruntime.Snapshot{}, nil
	}
	if code, err := runSupervisorReady(ordinary); code != 0 || err != nil {
		t.Fatalf("ordinary ready command = (%d, %v)", code, err)
	}
	want = supervisor.ReadinessExpectation{Instance: "relay-personal", ReleaseVersion: "1.2.3", Revision: "revision-1", BundleHash: "web-1"}
	if captured != want {
		t.Fatalf("ordinary readiness expectation = %+v, want %+v", captured, want)
	}

	for name, args := range map[string][]string{
		"empty":                  nil,
		"managed without active": {"--managed", "--state", statePath, "--release-root", releaseRoot, "--instance", "relay-personal", "--local-health", "http://127.0.0.1:8375/readyz", "--public-health", "https://relay.example/readyz"},
		"ordinary with active":   {"--active-runtime", activePath, "--state", statePath, "--release-root", releaseRoot, "--instance", "relay-personal", "--local-health", "http://127.0.0.1:8375/readyz", "--public-health", "https://relay.example/readyz"},
	} {
		t.Run(name, func(t *testing.T) {
			if code, err := runSupervisorReady(args); code != 2 || err == nil {
				t.Fatalf("invalid ready command = (%d, %v)", code, err)
			}
		})
	}

	if code, err := runSupervisorReady([]string{"--unknown"}); code != 2 || err == nil {
		t.Fatalf("unknown ready flag = (%d, %v)", code, err)
	}
	valid := []string{
		"--managed", "--active-runtime", activePath,
		"--state", statePath, "--release-root", releaseRoot, "--instance", "relay-personal",
		"--local-health", "http://127.0.0.1:8375/readyz", "--public-health", "https://relay.example/readyz",
	}
	verifyReadinessRelease = func(string, string) (release.Manifest, error) {
		return release.Manifest{}, errors.New("verify release")
	}
	if code, err := runSupervisorReady(valid); code != 1 || err == nil || err.Error() != "verify release" {
		t.Fatalf("release verification failure = (%d, %v)", code, err)
	}
	verifyReadinessRelease = func(string, string) (release.Manifest, error) {
		return release.Manifest{Version: "wrong", Revision: "revision-1", Target: release.CurrentTarget(), WebHash: "web-1"}, nil
	}
	if code, err := runSupervisorReady(valid); code != 1 || err == nil {
		t.Fatalf("release identity failure = (%d, %v)", code, err)
	}
	verifyReadinessRelease = func(string, string) (release.Manifest, error) {
		return release.Manifest{Version: "1.2.3", Revision: "revision-1", Target: release.CurrentTarget(), WebHash: "web-1"}, nil
	}
	loadReadinessActiveRuntime = func(string) (activeruntime.Snapshot, error) {
		return activeruntime.Snapshot{}, errors.New("active runtime")
	}
	if code, err := runSupervisorReady(valid); code != 1 || err == nil || err.Error() != "active runtime" {
		t.Fatalf("active runtime failure = (%d, %v)", code, err)
	}
	loadReadinessActiveRuntime = func(string) (activeruntime.Snapshot, error) {
		return activeruntime.Snapshot{Generation: "generation-1"}, nil
	}
	verifyRunningReadiness = func(context.Context, string, string, string, supervisor.ReadinessExpectation) error {
		return errors.New("readiness")
	}
	if code, err := runSupervisorReady(valid); code != 1 || err == nil || err.Error() != "readiness" {
		t.Fatalf("readiness failure = (%d, %v)", code, err)
	}
}

type fixtureServiceSupervisor struct{ err error }

func (f fixtureServiceSupervisor) Run(context.Context) error { return f.err }

func containsEnvironment(environment []string, want string) bool {
	return slices.Contains(environment, want)
}

func containsAny(environment []string, values ...string) bool {
	joined := strings.Join(environment, "\n")
	for _, value := range values {
		if strings.Contains(joined, value) {
			return true
		}
	}
	return false
}

func writeSupervisorStateFixture(path string, state supervisor.State) error {
	state.Schema = 1
	state.UpdatedAt = "2026-09-06T00:00:00Z"
	data := []byte(`{"schema":1,"status":"` + string(state.Status) + `","failures":5,"reason":"fixture","updated_at":"` + state.UpdatedAt + `"}`)
	return os.WriteFile(path, data, 0o600)
}

func TestVerifyReleaseRejectsCrossTargetCandidateMode(t *testing.T) {
	for _, candidateFlag := range []string{"--version", "--revision"} {
		t.Run(candidateFlag, func(t *testing.T) {
			code, err := run([]string{"verify-release", "--allow-cross-target", candidateFlag, "candidate"})
			if code != 2 || err == nil || !strings.Contains(err.Error(), "cannot be combined") {
				t.Fatalf("run() = (%d, %v), want usage error", code, err)
			}
		})
	}
}
