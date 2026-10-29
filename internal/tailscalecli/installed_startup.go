package tailscalecli

import (
	"context"
	"time"
)

// Each acquisition/initial observation has a 20s budget. Availability delays
// are 1, 2, 4, 8, 16, 32, then 60 seconds, capped for the service lifetime.
// Waiting is not readiness. All attempt locks unwind before sleeping.
type installedStartupPolicy struct {
	attempt time.Duration
	wait    func(context.Context, time.Duration) error
}

func waitInstalledDelay(ctx context.Context, delay time.Duration) error {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return ctx.Err()
	}
}

// Every leaf must be the positively classified sentinel. Combined terminal or
// unknown evidence wins, including errors.Join(transient, terminal).
func onlyError(err, allowed error) bool {
	if err == allowed {
		return true
	}
	if many, ok := err.(interface{ Unwrap() []error }); ok {
		children := many.Unwrap()
		if len(children) == 0 {
			return false
		}
		for _, child := range children {
			if !onlyError(child, allowed) {
				return false
			}
		}
		return true
	}
	if one, ok := err.(interface{ Unwrap() error }); ok {
		return onlyError(one.Unwrap(), allowed)
	}
	return false
}

func (m *Manager) waitInstalledStartup(ctx context.Context, binding InstalledBinding, run func(context.Context, func(context.Context) (RouteStatus, error), InstalledOwnerMetadata) error) error {
	if ctx == nil {
		return ErrWorkflowRequired
	}
	in := binding.input
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := m.requireDevelopmentTuple("production", in.HTTPSPort, in.BackendPort); err != nil {
		return err
	}
	record, err := m.readRegistration()
	if err != nil {
		return err
	}
	if record == nil || record.State != StateRegistered || !record.MutationAcknowledged || record.ReservationID != "" {
		return ErrUncertain
	}
	policy := installedStartupPolicy{20 * time.Second, waitInstalledDelay}
	if m.installedStartupPolicy != nil {
		policy = *m.installedStartupPolicy
	}
	// Pin the complete acknowledged reservation at the first acquired boundary.
	var pinned *backendPortReservation
	validate := func() error {
		if err := binding.Validate(); err != nil {
			return err
		}
		reservation, err := m.readInstalledReservation(record)
		if err != nil {
			return err
		}
		if pinned == nil {
			copy := *reservation
			pinned = &copy
		}
		if *reservation != *pinned {
			return ErrUncertain
		}
		return nil
	}
	delay := time.Second
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := binding.Validate(); err != nil {
			return err
		}
		attempt, stop := context.WithTimeout(ctx, policy.attempt)
		entered := false
		err := m.acquireInstalledResume(attempt, ctx, record, in.InstallationID, in.Origin, in.HTTPSPort, in.BackendPort, validate,
			func(lifetime context.Context, check func(context.Context) (RouteStatus, error)) error {
				if err := attempt.Err(); err != nil {
					return err
				}
				stop() // The application retains only the original service lifetime.
				entered = true
				return run(lifetime, check, InstalledOwnerMetadata{NodeID: record.NodeID, Profile: record.Profile})
			})
		stop()
		if entered {
			return err
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if !onlyError(err, ErrTransientUnavailable) {
			return err
		}
		if err := policy.wait(ctx, delay); err != nil {
			return err
		}
		delay *= 2
		if delay > time.Minute {
			delay = time.Minute
		}
	}
}
