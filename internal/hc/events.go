package hc

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"net"
	"strings"
	"sync"
	"time"

	"github.com/0cv/herdr-mobile-relay/internal/herdr"
)

const eventReadTimeout = 45 * time.Second

type topologyStream struct {
	client    *Client
	conn      net.Conn
	reader    *bufio.Reader
	stop      func() bool
	closeOnce sync.Once
	closeErr  error
}

func (c *Client) Events(ctx context.Context) (herdr.TopologySnapshot, herdr.TopologyStream, error) {
	conn, err := (&net.Dialer{}).DialContext(ctx, "unix", c.socketPath)
	if err != nil {
		return herdr.TopologySnapshot{}, nil, err
	}
	s := &topologyStream{client: c, conn: conn, reader: bufio.NewReader(conn)}
	s.stop = context.AfterFunc(ctx, func() { conn.Close() })
	success := false
	defer func() {
		if !success {
			s.Close()
		}
	}()
	deadline := time.Now().Add(15 * time.Second)
	if d, ok := ctx.Deadline(); ok && d.Before(deadline) {
		deadline = d
	}
	if err := conn.SetDeadline(deadline); err != nil {
		return herdr.TopologySnapshot{}, nil, err
	}
	if err := hello(ctx, conn, s.reader); err != nil {
		return herdr.TopologySnapshot{}, nil, err
	}
	if _, _, err := request(ctx, conn, s.reader, "subscribe", "events.subscribe", map[string]any{}, false); err != nil {
		return herdr.TopologySnapshot{}, nil, err
	}
	if err := conn.SetDeadline(time.Time{}); err != nil {
		return herdr.TopologySnapshot{}, nil, err
	}
	c.mu.Lock()
	reconnect := c.eventsStarted
	c.eventsStarted = true
	c.mu.Unlock()
	if reconnect {
		// A gap can hide a terminal replacement even when its PID is unchanged.
		c.bumpAllEpochs()
	}
	snapshot, err := c.snapshot(ctx)
	if err != nil {
		return herdr.TopologySnapshot{}, nil, err
	}
	success = true
	return snapshot, s, nil
}

func (c *Client) snapshot(ctx context.Context) (herdr.TopologySnapshot, error) {
	spaces, tabs, panes, err := c.topology(ctx)
	return herdr.TopologySnapshot{Workspaces: spaces, Tabs: tabs, Panes: panes}, err
}

func (s *topologyStream) Next(ctx context.Context) (herdr.TopologySnapshot, error) {
	stop := context.AfterFunc(ctx, func() { s.conn.Close() })
	defer stop()
	fail := func(err error) (herdr.TopologySnapshot, error) {
		s.client.bumpAllEpochs()
		return herdr.TopologySnapshot{}, err
	}
	for {
		if err := ctx.Err(); err != nil {
			return fail(err)
		}
		deadline := time.Now().Add(eventReadTimeout)
		if d, ok := ctx.Deadline(); ok && d.Before(deadline) {
			deadline = d
		}
		if err := s.conn.SetReadDeadline(deadline); err != nil {
			return fail(err)
		}
		line, err := s.reader.ReadBytes('\n')
		if err != nil {
			return fail(err)
		}
		var notification struct {
			JSONRPC string `json:"jsonrpc"`
			Method  string `json:"method"`
			Params  struct {
				Type     string `json:"type"`
				Terminal string `json:"terminal"`
				Seq      int    `json:"seq"`
			} `json:"params"`
		}
		if err := json.Unmarshal(line, &notification); err != nil {
			return fail(err)
		}
		if notification.JSONRPC == "2.0" && notification.Method == "heartbeat" {
			continue
		}
		if notification.JSONRPC != "2.0" || notification.Method != "event" || notification.Params.Type == "" {
			return fail(errors.New("invalid core event notification"))
		}
		event := notification.Params
		if strings.Contains(event.Terminal, "/") {
			continue
		}
		if event.Type == "missed" {
			return fail(errors.New("core events missed"))
		}
		switch event.Type {
		case "lost", "restarted", "finished", "removed":
			s.client.bumpEpoch(event.Terminal)
		}
		if event.Terminal != "" {
			s.client.setRevision(event.Terminal, event.Seq)
		}
		return s.client.snapshot(ctx)
	}
}

func (s *topologyStream) Close() error {
	s.closeOnce.Do(func() {
		s.stop()
		s.closeErr = s.conn.Close()
	})
	return s.closeErr
}
