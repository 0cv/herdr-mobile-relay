package coordinator

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/0cv/herdr-mobile-relay/internal/herdr"
)

func TestManualTerminalNativeFixture(t *testing.T) {
	work := os.Getenv("HERDR_MANUAL_WORK")
	if work == "" {
		t.Skip("requires an explicitly launched private native text dialog")
	}
	if os.Getenv("HOME") != filepath.Join(work, "home") {
		t.Fatal("private HOME required")
	}
	client := herdr.NewClient(filepath.Join(work, "fake-herdr"), filepath.Join(work, "herdr.sock"))
	defer client.Close()
	state := NewState(testLogger())
	state.CommitInventory([]*AgentState{{PaneID: "native-dialog", TerminalID: "native-terminal", Agent: "pi", Status: "blocked", AttentionKind: "unknown"}}, state.RevisionCounter())
	d := NewDispatcher(client, state, nil, testLogger())
	defer d.Close(context.Background())
	text := "  /literal-command 界😀  "
	result := d.Handle(context.Background(), map[string]any{"action": "send_terminal_text", "request_id": "native-text", "pane_id": "native-dialog", "text": text})
	if !result.OK {
		t.Fatalf("native text: %+v", result)
	}
	if err := os.WriteFile(filepath.Join(work, "text-delivered"), []byte("ok"), 0600); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		if _, err := os.Stat(filepath.Join(work, "enter-ready")); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("explicit Enter was not requested")
		}
		time.Sleep(10 * time.Millisecond)
	}
	result = d.Handle(context.Background(), map[string]any{"action": "send_keys", "request_id": "native-enter", "pane_id": "native-dialog", "keys": []any{"Enter"}})
	if !result.OK {
		t.Fatalf("native Enter: %+v", result)
	}
}

func TestTerminalTextPolicy(t *testing.T) {
	for _, text := range []string{"  /arbitrary 界😀  ", strings.Repeat(" ", 65536), strings.Repeat("界", 21845) + "x"} {
		if !validTerminalText(text) {
			t.Fatalf("rejected valid text (%d bytes)", len(text))
		}
	}
	for _, text := range []string{"", "a\n", "a\r", "a\t", "a\x1b", "\x00", "\x7f", "\u0085", "\u2028", "\u2029", "\xff", strings.Repeat("界", 21846)} {
		if validTerminalText(text) {
			t.Fatalf("accepted invalid text (%d bytes)", len(text))
		}
	}
}

func TestTerminalTextLiteralDispatchAndIdempotency(t *testing.T) {
	d, fake := newControlledFilterDispatcher(t)
	before, _ := d.state.Agent("pane-1")
	text := "  /arbitrary 界😀  "
	message := map[string]any{"action": "send_terminal_text", "request_id": "manual-1", "pane_id": "pane-1", "text": text}
	for attempt := 0; attempt < 2; attempt++ {
		result := d.Handle(context.Background(), message)
		if !result.OK || result.Phase != "completed" {
			t.Fatalf("result: %+v", result)
		}
	}
	want := []filterSocketWrite{{"pane-1", "pane.send_input", text}}
	if got := fake.recordedWrites(); !reflect.DeepEqual(got, want) {
		t.Fatalf("writes = %#v", got)
	}
	after, _ := d.state.Agent("pane-1")
	if !reflect.DeepEqual(before, after) {
		t.Fatal("delivery changed observed agent state")
	}
	message["text"] = "different"
	if result := d.Handle(context.Background(), message); result.OK {
		t.Fatal("reused request accepted different text")
	}
	if len(fake.recordedWrites()) != 1 {
		t.Fatal("duplicate native mutation")
	}
}

func TestTerminalTextLedgerUsesKeyedNonReversibleConflictToken(t *testing.T) {
	d, fake := newControlledFilterDispatcher(t)
	other, _ := newControlledFilterDispatcher(t)
	text := "private low-entropy answer"
	token := d.terminalTextConflictToken(text)
	decoded, err := hex.DecodeString(token)
	if err != nil || len(decoded) != sha256.Size || strings.Contains(string(decoded), text) {
		t.Fatal("conflict token is not a fixed-size digest")
	}
	unkeyed := sha256.Sum256([]byte(text))
	if token == hashPayload(text) || token == hex.EncodeToString(unkeyed[:]) {
		t.Fatal("conflict token exposes reversible or unkeyed text encoding")
	}
	if token != d.terminalTextConflictToken(text) || token == d.terminalTextConflictToken(text+" changed") || token == other.terminalTextConflictToken(text) {
		t.Fatal("conflict token does not preserve per-dispatcher equality and conflict semantics")
	}
	message := map[string]any{"action": "send_terminal_text", "request_id": "private-ledger", "pane_id": "pane-1", "text": text}
	if result := d.Handle(context.Background(), message); !result.OK {
		t.Fatalf("dispatch: %+v", result)
	}
	key := "send_terminal_text\x00pane-1\x00private-ledger"
	result, found, err := d.scheduler.ReplayLedger(key, token)
	if err != nil || !found || result == nil || !result.OK {
		t.Fatal("ledger did not retain the keyed conflict token")
	}
	if _, _, err := d.scheduler.ReplayLedger(key, hashPayload(text)); !errors.Is(err, ErrConflict) {
		t.Fatal("ledger accepted the reversible text encoding")
	}
	if len(fake.recordedWrites()) != 1 {
		t.Fatal("ledger query replayed terminal input")
	}
}

func TestTerminalTextUncertainDeliveryIsNotReplayed(t *testing.T) {
	d, fake := newControlledFilterDispatcher(t)
	fake.dropInputReply = true
	message := map[string]any{"action": "send_terminal_text", "request_id": "lost-text", "pane_id": "pane-1", "text": "private text"}
	for attempt := 0; attempt < 2; attempt++ {
		result := d.Handle(context.Background(), message)
		if result.OK || result.Phase != "dispatched_unknown" {
			t.Fatalf("unsafe receipt: %+v", result)
		}
	}
	if len(fake.recordedWrites()) != 1 {
		t.Fatal("uncertain text replayed")
	}
}

func TestTerminalTextRechecksDispatchAuthorization(t *testing.T) {
	d, fake := newControlledFilterDispatcher(t)
	ctx := herdr.WithDispatchCheck(context.Background(), func() error { return errors.New("revoked") })
	result := d.Handle(ctx, map[string]any{"action": "send_terminal_text", "pane_id": "pane-1", "text": "private text"})
	if result.OK || len(fake.recordedWrites()) != 0 {
		t.Fatalf("revoked mutation: %+v", result)
	}
}

func TestTerminalTextRejectsControlsAndKeysBeforeDispatch(t *testing.T) {
	d, fake := newControlledFilterDispatcher(t)
	for _, text := range []string{"\n", "\r", "\t", "\x1b[200~", "\u0085", strings.Repeat("x", 65537)} {
		if result := d.Handle(context.Background(), map[string]any{"action": "send_terminal_text", "pane_id": "pane-1", "text": text}); result.OK {
			t.Fatal("invalid text dispatched")
		}
	}
	if result := d.Handle(context.Background(), map[string]any{"action": "send_terminal_text", "pane_id": "pane-1", "text": "text", "keys": []any{"Enter"}}); result.OK {
		t.Fatal("implicit Enter allowed")
	}
	if len(fake.recordedWrites()) != 0 {
		t.Fatal("unexpected mutation")
	}
}
