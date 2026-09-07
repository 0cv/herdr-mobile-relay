package coordinator

import (
	"context"
	"sync"
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

func TestTopologyAdmissionAcquiresExecutionFenceBeforeTopologyLock(t *testing.T) {
	dispatcher := NewDispatcher(nil, NewState(testLogger()), nil, testLogger())
	t.Cleanup(func() { _ = dispatcher.Close(context.Background()) })

	serialization := make(chan struct{}, 1)
	serialization <- struct{}{}
	releaseSerialization := func() {
		select {
		case serialization <- struct{}{}:
		default:
		}
	}

	var agentFenceOnce sync.Once
	agentCtx := WithExecutionFence(context.Background(), func() *CommandResult {
		agentFenceOnce.Do(func() { <-serialization })
		return nil
	})
	if result := executionFenceResult(agentCtx); result != nil {
		t.Fatalf("agent execution fence = %+v", result)
	}

	workspaceFenceEntered := make(chan struct{})
	var workspaceFenceOnce sync.Once
	workspaceCtx := WithExecutionFence(context.Background(), func() *CommandResult {
		workspaceFenceOnce.Do(func() {
			close(workspaceFenceEntered)
			<-serialization
		})
		return nil
	})
	admitted := make(chan struct{})
	var workspaceRan atomic.Bool
	workspaceDone := make(chan *CommandResult, 1)
	go func() {
		result := dispatcher.HandleTopologyAdmitted(
			workspaceCtx,
			"workspace", "workspace_rename",
			func() { close(admitted) },
			func(context.Context) *CommandResult {
				workspaceRan.Store(true)
				return completed("workspace", "workspace_rename", "", nil)
			},
		)
		releaseSerialization()
		workspaceDone <- result
	}()
	<-admitted
	<-workspaceFenceEntered

	agentDone := make(chan EffectResult, 1)
	go func() {
		result := dispatcher.topologyEffect(
			agentCtx,
			"agent", CommandStart, "pane-1",
			EffectFunc(func(context.Context, WorkerToken) EffectResult {
				return EffectResult{Result: completed("agent", string(CommandStart), "pane-1", nil)}
			}),
		).Run(context.Background(), WorkerToken{})
		releaseSerialization()
		agentDone <- result
	}()

	select {
	case result := <-agentDone:
		if result.Result == nil || !result.Result.OK {
			t.Fatalf("agent topology result = %+v", result)
		}
	case <-time.After(500 * time.Millisecond):
		releaseSerialization()
		<-agentDone
		<-workspaceDone
		t.Fatal("workspace and agent topology paths deadlocked")
	}

	select {
	case result := <-workspaceDone:
		if result.Error != "Agent topology changed before execution; refresh and retry" || workspaceRan.Load() {
			t.Fatalf("workspace topology result = %+v, ran=%t", result, workspaceRan.Load())
		}
	case <-time.After(time.Second):
		t.Fatal("workspace topology path did not resume after the agent completed")
	}
}

func fenceCommand(requestID string) ScheduleOptions {
	now := time.Now()
	return ScheduleOptions{Command: Command{
		RequestID: requestID, ReceivedAt: now, Deadline: now.Add(time.Second), Kind: CommandText, PaneID: "pane-1",
	}}
}
