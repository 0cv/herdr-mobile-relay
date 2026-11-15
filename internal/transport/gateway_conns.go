package transport

import "maps"

// addConn registers a demultiplexed connection. It reports false when the
// local client cap is reached. A repeated connection id means the gateway has
// already discarded our entry, so the stale one is torn down.
func (c *GatewayClient) addConn(connID uint32, conn *ChunkConn) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if stale, ok := c.conns[connID]; ok {
		delete(c.conns, connID)
		stale.Shutdown()
	}
	if len(c.conns) >= c.maxClients {
		return false
	}
	c.conns[connID] = conn
	return true
}

func (c *GatewayClient) lookupConn(connID uint32) *ChunkConn {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.conns[connID]
}

// liveConns snapshots the registry so a sweep can act on connections without
// holding the lock that every send and delivery contends for.
func (c *GatewayClient) liveConns() map[uint32]*ChunkConn {
	c.mu.Lock()
	defer c.mu.Unlock()
	return maps.Clone(c.conns)
}

// removeConn drops the entry only when it still refers to conn, so a late
// teardown from a previous link never evicts a live connection.
func (c *GatewayClient) removeConn(connID uint32, conn *ChunkConn) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if current, ok := c.conns[connID]; ok && current == conn {
		delete(c.conns, connID)
	}
}

func (c *GatewayClient) takeConn(connID uint32) *ChunkConn {
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
	c.conns = make(map[uint32]*ChunkConn)
	c.mu.Unlock()
	for _, conn := range conns {
		conn.Shutdown()
	}
}
