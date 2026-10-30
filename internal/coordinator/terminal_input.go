package coordinator

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/0cv/herdr-mobile-relay/internal/herdr"
)

func validTerminalText(text string) bool {
	if text == "" || len(text) > herdr.MaxPaneInputTextBytes || !utf8.ValidString(text) {
		return false
	}
	for _, value := range text {
		if unicode.IsControl(value) || value == '\u2028' || value == '\u2029' {
			return false
		}
	}
	return true
}

func (d *Dispatcher) terminalTextConflictToken(text string) string {
	mac := hmac.New(sha256.New, []byte(d.terminalTextKey))
	mac.Write([]byte(text))
	return hex.EncodeToString(mac.Sum(nil))
}

func (d *Dispatcher) handleTerminalText(ctx context.Context, receivedAt time.Time, requestID, paneID string, message map[string]any) *CommandResult {
	const action = "send_terminal_text"
	if paneID == "" || !validTerminalText(stringValue(message, "text")) || message["keys"] != nil {
		return d.fail(requestID, action, paneID, "Use a single line of text up to 65,536 UTF-8 bytes, without control characters; send Enter separately")
	}
	text := stringValue(message, "text")
	result := d.schedule(ctx, ScheduleOptions{
		Command:     d.command(ctx, receivedAt, requestID, CommandTerminalText, paneID, commandDeadline, len(text)),
		LedgerKey:   action + "\x00" + paneID + "\x00" + requestID,
		PayloadHash: d.terminalTextConflictToken(text),
	}, EffectFunc(func(effectCtx context.Context, token WorkerToken) EffectResult {
		if stale := d.paneSessionCurrent(token, requestID, action); stale != nil {
			return EffectResult{Result: stale}
		}
		if err := d.herdr.SendInput(effectCtx, paneID, herdr.PaneInput{Text: text}); err != nil {
			return EffectResult{Result: d.failErr(requestID, action, paneID, err)}
		}
		return EffectResult{Result: completed(requestID, action, paneID, nil)}
	}))
	if result.OK && !result.replayed {
		d.recordActivity(action, "sent", "Terminal text delivered; Enter not sent", paneID, requestID)
		d.wake()
	}
	return result
}
