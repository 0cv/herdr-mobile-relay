package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"github.com/coder/websocket"

	"github.com/0cv/herdr-mobile-relay/internal/gatewaywire"
)

// errTooManyClients reports that a registration is at its concurrent client cap.
var errTooManyClients = errors.New("gateway: relay is at its client cap")

// errAtCapacity reports that the gateway as a whole is at its global client
// ceiling, whatever the relay's own cap allows.
var errAtCapacity = errors.New("gateway: at its global client capacity")

// errLinkClosed reports that a registration went away mid-attach.
var errLinkClosed = errors.New("gateway: relay link is closed")

// relayLink is one registered computer relay: a multiplexed WebSocket carrying
// every phone connection paired with that relay id.
type relayLink struct {
	server  *Server
	relayID string
	conn    *websocket.Conn
	logger  *slog.Logger
	ctx     context.Context
	cancel  context.CancelFunc

	// unansweredPings counts pings sent since the last pong.
	unansweredPings atomic.Int64

	mu     sync.Mutex
	conns  map[uint32]*clientConn
	nextID uint32
	closed bool

	closeOnce sync.Once
	// closeDone is closed once the close handshake has run, so the HTTP handler
	// does not tear the socket down underneath it.
	closeDone chan struct{}
}

// handleRelay accepts one outbound registration from a computer relay and runs
// its multiplexed link until either side goes away.
func (s *Server) handleRelay(w http.ResponseWriter, r *http.Request) {
	if s.stopped() {
		http.Error(w, "gateway is shutting down", http.StatusServiceUnavailable)
		return
	}
	if !s.connectLimiter.allow("relay:"+s.clientIP(r), s.now()) {
		http.Error(w, "too many registrations", http.StatusTooManyRequests)
		return
	}

	conn, err := websocket.Accept(w, r, acceptOptions())
	if err != nil {
		s.logger.Debug("relay upgrade failed", "error", err)
		return
	}
	defer conn.CloseNow()
	conn.SetReadLimit(gatewaywire.HeaderSize + gatewaywire.MaxFramePayload)

	relayID, ok := s.relayHello(r.Context(), conn)
	if !ok {
		return
	}

	link, refusal := s.registerRelay(r.Context(), relayID, conn)
	if link == nil {
		s.logger.Warn("relay registration refused",
			"relay", shortID(relayID), "code", refusal)
		reject(r.Context(), conn, refusal, refusalMessage(refusal))
		return
	}
	if err := writeJSON(r.Context(), conn, gatewaywire.ReadyMessage{
		Type:  gatewaywire.TypeReady,
		Proto: gatewaywire.Proto,
	}); err != nil {
		s.logger.Debug("relay ready write failed", "relay", shortID(relayID), "error", err)
		link.close(websocket.StatusInternalError, "")
		link.awaitClose()
		s.unregisterRelay(link)
		return
	}
	s.logger.Info("relay registered", "relay", shortID(relayID))
	link.run()
}

// relayHello performs the challenge exchange. A relay ignores the nonce; it is
// sent so both endpoints share one hello shape.
func (s *Server) relayHello(ctx context.Context, conn *websocket.Conn) (string, bool) {
	nonce, err := randomNonce()
	if err != nil {
		s.logger.Error("nonce generation failed", "error", err)
		reject(ctx, conn, gatewaywire.CodeInternal, "gateway could not generate a challenge")
		return "", false
	}
	if err := writeJSON(ctx, conn, gatewaywire.ServerHello{
		Type:     gatewaywire.TypeServerHello,
		Proto:    gatewaywire.Proto,
		Nonce:    nonce,
		StunPort: s.stunPort,
	}); err != nil {
		s.logger.Debug("relay hello write failed", "error", err)
		return "", false
	}

	data, err := readHello(ctx, conn, helloTimeout)
	if err != nil {
		s.logger.Debug("relay hello read failed", "error", err)
		reject(ctx, conn, gatewaywire.CodeBadHello, "register hello was not received")
		return "", false
	}
	var hello gatewaywire.RegisterHello
	if err := json.Unmarshal(data, &hello); err != nil || hello.Type != gatewaywire.TypeRegister {
		reject(ctx, conn, gatewaywire.CodeBadHello, "register hello is malformed")
		return "", false
	}
	if hello.Proto != gatewaywire.Proto {
		reject(ctx, conn, gatewaywire.CodeBadHello, "unsupported gateway protocol version")
		return "", false
	}
	if !gatewaywire.ValidRelayID(hello.RelayID) {
		reject(ctx, conn, gatewaywire.CodeBadHello, "relay id is malformed")
		return "", false
	}
	return hello.RelayID, true
}

// registerRelay installs a registration, replacing any earlier link for the
// same relay id. Re-registration is the common case: a relay that lost its
// connection reconnects, and the stale link must not keep the id hostage.
//
// It returns nil when the gateway is already holding MaxRelays registrations.
// Only a genuinely new id can hit that ceiling: replacing an id that is already
// in the table does not grow it, and refusing a reconnect would strand a relay
// that is merely re-establishing its own link.
func (s *Server) registerRelay(parent context.Context, relayID string, conn *websocket.Conn) (*relayLink, string) {
	ctx, cancel := context.WithCancel(parent)
	link := &relayLink{
		server:    s,
		relayID:   relayID,
		conn:      conn,
		logger:    s.logger,
		ctx:       ctx,
		cancel:    cancel,
		conns:     make(map[uint32]*clientConn),
		closeDone: make(chan struct{}),
	}

	s.mu.Lock()
	previous := s.relays[relayID]
	if previous == nil && s.opts.MaxRelays >= 0 && len(s.relays) >= s.opts.MaxRelays {
		s.mu.Unlock()
		cancel()
		return nil, gatewaywire.CodeAtCapacity
	}
	if previous == nil {
		s.relays[relayID] = link
		s.mu.Unlock()
		return link, ""
	}
	// An incumbent is claimed but not yet displaced: probing it takes a round
	// trip, and the server mutex must not be held across it.
	s.mu.Unlock()

	if previous.alive(parent) {
		// The id belongs to a relay that is demonstrably still there. Displacing
		// it on demand would hand anyone who ever learned a relay id — a shared
		// QR, a compromised phone — a way to evict the real relay in a loop, and
		// on a shared gateway that is an availability lever pointed at a user
		// who did nothing wrong.
		s.logger.Info("relay registration refused, incumbent is live", "relay", shortID(relayID))
		cancel()
		return nil, gatewaywire.CodeRelayBusy
	}

	s.mu.Lock()
	// The table may have moved while the probe ran: another registration could
	// have displaced the incumbent, or it could have unregistered itself. Only
	// replace what we actually probed, and never grow past the ceiling.
	current := s.relays[relayID]
	if current != previous && current != nil {
		s.mu.Unlock()
		s.logger.Info("relay registration lost the displacement race", "relay", shortID(relayID))
		cancel()
		return nil, gatewaywire.CodeRelayBusy
	}
	if current == nil && s.opts.MaxRelays >= 0 && len(s.relays) >= s.opts.MaxRelays {
		s.mu.Unlock()
		cancel()
		return nil, gatewaywire.CodeAtCapacity
	}
	s.relays[relayID] = link
	s.mu.Unlock()

	s.logger.Info("relay registration replaced", "relay", shortID(relayID))
	previous.detachClients(reasonRelayReplaced)
	previous.close(websocket.StatusPolicyViolation, gatewaywire.CodeRelayBusy)
	return link, ""
}

// refusalMessage renders the human half of a registration refusal.
func refusalMessage(code string) string {
	if code == gatewaywire.CodeRelayBusy {
		return "another link for this relay id is still live"
	}
	return "gateway is at its relay capacity"
}

// alive reports whether a registered link still answers. It reuses the link's
// own ping/pong machinery rather than inventing a second liveness mechanism, so
// a relay that is merely quiet — the normal state between phone connections —
// is never mistaken for a dead one.
func (l *relayLink) alive(parent context.Context) bool {
	if l == nil || l.conn == nil {
		return false
	}
	select {
	case <-l.ctx.Done():
		return false
	default:
	}
	ctx, cancel := context.WithTimeout(parent, displaceProbeTimeout)
	defer cancel()
	return l.conn.Ping(ctx) == nil
}

// unregisterRelay removes a link only if it is still the live registration, so a
// replaced link cannot evict its successor while unwinding.
func (s *Server) unregisterRelay(link *relayLink) {
	s.mu.Lock()
	if s.relays[link.relayID] == link {
		delete(s.relays, link.relayID)
	}
	s.mu.Unlock()
}

// run pumps the link until it fails, then tears down everything it owns.
func (l *relayLink) run() {
	defer func() {
		l.close(websocket.StatusNormalClosure, "")
		l.detachClients(reasonRelayGone)
		l.server.unregisterRelay(l)
		l.logger.Info("relay unregistered", "relay", shortID(l.relayID))
		l.awaitClose()
	}()
	go l.maintain()
	l.readPump()
}

// readPump dispatches multiplexed frames from the relay.
func (l *relayLink) readPump() {
	for {
		messageType, data, err := l.conn.Read(l.ctx)
		if err != nil {
			return
		}
		if messageType != websocket.MessageBinary {
			l.close(websocket.StatusUnsupportedData, reasonBinaryOnly)
			return
		}
		op, connID, payload, err := gatewaywire.DecodeFrame(data)
		if err != nil {
			l.logger.Warn("relay frame rejected", "relay", shortID(l.relayID), "error", err)
			l.close(websocket.StatusProtocolError, "malformed_frame")
			return
		}

		switch op {
		case gatewaywire.OpData:
			l.deliverData(connID, payload)
		case gatewaywire.OpClose:
			if client := l.remove(connID); client != nil {
				client.finish(websocket.StatusPolicyViolation, sanitizeReason(payload))
			}
		case gatewaywire.OpPong:
			l.unansweredPings.Store(0)
		case gatewaywire.OpPing:
			if err := l.writeFrame(gatewaywire.OpPong, 0, nil); err != nil {
				return
			}
		default:
			// OpOpen and OpNotice flow gateway to relay only.
			l.logger.Warn("relay sent a gateway-only opcode", "relay", shortID(l.relayID), "opcode", op)
			l.close(websocket.StatusProtocolError, "unexpected_opcode")
			return
		}
	}
}

// deliverData hands one relayed frame to its phone connection and charges it
// against the relay's monthly quota.
func (l *relayLink) deliverData(connID uint32, payload []byte) {
	client := l.client(connID)
	if client == nil {
		// The relay is talking about a connection the gateway already dropped.
		// Tell it once so it can release its own state.
		_ = l.writeFrame(gatewaywire.OpClose, connID, []byte(reasonUnknownConn))
		return
	}
	client.touch(l.server.now())
	l.notify(l.server.quota.add(l.relayID, uint64(len(payload)), l.server.now()))
	if client.deliver(payload) {
		return
	}
	if l.remove(connID) != nil {
		client.kill()
		_ = l.writeFrame(gatewaywire.OpClose, connID, []byte(reasonSlowClient))
	}
}

// maintain keeps the link alive and reaps idle phone connections.
func (l *relayLink) maintain() {
	ping := time.NewTicker(pingInterval)
	defer ping.Stop()

	var idle <-chan time.Time
	if interval := l.server.idleSweepInterval(); interval > 0 {
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		idle = ticker.C
	}

	for {
		select {
		case <-l.ctx.Done():
			return
		case <-ping.C:
			if l.unansweredPings.Load() >= maxUnansweredPings {
				l.logger.Info("relay dropped after missed pongs", "relay", shortID(l.relayID))
				l.close(websocket.StatusPolicyViolation, reasonPingTimeout)
				return
			}
			l.unansweredPings.Add(1)
			if err := l.writeFrame(gatewaywire.OpPing, 0, nil); err != nil {
				l.close(websocket.StatusInternalError, "")
				return
			}
		case <-idle:
			l.sweepIdle()
		}
	}
}

// sweepIdle closes phone connections that carried no traffic for IdleTimeout.
func (l *relayLink) sweepIdle() {
	deadline := l.server.now().Add(-l.server.opts.IdleTimeout).UnixNano()

	l.mu.Lock()
	var stale []*clientConn
	for connID, client := range l.conns {
		if client.lastActivity.Load() > deadline {
			continue
		}
		delete(l.conns, connID)
		stale = append(stale, client)
	}
	l.mu.Unlock()
	l.server.releaseClients(len(stale))

	for _, client := range stale {
		client.finish(websocket.StatusPolicyViolation, reasonIdleTimeout)
		_ = l.writeFrame(gatewaywire.OpClose, client.connID, []byte(reasonIdleTimeout))
	}
}

// attach allocates a connection id and registers a phone connection under it.
// Ids increase monotonically per link and are never reused, so a late frame for
// a dead connection can never be mistaken for a live one.
//
// The global ceiling is charged first: checking the per-relay cap first would
// let a flood spread over many relay ids stay under every per-relay cap while
// blowing past what the gateway as a whole can hold.
func (l *relayLink) attach(client *clientConn) error {
	if !l.server.reserveClient() {
		return errAtCapacity
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.closed {
		l.server.releaseClients(1)
		return errLinkClosed
	}
	if limit := l.server.opts.MaxClientsPerRelay; limit >= 0 && len(l.conns) >= limit {
		l.server.releaseClients(1)
		return errTooManyClients
	}
	l.nextID++
	if l.nextID == 0 {
		l.nextID = 1
	}
	client.connID = l.nextID
	l.conns[client.connID] = client
	return nil
}

func (l *relayLink) client(connID uint32) *clientConn {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.conns[connID]
}

// remove detaches a connection id. Exactly one caller wins, and the winner owns
// closing the phone socket, notifying the relay, and — through this call —
// giving the slot back to the global ceiling.
func (l *relayLink) remove(connID uint32) *clientConn {
	l.mu.Lock()
	defer l.mu.Unlock()
	client := l.conns[connID]
	if client != nil {
		delete(l.conns, connID)
		l.server.releaseClients(1)
	}
	return client
}

// detachClients closes every phone connection on this link. The relay is not
// notified per connection: the link itself is going away.
func (l *relayLink) detachClients(reason string) {
	l.mu.Lock()
	clients := make([]*clientConn, 0, len(l.conns))
	for connID, client := range l.conns {
		clients = append(clients, client)
		delete(l.conns, connID)
	}
	l.mu.Unlock()
	l.server.releaseClients(len(clients))

	for _, client := range clients {
		client.finish(websocket.StatusGoingAway, reason)
	}
}

// close ends the link once, sending a close frame so the relay learns why it was
// dropped. Later calls keep the first status.
//
// The handshake runs in the background because the WebSocket library's Close
// waits for the peer's echo behind the same lock the read pump holds; doing it
// synchronously would stall whoever is retiring the link — notably a relay that
// is re-registering over a stale one. The context is cancelled afterwards so a
// silent peer still loses the link once the handshake times out.
func (l *relayLink) close(status websocket.StatusCode, reason string) {
	l.closeOnce.Do(func() {
		l.mu.Lock()
		l.closed = true
		l.mu.Unlock()
		go func() {
			defer close(l.closeDone)
			_ = l.conn.Close(status, reason)
			l.cancel()
		}()
	})
}

// awaitClose blocks until the close handshake started by close has finished. The
// HTTP handler calls it before returning, because returning would drop the raw
// socket and replace the close frame with an unexplained EOF.
func (l *relayLink) awaitClose() {
	<-l.closeDone
}

// writeFrame sends one multiplexed frame. Writes from several phone goroutines
// are safe: coder/websocket serializes them internally.
func (l *relayLink) writeFrame(op byte, connID uint32, payload []byte) error {
	return l.writeRaw(gatewaywire.EncodeFrame(op, connID, payload))
}

// writeRaw sends an already-encoded multiplexed frame.
func (l *relayLink) writeRaw(frame []byte) error {
	ctx, cancel := context.WithTimeout(l.ctx, writeTimeout)
	defer cancel()
	return l.conn.Write(ctx, websocket.MessageBinary, frame)
}

// notify forwards advisory quota notices to the relay. Notices are best effort:
// a failed write is a link problem the read pump will observe anyway.
func (l *relayLink) notify(notices []gatewaywire.NoticePayload) {
	for i := range notices {
		payload, err := json.Marshal(notices[i])
		if err != nil {
			l.logger.Error("notice encoding failed", "error", err)
			continue
		}
		if err := l.writeFrame(gatewaywire.OpNotice, 0, payload); err != nil {
			l.logger.Debug("notice write failed", "relay", shortID(l.relayID), "error", err)
			return
		}
		l.logger.Info("quota notice sent", "relay", shortID(l.relayID), "kind", notices[i].Kind)
	}
}
