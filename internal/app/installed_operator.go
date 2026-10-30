package app

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"log/slog"

	"github.com/0cv/herdr-mobile-relay/internal/config"
	"github.com/0cv/herdr-mobile-relay/internal/deviceauth"
	"github.com/0cv/herdr-mobile-relay/internal/localcontrol"
	"github.com/0cv/herdr-mobile-relay/internal/tailscalecli"
)

// InstalledOperator is a trusted SERVICE-LIFETIME endpoint callback, not a pane
// or request callback. Credentials are secret-bearing. A future transport must
// join whole handlers and suppress output on disconnect/cancellation before it
// returns. In-process result checks do not promise atomic network delivery.
// Returning retires operator authority, not the healthy service or its devices.
type InstalledOperator func(context.Context, localcontrol.InstalledControl, localcontrol.InstalledCredentials) error

func RunInstalledSetupOperatorCLI(ctx context.Context, selected config.InstalledConfig, version, revision string, logger *slog.Logger, manager *tailscalecli.Manager, operator InstalledOperator) error {
	if operator == nil {
		return tailscalecli.ErrWorkflowRequired
	}
	return runInstalledSetupCLI(ctx, selected, version, revision, logger, manager, nil, operator, nil)
}

// RunInstalledSetupPromotionCLI keeps publication intent inside the owned core's
// authenticated, serialized BeforePublish boundary. The hook must durably record
// intent; a failed hook leaves the proposal unconsumed and never publishes.
func RunInstalledSetupPromotionCLI(ctx context.Context, selected config.InstalledConfig, version, revision string, logger *slog.Logger, manager *tailscalecli.Manager, beforePublish func(context.Context) error, operator InstalledOperator) error {
	if beforePublish == nil || operator == nil {
		return tailscalecli.ErrWorkflowRequired
	}
	return runInstalledSetupCLI(ctx, selected, version, revision, logger, manager, nil, operator, beforePublish)
}

// RunInstalledResumeOperatorCLI never falls through to setup or publication.
func RunInstalledResumeOperatorCLI(ctx context.Context, selected config.InstalledConfig, version, revision string, logger *slog.Logger, manager *tailscalecli.Manager, operator InstalledOperator) error {
	if operator == nil {
		return tailscalecli.ErrWorkflowRequired
	}
	return runInstalledCLI(ctx, selected, version, revision, logger, manager, operator)
}

// Stop AND join cancellation bridges before releasing the authority they cover.
func installedCancelBridge(parent context.Context, cancel context.CancelFunc) func() {
	done := make(chan struct{})
	stop := context.AfterFunc(parent, func() { defer close(done); cancel() })
	return func() {
		if !stop() {
			<-done
		}
	}
}

func invokeInstalledOperator(ctx context.Context, control localcontrol.InstalledControl, credentials localcontrol.InstalledCredentials, operator InstalledOperator) error {
	lifetime, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	stop := context.AfterFunc(lifetime, func() { defer close(done); control.Retire() })
	defer func() {
		control.Retire()
		cancel()
		if !stop() {
			<-done
		}
	}()
	if err := lifetime.Err(); err != nil {
		return err
	}
	return operator(lifetime, control, credentials)
}

func (s *Server) runInstalledOperator(ctx context.Context, selected config.InstalledConfig, operator InstalledOperator) error {
	control, credentials, err := s.composeInstalledOperator(ctx, selected, InstalledSetupControl{})
	if err != nil {
		return err
	}
	lifetime, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	var operatorErr error
	join := func() { cancel(); <-done }
	defer join()
	go func() {
		defer close(done)
		defer control.Retire()
		select {
		case <-lifetime.Done():
			return
		case <-s.listenerBound:
		}
		operatorErr = invokeInstalledOperator(lifetime, control, credentials, operator)
	}()
	err = installedCLIRunner()(s, lifetime)
	join()
	return errors.Join(err, operatorErr)
}

func (s *Server) composeInstalledOperator(ctx context.Context, selected config.InstalledConfig, setup InstalledSetupControl) (localcontrol.InstalledControl, localcontrol.InstalledCredentials, error) {
	fail := func(err error) (localcontrol.InstalledControl, localcontrol.InstalledCredentials, error) {
		return localcontrol.InstalledControl{}, localcontrol.InstalledCredentials{}, err
	}
	if err := selected.Validate(s.cfg); err != nil {
		return fail(err)
	}
	port, err := tailscaleCLIHTTPSPort(s.cfg.TailscaleCLIOrigin)
	if err != nil {
		return fail(err)
	}
	var reference [16]byte
	if _, err := rand.Read(reference[:]); err != nil {
		return fail(err)
	}
	// This random reference names the exact selection retained by all closures;
	// it encodes neither paths nor a hash of redacted formatting.
	selection := localcontrol.InstalledSelection{Selection: hex.EncodeToString(reference[:]), Origin: s.cfg.TailscaleCLIOrigin, HTTPSPort: uint16(port), RelayPort: uint16(s.cfg.Port)}
	backend := localcontrol.InstalledBackend{
		Status: func(ctx context.Context) (localcontrol.InstalledObservation, error) {
			s.mu.RLock()
			quarantined := s.installedTerminal || s.quarantined
			s.mu.RUnlock()
			observation := localcontrol.InstalledObservation{Quarantined: quarantined || s.bootstrapGate.AdmissionQuarantined()}
			if err := selected.Validate(s.cfg); err != nil {
				return observation, err
			}
			if s.installedSetup != nil {
				if err := s.installedSetup.session.ValidateRuntimeBindingContext(ctx, selected.Binding()); err != nil {
					return observation, err
				}
				// The channel is a historical receipt, not a current observation.
				// Waiting must not recursively enter registration verification.
				if s.installedSetup.registered() {
					route, err := s.installedSetup.session.VerifyRegisteredRoute(ctx, selected.Binding())
					if err != nil {
						return observation, err
					}
					observation.Registered = route.JournalState == tailscalecli.StateRegistered
				}
			} else {
				if err := s.installedCLI.ValidateRuntimeBinding(selected.Binding()); err != nil {
					return observation, err
				}
				route, err := s.installedCLI.VerifyRegisteredRoute(ctx, "production", s.cfg.InstanceID, selection.Origin, port, s.cfg.Port)
				if err != nil {
					return observation, err
				}
				observation.Registered = route.JournalState == tailscalecli.StateRegistered
			}
			// Observe store evidence even while admission is closed. This is a
			// read-only snapshot; no secret is included in the observation.
			if store := s.deviceStore(); store != nil {
				_, err := store.CurrentBootstrapInvitation(ctx)
				available, err := installedInvitationObservation(err)
				if err != nil {
					return observation, err
				}
				observation.InvitationAvailable = available && !observation.Quarantined && s.bootstrapGate != nil && s.bootstrapGate.OpenStatus()
			} else {
				return observation, deviceauth.ErrManagedArmRecovery
			}
			return observation, ctx.Err()
		},
		Admit: func(ctx context.Context) error {
			if err := s.installedPairingResultBoundary(ctx, selected); err != nil {
				return err
			}
			return s.withInstalledPairingScope(ctx, func(ctx context.Context) error {
				if s.installedAdmissionRevoked() {
					return tailscalecli.ErrWorkflowRequired
				}
				status, err := s.admitTailscaleCLILocked(ctx)
				if err := s.installedPairingAdmission(status, err); err != nil {
					return err
				}
				return s.installedPairingResultBoundary(ctx, selected)
			}, s.installedOperatorWaiting)
		},
		Arm: func(ctx context.Context) (string, error) {
			payload, _, err := s.armInstalledPairing(ctx, selected)
			return payload.URL, err
		},
		Reprint: func(ctx context.Context) (string, error) {
			payload, _, err := s.reprintInstalledPairing(ctx, selected)
			return payload.URL, err
		},
	}
	var control localcontrol.InstalledControl
	var credentials localcontrol.InstalledCredentials
	if setup.state != nil {
		p, err := setup.Proposal(ctx)
		if err != nil {
			return fail(err)
		}
		selection.Node, selection.Profile, selection.Attempt = p.NodeID, string(p.Profile), p.ReservationID
		backend.PublicationSelection, err = selected.Binding().Fingerprint()
		if err != nil {
			return fail(err)
		}
		backend.AcknowledgedPublication = func(ctx context.Context) (tailscalecli.AcknowledgedPublication, error) {
			var value tailscalecli.AcknowledgedPublication
			err := setup.operation(ctx, func(ctx context.Context, a *installedSetupState) error {
				a.mu.Lock()
				b := a.backend
				a.mu.Unlock()
				if b == nil {
					return tailscalecli.ErrWorkflowRequired
				}
				return b.WithLease(ctx, func() error {
					call, cancel := context.WithCancel(ctx)
					stop := installedCancelBridge(b.lifetime, cancel)
					defer stop()
					defer cancel()
					if b.lifetime.Err() != nil {
						cancel()
					}
					if err := selected.Validate(s.cfg); err != nil {
						return err
					}
					var err error
					value, err = a.session.AcknowledgedPublication(call, a.binding)
					s.mu.RLock()
					terminal := s.installedTerminal
					s.mu.RUnlock()
					var refusal error
					if terminal || a.lifetime.Err() != nil || b.lifetime.Err() != nil {
						refusal = tailscalecli.ErrWorkflowRequired
					}
					return errors.Join(err, refusal, selected.Validate(s.cfg), call.Err(), a.lifetime.Err(), b.lifetime.Err())
				})
			})
			if err != nil {
				return tailscalecli.AcknowledgedPublication{}, err
			}
			return value, nil
		}
		backend.BeforePublish = func(ctx context.Context) error {
			return setup.operation(ctx, func(ctx context.Context, a *installedSetupState) error {
				a.mu.Lock()
				b := a.backend
				a.mu.Unlock()
				if b == nil {
					return tailscalecli.ErrWorkflowRequired
				}
				call, cancel := context.WithCancel(ctx)
				stop := installedCancelBridge(b.lifetime, cancel)
				defer stop()
				defer cancel()
				if b.lifetime.Err() != nil {
					cancel()
				}
				if err := a.session.CheckReadiness(call, selected.Binding(), b); err != nil {
					return err
				}
				if b.lifetime.Err() != nil || a.lifetime.Err() != nil {
					return context.Canceled
				}
				if setup.beforePublish != nil {
					return errors.Join(setup.beforePublish(call), call.Err())
				}
				return call.Err()
			})
		}
		// The core calls Publish ONLY after validating the exact proposal and all
		// six actual wire acknowledgements. No removal or recovery grant exists.
		backend.Publish = func(ctx context.Context) error {
			return setup.Publish(ctx, tailscalecli.Consent{Accepted: true, Scope: "production", NodeID: p.NodeID, Origin: selection.Origin, HTTPSPort: port, BackendPort: s.cfg.Port, ReservationID: p.ReservationID, RouteConfirmation: p.Confirmation, PersistentRouteAccepted: true, CheckToWriteRaceAccepted: true, PortReuseRiskAccepted: true, NoRollbackAccepted: true, NoRemoteDrainAccepted: true})
		}
		control, credentials, err = localcontrol.NewInstalledControl(s.cfg.InstanceID, selection, backend)
	} else {
		metadata, err := s.installedCLI.Metadata(selected.Binding())
		if err != nil {
			return fail(err)
		}
		selection.Node, selection.Profile = metadata.NodeID, string(metadata.Profile)
		control, credentials, err = localcontrol.NewInstalledResumeControl(s.cfg.InstanceID, selection, backend)
	}
	if err != nil {
		return fail(err)
	}
	if setup.state != nil {
		if err := setup.state.session.BindRuntimeGeneration(ctx, selected.Binding(), credentials.Run); err != nil {
			control.Retire()
			return fail(err)
		}
	}
	if err := s.pinInstalledGeneration(credentials.Run); err != nil {
		control.Retire()
		return fail(err)
	}
	return control, credentials, nil
}

// Initialization happens before any runner/listener exposure, never by replacing
// an already visible generation. Operator callers pin the core's actual Run.
func (s *Server) initializeInstalledGeneration() error {
	var generation [16]byte
	if _, err := rand.Read(generation[:]); err != nil {
		return err
	}
	return s.pinInstalledGeneration(hex.EncodeToString(generation[:]))
}

func (s *Server) pinInstalledGeneration(generation string) error {
	s.installedRunMu.Lock()
	defer s.installedRunMu.Unlock()
	if generation == "" || s.installedRun != "" {
		return tailscalecli.ErrWorkflowRequired
	}
	s.installedRun = generation
	return nil
}

// Only a wholly ordinary unavailable tree is a successful negative observation.
// Cancellation, uncertainty and unknown siblings are bounded refusals.
func installedInvitationObservation(err error) (bool, error) {
	if err == nil {
		return true, nil
	}
	if installedInvitationAbsent(err) {
		return false, nil
	}
	return false, redactInstalledEvidence(err)
}

func installedInvitationAbsent(err error) bool {
	if many, ok := err.(interface{ Unwrap() []error }); ok {
		children := many.Unwrap()
		if len(children) == 0 {
			return false
		}
		for _, child := range children {
			if !installedInvitationAbsent(child) {
				return false
			}
		}
		return true
	}
	if one, ok := err.(interface{ Unwrap() error }); ok && one.Unwrap() != nil {
		return installedInvitationAbsent(one.Unwrap())
	}
	return err == deviceauth.ErrBootstrapUnavailable || err == deviceauth.ErrInvitationExpired ||
		err == deviceauth.ErrInvitationBurned || err == deviceauth.ErrRateLimited
}
