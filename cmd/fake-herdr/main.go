// Command fake-herdr is a strict, stateful Herdr 0.7.5 CLI fake.
package main

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

type Pane struct {
	ID          string `json:"pane_id"`
	TerminalID  string `json:"terminal_id,omitempty"`
	TabID       string `json:"tab_id,omitempty"`
	WorkspaceID string `json:"workspace_id,omitempty"`
	Agent       string `json:"agent,omitempty"`
	Name        string `json:"name,omitempty"`
	Status      string `json:"agent_status,omitempty"`
	Focused     bool   `json:"focused,omitempty"`
	Cwd         string `json:"cwd,omitempty"`
	Revision    int    `json:"revision,omitempty"`
	Scroll      struct {
		MaxOffsetFromBottom int `json:"max_offset_from_bottom"`
	} `json:"scroll,omitempty"`
	ForegroundCwd string `json:"foreground_cwd,omitempty"`
}

type Workspace struct {
	ID    string `json:"workspace_id"`
	Label string `json:"label"`
}

type Tab struct {
	ID          string `json:"tab_id"`
	WorkspaceID string `json:"workspace_id"`
	Label       string `json:"label"`
	Number      int    `json:"number,omitempty"`
	Cwd         string `json:"cwd"`
}

type Agent struct {
	PaneID  string `json:"pane_id"`
	Agent   string `json:"agent"`
	Name    string `json:"name"`
	Status  string `json:"agent_status"`
	Running bool   `json:"running"`
}

type Injection struct {
	DelayMS               int    `json:"delay_ms,omitempty"`
	Error                 string `json:"error,omitempty"`
	Hang                  bool   `json:"hang,omitempty"`
	BeforeExecuteBarrier  string `json:"before_execute_barrier,omitempty"`
	BeforeResponseBarrier string `json:"before_response_barrier,omitempty"`
}

type Scenario struct {
	Panes      []Pane               `json:"panes"`
	Workspaces []Workspace          `json:"workspaces,omitempty"`
	Tabs       []Tab                `json:"tabs,omitempty"`
	Agents     map[string]Agent     `json:"agents,omitempty"`
	Content    map[string]string    `json:"content,omitempty"`
	Responses  map[string]string    `json:"responses,omitempty"`
	Injections map[string]Injection `json:"injections,omitempty"`
	FailNext   bool                 `json:"fail_next,omitempty"`
	HangNext   bool                 `json:"hang_next,omitempty"`
	Sequence   uint64               `json:"sequence,omitempty"`
}

type Operation struct {
	Sequence    uint64            `json:"sequence"`
	Argv        []string          `json:"argv"`
	Environment map[string]string `json:"environment"`
	StartedAt   string            `json:"started_at"`
	CompletedAt string            `json:"completed_at"`
	Outcome     string            `json:"outcome"`
	Injected    string            `json:"injected,omitempty"`
}

type ControlCmd struct {
	Cmd         string      `json:"cmd"`
	Pane        string      `json:"pane,omitempty"`
	Status      string      `json:"status,omitempty"`
	Agent       string      `json:"agent,omitempty"`
	Name        string      `json:"name,omitempty"`
	WorkspaceID string      `json:"workspace_id,omitempty"`
	Label       string      `json:"label,omitempty"`
	Cwd         string      `json:"cwd,omitempty"`
	Command     string      `json:"command,omitempty"`
	Content     string      `json:"content,omitempty"`
	Barrier     string      `json:"barrier,omitempty"`
	Injection   *Injection  `json:"injection,omitempty"`
	AgentInfo   *Agent      `json:"agent_info,omitempty"`
	Workspace   *Workspace  `json:"workspace,omitempty"`
	Raw         interface{} `json:"raw,omitempty"`
}

type stateStore struct {
	path string
}

func main() {
	args := os.Args[1:]
	if len(args) == 1 && args[0] == "control-server" {
		if err := runControlServer(); err != nil {
			fatal("%v", err)
		}
		return
	}
	if len(args) < 2 {
		fatal("expected <group> <command> [args]")
	}

	store, err := openStateStore()
	if err != nil {
		fatal("%v", err)
	}

	started := time.Now().UTC()
	scenario, err := store.update(func(s *Scenario) error {
		s.Sequence++
		return nil
	})
	if err != nil {
		fatal("%v", err)
	}

	key := strings.Join(args[:2], " ")
	injection := scenario.Injections[key]
	if scenario.HangNext {
		_, _ = store.update(func(s *Scenario) error { s.HangNext = false; return nil })
		injection.Hang = true
	}
	if scenario.FailNext {
		_, _ = store.update(func(s *Scenario) error { s.FailNext = false; return nil })
		injection.Error = "injected failure"
	}
	injected := injectionLabel(injection)
	recordOperation(scenario.Sequence, args, started, time.Time{}, "started", injected)
	if injection.DelayMS > 0 {
		time.Sleep(time.Duration(injection.DelayMS) * time.Millisecond)
	}
	if injection.Hang {
		select {}
	}
	if injection.Error != "" {
		recordOperation(scenario.Sequence, args, started, time.Now().UTC(), "failed", injected)
		fatal("%s", injection.Error)
	}
	if injection.BeforeExecuteBarrier != "" {
		waitBarrier(injection.BeforeExecuteBarrier, scenario.Sequence)
	}

	output, commandErr := execute(store, scenario, args)
	if injection.BeforeResponseBarrier != "" {
		waitBarrier(injection.BeforeResponseBarrier, scenario.Sequence)
	}
	outcome := "succeeded"
	if commandErr != nil {
		outcome = "failed"
	}
	recordOperation(scenario.Sequence, args, started, time.Now().UTC(), outcome, injected)

	if commandErr != nil {
		fatal("%v", commandErr)
	}
	if output != "" {
		fmt.Print(output)
		if !strings.HasSuffix(output, "\n") {
			fmt.Println()
		}
	}
}

func injectionLabel(injection Injection) string {
	switch {
	case injection.Hang:
		return "hang"
	case injection.Error != "":
		return "error"
	case injection.BeforeExecuteBarrier != "" || injection.BeforeResponseBarrier != "":
		return "barrier"
	case injection.DelayMS > 0:
		return "delay"
	default:
		return ""
	}
}

func execute(store *stateStore, scenario Scenario, args []string) (string, error) {
	group, command, rest := args[0], args[1], args[2:]
	if raw, ok := scenario.Responses[strings.Join(args, "\x00")]; ok {
		return raw, nil
	}

	switch group + " " + command {
	case "pane list":
		if err := exactLen(rest, 0); err != nil {
			return "", err
		}
		return envelope(map[string]any{"panes": scenario.Panes}), nil
	case "pane read":
		if err := validatePaneRead(rest); err != nil {
			return "", err
		}
		content := scenario.Content[rest[0]]
		if content == "" {
			content = "fake terminal content for pane " + rest[0]
		}
		return content, nil
	case "pane send-keys":
		if len(rest) < 2 {
			return "", errors.New("pane send-keys requires pane ID and at least one key")
		}
		if strings.HasPrefix(rest[0], "-") {
			return "", errors.New("pane send-keys has invalid pane ID")
		}
		return envelope(map[string]any{"ok": true}), nil
	case "pane send-text":
		if err := exactLen(rest, 2); err != nil {
			return "", err
		}
		return envelope(map[string]any{"ok": true}), nil
	case "pane run":
		if err := exactLen(rest, 2); err != nil {
			return "", err
		}
		return envelope(map[string]any{"pane_id": rest[0]}), nil
	case "pane close":
		if err := exactLen(rest, 1); err != nil {
			return "", err
		}
		_, err := store.update(func(s *Scenario) error {
			s.Panes = removePane(s.Panes, rest[0])
			delete(s.Agents, rest[0])
			return nil
		})
		return envelope(map[string]any{"pane_id": rest[0]}), err
	case "workspace list":
		if err := exactLen(rest, 0); err != nil {
			return "", err
		}
		return envelope(map[string]any{"workspaces": scenario.Workspaces}), nil
	case "workspace create":
		values, err := exactFlags(rest, []string{"--cwd", "--label"}, true)
		if err != nil {
			return "", err
		}
		var createdPane Pane
		var createdTab Tab
		var createdWorkspace Workspace
		_, err = store.update(func(s *Scenario) error {
			id := fmt.Sprintf("workspace-%d", s.Sequence)
			tabID := fmt.Sprintf("tab-%d", s.Sequence)
			paneID := fmt.Sprintf("pane-%d", s.Sequence)
			createdWorkspace = Workspace{ID: id, Label: values["--label"]}
			createdTab = Tab{ID: tabID, WorkspaceID: id, Label: values["--label"], Cwd: values["--cwd"]}
			createdPane = Pane{ID: paneID, TabID: tabID, WorkspaceID: id, Cwd: values["--cwd"], Status: "idle"}
			s.Workspaces = append(s.Workspaces, createdWorkspace)
			s.Tabs = append(s.Tabs, createdTab)
			s.Panes = append(s.Panes, createdPane)
			return nil
		})
		if err != nil {
			return "", err
		}
		return envelope(map[string]any{
			"type":      "workspace_created",
			"workspace": createdWorkspace,
			"tab":       createdTab,
			"root_pane": createdPane,
		}), nil
	case "tab create":
		values, err := exactFlags(rest, []string{"--workspace", "--cwd", "--label"}, true)
		if err != nil {
			return "", err
		}
		var createdPane Pane
		var createdTab Tab
		_, err = store.update(func(s *Scenario) error {
			tabID := fmt.Sprintf("tab-%d", s.Sequence)
			paneID := fmt.Sprintf("pane-%d", s.Sequence)
			createdTab = Tab{ID: tabID, WorkspaceID: values["--workspace"], Label: values["--label"], Cwd: values["--cwd"]}
			createdPane = Pane{ID: paneID, TabID: tabID, WorkspaceID: values["--workspace"], Cwd: values["--cwd"], Status: "idle"}
			s.Tabs = append(s.Tabs, createdTab)
			s.Panes = append(s.Panes, createdPane)
			return nil
		})
		if err != nil {
			return "", err
		}
		return envelope(map[string]any{
			"type":      "tab_created",
			"tab":       createdTab,
			"root_pane": createdPane,
		}), nil
	case "tab list":
		if err := exactLen(rest, 0); err != nil {
			return "", err
		}
		return envelope(map[string]any{"tabs": scenario.Tabs}), nil
	case "tab rename":
		if err := exactLen(rest, 2); err != nil {
			return "", err
		}
		_, err := store.update(func(s *Scenario) error {
			for i := range s.Tabs {
				if s.Tabs[i].ID == rest[0] {
					s.Tabs[i].Label = rest[1]
				}
			}
			return nil
		})
		return envelope(map[string]any{"tab_id": rest[0]}), err
	case "agent prompt":
		if err := exactLen(rest, 2); err != nil {
			return "", err
		}
		return envelope(map[string]any{"pane_id": rest[0]}), nil
	case "agent rename":
		if err := exactLen(rest, 2); err != nil {
			return "", err
		}
		_, err := store.update(func(s *Scenario) error {
			for i := range s.Panes {
				if s.Panes[i].ID == rest[0] {
					s.Panes[i].Name = rest[1]
				}
			}
			info := s.Agents[rest[0]]
			info.Name = rest[1]
			s.Agents[rest[0]] = info
			return nil
		})
		return envelope(map[string]any{"pane_id": rest[0]}), err
	case "agent get":
		if err := exactLen(rest, 1); err != nil {
			return "", err
		}
		info, ok := scenario.Agents[rest[0]]
		if !ok {
			return "", errors.New("agent is not live")
		}
		return envelope(info), nil
	case "agent start":
		if len(rest) != 7 || rest[1] != "--kind" || rest[3] != "--pane" || rest[5] != "--timeout" {
			return "", errors.New("agent start schema: <name> --kind <kind> --pane <pane> --timeout <milliseconds>")
		}
		if rest[0] == "" || rest[2] == "" || rest[4] == "" {
			return "", errors.New("agent start values must be non-empty")
		}
		if _, err := strconv.Atoi(rest[6]); err != nil {
			return "", errors.New("agent start timeout must be an integer")
		}
		var started Agent
		_, err := store.update(func(s *Scenario) error {
			if s.Agents == nil {
				s.Agents = make(map[string]Agent)
			}
			started = Agent{PaneID: rest[4], Agent: rest[2], Name: rest[0], Status: "idle", Running: true}
			s.Agents[rest[4]] = started
			for i := range s.Panes {
				if s.Panes[i].ID == rest[4] {
					s.Panes[i].Agent = rest[2]
					s.Panes[i].Name = rest[0]
				}
			}
			return nil
		})
		return envelope(map[string]any{"type": "agent_started", "agent": started}), err
	default:
		return "", fmt.Errorf("unknown command %q", group+" "+command)
	}
}

func validatePaneRead(args []string) error {
	if len(args) != 7 {
		return errors.New("pane read schema: <pane> --lines <n> --source recent-unwrapped --format <text|ansi>")
	}
	if args[1] != "--lines" || args[3] != "--source" || args[4] != "recent-unwrapped" || args[5] != "--format" {
		return errors.New("pane read flags are missing, duplicated, or out of order")
	}
	if _, err := strconv.Atoi(args[2]); err != nil {
		return errors.New("pane read lines must be an integer")
	}
	if args[6] != "text" && args[6] != "ansi" {
		return errors.New("pane read format must be text or ansi")
	}
	return nil
}

func exactLen(args []string, want int) error {
	if len(args) != want {
		return fmt.Errorf("expected %d arguments, got %d", want, len(args))
	}
	return nil
}

func exactFlags(args, flags []string, noFocus bool) (map[string]string, error) {
	want := len(flags) * 2
	if noFocus {
		want++
	}
	if len(args) != want {
		return nil, fmt.Errorf("expected %d flag arguments, got %d", want, len(args))
	}
	values := make(map[string]string, len(flags))
	at := 0
	for _, flag := range flags {
		if args[at] != flag || at+1 >= len(args) || args[at+1] == "" {
			return nil, fmt.Errorf("expected %s followed by a value", flag)
		}
		if _, exists := values[flag]; exists {
			return nil, fmt.Errorf("duplicate flag %s", flag)
		}
		values[flag] = args[at+1]
		at += 2
	}
	if noFocus && args[at] != "--no-focus" {
		return nil, errors.New("expected terminal --no-focus flag")
	}
	return values, nil
}

func envelope(result any) string {
	data, _ := json.Marshal(map[string]any{"result": result})
	return string(data)
}

func removePane(panes []Pane, id string) []Pane {
	out := panes[:0]
	for _, pane := range panes {
		if pane.ID != id {
			out = append(out, pane)
		}
	}
	return out
}

func openStateStore() (*stateStore, error) {
	scenarioPath := os.Getenv("FAKE_HERDR_SCENARIO")
	statePath := os.Getenv("FAKE_HERDR_STATE")
	if statePath == "" && scenarioPath != "" {
		statePath = scenarioPath + ".state"
	}
	if statePath == "" {
		return nil, errors.New("FAKE_HERDR_SCENARIO or FAKE_HERDR_STATE is required")
	}
	if _, err := os.Stat(statePath); errors.Is(err, os.ErrNotExist) {
		initial := []byte(`{}`)
		if scenarioPath != "" {
			data, readErr := os.ReadFile(scenarioPath)
			if readErr != nil {
				return nil, fmt.Errorf("read scenario: %w", readErr)
			}
			initial = data
		}
		if err := os.MkdirAll(filepath.Dir(statePath), 0o700); err != nil {
			return nil, err
		}
		f, createErr := os.OpenFile(statePath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
		if createErr == nil {
			_, createErr = f.Write(initial)
			closeErr := f.Close()
			if createErr != nil {
				return nil, createErr
			}
			if closeErr != nil {
				return nil, closeErr
			}
		} else if !errors.Is(createErr, os.ErrExist) {
			return nil, createErr
		}
	}
	return &stateStore{path: statePath}, nil
}

func (s *stateStore) update(fn func(*Scenario) error) (Scenario, error) {
	lock, err := os.OpenFile(s.path+".lock", os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return Scenario{}, err
	}
	defer lock.Close()
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX); err != nil {
		return Scenario{}, err
	}
	defer syscall.Flock(int(lock.Fd()), syscall.LOCK_UN)

	data, err := os.ReadFile(s.path)
	if err != nil {
		return Scenario{}, err
	}
	var scenario Scenario
	if err := json.Unmarshal(data, &scenario); err != nil {
		return Scenario{}, fmt.Errorf("invalid state: %w", err)
	}
	if scenario.Agents == nil {
		scenario.Agents = make(map[string]Agent)
	}
	if scenario.Content == nil {
		scenario.Content = make(map[string]string)
	}
	if scenario.Responses == nil {
		scenario.Responses = make(map[string]string)
	}
	if scenario.Injections == nil {
		scenario.Injections = make(map[string]Injection)
	}
	if err := fn(&scenario); err != nil {
		return Scenario{}, err
	}
	encoded, err := json.MarshalIndent(scenario, "", "  ")
	if err != nil {
		return Scenario{}, err
	}
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, encoded, 0o600); err != nil {
		return Scenario{}, err
	}
	if err := os.Rename(tmp, s.path); err != nil {
		return Scenario{}, err
	}
	return scenario, nil
}

func recordOperation(sequence uint64, argv []string, started, completed time.Time, outcome, injected string) {
	path := os.Getenv("FAKE_HERDR_OPERATIONS")
	if path == "" {
		return
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return
	}
	lock, err := os.OpenFile(path+".lock", os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return
	}
	defer lock.Close()
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX); err != nil {
		return
	}
	defer syscall.Flock(int(lock.Fd()), syscall.LOCK_UN)

	op := Operation{
		Sequence: sequence,
		Argv:     append([]string(nil), argv...),
		Environment: map[string]string{
			"HERDR_SOCKET_PATH": os.Getenv("HERDR_SOCKET_PATH"),
		},
		StartedAt: started.Format(time.RFC3339Nano),
		Outcome:   outcome,
		Injected:  injected,
	}
	if !completed.IsZero() {
		op.CompletedAt = completed.Format(time.RFC3339Nano)
	}
	var operations []Operation
	if data, readErr := os.ReadFile(path); readErr == nil {
		for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
			if line == "" {
				continue
			}
			var existing Operation
			if json.Unmarshal([]byte(line), &existing) == nil {
				operations = append(operations, existing)
			}
		}
	}
	replaced := false
	for index := range operations {
		if operations[index].Sequence == sequence {
			operations[index] = op
			replaced = true
			break
		}
	}
	if !replaced {
		operations = append(operations, op)
	}
	temp := path + ".tmp." + strconv.FormatUint(sequence, 10)
	file, err := os.OpenFile(temp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return
	}
	encoder := json.NewEncoder(file)
	for _, operation := range operations {
		if err := encoder.Encode(operation); err != nil {
			_ = file.Close()
			_ = os.Remove(temp)
			return
		}
	}
	if err := file.Close(); err != nil {
		_ = os.Remove(temp)
		return
	}
	_ = os.Rename(temp, path)
}

func waitBarrier(name string, sequence uint64) {
	root := os.Getenv("FAKE_HERDR_BARRIER_DIR")
	if root == "" {
		fatal("barrier %q requested without FAKE_HERDR_BARRIER_DIR", name)
	}
	if filepath.Base(name) != name || name == "." || name == ".." {
		fatal("invalid barrier name %q", name)
	}
	if err := os.MkdirAll(root, 0o700); err != nil {
		fatal("create barrier directory: %v", err)
	}
	arrived := filepath.Join(root, fmt.Sprintf("%s.%d.arrived", name, sequence))
	if err := os.WriteFile(arrived, []byte("arrived\n"), 0o600); err != nil {
		fatal("signal barrier %q: %v", name, err)
	}
	release := filepath.Join(root, name+".release")
	for {
		if _, err := os.Stat(release); err == nil {
			return
		} else if !errors.Is(err, os.ErrNotExist) {
			fatal("read barrier %q: %v", name, err)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func runControlServer() error {
	store, err := openStateStore()
	if err != nil {
		return err
	}
	path := os.Getenv("FAKE_HERDR_CONTROL")
	if path == "" {
		return errors.New("FAKE_HERDR_CONTROL is required")
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
		go handleControl(store, conn)
	}
}

func handleControl(store *stateStore, conn net.Conn) {
	defer conn.Close()
	scanner := bufio.NewScanner(conn)
	for scanner.Scan() {
		var command ControlCmd
		if err := json.Unmarshal(scanner.Bytes(), &command); err != nil {
			writeControl(conn, nil, err)
			continue
		}
		state, err := store.update(func(s *Scenario) error {
			switch command.Cmd {
			case "set_status":
				for i := range s.Panes {
					if s.Panes[i].ID == command.Pane {
						s.Panes[i].Status = command.Status
						return nil
					}
				}
				return errors.New("pane not found")
			case "add_pane":
				s.Panes = append(s.Panes, Pane{ID: command.Pane, Agent: command.Agent, Name: command.Name, Status: command.Status, WorkspaceID: command.WorkspaceID, Cwd: command.Cwd})
			case "remove_pane":
				s.Panes = removePane(s.Panes, command.Pane)
			case "add_workspace":
				if command.Workspace == nil {
					return errors.New("workspace is required")
				}
				s.Workspaces = append(s.Workspaces, *command.Workspace)
			case "set_agent":
				if command.AgentInfo == nil {
					return errors.New("agent_info is required")
				}
				s.Agents[command.Pane] = *command.AgentInfo
			case "set_content":
				if command.Pane == "" {
					return errors.New("pane is required")
				}
				s.Content[command.Pane] = command.Content
			case "inject":
				if command.Command == "" || command.Injection == nil {
					return errors.New("command and injection are required")
				}
				s.Injections[command.Command] = *command.Injection
			case "clear_injection":
				delete(s.Injections, command.Command)
			case "release_barrier":
				if filepath.Base(command.Barrier) != command.Barrier || command.Barrier == "" ||
					command.Barrier == "." || command.Barrier == ".." {
					return errors.New("valid barrier is required")
				}
				root := os.Getenv("FAKE_HERDR_BARRIER_DIR")
				if root == "" {
					return errors.New("FAKE_HERDR_BARRIER_DIR is required")
				}
				if err := os.MkdirAll(root, 0o700); err != nil {
					return err
				}
				return os.WriteFile(filepath.Join(root, command.Barrier+".release"), []byte("released\n"), 0o600)
			case "snapshot":
			default:
				return fmt.Errorf("unknown control command %q", command.Cmd)
			}
			return nil
		})
		writeControl(conn, state, err)
	}
}

func writeControl(conn net.Conn, value any, err error) {
	if err != nil {
		_ = json.NewEncoder(conn).Encode(map[string]any{"ok": false, "error": err.Error()})
		return
	}
	_ = json.NewEncoder(conn).Encode(map[string]any{"ok": true, "state": value})
}

func fatal(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "fake-herdr: "+format+"\n", args...)
	os.Exit(1)
}
