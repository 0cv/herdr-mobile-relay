//go:build herdr_installed_inert_test

package app

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/0cv/herdr-mobile-relay/internal/localcontrol"
	"github.com/0cv/herdr-mobile-relay/internal/tailscalecli"
)

func TestInstalledAcknowledgementPhysicalDrain(t *testing.T) {
	m, selected, _, _ := installedOperatorFixture(t)
	var current *Server
	resource := &installedLogicalListener{}
	entered, release := make(chan struct{}), make(chan struct{})
	fault := errors.New("manager post-unlock error")
	SetInstalledInertRunner(t, func(s *Server, ctx context.Context) error {
		current = s
		s.installedBackend = newInstalledBackend(ctx, resource)
		defer s.installedBackend.Close()
		tailscalecli.SetInstalledSetupReadinessFixture(s.installedSetup.session, func(call context.Context) error { return call.Err() })
		tailscalecli.SetInstalledAcknowledgementCompletionFixture(s.installedSetup.session, func() error { close(entered); <-release; return fault })
		close(s.listenerBound)
		<-s.installedSetup.lifetime.Done()
		return nil
	})
	err := RunInstalledSetupOperatorCLI(context.Background(), selected, "fixture", "fixture", slog.New(slog.NewTextHandler(io.Discard, nil)), m, func(ctx context.Context, c localcontrol.InstalledControl, credentials localcontrol.InstalledCredentials) (result error) {
		p := installedOperatorRequest(ctx, c, credentials, "proposal", nil).Proposal
		if p == nil {
			return errors.New("missing proposal")
		}
		if installedOperatorRequest(ctx, c, credentials, "publish", installedOperatorConsent(*p)).Publication != "acknowledged" {
			return errors.New("publish refused")
		}
		call, cancel := context.WithCancel(ctx)
		done := make(chan struct{})
		closed := make(chan struct{})
		closeStarted := false
		var value tailscalecli.AcknowledgedPublication
		var getterErr error
		defer func() {
			cancel()
			close(release)
			<-done
			if closeStarted {
				<-closed
			}
			if result == nil && (value != (tailscalecli.AcknowledgedPublication{}) || !errors.Is(getterErr, fault) || !errors.Is(getterErr, context.Canceled) || !errors.Is(getterErr, tailscalecli.ErrWorkflowRequired)) {
				result = errors.New("late refusal lost manager/cancellation siblings")
			}
		}()
		go func() { defer close(done); value, getterErr = c.AcknowledgedPublication(call) }()
		select {
		case <-entered:
		case <-time.After(5 * time.Second):
			return errors.New("getter not entered")
		}
		closeStarted = true
		go func() { defer close(closed); _ = current.installedBackend.Close() }()
		select {
		case <-current.installedBackend.lifetime.Done():
		case <-time.After(5 * time.Second):
			return errors.New("close did not cancel lifetime")
		}
		if resource.closed.Load() {
			return errors.New("physical resource closed through held getter")
		}
		select {
		case <-closed:
			return errors.New("close did not drain lease")
		default:
		}
		// The callback is still held after cancellation. Release and join before
		// reading its result; the deferred cleanup covers every earlier failure.
		cancel()
		current.mu.Lock()
		current.installedTerminal = true
		current.mu.Unlock()
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if !resource.closed.Load() {
		t.Fatal("physical resource not closed")
	}
}
