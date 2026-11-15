package coordinator

// Tests for the no-echo secret path. A secret must reach the pane as a single
// secret send (no bracketed paste, no partial submission) and must never
// appear in the activity journal.

import (
	"context"
	"encoding/json"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/0cv/herdr-mobile-relay/internal/activity"
)

func TestSendSecretDispatchesOneKeystrokeBatch(t *testing.T) {
	client := newFakeBackend()
	client.contents["pane-1"] = "Password:"
	state := NewState(testLogger())
	state.CommitInventory(
		[]*AgentState{{PaneID: "pane-1", Agent: "omp", Status: "working"}},
		state.RevisionCounter(),
	)
	journal, err := activity.OpenJournal(filepath.Join(t.TempDir(), "cache"))
	if err != nil {
		t.Fatal(err)
	}
	d := NewDispatcher(client, state, journal, testLogger())
	t.Cleanup(func() {
		_ = d.Close(context.Background())
	})

	result := d.Handle(context.Background(), map[string]any{
		"type": "send_secret", "request_id": "secret-1", "pane_id": "pane-1",
		"text": "hunter 2!",
	})
	if !result.OK || result.Action != "send_secret" || result.Phase != "completed" {
		t.Fatalf("send_secret result = %+v", result)
	}

	want := []fakeCall{{Method: "SendSecret", Args: []any{"pane-1", "hunter 2!"}}}
	if got := client.callsTo("SendSecret"); !reflect.DeepEqual(got, want) {
		t.Fatalf("SendSecret calls = %+v, want %+v", got, want)
	}
	for _, call := range client.recorded() {
		if call.Method != "ReadPaneVisible" && call.Method != "SendSecret" {
			t.Fatalf("send_secret made an unexpected Herdr call: %+v", call)
		}
	}

	recent := journal.Recent(5)
	if len(recent) != 1 {
		t.Fatalf("journal holds %d entries, want 1: %+v", len(recent), recent)
	}
	if recent[0].Summary != "Password entered" || recent[0].Extract != "" {
		t.Fatalf("secret activity = %+v", recent[0])
	}
	encoded, err := json.Marshal(recent[0])
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), "hunter") {
		t.Fatalf("activity journal retained secret material: %s", encoded)
	}
}

func TestSendSecretRejectsUnsendableSecrets(t *testing.T) {
	tests := []struct {
		name string
		text string
	}{
		{name: "empty", text: ""},
		{name: "control rune", text: "pass\nword"},
		{name: "delete rune", text: "pass\x7fword"},
		{name: "escape rune", text: "pass\x1bword"},
		{name: "too long", text: strings.Repeat("x", 257)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			client := newFakeBackend()
			client.contents["pane-1"] = "Password:"
			state := NewState(testLogger())
			state.CommitInventory(
				[]*AgentState{{PaneID: "pane-1", Agent: "omp", Status: "working"}},
				state.RevisionCounter(),
			)
			d := newTestDispatcher(t, client, state)

			result := d.Handle(context.Background(), map[string]any{
				"type": "send_secret", "request_id": "secret-1", "pane_id": "pane-1",
				"text": tt.text,
			})
			if result.OK {
				t.Fatalf("send_secret accepted %q: %+v", tt.name, result)
			}
			if calls := client.recorded(); len(calls) != 0 {
				t.Fatalf("rejected secret still reached Herdr: %+v", calls)
			}
		})
	}
}
