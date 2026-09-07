package supervisor

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/0cv/herdr-mobile-relay/internal/release"
)

func TestChildEnvironmentsAreSeparateExplicitAllowlists(t *testing.T) {
	if environment := RelayEnvironment(nil); len(environment) != 0 {
		t.Fatalf("empty source environment = %v", environment)
	}
	source := []string{
		"PATH=/managed/bin", "HOME=/managed/home", "TMPDIR=/managed/tmp", "LANG=en_US.UTF-8",
		"CLAUDE_CONFIG_DIR=/native/claude", "CODEX_HOME=/native/codex", "PI_CODING_AGENT_DIR=/native/pi",
		"OMO_CODING_AGENT_DIR=/native/omo", "SENPI_CODING_AGENT_DIR=/native/senpi", "KIMI_CODE_HOME=/native/kimi",
		"HERMES_HOME=/native/hermes", "HERDR_HERMES_DATA_DIRS=/native/hermes-data",
		"HTTP_PROXY=http://upper-http.proxy", "HTTPS_PROXY=https://upper-https.proxy", "NO_PROXY=127.0.0.1,.example.test",
		"http_proxy=http://lower-http.proxy", "https_proxy=https://lower-https.proxy", "no_proxy=localhost,.internal.test",
		"HERDR_RELAY_TOKEN=relay-secret-sentinel", "HERDR_RELAY_HOST=127.0.0.1", "HERDR_BIN=/managed/herdr",
		"HERDR_RELAY_TOPOLOGY_COMMIT_HELPER=/managed/OuroWorkbenchRemote", "OURO_REMOTE_CONFIG=/managed/profiles.json",
		"OURO_LEDGER_ROOT=/managed/ledger", "OURO_SESSION_MAP=/managed/session-map.json",
		"OURO_SHIM_DIRECTORY=/managed/shims", "OURO_ZDOTDIR=/managed/zdotdir",
		"HERDR_APP_DEPLOY_ORIGIN=https://app.example.test", "HERDR_CLOUDFLARE_PAGES_PROJECT=app-project",
		"HERDR_CLOUDFLARE_PAGES_BRANCH=main", "HERDR_APP_DEPLOY_NPX=/managed/bin/npx", "HERDR_APP_DEPLOY_NODE_DIR=/managed/bin",
		"HERDR_WEB_ROOT=/checkout/frontend/dist",
		"GH_TOKEN=github-gh-sentinel", "GITHUB_TOKEN=github-sentinel", "HERDR_GITHUB_TOKEN_FILE=/secret/token",
		"CLOUDFLARE_API_TOKEN=cloudflare-sentinel", "RANDOM_AMBIENT=random-sentinel",
	}
	relay := RelayEnvironment(source)
	tunnel := TunnelEnvironment(source)

	assertEnvironmentValue(t, relay, "HERDR_RELAY_TOKEN", "relay-secret-sentinel")
	assertEnvironmentValue(t, relay, "HERDR_BIN", "/managed/herdr")
	assertEnvironmentValue(t, relay, "HERDR_APP_DEPLOY_ORIGIN", "https://app.example.test")
	assertEnvironmentValue(t, relay, "HERDR_CLOUDFLARE_PAGES_PROJECT", "app-project")
	assertEnvironmentValue(t, relay, "HERDR_CLOUDFLARE_PAGES_BRANCH", "main")
	assertEnvironmentValue(t, relay, "HERDR_APP_DEPLOY_NPX", "/managed/bin/npx")
	assertEnvironmentValue(t, relay, "HERDR_APP_DEPLOY_NODE_DIR", "/managed/bin")
	assertEnvironmentValue(t, tunnel, "PATH", "/managed/bin")
	for key, want := range map[string]string{
		"CLAUDE_CONFIG_DIR": "/native/claude", "CODEX_HOME": "/native/codex", "PI_CODING_AGENT_DIR": "/native/pi",
		"OMO_CODING_AGENT_DIR": "/native/omo", "SENPI_CODING_AGENT_DIR": "/native/senpi", "KIMI_CODE_HOME": "/native/kimi",
		"HERMES_HOME": "/native/hermes", "HERDR_HERMES_DATA_DIRS": "/native/hermes-data",
		"HERDR_RELAY_TOPOLOGY_COMMIT_HELPER": "/managed/OuroWorkbenchRemote", "OURO_REMOTE_CONFIG": "/managed/profiles.json",
		"OURO_LEDGER_ROOT": "/managed/ledger", "OURO_SESSION_MAP": "/managed/session-map.json",
		"OURO_SHIM_DIRECTORY": "/managed/shims", "OURO_ZDOTDIR": "/managed/zdotdir",
	} {
		assertEnvironmentValue(t, relay, key, want)
		if slices.ContainsFunc(tunnel, func(entry string) bool { return strings.HasPrefix(entry, key+"=") }) {
			t.Fatalf("tunnel inherited relay-only native root %s: %v", key, tunnel)
		}
	}

	for key, want := range map[string]string{
		"HTTP_PROXY": "http://upper-http.proxy", "HTTPS_PROXY": "https://upper-https.proxy", "NO_PROXY": "127.0.0.1,.example.test",
		"http_proxy": "http://lower-http.proxy", "https_proxy": "https://lower-https.proxy", "no_proxy": "localhost,.internal.test",
	} {
		assertEnvironmentValue(t, relay, key, want)
		assertEnvironmentValue(t, tunnel, key, want)
	}
	for _, environment := range [][]string{relay, tunnel} {
		joined := strings.Join(environment, "\n")
		for _, secret := range []string{"github-gh-sentinel", "github-sentinel", "/secret/token", "cloudflare-sentinel", "random-sentinel", "/checkout/frontend/dist"} {
			if strings.Contains(joined, secret) {
				t.Fatalf("child environment leaked %q: %v", secret, environment)
			}
		}
	}
	if strings.Contains(strings.Join(tunnel, "\n"), "relay-secret-sentinel") {
		t.Fatalf("tunnel inherited relay token: %v", tunnel)
	}
	if !slices.IsSorted(relay) || !slices.IsSorted(tunnel) {
		t.Fatalf("child environments are not deterministic: relay=%v tunnel=%v", relay, tunnel)
	}
}

func TestHealthyPairToleratesManagedTopologyTransactionReadiness(t *testing.T) {
	service := newInjectedSupervisor(t)
	starter := &fakeStarter{}
	service.start = starter.Start
	ctx, cancel := context.WithCancel(context.Background())
	checks := 0
	service.health = func(context.Context) error {
		checks++
		switch checks {
		case 1:
			return nil
		case 2:
			return errTopologyTransactionPending
		default:
			cancel()
			return context.Canceled
		}
	}
	result := service.runPair(ctx, func(bool) error { return nil })
	if !result.cancelled || result.unsafe || result.reason != "" || checks < 3 {
		t.Fatalf("topology transaction readiness result = %+v, checks=%d", result, checks)
	}
}

func TestHealthyPairRecyclesAfterManagedTopologyTransactionStalls(t *testing.T) {
	service := newInjectedSupervisor(t)
	service.config.StartupTimeout = 5 * time.Millisecond
	service.config.HealthInterval = time.Millisecond
	starter := &fakeStarter{}
	service.start = starter.Start
	checks := 0
	service.health = func(context.Context) error {
		checks++
		if checks == 1 {
			return nil
		}
		return errTopologyTransactionPending
	}
	result := service.runPair(context.Background(), func(bool) error { return nil })
	if result.unsafe || !strings.Contains(result.reason, "topology transaction remained pending") || checks < 3 {
		t.Fatalf("stalled topology transaction result = %+v, checks=%d", result, checks)
	}
}

func TestReadReadinessRecognizesManagedTopologyTransaction(t *testing.T) {
	for _, test := range []struct {
		name              string
		status            int
		requireGeneration bool
		wantPending       bool
	}{
		{name: "managed pending", status: http.StatusServiceUnavailable, requireGeneration: true, wantPending: true},
		{name: "ordinary unavailable", status: http.StatusServiceUnavailable},
		{name: "managed server failure", status: http.StatusInternalServerError, requireGeneration: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(test.status)
				_, _ = io.WriteString(w, `{"status":"unavailable","instance":"relay","release_version":"1.2.3","revision":"rev","bundle_hash":"web","generation":"g1","expected_inventory":{"state":"topology_transaction_pending","generation":"g1"}}`)
			}))
			defer server.Close()
			_, err := readReadinessMode(t.Context(), server.URL, test.requireGeneration)
			if errors.Is(err, errTopologyTransactionPending) != test.wantPending {
				t.Fatalf("topology transaction readiness error = %v, want pending=%t", err, test.wantPending)
			}
		})
	}

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = io.WriteString(w, `{"status":"unavailable","generation":"g1","expected_inventory":{"state":"topology_transaction_pending","generation":"g1"}}`)
	}))
	defer server.Close()
	if _, err := readReadinessMode(t.Context(), server.URL, true); errors.Is(err, errTopologyTransactionPending) {
		t.Fatal("incomplete pending identity bypassed readiness verification")
	}
}

func TestNewRestrictsHealthChecksToExactLocalAndPublicReadinessBoundaries(t *testing.T) {
	base := testConfig(t, t.TempDir())
	tests := map[string]func(*Config){
		"local https":       func(config *Config) { config.LocalHealthURL = "https://127.0.0.1:8375/readyz" },
		"local remote host": func(config *Config) { config.LocalHealthURL = "http://example.test:8375/readyz" },
		"local wrong path":  func(config *Config) { config.LocalHealthURL = "http://127.0.0.1:8375/healthz" },
		"local no port":     func(config *Config) { config.LocalHealthURL = "http://127.0.0.1/readyz" },
		"public http":       func(config *Config) { config.PublicHealthURL = "http://relay.example/readyz" },
		"public loopback":   func(config *Config) { config.PublicHealthURL = "https://127.0.0.1/readyz" },
		"public wrong path": func(config *Config) { config.PublicHealthURL = "https://relay.example/healthz" },
		"public query":      func(config *Config) { config.PublicHealthURL = "https://relay.example/readyz?secret=x" },
	}
	for name, mutate := range tests {
		t.Run(name, func(t *testing.T) {
			config := base
			mutate(&config)
			if _, err := New(config); err == nil {
				t.Fatal("unsafe health boundary accepted")
			}
		})
	}
}

func TestNewAllowsOrdinarySupervisorWithoutManagedRuntimePointer(t *testing.T) {
	config := testConfig(t, t.TempDir())
	config.Managed = false
	config.ActiveRuntimePath = ""
	if _, err := New(config); err != nil {
		t.Fatalf("ordinary supervisor required a managed runtime pointer: %v", err)
	}
}

func TestSupervisorOwnsOneLifetimePerStateRootAndReleasesIt(t *testing.T) {
	root := t.TempDir()
	first, err := New(testConfig(t, root))
	if err != nil {
		t.Fatal(err)
	}
	started := make(chan struct{})
	first.runCycle = func(ctx context.Context, _ func(bool) error) cycleResult {
		close(started)
		<-ctx.Done()
		return cycleResult{cancelled: true}
	}
	ctx, cancel := context.WithCancel(context.Background())
	firstDone := make(chan error, 1)
	go func() { firstDone <- first.Run(ctx) }()
	<-started

	second, err := New(testConfig(t, root))
	if err != nil {
		t.Fatal(err)
	}
	secondRan := false
	second.runCycle = func(context.Context, func(bool) error) cycleResult {
		secondRan = true
		return cycleResult{cancelled: true}
	}
	if err := second.Run(context.Background()); !errors.Is(err, ErrAlreadyRunning) {
		t.Fatalf("concurrent supervisor error = %v, want ErrAlreadyRunning", err)
	}
	if secondRan {
		t.Fatal("concurrent supervisor reached its child cycle")
	}

	cancel()
	if err := <-firstDone; err != nil {
		t.Fatalf("first supervisor stop = %v", err)
	}
	third, err := New(testConfig(t, root))
	if err != nil {
		t.Fatal(err)
	}
	third.runCycle = func(context.Context, func(bool) error) cycleResult { return cycleResult{cancelled: true} }
	if err := third.Run(context.Background()); err != nil {
		t.Fatalf("released lifetime lock was not reacquirable: %v", err)
	}
}

func TestVerifyRunningReadinessBindsLiveSupervisorAndExactManagedIdentity(t *testing.T) {
	root := t.TempDir()
	statePath := filepath.Join(root, "supervisor.json")
	if err := writeState(statePath, State{Status: StatusRunning}); err != nil {
		t.Fatal(err)
	}
	lock, err := acquireLifetimeLock(statePath + ".lock")
	if err != nil {
		t.Fatal(err)
	}
	body := `{"status":"ready","instance":"relay-personal","release_version":"1.2.3","revision":"revision-1","bundle_hash":"web-1","generation":"generation-1","expected_inventory":{"ready":true,"state":"ready","generation":"generation-1","expected":2,"observed":2}}`
	originalClient := readinessHTTPClient
	readinessHTTPClient = &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(body)), Header: make(http.Header), Request: request}, nil
	})}
	t.Cleanup(func() { readinessHTTPClient = originalClient })

	expectation := ReadinessExpectation{
		Managed: true, Instance: "relay-personal", ReleaseVersion: "1.2.3", Revision: "revision-1", BundleHash: "web-1", Generation: "generation-1",
	}
	if err := VerifyRunningReadiness(context.Background(), statePath, "http://127.0.0.1:8375/readyz", "https://relay.example/readyz", expectation); err != nil {
		t.Fatalf("exact running readiness rejected: %v", err)
	}

	for name, mutate := range map[string]func(*ReadinessExpectation){
		"instance": func(value *ReadinessExpectation) { value.Instance = "other" },
		"version":  func(value *ReadinessExpectation) { value.ReleaseVersion = "9.9.9" },
		"revision": func(value *ReadinessExpectation) { value.Revision = "other" },
		"web hash": func(value *ReadinessExpectation) { value.BundleHash = "other" },
		"generation": func(value *ReadinessExpectation) {
			value.Generation = "other"
		},
	} {
		t.Run(name, func(t *testing.T) {
			changed := expectation
			mutate(&changed)
			if err := VerifyRunningReadiness(context.Background(), statePath, "http://127.0.0.1:8375/readyz", "https://relay.example/readyz", changed); err == nil {
				t.Fatal("mismatched readiness identity accepted")
			}
		})
	}

	body = `{"status":"acknowledged_empty","instance":"relay-personal","release_version":"1.2.3","revision":"revision-1","bundle_hash":"web-1","generation":"generation-1","expected_inventory":{"ready":true,"state":"acknowledged_empty","generation":"generation-1","expected":0,"observed":0}}`
	if err := VerifyRunningReadiness(context.Background(), statePath, "http://127.0.0.1:8375/readyz", "https://relay.example/readyz", expectation); err != nil {
		t.Fatalf("explicit acknowledged-empty readiness rejected: %v", err)
	}
	body = `{"status":"acknowledged_empty","instance":"relay-personal","release_version":"1.2.3","revision":"revision-1","bundle_hash":"web-1","generation":"generation-1","expected_inventory":{"ready":true,"state":"ready","generation":"generation-1","expected":0,"observed":0}}`
	if err := VerifyRunningReadiness(context.Background(), statePath, "http://127.0.0.1:8375/readyz", "https://relay.example/readyz", expectation); err == nil {
		t.Fatal("self-asserted acknowledged-empty readiness accepted")
	}

	if err := lock.Close(); err != nil {
		t.Fatal(err)
	}
	body = `{"status":"ready","instance":"relay-personal","release_version":"1.2.3","revision":"revision-1","bundle_hash":"web-1","generation":"generation-1","expected_inventory":{"ready":true,"state":"ready","generation":"generation-1","expected":2,"observed":2}}`
	if err := VerifyRunningReadiness(context.Background(), statePath, "http://127.0.0.1:8375/readyz", "https://relay.example/readyz", expectation); err == nil {
		t.Fatal("stale running state without a live supervisor accepted")
	}
}

func TestPermanentFailureTripsAtCapAndRequiresExplicitReset(t *testing.T) {
	root := t.TempDir()
	config := testConfig(t, root)
	config.MaxFailures = 3
	starter := &fakeStarter{failRelay: true}
	supervisor, err := New(config)
	if err != nil {
		t.Fatal(err)
	}
	supervisor.start = starter.Start
	supervisor.health = func(context.Context) error { return nil }

	err = supervisor.Run(context.Background())
	if !errors.Is(err, ErrTripped) {
		t.Fatalf("run error = %v, want tripped", err)
	}
	if starter.Starts("relay") != 3 || starter.Starts("cloudflared") != 3 {
		t.Fatalf("starts = relay %d, tunnel %d", starter.Starts("relay"), starter.Starts("cloudflared"))
	}
	state, err := ReadState(config.StatePath)
	if err != nil {
		t.Fatal(err)
	}
	if state.Status != StatusTripped || state.Failures != 3 || !strings.Contains(state.Reason, "fixture relay exit") {
		t.Fatalf("tripped state = %+v", state)
	}
	info, err := os.Stat(config.StatePath)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("state mode = %o", info.Mode().Perm())
	}

	before := starter.Starts("relay")
	if err := supervisor.Run(context.Background()); !errors.Is(err, ErrTripped) {
		t.Fatalf("tripped rerun = %v", err)
	}
	if starter.Starts("relay") != before {
		t.Fatal("tripped supervisor spawned another child")
	}
	if err := Reset(config.StatePath); err != nil {
		t.Fatal(err)
	}
	state, err = ReadState(config.StatePath)
	if err != nil || state.Status != StatusReset || state.Failures != 0 {
		t.Fatalf("reset state = %+v, %v", state, err)
	}
}

func TestHungHealthStopsWholePairBeforeRetry(t *testing.T) {
	root := t.TempDir()
	config := testConfig(t, root)
	config.MaxFailures = 2
	starter := &fakeStarter{}
	supervisor, err := New(config)
	if err != nil {
		t.Fatal(err)
	}
	supervisor.start = starter.Start
	supervisor.health = func(ctx context.Context) error {
		<-ctx.Done()
		return ctx.Err()
	}

	err = supervisor.Run(context.Background())
	if !errors.Is(err, ErrTripped) {
		t.Fatalf("hung run = %v", err)
	}
	if starter.MaxActive() > 2 || starter.Active() != 0 {
		t.Fatalf("child overlap active=%d max=%d", starter.Active(), starter.MaxActive())
	}
	if starter.Starts("relay") != 2 || starter.Starts("cloudflared") != 2 {
		t.Fatalf("retry starts = relay %d, tunnel %d", starter.Starts("relay"), starter.Starts("cloudflared"))
	}
	if starter.Signals() != 4 {
		t.Fatalf("graceful stop signals = %d, want 4", starter.Signals())
	}
}

func TestCancellationStopsOwnedChildrenWithoutTripping(t *testing.T) {
	root := t.TempDir()
	starter := &fakeStarter{}
	supervisor, err := New(testConfig(t, root))
	if err != nil {
		t.Fatal(err)
	}
	supervisor.start = starter.Start
	supervisor.health = func(context.Context) error { return nil }
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- supervisor.Run(ctx) }()
	deadline := time.Now().Add(time.Second)
	for starter.Active() != 2 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatalf("cancelled run = %v", err)
	}
	if starter.Active() != 0 || starter.Signals() != 2 {
		t.Fatalf("cancel cleanup active=%d signals=%d", starter.Active(), starter.Signals())
	}
	state, err := ReadState(supervisor.config.StatePath)
	if err != nil {
		t.Fatal(err)
	}
	if state.Status != StatusStopped || state.Failures != 0 {
		t.Fatalf("cancel state = %+v", state)
	}
}

func TestCancellationDuringHealthyReadinessCheckStopsWithoutFailure(t *testing.T) {
	service := newInjectedSupervisor(t)
	service.config.HealthInterval = time.Millisecond
	starter := &fakeStarter{}
	service.start = starter.Start
	healthEntered := make(chan struct{})
	checks := 0
	service.health = func(ctx context.Context) error {
		checks++
		if checks == 1 {
			return nil
		}
		close(healthEntered)
		<-ctx.Done()
		return errors.New("cancelled health request")
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan cycleResult, 1)
	go func() { done <- service.runPair(ctx, func(bool) error { return nil }) }()
	<-healthEntered
	cancel()
	result := <-done
	if !result.cancelled || result.unsafe || result.reason != "" {
		t.Fatalf("cancelled readiness result = %+v", result)
	}
}

func TestHealthyPairTripsWhenCurrentReleaseIdentityDrifts(t *testing.T) {
	for _, test := range []struct {
		name   string
		mutate func(*release.Manifest, *error)
	}{
		{name: "web identity changes", mutate: func(manifest *release.Manifest, _ *error) { manifest.WebHash = "web-2" }},
		{name: "release becomes unreadable", mutate: func(_ *release.Manifest, err *error) { *err = errors.New("manifest unreadable") }},
	} {
		t.Run(test.name, func(t *testing.T) {
			config := testConfig(t, t.TempDir())
			config.HealthInterval = time.Millisecond
			config.HealthTimeout = time.Second
			service, err := New(config)
			if err != nil {
				t.Fatal(err)
			}
			starter := &fakeStarter{}
			service.start = starter.Start
			service.openLog = func(string, int64, int) (io.Writer, error) { return io.Discard, nil }
			manifest := release.Manifest{Version: "1.2.3", Revision: "revision-1", WebHash: "web-1"}
			var releaseErr error
			checks := 0
			service.release = func() (release.Manifest, error) {
				checks++
				return manifest, releaseErr
			}
			body := `{"status":"ready","instance":"relay-test","release_version":"1.2.3","revision":"revision-1","bundle_hash":"web-1","generation":"g1","expected_inventory":{"ready":true,"state":"ready","generation":"g1","expected":1,"observed":1}}`
			originalClient := readinessHTTPClient
			readinessHTTPClient = &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
				return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(body)), Header: make(http.Header), Request: request}, nil
			})}
			t.Cleanup(func() { readinessHTTPClient = originalClient })

			result := service.runPair(context.Background(), func(bool) error {
				test.mutate(&manifest, &releaseErr)
				return nil
			})
			if !strings.Contains(result.reason, "joint readiness failed") || result.cancelled || result.unsafe {
				t.Fatalf("release drift result = %+v", result)
			}
			if checks < 2 {
				t.Fatalf("release verification checks = %d, want continuous verification", checks)
			}
		})
	}
}

func TestCancelledContextWinsWhenClassifyingChildCompletion(t *testing.T) {
	service := newInjectedSupervisor(t)
	starter := &fakeStarter{}
	service.start = starter.Start
	relay, err := service.startChild(service.config.Relay, io.Discard, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	tunnel, err := service.startChild(service.config.Tunnel, io.Discard, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	result, stopped := service.stopIfCancelled(ctx, relay, tunnel)
	if !stopped || !result.cancelled || result.unsafe || result.reason != "" {
		t.Fatalf("cancelled child completion result = %+v, stopped=%t", result, stopped)
	}
}

func TestStopChildrenTerminatesTheWholeOwnedProcessGroup(t *testing.T) {
	root := t.TempDir()
	childPath := filepath.Join(root, "descendant.pid")
	script := filepath.Join(root, "tree.sh")
	contents := "#!/bin/sh\n/bin/sleep 30 &\nprintf '%s' \"$!\" > \"" + childPath + "\"\nwait\n"
	if err := os.WriteFile(script, []byte(contents), 0o700); err != nil {
		t.Fatal(err)
	}
	service := newInjectedSupervisor(t)
	process, err := startCommand(Command{Name: "tree", Path: script}, io.Discard, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	running := &runningChild{name: "tree", process: process, done: make(chan struct{})}
	go func() {
		running.err = process.Wait()
		close(running.done)
	}()
	var descendant int
	deadline := time.Now().Add(2 * time.Second)
	for descendant == 0 && time.Now().Before(deadline) {
		data, readErr := os.ReadFile(childPath)
		if readErr == nil {
			descendant, _ = strconv.Atoi(string(data))
		}
		time.Sleep(5 * time.Millisecond)
	}
	if descendant == 0 {
		_ = process.Kill()
		t.Fatal("descendant pid was not recorded")
	}
	t.Cleanup(func() { _ = syscall.Kill(descendant, syscall.SIGKILL) })
	if err := service.stopChildren(running, nil); err != nil {
		t.Fatal(err)
	}
	if err := syscall.Kill(descendant, 0); !errors.Is(err, syscall.ESRCH) {
		t.Fatalf("descendant %d survived group stop: %v", descendant, err)
	}
}

func TestBackoffCapsExponentially(t *testing.T) {
	config := testConfig(t, t.TempDir())
	config.InitialBackoff = 10 * time.Millisecond
	config.MaxBackoff = 25 * time.Millisecond
	want := []time.Duration{10 * time.Millisecond, 20 * time.Millisecond, 25 * time.Millisecond, 25 * time.Millisecond}
	for index, expected := range want {
		if got := retryDelay(config, index+1); got != expected {
			t.Fatalf("failure %d delay = %s, want %s", index+1, got, expected)
		}
	}
}

func TestRotatingWriterBoundsLogsAndBackups(t *testing.T) {
	path := filepath.Join(t.TempDir(), "relay.stdout.log")
	writer, err := newRotatingWriter(path, 12, 2)
	if err != nil {
		t.Fatal(err)
	}
	for _, chunk := range []string{"12345678", "abcdefgh", "ABCDEFGH", "oversized-payload-that-is-truncated"} {
		if _, err := io.WriteString(writer, chunk); err != nil {
			t.Fatal(err)
		}
	}
	for _, candidate := range []string{path, path + ".1", path + ".2"} {
		info, err := os.Stat(candidate)
		if err != nil {
			t.Fatal(err)
		}
		if info.Size() > 12 {
			t.Fatalf("%s grew to %d bytes", candidate, info.Size())
		}
	}
	if _, err := os.Stat(path + ".3"); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("extra backup exists: %v", err)
	}
}

func TestJointReadinessRequiresMatchingLocalAndPublicRelay(t *testing.T) {
	local := httptest.NewServer(readyHandler(http.StatusOK, "relay-1", "revision-1", "generation-1"))
	defer local.Close()
	var publicStatus atomic.Int64
	publicStatus.Store(http.StatusServiceUnavailable)
	publicIdentity := atomic.Value{}
	publicIdentity.Store([3]string{"relay-1", "revision-1", "generation-1"})
	public := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		identity := publicIdentity.Load().([3]string)
		readyHandler(int(publicStatus.Load()), identity[0], identity[1], identity[2]).ServeHTTP(writer, request)
	}))
	defer public.Close()

	if err := checkJointReadiness(context.Background(), local.URL, public.URL); err == nil || !strings.Contains(err.Error(), "public") {
		t.Fatalf("public failure = %v", err)
	}
	publicStatus.Store(http.StatusOK)
	publicIdentity.Store([3]string{"other-relay", "revision-1", "generation-1"})
	if err := checkJointReadiness(context.Background(), local.URL, public.URL); err == nil || !strings.Contains(err.Error(), "identity") {
		t.Fatalf("misrouted public relay = %v", err)
	}
	publicIdentity.Store([3]string{"relay-1", "revision-1", "generation-1"})
	if err := checkJointReadiness(context.Background(), local.URL, public.URL); err != nil {
		t.Fatalf("matching local/public readiness = %v", err)
	}
}

func TestJointManagedReadinessRequiresExactInventoryProof(t *testing.T) {
	body := `{"status":"ready","instance":"relay-1","release_version":"test-version","revision":"revision-1","bundle_hash":"test-web","generation":"generation-1"}`
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(writer, body)
	}))
	defer server.Close()
	if err := checkJointReadiness(context.Background(), server.URL, server.URL); err == nil || !strings.Contains(err.Error(), "expected-inventory proof") {
		t.Fatalf("managed readiness without exact inventory = %v", err)
	}
}

func TestReadinessRefusesRedirectsAcrossItsPinnedBoundary(t *testing.T) {
	target := httptest.NewServer(readyHandler(http.StatusOK, "relay-1", "revision-1", "generation-1"))
	defer target.Close()
	redirect := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		http.Redirect(writer, request, target.URL, http.StatusFound)
	}))
	defer redirect.Close()
	if _, err := readReadiness(context.Background(), redirect.URL); err == nil {
		t.Fatal("redirected readiness response accepted")
	}
}

func TestJointReadinessPublicRedNeverRunsThenRecovers(t *testing.T) {
	root := t.TempDir()
	local := httptest.NewServer(readyHandler(http.StatusOK, "relay-1", "revision-1", "g1"))
	defer local.Close()
	var publicStatus atomic.Int64
	publicStatus.Store(http.StatusServiceUnavailable)
	public := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		readyHandler(int(publicStatus.Load()), "relay-1", "revision-1", "g1").ServeHTTP(writer, request)
	}))
	defer public.Close()
	config := testConfig(t, root)
	config.StartupTimeout = time.Second
	config.HealthTimeout = 100 * time.Millisecond
	config.HealthInterval = 5 * time.Millisecond
	starter := &fakeStarter{}
	supervisor, err := New(config)
	if err != nil {
		t.Fatal(err)
	}
	supervisor.start = starter.Start
	supervisor.health = func(ctx context.Context) error {
		return checkJointReadiness(ctx, local.URL, public.URL)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- supervisor.Run(ctx) }()
	waitForStarts(t, starter, "relay", 1)
	waitForState(t, config.StatePath, StatusStarting)
	time.Sleep(25 * time.Millisecond)
	state, err := ReadState(config.StatePath)
	if err != nil {
		t.Fatal(err)
	}
	if state.Status != StatusStarting {
		t.Fatalf("local-green/public-red state = %s, want starting", state.Status)
	}
	publicStatus.Store(http.StatusOK)
	waitForState(t, config.StatePath, StatusRunning)
	cancel()
	if err := <-done; err != nil {
		t.Fatalf("recovered supervisor stop = %v", err)
	}
}

func TestStableFailureCounterRequiresContinuouslyHealthyWindow(t *testing.T) {
	root := t.TempDir()
	config := testConfig(t, root)
	config.StartupTimeout = time.Second
	config.HealthInterval = time.Millisecond
	config.HealthTimeout = 20 * time.Millisecond
	config.StableAfter = 100 * time.Millisecond
	config.StopTimeout = 20 * time.Millisecond
	starter := &fakeStarter{}
	service, err := New(config)
	if err != nil {
		t.Fatal(err)
	}
	service.start = starter.Start
	started := time.Now()
	service.health = func(context.Context) error {
		if time.Since(started) < 80*time.Millisecond {
			return errors.New("warming")
		}
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()
	stableReports := 0
	result := service.runPair(ctx, func(stable bool) error {
		if stable {
			stableReports++
		}
		return nil
	})
	if !result.cancelled || result.unsafe {
		t.Fatalf("cancelled warming pair = %+v", result)
	}
	if stableReports != 0 {
		t.Fatalf("stable reports = %d before one continuously healthy window", stableReports)
	}
}

func TestActiveRuntimePromotionRestartsPairWithOneCoherentSnapshot(t *testing.T) {
	root := t.TempDir()
	config := testConfig(t, root)
	config.StartupTimeout = time.Second
	config.HealthInterval = 5 * time.Millisecond
	starter := &fakeStarter{}
	supervisor, err := New(config)
	if err != nil {
		t.Fatal(err)
	}
	supervisor.start = starter.Start
	supervisor.health = func(context.Context) error { return nil }
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- supervisor.Run(ctx) }()
	waitForStarts(t, starter, "relay", 1)
	waitForState(t, config.StatePath, StatusRunning)

	writeActiveRuntimeFixture(t, root, "g2")
	waitForStarts(t, starter, "relay", 2)
	commands := starter.Commands("relay")
	if len(commands) != 2 {
		t.Fatalf("relay commands = %d, want two generations", len(commands))
	}
	for index, generation := range []string{"g1", "g2"} {
		sessionRoot := filepath.Join(root, "sessions", generation)
		assertEnvironmentValue(t, commands[index].Env, "HERDR_RELAY_ACTIVE_GENERATION", generation)
		assertEnvironmentValue(t, commands[index].Env, "HERDR_SOCKET_PATH", filepath.Join(sessionRoot, "herdr.sock"))
		assertEnvironmentValue(t, commands[index].Env, "HERDR_RELAY_EXPECTED_INVENTORY", filepath.Join(sessionRoot, "expected-inventory.json"))
		assertEnvironmentValue(t, commands[index].Env, "HERDR_RELAY_ACTIVE_RUNTIME", config.ActiveRuntimePath)
	}
	if starter.MaxActive() > 2 {
		t.Fatalf("promotion overlapped child pairs: max active = %d", starter.MaxActive())
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatalf("promoted supervisor stop = %v", err)
	}
}

func testConfig(t *testing.T, root string) Config {
	t.Helper()
	writeActiveRuntimeFixture(t, root, "g1")
	return Config{
		Managed:   true,
		Relay:     Command{Name: "relay", Path: "/fixture/relay", Args: []string{"serve"}},
		Tunnel:    Command{Name: "cloudflared", Path: "/fixture/cloudflared", Args: []string{"tunnel", "run"}},
		StatePath: filepath.Join(root, "supervisor-state.json"), LogDir: filepath.Join(root, "logs"),
		ReleaseRoot: filepath.Join(root, "release"), Instance: "relay-test",
		ActiveRuntimePath: filepath.Join(root, "active-runtime.json"),
		LocalHealthURL:    "http://127.0.0.1:8375/readyz",
		PublicHealthURL:   "https://relay.example/readyz",
		MaxFailures:       3, InitialBackoff: time.Millisecond, MaxBackoff: 2 * time.Millisecond,
		StartupTimeout: 4 * time.Millisecond, HealthInterval: time.Millisecond, HealthTimeout: time.Millisecond,
		StableAfter: time.Hour, StopTimeout: 20 * time.Millisecond, MaxLogBytes: 1024, LogBackups: 2,
	}
}

func readyHandler(status int, instance, revision, generation string) http.Handler {
	return http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Type", "application/json")
		writer.WriteHeader(status)
		_ = json.NewEncoder(writer).Encode(map[string]any{
			"status": "ready", "instance": instance, "release_version": "test-version", "revision": revision, "bundle_hash": "test-web", "generation": generation,
			"expected_inventory": map[string]any{"ready": true, "state": "ready", "generation": generation, "expected": 1, "observed": 1},
		})
	})
}

func writeActiveRuntimeFixture(t *testing.T, root, generation string) {
	t.Helper()
	sessionRoot := filepath.Join(root, "sessions", generation)
	content := fmt.Sprintf(
		`{"schemaVersion":1,"generation":%q,"sessionName":%q,"socketPath":%q,"expectedInventoryPath":%q}`,
		generation, generation, filepath.Join(sessionRoot, "herdr.sock"), filepath.Join(sessionRoot, "expected-inventory.json"),
	)
	temporary := filepath.Join(root, ".active-runtime.fixture")
	if err := os.WriteFile(temporary, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(temporary, filepath.Join(root, "active-runtime.json")); err != nil {
		t.Fatal(err)
	}
}

func waitForStarts(t *testing.T, starter *fakeStarter, name string, count int) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for starter.Starts(name) < count && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if got := starter.Starts(name); got < count {
		t.Fatalf("%s starts = %d, want at least %d", name, got, count)
	}
}

func waitForState(t *testing.T, path string, want Status) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		state, err := ReadState(path)
		if err == nil && state.Status == want {
			return
		}
		time.Sleep(time.Millisecond)
	}
	state, err := ReadState(path)
	t.Fatalf("state = %+v, %v, want %s", state, err, want)
}

func assertEnvironmentValue(t *testing.T, environment []string, key, want string) {
	t.Helper()
	prefix := key + "="
	for _, entry := range environment {
		if strings.HasPrefix(entry, prefix) {
			if got := strings.TrimPrefix(entry, prefix); got != want {
				t.Fatalf("%s = %q, want %q", key, got, want)
			}
			return
		}
	}
	t.Fatalf("%s missing from %v", key, environment)
}

type fakeStarter struct {
	mu        sync.Mutex
	active    int
	maxActive int
	starts    map[string]int
	signals   int
	failRelay bool
	failStart string
	commands  []Command
}

func (f *fakeStarter) Start(command Command, stdout, stderr io.Writer) (process, error) {
	_, _ = stdout, stderr
	f.mu.Lock()
	if f.failStart == command.Name {
		f.mu.Unlock()
		return nil, errors.New("fixture " + command.Name + " start")
	}
	if f.starts == nil {
		f.starts = make(map[string]int)
	}
	f.starts[command.Name]++
	f.commands = append(f.commands, command)
	f.active++
	if f.active > f.maxActive {
		f.maxActive = f.active
	}
	f.mu.Unlock()
	child := &fakeProcess{starter: f, done: make(chan error, 1)}
	if f.failRelay && command.Name == "relay" {
		child.finish(errors.New("fixture relay exit"))
	}
	return child, nil
}

func (f *fakeStarter) Starts(name string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.starts[name]
}
func (f *fakeStarter) Active() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.active
}
func (f *fakeStarter) MaxActive() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.maxActive
}
func (f *fakeStarter) Signals() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.signals
}
func (f *fakeStarter) Commands(name string) []Command {
	f.mu.Lock()
	defer f.mu.Unlock()
	var result []Command
	for _, command := range f.commands {
		if command.Name == name {
			result = append(result, command)
		}
	}
	return result
}

type fakeProcess struct {
	starter *fakeStarter
	done    chan error
	once    sync.Once
}

func (p *fakeProcess) Wait() error { return <-p.done }
func (p *fakeProcess) Signal(os.Signal) error {
	p.starter.mu.Lock()
	p.starter.signals++
	p.starter.mu.Unlock()
	p.finish(errors.New("terminated"))
	return nil
}
func (p *fakeProcess) Kill() error {
	p.finish(errors.New("killed"))
	return nil
}
func (p *fakeProcess) Alive() bool {
	select {
	case <-p.done:
		return false
	default:
		return true
	}
}
func (p *fakeProcess) finish(err error) {
	p.once.Do(func() {
		p.starter.mu.Lock()
		p.starter.active--
		p.starter.mu.Unlock()
		p.done <- err
		close(p.done)
	})
}
