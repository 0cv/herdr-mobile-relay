package transport

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/coder/websocket"
)

// ErrFrameConnClosed reports an orderly close initiated by the peer. Callers
// distinguish it from unexpected read failures that deserve a log line.
var ErrFrameConnClosed = errors.New("frame connection closed")

// CloseStatus selects the close semantics a transport reports to its peer.
type CloseStatus int

const (
	// CloseNormal ends a connection that finished for ordinary reasons.
	CloseNormal CloseStatus = iota
	// CloseGoingAway ends a connection because the relay is shutting down.
	CloseGoingAway
	// CloseUnauthorized ends a connection whose device authentication this
	// relay refuses. Retrying with the same credential cannot succeed, so the
	// phone stops reconnecting and asks to be paired again.
	CloseUnauthorized
)

// UnauthorizedCloseCode is the WebSocket close code for CloseUnauthorized. It
// sits in the application range so no proxy rewrites it.
const UnauthorizedCloseCode = 4401

// DeviceUnauthorizedReason carries CloseUnauthorized across gateway
// multiplexing, which has no native close code.
const DeviceUnauthorizedReason = "device_unauthorized"

// Transport names reported by FrameConn implementations. They are surfaced in
// logs, metrics, and the signaling gate that only accepts WebRTC negotiation
// from a relayed connection.
const (
	TransportWebSocket = "websocket"
	TransportGateway   = "gateway"
	TransportWebRTC    = "webrtc"
)

// FrameConn is a logical-frame duplex connection. Exactly one complete logical
// frame is returned per ReadFrame and consumed per WriteFrame; chunking,
// reassembly, and multiplexing belong to the implementation. The hub, the
// admission ordering, the send buffers, slow-client eviction, metrics, and the
// encrypted handshake are all written against this interface so that
// WebSocket, gateway-relayed, and WebRTC connections share one code path.
type FrameConn interface {
	// ReadFrame blocks until one logical frame arrives. It returns
	// ErrFrameConnClosed when the peer closed the connection cleanly.
	ReadFrame(ctx context.Context) ([]byte, error)
	// WriteFrame sends one logical frame.
	WriteFrame(ctx context.Context, frame []byte) error
	// Close performs a best-effort graceful close and is idempotent.
	Close(status CloseStatus, reason string)
	// CloseNow drops the connection without a closing handshake.
	CloseNow()
	// Codec reports the encrypted-frame encoding this transport carries.
	Codec() FrameCodec
	// TransportName identifies the path for logs, metrics, and policy.
	TransportName() string
}

// webSocketConn adapts coder/websocket to FrameConn. Encrypted connections
// require text frames, preserving the pre-existing wire contract with the PWA.
type webSocketConn struct {
	conn        *websocket.Conn
	requireText bool
}

func newWebSocketConn(conn *websocket.Conn, requireText bool) *webSocketConn {
	return &webSocketConn{conn: conn, requireText: requireText}
}

func (c *webSocketConn) ReadFrame(ctx context.Context) ([]byte, error) {
	messageType, data, err := c.conn.Read(ctx)
	if err != nil {
		if websocket.CloseStatus(err) != -1 {
			return nil, ErrFrameConnClosed
		}
		return nil, err
	}
	if c.requireText && messageType != websocket.MessageText {
		return nil, errors.New("encrypted websocket frames must be text")
	}
	return data, nil
}

func (c *webSocketConn) WriteFrame(ctx context.Context, frame []byte) error {
	return c.conn.Write(ctx, websocket.MessageText, frame)
}

func (c *webSocketConn) Close(status CloseStatus, reason string) {
	code := websocket.StatusNormalClosure
	switch status {
	case CloseGoingAway:
		code = websocket.StatusGoingAway
	case CloseUnauthorized:
		code = websocket.StatusCode(UnauthorizedCloseCode)
	}
	ctx, cancel := context.WithTimeout(context.Background(), wsCloseTimeout)
	defer cancel()
	closed := make(chan struct{})
	go func() {
		_ = c.conn.Close(code, reason)
		close(closed)
	}()
	select {
	case <-closed:
	case <-ctx.Done():
		c.conn.CloseNow()
	}
}

func (c *webSocketConn) CloseNow() { c.conn.CloseNow() }

func (c *webSocketConn) Codec() FrameCodec { return CodecJSON }

func (c *webSocketConn) TransportName() string { return TransportWebSocket }

// pipeConn is an in-process FrameConn pair used by the gateway and WebRTC
// adapters to hand a demultiplexed logical stream to the hub. Writes are
// delivered to the owning multiplexer through send; reads come from a bounded
// queue the multiplexer fills.
type pipeConn struct {
	codec     FrameCodec
	name      string
	send      func(ctx context.Context, frame []byte) error
	closeFunc func(status CloseStatus, reason string)
	inbound   chan []byte
	closed    chan struct{}
	closeOnce sync.Once
}

// newPipeConn builds a FrameConn whose reads are fed by Deliver and whose
// writes are forwarded to send. queue bounds how many logical frames may wait
// for the hub before the peer is considered unreadably slow.
func newPipeConn(
	codec FrameCodec,
	name string,
	queue int,
	send func(ctx context.Context, frame []byte) error,
	closeFunc func(status CloseStatus, reason string),
) *pipeConn {
	return &pipeConn{
		codec:     codec,
		name:      name,
		send:      send,
		closeFunc: closeFunc,
		inbound:   make(chan []byte, queue),
		closed:    make(chan struct{}),
	}
}

// Deliver queues one inbound logical frame. It reports false when the
// connection is closed or the reader is too far behind.
func (c *pipeConn) Deliver(frame []byte) bool {
	select {
	case <-c.closed:
		return false
	default:
	}
	select {
	case c.inbound <- frame:
		return true
	case <-c.closed:
		return false
	default:
		return false
	}
}

func (c *pipeConn) ReadFrame(ctx context.Context) ([]byte, error) {
	select {
	case frame := <-c.inbound:
		return frame, nil
	case <-c.closed:
		return nil, ErrFrameConnClosed
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func (c *pipeConn) WriteFrame(ctx context.Context, frame []byte) error {
	select {
	case <-c.closed:
		return ErrFrameConnClosed
	default:
	}
	return c.send(ctx, frame)
}

func (c *pipeConn) Close(status CloseStatus, reason string) {
	c.closeOnce.Do(func() {
		close(c.closed)
		if c.closeFunc != nil {
			c.closeFunc(status, reason)
		}
	})
}

func (c *pipeConn) CloseNow() { c.Close(CloseNormal, "") }

// Shutdown ends the logical connection without notifying the peer, for use
// when the underlying multiplexed link already failed.
func (c *pipeConn) Shutdown() {
	c.closeOnce.Do(func() { close(c.closed) })
}

func (c *pipeConn) Codec() FrameCodec { return c.codec }

func (c *pipeConn) TransportName() string { return c.name }

// frameWriteTimeout bounds a single logical write on multiplexed transports so
// one stalled peer cannot pin a multiplexer goroutine.
const frameWriteTimeout = 5 * time.Second
