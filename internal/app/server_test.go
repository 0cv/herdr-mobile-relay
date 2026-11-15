package app

import (
	"bufio"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/0cv/herdr-mobile-relay/internal/activity"
	"github.com/0cv/herdr-mobile-relay/internal/config"
	"github.com/0cv/herdr-mobile-relay/internal/coordinator"
	"github.com/0cv/herdr-mobile-relay/internal/copyresponse"
	"github.com/0cv/herdr-mobile-relay/internal/deviceauth"
	"github.com/0cv/herdr-mobile-relay/internal/herdr"
	"github.com/0cv/herdr-mobile-relay/internal/panedelta"
	"github.com/0cv/herdr-mobile-relay/internal/protocol"
	"github.com/0cv/herdr-mobile-relay/internal/push"
	"github.com/0cv/herdr-mobile-relay/internal/question"
	"github.com/0cv/herdr-mobile-relay/internal/session"
	"github.com/0cv/herdr-mobile-relay/internal/slashcmd"
	"github.com/0cv/herdr-mobile-relay/internal/speech"
	"github.com/0cv/herdr-mobile-relay/internal/transport"
	"github.com/coder/websocket"
)

func testServer() *Server {
	return testServerWithCacheDir("")
}

func testServerWithCacheDir(cacheDir string) *Server {
	cfg := &config.Config{
		Host:       "127.0.0.1",
		Port:       8375,
		InstanceID: "test-instance",
		CacheDir:   cacheDir,
	}
	return New(cfg, "0.9.0", "abc123", slog.New(slog.NewTextHandler(io.Discard, nil)))
}

func isAuditedWrite(action string) bool {
	metadata, known := protocol.ClassifyAction(action)
	return known && metadata.Audited
}

func isCoordinatorMutation(action string) bool {
	metadata, known := protocol.ClassifyAction(action)
	return known && metadata.Coordinated
}

func (s *Server) committedAgents() []*coordinator.AgentState {
	return s.committedInventorySnapshot().agents
}

func (s *Server) committedInventoryStatus() map[string]any {
	return s.committedInventorySnapshot().status
}

func publishInventoryForTest(t *testing.T, server *Server) {
	t.Helper()
	if err := server.publishCurrentInventory(context.Background()); err != nil {
		t.Fatal(err)
	}
}
func TestResolveAgentSessionName(t *testing.T) {
	home := t.TempDir()
	codexDir := filepath.Join(home, ".codex")
	const codexSessionID = "123e4567-e89b-12d3-a456-426614174001"
	rolloutPath := filepath.Join(codexDir, "sessions", "2026", "08", "12", "rollout-2026-08-12T10-00-00-"+codexSessionID+".jsonl")
	if err := os.MkdirAll(filepath.Dir(rolloutPath), 0o755); err != nil {
		t.Fatal(err)
	}
	row, err := json.Marshal(map[string]any{
		"timestamp": "2026-08-12T10:00:00Z", "type": "response_item",
		"payload": map[string]any{"type": "message", "role": "user",
			"content": []any{map[string]any{"type": "input_text", "text": "build it"}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(rolloutPath, append(row, '\n'), 0o644); err != nil {
		t.Fatal(err)
	}
	index := []byte("{\"id\":\"" + codexSessionID + "\",\"thread_name\":\"current-session\"}\n")
	if err := os.WriteFile(filepath.Join(codexDir, "session_index.jsonl"), index, 0o644); err != nil {
		t.Fatal(err)
	}

	s := testServer()
	s.sessions = session.NewResolverWithReader(home, nil)

	// named carries a canonical session id with both an index record and the
	// rollout the reader would serve, the shape Locate requires post round-3.
	named := &coordinator.AgentState{Agent: "codex", Session: codexSessionID}
	s.resolveAgentSessionName(named)
	if named.SessionName != "current-session" || named.Session != "current-session" ||
		named.SessionID != codexSessionID || !named.ConversationHistoryAvailable {
		t.Fatalf("named session = %#v, want resolved display name and retained history identity", named)
	}

	unnamed := &coordinator.AgentState{Agent: "codex", Session: "missing-session"}
	s.resolveAgentSessionName(unnamed)
	if unnamed.SessionName != "" || unnamed.Session != "missing-session" ||
		unnamed.SessionID != "missing-session" || !unnamed.ConversationHistoryAvailable {
		t.Fatalf("unnamed session = %#v, want preserved history identity", unnamed)
	}
}

// A whitespace-only session value is what herdr reports for a pane with no
// real session yet - not the same as an empty string, but every other
// consumer (Reader.Read, latestConversationResponse, the activity backfill
// path) treats it as one via TrimSpace. Before the fix,
// resolveAgentSessionName did not, so the untrimmed value survived the
// `== ""` guard and reached the resolver's empty-id sole-transcript
// heuristic, which - given a cwd whose project directory holds exactly one
// transcript - invents a title for a session that was never reported. The
// conversation view for the same (agent, cwd) reads its session id through
// Reader.Read, which trims it back to empty and answers "not available", so
// the pane would show a confident title next to an unavailable transcript.
func TestResolveAgentSessionNameTrimsWhitespaceOnlySession(t *testing.T) {
	home := t.TempDir()
	const cwd = "/work/app"
	writeClaudeTranscript(t, filepath.Join(home, ".claude", "projects", "-work-app", invariantSession+".jsonl"), "Sole Transcript Title")

	s := testServer()
	s.sessions = session.NewResolverWithReader(home, nil)

	agent := &coordinator.AgentState{Agent: "claude", Cwd: cwd, Session: "   "}
	s.resolveAgentSessionName(agent)
	if agent.SessionName != "" {
		t.Fatalf("SessionName = %q, want empty for a whitespace-only session", agent.SessionName)
	}
	if agent.SessionID != "" {
		t.Fatalf("SessionID = %q, want empty for a whitespace-only session", agent.SessionID)
	}
	if agent.ConversationHistoryAvailable {
		t.Fatal("ConversationHistoryAvailable = true, want false for a whitespace-only session")
	}
}

func TestForegroundCwdIsNotSerializedInAgentPayload(t *testing.T) {
	agent := &coordinator.AgentState{Agent: "claude", Cwd: "/work/pane", ForegroundCwd: "/work/foreground"}
	data, err := json.Marshal(agent)
	if err != nil {
		t.Fatal(err)
	}
	var payload map[string]any
	if err := json.Unmarshal(data, &payload); err != nil {
		t.Fatal(err)
	}
	if payload["cwd"] != agent.Cwd || strings.Contains(string(data), "foreground_cwd") || strings.Contains(string(data), agent.ForegroundCwd) {
		t.Fatalf("serialized agent payload = %s, want pane cwd only", data)
	}
}

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

func TestHealth(t *testing.T) {
	s := testServer()
	req := httptest.NewRequest("GET", "/health", nil)
	w := httptest.NewRecorder()

	s.handleHealth(w, req)

	if w.Code != 200 {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	if body := w.Body.String(); body != "ok\n" {
		t.Errorf("body = %q, want \"ok\\n\"", body)
	}
	if inst := w.Header().Get("X-Herdr-Relay-Instance"); inst != "test-instance" {
		t.Errorf("instance header = %q, want test-instance", inst)
	}
}

func TestPaneDeltaResponsePreservesTruncation(t *testing.T) {
	delta := paneDeltaResponse(
		map[string]any{
			"type":      "pane_content",
			"content":   "new output",
			"truncated": true,
			"format":    "ansi",
		},
		"content-1",
		nil,
	)
	if delta["truncated"] != true {
		t.Fatalf("delta truncation = %#v, want true", delta["truncated"])
	}
	if _, ok := delta["content"]; ok {
		t.Fatal("delta unexpectedly included full content")
	}
}

func TestPaneWatchUpdateSendsMetadataOnlyDelta(t *testing.T) {
	response := map[string]any{
		"type":           "pane_content",
		"content":        "unchanged\nquestion\n",
		"attention_kind": question.AttentionQuestion,
		"interaction":    map[string]any{"id": "question-1"},
	}
	previous := map[string]any{
		"type":           "pane_content",
		"content":        "unchanged\nquestion\n",
		"attention_kind": question.AttentionUnknown,
	}
	acknowledged := &paneWatchFrame{
		content:            "unchanged\nquestion\n",
		contentFingerprint: "content-1",
		frameFingerprint:   paneFrameFingerprint(previous),
	}
	current := &paneWatchFrame{
		content:            "unchanged\nquestion\n",
		contentFingerprint: "content-1",
		frameFingerprint:   paneFrameFingerprint(response),
	}
	if acknowledged.frameFingerprint == current.frameFingerprint {
		t.Fatal("question metadata did not change the pane frame fingerprint")
	}
	initial := *acknowledged
	initial.frameFingerprint = ""
	if paneWatchUpdate(response, &initial, current) == nil {
		t.Fatal("initial pane watch suppressed current interaction metadata")
	}

	update := paneWatchUpdate(response, acknowledged, current)
	if update["type"] != "pane_delta" ||
		update["base_fingerprint"] != "content-1" {
		t.Fatalf("metadata update = %#v", update)
	}
	if _, ok := update["content"]; ok {
		t.Fatal("metadata delta unexpectedly included full terminal content")
	}
	segments, ok := update["segments"].([]panedelta.Segment)
	if !ok || len(segments) != 1 || segments[0].CopyStart != 0 ||
		segments[0].CopyLines != 3 || segments[0].Text != "" {
		t.Fatalf("metadata delta segments = %#v, want one whole-frame copy", update["segments"])
	}
	applied, appliedOK := panedelta.Apply(acknowledged.content, segments)
	if !appliedOK || applied != current.content {
		t.Fatalf("metadata delta applied = %q, %v; want %q, true", applied, appliedOK, current.content)
	}
	encoded, err := json.Marshal(update)
	if err != nil {
		t.Fatalf("marshal metadata delta: %v", err)
	}
	if strings.Contains(string(encoded), `"segments":null`) {
		t.Fatalf("metadata delta encoded a null segment list: %s", encoded)
	}
}

func TestPaneWatchUpdateSendsResizeSettledDelta(t *testing.T) {
	settlingResponse := map[string]any{
		"type":            "pane_content",
		"content":         "unchanged terminal",
		"resize_settling": true,
	}
	settledResponse := map[string]any{
		"type":    "pane_content",
		"content": "unchanged terminal",
	}
	acknowledged := &paneWatchFrame{
		content:            "unchanged terminal",
		contentFingerprint: "content-1",
		frameFingerprint:   paneFrameFingerprint(settlingResponse),
		resizeSettling:     true,
	}
	current := &paneWatchFrame{
		content:            "unchanged terminal",
		contentFingerprint: "content-1",
		frameFingerprint:   paneFrameFingerprint(settledResponse),
	}
	if acknowledged.frameFingerprint == current.frameFingerprint {
		t.Fatal("resize settling state did not change the pane frame fingerprint")
	}

	if !paneWatchNeedsFrameRead("probe-1", "probe-1", acknowledged, "") {
		t.Fatal("settling pane frame would not be re-read after an unchanged probe")
	}
	if paneWatchNeedsFrameRead("probe-1", "probe-1", current, "") {
		t.Fatal("settled pane frame would be re-read after an unchanged probe")
	}
	update := paneWatchUpdate(settledResponse, acknowledged, current)
	if update["type"] != "pane_delta" ||
		update["base_fingerprint"] != "content-1" {
		t.Fatalf("resize settled update = %#v", update)
	}
	if update["resize_settling"] == true {
		t.Fatalf("resize settled update remained transient: %#v", update)
	}
}

func TestPreparePaneResponsePreservesHistoryForViewportOnlyRead(t *testing.T) {
	s := testServerWithCacheDir(t.TempDir())
	s.state.CommitInventory([]*coordinator.AgentState{{
		PaneID: "pane-1", Agent: "claude", Status: "idle",
	}}, s.state.RevisionCounter())
	baseline := "history 1\nhistory 2\nhistory 3\nhistory 4\nhistory 5\nhistory 6\nhistory 7\nhistory 8"
	s.historyM.Merge("pane-1", baseline)

	response := map[string]any{
		"type": "pane_content", "pane_id": "pane-1",
		"content": "current 1\ncurrent 2", "format": "ansi",
		"truncated": false, "viewport_only": true,
	}
	s.preparePaneResponse(
		map[string]any{"pane_id": "pane-1", "lines": 100, "terminal_columns": 59},
		response,
	)

	if response["content"] != "current 1\ncurrent 2" {
		t.Fatalf("resized content = %q, want clean current viewport", response["content"])
	}
	if response["truncated"] != false {
		t.Fatalf("resized truncation = %#v, want false", response["truncated"])
	}
	if content := s.historyM.Content("pane-1", 100); content != baseline {
		t.Fatalf("history changed during resized read:\n%s", content)
	}
}

func TestCopyBlockedMessage(t *testing.T) {
	tests := []struct {
		name  string
		agent *coordinator.AgentState
		want  string
	}{
		{name: "nil", want: ""},
		{name: "working question", agent: &coordinator.AgentState{
			Status: "working", AttentionKind: question.AttentionQuestion,
		}, want: ""},
		{name: "question", agent: &coordinator.AgentState{
			Status: "blocked", AttentionKind: question.AttentionQuestion,
		}, want: "Agent is waiting for an answer"},
		{name: "approval", agent: &coordinator.AgentState{
			Status: "blocked", AttentionKind: question.AttentionApproval,
		}, want: "Agent is waiting for approval"},
		{name: "unknown blocked", agent: &coordinator.AgentState{
			Status: "blocked", AttentionKind: question.AttentionUnknown,
		}, want: ""},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := copyBlockedMessage(test.agent); got != test.want {
				t.Fatalf("copyBlockedMessage() = %q, want %q", got, test.want)
			}
		})
	}
}

func TestClassifyBlockedTransitionRetriesPartialCodexQuestion(t *testing.T) {
	partial := `
Question 1/3 (3 unanswered)
After a respondent finishes a questionnaire, what should the app do?

› 1. Builder-defined result (Recommended)  Show the configured result.
  2. Response dashboard                    Store responses for review.
  3. Personalized output                   Generate a tailored result.
  4. None of the above                     Optionally, add details in notes (tab).
`
	complete := partial + `
tab to add notes | enter to submit answer | ←/→ to navigate questions | esc to interrupt
`
	calls := 0
	classification, err := classifyBlockedTransition(
		context.Background(),
		"codex",
		func(context.Context) (string, error) {
			calls++
			if calls == 1 {
				return partial, nil
			}
			return complete, nil
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	if calls != 2 ||
		classification.Kind != question.AttentionQuestion ||
		classification.Interaction == nil ||
		classification.Interaction.Question !=
			"After a respondent finishes a questionnaire, what should the app do?" {
		t.Fatalf("classification after %d reads = %+v", calls, classification)
	}
}

func TestCopyResponseError(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want string
	}{
		{name: "busy composer", err: copyresponse.ErrComposerBusy, want: "The agent composer is busy; finish or clear the current prompt first"},
		{name: "open picker", err: copyresponse.ErrPickerOpen, want: "The agent already has a copy menu open; close it and try again"},
		{name: "stale output", err: copyresponse.ErrStaleOutput, want: "The copied response changed before it could be read; try again"},
		{name: "no copy", err: copyresponse.ErrNoCopy, want: "The agent did not confirm a copied response; try again"},
		{name: "timeout", err: context.DeadlineExceeded, want: "Copying the agent response timed out; try again"},
		{name: "unknown", err: errors.New("internal detail"), want: "Could not copy the agent response; try again"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := copyResponseError(test.err); got != test.want {
				t.Fatalf("copyResponseError() = %q, want %q", got, test.want)
			}
		})
	}
}
func openCopyTestClient(t *testing.T, s *Server) *websocket.Conn {
	t.Helper()
	useProductionHandler(t, s)
	conn := dialHub(t, s)
	return conn
}

func sendCopyRequest(t *testing.T, conn *websocket.Conn, requestID, paneID string) map[string]any {
	t.Helper()
	payload, err := json.Marshal(map[string]any{
		"type": "copy_agent_response", "request_id": requestID, "pane_id": paneID, "protocol": protocol.Version,
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := conn.Write(ctx, websocket.MessageText, payload); err != nil {
		t.Fatalf("write copy request: %v", err)
	}
	_, data, err := conn.Read(ctx)
	if err != nil {
		t.Fatalf("read copy result: %v", err)
	}
	var result map[string]any
	if err := json.Unmarshal(data, &result); err != nil {
		t.Fatalf("decode copy result: %v", err)
	}
	return result
}

func TestSpeakTextSynthesizesOverTheWire(t *testing.T) {
	s := testServer()
	synthesized := ""
	spokenLanguage := ""
	availableLanguages := []string{"en", "fr"}
	s.speechStatus = func() speech.Catalog {
		return speech.Catalog{Languages: append([]string(nil), availableLanguages...)}
	}
	s.speechSynth = func(_ context.Context, text, language string) ([]byte, error) {
		synthesized = text
		spokenLanguage = language
		if strings.Contains(text, "fail") {
			return nil, errors.New("engine detail stays server-side")
		}
		return []byte("RIFFfakewav"), nil
	}
	useProductionHandler(t, s)
	conn := dialHub(t, s)
	request := func(text, language string) map[string]any {
		t.Helper()
		payload, err := json.Marshal(map[string]any{
			"type":       "speak_text",
			"protocol":   protocol.Version,
			"request_id": "req-1",
			"text":       text,
			"language":   language,
		})
		if err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		if err := conn.Write(ctx, websocket.MessageText, payload); err != nil {
			t.Fatalf("write speak request: %v", err)
		}
		_, data, err := conn.Read(ctx)
		if err != nil {
			t.Fatalf("read speak result: %v", err)
		}
		var result map[string]any
		if err := json.Unmarshal(data, &result); err != nil {
			t.Fatal(err)
		}
		return result
	}

	result := request("hello phone", "fr")
	data, _ := result["data"].(map[string]any)
	if result["ok"] != true || data["format"] != "wav" ||
		data["audio"] != base64.StdEncoding.EncodeToString([]byte("RIFFfakewav")) {
		t.Fatalf("speak result = %+v, want base64 wav payload", result)
	}
	if synthesized != "hello phone" || spokenLanguage != "fr" {
		t.Fatalf("synthesized %q in %q, want the requested text and language", synthesized, spokenLanguage)
	}

	// Engine details never reach the phone; the toast stays generic.
	failed := request("please fail", "en")
	if failed["ok"] != false || failed["error"] != "Speech synthesis failed on this computer" {
		t.Fatalf("failed result = %+v, want generic synthesis failure", failed)
	}

	// A language this host has no voice for is refused before synthesis.
	unsupported := request("hallo", "de")
	if unsupported["ok"] != false || unsupported["error"] != "This computer has no voice for that language" {
		t.Fatalf("unsupported language result = %+v", unsupported)
	}

	availableLanguages = nil
	missing := request("hello", "en")
	if missing["ok"] != false || missing["error"] != "No speech engine is installed on this computer" {
		t.Fatalf("missing engine result = %+v", missing)
	}
}

func TestCancelSpeechStopsRelaySynthesis(t *testing.T) {
	s := testServer()
	s.speechStatus = func() speech.Catalog {
		return speech.Catalog{Languages: []string{"en"}}
	}
	started := make(chan struct{})
	s.speechSynth = func(ctx context.Context, _, _ string) ([]byte, error) {
		close(started)
		<-ctx.Done()
		return nil, ctx.Err()
	}
	useProductionHandler(t, s)
	conn := dialHub(t, s)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := conn.Write(ctx, websocket.MessageText, []byte(fmt.Sprintf(
		`{"type":"speak_text","protocol":%d,"request_id":"speak-1","speech_request_id":"speech-1","text":"stop this","language":"en"}`, protocol.Version,
	))); err != nil {
		t.Fatal(err)
	}
	select {
	case <-started:
	case <-ctx.Done():
		t.Fatal("speech synthesis did not start")
	}
	if err := conn.Write(ctx, websocket.MessageText, []byte(fmt.Sprintf(
		`{"type":"cancel_speech","protocol":%d,"speech_request_id":"speech-1"}`, protocol.Version,
	))); err != nil {
		t.Fatal(err)
	}
	_, data, err := conn.Read(ctx)
	if err != nil {
		t.Fatalf("read cancelled speech result: %v", err)
	}
	var result map[string]any
	if err := json.Unmarshal(data, &result); err != nil {
		t.Fatal(err)
	}
	if result["request_id"] != "speak-1" || result["ok"] != false {
		t.Fatalf("cancelled speech result = %#v", result)
	}
}

func TestSpeechVoiceManagementOverTheWire(t *testing.T) {
	s := testServer()
	installed := map[string]bool{"en": true}
	s.speechStatus = func() speech.Catalog {
		status := speech.Catalog{CacheDir: "/cache/speech", EngineInstalled: true, ManagementSupported: true}
		for _, language := range speech.Offered {
			engine := "espeak-ng"
			if installed[language] {
				engine = "piper"
				status.Languages = append(status.Languages, language)
			}
			status.Voices = append(status.Voices, speech.VoiceStatus{
				Language:  language,
				Name:      language + "-voice",
				Installed: installed[language],
				Bytes:     63 << 20,
				Engine:    engine,
			})
		}
		return status
	}
	requested := ""
	s.speechInstall = func(_ context.Context, language string) error {
		requested = language
		if language == "zh" {
			return errors.New("engine detail stays server-side")
		}
		installed[language] = true
		return nil
	}
	s.speechRemove = func(language string) error {
		delete(installed, language)
		return nil
	}
	useProductionHandler(t, s)
	conn := dialHub(t, s)
	exchange := func(request map[string]any) []map[string]any {
		t.Helper()
		payload, err := json.Marshal(request)
		if err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		if err := conn.Write(ctx, websocket.MessageText, payload); err != nil {
			t.Fatalf("write %v: %v", request["type"], err)
		}
		var messages []map[string]any
		for {
			_, data, err := conn.Read(ctx)
			if err != nil {
				t.Fatalf("read %v result: %v", request["type"], err)
			}
			var message map[string]any
			if err := json.Unmarshal(data, &message); err != nil {
				t.Fatal(err)
			}
			messages = append(messages, message)
			if message["type"] == "command_result" {
				return messages
			}
		}
	}
	voiceState := func(message map[string]any) map[string]bool {
		t.Helper()
		payload, _ := message["data"].(map[string]any)
		if payload == nil {
			payload = message
		}
		voices, _ := payload["voices"].([]any)
		if len(voices) != len(speech.Offered) {
			t.Fatalf("payload lists %d voices, want %d", len(voices), len(speech.Offered))
		}
		state := map[string]bool{}
		for _, entry := range voices {
			voice, _ := entry.(map[string]any)
			language, _ := voice["language"].(string)
			state[language], _ = voice["installed"].(bool)
		}
		return state
	}

	if metadata, known := protocol.ClassifyAction("speech_voices_list"); !known || metadata.Class != protocol.ActionReadOnly {
		t.Error("listing voices must stay a read-only action")
	}
	for _, action := range []string{"speech_voice_install", "speech_voice_remove"} {
		if metadata, known := protocol.ClassifyAction(action); !known || metadata.Class != protocol.ActionMutating {
			t.Errorf("%s must be a mutating action so readers cannot run it", action)
		}
	}

	listed := exchange(map[string]any{"type": "speech_voices_list", "request_id": "list-1"})
	result := listed[len(listed)-1]
	data, _ := result["data"].(map[string]any)
	if result["ok"] != true || data["cache_dir"] != "/cache/speech" || data["engine_installed"] != true {
		t.Fatalf("list result = %+v", result)
	}
	if state := voiceState(result); !state["en"] || state["fr"] {
		t.Fatalf("listed voices = %+v, want English only", state)
	}

	// Installing answers the caller and tells every phone what the computer
	// can speak now, so a second device is never left with a stale list.
	messages := exchange(map[string]any{"type": "speech_voice_install", "request_id": "install-1", "language": "fr", "protocol": protocol.Version})
	if requested != "fr" {
		t.Fatalf("installed language = %q, want fr", requested)
	}
	broadcast := map[string]any{}
	for _, message := range messages {
		if message["type"] == "speech_voices" {
			broadcast = message
		}
	}
	if len(broadcast) == 0 {
		t.Fatalf("install sent no speech_voices broadcast: %+v", messages)
	}
	if state := voiceState(broadcast); !state["fr"] {
		t.Fatalf("broadcast voices = %+v, want French installed", state)
	}
	if languages, _ := broadcast["languages"].([]any); len(languages) != 2 {
		t.Fatalf("broadcast languages = %+v, want English and French", broadcast["languages"])
	}
	if s.speakableLanguages()[1] != "fr" {
		t.Fatalf("relay speakable languages = %v, want French included", s.speakableLanguages())
	}

	removed := exchange(map[string]any{"type": "speech_voice_remove", "request_id": "remove-1", "language": "fr", "protocol": protocol.Version})
	if state := voiceState(removed[len(removed)-1]); state["fr"] {
		t.Fatalf("remove result = %+v, want French gone", removed[len(removed)-1])
	}

	// A failed download keeps engine details server-side and still reports the
	// unchanged catalog.
	failed := exchange(map[string]any{"type": "speech_voice_install", "request_id": "install-2", "language": "zh", "protocol": protocol.Version})
	result = failed[len(failed)-1]
	if result["ok"] != false || result["error"] != "Downloading the Chinese voice failed on this computer" {
		t.Fatalf("failed install result = %+v", result)
	}
	if state := voiceState(result); state["zh"] {
		t.Fatalf("failed install reported Chinese as installed: %+v", result)
	}

	unsupported := exchange(map[string]any{"type": "speech_voice_install", "request_id": "install-3", "language": "ja", "protocol": protocol.Version})
	result = unsupported[len(unsupported)-1]
	if result["ok"] != false || result["error"] != "That language is not one this app reads aloud" {
		t.Fatalf("unsupported language result = %+v", result)
	}
}

func TestCopyAgentResponseValidatesPaneState(t *testing.T) {
	tests := []struct {
		name    string
		paneID  string
		setup   func(*Server)
		wantErr string
	}{
		{name: "missing pane id", wantErr: "Agent is required"},
		{name: "missing pane", paneID: "missing", wantErr: "Agent pane not found"},
		{
			name:   "clipboard unavailable",
			paneID: "pane-1",
			setup: func(s *Server) {
				s.state.CommitInventory([]*coordinator.AgentState{{PaneID: "pane-1", Agent: "claude", Status: "idle"}}, s.state.RevisionCounter())
				s.clipboardRead = nil
				s.clipboardWrite = nil
			},
			wantErr: "Host clipboard is unavailable",
		},
		{
			name:   "unsupported agent",
			paneID: "pane-1",
			setup: func(s *Server) {
				s.state.CommitInventory([]*coordinator.AgentState{{PaneID: "pane-1", Agent: "unknown", Status: "idle"}}, s.state.RevisionCounter())
				s.clipboardRead = func(context.Context) ([]byte, error) { return nil, nil }
				s.clipboardWrite = func(context.Context, []byte) error { return nil }
			},
			wantErr: "Agent does not support response copying",
		},
		{
			name:   "working agent",
			paneID: "pane-1",
			setup: func(s *Server) {
				s.state.CommitInventory([]*coordinator.AgentState{{PaneID: "pane-1", Agent: "claude", Status: "working"}}, s.state.RevisionCounter())
				s.clipboardRead = func(context.Context) ([]byte, error) { return nil, nil }
				s.clipboardWrite = func(context.Context, []byte) error { return nil }
			},
			wantErr: "Agent is still working; wait for the current turn to finish",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			s := testServer()
			if test.setup != nil {
				test.setup(s)
			}
			result := sendCopyRequest(t, openCopyTestClient(t, s), test.name, test.paneID)
			if got, _ := result["error"].(string); got != test.wantErr {
				t.Fatalf("copy error = %q, want %q; response = %#v", got, test.wantErr, result)
			}
		})
	}
}

func TestCopyAgentResponseRejectsReplacedPane(t *testing.T) {
	s := testServer()
	const paneID = "pane-1"
	s.state.CommitInventory([]*coordinator.AgentState{
		{PaneID: paneID, Agent: "claude", Status: "idle", PaneRevision: 4},
	}, s.state.RevisionCounter())
	s.clipboardRead = func(context.Context) ([]byte, error) { return []byte("before"), nil }
	s.clipboardWrite = func(context.Context, []byte) error { return nil }
	s.copyRunner = func(
		_ context.Context,
		paneID string,
		_ slashcmd.CopyProfile,
		_ copyresponse.Pane,
		_ copyresponse.ClipboardReader,
		_ copyresponse.ClipboardWriter,
		_ int64,
		_ copyresponse.RevisionReader,
	) (copyresponse.Result, error) {
		s.state.BumpGeneration(paneID)
		return copyresponse.Result{Text: "response", Source: "clipboard", Chars: 8, Lines: 1}, nil
	}
	result := sendCopyRequest(t, openCopyTestClient(t, s), "replaced", paneID)
	if got, _ := result["error"].(string); got != "The agent pane was replaced while the response was being copied" {
		t.Fatalf("copy error = %q; response = %#v", got, result)
	}
}

func TestCopyAgentResponseReturnsCopiedData(t *testing.T) {
	s := testServer()
	const paneID = "pane-1"
	s.state.CommitInventory([]*coordinator.AgentState{
		{PaneID: paneID, Agent: "claude", Status: "idle", PaneRevision: 4},
	}, s.state.RevisionCounter())
	s.clipboardRead = func(context.Context) ([]byte, error) { return []byte("before"), nil }
	s.clipboardWrite = func(context.Context, []byte) error { return nil }
	s.copyRunner = func(
		context.Context,
		string,
		slashcmd.CopyProfile,
		copyresponse.Pane,
		copyresponse.ClipboardReader,
		copyresponse.ClipboardWriter,
		int64,
		copyresponse.RevisionReader,
	) (copyresponse.Result, error) {
		return copyresponse.Result{Text: "response", Source: "clipboard", Chars: 8, Lines: 1}, nil
	}
	result := sendCopyRequest(t, openCopyTestClient(t, s), "success", paneID)
	if ok, _ := result["ok"].(bool); !ok {
		t.Fatalf("copy result = %#v, want success", result)
	}
	data, _ := result["data"].(map[string]any)
	if data["text"] != "response" || data["source"] != "clipboard" {
		t.Fatalf("copy data = %#v", data)
	}
}

func TestHealthz(t *testing.T) {
	s := testServer()
	s.ready = true
	s.state.CommitInventory(nil, 0)
	req := httptest.NewRequest("GET", "/healthz", nil)
	w := httptest.NewRecorder()

	s.handleHealthz(w, req)

	if w.Code != 200 {
		t.Fatalf("status = %d, want 200", w.Code)
	}

	var resp map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if resp["status"] != "ok" {
		t.Errorf("status = %v, want ok", resp["status"])
	}
	if resp["readiness"] != "ready" {
		t.Errorf("readiness = %v, want ready", resp["readiness"])
	}
	if resp["instance"] != "test-instance" {
		t.Errorf("instance = %v, want test-instance", resp["instance"])
	}
	if resp["release_version"] != "0.9.0" {
		t.Errorf("release_version = %v, want 0.9.0", resp["release_version"])
	}
	if resp["revision"] != "abc123" {
		t.Errorf("revision = %v, want abc123", resp["revision"])
	}
	if resp["protocol"] != float64(protocol.Version) {
		t.Errorf("protocol = %v, want %d", resp["protocol"], protocol.Version)
	}
	if resp["gateway_available_version"] != "0.9.0" {
		t.Errorf("gateway_available_version = %v, want 0.9.0", resp["gateway_available_version"])
	}
}

func TestReadyzIncludesReleaseIdentityForEmptyInventory(t *testing.T) {
	s := testServer()
	s.ready = true
	s.state.CommitInventory(nil, 0)
	w := httptest.NewRecorder()
	s.handleReadyz(w, httptest.NewRequest("GET", "/readyz", nil))
	var response map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if w.Code != http.StatusOK || response["status"] != "ready" || response["instance"] != "test-instance" || response["release_version"] != "0.9.0" || response["revision"] != "abc123" {
		t.Fatalf("readiness response: %d %+v", w.Code, response)
	}
	if w.Header().Get("Cache-Control") != "no-store" {
		t.Fatal("readiness may be cached")
	}
}

func TestReadyzNotReady(t *testing.T) {
	s := testServer()
	s.ready = false
	req := httptest.NewRequest("GET", "/readyz", nil)
	w := httptest.NewRecorder()

	s.handleReadyz(w, req)

	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", w.Code)
	}

	var resp map[string]any
	json.Unmarshal(w.Body.Bytes(), &resp)
	if resp["status"] != "unavailable" {
		t.Errorf("status = %v, want unavailable", resp["status"])
	}
}

func enrollBootstrapDevice(t *testing.T, runtimeDir, token string) {
	t.Helper()
	store, err := deviceauth.Open(filepath.Join(runtimeDir, "device-auth"))
	if err != nil {
		t.Fatal(err)
	}
	if err := store.EnsureBootstrapInvitation([]byte(token), "relay", "en"); err != nil {
		t.Fatal(err)
	}
	result, err := store.CompleteE2EEAuth(context.Background(), transport.E2EEAuthSelector{
		Kind: transport.E2EEAuthInvitation, ID: "bootstrap", Version: 1, Locale: "en",
	}, true)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.CompleteE2EEAuth(context.Background(), transport.E2EEAuthSelector{
		Kind: transport.E2EEAuthCredential, ID: result.Identity.CredentialID,
		Version: result.Identity.CredentialVersion, Locale: result.Identity.Locale,
	}, true); err != nil {
		t.Fatal(err)
	}
}

func TestRearmedLaunchStartsWithoutStrandedDevices(t *testing.T) {
	token := strings.Repeat("k", 32)
	for _, test := range []struct {
		name    string
		rearm   bool
		devices int
	}{
		{name: "quick tunnel forgets devices stranded under the previous hostname", rearm: true, devices: 0},
		{name: "stable install keeps its paired devices", rearm: false, devices: 1},
	} {
		t.Run(test.name, func(t *testing.T) {
			root := t.TempDir()
			runtimeDir := filepath.Join(root, "runtime")
			enrollBootstrapDevice(t, runtimeDir, token)
			cfg := &config.Config{
				Token:          token,
				RearmBootstrap: test.rearm,
				RuntimeDir:     runtimeDir,
				CacheDir:       filepath.Join(root, "cache"),
			}
			s := New(cfg, "0.9.0", "abc123", slog.New(slog.NewTextHandler(io.Discard, nil)))
			if s.deviceAuth == nil {
				t.Fatal("device store was not initialized")
			}
			if got := len(s.deviceAuth.ListCredentials("")); got != test.devices {
				t.Fatalf("paired devices after launch = %d, want %d", got, test.devices)
			}
		})
	}
}

func TestServerRunAndShutdown(t *testing.T) {
	root := t.TempDir()
	webRoot := filepath.Join(root, "web")
	if err := os.MkdirAll(webRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(webRoot, "index.html"), []byte("<html></html>"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{
		Host:        "127.0.0.1",
		Port:        18999,
		InstanceID:  "shutdown-test",
		WebRoot:     webRoot,
		RuntimeDir:  filepath.Join(root, "runtime"),
		CacheDir:    filepath.Join(root, "cache"),
		ConfigHome:  filepath.Join(root, "config"),
		ReleaseRoot: filepath.Join(root, "release"),
	}
	s := New(cfg, "0.9.0", "abc123", slog.New(slog.NewTextHandler(io.Discard, nil)))

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- s.Run(ctx) }()

	// One second of polling is not a startup budget on a loaded CI runner under
	// -race, and a server that died immediately should say so instead of timing
	// out with a connection refused.
	deadline := time.Now().Add(15 * time.Second)
	var resp *http.Response
	var err error
	for time.Now().Before(deadline) {
		select {
		case runErr := <-done:
			t.Fatalf("Run returned before the server answered: %v", runErr)
		default:
		}
		if resp, err = http.Get("http://127.0.0.1:18999/health"); err == nil {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if err != nil {
		t.Fatalf("server did not answer /health within 15s: %v", err)
	}
	resp.Body.Close()

	pid, err := os.ReadFile(filepath.Join(cfg.RuntimeDir, "relay.pid"))
	if err != nil || strings.TrimSpace(string(pid)) != strconv.Itoa(os.Getpid()) {
		t.Fatalf("relay.pid = %q, %v; want this process id", pid, err)
	}

	cancel()
	if err := <-done; err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
	if _, err := os.Stat(filepath.Join(cfg.RuntimeDir, "relay.pid")); !os.IsNotExist(err) {
		t.Fatalf("relay.pid survives shutdown: %v", err)
	}
}

func TestRecentSafeErrorsAreBoundedAndSingleLine(t *testing.T) {
	s := testServer()
	for index := 0; index < 25; index++ {
		s.recordSafeError("component failed", errors.New("safe\nmessage"))
	}
	recent := s.recentSafeErrors()
	if len(recent) != 20 {
		t.Fatalf("recent errors = %d, want 20", len(recent))
	}
	for _, message := range recent {
		if strings.Contains(message, "\n") || !strings.Contains(message, "component failed: safe message") {
			t.Fatalf("unsafe recent error = %q", message)
		}
	}
}

func TestRequestedPaneWatchInterval(t *testing.T) {
	tests := []struct {
		name  string
		value any
		want  time.Duration
	}{
		{name: "fast", value: float64(100), want: 100 * time.Millisecond},
		{name: "default", value: float64(250), want: 250 * time.Millisecond},
		{name: "balanced battery", value: float64(500), want: 500 * time.Millisecond},
		{name: "slow", value: float64(1_000), want: time.Second},
		{name: "missing", value: nil, want: defaultPaneWatchInterval},
		{name: "unsupported", value: float64(333), want: defaultPaneWatchInterval},
		{name: "fractional", value: 100.5, want: defaultPaneWatchInterval},
		{name: "wrong type", value: "100", want: defaultPaneWatchInterval},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := requestedPaneWatchInterval(test.value); got != test.want {
				t.Fatalf("requestedPaneWatchInterval(%v) = %s, want %s", test.value, got, test.want)
			}
		})
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
	s.commitAgent(current, map[string]any{"type": "agent_update", "pane_id": "pane-1"})
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
		map[string]any{"type": "agent_update", "pane_id": "pane-1"},
	)
	s.commitAgent(
		&coordinator.AgentState{PaneID: "pane-1", Status: "blocked", BlockedEventID: "stale-event", StateRevision: revision - 1},
		map[string]any{"type": "agent_update", "pane_id": "pane-1"},
	)
	publishInventoryForTest(t, s)

	agents := s.committedAgents()
	if len(agents) != 1 || agents[0].Status != "working" ||
		agents[0].StateRevision != revision || agents[0].BlockedEventID != "" {
		t.Fatalf("reconnect snapshot regressed after stale messages: %+v", agents)
	}
}

func transitionPushQueue(t *testing.T, s *Server) func() []push.Payload {
	t.Helper()
	directory := t.TempDir()
	manager, err := push.NewManager(directory, s.logger)
	if err != nil {
		t.Fatal(err)
	}
	subscription := push.Subscription{
		Endpoint: "https://fcm.googleapis.com/phone", DeviceID: "device-a",
	}
	subscription.Keys.P256dh = "key"
	subscription.Keys.Auth = "auth"
	if err := manager.Subscribe(subscription); err != nil {
		t.Fatal(err)
	}
	policy := manager.Policy(subscription.DeviceID, "en")
	policy.Categories[push.CategoryFinished] = true
	if err := manager.SetPolicy(policy); err != nil {
		t.Fatal(err)
	}
	s.pushM = manager
	return func() []push.Payload { return queuedPushPayloads(t, directory) }
}

func queuedPushPayloads(t *testing.T, directory string) []push.Payload {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(directory, "queue.json"))
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
	var queue struct {
		Entries map[string]struct {
			Event push.PushEvent `json:"event"`
		} `json:"entries"`
	}
	if err := json.Unmarshal(data, &queue); err != nil {
		t.Fatal(err)
	}
	payloads := make([]push.Payload, 0, len(queue.Entries))
	for _, entry := range queue.Entries {
		var payload push.Payload
		if err := json.Unmarshal(entry.Event.Payload, &payload); err != nil {
			t.Fatal(err)
		}
		payloads = append(payloads, payload)
	}
	return payloads
}

type recordingTransitionBroadcast struct {
	messages chan any
}

func (b *recordingTransitionBroadcast) Broadcast(message any) {
	b.messages <- message
}

func TestUnchangedPollPreservesBlockedTransitionSideEffects(t *testing.T) {
	root := t.TempDir()
	s := testServer()
	journal, err := activity.OpenJournal(filepath.Join(root, "cache"))
	if err != nil {
		t.Fatal(err)
	}
	s.dispatcher = coordinator.NewDispatcher(nil, s.state, journal, s.logger)
	t.Cleanup(func() {
		_ = s.dispatcher.Close(context.Background())
	})
	queuedPushes := transitionPushQueue(t, s)
	broadcast := &recordingTransitionBroadcast{messages: make(chan any, 2)}
	s.transitionBroadcast = broadcast
	enrichStarted := make(chan struct{})
	enrichRelease := make(chan struct{})
	s.transitionEnrich = func(_ context.Context, agent *coordinator.AgentState) {
		close(enrichStarted)
		<-enrichRelease
		agent.AttentionKind = "approval"
		agent.Command = "Approve deployment"
		agent.Prompt = "Allow this command?"
		agent.Options = []string{"Approve", "Reject"}
	}
	s.state.CommitInventory([]*coordinator.AgentState{{
		PaneID: "pane-1", TerminalID: "terminal-1", Agent: "codex", Project: "relay", Status: "working",
	}}, s.state.RevisionCounter())
	s.state.CommitEvent("pane-1", "blocked", time.Now().UnixMilli())
	revision := s.state.Revision("pane-1")

	done := make(chan struct{})
	go func() {
		s.handleTransition(context.Background(), "pane-1", "codex", "relay", "blocked", revision, time.Now().UnixMilli())
		close(done)
	}()
	<-enrichStarted
	s.state.CommitInventory([]*coordinator.AgentState{{
		PaneID: "pane-1", TerminalID: "terminal-1", Agent: "codex", Project: "relay", Status: "blocked",
	}}, s.state.RevisionCounter())
	close(enrichRelease)
	<-done

	if entries := journal.Recent(10); len(entries) != 1 ||
		entries[0].Kind != "blocked" || entries[0].Summary != "Approve deployment" {
		t.Fatalf("blocked activity entries = %+v, want exactly one enriched entry", entries)
	}
	payloads := queuedPushes()
	if len(payloads) != 1 {
		t.Fatalf("blocked pushes = %+v, want exactly one", payloads)
	}
	agent, _ := s.state.Agent("pane-1")
	wantKey := push.PushEventKey{
		DeviceID: "device-a", ServerSessionID: "primary", PaneID: agent.PaneID,
		TerminalID: agent.TerminalID, AgentSessionID: agent.SessionID, Generation: agent.Generation,
		EventID: agent.BlockedEventID, InteractionRevision: s.state.AttentionRevision(agent.PaneID),
		Category: push.CategoryAttention,
	}
	if payloads[0].Version != 1 || payloads[0].Key != wantKey || payloads[0].EventRef == "" {
		t.Fatalf("blocked push = %+v, want key %+v with event reference", payloads[0], wantKey)
	}
	if len(broadcast.messages) != 1 {
		t.Fatalf("blocked broadcasts = %d, want exactly one", len(broadcast.messages))
	}
}

func TestBlockedTransitionDoesNotPublishOrBroadcastAfterNewerWorkingState(t *testing.T) {
	s := testServer()
	queuedPushes := transitionPushQueue(t, s)
	broadcast := &recordingTransitionBroadcast{messages: make(chan any, 1)}
	s.transitionBroadcast = broadcast
	enrichStarted := make(chan struct{})
	enrichRelease := make(chan struct{})
	s.transitionEnrich = func(_ context.Context, agent *coordinator.AgentState) {
		close(enrichStarted)
		<-enrichRelease
		agent.AttentionKind = question.AttentionApproval
	}
	s.state.CommitInventory([]*coordinator.AgentState{{
		PaneID: "pane-1", TerminalID: "terminal-1", Agent: "codex", Project: "relay", Status: "working",
	}}, s.state.RevisionCounter())
	s.state.CommitEvent("pane-1", "blocked", time.Now().UnixMilli())
	revision := s.state.Revision("pane-1")

	done := make(chan struct{})
	go func() {
		s.handleTransition(context.Background(), "pane-1", "codex", "relay", "blocked", revision, time.Now().UnixMilli())
		close(done)
	}()
	<-enrichStarted

	s.state.CommitEvent("pane-1", "working", time.Now().UnixMilli())
	close(enrichRelease)
	<-done

	if payloads := queuedPushes(); len(payloads) != 0 {
		t.Fatalf("stale blocked transition queued pushes: %+v", payloads)
	}
	select {
	case message := <-broadcast.messages:
		t.Fatalf("stale blocked transition was broadcast after working revision: %#v", message)
	default:
	}
}

func TestApprovalPushSuppressedWhenAttentionIsReclassified(t *testing.T) {
	s := testServer()
	queuedPushes := transitionPushQueue(t, s)
	broadcast := &recordingTransitionBroadcast{messages: make(chan any, 1)}
	s.transitionBroadcast = broadcast
	enrichStarted := make(chan struct{})
	enrichRelease := make(chan struct{})
	s.transitionEnrich = func(_ context.Context, agent *coordinator.AgentState) {
		close(enrichStarted)
		<-enrichRelease
		agent.AttentionKind = question.AttentionApproval
		agent.Command = "Approve deployment"
		agent.Prompt = "Allow this command?"
		agent.Options = []string{"Approve", "Reject"}
	}
	s.state.CommitInventory([]*coordinator.AgentState{{
		PaneID: "pane-1", TerminalID: "terminal-1", Agent: "codex", Project: "relay", Status: "working",
	}}, s.state.RevisionCounter())
	s.state.CommitEvent("pane-1", "blocked", time.Now().UnixMilli())
	revision := s.state.Revision("pane-1")

	done := make(chan struct{})
	go func() {
		s.handleTransition(context.Background(), "pane-1", "codex", "relay", "blocked", revision, time.Now().UnixMilli())
		close(done)
	}()
	<-enrichStarted

	agent, ok := s.state.Agent("pane-1")
	if !ok {
		t.Fatal("blocked agent disappeared")
	}
	generation, active := s.state.PaneSession("pane-1")
	if !active {
		t.Fatal("blocked pane session disappeared")
	}
	reclassified, committed := s.state.CommitAttentionClassification(
		"pane-1",
		agent.BlockedEventID,
		generation,
		s.state.ContentRevision("pane-1"),
		question.Classification{
			Kind:   question.AttentionChat,
			Prompt: "What would you like to work on next?",
		},
	)
	if !committed {
		t.Fatal("chat reclassification was not committed")
	}
	close(enrichRelease)
	<-done

	if payloads := queuedPushes(); len(payloads) != 0 {
		t.Fatalf("reclassified approval queued pushes: %+v", payloads)
	}
	select {
	case message := <-broadcast.messages:
		t.Fatalf("stale approval was broadcast after reclassification: %#v", message)
	default:
	}

	s.handleChatCompletion(context.Background(), reclassified)
	if payloads := queuedPushes(); len(payloads) != 1 || payloads[0].Key.Category != push.CategoryFinished {
		t.Fatalf("chat reclassification pushes = %+v, want one completion", payloads)
	}
}

func TestChatClassificationUsesOneCompletionPath(t *testing.T) {
	root := t.TempDir()
	s := testServer()
	journal, err := activity.OpenJournal(filepath.Join(root, "cache"))
	if err != nil {
		t.Fatal(err)
	}
	s.dispatcher = coordinator.NewDispatcher(nil, s.state, journal, s.logger)
	t.Cleanup(func() {
		_ = s.dispatcher.Close(context.Background())
	})
	queuedPushes := transitionPushQueue(t, s)
	broadcast := &recordingTransitionBroadcast{messages: make(chan any, 3)}
	s.transitionBroadcast = broadcast
	s.transitionEnrich = func(_ context.Context, agent *coordinator.AgentState) {
		agent.AttentionKind = "chat"
		agent.Prompt = "Hello! What would you like to work on next?"
		agent.Options = []string{"fabricated", "controls"}
	}
	s.state.CommitInventory([]*coordinator.AgentState{{
		PaneID: "pane-1", TerminalID: "terminal-1", Agent: "codex", Project: "relay", Status: "working",
	}}, s.state.RevisionCounter())
	s.state.CommitEvent("pane-1", "blocked", time.Now().UnixMilli())
	s.handleTransition(
		context.Background(), "pane-1", "codex", "relay", "blocked",
		s.state.Revision("pane-1"), time.Now().UnixMilli(),
	)

	entries := journal.Recent(10)
	if len(entries) != 1 || entries[0].Kind != "finished" ||
		entries[0].Extract != "Hello! What would you like to work on next?" {
		t.Fatalf("chat activities = %+v, want one completion", entries)
	}
	payloads := queuedPushes()
	if len(payloads) != 1 {
		t.Fatalf("chat pushes = %+v, want one completion push", payloads)
	}
	payload := payloads[0]
	if payload.Key.Category != push.CategoryFinished || !strings.Contains(payload.Title, "finished") ||
		len(payload.Actions) != 0 || len(payload.ActionRefs) != 0 {
		t.Fatalf("chat push = %+v", payload)
	}
	message := (<-broadcast.messages).(map[string]any)
	if fmt.Sprint(message["attention_kind"]) != "chat" {
		t.Fatalf("chat broadcast = %+v", message)
	}
	if options, ok := message["options"].([]string); ok && len(options) != 0 {
		t.Fatalf("chat broadcast retained controls: %+v", message)
	}

	s.state.CommitEvent("pane-1", "idle", time.Now().UnixMilli())
	s.handleTransition(
		context.Background(), "pane-1", "codex", "relay", "idle",
		s.state.Revision("pane-1"), time.Now().UnixMilli(),
	)
	if len(journal.Recent(10)) != 1 || len(queuedPushes()) != 1 {
		t.Fatal("raw idle duplicated the classified chat completion")
	}
}

func TestUnknownClassificationHasNoNotificationActions(t *testing.T) {
	root := t.TempDir()
	s := testServer()
	journal, err := activity.OpenJournal(filepath.Join(root, "cache"))
	if err != nil {
		t.Fatal(err)
	}
	s.dispatcher = coordinator.NewDispatcher(nil, s.state, journal, s.logger)
	t.Cleanup(func() {
		_ = s.dispatcher.Close(context.Background())
	})
	queuedPushes := transitionPushQueue(t, s)
	s.transitionBroadcast = &recordingTransitionBroadcast{messages: make(chan any, 1)}
	s.transitionEnrich = func(_ context.Context, agent *coordinator.AgentState) {
		agent.AttentionKind = "unknown"
		agent.Prompt = "Agent needs inspection"
		agent.Options = []string{"Approve", "Reject"}
	}
	s.state.CommitInventory([]*coordinator.AgentState{{
		PaneID: "pane-1", TerminalID: "terminal-1", Agent: "opencode", Project: "relay", Status: "working",
	}}, s.state.RevisionCounter())
	s.state.CommitEvent("pane-1", "blocked", time.Now().UnixMilli())
	s.handleTransition(
		context.Background(), "pane-1", "opencode", "relay", "blocked",
		s.state.Revision("pane-1"), time.Now().UnixMilli(),
	)

	payloads := queuedPushes()
	if len(payloads) != 1 {
		t.Fatalf("unknown pushes = %+v, want one inspection push", payloads)
	}
	payload := payloads[0]
	if payload.Key.Category != push.CategoryAttention || payload.Title != "Herdr notification" ||
		len(payload.Actions) != 0 || len(payload.ActionRefs) != 0 {
		t.Fatalf("unknown push = %+v", payload)
	}
	agent, _ := s.state.Agent("pane-1")
	if len(agent.Options) != 0 || agent.Interaction != nil {
		t.Fatalf("unknown classification retained controls: %+v", agent)
	}
}

func TestUnchangedPaneResponseSuppressesTerminalContent(t *testing.T) {
	response := map[string]any{
		"request_id": "identified-read",
		"type":       "pane_content",
		"pane_id":    "w1:p1",
		"content":    "unchanged output",
		"format":     "ansi",
		"target": protocol.TargetRef{
			ServerSessionID: "primary",
			PaneID:          "w1:p1",
			TerminalID:      "terminal-w1:p1",
			Generation:      4,
		},
	}
	fingerprint := paneFingerprint("unchanged output")
	unchanged := unchangedPaneResponse(
		map[string]any{"content_fingerprint": fingerprint},
		response,
	)
	if unchanged == nil {
		t.Fatal("matching terminal content was not suppressed")
	}
	if unchanged["type"] != "pane_unchanged" || unchanged["pane_id"] != "w1:p1" {
		t.Fatalf("unexpected unchanged response: %#v", unchanged)
	}
	if unchanged["target"] != response["target"] {
		t.Fatalf("unchanged response lost exact target: %#v", unchanged)
	}
	if unchanged["request_id"] != response["request_id"] {
		t.Fatalf("unchanged response lost read identity: %#v", unchanged)
	}
	if _, included := unchanged["content"]; included {
		t.Fatalf("unchanged response included terminal content: %#v", unchanged)
	}
	if response["content_fingerprint"] != fingerprint {
		t.Fatalf("full response fingerprint = %v, want %s", response["content_fingerprint"], fingerprint)
	}

	changed := unchangedPaneResponse(
		map[string]any{"content_fingerprint": "older"},
		response,
	)
	if changed != nil {
		t.Fatalf("changed terminal content was suppressed: %#v", changed)
	}
}
