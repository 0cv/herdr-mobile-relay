package coordinator

import (
	"context"
	"sync/atomic"
	"testing"
	"time"
)

func TestExecutionFenceRunsAtWorkerAndTopologyExecutionBoundaries(t *testing.T) {
	state := NewState(testLogger())
	state.CommitInventory([]*AgentState{{PaneID: "pane-1", Status: "idle"}}, state.RevisionCounter())
	dispatcher := NewDispatcher(nil, state, nil, testLogger())
	t.Cleanup(func() { _ = dispatcher.Close(context.Background()) })

	started := make(chan struct{})
	release := make(chan struct{})
	firstDone := make(chan *CommandResult, 1)
	go func() {
		firstDone <- dispatcher.schedule(context.Background(), fenceCommand("first"), EffectFunc(func(context.Context, WorkerToken) EffectResult {
			close(started)
			<-release
			return EffectResult{Result: completed("first", "text", "pane-1", nil)}
		}))
	}()
	<-started

	var blocked atomic.Bool
	workerRan := atomic.Bool{}
	admitted := make(chan struct{})
	fenced := WithExecutionFence(context.Background(), func() *CommandResult {
		if blocked.Load() {
			return &CommandResult{RequestID: "second", Action: "text", Phase: "not_started", Error: "generation changed", PaneID: "pane-1"}
		}
		return nil
	})
	fenced = context.WithValue(fenced, admissionContextKey{}, func() { close(admitted) })
	secondDone := make(chan *CommandResult, 1)
	go func() {
		secondDone <- dispatcher.schedule(fenced, fenceCommand("second"), EffectFunc(func(context.Context, WorkerToken) EffectResult {
			workerRan.Store(true)
			return EffectResult{Result: completed("second", "text", "pane-1", nil)}
		}))
	}()
	<-admitted
	blocked.Store(true)
	close(release)
	if first := <-firstDone; !first.OK {
		t.Fatalf("first result = %+v", first)
	}
	if second := <-secondDone; second.Error != "generation changed" || workerRan.Load() {
		t.Fatalf("fenced worker result = %+v, ran=%t", second, workerRan.Load())
	}

	dispatcher.topologyMu.Lock()
	topologyRan := atomic.Bool{}
	topologyAdmitted := make(chan struct{})
	topologyDone := make(chan *CommandResult, 1)
	go func() {
		topologyDone <- dispatcher.HandleTopologyAdmitted(fenced, "workspace", "workspace_rename", func() { close(topologyAdmitted) }, func(context.Context) *CommandResult {
			topologyRan.Store(true)
			return completed("topology", "workspace_create", "", nil)
		})
	}()
	<-topologyAdmitted
	dispatcher.topologyMu.Unlock()
	if result := <-topologyDone; result.Error != "generation changed" || topologyRan.Load() {
		t.Fatalf("fenced topology result = %+v, ran=%t", result, topologyRan.Load())
	}
}

func fenceCommand(requestID string) ScheduleOptions {
	now := time.Now()
	return ScheduleOptions{Command: Command{
		RequestID: requestID, ReceivedAt: now, Deadline: now.Add(time.Second), Kind: CommandText, PaneID: "pane-1",
	}}
}
