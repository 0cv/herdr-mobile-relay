package webrtclink

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/pion/webrtc/v4"

	"github.com/0cv/herdr-mobile-relay/internal/framing"
	"github.com/0cv/herdr-mobile-relay/internal/transport"
)

const (
	// readQueueDepth bounds how many logical frames may wait for the hub before
	// the peer counts as unreadably slow, matching the relay's ingress bound.
	readQueueDepth = 32

	// pauseBufferedAmount stops the sender until the DataChannel drains back to
	// resumeBufferedAmount, reported through OnBufferedAmountLow.
	pauseBufferedAmount  = 4 << 20
	resumeBufferedAmount = 1 << 20

	// frameWriteTimeout bounds one logical write so a peer that never drains is
	// evicted on the same schedule as a stalled WebSocket client.
	frameWriteTimeout = 5 * time.Second
)

// dataChannelConn presents one open herdr-dc-v1 DataChannel to the relay hub as
// a transport.FrameConn. Inbound chunks are reassembled into logical frames on
// the pion read goroutine and queued for the hub; outbound frames are chunked
// and paced by the DataChannel's buffered amount. Any framing violation ends
// the whole session through onFatal.
type dataChannelConn struct {
	dc      *webrtc.DataChannel
	onFatal func(reason string)

	frames    chan []byte
	closed    chan struct{}
	closeOnce sync.Once

	lowWater chan struct{}

	assembleMu sync.Mutex
	assembler  *framing.Reassembler

	writeMu sync.Mutex
	chunks  [][]byte
}

func newDataChannelConn(dc *webrtc.DataChannel, onFatal func(reason string)) *dataChannelConn {
	return &dataChannelConn{
		dc:        dc,
		onFatal:   onFatal,
		frames:    make(chan []byte, readQueueDepth),
		closed:    make(chan struct{}),
		lowWater:  make(chan struct{}, 1),
		assembler: framing.NewReassembler(framing.WebRTCChunkSize),
	}
}

// handleMessage is the DataChannel OnMessage handler. It never logs or returns
// frame bytes; a violation is reported as a static reason.
func (c *dataChannelConn) handleMessage(msg webrtc.DataChannelMessage) {
	if msg.IsString {
		c.fail("text datachannel message")
		return
	}

	c.assembleMu.Lock()
	frame, err := c.assembler.Push(msg.Data)
	c.assembleMu.Unlock()
	if err != nil {
		c.fail("invalid chunk framing")
		return
	}
	if frame == nil {
		return
	}

	select {
	case <-c.closed:
		return
	default:
	}
	select {
	case c.frames <- frame:
	case <-c.closed:
	default:
		c.fail("inbound frame queue overflow")
	}
}

// signalLowWater wakes a writer paused by backpressure.
func (c *dataChannelConn) signalLowWater() {
	select {
	case c.lowWater <- struct{}{}:
	default:
	}
}

// stalled reports whether an incomplete inbound frame timed out.
func (c *dataChannelConn) stalled(now time.Time) bool {
	c.assembleMu.Lock()
	defer c.assembleMu.Unlock()
	return c.assembler.Expired(now)
}

// fail ends the session behind this connection. It is safe to call from pion
// callbacks: the teardown runs on its own goroutine.
func (c *dataChannelConn) fail(reason string) {
	c.shutdown()
	go c.onFatal(reason)
}

func (c *dataChannelConn) ReadFrame(ctx context.Context) ([]byte, error) {
	select {
	case frame := <-c.frames:
		return frame, nil
	case <-c.closed:
		return nil, transport.ErrFrameConnClosed
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func (c *dataChannelConn) WriteFrame(ctx context.Context, frame []byte) error {
	if len(frame) > framing.MaxLogicalBytes {
		return framing.ErrFrameTooLarge
	}

	c.writeMu.Lock()
	defer c.writeMu.Unlock()

	select {
	case <-c.closed:
		return transport.ErrFrameConnClosed
	default:
	}

	ctx, cancel := context.WithTimeout(ctx, frameWriteTimeout)
	defer cancel()

	c.chunks = framing.Chunk(c.chunks[:0], frame, framing.WebRTCChunkSize)
	for _, part := range c.chunks {
		if err := c.awaitCapacity(ctx); err != nil {
			return err
		}
		if err := c.dc.Send(part); err != nil {
			return fmt.Errorf("webrtclink: datachannel send: %w", err)
		}
	}
	return nil
}

// awaitCapacity blocks while the DataChannel send buffer is above the pause
// threshold. The low-water signal is buffered, so a drain that happens between
// the check and the wait cannot be missed.
func (c *dataChannelConn) awaitCapacity(ctx context.Context) error {
	for {
		select {
		case <-c.closed:
			return transport.ErrFrameConnClosed
		case <-ctx.Done():
			return ctx.Err()
		default:
		}
		if c.dc.BufferedAmount() < pauseBufferedAmount {
			return nil
		}
		select {
		case <-c.lowWater:
		case <-c.closed:
			return transport.ErrFrameConnClosed
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

// Close ends the logical connection and, with it, the peer connection carrying
// it. WebRTC has no close frame, so status only shapes the logged reason.
func (c *dataChannelConn) Close(status transport.CloseStatus, reason string) {
	if reason == "" {
		reason = "hub closed connection"
		if status == transport.CloseGoingAway {
			reason = "relay going away"
		}
	}
	c.shutdown()
	c.onFatal(reason)
}

func (c *dataChannelConn) CloseNow() { c.Close(transport.CloseNormal, "hub dropped connection") }

// shutdown stops reads and writes without touching the peer connection, for use
// when the session teardown already owns that.
func (c *dataChannelConn) shutdown() {
	c.closeOnce.Do(func() { close(c.closed) })
}

func (c *dataChannelConn) Codec() transport.FrameCodec { return transport.CodecBinary }

func (c *dataChannelConn) TransportName() string { return transport.TransportWebRTC }
