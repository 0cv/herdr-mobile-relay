package deviceauth

import (
	"context"
	"errors"
	"time"
)

// BootstrapInvitationSnapshot contains value-only copies of the actual stored
// selector and encoded secret. Acquisition is not operator authentication.
type BootstrapInvitationSnapshot struct {
	InvitationID string
	Version      uint64
	Secret       string
	ExpiresAt    time.Time
	Pending      bool
}

func (BootstrapInvitationSnapshot) String() string     { return "bootstrap invitation (redacted)" }
func (v BootstrapInvitationSnapshot) GoString() string { return v.String() }

var ErrBootstrapUnavailable = errors.New("current bootstrap invitation unavailable")

// CurrentBootstrapInvitation never resolves, refreshes, repairs or writes.
// Pending invitations are deliberately refused: their retry belongs to the
// already-created credential, not another operator enrollment.
func (s *Store) CurrentBootstrapInvitation(ctx context.Context) (BootstrapInvitationSnapshot, error) {
	if s == nil || ctx == nil {
		return BootstrapInvitationSnapshot{}, ErrBootstrapUnavailable
	}
	if err := s.mu.acquire(ctx, false); err != nil {
		return BootstrapInvitationSnapshot{}, err
	}
	defer s.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return BootstrapInvitationSnapshot{}, err
	}
	if s.managedUncertain || s.managedArmBaseline == nil {
		return BootstrapInvitationSnapshot{}, ErrManagedArmRecovery
	}
	current, err := readManagedStoreSnapshot(s.dir, s.path)
	if err != nil || !sameManagedStoreSnapshot(*s.managedArmBaseline, current) {
		return BootstrapInvitationSnapshot{}, ErrManagedArmRecovery
	}
	record := s.state.Invitation
	if s.state.SchemaVersion != storeSchemaVersion || record == nil || record.FailedAttempts < 0 || record.InvitationID != bootstrapInvitationID || record.Version != 1 || !validSecret(record.Secret) || record.Role != RoleController || record.PendingCredentialID != "" {
		return BootstrapInvitationSnapshot{}, ErrBootstrapUnavailable
	}
	now := s.now().UTC()
	if !now.Before(record.ExpiresAt) {
		return BootstrapInvitationSnapshot{}, ErrInvitationExpired
	}
	if record.FailedAttempts >= maxInviteAttempts {
		return BootstrapInvitationSnapshot{}, ErrInvitationBurned
	}
	if now.Before(record.NextAttemptAt) {
		return BootstrapInvitationSnapshot{}, ErrRateLimited
	}
	if err := ctx.Err(); err != nil {
		return BootstrapInvitationSnapshot{}, err
	}
	return BootstrapInvitationSnapshot{InvitationID: record.InvitationID, Version: record.Version, Secret: record.Secret, ExpiresAt: record.ExpiresAt}, nil
}

// CurrentBootstrapInvitation holds gate exclusion across store acquisition and
// checks the atomic cancellation/revocation guards again before releasing data.
func (g *BootstrapGate) CurrentBootstrapInvitation(ctx context.Context) (BootstrapInvitationSnapshot, error) {
	if g == nil || ctx == nil {
		return BootstrapInvitationSnapshot{}, ErrBootstrapGateClosed
	}
	if err := g.mu.acquire(ctx, true); err != nil {
		return BootstrapInvitationSnapshot{}, err
	}
	defer g.mu.RUnlock()
	guard := g.admissionProbeLocked()
	if !g.invitationOpen || g.store == nil || guard() != nil {
		return BootstrapInvitationSnapshot{}, ErrBootstrapGateClosed
	}
	value, err := g.store.CurrentBootstrapInvitation(ctx)
	if err != nil {
		return BootstrapInvitationSnapshot{}, err
	}
	if err := ctx.Err(); err != nil {
		return BootstrapInvitationSnapshot{}, err
	}
	if guard() != nil {
		return BootstrapInvitationSnapshot{}, ErrBootstrapGateClosed
	}
	return value, nil
}
