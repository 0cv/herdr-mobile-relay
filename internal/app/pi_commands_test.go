package app

import (
	"bufio"
	"context"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/0cv/herdr-mobile-relay/internal/coordinator"
	"github.com/0cv/herdr-mobile-relay/internal/herdr"
	"github.com/0cv/herdr-mobile-relay/internal/pibridge"
	"github.com/0cv/herdr-mobile-relay/internal/protocol"
	"github.com/0cv/herdr-mobile-relay/internal/transport"
)

type fixtureProcesses struct {
	pid     int
	calls   int
	replace bool
}

func (f *fixtureProcesses) PaneProcessInfo(_ context.Context, pane string) (*herdr.PaneProcessInfo, error) {
	f.calls++
	pid := f.pid
	if f.replace && f.calls > 1 {
		pid++
	}
	return &herdr.PaneProcessInfo{PaneID: pane, ShellPID: 1, ForegroundProcessGroupID: pid, ForegroundProcesses: []herdr.PaneProcess{{PID: pid}}}, nil
}

func TestSlashCatalogTargetRecheck(t *testing.T) {
	before := coordinator.AgentState{PaneID: "pane", ServerSessionID: "server", TerminalID: "terminal", Generation: 1, SessionID: "session", Cwd: "/project", Agent: "pi"}
	if !sameSlashTarget(&before, &before) || sameSlashTarget(&before, nil) {
		t.Fatal("incorrect target equality")
	}
	for _, change := range []func(*coordinator.AgentState){
		func(a *coordinator.AgentState) { a.Generation++ },
		func(a *coordinator.AgentState) { a.SessionID = "replacement" },
		func(a *coordinator.AgentState) { a.TerminalID = "replacement" },
		func(a *coordinator.AgentState) { a.PaneID = "replacement" },
		func(a *coordinator.AgentState) { a.ServerSessionID = "replacement" },
		func(a *coordinator.AgentState) { a.Cwd = "/other" },
		func(a *coordinator.AgentState) { a.Agent = "claude" },
	} {
		after := before
		change(&after)
		if sameSlashTarget(&before, &after) {
			t.Fatal("accepted a changed discovery target")
		}
	}
}

func TestReaderCanDiscoverButCannotSubmitPiCommands(t *testing.T) {
	identity := transport.AuthenticatedIdentity{Role: string(protocol.RoleReader)}
	for _, operation := range []string{"list_slash_commands", "submit_prompt", "send_keys"} {
		action, exists := protocol.ClassifyAction(operation)
		if !exists {
			t.Fatalf("unknown operation %s", operation)
		}
		err := authorizeAuthenticatedIdentity(identity, true, action, "")
		if (err == nil) != (operation == "list_slash_commands") {
			t.Fatalf("incorrect reader permission for %s", operation)
		}
	}
}

func TestForegroundRootRejectsAmbiguousAndShell(t *testing.T) {
	for _, info := range []*herdr.PaneProcessInfo{
		nil, {PaneID: "other"}, {PaneID: "pane", ShellPID: 1, ForegroundProcessGroupID: 1},
		{PaneID: "pane", ShellPID: 1, ForegroundProcessGroupID: 2, ForegroundProcesses: []herdr.PaneProcess{{PID: 3}}},
		{PaneID: "pane", ShellPID: 1, ForegroundProcessGroupID: 2, ForegroundProcesses: []herdr.PaneProcess{{PID: 2}, {PID: 2}}},
	} {
		if _, err := foregroundRoot(info, "pane"); err == nil {
			t.Fatalf("accepted ambiguous root: %+v", info)
		}
	}
}

func TestRuntimePiCatalogAcrossBunSocket(t *testing.T) {
	bun, err := exec.LookPath("bun")
	if err != nil {
		t.Skip("Bun required for cross-runtime integration")
	}
	dir, err := os.MkdirTemp("/tmp", "pi-app-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)
	socket := filepath.Join(dir, "herdr.sock")
	listener, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	instance, err := pibridge.Instance(socket)
	if err != nil {
		t.Fatal(err)
	}
	directory := pibridge.Directory(instance)
	defer os.RemoveAll(directory)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, bun, "../../tests/fixtures/pi-bridge-server.mjs", instance, "pane", "session", directory)
	output, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = cmd.Process.Signal(syscall.SIGTERM); _ = cmd.Wait() }()
	scan := bufio.NewScanner(output)
	if !scan.Scan() || scan.Text() != "ready" {
		t.Fatal("bridge did not start")
	}
	processes := &fixtureProcesses{pid: cmd.Process.Pid}
	catalog, err := runtimePiCatalog(ctx, processes, socket, "pane", "session")
	if err != nil {
		t.Fatal(err)
	}
	if catalog.Status != "available" || catalog.Metadata["/orchestrate"].Kind != "extension" || catalog.Metadata["/model"].Kind != "builtin" {
		t.Fatalf("bad merged catalog: %+v", catalog)
	}
	for _, target := range [][2]string{{"other", "session"}, {"pane", "child"}, {"pane", ""}} {
		if _, err := runtimePiCatalog(ctx, processes, socket, target[0], target[1]); err == nil {
			t.Fatal("accepted wrong identity")
		}
	}
	processes.calls = 0
	processes.replace = true
	if _, err := runtimePiCatalog(ctx, processes, socket, "pane", "session"); err == nil {
		t.Fatal("accepted replaced process")
	}
}
