package hc

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/0cv/herdr-mobile-relay/internal/herdr"
)

func socketDir(t *testing.T) string {
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

type fakeRequest struct {
	JSONRPC string         `json:"jsonrpc"`
	ID      string         `json:"id"`
	Method  string         `json:"method"`
	Params  map[string]any `json:"params"`
}

func fakeCore(t *testing.T, generation int, respond func(net.Conn, fakeRequest)) string {
	t.Helper()
	path := filepath.Join(socketDir(t), "c.sock")
	listener, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	t.Cleanup(func() { listener.Close(); <-done })
	go func() {
		defer close(done)
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			func() {
				keep := false
				defer func() {
					if !keep {
						conn.Close()
					}
				}()
				conn.SetDeadline(time.Now().Add(5 * time.Second))
				reader := bufio.NewReader(conn)
				for i := 0; i < 2; i++ {
					line, err := reader.ReadBytes('\n')
					if err != nil {
						return
					}
					var req fakeRequest
					if err := json.Unmarshal(line, &req); err != nil {
						t.Error(err)
						return
					}
					if req.JSONRPC != "2.0" {
						t.Errorf("jsonrpc = %q", req.JSONRPC)
						return
					}
					if i == 0 {
						if req.Method != "hello" || len(req.Params) != 1 || req.Params["generation"] != float64(1) {
							t.Errorf("hello = %+v", req)
							return
						}
						reply(conn, req.ID, "result", map[string]any{"generation": generation, "version": "0.1.17", "core": map[string]any{}, "framed": false, "methods": []string{}, "features": []string{}})
					} else {
						if req.Method == "events.subscribe" {
							keep = true
							conn.SetDeadline(time.Time{})
							t.Cleanup(func() { conn.Close() })
						}
						respond(conn, req)
					}
				}
			}()
		}
	}()
	return path
}

func reply(conn net.Conn, id, field string, value any) {
	json.NewEncoder(conn).Encode(map[string]any{"jsonrpc": "2.0", "id": id, field: value})
}

func TestCallBoundary(t *testing.T) {
	for _, tc := range []struct {
		name                                  string
		generation                            int
		kind                                  string
		rpcCode                               int
		notStarted, unknown, refused, missing bool
		code                                  string
	}{
		{name: "success"},
		{name: "missing socket", notStarted: true},
		{name: "generation", generation: 2, notStarted: true, refused: true, code: "protocol_mismatch"},
		{name: "closed", unknown: true},
		{name: "garbage", unknown: true},
		{name: "wrong id", unknown: true},
		{name: "blocked", kind: "blocked", refused: true, code: "blocked"},
		{name: "status mismatch", kind: "status_mismatch", refused: true, code: "status_mismatch"},
		{name: "finished", kind: "finished", refused: true, code: "finished"},
		{name: "needs target", kind: "needs_target", refused: true, code: "needs_target"},
		{name: "not found", kind: "not_found", missing: true, code: "pane_not_found"},
		{name: "invalid params", rpcCode: -32602, refused: true, code: "invalid_params"},
		{name: "dispatch denied", notStarted: true},
		{name: "cancel before write", notStarted: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			generation := tc.generation
			if generation == 0 {
				generation = 1
			}
			path := filepath.Join(socketDir(t), "missing.sock")
			if tc.name != "missing socket" {
				path = fakeCore(t, generation, func(conn net.Conn, req fakeRequest) {
					if tc.notStarted {
						t.Error("method reached core")
						return
					}
					if req.Method != "test.call" || req.Params["value"] != "test" {
						t.Errorf("request = %+v", req)
					}
					switch tc.name {
					case "closed":
						return
					case "garbage":
						conn.Write([]byte("garbage\n"))
						return
					case "wrong id":
						reply(conn, "wrong", "result", map[string]any{})
						return
					}
					if tc.kind != "" || tc.rpcCode != 0 {
						code := tc.rpcCode
						if code == 0 {
							code = -32000
						}
						reply(conn, req.ID, "error", map[string]any{"code": code, "message": "rejected", "data": map[string]any{"kind": tc.kind, "hint": "hint"}})
						return
					}
					reply(conn, req.ID, "result", map[string]any{"ok": true})
				})
			}
			switch tc.name {
			case "dispatch denied":
				ctx = herdr.WithDispatchCheck(ctx, func() error { return errors.New("denied") })
			case "cancel before write":
				ctx = herdr.WithDispatchCheck(ctx, func() error { cancel(); return nil })
			}
			result, err := New(path).call(ctx, "test.call", map[string]any{"value": "test"})
			if tc.name == "success" {
				if err != nil || string(result) != `{"ok":true}` {
					t.Fatalf("result = %s, err = %v", result, err)
				}
				return
			}
			if err == nil {
				t.Fatal("expected error")
			}
			if errors.Is(err, herdr.ErrNotStarted) != tc.notStarted || errors.Is(err, herdr.ErrDispatchedUnknown) != tc.unknown || herdr.IsRefused(err) != tc.refused || herdr.IsPaneNotFound(err) != tc.missing {
				t.Fatalf("unexpected classification: %v", err)
			}
			if tc.code != "" {
				var cliErr *herdr.CLIError
				if !errors.As(err, &cliErr) || cliErr.Code != tc.code {
					t.Fatalf("CLI error = %v", err)
				}
				if tc.generation == 0 && cliErr.Message != "rejected" {
					t.Fatalf("message = %q", cliErr.Message)
				}
			}
		})
	}
}
