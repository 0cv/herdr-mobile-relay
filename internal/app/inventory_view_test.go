package app

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/0cv/herdr-mobile-relay/internal/config"
	"github.com/0cv/herdr-mobile-relay/internal/coordinator"
	"github.com/0cv/herdr-mobile-relay/internal/herdr"
	"github.com/0cv/herdr-mobile-relay/internal/push"
	"github.com/0cv/herdr-mobile-relay/internal/question"
	"github.com/0cv/herdr-mobile-relay/internal/transport"
	"github.com/coder/websocket"
)

func TestPublishCurrentInventoryRepairsReadyRecovery(t *testing.T) {
	server := testServer()
	t.Cleanup(func() {
		if server.conversationB != nil {
			_ = server.conversationB.Close()
		}
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_ = server.hub.Shutdown(ctx)
	})
	server.state.CommitWorkspaces([]herdr.Workspace{{ID: "workspace-1", Label: "Project"}})
	server.setInventoryPublisher(context.Background())
	server.state.CommitInventory([]*coordinator.AgentState{{
		PaneID: "pane-1", Agent: "codex", Status: "working", Project: "Project",
	}}, server.state.RevisionCounter())
	publishInventoryForTest(t, server)
	server.state.MarkInventoryFailure(errors.New("topology churn"))
	publishInventoryForTest(t, server)
	if got := server.committedInventoryStatus()["state"]; got != "error" {
		t.Fatalf("degraded committed status = %v, want error", got)
	}
	server.state.CommitInventory(server.state.Snapshot(), server.state.RevisionCounter())
	publishInventoryForTest(t, server)
	committed := server.committedInventorySnapshot()
	if committed.status["state"] != "ready" || len(committed.agents) != 1 || len(committed.workspaces) != 1 {
		t.Fatalf("recovery committed snapshot = %#v, want ready topology", committed)
	}
}

func TestPublishCurrentInventoryCommitsEmptyReadyRecoveryWithoutClients(t *testing.T) {
	server := testServer()
	t.Cleanup(func() {
		if server.conversationB != nil {
			_ = server.conversationB.Close()
		}
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_ = server.hub.Shutdown(ctx)
	})
	server.setInventoryPublisher(context.Background())
	server.state.CommitInventory(nil, server.state.RevisionCounter())
	publishInventoryForTest(t, server)
	server.state.MarkInventoryFailure(errors.New("command failed"))
	publishInventoryForTest(t, server)
	server.state.CommitInventory(nil, server.state.RevisionCounter())
	publishInventoryForTest(t, server)
	committed := server.committedInventorySnapshot()
	if committed.status["state"] != "ready" || committed.agents == nil || committed.workspaces == nil {
		t.Fatalf("empty recovery committed snapshot = %#v", committed)
	}
	if len(committed.agents) != 0 || len(committed.workspaces) != 0 {
		t.Fatalf("empty recovery retained topology: %#v", committed)
	}
}

func TestPublishCurrentInventoryOrdersRecoveryForConnectedRefreshAndReconnect(t *testing.T) {
	server := testServer()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	defer server.hub.Shutdown(ctx)
	server.setInventoryPublisher(ctx)
	server.state.CommitWorkspaces([]herdr.Workspace{{ID: "workspace-1", Label: "Project"}})
	server.state.CommitInventory([]*coordinator.AgentState{{PaneID: "pane-1", Agent: "codex", Status: "idle"}}, server.state.RevisionCounter())
	publishInventoryForTest(t, server)
	connected := make(chan *transport.ClientConn, 2)
	server.hub.SetOnConnect(func(client *transport.ClientConn) { connected <- client })
	readType := func(conn *websocket.Conn) string {
		_, data, err := conn.Read(ctx)
		if err != nil {
			t.Fatal(err)
		}
		var message map[string]any
		if err := json.Unmarshal(data, &message); err != nil {
			t.Fatal(err)
		}
		typeName, _ := message["type"].(string)
		return typeName
	}
	conn := dialHub(t, server)
	client := <-connected
	server.state.MarkInventoryFailure(errors.New("topology churn"))
	publishInventoryForTest(t, server)
	if got := readType(conn); got != "inventory_status" {
		t.Fatalf("degraded connected frame = %q, want inventory_status", got)
	}
	server.state.CommitInventory([]*coordinator.AgentState{{PaneID: "pane-1", Agent: "codex", Status: "idle"}}, server.state.RevisionCounter())
	publishInventoryForTest(t, server)
	for index, want := range []string{"inventory_status", "agents", "workspaces"} {
		if got := readType(conn); got != want {
			t.Fatalf("recovery frame %d = %q, want %q", index, got, want)
		}
	}
	server.requestAgentRefresh(client)
	for index, want := range []string{"inventory_status", "agents", "workspaces"} {
		if got := readType(conn); got != want {
			t.Fatalf("immediate refresh frame %d = %q, want %q", index, got, want)
		}
	}
	// The production refresh handler queues a deferred completion. An unchanged
	// successful publication must drain it even though no agent diff occurred.
	publishInventoryForTest(t, server)
	for index, want := range []string{"inventory_status", "agents", "workspaces"} {
		if got := readType(conn); got != want {
			t.Fatalf("unchanged deferred refresh frame %d = %q, want %q", index, got, want)
		}
	}
	conn.CloseNow()

	server.hub.SetOnConnect(func(client *transport.ClientConn) {
		server.sendConnectionSnapshot(client)
		connected <- client
	})
	reconnected := dialHub(t, server)
	<-connected
	for index, want := range []string{"push_config", "agents", "workspaces", "activity_history", "inventory_status"} {
		if got := readType(reconnected); got != want {
			t.Fatalf("reconnect frame %d = %q, want %q", index, got, want)
		}
	}
	reconnected.CloseNow()
}

func TestProductionInventoryPublisherOrdersRefreshAndRegistrationBarriers(t *testing.T) {
	server := testServer()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	defer func() {
		if server.conversationB != nil {
			_ = server.conversationB.Close()
		}
		_ = server.hub.Shutdown(ctx)
	}()
	server.setInventoryPublisher(ctx)
	workspace := herdr.Workspace{ID: "workspace-1", Label: "Project"}
	agent := &coordinator.AgentState{PaneID: "pane-1", Agent: "codex", Status: "idle", Project: "Project"}
	server.state.CommitWorkspaces([]herdr.Workspace{workspace})
	server.state.CommitInventory([]*coordinator.AgentState{agent}, server.state.RevisionCounter())
	if err := server.publishCurrentInventory(ctx); err != nil {
		t.Fatal(err)
	}

	connected := make(chan *transport.ClientConn, 2)
	server.hub.SetOnConnect(func(client *transport.ClientConn) {
		server.sendConnectionSnapshot(client)
		connected <- client
	})
	readMessage := func(conn *websocket.Conn) map[string]any {
		t.Helper()
		_, data, err := conn.Read(ctx)
		if err != nil {
			t.Fatal(err)
		}
		var message map[string]any
		if err := json.Unmarshal(data, &message); err != nil {
			t.Fatal(err)
		}
		return message
	}
	readHandshake := func(conn *websocket.Conn) string {
		t.Helper()
		push := readMessage(conn)
		if push["type"] != "push_config" {
			t.Fatalf("handshake push_config = %#v", push)
		}
		inventory, ok := push["inventory"].(map[string]any)
		if !ok {
			t.Fatalf("handshake inventory = %#v", push)
		}
		state, _ := inventory["state"].(string)
		if state != "ready" && state != "error" {
			t.Fatalf("handshake inventory state = %#v", inventory)
		}
		if agents := readMessage(conn); agents["type"] != "agents" {
			t.Fatalf("handshake agents = %#v", agents)
		}
		if workspaces := readMessage(conn); workspaces["type"] != "workspaces" {
			t.Fatalf("handshake workspaces = %#v", workspaces)
		}
		if activity := readMessage(conn); activity["type"] != "activity_history" {
			t.Fatalf("handshake activity = %#v", activity)
		}
		status := readMessage(conn)
		if status["type"] != "inventory_status" || status["state"] != state {
			t.Fatalf("handshake status = %#v, want %q", status, state)
		}
		return state
	}
	conn := dialHub(t, server)
	client := <-connected
	if got := readHandshake(conn); got != "ready" {
		t.Fatalf("initial handshake state = %q", got)
	}

	server.state.MarkInventoryFailure(errors.New("topology churn"))
	if err := server.publishCurrentInventory(ctx); err != nil {
		t.Fatal(err)
	}
	degraded := readMessage(conn)
	if degraded["type"] != "inventory_status" || degraded["state"] != "error" {
		t.Fatalf("degraded publication = %#v", degraded)
	}

	// Hold the real Hub registration barrier while the production publisher,
	// immediate refresh, and a new connection all wait behind it. The state is
	// restored before release, so any operation that captured a stale payload
	// outside the barrier would be observable as an error after a ready batch.
	barrierEntered := make(chan struct{})
	releaseBarrier := make(chan struct{})
	batchDone := make(chan error, 1)
	go func() {
		batchDone <- server.hub.BroadcastBatchPrepared(func() ([]any, func(), error) {
			close(barrierEntered)
			<-releaseBarrier
			return nil, func() {}, nil
		})
	}()
	select {
	case <-barrierEntered:
	case <-ctx.Done():
		t.Fatal("inventory barrier did not start")
	}

	server.state.CommitInventory([]*coordinator.AgentState{agent}, server.state.RevisionCounter())
	published := make(chan error, 1)
	go func() { published <- server.publishCurrentInventory(ctx) }()
	refreshDone := make(chan struct{})
	go func() {
		server.requestAgentRefresh(client)
		close(refreshDone)
	}()
	httpServer := httptest.NewServer(http.HandlerFunc(server.hub.HandleWebSocket))
	defer httpServer.Close()
	secondDial := make(chan *websocket.Conn, 1)
	go func() {
		conn, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(httpServer.URL, "http"), nil)
		if err != nil {
			secondDial <- nil
			return
		}
		secondDial <- conn
	}()
	waitForBlocked := func(name string, done func() bool) {
		t.Helper()
		if done() {
			t.Fatalf("%s overtook the registration barrier", name)
		}
		time.Sleep(20 * time.Millisecond)
		if done() {
			t.Fatalf("%s overtook the registration barrier", name)
		}
	}
	waitForBlocked("inventory publication", func() bool {
		select {
		case <-published:
			return true
		default:
			return false
		}
	})
	waitForBlocked("immediate refresh", func() bool {
		select {
		case <-refreshDone:
			return true
		default:
			return false
		}
	})
	select {
	case <-connected:
		t.Fatal("connection registration overtook the registration barrier")
	case <-time.After(20 * time.Millisecond):
	}

	close(releaseBarrier)
	if err := <-batchDone; err != nil {
		t.Fatal(err)
	}
	if err := <-published; err != nil {
		t.Fatal(err)
	}
	<-refreshDone
	second := <-secondDial
	if second == nil {
		t.Fatal("second connection failed")
	}
	<-connected
	secondState := readHandshake(second)
	if secondState != "error" && secondState != "ready" {
		t.Fatalf("barrier reconnect state = %q", secondState)
	}

	readInventoryBatch := func(wantState string) {
		t.Helper()
		status := readMessage(conn)
		if status["type"] != "inventory_status" || status["state"] != wantState {
			t.Fatalf("barrier refresh status = %#v, want %q", status, wantState)
		}
		agents := readMessage(conn)
		if agents["type"] != "agents" {
			t.Fatalf("barrier refresh agents = %#v", agents)
		}
		rows, ok := agents["agents"].([]any)
		if !ok || len(rows) != 1 || rows[0].(map[string]any)["pane_id"] != agent.PaneID {
			t.Fatalf("barrier refresh agent tuple = %#v", agents)
		}
		workspaces := readMessage(conn)
		if workspaces["type"] != "workspaces" {
			t.Fatalf("barrier refresh workspaces = %#v", workspaces)
		}
		workspaceRows, ok := workspaces["workspaces"].([]any)
		if !ok || len(workspaceRows) != 1 || workspaceRows[0].(map[string]any)["workspace_id"] != workspace.ID {
			t.Fatalf("barrier refresh workspace tuple = %#v", workspaces)
		}
	}
	// The request may acquire the barrier first (error, then ready) or the
	// recovery publication may acquire it first (ready, then ready). In either
	// order there must be no error batch after the first ready batch.
	firstStatus := readMessage(conn)
	if firstStatus["type"] != "inventory_status" {
		t.Fatalf("first barrier status = %#v", firstStatus)
	}
	firstState, _ := firstStatus["state"].(string)
	if firstState != "error" && firstState != "ready" {
		t.Fatalf("first barrier state = %#v", firstStatus)
	}
	firstAgents := readMessage(conn)
	firstWorkspaces := readMessage(conn)
	if firstAgents["type"] != "agents" || firstWorkspaces["type"] != "workspaces" {
		t.Fatalf("first barrier tuple = %#v %#v", firstAgents, firstWorkspaces)
	}
	readInventoryBatch("ready")
	if got := server.committedInventoryStatus()["state"]; got != "ready" {
		t.Fatalf("barrier committed state = %v", got)
	}
	second.CloseNow()
	conn.CloseNow()
}

func TestCommittedInventoryPublicationRepairsZeroListenerRefreshAndReconnect(t *testing.T) {
	server := testServer()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	defer func() {
		if server.conversationB != nil {
			_ = server.conversationB.Close()
		}
		_ = server.hub.Shutdown(ctx)
	}()
	server.setInventoryPublisher(ctx)
	workspace := herdr.Workspace{ID: "workspace-1", Label: "Project"}
	agent := &coordinator.AgentState{PaneID: "pane-1", Agent: "codex", Status: "idle", Project: "Project"}
	server.state.CommitWorkspaces([]herdr.Workspace{workspace})
	server.state.CommitInventory([]*coordinator.AgentState{agent}, server.state.RevisionCounter())
	// This is the same signal path installed by Server.Run, with no listeners
	// attached. The committed cache must still become authoritative.
	publishInventoryForTest(t, server)
	server.state.MarkInventoryFailure(errors.New("topology churn"))
	publishInventoryForTest(t, server)
	server.state.CommitTopology([]*coordinator.AgentState{agent}, []herdr.Workspace{workspace}, server.state.RevisionCounter())
	publishInventoryForTest(t, server)
	committed := server.committedInventorySnapshot()
	if committed.status["state"] != "ready" || len(committed.agents) != 1 || committed.agents[0].PaneID != agent.PaneID || len(committed.workspaces) != 1 || committed.workspaces[0].ID != workspace.ID {
		t.Fatalf("zero-listener production recovery = %#v", committed)
	}

	connected := make(chan *transport.ClientConn, 2)
	server.hub.SetOnConnect(func(client *transport.ClientConn) {
		server.sendConnectionSnapshot(client)
		connected <- client
	})
	readMessage := func(conn *websocket.Conn) map[string]any {
		_, data, readErr := conn.Read(ctx)
		if readErr != nil {
			t.Fatal(readErr)
		}
		var message map[string]any
		if err := json.Unmarshal(data, &message); err != nil {
			t.Fatal(err)
		}
		return message
	}
	assertReady := func(message map[string]any) {
		t.Helper()
		if message["state"] != "ready" {
			t.Fatalf("not-ready inventory message = %#v", message)
		}
	}
	assertTopology := func(message map[string]any) {
		t.Helper()
		if message["type"] == "agents" {
			agents, ok := message["agents"].([]any)
			if !ok || len(agents) != 1 {
				t.Fatalf("agents payload = %#v", message)
			}
			row, ok := agents[0].(map[string]any)
			if !ok || row["pane_id"] != agent.PaneID {
				t.Fatalf("agents payload lost committed pane = %#v", message)
			}
		}
		if message["type"] == "workspaces" {
			workspaces, ok := message["workspaces"].([]any)
			if !ok || len(workspaces) != 1 {
				t.Fatalf("workspaces payload = %#v", message)
			}
			row, ok := workspaces[0].(map[string]any)
			if !ok || row["workspace_id"] != workspace.ID {
				t.Fatalf("workspaces payload lost committed workspace = %#v", message)
			}
		}
	}
	conn := dialHub(t, server)
	client := <-connected
	pushConfig := readMessage(conn)
	if pushConfig["type"] != "push_config" {
		t.Fatalf("initial handshake first message = %#v", pushConfig)
	}
	initialInventory, _ := pushConfig["inventory"].(map[string]any)
	assertReady(initialInventory)
	initialAgents := readMessage(conn)
	initialWorkspaces := readMessage(conn)
	if initialAgents["type"] != "agents" || initialWorkspaces["type"] != "workspaces" {
		t.Fatalf("initial handshake topology = %#v %#v", initialAgents, initialWorkspaces)
	}
	assertTopology(initialAgents)
	assertTopology(initialWorkspaces)
	_ = readMessage(conn) // activity_history
	assertReady(readMessage(conn))

	server.state.MarkInventoryFailure(errors.New("server_not_running"))
	publishInventoryForTest(t, server)
	degraded := readMessage(conn)
	if degraded["type"] != "inventory_status" || degraded["state"] != "error" {
		t.Fatalf("connected degraded publication = %#v", degraded)
	}
	server.state.CommitTopology([]*coordinator.AgentState{agent}, []herdr.Workspace{workspace}, server.state.RevisionCounter())
	publishInventoryForTest(t, server)
	for index, expectedType := range []string{"inventory_status", "agents", "workspaces"} {
		message := readMessage(conn)
		if message["type"] != expectedType {
			t.Fatalf("event recovery message %d = %#v, want %s", index, message, expectedType)
		}
		if expectedType == "inventory_status" {
			assertReady(message)
		} else {
			assertTopology(message)
		}
	}
	server.requestAgentRefresh(client)
	for index, expectedType := range []string{"inventory_status", "agents", "workspaces"} {
		message := readMessage(conn)
		if message["type"] != expectedType {
			t.Fatalf("immediate refresh message %d = %#v, want %s", index, message, expectedType)
		}
		if expectedType == "inventory_status" {
			assertReady(message)
		} else {
			assertTopology(message)
		}
	}
	// The deferred request is drained by an unchanged production publication;
	// it must not depend on a topology diff.
	publishInventoryForTest(t, server)
	for index, expectedType := range []string{"inventory_status", "agents", "workspaces"} {
		message := readMessage(conn)
		if message["type"] != expectedType {
			t.Fatalf("deferred refresh message %d = %#v, want %s", index, message, expectedType)
		}
		if expectedType == "inventory_status" {
			assertReady(message)
		} else {
			assertTopology(message)
		}
	}
	conn.CloseNow()
	reconnected := dialHub(t, server)
	<-connected
	for index, expectedType := range []string{"push_config", "agents", "workspaces", "activity_history", "inventory_status"} {
		message := readMessage(reconnected)
		if message["type"] != expectedType {
			t.Fatalf("reconnect message %d = %#v, want %s", index, message, expectedType)
		}
		if expectedType == "agents" || expectedType == "workspaces" {
			assertTopology(message)
		} else if expectedType == "push_config" {
			inventory, _ := message["inventory"].(map[string]any)
			assertReady(inventory)
		} else if expectedType == "inventory_status" {
			assertReady(message)
		}
	}
	reconnected.CloseNow()
}

// Cancelling the server drops in-flight socket connections, so the fixture sees
// ordinary teardown errors that say nothing about the behaviour under test.
func fixtureConnectionTornDown(err error) bool {
	return errors.Is(err, io.EOF) || errors.Is(err, net.ErrClosed) ||
		errors.Is(err, syscall.EPIPE) || errors.Is(err, syscall.ECONNRESET)
}

func TestProductionEventInventoryRecoveryDrainsRefreshAcrossReconnect(t *testing.T) {
	socketPath := filepath.Join(testSocketDir(t), "s")
	listener, err := net.Listen("unix", socketPath)
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	workspace := map[string]any{"workspace_id": "workspace-1", "label": "Project"}
	pane := map[string]any{
		"pane_id": "pane-1", "terminal_id": "terminal-1", "workspace_id": "workspace-1",
		"tab_id": "tab-1", "agent": "codex", "agent_status": "idle", "cwd": "/work/project",
	}
	agent := map[string]any{
		"pane_id": "pane-1", "terminal_id": "terminal-1", "workspace_id": "workspace-1",
		"tab_id": "tab-1", "agent": "codex", "agent_status": "idle", "cwd": "/work/project",
		"agent_session": map[string]any{"value": "session-1", "kind": "id"},
	}
	snapshot := map[string]any{
		"workspaces": []any{workspace},
		"panes":      []any{pane},
		"agents":     []any{agent},
	}
	pollReady := make(chan struct{})
	pollFailed := make(chan struct{})
	var inventoryCalls atomic.Int32
	var failNextInventory atomic.Bool
	serverDone := make(chan error, 1)
	go func() {
		var serveErr error
		defer func() { serverDone <- serveErr }()
		for {
			conn, acceptErr := listener.Accept()
			if acceptErr != nil {
				if errors.Is(acceptErr, net.ErrClosed) {
					return
				}
				serveErr = acceptErr
				return
			}
			func() {
				defer conn.Close()
				var request struct {
					ID     string `json:"id"`
					Method string `json:"method"`
				}
				if decodeErr := json.NewDecoder(bufio.NewReader(conn)).Decode(&request); decodeErr != nil {
					serveErr = decodeErr
					return
				}
				encoder := json.NewEncoder(conn)
				switch request.Method {
				case "agent.list":
					inventoryCalls.Add(1)
					if failNextInventory.CompareAndSwap(true, false) {
						close(pollFailed)
						serveErr = encoder.Encode(map[string]any{
							"id":    request.ID,
							"error": map[string]any{"code": "server_not_running", "message": "Herdr stopped"},
						})
						return
					}
					serveErr = encoder.Encode(map[string]any{
						"id": request.ID,
						"result": map[string]any{
							"type": "agent_list", "agents": []any{pane},
						},
					})
				case "workspace.list":
					serveErr = encoder.Encode(map[string]any{
						"id": request.ID,
						"result": map[string]any{
							"type": "workspace_list", "workspaces": []any{workspace},
						},
					})
					if inventoryCalls.Load() == 1 {
						close(pollReady)
					}
				case "tab.list", "pane.list":
					serveErr = encoder.Encode(map[string]any{
						"id":    request.ID,
						"error": map[string]any{"code": "unsupported", "message": "fixture omits topology detail"},
					})
				case "events.subscribe":
					serveErr = encoder.Encode(map[string]any{
						"id":     request.ID,
						"result": map[string]any{"type": "subscription_started"},
					})
				case "session.snapshot":
					serveErr = encoder.Encode(map[string]any{
						"id":     request.ID,
						"result": map[string]any{"type": "session_snapshot", "snapshot": snapshot},
					})
				default:
					serveErr = fmt.Errorf("unexpected Herdr event method %q", request.Method)
				}
			}()
			if serveErr != nil {
				return
			}
		}
	}()

	cfg := &config.Config{
		Host: "127.0.0.1", Port: 8375, SocketPath: socketPath,
		PollInterval: 15, CacheDir: t.TempDir(), ConfigHome: t.TempDir(), RuntimeDir: t.TempDir(),
	}
	server := New(cfg, "0.9.0", "abc123", slog.New(slog.NewTextHandler(io.Discard, nil)))
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
	defer cancel()
	defer func() {
		if server.conversationB != nil {
			_ = server.conversationB.Close()
		}
		_ = server.hub.Shutdown(ctx)
	}()
	server.setInventoryPublisher(ctx)
	pollDone := make(chan struct{})
	go func() {
		server.poller.Run(ctx)
		close(pollDone)
	}()
	select {
	case <-pollReady:
	case <-ctx.Done():
		t.Fatal("production poll did not commit its initial inventory")
	}
	// The fake closes pollReady while workspace.list is still returning. Wait
	// for the installed publisher to finish the whole first poll before adding
	// the enrichment barrier, otherwise that barrier could pause the initial
	// poll instead of the event recovery operation.
	waitCommittedState := func(want string) {
		t.Helper()
		for {
			if server.committedInventoryStatus()["state"] == want {
				return
			}
			select {
			case <-ctx.Done():
				t.Fatalf("production committed inventory did not reach %q", want)
			default:
				time.Sleep(time.Millisecond)
			}
		}
	}
	waitCommittedState("ready")

	enteredEnrichment := make(chan struct{})
	releaseEnrichment := make(chan struct{})
	var firstEnrichment atomic.Bool
	server.poller.SetEnrich(func(context.Context, []*coordinator.AgentState) {
		if !firstEnrichment.CompareAndSwap(false, true) {
			return
		}
		close(enteredEnrichment)
		<-releaseEnrichment
	})
	var reconnectWaits atomic.Int32
	server.poller.SetEventReconnectWait(func(context.Context) bool {
		return reconnectWaits.Add(1) == 1
	})

	connected := make(chan *transport.ClientConn, 2)
	server.hub.SetOnConnect(func(client *transport.ClientConn) {
		server.sendConnectionSnapshot(client)
		connected <- client
	})
	readMessage := func(conn *websocket.Conn) map[string]any {
		_, data, readErr := conn.Read(ctx)
		if readErr != nil {
			t.Fatal(readErr)
		}
		var message map[string]any
		if unmarshalErr := json.Unmarshal(data, &message); unmarshalErr != nil {
			t.Fatal(unmarshalErr)
		}
		return message
	}
	assertTuple := func(message map[string]any, wantState string) {
		t.Helper()
		switch message["type"] {
		case "inventory_status":
			if message["state"] != wantState {
				t.Fatalf("inventory status = %#v, want %q", message, wantState)
			}
		case "agents":
			rows, ok := message["agents"].([]any)
			if !ok || len(rows) != 1 {
				t.Fatalf("agents tuple = %#v", message)
			}
			row, ok := rows[0].(map[string]any)
			if !ok || row["pane_id"] != "pane-1" {
				t.Fatalf("agents tuple lost pane = %#v", message)
			}
		case "workspaces":
			rows, ok := message["workspaces"].([]any)
			if !ok || len(rows) != 1 {
				t.Fatalf("workspaces tuple = %#v", message)
			}
			row, ok := rows[0].(map[string]any)
			if !ok || row["workspace_id"] != "workspace-1" {
				t.Fatalf("workspaces tuple lost workspace = %#v", message)
			}
		default:
			t.Fatalf("unexpected inventory tuple message = %#v", message)
		}
	}

	eventDone := make(chan struct{})
	go func() {
		server.poller.RunEvents(ctx)
		close(eventDone)
	}()
	select {
	case <-enteredEnrichment:
	case <-ctx.Done():
		t.Fatal("production event bootstrap did not reach enrichment")
	}
	// The event operation has started and is paused before its commit. A real
	// poll failure now races that in-flight event recovery through the installed
	// production publisher; the later ready commit must not be lost.
	failNextInventory.Store(true)
	server.poller.Wake()
	select {
	case <-pollFailed:
	case <-ctx.Done():
		t.Fatal("production poll did not publish its failure")
	}
	waitCommittedState("error")
	conn := dialHub(t, server)
	client := <-connected
	for index := 0; index < 5; index++ {
		message := readMessage(conn)
		if index == 0 {
			inventory, ok := message["inventory"].(map[string]any)
			if !ok || inventory["state"] != "error" {
				t.Fatalf("stale handshake inventory = %#v", message)
			}
			continue
		}
		if index == 1 || index == 2 || index == 4 {
			if index == 4 && message["type"] != "inventory_status" {
				t.Fatalf("handshake status message = %#v", message)
			}
			if index == 1 && message["type"] != "agents" || index == 2 && message["type"] != "workspaces" {
				t.Fatalf("handshake topology message = %#v", message)
			}
			if index == 1 || index == 2 || index == 4 {
				assertTuple(message, "error")
			}
		}
	}
	for server.hub.ClientCount() == 0 {
		select {
		case <-ctx.Done():
			t.Fatal("websocket client was not registered")
		default:
			time.Sleep(time.Millisecond)
		}
	}
	server.requestAgentRefresh(client)
	for index, expected := range []string{"inventory_status", "agents", "workspaces"} {
		message := readMessage(conn)
		if message["type"] != expected {
			t.Fatalf("immediate refresh message %d = %#v, want %s", index, message, expected)
		}
		assertTuple(message, "error")
	}
	close(releaseEnrichment)
	for batch := 0; batch < 2; batch++ {
		for index, expected := range []string{"inventory_status", "agents", "workspaces"} {
			message := readMessage(conn)
			if message["type"] != expected {
				t.Fatalf("event publication batch %d message %d = %#v, want %s", batch, index, message, expected)
			}
			assertTuple(message, "ready")
		}
	}

	conn.CloseNow()
	select {
	case <-eventDone:
	case <-ctx.Done():
		t.Fatal("production event loop did not finish its reconnect schedule")
	}
	if got := reconnectWaits.Load(); got != 2 {
		t.Fatalf("event reconnect waits = %d, want 2", got)
	}
	latest := dialHub(t, server)
	<-connected
	for index := 0; index < 5; index++ {
		message := readMessage(latest)
		if index == 0 {
			inventory, ok := message["inventory"].(map[string]any)
			if !ok || inventory["state"] != "ready" {
				t.Fatalf("reconnect handshake inventory = %#v", message)
			}
		} else if index == 1 || index == 2 || index == 4 {
			if index == 1 && message["type"] != "agents" || index == 2 && message["type"] != "workspaces" || index == 4 && message["type"] != "inventory_status" {
				t.Fatalf("reconnect tuple message = %#v", message)
			}
			assertTuple(message, "ready")
		}
	}
	latest.CloseNow()
	if err := listener.Close(); err != nil {
		t.Fatal(err)
	}
	if serveErr := <-serverDone; serveErr != nil && !fixtureConnectionTornDown(serveErr) {
		t.Fatal(serveErr)
	}
	cancel()
	select {
	case <-pollDone:
	case <-time.After(time.Second):
		t.Fatal("production poller did not stop")
	}
}

func TestProductionPollInventoryRecoveryCommitsWithoutListeners(t *testing.T) {
	socketPath := filepath.Join(testSocketDir(t), "herdr.sock")
	listener, err := net.Listen("unix", socketPath)
	if err != nil {
		t.Fatal(err)
	}
	workspace := map[string]any{"workspace_id": "workspace-1", "label": "Project"}
	pane := map[string]any{
		"pane_id": "pane-1", "terminal_id": "terminal-1", "workspace_id": "workspace-1",
		"tab_id": "tab-1", "agent": "codex", "agent_status": "idle", "cwd": "/work/project",
	}
	firstReady := make(chan struct{})
	degraded := make(chan struct{})
	degradedAgain := make(chan struct{})
	recovered := make(chan struct{})
	unchanged := make(chan struct{})
	var inventoryCalls atomic.Int32
	serverDone := make(chan error, 1)
	go func() {
		var serveErr error
		defer func() { serverDone <- serveErr }()
		for {
			conn, acceptErr := listener.Accept()
			if acceptErr != nil {
				if errors.Is(acceptErr, net.ErrClosed) {
					return
				}
				serveErr = acceptErr
				return
			}
			func() {
				defer conn.Close()
				var request struct {
					ID     string `json:"id"`
					Method string `json:"method"`
				}
				if decodeErr := json.NewDecoder(bufio.NewReader(conn)).Decode(&request); decodeErr != nil {
					serveErr = decodeErr
					return
				}
				encoder := json.NewEncoder(conn)
				switch request.Method {
				case "agent.list":
					call := inventoryCalls.Add(1)
					if call == 2 || call == 3 {
						if call == 2 {
							close(degraded)
						} else {
							close(degradedAgain)
						}
						serveErr = encoder.Encode(map[string]any{
							"id":    request.ID,
							"error": map[string]any{"code": "server_not_running", "message": "Herdr stopped"},
						})
						return
					}
					serveErr = encoder.Encode(map[string]any{
						"id":     request.ID,
						"result": map[string]any{"type": "agent_list", "agents": []any{pane}},
					})
				case "workspace.list":
					serveErr = encoder.Encode(map[string]any{
						"id":     request.ID,
						"result": map[string]any{"type": "workspace_list", "workspaces": []any{workspace}},
					})
					switch inventoryCalls.Load() {
					case 1:
						close(firstReady)
					case 4:
						close(recovered)
					case 5:
						close(unchanged)
					}
				case "tab.list", "pane.list":
					serveErr = encoder.Encode(map[string]any{
						"id":    request.ID,
						"error": map[string]any{"code": "unsupported", "message": "fixture omits topology detail"},
					})
				default:
					serveErr = fmt.Errorf("unexpected Herdr poll method %q", request.Method)
				}
			}()
			if serveErr != nil {
				return
			}
		}
	}()

	cfg := &config.Config{
		Host: "127.0.0.1", Port: 8375, SocketPath: socketPath,
		PollInterval: 15, CacheDir: t.TempDir(), ConfigHome: t.TempDir(), RuntimeDir: t.TempDir(),
	}
	server := New(cfg, "0.9.0", "abc123", slog.New(slog.NewTextHandler(io.Discard, nil)))
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
	defer cancel()
	defer func() {
		if server.conversationB != nil {
			_ = server.conversationB.Close()
		}
		_ = server.hub.Shutdown(ctx)
	}()
	server.setInventoryPublisher(ctx)
	pollDone := make(chan struct{})
	go func() {
		server.poller.Run(ctx)
		close(pollDone)
	}()
	select {
	case <-firstReady:
	case <-ctx.Done():
		t.Fatal("production poll did not reach its initial successful inventory")
	}
	server.poller.Wake()
	waitForCommittedState := func(want string) {
		t.Helper()
		ticker := time.NewTicker(time.Millisecond)
		defer ticker.Stop()
		for {
			if server.committedInventoryStatus()["state"] == want {
				return
			}
			select {
			case <-ticker.C:
			case <-ctx.Done():
				t.Fatalf("committed inventory did not reach %q", want)
			}
		}
	}
	select {
	case <-degraded:
	case <-ctx.Done():
		t.Fatal("production poll did not reach its failure")
	}
	waitForCommittedState("error")
	server.poller.Wake()
	select {
	case <-degradedAgain:
	case <-ctx.Done():
		t.Fatal("production poll did not reach its repeated failure")
	}
	waitForCommittedState("error")
	server.poller.Wake()
	select {
	case <-recovered:
	case <-ctx.Done():
		t.Fatal("production poll did not reach its recovery")
	}
	waitForCommittedState("ready")
	committed := server.committedInventorySnapshot()
	if committed.status["state"] != "ready" || len(committed.agents) != 1 || len(committed.workspaces) != 1 {
		t.Fatalf("zero-listener recovered snapshot = %#v, want ready topology", committed)
	}

	connected := make(chan *transport.ClientConn, 1)
	server.hub.SetOnConnect(func(client *transport.ClientConn) {
		server.sendConnectionSnapshot(client)
		connected <- client
	})
	conn := dialHub(t, server)
	client := <-connected
	for index := 0; index < 5; index++ {
		_, data, readErr := conn.Read(ctx)
		if readErr != nil {
			t.Fatal(readErr)
		}
		var message map[string]any
		if err := json.Unmarshal(data, &message); err != nil {
			t.Fatal(err)
		}
		if index == 0 {
			inventory, ok := message["inventory"].(map[string]any)
			if !ok || inventory["state"] != "ready" {
				t.Fatalf("zero-listener handshake inventory = %#v", message)
			}
		}
		if index == 4 && (message["type"] != "inventory_status" || message["state"] != "ready") {
			t.Fatalf("zero-listener handshake status = %#v", message)
		}
	}
	conn.CloseNow()
	for server.hub.ClientCount() != 0 {
		select {
		case <-ctx.Done():
			t.Fatal("disconnected refresh client remained registered")
		default:
			time.Sleep(time.Millisecond)
		}
	}
	server.requestAgentRefresh(client)
	select {
	case <-unchanged:
	case <-ctx.Done():
		t.Fatal("unchanged production poll did not complete for disconnected refresh")
	}
	for {
		server.refreshMu.Lock()
		pending := len(server.refreshClients)
		server.refreshMu.Unlock()
		if pending == 0 {
			break
		}
		select {
		case <-ctx.Done():
			t.Fatal("disconnected refresh remained pending")
		default:
			time.Sleep(time.Millisecond)
		}
	}
	cancel()
	select {
	case <-pollDone:
	case <-time.After(time.Second):
		t.Fatal("production poller did not stop")
	}
	if err := listener.Close(); err != nil {
		t.Fatal(err)
	}
	if serveErr := <-serverDone; serveErr != nil && !fixtureConnectionTornDown(serveErr) {
		t.Fatal(serveErr)
	}
}

func TestCommittedInventorySnapshotDeepCopiesInteractionSummaries(t *testing.T) {
	server := testServer()
	interaction := &question.Interaction{
		ID: "interaction-1",
		Options: []question.Option{{
			Index:   1,
			Label:   "Keep",
			Summary: []question.SummaryEntry{{Question: "Deploy?", Answer: "No"}},
		}},
	}
	server.stateViewMu.Lock()
	server.agentView = []*coordinator.AgentState{{PaneID: "pane-1", Interaction: interaction}}
	server.stateViewMu.Unlock()

	snapshot := server.committedInventorySnapshot()
	snapshot.agents[0].Interaction.Options[0].Summary[0].Answer = "Mutated"
	fresh := server.committedInventorySnapshot()
	if got := fresh.agents[0].Interaction.Options[0].Summary[0].Answer; got != "No" {
		t.Fatalf("committed interaction summary answer = %q, want No", got)
	}
}

func TestStatusClockIncrementalCompletionAndAcknowledgement(t *testing.T) {
	e := newFrameEnv(t, &coordinator.AgentState{PaneID: "pane-1", TerminalID: "terminal", Status: "working"})
	s := e.server
	const completedAt = int64(1_753_178_400_000)
	s.state.CommitEvent("pane-1", "idle", completedAt)
	raw, _ := s.state.Agent("pane-1")
	s.commitAgent(raw, "agent_update")
	frame := readFrame(t, e.conn)
	if frame["type"] != "agent_update" || frame["status"] != "done" || frame["status_since_at"] != float64(completedAt) {
		t.Fatal(frame)
	}
	if a := s.committedAgents()[0]; a.Status != "done" || a.StatusSinceAt != completedAt {
		t.Fatalf("committed result = %+v", a)
	}

	ack, _ := s.state.AcknowledgePane("pane-1")
	if ack == nil {
		t.Fatal("result did not change on acknowledgement")
	}
	s.broadcastCommitted(coordinator.AgentUpdate{Agent: ack})
	frame = readFrame(t, e.conn)
	if frame["type"] != "agent_update" || frame["status"] != "idle" {
		t.Fatal(frame)
	}
	if _, present := frame["status_since_at"]; present {
		t.Fatalf("acknowledgement kept a clock: %v", frame)
	}
	publishInventoryForTest(t, s)
	for _, frame := range e.settle(t) {
		if frame["type"] == "agents" {
			var agents []*coordinator.AgentState
			decodeInto(t, frame["agents"], &agents)
			if agents[0].Status != "idle" || agents[0].StatusSinceAt != 0 {
				t.Fatal(frame)
			}
		}
	}
	if a := s.committedAgents()[0]; a.Status != "idle" || a.StatusSinceAt != 0 {
		t.Fatalf("committed acknowledgement = %+v", a)
	}
}

func TestStatusClockOnlyPublicationSkipsAgentSideEffects(t *testing.T) {
	pane := &coordinator.AgentState{PaneID: "pane-1", Agent: "codex", Status: "idle"}
	capture := &coordinator.AgentState{PaneID: "pane-2", Agent: "claude", Status: "working"}
	e := newFrameEnv(t, pane, capture)
	s := e.server
	const startedAt = int64(1_753_178_400_000)
	s.state.CommitEvent("pane-1", "working", startedAt)
	publishInventoryForTest(t, s)
	e.settle(t)

	var err error
	s.pushM, err = push.NewManager(t.TempDir(), s.logger)
	if err != nil {
		t.Fatal(err)
	}
	s.historyTasks = newLifecycleTasks(t.Context())
	defer s.historyTasks.Stop()
	s.stateViewMu.Lock()
	s.agentView[0].StatusSinceAt = 0
	s.stateViewMu.Unlock()

	publishInventoryForTest(t, s)
	frame := readFrame(t, e.conn)
	var agents []*coordinator.AgentState
	decodeInto(t, frame["agents"], &agents)
	if frame["type"] != "agents" || len(agents) != 2 || agents[0].StatusSinceAt != startedAt || agents[1].StatusSinceAt != 0 {
		t.Fatal(frame)
	}
	if got := s.committedAgents()[0].StatusSinceAt; got != startedAt {
		t.Fatalf("committed clock = %d, want %d", got, startedAt)
	}
	s.historyCaptureMu.Lock()
	scheduled := len(s.historyLast) > 0
	s.historyCaptureMu.Unlock()
	if reads := e.backend.called("ReadPane"); scheduled || s.pushReconciled || len(reads) > 0 {
		t.Fatalf("time-only publication scheduled history=%t, reconciled push=%t, reads=%v", scheduled, s.pushReconciled, reads)
	}
	publishInventoryForTest(t, s)
	if frames := e.settle(t); len(frames) != 0 {
		t.Fatalf("unchanged time sent extra frames: %v", frames)
	}

	pane.Status = "working"
	pane.Name = "renamed"
	s.state.CommitInventory([]*coordinator.AgentState{pane, capture}, s.state.RevisionCounter())
	publishInventoryForTest(t, s)
	s.historyCaptureMu.Lock()
	scheduled = !s.historyLast[capture.PaneID].IsZero()
	s.historyCaptureMu.Unlock()
	if !scheduled || !s.pushReconciled {
		t.Fatalf("ordinary inventory change scheduled history=%t, reconciled push=%t", scheduled, s.pushReconciled)
	}
}

func TestCommittedStateViewTracksSnapshotsAndDeltas(t *testing.T) {
	s := testServer()
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_ = s.hub.Shutdown(ctx)
	})
	s.state.CommitInventory([]*coordinator.AgentState{{
		PaneID: "pane-1", Status: "blocked", Project: "project",
		BlockedEventID: "event-1", AttentionKind: question.AttentionApproval,
		Options: []string{"Approve", "Deny"}, ApprovalFingerprint: "approval-fingerprint-1",
	}}, s.state.RevisionCounter())
	publishInventoryForTest(t, s)
	current, _ := s.state.Agent("pane-1")
	current.Project = "renamed"
	s.commitAgent(current, "agent_update")
	agents := s.committedAgents()
	if len(agents) != 1 || agents[0].Status != "blocked" || agents[0].Project != "renamed" ||
		agents[0].BlockedEventID != "event-1" ||
		agents[0].ApprovalFingerprint != "approval-fingerprint-1" {
		t.Fatalf("committed agents = %+v", agents)
	}
	inventory := s.committedInventoryStatus()
	if inventory["state"] != "ready" || inventory["stale"] != false {
		t.Fatalf("committed inventory = %+v", inventory)
	}
	if _, leaked := inventory["type"]; leaked {
		t.Fatalf("inventory view leaked transport type: %+v", inventory)
	}
}

func TestCommittedStateViewRejectsStalePerPaneUpdates(t *testing.T) {
	s := testServer()
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_ = s.hub.Shutdown(ctx)
	})
	s.state.CommitInventory([]*coordinator.AgentState{{
		PaneID: "pane-1", Status: "idle",
	}}, s.state.RevisionCounter())
	publishInventoryForTest(t, s)
	revision := s.state.Revision("pane-1") + 2
	s.commitAgent(
		&coordinator.AgentState{PaneID: "pane-1", Status: "working", StateRevision: revision},
		"agent_update",
	)
	s.commitAgent(
		&coordinator.AgentState{PaneID: "pane-1", Status: "blocked", BlockedEventID: "stale-event", StateRevision: revision - 1},
		"agent_update",
	)
	publishInventoryForTest(t, s)

	agents := s.committedAgents()
	if len(agents) != 1 || agents[0].Status != "working" ||
		agents[0].StateRevision != revision || agents[0].BlockedEventID != "" {
		t.Fatalf("reconnect snapshot regressed after stale messages: %+v", agents)
	}
}
