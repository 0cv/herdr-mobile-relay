package app

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/0cv/herdr-mobile-relay/internal/audit"
	"github.com/0cv/herdr-mobile-relay/internal/coordinator"
	"github.com/0cv/herdr-mobile-relay/internal/protocol"
	"github.com/0cv/herdr-mobile-relay/internal/transport"
	"github.com/coder/websocket"
)

func dialHub(t *testing.T, s *Server) *websocket.Conn {
	t.Helper()
	httpServer := httptest.NewServer(http.HandlerFunc(s.hub.HandleWebSocket))
	t.Cleanup(httpServer.Close)
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_ = s.hub.Shutdown(ctx)
	})
	conn, _, err := websocket.Dial(t.Context(), "ws"+strings.TrimPrefix(httpServer.URL, "http"), nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.CloseNow() })
	return conn
}

func useProductionHandler(t *testing.T, s *Server) {
	t.Helper()
	s.hub.SetHandler(func(client *transport.ClientConn, message map[string]any, admitted func()) {
		s.handleMessage(t.Context(), client, message, admitted)
	})
	s.hub.SetOnDisconnect(s.handleDisconnect)
}

func connectedClient(t *testing.T, s *Server) (*websocket.Conn, *transport.ClientConn) {
	t.Helper()
	connected := make(chan *transport.ClientConn, 1)
	s.hub.SetOnConnect(func(client *transport.ClientConn) { connected <- client })
	conn := dialHub(t, s)
	select {
	case client := <-connected:
		return conn, client
	case <-time.After(2 * time.Second):
		t.Fatal("client did not connect")
		return nil, nil
	}
}

func sendFrame(t *testing.T, conn *websocket.Conn, message map[string]any) {
	t.Helper()
	payload, err := json.Marshal(message)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancel()
	if err := conn.Write(ctx, websocket.MessageText, payload); err != nil {
		t.Fatal(err)
	}
}

func readFrame(t *testing.T, conn *websocket.Conn) map[string]any {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancel()
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

func roundTrip(t *testing.T, conn *websocket.Conn, message map[string]any) map[string]any {
	t.Helper()
	sendFrame(t, conn, message)
	return readFrame(t, conn)
}

type gateReject struct {
	name    string
	message map[string]any
	code    string
	args    map[string]any
}

func rejectedMessages() []gateReject {
	target := func(paneID string, generation int) map[string]any {
		return map[string]any{
			"server_session_id": protocol.PrimaryServerSessionID, "pane_id": paneID, "terminal_id": "terminal-1",
			"generation": generation, "agent_session_id": "agent-session-1",
		}
	}
	return []gateReject{
		{
			name:    "payload that cannot be decoded",
			message: map[string]any{"type": 5, "request_id": "req-1"},
			code:    protocol.ErrorInvalidRequest,
		},
		{
			name:    "unknown action",
			message: map[string]any{"type": "no_such_action", "request_id": "req-1", "protocol": protocol.Version},
			code:    protocol.ErrorUnknownAction,
			args:    map[string]any{"operation": "no_such_action"},
		},
		{
			name:    "mutating action without protocol",
			message: map[string]any{"type": "send_keys", "request_id": "req-1"},
			code:    protocol.ErrorIncompatibleProtocol,
			args:    map[string]any{"received": "invalid", "required": fmt.Sprint(protocol.Version)},
		},
		{
			name: "session ids that differ",
			message: map[string]any{
				"type": "read_pane", "request_id": "req-1", "server_session_id": "primary",
				"target": map[string]any{"server_session_id": "other"},
			},
			code: protocol.ErrorInvalidRequest,
			args: map[string]any{"field": "server_session_id"},
		},
		{
			name:    "session other than primary",
			message: map[string]any{"type": "read_pane", "request_id": "req-1", "server_session_id": "other"},
			code:    protocol.ErrorInvalidRequest,
			args:    map[string]any{"field": "server_session_id"},
		},
		{
			name:    "pi integration read by an unauthenticated client",
			message: map[string]any{"type": "get_pi_integration", "request_id": "req-1"},
			code:    protocol.ErrorReaderDenied,
			args:    map[string]any{"operation": "get_pi_integration"},
		},
		{
			name:    "push policy read by an unauthenticated client",
			message: map[string]any{"type": "push_policy_get", "request_id": "req-1"},
			code:    protocol.ErrorReaderDenied,
			args:    map[string]any{"operation": "push_policy_get"},
		},
		{
			name: "target pane that differs from pane_id",
			message: map[string]any{
				"type": "read_pane", "request_id": "req-1", "pane_id": "pane-1",
				"target": target("pane-2", 1),
			},
			code: protocol.ErrorInvalidRequest,
			args: map[string]any{"field": "target.pane_id"},
		},
		{
			name: "target that no longer matches the agent",
			message: map[string]any{
				"type": "read_pane", "request_id": "req-1", "pane_id": "pane-1",
				"target": target("pane-1", 2),
			},
			code: protocol.ErrorInvalidRequest,
			args: map[string]any{"field": "target"},
		},
	}
}

func handlerTestServer(t *testing.T) *Server {
	t.Helper()
	s := testServerWithCacheDir(t.TempDir())
	s.state.CommitInventory([]*coordinator.AgentState{{
		PaneID: "pane-1", Agent: "claude", TerminalID: "terminal-1", Generation: 1,
		SessionID: "agent-session-1", ServerSessionID: protocol.PrimaryServerSessionID, Status: "working",
	}}, s.state.RevisionCounter())
	return s
}

func apiErrorOf(t *testing.T, response map[string]any) map[string]any {
	t.Helper()
	if receipt, ok := response["receipt"].(map[string]any); ok {
		response = receipt
	}
	apiError, ok := response["error"].(map[string]any)
	if !ok {
		t.Fatalf("response carries no error: %#v", response)
	}
	return apiError
}

func TestHandleMessageGate(t *testing.T) {
	s := handlerTestServer(t)
	useProductionHandler(t, s)
	conn := dialHub(t, s)

	for _, reject := range rejectedMessages() {
		t.Run(reject.name, func(t *testing.T) {
			apiError := apiErrorOf(t, roundTrip(t, conn, reject.message))
			if apiError["code"] != reject.code {
				t.Fatalf("code = %v, want %s", apiError["code"], reject.code)
			}
			if args, _ := apiError["args"].(map[string]any); !reflect.DeepEqual(args, reject.args) {
				t.Fatalf("args = %#v, want %#v", apiError["args"], reject.args)
			}
		})
	}

	t.Run("device list from an unauthenticated client reaches its body", func(t *testing.T) {
		response := roundTrip(t, conn, map[string]any{"type": "device_list", "request_id": "req-1"})
		if response["type"] != "command_result" || response["action"] != "device_list" ||
			response["ok"] != false || response["error"] != "Device management is unavailable" {
			t.Fatalf("response = %#v", response)
		}
	})
}

func TestHandleMessageAdmitsOnEveryRejectPath(t *testing.T) {
	s := handlerTestServer(t)
	useProductionHandler(t, s)
	_, client := connectedClient(t, s)

	messages := map[string]map[string]any{
		"successful uncoordinated action": {"type": "list_directories", "request_id": "req-1", "path": s.home},
	}
	for _, reject := range rejectedMessages() {
		messages[reject.name] = reject.message
	}
	for name, message := range messages {
		t.Run(name, func(t *testing.T) {
			var calls atomic.Int32
			s.handleMessage(t.Context(), client, message, func() { calls.Add(1) })
			if got := calls.Load(); got != 2 {
				t.Fatalf("admitted called %d times, want 2", got)
			}
		})
	}
}

func readAuditRecords(t *testing.T, cacheDir string) []audit.Record {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(cacheDir, "audit", "remote-writes.jsonl"))
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
	var records []audit.Record
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		if line == "" {
			continue
		}
		var record audit.Record
		if err := json.Unmarshal([]byte(line), &record); err != nil {
			t.Fatal(err)
		}
		records = append(records, record)
	}
	return records
}

func TestHandleMessageAuditsMutationsBeforeDispatch(t *testing.T) {
	cacheDir := t.TempDir()
	s := testServerWithCacheDir(cacheDir)
	auditLog, err := audit.Open(cacheDir)
	if err != nil {
		t.Fatal(err)
	}
	s.auditLog = auditLog
	useProductionHandler(t, s)
	conn := dialHub(t, s)

	roundTrip(t, conn, map[string]any{"type": "list_directories", "request_id": "read-1", "path": s.home})
	roundTrip(t, conn, map[string]any{"type": "rename_device", "request_id": "no-protocol", "device_id": "device-1", "name": "Phone"})
	if records := readAuditRecords(t, cacheDir); len(records) != 0 {
		t.Fatalf("read-only and rejected actions wrote audit records: %#v", records)
	}

	roundTrip(t, conn, map[string]any{
		"type": "rename_device", "request_id": "write-1", "protocol": protocol.Version, "device_id": "device-1", "name": "Phone",
	})
	records := readAuditRecords(t, cacheDir)
	if len(records) != 2 {
		t.Fatalf("audit records = %#v, want an attempt and a result", records)
	}
	if attempt := records[0]; attempt.Stage != "attempt" || attempt.Action != "rename_device" || attempt.RequestID != "write-1" {
		t.Fatalf("first record = %#v, want the attempt", attempt)
	}
	if result := records[1]; result.Stage != "result" || result.OK == nil || *result.OK {
		t.Fatalf("second record = %#v, want the failed result", result)
	}
}
