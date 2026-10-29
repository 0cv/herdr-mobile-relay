//go:build herdr_installed_inert_test

package tailscalecli

import (
	"context"
	"testing"
	"time"
)

// ScriptInstalledInertStartup modifies only a manager created by the inert
// fixture. All observations remain in-memory; no fallback executable exists.
func ScriptInstalledInertStartup(t *testing.T, m *Manager, states []string, wait func(context.Context, time.Duration) error) {
	t.Helper()
	if m == nil || !m.fixtureMutations || m.client.binary != "/inert-fixture/tailscale" {
		t.Fatal("not an inert manager")
	}
	original := m.client
	index := 0
	m.client = newTestClientForPlatform(original.binary, func(ctx context.Context, _ string, args ...string) (commandResult, error) {
		if len(args) == 2 && args[0] == "status" && index < len(states) {
			state := states[index]
			index++
			if state == "incomplete" {
				return commandResult{}, ErrUncertain
			}
			return commandResult{stdout: []byte("{\"BackendState\":\"" + state + "\"}"), dispatched: true}, nil
		}
		return original.run(ctx, original.binary, args...)
	}, "darwin", "arm64")
	m.installedStartupPolicy = &installedStartupPolicy{time.Second, wait}
	t.Cleanup(func() { m.client = original; m.installedStartupPolicy = nil })
}
