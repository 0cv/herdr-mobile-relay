package coordinator

import (
	"bufio"
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
)

type socketRequestLog struct {
	mu      sync.Mutex
	methods []string
}

func (l *socketRequestLog) add(method string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.methods = append(l.methods, method)
}

func (l *socketRequestLog) recorded() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return slices.Clone(l.methods)
}

type socketFixtureOption func(*socketFixtureConfig)

type socketFixtureConfig struct {
	extraLines func(method string) []any
	log        *socketRequestLog
}

func withExtraLines(extraLines func(method string) []any) socketFixtureOption {
	return func(config *socketFixtureConfig) { config.extraLines = extraLines }
}

func withRequestLog(log *socketRequestLog) socketFixtureOption {
	return func(config *socketFixtureConfig) { config.log = log }
}

// startSocketFixture serves one JSON request per connection on a Unix socket.
// The responder runs on a single goroutine, so it may keep unsynchronised counters.
func startSocketFixture(t *testing.T, responder func(method string) (result any, errorCode string), options ...socketFixtureOption) string {
	t.Helper()
	var config socketFixtureConfig
	for _, option := range options {
		option(&config)
	}
	socketPath := filepath.Join(testSocketDir(t), "s")
	listener, err := net.Listen("unix", socketPath)
	if err != nil {
		t.Fatalf("listen socket fixture: %v", err)
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			conn, acceptErr := listener.Accept()
			if acceptErr != nil {
				return
			}
			func() {
				defer conn.Close()
				request := struct {
					ID     string `json:"id"`
					Method string `json:"method"`
				}{}
				if err := json.NewDecoder(bufio.NewReader(conn)).Decode(&request); err != nil {
					return
				}
				if config.log != nil {
					config.log.add(request.Method)
				}
				result, errorCode := responder(request.Method)
				encoder := json.NewEncoder(conn)
				if errorCode != "" {
					_ = encoder.Encode(map[string]any{
						"id": request.ID,
						"error": map[string]string{
							"code": errorCode, "message": errorCode,
						},
					})
					return
				}
				_ = encoder.Encode(map[string]any{"id": request.ID, "result": result})
				if config.extraLines == nil {
					return
				}
				for _, line := range config.extraLines(request.Method) {
					_ = encoder.Encode(line)
				}
			}()
		}
	}()
	t.Cleanup(func() {
		_ = listener.Close()
		<-done
	})
	return socketPath
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

func TestSocketFixtureDirectoryUsesSelectedTempAndCleanup(t *testing.T) {
	var directory string
	t.Run(strings.Repeat("long-test-name-", 8), func(t *testing.T) {
		directory = testSocketDir(t)
		if filepath.Dir(directory) != filepath.Clean(os.TempDir()) {
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
