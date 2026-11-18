package herdr

import (
	"context"
	"errors"
	"fmt"
	"strings"
)

const MaxPaneInputTextBytes = 64 * 1024

var (
	ErrInvalidPaneInput    = errors.New("invalid_pane_input")
	ErrUnsupportedInputKey = errors.New("unsupported_input_key")
	ErrPaneInputTooLarge   = errors.New("pane_input_too_large")
)

// PaneInput is the typed, paste-aware pane.send_input request. Text is passed
// through the official input method so Herdr can honor the pane's paste mode;
// Keys are semantic parser names, never raw terminal bytes or escape sequences.
type PaneInput struct {
	Text string
	Keys []string
}

var semanticBaseKeys = map[string]string{
	"enter":     "Enter",
	"esc":       "Esc",
	"tab":       "Tab",
	"backspace": "Backspace",
	"up":        "Up",
	"down":      "Down",
	"left":      "Left",
	"right":     "Right",
}

var supportedInputModifiers = map[string]string{
	"ctrl":  "ctrl",
	"alt":   "alt",
	"shift": "shift",
}

func ValidatePaneInput(input PaneInput) (PaneInput, error) {
	return normalizePaneInput(input)
}

func normalizePaneInput(input PaneInput) (PaneInput, error) {
	if input.Text == "" && len(input.Keys) == 0 {
		return PaneInput{}, ErrInvalidPaneInput
	}
	if len(input.Text) > MaxPaneInputTextBytes {
		return PaneInput{}, ErrPaneInputTooLarge
	}
	normalized := PaneInput{Text: input.Text, Keys: make([]string, len(input.Keys))}
	for index, key := range input.Keys {
		parsed, err := normalizeSemanticInputKey(key)
		if err != nil {
			return PaneInput{}, err
		}
		normalized.Keys[index] = parsed
	}
	return normalized, nil
}

func normalizeSemanticInputKey(value string) (string, error) {
	if value == "" || strings.TrimSpace(value) != value {
		return "", ErrUnsupportedInputKey
	}
	parts := strings.Split(value, "+")
	if len(parts) == 0 || len(parts) > 4 {
		return "", ErrUnsupportedInputKey
	}
	modifiers := make(map[string]bool, len(parts)-1)
	for _, part := range parts[:len(parts)-1] {
		modifier, ok := supportedInputModifiers[strings.ToLower(part)]
		if !ok || modifiers[modifier] {
			return "", ErrUnsupportedInputKey
		}
		modifiers[modifier] = true
	}
	base, ok := semanticBaseKeys[strings.ToLower(parts[len(parts)-1])]
	if !ok {
		lower := strings.ToLower(parts[len(parts)-1])
		if len(lower) < 2 || lower[0] != 'f' {
			return "", ErrUnsupportedInputKey
		}
		var number int
		if _, err := fmt.Sscanf(lower, "f%d", &number); err != nil || number < 1 || number > 24 || lower != fmt.Sprintf("f%d", number) {
			return "", ErrUnsupportedInputKey
		}
		base = fmt.Sprintf("F%d", number)
	}
	ordered := make([]string, 0, len(modifiers)+1)
	for _, modifier := range []string{"ctrl", "alt", "shift"} {
		if modifiers[modifier] {
			ordered = append(ordered, modifier)
		}
	}
	ordered = append(ordered, base)
	return strings.Join(ordered, "+"), nil
}

// SendInput dispatches exactly one official pane.send_input request. It does
// not retry: after request bytes reach Herdr, a missing response is ambiguous.
func (c *Client) SendInput(ctx context.Context, paneID string, input PaneInput) error {
	if c == nil || c.api == nil || paneID == "" {
		return fmt.Errorf("%w: pane is required", ErrInvalidPaneInput)
	}
	normalized, err := normalizePaneInput(input)
	if err != nil {
		return err
	}
	params := map[string]any{"pane_id": paneID}
	if normalized.Text != "" {
		params["text"] = normalized.Text
	}
	if len(normalized.Keys) > 0 {
		params["keys"] = normalized.Keys
	}

	err = c.api.requestResult(ctx, "pane.send_input", params, "ok", nil)
	var cliErr *CLIError
	// Herdr types the text before it validates the keys, so even a refusal may have applied.
	if errors.As(err, &cliErr) {
		return errors.Join(ErrDispatchedUnknown, err)
	}
	return err
}
