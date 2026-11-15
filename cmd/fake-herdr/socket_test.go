package main

import (
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func socketCall(t *testing.T, server *socketServer, request string) map[string]any {
	t.Helper()
	client, serverSide := net.Pipe()
	go server.serve(serverSide)
	defer client.Close()
	if _, err := client.Write([]byte(request + "\n")); err != nil {
		t.Fatal(err)
	}
	var response map[string]any
	if err := json.NewDecoder(client).Decode(&response); err != nil {
		return nil
	}
	return response
}

func TestSocketServerServesScenarioInventory(t *testing.T) {
	dir := t.TempDir()
	overrides := filepath.Join(dir, "overrides.json")
	if err := os.WriteFile(overrides, []byte(`{"pane-1":{"cwd":"/moved"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	server := &socketServer{
		scenario: socketScenario{
			Panes: []map[string]any{{"pane_id": "pane-1", "workspace_id": "ws-1", "cwd": "/tmp"}},
			Tabs:  []map[string]any{},
		},
		overrides: overrides,
		inputLog:  filepath.Join(dir, "input.jsonl"),
	}
	server.scenario.Workspaces = deriveWorkspaces(server.scenario)

	panes := socketCall(t, server, `{"id":"1","method":"pane.list"}`)["result"].(map[string]any)["panes"].([]any)
	if pane := panes[0].(map[string]any); pane["cwd"] != "/moved" {
		t.Fatalf("pane = %+v, want override applied", pane)
	}
	workspaces := socketCall(t, server, `{"id":"2","method":"workspace.list"}`)["result"].(map[string]any)["workspaces"].([]any)
	if workspace := workspaces[0].(map[string]any); workspace["workspace_id"] != "ws-1" {
		t.Fatalf("workspace = %+v", workspace)
	}
	if response := socketCall(t, server, `{"id":"3","method":"workspace.move_block","params":{"workspace_ids":[]}}`); response["error"].(map[string]any)["code"] != "workspace_move_block_failed" {
		t.Fatalf("empty move_block = %+v", response)
	}
	if response := socketCall(t, server, `{"id":"4","method":"pane.read","params":{"pane_id":""}}`); response["error"].(map[string]any)["code"] != "pane_not_found" {
		t.Fatalf("empty pane.read = %+v", response)
	}
	server.scenario.Content = map[string]string{"pane-1": "text"}
	if response := socketCall(t, server, `{"id":"5","method":"pane.read","params":{"pane_id":"pane-1"}}`); response["error"].(map[string]any)["code"] != "unknown_method" {
		t.Fatalf("pane.read with scenario content = %+v", response)
	}
}

func TestSocketServerLogsInputAndDropsUncertainReply(t *testing.T) {
	log := filepath.Join(t.TempDir(), "input.jsonl")
	server := &socketServer{inputLog: log}

	if response := socketCall(t, server, `{"id":"1","method":"pane.send_input","params":{"pane_id":"pane-1","text":"hello"}}`); response["result"].(map[string]any)["type"] != "ok" {
		t.Fatalf("send_input = %+v", response)
	}
	if response := socketCall(t, server, `{"id":"2","method":"pane.send_input","params":{"pane_id":"pane-1","text":"`+uncertainInputText+`"}}`); response != nil {
		t.Fatalf("uncertain send_input replied: %+v", response)
	}
	data, err := os.ReadFile(log)
	if err != nil {
		t.Fatal(err)
	}
	if lines := strings.Count(string(data), "\n"); lines != 2 || !strings.Contains(string(data), `"text":"hello"`) {
		t.Fatalf("input log = %q", data)
	}

	unlogged := &socketServer{}
	if response := socketCall(t, unlogged, `{"id":"3","method":"pane.send_input","params":{"pane_id":"pane-1","text":"x"}}`); response["error"].(map[string]any)["code"] != "unknown_method" {
		t.Fatalf("send_input without log = %+v", response)
	}
}
