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
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/0cv/herdr-mobile-relay/internal/deviceauth"
	"github.com/0cv/herdr-mobile-relay/internal/localcontrol"
	"github.com/0cv/herdr-mobile-relay/internal/tailscalecli"
)

func TestInstalledOperatorSetupValidationQueueCancellation(t *testing.T) {
	for _, op := range []string{"status", "admit_boundary", "pairing_leaf"} {
		t.Run(op, func(t *testing.T) {
			m, selected, _, mutations := installedOperatorFixture(t)
			var current *Server
			var held atomic.Bool
			var once sync.Once
			queued := make(chan struct{})
			endpointDone := make(chan struct{})
			SetInstalledInertRunner(t, func(s *Server, ctx context.Context) error {
				current = s
				s.developmentCLIHealthClient = func(time.Duration) *http.Client { return &http.Client{Transport: installedTerminalDenyHTTP{}} }
				s.installedBackend = newInstalledBackend(ctx, &installedLogicalListener{})
				defer s.installedBackend.Close()
				tailscalecli.SetInstalledSetupReadinessFixture(s.installedSetup.session, func(context.Context) error { return nil })
				tailscalecli.SetInstalledSetupQueueObserverFixture(s.installedSetup.session, func() {
					if held.Load() {
						once.Do(func() { close(queued) })
					}
				})
				close(s.listenerBound)
				<-endpointDone
				return nil
			})
			err := RunInstalledSetupOperatorCLI(context.Background(), selected, "fixture", "fixture", slog.New(slog.NewTextHandler(io.Discard, nil)), m, func(ctx context.Context, c localcontrol.InstalledControl, secret localcontrol.InstalledCredentials) error {
				defer close(endpointDone)
				p := installedOperatorRequest(ctx, c, secret, "proposal", nil).Proposal
				if p == nil {
					return errors.New("missing proposal")
				}
				if reply := installedOperatorRequest(ctx, c, secret, "publish", installedOperatorConsent(*p)); reply.Code != "ok" {
					return fmt.Errorf("publish code=%s", reply.Code)
				}
				entered, release, holderDone := make(chan struct{}), make(chan struct{}), make(chan struct{})
				var releaseOnce sync.Once
				unblock := func() { releaseOnce.Do(func() { close(release) }) }
				go func() {
					defer close(holderDone)
					_ = tailscalecli.WithInstalledSetupSerializationFixture(ctx, current.installedSetup.session, func() error {
						held.Store(true)
						close(entered)
						<-release
						held.Store(false)
						return nil
					})
				}()
				request, cancel := context.WithCancel(ctx)
				done := make(chan struct{})
				var requestErr error
				started := false
				defer func() {
					cancel()
					unblock()
					<-holderDone
					if started {
						<-done
					}
				}()
				select {
				case <-entered:
				case <-holderDone:
					return errors.New("session holder did not enter")
				case <-time.After(5 * time.Second):
					return errors.New("session holder entry timeout")
				}
				started = true
				go func() {
					defer close(done)
					switch op {
					case "status":
						reply := installedOperatorRequest(request, c, secret, "status", nil)
						if reply.Code != "cancelled" || reply.Status != nil || reply.Invitation != "" {
							requestErr = fmt.Errorf("status queue code=%s", reply.Code)
						}
					case "admit_boundary":
						requestErr = current.installedPairingResultBoundary(request, selected)
						if errors.Is(requestErr, context.Canceled) {
							requestErr = nil
						} else {
							requestErr = errors.New("boundary lost cancellation")
						}
					case "pairing_leaf":
						payload, _, err := current.reprintInstalledPairing(request, selected)
						if !errors.Is(err, context.Canceled) || payload.URL != "" {
							requestErr = errors.New("pairing queue lost cancellation")
						}
					}
				}()
				select {
				case <-queued:
				case <-done:
					return errors.New("request never entered session queue")
				case <-time.After(5 * time.Second):
					return errors.New("session queue entry timeout")
				}
				cancel()
				select {
				case <-done:
				case <-time.After(5 * time.Second):
					return errors.New("cancelled request waited for held session")
				}
				if requestErr != nil {
					return requestErr
				}
				select {
				case <-holderDone:
					return errors.New("serialization was not held")
				default:
				}
				unblock()
				<-holderDone
				if reply := installedOperatorRequest(ctx, c, secret, "status", nil); reply.Code != "ok" {
					return fmt.Errorf("control lost after cancellation: %s", reply.Code)
				}
				return nil
			})
			if err != nil || mutations() != 1 {
				t.Fatalf("setup validation cancellation: %v", err)
			}
		})
	}
}

func TestInstalledLegacyGenerationBeforeExposure(t *testing.T) {
	for _, mode := range []string{"setup", "resume"} {
		t.Run(mode, func(t *testing.T) {
			m, selected, _, _ := installedOperatorFixture(t, mode == "resume")
			var generation string
			SetInstalledInertRunner(t, func(s *Server, ctx context.Context) error {
				generation = s.controlRunID()
				if generation == "" || s.cfg.ControlRunID != "" || s.cfg.PairingSocketPath != "" {
					return errors.New("missing generation or config mutation before exposure")
				}
				if err := selected.Validate(s.cfg); err != nil {
					return err
				}
				if err := s.pinInstalledGeneration("replacement"); err == nil || s.controlRunID() != generation {
					return errors.New("generation replaced")
				}
				s.developmentCLIHealthClient = func(time.Duration) *http.Client { return &http.Client{Transport: installedTerminalDenyHTTP{}} }
				s.installedBackend = newInstalledBackend(ctx, &installedLogicalListener{})
				defer s.installedBackend.Close()
				if mode == "setup" {
					tailscalecli.SetInstalledSetupReadinessFixture(s.installedSetup.session, func(context.Context) error { return tailscalecli.ErrTransientUnavailable })
					// A different generation must not be accepted: preflight is pinned
					// before the service becomes observable, even without an operator.
					if err := s.installedSetup.session.BindRuntimeGeneration(ctx, selected.Binding(), "different"); err == nil {
						return errors.New("setup generation not pinned")
					}
					close(s.listenerBound)
					<-s.installedSetup.lifetime.Done()
					if ctx.Err() != nil || s.controlRunID() != generation {
						return errors.New("callback retired generation")
					}
				}
				return nil
			})
			logger := slog.New(slog.NewTextHandler(io.Discard, nil))
			var err error
			if mode == "setup" {
				err = RunInstalledSetupCLI(context.Background(), selected, "fixture", "fixture", logger, m, func(context.Context, InstalledSetupControl) error { return nil })
			} else {
				err = RunInstalledCLI(context.Background(), selected, "fixture", "fixture", logger, m)
			}
			if err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestInstalledOperatorRequestCancellationKeepsProposal(t *testing.T) {
	m, selected, _, mutations := installedOperatorFixture(t)
	entered := make(chan struct{})
	var current *Server
	SetInstalledInertRunner(t, func(s *Server, ctx context.Context) error {
		current = s
		s.developmentCLIHealthClient = func(time.Duration) *http.Client { return &http.Client{Transport: installedTerminalDenyHTTP{}} }
		s.installedBackend = newInstalledBackend(ctx, &installedLogicalListener{})
		defer s.installedBackend.Close()
		tailscalecli.SetInstalledSetupReadinessFixture(s.installedSetup.session, func(call context.Context) error {
			close(entered)
			<-call.Done()
			return call.Err()
		})
		close(s.listenerBound)
		<-s.installedSetup.lifetime.Done()
		if ctx.Err() != nil {
			return errors.New("request stopped service")
		}
		return nil
	})
	err := RunInstalledSetupOperatorCLI(context.Background(), selected, "fixture", "fixture", slog.New(slog.NewTextHandler(io.Discard, nil)), m, func(ctx context.Context, c localcontrol.InstalledControl, secret localcontrol.InstalledCredentials) error {
		p := installedOperatorRequest(ctx, c, secret, "proposal", nil).Proposal
		if p == nil {
			return errors.New("missing proposal")
		}
		request, cancel := context.WithCancel(ctx)
		defer cancel()
		done := make(chan struct{})
		var reply localcontrol.InstalledReply
		go func() {
			defer close(done)
			reply = installedOperatorRequest(request, c, secret, "publish", installedOperatorConsent(*p))
		}()
		defer func() { cancel(); <-done }()
		select {
		case <-entered:
		case <-done:
			return fmt.Errorf("preflight never entered: %s", reply.Code)
		case <-time.After(5 * time.Second):
			return errors.New("preflight entry timeout")
		}
		cancel()
		<-done
		if reply.Code != "cancelled" || reply.Invitation != "" || reply.Status != nil || reply.Proposal != nil || reply.Publication != "not_dispatched" {
			return fmt.Errorf("request cancellation code=%s", reply.Code)
		}
		if ctx.Err() != nil || current.installedSetup.attempted || current.installedAdmissionRevoked() {
			return errors.New("request retired or consumed authority")
		}
		again := installedOperatorRequest(ctx, c, secret, "proposal", nil).Proposal
		if again == nil || *again != *p {
			return errors.New("same proposal unusable")
		}
		status := installedOperatorRequest(ctx, c, secret, "status", nil)
		if status.Code != "ok" || status.Status == nil || status.Status.Registered {
			return fmt.Errorf("same control unusable: %s", status.Code)
		}
		return nil
	})
	if err != nil || mutations() != 0 {
		t.Fatalf("request lifetime: %v", err)
	}
}

func TestInstalledOperatorReadOnlyStatusEvidence(t *testing.T) {
	for _, mode := range []string{"waiting", "published", "resume"} {
		for _, change := range []string{"none", "transient", "gate_suspended", "terminal", "missing_registration", "changed_reservation", "uncertain_store"} {
			t.Run(mode+"/"+change, func(t *testing.T) {
				m, selected, cfg, mutations := installedOperatorFixture(t, mode == "resume")
				// Explicit fixture initialization precedes the sole live owner.
				if _, err := deviceauth.Open(filepath.Join(cfg.RuntimeDir, "device-auth")); err != nil {
					t.Fatal(err)
				}
				storePath := filepath.Join(cfg.RuntimeDir, "device-auth", "devices.json")
				before, err := os.ReadFile(storePath)
				if err != nil {
					t.Fatal(err)
				}
				var current *Server
				endpointDone := make(chan struct{})
				SetInstalledInertRunner(t, func(s *Server, ctx context.Context) error {
					current = s
					s.developmentCLIHealthClient = func(time.Duration) *http.Client { return &http.Client{Transport: installedTerminalDenyHTTP{}} }
					s.installedBackend = newInstalledBackend(ctx, &installedLogicalListener{})
					defer s.installedBackend.Close()
					if s.installedSetup != nil {
						tailscalecli.SetInstalledSetupReadinessFixture(s.installedSetup.session, func(context.Context) error { return nil })
					}
					close(s.listenerBound)
					<-endpointDone
					return nil
				})
				// Callback completion is explicitly signalled; the entrypoint joins
				// retirement before restoring any fixture environment.
				callback := func(ctx context.Context, c localcontrol.InstalledControl, secret localcontrol.InstalledCredentials) error {
					defer close(endpointDone)
					if mode == "published" {
						p := installedOperatorRequest(ctx, c, secret, "proposal", nil).Proposal
						if p == nil {
							return errors.New("missing proposal")
						}
						if reply := installedOperatorRequest(ctx, c, secret, "publish", installedOperatorConsent(*p)); reply.Code != "ok" {
							return fmt.Errorf("publish code=%s", reply.Code)
						}
					}
					registered := mode != "waiting"
					initial := installedOperatorRequest(ctx, c, secret, "status", nil)
					if initial.Code != "ok" || initial.Status == nil || initial.Status.Registered != registered || initial.Status.InvitationAvailable {
						return fmt.Errorf("initial status code=%s", initial.Code)
					}
					switch change {
					case "transient":
						current.quarantineTailscaleCLI()
					case "gate_suspended":
						current.bootstrapGate.SuspendAdmission()
					case "terminal":
						current.recordInstalledPairingFailure(tailscalecli.ErrConflict)
					case "missing_registration":
						if registered {
							if err := os.Remove(filepath.Join(cfg.TailscaleCLIStateRoot, "registration.json")); err != nil {
								return err
							}
						}
					case "changed_reservation":
						path := filepath.Join(cfg.TailscaleCLICoordinationRoot, fmt.Sprintf("backend-port-%d.json", cfg.Port))
						if err := os.WriteFile(path, []byte("{}"), 0600); err != nil {
							return err
						}
					case "uncertain_store":
						if err := os.WriteFile(storePath, []byte("{}"), 0600); err != nil {
							return err
						}
					}
					reply := installedOperatorRequest(ctx, c, secret, "status", nil)
					refused := change == "uncertain_store" || (registered && (change == "missing_registration" || change == "changed_reservation"))
					if refused {
						if reply.Code != "backend_refused" || reply.Status != nil {
							return fmt.Errorf("uncertain status code=%s", reply.Code)
						}
					} else {
						if reply.Code != "ok" || reply.Status == nil || reply.Status.Registered != registered || reply.Status.InvitationAvailable {
							return fmt.Errorf("status code=%s", reply.Code)
						}
						if (change == "transient" || change == "gate_suspended" || change == "terminal") && !reply.Status.Quarantined {
							return errors.New("quarantine hidden")
						}
					}
					if current.bootstrapGate.OpenStatus() {
						return errors.New("status opened gate")
					}
					if (change == "transient" || change == "gate_suspended") && current.installedAdmissionRevoked() {
						return errors.New("status latched reversible quarantine")
					}
					if change == "terminal" && !current.installedAdmissionRevoked() {
						return errors.New("status cleared terminal latch")
					}
					after, err := os.ReadFile(storePath)
					if err != nil {
						return err
					}
					expected := before
					if change == "uncertain_store" {
						expected = []byte("{}")
					}
					if !bytes.Equal(after, expected) {
						return errors.New("status mutated device evidence")
					}
					return nil
				}
				logger := slog.New(slog.NewTextHandler(io.Discard, nil))
				if mode == "resume" {
					err = RunInstalledResumeOperatorCLI(context.Background(), selected, "fixture", "fixture", logger, m, callback)
				} else {
					err = RunInstalledSetupOperatorCLI(context.Background(), selected, "fixture", "fixture", logger, m, callback)
				}
				want := 0
				if mode == "published" {
					want = 1
				}
				if err != nil || mutations() != want {
					t.Fatalf("status effects: %v", err)
				}
			})
		}
	}
}

func TestInstalledOperatorResumeRequestQueueCancellation(t *testing.T) {
	m, selected, _, _ := installedOperatorFixture(t, true)
	waiting, finished := make(chan struct{}), make(chan struct{})
	var once sync.Once
	request, cancel := context.WithCancel(context.Background())
	defer cancel()
	SetInstalledInertRunner(t, func(s *Server, ctx context.Context) error {
		s.developmentCLIHealthClient = func(time.Duration) *http.Client { return &http.Client{Transport: installedTerminalDenyHTTP{}} }
		s.installedBackend = newInstalledBackend(ctx, &installedLogicalListener{})
		defer s.installedBackend.Close()
		unlock, err := s.externalArmMu.Lock(ctx)
		if err != nil {
			return err
		}
		defer unlock()
		s.installedOperatorWaiting = func() { once.Do(func() { close(waiting) }) }
		close(s.listenerBound)
		select {
		case <-waiting:
		case <-finished:
			return errors.New("queue not entered")
		}
		cancel()
		<-finished // dispatcher and status joined WHILE serialization is held
		if ctx.Err() != nil {
			return errors.New("request stopped service")
		}
		return nil
	})
	err := RunInstalledResumeOperatorCLI(context.Background(), selected, "fixture", "fixture", slog.New(slog.NewTextHandler(io.Discard, nil)), m, func(ctx context.Context, c localcontrol.InstalledControl, secret localcontrol.InstalledCredentials) error {
		defer close(finished)
		reply := installedOperatorRequest(request, c, secret, "reprint", nil)
		if reply.Code != "cancelled" || reply.Invitation != "" || reply.Status != nil || reply.Proposal != nil || reply.ArmReceipt != "" {
			return fmt.Errorf("request queue code=%s", reply.Code)
		}
		status := installedOperatorRequest(ctx, c, secret, "status", nil)
		if status.Code != "ok" || status.Status == nil || !status.Status.Registered {
			return fmt.Errorf("control retired with request: %s", status.Code)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}
