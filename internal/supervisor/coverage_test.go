package supervisor

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/0cv/herdr-mobile-relay/internal/activeruntime"
	"github.com/0cv/herdr-mobile-relay/internal/release"
)

func TestNewRejectsMissingCommandsUnsafePathsAndInvalidLimits(t *testing.T) {
	base := testConfig(t, t.TempDir())
	tests := map[string]func(*Config){
		"missing relay name":       func(config *Config) { config.Relay.Name = "" },
		"relative relay":           func(config *Config) { config.Relay.Path = "relay" },
		"ordinary active runtime":  func(config *Config) { config.Managed = false },
		"managed relative runtime": func(config *Config) { config.ActiveRuntimePath = "active.json" },
		"zero failures":            func(config *Config) { config.MaxFailures = 0 },
		"zero initial backoff":     func(config *Config) { config.InitialBackoff = 0 },
		"backoff range":            func(config *Config) { config.MaxBackoff = config.InitialBackoff / 2 },
		"zero startup timeout":     func(config *Config) { config.StartupTimeout = 0 },
		"zero health interval":     func(config *Config) { config.HealthInterval = 0 },
		"zero health timeout":      func(config *Config) { config.HealthTimeout = 0 },
		"zero stable interval":     func(config *Config) { config.StableAfter = 0 },
		"zero stop timeout":        func(config *Config) { config.StopTimeout = 0 },
		"zero log bound":           func(config *Config) { config.MaxLogBytes = 0 },
		"negative log backups":     func(config *Config) { config.LogBackups = -1 },
	}
	for name, mutate := range tests {
		t.Run(name, func(t *testing.T) {
			config := base
			mutate(&config)
			if _, err := New(config); err == nil {
				t.Fatal("invalid supervisor config accepted")
			}
		})
	}
}

func TestRunStateMachinePropagatesEveryDurableBoundary(t *testing.T) {
	t.Run("read", func(t *testing.T) {
		service := newInjectedSupervisor(t)
		service.readState = func(string) (State, error) { return State{}, errors.New("read") }
		if err := service.Run(context.Background()); err == nil || err.Error() != "read" {
			t.Fatalf("read boundary = %v", err)
		}
	})

	t.Run("already cancelled", func(t *testing.T) {
		service := newInjectedSupervisor(t)
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		if err := service.Run(ctx); err != nil {
			t.Fatalf("cancelled boundary = %v", err)
		}
	})

	for _, failStatus := range []Status{StatusStarting, StatusRunning, StatusStopped, StatusRetrying, StatusTripped} {
		t.Run("write "+string(failStatus), func(t *testing.T) {
			service := newInjectedSupervisor(t)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			service.writeState = func(_ string, state State) error {
				if state.Status == failStatus {
					return errors.New("write " + string(failStatus))
				}
				return nil
			}
			switch failStatus {
			case StatusRunning:
				service.runCycle = func(_ context.Context, report func(bool) error) cycleResult {
					return cycleResult{reason: errorString(report(false)), unsafe: true}
				}
			case StatusStopped:
				service.runCycle = func(context.Context, func(bool) error) cycleResult { return cycleResult{cancelled: true} }
			case StatusRetrying:
				service.runCycle = func(context.Context, func(bool) error) cycleResult { return cycleResult{reason: "retry"} }
			case StatusTripped:
				service.runCycle = func(context.Context, func(bool) error) cycleResult {
					return cycleResult{unsafe: true, reason: "unsafe"}
				}
			}
			err := service.Run(ctx)
			if failStatus == StatusRunning {
				if !errors.Is(err, ErrTripped) {
					t.Fatalf("%s boundary = %v", failStatus, err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), "write ") {
				t.Fatalf("%s boundary = %v", failStatus, err)
			}
		})
	}
}

func TestRunRestoresFailureCountResetsOnlyAfterStableAndReloads(t *testing.T) {
	service := newInjectedSupervisor(t)
	service.readState = func(string) (State, error) { return State{Status: StatusRunning, Failures: 2}, nil }
	var written []State
	service.writeState = func(_ string, state State) error {
		written = append(written, state)
		return nil
	}
	ctx, cancel := context.WithCancel(context.Background())
	cycles := 0
	service.runCycle = func(_ context.Context, report func(bool) error) cycleResult {
		cycles++
		if cycles == 1 {
			if err := report(true); err != nil {
				t.Fatal(err)
			}
			return cycleResult{reload: true}
		}
		cancel()
		return cycleResult{cancelled: true}
	}
	if err := service.Run(ctx); err != nil {
		t.Fatal(err)
	}
	if cycles != 2 {
		t.Fatalf("cycles = %d", cycles)
	}
	foundReset := false
	for _, state := range written {
		if state.Status == StatusRunning && state.Failures == 0 {
			foundReset = true
		}
	}
	if !foundReset {
		t.Fatalf("stable reset not written: %+v", written)
	}
}

func TestRunCancellationDuringBackoffStopsCleanly(t *testing.T) {
	service := newInjectedSupervisor(t)
	service.config.InitialBackoff = time.Hour
	service.config.MaxBackoff = time.Hour
	ctx, cancel := context.WithCancel(context.Background())
	service.runCycle = func(context.Context, func(bool) error) cycleResult {
		cancel()
		return cycleResult{reason: "retry"}
	}
	if err := service.Run(ctx); err != nil {
		t.Fatal(err)
	}
}

func TestRunCancellationDuringBackoffPropagatesStoppedWriteFailure(t *testing.T) {
	service := newInjectedSupervisor(t)
	service.config.InitialBackoff = time.Hour
	service.config.MaxBackoff = time.Hour
	ctx, cancel := context.WithCancel(context.Background())
	service.runCycle = func(context.Context, func(bool) error) cycleResult { return cycleResult{reason: "retry"} }
	service.writeState = func(_ string, state State) error {
		if state.Status == StatusRetrying {
			cancel()
		}
		if state.Status == StatusStopped {
			return errors.New("stopped write")
		}
		return nil
	}
	if err := service.Run(ctx); err == nil || err.Error() != "stopped write" {
		t.Fatalf("backoff stopped boundary = %v", err)
	}
}

func TestDefaultHealthClosureUsesConfiguredMode(t *testing.T) {
	config := testConfig(t, t.TempDir())
	config.Managed = false
	config.ActiveRuntimePath = ""
	service, err := New(config)
	if err != nil {
		t.Fatal(err)
	}
	service.release = func() (release.Manifest, error) {
		return release.Manifest{Version: "test-version", Revision: "test-revision", WebHash: "test-web"}, nil
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := service.health(ctx); err == nil || !strings.Contains(err.Error(), "local readiness") {
		t.Fatalf("default health = %v", err)
	}
}

func TestDefaultReleaseAndManagedHealthPropagateIdentityFailures(t *testing.T) {
	service, err := New(testConfig(t, t.TempDir()))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.release(); err == nil {
		t.Fatal("missing current release was accepted")
	}
	service.release = func() (release.Manifest, error) {
		return release.Manifest{Version: "test-version", Revision: "test-revision", WebHash: "test-web"}, nil
	}
	service.active = func() (activeruntime.Snapshot, error) { return activeruntime.Snapshot{}, errors.New("active") }
	if err := service.health(context.Background()); err == nil || !strings.Contains(err.Error(), "load active runtime identity") {
		t.Fatalf("managed active-runtime failure = %v", err)
	}
}

func TestRunPairPropagatesActiveLogAndStartFailures(t *testing.T) {
	service := newInjectedSupervisor(t)
	service.active = func() (activeruntime.Snapshot, error) { return activeruntime.Snapshot{}, errors.New("active") }
	if result := service.runPair(context.Background(), func(bool) error { return nil }); !strings.Contains(result.reason, "load active runtime") {
		t.Fatalf("active result = %+v", result)
	}

	for failedOpen := 1; failedOpen <= 4; failedOpen++ {
		service = newInjectedSupervisor(t)
		opens := 0
		service.openLog = func(string, int64, int) (io.Writer, error) {
			opens++
			if opens == failedOpen {
				return nil, errors.New("open")
			}
			return io.Discard, nil
		}
		result := service.runPair(context.Background(), func(bool) error { return nil })
		if !strings.Contains(result.reason, "open ") {
			t.Fatalf("open %d result = %+v", failedOpen, result)
		}
	}

	for _, failedChild := range []string{"relay", "cloudflared"} {
		service = newInjectedSupervisor(t)
		starter := &fakeStarter{failStart: failedChild}
		service.start = starter.Start
		result := service.runPair(context.Background(), func(bool) error { return nil })
		if !strings.Contains(result.reason, "start "+failedChild) {
			t.Fatalf("%s start result = %+v", failedChild, result)
		}
	}
}

func TestRunPairTreatsCancellationAtEveryChildBoundaryAsCleanStop(t *testing.T) {
	t.Run("relay start", func(t *testing.T) {
		service := newInjectedSupervisor(t)
		service.start = (&fakeStarter{failStart: "relay"}).Start
		ctx := &latentCancelledContext{Context: context.Background()}
		ctx.cancel()
		if result := service.runPair(ctx, func(bool) error { return nil }); !result.cancelled || result.unsafe {
			t.Fatalf("relay start cancellation = %+v", result)
		}
	})

	t.Run("tunnel start", func(t *testing.T) {
		service := newInjectedSupervisor(t)
		starter := &fakeStarter{}
		ctx := &latentCancelledContext{Context: context.Background()}
		service.start = func(command Command, stdout, stderr io.Writer) (process, error) {
			if command.Name == "cloudflared" {
				ctx.cancel()
				return nil, errors.New("tunnel start")
			}
			return starter.Start(command, stdout, stderr)
		}
		if result := service.runPair(ctx, func(bool) error { return nil }); !result.cancelled || result.unsafe {
			t.Fatalf("tunnel start cancellation = %+v", result)
		}
	})

	for _, childName := range []string{"relay", "cloudflared"} {
		t.Run(childName+" exit", func(t *testing.T) {
			service := newInjectedSupervisor(t)
			service.config.HealthInterval = time.Hour
			service.config.StartupTimeout = time.Hour
			starter := &fakeStarter{}
			ctx := &latentCancelledContext{Context: context.Background()}
			var selected *fakeProcess
			service.start = func(command Command, stdout, stderr io.Writer) (process, error) {
				child, err := starter.Start(command, stdout, stderr)
				if command.Name == childName && err == nil {
					selected = child.(*fakeProcess)
				}
				return child, err
			}
			var once sync.Once
			service.health = func(context.Context) error {
				once.Do(func() {
					time.AfterFunc(10*time.Millisecond, func() {
						ctx.cancel()
						selected.finish(errors.New("child exit"))
					})
				})
				return nil
			}
			if result := service.runPair(ctx, func(bool) error { return nil }); !result.cancelled || result.unsafe {
				t.Fatalf("%s cancellation = %+v", childName, result)
			}
		})
	}

	t.Run("startup timeout", func(t *testing.T) {
		service := newInjectedSupervisor(t)
		service.config.StartupTimeout = 20 * time.Millisecond
		service.config.HealthInterval = time.Hour
		service.start = (&fakeStarter{}).Start
		ctx := &latentCancelledContext{Context: context.Background()}
		var once sync.Once
		service.health = func(context.Context) error {
			once.Do(func() { time.AfterFunc(5*time.Millisecond, ctx.cancel) })
			return errors.New("warming")
		}
		if result := service.runPair(ctx, func(bool) error { return nil }); !result.cancelled || result.unsafe {
			t.Fatalf("startup cancellation = %+v", result)
		}
	})

	t.Run("context channel", func(t *testing.T) {
		service := newInjectedSupervisor(t)
		service.config.HealthInterval = time.Hour
		service.config.StartupTimeout = time.Hour
		service.start = (&fakeStarter{}).Start
		ctx, cancel := context.WithCancel(context.Background())
		var once sync.Once
		service.health = func(context.Context) error {
			once.Do(func() { time.AfterFunc(10*time.Millisecond, cancel) })
			return nil
		}
		if result := service.runPair(ctx, func(bool) error { return nil }); !result.cancelled || result.unsafe {
			t.Fatalf("context cancellation = %+v", result)
		}
	})

	t.Run("managed active check", func(t *testing.T) {
		service := newInjectedSupervisor(t)
		service.start = (&fakeStarter{}).Start
		active, err := activeruntime.Load(service.config.ActiveRuntimePath)
		if err != nil {
			t.Fatal(err)
		}
		ctx := &latentCancelledContext{Context: context.Background()}
		loads := 0
		service.active = func() (activeruntime.Snapshot, error) {
			loads++
			if loads == 2 {
				ctx.cancel()
			}
			return active, nil
		}
		if result := service.runPair(ctx, func(bool) error { return nil }); !result.cancelled || result.unsafe {
			t.Fatalf("managed active-check cancellation = %+v", result)
		}
	})
}

func TestRunPairCoversChildExitTimeoutHealthAndStableTransitions(t *testing.T) {
	t.Run("ordinary stable success", func(t *testing.T) {
		service := newInjectedSupervisor(t)
		service.config.Managed = false
		service.config.ActiveRuntimePath = ""
		service.config.StableAfter = time.Nanosecond
		service.active = nil
		starter := &fakeStarter{}
		service.start = starter.Start
		ctx, cancel := context.WithCancel(context.Background())
		reports := 0
		result := service.runPair(ctx, func(stable bool) error {
			reports++
			if stable {
				cancel()
			}
			return nil
		})
		if !result.cancelled || result.unsafe || reports != 2 {
			t.Fatalf("ordinary stable result = %+v, reports=%d", result, reports)
		}
	})

	t.Run("stable report succeeds before child exit", func(t *testing.T) {
		service := newInjectedSupervisor(t)
		service.config.Managed = false
		service.config.ActiveRuntimePath = ""
		service.config.StableAfter = time.Nanosecond
		service.config.HealthInterval = time.Hour
		starter := &fakeStarter{}
		var tunnel *fakeProcess
		service.start = func(command Command, stdout, stderr io.Writer) (process, error) {
			child, err := starter.Start(command, stdout, stderr)
			if command.Name == "cloudflared" && err == nil {
				tunnel = child.(*fakeProcess)
			}
			return child, err
		}
		reports := 0
		result := service.runPair(context.Background(), func(stable bool) error {
			reports++
			if stable {
				time.AfterFunc(10*time.Millisecond, func() { tunnel.finish(nil) })
			}
			return nil
		})
		if result.reason != "cloudflared exited" || result.unsafe || reports != 2 {
			t.Fatalf("stable success result = %+v, reports=%d", result, reports)
		}
	})

	t.Run("tunnel exits cleanly", func(t *testing.T) {
		service := newInjectedSupervisor(t)
		starter := &fakeStarter{}
		tunnelStarted := make(chan *fakeProcess, 1)
		service.start = func(command Command, stdout, stderr io.Writer) (process, error) {
			child, err := starter.Start(command, stdout, stderr)
			if command.Name == "cloudflared" && err == nil {
				tunnelStarted <- child.(*fakeProcess)
			}
			return child, err
		}
		done := make(chan cycleResult, 1)
		go func() { done <- service.runPair(context.Background(), func(bool) error { return nil }) }()
		tunnel := <-tunnelStarted
		tunnel.finish(nil)
		result := <-done
		if result.reason != "cloudflared exited" || result.unsafe {
			t.Fatalf("tunnel result = %+v", result)
		}
	})

	t.Run("startup timeout", func(t *testing.T) {
		service := newInjectedSupervisor(t)
		starter := &fakeStarter{}
		service.start = starter.Start
		service.health = func(context.Context) error { return errors.New("warming") }
		result := service.runPair(context.Background(), func(bool) error { return nil })
		if !strings.Contains(result.reason, "startup timeout") || result.unsafe {
			t.Fatalf("startup result = %+v", result)
		}
	})

	t.Run("active runtime invalidates", func(t *testing.T) {
		service := newInjectedSupervisor(t)
		active, err := activeruntime.Load(service.config.ActiveRuntimePath)
		if err != nil {
			t.Fatal(err)
		}
		calls := 0
		service.active = func() (activeruntime.Snapshot, error) {
			calls++
			if calls == 1 {
				return active, nil
			}
			return activeruntime.Snapshot{}, errors.New("invalid")
		}
		starter := &fakeStarter{}
		service.start = starter.Start
		result := service.runPair(context.Background(), func(bool) error { return nil })
		if !strings.Contains(result.reason, "became invalid") || result.unsafe {
			t.Fatalf("active result = %+v", result)
		}
	})

	t.Run("health regresses", func(t *testing.T) {
		service := newInjectedSupervisor(t)
		starter := &fakeStarter{}
		service.start = starter.Start
		checks := 0
		service.health = func(context.Context) error {
			checks++
			if checks == 1 {
				return nil
			}
			return errors.New("regressed")
		}
		result := service.runPair(context.Background(), func(bool) error { return nil })
		if !strings.Contains(result.reason, "joint readiness failed") || result.unsafe {
			t.Fatalf("health result = %+v", result)
		}
	})

	for _, failStable := range []bool{false, true} {
		name := "initial running report"
		if failStable {
			name = "stable running report"
		}
		t.Run(name, func(t *testing.T) {
			service := newInjectedSupervisor(t)
			starter := &fakeStarter{}
			service.start = starter.Start
			service.health = func(context.Context) error { return nil }
			if failStable {
				service.config.StableAfter = time.Nanosecond
			}
			reports := 0
			result := service.runPair(context.Background(), func(stable bool) error {
				reports++
				if stable == failStable {
					return errors.New("write")
				}
				return nil
			})
			if !result.unsafe || !strings.Contains(result.reason, "write ") || reports < 1 {
				t.Fatalf("report result = %+v, calls=%d", result, reports)
			}
		})
	}
}

func TestStopChildrenEscalatesToKillAndFailsIfKillCannotStop(t *testing.T) {
	service := newInjectedSupervisor(t)
	service.config.StopTimeout = time.Millisecond
	stopsAfterKill := newCoverProcess(false, true)
	child := &runningChild{name: "relay", process: stopsAfterKill, done: stopsAfterKill.done}
	if err := service.stopChildren(child, nil); err != nil {
		t.Fatalf("kill cleanup = %v", err)
	}
	if stopsAfterKill.signals != 1 || stopsAfterKill.kills != 1 {
		t.Fatalf("cleanup calls = signal %d kill %d", stopsAfterKill.signals, stopsAfterKill.kills)
	}

	stuck := newCoverProcess(false, false)
	child = &runningChild{name: "relay", process: stuck, done: stuck.done}
	if err := service.stopChildren(child, nil); err == nil {
		t.Fatal("unstoppable child accepted")
	}
}

func TestSupervisorHelpersCoverEveryResultShape(t *testing.T) {
	if got := childFailure(&runningChild{name: "relay"}); got != "relay exited" {
		t.Fatalf("clean child failure = %q", got)
	}
	if got := joinFailure("failed", errors.New("cleanup")); !strings.Contains(got, "cleanup") {
		t.Fatalf("joined failure = %q", got)
	}
	config := Config{InitialBackoff: 3 * time.Second, MaxBackoff: 2 * time.Second}
	if got := retryDelay(config, 1); got != 2*time.Second {
		t.Fatalf("invalid delay cap = %s", got)
	}
	if !waitChildren(nil, time.Millisecond) {
		t.Fatal("empty child set did not stop")
	}
	active := activeruntime.Snapshot{Generation: "g1", SocketPath: "/socket", ExpectedInventoryPath: "/inventory"}
	command := commandForRuntime(Command{Env: []string{"PATH=/bin", "BROKEN", "HERDR_SOCKET_PATH=/old"}}, "/active", active)
	if !strings.Contains(strings.Join(command.Env, "\n"), "PATH=/bin") || strings.Contains(strings.Join(command.Env, "\n"), "BROKEN") {
		t.Fatalf("runtime environment = %v", command.Env)
	}
}

func TestStartCommandCoversFailureWaitSignalAndKill(t *testing.T) {
	if err := (&commandProcess{}).Signal(nonSyscallSignal{}); err == nil {
		t.Fatal("non-syscall child signal accepted")
	}
	if _, err := startCommand(Command{Path: "/missing/ouro-command"}, io.Discard, io.Discard); err == nil {
		t.Fatal("missing child command accepted")
	}
	for _, stop := range []func(process) error{
		func(child process) error { return child.Signal(syscall.SIGTERM) },
		func(child process) error { return child.Kill() },
	} {
		child, err := startCommand(Command{Path: "/bin/sleep", Args: []string{"10"}}, io.Discard, io.Discard)
		if err != nil {
			t.Fatal(err)
		}
		if err := stop(child); err != nil {
			t.Fatal(err)
		}
		if err := child.Wait(); err == nil {
			t.Fatal("terminated child reported success")
		}
	}
}

type nonSyscallSignal struct{}

func (nonSyscallSignal) Signal()        {}
func (nonSyscallSignal) String() string { return "fixture" }

func TestAcquireLifetimeLockPropagatesEveryBoundaryAndClosesOpenedFile(t *testing.T) {
	base := func(file *lockFileFixture) lifetimeLockIO {
		return lifetimeLockIO{
			mkdirAll: func(string, os.FileMode) error { return nil },
			chmod:    func(string, os.FileMode) error { return nil },
			openFile: func(string, int, os.FileMode) (lifetimeLockFile, error) { return file, nil },
			flock:    func(int, int) error { return nil },
		}
	}
	tests := map[string]func(*lifetimeLockIO, *lockFileFixture){
		"mkdir": func(ops *lifetimeLockIO, _ *lockFileFixture) {
			ops.mkdirAll = func(string, os.FileMode) error { return errors.New("mkdir") }
		},
		"chmod directory": func(ops *lifetimeLockIO, _ *lockFileFixture) {
			ops.chmod = func(string, os.FileMode) error { return errors.New("chmod directory") }
		},
		"open": func(ops *lifetimeLockIO, _ *lockFileFixture) {
			ops.openFile = func(string, int, os.FileMode) (lifetimeLockFile, error) { return nil, errors.New("open") }
		},
		"chmod file": func(_ *lifetimeLockIO, file *lockFileFixture) { file.chmodErr = errors.New("chmod file") },
		"stat":       func(_ *lifetimeLockIO, file *lockFileFixture) { file.statErr = errors.New("stat") },
		"unsafe mode": func(_ *lifetimeLockIO, file *lockFileFixture) {
			file.info = lockInfoFixture{mode: os.ModeDir | 0o700}
		},
		"flock": func(ops *lifetimeLockIO, _ *lockFileFixture) {
			ops.flock = func(int, int) error { return syscall.EINVAL }
		},
	}
	for name, mutate := range tests {
		t.Run(name, func(t *testing.T) {
			file := &lockFileFixture{info: lockInfoFixture{mode: 0o600}}
			ops := base(file)
			mutate(&ops, file)
			if _, err := acquireLifetimeLockWith(ops, "/absolute/supervisor.lock"); err == nil {
				t.Fatal("lifetime lock boundary failure was ignored")
			}
			if name != "mkdir" && name != "chmod directory" && name != "open" && !file.closed {
				t.Fatal("opened lifetime lock file was not closed after failure")
			}
		})
	}
}

func TestReadinessCoversLocalFailureInvalidBodiesAndOrdinaryGeneration(t *testing.T) {
	if err := checkJointReadiness(context.Background(), ":", "https://relay.example/readyz"); err == nil || !strings.Contains(err.Error(), "local") {
		t.Fatalf("local failure = %v", err)
	}
	if validPublicHealthURL("https://bad_host.example/readyz") {
		t.Fatal("invalid public hostname accepted")
	}
	for _, value := range []string{"%", "https:///readyz"} {
		if _, ok := parseExactReadinessURL(value); ok {
			t.Fatalf("invalid exact readiness URL accepted: %q", value)
		}
	}

	tests := map[string]string{
		"oversized":     strings.Repeat("x", maxReadinessBytes+1),
		"malformed":     "{",
		"trailing":      `{"status":"ready","instance":"i","revision":"r","generation":"g"}{}`,
		"incomplete":    `{"status":"ready","instance":"","revision":"r","generation":"g"}`,
		"bad status":    `{"status":"degraded","instance":"i","revision":"r","generation":"g"}`,
		"no generation": `{"status":"ready","instance":"i","release_version":"v","revision":"r","bundle_hash":"web","generation":""}`,
	}
	for name, body := range tests {
		t.Run(name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(writer, body) }))
			defer server.Close()
			if _, err := readReadiness(context.Background(), server.URL); err == nil {
				t.Fatal("invalid readiness accepted")
			}
		})
	}

	ordinary := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(writer, `{"status":"acknowledged_empty","instance":"i","release_version":"v","revision":"r","bundle_hash":"web","generation":""}`)
	}))
	defer ordinary.Close()
	if identity, err := readReadinessMode(context.Background(), ordinary.URL, false); err != nil || identity.Status != "acknowledged_empty" {
		t.Fatalf("ordinary readiness = %+v, %v", identity, err)
	}
	if err := validateReadinessMode(readinessIdentity{Status: "acknowledged_empty"}, false); err == nil {
		t.Fatal("ordinary acknowledged-empty readiness accepted")
	}

	originalClient := readinessHTTPClient
	readinessHTTPClient = &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: http.StatusOK, Body: errorReadCloser{}}, nil
	})}
	t.Cleanup(func() { readinessHTTPClient = originalClient })
	if _, err := readReadiness(context.Background(), "http://relay.example/readyz"); err == nil || err.Error() != "read" {
		t.Fatalf("body read failure = %v", err)
	}
}

func TestVerifyReadinessExpectationCoversEveryManagedAndOrdinaryPolicyOutcome(t *testing.T) {
	expected := ReadinessExpectation{Managed: true, Instance: "relay", ReleaseVersion: "1.2.3", Revision: "rev", BundleHash: "web", Generation: "g1"}
	valid := readinessIdentity{
		Status: "ready", Instance: "relay", ReleaseVersion: "1.2.3", Revision: "rev", BundleHash: "web", Generation: "g1",
		ExpectedInventory: &readinessInventoryResult{Ready: true, State: "ready", Generation: "g1", Expected: 1, Observed: 1},
	}
	if err := verifyReadinessExpectation(valid, expected); err != nil {
		t.Fatalf("valid managed readiness = %v", err)
	}
	tests := map[string]struct {
		mutate func(*readinessIdentity, *ReadinessExpectation)
		want   string
	}{
		"identity":             {func(value *readinessIdentity, _ *ReadinessExpectation) { value.Instance = "other" }, "expected instance"},
		"missing inventory":    {func(value *readinessIdentity, _ *ReadinessExpectation) { value.ExpectedInventory = nil }, "expected-inventory proof"},
		"inventory not ready":  {func(value *readinessIdentity, _ *ReadinessExpectation) { value.ExpectedInventory.Ready = false }, "expected-inventory proof"},
		"inventory generation": {func(value *readinessIdentity, _ *ReadinessExpectation) { value.ExpectedInventory.Generation = "other" }, "expected-inventory proof"},
		"inventory count":      {func(value *readinessIdentity, _ *ReadinessExpectation) { value.ExpectedInventory.Observed = 0 }, "expected-inventory proof"},
		"ready state": {func(value *readinessIdentity, _ *ReadinessExpectation) {
			value.ExpectedInventory.State = "acknowledged_empty"
		}, "non-empty exact inventory"},
		"ready empty": {func(value *readinessIdentity, _ *ReadinessExpectation) {
			value.ExpectedInventory.Expected = 0
			value.ExpectedInventory.Observed = 0
		}, "non-empty exact inventory"},
		"empty state": {func(value *readinessIdentity, _ *ReadinessExpectation) { value.Status = "acknowledged_empty" }, "explicitly acknowledged"},
		"empty expected": {func(value *readinessIdentity, _ *ReadinessExpectation) {
			value.Status = "acknowledged_empty"
			value.ExpectedInventory.State = "acknowledged_empty"
			value.ExpectedInventory.Expected = 1
		}, "explicitly acknowledged"},
		"empty observed": {func(value *readinessIdentity, _ *ReadinessExpectation) {
			value.Status = "acknowledged_empty"
			value.ExpectedInventory.State = "acknowledged_empty"
			value.ExpectedInventory.Observed = 1
		}, "explicitly acknowledged"},
		"managed status": {func(value *readinessIdentity, _ *ReadinessExpectation) { value.Status = "degraded" }, "managed relay is not ready"},
		"ordinary status": {func(value *readinessIdentity, expectation *ReadinessExpectation) {
			expectation.Managed = false
			expectation.Generation = ""
			value.Generation = ""
			value.Status = "acknowledged_empty"
			value.ExpectedInventory = nil
		}, "ordinary relay is not ready"},
	}
	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			value := valid
			inventory := *valid.ExpectedInventory
			value.ExpectedInventory = &inventory
			expectation := expected
			test.mutate(&value, &expectation)
			if err := verifyReadinessExpectation(value, expectation); err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("policy result = %v, want %q", err, test.want)
			}
		})
	}
	ordinary := valid
	ordinary.Generation = ""
	ordinary.ExpectedInventory = nil
	ordinaryExpectation := expected
	ordinaryExpectation.Managed = false
	ordinaryExpectation.Generation = ""
	if err := verifyReadinessExpectation(ordinary, ordinaryExpectation); err != nil {
		t.Fatalf("valid ordinary readiness = %v", err)
	}
	rightOnly := ordinary
	rightOnly.ExpectedInventory = valid.ExpectedInventory
	if sameReadinessIdentity(ordinary, rightOnly) {
		t.Fatal("one-sided inventory proof treated as the same identity")
	}
}

func TestJointReadinessExpectationPropagatesPublicFailureAndIdentityMismatch(t *testing.T) {
	expected := ReadinessExpectation{Managed: true, Instance: "relay", ReleaseVersion: "1.2.3", Revision: "rev", BundleHash: "web", Generation: "g1"}
	validBody := `{"status":"ready","instance":"relay","release_version":"1.2.3","revision":"rev","bundle_hash":"web","generation":"g1","expected_inventory":{"ready":true,"state":"ready","generation":"g1","expected":1,"observed":1}}`
	publicErr := true
	publicBody := validBody
	originalClient := readinessHTTPClient
	readinessHTTPClient = &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		if request.URL.Scheme == "https" && publicErr {
			return nil, errors.New("public")
		}
		body := validBody
		if request.URL.Scheme == "https" {
			body = publicBody
		}
		return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(body)), Header: make(http.Header), Request: request}, nil
	})}
	t.Cleanup(func() { readinessHTTPClient = originalClient })
	if err := checkJointReadinessExpectation(context.Background(), "http://127.0.0.1:8375/readyz", "https://relay.example/readyz", expected); err == nil || !strings.Contains(err.Error(), "public readiness") {
		t.Fatalf("public expectation failure = %v", err)
	}
	publicErr = false
	publicBody = strings.Replace(validBody, `"instance":"relay"`, `"instance":"other"`, 1)
	if err := checkJointReadinessExpectation(context.Background(), "http://127.0.0.1:8375/readyz", "https://relay.example/readyz", expected); err == nil || !strings.Contains(err.Error(), "identities differ") {
		t.Fatalf("expectation mismatch = %v", err)
	}
}

func TestVerifyRunningReadinessPropagatesStateLockAndEndpointFailures(t *testing.T) {
	expected := ReadinessExpectation{Managed: true, Instance: "relay", ReleaseVersion: "1.2.3", Revision: "rev", BundleHash: "web", Generation: "g1"}
	localURL := "http://127.0.0.1:8375/readyz"
	publicURL := "https://relay.example/readyz"
	if err := VerifyRunningReadiness(context.Background(), "relative", localURL, publicURL, expected); err == nil || !strings.Contains(err.Error(), "required") {
		t.Fatalf("invalid boundary = %v", err)
	}
	missingGeneration := expected
	missingGeneration.Generation = ""
	if err := VerifyRunningReadiness(context.Background(), filepath.Join(t.TempDir(), "state.json"), localURL, publicURL, missingGeneration); err == nil || !strings.Contains(err.Error(), "required") {
		t.Fatalf("managed missing generation boundary = %v", err)
	}
	ordinaryWithGeneration := expected
	ordinaryWithGeneration.Managed = false
	if err := VerifyRunningReadiness(context.Background(), filepath.Join(t.TempDir(), "state.json"), localURL, publicURL, ordinaryWithGeneration); err == nil || !strings.Contains(err.Error(), "required") {
		t.Fatalf("ordinary unexpected generation boundary = %v", err)
	}
	missing := filepath.Join(t.TempDir(), "missing.json")
	if err := VerifyRunningReadiness(context.Background(), missing, localURL, publicURL, expected); err == nil || !strings.Contains(err.Error(), "read supervisor state") {
		t.Fatalf("missing state = %v", err)
	}
	stopped := filepath.Join(t.TempDir(), "stopped.json")
	if err := writeState(stopped, State{Status: StatusStopped}); err != nil {
		t.Fatal(err)
	}
	if err := VerifyRunningReadiness(context.Background(), stopped, localURL, publicURL, expected); err == nil || !strings.Contains(err.Error(), "not running") {
		t.Fatalf("stopped state = %v", err)
	}
	badLock := filepath.Join(t.TempDir(), "bad-lock.json")
	if err := writeState(badLock, State{Status: StatusRunning}); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(badLock+".lock", 0o700); err != nil {
		t.Fatal(err)
	}
	if err := VerifyRunningReadiness(context.Background(), badLock, localURL, publicURL, expected); err == nil || !strings.Contains(err.Error(), "verify supervisor lifetime") {
		t.Fatalf("invalid lifetime lock = %v", err)
	}

	statePath := filepath.Join(t.TempDir(), "running.json")
	if err := writeState(statePath, State{Status: StatusRunning}); err != nil {
		t.Fatal(err)
	}
	lock, err := acquireLifetimeLock(statePath + ".lock")
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close()
	validBody := `{"status":"ready","instance":"relay","release_version":"1.2.3","revision":"rev","bundle_hash":"web","generation":"g1","expected_inventory":{"ready":true,"state":"ready","generation":"g1","expected":1,"observed":1}}`
	localBody, publicBody := validBody, validBody
	localErr, publicErr := false, false
	originalClient := readinessHTTPClient
	readinessHTTPClient = &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		if request.URL.Scheme == "http" && localErr {
			return nil, errors.New("local endpoint")
		}
		if request.URL.Scheme == "https" && publicErr {
			return nil, errors.New("public endpoint")
		}
		body := publicBody
		if request.URL.Scheme == "http" {
			body = localBody
		}
		return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(body)), Header: make(http.Header), Request: request}, nil
	})}
	t.Cleanup(func() { readinessHTTPClient = originalClient })

	localErr = true
	if err := VerifyRunningReadiness(context.Background(), statePath, localURL, publicURL, expected); err == nil || !strings.Contains(err.Error(), "local readiness") {
		t.Fatalf("local endpoint failure = %v", err)
	}
	localErr, publicErr = false, true
	if err := VerifyRunningReadiness(context.Background(), statePath, localURL, publicURL, expected); err == nil || !strings.Contains(err.Error(), "public readiness") {
		t.Fatalf("public endpoint failure = %v", err)
	}
	publicErr = false
	publicBody = strings.Replace(validBody, `"instance":"relay"`, `"instance":"other"`, 1)
	if err := VerifyRunningReadiness(context.Background(), statePath, localURL, publicURL, expected); err == nil || !strings.Contains(err.Error(), "identities differ") {
		t.Fatalf("endpoint identity mismatch = %v", err)
	}
}

type coverProcess struct {
	mu           sync.Mutex
	done         chan struct{}
	stopOnSignal bool
	stopOnKill   bool
	signals      int
	kills        int
	once         sync.Once
}

type latentCancelledContext struct {
	context.Context
	cancelled atomic.Bool
}

func (c *latentCancelledContext) Err() error {
	if c.cancelled.Load() {
		return context.Canceled
	}
	return nil
}

func (c *latentCancelledContext) cancel() { c.cancelled.Store(true) }

func newCoverProcess(stopOnSignal, stopOnKill bool) *coverProcess {
	return &coverProcess{done: make(chan struct{}), stopOnSignal: stopOnSignal, stopOnKill: stopOnKill}
}

func (p *coverProcess) Wait() error { <-p.done; return nil }
func (p *coverProcess) Signal(os.Signal) error {
	p.mu.Lock()
	p.signals++
	p.mu.Unlock()
	if p.stopOnSignal {
		p.once.Do(func() { close(p.done) })
	}
	return nil
}
func (p *coverProcess) Kill() error {
	p.mu.Lock()
	p.kills++
	p.mu.Unlock()
	if p.stopOnKill {
		p.once.Do(func() { close(p.done) })
	}
	return nil
}
func (p *coverProcess) Alive() bool {
	select {
	case <-p.done:
		return false
	default:
		return true
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (function roundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return function(request)
}

type errorReadCloser struct{}

func (errorReadCloser) Read([]byte) (int, error) { return 0, errors.New("read") }
func (errorReadCloser) Close() error             { return nil }

type lockFileFixture struct {
	info     os.FileInfo
	chmodErr error
	statErr  error
	closed   bool
}

func (f *lockFileFixture) Chmod(os.FileMode) error    { return f.chmodErr }
func (f *lockFileFixture) Stat() (os.FileInfo, error) { return f.info, f.statErr }
func (f *lockFileFixture) Fd() uintptr                { return 42 }
func (f *lockFileFixture) Close() error               { f.closed = true; return nil }

type lockInfoFixture struct{ mode os.FileMode }

func (f lockInfoFixture) Name() string       { return "supervisor.lock" }
func (f lockInfoFixture) Size() int64        { return 0 }
func (f lockInfoFixture) Mode() os.FileMode  { return f.mode }
func (f lockInfoFixture) ModTime() time.Time { return time.Time{} }
func (f lockInfoFixture) IsDir() bool        { return f.mode.IsDir() }
func (f lockInfoFixture) Sys() any           { return nil }

func newInjectedSupervisor(t *testing.T) *Supervisor {
	t.Helper()
	service, err := New(testConfig(t, t.TempDir()))
	if err != nil {
		t.Fatal(err)
	}
	service.readState = func(string) (State, error) { return State{}, os.ErrNotExist }
	service.writeState = func(string, State) error { return nil }
	service.openLog = func(string, int64, int) (io.Writer, error) { return io.Discard, nil }
	service.start = (&fakeStarter{failRelay: true}).Start
	service.health = func(context.Context) error { return nil }
	service.runCycle = service.runPair
	return service
}
