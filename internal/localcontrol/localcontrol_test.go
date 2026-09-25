package localcontrol

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestServerRequiresRunIdentityAndAcknowledgesArm(t *testing.T) {
	path := testSocketPath(t)
	armed := false
	server, err := New(path, "run-1", "instance-1", func() Status {
		return Status{Ready: true, InvitationArmed: armed, InvitationExpiresAt: "2026-01-01T00:00:00Z"}
	}, func() (Status, error) {
		armed = true
		return Status{Ready: true, InvitationArmed: true, InvitationExpiresAt: "2026-01-01T00:00:00Z"}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = server.Run(ctx) }()
	t.Cleanup(func() { _ = server.Close() })

	response, err := Request(context.Background(), path, "status", "run-1", "instance-1")
	if err != nil || !response.OK || !response.Ready {
		t.Fatalf("status response = %#v, err = %v", response, err)
	}
	if _, err := Request(context.Background(), path, "arm_bootstrap", "wrong", "instance-1"); err == nil {
		t.Fatal("wrong run identity accepted")
	}
	response, err = Request(context.Background(), path, "arm_bootstrap", "run-1", "instance-1")
	if err != nil || !response.OK || !response.InvitationArmed {
		t.Fatalf("arm response = %#v, err = %v", response, err)
	}
	cancel()
	time.Sleep(10 * time.Millisecond)
}

func TestManagedLifecycleOperationsReturnRedactedOwnerState(t *testing.T) {
	path := testSocketPath(t)
	retired := make(chan struct{})
	server, err := NewManaged(path, "run-managed", "instance-managed", Callbacks{
		Status: func(context.Context) Status {
			return Status{OwnerHeld: true, LocalReady: true, RemoteWatchRetirementUnknown: true}
		},
		Activate: func(context.Context) (Status, error) {
			return Status{OwnerHeld: true, LocalReady: true, ServeReady: true}, nil
		},
		Arm: func(context.Context) (Status, error) {
			return Status{OwnerHeld: true, LocalReady: true, ServeReady: true, Ready: true,
				InvitationArmed: true, InvitationExpiresAt: "2026-01-01T00:00:00Z"}, nil
		},
		Retire: func(context.Context) (Status, error) {
			return Status{OwnerHeld: true, Quarantined: true, RouteCleared: true,
				LocalWatchClosed: true, RemoteWatchRetirementUnknown: true}, nil
		},
		Retired: func() { close(retired) },
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = server.Run(ctx) }()
	t.Cleanup(func() { _ = server.Close() })

	status, err := Request(context.Background(), path, "status", "run-managed", "instance-managed")
	if err != nil || !status.OwnerHeld || !status.LocalReady || !status.RemoteWatchRetirementUnknown {
		t.Fatalf("status = %+v, err = %v", status, err)
	}
	activated, err := Request(context.Background(), path, "activate", "run-managed", "instance-managed")
	if err != nil || !activated.ServeReady {
		t.Fatalf("activate = %+v, err = %v", activated, err)
	}
	armed, err := Request(context.Background(), path, "arm_bootstrap", "run-managed", "instance-managed")
	if err != nil || !armed.Ready || !armed.InvitationArmed {
		t.Fatalf("arm = %+v, err = %v", armed, err)
	}
	retirement, err := Request(context.Background(), path, "retire", "run-managed", "instance-managed")
	if err != nil || !retirement.RouteCleared || !retirement.LocalWatchClosed || !retirement.RemoteWatchRetirementUnknown {
		t.Fatalf("retire = %+v, err = %v", retirement, err)
	}
	select {
	case <-retired:
	case <-time.After(time.Second):
		t.Fatal("retirement acknowledgement callback was not invoked")
	}
}

func TestManagedControlClientCancellationCancelsLifecycleCallback(t *testing.T) {
	path := testSocketPath(t)
	started := make(chan struct{})
	callbackCancelled := make(chan struct{})
	server, err := NewManaged(path, "run-cancel", "instance-cancel", Callbacks{
		Status: func(context.Context) Status { return Status{OwnerHeld: true} },
		Activate: func(ctx context.Context) (Status, error) {
			close(started)
			<-ctx.Done()
			close(callbackCancelled)
			return Status{}, ctx.Err()
		},
		Arm: func(context.Context) (Status, error) { return Status{}, nil },
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancelServer := context.WithCancel(context.Background())
	defer cancelServer()
	go func() { _ = server.Run(ctx) }()
	t.Cleanup(func() { _ = server.Close() })

	requestCtx, cancelRequest := context.WithCancel(context.Background())
	requestDone := make(chan error, 1)
	go func() {
		_, err := Request(requestCtx, path, "activate", "run-cancel", "instance-cancel")
		requestDone <- err
	}()
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("activation callback did not start")
	}
	cancelRequest()
	select {
	case <-callbackCancelled:
	case <-time.After(time.Second):
		t.Fatal("client disconnect did not cancel activation callback")
	}
	select {
	case <-requestDone:
	case <-time.After(time.Second):
		t.Fatal("cancelled client request did not return")
	}
	status, err := Request(context.Background(), path, "status", "run-cancel", "instance-cancel")
	if err != nil || !status.OwnerHeld {
		t.Fatalf("control server did not recover after cancellation: %+v, %v", status, err)
	}
}

func testSocketPath(t *testing.T) string {
	t.Helper()
	root, err := os.MkdirTemp("/tmp", "lc-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(root) })
	return filepath.Join(root, "control.sock")
}

func TestNewRejectsSocketCollision(t *testing.T) {
	path := testSocketPath(t)
	if err := os.WriteFile(path, []byte("owned by another run"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := New(path, "run", "instance", func() Status { return Status{} }, func() (Status, error) { return Status{}, nil }); err == nil {
		t.Fatal("regular-file collision accepted")
	}
}
