//go:build herdr_installed_inert_test

package app

import (
	"context"
	"errors"
	"net"
	"net/http"
	"testing"
	"time"

	"github.com/0cv/herdr-mobile-relay/internal/deviceauth"
)

type installedCommandDenyHTTP struct{}

func (installedCommandDenyHTTP) RoundTrip(*http.Request) (*http.Response, error) {
	return nil, errors.New("installed command fixture denies HTTP")
}

// DenyInstalledCommandHTTP covers default clients before app construction.
func DenyInstalledCommandHTTP(t *testing.T) {
	t.Helper()
	previous := http.DefaultTransport
	http.DefaultTransport = installedCommandDenyHTTP{}
	t.Cleanup(func() { http.DefaultTransport = previous })
}

type installedCommandLogicalListener struct{}

func (*installedCommandLogicalListener) Accept() (net.Conn, error) {
	return nil, errors.New("logical listener only")
}
func (*installedCommandLogicalListener) Close() error   { return nil }
func (*installedCommandLogicalListener) Addr() net.Addr { return nil }

// RunInstalledCommandLogicalBackend attaches only a logical lease and notification.
// It never calls Run, binds a socket, or establishes readiness/qualification.
func RunInstalledCommandLogicalBackend(s *Server, ctx context.Context, observe func(*deviceauth.Store, string) error) error {
	s.developmentCLIHealthClient = func(time.Duration) *http.Client {
		return &http.Client{Transport: installedCommandDenyHTTP{}}
	}
	if s.deviceStore() == nil || s.bootstrapGate.OpenStatus() || s.controlRunID() == "" {
		return errors.New("installed command fixture missing closed store/generation")
	}
	s.installedBackend = newInstalledBackend(ctx, &installedCommandLogicalListener{})
	defer s.installedBackend.Close()
	if err := observe(s.deviceStore(), s.controlRunID()); err != nil {
		return err
	}
	close(s.listenerBound)
	<-ctx.Done()
	return ctx.Err()
}
