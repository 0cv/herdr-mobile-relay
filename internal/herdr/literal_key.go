package herdr

import (
	"context"
	"errors"
	"fmt"
	"strings"
)

var ErrUnsupportedLiteralKey = errors.New("unsupported_literal_key")

const FilterTextMaxLength = 32

func ValidateFilterText(text string) error {
	if len(text) == 0 || len(text) > FilterTextMaxLength || strings.HasSuffix(text, " ") {
		return fmt.Errorf("filter text must contain 1–32 supported characters and must not end with a space")
	}
	for _, character := range text {
		if _, err := literalKeyName(string(character)); err != nil {
			return err
		}
	}
	return nil
}

func literalKeyName(character string) (string, error) {
	if character == " " {
		return "Space", nil
	}
	if len(character) != 1 || !strings.ContainsRune("abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789.-_/+:()[]", rune(character[0])) {
		return "", ErrUnsupportedLiteralKey
	}
	return character, nil
}

// SendLiteralKey sends one literal character without retrying. An acknowledgement
// confirms transport only; callers must check that the picker consumed the key.
func (c *Client) SendLiteralKey(ctx context.Context, paneID, character string) error {
	key, err := literalKeyName(character)
	if err != nil {
		return err
	}
	if c == nil || c.api == nil || paneID == "" {
		return fmt.Errorf("%w: pane is required", ErrInvalidPaneInput)
	}
	if err := ctx.Err(); err != nil {
		return errors.Join(ErrNotStarted, err)
	}
	api := c.api
	api.mu.Lock()
	defer api.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return errors.Join(ErrNotStarted, err)
	}
	_ = api.closeLocked()
	if err := api.connect(ctx); err != nil {
		return errors.Join(ErrNotStarted, err)
	}
	response, wrote, err := api.requestConnected(ctx, "pane.send_keys", map[string]any{
		"pane_id": paneID,
		"keys":    []string{key},
	})
	_ = api.closeLocked()
	if err != nil {
		if wrote {
			return errors.Join(ErrDispatchedUnknown, err)
		}
		return errors.Join(ErrNotStarted, err)
	}
	if response.Result.Type != "ok" {
		return errors.Join(ErrDispatchedUnknown, fmt.Errorf("Herdr socket API returned %q for pane.send_keys", response.Result.Type))
	}
	return nil
}
