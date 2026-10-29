package tailscalecli

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestInstalledResumeCoexistence(t *testing.T) {
	root := t.TempDir()
	fixture := &coexistenceCLI{t: t, backends: make(map[int]int)}
	coord := filepath.Join(root, "coord")
	if err := os.Mkdir(coord, 0700); err != nil {
		t.Fatal(err)
	}
	makeManager := func(name string) *Manager {
		state := filepath.Join(root, name)
		if err := os.Mkdir(state, 0700); err != nil {
			t.Fatal(err)
		}
		m, err := NewManager(state, coord, newTestClientForPlatform("/fixture path/tailscale", fixture.run, "darwin", "arm64"))
		if err != nil {
			t.Fatal(err)
		}
		m.fixtureMutations = true
		m.skipBackendReadiness = true
		return m
	}
	prod, dev := makeManager("prod"), makeManager("dev")
	pr := fixtureRequest(true)
	pr.Scope = "production"
	pr.Consent.Scope = "production"
	dr := fixtureRequest(true)
	dr.InstallationID = "development-other"
	dr.HTTPSPort = 9443
	dr.BackendPort = 19375
	dr.Origin = "https://herdr.tailnet.ts.net:9443"
	dr.Consent.HTTPSPort = dr.HTTPSPort
	dr.Consent.BackendPort = dr.BackendPort
	dr.Consent.Origin = dr.Origin
	if err := prod.Publish(context.Background(), pr); err != nil {
		t.Fatal(err)
	}
	journal, _ := os.ReadFile(filepath.Join(prod.stateRoot, journalName))
	reservation, _ := os.ReadFile(filepath.Join(coord, "backend-port-18377.json"))
	err := prod.WithInstalledResume(context.Background(), pr.InstallationID, pr.Origin, pr.HTTPSPort, pr.BackendPort,
		func(ctx context.Context, check func() (RouteStatus, error)) error {
			if err := dev.Publish(ctx, dr); err != nil {
				t.Fatal(err)
			}
			if err := dev.Unpublish(ctx, dr.Consent); err != nil {
				t.Fatal(err)
			}
			// Separate manager handles contend on disk, not a process-local flag.
			duplicate := *prod
			for _, operation := range []func(context.Context) error{
				func(c context.Context) error {
					return duplicate.WithInstalledResume(c, pr.InstallationID, pr.Origin, pr.HTTPSPort, pr.BackendPort, func(context.Context, func() (RouteStatus, error)) error { t.Fatal("duplicate entered"); return nil })
				},
				func(c context.Context) error { return duplicate.Unpublish(c, pr.Consent) },
				func(c context.Context) error { return duplicate.Publish(c, pr) },
			} {
				c, cancel := context.WithCancel(ctx)
				waited := false
				duplicate.fixtureLockWaitHook = func(string) { waited = true; cancel() }
				err := operation(c)
				cancel()
				if err == nil || !waited {
					t.Fatalf("same-instance operation not excluded: %v", err)
				}
			}
			// A different journal cannot acquire the already-owned backend either.
			c, cancel := context.WithCancel(ctx)
			dev.fixtureLockWaitHook = func(string) { cancel() }
			if err := dev.withLifetimeOwnership(c, pr.BackendPort, func() error { t.Fatal("backend duplicate entered"); return nil }); err == nil {
				t.Fatal("backend ownership lost")
			}
			cancel()
			dev.fixtureLockWaitHook = nil
			status, err := check()
			if err != nil || status.Readiness != ReadinessReady {
				t.Fatalf("production lost readiness: %+v %v", status, err)
			}
			return nil
		})
	if err != nil {
		t.Fatal(err)
	}
	after, _ := os.ReadFile(filepath.Join(prod.stateRoot, journalName))
	afterReservation, _ := os.ReadFile(filepath.Join(coord, "backend-port-18377.json"))
	if string(after) != string(journal) || string(afterReservation) != string(reservation) {
		t.Fatal("production evidence changed")
	}
	if len(fixture.backends) != 1 || fixture.backends[pr.HTTPSPort] != pr.BackendPort {
		t.Fatal("production route changed")
	}
	// Retirement releases ownership for explicit removal.
	if err := prod.Unpublish(context.Background(), pr.Consent); err != nil {
		t.Fatal(err)
	}
}
