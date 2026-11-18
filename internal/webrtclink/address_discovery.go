package webrtclink

import (
	"context"
	"errors"
	"fmt"
	"hash/fnv"
	"maps"
	"net"
	"net/netip"
	"slices"
	"strconv"
	"strings"
)

func (m *Manager) DiscoverMappedAddress(ctx context.Context) (netip.AddrPort, error) {
	m.mu.Lock()
	closed := m.closed
	server := m.stunServer
	m.mu.Unlock()

	if closed {
		return netip.AddrPort{}, errManagerClosed
	}
	if server == "" {
		return netip.AddrPort{}, errNoSTUNServer
	}
	hostPort, err := stunHostPort(server)
	if err != nil {
		return netip.AddrPort{}, err
	}

	// Deliberately one family. IPv6 needs no reflexive discovery at all: there
	// is no NAT to reflect through, so a global IPv6 address is already a
	// reachable host candidate and pion gathers it directly (the socket is
	// dual-stack and UDP6 is in the network types). Asking for it anyway costs
	// real time — pion's universal mux cannot answer an IPv6 XOR-mapped request
	// through the shared socket, verified against v4.4.2, so every attempt would
	// burn the full discovery timeout on a keepalive that runs every 10 s.
	serverAddr, err := net.ResolveUDPAddr("udp", hostPort)
	if err != nil {
		return netip.AddrPort{}, fmt.Errorf("webrtclink: resolve stun server: %w", err)
	}
	ctx, cancel := context.WithTimeout(ctx, stunDiscoveryTimeout)
	defer cancel()
	reflected, err := m.mux.GetXORMappedAddrContext(ctx, serverAddr, stunDiscoveryTimeout)
	if err != nil {
		return netip.AddrPort{}, fmt.Errorf("webrtclink: stun address discovery: %w", err)
	}
	addr, ok := netip.AddrFromSlice(reflected.IP)
	if !ok {
		return netip.AddrPort{}, errors.New("webrtclink: stun server returned an unusable address")
	}
	return netip.AddrPortFrom(addr.Unmap(), uint16(reflected.Port)), nil
}

// stunHostPort turns a "stun:host:port" endpoint into a dialable host:port. A
// bare host:port is accepted too, so an operator-supplied value works either
// way.
func stunHostPort(server string) (string, error) {
	hostPort := strings.TrimPrefix(server, "stun:")
	host, port, err := net.SplitHostPort(hostPort)
	if err != nil {
		return "", fmt.Errorf("webrtclink: invalid stun server %q", server)
	}
	if host == "" || port == "" {
		return "", fmt.Errorf("webrtclink: invalid stun server %q", server)
	}
	return net.JoinHostPort(host, port), nil
}

func (m *Manager) PublishMappedAddress(mapped netip.AddrPort) {
	if !mapped.IsValid() || mapped.Addr().IsUnspecified() {
		return
	}
	mapped = netip.AddrPortFrom(mapped.Addr().Unmap(), mapped.Port())

	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return
	}
	m.discovered = mapped
	m.api = m.newAPILocked()
	live := slices.Collect(maps.Values(m.sessions))
	m.mu.Unlock()

	for _, s := range live {
		s.publishSrflx(mapped)
	}
	m.logger.Info("webrtc mapped address discovered", "sessions_notified", len(live))
}

// srflxCandidate renders one synthesised server-reflexive candidate for the
// discovered mapping. Synthesising it is legitimate because the mapping
// belongs to the very socket ICE listens on: the phone probes this address,
// the connectivity check arrives on our mux socket, and pion pairs the source
// as a peer-reflexive candidate exactly as it would for a gathered one.
func (m *Manager) srflxCandidate(mapped netip.AddrPort) string {
	relatedIP, relatedPort := m.relatedAddress(mapped.Addr())
	return fmt.Sprintf("candidate:%s %d udp %d %s %d typ srflx raddr %s rport %d",
		srflxFoundation(mapped.Addr()), srflxComponent, srflxPriority,
		mapped.Addr().String(), mapped.Port(), relatedIP, relatedPort)
}

// relatedAddress reports the base of the synthesised candidate. The shared
// socket is normally bound to the wildcard address, and raddr must stay in the
// mapped address' family for the line to parse, so an unusable base degrades
// to the unspecified address rather than to a mismatched one.
func (m *Manager) relatedAddress(mapped netip.Addr) (string, int) {
	unspecified := "0.0.0.0"
	if !mapped.Is4() {
		unspecified = "::"
	}
	addr, ok := m.udp.LocalAddr().(*net.UDPAddr)
	if !ok {
		return unspecified, 0
	}
	local, valid := netip.AddrFromSlice(addr.IP)
	if !valid || local.Unmap().Is4() != mapped.Is4() || local.IsUnspecified() {
		return unspecified, addr.Port
	}
	return local.Unmap().String(), addr.Port
}

// srflxFoundation derives a foundation that is stable per discovered address.
// RFC 8445 only requires candidates sharing a type, base and server to share a
// foundation, and the relay has exactly one of each.
func srflxFoundation(addr netip.Addr) string {
	sum := fnv.New32a()
	_, _ = sum.Write([]byte(addr.String()))
	return strconv.FormatUint(uint64(sum.Sum32()), 10)
}
