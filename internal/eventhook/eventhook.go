package eventhook

import (
	"encoding/json"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

type Event struct {
	Data EventData `json:"data"`
}

type EventData struct {
	PaneID       string `json:"pane_id"`
	TabID        string `json:"tab_id"`
	TabLabel     string `json:"tab_label"`
	TabName      string `json:"tab_name"`
	Label        string `json:"label"`
	TabNumber    *int   `json:"tab_number"`
	WorkspaceID  string `json:"workspace_id"`
	AgentStatus  string `json:"agent_status"`
	Agent        string `json:"agent"`
	DisplayAgent string `json:"display_agent"`
	Cwd          string `json:"cwd"`
}

type Payload struct {
	Type        string `json:"type"`
	SocketPath  string `json:"socket_path"`
	PaneID      string `json:"pane_id"`
	TabID       string `json:"tab_id"`
	TabLabel    string `json:"tab_label"`
	TabNumber   *int   `json:"tab_number"`
	WorkspaceID string `json:"workspace_id"`
	Status      string `json:"status"`
	Agent       string `json:"agent"`
	Project     string `json:"project"`
	Cwd         string `json:"cwd"`
	Host        string `json:"host"`
}

func Run() error {
	loadEnvironment()
	raw := os.Getenv("HERDR_PLUGIN_EVENT_JSON")
	if raw == "" {
		raw = "{}"
	}
	var event Event
	if err := json.Unmarshal([]byte(raw), &event); err != nil {
		return fmt.Errorf("parse HERDR_PLUGIN_EVENT_JSON: %w", err)
	}
	port := 8376
	if value := os.Getenv("HERDR_RELAY_PLUGIN_PORT"); value != "" {
		parsed, err := strconv.Atoi(value)
		if err != nil || parsed < 1 || parsed > 65535 {
			return fmt.Errorf("invalid HERDR_RELAY_PLUGIN_PORT %q", value)
		}
		port = parsed
	}
	payload, err := Build(event.Data)
	if err != nil {
		return err
	}
	data, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	connection, err := net.DialTimeout("udp", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)), time.Second)
	if err != nil {
		return fmt.Errorf("open event socket: %w", err)
	}
	defer connection.Close()
	_ = connection.SetWriteDeadline(time.Now().Add(time.Second))
	if _, err := connection.Write(data); err != nil {
		return fmt.Errorf("send event: %w", err)
	}
	return nil
}

func Build(data EventData) (Payload, error) {
	socketPath := os.Getenv("HERDR_SOCKET_PATH")
	if socketPath == "" {
		configHome := os.Getenv("XDG_CONFIG_HOME")
		if configHome == "" {
			home, err := os.UserHomeDir()
			if err != nil {
				return Payload{}, err
			}
			configHome = filepath.Join(home, ".config")
		}
		socketPath = filepath.Join(configHome, "herdr", "herdr.sock")
	}
	absoluteSocket, err := filepath.Abs(socketPath)
	if err != nil {
		return Payload{}, fmt.Errorf("resolve Herdr socket path: %w", err)
	}
	tabLabel := first(data.TabLabel, data.TabName, data.Label)
	agent := first(data.Agent, data.DisplayAgent)
	hostname, _ := os.Hostname()
	if short, _, found := strings.Cut(hostname, "."); found {
		hostname = short
	}
	return Payload{
		Type:        "agent_event",
		SocketPath:  absoluteSocket,
		PaneID:      data.PaneID,
		TabID:       data.TabID,
		TabLabel:    tabLabel,
		TabNumber:   data.TabNumber,
		WorkspaceID: data.WorkspaceID,
		Status:      strings.ToLower(data.AgentStatus),
		Agent:       strings.ToLower(agent),
		Project:     filepath.Base(data.Cwd),
		Cwd:         data.Cwd,
		Host:        hostname,
	}, nil
}

func loadEnvironment() {
	filename := os.Getenv("HERDR_RELAY_ENV")
	if filename == "" {
		if directory := os.Getenv("HERDR_PLUGIN_CONFIG_DIR"); directory != "" {
			filename = filepath.Join(directory, "relay.env")
		}
	}
	if filename == "" {
		return
	}
	data, err := os.ReadFile(filename)
	if err != nil {
		return
	}
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		key, value, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		key = strings.TrimSpace(key)
		value = strings.Trim(strings.TrimSpace(value), `"'`)
		if key == "" {
			continue
		}
		if _, exists := os.LookupEnv(key); !exists {
			_ = os.Setenv(key, value)
		}
	}
}

func first(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}
	return ""
}
