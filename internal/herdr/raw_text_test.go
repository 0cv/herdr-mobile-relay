package herdr

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func rawSocketFixture(t *testing.T, respond func(net.Conn, string)) (string, <-chan string, *atomic.Int32) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "raw.sock")
	listener, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	requests := make(chan string, 8)
	var connections atomic.Int32
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			connections.Add(1)
			_ = conn.SetDeadline(time.Now().Add(time.Second))
			line, _ := bufio.NewReader(conn).ReadString('\n')
			requests <- line
			respond(conn, line)
			_ = conn.Close()
		}
	}()
	t.Cleanup(func() { _ = listener.Close(); <-done })
	return path, requests, &connections
}

func TestSendRawTextExactSocketPayload(t *testing.T) {
	text := "\x1b[<64;40;10M"
	path, requests, connections := rawSocketFixture(t, func(conn net.Conn, _ string) {
		_, _ = conn.Write([]byte("{\"id\":\"mobile-relay-api-1\",\"result\":{\"type\":\"ok\"}}\n"))
	})
	client := NewClient("must-not-execute-herdr-cli", path)
	defer client.Close()
	if err := client.SendRawText(context.Background(), "pane", text); err != nil {
		t.Fatal(err)
	}
	want := "{\"id\":\"mobile-relay-api-1\",\"method\":\"pane.send_text\",\"params\":{\"pane_id\":\"pane\",\"text\":\"\\u001b[\\u003c64;40;10M\"}}\n"
	if got := <-requests; got != want {
		t.Fatalf("request = %q, want %q", got, want)
	}
	if connections.Load() != 1 {
		t.Fatal("raw writer opened more than one connection")
	}
}

func TestSendRawTextPostWriteFailuresAreUnknownAndNeverRetried(t *testing.T) {
	for _, reply := range []string{
		"", "{\n", "{\"id\":\"wrong\",\"result\":{\"type\":\"ok\"}}\n",
		"{\"id\":\"mobile-relay-api-1\",\"result\":{\"type\":\"pane_read\"}}\n",
		"{\"id\":\"mobile-relay-api-1\",\"result\":null}\n",
		"{\"id\":\"mobile-relay-api-1\",\"error\":{\"code\":\"unknown_method\",\"message\":\"unsupported\"}}\n",
		"{\"id\":\"\",\"error\":{\"code\":\"invalid_request\",\"message\":\"invalid request: fixture\"}}\n",
	} {
		t.Run(reply, func(t *testing.T) {
			path, requests, connections := rawSocketFixture(t, func(conn net.Conn, _ string) { _, _ = conn.Write([]byte(reply)) })
			client := NewClient("must-not-execute-herdr-cli", path)
			defer client.Close()
			err := client.SendRawText(context.Background(), "pane", "\x1b[<65;40;10M")
			if !errors.Is(err, ErrDispatchedUnknown) || errors.Is(err, ErrNotStarted) {
				t.Fatalf("lost mutation boundary: %v", err)
			}
			if line := <-requests; !strings.Contains(line, "pane.send_text") || connections.Load() != 1 {
				t.Fatalf("unexpected request or retry: %q, %d", line, connections.Load())
			}
		})
	}
}

func TestSendRawTextLateReplyIsUnknown(t *testing.T) {
	path, requests, connections := rawSocketFixture(t, func(conn net.Conn, _ string) {
		time.Sleep(40 * time.Millisecond)
		_, _ = conn.Write([]byte("{\"id\":\"mobile-relay-api-1\",\"result\":{\"type\":\"ok\"}}\n"))
	})
	client := NewClient("must-not-execute-herdr-cli", path)
	defer client.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Millisecond)
	defer cancel()
	err := client.SendRawText(ctx, "pane", "\x1b[<64;1;1M")
	if !errors.Is(err, ErrDispatchedUnknown) || errors.Is(err, ErrNotStarted) {
		t.Fatalf("late acknowledgement = %v", err)
	}
	if <-requests == "" || connections.Load() != 1 {
		t.Fatal("fixture did not observe exactly one mutation attempt")
	}
}

func TestSendRawTextPreWriteRefusal(t *testing.T) {
	client := NewClient("must-not-execute-herdr-cli", filepath.Join(t.TempDir(), "missing.sock"))
	defer client.Close()
	if err := client.SendRawText(context.Background(), "pane", "x"); !errors.Is(err, ErrNotStarted) || errors.Is(err, ErrDispatchedUnknown) {
		t.Fatalf("connect error = %v", err)
	}
	path, requests, _ := rawSocketFixture(t, func(net.Conn, string) {})
	client = NewClient("must-not-execute-herdr-cli", path)
	defer client.Close()
	denied := errors.New("control revoked")
	var calls atomic.Int32
	ctx := WithDispatchCheck(context.Background(), func() error { calls.Add(1); return denied })
	err := client.SendRawText(ctx, "pane", "x")
	if !errors.Is(err, denied) || !errors.Is(err, ErrNotStarted) || calls.Load() != 1 {
		t.Fatalf("dispatch refusal = %v, calls = %d", err, calls.Load())
	}
	if line := <-requests; line != "" {
		t.Fatalf("denied write sent %q", line)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := client.SendRawText(ctx, "pane", "x"); !errors.Is(err, ErrNotStarted) {
		t.Fatalf("cancelled write = %v", err)
	}
}

func TestSendRawTextAdmissionFenceAfterDelayedConnect(t *testing.T) {
	path, requests, connections := rawSocketFixture(t, func(net.Conn, string) {})
	client := NewClient("must-not-execute-herdr-cli", path)
	defer client.Close()
	admission := time.Now().Add(10 * time.Millisecond)
	client.api.unaryDial = func(ctx context.Context, network, path string) (net.Conn, error) {
		conn, err := (&net.Dialer{}).DialContext(ctx, network, path)
		if err == nil {
			time.Sleep(time.Until(admission) + 5*time.Millisecond)
		}
		return conn, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	ctx = WithDispatchCheck(ctx, func() error {
		if !time.Now().Before(admission) {
			return context.DeadlineExceeded
		}
		return nil
	})
	err := client.SendRawText(ctx, "pane", "\x1b[<64;1;1M")
	if !errors.Is(err, ErrNotStarted) || !errors.Is(err, context.DeadlineExceeded) || errors.Is(err, ErrDispatchedUnknown) {
		t.Fatalf("late connect allowed a mutation: %v", err)
	}
	if line := <-requests; line != "" || connections.Load() != 1 {
		t.Fatalf("expired admission wrote request bytes: %q", line)
	}
}

func TestSendRawTextValidation(t *testing.T) {
	client := NewClient("must-not-execute-herdr-cli", "")
	defer client.Close()
	for _, input := range [][2]string{{"", "x"}, {"pane", ""}, {"pane", strings.Repeat("x", MaxPaneInputTextBytes+1)}} {
		if err := client.SendRawText(context.Background(), input[0], input[1]); !errors.Is(err, ErrNotStarted) || !errors.Is(err, ErrInvalidPaneInput) {
			t.Fatalf("invalid raw input = %v", err)
		}
	}
}

func TestSocketPaneProcessInfo(t *testing.T) {
	for _, value := range []any{
		map[string]any{"type": "pane_process_info", "process_info": map[string]any{"pane_id": "pane", "shell_pid": 1, "foreground_process_group_id": 2, "foreground_processes": []any{map[string]any{"pid": 2, "name": "pi"}}}},
		map[string]any{"type": "pane_process_info"},
		map[string]any{"type": "pane_process_info", "process_info": map[string]any{"pane_id": "other"}},
		map[string]any{"type": "ok"},
	} {
		t.Run("response", func(t *testing.T) {
			path, requests, connections := rawSocketFixture(t, func(conn net.Conn, line string) {
				var request struct {
					ID string `json:"id"`
				}
				_ = json.Unmarshal([]byte(line), &request)
				_ = json.NewEncoder(conn).Encode(map[string]any{"id": request.ID, "result": value})
			})
			client := NewClient("must-not-execute-herdr-cli", path)
			defer client.Close()
			info, err := client.SocketPaneProcessInfo(context.Background(), "pane")
			wantOK := value.(map[string]any)["process_info"] != nil && value.(map[string]any)["process_info"].(map[string]any)["pane_id"] == "pane"
			if wantOK != (err == nil) {
				t.Fatalf("process response: %+v, %v", info, err)
			}
			if wantOK && (info.ForegroundProcessGroupID != 2 || info.ForegroundProcesses[0].PID != 2) {
				t.Fatalf("process model = %+v", info)
			}
			if line := <-requests; line != "{\"id\":\"mobile-relay-api-1\",\"method\":\"pane.process_info\",\"params\":{\"pane_id\":\"pane\"}}\n" || connections.Load() != 1 {
				t.Fatalf("process request = %q, count = %d", line, connections.Load())
			}
		})
	}
}

func TestSocketPaneProcessInfoReadFailureHasNoMutationOutcome(t *testing.T) {
	path, _, _ := rawSocketFixture(t, func(net.Conn, string) {})
	client := NewClient("must-not-execute-herdr-cli", path)
	defer client.Close()
	_, err := client.SocketPaneProcessInfo(context.Background(), "pane")
	if err == nil || errors.Is(err, ErrDispatchedUnknown) {
		t.Fatalf("read failure misclassified as wheel mutation: %v", err)
	}
}

func TestDisposableRawTextSocket(t *testing.T) {
	path, pane := os.Getenv("HERDR_RAW_FIXTURE_SOCKET"), os.Getenv("HERDR_RAW_FIXTURE_PANE")
	if path == "" || pane == "" {
		t.Skip("requires an isolated raw PTY fixture")
	}
	root := filepath.Dir(path)
	marker, err := os.ReadFile(filepath.Join(root, "fixture-owner"))
	if !strings.HasPrefix(root, "/tmp/herdr-scroll-p1-") || err != nil || string(marker) != "disposable-scroll-fixture\n" {
		t.Fatal("refusing an unmarked or non-disposable socket")
	}
	client := NewClient("must-not-execute-herdr-cli", path)
	defer client.Close()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	info, err := client.SocketPaneProcessInfo(ctx, pane)
	if err != nil || info.ForegroundProcessGroupID <= 0 {
		t.Fatalf("fixture process: %+v, %v", info, err)
	}
	for _, text := range []string{"\x1b[<64;40;10M", "\x1b[<65;40;10M"} {
		if err := client.SendRawText(ctx, pane, text); err != nil {
			t.Fatal(err)
		}
	}
}
