package coordinator

import (
	"context"
	"errors"
	"reflect"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/0cv/herdr-mobile-relay/internal/backend"
	"github.com/0cv/herdr-mobile-relay/internal/herdr"
)

type fakeCall struct {
	Method string
	Args   []any
}

type fakeHook func(ctx context.Context, call fakeCall) error

// fakeBackend implements the Herdr calls a test uses. Calling any other method panics on the nil embedded interface.
type fakeBackend struct {
	backend.Backend
	mu         sync.Mutex
	calls      []fakeCall
	hooks      map[string]fakeHook
	panes      []herdr.Pane
	workspaces []herdr.Workspace
	tabs       []herdr.Tab
	contents   map[string]string
	readErr    error
	agent      herdr.AgentInfo
	status     herdr.ServerStatus

	// target is the pane that workspace and tab creation make, and that a launch returns.
	target herdr.Pane
	// ready is what a launch reports about the new terminal being ready for a prompt.
	ready bool
	// inventoryOverrides are served by the next GetInventory calls, one per call, before the canned panes.
	inventoryOverrides [][]herdr.Pane
}

func newFakeBackend() *fakeBackend {
	return &fakeBackend{
		hooks:    map[string]fakeHook{},
		contents: map[string]string{},
		status:   herdr.NewClient("", "").CapabilityStatus(),
	}
}

func newTestDispatcher(t *testing.T, client backend.Backend, state *State) *Dispatcher {
	t.Helper()
	dispatcher := NewDispatcher(client, state, nil, testLogger())
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_ = dispatcher.Close(ctx)
	})
	return dispatcher
}

func newTestScheduler(t *testing.T, capacity int) *Scheduler {
	t.Helper()
	scheduler := NewScheduler(capacity)
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		if err := scheduler.Close(ctx); err != nil {
			t.Errorf("close scheduler: %v", err)
		}
	})
	return scheduler
}

func startedFailure(message string) error {
	return &herdr.OutcomeError{Started: true, Err: errors.New(message)}
}

func refusalByCode(code string) error {
	return &herdr.OutcomeError{Started: true, Err: &herdr.CLIError{Code: code}}
}

func (b *fakeBackend) enableOneStepLaunch() {
	b.update(func(b *fakeBackend) {
		b.status.OneStepLaunch = true
		b.status.Features = map[string]herdr.FeatureEvidence{herdr.FeatureAgentLaunch: {State: herdr.FeatureSupported}}
	})
}

func (b *fakeBackend) update(change func(*fakeBackend)) {
	b.mu.Lock()
	defer b.mu.Unlock()
	change(b)
}

func (b *fakeBackend) setHook(method string, hook fakeHook) {
	b.update(func(b *fakeBackend) { b.hooks[method] = hook })
}

func (b *fakeBackend) recorded() []fakeCall {
	b.mu.Lock()
	defer b.mu.Unlock()
	return slices.Clone(b.calls)
}

func (b *fakeBackend) callsTo(method string) []fakeCall {
	var matching []fakeCall
	for _, call := range b.recorded() {
		if call.Method == method {
			matching = append(matching, call)
		}
	}
	return matching
}

func (b *fakeBackend) count(method string) int {
	return len(b.callsTo(method))
}

func (b *fakeBackend) invoke(ctx context.Context, method string, args ...any) error {
	if err := herdr.CheckDispatch(ctx); err != nil {
		return &herdr.OutcomeError{Started: false, Err: err}
	}
	call := fakeCall{Method: method, Args: args}
	b.mu.Lock()
	b.calls = append(b.calls, call)
	hook := b.hooks[method]
	b.mu.Unlock()
	if hook == nil {
		return nil
	}
	return hook(ctx, call)
}

func (b *fakeBackend) GetInventory(ctx context.Context) (*herdr.Inventory, error) {
	if err := b.invoke(ctx, "GetInventory"); err != nil {
		return nil, err
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if len(b.inventoryOverrides) > 0 {
		panes := b.inventoryOverrides[0]
		b.inventoryOverrides = b.inventoryOverrides[1:]
		return &herdr.Inventory{Panes: slices.Clone(panes)}, nil
	}
	return &herdr.Inventory{Panes: slices.Clone(b.panes)}, nil
}

func (b *fakeBackend) PaneList(ctx context.Context) ([]herdr.Pane, error) {
	if err := b.invoke(ctx, "PaneList"); err != nil {
		return nil, err
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	return slices.Clone(b.panes), nil
}

func (b *fakeBackend) WorkspaceList(ctx context.Context) ([]herdr.Workspace, error) {
	if err := b.invoke(ctx, "WorkspaceList"); err != nil {
		return nil, err
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	return slices.Clone(b.workspaces), nil
}

func (b *fakeBackend) TabList(ctx context.Context) ([]herdr.Tab, error) {
	if err := b.invoke(ctx, "TabList"); err != nil {
		return nil, err
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	return slices.Clone(b.tabs), nil
}

func (b *fakeBackend) WorkspaceCreate(ctx context.Context, cwd, label string) (*herdr.CreateResult, error) {
	return b.createTarget(ctx, "WorkspaceCreate", cwd, label)
}

func (b *fakeBackend) TabCreate(ctx context.Context, workspaceID, cwd, label string) (*herdr.CreateResult, error) {
	return b.createTarget(ctx, "TabCreate", workspaceID, cwd, label)
}

func (b *fakeBackend) createTarget(ctx context.Context, method string, args ...any) (*herdr.CreateResult, error) {
	if err := b.invoke(ctx, method, args...); err != nil {
		return nil, err
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	serving := slices.ContainsFunc(b.panes, func(pane herdr.Pane) bool { return pane.ID == b.target.ID })
	if b.target.ID != "" && !serving {
		b.panes = append(b.panes, b.target)
	}
	return &herdr.CreateResult{PaneID: b.target.ID, TabID: b.target.TabID, WorkspaceID: b.target.WorkspaceID}, nil
}

func (b *fakeBackend) WorkspaceRename(ctx context.Context, workspaceID, label string) error {
	return b.invoke(ctx, "WorkspaceRename", workspaceID, label)
}

func (b *fakeBackend) WorkspaceMoveBlock(ctx context.Context, workspaceIDs []string, beforeWorkspaceID string) error {
	return b.invoke(ctx, "WorkspaceMoveBlock", slices.Clone(workspaceIDs), beforeWorkspaceID)
}

func (b *fakeBackend) WorkspaceClose(ctx context.Context, workspaceID string, closeGroup bool) error {
	return b.invoke(ctx, "WorkspaceClose", workspaceID, closeGroup)
}

func (b *fakeBackend) ReadPane(ctx context.Context, paneID string, lines int, format string) (herdr.PaneRead, error) {
	return b.read(ctx, "ReadPane", paneID, lines, format)
}

func (b *fakeBackend) ReadPaneRecent(ctx context.Context, paneID string, lines int, format string) (herdr.PaneRead, error) {
	return b.read(ctx, "ReadPaneRecent", paneID, lines, format)
}

func (b *fakeBackend) ReadPaneVisible(ctx context.Context, paneID string, lines int, format string) (herdr.PaneRead, error) {
	return b.read(ctx, "ReadPaneVisible", paneID, lines, format)
}

func (b *fakeBackend) read(ctx context.Context, method, paneID string, lines int, format string) (herdr.PaneRead, error) {
	if err := b.invoke(ctx, method, paneID, lines, format); err != nil {
		return herdr.PaneRead{}, err
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.readErr != nil {
		return herdr.PaneRead{}, b.readErr
	}
	return herdr.PaneRead{Content: []byte(b.contents[paneID])}, nil
}

func (b *fakeBackend) Prompt(ctx context.Context, paneID, text string) error {
	return b.invoke(ctx, "Prompt", paneID, text)
}

func (b *fakeBackend) SendKeys(ctx context.Context, paneID string, keys []string) error {
	return b.invoke(ctx, "SendKeys", paneID, slices.Clone(keys))
}

func (b *fakeBackend) SendSecret(ctx context.Context, paneID, text string) error {
	return b.invoke(ctx, "SendSecret", paneID, text)
}

func (b *fakeBackend) SendInput(ctx context.Context, paneID string, input herdr.PaneInput) error {
	return b.invoke(ctx, "SendInput", paneID, input)
}

func (b *fakeBackend) SendLiteralKey(ctx context.Context, paneID, character string) error {
	return b.invoke(ctx, "SendLiteralKey", paneID, character)
}

func (b *fakeBackend) SendText(ctx context.Context, paneID, text string) error {
	return b.invoke(ctx, "SendText", paneID, text)
}

func (b *fakeBackend) StartAgent(ctx context.Context, name, kind, paneID string, timeoutMs int, agentArgs ...string) (string, error) {
	if err := b.invoke(ctx, "StartAgent", name, kind, paneID, timeoutMs, slices.Clone(agentArgs)); err != nil {
		return "", err
	}
	return paneID, nil
}

func (b *fakeBackend) LaunchTerminal(ctx context.Context, argv []string, cwd, name, workspaceID, tabID string, waitReady bool) (*herdr.Pane, bool, error) {
	if err := b.invoke(ctx, "LaunchTerminal", slices.Clone(argv), cwd, name, workspaceID, tabID, waitReady); err != nil {
		return nil, false, err
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	pane := b.target
	pane.Cwd, pane.Name = cwd, name
	return &pane, b.ready, nil
}

func (b *fakeBackend) PaneRun(ctx context.Context, paneID string, argv []string) error {
	return b.invoke(ctx, "PaneRun", paneID, slices.Clone(argv))
}

func (b *fakeBackend) TabRename(ctx context.Context, tabID, label string) error {
	return b.invoke(ctx, "TabRename", tabID, label)
}

func (b *fakeBackend) TabMove(ctx context.Context, tabID string, insertIndex int) error {
	return b.invoke(ctx, "TabMove", tabID, insertIndex)
}

func (b *fakeBackend) AgentGet(ctx context.Context, paneID string) (*herdr.AgentInfo, error) {
	if err := b.invoke(ctx, "AgentGet", paneID); err != nil {
		return nil, err
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	info := b.agent
	return &info, nil
}

func (b *fakeBackend) RenameAgent(ctx context.Context, paneID, name string) error {
	return b.invoke(ctx, "RenameAgent", paneID, name)
}

func (b *fakeBackend) StopPane(ctx context.Context, paneID string) error {
	return b.invoke(ctx, "StopPane", paneID)
}

func (b *fakeBackend) CapabilityStatus() herdr.ServerStatus {
	b.mu.Lock()
	hook := b.hooks["CapabilityStatus"]
	b.mu.Unlock()
	if hook != nil {
		_ = hook(context.Background(), fakeCall{Method: "CapabilityStatus"})
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.status
}

func TestFakeBackendRecordsCallsInOrder(t *testing.T) {
	client := newFakeBackend()
	ctx := context.Background()
	_ = client.SendKeys(ctx, "pane-1", []string{"Up", "Enter"})
	_, _ = client.PaneList(ctx)
	_ = client.SendKeys(ctx, "pane-2", []string{"Down"})
	_ = client.CapabilityStatus()
	want := []fakeCall{
		{Method: "SendKeys", Args: []any{"pane-1", []string{"Up", "Enter"}}},
		{Method: "PaneList", Args: nil},
		{Method: "SendKeys", Args: []any{"pane-2", []string{"Down"}}},
	}
	if got := client.recorded(); !reflect.DeepEqual(got, want) {
		t.Fatalf("calls = %+v, want %+v", got, want)
	}
	if got := client.callsTo("SendKeys"); len(got) != 2 || client.count("SendKeys") != 2 {
		t.Fatalf("SendKeys calls = %+v", got)
	}
}

func TestFakeBackendHookBlocksUntilContextEnds(t *testing.T) {
	client := newFakeBackend()
	started := make(chan struct{})
	client.setHook("Prompt", func(ctx context.Context, call fakeCall) error {
		close(started)
		<-ctx.Done()
		return &herdr.OutcomeError{Started: true, Err: ctx.Err()}
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- client.Prompt(ctx, "pane-1", "hello") }()
	<-started
	select {
	case err := <-done:
		t.Fatalf("prompt returned before the context ended: %v", err)
	case <-time.After(20 * time.Millisecond):
	}
	cancel()
	err := <-done
	if !errors.Is(err, herdr.ErrDispatchedUnknown) || !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v, want dispatched-unknown cancellation", err)
	}
	if client.count("Prompt") != 1 {
		t.Fatalf("calls = %+v", client.recorded())
	}
}

func TestFakeBackendRefusingDispatchCheckRecordsNothing(t *testing.T) {
	refusal := errors.New("revoked")
	ctx := herdr.WithDispatchCheck(context.Background(), func() error { return refusal })
	for name, call := range map[string]func(*fakeBackend) error{
		"read":  func(b *fakeBackend) error { _, err := b.PaneList(ctx); return err },
		"write": func(b *fakeBackend) error { return b.SendKeys(ctx, "pane-1", []string{"Enter"}) },
	} {
		t.Run(name, func(t *testing.T) {
			client := newFakeBackend()
			hooked := false
			client.setHook("PaneList", func(context.Context, fakeCall) error { hooked = true; return nil })
			client.setHook("SendKeys", func(context.Context, fakeCall) error { hooked = true; return nil })
			err := call(client)
			if !errors.Is(err, herdr.ErrNotStarted) || !errors.Is(err, refusal) || errors.Is(err, herdr.ErrDispatchedUnknown) {
				t.Fatalf("error = %v, want not-started refusal", err)
			}
			if calls := client.recorded(); len(calls) != 0 || hooked {
				t.Fatalf("refused call was recorded or hooked: %+v", calls)
			}
		})
	}
}

func TestFakeBackendDefaultCapabilitiesMatchFreshClient(t *testing.T) {
	got := newFakeBackend().CapabilityStatus()
	want := herdr.NewClient("herdr", "").CapabilityStatus()
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("status = %+v, want %+v", got, want)
	}
}
