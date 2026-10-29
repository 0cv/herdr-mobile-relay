package tailscalecli

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// Inert CLI and temporary private journals only: no listener, child process,
// service controller, real CLI, or device store is opened by these tests.
func TestInstalledResumePreservesRouteAcrossSessions(t *testing.T) {
	f := newFakeCLI(t)
	m := newFixtureManager(t, f, "installed")
	r := fixtureRequest(true)
	r.Scope, r.Consent.Scope = "production", "production"
	if err := m.Publish(context.Background(), r); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(filepath.Join(m.stateRoot, "registration.json"))
	if err != nil {
		t.Fatal(err)
	}
	stop := errors.New("fixture backend stopped")
	for i := 0; i < 2; i++ {
		err := m.WithInstalledResume(context.Background(), r.InstallationID, r.Origin, r.HTTPSPort, r.BackendPort,
			func(ctx context.Context, check func() (RouteStatus, error)) error {
				status, err := check()
				if err != nil || status.Readiness != ReadinessReady {
					t.Fatalf("resume readiness: %+v %v", status, err)
				}
				// A concurrent setup/service cannot acquire this instance's ownership.
				cancelled, cancel := context.WithCancel(ctx)
				defer cancel()
				waited := false
				m.fixtureLockWaitHook = func(string) { waited = true; cancel() }
				if err := m.WithInstalledResume(cancelled, r.InstallationID, r.Origin, r.HTTPSPort, r.BackendPort,
					func(context.Context, func() (RouteStatus, error)) error { t.Fatal("second writer entered"); return nil }); err == nil {
					t.Fatal("competing session accepted")
				}
				m.fixtureLockWaitHook = nil
				if !waited {
					t.Fatal("competing session did not contend for service authority")
				}
				return stop
			})
		if !errors.Is(err, stop) {
			t.Fatalf("session: %v", err)
		}
	}
	after, err := os.ReadFile(filepath.Join(m.stateRoot, "registration.json"))
	if err != nil || string(before) != string(after) || f.mutationCalls() != 1 || f.serve != fixtureRoute {
		t.Fatalf("resume/stop changed persistent evidence or route: %v", err)
	}
	f.serve = `{}`
	if err := m.WithInstalledResume(context.Background(), r.InstallationID, r.Origin, r.HTTPSPort, r.BackendPort,
		func(context.Context, func() (RouteStatus, error)) error {
			t.Fatal("missing route admitted")
			return nil
		}); err == nil {
		t.Fatal("missing route accepted")
	}
	if f.mutationCalls() != 1 {
		t.Fatal("resume repaired a missing route")
	}
}

func TestInstalledResumeProductionGateBeforeCLI(t *testing.T) {
	f := newFakeCLI(t)
	m := newPolicyManager(t, f, "darwin", "arm64", "closed")
	f.calls = nil
	err := m.WithInstalledResume(context.Background(), "install-fixture", "https://herdr.tailnet.ts.net:8443", 8443, 18377,
		func(context.Context, func() (RouteStatus, error)) error {
			t.Fatal("production callback entered")
			return nil
		})
	if !errors.Is(err, ErrUnsupported) || len(f.calls) != 0 {
		t.Fatalf("production gate: %v, CLI calls=%d", err, len(f.calls))
	}
	binding := installedFixtureBinding(t, m, fixtureRequest(true))
	err = m.WithInstalledSession(context.Background(), binding, func(context.Context, *InstalledSession) error {
		t.Fatal("data binding enabled production")
		return nil
	})
	if !errors.Is(err, ErrUnsupported) || len(f.calls) != 0 {
		t.Fatalf("binding production gate: %v", err)
	}
	for _, root := range []string{binding.input.RuntimeRoot, binding.input.CacheRoot} {
		if _, err := os.Lstat(root); !os.IsNotExist(err) {
			t.Fatalf("production created resources: %v", err)
		}
	}
}
