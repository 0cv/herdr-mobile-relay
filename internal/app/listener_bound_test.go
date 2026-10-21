package app

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/0cv/herdr-mobile-relay/internal/config"
)

// idleListener stands in for a bound TCP listener without opening a socket.
type idleListener struct {
	closed chan struct{}
	once   sync.Once
}

func (l *idleListener) Accept() (net.Conn, error) {
	<-l.closed
	return nil, net.ErrClosed
}

func (l *idleListener) Close() error {
	l.once.Do(func() { close(l.closed) })
	return nil
}

func (l *idleListener) Addr() net.Addr { return &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1)} }

func newListenerBoundServer(t *testing.T) *Server {
	t.Helper()
	root := t.TempDir()
	webRoot := filepath.Join(root, "web")
	if err := os.MkdirAll(webRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(webRoot, "index.html"), []byte("<html></html>"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{
		Host:        "127.0.0.1",
		Port:        18375,
		InstanceID:  "listener-bound-test",
		WebRoot:     webRoot,
		RuntimeDir:  filepath.Join(root, "runtime"),
		CacheDir:    filepath.Join(root, "cache"),
		ConfigHome:  filepath.Join(root, "config"),
		ReleaseRoot: filepath.Join(root, "release"),
	}
	return New(cfg, "0.9.0", "abc123", slog.New(slog.NewTextHandler(io.Discard, nil)))
}

// A relay without a managed owner owns its instance only once Run holds the
// listener, so a duplicate start whose bind fails must never look bound.
func TestListenerBoundStaysOpenWhenTheBindFails(t *testing.T) {
	s := newListenerBoundServer(t)
	bound := s.ListenerBound()
	if bound == nil {
		t.Fatal("a constructed server has no listener-bound signal")
	}
	bindErr := errors.New("address already in use")
	s.listen = func(string, string) (net.Listener, error) { return nil, bindErr }
	if err := s.Run(t.Context()); !errors.Is(err, bindErr) {
		t.Fatalf("Run = %v, want the bind failure", err)
	}
	select {
	case <-bound:
		t.Fatal("the signal closed although the listener was never bound")
	default:
	}
}

func TestListenerBoundClosesOnlyOnceRunHoldsTheListener(t *testing.T) {
	s := newListenerBoundServer(t)
	bound := s.ListenerBound()
	listener := &idleListener{closed: make(chan struct{})}
	s.listen = func(network, address string) (net.Listener, error) {
		select {
		case <-bound:
			t.Error("the signal closed before the listener was bound")
		default:
		}
		return listener, nil
	}
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	joined := false
	defer func() {
		cancel()
		_ = listener.Close()
		if !joined {
			select {
			case <-done:
			case <-time.After(15 * time.Second):
				t.Error("Run did not stop after cancellation and listener close")
			}
		}
	}()
	go func() { done <- s.Run(ctx) }()
	select {
	case <-bound:
	case err := <-done:
		joined = true
		t.Fatalf("Run returned before binding the listener: %v", err)
	case <-time.After(15 * time.Second):
		t.Fatal("the signal did not close after the listener was bound")
	}
}

func TestListenerBoundIsNilForAnAbsentServer(t *testing.T) {
	var s *Server
	if s.ListenerBound() != nil {
		t.Fatal("a nil server reports a bind signal")
	}
	if (&Server{}).ListenerBound() != nil {
		t.Fatal("a server built without a constructor reports a bind signal")
	}
}
