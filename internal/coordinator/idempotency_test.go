package coordinator

// Idempotency regressions beyond approvals: pre-pane Agent Start (§9.6/§16.9)
// and structured-question answers (§9.6).

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/0cv/herdr-mobile-relay/internal/herdr"
	"github.com/0cv/herdr-mobile-relay/internal/question"
)

func recordingHerdr(t *testing.T, dir, record, stdout string) string {
	return writeScript(t, dir, "herdr", "#!/bin/sh\n"+
		"printf '%s\\n' \"$*\" >> \""+record+"\"\n"+
		"printf '"+stdout+"\\n'\n")
}

// §9.6 / §16.9: two Agent Start requests with the same identity (client +
// request_id + action) must create at most one pane. There is no relay-level
// pre-pane scheduler ledger, so both callers observe the same pane creation.
func TestDuplicateAgentStartCreatesOnePane(t *testing.T) {
	dir := t.TempDir()
	record := filepath.Join(dir, "starts.log")
	bin := recordingHerdr(t, dir, record, `{"result":{"pane_id":"pane-new"}}`)

	d := NewDispatcher(herdr.NewClient(bin, filepath.Join(dir, "sock")), NewState(testLogger()), nil, testLogger())

	start := func() {
		d.Handle(context.Background(), map[string]any{
			"action":     "agent_start",
			"request_id": "same-req",
			"profile_id": "claude",
			"name":       "proj",
			"cwd":        "/tmp",
		})
	}

	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); start() }()
	go func() { defer wg.Done(); start() }()
	wg.Wait()

	data, _ := os.ReadFile(record)
	starts := strings.Count(string(data), "--kind")
	if starts != 1 {
		t.Fatalf("duplicate agent_start created %d panes, want 1\nrecord:\n%s", starts, data)
	}
}

func TestAgentStartRetryDoesNotResubmitInitialPrompt(t *testing.T) {
	dir := t.TempDir()
	record := filepath.Join(dir, "starts.log")
	bin := recordingHerdr(t, dir, record, `{"result":{"pane_id":"pane-new"}}`)

	// Pre-check: verify the script produces valid JSON output directly.
	preDir := filepath.Join(dir, "pre")
	os.MkdirAll(preDir, 0o755)
	preRecord := filepath.Join(preDir, "pre.log")
	preBin := recordingHerdr(t, preDir, preRecord, `{"result":{"pane_id":"pane-new"}}`)
	preCmd := exec.Command(preBin, "agent", "start", "pre", "--kind", "claude", "--pane", "p", "--timeout", "30000")
	preOut, preErr := preCmd.CombinedOutput()
	if preErr != nil {
		t.Fatalf("script pre-check failed: %v\noutput: %q", preErr, preOut)
	}
	var envelope struct {
		Result json.RawMessage `json:"result"`
	}
	if err := json.Unmarshal(preOut, &envelope); err != nil {
		t.Fatalf("script pre-check produced invalid JSON: %v\nraw output: %q", err, preOut)
	}

	d := NewDispatcher(herdr.NewClient(bin, filepath.Join(dir, "sock")), NewState(testLogger()), nil, testLogger())
	message := map[string]any{
		"action": "agent_start", "request_id": "same-req",
		"profile_id": "claude", "name": "proj", "cwd": "/tmp", "prompt": "hello",
	}
	first := d.Handle(context.Background(), message)
	second := d.Handle(context.Background(), message)
	if !first.OK || !second.OK {
		data, _ := os.ReadFile(record)
		t.Fatalf("start results = %+v, %+v\nrecord:\n%s", first, second, data)
	}
	data, _ := os.ReadFile(record)
	if starts := strings.Count(string(data), "--kind"); starts != 1 {
		t.Fatalf("start invocations = %d, want 1\n%s", starts, data)
	}
	if prompts := strings.Count(string(data), "agent prompt"); prompts != 1 {
		t.Fatalf("initial prompt invocations = %d, want 1\n%s", prompts, data)
	}
}

func TestAgentStartRetryReplaysInitialPromptWarning(t *testing.T) {
	dir := t.TempDir()
	record := filepath.Join(dir, "starts.log")
	bin := writeScript(t, dir, "herdr", `#!/bin/sh
printf '%s\n' "$*" >> "`+record+`"
case "$1 $2" in
  "agent start") printf '%s\n' '{"result":{"pane_id":"pane-new"}}' ;;
  "agent prompt") exit 1 ;;
  *) printf '%s\n' '{"result":{}}' ;;
esac
`)
	d := NewDispatcher(herdr.NewClient(bin, filepath.Join(dir, "sock")), NewState(testLogger()), nil, testLogger())
	message := map[string]any{
		"action": "agent_start", "request_id": "same-warning",
		"profile_id": "claude", "name": "proj", "cwd": "/tmp", "prompt": "hello",
	}
	first := d.Handle(context.Background(), message)
	second := d.Handle(context.Background(), message)
	if first.Phase != "completed_with_warning" || second.Phase != "completed_with_warning" {
		t.Fatalf("start warning results = %+v, %+v", first, second)
	}
	data, _ := os.ReadFile(record)
	if starts := strings.Count(string(data), "--kind"); starts != 1 {
		t.Fatalf("start invocations = %d, want 1\n%s", starts, data)
	}
	if prompts := strings.Count(string(data), "agent prompt"); prompts != 1 {
		t.Fatalf("initial prompt invocations = %d, want 1\n%s", prompts, data)
	}
}

func TestSendInitialPromptCoversQoderDeliveryOutcomes(t *testing.T) {
	newDispatcher := func(t *testing.T, script string) (*Dispatcher, string) {
		t.Helper()
		dir := t.TempDir()
		bin := writeScript(t, dir, "herdr", script)
		state := NewState(testLogger())
		state.CommitInventory([]*AgentState{{PaneID: "pane-1", Agent: "qodercli", Status: "idle"}}, 0)
		return NewDispatcher(herdr.NewClient(bin, filepath.Join(dir, "sock")), state, nil, testLogger()), dir
	}

	t.Run("success", func(t *testing.T) {
		dispatcher, _ := newDispatcher(t, "#!/bin/sh\nprintf '%s\\n' '{\"result\":{}}'\n")
		generation, _ := dispatcher.state.PaneSession("pane-1")
		if err := dispatcher.sendInitialPrompt(t.Context(), "pane-1", "hello", uint64(generation), false); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("send text failure", func(t *testing.T) {
		dispatcher, _ := newDispatcher(t, "#!/bin/sh\ncase \"$1 $2\" in\n  'pane send-text') exit 1 ;;\n  *) printf '%s\\n' '{\"result\":{}}' ;;\nesac\n")
		generation, _ := dispatcher.state.PaneSession("pane-1")
		if err := dispatcher.sendInitialPrompt(t.Context(), "pane-1", "hello", uint64(generation), false); err == nil {
			t.Fatal("send-text failure was hidden")
		}
	})
	t.Run("send keys failure", func(t *testing.T) {
		dispatcher, _ := newDispatcher(t, "#!/bin/sh\ncase \"$1 $2\" in\n  'pane send-keys') exit 1 ;;\n  *) printf '%s\\n' '{\"result\":{}}' ;;\nesac\n")
		generation, _ := dispatcher.state.PaneSession("pane-1")
		if err := dispatcher.sendInitialPrompt(t.Context(), "pane-1", "hello", uint64(generation), false); err == nil || !strings.Contains(err.Error(), "prompt text was already delivered") {
			t.Fatalf("send-keys failure = %v", err)
		}
	})
	t.Run("pane replacement after text", func(t *testing.T) {
		markerDir := t.TempDir()
		entered := filepath.Join(markerDir, "entered")
		release := filepath.Join(markerDir, "release")
		script := fmt.Sprintf("#!/bin/sh\ncase \"$1 $2\" in\n  'pane send-text') touch %q; while [ ! -f %q ]; do sleep 0.01; done ;;\nesac\nprintf '%%s\\n' '{\"result\":{}}'\n", entered, release)
		dispatcher, _ := newDispatcher(t, script)
		generation, _ := dispatcher.state.PaneSession("pane-1")
		result := make(chan error, 1)
		go func() {
			result <- dispatcher.sendInitialPrompt(t.Context(), "pane-1", "hello", uint64(generation), false)
		}()
		deadline := time.Now().Add(time.Second)
		for {
			if _, err := os.Stat(entered); err == nil {
				break
			}
			if time.Now().After(deadline) {
				t.Fatal("send-text command did not start")
			}
			time.Sleep(10 * time.Millisecond)
		}
		dispatcher.state.BumpGeneration("pane-1")
		if err := os.WriteFile(release, nil, 0o600); err != nil {
			t.Fatal(err)
		}
		if err := <-result; err == nil || !strings.Contains(err.Error(), "prompt text was already delivered") {
			t.Fatalf("pane replacement result = %v", err)
		}
	})
}

func TestLedgerReplayReturnsConfirmationWatchPhase(t *testing.T) {
	scheduler := NewScheduler(1, testLogger())
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		if err := scheduler.Close(ctx); err != nil {
			t.Errorf("close scheduler: %v", err)
		}
	})

	const ledgerKey = "approval\x00pane-1\x00event-1"
	options := func() ScheduleOptions {
		now := time.Now()
		return ScheduleOptions{
			Command: Command{
				ID:         scheduler.NextCommandID(),
				RequestID:  "approve",
				ReceivedAt: now,
				Deadline:   now.Add(time.Second),
				Kind:       CommandApproval,
				PaneID:     "pane-1",
			},
			LedgerKey:   ledgerKey,
			PayloadHash: "choice-1-of-2",
		}
	}
	runs := 0
	runner := EffectFunc(func(context.Context, WorkerToken) EffectResult {
		runs++
		result := completed("approve", "approval", "pane-1", nil)
		result.Phase = "accepted"
		return EffectResult{Result: result}
	})

	first, err := scheduler.Execute(context.Background(), options(), runner)
	if err != nil || first == nil || first.Phase != "accepted" {
		t.Fatalf("first result = %+v, err = %v", first, err)
	}
	if !scheduler.UpdateLedgerPhase(ledgerKey, 0, "confirmed") {
		t.Fatal("confirmation phase was not applied")
	}

	replay, err := scheduler.Execute(context.Background(), options(), runner)
	if err != nil {
		t.Fatalf("replay: %v", err)
	}
	if replay == nil || replay.Phase != "confirmed" || !replay.OK || !replay.replayed {
		t.Fatalf("replay = %+v, want confirmed stored phase", replay)
	}
	if runs != 1 {
		t.Fatalf("effect runs = %d, want 1", runs)
	}
	if scheduler.UpdateLedgerPhase(ledgerKey, 1, "unconfirmed") {
		t.Fatal("stale-generation phase update was applied")
	}
}

func TestCoalescedWaitersReceiveOnlyTheFinalizedResult(t *testing.T) {
	scheduler := NewScheduler(1, testLogger())
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		if err := scheduler.Close(ctx); err != nil {
			t.Errorf("close scheduler: %v", err)
		}
	})
	dispatcher := &Dispatcher{scheduler: scheduler, state: NewState(testLogger())}
	effectStarted := make(chan struct{})
	releaseEffect := make(chan struct{})
	finalizerStarted := make(chan struct{})
	releaseFinalizer := make(chan struct{})
	var runs atomic.Int32
	var finalizations atomic.Int32
	ctx := WithResultFinalizer(context.Background(), func(_ context.Context, result *CommandResult) *CommandResult {
		finalizations.Add(1)
		close(finalizerStarted)
		<-releaseFinalizer
		result.Phase = "checkpointed"
		return result
	})
	options := func(requestID string) ScheduleOptions {
		now := time.Now()
		return ScheduleOptions{
			Command:    Command{ID: scheduler.NextCommandID(), RequestID: requestID, ReceivedAt: now, Deadline: now.Add(time.Second), Kind: CommandStart},
			RelayLevel: true, LedgerKey: "start\x00same", PayloadHash: "same",
		}
	}
	runner := EffectFunc(func(context.Context, WorkerToken) EffectResult {
		runs.Add(1)
		close(effectStarted)
		<-releaseEffect
		return EffectResult{Result: completed("first", "agent_start", "pane-1", nil)}
	})
	results := make(chan *CommandResult, 2)
	topologyRunner := dispatcher.topologyEffect(ctx, "first", CommandStart, "", runner)
	go func() { results <- dispatcher.schedule(ctx, options("first"), topologyRunner) }()
	<-effectStarted
	go func() { results <- dispatcher.schedule(ctx, options("second"), topologyRunner) }()
	close(releaseEffect)
	<-finalizerStarted
	select {
	case result := <-results:
		t.Fatalf("coalesced waiter returned before finalization: %+v", result)
	default:
	}
	close(releaseFinalizer)
	first, second := <-results, <-results
	if first.Phase != "checkpointed" || second.Phase != "checkpointed" || runs.Load() != 1 || finalizations.Load() != 1 {
		t.Fatalf("coalesced results = (%+v, %+v), runs=%d finalizations=%d", first, second, runs.Load(), finalizations.Load())
	}
}

func TestCoalescedStopSurvivesItsOwnFinalizedTopologyRemoval(t *testing.T) {
	scheduler := NewScheduler(1, testLogger())
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		if err := scheduler.Close(ctx); err != nil {
			t.Errorf("close scheduler: %v", err)
		}
	})
	state := NewState(testLogger())
	state.CommitInventory([]*AgentState{{PaneID: "pane-1", Status: "idle"}}, 0)
	dispatcher := &Dispatcher{scheduler: scheduler, state: state, logger: testLogger()}
	scheduler.testCoalesced = make(chan struct{}, 1)
	if !scheduler.ApplyTopology(map[string]bool{"pane-1": true}, map[string]uint64{"pane-1": 0}) {
		t.Fatal("initial topology was not applied")
	}
	started := make(chan struct{})
	release := make(chan struct{})
	ctx := WithResultFinalizer(context.Background(), func(_ context.Context, result *CommandResult) *CommandResult {
		state.BumpGeneration("pane-1")
		if !scheduler.ApplyTopology(map[string]bool{}) {
			t.Fatal("finalized topology was not applied")
		}
		return result
	})
	options := func(requestID string) ScheduleOptions {
		now := time.Now()
		return ScheduleOptions{
			Command: Command{
				ID: scheduler.NextCommandID(), RequestID: requestID, ReceivedAt: now,
				Deadline: now.Add(time.Second), Kind: CommandStop, PaneID: "pane-1",
			},
			LedgerKey: "stop\x00pane-1", PayloadHash: "same",
		}
	}
	runner := EffectFunc(func(context.Context, WorkerToken) EffectResult {
		close(started)
		<-release
		return EffectResult{Result: completed("first", "agent_stop", "pane-1", nil), BumpGeneration: true}
	})
	results := make(chan *CommandResult, 2)
	topologyRunner := dispatcher.topologyEffect(ctx, "first", CommandStop, "pane-1", runner)
	go func() { results <- dispatcher.schedule(ctx, options("first"), topologyRunner) }()
	<-started
	go func() { results <- dispatcher.schedule(ctx, options("second"), topologyRunner) }()
	select {
	case <-scheduler.testCoalesced:
	case <-time.After(time.Second):
		t.Fatal("second stop request did not coalesce")
	}
	close(release)
	first, second := <-results, <-results
	if !first.OK || !second.OK || first.Phase != "completed" || second.Phase != "completed" {
		t.Fatalf("coalesced stop results = (%+v, %+v)", first, second)
	}
	replay := dispatcher.schedule(ctx, options("third"), topologyRunner)
	if !replay.OK || replay.Phase != "completed" || !replay.replayed {
		t.Fatalf("stop replay = %+v, want cached finalized success", replay)
	}
}

func TestInitialPromptSharesPaneEffectSerialization(t *testing.T) {
	dir := t.TempDir()
	record := filepath.Join(dir, "commands.log")
	bin := recordingHerdr(t, dir, record, `{"result":{}}`)
	state := NewState(testLogger())
	state.CommitInventory([]*AgentState{{PaneID: "pane-1", Agent: "claude", Status: "idle"}}, 0)
	dispatcher := NewDispatcher(herdr.NewClient(bin, filepath.Join(dir, "sock")), state, nil, testLogger())

	unlock := dispatcher.lockPaneEffect("pane-1")
	done := make(chan *CommandResult, 1)
	go func() {
		done <- dispatcher.handlePrompt(t.Context(), time.Now(), "prompt-1", "pane-1", map[string]any{"text": "existing"})
	}()
	time.Sleep(50 * time.Millisecond)
	if data, _ := os.ReadFile(record); len(data) != 0 {
		t.Fatalf("scheduled prompt bypassed pane serialization: %s", data)
	}
	unlock()
	if result := <-done; result == nil || !result.OK {
		t.Fatalf("scheduled prompt result = %+v", result)
	}

	unlock = dispatcher.lockPaneEffect("pane-1")
	generation, _ := dispatcher.state.PaneSession("pane-1")
	initialDone := make(chan error, 1)
	go func() {
		initialDone <- dispatcher.sendInitialPrompt(t.Context(), "pane-1", "initial", uint64(generation), false)
	}()
	time.Sleep(50 * time.Millisecond)
	data, _ := os.ReadFile(record)
	if strings.Count(string(data), "agent prompt") != 1 {
		t.Fatalf("initial prompt bypassed pane serialization: %s", data)
	}
	unlock()
	if err := <-initialDone; err != nil {
		t.Fatal(err)
	}

	unlock = dispatcher.lockPaneEffect("pane-1")
	staleGeneration, _ := dispatcher.state.PaneSession("pane-1")
	staleDone := make(chan error, 1)
	go func() {
		staleDone <- dispatcher.sendInitialPrompt(t.Context(), "pane-1", "stale", uint64(staleGeneration), false)
	}()
	time.Sleep(50 * time.Millisecond)
	dispatcher.state.BumpGeneration("pane-1")
	unlock()
	if err := <-staleDone; !errors.Is(err, ErrPaneReplaced) {
		t.Fatalf("stale initial prompt result = %v, want pane replacement", err)
	}
	data, _ = os.ReadFile(record)
	if strings.Count(string(data), "agent prompt") != 2 {
		t.Fatalf("stale initial prompt reached replacement pane: %s", data)
	}
}

func TestTopologyInvalidatesOnlyOlderUnprotectedLedgerEntries(t *testing.T) {
	inflight := &scheduledOperation{operation: 7}
	slot := &PaneSlot{Generation: 2, InFlight: &InFlightOperation{OperationID: 7}}
	tests := []struct {
		name  string
		entry *ledgerEntry
		slot  *PaneSlot
		want  bool
	}{
		{name: "different pane", entry: &ledgerEntry{paneID: "other", generation: 1}, slot: slot},
		{name: "current generation", entry: &ledgerEntry{paneID: "pane-1", generation: 2}, slot: slot},
		{name: "completed old entry", entry: &ledgerEntry{paneID: "pane-1", generation: 1}, slot: slot, want: true},
		{name: "old entry without inflight", entry: &ledgerEntry{paneID: "pane-1", generation: 1, operation: inflight}, slot: &PaneSlot{Generation: 2}, want: true},
		{name: "different inflight operation", entry: &ledgerEntry{paneID: "pane-1", generation: 1, operation: &scheduledOperation{operation: 8}}, slot: slot, want: true},
		{name: "protected inflight operation", entry: &ledgerEntry{paneID: "pane-1", generation: 1, operation: inflight}, slot: slot},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := topologyInvalidatesLedgerEntry(test.entry, "pane-1", test.slot); got != test.want {
				t.Fatalf("topologyInvalidatesLedgerEntry() = %t, want %t", got, test.want)
			}
		})
	}
	ledger := map[string]*ledgerEntry{
		"keep":   {paneID: "pane-1", generation: 1, operation: inflight},
		"delete": {paneID: "pane-1", generation: 1},
	}
	pruneInvalidatedLedgerEntries(ledger, "pane-1", slot)
	if len(ledger) != 1 || ledger["keep"] == nil {
		t.Fatalf("pruned ledger = %+v, want only protected in-flight entry", ledger)
	}
	if commandResultOK(nil) || commandResultOK(&CommandResult{}) || !commandResultOK(&CommandResult{OK: true}) {
		t.Fatal("commandResultOK did not distinguish nil, failed, and successful results")
	}
}

func TestAgentStopRetryDoesNotCloseOrBumpTwice(t *testing.T) {
	dir := t.TempDir()
	record := filepath.Join(dir, "stops.log")
	bin := recordingHerdr(t, dir, record, `{"ok":true}`)
	state := NewState(testLogger())
	state.CommitInventory([]*AgentState{{PaneID: "pane-1", Status: "working"}}, state.RevisionCounter())
	d := NewDispatcher(herdr.NewClient(bin, filepath.Join(dir, "sock")), state, nil, testLogger())
	message := map[string]any{
		"action": "agent_stop", "request_id": "stop-1", "pane_id": "pane-1",
	}
	first := d.Handle(context.Background(), message)
	second := d.Handle(context.Background(), message)
	if !first.OK || !second.OK {
		t.Fatalf("stop results = %+v, %+v", first, second)
	}
	data, _ := os.ReadFile(record)
	if closes := strings.Count(string(data), "pane close"); closes != 1 {
		t.Fatalf("pane close invocations = %d, want 1\n%s", closes, data)
	}
	if generation := state.Generation("pane-1"); generation != 1 {
		t.Fatalf("state generation = %d, want 1", generation)
	}
}

func TestTabRenameAcceptsNaturalLabel(t *testing.T) {
	dir := t.TempDir()
	record := filepath.Join(dir, "renames.log")
	state := NewState(testLogger())
	state.CommitInventory([]*AgentState{{
		PaneID: "pane-1", TabID: "tab-1", Status: "working",
	}}, state.RevisionCounter())
	d := NewDispatcher(
		herdr.NewClient(recordingHerdr(t, dir, record, `{"ok":true}`), filepath.Join(dir, "sock")),
		state,
		nil,
		testLogger(),
	)

	result := d.Handle(context.Background(), map[string]any{
		"action": "agent_rename", "request_id": "rename-1", "pane_id": "pane-1", "name": "123",
	})
	if !result.OK {
		t.Fatalf("numeric rename failed: %+v", result)
	}
	data, err := os.ReadFile(record)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "tab rename tab-1 123") {
		t.Fatalf("rename commands = %q, missing tab rename", data)
	}
	if strings.Contains(string(data), "agent rename") {
		t.Fatalf("tab label was sent through agent rename: %q", data)
	}
}

func TestTabReorderUsesHerdrSocketAPI(t *testing.T) {
	// t.TempDir includes the test name and can exceed macOS's Unix-socket path
	// limit. Keep the socket fixture deliberately short.
	dir, err := os.MkdirTemp("/tmp", "hmr-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	socketPath := filepath.Join(dir, "h.sock")
	listener, err := net.Listen("unix", socketPath)
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	serverResult := make(chan error, 1)
	go func() {
		conn, acceptErr := listener.Accept()
		if acceptErr != nil {
			serverResult <- acceptErr
			return
		}
		defer conn.Close()
		var request struct {
			ID     string `json:"id"`
			Method string `json:"method"`
			Params struct {
				TabID       string `json:"tab_id"`
				InsertIndex int    `json:"insert_index"`
			} `json:"params"`
		}
		if decodeErr := json.NewDecoder(conn).Decode(&request); decodeErr != nil {
			serverResult <- decodeErr
			return
		}
		if request.Method != "tab.move" || request.Params.TabID != "tab-1" || request.Params.InsertIndex != 2 {
			serverResult <- fmt.Errorf("unexpected tab move request: %+v", request)
			return
		}
		serverResult <- json.NewEncoder(conn).Encode(map[string]any{
			"id": request.ID,
			"result": map[string]any{
				"type": "tab_list",
				"tabs": []any{},
			},
		})
	}()

	state := NewState(testLogger())
	state.CommitInventory([]*AgentState{{
		PaneID: "pane-1", TabID: "tab-1", Status: "working",
	}}, state.RevisionCounter())
	client := herdr.NewClient("/binary/must-not-run", socketPath)
	defer client.Close()
	dispatcher := NewDispatcher(client, state, nil, testLogger())
	result := dispatcher.Handle(context.Background(), map[string]any{
		"action": "tab_reorder", "request_id": "move-1", "pane_id": "pane-1", "insert_index": 2,
	})
	if !result.OK {
		t.Fatalf("tab reorder failed: %+v", result)
	}
	if serverErr := <-serverResult; serverErr != nil {
		t.Fatal(serverErr)
	}
	invalid := dispatcher.Handle(context.Background(), map[string]any{
		"action": "tab_reorder", "request_id": "move-invalid", "pane_id": "pane-1", "insert_index": 1.5,
	})
	if invalid.OK || invalid.Error != "Tab position is invalid" {
		t.Fatalf("fractional tab position accepted: %+v", invalid)
	}
}

func TestApprovalRetryAfterStatusChangeReturnsStoredPhase(t *testing.T) {
	dir := t.TempDir()
	record := filepath.Join(dir, "approvals.log")
	bin := writeScript(t, dir, "herdr", "#!/bin/sh\n"+
		"printf '%s\\n' \"$*\" >> \""+record+"\"\n"+
		"if [ \"$1 $2\" = \"pane read\" ]; then\n"+
		"  printf '"+approvalPane+"\\n'\n"+
		"else\n"+
		"  printf '{\"ok\":true}\\n'\n"+
		"fi\n")
	state := NewState(testLogger())
	commitApproval(state, "pane-1")
	eventID := blockedEventID(t, &Dispatcher{state: state}, "pane-1")
	d := NewDispatcher(herdr.NewClient(bin, filepath.Join(dir, "sock")), state, nil, testLogger())
	fingerprint, choice := approvalIdentity(t, d, "pane-1", 0)
	updates := make(chan map[string]any, 1)
	d.SetBroadcast(func(message any) {
		if update, ok := message.(map[string]any); ok {
			updates <- update
		}
	})
	message := map[string]any{
		"action": "respond", "request_id": "approval-1", "pane_id": "pane-1",
		"event_id": eventID, "approval_fingerprint": fingerprint, "choice": choice,
		"index": float64(0), "total": float64(2),
	}
	first := d.Handle(context.Background(), message)
	if !first.OK || first.Phase != "accepted" {
		t.Fatalf("first approval = %+v", first)
	}
	state.CommitEvent("pane-1", "idle", time.Now().UnixMilli())
	select {
	case update := <-updates:
		if update["phase"] != "confirmed" {
			t.Fatalf("watch update = %+v", update)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("confirmation watch did not complete")
	}
	replay := d.Handle(context.Background(), message)
	if !replay.OK || replay.Phase != "confirmed" {
		t.Fatalf("approval replay = %+v, want stored confirmed phase", replay)
	}
	data, _ := os.ReadFile(record)
	if sends := strings.Count(string(data), "send-keys"); sends != 1 {
		t.Fatalf("approval sends = %d, want 1\n%s", sends, data)
	}
}

// §9.6: repeating answer_question with the same interaction_id must send the
// answer keys only once.
func TestDuplicateQuestionAnswerSendsOnce(t *testing.T) {
	dir := t.TempDir()
	record := filepath.Join(dir, "keys.log")
	questionView := "Which deployment target?\n❯ 1. Development\n2. Staging\n3. Type something.\n4. Chat about this\nEnter to select · ↑/↓ to navigate · Esc to cancel"
	interaction := question.Parse(questionView, "claude")
	if interaction == nil {
		t.Fatal("test question did not parse")
	}
	bin := writeScript(t, dir, "herdr", "#!/bin/sh\n"+
		"printf '%s\\n' \"$*\" >> \""+record+"\"\n"+
		"if [ \"$1 $2\" = \"pane read\" ]; then\n"+
		"  printf '%s\\n' '"+questionView+"'\n"+
		"else\n"+
		"  printf '{\"ok\":true}\\n'\n"+
		"fi\n")

	d := NewDispatcher(herdr.NewClient(bin, filepath.Join(dir, "sock")), NewState(testLogger()), nil, testLogger())
	d.state.CommitInventory([]*AgentState{{PaneID: "pane-1", Agent: "claude", Status: "blocked"}}, d.state.RevisionCounter())

	answer := func() *CommandResult {
		return d.Handle(context.Background(), map[string]any{
			"action":           "answer_question",
			"request_id":       "q",
			"pane_id":          "pane-1",
			"interaction_id":   interaction.ID,
			"selected_indices": []any{float64(0)},
		})
	}

	answer() // first submission
	answer() // duplicate: retry or a second client on the same interaction

	conflict := d.Handle(context.Background(), map[string]any{
		"action":           "answer_question",
		"request_id":       "q",
		"pane_id":          "pane-1",
		"interaction_id":   interaction.ID,
		"selected_indices": []any{float64(1)},
	})
	if conflict.OK || !strings.Contains(conflict.Error, "different response") {
		t.Fatalf("same request with different answer = %+v, want conflict", conflict)
	}

	data, _ := os.ReadFile(record)
	// Each answer confirms with a single Enter; a deduped answer sends it once.
	enters := strings.Count(string(data), "Enter")
	if enters != 1 {
		t.Fatalf("duplicate question answer confirmed %d times, want 1\nrecord:\n%s", enters, data)
	}
}

func TestQuestionWatcherWaitsThroughTransientUnparseableFrames(t *testing.T) {
	tests := []struct {
		name          string
		initialView   string
		nextView      string
		action        string
		direction     string
		expectedPhase string
	}{
		{
			name:          "answer advances",
			initialView:   "Question 1/2 (2 unanswered)\nChoose the first value\n\n❯ 1. Alpha\n  2. Beta\n  3. None of the above\n\ntab to add notes | enter to submit answer | ←/→ to navigate questions",
			nextView:      "Question 2/2 (1 unanswered)\nChoose the second value\n\n❯ 1. Gamma\n  2. Delta\n  3. None of the above\n\ntab to add notes | enter to submit all | ←/→ to navigate questions",
			action:        "answer_question",
			expectedPhase: "advanced",
		},
		{
			name:          "previous navigates",
			initialView:   "Question 2/2 (1 unanswered)\nChoose the second value\n\n❯ 1. Gamma\n  2. Delta\n  3. None of the above\n\ntab to add notes | enter to submit all | ←/→ to navigate questions",
			nextView:      "Question 1/2 (2 unanswered)\nChoose the first value\n\n❯ 1. Alpha\n  2. Beta\n  3. None of the above\n\ntab to add notes | enter to submit answer | ←/→ to navigate questions",
			action:        "navigate_question",
			direction:     "previous",
			expectedPhase: "navigated",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			dir := t.TempDir()
			countFile := filepath.Join(dir, "reads")
			if err := os.WriteFile(countFile, []byte("0\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			bin := writeScript(t, dir, "herdr", "#!/bin/sh\n"+
				"if [ \"$1 $2 $3\" = \"pane read pane-1\" ]; then\n"+
				"  count=$(cat \""+countFile+"\")\n"+
				"  count=$((count + 1))\n"+
				"  printf '%s\\n' \"$count\" > \""+countFile+"\"\n"+
				"  if [ \"$count\" -eq 1 ]; then\n"+
				"    printf '%s\\n' '"+test.initialView+"'\n"+
				"  elif [ \"$count\" -eq 2 ]; then\n"+
				"    printf '%s\\n' 'redrawing question'\n"+
				"  else\n"+
				"    printf '%s\\n' '"+test.nextView+"'\n"+
				"  fi\n"+
				"else\n"+
				"  printf '{\"ok\":true}\\n'\n"+
				"fi\n")
			state := NewState(testLogger())
			state.CommitInventory([]*AgentState{{
				PaneID: "pane-1", Agent: "codex", Status: "blocked",
			}}, state.RevisionCounter())
			d := NewDispatcher(herdr.NewClient(bin, filepath.Join(dir, "sock")), state, nil, testLogger())
			t.Cleanup(func() {
				ctx, cancel := context.WithTimeout(context.Background(), time.Second)
				defer cancel()
				_ = d.Close(ctx)
			})
			updates := make(chan map[string]any, 8)
			d.SetBroadcast(func(message any) {
				if update, ok := message.(map[string]any); ok {
					updates <- update
				}
			})
			interaction := question.Parse(test.initialView, "codex")
			if interaction == nil {
				t.Fatal("initial question did not parse")
			}
			message := map[string]any{
				"action":         test.action,
				"request_id":     "transition",
				"pane_id":        "pane-1",
				"interaction_id": interaction.ID,
			}
			if test.direction != "" {
				message["direction"] = test.direction
			} else {
				message["selected_indices"] = []any{float64(0)}
			}
			if accepted := d.Handle(context.Background(), message); !accepted.OK || accepted.Phase != "accepted" {
				t.Fatalf("accepted result = %+v", accepted)
			}
			timeout := time.After(3 * time.Second)
			for {
				select {
				case update := <-updates:
					if update["request_id"] != "transition" {
						continue
					}
					if update["phase"] == "confirmed" {
						t.Fatalf("transient frame was treated as final: %+v", update)
					}
					if update["phase"] == test.expectedPhase {
						return
					}
				case <-timeout:
					t.Fatalf("no %s result", test.expectedPhase)
				}
			}
		})
	}
}

func TestAcknowledgeOnlyBroadcastsDisplayedStatusChanges(t *testing.T) {
	state := NewState(testLogger())
	state.CommitInventory([]*AgentState{{
		PaneID: "pane-1", Agent: "claude", Status: "blocked",
	}}, state.RevisionCounter())
	d := NewDispatcher(nil, state, nil, testLogger())
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_ = d.Close(ctx)
	})
	updates := make(chan map[string]any, 2)
	d.SetBroadcast(func(message any) {
		if update, ok := message.(map[string]any); ok {
			updates <- update
		}
	})

	if result := d.handleAcknowledge("blocked", "pane-1"); !result.OK {
		t.Fatalf("blocked acknowledgement = %+v", result)
	}
	select {
	case update := <-updates:
		t.Fatalf("unchanged blocked status broadcast a sparse update: %+v", update)
	default:
	}

	state.CommitInventory([]*AgentState{{
		PaneID: "pane-1", Agent: "claude", Status: "done",
	}}, state.RevisionCounter())
	if result := d.handleAcknowledge("done", "pane-1"); !result.OK {
		t.Fatalf("done acknowledgement = %+v", result)
	}
	select {
	case update := <-updates:
		if update["status"] != "idle" {
			t.Fatalf("acknowledgement update = %+v", update)
		}
	case <-time.After(time.Second):
		t.Fatal("displayed done-to-idle transition was not broadcast")
	}
}

func TestQuestionNavigationHasRequestIdentityAndReplaysTerminalInteraction(t *testing.T) {
	dir := t.TempDir()
	record := filepath.Join(dir, "keys.log")
	stateFile := filepath.Join(dir, "pane.txt")
	firstFile := filepath.Join(dir, "first.txt")
	secondFile := filepath.Join(dir, "second.txt")
	firstView := "Question 1/2 (1 unanswered)\nChoose the first value\n\n❯ 1. Alpha\n  2. Beta\n  3. None of the above\n\ntab to add notes | enter to submit answer | ←/→ to navigate questions"
	secondView := "Question 2/2 (1 unanswered)\nChoose the second value\n\n❯ 1. Gamma\n  2. Delta\n  3. None of the above\n\ntab to add notes | enter to submit all | ←/→ to navigate questions"
	if err := os.WriteFile(firstFile, []byte(firstView), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(secondFile, []byte(secondView), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(stateFile, []byte(secondView), 0o600); err != nil {
		t.Fatal(err)
	}
	bin := writeScript(t, dir, "herdr", "#!/bin/sh\n"+
		"printf '%s\\n' \"$*\" >> \""+record+"\"\n"+
		"if [ \"$1 $2 $3\" = \"pane read pane-1\" ]; then\n"+
		"  cat \""+stateFile+"\"\n"+
		"elif printf '%s' \"$*\" | grep -q 'Left'; then\n"+
		"  cp \""+firstFile+"\" \""+stateFile+"\"\n"+
		"  printf '{\"ok\":true}\\n'\n"+
		"elif printf '%s' \"$*\" | grep -q 'Enter'; then\n"+
		"  cp \""+secondFile+"\" \""+stateFile+"\"\n"+
		"  printf '{\"ok\":true}\\n'\n"+
		"else\n"+
		"  printf '{\"ok\":true}\\n'\n"+
		"fi\n")

	state := NewState(testLogger())
	state.CommitInventory([]*AgentState{{PaneID: "pane-1", Agent: "codex", Status: "blocked"}}, state.RevisionCounter())
	d := NewDispatcher(herdr.NewClient(bin, filepath.Join(dir, "sock")), state, nil, testLogger())
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		if err := d.Close(ctx); err != nil {
			t.Errorf("close dispatcher: %v", err)
		}
	})
	updates := make(chan map[string]any, 8)
	d.SetBroadcast(func(message any) {
		if update, ok := message.(map[string]any); ok {
			updates <- update
		}
	})
	second := question.Parse(secondView, "codex")
	first := question.Parse(firstView, "codex")
	if first == nil || second == nil {
		t.Fatalf("question fixtures did not parse: first=%+v second=%+v", first, second)
	}

	navigate := func(requestID string, interaction *question.Interaction) *CommandResult {
		return d.Handle(context.Background(), map[string]any{
			"action":         "navigate_question",
			"request_id":     requestID,
			"pane_id":        "pane-1",
			"interaction_id": interaction.ID,
			"direction":      "previous",
		})
	}
	awaitUpdate := func(requestID, phase string) map[string]any {
		t.Helper()
		timeout := time.After(2 * time.Second)
		for {
			select {
			case update := <-updates:
				if update["request_id"] == requestID && update["phase"] == phase {
					return update
				}
			case <-timeout:
				t.Fatalf("no %s update for %s", phase, requestID)
			}
		}
	}

	if result := navigate("previous-1", second); !result.OK || result.Phase != "accepted" {
		t.Fatalf("first navigation = %+v", result)
	}
	firstUpdate := awaitUpdate("previous-1", "navigated")
	firstData, _ := firstUpdate["data"].(map[string]any)
	firstInteraction, _ := firstData["interaction"].(*question.Interaction)
	if firstInteraction == nil || firstInteraction.QuestionIndex != 1 {
		t.Fatalf("first navigation data = %#v", firstUpdate["data"])
	}
	replay := navigate("previous-1", second)
	replayedData, _ := replay.Data.(map[string]any)
	replayedInteraction, _ := replayedData["interaction"].(*question.Interaction)
	if !replay.OK || replay.Phase != "navigated" || replayedInteraction == nil ||
		replayedInteraction.QuestionIndex != 1 {
		t.Fatalf("navigation replay = %+v", replay)
	}

	answer := d.Handle(context.Background(), map[string]any{
		"action":           "answer_question",
		"request_id":       "answer-1",
		"pane_id":          "pane-1",
		"interaction_id":   first.ID,
		"selected_indices": []any{float64(0)},
	})
	if !answer.OK || answer.Phase != "accepted" {
		t.Fatalf("answer = %+v", answer)
	}
	awaitUpdate("answer-1", "advanced")

	if result := navigate("previous-2", second); !result.OK || result.Phase != "accepted" {
		t.Fatalf("second navigation = %+v", result)
	}
	awaitUpdate("previous-2", "navigated")

	changedAnswer := d.Handle(context.Background(), map[string]any{
		"action":           "answer_question",
		"request_id":       "answer-2",
		"pane_id":          "pane-1",
		"interaction_id":   first.ID,
		"selected_indices": []any{float64(1)},
	})
	if !changedAnswer.OK || changedAnswer.Phase != "accepted" {
		t.Fatalf("changed answer after returning to question = %+v", changedAnswer)
	}
	awaitUpdate("answer-2", "advanced")

	data, err := os.ReadFile(record)
	if err != nil {
		t.Fatal(err)
	}
	if lefts := strings.Count(string(data), "Left"); lefts != 2 {
		t.Fatalf("Left dispatches = %d, want 2\n%s", lefts, data)
	}
	if enters := strings.Count(string(data), "Enter"); enters != 2 {
		t.Fatalf("answer dispatches = %d, want 2\n%s", enters, data)
	}
}

func TestUnchangedQuestionNavigationReturnsReplayableError(t *testing.T) {
	dir := t.TempDir()
	questionView := "Question 2/2 (1 unanswered)\nChoose the second value\n\n❯ 1. Gamma\n  2. Delta\n  3. None of the above\n\ntab to add notes | enter to submit all | ←/→ to navigate questions"
	interaction := question.Parse(questionView, "codex")
	if interaction == nil {
		t.Fatal("question fixture did not parse")
	}
	bin := writeScript(t, dir, "herdr", "#!/bin/sh\n"+
		"if [ \"$1 $2 $3\" = \"pane read pane-1\" ]; then\n"+
		"  printf '%s\\n' '"+questionView+"'\n"+
		"else\n"+
		"  printf '{\"ok\":true}\\n'\n"+
		"fi\n")
	state := NewState(testLogger())
	state.CommitInventory([]*AgentState{{PaneID: "pane-1", Agent: "codex", Status: "blocked"}}, state.RevisionCounter())
	d := NewDispatcher(herdr.NewClient(bin, filepath.Join(dir, "sock")), state, nil, testLogger())
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_ = d.Close(ctx)
	})
	updates := make(chan map[string]any, 1)
	d.SetBroadcast(func(message any) {
		if update, ok := message.(map[string]any); ok {
			updates <- update
		}
	})
	message := map[string]any{
		"action":         "navigate_question",
		"request_id":     "unchanged",
		"pane_id":        "pane-1",
		"interaction_id": interaction.ID,
		"direction":      "previous",
	}
	if accepted := d.Handle(context.Background(), message); !accepted.OK || accepted.Phase != "accepted" {
		t.Fatalf("accepted = %+v", accepted)
	}
	select {
	case update := <-updates:
		if update["ok"] != false || update["phase"] != "unconfirmed" ||
			!strings.Contains(fmt.Sprint(update["error"]), "still shows the same question") {
			t.Fatalf("unconfirmed update = %+v", update)
		}
	case <-time.After(approvalPollTimeout + 2*time.Second):
		t.Fatal("unchanged navigation did not fail")
	}
	replay := d.Handle(context.Background(), message)
	if replay.OK || replay.Phase != "unconfirmed" ||
		!strings.Contains(replay.Error, "still shows the same question") {
		t.Fatalf("unconfirmed replay = %+v", replay)
	}
}

func TestSchedulerEvictsOldCompletedLedgerEntries(t *testing.T) {
	scheduler := NewScheduler(1, testLogger())
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		if err := scheduler.Close(ctx); err != nil {
			t.Errorf("close scheduler: %v", err)
		}
	})

	executions := make(map[string]int)
	run := func(key string) *CommandResult {
		now := time.Now()
		result, err := scheduler.Execute(context.Background(), ScheduleOptions{
			Command: Command{
				ID: scheduler.NextCommandID(), RequestID: key, ReceivedAt: now,
				Deadline: now.Add(time.Second), Kind: CommandStart,
			},
			RelayLevel:  true,
			LedgerKey:   key,
			PayloadHash: "same",
		}, EffectFunc(func(context.Context, WorkerToken) EffectResult {
			executions[key]++
			return EffectResult{Result: completed(key, "agent_start", "", nil)}
		}))
		if err != nil {
			t.Fatal(err)
		}
		return result
	}

	for index := 0; index < maxLedgerEntries+8; index++ {
		run(fmt.Sprintf("start-%03d", index))
	}
	run("start-000")
	if executions["start-000"] != 2 {
		t.Fatalf("old ledger entry executions = %d, want eviction and rerun", executions["start-000"])
	}
	latest := fmt.Sprintf("start-%03d", maxLedgerEntries+7)
	result := run(latest)
	if executions[latest] != 1 || result == nil || !result.replayed {
		t.Fatalf("latest ledger entry was not replayed: executions=%d result=%+v", executions[latest], result)
	}
}
