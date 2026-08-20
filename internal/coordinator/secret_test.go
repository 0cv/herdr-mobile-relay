package coordinator

// Tests for the no-echo secret path. A secret must reach the pane as a single
// keystroke batch (no bracketed paste, no partial submission) and must never
// appear in the activity journal.
// Helpers testLogger/writeScript live in safety_test.go and
// dispatch_safety_test.go.

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/0cv/herdr-mobile-relay/internal/activity"
	"github.com/0cv/herdr-mobile-relay/internal/herdr"
)

func recordingSecretHerdr(t *testing.T, dir, record string) string {
	t.Helper()
	return writeScript(t, dir, "herdr", "#!/bin/sh\n"+
		"{ printf '%s\\n' \"$@\"; echo '--'; } >> \""+record+"\"\n"+
		"printf '{\"ok\":true}\\n'\n")
}

func recordedInvocations(t *testing.T, record string) [][]string {
	t.Helper()
	data, err := os.ReadFile(record)
	if err != nil {
		t.Fatalf("read invocations: %v", err)
	}
	invocations := make([][]string, 0, 2)
	for _, block := range strings.Split(string(data), "--\n") {
		if block == "" {
			continue
		}
		invocations = append(invocations, strings.Split(strings.TrimSuffix(block, "\n"), "\n"))
	}
	return invocations
}

func TestSendSecretDispatchesOneKeystrokeBatch(t *testing.T) {
	dir := t.TempDir()
	record := filepath.Join(dir, "invocations.log")
	bin := recordingSecretHerdr(t, dir, record)
	state := NewState(testLogger())
	state.CommitInventory(
		[]*AgentState{{PaneID: "pane-1", Agent: "omp", Status: "working"}},
		state.RevisionCounter(),
	)
	journal, err := activity.OpenJournal(filepath.Join(dir, "cache"))
	if err != nil {
		t.Fatal(err)
	}
	d := NewDispatcher(herdr.NewClient(bin, filepath.Join(dir, "sock")), state, journal, testLogger())
	t.Cleanup(func() {
		_ = d.Close(context.Background())
	})

	result := d.Handle(context.Background(), map[string]any{
		"action": "send_secret", "request_id": "secret-1", "pane_id": "pane-1",
		"text": "hunter 2!",
	})
	if !result.OK || result.Action != "send_secret" || result.Phase != "completed" {
		t.Fatalf("send_secret result = %+v", result)
	}

	invocations := recordedInvocations(t, record)
	if len(invocations) != 1 {
		t.Fatalf("send_secret ran %d Herdr invocations, want 1: %#v", len(invocations), invocations)
	}
	want := []string{
		"pane", "send-keys", "pane-1",
		"h", "u", "n", "t", "e", "r", " ", "2", "!", "Enter",
	}
	if !reflect.DeepEqual(invocations[0], want) {
		t.Fatalf("Herdr arguments = %#v, want %#v", invocations[0], want)
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
			dir := t.TempDir()
			record := filepath.Join(dir, "invocations.log")
			bin := recordingSecretHerdr(t, dir, record)
			state := NewState(testLogger())
			state.CommitInventory(
				[]*AgentState{{PaneID: "pane-1", Agent: "omp", Status: "working"}},
				state.RevisionCounter(),
			)
			d := NewDispatcher(herdr.NewClient(bin, filepath.Join(dir, "sock")), state, nil, testLogger())
			t.Cleanup(func() {
				_ = d.Close(context.Background())
			})

			result := d.Handle(context.Background(), map[string]any{
				"action": "send_secret", "request_id": "secret-1", "pane_id": "pane-1",
				"text": tt.text,
			})
			if result.OK {
				t.Fatalf("send_secret accepted %q: %+v", tt.name, result)
			}
			if _, err := os.Stat(record); !os.IsNotExist(err) {
				t.Fatalf("rejected secret still reached Herdr: %#v", recordedInvocations(t, record))
			}
		})
	}
}
