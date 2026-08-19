package webrtclink

import (
	"net"
	"net/netip"
	"sync/atomic"

	"github.com/pion/ice/v4"
)

// gatedConn defers reads until the socket's owner says it is safe to deliver
// datagrams. It exists for exactly one reason: pion's
// NewUniversalUDPMuxDefault launches the read worker before it finishes
// assigning the mux's own fields, so a packet that arrives mid-construction is
// dispatched against a struct that is still being written. Gating the reads
// makes the ordering explicit instead of relying on the socket being quiet.
//
// It deliberately implements ice.AddrPortReaderWriter: pion only takes the
// allocation-free read path when the connection offers ReadFromAddrPort, and a
// wrapper that dropped it would add a per-datagram allocation on the data path.
type gatedConn struct {
	*net.UDPConn

	ready atomic.Bool
	open  chan struct{}
}

// The fast path is a silent contract: if this assertion ever fails, pion falls
// back to allocating a net.Addr per datagram instead of erroring.
var _ ice.AddrPortReaderWriter = (*gatedConn)(nil)

// wait blocks the first read until the gate opens. Afterwards it is one relaxed
// atomic load per datagram, rather than a channel receive.
func (c *gatedConn) wait() {
	if c.ready.Load() {
		return
	}
	<-c.open
	c.ready.Store(true)
}

func (c *gatedConn) ReadFrom(b []byte) (int, net.Addr, error) {
	c.wait()
	return c.UDPConn.ReadFrom(b)
}

func (c *gatedConn) ReadFromAddrPort(b []byte) (int, netip.AddrPort, error) {
	c.wait()
	return c.UDPConn.ReadFromUDPAddrPort(b)
}

// WriteToAddrPort is ungated: nothing writes before the mux exists, and the
// send path must stay allocation-free.
func (c *gatedConn) WriteToAddrPort(b []byte, addr netip.AddrPort) (int, error) {
	return c.UDPConn.WriteToUDPAddrPort(b, addr)
}
