package tailscalecli

import (
	"context"
	"errors"
	"strings"
	"testing"
)

func TestExactAppStoreProfileEnablesDevelopmentWithoutLiveQualification(t *testing.T) {
	fixture := newFakeCLI(t)
	manager := newPolicyManager(t, fixture, "darwin", "arm64", "development-enabled")
	request := fixtureRequest(true)
	if err := manager.ReserveBackendPort(context.Background(), request.InstallationID, request.Scope,
		request.ExpectedNodeID, request.Origin, request.HTTPSPort, request.BackendPort, request.ReservationID); err != nil {
		t.Fatalf("exact App Store candidate was not enabled for development: %v", err)
	}
	request.Consent.RouteConfirmation = PublishRouteConfirmation(request.ExpectedNodeID, request.Origin, request.HTTPSPort, request.BackendPort)
	if err := manager.Publish(context.Background(), request); err != nil {
		t.Fatalf("route-bound development publication failed for exact profile: %v", err)
	}
	status, err := manager.VerifyRegisteredRoute(context.Background(), "development", request.InstallationID,
		request.Origin, request.HTTPSPort, request.BackendPort)
	if err != nil || !status.DevelopmentQualificationEnabled || status.RuntimeQualified ||
		status.Readiness != ReadinessReady {
		t.Fatalf("development enablement and runtime qualification were conflated: %+v err=%v", status, err)
	}
	if fixture.mutationCalls() != 1 {
		t.Fatalf("development-enabled profile dispatched %d Serve mutations, want one", fixture.mutationCalls())
	}
}

func TestProductionScopeRefusesAppStoreCandidateBeforeCLIAccess(t *testing.T) {
	fixture := newFakeCLI(t)
	manager := newPolicyManager(t, fixture, "darwin", "arm64", "production-refusal")
	request := fixtureRequest(true)
	request.Scope = "production"
	request.Consent.Scope = "production"
	if err := manager.Publish(context.Background(), request); !errors.Is(err, ErrUnsupported) {
		t.Fatalf("production scope publication = %v, want ErrUnsupported", err)
	}
	if report, err := manager.Recover(context.Background(), "production", request.InstallationID,
		request.Origin, request.HTTPSPort, request.BackendPort); !errors.Is(err, ErrUnsupported) ||
		report.Route.DevelopmentQualificationEnabled || report.Route.RuntimeQualified {
		t.Fatalf("production scope status was not refused distinctly: %+v err=%v", report, err)
	}
	if len(fixture.calls) != 0 || fixture.mutationCalls() != 0 {
		t.Fatalf("production scope contacted the CLI: calls=%d mutations=%d", len(fixture.calls), fixture.mutationCalls())
	}
}

func TestLinuxAndOtherProfilesCannotUseDevelopmentEnablement(t *testing.T) {
	linuxVersion := linuxFixtureVersion()
	linuxStatus := strings.ReplaceAll(fixtureStatus, fixtureLong, "1.102.4-tbbcd7d1fc")

	for _, test := range []struct {
		name    string
		goos    string
		goarch  string
		version string
		status  string
	}{
		{name: "Linux source candidate", goos: "linux", goarch: "amd64", version: linuxVersion, status: linuxStatus},
		{name: "other version", goos: "darwin", goarch: "arm64", version: strings.Replace(fixtureVersion, `"short":"1.102.4"`, `"short":"1.102.5"`, 1), status: fixtureStatus},
		{name: "MacSys channel", goos: "darwin", goarch: "arm64", version: strings.Replace(fixtureVersion, `"osVariant":"appstore"`, `"osVariant":"macsys"`, 1), status: fixtureStatus},
	} {
		t.Run(test.name, func(t *testing.T) {
			fixture := newFakeCLI(t)
			fixture.version = test.version
			fixture.status = test.status
			manager := newPolicyManager(t, fixture, test.goos, test.goarch, "unsupported-profile")
			err := manager.ReserveBackendPort(context.Background(), "install-fixture", "development", "node-fixture",
				"https://herdr.tailnet.ts.net:8443", 8443, 18377, "00000000000000000000000000000001")
			if !errors.Is(err, ErrUnsupported) {
				t.Fatalf("unsupported development profile was accepted: %v", err)
			}
			if fixture.mutationCalls() != 0 {
				t.Fatalf("unsupported profile dispatched a Serve mutation: %d", fixture.mutationCalls())
			}
		})
	}
}

func TestDevelopmentPublishRequiresRouteBoundConfirmation(t *testing.T) {
	fixture := newFakeCLI(t)
	manager := newPolicyManager(t, fixture, "darwin", "arm64", "missing-route-confirmation")
	request := fixtureRequest(true)
	if err := manager.ReserveBackendPort(context.Background(), request.InstallationID, request.Scope,
		request.ExpectedNodeID, request.Origin, request.HTTPSPort, request.BackendPort, request.ReservationID); err != nil {
		t.Fatal(err)
	}
	request.Consent.RouteConfirmation = "PUBLISH"
	if err := manager.Publish(context.Background(), request); err == nil {
		t.Fatal("blanket confirmation was accepted without naming the node, listener, and backend")
	}
	if fixture.mutationCalls() != 0 {
		t.Fatalf("invalid route-bound confirmation dispatched a mutation: %d", fixture.mutationCalls())
	}
}

func linuxFixtureVersion() string {
	version := strings.ReplaceAll(fixtureVersion, fixtureLong, "1.102.4-tbbcd7d1fc")
	version = strings.ReplaceAll(version, "3caf7d9e7dcaba589cfc58beda596929733e4fea", "bbcd7d1fc2054b9189ebc1531acf74bd880ca0c8")
	version = strings.ReplaceAll(version, "084ee3b64537a1276e56fc38cdf0a711da9f4936", "")
	version = strings.ReplaceAll(version, `"osVariant":"appstore"`, `"osVariant":""`)
	version = strings.ReplaceAll(version, `"cap":142`, `"cap":141`)
	return version
}
