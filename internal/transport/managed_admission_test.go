package transport

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/0cv/herdr-mobile-relay/internal/config"
)

type inertFrameConn struct {
	once   sync.Once
	closed chan struct{}
}

func newInertFrameConn() *inertFrameConn { return &inertFrameConn{closed: make(chan struct{})} }

func (c *inertFrameConn) ReadFrame(ctx context.Context) ([]byte, error) {
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-c.closed:
		return nil, ErrFrameConnClosed
	}
}

func (*inertFrameConn) WriteFrame(context.Context, []byte) error { return nil }
func (c *inertFrameConn) Close(CloseStatus, string)              { c.CloseNow() }
func (c *inertFrameConn) CloseNow()                              { c.once.Do(func() { close(c.closed) }) }
func (*inertFrameConn) Codec() FrameCodec                        { return CodecJSON }
func (*inertFrameConn) TransportName() string                    { return "test-managed" }

func TestHubAdmissionTransitionHonorsCallerDeadlineWithoutQueuedMutation(t *testing.T) {
	hub := NewHub(&config.Config{}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	hub.register.Lock()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	err := hub.SetAcceptingContext(ctx, false)
	cancel()
	hub.register.Unlock()
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("SetAcceptingContext with held registration lock = %v", err)
	}
	hub.mu.RLock()
	accepting := hub.accepting
	hub.mu.RUnlock()
	if !accepting {
		t.Fatal("expired Hub admission transition ran after returning")
	}
	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), time.Second)
	defer shutdownCancel()
	if err := hub.Shutdown(shutdownCtx); err != nil {
		t.Fatalf("hub shutdown: %v", err)
	}
}

func TestManagedHubAdmissionClosedBeforeHandshake(t *testing.T) {
	hub := NewHub(&config.Config{}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	hub.SetAccepting(false)
	conn := newInertFrameConn()
	hub.Serve(context.Background(), conn)
	select {
	case <-conn.closed:
	case <-time.After(time.Second):
		t.Fatal("closed managed admission did not reject the transport")
	}
	if got := hub.ClientCount(); got != 0 {
		t.Fatalf("client count after fail-closed admission = %d", got)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := hub.Shutdown(ctx); err != nil {
		t.Fatalf("hub shutdown: %v", err)
	}
}
