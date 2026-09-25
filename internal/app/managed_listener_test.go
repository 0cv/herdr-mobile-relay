//go:build !herdr_tailscale_test

package app

import (
	"errors"
	"net"
	"net/http"
	"sync"
	"testing"
	"time"
)

type failOnceManagedListener struct {
	net.Listener
	failed bool
}

func (l *failOnceManagedListener) Accept() (net.Conn, error) {
	if !l.failed {
		l.failed = true
		return nil, errors.New("injected accept failure")
	}
	return l.Listener.Accept()
}

func TestRetainedListenerHTTPServerCloseUnblocksAcceptAndKeepsPort(t *testing.T) {
	underlying, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	retained := retainTCPListener(underlying)
	t.Cleanup(func() { _ = retained.Close() })
	address := retained.Addr().String()
	view := retained.ServeListener()
	served := make(chan struct{})
	server := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		close(served)
		w.WriteHeader(http.StatusNoContent)
	})}
	serveDone := make(chan error, 1)
	go func() { serveDone <- server.Serve(view) }()

	client := &http.Client{Timeout: time.Second}
	response, err := client.Get("http://" + address + "/")
	if err != nil {
		t.Fatal(err)
	}
	_ = response.Body.Close()
	if response.StatusCode != http.StatusNoContent {
		t.Fatalf("backend status = %d, want %d", response.StatusCode, http.StatusNoContent)
	}
	select {
	case <-served:
	case <-time.After(time.Second):
		t.Fatal("HTTP handler did not run")
	}

	if err := server.Close(); err != nil {
		t.Fatalf("close HTTP server: %v", err)
	}
	select {
	case err := <-serveDone:
		if err != http.ErrServerClosed && !errors.Is(err, net.ErrClosed) {
			t.Fatalf("Serve after HTTP close = %v, want a closed-server error", err)
		}
	case <-time.After(time.Second):
		t.Fatal("HTTP Close did not unblock the retained listener view's Accept")
	}
	assertManagedPortReserved(t, address)

	if err := retained.Close(); err != nil {
		t.Fatalf("release retained listener: %v", err)
	}
	select {
	case <-retained.acceptDone:
	case <-time.After(time.Second):
		t.Fatal("accept broker did not stop after physical listener release")
	}
	assertManagedPortReleased(t, address)
}

func TestRetainedListenerConcurrentCloseIsIdempotent(t *testing.T) {
	underlying, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	retained := retainTCPListener(underlying)
	t.Cleanup(func() { _ = retained.Close() })
	address := retained.Addr().String()
	view := retained.ServeListener().(*retainedListenerView)
	acceptDone := make(chan error, 1)
	go func() {
		_, err := view.Accept()
		acceptDone <- err
	}()

	var closeWG sync.WaitGroup
	for i := 0; i < 8; i++ {
		closeWG.Add(1)
		go func() {
			defer closeWG.Done()
			_ = view.Close()
		}()
	}
	for i := 0; i < 8; i++ {
		closeWG.Add(1)
		go func() {
			defer closeWG.Done()
			_ = retained.Close()
		}()
	}
	closeWG.Wait()
	select {
	case err := <-acceptDone:
		if !errors.Is(err, net.ErrClosed) {
			t.Fatalf("view Accept after Close = %v, want net.ErrClosed", err)
		}
	case <-time.After(time.Second):
		t.Fatal("concurrent view Close did not unblock Accept")
	}
	select {
	case <-retained.acceptDone:
	case <-time.After(time.Second):
		t.Fatal("accept broker did not stop after concurrent physical Close")
	}
	assertManagedPortReleased(t, address)
}

func TestManagedServeErrorRetainsPortAndHandsOffInertHTTP(t *testing.T) {
	underlying, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	retained := retainTCPListener(&failOnceManagedListener{Listener: underlying})
	t.Cleanup(func() { _ = retained.Close() })
	address := retained.Addr().String()
	server := &http.Server{Handler: http.NotFoundHandler()}
	serveDone := make(chan error, 1)
	go func() { serveDone <- server.Serve(retained.ServeListener()) }()
	select {
	case err := <-serveDone:
		if err == nil || err == http.ErrServerClosed {
			t.Fatalf("primary Serve error = %v, want injected accept failure", err)
		}
	case <-time.After(time.Second):
		t.Fatal("primary Serve did not return after injected accept failure")
	}

	assertManagedPortReserved(t, address)
	inert, inertDone := serveManagedInertHTTP(retained)
	client := &http.Client{Timeout: time.Second}
	response, err := client.Get("http://" + address + "/healthz")
	if err != nil {
		_ = inert.Close()
		t.Fatalf("inert backend request: %v", err)
	}
	_ = response.Body.Close()
	if response.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("inert backend status = %d, want 503", response.StatusCode)
	}
	assertManagedPortReserved(t, address)

	if err := inert.Close(); err != nil {
		t.Fatalf("close inert HTTP server: %v", err)
	}
	select {
	case <-inertDone:
	case <-time.After(time.Second):
		t.Fatal("inert HTTP Close did not unblock its listener view's Accept")
	}
	assertManagedPortReserved(t, address)
	if err := retained.Close(); err != nil {
		t.Fatalf("explicitly release retained listener: %v", err)
	}
	select {
	case <-retained.acceptDone:
	case <-time.After(time.Second):
		t.Fatal("accept broker did not stop after explicit physical release")
	}
	assertManagedPortReleased(t, address)
}

func assertManagedPortReserved(t *testing.T, address string) {
	t.Helper()
	probe, err := net.Listen("tcp", address)
	if err == nil {
		_ = probe.Close()
		t.Fatalf("another listener bound unresolved managed backend port %s", address)
	}
}

func assertManagedPortReleased(t *testing.T, address string) {
	t.Helper()
	probe, err := net.Listen("tcp", address)
	if err != nil {
		t.Fatalf("port remained reserved after explicit physical release: %v", err)
	}
	_ = probe.Close()
}
