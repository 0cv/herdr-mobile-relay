package herdr

import (
	"context"
	"net"
	"runtime"
	"testing"
	"time"
)

type closeSignalConn struct {
	net.Conn
	closed chan struct{}
}

func (c *closeSignalConn) Close() error {
	close(c.closed)
	return nil
}

func TestCloseOnContextDoneStopsBeforeCancellation(t *testing.T) {
	previous := runtime.GOMAXPROCS(1)
	defer runtime.GOMAXPROCS(previous)

	for range 1000 {
		ctx, cancel := context.WithCancel(context.Background())
		conn := &closeSignalConn{closed: make(chan struct{})}
		stop := closeOnContextDone(ctx, conn)
		stop()
		cancel()
		runtime.Gosched()
		select {
		case <-conn.closed:
			t.Fatal("stopped watcher closed the connection after cancellation")
		default:
		}
	}
}

func TestCloseOnContextDoneClosesOnCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	conn := &closeSignalConn{closed: make(chan struct{})}
	stop := closeOnContextDone(ctx, conn)
	defer stop()

	cancel()
	select {
	case <-conn.closed:
	case <-time.After(5 * time.Second):
		t.Fatal("watcher did not close the connection after cancellation")
	}
}
