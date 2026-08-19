package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"github.com/coder/websocket"

	"github.com/0cv/herdr-mobile-relay/internal/gatewaywire"
)

// clientConn is one phone connection. Unlike the relay link it is not
// multiplexed: after the hello exchange it carries bare binary Herdr frames,
// which the gateway wraps in OpData toward the relay and unwraps on the way
// back.
type clientConn struct {
	link   *relayLink
	conn   *websocket.Conn
	connID uint32
	ctx    context.Context
	cancel context.CancelFunc

	// lastActivity is the unix-nano stamp of the last frame in either
	// direction, read by the link's idle sweep.
	lastActivity atomic.Int64

	// frameBuf is the reusable multiplex frame for phone-to-relay copies. Only
	// the read pump touches it, so the hot path allocates nothing.
	frameBuf []byte

	// out isolates this phone from its siblings: the relay read pump enqueues
	// without blocking, so one stalled phone cannot stall the whole link.
	out chan []byte

	sendMu      sync.Mutex
	outClosed   bool
	queuedBytes int
	drainStatus websocket.StatusCode
	drainReason string

	writerDone chan struct{}
	closeOnce  sync.Once
}

// handleConnect pairs one phone with a registered relay and copies frames until
// either side goes away.
func (s *Server) handleConnect(w http.ResponseWriter, r *http.Request) {
	if s.stopped() {
		http.Error(w, "gateway is shutting down", http.StatusServiceUnavailable)
		return
	}

	conn, err := websocket.Accept(w, r, acceptOptions())
	if err != nil {
		s.logger.Debug("client upgrade failed", "error", err)
		return
	}
	defer conn.CloseNow()
	conn.SetReadLimit(gatewaywire.MaxFramePayload)

	ctx := r.Context()
	nonce, err := randomNonce()
	if err != nil {
		s.logger.Error("nonce generation failed", "error", err)
		reject(ctx, conn, gatewaywire.CodeInternal, "gateway could not generate a challenge")
		return
	}
	if err := writeJSON(ctx, conn, gatewaywire.ServerHello{
		Type:     gatewaywire.TypeServerHello,
		Proto:    gatewaywire.Proto,
		Nonce:    nonce,
		StunPort: s.stunPort,
	}); err != nil {
		s.logger.Debug("client hello write failed", "error", err)
		return
	}

	hello, ok := s.connectHello(ctx, conn)
	if !ok {
		return
	}

	// The rate limit is charged before the registration lookup so a stranger
	// cannot enumerate relay ids cheaply.
	if !s.connectLimiter.allow("connect:"+s.clientIP(r), s.now()) {
		reject(ctx, conn, gatewaywire.CodeRateLimited, "too many connection attempts")
		return
	}

	link := s.lookupRelay(hello.RelayID)
	if link == nil {
		reject(ctx, conn, gatewaywire.CodeUnknownRelay, "no relay is registered under that id")
		return
	}

	exceeded, notices := s.quota.exceeded(hello.RelayID, s.now())
	link.notify(notices)
	if exceeded {
		reject(ctx, conn, gatewaywire.CodeQuotaExceeded, "relay exceeded its monthly relayed-byte quota")
		return
	}

	client := &clientConn{
		link:       link,
		conn:       conn,
		out:        make(chan []byte, clientQueueDepth),
		writerDone: make(chan struct{}),
	}
	client.ctx, client.cancel = context.WithCancel(ctx)
	defer client.cancel()
	client.touch(s.now())

	switch err := link.attach(client); {
	case errors.Is(err, errAtCapacity):
		reject(ctx, conn, gatewaywire.CodeAtCapacity, "gateway is at its client capacity")
		return
	case errors.Is(err, errTooManyClients):
		reject(ctx, conn, gatewaywire.CodeTooManyClient, "relay already has its maximum number of clients")
		return
	case err != nil:
		reject(ctx, conn, gatewaywire.CodeUnknownRelay, "relay link closed")
		return
	}

	// Ready must reach the phone before the relay learns the connection id, so
	// no relayed frame can ever precede the ready message.
	if err := writeJSON(ctx, conn, gatewaywire.ReadyMessage{
		Type:  gatewaywire.TypeReady,
		Proto: gatewaywire.Proto,
	}); err != nil {
		s.logger.Debug("client ready write failed", "error", err)
		link.remove(client.connID)
		return
	}

	open, err := json.Marshal(gatewaywire.OpenPayload{Nonce: nonce, Proof: hello.Proof})
	if err != nil {
		s.logger.Error("open payload encoding failed", "error", err)
		link.remove(client.connID)
		reject(ctx, conn, gatewaywire.CodeInternal, "gateway could not announce the connection")
		return
	}
	if err := link.writeFrame(gatewaywire.OpOpen, client.connID, open); err != nil {
		s.logger.Debug("open frame write failed", "relay", shortID(link.relayID), "error", err)
		link.remove(client.connID)
		reject(ctx, conn, gatewaywire.CodeInternal, "gateway could not reach the relay")
		return
	}

	s.logger.Info("client paired", "relay", shortID(link.relayID), "conn", client.connID)
	client.run()
	s.logger.Info("client closed", "relay", shortID(link.relayID), "conn", client.connID)
}

// connectHello reads and validates the phone's answer to the challenge. The
// gateway checks only the shape: the proof is verified by the relay, which is
// the only party holding the rendezvous key.
func (s *Server) connectHello(ctx context.Context, conn *websocket.Conn) (gatewaywire.ConnectHello, bool) {
	var hello gatewaywire.ConnectHello
	data, err := readHello(ctx, conn, helloTimeout)
	if err != nil {
		s.logger.Debug("client hello read failed", "error", err)
		reject(ctx, conn, gatewaywire.CodeBadHello, "connect hello was not received")
		return hello, false
	}
	if err := json.Unmarshal(data, &hello); err != nil || hello.Type != gatewaywire.TypeConnect {
		reject(ctx, conn, gatewaywire.CodeBadHello, "connect hello is malformed")
		return hello, false
	}
	if hello.Proto != gatewaywire.Proto {
		reject(ctx, conn, gatewaywire.CodeBadHello, "unsupported gateway protocol version")
		return hello, false
	}
	if !gatewaywire.ValidRelayID(hello.RelayID) {
		reject(ctx, conn, gatewaywire.CodeBadHello, "relay id is malformed")
		return hello, false
	}
	if !validProof(hello.Proof) {
		reject(ctx, conn, gatewaywire.CodeBadHello, "proof is malformed")
		return hello, false
	}
	return hello, true
}

// run copies frames until the phone or the relay ends the connection.
func (c *clientConn) run() {
	go c.writePump()
	status, reason := c.readPump()

	// Whoever detaches the connection id owns telling the relay about it. If the
	// relay initiated the close it already detached, and no OpClose is echoed.
	if c.link.remove(c.connID) != nil {
		_ = c.link.writeFrame(gatewaywire.OpClose, c.connID, []byte(reason))
	}
	c.closeWith(status, reason)
	<-c.writerDone
}

// readPump forwards every phone frame to the relay as OpData.
func (c *clientConn) readPump() (websocket.StatusCode, string) {
	server := c.link.server
	for {
		messageType, data, err := c.conn.Read(c.ctx)
		if err != nil {
			return websocket.StatusNormalClosure, reasonClientClosed
		}
		if messageType != websocket.MessageBinary {
			return websocket.StatusUnsupportedData, reasonBinaryOnly
		}
		c.touch(server.now())
		c.frameBuf = gatewaywire.AppendFrame(c.frameBuf[:0], gatewaywire.OpData, c.connID, data)
		if err := c.link.writeRaw(c.frameBuf); err != nil {
			return websocket.StatusGoingAway, reasonRelayWriteFail
		}
		c.link.notify(server.quota.add(c.link.relayID, uint64(len(data)), server.now()))
	}
}

// writePump drains the outbound queue. When the queue is closed by finish it
// flushes what is left before closing the socket, so an orderly relay close
// never truncates data the relay already handed over.
func (c *clientConn) writePump() {
	defer close(c.writerDone)
	for {
		select {
		case <-c.ctx.Done():
			return
		case frame, ok := <-c.out:
			if !ok {
				c.sendMu.Lock()
				status, reason := c.drainStatus, c.drainReason
				c.sendMu.Unlock()
				c.closeWith(status, reason)
				return
			}
			c.sendMu.Lock()
			c.queuedBytes -= len(frame)
			c.sendMu.Unlock()

			ctx, cancel := context.WithTimeout(c.ctx, writeTimeout)
			err := c.conn.Write(ctx, websocket.MessageBinary, frame)
			cancel()
			if err != nil {
				c.cancel()
				return
			}
		}
	}
}

// deliver queues one relayed frame. It never blocks: a phone that cannot keep up
// is reported to the caller, which drops it instead of stalling the relay link.
func (c *clientConn) deliver(frame []byte) bool {
	c.sendMu.Lock()
	defer c.sendMu.Unlock()
	if c.outClosed {
		return false
	}
	if c.queuedBytes+len(frame) > maxQueuedBytes {
		return false
	}
	select {
	case c.out <- frame:
		c.queuedBytes += len(frame)
		return true
	default:
		return false
	}
}

// finish stops accepting frames and asks the writer to flush what is already
// queued and then close with this status. It never blocks, so the relay link and
// its maintenance goroutine can retire a phone connection without waiting for a
// close handshake. Every caller detaches the connection id first, so no delivery
// can race the channel close.
func (c *clientConn) finish(status websocket.StatusCode, reason string) {
	c.sendMu.Lock()
	defer c.sendMu.Unlock()
	if c.outClosed {
		return
	}
	c.outClosed = true
	c.drainStatus = status
	c.drainReason = reason
	close(c.out)
}

// closeWith closes the phone socket once with a proper close frame, dropping
// anything still queued. The frame is written before the context is cancelled,
// because cancelling it would tear the socket down and leave the peer with an
// unexplained EOF. Only the connection's own goroutines call this.
func (c *clientConn) closeWith(status websocket.StatusCode, reason string) {
	c.closeOnce.Do(func() {
		c.sendMu.Lock()
		c.outClosed = true
		c.sendMu.Unlock()
		_ = c.conn.Close(status, reason)
		c.cancel()
	})
}

// kill drops the socket without a close handshake. It is reserved for a phone
// that stopped reading, where waiting for a graceful close would stall the relay
// link that is trying to get rid of it.
func (c *clientConn) kill() {
	c.closeOnce.Do(func() {
		c.sendMu.Lock()
		c.outClosed = true
		c.sendMu.Unlock()
		c.cancel()
		c.conn.CloseNow()
	})
}

func (c *clientConn) touch(now time.Time) {
	c.lastActivity.Store(now.UnixNano())
}
