package tailscalecli

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func developmentWorkflowFixture(t *testing.T, f *fakeCLI, goos, goarch string) (*DevelopmentWorkflow, string, string, string) {
	t.Helper()
	base, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	root := filepath.Join(base, "development")
	state := filepath.Join(root, "registration")
	coordination := filepath.Join(base, "coordination")
	for _, directory := range []string{root, state, coordination} {
		if err := os.Mkdir(directory, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	client := newTestClientForPlatform("/fixture path/tailscale", f.run, goos, goarch)
	workflow, _, err := newDevelopmentWorkflow(context.Background(), root, state, coordination, client)
	if err != nil {
		t.Fatalf("create workflow: %v", err)
	}
	workflow.manager.skipBackendReadiness = true
	return workflow, root, state, coordination
}

func TestDevelopmentWorkflowOwnsPositiveRouteOperation(t *testing.T) {
	fixture := newFakeCLI(t)
	workflow, root, state, coordination := developmentWorkflowFixture(t, fixture, "darwin", "arm64")
	if err := workflow.ValidateRuntimeBinding(root, state, coordination, "/fixture path/tailscale", "development",
		"install-fixture", "https://herdr.tailnet.ts.net:8443", DevelopmentHTTPSPort, DevelopmentBackendPort, DevelopmentPluginPort); err != nil {
		t.Fatalf("authorized tuple refused: %v", err)
	}
	if err := workflow.ValidateRuntimeBinding(root, state, coordination, "/fixture path/tailscale", "development",
		"install-fixture", "https://herdr.tailnet.ts.net:8443", 8444, DevelopmentBackendPort, DevelopmentPluginPort); !errors.Is(err, ErrWorkflowRequired) {
		t.Fatalf("non-profile HTTPS port accepted: %v", err)
	}
	request := fixtureRequest(true)
	if err := workflow.ReserveBackendPort(context.Background(), request.InstallationID, request.ExpectedNodeID,
		request.Origin, request.ReservationID); err != nil {
		t.Fatalf("reserve through workflow: %v", err)
	}
	if err := workflow.Publish(context.Background(), request); err != nil {
		t.Fatalf("publish through workflow: %v", err)
	}
	route, err := workflow.VerifyRegisteredRoute(context.Background(), "development", request.InstallationID,
		request.Origin, DevelopmentHTTPSPort, DevelopmentBackendPort)
	if err != nil || route.Readiness != ReadinessReady || route.JournalState != StateRegistered {
		t.Fatalf("workflow route = %+v, err=%v", route, err)
	}
	if fixture.mutationCalls() != 1 {
		t.Fatalf("Serve mutation calls = %d, want one workflow-owned publication", fixture.mutationCalls())
	}
}

func TestDevelopmentWorkflowWaitsForOwnedBackendBindBeforePublish(t *testing.T) {
	fixture := newFakeCLI(t)
	workflow, _, _, _ := developmentWorkflowFixture(t, fixture, "darwin", "arm64")
	workflow.manager.skipBackendReadiness = false
	request := fixtureRequest(true)
	request.BackendBound = make(chan struct{})
	if err := workflow.ReserveBackendPort(context.Background(), request.InstallationID, request.ExpectedNodeID,
		request.Origin, request.ReservationID); err != nil {
		t.Fatalf("reserve through workflow: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	err := workflow.Publish(ctx, request)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("publish without an owned backend bind signal = %v, want context deadline", err)
	}
	if fixture.mutationCalls() != 0 {
		t.Fatalf("backend-bind refusal dispatched %d Serve mutations", fixture.mutationCalls())
	}
}

func TestDevelopmentWorkflowRefusesWrongPlatformAndUnboundManager(t *testing.T) {
	fixture := newFakeCLI(t)
	base, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	root := filepath.Join(base, "development")
	state := filepath.Join(root, "registration")
	coordination := filepath.Join(base, "coordination")
	for _, directory := range []string{root, state, coordination} {
		if err := os.Mkdir(directory, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	client := newTestClientForPlatform("/fixture path/tailscale", fixture.run, "darwin", "amd64")
	if _, _, err := newDevelopmentWorkflow(context.Background(), root, state, coordination, client); !errors.Is(err, ErrUnsupported) {
		t.Fatalf("Darwin/amd64 workflow accepted: %v", err)
	}
	if _, err := NewDevelopmentManager(root, state, coordination, client); !errors.Is(err, ErrWorkflowRequired) {
		t.Fatalf("standalone manager constructor returned capability: %v", err)
	}
	if fixture.mutationCalls() != 0 {
		t.Fatalf("refused workflow dispatched a Serve mutation: %d", fixture.mutationCalls())
	}
}
