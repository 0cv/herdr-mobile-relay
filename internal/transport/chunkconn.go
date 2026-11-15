package transport

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/0cv/herdr-mobile-relay/internal/framing"
)

// inboundQueueDepth bounds how many logical frames may wait for the hub before
// the peer counts as unreadably slow, matching the relay's ingress bound.
const inboundQueueDepth = 32

// ErrInboundQueueFull reports that the hub stopped reading and the queue of
// complete inbound frames is full.
var ErrInboundQueueFull = errors.New("inbound frame queue full")

// ChunkConnOptions describes the path a ChunkConn rides on.
type ChunkConnOptions struct {
	// Name is the transport name reported by TransportName.
	Name string
	// ChunkSize is the largest chunk, header included, in either direction.
	ChunkSize int
	// SendPart sends one outbound chunk. The slice is reused after it returns.
	SendPart func(ctx context.Context, part []byte) error
	// OnClose runs after Close ends the connection. It never runs for Shutdown
	// and may call Shutdown or Close on the same connection.
	OnClose func(status CloseStatus, reason string)
}

// ChunkConn is a FrameConn that carries each logical frame as a sequence of
// bounded chunks. The path feeds inbound chunks to Receive and supplies
// SendPart for outbound ones.
type ChunkConn struct {
	opts ChunkConnOptions

	inbound   chan []byte
	closed    chan struct{}
	closeOnce sync.Once

	// sendMu is held across a whole logical frame so concurrent writers never
	// interleave their chunk sequences.
	sendMu sync.Mutex
	chunks [][]byte

	recvMu sync.Mutex
	recv   *framing.Reassembler
}

var _ FrameConn = (*ChunkConn)(nil)

func NewChunkConn(opts ChunkConnOptions) *ChunkConn {
	return &ChunkConn{
		opts:    opts,
		inbound: make(chan []byte, inboundQueueDepth),
		closed:  make(chan struct{}),
		recv:    framing.NewReassembler(opts.ChunkSize),
	}
}

// Receive feeds one inbound chunk. It returns a framing error for a violation,
// after dropping the partial frame, and ErrInboundQueueFull when the hub is too
// far behind. A closed connection drops the chunk.
func (c *ChunkConn) Receive(part []byte) error {
	select {
	case <-c.closed:
		return nil
	default:
	}

	c.recvMu.Lock()
	frame, err := c.recv.Push(part)
	if err != nil {
		c.recv.Reset()
	}
	c.recvMu.Unlock()
	if err != nil {
		return err
	}
	if frame == nil {
		return nil
	}

	select {
	case c.inbound <- frame:
		return nil
	case <-c.closed:
		return nil
	default:
		return ErrInboundQueueFull
	}
}

// Stalled reports whether a partially received frame stopped making progress.
func (c *ChunkConn) Stalled(now time.Time) bool {
	c.recvMu.Lock()
	defer c.recvMu.Unlock()
	return c.recv.Expired(now)
}

// Done is closed once the connection has ended by Close or Shutdown.
func (c *ChunkConn) Done() <-chan struct{} { return c.closed }

func (c *ChunkConn) ReadFrame(ctx context.Context) ([]byte, error) {
	select {
	case frame := <-c.inbound:
		return frame, nil
	case <-c.closed:
		return nil, ErrFrameConnClosed
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func (c *ChunkConn) WriteFrame(ctx context.Context, frame []byte) error {
	if len(frame) > framing.MaxLogicalBytes {
		return framing.ErrFrameTooLarge
	}

	c.sendMu.Lock()
	defer c.sendMu.Unlock()

	select {
	case <-c.closed:
		return ErrFrameConnClosed
	default:
	}

	ctx, cancel := context.WithTimeout(ctx, frameWriteTimeout)
	defer cancel()

	c.chunks = framing.Chunk(c.chunks[:0], frame, c.opts.ChunkSize)
	for _, part := range c.chunks {
		if err := c.opts.SendPart(ctx, part); err != nil {
			return err
		}
	}
	return nil
}

// Close ends the connection and then runs OnClose, unless the connection
// already ended. OnClose runs outside the close-once so it may call Shutdown.
func (c *ChunkConn) Close(status CloseStatus, reason string) {
	if !c.end() || c.opts.OnClose == nil {
		return
	}
	c.opts.OnClose(status, reason)
}

func (c *ChunkConn) CloseNow() { c.Close(CloseNormal, "") }

// Shutdown ends the connection without running OnClose, for use when the link
// carrying it already failed.
func (c *ChunkConn) Shutdown() { c.end() }

// end closes the connection and reports whether this call did it.
func (c *ChunkConn) end() bool {
	first := false
	c.closeOnce.Do(func() {
		first = true
		close(c.closed)
	})
	return first
}

func (c *ChunkConn) Codec() FrameCodec { return CodecBinary }

func (c *ChunkConn) TransportName() string { return c.opts.Name }
