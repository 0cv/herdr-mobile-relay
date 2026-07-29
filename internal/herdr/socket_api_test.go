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
					"read": map[string]any{"text": fmt.Sprintf("frame %d", index)},
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
		if got, want := string(content), fmt.Sprintf("frame %d", index); got != want {
			t.Fatalf("read %d content = %q, want %q", index, got, want)
		}
	}
	if serverErr := <-serverResult; serverErr != nil {
		t.Fatal(serverErr)
	}
}
