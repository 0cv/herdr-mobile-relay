package eventhook

import (
	"cmp"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/0cv/herdr-mobile-relay/internal/relayenv"
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

var relayEnvironmentKeys = []string{"HERDR_SOCKET_PATH", "HERDR_RELAY_PLUGIN_PORT", "XDG_CONFIG_HOME"}

func Run() error {
	if err := loadEnvironment(); err != nil {
		return err
	}
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
	tabLabel := cmp.Or(data.TabLabel, data.TabName, data.Label)
	agent := cmp.Or(data.Agent, data.DisplayAgent)
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

func loadEnvironment() error {
	values, err := relayenv.Read(relayEnvironmentKeys...)
	if err != nil {
		return err
	}
	for key, value := range values {
		if _, exists := os.LookupEnv(key); !exists {
			_ = os.Setenv(key, value)
		}
	}
	return nil
}
