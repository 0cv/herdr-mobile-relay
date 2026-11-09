package herdr

import (
	"context"
	"encoding/json"
	"testing"
)

func TestTopologyStreamYieldsChangedBufferedSnapshots(t *testing.T) {
	stream := &topologyStream{
		cache: NewSessionCache(SessionSnapshot{Panes: []SnapshotPane{{ID: "pane-1", TerminalID: "terminal-1", Status: "working"}}}),
		buffered: []Event{
			{Event: "pane.output_changed", Data: json.RawMessage(`{"pane_id":"pane-1"}`)},
			{Event: "pane.agent_detected", Data: json.RawMessage(`{"pane_id":"pane-1","agent":"codex","final_status":"idle"}`)},
			{Event: "pane.updated", Data: json.RawMessage(`{"pane":{"pane_id":"pane-1","terminal_id":"terminal-2","agent_status":"idle"}}`)},
		},
	}
	snapshot, err := stream.Next(context.Background())
	if err != nil || len(snapshot.Panes) != 1 || snapshot.Panes[0].Status != "idle" || snapshot.Panes[0].TerminalID != "terminal-1" {
		t.Fatalf("status snapshot = %+v, err = %v", snapshot, err)
	}
	snapshot, err = stream.Next(context.Background())
	if err != nil || len(snapshot.Panes) != 1 || snapshot.Panes[0].TerminalID != "terminal-2" {
		t.Fatalf("replacement snapshot = %+v, err = %v", snapshot, err)
	}
}

func TestTopologyStreamReturnsBufferedDecodeError(t *testing.T) {
	stream := &topologyStream{
		cache:    NewSessionCache(SessionSnapshot{}),
		buffered: []Event{{Event: "pane.updated", Data: json.RawMessage(`{`)}},
	}
	if _, err := stream.Next(context.Background()); err == nil {
		t.Fatal("expected decode error")
	}
}
