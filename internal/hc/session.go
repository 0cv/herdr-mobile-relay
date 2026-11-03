package hc

import (
	"context"
	"strconv"
)

const SessionLabel = "herdr-mobile-relay.session"
const SessionPIDLabel = "herdr-mobile-relay.pid"

func (c *Client) ReportSession(ctx context.Context, terminal, session string, pid int) error {
	_, err := c.call(ctx, "terminal.label", map[string]any{
		"terminal": terminal,
		"labels":   map[string]string{SessionLabel: session, SessionPIDLabel: strconv.Itoa(pid)},
	})
	return err
}
