package main

import (
	"encoding/json"
	"errors"
	"io"
	"net"
	"os"
	"path/filepath"
	"sync"
	"time"
)

const uncertainInputText = "uncertain-fixture-text"

type socketScenario struct {
	Panes              []map[string]any  `json:"panes"`
	Tabs               []map[string]any  `json:"tabs"`
	Workspaces         []map[string]any  `json:"workspaces"`
	Content            map[string]string `json:"content"`
	FocusedWorkspaceID string            `json:"focused_workspace_id"`
}

type socketRequest struct {
	ID     string          `json:"id"`
	Method string          `json:"method"`
	Params json.RawMessage `json:"params"`
}

type socketServer struct {
	scenario     socketScenario
	overrides    string
	inputLog     string
	inputLogLock sync.Mutex
}

func runSocketServer() error {
	path := os.Getenv("FAKE_HERDR_SOCKET")
	if path == "" {
		return errors.New("FAKE_HERDR_SOCKET is required")
	}
	scenarioPath := os.Getenv("FAKE_HERDR_SCENARIO")
	if scenarioPath == "" {
		return errors.New("FAKE_HERDR_SCENARIO is required")
	}
	data, err := os.ReadFile(scenarioPath)
	if err != nil {
		return err
	}
	server := &socketServer{
		overrides: os.Getenv("FAKE_HERDR_PANE_OVERRIDES"),
		inputLog:  os.Getenv("FAKE_HERDR_INPUT_LOG"),
	}
	if err := json.Unmarshal(data, &server.scenario); err != nil {
		return err
	}
	if server.scenario.Panes == nil {
		server.scenario.Panes = []map[string]any{}
	}
	if server.scenario.Tabs == nil {
		server.scenario.Tabs = []map[string]any{}
	}
	if server.scenario.Workspaces == nil {
		server.scenario.Workspaces = deriveWorkspaces(server.scenario)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	_ = os.Remove(path)
	listener, err := net.Listen("unix", path)
	if err != nil {
		return err
	}
	defer listener.Close()
	for {
		conn, err := listener.Accept()
		if err != nil {
			return err
		}
		go server.serve(conn)
	}
}

func deriveWorkspaces(scenario socketScenario) []map[string]any {
	workspaces := []map[string]any{}
	seen := map[string]bool{}
	for _, items := range [][]map[string]any{scenario.Panes, scenario.Tabs} {
		for _, item := range items {
			id, _ := item["workspace_id"].(string)
			if id == "" || seen[id] {
				continue
			}
			seen[id] = true
			workspaces = append(workspaces, map[string]any{
				"workspace_id": id,
				"number":       len(workspaces) + 1,
				"label":        id,
				"pane_count":   0,
				"tab_count":    0,
			})
		}
	}
	return workspaces
}

func (s *socketServer) serve(conn net.Conn) {
	defer conn.Close()
	var request socketRequest
	if json.NewDecoder(conn).Decode(&request) != nil {
		return
	}
	var params struct {
		PaneID       string `json:"pane_id"`
		Text         string `json:"text"`
		WorkspaceIDs []any  `json:"workspace_ids"`
	}
	_ = json.Unmarshal(request.Params, &params)
	encoder := json.NewEncoder(conn)
	send := func(result any) {
		_ = encoder.Encode(map[string]any{"id": request.ID, "result": result})
	}
	sendError := func(code, message string) {
		_ = encoder.Encode(map[string]any{
			"id":    request.ID,
			"error": map[string]any{"code": code, "message": message},
		})
	}

	panes := s.panes()
	switch request.Method {
	case "ping":
		send(map[string]any{
			"type": "pong", "version": "0.9.0", "protocol": 1,
			"capabilities": map[string]any{
				"endpoint_protocol_generation": 1,
				"surface_interest":             true,
				"health_check":                 true,
			},
		})
	case "agent.list":
		send(map[string]any{"type": "agent_list", "agents": panes})
	case "pane.list":
		send(map[string]any{"type": "pane_list", "panes": panes})
	case "workspace.list":
		send(map[string]any{"type": "workspace_list", "workspaces": s.scenario.Workspaces})
	case "tab.list":
		send(map[string]any{"type": "tab_list", "tabs": s.scenario.Tabs})
	case "session.snapshot":
		snapshot := map[string]any{
			"version": "0.9.0", "protocol": 1,
			"workspaces": s.scenario.Workspaces, "tabs": s.scenario.Tabs,
			"panes": panes, "agents": panes,
		}
		if s.scenario.FocusedWorkspaceID != "" {
			snapshot["focused_workspace_id"] = s.scenario.FocusedWorkspaceID
		}
		send(map[string]any{"type": "session_snapshot", "snapshot": snapshot})
	case "events.subscribe":
		send(map[string]any{"type": "subscription_started"})
		_, _ = io.Copy(io.Discard, conn)
	case "workspace.move_block":
		if len(params.WorkspaceIDs) == 0 {
			sendError("workspace_move_block_failed", "empty selection")
			break
		}
		send(map[string]any{"type": "workspace_list", "workspaces": s.scenario.Workspaces})
	case "workspace.move":
		send(map[string]any{"type": "workspace_list", "workspaces": s.scenario.Workspaces})
	case "pane.send_input":
		if s.inputLog == "" {
			sendError("unknown_method", "unknown method")
			break
		}
		s.logInput(request)
		if params.Text == uncertainInputText {
			return
		}
		send(map[string]any{"type": "ok"})
	case "pane.read":
		if len(s.scenario.Content) > 0 {
			sendError("unknown_method", "scenario content uses the CLI fixture")
			break
		}
		if params.PaneID == "" {
			sendError("pane_not_found", "pane  not found")
			break
		}
		send(map[string]any{
			"type": "pane_read",
			"read": map[string]any{"text": "blackbox fixture pane", "truncated": false},
		})
	default:
		sendError("unknown_method", "unknown method")
	}
}

func (s *socketServer) panes() []map[string]any {
	overrides := map[string]map[string]any{}
	if s.overrides != "" {
		if data, err := os.ReadFile(s.overrides); err == nil {
			_ = json.Unmarshal(data, &overrides)
		}
	}
	panes := make([]map[string]any, 0, len(s.scenario.Panes))
	for _, pane := range s.scenario.Panes {
		merged := make(map[string]any, len(pane))
		for key, value := range pane {
			merged[key] = value
		}
		id, _ := pane["pane_id"].(string)
		for key, value := range overrides[id] {
			merged[key] = value
		}
		panes = append(panes, merged)
	}
	return panes
}

func (s *socketServer) logInput(request socketRequest) {
	record, err := json.Marshal(map[string]any{
		"method":     request.Method,
		"params":     request.Params,
		"started_at": time.Now().UTC().Format("2006-01-02T15:04:05.000Z"),
	})
	if err != nil {
		return
	}
	s.inputLogLock.Lock()
	defer s.inputLogLock.Unlock()
	file, err := os.OpenFile(s.inputLog, os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o600)
	if err != nil {
		return
	}
	defer file.Close()
	_, _ = file.Write(append(record, '\n'))
}
