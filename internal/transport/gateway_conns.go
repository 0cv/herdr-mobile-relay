package transport

import (
	"maps"
	"sync"
	"time"

	"github.com/0cv/herdr-mobile-relay/internal/framing"
)

// gatewayConn is one demultiplexed phone connection: the hub-facing pipe plus
// the chunk framing state of both directions. A logical frame travels as a
// sequence of bounded OpData chunks, so outbound sends serialize on sendMu —
// two goroutines writing the same logical connection must never interleave
// their chunk sequences on the shared writer queue. Chunks of different
// connection ids may interleave freely: reassembly is per connection.
type gatewayConn struct {
	pipe *pipeConn

	sendMu sync.Mutex
	chunks [][]byte

	recvMu sync.Mutex
	recv   *framing.Reassembler
}

// push feeds one inbound chunk to the reassembler and returns the completed
// logical frame, or nil while it is still incomplete. A violation drops the
// partial assembly immediately: the connection is about to be closed and the
// buffer can hold megabytes.
func (c *gatewayConn) push(part []byte) ([]byte, error) {
	c.recvMu.Lock()
	defer c.recvMu.Unlock()
	frame, err := c.recv.Push(part)
	if err != nil {
		c.recv.Reset()
		return nil, err
	}
	return frame, nil
}

// stalled reports whether a partially received frame stopped making progress.
func (c *gatewayConn) stalled(now time.Time) bool {
	c.recvMu.Lock()
	defer c.recvMu.Unlock()
	return c.recv.Expired(now)
}

// addConn registers a demultiplexed connection. It reports false when the
// local client cap is reached. A repeated connection id means the gateway has
// already discarded our entry, so the stale one is torn down.
func (c *GatewayClient) addConn(connID uint32, conn *gatewayConn) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if stale, ok := c.conns[connID]; ok {
		delete(c.conns, connID)
		stale.pipe.Shutdown()
	}
	if len(c.conns) >= c.maxClients {
		return false
	}
	c.conns[connID] = conn
	return true
}

func (c *GatewayClient) lookupConn(connID uint32) *gatewayConn {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.conns[connID]
}

// liveConns snapshots the registry so a sweep can act on connections without
// holding the lock that every send and delivery contends for.
func (c *GatewayClient) liveConns() map[uint32]*gatewayConn {
	c.mu.Lock()
	defer c.mu.Unlock()
	return maps.Clone(c.conns)
}

// removeConn drops the entry only when it still refers to conn, so a late
// teardown from a previous link never evicts a live connection.
func (c *GatewayClient) removeConn(connID uint32, conn *gatewayConn) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if current, ok := c.conns[connID]; ok && current == conn {
		delete(c.conns, connID)
	}
}

func (c *GatewayClient) takeConn(connID uint32) *gatewayConn {
	c.mu.Lock()
	defer c.mu.Unlock()
	conn, ok := c.conns[connID]
	if !ok {
		return nil
	}
	delete(c.conns, connID)
	return conn
}

// evictAll ends every multiplexed connection without notifying the peer: the
// link that carried them is already gone.
func (c *GatewayClient) evictAll() {
	c.mu.Lock()
	conns := c.conns
	c.conns = make(map[uint32]*gatewayConn)
	c.mu.Unlock()
	for _, conn := range conns {
		conn.pipe.Shutdown()
	}
}
