package hc

import (
	"encoding/json"
	"net"
	"reflect"
	"sync"
	"testing"

	"github.com/0cv/herdr-mobile-relay/internal/herdr"
)

func eventCore(t *testing.T) (*Client, func(string)) {
	t.Helper()
	var mu sync.Mutex
	var subscription net.Conn
	c := New(fakeCore(t, 1, func(conn net.Conn, req fakeRequest) {
		switch req.Method {
		case "events.subscribe":
			if len(req.Params) != 0 {
				t.Errorf("subscribe params: %v", req.Params)
			}
			mu.Lock()
			subscription = conn
			mu.Unlock()
			reply(conn, req.ID, "result", map[string]any{})
		case "terminal.list":
			reply(conn, req.ID, "result", json.RawMessage(`{"terminals":[{"address":"t1","pid":10,"state":"live","foreground":{"name":"pi"}},{"address":"t2","pid":20,"state":"live","foreground":{"name":"pi"}}]}`))
		case "doc.get":
			reply(conn, req.ID, "result", json.RawMessage(`{"value":{"spaces":[{"id":"s","name":"Space","tabs":[{"id":"tab","name":"Tab"}]}]}}`))
		default:
			t.Errorf("unexpected request: %+v", req)
		}
	}))
	return c, func(line string) {
		t.Helper()
		mu.Lock()
		defer mu.Unlock()
		if line == "" {
			subscription.Close()
			return
		}
		if _, err := subscription.Write([]byte(line + "\n")); err != nil {
			t.Fatal(err)
		}
	}
}

func eventLine(kind, terminal string, seq int) string {
	b, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "method": "event", "params": map[string]any{"type": kind, "terminal": terminal, "seq": seq, "data": map[string]any{"status": "ignored"}}})
	return string(b)
}

func TestEventsSnapshotsAndIdentity(t *testing.T) {
	c, push := eventCore(t)
	before, err := c.GetInventory(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	first, stream, err := c.Events(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer stream.Close()
	if !reflect.DeepEqual(first.Panes, before.Panes) || len(first.Workspaces) != 2 || first.Workspaces[0].Label != "Space" || len(first.Tabs) != 2 || first.Tabs[0].Label != "Tab" {
		t.Fatalf("snapshot: %+v", first)
	}
	next := func() herdr.TopologySnapshot {
		t.Helper()
		s, err := stream.Next(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		return s
	}
	push(eventLine("status", "t1", 1))
	beforeHeartbeat := next()
	push(`{"jsonrpc":"2.0","method":"heartbeat","params":{"seq":1}}`)
	push(eventLine("lost", "rohan/t7", 2))
	push(eventLine("status", "t1", 3))
	status := next()
	if status.Panes[0].Revision != 3 || status.Panes[0].TerminalID != first.Panes[0].TerminalID || !reflect.DeepEqual(status.Workspaces, first.Workspaces) || !reflect.DeepEqual(status.Tabs, first.Tabs) {
		t.Fatalf("status snapshot: %+v", status)
	}
	for i, pane := range status.Panes {
		if pane.TerminalID != beforeHeartbeat.Panes[i].TerminalID {
			t.Fatalf("heartbeat changed identity: %+v, %+v", beforeHeartbeat.Panes, status.Panes)
		}
	}
	push(eventLine("lost", "t1", 4))
	push(eventLine("restarted", "t1", 5))
	lost := next()
	restarted := next()
	inv, err := c.GetInventory(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if lost.Panes[0].TerminalID != first.Panes[0].TerminalID || restarted.Panes[0].TerminalID != lost.Panes[0].TerminalID || inv.Panes[0].TerminalID != restarted.Panes[0].TerminalID || inv.Panes[1].TerminalID != first.Panes[1].TerminalID {
		t.Fatalf("identities: %+v, %+v, %+v", lost.Panes, restarted.Panes, inv.Panes)
	}
	for _, kind := range []string{"doc", "title", "keep", "foreground", "turn", "labels", "size", "created", "finished", "removed"} {
		push(eventLine(kind, "t1", 6))
		if next().Panes[0].Revision != 6 {
			t.Fatal(kind)
		}
	}
	if err := stream.Close(); err != nil {
		t.Fatal(err)
	}
	if err := stream.Close(); err != nil {
		t.Fatal(err)
	}
	prior, err := c.GetInventory(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	second, stream2, err := c.Events(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer stream2.Close()
	for i, p := range second.Panes {
		if p.TerminalID != prior.Panes[i].TerminalID {
			t.Fatal("reconnect changed identity")
		}
	}
}

func TestEventsFailuresPreserveIdentity(t *testing.T) {
	for _, line := range []string{eventLine("missed", "", 0), `{"jsonrpc":"2.0","method":"missed","params":{"seq":0}}`, "", "garbage", `{"jsonrpc":"2.0","method":"other"}`} {
		t.Run(line, func(t *testing.T) {
			c, push := eventCore(t)
			first, stream, err := c.Events(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			defer stream.Close()
			push(line)
			if _, err := stream.Next(t.Context()); err == nil {
				t.Fatal("expected stream failure")
			}
			inv, err := c.GetInventory(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			for i, p := range inv.Panes {
				if p.TerminalID != first.Panes[i].TerminalID {
					t.Fatal("failure changed identity")
				}
			}
		})
	}
}
