package herdr

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"net"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestValidateFilterText(t *testing.T) {
	for _, text := range []string{"grok", " GrOk 22.-_/+:()[]", strings.Repeat("g", 32)} {
		if err := ValidateFilterText(text); err != nil {
			t.Errorf("%q: %v", text, err)
		}
	}
	for _, text := range []string{"", " ", "g ", "g\n", "g\r", "g\t", "g\x1b", "g😀", "g中", "g\\", strings.Repeat("g", 33)} {
		if err := ValidateFilterText(text); err == nil {
			t.Errorf("accepted %q", text)
		}
	}
}

func TestLiteralKeyNames(t *testing.T) {
	for _, character := range "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789.-_/+:()[] " {
		value := string(character)
		want := value
		if value == " " {
			want = "Space"
		}
		got, err := literalKeyName(value)
		if err != nil || got != want {
			t.Fatalf("literalKeyName(%q) = %q, %v", value, got, err)
		}
	}
	for _, value := range []string{"", "grok", "Enter", "Space", "ctrl+c", "\n", "\r", "\t", "\x1b", "\x00", "\x7f", "é", "中", "😀", "\xff", "\\"} {
		if _, err := literalKeyName(value); !errors.Is(err, ErrUnsupportedLiteralKey) {
			t.Errorf("literalKeyName(%q) = %v", value, err)
		}
	}
}

func TestSendLiteralKeySeparateRequests(t *testing.T) {
	path := filepath.Join(t.TempDir(), "herdr.sock")
	listener, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	const input = "GrOk22 .-_/+:()[]"
	requests := make(chan map[string]any, len(input))
	done := make(chan error, 1)
	go func() {
		for range input {
			conn, err := listener.Accept()
			if err != nil {
				done <- err
				return
			}
			_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
			line, err := bufio.NewReader(conn).ReadBytes('\n')
			if err != nil {
				conn.Close()
				done <- err
				return
			}
			var request map[string]any
			if err := json.Unmarshal(line, &request); err != nil {
				conn.Close()
				done <- err
				return
			}
			requests <- request
			err = json.NewEncoder(conn).Encode(map[string]any{"id": request["id"], "result": map[string]any{"type": "ok"}})
			conn.Close()
			if err != nil {
				done <- err
				return
			}
		}
		done <- nil
	}()
	client := NewClient("unused", path)
	defer client.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	for _, character := range input {
		if err := client.SendLiteralKey(ctx, "pane-1", string(character)); err != nil {
			t.Fatal(err)
		}
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	var accepted strings.Builder
	for range input {
		envelope := <-requests
		if envelope["method"] != "pane.send_keys" {
			t.Fatalf("method = %v", envelope["method"])
		}
		params := envelope["params"].(map[string]any)
		keys := params["keys"].([]any)
		if params["pane_id"] != "pane-1" || len(params) != 2 || len(keys) != 1 {
			t.Fatalf("params = %#v", params)
		}
		key := keys[0].(string)
		if key == "Space" {
			key = " "
		}
		if len(key) == 1 {
			accepted.WriteString(key)
		}
	}
	if accepted.String() != input {
		t.Fatalf("accepted = %q, want %q", accepted.String(), input)
	}
}

func TestSendLiteralKeyRejectsBeforeConnection(t *testing.T) {
	client := NewClient("unused", filepath.Join(t.TempDir(), "absent.sock"))
	for _, value := range []string{"Enter", "grok", "ctrl+c", "\n", "😀"} {
		if err := client.SendLiteralKey(context.Background(), "pane-1", value); !errors.Is(err, ErrUnsupportedLiteralKey) {
			t.Fatalf("SendLiteralKey(%q) = %v", value, err)
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := client.SendLiteralKey(ctx, "pane-1", "g"); !errors.Is(err, ErrNotStarted) || !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled request = %v", err)
	}
	if err := client.SendLiteralKey(context.Background(), "pane-1", "g"); !errors.Is(err, ErrNotStarted) || errors.Is(err, ErrDispatchedUnknown) {
		t.Fatalf("absent socket = %v", err)
	}
}

func TestSendLiteralKeyLostAcknowledgementDoesNotRetry(t *testing.T) {
	path := filepath.Join(t.TempDir(), "herdr.sock")
	listener, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	requests := make(chan string, 1)
	go func() {
		conn, err := listener.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
		line, _ := bufio.NewReader(conn).ReadString('\n')
		requests <- line
	}()
	client := NewClient("unused", path)
	defer client.Close()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	err = client.SendLiteralKey(ctx, "pane-1", "g")
	if !errors.Is(err, ErrDispatchedUnknown) || errors.Is(err, ErrNotStarted) || errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("lost acknowledgement = %v", err)
	}
	if line := <-requests; !strings.Contains(line, "pane.send_keys") {
		t.Fatalf("request = %q", line)
	}
	if err := listener.(*net.UnixListener).SetDeadline(time.Now()); err != nil {
		t.Fatal(err)
	}
	if conn, err := listener.Accept(); err == nil {
		conn.Close()
		t.Fatal("writer retried an uncertain request")
	}
}
