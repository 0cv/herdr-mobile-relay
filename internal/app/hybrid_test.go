package app

import (
	"testing"
	"time"
)

// TestStringFieldPreservesSDPTerminator pins the signaling decode contract.
// SDP is line-oriented and its final terminator is significant; trimming it
// makes the relay answer no browser offer at all, which only shows up against
// a real browser. Keep this verbatim.
func TestStringFieldPreservesSDPTerminator(t *testing.T) {
	offer := "v=0\r\no=- 1 2 IN IP4 127.0.0.1\r\ns=-\r\nt=0 0\r\n"
	msg := map[string]any{"sdp": offer, "candidate": "candidate:1 1 udp 2130706431 192.0.2.1 4444 typ host"}

	if got := stringField(msg, "sdp"); got != offer {
		t.Fatalf("sdp = %q, want the offer verbatim %q", got, offer)
	}
	if got := stringField(msg, "candidate"); got != msg["candidate"] {
		t.Fatalf("candidate = %q, want it verbatim", got)
	}
	if got := stringField(msg, "missing"); got != "" {
		t.Fatalf("missing field = %q, want empty", got)
	}
}

func TestIntFieldAcceptsJSONNumbers(t *testing.T) {
	// Decoded JSON yields float64; hand-built maps may hold int.
	if got := intField(map[string]any{"sdp_mline_index": float64(3)}, "sdp_mline_index"); got != 3 {
		t.Fatalf("float index = %d, want 3", got)
	}
	if got := intField(map[string]any{"sdp_mline_index": 2}, "sdp_mline_index"); got != 2 {
		t.Fatalf("int index = %d, want 2", got)
	}
	if got := intField(map[string]any{"sdp_mline_index": "x"}, "sdp_mline_index"); got != 0 {
		t.Fatalf("non-numeric index = %d, want 0", got)
	}
}

// TestSignalBudgetLimitsNegotiation covers the per-client signaling rate limit
// required by plan section 9: a phone needs one offer per upgrade and one per
// network flip, so a tight loop must be refused rather than allowed to spawn
// peer connections.
func TestSignalBudgetLimitsNegotiation(t *testing.T) {
	h := &hybridTransport{signals: make(map[string]*signalBudget)}
	start := time.Now()

	if !h.allowSignal("client-1", start) {
		t.Fatal("first offer was refused")
	}
	if h.allowSignal("client-1", start.Add(signalMinInterval-time.Millisecond)) {
		t.Fatal("a re-offer inside the minimum interval was allowed")
	}
	now := start
	allowed := 1
	for range signalBurst * 2 {
		now = now.Add(signalMinInterval)
		if h.allowSignal("client-1", now) {
			allowed++
		}
	}
	if allowed != signalBurst {
		t.Fatalf("allowed %d offers in one window, want the %d burst", allowed, signalBurst)
	}
	// A different client has its own budget.
	if !h.allowSignal("client-2", now) {
		t.Fatal("a second client was charged the first client's budget")
	}
	// The window rolls over.
	if !h.allowSignal("client-1", now.Add(signalWindow)) {
		t.Fatal("budget did not refill after the window elapsed")
	}

	h.forgetClient("client-1")
	if _, ok := h.signals["client-1"]; ok {
		t.Fatal("disconnected client kept its budget entry")
	}
}

// TestHybridStatusReportsDisabled keeps /healthz honest on a relay with no
// gateway configured: the field is present and explicitly disabled rather than
// missing, so setup scripts can poll it unconditionally.
func TestHybridStatusReportsDisabled(t *testing.T) {
	var h *hybridTransport
	status := h.status()
	if status["enabled"] != false || status["registered"] != false {
		t.Fatalf("status = %v, want enabled/registered false", status)
	}
	if h.directEnabled() {
		t.Fatal("a relay without a gateway reported the direct path as available")
	}
}

// Every second a relay spends without a published mapping is a second in which
// an arriving phone can only use the relayed path, so an undiscovered mapping
// must retry within seconds of the gateway hello and never stall on the
// half-hourly self-test.
func TestReachabilityScheduleRetriesDiscoveryQuickly(t *testing.T) {
	next, retry := reachabilitySchedule(false, true, stunFirstRetry)
	if next != time.Second {
		t.Fatalf("first retry = %s, want 1s", next)
	}
	if retry != 2*time.Second {
		t.Fatalf("second retry = %s, want 2s", retry)
	}

	// Doubling is bounded: a gateway whose STUN datagrams are dropped must not
	// keep a relay probing forever at a high rate.
	retry = stunFirstRetry
	for range 12 {
		_, retry = reachabilitySchedule(false, true, retry)
	}
	if retry != stunRetryInterval {
		t.Fatalf("capped retry = %s, want %s", retry, stunRetryInterval)
	}

	// A learned mapping wakes every 10 seconds: well below an impatient NAT's
	// 30-second idle timeout, while also bounding relay-side network-change
	// discovery without platform-specific route monitors. A gateway with no
	// listener has nothing to keep alive and settles onto the self-test.
	next, retry = reachabilitySchedule(true, true, stunRetryInterval)
	if next != 10*time.Second || retry != stunFirstRetry {
		t.Fatalf("discovered schedule = %s/%s, want 10s/%s",
			next, retry, stunFirstRetry)
	}
	next, retry = reachabilitySchedule(false, false, stunRetryInterval)
	if next != selfTestInterval || retry != stunFirstRetry {
		t.Fatalf("no-listener schedule = %s/%s, want %s/%s",
			next, retry, selfTestInterval, stunFirstRetry)
	}
}

// TestSTUNURLUsesGatewayHostAndAdvertisedPort pins the trust boundary of
// address discovery. The host always comes from the URL this relay dialed, so
// a gateway can only ever reflect an address it already sees; only the port
// travels on the wire, and a port that is not a port disables discovery
// instead of being handed to the resolver.
func TestSTUNURLUsesGatewayHostAndAdvertisedPort(t *testing.T) {
	cases := []struct {
		name    string
		gateway string
		port    int
		want    string
	}{
		{"host comes from the dialed url", "wss://gw.example.com/relay", 3478, "stun:gw.example.com:3478"},
		{"websocket port is not reused", "wss://gw.example.com:8443/relay", 19302, "stun:gw.example.com:19302"},
		{"ipv6 literal stays bracketed", "wss://[2001:db8::1]:8443", 3478, "stun:[2001:db8::1]:3478"},
		{"absent port disables discovery", "wss://gw.example.com", 0, ""},
		{"out of range port disables discovery", "wss://gw.example.com", 70000, ""},
		{"negative port disables discovery", "wss://gw.example.com", -1, ""},
		{"unparsable gateway url disables discovery", "://gw.example.com", 3478, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := stunURL(tc.gateway, tc.port); got != tc.want {
				t.Fatalf("stunURL(%q, %d) = %q, want %q", tc.gateway, tc.port, got, tc.want)
			}
		})
	}
}
