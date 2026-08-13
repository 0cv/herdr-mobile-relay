package herdr

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"path/filepath"
	"testing"
)

func TestReadPaneReusesSocketAPIConnection(t *testing.T) {
	socketPath := filepath.Join(t.TempDir(), "herdr.sock")
	listener, err := net.Listen("unix", socketPath)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer listener.Close()

	serverResult := make(chan error, 1)
	go func() {
		conn, acceptErr := listener.Accept()
		if acceptErr != nil {
			serverResult <- acceptErr
			return
		}
		defer conn.Close()
		decoder := json.NewDecoder(conn)
		encoder := json.NewEncoder(conn)
		for index := 1; index <= 2; index++ {
			var request struct {
				ID     string `json:"id"`
				Method string `json:"method"`
				Params struct {
					PaneID string `json:"pane_id"`
					Source string `json:"source"`
					Lines  int    `json:"lines"`
					Format string `json:"format"`
				} `json:"params"`
			}
			if decodeErr := decoder.Decode(&request); decodeErr != nil {
				serverResult <- decodeErr
				return
			}
			if request.Method != "pane.read" || request.Params.PaneID != "w1:p1" ||
				request.Params.Source != "recent_unwrapped" || request.Params.Lines != 80 ||
				request.Params.Format != "ansi" {
				serverResult <- fmt.Errorf("unexpected request: %+v", request)
				return
			}
			response := map[string]any{
				"id": request.ID,
				"result": map[string]any{
					"type": "pane_read",
					"read": map[string]any{
						"text":      fmt.Sprintf("frame %d", index),
						"truncated": index == 1,
					},
				},
			}
			if encodeErr := encoder.Encode(response); encodeErr != nil {
				serverResult <- encodeErr
				return
			}
		}
		serverResult <- nil
	}()

	client := NewClient("/binary/must-not-run", socketPath)
	if !client.SupportsRealtimePane(context.Background()) {
		t.Fatal("socket API was not detected")
	}
	defer client.Close()
	for index := 1; index <= 2; index++ {
		content, readErr := client.ReadPane(context.Background(), "w1:p1", 80, "ansi")
		if readErr != nil {
			t.Fatalf("read %d: %v", index, readErr)
		}
		if got, want := string(content.Content), fmt.Sprintf("frame %d", index); got != want {
			t.Fatalf("read %d content = %q, want %q", index, got, want)
		}
		if got, want := content.Truncated, index == 1; got != want {
			t.Fatalf("read %d truncated = %v, want %v", index, got, want)
		}
	}
	if serverErr := <-serverResult; serverErr != nil {
		t.Fatal(serverErr)
	}
}

// Herdr 0.8.0 closes the API socket after every response. A connection cached
// by an earlier request must not surface as a failed tab move.
func TestTabMoveRetriesWhenServerClosesEachConnection(t *testing.T) {
	socketPath := filepath.Join(t.TempDir(), "herdr.sock")
	listener, err := net.Listen("unix", socketPath)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer listener.Close()

	requests := make(chan string, 8)
	go func() {
		for {
			conn, acceptErr := listener.Accept()
			if acceptErr != nil {
				return
			}
			var request struct {
				ID     string `json:"id"`
				Method string `json:"method"`
				Params struct {
					TabID       string `json:"tab_id"`
					InsertIndex int    `json:"insert_index"`
				} `json:"params"`
			}
			if json.NewDecoder(conn).Decode(&request) == nil && request.Method != "" {
				requests <- fmt.Sprintf("%s %s %d", request.Method, request.Params.TabID, request.Params.InsertIndex)
				_ = json.NewEncoder(conn).Encode(map[string]any{
					"id":     request.ID,
					"result": map[string]any{"type": "tab_list"},
				})
			}
			_ = conn.Close()
		}
	}()

	client := NewClient("/binary/must-not-run", socketPath)
	defer client.Close()
	if !client.SupportsRealtimePane(context.Background()) {
		t.Fatal("socket API was not detected")
	}
	if err := client.TabMove(context.Background(), "w1:t2", 0); err != nil {
		t.Fatalf("first move: %v", err)
	}
	if err := client.TabMove(context.Background(), "w1:t3", 2); err != nil {
		t.Fatalf("second move: %v", err)
	}
	close(requests)
	var seen []string
	for request := range requests {
		seen = append(seen, request)
	}
	want := []string{"tab.move w1:t2 0", "tab.move w1:t3 2"}
	if len(seen) != len(want) || seen[0] != want[0] || seen[1] != want[1] {
		t.Fatalf("requests = %v, want %v", seen, want)
	}
}
