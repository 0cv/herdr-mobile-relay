package hc

import (
	"context"
	"encoding/json"
	"strings"
	"time"

	"github.com/0cv/herdr-mobile-relay/internal/herdr"
)

func (c *Client) terminalCall(ctx context.Context, method, paneID string, params map[string]any) (json.RawMessage, error) {
	if strings.Contains(paneID, "/") {
		return nil, &herdr.CLIError{Code: "pane_not_found"}
	}
	params["terminal"] = paneID
	return c.call(ctx, method, params)
}

func (c *Client) ReadPane(ctx context.Context, paneID string, lines int, format string) (herdr.PaneRead, error) {
	raw, err := c.terminalCall(ctx, "terminal.read", paneID, map[string]any{"lines": min(max(lines, 0), 10000)})
	if err != nil {
		return herdr.PaneRead{}, err
	}
	var result struct {
		Text     string `json:"text"`
		Complete bool   `json:"complete"`
	}
	if err := json.Unmarshal(raw, &result); err != nil {
		return herdr.PaneRead{}, err
	}
	return herdr.PaneRead{Content: []byte(result.Text), Truncated: !result.Complete}, nil
}

func (c *Client) ReadPaneRecent(ctx context.Context, paneID string, lines int, format string) (herdr.PaneRead, error) {
	return c.ReadPane(ctx, paneID, lines, format)
}

func (c *Client) ReadPaneVisible(ctx context.Context, paneID string, lines int, format string) (herdr.PaneRead, error) {
	return c.ReadPane(ctx, paneID, 0, format)
}

func (c *Client) ProbePaneVisible(ctx context.Context, paneID string, lines int, format string) ([]byte, error) {
	read, err := c.ReadPaneVisible(ctx, paneID, lines, format)
	return read.Content, err
}

func (c *Client) Prompt(ctx context.Context, paneID, text string) error {
	_, err := c.terminalCall(ctx, "terminal.send", paneID, map[string]any{"text": text, "force": true})
	return err
}

func uppercaseKey(key string) bool {
	return len(key) == 1 && key[0] >= 'A' && key[0] <= 'Z'
}

func (c *Client) SendKeys(ctx context.Context, paneID string, keys []string) error {
	translated := make([]string, len(keys))
	for i, key := range keys {
		translated[i] = strings.ToLower(key)
		if uppercaseKey(key) {
			translated[i] = "shift+" + translated[i]
		}
		if translated[i] == "escape" {
			translated[i] = "esc"
		}
	}
	_, err := c.terminalCall(ctx, "terminal.keys", paneID, map[string]any{"keys": translated})
	return err
}

func (c *Client) SendLiteralKey(ctx context.Context, paneID, character string) error {
	if uppercaseKey(character) {
		character = "shift+" + strings.ToLower(character)
	}
	if character == " " {
		character = "space"
	}
	_, err := c.terminalCall(ctx, "terminal.keys", paneID, map[string]any{"keys": []string{character}})
	return err
}

func (c *Client) StopPane(ctx context.Context, paneID string) error {
	_, err := c.terminalCall(ctx, "terminal.kill", paneID, map[string]any{})
	return err
}

func (c *Client) IntegrationStatus(context.Context) ([]byte, error) { return []byte("{}"), nil }

func (c *Client) Close() error { return nil }

func (c *Client) CapabilityStatus() herdr.ServerStatus {
	return herdr.ServerStatus{Features: map[string]herdr.FeatureEvidence{
		herdr.FeatureTabRename: {State: herdr.FeatureSupported, Reason: "Shared layout tab names available"},
		herdr.FeatureTabMove:   {State: herdr.FeatureSupported, Reason: "Shared layout tab ordering available"},
		herdr.FeaturePaneRead:  {State: herdr.FeatureSupported, Reason: "Screen reads available"},
	}}
}

func (c *Client) SetCapabilityChangeCallback(func(herdr.ServerStatus)) {}

func (c *Client) RunCapabilityRefresh(ctx context.Context, interval time.Duration) { <-ctx.Done() }
