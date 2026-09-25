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

func TestHubAdmissionRevocationClosesConnectedClientsWithoutRegistrationBarrier(t *testing.T) {
	hub := NewHub(&config.Config{}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	conn := newInertFrameConn()
	serveDone := make(chan struct{})
	go func() {
		defer close(serveDone)
		hub.Serve(context.Background(), conn)
	}()
	deadline := time.Now().Add(time.Second)
	for hub.ClientCount() == 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if got := hub.ClientCount(); got != 1 {
		t.Fatalf("connected clients before revocation = %d, want 1", got)
	}

	// A normal admission transition can wait behind this lock; the emergency
	// retirement fence must still close authenticated existing sessions now.
	hub.register.Lock()
	hub.RevokeAdmission()
	select {
	case <-conn.closed:
	case <-time.After(time.Second):
		hub.register.Unlock()
		t.Fatal("admission revocation left an existing client connected")
	}
	if got := hub.ClientCount(); got != 0 {
		hub.register.Unlock()
		t.Fatalf("connected clients after revocation = %d, want 0", got)
	}
	hub.register.Unlock()
	if err := hub.SetAcceptingContext(context.Background(), true); err == nil {
		t.Fatal("permanently revoked Hub admission was reopened")
	}
	select {
	case <-serveDone:
	case <-time.After(time.Second):
		t.Fatal("revoked connection did not finish")
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
