package main

import (
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestAgentSessionStdout(t *testing.T) {
	for _, agent := range []string{"agy", "claude", "codex", "cursor", "omp", "opencode", "qoder", "unknown"} {
		for _, scenario := range []string{"success", "outside", "no socket", "invalid", "missing id", "oversized", "read error", "timeout", "lookup failure", "ancestry", "report failure", "extra args"} {
			t.Run(agent+"/"+scenario, func(t *testing.T) {
				t.Setenv("HC_TERMINAL_ID", "t1")
				t.Setenv("HC_TERMINAL_SOCKET", filepath.Join(t.TempDir(), "missing"))
				input, err := os.CreateTemp(t.TempDir(), "input")
				if err != nil {
					t.Fatal(err)
				}
				defer input.Close()
				body := `{"conversationId":"abc","session_id":"abc","hook_event_name":"SessionStart","transcript_path":"/tmp/log"}`
				if agent == "cursor" {
					body = `{"sessionId":"abc"}`
				}
				switch scenario {
				case "outside":
					t.Setenv("HC_TERMINAL_ID", "")
				case "no socket":
					t.Setenv("HC_TERMINAL_SOCKET", "")
				case "invalid":
					body = "{"
				case "missing id":
					body = "{}"
				case "oversized":
					body = strings.Repeat("x", (1<<20)+1)
				}
				if _, err := input.WriteString(body); err != nil {
					t.Fatal(err)
				}
				if _, err := input.Seek(0, 0); err != nil {
					t.Fatal(err)
				}
				if scenario == "read error" {
					input.Close()
				}
				if scenario == "timeout" {
					reader, writer, err := os.Pipe()
					if err != nil {
						t.Fatal(err)
					}
					defer reader.Close()
					defer writer.Close()
					input = reader
				}
				labels := make(chan map[string]any, 1)
				if scenario == "success" || scenario == "ancestry" || scenario == "report failure" {
					dir, err := os.MkdirTemp("", "agy-")
					if err != nil {
						t.Fatal(err)
					}
					defer os.RemoveAll(dir)
					path := filepath.Join(dir, "s")
					listener, err := net.Listen("unix", path)
					if err != nil {
						t.Fatal(err)
					}
					done := make(chan struct{})
					defer func() { listener.Close(); <-done }()
					go func() {
						defer close(done)
						for {
							conn, err := listener.Accept()
							if err != nil {
								return
							}
							func() {
								defer conn.Close()
								conn.SetDeadline(time.Now().Add(3 * time.Second))
								decoder := json.NewDecoder(conn)
								for range 2 {
									var req struct {
										ID     string
										Method string
										Params map[string]any
									}
									if decoder.Decode(&req) != nil {
										return
									}
									var result any = map[string]any{"generation": 1}
									switch req.Method {
									case "terminal.get":
										pid := os.Getppid()
										if scenario == "ancestry" {
											pid = 1
										}
										result = map[string]any{"address": "t1", "state": "live", "pid": pid, "foreground": map[string]any{"pid": pid}}
									case "terminal.label":
										labels <- req.Params
										if scenario == "report failure" {
											return
										}
									}
									if json.NewEncoder(conn).Encode(map[string]any{"jsonrpc": "2.0", "id": req.ID, "result": result}) != nil {
										return
									}
								}
							}()
						}
					}()
					t.Setenv("HC_TERMINAL_SOCKET", path)
				}
				output, err := os.CreateTemp(t.TempDir(), "output")
				if err != nil {
					t.Fatal(err)
				}
				defer output.Close()
				oldIn, oldOut := os.Stdin, os.Stdout
				os.Stdin, os.Stdout = input, output
				defer func() { os.Stdin, os.Stdout = oldIn, oldOut }()
				args := []string{"agent-session", agent}
				if scenario == "extra args" {
					args = append(args, "extra")
				}
				code, err := run(args)
				if code != 0 || err != nil {
					t.Fatalf("%d %v", code, err)
				}
				got, err := os.ReadFile(output.Name())
				want := ""
				if agent == "agy" {
					want = "{}"
				}
				if err != nil || string(got) != want {
					t.Fatalf("stdout %q, want %q: %v", got, want, err)
				}
				if agent != "unknown" && (scenario == "success" || scenario == "report failure") {
					select {
					case params := <-labels:
						if params["terminal"] != "t1" || params["labels"].(map[string]any)["herdr-mobile-relay.session"] != "abc" {
							t.Fatal(params)
						}
					default:
						t.Fatal("session was not reported")
					}
				} else if len(labels) != 0 {
					t.Fatal("unexpected report")
				}
			})
		}
	}
}

func TestAgentSessionAlwaysSilentSuccess(t *testing.T) {
	t.Setenv("HC_TERMINAL_ID", "t1")
	t.Setenv("HC_TERMINAL_SOCKET", t.TempDir()+"/missing.sock")
	input, err := os.CreateTemp(t.TempDir(), "input")
	if err != nil {
		t.Fatal(err)
	}
	defer input.Close()
	if _, err = input.WriteString(`{"session_id":"abc","hook_event_name":"SessionStart"}`); err != nil {
		t.Fatal(err)
	}
	output, err := os.CreateTemp(t.TempDir(), "output")
	if err != nil {
		t.Fatal(err)
	}
	defer output.Close()
	oldIn, oldOut := os.Stdin, os.Stdout
	os.Stdin, os.Stdout = input, output
	defer func() { os.Stdin, os.Stdout = oldIn, oldOut }()
	for _, args := range [][]string{{"agent-session"}, {"agent-session", "unknown"}, {"agent-session", "claude", "extra"}, {"agent-session", "claude"}, {"agent-session", "codex"}} {
		if _, err := input.Seek(0, 0); err != nil {
			t.Fatal(err)
		}
		code, err := run(args)
		if code != 0 || err != nil {
			t.Fatalf("%v: %d %v", args, code, err)
		}
	}
	stat, err := output.Stat()
	if err != nil || stat.Size() != 0 {
		t.Fatalf("stdout: %v %v", stat, err)
	}
}
