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

const (
	shiftModifier = 1 << iota
	altModifier
	ctrlModifier
)

func parseTerminalChord(key string) (string, int, bool) {
	parts := strings.Split(strings.ToLower(key), "+")
	base := parts[len(parts)-1]
	if base == "" {
		return "", 0, false
	}
	var modifiers int
	for _, part := range parts[:len(parts)-1] {
		var bit int
		switch part {
		case "shift":
			bit = shiftModifier
		case "alt":
			bit = altModifier
		case "ctrl":
			bit = ctrlModifier
		default:
			return "", 0, false
		}
		if modifiers&bit != 0 {
			return "", 0, false
		}
		modifiers |= bit
	}
	return base, modifiers, true
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
	lower, modifiers, ok := parseTerminalChord(value)
	if !ok {
		return "", ErrUnsupportedInputKey
	}
	base, ok := semanticBaseKeys[lower]
	if !ok {
		if len(lower) < 2 || lower[0] != 'f' {
			return "", ErrUnsupportedInputKey
		}
		var number int
		if _, err := fmt.Sscanf(lower, "f%d", &number); err != nil || number < 1 || number > 24 || lower != fmt.Sprintf("f%d", number) {
			return "", ErrUnsupportedInputKey
		}
		base = fmt.Sprintf("F%d", number)
	}
	var prefix string
	if modifiers&ctrlModifier != 0 {
		prefix += "ctrl+"
	}
	if modifiers&altModifier != 0 {
		prefix += "alt+"
	}
	if modifiers&shiftModifier != 0 {
		prefix += "shift+"
	}
	return prefix + base, nil
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
