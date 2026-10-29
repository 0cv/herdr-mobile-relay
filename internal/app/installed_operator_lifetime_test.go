//go:build herdr_installed_inert_test

package app

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"

	"github.com/0cv/herdr-mobile-relay/internal/deviceauth"
	"sync"
	"testing"
	"time"

	"github.com/0cv/herdr-mobile-relay/internal/localcontrol"
	"github.com/0cv/herdr-mobile-relay/internal/tailscalecli"
)

func TestInstalledOperatorSetupCancellationDrain(t *testing.T) {
	m, selected, _, mutations := installedOperatorFixture(t)
	service, cancel := context.WithCancel(context.Background())
	defer cancel()
	entered, observed, release := make(chan struct{}), make(chan struct{}), make(chan struct{})
	var once sync.Once
	unblock := func() { once.Do(func() { close(release) }) }
	defer unblock()
	callbackDone := make(chan struct{})
	var retained localcontrol.InstalledControl
	var credentials localcontrol.InstalledCredentials
	resource := &installedLogicalListener{}
	SetInstalledInertRunner(t, func(s *Server, ctx context.Context) error {
		s.developmentCLIHealthClient = func(time.Duration) *http.Client { return &http.Client{Transport: installedTerminalDenyHTTP{}} }
		s.installedBackend = newInstalledBackend(ctx, resource)
		defer s.installedBackend.Close()
		// Release the deliberately blocked observation before any assertion exit
		// waits on physical close. Service/entrypoint subsequently joins everything.
		defer unblock()
		tailscalecli.SetInstalledSetupReadinessFixture(s.installedSetup.session, func(call context.Context) error {
			close(entered)
			<-call.Done()
			close(observed)
			<-release
			return call.Err()
		})
		close(s.listenerBound)
		select {
		case <-entered:
		case <-callbackDone:
			return errors.New("callback failed before preflight")
		case <-s.installedSetup.lifetime.Done():
			return errors.New("setup retired before preflight")
		}
		cancel()
		<-observed
		if resource.closed.Load() {
			return errors.New("physical resource escaped active readiness")
		}
		select {
		case <-callbackDone:
			return errors.New("callback escaped in-flight operation")
		default:
		}
		unblock()
		return nil
	})
	err := RunInstalledSetupOperatorCLI(service, selected, "fixture", "fixture", slog.New(slog.NewTextHandler(io.Discard, nil)), m, func(ctx context.Context, c localcontrol.InstalledControl, secret localcontrol.InstalledCredentials) error {
		defer close(callbackDone)
		retained, credentials = c, secret
		p := installedOperatorRequest(ctx, c, secret, "proposal", nil).Proposal
		if p == nil {
			return errors.New("missing proposal")
		}
		completed := make(chan localcontrol.InstalledReply, 1)
		joined := make(chan struct{})
		go func() {
			defer close(joined)
			completed <- installedOperatorRequest(context.Background(), c, secret, "publish", installedOperatorConsent(*p))
		}()
		defer func() { <-joined }()
		// Every exit joins the actual admitted Dispatch before callback retirement.
		var reply localcontrol.InstalledReply
		select {
		case reply = <-completed:
			if ctx.Err() == nil {
				return errors.New("preflight returned before cancellation")
			}
		case <-ctx.Done():
			reply = <-completed
		}
		// Service/backend cancellation can precede asynchronous core retirement.
		// BeforePublish then refuses before consumption; neither outcome dispatches.
		if (reply.Code != "cancelled" && reply.Code != "readiness_unavailable") ||
			reply.Invitation != "" || reply.Status != nil || reply.Proposal != nil || reply.Publication != "not_dispatched" {
			return fmt.Errorf("cancelled preflight code=%q publication=%q", reply.Code, reply.Publication)
		}
		return nil
	})
	if err != nil && !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	select {
	case <-callbackDone:
	default:
		t.Fatal("callback not joined")
	}
	if !resource.closed.Load() || mutations() != 0 {
		t.Fatal("drain or pre-consumption cancellation")
	}
	if installedOperatorRequest(context.Background(), retained, credentials, "status", nil).Code != "retired" {
		t.Fatal("retained capability")
	}
}

func TestInstalledOperatorCallbackFailureKeepsService(t *testing.T) {
	m, selected, _, mutations := installedOperatorFixture(t)
	sentinel := errors.New("synthetic endpoint failure")
	var retained localcontrol.InstalledControl
	var credentials localcontrol.InstalledCredentials
	SetInstalledInertRunner(t, func(s *Server, ctx context.Context) error {
		s.developmentCLIHealthClient = func(time.Duration) *http.Client { return &http.Client{Transport: installedTerminalDenyHTTP{}} }
		s.installedBackend = newInstalledBackend(ctx, &installedLogicalListener{})
		defer s.installedBackend.Close()
		// All inert publication paths, even regressions, use the audited seam.
		tailscalecli.SetInstalledSetupReadinessFixture(s.installedSetup.session, func(context.Context) error { return tailscalecli.ErrTransientUnavailable })
		close(s.listenerBound)
		<-s.installedSetup.lifetime.Done()
		if ctx.Err() != nil || s.controlRunID() != credentials.Run || s.installedSetup.registered() {
			return errors.New("callback failure changed service")
		}
		if installedOperatorRequest(ctx, retained, credentials, "status", nil).Code != "retired" {
			return errors.New("failed endpoint retained authority")
		}
		return nil
	})
	err := RunInstalledSetupOperatorCLI(context.Background(), selected, "fixture", "fixture", slog.New(slog.NewTextHandler(io.Discard, nil)), m, func(_ context.Context, c localcontrol.InstalledControl, secret localcontrol.InstalledCredentials) error {
		retained, credentials = c, secret
		return sentinel
	})
	if !errors.Is(err, sentinel) || mutations() != 0 {
		t.Fatal("callback error lost or mutation")
	}
}

func TestInstalledSetupReadinessBeforeAppAttempt(t *testing.T) {
	m, selected, cfg, mutations := installedOperatorFixture(t)
	var ready bool
	SetInstalledInertRunner(t, func(s *Server, ctx context.Context) error {
		s.developmentCLIHealthClient = func(time.Duration) *http.Client { return &http.Client{Transport: installedTerminalDenyHTTP{}} }
		s.installedBackend = newInstalledBackend(ctx, &installedLogicalListener{})
		defer s.installedBackend.Close()
		tailscalecli.SetInstalledSetupReadinessFixture(s.installedSetup.session, func(context.Context) error {
			if !ready {
				return tailscalecli.ErrTransientUnavailable
			}
			return nil
		})
		close(s.listenerBound)
		<-s.installedSetup.lifetime.Done()
		return nil
	})
	err := RunInstalledSetupCLI(context.Background(), selected, "fixture", "fixture", slog.New(slog.NewTextHandler(io.Discard, nil)), m, func(ctx context.Context, h InstalledSetupControl) error {
		p, err := h.Proposal(ctx)
		if err != nil {
			return err
		}
		consent := tailscalecli.Consent{Accepted: true, Scope: "production", NodeID: p.NodeID, Origin: cfg.TailscaleCLIOrigin, HTTPSPort: 8443, BackendPort: cfg.Port, ReservationID: p.ReservationID, RouteConfirmation: p.Confirmation, PersistentRouteAccepted: true, CheckToWriteRaceAccepted: true, PortReuseRiskAccepted: true, NoRollbackAccepted: true, NoRemoteDrainAccepted: true}
		if !errors.Is(h.Publish(ctx, consent), tailscalecli.ErrTransientUnavailable) {
			return errors.New("unavailable readiness accepted")
		}
		phase, err := h.Status(ctx)
		if err != nil || phase != "waiting-explicit-publication" {
			return errors.New("unavailable consumed app attempt")
		}
		cancelled, cancel := context.WithCancel(ctx)
		cancel()
		if !errors.Is(h.Publish(cancelled, consent), context.Canceled) {
			return errors.New("cancelled attempt accepted")
		}
		again, err := h.Proposal(ctx)
		if err != nil || again != p {
			return errors.New("reservation/proposal changed")
		}
		ready = true
		return h.Publish(ctx, consent)
	})
	if err != nil || mutations() != 1 {
		t.Fatalf("explicit same-attempt retry: %v", err)
	}
}

func TestInstalledOperatorResumeQueuedCancellationDrain(t *testing.T) {
	m, selected, cfg, _ := installedOperatorFixture(t, true)
	if _, err := deviceauth.Open(filepath.Join(cfg.RuntimeDir, "device-auth")); err != nil {
		t.Fatal(err)
	}
	storePath := filepath.Join(cfg.RuntimeDir, "device-auth", "devices.json")
	before, err := os.ReadFile(storePath)
	if err != nil {
		t.Fatal(err)
	}
	service, cancel := context.WithCancel(context.Background())
	defer cancel()
	waiting, callbackDone := make(chan struct{}), make(chan struct{})
	var observe sync.Once
	var retained localcontrol.InstalledControl
	var credentials localcontrol.InstalledCredentials
	resource := &installedLogicalListener{}
	SetInstalledInertRunner(t, func(s *Server, ctx context.Context) error {
		s.developmentCLIHealthClient = func(time.Duration) *http.Client { return &http.Client{Transport: installedTerminalDenyHTTP{}} }
		s.installedBackend = newInstalledBackend(ctx, resource)
		defer s.installedBackend.Close()
		unlock, err := s.externalArmMu.Lock(ctx)
		if err != nil {
			return err
		}
		defer unlock()
		s.installedOperatorWaiting = func() { observe.Do(func() { close(waiting) }) }
		close(s.listenerBound)
		select {
		case <-waiting:
		case <-callbackDone:
			return errors.New("callback did not enter actual admission queue")
		}
		cancel()
		// Cancellation must complete while serialization remains held. The callback
		// joins its dispatcher before this signal; deferred unlock/close follow it.
		<-callbackDone
		if resource.closed.Load() {
			return errors.New("backend closed before runner cleanup")
		}
		return nil
	})
	err = RunInstalledResumeOperatorCLI(service, selected, "fixture", "fixture", slog.New(slog.NewTextHandler(io.Discard, nil)), m, func(ctx context.Context, c localcontrol.InstalledControl, secret localcontrol.InstalledCredentials) error {
		defer close(callbackDone)
		retained, credentials = c, secret
		completed := make(chan localcontrol.InstalledReply, 1)
		joined := make(chan struct{})
		go func() {
			defer close(joined)
			completed <- installedOperatorRequest(context.Background(), c, secret, "reprint", nil)
		}()
		defer func() { <-joined }()
		var reply localcontrol.InstalledReply
		select {
		case reply = <-completed:
			if ctx.Err() == nil {
				return errors.New("admission returned before cancellation")
			}
		case <-ctx.Done():
			reply = <-completed
		}
		// The backend lifetime can cancel the held queue before the core's
		// retirement bridge runs: Admit returns an error, not a pairing body.
		if (reply.Code != "cancelled" && reply.Code != "admission_refused") ||
			reply.Invitation != "" || reply.Status != nil || reply.Proposal != nil || reply.ArmReceipt != "" {
			return fmt.Errorf("resume cancelled queue code=%q", reply.Code)
		}
		return nil
	})
	if err != nil && !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if !resource.closed.Load() {
		t.Fatal("backend not closed")
	}
	after, readErr := os.ReadFile(storePath)
	if readErr != nil || !bytes.Equal(before, after) {
		t.Fatal("cancelled queue mutated device evidence")
	}
	if installedOperatorRequest(context.Background(), retained, credentials, "status", nil).Code != "retired" {
		t.Fatal("retained resume authority")
	}
}

func TestInstalledOperatorPostDispatchFailureConsumed(t *testing.T) {
	m, selected, _, mutations := installedOperatorFixture(t)
	var current *Server
	SetInstalledInertRunner(t, func(s *Server, ctx context.Context) error {
		current = s
		s.developmentCLIHealthClient = func(time.Duration) *http.Client { return &http.Client{Transport: installedTerminalDenyHTTP{}} }
		s.installedBackend = newInstalledBackend(ctx, &installedLogicalListener{})
		defer s.installedBackend.Close()
		calls := 0
		tailscalecli.SetInstalledSetupReadinessFixture(s.installedSetup.session, func(context.Context) error {
			calls++
			if calls <= 2 {
				return nil
			} // core and app pre-consumption checks
			return tailscalecli.ErrTransientUnavailable // dispatched manager transaction
		})
		close(s.listenerBound)
		<-s.installedSetup.lifetime.Done()
		return nil
	})
	err := RunInstalledSetupOperatorCLI(context.Background(), selected, "fixture", "fixture", slog.New(slog.NewTextHandler(io.Discard, nil)), m, func(ctx context.Context, c localcontrol.InstalledControl, secret localcontrol.InstalledCredentials) error {
		p := installedOperatorRequest(ctx, c, secret, "proposal", nil).Proposal
		if p == nil {
			return errors.New("missing proposal")
		}
		consent := installedOperatorConsent(*p)
		reply := installedOperatorRequest(ctx, c, secret, "publish", consent)
		if reply.Code != "outcome_unknown" || reply.Publication != "dispatched_unknown" || current.installedSetup.phase() != "publication-failed-separate-lifecycle-required" {
			return errors.New("post-dispatch failure treated as retryable")
		}
		if installedOperatorRequest(ctx, c, secret, "publish", consent).Code != "consumed" {
			return errors.New("post-dispatch replay")
		}
		return nil
	})
	if err != nil || mutations() != 0 {
		t.Fatalf("dispatch failure semantics: %v", err)
	}
}
