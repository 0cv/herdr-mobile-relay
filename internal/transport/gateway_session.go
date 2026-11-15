package transport

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/0cv/herdr-mobile-relay/internal/framing"
	"github.com/0cv/herdr-mobile-relay/internal/gatewaywire"
	"github.com/coder/websocket"
)

// gatewaySession is one registered link. Every frame leaving the relay passes
// through a single writer goroutine, so per-connection sends can never
// interleave halves of a multiplexed frame.
type gatewaySession struct {
	client     *GatewayClient
	conn       *websocket.Conn
	ctx        context.Context
	cancel     context.CancelFunc
	writes     chan []byte
	done       chan struct{}
	writerDone chan struct{}
	stopOnce   sync.Once
	serveWG    sync.WaitGroup

	failOnce sync.Once
	failErr  error
}

// stop ends the session: queued frames are abandoned and every reader unblocks.
func (s *gatewaySession) stop() {
	s.stopOnce.Do(func() {
		close(s.done)
		s.cancel()
	})
}

// fail records the first fault and tears the session down.
func (s *gatewaySession) fail(err error) {
	s.failOnce.Do(func() { s.failErr = err })
	s.stop()
}

// failure reports the recorded fault. It is only safe once the read and write
// pumps have both finished.
func (s *gatewaySession) failure() error { return s.failErr }

// enqueue hands one complete multiplexed frame to the writer. It never blocks:
// a saturated queue means the link cannot keep up, and the caller drops its own
// logical connection rather than stalling the others.
func (s *gatewaySession) enqueue(frame []byte) error {
	select {
	case <-s.done:
		return ErrFrameConnClosed
	default:
	}
	select {
	case s.writes <- frame:
		return nil
	case <-s.done:
		return ErrFrameConnClosed
	default:
		return errGatewayWriteQueueFull
	}
}

func (s *gatewaySession) writeLoop() {
	defer close(s.writerDone)
	for {
		select {
		case <-s.done:
			return
		case frame := <-s.writes:
			ctx, cancel := context.WithTimeout(s.ctx, frameWriteTimeout)
			err := s.conn.Write(ctx, websocket.MessageBinary, frame)
			cancel()
			if err != nil {
				s.fail(fmt.Errorf("write gateway frame: %w", err))
				return
			}
		}
	}
}

// sweepLoop abandons logical connections whose inbound frame assembly stopped
// making progress, so a phone cannot pin a partially received frame on the
// relay by going quiet mid-sequence. It ends with the session.
func (s *gatewaySession) sweepLoop() {
	defer s.serveWG.Done()
	ticker := time.NewTicker(gatewayStallSweep)
	defer ticker.Stop()
	for {
		select {
		case <-s.done:
			return
		case now := <-ticker.C:
			s.client.sweepStalled(now)
		}
	}
}

// healthLoop verifies the same public route new phones use. The maintained
// /relay WebSocket can remain alive through Caddy while Docker DNS for new
// requests is broken; link ping/pong cannot detect that split failure.
func (s *gatewaySession) healthLoop(base string) {
	defer s.serveWG.Done()
	if s.client.healthInterval <= 0 {
		return
	}
	ticker := time.NewTicker(s.client.healthInterval)
	defer ticker.Stop()
	failures := 0
	for {
		select {
		case <-s.done:
			return
		case <-ticker.C:
			ctx, cancel := context.WithTimeout(s.ctx, s.client.probeTimeout)
			_, err := s.client.probe(ctx, base)
			cancel()
			if err == nil {
				failures = 0
				continue
			}
			failures++
			if failures < gatewayHealthFailureLimit {
				s.client.logger.Debug("gateway public health probe failed",
					"url", base, "error", err)
				continue
			}
			s.fail(fmt.Errorf("gateway public path failed %d consecutive health probes: %w",
				failures, err))
			return
		}
	}
}

// sweepStalled closes every logical connection whose inbound assembly stopped
// making progress before now.
func (c *GatewayClient) sweepStalled(now time.Time) {
	for connID, conn := range c.liveConns() {
		if !conn.stalled(now) {
			continue
		}
		c.logger.Debug("gateway connection evicted for a stalled frame assembly",
			"conn_id", connID)
		conn.pipe.Close(CloseGoingAway, gatewayReasonSlow)
	}
}

func (s *gatewaySession) readLoop() error {
	for {
		messageType, data, err := s.conn.Read(s.ctx)
		if err != nil {
			// After framing starts the gateway has no TEXT channel left for an
			// ErrorMessage, so a policy-violation close carries the refusal
			// (for example the relay id was claimed by a newer link).
			var closed websocket.CloseError
			if errors.As(err, &closed) && closed.Code == websocket.StatusPolicyViolation {
				return &gatewayRejectedError{code: gatewaywire.CodeRelayBusy, message: closed.Reason}
			}
			return fmt.Errorf("read gateway frame: %w", err)
		}
		if messageType != websocket.MessageBinary {
			return errors.New("gateway sent a non-binary frame on a registered link")
		}
		op, connID, payload, err := gatewaywire.DecodeFrame(data)
		if err != nil {
			return fmt.Errorf("decode gateway frame: %w", err)
		}
		switch op {
		case gatewaywire.OpData:
			s.handleData(connID, payload)
		case gatewaywire.OpOpen:
			s.handleOpen(connID, payload)
		case gatewaywire.OpClose:
			s.handleClose(connID)
		case gatewaywire.OpPing:
			if err := s.enqueue(gatewaywire.EncodeFrame(gatewaywire.OpPong, connID, payload)); err != nil {
				return fmt.Errorf("answer gateway ping: %w", err)
			}
		case gatewaywire.OpNotice:
			s.handleNotice(payload)
		}
	}
}

// handleOpen authenticates a forwarded phone connection and, on success, hands
// it to the hub as an ordinary client. The gateway cannot perform this check:
// only the relay and the paired phone know the rendezvous key.
func (s *gatewaySession) handleOpen(connID uint32, payload []byte) {
	var open gatewaywire.OpenPayload
	if err := json.Unmarshal(payload, &open); err != nil {
		s.reject(connID, gatewayReasonUnauthorized)
		s.client.logger.Debug("gateway open payload rejected", "conn_id", connID)
		return
	}
	nonce, nonceErr := base64.RawURLEncoding.DecodeString(open.Nonce)
	proof, proofErr := base64.RawURLEncoding.DecodeString(open.Proof)
	if nonceErr != nil || proofErr != nil ||
		!gatewaywire.VerifyConnectProof(s.client.rendezvousKey, s.client.relayID, nonce, proof) {
		s.reject(connID, gatewayReasonUnauthorized)
		s.client.logger.Debug("gateway connect proof rejected", "conn_id", connID)
		return
	}

	var conn *gatewayConn
	// The ctx is unused: enqueue never blocks, and the shared writer bounds the
	// actual socket write with frameWriteTimeout.
	send := func(_ context.Context, frame []byte) error {
		// Reachable only for a frame the hub built above the logical cap every
		// transport shares; the gateway's own per-frame cap is what the chunks
		// below stay under.
		if len(frame) > framing.MaxLogicalBytes {
			return fmt.Errorf("frame of %d bytes exceeds the logical frame limit of %d",
				len(frame), framing.MaxLogicalBytes)
		}
		// The chunks of one logical frame must reach the single writer
		// goroutine contiguously and in order: the peer reassembles START..END
		// per connection and rejects anything else.
		conn.sendMu.Lock()
		defer conn.sendMu.Unlock()
		conn.chunks = framing.Chunk(conn.chunks[:0], frame, framing.GatewayChunkSize)
		for _, part := range conn.chunks {
			// EncodeFrame copies the chunk, so the shared chunk buffer is free
			// to be reused by the next send once every part is queued.
			err := s.enqueue(gatewaywire.EncodeFrame(gatewaywire.OpData, connID, part))
			if err == nil {
				continue
			}
			if errors.Is(err, errGatewayWriteQueueFull) {
				// The queue is full, so OpClose cannot be sent either; drop the
				// connection locally and let the gateway observe the eviction.
				// Dropping it also keeps the truncated chunk sequence from ever
				// being mistaken for a complete frame.
				s.client.removeConn(connID, conn)
				conn.pipe.Shutdown()
			}
			return err
		}
		return nil
	}
	closeConn := func(status CloseStatus, reason string) {
		s.client.removeConn(connID, conn)
		if status == CloseUnauthorized {
			reason = DeviceUnauthorizedReason
		}
		_ = s.enqueue(gatewaywire.EncodeFrame(gatewaywire.OpClose, connID, closeReasonBytes(reason)))
	}
	conn = &gatewayConn{recv: framing.NewReassembler(framing.GatewayChunkSize)}
	conn.pipe = newPipeConn(CodecBinary, TransportGateway, gatewayConnQueue, send, closeConn)

	if !s.client.addConn(connID, conn) {
		s.reject(connID, gatewayReasonBusy)
		s.client.logger.Warn("gateway connection refused at client cap",
			"conn_id", connID, "max_clients", s.client.maxClients)
		return
	}
	s.serveWG.Add(1)
	go func() {
		defer s.serveWG.Done()
		s.client.hub.Serve(s.ctx, conn.pipe)
		s.client.removeConn(connID, conn)
	}()
}

// handleData reassembles one inbound chunk. Only a completed logical frame
// reaches the hub.
func (s *gatewaySession) handleData(connID uint32, payload []byte) {
	conn := s.client.lookupConn(connID)
	if conn == nil {
		s.reject(connID, gatewayReasonUnknownConn)
		return
	}
	frame, err := conn.push(payload)
	if err != nil {
		// A framing violation is one phone's fault: every other connection on
		// this link keeps running. Nothing derived from the chunk is logged.
		s.client.logger.Debug("gateway chunk framing rejected", "conn_id", connID)
		conn.pipe.Close(CloseGoingAway, gatewayReasonFraming)
		return
	}
	if frame == nil {
		return
	}
	// Push already returned a frame that owns its bytes, so the hub needs no
	// copy of the reused read buffer.
	if !conn.pipe.Deliver(frame) {
		s.client.logger.Debug("gateway connection evicted for falling behind", "conn_id", connID)
		conn.pipe.Close(CloseGoingAway, gatewayReasonSlow)
	}
}

func (s *gatewaySession) handleClose(connID uint32) {
	if conn := s.client.takeConn(connID); conn != nil {
		conn.pipe.Shutdown()
	}
}

func (s *gatewaySession) handleNotice(payload []byte) {
	var notice gatewaywire.NoticePayload
	if err := json.Unmarshal(payload, &notice); err != nil {
		s.client.logger.Debug("gateway notice rejected")
		return
	}
	s.client.recordNotice(notice.Message)
	s.client.logger.Warn("gateway notice", "kind", notice.Kind, "message", notice.Message)
}

// reject closes a connection id the relay declined to serve.
func (s *gatewaySession) reject(connID uint32, reason string) {
	_ = s.enqueue(gatewaywire.EncodeFrame(gatewaywire.OpClose, connID, closeReasonBytes(reason)))
}

// closeReasonBytes trims a reason to the wire limit without splitting a rune.
func closeReasonBytes(reason string) []byte {
	if reason == "" {
		return nil
	}
	if len(reason) > gatewaywire.MaxCloseReason {
		trimmed := reason[:gatewaywire.MaxCloseReason]
		for len(trimmed) > 0 && !utf8.ValidString(trimmed) {
			trimmed = trimmed[:len(trimmed)-1]
		}
		reason = trimmed
	}
	return []byte(reason)
}
