package tailscalecli

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
)

// AcknowledgedPublication is immutable historical DATA, never readiness or authority.
// Only an owned setup session can produce a nonzero value.
type AcknowledgedPublication struct{ data PublicationSnapshot }

// PublicationSnapshot contains no private records or paths. Digests bind canonical
// JSON encodings of complete parsed record values, NOT original file bytes.
type PublicationSnapshot struct {
	Selection, RegistrationDigest, ReservationDigest                    [32]byte
	Installation, Node, Profile, Origin, Attempt, Operation, Generation string
	HTTPSPort, BackendPort                                              int
}

func (AcknowledgedPublication) String() string                  { return "publication data [redacted]" }
func (a AcknowledgedPublication) GoString() string              { return a.String() }
func (PublicationSnapshot) String() string                      { return "publication snapshot [redacted]" }
func (a PublicationSnapshot) GoString() string                  { return a.String() }
func (a AcknowledgedPublication) Snapshot() PublicationSnapshot { return a.data }

func (s *InstalledSetupSession) AcknowledgedPublication(ctx context.Context, binding InstalledBinding) (AcknowledgedPublication, error) {
	var out AcknowledgedPublication
	if ctx == nil {
		return out, ErrWorkflowRequired
	}
	err := s.operation(ctx, binding, func(call context.Context) error {
		err := s.manager.withOwnedReadOnlyCoordination(call, s.inspection.Identity.NodeID, func() error {
			if s.receipt == nil || s.acknowledged == nil || s.runtimeGeneration == "" || !validLabel(s.runtimeGeneration) {
				return ErrUncertain
			}
			r, err := s.manager.readRegistration()
			if err != nil {
				return err
			}
			if r == nil || *r != *s.receipt || r.State != StateRegistered || !r.MutationAcknowledged || r.ReservationID != "" {
				return ErrUncertain
			}
			b, err := s.manager.readInstalledReservation(r)
			if err != nil {
				return err
			}
			if *b != *s.acknowledged {
				return ErrUncertain
			}
			in := binding.input
			if r.Scope != "production" || r.InstallationID != in.InstallationID || r.NodeID != s.inspection.Identity.NodeID || r.Profile != s.inspection.Profile || r.BinaryPath != in.Binary || r.HTTPSPort != in.HTTPSPort || r.BackendPort != in.BackendPort || r.Path != "/" || r.Backend != fmt.Sprintf("http://127.0.0.1:%d", in.BackendPort) || b.Origin != in.Origin || s.reservation.ReservationID == "" {
				return ErrUncertain
			}
			fingerprint, err := binding.Fingerprint()
			if err != nil {
				return err
			}
			rb, err := json.Marshal(r)
			if err != nil {
				return err
			}
			bb, err := json.Marshal(b)
			if err != nil {
				return err
			}
			out.data = PublicationSnapshot{Selection: fingerprint, RegistrationDigest: sha256.Sum256(rb), ReservationDigest: sha256.Sum256(bb), Installation: in.InstallationID, Node: r.NodeID, Profile: string(r.Profile), Origin: in.Origin, Attempt: s.reservation.ReservationID, Operation: r.OperationID, Generation: s.runtimeGeneration, HTTPSPort: in.HTTPSPort, BackendPort: in.BackendPort}
			return call.Err()
		})
		if s.manager.fixtureMutations && s.afterAcknowledgementCoordination != nil {
			err = errors.Join(err, s.afterAcknowledgementCoordination())
		}
		return errors.Join(err, binding.Validate(), call.Err())
	})
	if err != nil {
		return AcknowledgedPublication{}, err
	}
	return out, nil
}
