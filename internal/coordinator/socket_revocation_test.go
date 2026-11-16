package coordinator

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/0cv/herdr-mobile-relay/internal/herdr"
)

func TestWorkspaceCloseRevokedBehindTopologyLock(t *testing.T) {
	client := newFakeBackend()
	client.workspaces = []herdr.Workspace{{ID: "workspace", Label: "Project"}}
	d := newTestDispatcher(t, client, NewState(testLogger()))
	var revoked atomic.Bool
	var checks atomic.Int32
	ctx := herdr.WithDispatchCheck(context.Background(), func() error {
		checks.Add(1)
		if revoked.Load() {
			return errors.New("revoked")
		}
		return nil
	})
	d.topologyMu.Lock()
	admitted := make(chan struct{})
	result := make(chan *CommandResult, 1)
	go func() {
		result <- d.HandleTopologyAdmitted(ctx, func() { close(admitted) }, func(ctx context.Context) *CommandResult {
			return d.HandleWorkspaceClose(ctx, "close", "workspace", false, nil)
		})
	}()
	<-admitted
	revoked.Store(true)
	d.topologyMu.Unlock()
	select {
	case got := <-result:
		if got.OK {
			t.Fatalf("revoked close succeeded: %+v", got)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("close did not complete")
	}
	if checks.Load() != 1 {
		t.Fatalf("guard called %d times", checks.Load())
	}
	if client.count("WorkspaceClose") != 0 {
		t.Fatalf("recorded calls: %+v", client.recorded())
	}
}
