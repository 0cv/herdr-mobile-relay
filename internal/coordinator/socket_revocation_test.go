package coordinator

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"net"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/0cv/herdr-mobile-relay/internal/herdr"
)

func TestWorkspaceCloseRevokedBehindTopologyLock(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "socket")
	listener, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	var writes atomic.Int32
	accepted := make(chan struct{})
	go func() {
		defer close(accepted)
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			_ = conn.SetDeadline(time.Now().Add(time.Second))
			var request map[string]any
			if json.NewDecoder(bufio.NewReader(conn)).Decode(&request) == nil {
				writes.Add(1)
			}
			_ = conn.Close()
		}
	}()
	defer func() { _ = listener.Close(); <-accepted }()
	bin := writeScript(t, dir, "herdr", "#!/bin/sh\nexit 99\n")
	d := NewDispatcher(herdr.NewClient(bin, path), NewState(testLogger()), nil, testLogger())
	defer d.Close(context.Background())
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
	if writes.Load() != 0 {
		t.Fatalf("socket received %d requests", writes.Load())
	}
}
