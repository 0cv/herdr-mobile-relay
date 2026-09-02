package herdr

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"net"
	"path/filepath"
	"testing"
)

func TestSendInputUsesOfficialPasteAwareRequest(t *testing.T) {
	path := filepath.Join(t.TempDir(), "herdr.sock")
	listener, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	request := make(chan map[string]any, 1)
	go func() {
		conn, acceptErr := listener.Accept()
		if acceptErr != nil {
			return
		}
		defer conn.Close()
		line, readErr := bufio.NewReader(conn).ReadBytes('\n')
		if readErr != nil {
			return
		}
		var envelope map[string]any
		if json.Unmarshal(line, &envelope) != nil {
			return
		}
		request <- envelope
		_ = json.NewEncoder(conn).Encode(map[string]any{
			"id": envelope["id"], "result": map[string]any{"type": "ok"},
		})
	}()

	client := NewClient("unused", path)
	if err := client.SendInput(context.Background(), "pane-1", PaneInput{
		Text: "first\nsecond", Keys: []string{"shift+ctrl+Tab", "f12"},
	}); err != nil {
		t.Fatalf("SendInput() error = %v", err)
	}
	envelope := <-request
	if envelope["method"] != "pane.send_input" {
		t.Fatalf("method = %v", envelope["method"])
	}
	params := envelope["params"].(map[string]any)
	if params["pane_id"] != "pane-1" || params["text"] != "first\nsecond" {
		t.Fatalf("params = %#v", params)
	}
	keys := params["keys"].([]any)
	if keys[0] != "ctrl+shift+Tab" || keys[1] != "F12" {
		t.Fatalf("keys = %#v", keys)
	}
}

func TestSendInputWrittenStructuredErrorIsDispatchedUnknown(t *testing.T) {
	path := filepath.Join(t.TempDir(), "herdr.sock")
	listener, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	go func() {
		conn, acceptErr := listener.Accept()
		if acceptErr != nil {
			return
		}
		defer conn.Close()
		var request struct {
			ID string `json:"id"`
		}
		if json.NewDecoder(conn).Decode(&request) != nil {
			return
		}
		_ = json.NewEncoder(conn).Encode(map[string]any{
			"id": request.ID,
			"error": map[string]any{
				"code": "invalid_key", "message": "text was accepted before a key failed",
			},
		})
	}()

	client := NewClient("unused", path)
	err = client.SendInput(context.Background(), "pane-1", PaneInput{
		Text: "possibly applied", Keys: []string{"Enter"},
	})
	if !errors.Is(err, ErrDispatchedUnknown) || errors.Is(err, ErrNotStarted) {
		t.Fatalf("SendInput() error = %v, want dispatched unknown", err)
	}
	var cliErr *CLIError
	if !errors.As(err, &cliErr) || cliErr.Code != "invalid_key" {
		t.Fatalf("SendInput() error = %v, want structured CLI error", err)
	}
}

func TestSendInputRejectsUnsupportedKeysBeforeDispatch(t *testing.T) {
	for _, key := range []string{"Home", "End", "PageUp", "PageDown", "Delete", "Escape", "\\x1b[A", "meta+Enter", "ctrl+a", "F25"} {
		t.Run(key, func(t *testing.T) {
			client := NewClient("unused", filepath.Join(t.TempDir(), "absent.sock"))
			err := client.SendInput(context.Background(), "pane-1", PaneInput{Keys: []string{key}})
			if !errors.Is(err, ErrUnsupportedInputKey) {
				t.Fatalf("SendInput(%q) error = %v", key, err)
			}
			if errors.Is(err, ErrNotStarted) || errors.Is(err, ErrDispatchedUnknown) {
				t.Fatalf("unsupported key %q reached dispatch boundary: %v", key, err)
			}
		})
	}
}
