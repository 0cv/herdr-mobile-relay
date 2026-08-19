package transport

import (
	"bytes"
	"context"
	"crypto/ecdh"
	"crypto/hkdf"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/0cv/herdr-mobile-relay/internal/config"
	"github.com/0cv/herdr-mobile-relay/internal/framing"
	"github.com/0cv/herdr-mobile-relay/internal/gatewaywire"
)

const gatewayTestRelayKey = "0123456789abcdef0123456789abcdef"

// gatewayTestTimeout bounds every blocking assertion so a broken pump fails the
// test instead of hanging the package.
const gatewayTestTimeout = 5 * time.Second

type gatewayTestFrame struct {
	op      byte
	connID  uint32
	payload []byte
}

// fakeGatewayLink is one accepted relay registration, seen from the gateway's
// side of the wire.
type fakeGatewayLink struct {
	conn   *websocket.Conn
	frames chan gatewayTestFrame
	closed chan struct{}
}

func (l *fakeGatewayLink) readLoop() {
	defer close(l.closed)
	for {
		messageType, data, err := l.conn.Read(context.Background())
		if err != nil {
			return
		}
		if messageType != websocket.MessageBinary {
			return
		}
		op, connID, payload, err := gatewaywire.DecodeFrame(data)
		if err != nil {
			return
		}
		select {
		case l.frames <- gatewayTestFrame{op: op, connID: connID, payload: bytes.Clone(payload)}:
		case <-time.After(gatewayTestTimeout):
			return
		}
	}
}

func (l *fakeGatewayLink) write(t *testing.T, op byte, connID uint32, payload []byte) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), gatewayTestTimeout)
	defer cancel()
	frame := gatewaywire.EncodeFrame(op, connID, payload)
	if err := l.conn.Write(ctx, websocket.MessageBinary, frame); err != nil {
		t.Fatalf("write gateway frame op=%d conn=%d: %v", op, connID, err)
	}
}

func (l *fakeGatewayLink) next(t *testing.T) gatewayTestFrame {
	t.Helper()
	select {
	case frame := <-l.frames:
		return frame
	case <-l.closed:
		t.Fatal("relay link closed while waiting for a frame")
	case <-time.After(gatewayTestTimeout):
		t.Fatal("timed out waiting for a relay frame")
	}
	return gatewayTestFrame{}
}

func (l *fakeGatewayLink) expect(t *testing.T, op byte, connID uint32) gatewayTestFrame {
	t.Helper()
	frame := l.next(t)
	if frame.op != op || frame.connID != connID {
		t.Fatalf("frame op=%d conn=%d, want op=%d conn=%d", frame.op, frame.connID, op, connID)
	}
	return frame
}

// fakeGateway speaks the gatewaywire relay contract: hello, register, ready,
// then multiplexed binary frames. It never holds a relay secret, exactly like
// the real gateway.
type fakeGateway struct {
	url     string
	relayID string
	links   chan *fakeGatewayLink
}

func newFakeGateway(t *testing.T, relayID string) *fakeGateway {
	t.Helper()
	return newFakeGatewayWithHealthDelay(t, relayID, 0)
}

func newFakeGatewayWithHealthDelay(
	t *testing.T,
	relayID string,
	healthDelay time.Duration,
) *fakeGateway {
	t.Helper()
	gateway := &fakeGateway{relayID: relayID, links: make(chan *fakeGatewayLink, 8)}
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		timer := time.NewTimer(healthDelay)
		defer timer.Stop()
		select {
		case <-timer.C:
		case <-r.Context().Done():
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]bool{"ok": true})
	})
	mux.HandleFunc("/relay", func(w http.ResponseWriter, r *http.Request) {
		conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{InsecureSkipVerify: true})
		if err != nil {
			return
		}
		conn.SetReadLimit(gatewayReadLimit)
		if !gateway.handshake(r.Context(), conn) {
			conn.CloseNow()
			return
		}
		link := &fakeGatewayLink{
			conn:   conn,
			frames: make(chan gatewayTestFrame, 64),
			closed: make(chan struct{}),
		}
		go link.readLoop()
		select {
		case gateway.links <- link:
		default:
			conn.CloseNow()
		}
	})
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)
	gateway.url = "ws" + strings.TrimPrefix(server.URL, "http")
	return gateway
}

func (g *fakeGateway) handshake(ctx context.Context, conn *websocket.Conn) bool {
	nonce := make([]byte, gatewaywire.NonceBytes)
	if _, err := rand.Read(nonce); err != nil {
		return false
	}
	hello, err := json.Marshal(gatewaywire.ServerHello{
		Type:  gatewaywire.TypeServerHello,
		Proto: gatewaywire.Proto,
		Nonce: base64.RawURLEncoding.EncodeToString(nonce),
	})
	if err != nil {
		return false
	}
	if err := conn.Write(ctx, websocket.MessageText, hello); err != nil {
		return false
	}
	messageType, raw, err := conn.Read(ctx)
	if err != nil || messageType != websocket.MessageText {
		return false
	}
	var register gatewaywire.RegisterHello
	if err := json.Unmarshal(raw, &register); err != nil {
		return false
	}
	if register.Type != gatewaywire.TypeRegister ||
		register.Proto != gatewaywire.Proto ||
		register.RelayID != g.relayID {
		rejection, _ := json.Marshal(gatewaywire.ErrorMessage{
			Type:    gatewaywire.TypeError,
			Code:    gatewaywire.CodeBadHello,
			Message: "unexpected register hello",
		})
		_ = conn.Write(ctx, websocket.MessageText, rejection)
		return false
	}
	ready, err := json.Marshal(gatewaywire.ReadyMessage{
		Type:  gatewaywire.TypeReady,
		Proto: gatewaywire.Proto,
	})
	if err != nil {
		return false
	}
	return conn.Write(ctx, websocket.MessageText, ready) == nil
}

func (g *fakeGateway) nextLink(t *testing.T) *fakeGatewayLink {
	t.Helper()
	select {
	case link := <-g.links:
		return link
	case <-time.After(gatewayTestTimeout):
		t.Fatal("timed out waiting for a relay registration")
	}
	return nil
}

// gatewayHarness wires a hub, a gateway client, and a fake gateway together.
type gatewayHarness struct {
	hub           *Hub
	client        *GatewayClient
	gateway       *fakeGateway
	link          *fakeGatewayLink
	relayID       string
	rendezvousKey []byte
	connected     chan *ClientConn
	received      chan map[string]any
}

func newGatewayHarness(t *testing.T, maxClients int) *gatewayHarness {
	t.Helper()
	return newGatewayHarnessWithBackoff(t, maxClients, 20*time.Millisecond)
}

func newGatewayHarnessWithBackoff(t *testing.T, maxClients int, backoffBase time.Duration) *gatewayHarness {
	t.Helper()
	relayID, err := gatewaywire.DeriveRelayID(gatewayTestRelayKey)
	if err != nil {
		t.Fatal(err)
	}
	rendezvousKey, err := gatewaywire.DeriveRendezvousKey(gatewayTestRelayKey)
	if err != nil {
		t.Fatal(err)
	}

	hub := NewHub(&config.Config{Token: gatewayTestRelayKey}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	harness := &gatewayHarness{
		hub:           hub,
		relayID:       relayID,
		rendezvousKey: rendezvousKey,
		connected:     make(chan *ClientConn, 4),
		received:      make(chan map[string]any, 4),
	}
	hub.SetOnConnect(func(client *ClientConn) { harness.connected <- client })
	hub.SetHandler(func(_ *ClientConn, message map[string]any, admitted func()) {
		admitted()
		harness.received <- message
	})

	harness.gateway = newFakeGateway(t, relayID)
	client, err := NewGatewayClient(hub, GatewayOptions{
		URL:        harness.gateway.url,
		RelayKey:   gatewayTestRelayKey,
		MaxClients: maxClients,
		Logger:     slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	if err != nil {
		t.Fatal(err)
	}
	// Keep reconnects fast enough for a unit test while still exercising the
	// real backoff path.
	client.backoffBase = backoffBase
	client.backoffMax = 4 * backoffBase
	harness.client = client

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		client.Run(ctx)
	}()
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(gatewayTestTimeout):
			t.Error("gateway client did not stop")
		}
		shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), gatewayTestTimeout)
		defer shutdownCancel()
		if err := hub.Shutdown(shutdownCtx); err != nil {
			t.Errorf("hub shutdown: %v", err)
		}
	})

	harness.link = harness.gateway.nextLink(t)
	return harness
}

// openPayload builds the OpOpen body the gateway forwards after a phone
// answered its challenge.
func (h *gatewayHarness) openPayload(t *testing.T, key []byte) []byte {
	t.Helper()
	nonce := make([]byte, gatewaywire.NonceBytes)
	if _, err := rand.Read(nonce); err != nil {
		t.Fatal(err)
	}
	payload, err := json.Marshal(gatewaywire.OpenPayload{
		Nonce: base64.RawURLEncoding.EncodeToString(nonce),
		Proof: base64.RawURLEncoding.EncodeToString(gatewaywire.ConnectProof(key, h.relayID, nonce)),
	})
	if err != nil {
		t.Fatal(err)
	}
	return payload
}

func (h *gatewayHarness) waitRegistered(t *testing.T) {
	t.Helper()
	waitFor(t, "gateway registration", func() bool { return h.client.Status().Registered })
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(gatewayTestTimeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

// gatewayTestPhone drives the phone side of one multiplexed connection: it
// speaks the E2EE handshake with the binary codec over chunked OpData frames.
// Every logical frame — handshake JSON included — is fragmented and reassembled
// exactly like the real phone does.
type gatewayTestPhone struct {
	link    *fakeGatewayLink
	connID  uint32
	session *e2eeSession

	chunks     [][]byte
	recv       *framing.Reassembler
	lastChunks int
}

// send fragments one logical frame so no OpData payload exceeds the gateway's
// per-frame budget.
func (p *gatewayTestPhone) send(t *testing.T, frame []byte) {
	t.Helper()
	p.chunks = framing.Chunk(p.chunks[:0], frame, framing.GatewayChunkSize)
	for _, part := range p.chunks {
		p.link.write(t, gatewaywire.OpData, p.connID, part)
	}
}

// readFrame reassembles the next logical frame the relay sends on this
// connection and records how many chunks it took.
func (p *gatewayTestPhone) readFrame(t *testing.T) []byte {
	t.Helper()
	if p.recv == nil {
		p.recv = framing.NewReassembler(framing.GatewayChunkSize)
	}
	p.lastChunks = 0
	for {
		wire := p.link.expect(t, gatewaywire.OpData, p.connID)
		if len(wire.payload) > framing.GatewayChunkSize {
			t.Fatalf("relay chunk of %d bytes exceeds the gateway chunk size %d",
				len(wire.payload), framing.GatewayChunkSize)
		}
		p.lastChunks++
		frame, err := p.recv.Push(wire.payload)
		if err != nil {
			t.Fatalf("reassemble relay chunk: %v", err)
		}
		if frame != nil {
			return frame
		}
	}
}

func (p *gatewayTestPhone) handshake(t *testing.T, token string) {
	t.Helper()
	curve := ecdh.P256()
	privateKey, err := curve.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	clientNonce := make([]byte, e2eeNonceBytes)
	if _, err := rand.Read(clientNonce); err != nil {
		t.Fatal(err)
	}
	clientPublic := privateKey.PublicKey().Bytes()
	hello, err := json.Marshal(e2eeClientHello{
		Type:      "e2ee_client_hello",
		Version:   e2eeVersion,
		Nonce:     base64.RawURLEncoding.EncodeToString(clientNonce),
		PublicKey: base64.RawURLEncoding.EncodeToString(clientPublic),
		Proof: base64.RawURLEncoding.EncodeToString(
			e2eeAuthTag(token, e2eeClientProofLabel, clientNonce, clientPublic)),
	})
	if err != nil {
		t.Fatal(err)
	}
	p.send(t, hello)

	frame := p.readFrame(t)
	var serverHello e2eeServerHello
	if err := json.Unmarshal(frame, &serverHello); err != nil {
		t.Fatalf("decode server hello: %v", err)
	}
	serverNonce, err := decodeE2EEField(serverHello.Nonce, e2eeNonceBytes)
	if err != nil {
		t.Fatal(err)
	}
	serverPublicBytes, err := decodeE2EEField(serverHello.PublicKey, e2eePublicKeyBytes)
	if err != nil {
		t.Fatal(err)
	}
	serverProof, err := decodeE2EEField(serverHello.Proof, sha256.Size)
	if err != nil {
		t.Fatal(err)
	}
	transcript := e2eeTranscript(clientNonce, clientPublic, serverNonce, serverPublicBytes)
	if !hmac.Equal(serverProof, e2eeAuthTag(token, e2eeServerProofLabel, transcript)) {
		t.Fatal("server proof did not authenticate over the gateway path")
	}
	serverPublic, err := curve.NewPublicKey(serverPublicBytes)
	if err != nil {
		t.Fatal(err)
	}
	sharedSecret, err := privateKey.ECDH(serverPublic)
	if err != nil {
		t.Fatal(err)
	}
	keySalt := e2eeAuthTag(token, e2eeKeySaltLabel, transcript)
	clientKey, err := hkdf.Key(sha256.New, sharedSecret, keySalt, "herdr-e2ee-v1 c2s", 32)
	if err != nil {
		t.Fatal(err)
	}
	serverKey, err := hkdf.Key(sha256.New, sharedSecret, keySalt, "herdr-e2ee-v1 s2c", 32)
	if err != nil {
		t.Fatal(err)
	}
	session, err := newE2EESession(clientKey, serverKey, e2eeClientDirection, e2eeServerDirection)
	if err != nil {
		t.Fatal(err)
	}
	session.codec = CodecBinary
	p.session = session

	finish, err := json.Marshal(e2eeClientFinish{Type: "e2ee_client_finish", Version: e2eeVersion})
	if err != nil {
		t.Fatal(err)
	}
	sealed, err := session.seal(finish)
	if err != nil {
		t.Fatal(err)
	}
	p.send(t, sealed)
}

func (p *gatewayTestPhone) sendMessage(t *testing.T, plaintext string) {
	t.Helper()
	sealed, err := p.session.seal([]byte(plaintext))
	if err != nil {
		t.Fatal(err)
	}
	p.send(t, sealed)
}

func (p *gatewayTestPhone) readMessage(t *testing.T) []byte {
	t.Helper()
	frame := p.readFrame(t)
	plaintext, err := p.session.open(frame)
	if err != nil {
		t.Fatalf("open relay frame: %v", err)
	}
	return plaintext
}

func TestGatewayClientRegistersAndReconnectsWithBackoff(t *testing.T) {
	harness := newGatewayHarness(t, 4)
	harness.waitRegistered(t)

	status := harness.client.Status()
	if !status.Enabled {
		t.Error("status.Enabled = false, want true")
	}
	if status.RelayID != harness.relayID {
		t.Errorf("status.RelayID = %q, want %q", status.RelayID, harness.relayID)
	}
	if status.URL != harness.gateway.url {
		t.Errorf("status.URL = %q, want %q", status.URL, harness.gateway.url)
	}
	if status.ConnectedSince.IsZero() {
		t.Error("status.ConnectedSince is zero while registered")
	}

	dropped := time.Now()
	harness.link.conn.CloseNow()

	link := harness.gateway.nextLink(t)
	if elapsed := time.Since(dropped); elapsed < time.Duration(float64(harness.client.backoffBase)*0.8) {
		t.Fatalf("reconnected after %s, want at least one jittered backoff interval", elapsed)
	}
	harness.link = link
	harness.waitRegistered(t)
}

func TestGatewayBackoffGrowsAndCaps(t *testing.T) {
	client := &GatewayClient{backoffMax: gatewayBackoffMax}
	delay := gatewayBackoffBase
	want := []time.Duration{2, 4, 8, 16, 32, 60, 60}
	for i, expected := range want {
		delay = client.nextBackoff(delay)
		if delay != expected*time.Second {
			t.Fatalf("backoff[%d] = %s, want %s", i, delay, expected*time.Second)
		}
	}
	for range 200 {
		jittered := jitterDelay(10 * time.Second)
		if jittered < 8*time.Second || jittered > 12*time.Second {
			t.Fatalf("jittered delay %s outside the +/-20%% band", jittered)
		}
	}
}

func TestGatewayClientServesAuthenticatedConnection(t *testing.T) {
	harness := newGatewayHarness(t, 4)
	harness.waitRegistered(t)

	harness.link.write(t, gatewaywire.OpOpen, 7, harness.openPayload(t, harness.rendezvousKey))
	phone := &gatewayTestPhone{link: harness.link, connID: 7}
	phone.handshake(t, gatewayTestRelayKey)

	var client *ClientConn
	select {
	case client = <-harness.connected:
	case <-time.After(gatewayTestTimeout):
		t.Fatal("hub never registered the gateway connection")
	}
	if client.Transport() != TransportGateway {
		t.Errorf("transport = %q, want %q", client.Transport(), TransportGateway)
	}
	if got := harness.client.Status().Clients; got != 1 {
		t.Errorf("status.Clients = %d, want 1", got)
	}

	if !harness.hub.Send(client, map[string]any{"type": "pane_content", "content": "relayed output"}) {
		t.Fatal("hub refused to send to the gateway client")
	}
	plaintext := phone.readMessage(t)
	if !bytes.Contains(plaintext, []byte("relayed output")) {
		t.Fatalf("decrypted relay message = %s", plaintext)
	}

	phone.sendMessage(t, `{"type":"refresh_agents"}`)
	select {
	case message := <-harness.received:
		if message["type"] != "refresh_agents" {
			t.Fatalf("received type = %v, want refresh_agents", message["type"])
		}
	case <-time.After(gatewayTestTimeout):
		t.Fatal("hub never received the phone message")
	}
}

func TestGatewayClientReportsPolicyViolationClose(t *testing.T) {
	harness := newGatewayHarnessWithBackoff(t, 4, 500*time.Millisecond)
	harness.waitRegistered(t)

	// The gateway replaces a relay id claimed by a newer link with a 1008
	// close; framing has started, so there is no TEXT error frame to read.
	if err := harness.link.conn.Close(websocket.StatusPolicyViolation, gatewaywire.CodeRelayBusy); err != nil {
		t.Fatalf("close relay link: %v", err)
	}

	waitFor(t, "recorded gateway rejection", func() bool {
		return strings.Contains(harness.client.Status().LastError, gatewaywire.CodeRelayBusy)
	})
	if harness.client.Status().Registered {
		t.Error("status.Registered = true after the gateway closed the link")
	}
	harness.link = harness.gateway.nextLink(t)
	harness.waitRegistered(t)
}

func TestGatewayClientRejectsInvalidProof(t *testing.T) {
	harness := newGatewayHarness(t, 4)
	harness.waitRegistered(t)

	wrongKey := make([]byte, len(harness.rendezvousKey))
	copy(wrongKey, harness.rendezvousKey)
	wrongKey[0] ^= 0xff
	harness.link.write(t, gatewaywire.OpOpen, 3, harness.openPayload(t, wrongKey))

	frame := harness.link.expect(t, gatewaywire.OpClose, 3)
	if string(frame.payload) != gatewayReasonUnauthorized {
		t.Fatalf("close reason = %q, want %q", frame.payload, gatewayReasonUnauthorized)
	}
	if got := harness.client.Status().Clients; got != 0 {
		t.Fatalf("status.Clients = %d, want 0", got)
	}
	if got := harness.hub.ClientCount(); got != 0 {
		t.Fatalf("hub client count = %d, want 0", got)
	}
}

func TestGatewayClientEnforcesMaxClients(t *testing.T) {
	harness := newGatewayHarness(t, 1)
	harness.waitRegistered(t)

	harness.link.write(t, gatewaywire.OpOpen, 1, harness.openPayload(t, harness.rendezvousKey))
	waitFor(t, "first gateway connection", func() bool { return harness.client.Status().Clients == 1 })

	harness.link.write(t, gatewaywire.OpOpen, 2, harness.openPayload(t, harness.rendezvousKey))
	frame := harness.link.expect(t, gatewaywire.OpClose, 2)
	if string(frame.payload) != gatewayReasonBusy {
		t.Fatalf("close reason = %q, want %q", frame.payload, gatewayReasonBusy)
	}
	if got := harness.client.Status().Clients; got != 1 {
		t.Fatalf("status.Clients = %d, want 1", got)
	}
}

func TestGatewayLinkLossEvictsHubClients(t *testing.T) {
	harness := newGatewayHarness(t, 4)
	harness.waitRegistered(t)

	harness.link.write(t, gatewaywire.OpOpen, 11, harness.openPayload(t, harness.rendezvousKey))
	phone := &gatewayTestPhone{link: harness.link, connID: 11}
	phone.handshake(t, gatewayTestRelayKey)
	select {
	case <-harness.connected:
	case <-time.After(gatewayTestTimeout):
		t.Fatal("hub never registered the gateway connection")
	}
	waitFor(t, "hub client registration", func() bool { return harness.hub.ClientCount() == 1 })

	harness.link.conn.CloseNow()

	waitFor(t, "hub client eviction", func() bool { return harness.hub.ClientCount() == 0 })
	waitFor(t, "gateway connection cleanup", func() bool { return harness.client.Status().Clients == 0 })
}

func TestGatewayClientRequiresWebSocketURLAndRelayKey(t *testing.T) {
	hub := NewHub(&config.Config{}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), gatewayTestTimeout)
		defer cancel()
		if err := hub.Shutdown(ctx); err != nil {
			t.Errorf("hub shutdown: %v", err)
		}
	})

	if _, err := NewGatewayClient(hub, GatewayOptions{URL: "", RelayKey: gatewayTestRelayKey}); err == nil {
		t.Error("expected an error for an empty gateway url")
	}
	if _, err := NewGatewayClient(hub, GatewayOptions{URL: "https://gw.example.com", RelayKey: gatewayTestRelayKey}); err == nil {
		t.Error("expected an error for a non-websocket gateway url")
	}
	if _, err := NewGatewayClient(hub, GatewayOptions{URL: "wss://gw.example.com"}); err == nil {
		t.Error("expected an error for a missing relay key")
	}

	client, err := NewGatewayClient(hub, GatewayOptions{
		URL:      "wss://gw.example.com/",
		RelayKey: gatewayTestRelayKey,
		Logger:   slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	if err != nil {
		t.Fatal(err)
	}
	if client.CurrentURL() != "wss://gw.example.com" {
		t.Errorf("current url = %q, want wss://gw.example.com", client.CurrentURL())
	}
	if client.maxClients != gatewayDefaultMaxClients {
		t.Errorf("max clients = %d, want %d", client.maxClients, gatewayDefaultMaxClients)
	}
	relayID, err := gatewaywire.DeriveRelayID(gatewayTestRelayKey)
	if err != nil {
		t.Fatal(err)
	}
	if client.RelayID() != relayID {
		t.Errorf("relay id = %q, want %q", client.RelayID(), relayID)
	}
	if status := (*GatewayClient)(nil).Status(); status.Enabled {
		t.Error("nil gateway client reported an enabled gateway")
	}
}

func TestGatewayClientValidatesTheGatewayURLList(t *testing.T) {
	hub := NewHub(&config.Config{}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), gatewayTestTimeout)
		defer cancel()
		if err := hub.Shutdown(ctx); err != nil {
			t.Errorf("hub shutdown: %v", err)
		}
	})

	_, err := NewGatewayClient(hub, GatewayOptions{
		URLs:     []string{"wss://a.example.com", "https://b.example.com"},
		RelayKey: gatewayTestRelayKey,
	})
	if err == nil || !strings.Contains(err.Error(), "https://b.example.com") {
		t.Errorf("error = %v, want the offending list entry named", err)
	}
	if _, err := NewGatewayClient(hub, GatewayOptions{
		URL:      "wss://b.example.com",
		URLs:     []string{"wss://a.example.com", "wss://b.example.com"},
		RelayKey: gatewayTestRelayKey,
	}); err == nil {
		t.Error("expected an error when the primary url is not the first list entry")
	}

	client, err := NewGatewayClient(hub, GatewayOptions{
		URL:      "wss://a.example.com",
		URLs:     []string{" wss://a.example.com/ ", "", "wss://b.example.com"},
		RelayKey: gatewayTestRelayKey,
		Logger:   slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"wss://a.example.com", "wss://b.example.com"}
	if !slices.Equal(client.urls, want) {
		t.Errorf("gateway urls = %v, want %v", client.urls, want)
	}
	if client.CurrentURL() != want[0] {
		t.Errorf("current url = %q, want %q", client.CurrentURL(), want[0])
	}
	client.advance()
	if client.CurrentURL() != want[1] {
		t.Errorf("current url after rotation = %q, want %q", client.CurrentURL(), want[1])
	}
	client.advance()
	if client.CurrentURL() != want[0] {
		t.Errorf("current url after wrapping = %q, want %q", client.CurrentURL(), want[0])
	}
}

// TestGatewayClientSelectsLowestLatencyAndKeepsOneRegistration exercises the
// production health probe rather than a timing mock. Both gateways answer, but
// only the faster one receives a relay registration; after that link fails the
// other becomes the cold fallback.
func TestGatewayClientSelectsLowestLatencyAndKeepsOneRegistration(t *testing.T) {
	relayID, err := gatewaywire.DeriveRelayID(gatewayTestRelayKey)
	if err != nil {
		t.Fatal(err)
	}
	slow := newFakeGatewayWithHealthDelay(t, relayID, 80*time.Millisecond)
	fast := newFakeGatewayWithHealthDelay(t, relayID, 5*time.Millisecond)
	hub := NewHub(&config.Config{Token: gatewayTestRelayKey}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	client, err := NewGatewayClient(hub, GatewayOptions{
		URL:      slow.url,
		URLs:     []string{slow.url, fast.url},
		RelayKey: gatewayTestRelayKey,
		Logger:   slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	if err != nil {
		t.Fatal(err)
	}
	client.backoffBase = 10 * time.Millisecond
	client.backoffMax = 40 * time.Millisecond

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		client.Run(ctx)
	}()
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(gatewayTestTimeout):
			t.Error("gateway client did not stop")
		}
		shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), gatewayTestTimeout)
		defer shutdownCancel()
		if err := hub.Shutdown(shutdownCtx); err != nil {
			t.Errorf("hub shutdown: %v", err)
		}
	})

	fastLink := fast.nextLink(t)
	waitFor(t, "latency-selected gateway status", func() bool {
		status := client.Status()
		return status.Registered && status.URL == fast.url &&
			slices.Equal(status.URLs, []string{fast.url, slow.url})
	})
	select {
	case <-slow.links:
		t.Fatal("slow gateway received a maintained registration")
	default:
	}

	fastLink.conn.CloseNow()
	slow.nextLink(t)
	waitFor(t, "cold fallback gateway status", func() bool {
		status := client.Status()
		return status.Registered && status.URL == slow.url
	})
}

// TestGatewayLatencySelectionProbesConcurrentlyAndKeepsCloseTiesOrdered guards
// both non-obvious contracts: one slow endpoint cannot serially delay every
// other probe, and small timing noise cannot overturn configured order.
func TestGatewayLatencySelectionProbesConcurrentlyAndKeepsCloseTiesOrdered(t *testing.T) {
	hub := NewHub(&config.Config{}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), gatewayTestTimeout)
		defer cancel()
		if err := hub.Shutdown(ctx); err != nil {
			t.Errorf("hub shutdown: %v", err)
		}
	})
	client, err := NewGatewayClient(hub, GatewayOptions{
		URLs: []string{
			"wss://a.example.com",
			"wss://b.example.com",
			"wss://c.example.com",
		},
		RelayKey: gatewayTestRelayKey,
		Logger:   slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	if err != nil {
		t.Fatal(err)
	}
	entered := make(chan string, 3)
	release := make(chan struct{})
	client.probe = func(_ context.Context, gateway string) (time.Duration, error) {
		entered <- gateway
		<-release
		if strings.Contains(gateway, "b.example.com") {
			return 10 * time.Millisecond, nil
		}
		return 50 * time.Millisecond, nil
	}
	selected := make(chan bool, 1)
	go func() { selected <- client.selectLowestLatency(context.Background(), -1) }()
	for range 3 {
		select {
		case <-entered:
		case <-time.After(gatewayTestTimeout):
			t.Fatal("gateway probes did not start concurrently")
		}
	}
	close(release)
	if !<-selected || client.CurrentURL() != "wss://b.example.com" {
		t.Fatalf("selected %q, want the lower-latency second gateway", client.CurrentURL())
	}

	// The global minimum is C at 10 ms. B is within the 20 ms tie window and
	// precedes C, while A is outside it; B must win regardless of result order.
	client.probe = func(_ context.Context, gateway string) (time.Duration, error) {
		switch {
		case strings.Contains(gateway, "a.example.com"):
			return 40 * time.Millisecond, nil
		case strings.Contains(gateway, "b.example.com"):
			return 25 * time.Millisecond, nil
		default:
			return 10 * time.Millisecond, nil
		}
	}
	if !client.selectLowestLatency(context.Background(), -1) {
		t.Fatal("close-tie gateway selection failed")
	}
	if client.CurrentURL() != "wss://b.example.com" {
		t.Fatalf("close tie selected %q, want earliest gateway within 20 ms of the minimum", client.CurrentURL())
	}

	client.setActive(0)
	client.probe = func(_ context.Context, gateway string) (time.Duration, error) {
		return 0, fmt.Errorf("%s has no health response", gateway)
	}
	if client.selectLowestLatency(context.Background(), -1) {
		t.Fatal("all-failed probes reported a selected gateway")
	}
	if client.CurrentURL() != "wss://a.example.com" {
		t.Fatalf("all-failed probes changed configured fallback to %q", client.CurrentURL())
	}
}

// TestGatewayClientFailsOverToTheNextGatewayURL closes a server before the
// client starts, so its port refuses connections. The relay must land on the
// second entry instead of retrying the dead primary forever.
func TestGatewayClientFailsOverToTheNextGatewayURL(t *testing.T) {
	relayID, err := gatewaywire.DeriveRelayID(gatewayTestRelayKey)
	if err != nil {
		t.Fatal(err)
	}
	dead := httptest.NewServer(http.NewServeMux())
	deadURL := "ws" + strings.TrimPrefix(dead.URL, "http")
	dead.Close()

	gateway := newFakeGateway(t, relayID)
	hub := NewHub(&config.Config{Token: gatewayTestRelayKey}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	client, err := NewGatewayClient(hub, GatewayOptions{
		URL:      deadURL,
		URLs:     []string{deadURL, gateway.url},
		RelayKey: gatewayTestRelayKey,
		Logger:   slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	if err != nil {
		t.Fatal(err)
	}
	client.backoffBase = 10 * time.Millisecond
	client.backoffMax = 40 * time.Millisecond

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		client.Run(ctx)
	}()
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(gatewayTestTimeout):
			t.Error("gateway client did not stop")
		}
		shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), gatewayTestTimeout)
		defer shutdownCancel()
		if err := hub.Shutdown(shutdownCtx); err != nil {
			t.Errorf("hub shutdown: %v", err)
		}
	})

	// The registration can only arrive on the second entry: nothing listens on
	// the first.
	gateway.nextLink(t)
	waitFor(t, "status to report the gateway entry in use", func() bool {
		status := client.Status()
		return status.Registered && status.URL == gateway.url
	})
}

// gatewayTestLargeFrame is comfortably above gatewaywire.MaxFramePayload, the
// shape of the data URL an image upload carries in a single logical message.
const gatewayTestLargeFrame = 3 << 20

// openGatewayPhone opens one authenticated logical connection and returns both
// of its ends: the phone that speaks chunk framing and the hub client the relay
// registered for it.
func openGatewayPhone(t *testing.T, harness *gatewayHarness, connID uint32) (*gatewayTestPhone, *ClientConn) {
	t.Helper()
	harness.link.write(t, gatewaywire.OpOpen, connID, harness.openPayload(t, harness.rendezvousKey))
	phone := &gatewayTestPhone{link: harness.link, connID: connID}
	phone.handshake(t, gatewayTestRelayKey)
	select {
	case client := <-harness.connected:
		return phone, client
	case <-time.After(gatewayTestTimeout):
		t.Fatalf("hub never registered gateway connection %d", connID)
	}
	return nil, nil
}

func TestGatewayFragmentsFramesAboveTheWireCap(t *testing.T) {
	harness := newGatewayHarness(t, 4)
	harness.waitRegistered(t)
	phone, client := openGatewayPhone(t, harness, 5)

	// Relay -> phone: one logical frame several times the gateway's per-frame
	// cap. readMessage asserts every observed chunk stays inside the budget.
	relayed := strings.Repeat("x", gatewayTestLargeFrame)
	if !harness.hub.Send(client, map[string]any{"type": "pane_content", "content": relayed}) {
		t.Fatal("hub refused to send the oversized frame")
	}
	var inbound map[string]any
	if err := json.Unmarshal(phone.readMessage(t), &inbound); err != nil {
		t.Fatalf("decode relayed message: %v", err)
	}
	got, _ := inbound["content"].(string)
	if len(got) != len(relayed) || got != relayed {
		t.Fatalf("relayed content of %d bytes did not survive fragmentation, want %d bytes",
			len(got), len(relayed))
	}
	if want := framing.Count(gatewayTestLargeFrame, framing.GatewayChunkSize); phone.lastChunks < want {
		t.Fatalf("relay used %d chunks, want at least %d", phone.lastChunks, want)
	}

	// Phone -> relay: the same frame size in the other direction, fragmented by
	// the phone and reassembled by the relay.
	uploaded := strings.Repeat("y", gatewayTestLargeFrame)
	message, err := json.Marshal(map[string]any{"type": "refresh_agents", "padding": uploaded})
	if err != nil {
		t.Fatal(err)
	}
	phone.sendMessage(t, string(message))
	select {
	case received := <-harness.received:
		if received["type"] != "refresh_agents" {
			t.Fatalf("received type = %v, want refresh_agents", received["type"])
		}
		padding, _ := received["padding"].(string)
		if len(padding) != len(uploaded) || padding != uploaded {
			t.Fatalf("uploaded padding of %d bytes did not survive reassembly, want %d bytes",
				len(padding), len(uploaded))
		}
	case <-time.After(gatewayTestTimeout):
		t.Fatal("hub never received the fragmented phone message")
	}
}

func TestGatewayFramingViolationClosesOnlyThatConnection(t *testing.T) {
	harness := newGatewayHarness(t, 4)
	harness.waitRegistered(t)
	_, _ = openGatewayPhone(t, harness, 21)
	survivor, survivorClient := openGatewayPhone(t, harness, 22)

	// A continuation chunk with no START before it is a protocol violation.
	harness.link.write(t, gatewaywire.OpData, 21, []byte{framing.Version, framing.FlagEnd, 'x'})

	closed := harness.link.expect(t, gatewaywire.OpClose, 21)
	if string(closed.payload) != gatewayReasonFraming {
		t.Fatalf("close reason = %q, want %q", closed.payload, gatewayReasonFraming)
	}
	waitFor(t, "the offending connection to be dropped", func() bool {
		return harness.client.Status().Clients == 1
	})

	if !harness.hub.Send(survivorClient, map[string]any{"type": "pane_content", "content": "still here"}) {
		t.Fatal("hub refused to send on the surviving connection")
	}
	if plaintext := survivor.readMessage(t); !bytes.Contains(plaintext, []byte("still here")) {
		t.Fatalf("surviving connection read %s", plaintext)
	}
	survivor.sendMessage(t, `{"type":"refresh_agents"}`)
	select {
	case received := <-harness.received:
		if received["type"] != "refresh_agents" {
			t.Fatalf("received type = %v, want refresh_agents", received["type"])
		}
	case <-time.After(gatewayTestTimeout):
		t.Fatal("surviving connection stopped delivering to the hub")
	}
}

func TestGatewayConcurrentSendersKeepChunkSequencesIntact(t *testing.T) {
	harness := newGatewayHarness(t, 4)
	harness.waitRegistered(t)

	phones := make(map[uint32]*gatewayTestPhone, 2)
	clients := make(map[uint32]*ClientConn, 2)
	for _, connID := range []uint32{31, 32} {
		phones[connID], clients[connID] = openGatewayPhone(t, harness, connID)
	}

	// Both frames need several chunks each, so the two send paths race for the
	// shared writer queue.
	want := map[uint32]string{
		31: strings.Repeat("a", gatewayTestLargeFrame),
		32: strings.Repeat("b", gatewayTestLargeFrame),
	}
	var senders sync.WaitGroup
	for connID, content := range want {
		senders.Add(1)
		go func() {
			defer senders.Done()
			if !harness.hub.Send(clients[connID], map[string]any{"type": "pane_content", "content": content}) {
				t.Errorf("hub refused to send on connection %d", connID)
			}
		}()
	}
	senders.Wait()

	assemblers := map[uint32]*framing.Reassembler{
		31: framing.NewReassembler(framing.GatewayChunkSize),
		32: framing.NewReassembler(framing.GatewayChunkSize),
	}
	counts := make(map[uint32]int, 2)
	frames := make(map[uint32][]byte, 2)
	for len(frames) < len(want) {
		wire := harness.link.next(t)
		if wire.op != gatewaywire.OpData {
			t.Fatalf("op %d on connection %d, want OpData", wire.op, wire.connID)
		}
		assembler, ok := assemblers[wire.connID]
		if !ok {
			t.Fatalf("frame for unexpected connection %d", wire.connID)
		}
		if len(wire.payload) > framing.GatewayChunkSize {
			t.Fatalf("chunk of %d bytes on connection %d exceeds the gateway chunk size %d",
				len(wire.payload), wire.connID, framing.GatewayChunkSize)
		}
		counts[wire.connID]++
		frame, err := assembler.Push(wire.payload)
		if err != nil {
			t.Fatalf("reassemble connection %d: %v", wire.connID, err)
		}
		if frame != nil {
			frames[wire.connID] = frame
		}
	}

	for connID, frame := range frames {
		plaintext, err := phones[connID].session.open(frame)
		if err != nil {
			t.Fatalf("open connection %d frame: %v", connID, err)
		}
		var message map[string]any
		if err := json.Unmarshal(plaintext, &message); err != nil {
			t.Fatalf("decode connection %d message: %v", connID, err)
		}
		content, _ := message["content"].(string)
		if content != want[connID] {
			t.Fatalf("connection %d content of %d bytes is corrupted, want %d bytes of %q",
				connID, len(content), len(want[connID]), want[connID][:1])
		}
		if expected := framing.Count(gatewayTestLargeFrame, framing.GatewayChunkSize); counts[connID] < expected {
			t.Fatalf("connection %d used %d chunks, want at least %d", connID, counts[connID], expected)
		}
	}
}

func TestGatewaySweepsStalledAssemblies(t *testing.T) {
	harness := newGatewayHarness(t, 4)
	harness.waitRegistered(t)
	openGatewayPhone(t, harness, 41)

	// Only the START chunk of a two chunk frame arrives, so the assembly can
	// never complete and would otherwise pin its buffer for the whole session.
	partial := framing.Chunk(nil, bytes.Repeat([]byte{7}, framing.GatewayChunkSize), framing.GatewayChunkSize)
	if len(partial) < 2 {
		t.Fatalf("expected a fragmented frame, got %d chunks", len(partial))
	}
	harness.link.write(t, gatewaywire.OpData, 41, partial[0])
	waitFor(t, "the partial assembly to be recorded", func() bool {
		conn := harness.client.lookupConn(41)
		return conn != nil && conn.stalled(time.Now().Add(framing.StallTimeout+time.Second))
	})

	harness.client.sweepStalled(time.Now().Add(framing.StallTimeout + time.Second))

	closed := harness.link.expect(t, gatewaywire.OpClose, 41)
	if string(closed.payload) != gatewayReasonSlow {
		t.Fatalf("close reason = %q, want %q", closed.payload, gatewayReasonSlow)
	}
	waitFor(t, "the stalled connection to be dropped", func() bool {
		return harness.client.Status().Clients == 0
	})
}
