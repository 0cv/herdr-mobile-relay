package hc

import (
	"context"
	"errors"
	"unicode"
	"unicode/utf8"

	"github.com/0cv/herdr-mobile-relay/internal/herdr"
)

func validateText(text string) error {
	if !utf8.ValidString(text) || text == "" {
		return herdr.ErrInvalidPaneInput
	}
	if len(text) > herdr.MaxPaneInputTextBytes {
		return herdr.ErrPaneInputTooLarge
	}
	for _, r := range text {
		if unicode.IsControl(r) || r == '\u2028' || r == '\u2029' {
			return &herdr.CLIError{Code: "invalid_params", Message: "Core can only type single-line text without control characters"}
		}
	}
	return nil
}

func (c *Client) SendInput(ctx context.Context, paneID string, input herdr.PaneInput) error {
	if len(input.Keys) != 0 {
		return unsupported()
	}
	return c.SendText(ctx, paneID, input.Text)
}

func (c *Client) SendText(ctx context.Context, paneID, text string) error {
	if err := validateText(text); err != nil {
		return err
	}
	return c.typeText(ctx, paneID, []byte(text))
}

func (c *Client) SendSecret(ctx context.Context, paneID, text string) error {
	if err := validateText(text); err != nil {
		return err
	}
	return c.typeText(ctx, paneID, []byte(text+"\r"))
}

func (c *Client) typeText(ctx context.Context, paneID string, data []byte) error {
	t, err := c.getTerminal(ctx, paneID)
	if err != nil {
		return err
	}
	identity := c.pane(*t).TerminalID
	a, err := c.attach(ctx, paneID, t.Cols, t.Rows)
	if err != nil {
		return errors.Join(herdr.ErrNotStarted, err)
	}
	defer a.Close()
	stop := context.AfterFunc(ctx, func() { a.Close() })
	defer stop()
	p, err := c.PaneGet(ctx, paneID)
	if err != nil {
		return err
	}
	if p.TerminalID != identity {
		return &herdr.CLIError{Code: "pane_not_found"}
	}
	if err := herdr.CheckDispatch(ctx); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return errors.Join(herdr.ErrNotStarted, err)
	}
	if err := a.input(data); err != nil {
		return err
	}
	if a.ended() || ctx.Err() != nil {
		return herdr.ErrDispatchedUnknown
	}
	if err := a.Close(); err != nil {
		return herdr.ErrDispatchedUnknown
	}
	return nil
}

func (a *attachment) input(data []byte) error {
	if err := a.write(a.channel, frameData, data); err != nil {
		return herdr.ErrDispatchedUnknown
	}
	return nil
}
