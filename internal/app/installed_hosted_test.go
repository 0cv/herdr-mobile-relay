//go:build herdr_installed_inert_test && herdr_installed_hosted_test

package app

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/0cv/herdr-mobile-relay/internal/config"
	"github.com/0cv/herdr-mobile-relay/internal/deviceauth"
	"github.com/0cv/herdr-mobile-relay/internal/tailscalecli"
	"github.com/0cv/herdr-mobile-relay/internal/transport"
)

// No fixture, environment mutation, listener or goroutine precedes this guard.
func requireInstalledHosted(t *testing.T) {
	t.Helper()
	if os.Getenv("HERDR_INSTALLED_HOSTED_TEST") != "1" || os.Getenv("GITHUB_ACTIONS") != "true" || os.Getenv("RUNNER_ENVIRONMENT") != "github-hosted" {
		t.Skip("requires explicitly authorized GitHub-hosted execution")
	}
	want := map[string]string{"linux": "Linux", "darwin": "macOS"}[runtime.GOOS]
	if want == "" || os.Getenv("RUNNER_OS") != want || os.Getenv("GITHUB_RUN_ID") == "" || os.Getenv("GITHUB_RUN_ATTEMPT") == "" || os.Getenv("GITHUB_SHA") == "" {
		t.Fatal("incomplete hosted execution identity")
	}
}

type installedHostedDenyHTTP struct{}

func (installedHostedDenyHTTP) RoundTrip(*http.Request) (*http.Response, error) {
	return nil, errors.New("hosted lifecycle fixture denies HTTP egress")
}

func TestInstalledHostedRealRunBindFailure(t *testing.T) {
	requireInstalledHosted(t)
	installedHostedLifecycle(t, true, false, nil)
}

func TestInstalledHostedRealRunShutdownRestart(t *testing.T) {
	requireInstalledHosted(t)
	installedHostedLifecycle(t, false, false, nil)
}

func TestInstalledHostedRealRunCancelBeforeServe(t *testing.T) {
	requireInstalledHosted(t)
	installedHostedLifecycle(t, false, true, nil)
}

func installedHostedLifecycle(t *testing.T, collision, pauseServe bool, afterRun func(*Server)) {
	t.Helper()
	// Empty the inherited environment, including all tool, socket and credential
	// overrides. PATH contains no executable. Explicit missing Herdr paths never
	// resolve an ambient daemon or CLI. No child process can start.
	IsolateInstalledFixture(t)
	// Defense in depth for default HTTP users; explicit managed clients below
	// also deny egress. No timing assumption about recovery/update ticks.
	previousTransport := http.DefaultTransport
	http.DefaultTransport = installedHostedDenyHTTP{}
	t.Cleanup(func() { http.DefaultTransport = previousTransport })
	m, state, coordination, binary := tailscalecli.NewInstalledInertFixture(t)
	base := filepath.Dir(state)
	cfg := &config.Config{
		Transport: config.TransportTailscaleCLI, Host: "127.0.0.1", Port: 18377, PluginPort: 18378,
		TailscaleCLIScope: "production", TailscaleCLIOrigin: "https://herdr.tailnet.ts.net:8443",
		InstanceID: "install-fixture", TailscaleCLIStateRoot: state, TailscaleCLICoordinationRoot: coordination, TailscaleCLIBin: binary,
		RuntimeDir: filepath.Join(base, "config", "relay"), CacheDir: filepath.Join(base, "cache"), ConfigHome: filepath.Join(base, "config"), ReleaseRoot: filepath.Join(base, "release"), WebRoot: filepath.Join(base, "web"), PollInterval: 1,
		HerdrBin: filepath.Join(base, "absent-tool", "herdr"), SocketPath: filepath.Join(base, "absent-socket", "herdr.sock"),
		Token: "01234567890123456789012345678901", PhoneAppOrigin: "https://herdr.tailnet.ts.net:8443",
	}
	for _, dir := range []string{cfg.RuntimeDir, cfg.CacheDir, cfg.ReleaseRoot, cfg.WebRoot} {
		if err := os.MkdirAll(dir, 0700); err != nil {
			t.Fatal(err)
		}
	}
	selected, err := config.BindInstalledCLI(cfg)
	if err != nil {
		t.Fatal(err)
	}
	store, err := deviceauth.Open(filepath.Join(cfg.RuntimeDir, "device-auth"))
	if err != nil {
		t.Fatal(err)
	}
	var selectors []transport.E2EEAuthSelector
	var secrets [][]byte
	for _, role := range []deviceauth.Role{deviceauth.RoleController, deviceauth.RoleReader} {
		invitation, err := store.CreateInvitation(string(role), role, "en")
		if err != nil {
			t.Fatal(err)
		}
		result, err := store.CompleteE2EEAuth(context.Background(), transport.E2EEAuthSelector{Kind: transport.E2EEAuthInvitation, ID: invitation.InvitationID, Version: invitation.Version}, true)
		if err != nil {
			t.Fatal(err)
		}
		selector := transport.E2EEAuthSelector{Kind: transport.E2EEAuthCredential, ID: result.Identity.CredentialID, Version: result.Identity.CredentialVersion, Locale: result.Identity.Locale}
		if _, err := store.CompleteE2EEAuth(context.Background(), selector, true); err != nil {
			t.Fatal(err)
		}
		secret, err := store.ResolveE2EESecret(context.Background(), selector)
		if err != nil {
			t.Fatal(err)
		}
		selectors = append(selectors, selector)
		secrets = append(secrets, secret)
	}
	credentials := store.ListCredentials("")
	read := func(path string) []byte {
		t.Helper()
		b, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		return b
	}
	paths := []string{filepath.Join(cfg.RuntimeDir, "device-auth", "devices.json"), filepath.Join(state, "registration.json"), filepath.Join(coordination, "backend-port-18377.json")}
	original := make([][]byte, len(paths))
	for i, path := range paths {
		original[i] = read(path)
	}
	unchanged := func(s *Server) {
		t.Helper()
		for i, path := range paths {
			if !bytes.Equal(original[i], read(path)) {
				t.Fatal("persistent evidence changed")
			}
		}
		if !reflect.DeepEqual(credentials, s.deviceStore().ListCredentials("")) {
			t.Fatal("credential identities/roles changed")
		}
		for i, selector := range selectors {
			secret, err := s.deviceStore().ResolveE2EESecret(context.Background(), selector)
			if err != nil || !bytes.Equal(secret, secrets[i]) {
				t.Fatal("credential secret changed")
			}
		}
		if s.bootstrapGate.OpenStatus() || s.pairingControl != nil {
			t.Fatal("unexpected bootstrap/control admission")
		}
	}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	run := func(ctx context.Context) error {
		return RunInstalledCLI(ctx, selected, "fixture", "fixture", logger, m)
	}
	competing := func() {
		t.Helper()
		ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
		defer cancel()
		if err := m.WithInstalledSession(ctx, selected.Binding(), func(context.Context, *tailscalecli.InstalledSession) error {
			return errors.New("unexpected acquisition")
		}); !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("ownership not held: %v", err)
		}
	}
	for attempt := 0; attempt < 2; attempt++ {
		var blocker net.Listener
		if collision && attempt == 0 {
			blocker, err = net.Listen("tcp", cfg.Addr())
			if err != nil {
				t.Fatal("cannot reserve fixture backend", err)
			}
			t.Cleanup(func() { _ = blocker.Close() })
		}
		entered := make(chan *Server, 1)
		returned := make(chan error, 1)
		release := make(chan struct{})
		done := make(chan error, 1)
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		serveEntered, serveRelease := make(chan struct{}), make(chan struct{})
		serveDraining := make(chan struct{})
		var releaseServeOnce sync.Once
		releaseServe := func() { releaseServeOnce.Do(func() { close(serveRelease) }) }
		SetInstalledInertRunner(t, func(s *Server, ctx context.Context) error {
			s.developmentCLIHealthClient = func(time.Duration) *http.Client { return &http.Client{Transport: installedHostedDenyHTTP{}} }
			s.verifyPublicBundle = func(context.Context, string, string, string, string) error {
				return errors.New("hosted lifecycle fixture denies bundle egress")
			}
			if pauseServe {
				s.installedServeDraining = func() { close(serveDraining) }
				s.installedBeforeServe = func() {
					close(serveEntered)
					<-serveRelease
				}
			}
			entered <- s
			err := s.Run(ctx) // Actual construction, TCP/UDP binding, backgrounds and cleanup.
			if afterRun != nil && s.dispatcher != nil {
				// Safety only, after the observer sees Run's own completion.
				// Also joins an ordinary dispatcher if factory selection regresses.
				defer s.dispatcher.Close(context.Background())
			}
			returned <- err
			<-release // Keep concrete ownership held while asserting Run has closed TCP.
			return err
		})
		go func() { done <- run(ctx) }()
		released := false
		completed := false
		finish := func() {
			if !released {
				close(release)
				released = true
			}
		}
		// Registered before any assertion: failures cancel, unblock and join Run.
		cleanup := func() {
			cancel()
			releaseServe()
			finish()
			if !completed {
				select {
				case <-done:
					completed = true
				case <-time.After(35 * time.Second):
					t.Error("Run cleanup did not complete")
					// Retain isolation until joined; the hosted driver's go test
					// -timeout=3m terminates the process if Run never finishes.
					<-done
					completed = true
				}
			}
		}
		t.Cleanup(cleanup)
		var s *Server
		select {
		case s = <-entered:
		case err := <-done:
			completed = true
			t.Fatalf("app entry: %v", err)
		case <-time.After(10 * time.Second):
			t.Fatal("app entry deadline")
		}
		if blocker == nil {
			select {
			case <-s.ListenerBound():
			case err := <-returned:
				t.Fatalf("Run before bind: %v", err)
			case <-time.After(10 * time.Second):
				t.Fatal("listener deadline")
			}
			if pauseServe {
				select {
				case <-serveEntered:
				case <-time.After(5 * time.Second):
					t.Fatal("pre-Serve scheduling deadline")
				}
			}
			competing()
			// A deterministic in-flight lifecycle task proves that cancellation
			// cannot release authority before Run drains its owned work.
			taskCanceled, taskRelease := make(chan struct{}), make(chan struct{})
			var releaseTaskOnce sync.Once
			releaseTask := func() { releaseTaskOnce.Do(func() { close(taskRelease) }) }
			t.Cleanup(releaseTask)
			if !s.transitionTasks.Start(func(taskCtx context.Context) {
				<-taskCtx.Done()
				close(taskCanceled)
				<-taskRelease
			}) {
				t.Fatal("lifecycle task admission closed before cancellation")
			}
			cancel()
			select {
			case <-taskCanceled:
			case <-time.After(5 * time.Second):
				t.Fatal("lifecycle task cancellation deadline")
			}
			competing()
			select {
			case <-returned:
				t.Fatal("Run returned before draining its task")
			default:
			}
			releaseTask()
			if pauseServe {
				select {
				case <-serveDraining:
				case <-time.After(15 * time.Second):
					t.Fatal("serving join deadline")
				}
				probe, err := net.Listen("tcp", cfg.Addr())
				if err != nil {
					t.Fatal("physical listener not closed before join", err)
				}
				if err := probe.Close(); err != nil {
					t.Fatal(err)
				}
				// All other drain work can finish, but Run must still own and
				// join the paused serving goroutine before returning.
				competing()
				select {
				case <-returned:
					t.Fatal("Run escaped unfinished pre-Serve work")
				default:
				}
				releaseServe()
			}
		}
		var runErr error
		select {
		case runErr = <-returned:
		case <-time.After(20 * time.Second):
			t.Fatal("Run return deadline")
		}
		if afterRun != nil {
			afterRun(s)
		}
		if blocker != nil {
			if !errors.Is(runErr, syscall.EADDRINUSE) {
				t.Fatalf("not a concrete bind collision: %v", runErr)
			}
			select {
			case <-s.ListenerBound():
				t.Fatal("failed bind claimed ownership")
			default:
			}
			if err := blocker.Close(); err != nil {
				t.Fatal(err)
			}
		} else if runErr != nil {
			t.Fatalf("shutdown: %v", runErr)
		}
		// Run has returned but the runner callback still holds session ownership.
		competing()
		probe, err := net.Listen("tcp", cfg.Addr())
		if err != nil {
			t.Fatal("backend survived Run cleanup", err)
		}
		if err := probe.Close(); err != nil {
			t.Fatal(err)
		}
		udp, err := net.ListenPacket("udp", "127.0.0.1:18378")
		if err != nil {
			t.Fatal("UDP survived Run cleanup", err)
		}
		if err := udp.Close(); err != nil {
			t.Fatal(err)
		}
		if _, err := os.Lstat(s.pidFilePath()); !os.IsNotExist(err) {
			t.Fatal("PID file survived Run cleanup", err)
		}
		unchanged(s)
		finish()
		select {
		case err := <-done:
			completed = true
			if !errors.Is(err, runErr) {
				t.Fatalf("entrypoint outcome: %v", err)
			}
		case <-time.After(10 * time.Second):
			t.Fatal("authority release deadline")
		}
		cancel()
		if err := s.installedCLI.ValidateRuntimeBinding(selected.Binding()); err == nil {
			t.Fatal("retained authority survived retirement")
		}
		acquire, cancelAcquire := context.WithTimeout(context.Background(), 5*time.Second)
		acquired := false
		err = m.WithInstalledSession(acquire, selected.Binding(), func(context.Context, *tailscalecli.InstalledSession) error { acquired = true; return nil })
		cancelAcquire()
		if err != nil || !acquired {
			t.Fatalf("ownership not released: %v", err)
		}
		unchanged(s)
	}
}
