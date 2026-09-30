package app

import (
	"context"
	"errors"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/0cv/herdr-mobile-relay/internal/tailscalecli"
)

func TestDevelopmentBackendLeaseKeepsListenerBoundThroughPublication(t *testing.T) {
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	server := &Server{
		developmentBackendBound:  make(chan struct{}),
		developmentBackendActive: true,
	}
	ownedListener := &developmentBackendListener{Listener: listener, server: server}
	t.Cleanup(func() { _ = ownedListener.Close() })
	lease := server.DevelopmentBackendLease()

	publicationEntered := make(chan struct{})
	releasePublication := make(chan struct{})
	var releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { close(releasePublication) }) }
	defer release()
	publicationDone := make(chan error, 1)
	go func() {
		publicationDone <- lease.WithLease(context.Background(), func() error {
			close(publicationEntered)
			<-releasePublication
			return nil
		})
	}()
	<-publicationEntered

	closeDone := make(chan error, 1)
	go func() { closeDone <- ownedListener.Close() }()
	select {
	case err := <-closeDone:
		t.Fatalf("listener closed while publication lease was active: %v", err)
	case <-time.After(50 * time.Millisecond):
	}
	if probe, err := net.Listen("tcp4", listener.Addr().String()); err == nil {
		_ = probe.Close()
		t.Fatal("backend port was released during the publication lease")
	}

	release()
	if err := <-publicationDone; err != nil {
		t.Fatalf("publication lease: %v", err)
	}
	if err := <-closeDone; err != nil {
		t.Fatalf("close owned listener after publication: %v", err)
	}
	if err := lease.WithLease(context.Background(), func() error { return nil }); !errors.Is(err, tailscalecli.ErrConflict) {
		t.Fatalf("lease after listener close = %v, want ownership conflict", err)
	}
}
