package gateway

import (
	"crypto/rand"
	"encoding/binary"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"sync"
	"time"
)

// Address discovery. Both ends of the hybrid transport must learn the address
// their NAT maps them to before ICE can find a direct path, and the gateway is
// the only server both of them already talk to. Reflecting a source address
// back to the peer it came from teaches the gateway nothing it did not already
// read off the datagram, so this costs none of the blind-gateway property and
// avoids depending on a third-party STUN service.
//
// This is the server half of RFC 5389 reduced to what a candidate gatherer
// needs: Binding Requests in, Binding Success Responses carrying
// XOR-MAPPED-ADDRESS out. There is no authentication (vanilla STUN has none),
// no ALTERNATE-SERVER, and deliberately no error response: a datagram that is
// not a well-formed Binding Request is dropped, because every byte this port
// sends to an unverified address is a byte an attacker could aim at a victim.
const (
	// stunHeaderSize is the fixed header: type, length, cookie, transaction id.
	stunHeaderSize = 20
	// stunMagicCookie identifies a STUN message and seeds the XOR encoding.
	stunMagicCookie = 0x2112A442
	// stunTxIDSize is the 96-bit transaction id echoed in every response.
	stunTxIDSize = 12
	// stunAttrHeaderSize is an attribute's type and length.
	stunAttrHeaderSize = 4

	// The only two message types this server handles.
	stunBindingRequest = 0x0001
	stunBindingSuccess = 0x0101

	// Attribute types used here.
	stunAttrXORMappedAddress = 0x0020
	stunAttrSoftware         = 0x8022

	// Address families in XOR-MAPPED-ADDRESS.
	stunFamilyIPv4 = 0x01
	stunFamilyIPv6 = 0x02

	// stunMaxRequest is the read buffer, sized to the IPv6 minimum MTU. Any
	// Binding Request an ICE agent sends is far smaller; a larger datagram is
	// truncated here, then fails the length check and is dropped.
	stunMaxRequest = 1280

	// stunSoftware is a packet-capture aid. It carries no version, because a
	// blind gateway should not fingerprint itself to strangers.
	stunSoftware = "herdr-gateway"
)

// stunMaxResponse bounds one response: the header, XOR-MAPPED-ADDRESS for an
// IPv6 source, and SOFTWARE padded to the 4-byte boundary RFC 5389 requires.
const stunMaxResponse = stunHeaderSize +
	stunAttrHeaderSize + 4 + 16 +
	stunAttrHeaderSize + (len(stunSoftware)+3)/4*4

// stunMandatoryResponse is the largest answer that carries nothing optional:
// header plus XOR-MAPPED-ADDRESS for an IPv6 source. A bare 20-byte request
// from an IPv6 peer cannot be answered in fewer bytes, so this is the
// protocol's own amplification floor and the one case where a response exceeds
// twice its request. Everything optional is gated on the 2x rule below.
const stunMandatoryResponse = stunHeaderSize + stunAttrHeaderSize + 4 + 16

// STUN rate limit. ICE gathering sends one Binding Request per local candidate
// and retransmits a few times, so a real peer needs well under ten datagrams
// per connection attempt; 20 per 5 seconds leaves room for repeated gathering
// while capping what any single source address — forged or not — can make this
// port emit.
const (
	stunRateLimit  = 20
	stunRateWindow = 5 * time.Second
	// stunLimiterSlots is a power of two so the slot index is a mask.
	stunLimiterSlots = 4096
)

// Global STUN ceiling. The per-source limiter cannot see a flood spread over
// thousands of forged source addresses: every datagram lands in a different
// slot and every one is under its own budget, so the listener would parse and
// answer all of them. This caps the whole port instead. A real ICE gathering
// needs single-digit datagrams, and even a busy gateway pairing many peers at
// once stays orders of magnitude below 2000 per second, so the ceiling only
// ever trims an attack. It is a constant rather than an env var because it is a
// property of what the protocol costs, not of the deployment.
const (
	stunGlobalRate   = 2000
	stunGlobalWindow = time.Second
)

// stunLimiter is a fixed-window counter like rateLimiter, but over a
// fixed-size table instead of a map. A UDP source address is trivially forged,
// so the map-per-key limiter would be a memory-exhaustion target on this path.
// Addresses that land in the same slot share one window, which can only deny
// more, never less; the seed is random per process so nobody can pick source
// addresses that collide with a chosen victim.
type stunLimiter struct {
	seed uint64

	mu sync.Mutex
	// global is the whole-listener window; slots are the per-source ones.
	global stunWindow
	slots  [stunLimiterSlots]stunWindow
}

type stunWindow struct {
	start time.Time
	count int
}

// charge counts one datagram against a fixed window and reports whether it fit.
// A window that has elapsed restarts at this datagram.
func (w *stunWindow) charge(now time.Time, window time.Duration, limit int) bool {
	if now.Sub(w.start) >= window {
		w.start = now
		w.count = 1
		return true
	}
	if w.count >= limit {
		return false
	}
	w.count++
	return true
}

func newSTUNLimiter() (*stunLimiter, error) {
	var seed [8]byte
	if _, err := rand.Read(seed[:]); err != nil {
		return nil, fmt.Errorf("gateway: stun limiter seed: %w", err)
	}
	return &stunLimiter{seed: binary.BigEndian.Uint64(seed[:])}, nil
}

// allow charges one datagram against the whole listener and against its source
// address. The global window is charged first: once the port is saturated the
// source is irrelevant, and stopping there keeps the flood's cost at one
// comparison per datagram.
func (l *stunLimiter) allow(source netip.Addr, now time.Time) bool {
	// Seeded FNV-1a over the 16-byte form: one code path for both families,
	// and no allocation per datagram.
	key := source.As16()
	hash := l.seed
	for _, b := range key {
		hash ^= uint64(b)
		hash *= 1099511628211
	}

	l.mu.Lock()
	defer l.mu.Unlock()
	if !l.global.charge(now, stunGlobalWindow, stunGlobalRate) {
		return false
	}
	return l.slots[hash&(stunLimiterSlots-1)].charge(now, stunRateWindow, stunRateLimit)
}

// startSTUN binds the address-discovery listener and records the port it
// actually received, so a caller may ask for port 0 and still advertise
// something a peer can dial.
func (s *Server) startSTUN(addr string) error {
	udpAddr, err := net.ResolveUDPAddr("udp", addr)
	if err != nil {
		return fmt.Errorf("gateway: stun address %q: %w", addr, err)
	}
	conn, err := net.ListenUDP("udp", udpAddr)
	if err != nil {
		return fmt.Errorf("gateway: stun listen on %q: %w", addr, err)
	}
	local, ok := conn.LocalAddr().(*net.UDPAddr)
	if !ok {
		conn.Close()
		return fmt.Errorf("gateway: stun listener has no UDP address")
	}
	limiter, err := newSTUNLimiter()
	if err != nil {
		conn.Close()
		return err
	}

	s.stunPort = local.Port
	s.stunLimiter = limiter

	// A blocking read cannot watch a context, so cancellation closes the socket
	// and the loop returns on the resulting net.ErrClosed.
	s.stunWG.Add(2)
	go func() {
		defer s.stunWG.Done()
		<-s.ctx.Done()
		conn.Close()
	}()
	go s.runSTUN(conn)

	s.logger.Info("stun listener started", "port", local.Port)
	return nil
}

// runSTUN answers Binding Requests until the socket closes. Work per datagram
// is bounded: one fixed read buffer, one response buffer reused across
// iterations, and no per-attribute parsing at all.
func (s *Server) runSTUN(conn *net.UDPConn) {
	defer s.stunWG.Done()
	defer conn.Close()

	request := make([]byte, stunMaxRequest)
	response := make([]byte, 0, stunMaxResponse)
	for {
		n, from, err := conn.ReadFromUDPAddrPort(request)
		if err != nil {
			if errors.Is(err, net.ErrClosed) || s.stopped() {
				return
			}
			// A transient error — an ICMP port-unreachable from an earlier
			// reply, say — must not end address discovery for everyone.
			s.logger.Debug("stun read failed", "error", err)
			continue
		}

		txID, ok := stunBindingTxID(request[:n])
		if !ok {
			continue
		}
		// The datagram is validated before the limiter is charged, so garbage
		// from a forged address cannot burn a real peer's budget. A dual-stack
		// socket reports IPv4 peers as ::ffff:a.b.c.d, and both the limiter key
		// and the reflected family want the real address.
		source := from.Addr().Unmap()
		if !s.stunLimiter.allow(source, s.now()) {
			continue
		}

		response = appendBindingSuccess(response[:0], txID, source, from.Port(), n)
		if _, err := conn.WriteToUDPAddrPort(response, from); err != nil {
			s.logger.Debug("stun write failed", "error", err)
		}
	}
}

// stunBindingTxID validates a datagram as a Binding Request and returns its
// transaction id, which aliases request. A wrong message type, a wrong magic
// cookie, a header shorter than 20 bytes, or a length field that disagrees with
// the datagram all mean "not a request", and an unrecognised datagram is
// answered with silence.
func stunBindingTxID(request []byte) ([]byte, bool) {
	if len(request) < stunHeaderSize {
		return nil, false
	}
	if binary.BigEndian.Uint16(request[0:2]) != stunBindingRequest {
		return nil, false
	}
	if binary.BigEndian.Uint32(request[4:8]) != stunMagicCookie {
		return nil, false
	}
	// The length counts attribute bytes only, and every attribute is padded to
	// a 4-byte boundary, so this rejects both truncated and over-long claims.
	length := int(binary.BigEndian.Uint16(request[2:4]))
	if length%4 != 0 || stunHeaderSize+length != len(request) {
		return nil, false
	}
	return request[8:stunHeaderSize], true
}

// appendBindingSuccess appends one Binding Success Response to dst. requestSize
// is the size of the datagram being answered: SOFTWARE is appended only while
// the response stays within twice the request, so nothing optional ever makes
// this port more attractive as a reflector.
func appendBindingSuccess(dst, txID []byte, source netip.Addr, port uint16, requestSize int) []byte {
	head := len(dst)
	dst = binary.BigEndian.AppendUint16(dst, stunBindingSuccess)
	dst = binary.BigEndian.AppendUint16(dst, 0) // patched once the length is known
	dst = binary.BigEndian.AppendUint32(dst, stunMagicCookie)
	dst = append(dst, txID...)
	dst = appendXORMappedAddress(dst, txID, source, port)

	software := stunAttrHeaderSize + (len(stunSoftware)+3)/4*4
	if len(dst)-head+software <= 2*requestSize {
		dst = appendSTUNAttr(dst, stunAttrSoftware, []byte(stunSoftware))
	}

	binary.BigEndian.PutUint16(dst[head+2:head+4], uint16(len(dst)-head-stunHeaderSize))
	return dst
}

// appendXORMappedAddress appends the one mandatory attribute. The XOR encoding
// exists so that a NAT rewriting payloads which happen to match its own
// mapping table cannot corrupt the reflected address.
func appendXORMappedAddress(dst, txID []byte, source netip.Addr, port uint16) []byte {
	var pad [4 + stunTxIDSize]byte
	binary.BigEndian.PutUint32(pad[0:4], stunMagicCookie)
	copy(pad[4:], txID)

	var value [4 + 16]byte
	binary.BigEndian.PutUint16(value[2:4], port^uint16(stunMagicCookie>>16))

	var size int
	if source.Is4() {
		value[1] = stunFamilyIPv4
		address := source.As4()
		size = len(address)
		for i, b := range address {
			value[4+i] = b ^ pad[i]
		}
	} else {
		value[1] = stunFamilyIPv6
		address := source.As16()
		size = len(address)
		for i, b := range address {
			value[4+i] = b ^ pad[i]
		}
	}
	return appendSTUNAttr(dst, stunAttrXORMappedAddress, value[:4+size])
}

// appendSTUNAttr appends one attribute with the zero padding that keeps the
// next attribute on a 4-byte boundary. The padding is not counted in the
// attribute length, per RFC 5389.
func appendSTUNAttr(dst []byte, attrType uint16, value []byte) []byte {
	dst = binary.BigEndian.AppendUint16(dst, attrType)
	dst = binary.BigEndian.AppendUint16(dst, uint16(len(value)))
	dst = append(dst, value...)
	for padding := (4 - len(value)%4) % 4; padding > 0; padding-- {
		dst = append(dst, 0)
	}
	return dst
}
