package deviceauth

import (
	"context"
	"errors"
	"sync"

	"github.com/0cv/herdr-mobile-relay/internal/transport"
)

// ErrBootstrapGateClosed is a transient refusal. In particular, the E2EE
// client must not discard a credential merely because managed startup has not
// completed owner and route validation yet.
var ErrBootstrapGateClosed = errors.New("device authentication is temporarily unavailable")

// BootstrapGate is the stable resolver installed in transport.Hub for a
// managed Tailscale run. It can be attached once, and bootstrap invitations
// stay unusable until the owner has persisted an invitation and opens the
// gate. Revoke is irreversible for this gate instance.
type BootstrapGate struct {
	mu sync.RWMutex

	store          *Store
	invitationOpen bool
	revoked        bool
	attached       bool
}

func NewBootstrapGate() *BootstrapGate { return &BootstrapGate{} }

// Attach publishes the initialized store without changing its contents. The
// gate remains closed to bootstrap invitations until Open succeeds.
func (g *BootstrapGate) Attach(store *Store) error {
	if g == nil || store == nil {
		return ErrBootstrapGateClosed
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.revoked || g.attached {
		return ErrBootstrapGateClosed
	}
	g.store = store
	g.attached = true
	return nil
}

// Open enables invitation resolution/completion after durable owner-side
// arming. It is idempotent while active but cannot undo Revoke.
func (g *BootstrapGate) Open() error {
	if g == nil {
		return ErrBootstrapGateClosed
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.revoked || !g.attached || g.store == nil {
		return ErrBootstrapGateClosed
	}
	g.invitationOpen = true
	return nil
}

// ArmBootstrapInvitation runs final admission while excluding Revoke, durably
// stores the invitation with exact-state rollback on failure, then publishes
// the already-committed record by opening the gate. The final store callback is
// the last fallible admission point while this mutex is held; no resolver or
// Revoke can interleave between that commit and OpenStatus becoming true. Once
// the gate opens, callers must treat a lost acknowledgement as
// committed/ambiguous and never roll back the invitation.
func (g *BootstrapGate) ArmBootstrapInvitation(secret []byte, name, locale string, admit func() error, beforeCommit ...func() error) error {
	if g == nil {
		return ErrBootstrapGateClosed
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.revoked || !g.attached || g.store == nil {
		return ErrBootstrapGateClosed
	}
	if admit != nil {
		if err := admit(); err != nil {
			return err
		}
	}
	if err := g.store.ArmBootstrapInvitationTransactional(secret, name, locale, beforeCommit...); err != nil {
		return err
	}
	g.invitationOpen = true
	return nil
}

// Revoke closes invitation use and waits for every resolver operation already
// in flight to leave the store before returning.
func (g *BootstrapGate) Revoke() {
	if g == nil {
		return
	}
	g.mu.Lock()
	g.invitationOpen = false
	g.revoked = true
	g.mu.Unlock()
}

// OpenStatus reports the invitation gate state without exposing its Store.
func (g *BootstrapGate) OpenStatus() bool {
	if g == nil {
		return false
	}
	g.mu.RLock()
	defer g.mu.RUnlock()
	return g.invitationOpen && !g.revoked && g.store != nil
}

func (g *BootstrapGate) ResolveE2EESecret(ctx context.Context, selector transport.E2EEAuthSelector) ([]byte, error) {
	if g == nil {
		return nil, ErrBootstrapGateClosed
	}
	g.mu.RLock()
	defer g.mu.RUnlock()
	store, ok := g.storeForLocked(selector)
	if !ok {
		return nil, ErrBootstrapGateClosed
	}
	return store.ResolveE2EESecret(ctx, selector)
}

func (g *BootstrapGate) CompleteE2EEAuth(ctx context.Context, selector transport.E2EEAuthSelector, authenticated bool) (transport.E2EEAuthResult, error) {
	if g == nil {
		return transport.E2EEAuthResult{}, ErrBootstrapGateClosed
	}
	g.mu.RLock()
	defer g.mu.RUnlock()
	store, ok := g.storeForLocked(selector)
	if !ok {
		return transport.E2EEAuthResult{}, ErrBootstrapGateClosed
	}
	return store.CompleteE2EEAuth(ctx, selector, authenticated)
}

func (g *BootstrapGate) IsE2EEAuthRejected(err error) bool {
	if errors.Is(err, ErrBootstrapGateClosed) {
		return false
	}
	if g == nil {
		return false
	}
	g.mu.RLock()
	store := g.store
	g.mu.RUnlock()
	return store != nil && store.IsE2EEAuthRejected(err)
}

func (g *BootstrapGate) storeForLocked(selector transport.E2EEAuthSelector) (*Store, bool) {
	if g.revoked || g.store == nil {
		return nil, false
	}
	if selector.Kind == transport.E2EEAuthInvitation && !g.invitationOpen {
		return nil, false
	}
	return g.store, true
}
