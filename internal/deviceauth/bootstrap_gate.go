package deviceauth

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"

	"github.com/0cv/herdr-mobile-relay/internal/transport"
)

// ErrBootstrapGateClosed is a transient refusal. In particular, the E2EE
// client must not discard a credential merely because managed startup has not
// completed owner and route validation yet.
var ErrBootstrapGateClosed = errors.New("device authentication is temporarily unavailable")

// BootstrapGate is the stable resolver installed in transport.Hub for a
// managed Tailscale run. A required authority admission guard is composed only
// from live channels exported by the real SessionAuthority; the guard reads
// those channels directly so watch EOF is effective before its monitor can
// acquire the authority operation lock.
type BootstrapGate struct {
	mu sync.RWMutex

	store                     *Store
	invitationOpen            bool
	revoked                   atomic.Bool
	attached                  bool
	requireAuthorityAdmission bool
	watchEnded                <-chan struct{}
	authorityInvalidated      <-chan struct{}
}

func NewBootstrapGate() *BootstrapGate { return &BootstrapGate{} }

// RequireAuthorityAdmission makes a bound live SessionAuthority watch a
// prerequisite for arm and both handshake resolver operations. Managed
// Tailscale servers set this before publishing the gate to the Hub.
func (g *BootstrapGate) RequireAuthorityAdmission() {
	if g == nil {
		return
	}
	g.mu.Lock()
	g.requireAuthorityAdmission = true
	g.mu.Unlock()
}

// BindAuthorityAdmission installs only the actual watch completion and
// in-process invalidation channels. It accepts no owner/readiness booleans.
func (g *BootstrapGate) BindAuthorityAdmission(watchEnded, invalidated <-chan struct{}) error {
	if g == nil || watchEnded == nil || invalidated == nil {
		return ErrBootstrapGateClosed
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.revoked.Load() || (g.watchEnded != nil && (g.watchEnded != watchEnded || g.authorityInvalidated != invalidated)) {
		return ErrBootstrapGateClosed
	}
	g.watchEnded = watchEnded
	g.authorityInvalidated = invalidated
	return nil
}

// Attach publishes the initialized store without changing its contents. The
// gate remains closed to bootstrap invitations until durable owner-side
// arming. Revoke is an atomic one-way latch and never waits behind a handshake.
func (g *BootstrapGate) Attach(store *Store) error {
	if g == nil || store == nil {
		return ErrBootstrapGateClosed
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.revoked.Load() || g.attached {
		return ErrBootstrapGateClosed
	}
	g.store = store
	g.attached = true
	return nil
}

// Open enables invitation resolution/completion after durable owner-side
// arming. It is idempotent while active but cannot undo Revoke or authority
// watch loss.
func (g *BootstrapGate) Open() error {
	if g == nil {
		return ErrBootstrapGateClosed
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.revoked.Load() || !g.attached || g.store == nil || g.admissionErrorLocked() != nil {
		return ErrBootstrapGateClosed
	}
	g.invitationOpen = true
	return nil
}

// ArmBootstrapInvitation stores a fresh one-use invitation without resetting
// enrolled devices. The gate transition is made by the store's final
// transactional admission callback, after its durable write but before its
// successful return; readers remain excluded by mu until the callback exits.
// A watch loss or revocation after that commit leaves the durable record in
// place, but the dynamic guard denies resolution and completion.
func (g *BootstrapGate) ArmBootstrapInvitation(secret []byte, name, locale string, admit func() error, beforeCommit ...func() error) error {
	if g == nil {
		return ErrBootstrapGateClosed
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.revoked.Load() || !g.attached || g.store == nil || g.admissionErrorLocked() != nil {
		return ErrBootstrapGateClosed
	}
	if admit != nil {
		if err := admit(); err != nil {
			return err
		}
	}
	finalAdmission := func() error {
		if err := g.admissionErrorLocked(); err != nil {
			return err
		}
		for _, check := range beforeCommit {
			if check != nil {
				if err := check(); err != nil {
					return err
				}
			}
		}
		if err := g.admissionErrorLocked(); err != nil {
			return err
		}
		if g.revoked.Load() {
			return ErrBootstrapGateClosed
		}
		g.invitationOpen = true
		return nil
	}
	if err := g.store.ArmBootstrapInvitationTransactional(secret, name, locale, finalAdmission); err != nil {
		return err
	}
	if g.revoked.Load() || g.admissionErrorLocked() != nil {
		g.invitationOpen = false
		return ErrBootstrapGateClosed
	}
	return nil
}

// Revoke immediately latches denial without waiting for an in-flight resolver
// or lifecycle operation. The stable store is never detached or destroyed.
func (g *BootstrapGate) Revoke() {
	if g != nil {
		g.revoked.Store(true)
	}
}

// OpenStatus reports a currently usable invitation gate without exposing its
// Store. Raw watch EOF and authority invalidation are checked synchronously.
func (g *BootstrapGate) OpenStatus() bool {
	if g == nil {
		return false
	}
	g.mu.RLock()
	defer g.mu.RUnlock()
	return g.invitationOpen && !g.revoked.Load() && g.store != nil && g.admissionErrorLocked() == nil
}

func (g *BootstrapGate) ResolveE2EESecret(ctx context.Context, selector transport.E2EEAuthSelector) ([]byte, error) {
	if g == nil {
		return nil, ErrBootstrapGateClosed
	}
	g.mu.RLock()
	store, ok := g.storeForLocked(selector)
	guard := g.admissionProbeLocked()
	g.mu.RUnlock()
	if !ok || guard() != nil {
		return nil, ErrBootstrapGateClosed
	}
	secret, err := store.ResolveE2EESecret(ctx, selector)
	if err != nil {
		return nil, err
	}
	if guard() != nil {
		clear(secret)
		return nil, ErrBootstrapGateClosed
	}
	return secret, nil
}

func (g *BootstrapGate) CompleteE2EEAuth(ctx context.Context, selector transport.E2EEAuthSelector, authenticated bool) (transport.E2EEAuthResult, error) {
	if g == nil {
		return transport.E2EEAuthResult{}, ErrBootstrapGateClosed
	}
	g.mu.RLock()
	store, ok := g.storeForLocked(selector)
	guard := g.admissionProbeLocked()
	g.mu.RUnlock()
	if !ok || guard() != nil {
		return transport.E2EEAuthResult{}, ErrBootstrapGateClosed
	}
	result, err := store.completeE2EEAuth(ctx, selector, authenticated, guard)
	if err != nil {
		return transport.E2EEAuthResult{}, err
	}
	if guard() != nil {
		clear(result.CredentialSecret)
		return transport.E2EEAuthResult{}, ErrBootstrapGateClosed
	}
	return result, nil
}

func (g *BootstrapGate) IsE2EEAuthRejected(err error) bool {
	if errors.Is(err, ErrBootstrapGateClosed) || g == nil {
		return false
	}
	g.mu.RLock()
	store := g.store
	g.mu.RUnlock()
	return store != nil && store.IsE2EEAuthRejected(err)
}

func (g *BootstrapGate) admissionProbeLocked() func() error {
	watchEnded := g.watchEnded
	invalidated := g.authorityInvalidated
	required := g.requireAuthorityAdmission
	revoked := &g.revoked
	return func() error {
		if revoked.Load() || (required && (watchEnded == nil || invalidated == nil)) {
			return ErrBootstrapGateClosed
		}
		for _, channel := range []<-chan struct{}{watchEnded, invalidated} {
			if channel == nil {
				continue
			}
			select {
			case <-channel:
				return ErrBootstrapGateClosed
			default:
			}
		}
		return nil
	}
}

func (g *BootstrapGate) admissionErrorLocked() error {
	if g.requireAuthorityAdmission && (g.watchEnded == nil || g.authorityInvalidated == nil) {
		return ErrBootstrapGateClosed
	}
	for _, invalidation := range []<-chan struct{}{g.watchEnded, g.authorityInvalidated} {
		if invalidation == nil {
			continue
		}
		select {
		case <-invalidation:
			return ErrBootstrapGateClosed
		default:
		}
	}
	return nil
}

func (g *BootstrapGate) storeForLocked(selector transport.E2EEAuthSelector) (*Store, bool) {
	if g.revoked.Load() || g.store == nil || g.admissionErrorLocked() != nil {
		return nil, false
	}
	if selector.Kind == transport.E2EEAuthInvitation && !g.invitationOpen {
		return nil, false
	}
	return g.store, true
}
