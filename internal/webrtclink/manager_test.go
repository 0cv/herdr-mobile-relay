package webrtclink

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/netip"
	"runtime"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/pion/ice/v4"
	"github.com/pion/webrtc/v4"

	"github.com/0cv/herdr-mobile-relay/internal/framing"
	"github.com/0cv/herdr-mobile-relay/internal/transport"
)

const testTimeout = 20 * time.Second

// webrtcStartBody is the body capacity of a START chunk on the WebRTC path.
const webrtcStartBody = framing.WebRTCChunkSize - framing.HeaderSize - framing.LengthPrefixSize

func quietLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// gate defers signaling work until the peer it targets is able to accept it,
// which is what a real signaling channel does with trickled candidates.
type gate struct {
	mu      sync.Mutex
	open    bool
	pending []func()
}

func (g *gate) do(fn func()) {
	g.mu.Lock()
	if g.open {
		g.mu.Unlock()
		fn()
		return
	}
	g.pending = append(g.pending, fn)
	g.mu.Unlock()
}

func (g *gate) release() {
	g.mu.Lock()
	g.open = true
	pending := g.pending
	g.pending = nil
	g.mu.Unlock()
	for _, fn := range pending {
		fn()
	}
}

// browserPeer is a second Pion PeerConnection standing in for the PWA: it
// creates the DataChannel, speaks the same chunk framing, and trickles ICE.
type browserPeer struct {
	pc     *webrtc.PeerConnection
	dc     *webrtc.DataChannel
	opened chan struct{}
	frames chan []byte

	toRelay   gate
	toBrowser gate

	assembler *framing.Reassembler
	sendMu    sync.Mutex
	chunks    [][]byte
}

func newBrowserPeer(t *testing.T, label string) *browserPeer {
	t.Helper()

	settings := webrtc.SettingEngine{}
	settings.SetNetworkTypes([]webrtc.NetworkType{webrtc.NetworkTypeUDP4, webrtc.NetworkTypeUDP6})
	settings.DisableActiveTCP(true)
	settings.SetICEMulticastDNSMode(ice.MulticastDNSModeDisabled)

	pc, err := webrtc.NewAPI(webrtc.WithSettingEngine(settings)).NewPeerConnection(webrtc.Configuration{})
	if err != nil {
		t.Fatalf("new browser peer connection: %v", err)
	}
	t.Cleanup(func() { _ = pc.Close() })

	peer := &browserPeer{
		pc:        pc,
		opened:    make(chan struct{}),
		frames:    make(chan []byte, 4),
		assembler: framing.NewReassembler(framing.WebRTCChunkSize),
	}

	dc, err := pc.CreateDataChannel(label, nil)
	if err != nil {
		t.Fatalf("create data channel: %v", err)
	}
	peer.dc = dc
	dc.OnOpen(func() { close(peer.opened) })
	dc.OnMessage(func(msg webrtc.DataChannelMessage) {
		frame, err := peer.assembler.Push(msg.Data)
		if err != nil || frame == nil {
			return
		}
		select {
		case peer.frames <- frame:
		default:
		}
	})
	return peer
}

// sendFrame chunks one logical frame onto the DataChannel.
func (p *browserPeer) sendFrame(t *testing.T, logical []byte) {
	t.Helper()
	p.sendMu.Lock()
	defer p.sendMu.Unlock()
	p.chunks = framing.Chunk(p.chunks[:0], logical, framing.WebRTCChunkSize)
	for _, part := range p.chunks {
		if err := p.dc.Send(part); err != nil {
			t.Fatalf("browser send: %v", err)
		}
	}
}

func (p *browserPeer) awaitFrame(t *testing.T) []byte {
	t.Helper()
	select {
	case frame := <-p.frames:
		return frame
	case <-time.After(testTimeout):
		t.Fatal("browser did not receive a frame")
		return nil
	}
}

// harness owns a Manager plus the browser peers signaling with it.
type harness struct {
	manager *Manager
	served  chan transport.FrameConn

	mu         sync.Mutex
	peers      map[SessionKey]*browserPeer
	candidates []Candidate
}

func newHarness(t *testing.T, maxSessions int, mappedIPs []string) *harness {
	t.Helper()

	h := &harness{
		served: make(chan transport.FrameConn, 4),
		peers:  make(map[SessionKey]*browserPeer),
	}
	manager, err := New(Options{
		Logger:           quietLogger(),
		MaxSessions:      maxSessions,
		NAT1To1IPs:       mappedIPs,
		OnLocalCandidate: h.routeCandidate,
		Serve: func(ctx context.Context, conn transport.FrameConn) {
			h.served <- conn
			<-ctx.Done()
		},
	})
	if err != nil {
		t.Fatalf("new manager: %v", err)
	}
	t.Cleanup(func() {
		if err := manager.Close(); err != nil {
			t.Errorf("close manager: %v", err)
		}
	})
	h.manager = manager
	return h
}

func (h *harness) routeCandidate(key SessionKey, c Candidate) {
	h.mu.Lock()
	peer := h.peers[key]
	h.candidates = append(h.candidates, c)
	h.mu.Unlock()
	if peer == nil {
		return
	}
	peer.toBrowser.do(func() {
		init := webrtc.ICECandidateInit{Candidate: c.Candidate, SDPMLineIndex: &c.SDPMLineIndex}
		if c.SDPMid != "" {
			init.SDPMid = &c.SDPMid
		}
		_ = peer.pc.AddICECandidate(init)
	})
}

func (h *harness) localCandidates() []Candidate {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]Candidate(nil), h.candidates...)
}

// negotiate runs the full offer/answer plus trickle exchange for one key and
// returns the browser peer.
func (h *harness) negotiate(t *testing.T, key SessionKey, label string) *browserPeer {
	t.Helper()

	peer := newBrowserPeer(t, label)
	h.mu.Lock()
	h.peers[key] = peer
	h.mu.Unlock()

	peer.pc.OnICECandidate(func(candidate *webrtc.ICECandidate) {
		if candidate == nil {
			return
		}
		init := candidate.ToJSON()
		remote := Candidate{Candidate: init.Candidate}
		if init.SDPMid != nil {
			remote.SDPMid = *init.SDPMid
		}
		if init.SDPMLineIndex != nil {
			remote.SDPMLineIndex = *init.SDPMLineIndex
		}
		peer.toRelay.do(func() {
			if err := h.manager.AddRemoteCandidate(key, remote); err != nil {
				t.Logf("add remote candidate: %v", err)
			}
		})
	})

	offer, err := peer.pc.CreateOffer(nil)
	if err != nil {
		t.Fatalf("create offer: %v", err)
	}
	if err := peer.pc.SetLocalDescription(offer); err != nil {
		t.Fatalf("set browser local description: %v", err)
	}

	answerSDP, err := h.manager.HandleOffer(context.Background(), key, offer.SDP)
	if err != nil {
		t.Fatalf("handle offer: %v", err)
	}
	peer.toRelay.release()

	if err := peer.pc.SetRemoteDescription(webrtc.SessionDescription{
		Type: webrtc.SDPTypeAnswer,
		SDP:  answerSDP,
	}); err != nil {
		t.Fatalf("set browser remote description: %v", err)
	}
	peer.toBrowser.release()
	return peer
}

func (h *harness) awaitServe(t *testing.T) transport.FrameConn {
	t.Helper()
	select {
	case conn := <-h.served:
		return conn
	case <-time.After(testTimeout):
		t.Fatal("no connection was handed to the hub")
		return nil
	}
}

func (h *harness) awaitSessionCount(t *testing.T, want int) {
	t.Helper()
	deadline := time.Now().Add(testTimeout)
	for {
		if got := h.manager.SessionCount(); got == want {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("session count = %d, want %d", h.manager.SessionCount(), want)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestManagerServesDataChannelOverLoopback(t *testing.T) {
	h := newHarness(t, 0, nil)
	if h.manager.LocalPort() <= 0 {
		t.Fatal("manager did not resolve an ice port")
	}

	key := SessionKey{ClientID: "client-1", RequestID: "req-1"}
	peer := h.negotiate(t, key, dataChannelLabel)

	conn := h.awaitServe(t)
	if conn.TransportName() != transport.TransportWebRTC {
		t.Fatalf("transport = %q, want %q", conn.TransportName(), transport.TransportWebRTC)
	}
	if conn.Codec() != transport.CodecBinary {
		t.Fatalf("codec = %v, want CodecBinary", conn.Codec())
	}

	select {
	case <-peer.opened:
	case <-time.After(testTimeout):
		t.Fatal("browser data channel never opened")
	}

	// Multi-chunk logical frame relay -> browser.
	outbound := bytes.Repeat([]byte("herdr-relay-payload"), 3000)
	if len(outbound) <= webrtcStartBody {
		t.Fatalf("outbound frame of %d bytes does not span chunks", len(outbound))
	}
	ctx, cancel := context.WithTimeout(context.Background(), testTimeout)
	defer cancel()
	if err := conn.WriteFrame(ctx, outbound); err != nil {
		t.Fatalf("write frame: %v", err)
	}
	if got := peer.awaitFrame(t); !bytes.Equal(got, outbound) {
		t.Fatalf("browser received %d bytes, want %d matching bytes", len(got), len(outbound))
	}

	// Multi-chunk logical frame browser -> relay.
	inbound := bytes.Repeat([]byte("herdr-browser-payload"), 3000)
	peer.sendFrame(t, inbound)
	got, err := conn.ReadFrame(ctx)
	if err != nil {
		t.Fatalf("read frame: %v", err)
	}
	if !bytes.Equal(got, inbound) {
		t.Fatalf("relay received %d bytes, want %d matching bytes", len(got), len(inbound))
	}

	// Close leaves no live session and unblocks the hub reader.
	if err := h.manager.Close(); err != nil {
		t.Fatalf("close manager: %v", err)
	}
	if count := h.manager.SessionCount(); count != 0 {
		t.Fatalf("session count after close = %d, want 0", count)
	}
	if _, err := conn.ReadFrame(context.Background()); !errors.Is(err, transport.ErrFrameConnClosed) {
		t.Fatalf("read after close = %v, want ErrFrameConnClosed", err)
	}
}

func TestManagerRefusesUnexpectedDataChannelLabel(t *testing.T) {
	h := newHarness(t, 0, nil)

	key := SessionKey{ClientID: "client-1", RequestID: "req-bad-label"}
	h.negotiate(t, key, "not-herdr")

	h.awaitSessionCount(t, 0)
	select {
	case <-h.served:
		t.Fatal("hub was handed a connection for a refused data channel")
	default:
	}
}

func TestManagerEnforcesMaxSessions(t *testing.T) {
	h := newHarness(t, 1, nil)

	first := SessionKey{ClientID: "client-1", RequestID: "req-1"}
	h.negotiate(t, first, dataChannelLabel)
	h.awaitServe(t)

	second := newBrowserPeer(t, dataChannelLabel)
	offer, err := second.pc.CreateOffer(nil)
	if err != nil {
		t.Fatalf("create offer: %v", err)
	}
	if err := second.pc.SetLocalDescription(offer); err != nil {
		t.Fatalf("set local description: %v", err)
	}

	key := SessionKey{ClientID: "client-2", RequestID: "req-2"}
	if _, err := h.manager.HandleOffer(context.Background(), key, offer.SDP); err == nil {
		t.Fatal("second offer was accepted over the session limit")
	} else if !strings.Contains(err.Error(), "session limit reached") {
		t.Fatalf("error = %v, want a session limit error", err)
	}
	if count := h.manager.SessionCount(); count != 1 {
		t.Fatalf("session count = %d, want 1", count)
	}
}

func TestManagerRenegotiatesKnownSession(t *testing.T) {
	h := newHarness(t, 1, nil)

	key := SessionKey{ClientID: "client-1", RequestID: "req-1"}
	peer := h.negotiate(t, key, dataChannelLabel)
	h.awaitServe(t)

	offer, err := peer.pc.CreateOffer(&webrtc.OfferOptions{ICERestart: true})
	if err != nil {
		t.Fatalf("create restart offer: %v", err)
	}
	if err := peer.pc.SetLocalDescription(offer); err != nil {
		t.Fatalf("set restart local description: %v", err)
	}

	answerSDP, err := h.manager.HandleOffer(context.Background(), key, offer.SDP)
	if err != nil {
		t.Fatalf("renegotiate: %v", err)
	}
	if answerSDP == "" {
		t.Fatal("renegotiation returned an empty answer")
	}
	if count := h.manager.SessionCount(); count != 1 {
		t.Fatalf("session count = %d, want 1", count)
	}
	select {
	case <-h.served:
		t.Fatal("renegotiation handed a second connection to the hub")
	default:
	}
}

func TestManagerCloseSessionAndUnknownSignaling(t *testing.T) {
	h := newHarness(t, 0, nil)

	unknown := SessionKey{ClientID: "client-x", RequestID: "req-x"}
	if err := h.manager.AddRemoteCandidate(unknown, Candidate{Candidate: "candidate:0 1 udp 1 127.0.0.1 1 typ host"}); !errors.Is(err, errNoSession) {
		t.Fatalf("candidate for unknown session = %v, want errNoSession", err)
	}
	h.manager.CloseSession(unknown, "test")

	key := SessionKey{ClientID: "client-1", RequestID: "req-1"}
	h.negotiate(t, key, dataChannelLabel)
	conn := h.awaitServe(t)

	h.manager.CloseSession(key, "test teardown")
	h.awaitSessionCount(t, 0)
	if _, err := conn.ReadFrame(context.Background()); !errors.Is(err, transport.ErrFrameConnClosed) {
		t.Fatalf("read after session close = %v, want ErrFrameConnClosed", err)
	}
}

func TestManagerAdvertisesMappedAddress(t *testing.T) {
	h := newHarness(t, 0, nil)
	h.manager.SetNAT1To1IPs([]string{"203.0.113.7"})

	key := SessionKey{ClientID: "client-1", RequestID: "req-1"}
	h.negotiate(t, key, dataChannelLabel)
	h.awaitServe(t)

	deadline := time.Now().Add(testTimeout)
	for {
		for _, candidate := range h.localCandidates() {
			if strings.Contains(candidate.Candidate, "203.0.113.7") &&
				strings.Contains(candidate.Candidate, "typ srflx") {
				return
			}
		}
		if time.Now().After(deadline) {
			t.Fatal("no server reflexive candidate for the mapped address was gathered")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestManagerClosesSessionOnFramingViolation(t *testing.T) {
	h := newHarness(t, 0, nil)

	key := SessionKey{ClientID: "client-1", RequestID: "req-1"}
	peer := h.negotiate(t, key, dataChannelLabel)
	conn := h.awaitServe(t)
	select {
	case <-peer.opened:
	case <-time.After(testTimeout):
		t.Fatal("browser data channel never opened")
	}

	// An END chunk with no START is a protocol violation.
	if err := peer.dc.Send([]byte{framing.Version, framing.FlagEnd, 0xaa}); err != nil {
		t.Fatalf("browser send: %v", err)
	}

	h.awaitSessionCount(t, 0)
	if _, err := conn.ReadFrame(context.Background()); !errors.Is(err, transport.ErrFrameConnClosed) {
		t.Fatalf("read after violation = %v, want ErrFrameConnClosed", err)
	}
}

func TestManagerSweepsStalledAssembly(t *testing.T) {
	h := newHarness(t, 0, nil)

	key := SessionKey{ClientID: "client-1", RequestID: "req-1"}
	peer := h.negotiate(t, key, dataChannelLabel)
	conn := h.awaitServe(t)
	select {
	case <-peer.opened:
	case <-time.After(testTimeout):
		t.Fatal("browser data channel never opened")
	}

	// Send only the START chunk of a two chunk frame, then let the sweeper run
	// with a clock past the stall timeout.
	parts := framing.Chunk(nil, bytes.Repeat([]byte{4}, webrtcStartBody+1), framing.WebRTCChunkSize)
	if err := peer.dc.Send(parts[0]); err != nil {
		t.Fatalf("browser send: %v", err)
	}

	deadline := time.Now().Add(testTimeout)
	for {
		h.manager.sweepStalled(time.Now().Add(framing.StallTimeout + time.Second))
		if h.manager.SessionCount() == 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("stalled session was not swept")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if _, err := conn.ReadFrame(context.Background()); !errors.Is(err, transport.ErrFrameConnClosed) {
		t.Fatalf("read after stall = %v, want ErrFrameConnClosed", err)
	}
}

func TestDataChannelConnRejectsOversizedFrame(t *testing.T) {
	conn := newDataChannelConn(nil, func(string) {})
	err := conn.WriteFrame(context.Background(), make([]byte, framing.MaxLogicalBytes+1))
	if !errors.Is(err, framing.ErrFrameTooLarge) {
		t.Fatalf("write oversized frame = %v, want %v", err, framing.ErrFrameTooLarge)
	}
}

func TestManagerCloseReleasesGoroutines(t *testing.T) {
	baseline := runtime.NumGoroutine()

	h := newHarness(t, 0, nil)
	key := SessionKey{ClientID: "client-1", RequestID: "req-1"}
	h.negotiate(t, key, dataChannelLabel)
	h.awaitServe(t)

	if err := h.manager.Close(); err != nil {
		t.Fatalf("close manager: %v", err)
	}
	if count := h.manager.SessionCount(); count != 0 {
		t.Fatalf("session count after close = %d, want 0", count)
	}

	// The browser peer is torn down by its own cleanup, so only its goroutines
	// may still be winding down; the manager must not keep any of its own.
	deadline := time.Now().Add(testTimeout)
	for runtime.NumGoroutine() > baseline+8 {
		if time.Now().After(deadline) {
			t.Fatalf("goroutines after close = %d, baseline %d", runtime.NumGoroutine(), baseline)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// stunMagicCookie is the RFC 5389 magic cookie every STUN message carries.
const stunMagicCookie = 0x2112A442

// startSTUNResponder runs a minimal STUN binding server on loopback. portDelta
// offsets the reflected port so a NAT that rewrites the port can be simulated
// without one.
func startSTUNResponder(t *testing.T, portDelta int) string {
	t.Helper()

	conn, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatalf("listen stun responder: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })

	go func() {
		buf := make([]byte, 1500)
		for {
			n, from, err := conn.ReadFromUDP(buf)
			if err != nil {
				return
			}
			reply, ok := stunBindingSuccess(buf[:n], from.IP, from.Port+portDelta)
			if !ok {
				continue
			}
			_, _ = conn.WriteToUDP(reply, from)
		}
	}()
	return conn.LocalAddr().String()
}

// stunBindingSuccess encodes a binding success carrying only an IPv4
// XOR-MAPPED-ADDRESS, which is all address discovery reads.
func stunBindingSuccess(request []byte, ip net.IP, port int) ([]byte, bool) {
	const bindingRequest, bindingSuccess, attrXORMappedAddress = 0x0001, 0x0101, 0x0020
	v4 := ip.To4()
	if len(request) < 20 || binary.BigEndian.Uint16(request[:2]) != bindingRequest || v4 == nil {
		return nil, false
	}

	reply := make([]byte, 0, 32)
	reply = binary.BigEndian.AppendUint16(reply, bindingSuccess)
	// One 4-byte attribute header plus its 8-byte IPv4 value.
	reply = binary.BigEndian.AppendUint16(reply, 12)
	reply = binary.BigEndian.AppendUint32(reply, stunMagicCookie)
	reply = append(reply, request[8:20]...) // echo the transaction id
	reply = binary.BigEndian.AppendUint16(reply, attrXORMappedAddress)
	reply = binary.BigEndian.AppendUint16(reply, 8)
	reply = append(reply, 0x00, 0x01) // reserved, address family IPv4
	reply = binary.BigEndian.AppendUint16(reply, uint16(port)^(stunMagicCookie>>16))
	return binary.BigEndian.AppendUint32(reply, binary.BigEndian.Uint32(v4)^stunMagicCookie), true
}

// TestDiscoverMappedAddressRequiresSTUNServer keeps a relay whose gateway
// advertises no STUN port from silently reporting a mapping it never learned.
func TestDiscoverMappedAddressRequiresSTUNServer(t *testing.T) {
	h := newHarness(t, 1, nil)

	if _, err := h.manager.DiscoverMappedAddresses(context.Background()); !errors.Is(err, errNoSTUNServer) {
		t.Fatalf("discovery without a server = %v, want errNoSTUNServer", err)
	}
}

// TestDiscoverMappedAddressReflectsSharedSocket is the whole point of the
// universal mux: the discovered mapping must belong to the one socket ICE
// gathers on, so the reflected port is the shared ICE port.
func TestDiscoverMappedAddressReflectsSharedSocket(t *testing.T) {
	server := startSTUNResponder(t, 0)
	h := newHarness(t, 1, nil)
	h.manager.SetSTUNServers([]string{"stun:" + server})

	ctx, cancel := context.WithTimeout(context.Background(), testTimeout)
	defer cancel()
	discovered, err := h.manager.DiscoverMappedAddresses(ctx)
	if err != nil {
		t.Fatalf("discover mapped address: %v", err)
	}
	if len(discovered) == 0 {
		t.Fatal("discovery returned no mappings")
	}
	mapped := discovered[0]
	if got := mapped.Addr().String(); got != "127.0.0.1" {
		t.Fatalf("mapped ip = %s, want 127.0.0.1", got)
	}
	if int(mapped.Port()) != h.manager.LocalPort() {
		t.Fatalf("mapped port = %d, want the shared ICE port %d", mapped.Port(), h.manager.LocalPort())
	}
}

// TestPublishMappedAddressPortPreservedFeedsNAT1To1 covers the common home
// router: the port survives the NAT, so pion can gather the reflexive
// candidate itself and no candidate has to be synthesised.
func TestPublishMappedAddressPortPreservedFeedsNAT1To1(t *testing.T) {
	h := newHarness(t, 1, []string{"198.51.100.7"})
	mapped := netip.AddrPortFrom(netip.MustParseAddr("203.0.113.9"), uint16(h.manager.LocalPort()))

	h.manager.PublishMappedAddresses([]netip.AddrPort{mapped})

	h.manager.mu.Lock()
	ips := h.manager.natIPsLocked()
	h.manager.mu.Unlock()
	if !slices.Contains(ips, "203.0.113.9") {
		t.Fatalf("nat 1:1 addresses = %v, want the discovered address", ips)
	}
	if !slices.Contains(ips, "198.51.100.7") {
		t.Fatalf("nat 1:1 addresses = %v, want the port mapping to survive discovery", ips)
	}
	if got := h.localCandidates(); len(got) != 0 {
		t.Fatalf("trickled %d candidates, want none when NAT1To1 covers the mapping", len(got))
	}
}

// TestPublishMappedAddressPortPreservedUpdatesPendingSession covers discovery
// racing the first offer. Rebuilding the API only affects later sessions, so a
// session already reserved from the old API must receive the mapping by
// trickle even when the NAT preserved the shared socket's port.
func TestPublishMappedAddressPortPreservedUpdatesPendingSession(t *testing.T) {
	h := newHarness(t, 1, nil)
	key := SessionKey{ClientID: "client-1", RequestID: "req-1"}
	h.manager.mu.Lock()
	h.manager.newSessionLocked(key)
	h.manager.mu.Unlock()
	mapped := netip.AddrPortFrom(netip.MustParseAddr("203.0.113.9"), uint16(h.manager.LocalPort()))

	h.manager.PublishMappedAddresses([]netip.AddrPort{mapped})
	// A keepalive reporting the same mapping must not duplicate the candidate.
	h.manager.PublishMappedAddresses([]netip.AddrPort{mapped})

	got := h.localCandidates()
	if len(got) != 1 {
		t.Fatalf("trickled %d candidates, want exactly 1", len(got))
	}
	line := got[0].Candidate
	if !strings.Contains(line, "typ srflx") {
		t.Fatalf("candidate = %q, want a server-reflexive candidate", line)
	}
	parsed, err := ice.UnmarshalCandidate(strings.TrimPrefix(line, "candidate:"))
	if err != nil {
		t.Fatalf("synthesised candidate %q does not parse: %v", line, err)
	}
	if parsed.Address() != "203.0.113.9" || parsed.Port() != h.manager.LocalPort() {
		t.Fatalf("candidate address = %s:%d, want the discovered 203.0.113.9:%d",
			parsed.Address(), parsed.Port(), h.manager.LocalPort())
	}
}

// TestPublishMappedAddressPortChangedTricklesSrflx covers carrier-grade NAT,
// which rewrites the port: NAT1To1 cannot express that, so exactly one
// reflexive candidate is synthesised and trickled per session.
func TestPublishMappedAddressPortChangedTricklesSrflx(t *testing.T) {
	h := newHarness(t, 1, nil)
	key := SessionKey{ClientID: "client-1", RequestID: "req-1"}
	h.manager.mu.Lock()
	h.manager.newSessionLocked(key)
	h.manager.mu.Unlock()

	externalPort := h.manager.LocalPort() + 1
	mapped := netip.AddrPortFrom(netip.MustParseAddr("203.0.113.9"), uint16(externalPort))
	h.manager.PublishMappedAddresses([]netip.AddrPort{mapped})
	// A repeated discovery of the same address must not duplicate it.
	h.manager.PublishMappedAddresses([]netip.AddrPort{mapped})

	got := h.localCandidates()
	if len(got) != 1 {
		t.Fatalf("trickled %d candidates, want exactly 1", len(got))
	}
	line := got[0].Candidate
	if !strings.HasPrefix(line, "candidate:") {
		t.Fatalf("candidate = %q, want the trickle prefix", line)
	}
	if !strings.Contains(line, "typ srflx") {
		t.Fatalf("candidate = %q, want a server-reflexive candidate", line)
	}
	if got[0].SDPMid != "0" || got[0].SDPMLineIndex != 0 {
		t.Fatalf("candidate mid = %q/%d, want the DataChannel section 0/0", got[0].SDPMid, got[0].SDPMLineIndex)
	}

	parsed, err := ice.UnmarshalCandidate(strings.TrimPrefix(line, "candidate:"))
	if err != nil {
		t.Fatalf("synthesised candidate %q does not parse: %v", line, err)
	}
	if parsed.Type() != ice.CandidateTypeServerReflexive {
		t.Fatalf("candidate type = %s, want srflx", parsed.Type())
	}
	if parsed.Address() != "203.0.113.9" || parsed.Port() != externalPort {
		t.Fatalf("candidate address = %s:%d, want the discovered 203.0.113.9:%d",
			parsed.Address(), parsed.Port(), externalPort)
	}
	related := parsed.RelatedAddress()
	if related == nil {
		t.Fatal("candidate carries no raddr/rport")
	}
	if related.Address != "0.0.0.0" || related.Port != h.manager.LocalPort() {
		t.Fatalf("candidate raddr/rport = %s:%d, want the shared socket 0.0.0.0:%d",
			related.Address, related.Port, h.manager.LocalPort())
	}
}

// Candidate types are read from strings the remote peer supplies, so the parser
// must be closed: an unknown or malformed type becomes "other" rather than a new
// map key an attacker chooses.
func TestCandidateTypeIsClosedSet(t *testing.T) {
	cases := map[string]string{
		"candidate:1 1 udp 2130706431 192.168.1.5 41234 typ host":                              "host",
		"candidate:2 1 udp 1694498815 203.0.113.7 51820 typ srflx raddr 0.0.0.0 rport 41234":   "srflx",
		"candidate:3 1 udp 1685987071 203.0.113.9 3478 typ prflx":                              "prflx",
		"candidate:4 1 udp 41885439 198.51.100.4 3478 typ relay raddr 203.0.113.7 rport 51820": "relay",
		"candidate:5 1 udp 2130706431 192.168.1.5 41234 typ bogus":                             "other",
		"candidate:6 1 udp 2130706431 192.168.1.5 41234 typ":                                   "other",
		"":         "other",
		"typ host": "host",
	}
	for line, want := range cases {
		if got := candidateType(line); got != want {
			t.Errorf("candidateType(%q) = %q, want %q", line, got, want)
		}
	}
}

// A report must never carry an address: /healthz is unauthenticated.
func TestSessionReportCarriesTypesOnly(t *testing.T) {
	h := newHarness(t, 0, nil)
	key := SessionKey{ClientID: "client-1", RequestID: "req-1"}
	h.negotiate(t, key, dataChannelLabel)

	if err := h.manager.AddRemoteCandidate(key, Candidate{
		Candidate: "candidate:9 1 udp 1694498815 203.0.113.7 51820 typ srflx raddr 0.0.0.0 rport 41234",
	}); err != nil {
		t.Fatalf("add remote candidate: %v", err)
	}

	reports := h.manager.SessionReports()
	if len(reports) != 1 {
		t.Fatalf("reports = %d, want 1", len(reports))
	}
	if reports[0].RemoteTypes["srflx"] != 1 {
		t.Fatalf("remote types = %v, want one srflx", reports[0].RemoteTypes)
	}
	encoded, err := json.Marshal(reports[0])
	if err != nil {
		t.Fatalf("marshal report: %v", err)
	}
	for _, forbidden := range []string{"203.0.113.7", "192.168", "51820", "41234"} {
		if bytes.Contains(encoded, []byte(forbidden)) {
			t.Fatalf("report leaked %q: %s", forbidden, encoded)
		}
	}
}

// The counters exist to answer whether the direct path forms in the field, so
// each finished session must land in exactly one bucket: a nominated pair is a
// direct session, and a session that closes without one is the cohort that
// would justify symmetric-NAT port prediction.
func TestOutcomeCountersClassifyEachSessionOnce(t *testing.T) {
	h := newHarness(t, 0, nil)
	if got := h.manager.Outcomes(); got.Direct != 0 || got.Relayed != 0 {
		t.Fatalf("fresh manager outcomes = %+v, want zeroes", got)
	}

	relayed := SessionKey{ClientID: "client-relayed", RequestID: "req-1"}
	h.negotiate(t, relayed, dataChannelLabel)
	h.manager.CloseSession(relayed, "test")
	// Closing twice must not double count; close is guarded by closeOnce.
	h.manager.CloseSession(relayed, "test again")

	if got := h.manager.Outcomes(); got.Direct != 0 || got.Relayed != 1 {
		t.Fatalf("after an unnominated session outcomes = %+v, want 0 direct / 1 relayed", got)
	}

	direct := SessionKey{ClientID: "client-direct", RequestID: "req-2"}
	h.negotiate(t, direct, dataChannelLabel)
	session := h.manager.lookup(direct)
	if session == nil {
		t.Fatal("expected a live session to mark as nominated")
	}
	session.mu.Lock()
	session.selectedLocal, session.selectedRemote = "host", "srflx"
	session.mu.Unlock()
	h.manager.CloseSession(direct, "test")

	if got := h.manager.Outcomes(); got.Direct != 1 || got.Relayed != 1 {
		t.Fatalf("after a nominated session outcomes = %+v, want 1 direct / 1 relayed", got)
	}
}
