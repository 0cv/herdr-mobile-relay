package coordinator

import (
	"context"
	"encoding/json"
	"log/slog"
	"net"
	"strconv"
	"sync/atomic"
	"time"
)

type UDPEvent struct {
	Type        string `json:"type"`
	SocketPath  string `json:"socket_path"`
	PaneID      string `json:"pane_id"`
	TabID       string `json:"tab_id"`
	WorkspaceID string `json:"workspace_id"`
	Status      string `json:"status"`
	Agent       string `json:"agent"`
	UpdatedAt   string `json:"updated_at"`
}

type UDPListener struct {
	conn       *net.UDPConn
	state      *State
	logger     *slog.Logger
	socketPath string
	onChange   func(agent *AgentState)
	onDirty    func()
	received   atomic.Uint64
	invalid    atomic.Uint64
	resyncs    atomic.Uint64
}

type UDPMetrics struct {
	Received uint64 `json:"received"`
	Invalid  uint64 `json:"invalid"`
	Resyncs  uint64 `json:"resyncs"`
}

func NewUDPListener(addr string, state *State, socketPath string, logger *slog.Logger) (*UDPListener, error) {
	udpAddr, err := net.ResolveUDPAddr("udp", addr)
	if err != nil {
		return nil, err
	}
	conn, err := net.ListenUDP("udp", udpAddr)
	if err != nil {
		return nil, err
	}
	return &UDPListener{
		conn:       conn,
		state:      state,
		logger:     logger,
		socketPath: socketPath,
	}, nil
}

func (l *UDPListener) SetOnChange(fn func(agent *AgentState)) {
	l.onChange = fn
}

func (l *UDPListener) SetOnDirty(fn func()) {
	l.onDirty = fn
}

func (l *UDPListener) Run(ctx context.Context) {
	buf := make([]byte, 65536)
	for {
		select {
		case <-ctx.Done():
			l.conn.Close()
			return
		default:
		}

		l.conn.SetReadDeadline(time.Now().Add(1 * time.Second))
		n, _, err := l.conn.ReadFromUDP(buf)
		if err != nil {
			if netErr, ok := err.(net.Error); ok && netErr.Timeout() {
				continue
			}
			select {
			case <-ctx.Done():
				return
			default:
				l.logger.Warn("udp read error", "error", err)
				continue
			}
		}

		var event UDPEvent
		if err := json.Unmarshal(buf[:n], &event); err != nil {
			l.invalid.Add(1)
			l.logger.Debug("invalid udp event", "error", err)
			continue
		}

		if event.Type != "agent_event" {
			l.logger.Debug("ignoring non-agent udp event", "type", event.Type)
			continue
		}

		if l.socketPath != "" && event.SocketPath != l.socketPath {
			l.logger.Debug("ignoring udp event from different socket", "event_socket", event.SocketPath)
			continue
		}

		if event.PaneID == "" || event.Status == "" {
			l.invalid.Add(1)
			continue
		}
		l.received.Add(1)

		updatedAt := parseUpdatedAt(event.UpdatedAt)

		committed := l.state.CommitEventForSession(
			event.PaneID,
			event.TabID,
			event.WorkspaceID,
			event.Status,
			updatedAt,
		)
		l.logger.Debug("udp event committed",
			"pane", event.PaneID,
			"status", event.Status,
		)

		if l.onChange != nil && committed {
			if agent, ok := l.state.Agent(event.PaneID); ok {
				l.onChange(agent)
			}
		} else if !committed && l.onDirty != nil {
			l.resyncs.Add(1)
			l.onDirty()
		}
	}
}

func (l *UDPListener) Metrics() UDPMetrics {
	return UDPMetrics{
		Received: l.received.Load(),
		Invalid:  l.invalid.Load(),
		Resyncs:  l.resyncs.Load(),
	}
}

func (l *UDPListener) Close() error {
	return l.conn.Close()
}

func parseUpdatedAt(s string) int64 {
	if s == "" {
		return time.Now().UnixMilli()
	}
	if n, err := strconv.ParseInt(s, 10, 64); err == nil {
		return n
	}
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t.UnixMilli()
	}
	return time.Now().UnixMilli()
}
