package app

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/0cv/herdr-mobile-relay/internal/coordinator"
	"github.com/0cv/herdr-mobile-relay/internal/copyresponse"
	"github.com/0cv/herdr-mobile-relay/internal/protocol"
	"github.com/0cv/herdr-mobile-relay/internal/question"
	"github.com/0cv/herdr-mobile-relay/internal/slashcmd"
	"github.com/coder/websocket"
)

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
