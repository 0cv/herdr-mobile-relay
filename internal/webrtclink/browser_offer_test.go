package webrtclink

import (
	"context"
	"strings"
	"testing"
)

// chromeDataChannelOffer is a verbatim offer captured from Chromium creating a
// `herdr-dc-v1` DataChannel with an empty ICE server list, exactly as the PWA
// does. It is kept byte-for-byte, trailing newline included: SDP is a
// line-oriented format whose final terminator is significant, and a signaling
// layer that trims it makes every parser fail at EOF. A live browser upgrade
// regressed on precisely that, so the fixture pins the real wire shape rather
// than a Pion-generated approximation.
const chromeDataChannelOffer = "v=0\r\n" +
	"o=- 1267785534327560929 2 IN IP4 127.0.0.1\r\n" +
	"s=-\r\n" +
	"t=0 0\r\n" +
	"a=group:BUNDLE 0\r\n" +
	"a=extmap-allow-mixed\r\n" +
	"a=msid-semantic: WMS\r\n" +
	"m=application 9 UDP/DTLS/SCTP webrtc-datachannel\r\n" +
	"c=IN IP4 0.0.0.0\r\n" +
	"a=ice-ufrag:LN66\r\n" +
	"a=ice-pwd:/yd1/7iRHbSoKfO3fCjf66Kw\r\n" +
	"a=ice-options:trickle\r\n" +
	"a=fingerprint:sha-256 D8:D7:7F:70:ED:38:3B:FC:6D:BF:63:C2:A6:69:7C:22:A9:8E:5C:24:39:35:3B:BF:C8:E1:E8:50:BB:51:CE:B1\r\n" +
	"a=setup:actpass\r\n" +
	"a=mid:0\r\n" +
	"a=sctp-port:5000\r\n" +
	"a=max-message-size:262144\r\n"

func TestManagerAnswersRealBrowserOffer(t *testing.T) {
	h := newHarness(t, 0, nil)

	key := SessionKey{ClientID: "client-1", RequestID: "req-chrome"}
	answer, err := h.manager.HandleOffer(context.Background(), key, chromeDataChannelOffer)
	if err != nil {
		t.Fatalf("browser offer was refused: %v", err)
	}
	if !strings.Contains(answer, "a=sctp-port:") {
		t.Fatalf("answer is not a data-channel answer: %q", answer)
	}
	if !strings.Contains(answer, "a=fingerprint:sha-256 ") {
		t.Fatal("answer carries no DTLS fingerprint to pin the certificate against")
	}
	if h.manager.SessionCount() != 1 {
		t.Fatalf("session count = %d, want 1", h.manager.SessionCount())
	}
	h.manager.CloseSession(key, "test complete")
	h.awaitSessionCount(t, 0)
}

// TestManagerRejectsOfferMissingLineTerminator documents the failure mode that
// trimming an SDP produces, so nobody re-introduces a TrimSpace on the way in.
func TestManagerRejectsOfferMissingLineTerminator(t *testing.T) {
	h := newHarness(t, 0, nil)

	key := SessionKey{ClientID: "client-1", RequestID: "req-trimmed"}
	trimmed := strings.TrimSpace(chromeDataChannelOffer)
	if _, err := h.manager.HandleOffer(context.Background(), key, trimmed); err == nil {
		t.Fatal("an offer stripped of its final line terminator was accepted")
	}
	h.awaitSessionCount(t, 0)
}
