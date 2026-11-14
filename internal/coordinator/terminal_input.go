package coordinator

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
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

func (d *Dispatcher) handleTerminalText(ctx context.Context, req commandRequest) *CommandResult {
	const action = "send_terminal_text"
	if req.paneID == "" || !validTerminalText(stringValue(req.message, "text")) || req.message["keys"] != nil {
		return d.fail(req.requestID, action, req.paneID, "Use a single line of text up to 65,536 UTF-8 bytes, without control characters; send Enter separately")
	}
	text := stringValue(req.message, "text")
	result := d.callPane(ctx, req, paneCall{
		action:      action,
		ledgerKey:   action + "\x00" + req.paneID + "\x00" + req.requestID,
		payloadHash: d.terminalTextConflictToken(text),
		run: func(callCtx context.Context) error {
			return d.herdr.SendInput(callCtx, req.paneID, herdr.PaneInput{Text: text})
		},
	})
	if result.OK && !result.replayed {
		d.recordActivity(action, "sent", "Terminal text delivered; Enter not sent", req.paneID, req.requestID)
		d.wake()
	}
	return result
}
