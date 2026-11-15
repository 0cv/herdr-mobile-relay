package transport

import (
	"context"
	"sync"
)

// pipeConn is an in-memory FrameConn for tests. Writes go to send; reads
// come from a bounded queue that tests fill with Deliver.
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

func (c *pipeConn) Codec() FrameCodec { return c.codec }

func (c *pipeConn) TransportName() string { return c.name }
