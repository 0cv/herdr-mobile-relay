package app

import (
	"encoding/json"
	"net"
	"path/filepath"
	"testing"
	"time"

	"github.com/0cv/herdr-mobile-relay/internal/config"
	"github.com/0cv/herdr-mobile-relay/internal/conversation"
	"github.com/0cv/herdr-mobile-relay/internal/coordinator"
	"github.com/0cv/herdr-mobile-relay/internal/hc"
)

func TestKimiCoreInference(t *testing.T) {
	home := t.TempDir()
	appKimiFixture(t, home, "core_session")
	socket := filepath.Join(t.TempDir(), "core.sock")
	listener, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			func() {
				defer conn.Close()
				_ = conn.SetDeadline(time.Now().Add(time.Second))
				decoder := json.NewDecoder(conn)
				for {
					var req struct{ ID, Method string }
					if decoder.Decode(&req) != nil {
						return
					}
					var result any
					switch req.Method {
					case "hello":
						result = map[string]int{"generation": 1}
					case "terminal.get":
						result = json.RawMessage(`{"address":"t12","state":"live","pid":10,"argv":["/bin/zsh","-l"],"cwd":"/other","reported":true,"status":{"app":"kimi-code","state":"error","msg":"No subscription"},"foreground":{"pid":20,"name":"kimi","cwd":"/fixture","argv":["kimi"]}}`)
					default:
						t.Errorf("unexpected core method %s", req.Method)
						return
					}
					if json.NewEncoder(conn).Encode(map[string]any{"jsonrpc": "2.0", "id": req.ID, "result": result}) != nil {
						return
					}
				}
			}()
		}
	}()
	t.Cleanup(func() { listener.Close(); <-done })
	client := hc.New(socket)
	t.Cleanup(func() { client.Close() })
	pane, err := client.PaneGet(t.Context(), "t12")
	if err != nil {
		t.Fatal(err)
	}
	if pane.Agent != "kimi-code" {
		t.Fatalf("agent=%q", pane.Agent)
	}
	s := &Server{cfg: &config.Config{Backend: "hc"}, herdrC: client, conversationM: conversation.NewReader(home), kimiProcessBirth: func(int) (string, error) { return "fixture-birth", nil }}
	a := &coordinator.AgentState{Agent: pane.Agent, PaneID: pane.ID, TerminalID: pane.TerminalID, Cwd: pane.Cwd, Status: pane.Status}
	s.enrichKimiSessions(t.Context(), []*coordinator.AgentState{a})
	if a.Session != "core_session" || !a.ConversationInferred || a.ForegroundCwd != "/fixture" || a.ConversationReason != "" {
		t.Fatalf("core inference=%+v", a)
	}
	page, err := s.conversationM.ReadFor(a.Agent, a.ForegroundCwd, a.Session, "", 80)
	if err != nil || len(page.Entries) != 2 {
		t.Fatalf("page=%+v err=%v", page, err)
	}
	a.Session = ""
	peer := &coordinator.AgentState{Agent: "kimi-code", PaneID: "t12", Cwd: "/fixture"}
	s.enrichKimiSessions(t.Context(), []*coordinator.AgentState{a, peer})
	if a.Session != "" || a.ConversationReason != "ambiguous_session" {
		t.Fatalf("ambiguous inference=%+v", a)
	}
}
