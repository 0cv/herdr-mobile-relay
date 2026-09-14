package herdr

import (
	"encoding/json"
	"fmt"
	"net"
	"path/filepath"
	"testing"
)

func startUnaryTestSocket(t *testing.T, expectedMethod string, result any) (string, <-chan error) {
	t.Helper()
	socketPath := filepath.Join(t.TempDir(), "herdr.sock")
	listener, err := net.Listen("unix", socketPath)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	done := make(chan error, 1)
	go func() {
		conn, acceptErr := listener.Accept()
		if acceptErr != nil {
			done <- acceptErr
			return
		}
		defer conn.Close()
		defer listener.Close()
		var request struct {
			ID     string `json:"id"`
			Method string `json:"method"`
		}
		if err := json.NewDecoder(conn).Decode(&request); err != nil {
			done <- err
			return
		}
		if request.Method != expectedMethod {
			done <- fmt.Errorf("method = %q, want %q", request.Method, expectedMethod)
			return
		}
		done <- json.NewEncoder(conn).Encode(map[string]any{
			"id":     request.ID,
			"result": result,
		})
	}()
	return socketPath, done
}
