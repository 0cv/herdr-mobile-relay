package herdr

import (
	"context"
	"errors"
)

func (c *Client) SendSecret(ctx context.Context, paneID, text string) error {
	keys := make([]string, 0, len([]rune(text))+1)
	for _, value := range text {
		keys = append(keys, string(value))
	}
	keys = append(keys, "Enter")
	err := c.SendKeys(ctx, paneID, keys)
	if err == nil {
		return nil
	}
	if errors.Is(err, ErrNotStarted) {
		return ErrNotStarted
	}
	if IsRefused(err) {
		return &CLIError{Code: RefusalCode(err)}
	}
	return ErrDispatchedUnknown
}
