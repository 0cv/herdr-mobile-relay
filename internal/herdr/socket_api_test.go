package herdr

import (
	"context"
	"encoding/json"
	"errors"
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

func TestWorkspaceMoveUsesSocketAPI(t *testing.T) {
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
		var request struct {
			ID     string `json:"id"`
			Method string `json:"method"`
			Params struct {
				WorkspaceID string `json:"workspace_id"`
				InsertIndex int    `json:"insert_index"`
			} `json:"params"`
		}
		if decodeErr := json.NewDecoder(conn).Decode(&request); decodeErr != nil {
			serverResult <- decodeErr
			return
		}
		if request.Method != "workspace.move" || request.Params.WorkspaceID != "w2" ||
			request.Params.InsertIndex != 0 {
			serverResult <- fmt.Errorf("unexpected request: %+v", request)
			return
		}
		serverResult <- json.NewEncoder(conn).Encode(map[string]any{
			"id":     request.ID,
			"result": map[string]any{"type": "workspace_list"},
		})
	}()

	client := NewClient("/binary/must-not-run", socketPath)
	defer client.Close()
	if err := client.WorkspaceMove(context.Background(), "w2", 0); err != nil {
		t.Fatalf("WorkspaceMove() error = %v", err)
	}
	if serverErr := <-serverResult; serverErr != nil {
		t.Fatal(serverErr)
	}
}

func TestWorkspaceMoveBlockKeepsLinkedWorktreesTogether(t *testing.T) {
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
		var request struct {
			ID     string `json:"id"`
			Method string `json:"method"`
			Params struct {
				WorkspaceIDs      []string `json:"workspace_ids"`
				BeforeWorkspaceID string   `json:"before_workspace_id"`
			} `json:"params"`
		}
		if decodeErr := json.NewDecoder(conn).Decode(&request); decodeErr != nil {
			serverResult <- decodeErr
			return
		}
		if request.Method != "workspace.move_block" ||
			fmt.Sprint(request.Params.WorkspaceIDs) != "[w1 w2]" ||
			request.Params.BeforeWorkspaceID != "w5" {
			serverResult <- fmt.Errorf("unexpected request: %+v", request)
			return
		}
		serverResult <- json.NewEncoder(conn).Encode(map[string]any{
			"id":     request.ID,
			"result": map[string]any{"type": "workspace_list"},
		})
	}()

	client := NewClient("/binary/must-not-run", socketPath)
	defer client.Close()
	if err := client.WorkspaceMoveBlock(context.Background(), []string{"w1", "w2"}, "w5"); err != nil {
		t.Fatalf("WorkspaceMoveBlock() error = %v", err)
	}
	if serverErr := <-serverResult; serverErr != nil {
		t.Fatal(serverErr)
	}
}

// A move whose request was written but never answered may have applied, even
// when a retry then fails to connect. The final error must classify as
// dispatched-unknown, never as retry-safe.
func TestWorkspaceMoveWrittenButUnansweredIsDispatchedUnknown(t *testing.T) {
	socketPath := filepath.Join(t.TempDir(), "herdr.sock")
	listener, err := net.Listen("unix", socketPath)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer listener.Close()

	go func() {
		for {
			conn, acceptErr := listener.Accept()
			if acceptErr != nil {
				return
			}
			// Consume the request, then close without responding.
			buffer := make([]byte, socketAPIBufferBytes)
			_, _ = conn.Read(buffer)
			_ = conn.Close()
		}
	}()

	client := NewClient("/binary/must-not-run", socketPath)
	defer client.Close()
	err = client.WorkspaceMove(context.Background(), "w2", 0)
	if err == nil {
		t.Fatal("WorkspaceMove() succeeded against an unanswering server")
	}
	if !errors.Is(err, ErrDispatchedUnknown) {
		t.Fatalf("err = %v, want ErrDispatchedUnknown", err)
	}
	if errors.Is(err, ErrNotStarted) {
		t.Fatalf("err = %v classifies a possibly-applied move as retry-safe", err)
	}
}

func TestTabMoveWrittenButUnansweredIsDispatchedUnknown(t *testing.T) {
	socketPath := filepath.Join(t.TempDir(), "herdr.sock")
	listener, err := net.Listen("unix", socketPath)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer listener.Close()

	go func() {
		for {
			conn, acceptErr := listener.Accept()
			if acceptErr != nil {
				return
			}
			buffer := make([]byte, socketAPIBufferBytes)
			_, _ = conn.Read(buffer)
			_ = conn.Close()
		}
	}()

	client := NewClient("/binary/must-not-run", socketPath)
	defer client.Close()
	err = client.TabMove(context.Background(), "w1:t2", 0)
	if !errors.Is(err, ErrDispatchedUnknown) || errors.Is(err, ErrNotStarted) {
		t.Fatalf("err = %v, want ErrDispatchedUnknown and not ErrNotStarted", err)
	}
}

// A connect failure proves no request bytes reached Herdr: the move is
// retry-safe.
func TestWorkspaceMoveConnectFailureIsRetrySafe(t *testing.T) {
	client := NewClient("/binary/must-not-run", filepath.Join(t.TempDir(), "missing.sock"))
	defer client.Close()
	err := client.WorkspaceMove(context.Background(), "w2", 0)
	if !errors.Is(err, ErrNotStarted) {
		t.Fatalf("err = %v, want ErrNotStarted", err)
	}
	if errors.Is(err, ErrDispatchedUnknown) {
		t.Fatalf("err = %v classifies an unsent move as dispatched-unknown", err)
	}
}

// A structured Herdr error response is a definitive refusal: the move did not
// apply, and the code survives for phase classification.
func TestWorkspaceMoveRefusalKeepsStructuredCode(t *testing.T) {
	socketPath := filepath.Join(t.TempDir(), "herdr.sock")
	listener, err := net.Listen("unix", socketPath)
	if err != nil {
		t.Fatalf("listen: %v", err)
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
			"id":    request.ID,
			"error": map[string]any{"code": "workspace_not_found", "message": "no such workspace"},
		})
	}()

	client := NewClient("/binary/must-not-run", socketPath)
	defer client.Close()
	err = client.WorkspaceMove(context.Background(), "w9", 0)
	var cliErr *CLIError
	if !errors.As(err, &cliErr) || cliErr.Code != "workspace_not_found" {
		t.Fatalf("err = %v, want *CLIError with workspace_not_found", err)
	}
	if errors.Is(err, ErrDispatchedUnknown) {
		t.Fatalf("err = %v classifies a definitive refusal as dispatched-unknown", err)
	}
}
