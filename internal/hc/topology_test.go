package hc

import (
	"encoding/json"
	"net"
	"reflect"
	"sync"
	"testing"

	"github.com/0cv/herdr-mobile-relay/internal/herdr"
)

func scriptedCore(t *testing.T, terminals string, doc string) *Client {
	t.Helper()
	var list []terminal
	if err := json.Unmarshal([]byte(terminals), &list); err != nil {
		t.Fatal(err)
	}
	return New(fakeCore(t, 1, func(conn net.Conn, req fakeRequest) {
		switch req.Method {
		case "terminal.list":
			if len(req.Params) != 0 {
				t.Errorf("list params: %v", req.Params)
			}
			reply(conn, req.ID, "result", map[string]any{"terminals": list})
		case "terminal.get":
			if len(req.Params) != 1 {
				t.Errorf("get params: %v", req.Params)
			}
			for _, term := range list {
				if term.Address == req.Params["terminal"] {
					reply(conn, req.ID, "result", term)
					return
				}
			}
			reply(conn, req.ID, "error", map[string]any{"code": -32000, "data": map[string]any{"kind": "not_found"}})
		case "doc.get":
			if len(req.Params) != 1 || req.Params["name"] != "herdr-app" {
				t.Errorf("doc params: %v", req.Params)
			}
			if doc == "" {
				reply(conn, req.ID, "error", map[string]any{"code": -32000, "data": map[string]any{"kind": "not_found"}})
			} else {
				reply(conn, req.ID, "result", json.RawMessage(doc))
			}
		default:
			t.Errorf("unexpected request: %+v", req)
		}
	}))
}

func TestMappingAndInventory(t *testing.T) {
	c := scriptedCore(t, `[
 {"address":"t1","pid":10,"state":"live","labels":{"herdr.space":"s","herdr.tab":"tab"},"name":"named","title":"ignored","cwd":"/start","turn":9,"reported":true,"status":{"app":"custom","state":"error"},"foreground":{"name":"zsh","pid":10,"cwd":"/now"}},
 {"address":"t2","pid":20,"state":"live","title":"title","foreground":{"name":"pi","pid":20}},
 {"address":"t3","pid":30,"state":"live","reported":false,"status":{"app":"ignored","state":"idle"},"foreground":{"name":"zsh"}},
 {"address":"t4","pid":40,"state":"finished","reported":true,"status":{"app":"pi"}},
 {"address":"t5","pid":null,"state":"live","foreground":{"name":"pi"}},
 {"address":"remote/t6","pid":60,"state":"live","foreground":{"name":"pi"}},
 {"address":"t7","pid":70,"state":"live","reported":true,"status":{"app":"","state":"working"},"foreground":{"name":"codex"}},
 {"address":"t8","pid":80,"state":"live","reported":true}
 ]`, "")
	c.setRevision("t1", 17)
	panes, err := c.PaneList(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if len(panes) != 5 {
		t.Fatalf("panes: %+v", panes)
	}
	want := herdr.Pane{ID: "t1", TerminalID: "t1:10:0", WorkspaceID: "s", TabID: "tab", Name: "named", Agent: "custom", Status: "done", Cwd: "/start", ForegroundCwd: "/now", Revision: 17, StateChangeSeq: 9}
	if !reflect.DeepEqual(panes[0], want) {
		t.Fatalf("pane: %+v, want %+v", panes[0], want)
	}
	if p := panes[1]; p.WorkspaceID != "core" || p.TabID != "core" || p.Name != "title" || p.Agent != "pi" || p.Status != "unknown" {
		t.Fatalf("synthetic pane: %+v", p)
	}
	if panes[2].Agent != "" || panes[3].Agent != "codex" || panes[4].Agent != "" {
		t.Fatalf("detection: %+v", panes)
	}
	inv, err := c.GetInventory(t.Context())
	if err != nil || len(inv.Panes) != 3 {
		t.Fatalf("inventory: %+v, %v", inv, err)
	}
	for _, p := range panes {
		got, err := c.PaneGet(t.Context(), p.ID)
		if err != nil || !reflect.DeepEqual(*got, p) {
			t.Fatalf("get: %+v, %v", got, err)
		}
	}
	for _, id := range []string{"t4", "t5", "remote/t6", "missing"} {
		if _, err := c.PaneGet(t.Context(), id); !herdr.IsPaneNotFound(err) {
			t.Fatalf("%s: %v", id, err)
		}
	}
}

func TestTopology(t *testing.T) {
	terminals := `[
 {"address":"t1","pid":1,"state":"live","labels":{"herdr.space":"s","herdr.tab":"a"},"cwd":"/pane"},
 {"address":"t2","pid":2,"state":"live","labels":{"herdr.space":"s","herdr.tab":"extra"},"cwd":"/extra"},
 {"address":"t3","pid":3,"state":"live","labels":{"herdr.space":"label","herdr.tab":"labeltab"},"cwd":"/label"},
 {"address":"t4","pid":4,"state":"live","cwd":"/core"}
 ]`
	c := scriptedCore(t, terminals, `{"value":{"spaces":[{"id":"s","name":"Space","folder":"/doc","tabs":[{"id":"a","name":"A"},{"id":"empty","name":"Empty"}]},{"id":"unused","name":"Unused","folder":"/unused","tabs":[]}]}}`)
	spaces, err := c.WorkspaceList(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	wantSpaces := []herdr.Workspace{
		{ID: "s", Label: "Space", Cwd: "/doc", Number: 1, PaneCount: 2, TabCount: 3},
		{ID: "unused", Label: "Unused", Cwd: "/unused", Number: 2},
		{ID: "label", Label: "label", Cwd: "/label", Number: 3, PaneCount: 1, TabCount: 1},
		{ID: "core", Label: "core", Cwd: "/core", Number: 4, PaneCount: 1, TabCount: 1},
	}
	if !reflect.DeepEqual(spaces, wantSpaces) {
		t.Fatalf("spaces: %+v", spaces)
	}
	tabs, err := c.TabList(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	wantTabs := []herdr.Tab{
		{ID: "a", WorkspaceID: "s", Label: "A", Cwd: "/doc", Number: 1},
		{ID: "empty", WorkspaceID: "s", Label: "Empty", Cwd: "/doc", Number: 2},
		{ID: "extra", WorkspaceID: "s", Label: "extra", Cwd: "/extra", Number: 3},
		{ID: "labeltab", WorkspaceID: "label", Label: "labeltab", Cwd: "/label", Number: 1},
		{ID: "core", WorkspaceID: "core", Label: "core", Cwd: "/core", Number: 1},
	}
	if !reflect.DeepEqual(tabs, wantTabs) {
		t.Fatalf("tabs: %+v", tabs)
	}
	missing := scriptedCore(t, terminals, "")
	spaces, err = missing.WorkspaceList(t.Context())
	if err != nil || len(spaces) != 3 || spaces[0].TabCount != 2 {
		t.Fatalf("missing doc: %+v, %v", spaces, err)
	}
}

func TestProcessShapes(t *testing.T) {
	for _, tc := range []struct {
		name, argv     string
		pid, fg, shell int
	}{
		{"direct", `["sleep","300"]`, 42, 42, 0},
		{"hosted", `["/bin/zsh","-l"]`, 10, 42, 10},
		{"idle", `["/bin/zsh"]`, 10, 10, 10},
		{"login", `["-zsh"]`, 10, 10, 10},
	} {
		t.Run(tc.name, func(t *testing.T) {
			data, _ := json.Marshal(map[string]any{"address": "t1", "state": "live", "pid": tc.pid, "argv": json.RawMessage(tc.argv), "foreground": map[string]any{"pid": tc.fg, "name": "process", "cwd": "/cwd", "argv": []string{"process", "arg"}}})
			c := scriptedCore(t, "["+string(data)+"]", "")
			info, err := c.PaneProcessInfoFresh(t.Context(), "t1")
			want := &herdr.PaneProcessInfo{PaneID: "t1", ShellPID: tc.shell, ForegroundProcessGroupID: tc.fg, ForegroundProcesses: []herdr.PaneProcess{{PID: tc.fg, Name: "process", Cwd: "/cwd", Argv: []string{"process", "arg"}}}}
			if err != nil || !reflect.DeepEqual(info, want) {
				t.Fatalf("info: %+v, %v", info, err)
			}
		})
	}
}

func TestIdentity(t *testing.T) {
	c := scriptedCore(t, `[{"address":"a","pid":10,"state":"live","foreground":{"name":"pi"}},{"address":"b","pid":10,"state":"live"}]`, "")
	get := func(id string) string {
		t.Helper()
		p, err := c.PaneGet(t.Context(), id)
		if err != nil {
			t.Fatal(err)
		}
		return p.TerminalID
	}
	a, b := get("a"), get("b")
	c.bumpEpoch("a")
	if get("a") == a || get("b") != b {
		t.Fatal("single bump changed wrong identities")
	}
	a = get("a")
	c.bumpAllEpochs()
	if get("a") == a || get("b") == b {
		t.Fatal("all bump did not invalidate")
	}
	panes, err := c.PaneList(t.Context())
	if err != nil || panes[0].TerminalID != get("a") {
		t.Fatalf("list: %+v, %v", panes, err)
	}
	inv, err := c.GetInventory(t.Context())
	if err != nil || inv.Panes[0].TerminalID != get("a") {
		t.Fatalf("inventory: %+v, %v", inv, err)
	}
	pid := 10
	term := terminal{Address: "later", PID: &pid}
	later := c.pane(term).TerminalID
	if later != "later:10:2" {
		t.Fatalf("new address epoch: %s", later)
	}
	pid = 11
	if c.pane(term).TerminalID == later {
		t.Fatal("pid replacement reused identity")
	}
	pid = 10
	c.bumpAllEpochs()
	if c.pane(term).TerminalID == later {
		t.Fatal("reused pid retained identity")
	}
	term.Address = "new"
	if got := c.pane(term).TerminalID; got != "new:10:3" {
		t.Fatalf("new floor: %s", got)
	}
}

func TestIdentityConcurrent(t *testing.T) {
	c := New("")
	pid := 10
	var wg sync.WaitGroup
	for range 8 {
		wg.Go(func() {
			for i := range 100 {
				c.bumpEpoch("a")
				c.bumpAllEpochs()
				c.setRevision("a", i)
				c.pane(terminal{Address: "a", PID: &pid})
			}
		})
	}
	wg.Wait()
}
