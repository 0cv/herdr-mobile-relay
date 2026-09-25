//go:build !herdr_tailscale_test

package app

import (
	"errors"
	"net"
	"net/http"
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
	assertManagedPortReserved(t, address)
	if err := retained.Close(); err != nil {
		t.Fatalf("explicitly release retained listener: %v", err)
	}
	select {
	case <-inertDone:
	case <-time.After(time.Second):
		t.Fatal("inert Serve loop did not stop after explicit listener release")
	}
	probe, err := net.Listen("tcp", address)
	if err != nil {
		t.Fatalf("port remained reserved after explicit safe-release simulation: %v", err)
	}
	_ = probe.Close()
}

func assertManagedPortReserved(t *testing.T, address string) {
	t.Helper()
	probe, err := net.Listen("tcp", address)
	if err == nil {
		_ = probe.Close()
		t.Fatalf("another listener bound unresolved managed backend port %s", address)
	}
}
