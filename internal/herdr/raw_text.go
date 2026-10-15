package herdr

import (
	"context"
	"errors"
	"fmt"
)

func (c *Client) SendRawText(ctx context.Context, paneID, text string) error {
	if c == nil || c.api == nil || paneID == "" || text == "" || len(text) > MaxPaneInputTextBytes {
		return errors.Join(ErrNotStarted, ErrInvalidPaneInput)
	}
	raw, wrote, err := c.api.requestUnary(ctx, "pane.send_text", map[string]any{
		"pane_id": paneID,
		"text":    text,
	})
	if err == nil {
		err = decodeSocketResult(raw, "ok", nil)
	}
	if err == nil {
		return nil
	}
	if wrote {
		return errors.Join(ErrDispatchedUnknown, err)
	}
	return errors.Join(ErrNotStarted, err)
}

func (c *Client) SocketPaneProcessInfo(ctx context.Context, paneID string) (*PaneProcessInfo, error) {
	if c == nil || c.api == nil || paneID == "" {
		return nil, errors.New("pane is required")
	}
	raw, _, err := c.api.requestUnary(ctx, "pane.process_info", map[string]any{"pane_id": paneID})
	if err != nil {
		return nil, err
	}
	var result struct {
		ProcessInfo *PaneProcessInfo `json:"process_info"`
	}
	if err := decodeSocketResult(raw, "pane_process_info", &result); err != nil {
		return nil, err
	}
	if result.ProcessInfo == nil || result.ProcessInfo.PaneID != paneID {
		return nil, fmt.Errorf("Herdr process info did not identify pane %q", paneID)
	}
	return result.ProcessInfo, nil
}
