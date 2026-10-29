//go:build linux || darwin

package localcontrol

import (
	"context"
	"net"
	"sync"
	"testing"
	"time"
)

type installedMemoryListener struct {
	incoming chan net.Conn
	closed   chan struct{}
	once     sync.Once
}

func (l *installedMemoryListener) Accept() (net.Conn, error) {
	select {
	case c := <-l.incoming:
		return c, nil
	case <-l.closed:
		return nil, net.ErrClosed
	}
}
func (l *installedMemoryListener) Close() error { l.once.Do(func() { close(l.closed) }); return nil }
func (*installedMemoryListener) Addr() net.Addr { return nil }

func TestInstalledEndpointHandlerBoundAndJoin(t *testing.T) {
	core, _, calls := installedFixture(t, nil)
	l := &installedMemoryListener{incoming: make(chan net.Conn), closed: make(chan struct{})}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	var result error
	entered := make(chan struct{}, installedHandlers)
	// Private in-memory peer seam makes NO native peer/ownership claim.
	go func() {
		defer close(done)
		result = installedAcceptIO(ctx, l, core, l.Close, func(net.Conn) error { entered <- struct{}{}; return nil })
	}()
	var peers []net.Conn
	defer func() {
		cancel()
		l.Close()
		for _, p := range peers {
			p.Close()
		}
		<-done
	}()
	for n := 0; n < installedHandlers+1; n++ {
		a, b := net.Pipe()
		peers = append(peers, a, b)
		select {
		case l.incoming <- a:
		case <-time.After(time.Second):
			t.Fatal("accept stalled")
		}
		if n < installedHandlers {
			select {
			case <-entered:
			case <-time.After(time.Second):
				t.Fatal("handler not entered")
			}
			continue
		}
		b.SetReadDeadline(time.Now().Add(time.Second))
		var one [1]byte
		if n, e := b.Read(one[:]); n != 0 || e == nil {
			t.Fatal("excess handler retained")
		}
	}
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("whole accept/handler join")
	}
	if result != nil || core.state.ctx.Err() == nil || *calls != [5]int{} {
		t.Fatal("endpoint retirement")
	}
}
