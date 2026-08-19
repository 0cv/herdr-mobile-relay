package gateway

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/netip"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/0cv/herdr-mobile-relay/internal/gatewaywire"
)

// stunSilenceWindow is how long a test waits before it believes a datagram drew
// no reply. Everything here is loopback, so a real answer arrives in
// microseconds.
const stunSilenceWindow = 250 * time.Millisecond

// newSTUNHarness starts a gateway whose address-discovery listener is bound to a
// loopback address on an ephemeral port, and returns a socket connected to it.
func newSTUNHarness(t *testing.T, ip net.IP) (*harness, *net.UDPConn) {
	t.Helper()
	h := newHarness(t, Options{STUNAddr: net.JoinHostPort(ip.String(), "0")})
	if h.server.stunPort == 0 {
		t.Fatal("stun listener reported no port")
	}
	conn, err := net.DialUDP("udp", nil, &net.UDPAddr{IP: ip, Port: h.server.stunPort})
	if err != nil {
		t.Fatalf("dial stun listener: %v", err)
	}
	t.Cleanup(func() { conn.Close() })
	return h, conn
}

// buildBindingRequest builds a Binding Request carrying attrs verbatim, so a
// test can imitate the padded, attribute-bearing requests a real ICE agent
// sends as well as a bare header.
func buildBindingRequest(txID, attrs []byte) []byte {
	request := make([]byte, 0, stunHeaderSize+len(attrs))
	request = binary.BigEndian.AppendUint16(request, stunBindingRequest)
	request = binary.BigEndian.AppendUint16(request, uint16(len(attrs)))
	request = binary.BigEndian.AppendUint32(request, stunMagicCookie)
	request = append(request, txID...)
	return append(request, attrs...)
}

func sendSTUN(t *testing.T, conn *net.UDPConn, datagram []byte) {
	t.Helper()
	if _, err := conn.Write(datagram); err != nil {
		t.Fatalf("send stun datagram: %v", err)
	}
}

func readSTUN(t *testing.T, conn *net.UDPConn) ([]byte, error) {
	t.Helper()
	if err := conn.SetReadDeadline(time.Now().Add(stunSilenceWindow)); err != nil {
		t.Fatalf("set read deadline: %v", err)
	}
	buffer := make([]byte, stunMaxRequest)
	n, err := conn.Read(buffer)
	if err != nil {
		return nil, err
	}
	return buffer[:n], nil
}

func mustReadSTUN(t *testing.T, conn *net.UDPConn) []byte {
	t.Helper()
	response, err := readSTUN(t, conn)
	if err != nil {
		t.Fatalf("read stun response: %v", err)
	}
	return response
}

func expectNoSTUNReply(t *testing.T, conn *net.UDPConn, what string) {
	t.Helper()
	response, err := readSTUN(t, conn)
	if err == nil {
		t.Fatalf("%s drew a %d-byte reply, want silence", what, len(response))
	}
	if !errors.Is(err, os.ErrDeadlineExceeded) {
		t.Fatalf("%s: read failed with %v, want a deadline", what, err)
	}
}

// parseBindingSuccess validates a Binding Success Response against the request
// it answers and returns the reflected address plus every attribute type it
// carried, in order.
func parseBindingSuccess(t *testing.T, response, txID []byte) (netip.AddrPort, []uint16) {
	t.Helper()
	if len(response) < stunHeaderSize {
		t.Fatalf("response is %d bytes, shorter than a header", len(response))
	}
	if messageType := binary.BigEndian.Uint16(response[0:2]); messageType != stunBindingSuccess {
		t.Fatalf("response type is %#04x, want a binding success", messageType)
	}
	if cookie := binary.BigEndian.Uint32(response[4:8]); cookie != stunMagicCookie {
		t.Fatalf("response cookie is %#08x", cookie)
	}
	if !bytes.Equal(response[8:stunHeaderSize], txID) {
		t.Fatal("response transaction id does not echo the request")
	}
	if length := int(binary.BigEndian.Uint16(response[2:4])); stunHeaderSize+length != len(response) {
		t.Fatalf("response length field is %d for a %d-byte datagram", length, len(response))
	}

	var reflected netip.AddrPort
	var types []uint16
	for body := response[stunHeaderSize:]; len(body) > 0; {
		if len(body) < stunAttrHeaderSize {
			t.Fatalf("%d trailing bytes are not an attribute", len(body))
		}
		attrType := binary.BigEndian.Uint16(body[0:2])
		size := int(binary.BigEndian.Uint16(body[2:4]))
		padded := (size + 3) / 4 * 4
		if stunAttrHeaderSize+padded > len(body) {
			t.Fatalf("attribute %#04x claims %d bytes of %d", attrType, size, len(body))
		}
		if attrType == stunAttrXORMappedAddress {
			reflected = decodeMappedAddress(t, body[stunAttrHeaderSize:stunAttrHeaderSize+size], txID)
		}
		types = append(types, attrType)
		body = body[stunAttrHeaderSize+padded:]
	}
	if !reflected.IsValid() {
		t.Fatal("response carries no XOR-MAPPED-ADDRESS")
	}
	return reflected, types
}

// decodeMappedAddress undoes the XOR encoding the way an ICE agent does.
func decodeMappedAddress(t *testing.T, value, txID []byte) netip.AddrPort {
	t.Helper()
	if len(value) < 8 {
		t.Fatalf("XOR-MAPPED-ADDRESS is %d bytes", len(value))
	}
	if value[0] != 0 {
		t.Fatalf("XOR-MAPPED-ADDRESS reserved byte is %#02x", value[0])
	}
	switch family := value[1]; {
	case family == stunFamilyIPv4 && len(value) == 8:
	case family == stunFamilyIPv6 && len(value) == 20:
	default:
		t.Fatalf("XOR-MAPPED-ADDRESS family %#02x with %d bytes", family, len(value))
	}

	var pad [4 + stunTxIDSize]byte
	binary.BigEndian.PutUint32(pad[0:4], stunMagicCookie)
	copy(pad[4:], txID)

	address := make([]byte, len(value)-4)
	for i := range address {
		address[i] = value[4+i] ^ pad[i]
	}
	addr, ok := netip.AddrFromSlice(address)
	if !ok {
		t.Fatalf("reflected address is %d bytes", len(address))
	}
	return netip.AddrPortFrom(addr, binary.BigEndian.Uint16(value[2:4])^uint16(stunMagicCookie>>16))
}

// localAddrPort is the address the gateway must reflect back to conn.
func localAddrPort(t *testing.T, conn *net.UDPConn) netip.AddrPort {
	t.Helper()
	local, ok := conn.LocalAddr().(*net.UDPAddr)
	if !ok {
		t.Fatalf("local address is %T, want a UDP address", conn.LocalAddr())
	}
	return netip.AddrPortFrom(local.AddrPort().Addr().Unmap(), uint16(local.Port))
}

func TestSTUNReflectsIPv4Source(t *testing.T) {
	_, conn := newSTUNHarness(t, net.IPv4(127, 0, 0, 1))

	txID := randomBytes(t, stunTxIDSize)
	sendSTUN(t, conn, buildBindingRequest(txID, nil))

	reflected, _ := parseBindingSuccess(t, mustReadSTUN(t, conn), txID)
	if want := localAddrPort(t, conn); reflected != want {
		t.Fatalf("reflected address is %s, want %s", reflected, want)
	}
}

func TestSTUNReflectsIPv6Source(t *testing.T) {
	probe, err := net.ListenUDP("udp6", &net.UDPAddr{IP: net.IPv6loopback})
	if err != nil {
		t.Skipf("no IPv6 loopback on this host: %v", err)
	}
	probe.Close()

	_, conn := newSTUNHarness(t, net.IPv6loopback)

	txID := randomBytes(t, stunTxIDSize)
	sendSTUN(t, conn, buildBindingRequest(txID, nil))

	reflected, _ := parseBindingSuccess(t, mustReadSTUN(t, conn), txID)
	want := localAddrPort(t, conn)
	if reflected != want {
		t.Fatalf("reflected address is %s, want %s", reflected, want)
	}
	if !reflected.Addr().Is6() || reflected.Addr().Is4In6() {
		t.Fatalf("reflected address %s is not a plain IPv6 address", reflected)
	}
}

// TestSTUNIgnoresEverythingButBindingRequests is the anti-reflection contract:
// the port answers nothing it did not fully recognise, and it keeps working
// afterwards so garbage cannot take address discovery down.
func TestSTUNIgnoresEverythingButBindingRequests(t *testing.T) {
	_, conn := newSTUNHarness(t, net.IPv4(127, 0, 0, 1))

	txID := randomBytes(t, stunTxIDSize)
	badCookie := buildBindingRequest(txID, nil)
	binary.BigEndian.PutUint32(badCookie[4:8], stunMagicCookie^0xFF)

	wrongType := buildBindingRequest(txID, nil)
	binary.BigEndian.PutUint16(wrongType[0:2], stunBindingSuccess)

	lyingLength := buildBindingRequest(txID, nil)
	binary.BigEndian.PutUint16(lyingLength[2:4], 8)

	for _, testCase := range []struct {
		name     string
		datagram []byte
	}{
		{"a truncated header", buildBindingRequest(txID, nil)[:stunHeaderSize-1]},
		{"an empty datagram", nil},
		{"a bad magic cookie", badCookie},
		{"a non-request message type", wrongType},
		{"a length that overruns the datagram", lyingLength},
	} {
		sendSTUN(t, conn, testCase.datagram)
		expectNoSTUNReply(t, conn, testCase.name)
	}

	sendSTUN(t, conn, buildBindingRequest(txID, nil))
	if _, types := parseBindingSuccess(t, mustReadSTUN(t, conn), txID); len(types) == 0 {
		t.Fatal("the listener stopped answering valid requests after garbage")
	}
}

// TestSTUNResponseIsNeverAnAmplifier pins the size rule: a response may only
// exceed twice its request by the mandatory attribute the protocol demands, and
// the optional SOFTWARE attribute appears only when it fits inside the 2x
// budget.
func TestSTUNResponseIsNeverAnAmplifier(t *testing.T) {
	_, conn := newSTUNHarness(t, net.IPv4(127, 0, 0, 1))

	for _, attrBytes := range []int{0, 4, 24, 64} {
		txID := randomBytes(t, stunTxIDSize)
		request := buildBindingRequest(txID, make([]byte, attrBytes))
		sendSTUN(t, conn, request)
		response := mustReadSTUN(t, conn)

		_, types := parseBindingSuccess(t, response, txID)
		budget := 2 * len(request)
		if budget < stunMandatoryResponse {
			budget = stunMandatoryResponse
		}
		if len(response) > budget {
			t.Fatalf("a %d-byte request drew a %d-byte response, over the %d-byte budget",
				len(request), len(response), budget)
		}

		hasSoftware := false
		for _, attrType := range types {
			if attrType == stunAttrSoftware {
				hasSoftware = true
			}
		}
		// SOFTWARE is optional, so the rule is only that the decision is the
		// tight one: it is present whenever it fits inside the 2x budget, and
		// absent whenever it would not have.
		software := stunAttrHeaderSize + (len(stunSoftware)+3)/4*4
		if !hasSoftware && len(response)+software <= 2*len(request) {
			t.Fatalf("a %d-byte request had room for SOFTWARE but the response omitted it", len(request))
		}
	}
}

// TestSTUNGlobalCeilingDropsFloodAndRecovers covers what the per-source limiter
// structurally cannot see: a flood spread over thousands of forged source
// addresses, where every datagram sits comfortably inside its own source
// budget. The flood is charged directly because a test cannot forge source
// addresses on the wire, but the silence and the recovery are observed on the
// real listener.
func TestSTUNGlobalCeilingDropsFloodAndRecovers(t *testing.T) {
	ip := net.IPv4(127, 0, 0, 1)
	clock := &testClock{now: time.Now()}
	h := newHarness(t, Options{STUNAddr: net.JoinHostPort(ip.String(), "0"), Now: clock.get})
	if h.server.stunPort == 0 {
		t.Fatal("stun listener reported no port")
	}
	conn, err := net.DialUDP("udp", nil, &net.UDPAddr{IP: ip, Port: h.server.stunPort})
	if err != nil {
		t.Fatalf("dial stun listener: %v", err)
	}
	t.Cleanup(func() { conn.Close() })

	for i := range stunGlobalRate {
		forged := netip.AddrFrom4([4]byte{10, byte(i >> 16), byte(i >> 8), byte(i)})
		if !h.server.stunLimiter.allow(forged, clock.get()) {
			t.Fatalf("forged datagram %d was dropped below the global ceiling", i)
		}
	}

	// The window is full, so even a well-formed request from a source that has
	// spent nothing gets silence.
	sendSTUN(t, conn, buildBindingRequest(randomBytes(t, stunTxIDSize), nil))
	expectNoSTUNReply(t, conn, "a binding request over the global ceiling")

	// The window is fixed, not sliding: the next one starts a fresh budget.
	clock.advance(stunGlobalWindow)
	txID := randomBytes(t, stunTxIDSize)
	sendSTUN(t, conn, buildBindingRequest(txID, nil))
	reflected, _ := parseBindingSuccess(t, mustReadSTUN(t, conn), txID)
	if want := localAddrPort(t, conn); reflected != want {
		t.Fatalf("reflected address is %s, want %s", reflected, want)
	}
}

// testClock is a hand-wound clock. The STUN listener reads it from its own
// goroutine, so it is mutex guarded.
type testClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *testClock) get() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *testClock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

func TestSTUNListenerStopsWithTheServer(t *testing.T) {
	h, conn := newSTUNHarness(t, net.IPv4(127, 0, 0, 1))
	port := h.server.stunPort

	txID := randomBytes(t, stunTxIDSize)
	sendSTUN(t, conn, buildBindingRequest(txID, nil))
	parseBindingSuccess(t, mustReadSTUN(t, conn), txID)

	// Close cancels the server context, which is the only thing that unblocks
	// the read loop, and then waits for it.
	if err := h.server.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	// A connected UDP socket learns about the vanished port from ICMP, on either
	// the send or the following read depending on timing. Both are proof; an
	// answer would be the failure.
	if _, err := conn.Write(buildBindingRequest(txID, nil)); err == nil {
		if response, err := readSTUN(t, conn); err == nil {
			t.Fatalf("a closed listener answered with %d bytes", len(response))
		}
	}

	// The socket is really gone, not merely quiet.
	rebound, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: port})
	if err != nil {
		t.Fatalf("stun port %d is still bound after Close: %v", port, err)
	}
	rebound.Close()
}

// TestSTUNPortIsAdvertisedOnBothHelloPaths covers the only way a peer learns
// address discovery exists: the hello it already reads.
func TestSTUNPortIsAdvertisedOnBothHelloPaths(t *testing.T) {
	h, _ := newSTUNHarness(t, net.IPv4(127, 0, 0, 1))
	port := h.server.stunPort

	for _, route := range []string{"/relay", "/connect"} {
		if hello := readServerHello(t, h, route); hello.StunPort != port {
			t.Fatalf("%s hello advertises stun port %d, want %d", route, hello.StunPort, port)
		}
	}

	var report healthResponse
	fetchHealthz(t, h, &report)
	if report.STUNPort != port {
		t.Fatalf("healthz reports stun port %d, want %d", report.STUNPort, port)
	}
}

// TestSTUNDisabledStartsNoListener is the default: an empty address means no UDP
// socket at all, and every advertisement says so.
func TestSTUNDisabledStartsNoListener(t *testing.T) {
	h := newHarness(t, Options{STUNAddr: ""})

	if h.server.stunPort != 0 {
		t.Fatalf("disabled address discovery reports port %d", h.server.stunPort)
	}
	if h.server.stunLimiter != nil {
		t.Fatal("disabled address discovery still built a limiter, so a socket was opened")
	}
	for _, route := range []string{"/relay", "/connect"} {
		if hello := readServerHello(t, h, route); hello.StunPort != 0 {
			t.Fatalf("%s hello advertises stun port %d, want 0", route, hello.StunPort)
		}
	}

	var report healthResponse
	fetchHealthz(t, h, &report)
	if report.STUNPort != 0 {
		t.Fatalf("healthz reports stun port %d, want 0", report.STUNPort)
	}
}

// readServerHello reads the first control message on a route and closes the
// connection: neither hello path waits for anything before sending it.
func readServerHello(t *testing.T, h *harness, route string) controlMessage {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), testDeadline)
	defer cancel()
	conn, _, err := websocket.Dial(ctx, h.wsURL+route, nil)
	if err != nil {
		t.Fatalf("dial %s: %v", route, err)
	}
	defer conn.CloseNow()

	hello := readControl(t, conn)
	if hello.Type != gatewaywire.TypeServerHello {
		t.Fatalf("%s sent %+v, want a server hello", route, hello)
	}
	return hello
}

func fetchHealthz(t *testing.T, h *harness, into *healthResponse) {
	t.Helper()
	response, err := http.Get(h.url + "/healthz")
	if err != nil {
		t.Fatalf("get healthz: %v", err)
	}
	defer response.Body.Close()
	if err := json.NewDecoder(response.Body).Decode(into); err != nil {
		t.Fatalf("decode healthz: %v", err)
	}
}
