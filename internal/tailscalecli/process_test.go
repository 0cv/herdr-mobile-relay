//go:build darwin || linux

package tailscalecli

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestRunCommandSelectsCLIInsteadOfGUIForMarkedAppStoreFixture(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("the production selector is added only on Darwin")
	}
	t.Setenv("TAILSCALE_BE_CLI", "0")
	binary := filepath.Join(t.TempDir(), "fake-app-store-tailscale")
	script := `#!/bin/sh
# HERDR_SYNTHETIC_TAILSCALE_CLI_FIXTURE_V1
if [ "${TAILSCALE_BE_CLI:-}" != "1" ]; then
    printf '%s\n' "The Tailscale GUI failed to start: The operation couldn't be completed. (Tailscale.CLIError error 3.)"
    exit 0
fi
printf '%s\n' 'CLI mode selected'
`
	if err := os.WriteFile(binary, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	result, err := runCommand(context.Background(), binary, "status", "--json")
	if err != nil || !result.dispatched || string(result.stdout) != "CLI mode selected\n" {
		t.Fatalf("real subprocess path did not select CLI mode: result=%+v err=%v", result, err)
	}
}

func TestRunCommandBoundsOutput(t *testing.T) {
	result, err := runCommand(context.Background(), "/bin/sh", "-c", "printf '%1200000s' x")
	if !errors.Is(err, ErrOutputTooLong) {
		t.Fatalf("oversized output error = %v, want ErrOutputTooLong", err)
	}
	if len(result.stdout) > MaxOutputBytes {
		t.Fatalf("retained %d output bytes, safety bound is %d", len(result.stdout), MaxOutputBytes)
	}
}

func TestRunCommandNonzeroExitIsTypedRedactedAndNonRetryable(t *testing.T) {
	for _, exitCode := range []int{1, 75, 97} {
		t.Run(strconv.Itoa(exitCode), func(t *testing.T) {
			binary := filepath.Join(t.TempDir(), "fake-tailscale")
			script := "#!/bin/sh\n# " + syntheticFixtureCLIMarker + "\nprintf '%s\\n' 'private@example.invalid secret-token' >&2\nprintf '%s\\n' 'private stdout token'\nexit " + strconv.Itoa(exitCode) + "\n"
			if err := os.WriteFile(binary, []byte(script), 0o700); err != nil {
				t.Fatal(err)
			}
			result, commandErr := runCommand(context.Background(), binary, "status", "--json")
			if !errors.Is(commandErr, ErrCommandFailed) || !result.dispatched || len(result.stdout) != 0 {
				t.Fatalf("nonzero subprocess retained output or lost dispatch evidence: result=%+v err=%v", result, commandErr)
			}
			client, err := newClient(binary)
			if err != nil {
				t.Fatal(err)
			}
			_, preflightErr := client.Preflight(context.Background(), 8443)
			if preflightErr == nil || !errors.Is(preflightErr, ErrCommandFailed) ||
				errors.Is(preflightErr, ErrTransientUnavailable) ||
				strings.Contains(preflightErr.Error(), "private@example.invalid") || strings.Contains(preflightErr.Error(), "secret-token") {
				t.Fatalf("nonzero child exit was not redacted/non-retryable: %v", preflightErr)
			}
			var failure CommandFailureError
			if !errors.As(preflightErr, &failure) {
				t.Fatalf("nonzero child exit was not represented by CommandFailureError: %T %v", preflightErr, preflightErr)
			}
		})
	}
}

func TestRunCommandCancellationRetainsDispatchedUncertainty(t *testing.T) {
	startedFile := filepath.Join(t.TempDir(), "started")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	type outcome struct {
		result commandResult
		err    error
	}
	completed := make(chan outcome, 1)
	go func() {
		result, err := runCommand(ctx, "/bin/sh", "-c", `printf started > "$1"; exec /bin/sleep 30`, "cli-fixture", startedFile)
		completed <- outcome{result: result, err: err}
	}()
	deadline := time.Now().Add(2 * time.Second)
	for {
		if _, err := os.Stat(startedFile); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("CLI subprocess did not start before cancellation")
		}
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
	select {
	case got := <-completed:
		if !got.result.dispatched || !errors.Is(got.err, context.Canceled) || errors.Is(got.err, ErrCommandFailed) {
			t.Fatalf("canceled dispatched command outcome = %+v, %v", got.result, got.err)
		}
	case <-time.After(ChildWaitDelay + 2*time.Second):
		t.Fatal("canceled CLI process did not clean up within its bound")
	}
}

func TestRealCLICommandFailureDoesNotReplayAmbiguousPublish(t *testing.T) {
	root := t.TempDir()
	stateRoot := filepath.Join(root, "registration")
	coordinationRoot := filepath.Join(root, "coordination")
	for _, path := range []string{stateRoot, coordinationRoot} {
		if err := os.Mkdir(path, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	binary := filepath.Join(root, "fake-tailscale")
	script := `#!/bin/sh
# HERDR_SYNTHETIC_TAILSCALE_CLI_FIXTURE_V1
STATE="${0%/*}/serve.json"
EVENTS="${0}.events"
case "$*" in
  'status --json') printf '%s\n' '` + fixtureStatus + `' ;;
  'version --json --daemon') printf '%s\n' '` + fixtureVersion + `' ;;
  'serve status --json') if [ -f "$STATE" ]; then cat "$STATE"; else printf '{}\n'; fi ;;
  'serve --bg --https=8443 --set-path=/ http://127.0.0.1:18377')
    printf '%s\n' 'private@example.invalid secret-token' >&2
    printf '%s\n' '` + fixtureRoute + `' > "$STATE"
    printf 'publish\n' >> "$EVENTS"; exit 17 ;;
  *) exit 2 ;;
esac
`
	if err := os.WriteFile(binary, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	client, err := newClient(binary)
	if err != nil {
		t.Fatal(err)
	}
	// Use the source-described App Store fixture profile on either native
	// hosted runner; this is synthetic identity data, not a runtime claim.
	client.profileOS, client.profileArch = "darwin", "arm64"
	manager, err := NewManager(stateRoot, coordinationRoot, client)
	if err != nil {
		t.Fatal(err)
	}
	manager.fixtureMutations = true
	manager.skipBackendReadiness = true

	firstErr := manager.Publish(context.Background(), fixtureRequest(true))
	if !errors.Is(firstErr, ErrUncertain) || strings.Contains(firstErr.Error(), "secret-token") {
		t.Fatalf("dispatched nonzero publish did not retain redacted uncertainty: %v", firstErr)
	}
	record, err := manager.readRegistration()
	if err != nil || record == nil || record.State != StatePublishUncertain || record.MutationAcknowledged {
		t.Fatalf("ambiguous command failure lost journal evidence: record=%+v err=%v", record, err)
	}
	if _, err := os.Stat(filepath.Join(root, "serve.json")); err != nil {
		t.Fatalf("fake CLI did not model a mutation before its nonzero exit: %v", err)
	}
	if err := manager.Publish(context.Background(), fixtureRequest(true)); !errors.Is(err, ErrUncertain) {
		t.Fatalf("ambiguous publish was automatically replayed: %v", err)
	}
	events, err := os.ReadFile(binary + ".events")
	if err != nil || strings.Count(string(events), "publish\n") != 1 {
		t.Fatalf("CLI publish dispatch count changed across recovery: %q err=%v", events, err)
	}
}

func TestRealCLICommandFailureRetainsAmbiguousUnpublishEvidence(t *testing.T) {
	backendPort := 0
	for backendPort == 0 || backendPort == 8443 {
		listener, err := net.Listen("tcp4", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		backendPort = listener.Addr().(*net.TCPAddr).Port
		_ = listener.Close()
	}
	root := t.TempDir()
	stateRoot := filepath.Join(root, "registration")
	coordinationRoot := filepath.Join(root, "coordination")
	for _, path := range []string{stateRoot, coordinationRoot} {
		if err := os.Mkdir(path, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	binary := filepath.Join(root, "fake-tailscale")
	route := strings.ReplaceAll(fixtureRoute, "18377", strconv.Itoa(backendPort))
	publishCommand := fmt.Sprintf("serve --bg --https=8443 --set-path=/ http://127.0.0.1:%d", backendPort)
	script := "#!/bin/sh\n# " + syntheticFixtureCLIMarker + "\n" +
		"STATE=\"${0%/*}/serve.json\"\nEVENTS=\"${0}.events\"\n" +
		"case \"$*\" in\n" +
		"  'status --json') printf '%s\\n' '" + fixtureStatus + "' ;;\n" +
		"  'version --json --daemon') printf '%s\\n' '" + fixtureVersion + "' ;;\n" +
		"  'serve status --json') if [ -f \"$STATE\" ]; then cat \"$STATE\"; else printf '{}\\n'; fi ;;\n" +
		"  '" + publishCommand + "') printf '%s\\n' '" + route + "' > \"$STATE\"; printf 'publish\\n' >> \"$EVENTS\" ;;\n" +
		"  'serve --bg --https=8443 --set-path=/ off') printf '{}\\n' > \"$STATE\"; printf 'unpublish\\n' >> \"$EVENTS\"; exit 17 ;;\n" +
		"  *) exit 2 ;;\n" +
		"esac\n"
	if err := os.WriteFile(binary, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	client, err := newClient(binary)
	if err != nil {
		t.Fatal(err)
	}
	client.profileOS, client.profileArch = "darwin", "arm64"
	manager, err := NewManager(stateRoot, coordinationRoot, client)
	if err != nil {
		t.Fatal(err)
	}
	manager.fixtureMutations = true
	manager.skipBackendReadiness = true
	request := fixtureRequest(true)
	request.BackendPort = backendPort
	request.Consent.BackendPort = backendPort
	if err := manager.Publish(context.Background(), request); err != nil {
		t.Fatalf("fixture setup publish failed: %v", err)
	}
	consent := fixtureConsent(true)
	consent.BackendPort = backendPort
	unpublishErr := manager.Unpublish(context.Background(), consent)
	if !errors.Is(unpublishErr, ErrUncertain) || strings.Contains(unpublishErr.Error(), "private@example.invalid") {
		t.Fatalf("dispatched unpublish did not retain redacted uncertainty: %v", unpublishErr)
	}
	record, err := manager.readRegistration()
	if err != nil || record == nil || record.State != StateRemoveUncertain || record.MutationAcknowledged {
		t.Fatalf("ambiguous unpublish lost durable recovery evidence: record=%+v err=%v", record, err)
	}
	if err := manager.Unpublish(context.Background(), consent); !errors.Is(err, ErrUncertain) {
		t.Fatalf("ambiguous unpublish was automatically replayed: %v", err)
	}
	events, err := os.ReadFile(binary + ".events")
	operations := strings.Split(strings.TrimSpace(string(events)), "\n")
	if err != nil || len(operations) != 2 || operations[0] != "publish" || operations[1] != "unpublish" {
		t.Fatalf("ambiguous mutation dispatch counts changed across recovery: %q err=%v", events, err)
	}
}

func TestRunCommandBoundsInheritedPipesAndKillsChildGroup(t *testing.T) {
	pidFile := filepath.Join(t.TempDir(), "child.pid")
	script := `sleep 30 & echo $! > "$1"; exit 0`
	started := time.Now()
	_, err := runCommand(context.Background(), "/bin/sh", "-c", script, "cli-fixture", pidFile)
	if !errors.Is(err, ErrUncertain) {
		t.Fatalf("inherited pipe error = %v, want ErrUncertain", err)
	}
	if elapsed := time.Since(started); elapsed > ChildWaitDelay+time.Second {
		t.Fatalf("command wait exceeded bound: %v", elapsed)
	}

	pidBytes, err := os.ReadFile(pidFile)
	if err != nil {
		t.Fatalf("read child pid: %v", err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(pidBytes)))
	if err != nil || pid <= 0 {
		t.Fatalf("invalid child pid %q: %v", pidBytes, err)
	}
	t.Cleanup(func() { _ = syscall.Kill(pid, syscall.SIGKILL) })
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if !processRunning(pid) || processIsZombie(pid) {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("child process %d survived bounded command cleanup", pid)
}

func processRunning(pid int) bool {
	return syscall.Kill(pid, 0) == nil
}

func processIsZombie(pid int) bool {
	command := exec.Command("ps", "-o", "stat=", "-p", fmt.Sprint(pid))
	state, err := command.Output()
	return err == nil && strings.HasPrefix(strings.TrimSpace(string(state)), "Z")
}
