package gateway

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/0cv/herdr-mobile-relay/internal/gatewaywire"
)

const (
	testRelayKey = "gateway-test-relay-key-0123456789"
	// testSecondRelayKey is a second identity, for the limits that span every
	// relay rather than one registration.
	testSecondRelayKey = "gateway-test-relay-key-second-001"
	testDeadline       = 5 * time.Second
)

// controlMessage is the union of every JSON control message the gateway sends,
// so a test can read one frame without knowing which it is.
type controlMessage struct {
	Type     string `json:"type"`
	Proto    int    `json:"proto"`
	Nonce    string `json:"nonce"`
	Code     string `json:"code"`
	Message  string `json:"message"`
	StunPort int    `json:"stun_port"`
	Version  string `json:"version"`
	Revision string `json:"revision"`
}

type muxFrame struct {
	op      byte
	connID  uint32
	payload []byte
}

type harness struct {
	t      *testing.T
	server *Server
	wsURL  string
	url    string
}

func newHarness(t *testing.T, opts Options) *harness {
	t.Helper()
	opts.Logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	server, err := New(opts)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	httpServer := httptest.NewServer(server.Handler())
	t.Cleanup(func() {
		if err := server.Close(); err != nil {
			t.Errorf("Close: %v", err)
		}
		httpServer.Close()
	})
	return &harness{
		t:      t,
		server: server,
		wsURL:  "ws" + strings.TrimPrefix(httpServer.URL, "http"),
		url:    httpServer.URL,
	}
}

func testIdentity(t *testing.T) (relayID string, rendezvous []byte) {
	t.Helper()
	return identityFor(t, testRelayKey)
}

// identityFor derives the public relay id and the rendezvous key a phone proves
// with, for one relay key.
func identityFor(t *testing.T, relayKey string) (relayID string, rendezvous []byte) {
	t.Helper()
	relayID, err := gatewaywire.DeriveRelayID(relayKey)
	if err != nil {
		t.Fatalf("DeriveRelayID: %v", err)
	}
	rendezvous, err = gatewaywire.DeriveRendezvousKey(relayKey)
	if err != nil {
		t.Fatalf("DeriveRendezvousKey: %v", err)
	}
	return relayID, rendezvous
}

func randomBytes(t *testing.T, n int) []byte {
	t.Helper()
	buf := make([]byte, n)
	if _, err := rand.Read(buf); err != nil {
		t.Fatalf("rand: %v", err)
	}
	return buf
}

func writeControl(t *testing.T, conn *websocket.Conn, value any) {
	t.Helper()
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatalf("marshal control message: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), testDeadline)
	defer cancel()
	if err := conn.Write(ctx, websocket.MessageText, data); err != nil {
		t.Fatalf("write control message: %v", err)
	}
}

func readControl(t *testing.T, conn *websocket.Conn) controlMessage {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), testDeadline)
	defer cancel()
	messageType, data, err := conn.Read(ctx)
	if err != nil {
		t.Fatalf("read control message: %v", err)
	}
	if messageType != websocket.MessageText {
		t.Fatalf("control message arrived as %v, want text", messageType)
	}
	var message controlMessage
	if err := json.Unmarshal(data, &message); err != nil {
		t.Fatalf("decode control message: %v", err)
	}
	return message
}

// testRelay is a computer relay: it registers, answers pings, and exposes every
// other multiplexed frame on a channel.
type testRelay struct {
	t      *testing.T
	conn   *websocket.Conn
	frames chan muxFrame
	closed chan error
}

// dialRelay registers a relay id and fails the test if the gateway refuses.
func (h *harness) dialRelay(relayID string) *testRelay {
	h.t.Helper()
	relay, refusal := h.register(relayID)
	if refusal != nil {
		h.t.Fatalf("registration refused with %s: %s", refusal.Code, refusal.Message)
	}
	return relay
}

// register performs the relay hello. Exactly one of the results is non-nil: a
// live link, or the refusal the gateway reported.
func (h *harness) register(relayID string) (*testRelay, *controlMessage) {
	h.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), testDeadline)
	defer cancel()
	conn, _, err := websocket.Dial(ctx, h.wsURL+"/relay", nil)
	if err != nil {
		h.t.Fatalf("dial relay: %v", err)
	}
	h.t.Cleanup(func() { conn.CloseNow() })
	conn.SetReadLimit(gatewaywire.HeaderSize + gatewaywire.MaxFramePayload)

	hello := readControl(h.t, conn)
	if hello.Type != gatewaywire.TypeServerHello || hello.Proto != gatewaywire.Proto {
		h.t.Fatalf("relay hello is %+v", hello)
	}
	writeControl(h.t, conn, gatewaywire.RegisterHello{
		Type:    gatewaywire.TypeRegister,
		Proto:   gatewaywire.Proto,
		RelayID: relayID,
	})

	reply := readControl(h.t, conn)
	switch reply.Type {
	case gatewaywire.TypeReady:
	case gatewaywire.TypeError:
		conn.CloseNow()
		return nil, &reply
	default:
		h.t.Fatalf("registration reply is %+v", reply)
		return nil, nil
	}

	relay := &testRelay{
		t:      h.t,
		conn:   conn,
		frames: make(chan muxFrame, 64),
		closed: make(chan error, 1),
	}
	go relay.readLoop()
	return relay, nil
}

func (r *testRelay) readLoop() {
	defer close(r.frames)
	for {
		messageType, data, err := r.conn.Read(context.Background())
		if err != nil {
			r.closed <- err
			return
		}
		if messageType != websocket.MessageBinary {
			r.closed <- fmt.Errorf("relay received %v, want binary", messageType)
			return
		}
		op, connID, payload, err := gatewaywire.DecodeFrame(data)
		if err != nil {
			r.closed <- err
			return
		}
		if op == gatewaywire.OpPing {
			if err := r.conn.Write(context.Background(), websocket.MessageBinary,
				gatewaywire.EncodeFrame(gatewaywire.OpPong, 0, nil)); err != nil {
				r.closed <- err
				return
			}
			continue
		}
		r.frames <- muxFrame{op: op, connID: connID, payload: payload}
	}
}

func (r *testRelay) expect(op byte) muxFrame {
	r.t.Helper()
	select {
	case frame, ok := <-r.frames:
		if !ok {
			r.t.Fatalf("relay link closed while waiting for opcode %d", op)
		}
		if frame.op != op {
			r.t.Fatalf("relay received opcode %d, want %d", frame.op, op)
		}
		return frame
	case <-time.After(testDeadline):
		r.t.Fatalf("timeout waiting for opcode %d", op)
		return muxFrame{}
	}
}

func (r *testRelay) write(op byte, connID uint32, payload []byte) {
	r.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), testDeadline)
	defer cancel()
	frame := gatewaywire.EncodeFrame(op, connID, payload)
	if err := r.conn.Write(ctx, websocket.MessageBinary, frame); err != nil {
		r.t.Fatalf("relay write: %v", err)
	}
}

func (r *testRelay) waitClosed() error {
	r.t.Helper()
	select {
	case err := <-r.closed:
		return err
	case <-time.After(testDeadline):
		r.t.Fatal("relay link was never closed")
		return nil
	}
}

// testPhone is one paired phone connection carrying bare binary frames.
type testPhone struct {
	t    *testing.T
	conn *websocket.Conn
}

// connect performs the phone hello. Exactly one of the results is non-nil: a
// live connection, or the refusal the gateway reported.
func (h *harness) connect(relayID string, rendezvous []byte) (*testPhone, *controlMessage) {
	h.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), testDeadline)
	defer cancel()
	conn, _, err := websocket.Dial(ctx, h.wsURL+"/connect", nil)
	if err != nil {
		h.t.Fatalf("dial connect: %v", err)
	}
	h.t.Cleanup(func() { conn.CloseNow() })
	conn.SetReadLimit(gatewaywire.MaxFramePayload)

	hello := readControl(h.t, conn)
	if hello.Type != gatewaywire.TypeServerHello || hello.Proto != gatewaywire.Proto {
		h.t.Fatalf("phone hello is %+v", hello)
	}
	nonce, err := base64.RawURLEncoding.DecodeString(hello.Nonce)
	if err != nil || len(nonce) != gatewaywire.NonceBytes {
		h.t.Fatalf("challenge %q is not %d base64url bytes", hello.Nonce, gatewaywire.NonceBytes)
	}
	writeControl(h.t, conn, gatewaywire.ConnectHello{
		Type:    gatewaywire.TypeConnect,
		Proto:   gatewaywire.Proto,
		RelayID: relayID,
		Proof: base64.RawURLEncoding.EncodeToString(
			gatewaywire.ConnectProof(rendezvous, relayID, nonce)),
	})

	reply := readControl(h.t, conn)
	switch reply.Type {
	case gatewaywire.TypeReady:
		return &testPhone{t: h.t, conn: conn}, nil
	case gatewaywire.TypeError:
		conn.CloseNow()
		return nil, &reply
	default:
		h.t.Fatalf("connect reply is %+v", reply)
		return nil, nil
	}
}

// mustConnect fails the test if the gateway refuses the connection.
func (h *harness) mustConnect(relayID string, rendezvous []byte) *testPhone {
	h.t.Helper()
	phone, refusal := h.connect(relayID, rendezvous)
	if refusal != nil {
		h.t.Fatalf("connect refused with %s: %s", refusal.Code, refusal.Message)
	}
	return phone
}

// mustRefuse fails the test if the gateway accepts the connection.
func (h *harness) mustRefuse(relayID string, rendezvous []byte) controlMessage {
	h.t.Helper()
	phone, refusal := h.connect(relayID, rendezvous)
	if refusal == nil {
		phone.conn.CloseNow()
		h.t.Fatal("connect was accepted, want refusal")
	}
	return *refusal
}

func (p *testPhone) send(payload []byte) {
	p.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), testDeadline)
	defer cancel()
	if err := p.conn.Write(ctx, websocket.MessageBinary, payload); err != nil {
		p.t.Fatalf("phone write: %v", err)
	}
}

func (p *testPhone) expectFrame(want []byte) {
	p.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), testDeadline)
	defer cancel()
	messageType, data, err := p.conn.Read(ctx)
	if err != nil {
		p.t.Fatalf("phone read: %v", err)
	}
	if messageType != websocket.MessageBinary {
		p.t.Fatalf("phone received %v, want binary", messageType)
	}
	if !bytes.Equal(data, want) {
		p.t.Fatalf("phone received %d bytes that differ from the %d sent", len(data), len(want))
	}
}

func (p *testPhone) expectClose() websocket.CloseError {
	p.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), testDeadline)
	defer cancel()
	_, _, err := p.conn.Read(ctx)
	var closeErr websocket.CloseError
	if !errors.As(err, &closeErr) {
		p.t.Fatalf("phone read error %v is not a WebSocket close", err)
	}
	return closeErr
}

// verifyOpen checks that the relay — the only holder of the rendezvous key —
// can authenticate the challenge the gateway forwarded, and returns the nonce.
func verifyOpen(t *testing.T, frame muxFrame, relayID string, rendezvous []byte) string {
	t.Helper()
	var open gatewaywire.OpenPayload
	if err := json.Unmarshal(frame.payload, &open); err != nil {
		t.Fatalf("decode open payload: %v", err)
	}
	nonce, err := base64.RawURLEncoding.DecodeString(open.Nonce)
	if err != nil {
		t.Fatalf("decode forwarded nonce: %v", err)
	}
	proof, err := base64.RawURLEncoding.DecodeString(open.Proof)
	if err != nil {
		t.Fatalf("decode forwarded proof: %v", err)
	}
	if !gatewaywire.VerifyConnectProof(rendezvous, relayID, nonce, proof) {
		t.Fatal("relay could not verify the challenge the gateway forwarded")
	}
	return open.Nonce
}

func TestRoundTripWithTwoClients(t *testing.T) {
	relayID, rendezvous := testIdentity(t)
	h := newHarness(t, Options{})
	relay := h.dialRelay(relayID)

	phoneA := h.mustConnect(relayID, rendezvous)
	openA := relay.expect(gatewaywire.OpOpen)
	phoneB := h.mustConnect(relayID, rendezvous)
	openB := relay.expect(gatewaywire.OpOpen)

	if openA.connID == 0 || openB.connID == 0 {
		t.Fatalf("connection ids must be non-zero, got %d and %d", openA.connID, openB.connID)
	}
	if openA.connID == openB.connID {
		t.Fatalf("both connections were given id %d", openA.connID)
	}
	if openB.connID <= openA.connID {
		t.Fatalf("connection ids must increase, got %d then %d", openA.connID, openB.connID)
	}
	nonceA := verifyOpen(t, openA, relayID, rendezvous)
	nonceB := verifyOpen(t, openB, relayID, rendezvous)
	if nonceA == nonceB {
		t.Fatal("both connections were challenged with the same nonce")
	}

	upA := randomBytes(t, 4096)
	phoneA.send(upA)
	frame := relay.expect(gatewaywire.OpData)
	if frame.connID != openA.connID {
		t.Fatalf("frame from phone A arrived on conn %d, want %d", frame.connID, openA.connID)
	}
	if !bytes.Equal(frame.payload, upA) {
		t.Fatal("frame from phone A was not relayed verbatim")
	}

	upB := randomBytes(t, 33)
	phoneB.send(upB)
	frame = relay.expect(gatewaywire.OpData)
	if frame.connID != openB.connID {
		t.Fatalf("frame from phone B arrived on conn %d, want %d", frame.connID, openB.connID)
	}
	if !bytes.Equal(frame.payload, upB) {
		t.Fatal("frame from phone B was not relayed verbatim")
	}

	// The downstream frames are read in order on each socket, so a crossed
	// conn_id would surface as a payload mismatch here.
	downA := randomBytes(t, 8192)
	downB := randomBytes(t, 128)
	relay.write(gatewaywire.OpData, openA.connID, downA)
	relay.write(gatewaywire.OpData, openB.connID, downB)
	phoneA.expectFrame(downA)
	phoneB.expectFrame(downB)

	relays, clients := h.server.counts()
	if relays != 1 || clients != 2 {
		t.Fatalf("healthz counts are %d relays and %d clients, want 1 and 2", relays, clients)
	}
}

func TestConnectUnknownRelayIsRefused(t *testing.T) {
	relayID, rendezvous := testIdentity(t)
	h := newHarness(t, Options{})

	refusal := h.mustRefuse(relayID, rendezvous)
	if refusal.Code != gatewaywire.CodeUnknownRelay {
		t.Fatalf("refusal code is %q, want %q", refusal.Code, gatewaywire.CodeUnknownRelay)
	}
}

func TestMaxClientsPerRelay(t *testing.T) {
	relayID, rendezvous := testIdentity(t)
	h := newHarness(t, Options{MaxClientsPerRelay: 2})
	relay := h.dialRelay(relayID)

	h.mustConnect(relayID, rendezvous)
	relay.expect(gatewaywire.OpOpen)
	h.mustConnect(relayID, rendezvous)
	relay.expect(gatewaywire.OpOpen)

	refusal := h.mustRefuse(relayID, rendezvous)
	if refusal.Code != gatewaywire.CodeTooManyClient {
		t.Fatalf("refusal code is %q, want %q", refusal.Code, gatewaywire.CodeTooManyClient)
	}
}

// TestMaxRelaysRefusesNewRegistrations covers the ceiling a shared instance
// needs on the registration table, and the reconnect it must never refuse.
func TestMaxRelaysRefusesNewRegistrations(t *testing.T) {
	firstID, _ := testIdentity(t)
	secondID, _ := identityFor(t, testSecondRelayKey)
	h := newHarness(t, Options{MaxRelays: 1})

	first := h.dialRelay(firstID)

	relay, refusal := h.register(secondID)
	if refusal == nil {
		relay.conn.CloseNow()
		t.Fatal("a second relay id registered although the gateway is full")
	}
	if refusal.Code != gatewaywire.CodeAtCapacity {
		t.Fatalf("refusal code is %q, want %q", refusal.Code, gatewaywire.CodeAtCapacity)
	}

	// A relay reconnecting under an id the table already holds replaces its
	// link instead of growing the table, so the ceiling must let it through.
	// The incumbent has to be gone first: a live one now wins its id.
	first.conn.CloseNow()
	h.dialRelay(firstID)
}

// TestGlobalMaxClientsSpansEveryRelay is the ceiling the per-relay cap cannot
// express: one phone too many is refused even when it names a relay that is
// nowhere near its own cap, and the slot comes back when a phone leaves.
func TestGlobalMaxClientsSpansEveryRelay(t *testing.T) {
	firstID, firstKey := testIdentity(t)
	secondID, secondKey := identityFor(t, testSecondRelayKey)
	h := newHarness(t, Options{MaxClients: 1})
	firstRelay := h.dialRelay(firstID)
	secondRelay := h.dialRelay(secondID)

	phone := h.mustConnect(firstID, firstKey)
	open := firstRelay.expect(gatewaywire.OpOpen)

	refusal := h.mustRefuse(secondID, secondKey)
	if refusal.Code != gatewaywire.CodeAtCapacity {
		t.Fatalf("refusal code is %q, want %q", refusal.Code, gatewaywire.CodeAtCapacity)
	}
	if _, clients := h.server.counts(); clients != 1 {
		t.Fatalf("the refused connection left the global count at %d, want 1", clients)
	}

	// The slot is released before the relay is told the connection ended, so
	// observing that close means capacity has already recovered.
	if err := phone.conn.Close(websocket.StatusNormalClosure, ""); err != nil {
		t.Fatalf("phone close: %v", err)
	}
	if closed := firstRelay.expect(gatewaywire.OpClose); closed.connID != open.connID {
		t.Fatalf("close arrived for conn %d, want %d", closed.connID, open.connID)
	}

	h.mustConnect(secondID, secondKey)
	secondRelay.expect(gatewaywire.OpOpen)
}

// TestCapacityLimitsFollowTheZeroAndNegativeConvention pins both halves of the
// Options contract for the two global ceilings.
func TestCapacityLimitsFollowTheZeroAndNegativeConvention(t *testing.T) {
	defaults := newHarness(t, Options{})
	if got := defaults.server.opts.MaxRelays; got != DefaultMaxRelays {
		t.Fatalf("zero MaxRelays became %d, want %d", got, DefaultMaxRelays)
	}
	if got := defaults.server.opts.MaxClients; got != DefaultMaxClients {
		t.Fatalf("zero MaxClients became %d, want %d", got, DefaultMaxClients)
	}

	h := newHarness(t, Options{MaxRelays: -1, MaxClients: -1})
	if h.server.opts.MaxRelays != -1 || h.server.opts.MaxClients != -1 {
		t.Fatalf("negative limits became %d relays and %d clients, want them left alone",
			h.server.opts.MaxRelays, h.server.opts.MaxClients)
	}

	// Both enforcement points are driven directly: proving a removed ceiling
	// needs more registrations and connections than sockets are worth, and
	// neither path touches the WebSocket it is handed until the link runs.
	links := make([]*relayLink, 0, DefaultMaxRelays+1)
	for i := range DefaultMaxRelays + 1 {
		link, _ := h.server.registerRelay(context.Background(), fmt.Sprintf("relay-%04d", i), nil)
		if link == nil {
			t.Fatalf("registration %d was refused although MaxRelays is negative", i)
		}
		links = append(links, link)
	}
	for _, link := range links {
		h.server.unregisterRelay(link)
		link.cancel()
	}

	for i := range DefaultMaxClients + 1 {
		if !h.server.reserveClient() {
			t.Fatalf("client %d was refused although MaxClients is negative", i)
		}
	}
	h.server.releaseClients(DefaultMaxClients + 1)
}

func TestConnectRateLimit(t *testing.T) {
	relayID, rendezvous := testIdentity(t)
	h := newHarness(t, Options{ConnectRatePerMinute: 2})
	relay := h.dialRelay(relayID)

	h.mustConnect(relayID, rendezvous)
	relay.expect(gatewaywire.OpOpen)
	h.mustConnect(relayID, rendezvous)
	relay.expect(gatewaywire.OpOpen)

	refusal := h.mustRefuse(relayID, rendezvous)
	if refusal.Code != gatewaywire.CodeRateLimited {
		t.Fatalf("refusal code is %q, want %q", refusal.Code, gatewaywire.CodeRateLimited)
	}
}

func expectNotice(t *testing.T, relay *testRelay, kind string) gatewaywire.NoticePayload {
	t.Helper()
	frame := relay.expect(gatewaywire.OpNotice)
	if frame.connID != 0 {
		t.Fatalf("notice arrived on conn %d, want 0", frame.connID)
	}
	var notice gatewaywire.NoticePayload
	if err := json.Unmarshal(frame.payload, &notice); err != nil {
		t.Fatalf("decode notice: %v", err)
	}
	if notice.Kind != kind {
		t.Fatalf("notice kind is %q, want %q", notice.Kind, kind)
	}
	return notice
}

func TestQuotaWarnsThenRefusesNewConnections(t *testing.T) {
	relayID, rendezvous := testIdentity(t)
	h := newHarness(t, Options{MonthlyBytes: 4096, QuotaWarnPercent: 50})
	relay := h.dialRelay(relayID)

	phone := h.mustConnect(relayID, rendezvous)
	relay.expect(gatewaywire.OpOpen)

	// Half the quota: the relay is warned exactly once.
	phone.send(randomBytes(t, 2048))
	relay.expect(gatewaywire.OpData)
	warning := expectNotice(t, relay, gatewaywire.NoticeQuotaWarning)
	if warning.QuotaBytes != 4096 || warning.RelayedBytes < 2048 {
		t.Fatalf("warning reports %d of %d bytes", warning.RelayedBytes, warning.QuotaBytes)
	}

	// Past the quota: one exceeded notice, and the connection keeps working.
	phone.send(randomBytes(t, 3000))
	relay.expect(gatewaywire.OpData)
	expectNotice(t, relay, gatewaywire.NoticeQuotaExceeded)

	refusal := h.mustRefuse(relayID, rendezvous)
	if refusal.Code != gatewaywire.CodeQuotaExceeded {
		t.Fatalf("refusal code is %q, want %q", refusal.Code, gatewaywire.CodeQuotaExceeded)
	}

	// The established connection still relays, and the refusal did not produce a
	// second notice: the next frame on the link is this data frame.
	survivor := randomBytes(t, 64)
	phone.send(survivor)
	frame := relay.expect(gatewaywire.OpData)
	if !bytes.Equal(frame.payload, survivor) {
		t.Fatal("established connection stopped relaying verbatim after the quota was exceeded")
	}

	down := randomBytes(t, 64)
	relay.write(gatewaywire.OpData, frame.connID, down)
	phone.expectFrame(down)
}

// A relay id is unguessable, but it is not a secret that never leaks: a shared
// QR or a compromised phone reveals the key it derives from. Displacing a link
// that is demonstrably alive would therefore hand anyone who learned an id a
// way to evict the real relay in a loop, so a live incumbent wins and the
// newcomer is told the id is busy.
func TestReRegistrationIsRefusedWhileTheIncumbentIsLive(t *testing.T) {
	relayID, rendezvous := testIdentity(t)
	h := newHarness(t, Options{})

	first := h.dialRelay(relayID)
	phone := h.mustConnect(relayID, rendezvous)
	first.expect(gatewaywire.OpOpen)

	rejected, refusal := h.register(relayID)
	if rejected != nil {
		rejected.conn.CloseNow()
	}
	if refusal == nil {
		t.Fatal("a second link displaced a relay that was still answering")
	}
	if refusal.Code != gatewaywire.CodeRelayBusy {
		t.Fatalf("refusal code is %q, want %q", refusal.Code, gatewaywire.CodeRelayBusy)
	}

	// The incumbent and its phone are untouched: traffic still flows.
	payload := randomBytes(t, 256)
	phone.send(payload)
	frame := first.expect(gatewaywire.OpData)
	if !bytes.Equal(frame.payload, payload) {
		t.Fatal("the surviving link stopped relaying verbatim")
	}
}

// The other half of the contract: a link that no longer answers must not keep
// its id hostage, or a crashed relay, a killed process, or a laptop that closed
// its lid could never reclaim it.
func TestReRegistrationReplacesADeadRelayLink(t *testing.T) {
	relayID, rendezvous := testIdentity(t)
	h := newHarness(t, Options{})

	first := h.dialRelay(relayID)
	phone := h.mustConnect(relayID, rendezvous)
	first.expect(gatewaywire.OpOpen)

	// Kill the incumbent's socket without a close handshake, which is what a
	// crash or a vanished network looks like from the gateway's side.
	first.conn.CloseNow()

	second := h.dialRelay(relayID)

	// Either path is correct and which one runs is a race: the dying link may
	// detach its own phones as it unwinds, or the replacement may displace them.
	// What matters is that the phone is detached and the id is reclaimed.
	phoneClose := phone.expectClose()
	if phoneClose.Reason != reasonRelayReplaced && phoneClose.Reason != reasonRelayGone {
		t.Fatalf("detached phone close reason is %q, want %q or %q",
			phoneClose.Reason, reasonRelayReplaced, reasonRelayGone)
	}

	// The replacement owns the relay id.
	replacement := h.mustConnect(relayID, rendezvous)
	open := second.expect(gatewaywire.OpOpen)
	verifyOpen(t, open, relayID, rendezvous)

	payload := randomBytes(t, 256)
	replacement.send(payload)
	frame := second.expect(gatewaywire.OpData)
	if !bytes.Equal(frame.payload, payload) {
		t.Fatal("replacement link did not relay verbatim")
	}
}

func TestRelayCloseReachesPhone(t *testing.T) {
	relayID, rendezvous := testIdentity(t)
	h := newHarness(t, Options{})
	relay := h.dialRelay(relayID)

	phone := h.mustConnect(relayID, rendezvous)
	open := relay.expect(gatewaywire.OpOpen)

	const reason = "handshake rejected"
	relay.write(gatewaywire.OpClose, open.connID, []byte(reason))

	closeErr := phone.expectClose()
	if closeErr.Code != websocket.StatusPolicyViolation {
		t.Fatalf("phone closed with %v, want %v", closeErr.Code, websocket.StatusPolicyViolation)
	}
	if closeErr.Reason != reason {
		t.Fatalf("phone close reason is %q, want %q", closeErr.Reason, reason)
	}
}

func TestPhoneCloseReachesRelay(t *testing.T) {
	relayID, rendezvous := testIdentity(t)
	h := newHarness(t, Options{})
	relay := h.dialRelay(relayID)

	phone := h.mustConnect(relayID, rendezvous)
	open := relay.expect(gatewaywire.OpOpen)

	if err := phone.conn.Close(websocket.StatusNormalClosure, ""); err != nil {
		t.Fatalf("phone close: %v", err)
	}
	frame := relay.expect(gatewaywire.OpClose)
	if frame.connID != open.connID {
		t.Fatalf("close arrived for conn %d, want %d", frame.connID, open.connID)
	}
}

func TestIdleTimeoutClosesPhone(t *testing.T) {
	relayID, rendezvous := testIdentity(t)
	h := newHarness(t, Options{IdleTimeout: 50 * time.Millisecond})
	relay := h.dialRelay(relayID)

	phone := h.mustConnect(relayID, rendezvous)
	open := relay.expect(gatewaywire.OpOpen)

	frame := relay.expect(gatewaywire.OpClose)
	if frame.connID != open.connID {
		t.Fatalf("idle close arrived for conn %d, want %d", frame.connID, open.connID)
	}
	if string(frame.payload) != reasonIdleTimeout {
		t.Fatalf("idle close reason is %q, want %q", frame.payload, reasonIdleTimeout)
	}
	closeErr := phone.expectClose()
	if closeErr.Reason != reasonIdleTimeout {
		t.Fatalf("phone close reason is %q, want %q", closeErr.Reason, reasonIdleTimeout)
	}
}

func TestConnectHelloValidation(t *testing.T) {
	relayID, _ := testIdentity(t)
	h := newHarness(t, Options{})
	h.dialRelay(relayID)

	cases := []struct {
		name  string
		hello gatewaywire.ConnectHello
	}{
		{
			name: "unsupported proto",
			hello: gatewaywire.ConnectHello{
				Type:    gatewaywire.TypeConnect,
				Proto:   gatewaywire.Proto + 1,
				RelayID: relayID,
				Proof:   base64.RawURLEncoding.EncodeToString(make([]byte, 32)),
			},
		},
		{
			name: "malformed relay id",
			hello: gatewaywire.ConnectHello{
				Type:    gatewaywire.TypeConnect,
				Proto:   gatewaywire.Proto,
				RelayID: "not-a-relay-id",
				Proof:   base64.RawURLEncoding.EncodeToString(make([]byte, 32)),
			},
		},
		{
			// The relay decodes proofs with unpadded base64url only, so a padded
			// proof is refused here instead of failing at the relay.
			name: "padded proof",
			hello: gatewaywire.ConnectHello{
				Type:    gatewaywire.TypeConnect,
				Proto:   gatewaywire.Proto,
				RelayID: relayID,
				Proof:   base64.URLEncoding.EncodeToString(make([]byte, 32)),
			},
		},
		{
			name: "wrong message type",
			hello: gatewaywire.ConnectHello{
				Type:    gatewaywire.TypeRegister,
				Proto:   gatewaywire.Proto,
				RelayID: relayID,
				Proof:   base64.RawURLEncoding.EncodeToString(make([]byte, 32)),
			},
		},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), testDeadline)
			defer cancel()
			conn, _, err := websocket.Dial(ctx, h.wsURL+"/connect", nil)
			if err != nil {
				t.Fatalf("dial connect: %v", err)
			}
			defer conn.CloseNow()

			if hello := readControl(t, conn); hello.Type != gatewaywire.TypeServerHello {
				t.Fatalf("phone hello is %+v", hello)
			}
			writeControl(t, conn, testCase.hello)
			reply := readControl(t, conn)
			if reply.Type != gatewaywire.TypeError || reply.Code != gatewaywire.CodeBadHello {
				t.Fatalf("reply is %+v, want a %s error", reply, gatewaywire.CodeBadHello)
			}
		})
	}
}

func TestRelaySendingGatewayOnlyOpcodeIsRejected(t *testing.T) {
	relayID, _ := testIdentity(t)
	h := newHarness(t, Options{})
	relay := h.dialRelay(relayID)

	relay.write(gatewaywire.OpOpen, 1, []byte(`{}`))

	var closeErr websocket.CloseError
	err := relay.waitClosed()
	if !errors.As(err, &closeErr) {
		t.Fatalf("relay link ended with %v, want a WebSocket close", err)
	}
	if closeErr.Code != websocket.StatusProtocolError {
		t.Fatalf("relay link closed with %v, want %v", closeErr.Code, websocket.StatusProtocolError)
	}
}

func postJSON(t *testing.T, url, body string) (int, map[string]any) {
	t.Helper()
	response, err := http.Post(url, "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatalf("post %s: %v", url, err)
	}
	defer response.Body.Close()
	data, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatalf("read response: %v", err)
	}
	decoded := map[string]any{}
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("decode response %q: %v", data, err)
	}
	return response.StatusCode, decoded
}

func TestProbeTargetsOnlyTheRequester(t *testing.T) {
	h := newHarness(t, Options{})

	listener, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatalf("listen udp: %v", err)
	}
	defer listener.Close()
	port := listener.LocalAddr().(*net.UDPAddr).Port

	token := randomBytes(t, 32)
	encoded := base64.RawURLEncoding.EncodeToString(token)

	// Privileged ports are refused, and the refusal does not consume the
	// caller's probe budget.
	status, body := postJSON(t, h.url+"/probe", fmt.Sprintf(`{"port":80,"token":%q}`, encoded))
	if status != http.StatusBadRequest || body["error"] != "invalid_port" {
		t.Fatalf("privileged port gave %d %v", status, body)
	}

	// A spoofed address in the body is ignored: the datagram goes to the source
	// address the gateway observed.
	status, body = postJSON(t, h.url+"/probe",
		fmt.Sprintf(`{"port":%d,"token":%q,"ip":"203.0.113.9","address":"203.0.113.9"}`, port, encoded))
	if status != http.StatusOK {
		t.Fatalf("probe gave %d %v", status, body)
	}
	if body["sent"] != true {
		t.Fatalf("probe response is %v", body)
	}
	if body["observed_ip"] != "127.0.0.1" {
		t.Fatalf("observed ip is %v, want 127.0.0.1", body["observed_ip"])
	}

	if err := listener.SetReadDeadline(time.Now().Add(testDeadline)); err != nil {
		t.Fatalf("set read deadline: %v", err)
	}
	buf := make([]byte, 64)
	n, from, err := listener.ReadFromUDP(buf)
	if err != nil {
		t.Fatalf("probe datagram never arrived: %v", err)
	}
	if !bytes.Equal(buf[:n], token) {
		t.Fatalf("probe datagram carried %d bytes, want the 32 token bytes", n)
	}
	if !from.IP.IsLoopback() {
		t.Fatalf("probe datagram came from %v, want loopback", from.IP)
	}

	// One probe per ten seconds per address.
	status, body = postJSON(t, h.url+"/probe", fmt.Sprintf(`{"port":%d,"token":%q}`, port, encoded))
	if status != http.StatusTooManyRequests || body["error"] != "rate_limited" {
		t.Fatalf("second probe gave %d %v", status, body)
	}
}

func TestWhoamiAndHealthz(t *testing.T) {
	h := newHarness(t, Options{Version: "0.17.1", Revision: "abc123"})

	response, err := http.Get(h.url + "/whoami")
	if err != nil {
		t.Fatalf("get whoami: %v", err)
	}
	defer response.Body.Close()
	var whoami whoamiResponse
	if err := json.NewDecoder(response.Body).Decode(&whoami); err != nil {
		t.Fatalf("decode whoami: %v", err)
	}
	if whoami.IP != "127.0.0.1" {
		t.Fatalf("whoami reports %q, want 127.0.0.1", whoami.IP)
	}

	health, err := http.Get(h.url + "/healthz")
	if err != nil {
		t.Fatalf("get healthz: %v", err)
	}
	defer health.Body.Close()
	var report healthResponse
	if err := json.NewDecoder(health.Body).Decode(&report); err != nil {
		t.Fatalf("decode healthz: %v", err)
	}
	if !report.OK || report.Relays != 0 || report.Clients != 0 {
		t.Fatalf("healthz reports %+v", report)
	}
	if report.Protocol != gatewaywire.Proto ||
		report.Version != "0.17.1" ||
		report.Revision != "abc123" {
		t.Fatalf("healthz build identity = %+v", report)
	}
}

func TestCountersSurviveRestart(t *testing.T) {
	relayID, rendezvous := testIdentity(t)
	statePath := filepath.Join(t.TempDir(), "state.json")

	first := newHarness(t, Options{MonthlyBytes: 1024, QuotaWarnPercent: -1, StatePath: statePath})
	relay := first.dialRelay(relayID)
	phone := first.mustConnect(relayID, rendezvous)
	relay.expect(gatewaywire.OpOpen)
	phone.send(randomBytes(t, 2048))
	relay.expect(gatewaywire.OpData)
	expectNotice(t, relay, gatewaywire.NoticeQuotaExceeded)

	if err := first.server.Close(); err != nil {
		t.Fatalf("close first gateway: %v", err)
	}

	data, err := os.ReadFile(statePath)
	if err != nil {
		t.Fatalf("read state: %v", err)
	}
	var state persistedState
	if err := json.Unmarshal(data, &state); err != nil {
		t.Fatalf("decode state: %v", err)
	}
	if state.Version != stateVersion {
		t.Fatalf("state version is %d, want %d", state.Version, stateVersion)
	}
	saved := state.Relays[relayID]
	if saved == nil {
		t.Fatalf("state has no counter for the relay: %v", state.Relays)
	}
	if saved.RelayedBytes < 2048 {
		t.Fatalf("state recorded %d bytes, want at least 2048", saved.RelayedBytes)
	}

	// A restarted gateway still knows the quota is spent.
	second := newHarness(t, Options{MonthlyBytes: 1024, QuotaWarnPercent: -1, StatePath: statePath})
	second.dialRelay(relayID)
	refusal := second.mustRefuse(relayID, rendezvous)
	if refusal.Code != gatewaywire.CodeQuotaExceeded {
		t.Fatalf("restarted gateway refused with %q, want %q", refusal.Code, gatewaywire.CodeQuotaExceeded)
	}
}
