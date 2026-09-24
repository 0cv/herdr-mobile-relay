package localcontrol

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TestMain keeps t.TempDir() paths short enough to bind a Unix socket. The
// gate harness points TMPDIR at a deep artifacts directory, and AF_UNIX
// pathname sockets reject paths of 108 bytes or more (including the trailing
// NUL), so t.TempDir() would otherwise fail to bind before the code under test
// runs. This mirrors the TMPDIR adjustment used by internal/coordinator tests.
func TestMain(m *testing.M) {
	probe := filepath.Join(os.TempDir(), "TestClosePreservesForeignRegularFile0000000000", "001", "control.sock")
	if len(probe) >= 108 {
		if short, err := os.MkdirTemp("/tmp", "localcontrol-"); err == nil {
			_ = os.Setenv("TMPDIR", short)
			code := m.Run()
			_ = os.RemoveAll(short)
			os.Exit(code)
		}
	}
	os.Exit(m.Run())
}

// newTestServer builds a server at path with a fixed identity and closes it
// during cleanup. Tests that call Close themselves rely on Close idempotence.
func newTestServer(t *testing.T, path string) *Server {
	t.Helper()
	server, err := New(path, "run-1", "instance-1",
		func() Status { return Status{Ready: true} },
		func() (Status, error) {
			return Status{Ready: true, InvitationArmed: true, InvitationExpiresAt: "2026-01-01T00:00:00Z"}, nil
		})
	if err != nil {
		t.Fatalf("New(%q) error = %v", path, err)
	}
	t.Cleanup(func() { _ = server.Close() })
	return server
}

func TestUnlinkOnCloseIsDisabled(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "control.sock")
	server := newTestServer(t, path)

	unixListener, ok := server.listener.(*net.UnixListener)
	if !ok {
		t.Fatalf("stored listener type = %T, want *net.UnixListener", server.listener)
	}
	// Closing the listener directly (not Server.Close) must leave the leaf in
	// place, which is only true when unlink-on-close has been disabled.
	if err := unixListener.Close(); err != nil {
		t.Fatalf("listener.Close() error = %v", err)
	}
	if _, err := os.Lstat(path); err != nil {
		t.Fatalf("socket removed by listener close: %v", err)
	}
}

func TestCloseRemovesOwnSocket(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "control.sock")
	server := newTestServer(t, path)

	if err := server.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}
	if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("socket still present after Close: err = %v", err)
	}
	if err := server.Close(); err != nil {
		t.Fatalf("second Close() error = %v", err)
	}
}

func TestClosePreservesForeignRegularFile(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "control.sock")
	server := newTestServer(t, path)

	if err := os.Remove(path); err != nil {
		t.Fatalf("remove owned socket: %v", err)
	}
	if err := os.WriteFile(path, []byte("foreign"), 0o600); err != nil {
		t.Fatalf("write foreign file: %v", err)
	}
	if err := server.Close(); err == nil {
		t.Fatal("Close() = nil, want replacement error")
	}
	data, err := os.ReadFile(path)
	if err != nil || string(data) != "foreign" {
		t.Fatalf("foreign file was disturbed: data = %q, err = %v", data, err)
	}
}

func TestClosePreservesForeignSocket(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "control.sock")
	server := newTestServer(t, path)

	foreignPath := filepath.Join(root, "foreign.sock")
	foreign, err := net.Listen("unix", foreignPath)
	if err != nil {
		t.Fatalf("listen foreign socket: %v", err)
	}
	t.Cleanup(func() { _ = foreign.Close() })
	if err := os.Rename(foreignPath, path); err != nil {
		t.Fatalf("rename foreign socket over control socket: %v", err)
	}
	if err := server.Close(); err == nil {
		t.Fatal("Close() = nil, want replacement error")
	}
	if _, err := os.Lstat(path); err != nil {
		t.Fatalf("foreign socket was removed: %v", err)
	}
	if err := os.Remove(path); err != nil {
		t.Fatalf("clean up foreign socket: %v", err)
	}
}

func TestConstructorErrorPreservesForeignPath(t *testing.T) {
	root := t.TempDir()
	regular := filepath.Join(root, "regular.sock")
	if err := os.WriteFile(regular, []byte("foreign"), 0o600); err != nil {
		t.Fatalf("write foreign regular file: %v", err)
	}
	if _, err := New(regular, "run-1", "instance-1", func() Status { return Status{} }, func() (Status, error) { return Status{}, nil }); err == nil {
		t.Fatal("regular-file collision accepted")
	}
	if data, err := os.ReadFile(regular); err != nil || string(data) != "foreign" {
		t.Fatalf("constructor disturbed foreign file: data = %q, err = %v", data, err)
	}

	target := filepath.Join(root, "target.sock")
	if err := os.WriteFile(target, []byte("target"), 0o600); err != nil {
		t.Fatalf("write symlink target: %v", err)
	}
	symlink := filepath.Join(root, "symlink.sock")
	if err := os.Symlink(target, symlink); err != nil {
		t.Fatalf("create symlink: %v", err)
	}
	if _, err := New(symlink, "run-1", "instance-1", func() Status { return Status{} }, func() (Status, error) { return Status{}, nil }); err == nil {
		t.Fatal("symlink collision accepted")
	}
	if info, err := os.Lstat(symlink); err != nil || info.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("constructor disturbed foreign symlink: info = %v, err = %v", info, err)
	}

	clean := filepath.Join(root, "clean.sock")
	server, err := New(clean, "run-1", "instance-1", func() Status { return Status{} }, func() (Status, error) { return Status{}, nil })
	if err != nil {
		t.Fatalf("New(%q) error = %v", clean, err)
	}
	if err := server.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}
	if _, err := os.Lstat(clean); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("socket left behind after New/Close: err = %v", err)
	}
}

func TestControlFramesBoundedAndIdentityChecked(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "control.sock")
	server, err := New(path, "run-1", "instance-1",
		func() Status { return Status{Ready: true} },
		func() (Status, error) { return Status{Ready: true}, nil })
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = server.Run(ctx) }()
	t.Cleanup(func() {
		cancel()
		_ = server.Close()
	})

	deadline := time.Now().Add(2 * time.Second)
	for {
		if _, err := os.Lstat(path); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("control socket never appeared")
		}
		time.Sleep(5 * time.Millisecond)
	}

	// An oversized frame must be rejected with an error response, not a hang.
	connection, err := net.DialTimeout("unix", path, time.Second)
	if err != nil {
		t.Fatalf("dial control socket: %v", err)
	}
	if err := connection.SetDeadline(time.Now().Add(IOTimeout + time.Second)); err != nil {
		t.Fatalf("set connection deadline: %v", err)
	}
	oversized := make([]byte, MaxRequestBytes+1024)
	for i := range oversized {
		oversized[i] = 'a'
	}
	oversized = append(oversized, '\n')
	if _, err := connection.Write(oversized); err != nil {
		t.Fatalf("write oversized frame: %v", err)
	}
	reader := bufio.NewReader(io.LimitReader(connection, MaxRequestBytes+1))
	line, err := reader.ReadBytes('\n')
	_ = connection.Close()
	if err != nil {
		t.Fatalf("read oversized response: %v", err)
	}
	var reply Response
	if err := json.Unmarshal([]byte(line), &reply); err != nil {
		t.Fatalf("decode oversized response %q: %v", line, err)
	}
	if reply.OK || reply.Error == "" {
		t.Fatalf("oversized frame response = %#v, want an error", reply)
	}

	// Wrong run identity must be refused by the real server.
	if _, err := Request(context.Background(), path, "status", "wrong", "instance-1"); err == nil {
		t.Fatal("wrong run identity accepted")
	}
	// Wrong instance identity must be refused by the real server.
	if _, err := Request(context.Background(), path, "status", "run-1", "wrong"); err == nil {
		t.Fatal("wrong instance identity accepted")
	}

	// A valid status request must still succeed within the deadline.
	requestCtx, requestCancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer requestCancel()
	response, err := Request(requestCtx, path, "status", "run-1", "instance-1")
	if err != nil || !response.OK || !response.Ready {
		t.Fatalf("valid status response = %#v, err = %v", response, err)
	}
}
