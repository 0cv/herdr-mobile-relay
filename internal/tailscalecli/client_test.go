package tailscalecli

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/0cv/herdr-mobile-relay/internal/tailscale"
)

// These are source-derived synthetic fixtures, not captured CLI output or a
// live daemon. The App Store metadata values came from the task handoff.
const fixtureLong = "1.102.4-t3caf7d9e7d-g084ee3b64537"
const fixtureStatus = `{"Version":"` + fixtureLong + `","BackendState":"Running","Self":{"ID":"node-fixture","UserID":123,"DNSName":"herdr.tailnet.ts.net."},"CurrentTailnet":{"Name":"fixture-account","MagicDNSSuffix":"tailnet.ts.net","MagicDNSEnabled":true},"CertDomains":["herdr.tailnet.ts.net"],"User":{"123":{"ID":123,"LoginName":"private@example.invalid","DisplayName":"Fixture","ProfilePicURL":""}}}`
const fixtureVersion = `{"majorMinorPatch":"1.102.4","short":"1.102.4","long":"` + fixtureLong + `","gitCommit":"3caf7d9e7dcaba589cfc58beda596929733e4fea","daemonLong":"` + fixtureLong + `","extraGitCommit":"084ee3b64537a1276e56fc38cdf0a711da9f4936","osVariant":"appstore","cap":142}`
const fixtureRoute = `{"TCP":{"8443":{"HTTPS":true}},"Web":{"herdr.tailnet.ts.net:8443":{"Handlers":{"/":{"Proxy":"http://127.0.0.1:18377"}}}}}`
const unrelatedRoute = `{"TCP":{"443":{"HTTPS":true}},"Web":{"other.tailnet.ts.net:443":{"Handlers":{"/":{"Proxy":"http://127.0.0.1:8080"}}}}}`
const unrelatedAndFixtureRoutes = `{"TCP":{"443":{"HTTPS":true},"8443":{"HTTPS":true}},"Web":{"other.tailnet.ts.net:443":{"Handlers":{"/":{"Proxy":"http://127.0.0.1:8080"}}},"herdr.tailnet.ts.net:8443":{"Handlers":{"/":{"Proxy":"http://127.0.0.1:18377"}}}}}`

type fakeCLI struct {
	t  *testing.T
	mu sync.Mutex

	status  string
	version string
	serve   string
	calls   [][]string

	publishErr        error
	publishDispatched bool
	publishNoEffect   bool
	removeErr         error
	removeDispatched  bool
	removeNoEffect    bool
	raceRoute         string
}

func newFakeCLI(t *testing.T) *fakeCLI {
	t.Helper()
	return &fakeCLI{t: t, status: fixtureStatus, version: fixtureVersion, serve: `{}`}
}

func (f *fakeCLI) run(_ context.Context, binary string, args ...string) (commandResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if binary != "/fixture path/tailscale" {
		f.t.Fatalf("fake runner received unexpected executable %q", binary)
	}
	f.calls = append(f.calls, append([]string(nil), args...))
	switch strings.Join(args, " ") {
	case "status --json":
		return commandResult{stdout: []byte(f.status), dispatched: true}, nil
	case "version --json --daemon":
		return commandResult{stdout: []byte(f.version), dispatched: true}, nil
	case "serve status --json":
		return commandResult{stdout: []byte(f.serve), dispatched: true}, nil
	case "serve --bg --https=8443 --set-path=/ http://127.0.0.1:18377":
		if f.publishErr != nil {
			return commandResult{dispatched: f.publishDispatched}, f.publishErr
		}
		if f.raceRoute != "" {
			// Model an external writer changing the selected listener after the
			// manager's final read-only inspection but before CLI dispatch.
			f.serve = f.raceRoute
			f.raceRoute = ""
		}
		if !f.publishNoEffect {
			if f.serve == unrelatedRoute {
				f.serve = unrelatedAndFixtureRoutes
			} else {
				f.serve = fixtureRoute
			}
		}
		return commandResult{dispatched: true}, nil
	case "serve --bg --https=8443 --set-path=/ off":
		if f.removeErr != nil {
			return commandResult{dispatched: f.removeDispatched}, f.removeErr
		}
		if !f.removeNoEffect {
			if f.serve == unrelatedAndFixtureRoutes {
				f.serve = unrelatedRoute
			} else {
				f.serve = `{}`
			}
		}
		return commandResult{dispatched: true}, nil
	default:
		f.t.Fatalf("fake runner intercepted unexpected argv: %q", strings.Join(args, " "))
		return commandResult{}, ErrUnsupported
	}
}

func newFixtureClient(f *fakeCLI) *Client {
	return newTestClientForPlatform("/fixture path/tailscale", f.run, "darwin", "arm64")
}

func TestFixtureBuildRejectsOrdinaryExecutableBeforeExecution(t *testing.T) {
	if !fixtureRuntimeQualificationEnabled() {
		t.Skip("fixture executable admission applies only to tagged fixture builds")
	}
	marker := filepath.Join(t.TempDir(), "ordinary-cli-was-executed")
	ordinary := filepath.Join(t.TempDir(), "tailscale")
	if err := os.WriteFile(ordinary, []byte("#!/bin/sh\ntouch \""+marker+"\"\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := NewClient(ordinary); !errors.Is(err, ErrProfileUnavailable) {
		t.Fatalf("fixture build accepted ordinary executable: %v", err)
	}
	if _, err := os.Stat(marker); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("ordinary executable was contacted during fixture validation: %v", err)
	}

	synthetic := filepath.Join(t.TempDir(), "tailscale-fixture")
	if err := os.WriteFile(synthetic, []byte("#!/bin/sh\n# "+syntheticFixtureCLIMarker+"\nexit 0\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := NewClient(synthetic); err != nil {
		t.Fatalf("tagged fixture rejected its marked synthetic CLI: %v", err)
	}
}

type coexistenceCLI struct {
	t        *testing.T
	mu       sync.Mutex
	backends map[int]int
	calls    [][]string
}

func (f *coexistenceCLI) run(_ context.Context, binary string, args ...string) (commandResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if binary != "/fixture path/tailscale" {
		f.t.Fatalf("coexistence fixture received unexpected executable %q", binary)
	}
	f.calls = append(f.calls, append([]string(nil), args...))
	switch strings.Join(args, " ") {
	case "status --json":
		return commandResult{stdout: []byte(fixtureStatus), dispatched: true}, nil
	case "version --json --daemon":
		return commandResult{stdout: []byte(fixtureVersion), dispatched: true}, nil
	case "serve status --json":
		if len(f.backends) == 0 {
			return commandResult{stdout: []byte(`{}`), dispatched: true}, nil
		}
		tcp := make(map[string]map[string]bool, len(f.backends))
		web := make(map[string]map[string]map[string]map[string]string, len(f.backends))
		for httpsPort, backendPort := range f.backends {
			port := strconv.Itoa(httpsPort)
			tcp[port] = map[string]bool{"HTTPS": true}
			web["herdr.tailnet.ts.net:"+port] = map[string]map[string]map[string]string{
				"Handlers": {"/": {"Proxy": "http://127.0.0.1:" + strconv.Itoa(backendPort)}},
			}
		}
		encoded, err := json.Marshal(map[string]any{"TCP": tcp, "Web": web})
		return commandResult{stdout: encoded, dispatched: true}, err
	}
	if len(args) == 5 && args[0] == "serve" && args[1] == "--bg" && args[3] == "--set-path=/" {
		httpsPort, err := strconv.Atoi(strings.TrimPrefix(args[2], "--https="))
		if err != nil {
			return commandResult{}, ErrUnsupported
		}
		if args[4] == "off" {
			delete(f.backends, httpsPort)
			return commandResult{dispatched: true}, nil
		}
		backendPort, err := strconv.Atoi(strings.TrimPrefix(args[4], "http://127.0.0.1:"))
		if err != nil || !strings.HasPrefix(args[4], "http://127.0.0.1:") {
			return commandResult{}, ErrUnsupported
		}
		f.backends[httpsPort] = backendPort
		return commandResult{dispatched: true}, nil
	}
	return commandResult{}, ErrUnsupported
}

func newFixtureManager(t *testing.T, f *fakeCLI, stateLeaf string) *Manager {
	t.Helper()
	base := t.TempDir()
	stateRoot := filepath.Join(base, stateLeaf)
	coordinationRoot := filepath.Join(base, "shared coordination")
	if err := os.Mkdir(stateRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(coordinationRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	manager, err := NewManager(stateRoot, coordinationRoot, newFixtureClient(f))
	if err != nil {
		t.Fatal(err)
	}
	manager.fixtureMutations = true     // unexported, package-test-only fixture capability
	manager.skipBackendReadiness = true // ordinary CLI fixtures do not start a loopback relay
	return manager
}

func fixtureRequest(consent bool) PublishRequest {
	return PublishRequest{
		InstallationID: "install-fixture",
		Scope:          "development",
		ExpectedNodeID: "node-fixture",
		Origin:         "https://herdr.tailnet.ts.net:8443",
		HTTPSPort:      8443,
		BackendPort:    18377,
		ReservationID:  "00000000000000000000000000000001",
		Consent:        fixtureConsent(consent),
	}
}

func fixtureConsent(accepted bool) Consent {
	return Consent{
		Accepted:                 accepted,
		Scope:                    "development",
		NodeID:                   "node-fixture",
		Origin:                   "https://herdr.tailnet.ts.net:8443",
		HTTPSPort:                8443,
		BackendPort:              18377,
		PersistentRouteAccepted:  accepted,
		RouteRemovalAccepted:     accepted,
		CheckToWriteRaceAccepted: accepted,
		PortReuseRiskAccepted:    accepted,
		NoRollbackAccepted:       accepted,
		NoRemoteDrainAccepted:    accepted,
	}
}

func (f *fakeCLI) mutationCalls() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	count := 0
	for _, call := range f.calls {
		if len(call) > 1 && call[0] == "serve" && call[1] == "--bg" {
			count++
		}
	}
	return count
}

func TestResolveBinaryAbsoluteAndAmbiguousCandidates(t *testing.T) {
	base := t.TempDir()
	firstDir := filepath.Join(base, "path with spaces")
	secondDir := filepath.Join(base, "another path")
	if err := os.Mkdir(firstDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(secondDir, 0o700); err != nil {
		t.Fatal(err)
	}
	first := filepath.Join(firstDir, "tailscale")
	second := filepath.Join(secondDir, "tailscale")
	for _, path := range []string{first, second} {
		if err := os.WriteFile(path, []byte("fixture executable"), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	canonicalFirst, canonicalErr := filepath.EvalSymlinks(first)
	if canonicalErr != nil {
		t.Fatal(canonicalErr)
	}
	got, err := ResolveBinary("", firstDir+string(os.PathListSeparator)+"relative-path", "linux")
	if err != nil || got != canonicalFirst {
		t.Fatalf("absolute PATH candidate: got %q, err=%v", got, err)
	}
	got, err = ResolveBinary("", firstDir+string(os.PathListSeparator)+firstDir, "linux")
	if err != nil || got != canonicalFirst {
		t.Fatalf("duplicate canonical candidate: got %q, err=%v", got, err)
	}
	if _, err := ResolveBinary("relative/tailscale", firstDir, "linux"); !errors.Is(err, ErrProfileUnavailable) {
		t.Fatalf("relative override did not fail closed: %v", err)
	}
	got, err = ResolveBinary(first, secondDir, "linux")
	if err != nil || got != canonicalFirst {
		t.Fatalf("explicit override did not win: got %q, err=%v", got, err)
	}
	if _, err := ResolveBinary("", firstDir+string(os.PathListSeparator)+secondDir, "linux"); err == nil {
		t.Fatal("ambiguous PATH candidates were guessed")
	}
	if _, err := ResolveBinary("", filepath.Join(base, "missing"), "linux"); !errors.Is(err, ErrProfileUnavailable) {
		t.Fatalf("missing CLI did not fail closed: %v", err)
	}
}

func TestResolveAppStoreBundleCandidateFromPrivateFixture(t *testing.T) {
	bundleCLI := filepath.Join(t.TempDir(), "Tailscale.app", "Contents", "MacOS", "Tailscale")
	if err := os.MkdirAll(filepath.Dir(bundleCLI), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(bundleCLI, []byte("fixture executable"), 0o700); err != nil {
		t.Fatal(err)
	}
	expected, err := filepath.EvalSymlinks(bundleCLI)
	if err != nil {
		t.Fatal(err)
	}
	got, err := resolveBinary("", "", "darwin", bundleCLI)
	if err != nil || got != expected {
		t.Fatalf("fixture App Store candidate = %q, want %q, error %v", got, expected, err)
	}
}

func TestCLIEnvironmentUsesConstrainedPathAndDropsTailscaleOverrides(t *testing.T) {
	t.Setenv("HOME", "/Users/fixture home")
	t.Setenv("PATH", "/fixture/bin")
	t.Setenv("TS_SOCKET", "/private/socket")
	t.Setenv("TS_AUTHKEY", "private-key")
	t.Setenv("TS_DEBUG", "1")
	t.Setenv("HERDR_RELAY_ENV", "/private/relay.env")
	values := map[string]string{}
	for _, entry := range cliEnvironment() {
		key, value, ok := strings.Cut(entry, "=")
		if !ok {
			t.Fatalf("malformed environment entry %q", entry)
		}
		values[key] = value
	}
	if values["HOME"] != "/Users/fixture home" || values["PATH"] != "/usr/bin:/bin:/usr/sbin:/sbin" {
		t.Fatalf("required environment context or constrained PATH is wrong: %#v", values)
	}
	for _, key := range []string{"TS_SOCKET", "TS_AUTHKEY", "TS_DEBUG", "HERDR_RELAY_ENV"} {
		if _, ok := values[key]; ok {
			t.Errorf("unsafe override %s inherited", key)
		}
	}
}

func TestStrictVersionJSONAndProfileCandidates(t *testing.T) {
	metadata, err := parseVersion([]byte(fixtureVersion))
	if err != nil {
		t.Fatal(err)
	}
	if got := identifyProfileFor(metadata, "darwin", "arm64"); got != ProfileAppStoreSupplied {
		t.Fatalf("App Store source metadata profile = %q", got)
	}
	if got := identifyProfileFor(metadata, "linux", "amd64"); got != ProfileUnknown {
		t.Fatalf("App Store metadata accepted on Linux: %q", got)
	}
	linuxVersion := strings.ReplaceAll(fixtureVersion, "3caf7d9e7dcaba589cfc58beda596929733e4fea", tailscale.SourceCommit)
	linuxVersion = strings.ReplaceAll(linuxVersion, "084ee3b64537a1276e56fc38cdf0a711da9f4936", "")
	linuxVersion = strings.ReplaceAll(linuxVersion, `,"extraGitCommit":""`, "")
	linuxVersion = strings.ReplaceAll(linuxVersion, `,"osVariant":"appstore"`, "")
	linuxVersion = strings.ReplaceAll(linuxVersion, `"cap":142`, `"cap":141`)
	linuxLong := "1.102.4-tbbcd7d1fc"
	linuxVersion = strings.ReplaceAll(linuxVersion, fixtureLong, linuxLong)
	metadata, err = parseVersion([]byte(linuxVersion))
	if err != nil || identifyProfileFor(metadata, "linux", "amd64") != ProfileLinuxSource {
		t.Fatalf("Linux source candidate not recognized: %+v, %v", metadata, err)
	}
	for _, input := range []string{
		strings.Replace(fixtureVersion, `"cap":142`, `"cap":"142"`, 1),
		strings.Replace(fixtureVersion, `"cap":142`, `"cap":142,"CAP":142`, 1),
		strings.Replace(fixtureVersion, `"cap":142`, `"cap":142,"unknown":true`, 1),
		`{"long":`,
	} {
		if _, err := parseVersion([]byte(input)); err == nil {
			t.Errorf("invalid version metadata accepted: %q", input)
		}
	}
	dirty := strings.TrimSuffix(fixtureVersion, "}") + `,"gitDirty":true}`
	metadata, err = parseVersion([]byte(dirty))
	if err != nil || identifyProfileFor(metadata, "darwin", "arm64") != ProfileUnknown {
		t.Fatalf("dirty build was not rejected at profile admission: %+v, %v", metadata, err)
	}
}

func TestStrictJSONRejectsDuplicatesAliasesTruncationAndBounds(t *testing.T) {
	for _, input := range []string{
		`{"a":1,"a":2}`,
		`{"a":1,"\u0061":2}`,
		`{"a":1,"A":2}`,
		`{"nested":{"x":1,"X":2}}`,
		`{} {}`,
		`{"truncated":`,
		string([]byte{'{', '"', 0xff, '"', ':', '0', '}'}),
		strings.Repeat("[", 34) + "0" + strings.Repeat("]", 34),
	} {
		if err := validateJSON([]byte(input), MaxOutputBytes, 32, 100000); err == nil {
			t.Errorf("invalid JSON accepted: %q", input)
		}
	}
	if err := validateJSON([]byte(strings.Repeat(" ", MaxOutputBytes)+"{}"), MaxOutputBytes, 32, 100000); err == nil {
		t.Fatal("oversized JSON accepted")
	}
}

func TestInspectReadOnlyUsesFixedArgvAndDoesNotQualifyRuntime(t *testing.T) {
	fixture := newFakeCLI(t)
	inspection, err := newFixtureClient(fixture).Inspect(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if inspection.Profile != ProfileAppStoreSupplied || !inspection.ProfileKnown || inspection.RuntimeQualified {
		t.Fatalf("inspection profile state: %+v", inspection)
	}
	if inspection.Identity.NodeID != "node-fixture" || inspection.Identity.DNSName != "herdr.tailnet.ts.net" || inspection.Identity.UserID != 123 {
		t.Fatalf("unexpected identity projection: %+v", inspection.Identity)
	}
	if fixture.mutationCalls() != 0 || len(fixture.calls) != 3 {
		t.Fatalf("read-only inspect invoked unexpected commands: %v", fixture.calls)
	}
	want := []string{"status --json", "version --json --daemon", "serve status --json"}
	for i, args := range fixture.calls {
		if strings.Join(args, " ") != want[i] {
			t.Errorf("command %d = %q, want %q", i, strings.Join(args, " "), want[i])
		}
	}
}

func TestInspectRefusesLoggedOutUnknownSchemaAndRedactsCommandError(t *testing.T) {
	for _, tc := range []struct {
		state string
		want  error
	}{
		{state: "NeedsLogin", want: ErrLoggedOut},
		{state: "NeedsMachineAuth", want: ErrPermissionDenied},
		{state: "Starting", want: ErrTransientUnavailable},
		{state: "Stopped", want: ErrTransientUnavailable},
	} {
		fixture := newFakeCLI(t)
		fixture.status = `{"BackendState":"` + tc.state + `"}`
		if _, err := newFixtureClient(fixture).Inspect(context.Background()); !errors.Is(err, tc.want) {
			t.Errorf("backend state %s error = %v, want %v", tc.state, err, tc.want)
		}
	}
	fixture := newFakeCLI(t)
	fixture.status = strings.Replace(fixture.status, `"MagicDNSEnabled":true`, `"MagicDNSEnabled":false`, 1)
	if _, err := newFixtureClient(fixture).Inspect(context.Background()); !errors.Is(err, ErrUnsupported) {
		t.Fatalf("disabled MagicDNS identity was accepted: %v", err)
	}
	fixture = newFakeCLI(t)
	fixture.serve = `{"Services":{"svc:test":{"Tun":true}}}`
	if _, err := newFixtureClient(fixture).Inspect(context.Background()); !errors.Is(err, ErrUnsupported) {
		t.Fatalf("unknown Services exposure was not refused: %v", err)
	}
	fixture = newFakeCLI(t)
	fixture.serve = `{"TCP":`
	if _, err := newFixtureClient(fixture).Inspect(context.Background()); !errors.Is(err, ErrInvalidJSON) {
		t.Fatalf("malformed Serve JSON error class = %v", err)
	}
	fixture = newFakeCLI(t)
	secretOutput := errors.New("private@example.invalid credential-bearing stderr")
	client := newTestClientForPlatform("/fixture path/tailscale", func(_ context.Context, _ string, args ...string) (commandResult, error) {
		if strings.Join(args, " ") != "status --json" {
			t.Fatalf("unexpected read after failed status: %v", args)
		}
		return commandResult{dispatched: true}, secretOutput
	}, "darwin", "arm64")
	if _, err := client.Inspect(context.Background()); err == nil || !errors.Is(err, ErrUnclassified) ||
		errors.Is(err, ErrTransientUnavailable) || strings.Contains(err.Error(), "private@example.invalid") ||
		strings.Contains(err.Error(), "credential-bearing") {
		t.Fatalf("untyped runner failure was not safely redacted and kept non-retryable: %v", err)
	}
}

func TestReadOnlyIncompleteObservationIsExplicitlyTransient(t *testing.T) {
	client := newTestClient("/fixture path/tailscale", func(context.Context, string, ...string) (commandResult, error) {
		return commandResult{dispatched: true}, ErrUncertain
	})
	if _, err := client.Preflight(context.Background(), 8443); !errors.Is(err, ErrTransientUnavailable) {
		t.Fatalf("incomplete read-only observation = %v, want transient unavailable", err)
	}
}

func TestReadOnlyCallerCancellationRemainsCancellation(t *testing.T) {
	client := newTestClient("/fixture path/tailscale", func(context.Context, string, ...string) (commandResult, error) {
		return commandResult{dispatched: true}, context.Canceled
	})
	if _, err := client.Preflight(context.Background(), 8443); !errors.Is(err, context.Canceled) ||
		errors.Is(err, ErrTransientUnavailable) {
		t.Fatalf("caller cancellation was not preserved as non-retryable: %v", err)
	}
}

func TestPublishPersistsReceiptPreservesOtherListenerAndReusesWithoutWrite(t *testing.T) {
	fixture := newFakeCLI(t)
	fixture.serve = unrelatedRoute
	manager := newFixtureManager(t, fixture, "instance state")
	if err := manager.Publish(context.Background(), fixtureRequest(true)); err != nil {
		t.Fatal(err)
	}
	if fixture.mutationCalls() != 1 {
		t.Fatalf("publish writes = %d, want exactly one", fixture.mutationCalls())
	}
	if fixture.serve != unrelatedAndFixtureRoutes {
		t.Fatalf("unrelated route was not preserved: %s", fixture.serve)
	}
	path := filepath.Join(manager.stateRoot, journalName)
	info, err := os.Lstat(path)
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("registration journal mode: info=%v err=%v", info, err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var receipt registration
	if err := json.Unmarshal(data, &receipt); err != nil || receipt.State != StateRegistered || !receipt.MutationAcknowledged || receipt.Profile != ProfileAppStoreSupplied {
		t.Fatalf("durable acknowledged receipt: %+v, %v", receipt, err)
	}
	if strings.Contains(string(data), "private@example.invalid") || strings.Contains(string(data), "credential") {
		t.Fatalf("private status details leaked into journal: %s", data)
	}
	if err := manager.Publish(context.Background(), fixtureRequest(true)); err != nil {
		t.Fatalf("exact acknowledged registration was not reused: %v", err)
	}
	if fixture.mutationCalls() != 1 {
		t.Fatalf("reuse issued another Serve write: %d", fixture.mutationCalls())
	}
}

func TestVerifyRegisteredRouteReportsReadyAndDriftWithoutMutation(t *testing.T) {
	fixture := newFakeCLI(t)
	manager := newFixtureManager(t, fixture, "instance state")
	if err := manager.Publish(context.Background(), fixtureRequest(true)); err != nil {
		t.Fatal(err)
	}

	status, err := manager.VerifyRegisteredRoute(context.Background(), "development", "install-fixture", "https://herdr.tailnet.ts.net:8443", 8443, 18377)
	if err != nil || status.Readiness != ReadinessReady || status.JournalState != StateRegistered || status.RuntimeQualified {
		t.Fatalf("registered route status = %+v, %v", status, err)
	}
	serialized, err := json.Marshal(status)
	if err != nil || strings.Contains(string(serialized), "node-fixture") || strings.Contains(string(serialized), "fixture-account") {
		t.Fatalf("route status exposed private identity details: %s, %v", serialized, err)
	}

	fixture.serve = unrelatedRoute
	status, err = manager.VerifyRegisteredRoute(context.Background(), "development", "install-fixture", "https://herdr.tailnet.ts.net:8443", 8443, 18377)
	if !errors.Is(err, ErrConflict) || status.Readiness != ReadinessDegraded {
		t.Fatalf("missing route status = %+v, %v", status, err)
	}
	if fixture.mutationCalls() != 1 {
		t.Fatalf("readiness/drift verification mutated Serve: %d writes", fixture.mutationCalls())
	}
}

func TestRecoverNeverResolvesAmbiguousPublicationFromObservedRoute(t *testing.T) {
	fixture := newFakeCLI(t)
	fixture.publishErr = errors.New("publish acknowledgement lost")
	fixture.publishDispatched = true
	manager := newFixtureManager(t, fixture, "recover uncertain publication")

	if err := manager.Publish(context.Background(), fixtureRequest(true)); !errors.Is(err, ErrUncertain) {
		t.Fatalf("publish without acknowledgement = %v, want uncertain", err)
	}
	for _, observed := range []struct {
		name  string
		route string
		want  string
	}{
		{name: "absent", route: `{}`, want: "selected-listener-absent"},
		{name: "matching", route: fixtureRoute, want: "exact-registered-route-present"},
	} {
		t.Run(observed.name, func(t *testing.T) {
			fixture.serve = observed.route
			report, err := manager.Recover(context.Background(), "development", "install-fixture", "https://herdr.tailnet.ts.net:8443", 8443, 18377)
			if !errors.Is(err, ErrUncertain) || report.Route.JournalState != StatePublishUncertain ||
				report.Route.Readiness != ReadinessUncertain || !report.RequiresOperatorAction || report.Observation != observed.want {
				t.Fatalf("recovery report = %+v, %v", report, err)
			}
			record, readErr := manager.readRegistration()
			if readErr != nil || record == nil || record.State != StatePublishUncertain {
				t.Fatalf("recovery changed uncertainty journal: record=%+v err=%v", record, readErr)
			}
		})
	}
	if got := fixture.mutationCalls(); got != 1 {
		t.Fatalf("recovery replayed a Serve mutation: %d total calls", got)
	}
}

func TestPendingPublishReconciliationRequiresExactOperationAndObservation(t *testing.T) {
	for _, tc := range []struct {
		name        string
		serve       string
		observation string
		wantState   RegistrationState
	}{
		{name: "present", serve: fixtureRoute, observation: "present", wantState: StateReconciledPresent},
		{name: "absent", serve: `{}`, observation: "absent", wantState: StateReconciledAbsent},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fixture := newFakeCLI(t)
			fixture.publishErr = errors.New("publication acknowledgement lost")
			fixture.publishDispatched = true
			manager := newFixtureManager(t, fixture, "reconcile "+tc.name)
			if err := manager.Publish(context.Background(), fixtureRequest(true)); !errors.Is(err, ErrUncertain) {
				t.Fatalf("ambiguous publish = %v", err)
			}
			record, err := manager.readRegistration()
			if err != nil || record == nil || record.State != StatePublishUncertain {
				t.Fatalf("pending record = %+v, %v", record, err)
			}
			fixture.serve = tc.serve
			report, recoverErr := manager.Recover(context.Background(), "development", "install-fixture",
				"https://herdr.tailnet.ts.net:8443", 8443, 18377)
			if !errors.Is(recoverErr, ErrUncertain) || report.OperationID != record.OperationID {
				t.Fatalf("recovery report = %+v, %v", report, recoverErr)
			}
			consent := fixtureConsent(true)
			consent.OperationID = record.OperationID
			consent.RecoveryObservation = tc.observation
			consent.RecoveryAccepted = true
			wrongOperation := consent
			wrongOperation.OperationID = "00000000000000000000000000000000"
			if err := manager.Reconcile(context.Background(), "development", "install-fixture",
				"https://herdr.tailnet.ts.net:8443", 8443, 18377, wrongOperation); err == nil {
				t.Fatal("reconciliation accepted consent for a different pending operation")
			}
			if err := manager.Reconcile(context.Background(), "development", "install-fixture",
				"https://herdr.tailnet.ts.net:8443", 8443, 18377, consent); err != nil {
				t.Fatalf("exact consented journal reconciliation: %v", err)
			}
			resolved, err := manager.readRegistration()
			if err != nil || resolved == nil || resolved.State != tc.wantState || resolved.MutationAcknowledged {
				t.Fatalf("reconciled observation was promoted to mutation acknowledgement: %+v, %v", resolved, err)
			}
			if fixture.mutationCalls() != 1 {
				t.Fatalf("local journal reconciliation retried Serve mutation: %d calls", fixture.mutationCalls())
			}
		})
	}
}

func TestReopenedManagerRecoversRegisteredRouteWithoutMutation(t *testing.T) {
	base := t.TempDir()
	stateRoot := filepath.Join(base, "registration state")
	coordinationRoot := filepath.Join(base, "shared coordination")
	for _, root := range []string{stateRoot, coordinationRoot} {
		if err := os.Mkdir(root, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	fixture := newFakeCLI(t)
	first, err := NewManager(stateRoot, coordinationRoot, newFixtureClient(fixture))
	if err != nil {
		t.Fatal(err)
	}
	first.fixtureMutations = true
	first.skipBackendReadiness = true
	if err := first.Publish(context.Background(), fixtureRequest(true)); err != nil {
		t.Fatalf("register fixture route: %v", err)
	}
	callsBeforeRecovery := len(fixture.calls)

	reopened, err := NewManager(stateRoot, coordinationRoot, newFixtureClient(fixture))
	if err != nil {
		t.Fatalf("reopen registration manager: %v", err)
	}
	report, err := reopened.Recover(context.Background(), "development", "install-fixture", "https://herdr.tailnet.ts.net:8443", 8443, 18377)
	if err != nil || report.Route.JournalState != StateRegistered ||
		report.Route.Readiness != ReadinessReady || report.Route.RuntimeQualified ||
		report.RequiresOperatorAction {
		t.Fatalf("reopened manager recovery = %+v, %v", report, err)
	}
	for _, call := range fixture.calls[callsBeforeRecovery:] {
		if strings.HasPrefix(strings.Join(call, " "), "serve --bg") {
			t.Fatalf("read-only restart recovery mutated Serve route: %q", call)
		}
	}
}

func TestNodeMutationLockCrashHelper(t *testing.T) {
	if os.Getenv("HERDR_TEST_HOLD_NODE_LOCK") != "1" {
		return
	}
	manager := &Manager{coordinationRoot: os.Getenv("HERDR_TEST_COORDINATION_ROOT")}
	marker := os.Getenv("HERDR_TEST_LOCK_HELD_MARKER")
	if err := manager.withNodeLock(context.Background(), "node-crash-fixture", func() error {
		if err := os.WriteFile(marker, []byte("held"), 0o600); err != nil {
			return err
		}
		for {
			time.Sleep(time.Hour)
		}
	}); err != nil {
		t.Fatalf("hold node lock: %v", err)
	}
}

func TestNodeMutationLockIsReleasedAfterProcessCrash(t *testing.T) {
	coordinationRoot := filepath.Join(t.TempDir(), "shared coordination")
	if err := os.Mkdir(coordinationRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(t.TempDir(), "lock-held")
	command := exec.Command(os.Args[0], "-test.run=^TestNodeMutationLockCrashHelper$")
	command.Env = append(os.Environ(),
		"HERDR_TEST_HOLD_NODE_LOCK=1",
		"HERDR_TEST_COORDINATION_ROOT="+coordinationRoot,
		"HERDR_TEST_LOCK_HELD_MARKER="+marker)
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	childRunning := true
	defer func() {
		if childRunning {
			_ = command.Process.Kill()
			_ = command.Wait()
		}
	}()
	deadline := time.Now().Add(5 * time.Second)
	for {
		if _, err := os.Stat(marker); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("child did not acquire the node lock")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if err := command.Process.Kill(); err != nil {
		t.Fatalf("kill lock holder: %v", err)
	}
	_ = command.Wait()
	childRunning = false

	manager := &Manager{coordinationRoot: coordinationRoot}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := manager.withNodeLock(ctx, "node-crash-fixture", func() error { return nil }); err != nil {
		t.Fatalf("process crash stranded the node lock: %v", err)
	}
	entries, err := os.ReadDir(coordinationRoot)
	if err != nil || len(entries) != 1 {
		t.Fatalf("crash-safe lock file state: entries=%v err=%v", entries, err)
	}
	info, err := entries[0].Info()
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("persistent private lock file = %v, %v", info, err)
	}
}

func TestProductionAndDevelopmentRegistrationsCoexistOnFixtureNode(t *testing.T) {
	base := t.TempDir()
	productionRoot := filepath.Join(base, "production registration")
	developmentRoot := filepath.Join(base, "development registration")
	coordinationRoot := filepath.Join(base, "shared node coordination")
	for _, root := range []string{productionRoot, developmentRoot, coordinationRoot} {
		if err := os.Mkdir(root, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	fixture := &coexistenceCLI{t: t, backends: make(map[int]int)}
	newManager := func(stateRoot string) *Manager {
		manager, err := NewManager(stateRoot, coordinationRoot,
			newTestClientForPlatform("/fixture path/tailscale", fixture.run, "darwin", "arm64"))
		if err != nil {
			t.Fatal(err)
		}
		manager.fixtureMutations = true
		manager.skipBackendReadiness = true
		return manager
	}
	production := newManager(productionRoot)
	development := newManager(developmentRoot)
	requests := []PublishRequest{
		{
			InstallationID: "install-production-fixture", Scope: "production", ExpectedNodeID: "node-fixture",
			Origin: "https://herdr.tailnet.ts.net:9443", HTTPSPort: 9443, BackendPort: 19375,
			Consent: Consent{
				Accepted: true, Scope: "production", NodeID: "node-fixture", Origin: "https://herdr.tailnet.ts.net:9443", HTTPSPort: 9443, BackendPort: 19375,
				PersistentRouteAccepted: true, CheckToWriteRaceAccepted: true, PortReuseRiskAccepted: true,
				NoRollbackAccepted: true, NoRemoteDrainAccepted: true,
			},
		},
		{
			InstallationID: "install-development-fixture", Scope: "development", ExpectedNodeID: "node-fixture",
			Origin: "https://herdr.tailnet.ts.net:8443", HTTPSPort: 8443, BackendPort: 18377,
			Consent: Consent{
				Accepted: true, Scope: "development", NodeID: "node-fixture", Origin: "https://herdr.tailnet.ts.net:8443", HTTPSPort: 8443, BackendPort: 18377,
				PersistentRouteAccepted: true, CheckToWriteRaceAccepted: true, PortReuseRiskAccepted: true,
				NoRollbackAccepted: true, NoRemoteDrainAccepted: true,
			},
		},
	}
	errorsByIndex := make([]error, len(requests))
	managers := []*Manager{production, development}
	var writers sync.WaitGroup
	for index, request := range requests {
		writers.Add(1)
		go func(index int, request PublishRequest) {
			defer writers.Done()
			errorsByIndex[index] = managers[index].Publish(context.Background(), request)
		}(index, request)
	}
	writers.Wait()
	for index, err := range errorsByIndex {
		if err != nil {
			t.Fatalf("publish %s fixture profile: %v", requests[index].Scope, err)
		}
	}

	fixture.mu.Lock()
	observed := make(map[int]int, len(fixture.backends))
	for port, backend := range fixture.backends {
		observed[port] = backend
	}
	callsBeforeRecovery := len(fixture.calls)
	fixture.mu.Unlock()
	if len(observed) != 2 || observed[9443] != 19375 || observed[8443] != 18377 {
		t.Fatalf("production/development fixture routes overwrote one another: %+v", observed)
	}
	for index, request := range requests {
		report, err := managers[index].Recover(context.Background(), request.Scope, request.InstallationID, request.Origin,
			request.HTTPSPort, request.BackendPort)
		if err != nil || report.Route.JournalState != StateRegistered || report.Route.Readiness != ReadinessReady ||
			report.Route.RuntimeQualified || report.RequiresOperatorAction {
			t.Fatalf("%s fixture recovery = %+v, %v", request.Scope, report, err)
		}
		journal, err := managers[index].readRegistration()
		if err != nil || journal.Scope != request.Scope || journal.InstallationID != request.InstallationID ||
			journal.HTTPSPort != request.HTTPSPort || journal.BackendPort != request.BackendPort {
			t.Fatalf("%s journal crossed profile boundary: %+v, %v", request.Scope, journal, err)
		}
	}
	fixture.mu.Lock()
	defer fixture.mu.Unlock()
	for _, call := range fixture.calls[callsBeforeRecovery:] {
		if len(call) > 0 && call[0] == "serve" && len(call) > 1 && call[1] == "--bg" {
			t.Fatalf("read-only coexistence recovery mutated Serve: %q", strings.Join(call, " "))
		}
	}
	if len(fixture.backends) != 2 || fixture.backends[9443] != 19375 || fixture.backends[8443] != 18377 {
		t.Fatalf("read-only recovery changed coexisting fixture routes: %+v", fixture.backends)
	}
}

func TestRecoverRefusesUnregisteredObservedRoute(t *testing.T) {
	fixture := newFakeCLI(t)
	fixture.serve = fixtureRoute
	manager := newFixtureManager(t, fixture, "recover unregistered route")

	report, err := manager.Recover(context.Background(), "development", "install-fixture", "https://herdr.tailnet.ts.net:8443", 8443, 18377)
	if !errors.Is(err, ErrConflict) || report.Route.JournalState != StateUnconfigured ||
		report.Route.Readiness != ReadinessConflicted || !report.RequiresOperatorAction ||
		report.Observation != "selected-listener-present-without-registration" {
		t.Fatalf("unregistered route recovery = %+v, %v", report, err)
	}
	if got := fixture.mutationCalls(); got != 0 {
		t.Fatalf("unregistered route recovery mutated Serve: %d calls", got)
	}
}

func TestVerifyRegisteredRouteRefusesUnrecordedMatchingRoute(t *testing.T) {
	fixture := newFakeCLI(t)
	fixture.serve = fixtureRoute
	manager := newFixtureManager(t, fixture, "instance state")

	status, err := manager.VerifyRegisteredRoute(context.Background(), "development", "install-fixture", "https://herdr.tailnet.ts.net:8443", 8443, 18377)
	if !errors.Is(err, ErrConflict) || status.Readiness != ReadinessConflicted || status.JournalState != StateUnconfigured {
		t.Fatalf("unrecorded route status = %+v, %v", status, err)
	}
	if fixture.mutationCalls() != 0 {
		t.Fatalf("unrecorded route verification mutated Serve: %d writes", fixture.mutationCalls())
	}
}

func TestPublishModelsNonCooperatingExternalWriterRace(t *testing.T) {
	fixture := newFakeCLI(t)
	fixture.raceRoute = `{"TCP":{"8443":{"HTTPS":true}},"Web":{"herdr.tailnet.ts.net:8443":{"Handlers":{"/":{"Proxy":"http://127.0.0.1:9999"}}}}}`
	manager := newFixtureManager(t, fixture, "instance state")

	if err := manager.Publish(context.Background(), fixtureRequest(true)); err != nil {
		t.Fatal(err)
	}
	if fixture.serve != fixtureRoute {
		t.Fatalf("race fixture did not demonstrate selected-listener replacement: %s", fixture.serve)
	}
	if fixture.mutationCalls() != 1 {
		t.Fatalf("external-writer race retried the mutation: %d", fixture.mutationCalls())
	}
}

func TestProductionManagerRefusesAllUnqualifiedProfilesBeforeMutation(t *testing.T) {
	fixture := newFakeCLI(t)
	manager := newFixtureManager(t, fixture, "instance state")
	manager.fixtureMutations = false
	if err := manager.Publish(context.Background(), fixtureRequest(true)); !errors.Is(err, ErrUnsupported) {
		t.Fatalf("unqualified profile activation = %v", err)
	}
	if fixture.mutationCalls() != 0 {
		t.Fatalf("unqualified profile reached a Serve mutator: %d", fixture.mutationCalls())
	}
	if _, err := os.Lstat(filepath.Join(manager.stateRoot, journalName)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("unqualified profile wrote a journal: %v", err)
	}
}

func TestBackendReadinessDoesNotFollowRedirect(t *testing.T) {
	var redirectedRequests atomic.Int32
	target := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		redirectedRequests.Add(1)
	}))
	defer target.Close()
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	backend := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Location", target.URL+"/redirect-target")
		w.WriteHeader(http.StatusTemporaryRedirect)
	}))
	backend.Listener = listener
	backend.Start()
	defer backend.Close()
	port, err := strconv.Atoi(strings.TrimPrefix(listener.Addr().String(), "127.0.0.1:"))
	if err != nil {
		t.Fatal(err)
	}
	if err := verifyBackendReadiness(context.Background(), port, "install-fixture", "https://herdr.tailnet.ts.net:8443"); !errors.Is(err, ErrConflict) {
		t.Fatalf("readiness accepted a redirect response: %v", err)
	}
	if got := redirectedRequests.Load(); got != 0 {
		t.Fatalf("loopback readiness followed a redirect to another HTTP service: %d requests", got)
	}
}

func TestPublishRejectsWrongBackendIdentityBeforeJournalOrServeWrite(t *testing.T) {
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"status":"ok","readiness":"ready","transport":"tailscale-cli","instance":"other-install","tailscale_cli_origin":"https://herdr.tailnet.ts.net:8443"}`))
	}))
	server.Listener = listener
	server.Start()
	defer server.Close()
	backendPort, err := strconv.Atoi(strings.TrimPrefix(listener.Addr().String(), "127.0.0.1:"))
	if err != nil {
		t.Fatal(err)
	}

	fixture := newFakeCLI(t)
	manager := newFixtureManager(t, fixture, "backend identity")
	manager.skipBackendReadiness = false
	request := fixtureRequest(true)
	request.BackendPort = backendPort
	request.Consent.BackendPort = backendPort
	if err := manager.Publish(context.Background(), request); !errors.Is(err, ErrConflict) {
		t.Fatalf("publish accepted a backend with another installation identity: %v", err)
	}
	if fixture.mutationCalls() != 0 {
		t.Fatalf("backend mismatch dispatched a Serve mutation: %d", fixture.mutationCalls())
	}
	if _, err := os.Lstat(filepath.Join(manager.stateRoot, journalName)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("backend mismatch persisted a publish intent: %v", err)
	}
}

func TestPublishRequiresAllRiskConsentAndRefusesListenerConflict(t *testing.T) {
	fixture := newFakeCLI(t)
	manager := newFixtureManager(t, fixture, "instance state")
	if err := manager.Publish(context.Background(), fixtureRequest(false)); err == nil {
		t.Fatal("missing consent was accepted")
	}
	if fixture.mutationCalls() != 0 {
		t.Fatal("missing consent dispatched a route mutation")
	}
	if _, err := os.Lstat(filepath.Join(manager.stateRoot, journalName)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("missing consent persisted a mutation intent: %v", err)
	}

	misbound := fixtureRequest(true)
	misbound.Consent.Origin = "https://other.tailnet.ts.net:8443"
	if err := manager.Publish(context.Background(), misbound); err == nil {
		t.Fatal("consent bound to a different HTTPS origin was accepted")
	}
	if fixture.mutationCalls() != 0 {
		t.Fatal("misbound HTTPS-origin consent dispatched a route mutation")
	}

	fixture.serve = `{"TCP":{"8443":{"HTTPS":true}},"Web":{"other.tailnet.ts.net:8443":{"Handlers":{"/":{"Proxy":"http://127.0.0.1:8080"}}}}}`
	if err := manager.Publish(context.Background(), fixtureRequest(true)); !errors.Is(err, ErrConflict) {
		t.Fatalf("selected listener conflict = %v", err)
	}
	if fixture.mutationCalls() != 0 {
		t.Fatal("listener conflict dispatched a route mutation")
	}
}

func TestPublishDispatchUncertaintyIsDurableAndNeverReplayed(t *testing.T) {
	fixture := newFakeCLI(t)
	fixture.publishErr = errors.New("fake timeout with private output")
	fixture.publishDispatched = true
	manager := newFixtureManager(t, fixture, "instance state")
	publishErr := manager.Publish(context.Background(), fixtureRequest(true))
	if !errors.Is(publishErr, ErrUncertain) {
		t.Fatalf("ambiguous publish result = %v", publishErr)
	}
	record, err := manager.readRegistration()
	if err != nil || record == nil || record.State != StatePublishUncertain {
		t.Fatalf("uncertain publish journal: %+v, %v", record, err)
	}
	if strings.Contains(errString(publishErr), "private output") {
		t.Fatal("raw CLI output escaped the adapter")
	}
	fixture.publishErr = nil
	if err := manager.Publish(context.Background(), fixtureRequest(true)); !errors.Is(err, ErrUncertain) {
		t.Fatalf("uncertain registration was replayed instead of blocked: %v", err)
	}
	if fixture.mutationCalls() != 1 {
		t.Fatalf("ambiguous publish was replayed %d times", fixture.mutationCalls())
	}
}

func TestPreDispatchFailureKeepsReservationUntilSafeRelease(t *testing.T) {
	fixture := newFakeCLI(t)
	fixture.publishErr = ErrProfileUnavailable
	fixture.publishDispatched = false
	manager := newFixtureManager(t, fixture, "instance state")
	request := fixtureRequest(true)
	if err := manager.Publish(context.Background(), request); !errors.Is(err, ErrProfileUnavailable) ||
		!errors.Is(err, ErrPublishNotDispatched) {
		t.Fatalf("pre-dispatch failure classification = %v", err)
	}
	record, err := manager.readRegistration()
	if err != nil || record == nil || record.State != StateUnconfigured || record.MutationAcknowledged {
		t.Fatalf("pre-dispatch state = %+v, %v", record, err)
	}
	reservation, err := manager.readBackendReservation(request.BackendPort)
	if err != nil || reservation == nil {
		t.Fatalf("pre-dispatch failure lost the reservation before listener shutdown: %+v, %v", reservation, err)
	}
	if fixture.mutationCalls() != 1 {
		t.Fatalf("expected one attempted, non-dispatched invocation, got %d", fixture.mutationCalls())
	}
	fixture.serve = `{}` // The fixture listener is stopped before verified cleanup.
	if err := manager.ReleaseBackendPort(context.Background(), request.InstallationID, request.Scope,
		request.ExpectedNodeID, request.Origin, request.HTTPSPort, request.BackendPort, request.ReservationID, true); err != nil {
		t.Fatalf("release safe no-route reservation after stop: %v", err)
	}
	if reservation, err = manager.readBackendReservation(request.BackendPort); err != nil || reservation != nil {
		t.Fatalf("reservation remained after verified release: %+v, %v", reservation, err)
	}
}

func TestPublishAckReadbackFailureNeverRetriesOrRollsBack(t *testing.T) {
	fixture := newFakeCLI(t)
	fixture.publishNoEffect = true
	manager := newFixtureManager(t, fixture, "instance state")
	if err := manager.Publish(context.Background(), fixtureRequest(true)); !errors.Is(err, ErrConflict) {
		t.Fatalf("missing route readback = %v", err)
	}
	record, err := manager.readRegistration()
	if err != nil || record == nil || record.State != StateRegistered || !record.MutationAcknowledged {
		t.Fatalf("acknowledged receipt lost after readback error: %+v, %v", record, err)
	}
	if err := manager.Publish(context.Background(), fixtureRequest(true)); !errors.Is(err, ErrConflict) {
		t.Fatalf("missing route triggered unsafe auto-repair: %v", err)
	}
	if fixture.mutationCalls() != 1 {
		t.Fatalf("readback failure caused retry/rollback: %d", fixture.mutationCalls())
	}
}

func TestUnpublishRequiresAcknowledgedExactRouteAndPersistsRemoval(t *testing.T) {
	fixture := newFakeCLI(t)
	manager := newFixtureManager(t, fixture, "instance state")
	if err := manager.Publish(context.Background(), fixtureRequest(true)); err != nil {
		t.Fatal(err)
	}
	missingRemovalConsent := fixtureConsent(true)
	missingRemovalConsent.RouteRemovalAccepted = false
	if err := manager.Unpublish(context.Background(), missingRemovalConsent); err == nil {
		t.Fatal("unpublish without explicit route-removal consent was accepted")
	}
	if fixture.mutationCalls() != 1 {
		t.Fatal("missing route-removal consent dispatched an unpublish")
	}
	missingRaceConsent := fixtureConsent(true)
	missingRaceConsent.CheckToWriteRaceAccepted = false
	if err := manager.Unpublish(context.Background(), missingRaceConsent); err == nil {
		t.Fatal("unpublish without explicit check-to-write-race consent was accepted")
	}
	if fixture.mutationCalls() != 1 {
		t.Fatal("missing race consent dispatched an unpublish")
	}
	if err := manager.Unpublish(context.Background(), fixtureConsent(true)); err != nil {
		t.Fatal(err)
	}
	record, err := manager.readRegistration()
	if err != nil || record == nil || record.State != StateRemoved || !record.MutationAcknowledged {
		t.Fatalf("verified removal journal: %+v, %v", record, err)
	}
	if fixture.mutationCalls() != 2 {
		t.Fatalf("publish/unpublish command count = %d", fixture.mutationCalls())
	}
	if fixture.serve != `{}` {
		t.Fatalf("unpublish did not remove only selected fixture route: %s", fixture.serve)
	}
	reservation, err := manager.readBackendReservation(18377)
	if err != nil || reservation != nil {
		t.Fatalf("backend reservation survived acknowledged route removal: %+v, %v", reservation, err)
	}
	last := fixture.calls[len(fixture.calls)-4]
	if strings.Join(last, " ") != "serve --bg --https=8443 --set-path=/ off" {
		t.Fatalf("unexpected unpublish argv: %v", last)
	}
}

func TestUnpublishUncertaintyAndDriftNeverRetryOrRemoveForeignRoute(t *testing.T) {
	fixture := newFakeCLI(t)
	manager := newFixtureManager(t, fixture, "instance state")
	if err := manager.Publish(context.Background(), fixtureRequest(true)); err != nil {
		t.Fatal(err)
	}
	fixture.removeErr = errors.New("dropped acknowledgement")
	fixture.removeDispatched = true
	if err := manager.Unpublish(context.Background(), fixtureConsent(true)); !errors.Is(err, ErrUncertain) {
		t.Fatalf("ambiguous unpublish result = %v", err)
	}
	record, err := manager.readRegistration()
	if err != nil || record == nil || record.State != StateRemoveUncertain {
		t.Fatalf("uncertain removal journal: %+v, %v", record, err)
	}
	if err := manager.Unpublish(context.Background(), fixtureConsent(true)); !errors.Is(err, ErrUncertain) {
		t.Fatalf("uncertain removal was retried: %v", err)
	}
	if fixture.mutationCalls() != 2 {
		t.Fatalf("uncertain removal invoked another mutator: %d", fixture.mutationCalls())
	}
}

func TestUnpublishNoWriteWhenRegisteredRouteWasReplaced(t *testing.T) {
	fixture := newFakeCLI(t)
	manager := newFixtureManager(t, fixture, "instance state")
	if err := manager.Publish(context.Background(), fixtureRequest(true)); err != nil {
		t.Fatal(err)
	}
	fixture.serve = `{"TCP":{"8443":{"HTTPS":true}},"Web":{"herdr.tailnet.ts.net:8443":{"Handlers":{"/":{"Proxy":"http://127.0.0.1:9999"}}}}}`
	if err := manager.Unpublish(context.Background(), fixtureConsent(true)); !errors.Is(err, ErrConflict) {
		t.Fatalf("replaced route removal = %v", err)
	}
	if fixture.mutationCalls() != 1 {
		t.Fatal("replaced backend was removed")
	}
}

func TestTransportSwitchRequiresExactRouteDisposition(t *testing.T) {
	fixture := newFakeCLI(t)
	manager := newFixtureManager(t, fixture, "transport switch journal")
	request := fixtureRequest(true)
	if err := manager.Publish(context.Background(), request); err != nil {
		t.Fatal(err)
	}
	if err := CheckTransportSwitch(manager.stateRoot, manager.coordinationRoot, request.InstallationID, request.BackendPort); !errors.Is(err, ErrUncertain) {
		t.Fatalf("transport switch accepted an acknowledged persistent route: %v", err)
	}
	if err := manager.Unpublish(context.Background(), fixtureConsent(true)); err != nil {
		t.Fatalf("explicit exact route removal: %v", err)
	}
	if err := CheckTransportSwitch(manager.stateRoot, manager.coordinationRoot, request.InstallationID, request.BackendPort); err != nil {
		t.Fatalf("transport switch remained blocked after exact route removal: %v", err)
	}
}

func TestPendingBackendReservationIsReportedAndReleasedOnlyByStoppedAttempt(t *testing.T) {
	fixture := newFakeCLI(t)
	manager := newFixtureManager(t, fixture, "stale backend reservation")
	request := fixtureRequest(true)
	if err := manager.ReserveBackendPort(context.Background(), request.InstallationID, request.Scope,
		request.ExpectedNodeID, request.Origin, request.HTTPSPort, request.BackendPort, request.ReservationID); err != nil {
		t.Fatal(err)
	}
	if err := CheckTransportSwitch(manager.stateRoot, manager.coordinationRoot, request.InstallationID, request.BackendPort); !errors.Is(err, ErrUncertain) {
		t.Fatalf("transport switch ignored a pending backend claim: %v", err)
	}
	report, recoverErr := manager.Recover(context.Background(), request.Scope, request.InstallationID,
		request.Origin, request.HTTPSPort, request.BackendPort)
	if !errors.Is(recoverErr, ErrUncertain) || report.ReservationAttemptID != request.ReservationID ||
		report.ReservationState != StatePublishPending || !report.ReservationReleasable ||
		report.Observation != "pending-backend-reservation-without-registration" {
		t.Fatalf("pending reservation was not reported for recovery: report=%+v err=%v", report, recoverErr)
	}
	if err := manager.ReleaseBackendPort(context.Background(), request.InstallationID, request.Scope,
		request.ExpectedNodeID, request.Origin, request.HTTPSPort, request.BackendPort, request.ReservationID, false); !errors.Is(err, ErrConflict) {
		t.Fatalf("reservation release accepted without a stopped-service confirmation: %v", err)
	}
	wrongID := "00000000000000000000000000000002"
	if err := manager.ReleaseBackendPort(context.Background(), request.InstallationID, request.Scope,
		request.ExpectedNodeID, request.Origin, request.HTTPSPort, request.BackendPort, wrongID, true); !errors.Is(err, ErrConflict) {
		t.Fatalf("different setup attempt released the reservation: %v", err)
	}
	if err := manager.ReleaseBackendPort(context.Background(), request.InstallationID, request.Scope,
		request.ExpectedNodeID, request.Origin, request.HTTPSPort, request.BackendPort, request.ReservationID, true); err != nil {
		t.Fatalf("exact stopped setup attempt could not release its claim: %v", err)
	}
	if err := CheckTransportSwitch(manager.stateRoot, manager.coordinationRoot, request.InstallationID, request.BackendPort); err != nil {
		t.Fatalf("transport switch remained blocked after exact reservation release: %v", err)
	}
}

func TestConcurrentSetupCannotReclaimPendingBackendReservation(t *testing.T) {
	fixture := newFakeCLI(t)
	managerA := newFixtureManager(t, fixture, "concurrent setup A")
	stateRootB := filepath.Join(t.TempDir(), "concurrent setup B")
	if err := os.Mkdir(stateRootB, 0o700); err != nil {
		t.Fatal(err)
	}
	managerB, err := NewManager(stateRootB, managerA.coordinationRoot, newFixtureClient(fixture))
	if err != nil {
		t.Fatal(err)
	}
	managerB.fixtureMutations = true
	managerB.skipBackendReadiness = true

	request := fixtureRequest(true)
	start := make(chan struct{})
	results := make(chan error, 2)
	for _, manager := range []*Manager{managerA, managerB} {
		go func(manager *Manager) {
			<-start
			results <- manager.ReserveBackendPort(context.Background(), request.InstallationID, request.Scope,
				request.ExpectedNodeID, request.Origin, request.HTTPSPort, request.BackendPort, request.ReservationID)
		}(manager)
	}
	close(start)
	first, second := <-results, <-results
	if (first == nil) == (second == nil) ||
		(first != nil && !errors.Is(first, ErrConflict)) || (second != nil && !errors.Is(second, ErrConflict)) {
		t.Fatalf("concurrent reservations must produce exactly one owner and one conflict, got %v and %v", first, second)
	}
	reservation, err := managerA.readBackendReservation(request.BackendPort)
	if err != nil || reservation == nil || reservation.State != StatePublishPending ||
		reservation.InstallationID != request.InstallationID || reservation.NodeID != request.ExpectedNodeID {
		t.Fatalf("winning setup reservation was lost or changed: %+v, %v", reservation, err)
	}
	if err := managerA.Publish(context.Background(), request); err != nil {
		t.Fatalf("winning setup could not publish with its retained reservation: %v", err)
	}
}

func TestBackendReservationRejectsServeRouteAlreadyUsingLocalPort(t *testing.T) {
	fixture := newFakeCLI(t)
	fixture.serve = unrelatedRoute
	manager := newFixtureManager(t, fixture, "backend route reservation")
	request := fixtureRequest(true)
	request.BackendPort = 8080
	request.Consent.BackendPort = 8080
	if err := manager.ReserveBackendPort(context.Background(), request.InstallationID, request.Scope,
		request.ExpectedNodeID, request.Origin, request.HTTPSPort, request.BackendPort, request.ReservationID); !errors.Is(err, ErrConflict) {
		t.Fatalf("backend port already referenced by another Serve route was reserved: %v", err)
	}
}

func TestBackendPortConflictRecognizesEquivalentLoopbackURLs(t *testing.T) {
	for _, test := range []struct {
		backend string
		want    bool
	}{
		{backend: "http://127.0.0.1:18377", want: true},
		{backend: "http://127.0.0.2:18377/", want: true},
		{backend: "http://localhost:18377/", want: true},
		{backend: "https://[::1]:18377/ready", want: true},
		{backend: "http://127.0.0.1", want: false},
		{backend: "http://relay.example.test:18377/", want: false},
	} {
		t.Run(test.backend, func(t *testing.T) {
			serve := tailscale.ServeStatus{Complete: true, ObservedRoutes: []tailscale.Route{{
				Port: 443, Listener: "HTTPS", Handler: "Proxy", Backend: test.backend,
			}}}
			if got := backendPortHasRoute(serve, 18377, nil); got != test.want {
				t.Fatalf("backend conflict for %q = %t, want %t", test.backend, got, test.want)
			}
		})
	}
}

func TestPersistentBackendReservationPreventsCrossInstallationReuse(t *testing.T) {
	base := t.TempDir()
	coordinationRoot := filepath.Join(base, "shared coordination")
	stateOne := filepath.Join(base, "production registration")
	stateTwo := filepath.Join(base, "development registration")
	for _, root := range []string{coordinationRoot, stateOne, stateTwo} {
		if err := os.Mkdir(root, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	fixture := newFakeCLI(t)
	production, err := NewManager(stateOne, coordinationRoot, newFixtureClient(fixture))
	if err != nil {
		t.Fatal(err)
	}
	production.fixtureMutations = true
	production.skipBackendReadiness = true
	productionRequest := fixtureRequest(true)
	productionRequest.Scope = "production"
	productionRequest.Consent.Scope = "production"
	if err := production.ReserveBackendPort(context.Background(), productionRequest.InstallationID,
		productionRequest.Scope, productionRequest.ExpectedNodeID, productionRequest.Origin,
		productionRequest.HTTPSPort, productionRequest.BackendPort, productionRequest.ReservationID); err != nil {
		t.Fatalf("reserve production backend before bind: %v", err)
	}
	development, err := NewManager(stateTwo, coordinationRoot, newFixtureClient(fixture))
	if err != nil {
		t.Fatal(err)
	}
	development.fixtureMutations = true
	development.skipBackendReadiness = true
	conflictingListener := fixtureRequest(true)
	conflictingListener.InstallationID = "install-other-listener"
	conflictingListener.BackendPort = 19377
	conflictingListener.Consent.BackendPort = 19377
	if err := development.ReserveBackendPort(context.Background(), conflictingListener.InstallationID,
		conflictingListener.Scope, conflictingListener.ExpectedNodeID, conflictingListener.Origin,
		conflictingListener.HTTPSPort, conflictingListener.BackendPort, conflictingListener.ReservationID); !errors.Is(err, ErrConflict) {
		t.Fatalf("second installation reserved an already-claimed HTTPS listener: %v", err)
	}
	if err := production.Publish(context.Background(), productionRequest); err != nil {
		t.Fatal(err)
	}
	request := fixtureRequest(true)
	request.InstallationID = "install-other-fixture"
	request.Scope = "development"
	request.Origin = "https://herdr.tailnet.ts.net:9443"
	request.HTTPSPort = 9443
	request.Consent = fixtureConsent(true)
	request.Consent.Scope = request.Scope
	request.Consent.Origin = request.Origin
	request.Consent.HTTPSPort = request.HTTPSPort
	if err := development.Publish(context.Background(), request); !errors.Is(err, ErrConflict) {
		t.Fatalf("second Herdr registration reused the reserved backend port: %v", err)
	}
	if fixture.mutationCalls() != 1 {
		t.Fatalf("conflicting backend reservation dispatched a route mutation: %d", fixture.mutationCalls())
	}
}

func TestSharedNodeLockSerializesDevelopmentAndProductionMutations(t *testing.T) {
	fixture := newFakeCLI(t)
	base := t.TempDir()
	coordinationRoot := filepath.Join(base, "coord")
	if err := os.Mkdir(coordinationRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	stateRoots := []string{filepath.Join(base, "dev"), filepath.Join(base, "production")}
	managers := make([]*Manager, 2)
	for i, stateRoot := range stateRoots {
		if err := os.Mkdir(stateRoot, 0o700); err != nil {
			t.Fatal(err)
		}
		manager, err := NewManager(stateRoot, coordinationRoot, newFixtureClient(fixture))
		if err != nil {
			t.Fatal(err)
		}
		manager.fixtureMutations = true
		manager.skipBackendReadiness = true
		managers[i] = manager
	}
	requests := []PublishRequest{fixtureRequest(true), fixtureRequest(true)}
	requests[1].Scope = "production"
	requests[1].Consent.Scope = "production"
	results := make(chan error, len(managers))
	var wait sync.WaitGroup
	for i := range managers {
		wait.Add(1)
		go func(i int) {
			defer wait.Done()
			results <- managers[i].Publish(context.Background(), requests[i])
		}(i)
	}
	wait.Wait()
	close(results)
	successes, conflicts := 0, 0
	for err := range results {
		if err == nil {
			successes++
		} else if errors.Is(err, ErrConflict) {
			conflicts++
		} else {
			t.Fatalf("unexpected concurrent result: %v", err)
		}
	}
	if successes != 1 || conflicts != 1 || fixture.mutationCalls() != 1 {
		t.Fatalf("cooperative serialization results: success=%d conflict=%d writes=%d", successes, conflicts, fixture.mutationCalls())
	}
}

func TestDefaultManagerRefusesNonPrivateOrOverlappingRoots(t *testing.T) {
	base := t.TempDir()
	state := filepath.Join(base, "state")
	coord := filepath.Join(base, "coord")
	if err := os.Mkdir(state, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(coord, 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := NewManager(state, coord, newFixtureClient(newFakeCLI(t))); !errors.Is(err, ErrPermissionDenied) {
		t.Fatalf("nonprivate state root accepted: %v", err)
	}
	if err := os.Chmod(state, 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := NewManager(state, state, newFixtureClient(newFakeCLI(t))); !errors.Is(err, ErrPermissionDenied) {
		t.Fatalf("overlapping roots accepted: %v", err)
	}

	linkBase := t.TempDir()
	stateTarget := filepath.Join(linkBase, "state-target")
	coordTarget := filepath.Join(linkBase, "coord-target")
	for _, root := range []string{stateTarget, coordTarget} {
		if err := os.Mkdir(root, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	stateLink := filepath.Join(linkBase, "state-link")
	coordLink := filepath.Join(linkBase, "coord-link")
	if err := os.Symlink(stateTarget, stateLink); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(coordTarget, coordLink); err != nil {
		t.Fatal(err)
	}
	for _, statePath := range []string{stateLink, stateLink + string(os.PathSeparator)} {
		if _, err := NewManager(statePath, coordTarget, newFixtureClient(newFakeCLI(t))); !errors.Is(err, ErrPermissionDenied) {
			t.Errorf("symlink registration root %q accepted: %v", statePath, err)
		}
	}
	for _, coordinationPath := range []string{coordLink, coordLink + string(os.PathSeparator)} {
		if _, err := NewManager(stateTarget, coordinationPath, newFixtureClient(newFakeCLI(t))); !errors.Is(err, ErrPermissionDenied) {
			t.Errorf("symlink coordination root %q accepted: %v", coordinationPath, err)
		}
	}
}

func TestInspectionDoesNotClaimRuntimeProfileOnAnyHost(t *testing.T) {
	fixture := newFakeCLI(t)
	inspection, err := newFixtureClient(fixture).Inspect(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if inspection.RuntimeQualified {
		t.Fatalf("fixture profile falsely qualified on %s/%s", runtime.GOOS, runtime.GOARCH)
	}
}

func errString(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}
