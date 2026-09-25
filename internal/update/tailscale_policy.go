package update

import (
	"errors"
	"os"
	"strings"
)

// The resolved policy is immutable for a relay lifetime. Raw process policy is
// re-read at each admission boundary and may veto, never override, that policy.
// Jobs and detached environments carry launch-scoped snapshots, not observations
// of relay.env. Independently switched on-disk configuration cannot be detected
// when both snapshots remain stale: S6 ownership/serialization is still required.
func transportAdmission(resolved, current string, requireCurrent bool) error {
	resolved = strings.ToLower(strings.TrimSpace(resolved))
	current = strings.ToLower(strings.TrimSpace(current))
	if resolved == "tailscale" || current == "tailscale" || resolved == "tailscale-external" || current == "tailscale-external" {
		return errors.New("Phone-managed updates are unavailable for foreground Tailscale Serve transports; stop the foreground relay and use the verified manual-update procedure")
	}
	legacy := func(value string) bool { return value == "cloudflare" || value == "gateway" }
	if !legacy(resolved) || (requireCurrent && current == "") || (current != "" && (!legacy(current) || current != resolved)) {
		return errors.New("Phone-managed updates require an explicit, consistent transport policy; stop and verify configuration before a manual update")
	}
	return nil
}

func (m *Manager) policyError() error {
	return transportAdmission(m.transport, os.Getenv("HERDR_RELAY_TRANSPORT"), false)
}

// Do not reconcile persisted worker state when refusing admission. In particular,
// a projection does not claim an existing foreign worker has been stopped.
func (m *Manager) readOnlyState() State {
	state, err := readState(m.statePath())
	if err != nil || !validState(state.State) {
		return m.state
	}
	return state
}
