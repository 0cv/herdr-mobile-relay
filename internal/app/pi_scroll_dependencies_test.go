package app

import (
	"context"
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"

	"github.com/0cv/herdr-mobile-relay/internal/config"
	"github.com/0cv/herdr-mobile-relay/internal/coordinator"
	"github.com/0cv/herdr-mobile-relay/internal/herdr"
	"github.com/0cv/herdr-mobile-relay/internal/ownedproc"
	"github.com/0cv/herdr-mobile-relay/internal/panesize"
)

type paneHelperWiringCall struct {
	name string
	args []string
}

type recordingPaneHelperWiringRunner struct {
	calls []paneHelperWiringCall
	err   error
}

func (r *recordingPaneHelperWiringRunner) Output(_ context.Context, name string, args ...string) ([]byte, error) {
	r.calls = append(r.calls, paneHelperWiringCall{name: name, args: slices.Clone(args)})
	return nil, r.err
}

type paneHelperWiringProvider struct{}

func (paneHelperWiringProvider) PaneProcessInfo(_ context.Context, pane string) (*herdr.PaneProcessInfo, error) {
	return &herdr.PaneProcessInfo{
		PaneID: pane, ForegroundProcessGroupID: 123,
		ForegroundProcesses: []herdr.PaneProcess{{PID: 123}},
	}, nil
}

func piDependencyFunctionSource(t *testing.T, relativePath, name string) string {
	t.Helper()
	_, source, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("test source location unavailable")
	}
	path := filepath.Join(filepath.Dir(source), relativePath)
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	positions := token.NewFileSet()
	file, err := parser.ParseFile(positions, path, data, 0)
	if err != nil {
		t.Fatal(err)
	}
	for _, declaration := range file.Decls {
		function, ok := declaration.(*ast.FuncDecl)
		if ok && function.Name.Name == name && function.Body != nil {
			return string(data[positions.Position(function.Body.Pos()).Offset:positions.Position(function.Body.End()).Offset])
		}
	}
	t.Fatalf("function %s missing from %s", name, relativePath)
	return ""
}

func requirePiDependencySource(t *testing.T, source string, required ...string) {
	t.Helper()
	for _, text := range required {
		if !strings.Contains(source, text) {
			t.Fatalf("construction source missing %q", text)
		}
	}
}

func TestPiScrollProductionConstructionDefaultsSource(t *testing.T) {
	requirePiDependencySource(t, piDependencyFunctionSource(t, "../panesize/manager.go", "NewManager"),
		"return NewManagerWithRunner(provider, nil, logger)")
	requirePiDependencySource(t, piDependencyFunctionSource(t, "../panesize/manager.go", "NewManagerWithRunner"),
		"if runner == nil", "runner = execCommandRunner{}",
		"newManager(provider, runner, runtime.GOOS, LeaseTTL, ReleaseGrace, time.Now, logger)")
	requirePiDependencySource(t, piDependencyFunctionSource(t, "../panesize/manager.go", "Output"),
		"childenv.CommandContext(ctx, name, args...).Output()")
	requirePiDependencySource(t, piDependencyFunctionSource(t, "../coordinator/scroll.go", "EnablePiScroll"),
		"d.enablePiScroll(socket, nil)")
	requirePiDependencySource(t, piDependencyFunctionSource(t, "server.go", "New"),
		"newServerWithDependencies(cfg, version, revision, logger, serverDependencies{speechLanguages: speech.Languages})")
}

func TestPiScrollManagerInjectedRunnerWiringWithoutGeometry(t *testing.T) {
	first := &recordingPaneHelperWiringRunner{err: errors.New("stop before geometry")}
	second := &recordingPaneHelperWiringRunner{err: errors.New("independent instance")}
	manager := panesize.NewManagerWithRunner(paneHelperWiringProvider{}, first, nil)
	other := panesize.NewManagerWithRunner(paneHelperWiringProvider{}, second, nil)
	if _, _, err := manager.Acquire(context.Background(), "owner", "pane", 80, 24); !errors.Is(err, panesize.ErrTTYUnavailable) {
		t.Fatalf("injected helper failure = %v", err)
	}
	if len(first.calls) != 1 || first.calls[0].name != "ps" || !slices.Equal(first.calls[0].args, []string{"-o", "tty=", "-p", "123"}) {
		t.Fatalf("Manager runner wiring = %+v", first.calls)
	}
	if len(second.calls) != 0 {
		t.Fatal("one Manager used another instance's runner")
	}
	if _, _, err := other.Acquire(context.Background(), "owner", "pane", 80, 24); !errors.Is(err, panesize.ErrTTYUnavailable) || len(second.calls) != 1 {
		t.Fatalf("second Manager runner wiring = %+v, %v", second.calls, err)
	}
	if _, active := manager.ActiveColumns("pane"); active {
		t.Fatal("wiring failure fabricated a lease")
	}
}

func TestPiScrollManagerNilRunnerMeansProductionDefault(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	for _, manager := range []*panesize.Manager{
		panesize.NewManager(paneHelperWiringProvider{}, nil),
		panesize.NewManagerWithRunner(paneHelperWiringProvider{}, nil, nil),
	} {
		if _, _, err := manager.Acquire(ctx, "owner", "pane", 80, 24); !errors.Is(err, panesize.ErrLeaseOwnerGone) {
			t.Fatalf("default Manager cancellation = %v", err)
		}
		if err := manager.Shutdown(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
}

func TestPiScrollReaderInjectedRunnerWiringWithoutGeometry(t *testing.T) {
	runner := &recordingPaneHelperWiringRunner{err: errors.New("stop before geometry")}
	grid, err := panesize.NewReader(runner, "linux").Read(context.Background(), 123)
	if !errors.Is(err, runner.err) || grid != (panesize.Grid{}) {
		t.Fatalf("Reader wiring produced geometry: %+v, %v", grid, err)
	}
	if len(runner.calls) != 1 || runner.calls[0].name != "ps" || !slices.Equal(runner.calls[0].args, []string{"-o", "tty=", "-p", "123"}) {
		t.Fatalf("Reader runner wiring = %+v", runner.calls)
	}
}

func TestPiScrollDispatcherConstructionAndReplacementRefusal(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	for _, construction := range []string{"legacy-default", "constructor-default", "injected"} {
		state := coordinator.NewState(logger)
		state.CommitInventory([]*coordinator.AgentState{{PaneID: "pane", TerminalID: "terminal", SessionID: "session", Agent: "pi"}}, 0)
		client := herdr.NewClient("/must-not-run", "/must-not-connect")
		runner := &recordingPaneHelperWiringRunner{err: errors.New("must not measure")}
		var dispatcher *coordinator.Dispatcher
		switch construction {
		case "injected":
			dispatcher = coordinator.NewDispatcherWithPiScroll(client, state, nil, logger, "/must-not-connect", runner)
		case "constructor-default":
			dispatcher = coordinator.NewDispatcherWithPiScroll(client, state, nil, logger, "/must-not-connect", nil)
		default:
			dispatcher = coordinator.NewDispatcher(client, state, nil, logger)
			dispatcher.EnablePiScroll("/must-not-connect")
		}
		before := dispatcher.ScrollState(context.Background(), "pane", "owner", false)
		if before == nil || before.Available || before.Rows != 0 || before.Columns != 0 {
			t.Fatalf("constructor fabricated geometry: %+v", before)
		}
		dispatcher.EnablePiScroll("/must-not-replace")
		after := dispatcher.ScrollState(context.Background(), "pane", "owner", false)
		if after == nil || after.Revision != before.Revision || len(runner.calls) != 0 {
			t.Fatal("scroll construction was replaced or unexpectedly measured")
		}
		if err := dispatcher.Close(context.Background()); err != nil {
			t.Fatal(err)
		}
		if err := client.Close(); err != nil {
			t.Fatal(err)
		}
	}
}

func TestPiScrollDispatcherReaderBindingSource(t *testing.T) {
	requirePiDependencySource(t, piDependencyFunctionSource(t, "../coordinator/scroll.go", "NewDispatcherWithPiScroll"),
		"d := NewDispatcher(client, state, journal, logger)", "d.enablePiScroll(socket, runner)", "return d")
	source := piDependencyFunctionSource(t, "../coordinator/scroll.go", "enablePiScroll")
	requirePiDependencySource(t, source, "d.readMu.Lock()", "defer d.readMu.Unlock()",
		"if d.piResolver != nil {\n\t\treturn\n\t}", "panesize.NewReader(runner, \"\")",
		"reader.Observe =", "Geometry: reader.Read", "Supported: d.herdr.SupportsPiScrollSocket",
		"Catalog: pibridge.Query", "Observe: func(metrics piscroll.ResolveMetrics)", "d.scroll = piscroll.New(d.piResolver)")
	if strings.Index(source, "if d.piResolver != nil") > strings.Index(source, "panesize.NewReader") {
		t.Fatal("replacement guard runs after geometry construction")
	}
}

func newPiDependencyWiringServer(t *testing.T, dependencies serverDependencies) *Server {
	t.Helper()
	cfg := &config.Config{CacheDir: t.TempDir(), HerdrBin: "/must-not-run", SocketPath: "/must-not-connect"}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	server := newServerWithDependencies(cfg, "wiring-test", "source-only", logger, dependencies)
	if server.initErr != nil {
		t.Fatal(server.initErr)
	}
	t.Cleanup(func() {
		if server.dispatcher != nil {
			_ = server.dispatcher.Close(context.Background())
		}
		if server.conversationB != nil {
			_ = server.conversationB.Close()
		}
		if server.uploadM != nil {
			_ = server.uploadM.Close()
		}
		_ = server.herdrC.Close()
	})
	return server
}

func TestPiScrollServerCapturedDependenciesAndConstructionLifecycle(t *testing.T) {
	leaseRunner := &recordingPaneHelperWiringRunner{err: errors.New("lease wiring only")}
	geometryRunner := &recordingPaneHelperWiringRunner{err: errors.New("geometry wiring only")}
	discoveries := 0
	dependencies := serverDependencies{
		paneSizeRunner: leaseRunner, geometryRunner: geometryRunner,
		speechLanguages: func() []string {
			discoveries++
			return []string{"wiring-only"}
		},
	}
	server := newPiDependencyWiringServer(t, dependencies)
	dependencies.geometryRunner = &recordingPaneHelperWiringRunner{err: errors.New("replacement must not be captured")}
	dependencies.speechLanguages = func() []string {
		t.Fatal("post-construction discovery used")
		return nil
	}
	if discoveries != 1 || !slices.Equal(server.speechLanguages, []string{"wiring-only"}) || server.paneSizeM == nil || server.dispatcher != nil {
		t.Fatal("Server dependencies were not fixed at construction")
	}
	if err := server.initializeDispatcher(); err == nil {
		t.Fatal("Dispatcher built outside the Run construction lifecycle")
	}
	server.state.CommitInventory([]*coordinator.AgentState{{PaneID: "pane", TerminalID: "terminal", SessionID: "session", Agent: "pi"}}, 0)
	if err := server.beginRun(); err != nil {
		t.Fatal(err)
	}
	if err := server.initializeDispatcher(); err != nil {
		t.Fatal(err)
	}
	original := server.dispatcher
	if original.ScrollState(context.Background(), "pane", "owner", false) == nil {
		t.Fatal("Server Dispatcher was not wired to the actual State")
	}
	if err := server.initializeDispatcher(); err == nil || server.dispatcher != original {
		t.Fatal("Dispatcher replacement was allowed")
	}
	if err := server.beginRun(); err == nil {
		t.Fatal("a second Run could replace the serving Dispatcher")
	}
	if discoveries != 1 || len(leaseRunner.calls) != 0 || len(geometryRunner.calls) != 0 {
		t.Fatal("construction unexpectedly discovered speech or measured geometry")
	}
}

func TestPiScrollServerRunWiringBeforeServingSource(t *testing.T) {
	constructor := piDependencyFunctionSource(t, "server.go", "newServerWithDependencies")
	requirePiDependencySource(t, constructor,
		"geometryRunner, socket := dependencies.geometryRunner, cfg.SocketPath",
		"panesize.NewManagerWithRunner(herdrClient, dependencies.paneSizeRunner, logger)",
		"coordinator.NewDispatcherWithPiScroll(herdrClient, state, journal, logger, socket, geometryRunner)",
		"speechLanguages := dependencies.speechLanguages()")
	if strings.Contains(constructor, "speech.Languages()") {
		t.Fatal("injected construction bypasses optional discovery dependency")
	}
	run := piDependencyFunctionSource(t, "server.go", "Run")
	requirePiDependencySource(t, run, "s.beginRun()", "s.initializeDispatcher()")
	for _, serving := range []string{"s.profiles.Profiles()", "s.hub.SetOnConnect", "coordinator.NewUDPListener", "net.Listen", "srv.Serve"} {
		if position := strings.Index(run, serving); position < 0 || position < strings.Index(run, "s.initializeDispatcher()") {
			t.Fatalf("Dispatcher construction does not precede %s", serving)
		}
	}
	if strings.Contains(run, "coordinator.NewDispatcher(") || strings.Contains(run, ".EnablePiScroll(") {
		t.Fatal("Run bypasses the captured Dispatcher constructor")
	}
	requirePiDependencySource(t, piDependencyFunctionSource(t, "server.go", "initializeDispatcher"),
		"if !s.runStarted || s.dispatcher != nil", "s.buildDispatcher(s.journal)",
		"dispatcher.SetProfiles(s.profiles)", "dispatcher.SetBroadcast(s.broadcastCommitted)",
		"dispatcher.SetWakePoll", "s.dispatcher = dispatcher")
}

func TestPiScrollServerMissingDiscoveryRefusedBeforeConstruction(t *testing.T) {
	server := newServerWithDependencies(nil, "", "", nil, serverDependencies{})
	if server.initErr == nil || server.paneSizeM != nil || server.herdrC != nil || server.buildDispatcher != nil {
		t.Fatal("missing optional discovery function did not fail before construction")
	}
	if err := server.Run(context.Background()); err != server.initErr {
		t.Fatalf("invalid dependency Run error = %v", err)
	}
}

func TestPiScrollFixtureConstructionDisablesOnlyOptionalDiscovery(t *testing.T) {
	dependencies := piScrollFixtureDependencies()
	if _, ok := dependencies.paneSizeRunner.(fixturePaneCommandRunner); !ok {
		t.Fatal("fixture lease runner is not the bounded real-command runner")
	}
	if _, ok := dependencies.geometryRunner.(fixturePaneCommandRunner); !ok {
		t.Fatal("fixture geometry runner is not the bounded real-command runner")
	}
	server := newPiDependencyWiringServer(t, dependencies)
	if len(server.speechLanguages) != 0 || server.dispatcher != nil {
		t.Fatal("fixture unexpectedly enumerated speech or started serving")
	}
	defaultRunners := newPiDependencyWiringServer(t, serverDependencies{speechLanguages: func() []string { return nil }})
	if defaultRunners.paneSizeM == nil || defaultRunners.buildDispatcher == nil {
		t.Fatal("nil runners did not retain the production construction defaults")
	}
}

func TestPiScrollFixtureCommandOwnershipAndWaitBoundsSource(t *testing.T) {
	source := piDependencyFunctionSource(t, "pi_scroll_fixture_test.go", "runFixtureCommand")
	requirePiDependencySource(t, source, "context.WithTimeout(ctx, 5*time.Second)",
		"command := childenv.Command(name, args...)", "command.WaitDelay = fixtureCommandWait",
		"ownedproc.New(inspectCtx, fixtureCommandInspector{process: command.Process}, command.Process.Pid, os.Getpid())",
		"if ownershipErr != nil", "registry.Signal(inspectCtx, command.Process.Pid, force)",
		"waitFixtureCommand(wait)", "stdout.snapshot()", "stderr.snapshot()", "ctx.Err()")
	for _, forbidden := range []string{"CommandContext", ".Kill(", "registry.Cancel(", "registry.Sample(", "registry.Finish("} {
		if strings.Contains(source, forbidden) {
			t.Fatalf("fixture helper added an unowned or descendant lifecycle path: %s", forbidden)
		}
	}
	requirePiDependencySource(t, piDependencyFunctionSource(t, "pi_scroll_fixture_test.go", "Signal"),
		"pid <= 0 || pid != i.process.Pid", "return i.process.Signal(signal)")
	requirePiDependencySource(t, piDependencyFunctionSource(t, "pi_scroll_fixture_test.go", "waitFixtureCommand"),
		"time.NewTimer(fixtureCommandWait)", "return ownedproc.ErrIncomplete, false")
}

func TestPiScrollFixtureCommandBoundsWithoutProcesses(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	stream := &fixtureCommandStream{cancel: cancel}
	n, err := stream.Write(make([]byte, fixtureCommandStreamLimit+1))
	if n != fixtureCommandStreamLimit || !errors.Is(err, errFixtureCommandStreamLimit) || ctx.Err() == nil {
		t.Fatal("producer overflow was not capped and cancelled")
	}
	before, err := stream.snapshot()
	if len(before) != fixtureCommandStreamLimit || !errors.Is(err, errFixtureCommandStreamLimit) {
		t.Fatal("overflow evidence was lost")
	}
	_, _ = stream.Write([]byte("late"))
	after, _ := stream.snapshot()
	if !slices.Equal(before, after) {
		t.Fatal("frozen stream changed after command completion")
	}
	if _, err := (fixturePaneCommandRunner{}).Output(context.Background(), "unsupported-helper"); err == nil {
		t.Fatal("unrelated fixture helper accepted")
	}
	if _, err := runFixtureCommand(context.Background(), "/must-not-run", strings.Repeat("x", fixtureCommandStreamLimit+1)); !errors.Is(err, errFixtureCommandStreamLimit) {
		t.Fatal("uncapped verifier input accepted")
	}
	inspector := fixtureCommandInspector{process: &os.Process{Pid: 123}}
	if err := inspector.Signal(-123, true); !errors.Is(err, ownedproc.ErrIncomplete) {
		t.Fatal("group signal not refused")
	}
	if err := inspector.Signal(124, false); !errors.Is(err, ownedproc.ErrIncomplete) {
		t.Fatal("non-child signal not refused")
	}
}
