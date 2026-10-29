//go:build herdr_installed_inert_test

package tailscalecli

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

var installedSetupRoutes sync.Map // inert manager -> observation-only route control

// SetInstalledSetupRouteAbsentFixture simulates external loss, never a Serve mutation.
func SetInstalledSetupRouteAbsentFixture(m *Manager) {
	control, ok := installedSetupRoutes.Load(m)
	if !ok {
		panic("inert route fixture unavailable")
	}
	control.(func(bool))(false)
}

// SetInstalledSetupRoutePresentFixture simulates a concurrent external route
// reappearance between consent and the manager's fresh absence checks.
func SetInstalledSetupRoutePresentFixture(m *Manager) {
	control, ok := installedSetupRoutes.Load(m)
	if !ok {
		panic("inert route fixture unavailable")
	}
	control.(func(bool))(true)
}

// NewInstalledSetupInertFixture starts genuinely empty. It replaces only CLI
// observations/resources, never ownership, consent, attempts or durable writes.
func NewInstalledSetupInertFixture(t *testing.T) (*Manager, string, string, string, func() int) {
	t.Helper()
	base, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	state, coordination := filepath.Join(base, "registration"), filepath.Join(base, "coordination")
	for _, p := range []string{state, coordination} {
		if err := os.Mkdir(p, 0700); err != nil {
			t.Fatal(err)
		}
	}
	binary := "/inert-fixture/tailscale"
	var mu sync.Mutex
	mutations := 0
	route := `{}`
	client := newTestClientForPlatform(binary, func(ctx context.Context, _ string, args ...string) (commandResult, error) {
		if err := ctx.Err(); err != nil {
			return commandResult{}, err
		}
		mu.Lock()
		defer mu.Unlock()
		var output string
		switch strings.Join(args, " ") {
		case "status --json":
			output = installedFixtureStatus
		case "version --json --daemon":
			output = installedFixtureVersion
		case "serve status --json":
			output = route
		case "serve --bg --https=8443 --set-path=/ http://127.0.0.1:18377":
			mutations++
			route = installedFixtureRoute
		case "serve --bg --https=8443 --set-path=/ off":
			mutations++
			route = `{}`
		default:
			t.Errorf("unexpected synthetic command: %v", args)
			return commandResult{}, ErrUnsupported
		}
		return commandResult{stdout: []byte(output), dispatched: true}, nil
	}, "darwin", "arm64")
	m := &Manager{stateRoot: state, coordinationRoot: coordination, client: client, fixtureMutations: true, skipBackendReadiness: true}
	installedSetupRoutes.Store(m, func(present bool) {
		mu.Lock()
		defer mu.Unlock()
		if present {
			route = installedFixtureRoute
		} else {
			route = `{}`
		}
	})
	t.Cleanup(func() { installedSetupRoutes.Delete(m) })
	return m, state, coordination, binary, func() int { mu.Lock(); defer mu.Unlock(); return mutations }
}

// SetInstalledSetupContentionFixture observes existing lock contention only.
// Configure before starting the owner; it cannot change lock outcomes.
func SetInstalledSetupContentionFixture(m *Manager, observe func(string)) {
	m.fixtureLockWaitHook = observe
}

// SetInstalledSetupReadinessFixture must be called before handing the setup
// control to another goroutine. No production build includes this seam.
func SetInstalledSetupReadinessFixture(s *InstalledSetupSession, observe func(context.Context) error) {
	s.observeReadiness = observe
}

// SetInstalledAcknowledgementCompletionFixture injects only a post-coordination
// observation/error. Real unlock/close has already completed; no descriptor is touched.
// Configure before concurrent use.
func SetInstalledAcknowledgementCompletionFixture(s *InstalledSetupSession, observe func() error) {
	s.afterAcknowledgementCoordination = observe
}

// Configure before concurrent use. This observer cannot change queue outcomes.
func SetInstalledSetupQueueObserverFixture(s *InstalledSetupSession, observe func()) {
	s.observeQueue = observe
}

// WithInstalledSetupSerializationFixture occupies the real owned serial queue,
// without changing registration, readiness or admission.
func WithInstalledSetupSerializationFixture(ctx context.Context, s *InstalledSetupSession, body func() error) error {
	return s.operation(ctx, s.binding, func(context.Context) error { return body() })
}
