//go:build darwin || linux

package herdr

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/0cv/herdr-mobile-relay/internal/childenv"
	"github.com/0cv/herdr-mobile-relay/internal/ownedproc"
)

func TestOwnedCLIHelper(t *testing.T) {
	role := os.Getenv("HERDR_TEST_OWNED_ROLE")
	if role == "" {
		return
	}
	signal.Ignore(syscall.SIGTERM)
	inspector := ownedproc.Native{}
	identity, err := inspector.Lookup(context.Background(), os.Getpid())
	if err != nil {
		t.Fatal(err)
	}
	if role == "leader" {
		binary, err := os.Executable()
		if err != nil {
			t.Fatal(err)
		}
		child := childenv.Command(binary, "-test.run=^TestOwnedCLIHelper$")
		var environment []string
		for _, value := range child.Env {
			if !strings.HasPrefix(value, "HERDR_TEST_OWNED_ROLE=") {
				environment = append(environment, value)
			}
		}
		child.Env = append(environment, "HERDR_TEST_OWNED_ROLE=child")
		child.Stdout = os.Stdout
		child.Stderr = os.Stderr
		if err := child.Start(); err != nil {
			t.Fatal(err)
		}
		go func() { _ = child.Wait() }()
	}
	data, err := json.Marshal(identity)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(os.Getenv("HERDR_TEST_OWNED_DIR"), role+".json")
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	fmt.Printf("owned fixture %s ready\n", role)
	time.Sleep(12 * time.Second)
}

func TestCancelledCommandCleansRecordedIncarnations(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HERDR_TEST_OWNED_DIR", dir)
	t.Setenv("HERDR_TEST_OWNED_ROLE", "leader")
	binary, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	c := NewClient(binary, filepath.Join(dir, "socket"))
	roots := make(chan ownedproc.Process, 1)
	registered := make(chan ownedproc.Process, ownedproc.MaxProcesses)
	c.commandStarted = func(r *ownedproc.Registry) {
		root, _ := r.IdentityFromRoot()
		roots <- root
		r.Registered = func(p ownedproc.Process) { registered <- p }
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	var commandErr error
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(16 * time.Second):
			t.Error("owned fixture cleanup/join incomplete")
		}
	})
	go func() { _, commandErr = c.run(ctx, 10*time.Second, "-test.run=^TestOwnedCLIHelper$"); close(done) }()
	var root ownedproc.Process
	select {
	case root = <-roots:
	case <-time.After(2 * time.Second):
		t.Fatal("root registration missing")
	}
	var child ownedproc.Process
	select {
	case child = <-registered:
	case <-time.After(2 * time.Second):
		t.Fatal("child registration missing")
	}
	if root.Birth == "" || child.Birth == "" || child.Parent != root.PID {
		t.Fatalf("proven edge missing: root=%+v child=%+v", root, child)
	}
	for _, expected := range []struct {
		role string
		p    ownedproc.Process
	}{{"leader", root}, {"child", child}} {
		deadline := time.Now().Add(time.Second)
		var published ownedproc.Process
		for time.Now().Before(deadline) {
			data, err := os.ReadFile(filepath.Join(dir, expected.role+".json"))
			if err == nil && json.Unmarshal(data, &published) == nil {
				break
			}
			time.Sleep(5 * time.Millisecond)
		}
		if published != expected.p {
			t.Fatalf("published identity does not match owned registry: %+v != %+v", published, expected.p)
		}
	}
	start := time.Now()
	cancel()
	select {
	case <-done:
	case <-time.After(8 * time.Second):
		t.Fatal("bounded cancellation failed")
	}
	if !errors.Is(commandErr, context.Canceled) || !errors.Is(commandErr, ErrDispatchedUnknown) || errors.Is(commandErr, ErrNotStarted) || errors.Is(commandErr, ownedproc.ErrIncomplete) {
		t.Fatalf("cancellation outcome: %v", commandErr)
	}
	for _, expected := range []ownedproc.Process{root, child} {
		current, err := (ownedproc.Native{}).Lookup(context.Background(), expected.PID)
		if err != nil && !errors.Is(err, ownedproc.ErrAbsent) {
			t.Fatal(err)
		}
		if err == nil && current.Birth == expected.Birth && !current.Exited {
			t.Fatalf("recorded incarnation survives: %+v", current)
		}
	}
	t.Logf("identity-checked cleanup: root=%+v child=%+v elapsed=%s; inherited stdout/stderr held until child exit", root, child, time.Since(start))
}
