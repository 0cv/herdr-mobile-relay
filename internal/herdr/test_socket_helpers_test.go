package herdr

import (
	"encoding/json"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSocketFixtureDirectoryUsesSelectedTempAndCleanup(t *testing.T) {
	var directory string
	t.Run(strings.Repeat("long-test-name-", 8), func(t *testing.T) {
		directory = testSocketDir(t)
		if filepath.Dir(directory) != os.TempDir() {
			t.Fatalf("socket directory %q is outside the selected temporary directory", directory)
		}
		info, err := os.Lstat(directory)
		if err != nil {
			t.Fatal(err)
		}
		if !info.IsDir() || info.Mode().Perm() != 0o700 {
			t.Fatalf("socket directory mode = %s", info.Mode())
		}
	})
	if _, err := os.Lstat(directory); !os.IsNotExist(err) {
		t.Fatalf("socket directory was not cleaned up: %v", err)
	}
}

func testSocketDir(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "s-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.RemoveAll(dir); err != nil {
			t.Error(err)
		}
	})
	return dir
}

func startUnaryTestSocket(t *testing.T, expectedMethod string, result any) (string, <-chan error) {
	t.Helper()
	socketPath := filepath.Join(testSocketDir(t), "herdr.sock")
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
