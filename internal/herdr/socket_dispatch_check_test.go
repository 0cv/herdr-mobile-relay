package herdr

import (
	"context"
	"errors"
	"net"
	"path/filepath"
	"testing"
	"time"
)

func TestSocketDispatchDenialWritesNothing(t *testing.T) {
	for _, method := range []string{"workspace.close", "workspace.move", "tab.move", "pane.input"} {
		t.Run(method, func(t *testing.T) {
			path := filepath.Join(testSocketDir(t), "api.sock")
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
			err = client.requestResult(ctx, method, nil, "unused", nil)
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
