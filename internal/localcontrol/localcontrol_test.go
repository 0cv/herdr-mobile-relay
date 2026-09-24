package localcontrol

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestServerRequiresRunIdentityAndAcknowledgesArm(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "control.sock")
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

func TestNewRejectsSocketCollision(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "control.sock")
	if err := os.WriteFile(path, []byte("owned by another run"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := New(path, "run", "instance", func() Status { return Status{} }, func() (Status, error) { return Status{}, nil }); err == nil {
		t.Fatal("regular-file collision accepted")
	}
}
