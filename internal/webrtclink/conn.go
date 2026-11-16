package webrtclink

import (
	"context"
	"errors"
	"fmt"

	"github.com/pion/webrtc/v4"

	"github.com/0cv/herdr-mobile-relay/internal/framing"
	"github.com/0cv/herdr-mobile-relay/internal/transport"
)

const (
	// pauseBufferedAmount stops the sender until the DataChannel drains back to
	// resumeBufferedAmount, reported through OnBufferedAmountLow.
	pauseBufferedAmount  = 4 << 20
	resumeBufferedAmount = 1 << 20
)

// dataChannelConn binds one open herdr-dc-v1 DataChannel to a transport.ChunkConn
// for the relay hub. It paces outbound chunks by the DataChannel's buffered
// amount and feeds inbound messages to the connection. Any framing violation
// ends the whole session through onFatal.
type dataChannelConn struct {
	dc       *webrtc.DataChannel
	onFatal  func(reason string)
	conn     *transport.ChunkConn
	lowWater chan struct{}
}

func newDataChannelConn(dc *webrtc.DataChannel, onFatal func(reason string)) *dataChannelConn {
	c := &dataChannelConn{
		dc:       dc,
		onFatal:  onFatal,
		lowWater: make(chan struct{}, 1),
	}
	c.conn = transport.NewChunkConn(transport.ChunkConnOptions{
		Name:      transport.TransportWebRTC,
		ChunkSize: framing.WebRTCChunkSize,
		SendPart:  c.sendPart,
		OnClose:   c.handleClose,
	})
	return c
}

// handleMessage is the DataChannel OnMessage handler. It never logs or returns
// frame bytes; a violation is reported as a static reason.
func (c *dataChannelConn) handleMessage(msg webrtc.DataChannelMessage) {
	if msg.IsString {
		c.fail("text datachannel message")
		return
	}
	switch err := c.conn.Receive(msg.Data); {
	case err == nil:
	case errors.Is(err, transport.ErrInboundQueueFull):
		c.fail("inbound frame queue overflow")
	default:
		c.fail("invalid chunk framing")
	}
}

// signalLowWater wakes a writer paused by backpressure.
func (c *dataChannelConn) signalLowWater() {
	select {
	case c.lowWater <- struct{}{}:
	default:
	}
}

// fail ends the session behind this connection. It is safe to call from pion
// callbacks: the teardown runs on its own goroutine.
func (c *dataChannelConn) fail(reason string) {
	c.conn.Shutdown()
	go c.onFatal(reason)
}

func (c *dataChannelConn) sendPart(ctx context.Context, part []byte) error {
	if err := c.awaitCapacity(ctx); err != nil {
		return err
	}
	if err := c.dc.Send(part); err != nil {
		return fmt.Errorf("webrtclink: datachannel send: %w", err)
	}
	return nil
}

// awaitCapacity blocks while the DataChannel send buffer is above the pause
// threshold. The low-water signal is buffered, so a drain that happens between
// the check and the wait cannot be missed.
func (c *dataChannelConn) awaitCapacity(ctx context.Context) error {
	for {
		select {
		case <-c.conn.Done():
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
		case <-c.conn.Done():
			return transport.ErrFrameConnClosed
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

// handleClose ends the session behind the hub's connection. WebRTC has no close
// frame, so status only shapes the logged reason.
func (c *dataChannelConn) handleClose(status transport.CloseStatus, reason string) {
	if reason == "" {
		reason = "hub closed connection"
		if status == transport.CloseGoingAway {
			reason = "relay going away"
		}
	}
	c.onFatal(reason)
}
