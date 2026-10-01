package herdr

import (
	"context"
	"errors"
	"net"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"
)

func TestSocketDispatchCheckAfterConnectionLock(t *testing.T) {
	clientConn, serverConn := net.Pipe()
	defer serverConn.Close()
	client := newSocketAPIClient("unused")
	client.conn = clientConn
	defer client.close()
	var revoked atomic.Bool
	denied := errors.New("revoked")
	ctx := WithDispatchCheck(context.Background(), func() error {
		if revoked.Load() {
			return denied
		}
		return nil
	})
	client.mu.Lock()
	result := make(chan error, 1)
	go func() { result <- client.tabMove(ctx, "tab", 0) }()
	revoked.Store(true)
	client.mu.Unlock()
	select {
	case err := <-result:
		if !errors.Is(err, denied) || !errors.Is(err, ErrNotStarted) {
			t.Fatalf("unexpected error: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("mutation attempted to write after revocation")
	}
}

// The retry re-evaluates the guard, but the first attempt already put request
// bytes on the wire, so a denial on the retry must not downgrade the outcome
// to "not started".
func TestSocketDispatchDenialAfterWriteStaysDispatched(t *testing.T) {
	path := filepath.Join(t.TempDir(), "api.sock")
	listener, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	read := make(chan int, 2)
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			_ = conn.SetReadDeadline(time.Now().Add(time.Second))
			buffer := make([]byte, 512)
			n, _ := conn.Read(buffer)
			read <- n
			conn.Close()
		}
	}()
	denied := errors.New("revoked")
	var calls atomic.Int32
	ctx := WithDispatchCheck(context.Background(), func() error {
		if calls.Add(1) == 1 {
			return nil
		}
		return denied
	})
	client := newSocketAPIClient(path)
	defer client.close()
	err = client.moveRequest(ctx, "tab.move", "tab_list", nil)
	if !errors.Is(err, ErrDispatchedUnknown) {
		t.Fatalf("write followed by a denied retry lost the dispatch boundary: %v", err)
	}
	if !errors.Is(err, denied) {
		t.Fatalf("denial reason was dropped: %v", err)
	}
	if got := calls.Load(); got != 2 {
		t.Fatalf("guard evaluated %d times, want one per attempt", got)
	}
	select {
	case n := <-read:
		if n == 0 {
			t.Fatal("first attempt wrote nothing; the test no longer covers the retry path")
		}
	case <-time.After(time.Second):
		t.Fatal("server never received the first attempt")
	}
	select {
	case n := <-read:
		if n != 0 {
			t.Fatalf("denied retry wrote %d bytes", n)
		}
	case <-time.After(time.Second):
	}
}

func TestSocketDispatchDenialWritesNothing(t *testing.T) {
	for _, method := range []string{"workspace.close", "workspace.move", "tab.move", "pane.input"} {
		t.Run(method, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "api.sock")
			listener, err := net.Listen("unix", path)
			if err != nil {
				t.Fatal(err)
			}
			defer listener.Close()
			received := make(chan int, 1)
			go func() {
				conn, err := listener.Accept()
				if err != nil {
					received <- -1
					return
				}
				defer conn.Close()
				_ = conn.SetReadDeadline(time.Now().Add(time.Second))
				b := make([]byte, 1)
				n, _ := conn.Read(b)
				received <- n
			}()
			denied := errors.New("revoked")
			calls := 0
			ctx := WithDispatchCheck(context.Background(), func() error { calls++; return denied })
			client := newSocketAPIClient(path)
			defer client.close()
			if method == "workspace.move" || method == "tab.move" {
				err = client.moveRequest(ctx, method, "unused", nil)
			} else {
				err = client.requestResult(ctx, method, nil, "unused", nil)
			}
			if !errors.Is(err, denied) || !errors.Is(err, ErrNotStarted) {
				t.Fatalf("unexpected error: %v", err)
			}
			if calls != 1 {
				t.Fatalf("guard called %d times", calls)
			}
			if n := <-received; n != 0 {
				t.Fatalf("received %d bytes", n)
			}
		})
	}
}
