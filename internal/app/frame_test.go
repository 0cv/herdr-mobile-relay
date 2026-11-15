package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/0cv/herdr-mobile-relay/internal/activity"
	"github.com/0cv/herdr-mobile-relay/internal/backend"
	"github.com/0cv/herdr-mobile-relay/internal/coordinator"
	"github.com/0cv/herdr-mobile-relay/internal/herdr"
	"github.com/0cv/herdr-mobile-relay/internal/protocol"
	"github.com/0cv/herdr-mobile-relay/internal/transport"
	"github.com/coder/websocket"
)

// frameBackend stands in for Herdr behind the real dispatcher. It records
// every call it receives and answers every pane read with one fixed content.
type frameBackend struct {
	backend.Backend
	mu      sync.Mutex
	calls   []string
	probes  []time.Time
	content string
}

func (b *frameBackend) record(format string, args ...any) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.calls = append(b.calls, fmt.Sprintf(format, args...))
}

func (b *frameBackend) called(prefix string) []string {
	b.mu.Lock()
	defer b.mu.Unlock()
	var matching []string
	for _, call := range b.calls {
		if strings.HasPrefix(call, prefix) {
			matching = append(matching, call)
		}
	}
	return matching
}

func (b *frameBackend) probeTimes() []time.Time {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]time.Time(nil), b.probes...)
}

func (b *frameBackend) read() herdr.PaneRead {
	b.mu.Lock()
	defer b.mu.Unlock()
	return herdr.PaneRead{Content: []byte(b.content)}
}

func (b *frameBackend) CapabilityStatus() herdr.ServerStatus {
	return herdr.ServerStatus{OneStepLaunch: true, Features: map[string]herdr.FeatureEvidence{
		herdr.FeatureAgentLaunch: {State: herdr.FeatureSupported},
	}}
}

func (b *frameBackend) SendKeys(_ context.Context, paneID string, keys []string) error {
	b.record("SendKeys %s %s", paneID, strings.Join(keys, ","))
	return nil
}

func (b *frameBackend) SendInput(_ context.Context, paneID string, input herdr.PaneInput) error {
	b.record("SendInput %s text=%q keys=%s", paneID, input.Text, strings.Join(input.Keys, ","))
	return nil
}

func (b *frameBackend) Prompt(_ context.Context, paneID, text string) error {
	b.record("Prompt %s %q", paneID, text)
	return nil
}

func (b *frameBackend) TabMove(_ context.Context, tabID string, insertIndex int) error {
	b.record("TabMove %s %d", tabID, insertIndex)
	return nil
}

func (b *frameBackend) ReadPane(_ context.Context, paneID string, lines int, format string) (herdr.PaneRead, error) {
	b.record("ReadPane %s lines=%d format=%s", paneID, lines, format)
	return b.read(), nil
}

func (b *frameBackend) ReadPaneRecent(_ context.Context, paneID string, lines int, format string) (herdr.PaneRead, error) {
	b.record("ReadPaneRecent %s lines=%d format=%s", paneID, lines, format)
	return b.read(), nil
}

func (b *frameBackend) ReadPaneVisible(_ context.Context, paneID string, lines int, format string) (herdr.PaneRead, error) {
	b.record("ReadPaneVisible %s lines=%d format=%s", paneID, lines, format)
	return b.read(), nil
}

func (b *frameBackend) ProbePaneVisible(_ context.Context, paneID string, lines int, format string) ([]byte, error) {
	b.record("ProbePaneVisible %s lines=%d format=%s", paneID, lines, format)
	b.mu.Lock()
	b.probes = append(b.probes, time.Now())
	b.mu.Unlock()
	return b.read().Content, nil
}

func (b *frameBackend) GetInventory(context.Context) (*herdr.Inventory, error) {
	return &herdr.Inventory{}, nil
}

func (b *frameBackend) WorkspaceList(context.Context) ([]herdr.Workspace, error) { return nil, nil }

func (b *frameBackend) PaneList(context.Context) ([]herdr.Pane, error) { return nil, nil }

func (b *frameBackend) LaunchTerminal(_ context.Context, argv []string, _, name, _, _ string, _ bool) (*herdr.Pane, bool, error) {
	b.record("LaunchTerminal %s %s", name, strings.Join(argv, " "))
	return nil, false, errors.New("launch refused")
}

func (b *frameBackend) PaneProcessInfoFresh(_ context.Context, paneID string) (*herdr.PaneProcessInfo, error) {
	b.record("PaneProcessInfoFresh %s", paneID)
	return nil, errors.New("no process information")
}

type frameEnv struct {
	server  *Server
	backend *frameBackend
	conn    *websocket.Conn
	client  *transport.ClientConn
}

// newFrameEnv starts a server with the real handler and dispatcher behind one
// connected client. Pane pane-1 is a working Claude agent with a tab.
func newFrameEnv(t *testing.T, agents ...*coordinator.AgentState) *frameEnv {
	t.Helper()
	s := testServerWithCacheDir(t.TempDir())
	fake := &frameBackend{content: "ready\n"}
	s.herdrC = fake
	journal, err := activity.OpenJournal(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	s.dispatcher = coordinator.NewDispatcher(fake, s.state, journal, s.logger)
	t.Cleanup(func() { _ = s.dispatcher.Close(context.Background()) })
	if len(agents) == 0 {
		agents = []*coordinator.AgentState{{
			PaneID: "pane-1", Agent: "claude", TerminalID: "terminal-1", TabID: "tab-1", Generation: 1,
			SessionID: "agent-session-1", ServerSessionID: protocol.PrimaryServerSessionID, Status: "working",
		}}
	}
	s.state.CommitInventory(agents, s.state.RevisionCounter())
	useProductionHandler(t, s)
	conn, client := connectedClient(t, s)
	return &frameEnv{server: s, backend: fake, conn: conn, client: client}
}

// exchange sends one frame for pane-1 and returns the next frame received.
// request_id and protocol are filled in unless the fields already carry them.
func (e *frameEnv) exchange(t *testing.T, fields map[string]any) map[string]any {
	t.Helper()
	sendFrame(t, e.conn, frameWithDefaults(fields))
	return readFrame(t, e.conn)
}

func frameWithDefaults(fields map[string]any) map[string]any {
	frame := map[string]any{"request_id": "req-1", "protocol": protocol.Version, "pane_id": "pane-1"}
	for key, value := range fields {
		frame[key] = value
	}
	return frame
}

func (e *frameEnv) target(t *testing.T, paneID string) map[string]any {
	t.Helper()
	agent, ok := e.server.state.Agent(paneID)
	if !ok {
		t.Fatalf("pane %s is not in the inventory", paneID)
	}
	target := map[string]any{
		"server_session_id": agent.ServerSessionID, "pane_id": agent.PaneID, "terminal_id": agent.TerminalID,
		"generation": agent.Generation,
	}
	if agent.SessionID != "" {
		target["agent_session_id"] = agent.SessionID
	}
	return target
}

func decodeInto(t *testing.T, value, destination any) {
	t.Helper()
	encoded, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(encoded, destination); err != nil {
		t.Fatal(err)
	}
}

// settle returns every frame the relay sent before it answered a later request.
func (e *frameEnv) settle(t *testing.T) []map[string]any {
	t.Helper()
	sendFrame(t, e.conn, frameWithDefaults(map[string]any{"type": "get_activity", "request_id": "settle"}))
	var earlier []map[string]any
	for {
		message := readFrame(t, e.conn)
		if message["type"] == "activity_history" {
			return earlier
		}
		earlier = append(earlier, message)
	}
}

func expectCommandResult(t *testing.T, response map[string]any, action string, ok bool, phase, errorText string) {
	t.Helper()
	if response["type"] != "command_result" || response["action"] != action || response["ok"] != ok ||
		response["phase"] != phase || response["error"] != errorText {
		t.Fatalf("%s result = %#v, want ok=%v phase=%s error=%q", action, response, ok, phase, errorText)
	}
}

func TestFrameWrongTypedFieldIsInvalidRequestWithRequestID(t *testing.T) {
	e := newFrameEnv(t)
	for name, fields := range map[string]map[string]any{
		"keys as a string":      {"type": "send_keys", "keys": "Enter"},
		"text as a number":      {"type": "submit_prompt", "text": 5},
		"lines as a string":     {"type": "read_pane", "lines": "30"},
		"insert_index fraction": {"type": "tab_reorder", "insert_index": 1.5},
		"pane_id as a number":   {"type": "send_keys", "pane_id": 7, "keys": []string{"Enter"}},
		"protocol as a string":  {"type": "send_keys", "protocol": "3", "keys": []string{"Enter"}},
		"target as a string":    {"type": "send_keys", "target": "pane-1", "keys": []string{"Enter"}},
	} {
		t.Run(name, func(t *testing.T) {
			response := e.exchange(t, fields)
			if response["type"] != "error" || response["request_id"] != "req-1" {
				t.Fatalf("response = %#v, want an error echoing the request id", response)
			}
			if apiError := apiErrorOf(t, response); apiError["code"] != protocol.ErrorInvalidRequest || apiError["args"] != nil {
				t.Fatalf("error = %#v, want invalid_request without args", apiError)
			}
		})
	}
	if calls := e.backend.called("Send"); len(calls) != 0 {
		t.Fatalf("a refused frame reached Herdr: %v", calls)
	}
}

func TestFrameWrongTypedRequestIDIsReturnedEmpty(t *testing.T) {
	e := newFrameEnv(t)
	response := e.exchange(t, map[string]any{"type": "read_pane", "request_id": 5})
	if response["type"] != "error" {
		t.Fatalf("response = %#v, want an error", response)
	}
	if requestID, present := response["request_id"]; present {
		t.Fatalf("request_id = %#v, want it left out", requestID)
	}
	if apiError := apiErrorOf(t, response); apiError["code"] != protocol.ErrorInvalidRequest {
		t.Fatalf("error = %#v, want invalid_request", apiError)
	}
}

func TestFrameWithoutTypeIsInvalidRequest(t *testing.T) {
	e := newFrameEnv(t)
	for name, fields := range map[string]map[string]any{
		"type left out":  {"keys": []string{"Enter"}},
		"empty type":     {"type": ""},
		"null type":      {"type": nil},
		"action instead": {"action": "send_keys", "keys": []string{"Enter"}},
	} {
		t.Run(name, func(t *testing.T) {
			response := e.exchange(t, fields)
			if response["type"] != "error" || response["request_id"] != "req-1" {
				t.Fatalf("response = %#v, want an error echoing the request id", response)
			}
			if apiError := apiErrorOf(t, response); apiError["code"] != protocol.ErrorInvalidRequest {
				t.Fatalf("error = %#v, want invalid_request", apiError)
			}
		})
	}
	if calls := e.backend.called("Send"); len(calls) != 0 {
		t.Fatalf("a refused frame reached Herdr: %v", calls)
	}
}

func TestFrameUnknownActionIsRefused(t *testing.T) {
	e := newFrameEnv(t)
	for _, action := range []string{"no_such_action", "command", "SEND_KEYS"} {
		t.Run(action, func(t *testing.T) {
			response := e.exchange(t, map[string]any{"type": action, "keys": []string{"Enter"}})
			if response["type"] != "error" || response["request_id"] != "req-1" {
				t.Fatalf("response = %#v, want an error echoing the request id", response)
			}
			apiError := apiErrorOf(t, response)
			if apiError["code"] != protocol.ErrorUnknownAction {
				t.Fatalf("error = %#v, want unknown_action", apiError)
			}
			if args, _ := apiError["args"].(map[string]any); args["operation"] != action {
				t.Fatalf("args = %#v, want the refused action", apiError["args"])
			}
		})
	}
	if calls := e.backend.called("Send"); len(calls) != 0 {
		t.Fatalf("a refused frame reached Herdr: %v", calls)
	}
}

func TestFrameIncompatibleProtocolReturnsTheReceipt(t *testing.T) {
	e := newFrameEnv(t)
	for name, test := range map[string]struct {
		protocol any
		received string
	}{
		"absent":    {nil, "invalid"},
		"too old":   {protocol.Version - 1, fmt.Sprint(protocol.Version - 1)},
		"too new":   {protocol.Version + 1, fmt.Sprint(protocol.Version + 1)},
		"zero":      {0, "invalid"},
		"negative":  {-1, "-1"},
		"far ahead": {protocol.Version + 10, fmt.Sprint(protocol.Version + 10)},
	} {
		t.Run(name, func(t *testing.T) {
			fields := map[string]any{"type": "send_keys", "keys": []string{"Enter"}, "action_id": "action-1", "protocol": test.protocol}
			response := e.exchange(t, fields)
			if response["type"] != "action_receipt" || response["request_id"] != "req-1" {
				t.Fatalf("response = %#v, want an action receipt echoing the request id", response)
			}
			receipt, _ := response["receipt"].(map[string]any)
			if receipt["action_id"] != "action-1" || receipt["phase"] != string(protocol.ReceiptFailedBeforeDispatch) {
				t.Fatalf("receipt = %#v", receipt)
			}
			apiError := apiErrorOf(t, response)
			args, _ := apiError["args"].(map[string]any)
			if apiError["code"] != protocol.ErrorIncompatibleProtocol || args["received"] != test.received ||
				args["required"] != fmt.Sprint(protocol.Version) {
				t.Fatalf("error = %#v", apiError)
			}
		})
	}
	if calls := e.backend.called("Send"); len(calls) != 0 {
		t.Fatalf("a refused frame reached Herdr: %v", calls)
	}

	t.Run("a read needs no protocol", func(t *testing.T) {
		response := e.exchange(t, map[string]any{"type": "get_activity", "protocol": nil})
		if response["type"] != "activity_history" {
			t.Fatalf("response = %#v, want the activity history", response)
		}
	})
}

func TestFrameUnknownServerSessionIsRefused(t *testing.T) {
	e := newFrameEnv(t)
	for name, fields := range map[string]map[string]any{
		"mutation":                  {"type": "send_keys", "keys": []string{"Enter"}, "server_session_id": "other"},
		"read":                      {"type": "read_pane", "server_session_id": "other"},
		"target alone":              {"type": "read_pane", "target": map[string]any{"server_session_id": "other", "pane_id": "pane-1"}},
		"session and target differ": {"type": "read_pane", "server_session_id": protocol.PrimaryServerSessionID, "target": map[string]any{"server_session_id": "other", "pane_id": "pane-1"}},
	} {
		t.Run(name, func(t *testing.T) {
			response := e.exchange(t, fields)
			if response["type"] != "error" || response["request_id"] != "req-1" {
				t.Fatalf("response = %#v, want an error echoing the request id", response)
			}
			apiError := apiErrorOf(t, response)
			if args, _ := apiError["args"].(map[string]any); apiError["code"] != protocol.ErrorInvalidRequest || args["field"] != "server_session_id" {
				t.Fatalf("error = %#v, want invalid_request on server_session_id", apiError)
			}
		})
	}
	if calls := e.backend.called("Send"); len(calls) != 0 {
		t.Fatalf("a refused frame reached Herdr: %v", calls)
	}
}
