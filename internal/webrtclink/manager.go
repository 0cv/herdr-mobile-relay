// Package webrtclink negotiates direct browser-to-relay WebRTC DataChannel
// sessions and hands each open channel to the relay hub as a
// transport.FrameConn, so the encrypted protocol is identical on the direct
// path and on the relayed WebSocket paths.
package webrtclink

import (
	"context"
	"errors"
	"fmt"
	"hash/fnv"
	"log/slog"
	"net"
	"net/netip"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/pion/ice/v4"
	"github.com/pion/webrtc/v4"

	"github.com/0cv/herdr-mobile-relay/internal/transport"
)

const (
	// dataChannelLabel is the only DataChannel label the relay accepts. A
	// channel with any other label ends the peer connection.
	dataChannelLabel = "herdr-dc-v1"

	// defaultMaxSessions bounds negotiated plus pending sessions.
	defaultMaxSessions = 16

	// maxRemoteCandidates bounds the trickled candidates accepted per session;
	// further candidates are dropped, not errors.
	maxRemoteCandidates = 64

	// stallSweepInterval is how often partially received frames are checked
	// against framing.StallTimeout.
	stallSweepInterval = 5 * time.Second

	// stunDiscoveryTimeout bounds one address-discovery round trip against the
	// gateway's STUN listener.
	stunDiscoveryTimeout = 5 * time.Second

	// srflxComponent is the ICE component id of the only component a
	// DataChannel-only peer connection ever gathers.
	srflxComponent = 1

	// srflxPriority is the RFC 8445 priority of a server-reflexive candidate:
	// type preference 100, local preference 65535, component 1.
	srflxPriority = 100<<24 | 65535<<8 | (256 - srflxComponent)
)

var (
	// errManagerClosed reports that the relay is shutting down.
	errManagerClosed = errors.New("webrtclink: manager closed")
	// errNoSession reports signaling for a session that does not exist.
	errNoSession = errors.New("webrtclink: unknown webrtc session")
	// errInvalidOffer replaces the pion error, which can quote SDP lines that
	// must never reach a log or a client.
	errInvalidOffer = errors.New("webrtclink: invalid webrtc offer")
	// errNoSTUNServer reports address discovery with nothing to discover
	// against: the gateway advertised no STUN port, or none has arrived yet.
	errNoSTUNServer = errors.New("webrtclink: no stun server configured")
	// errNoMappedAddress reports that address discovery ran and no address
	// family answered, which is what a silent or unreachable STUN listener
	// looks like.
	errNoMappedAddress = errors.New("webrtclink: stun address discovery produced no mapping")
)

// stunFamilies are the networks address discovery probes independently. A
// dual-stack relay has a mapping per family, and the IPv6 one is the easiest
// direct path that exists: there is no NAT in front of it to traverse.
var stunFamilies = [...]string{"udp4", "udp6"}

// SessionKey identifies one negotiation: the relayed client that asked for the
// upgrade and the request id it used for signaling.
type SessionKey struct {
	ClientID  string
	RequestID string
}

// Candidate is one trickled ICE candidate, in the shape both signaling
// directions use.
type Candidate struct {
	Candidate     string
	SDPMid        string
	SDPMLineIndex uint16
}

// Options configures the Manager. Serve is required; it receives every accepted
// DataChannel as a transport.FrameConn and owns it until it returns.
type Options struct {
	Logger *slog.Logger
	// UDPPort is the single ICE port shared by every session; 0 picks an
	// ephemeral port.
	UDPPort int
	// MaxSessions bounds concurrent sessions; 0 selects defaultMaxSessions.
	MaxSessions int
	// NAT1To1IPs are externally reachable addresses (from a port mapping or
	// operator configuration) advertised as server-reflexive candidates.
	NAT1To1IPs []string
	// STUNServers are "stun:host:port" endpoints used to discover the mapped
	// address of the shared ICE socket. Only the first is used; the relay has
	// exactly one gateway to ask.
	STUNServers []string
	// OnLocalCandidate delivers a locally gathered candidate to the signaling
	// layer. Nil disables trickle from the relay side.
	OnLocalCandidate func(SessionKey, Candidate)
	// Serve runs one logical connection for its whole lifetime.
	Serve func(context.Context, transport.FrameConn)
}

// Manager owns the process-wide WebRTC listener and every direct session on it.
// One UDP socket and one ICE mux serve all sessions, so the reachability the
// relay advertises (host address plus any mapped address) is valid for every
// client regardless of how many are connected.
type Manager struct {
	logger      *slog.Logger
	serve       func(context.Context, transport.FrameConn)
	onLocal     func(SessionKey, Candidate)
	maxSessions int

	udp *net.UDPConn
	// mux is a universal mux so address discovery reuses the one ICE socket:
	// the mapping a STUN server reflects is then the mapping ICE itself has.
	mux *ice.UniversalUDPMuxDefault

	baseCtx context.Context
	cancel  context.CancelFunc
	wg      sync.WaitGroup

	// Session outcome tallies, atomic because sessions close on their own
	// goroutines and /healthz reads them from the HTTP handler.
	direct  atomic.Int64
	relayed atomic.Int64

	mu     sync.Mutex
	closed bool
	// nat1To1 holds addresses supplied by the port mapper or the operator.
	nat1To1 []string
	// stunServers holds the address-discovery endpoints; see Options.
	stunServers []string
	// discovered holds the mappings STUN last reflected for the shared socket,
	// at most one per address family.
	discovered []netip.AddrPort
	api        *webrtc.API
	sessions   map[SessionKey]*session
}

// New binds the shared ICE socket and starts the stall sweeper.
func New(opts Options) (*Manager, error) {
	if opts.Serve == nil {
		return nil, errors.New("webrtclink: Serve is required")
	}
	logger := opts.Logger
	if logger == nil {
		logger = slog.Default()
	}
	maxSessions := opts.MaxSessions
	if maxSessions <= 0 {
		maxSessions = defaultMaxSessions
	}

	udp, err := net.ListenUDP("udp", &net.UDPAddr{Port: opts.UDPPort})
	if err != nil {
		return nil, fmt.Errorf("webrtclink: listen udp: %w", err)
	}

	// pion starts the mux read worker before it finishes assigning the mux's
	// own fields (ice v4.4.1, udp_mux_universal.go:87 writes UDPMuxDefault
	// after NewUDPMuxDefault has already launched connWorker), so a datagram
	// that lands during construction is handled against a half-written struct.
	// The race detector catches it whenever a stray STUN packet arrives on a
	// freshly bound port. Holding reads until the constructor returns closes
	// the window without reaching into pion's internals.
	gated := &gatedConn{UDPConn: udp, open: make(chan struct{})}
	mux := ice.NewUniversalUDPMuxDefault(ice.UniversalUDPMuxParams{UDPConn: gated})
	close(gated.open)

	m := &Manager{
		logger:      logger,
		serve:       opts.Serve,
		onLocal:     opts.OnLocalCandidate,
		maxSessions: maxSessions,
		udp:         udp,
		mux:         mux,
		nat1To1:     append([]string(nil), opts.NAT1To1IPs...),
		stunServers: append([]string(nil), opts.STUNServers...),
		sessions:    make(map[SessionKey]*session),
	}
	m.api = m.newAPILocked()
	m.baseCtx, m.cancel = context.WithCancel(context.Background())

	m.wg.Add(1)
	go m.sweep()

	m.logger.Info("webrtc listener ready", "port", m.LocalPort(), "max_sessions", maxSessions)
	return m, nil
}

// newAPILocked builds the API from the current mapped addresses. It must be
// called with mu held, or before the Manager is published.
func (m *Manager) newAPILocked() *webrtc.API {
	settings := webrtc.SettingEngine{}
	settings.SetICEUDPMux(m.mux)
	// Everything must flow through the one shared UDP socket: no TCP
	// candidates, and no mDNS resolver socket per peer connection. Browsers
	// that obfuscate their host candidates as .local names still connect,
	// because they probe the reachable candidates the relay publishes and the
	// resulting peer-reflexive candidate is enough.
	settings.SetNetworkTypes([]webrtc.NetworkType{webrtc.NetworkTypeUDP4, webrtc.NetworkTypeUDP6})
	settings.DisableActiveTCP(true)
	settings.SetICEMulticastDNSMode(ice.MulticastDNSModeDisabled)
	if ips := m.natIPsLocked(); len(ips) > 0 {
		settings.SetNAT1To1IPs(ips, webrtc.ICECandidateTypeSrflx)
	}
	return webrtc.NewAPI(webrtc.WithSettingEngine(settings))
}

// natIPsLocked composes the addresses advertised as 1:1 mapped: whatever the
// port mapper or the operator supplied, plus every discovered address whose
// port the NAT preserved. A discovery on a different port cannot be expressed
// as a 1:1 mapping and is trickled as a synthesised candidate instead. It must
// be called with mu held.
func (m *Manager) natIPsLocked() []string {
	ips := append([]string(nil), m.nat1To1...)
	for _, mapped := range m.discovered {
		if !mapped.IsValid() || int(mapped.Port()) != m.LocalPort() {
			continue
		}
		ip := mapped.Addr().String()
		if slices.Contains(ips, ip) {
			continue
		}
		ips = append(ips, ip)
	}
	return ips
}

// LocalPort reports the shared ICE UDP port, resolved when UDPPort was 0.
func (m *Manager) LocalPort() int {
	addr, ok := m.udp.LocalAddr().(*net.UDPAddr)
	if !ok {
		return 0
	}
	return addr.Port
}

// SetNAT1To1IPs replaces the externally reachable addresses advertised to new
// sessions. Existing sessions keep the addresses they negotiated with.
func (m *Manager) SetNAT1To1IPs(ips []string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return
	}
	m.nat1To1 = append([]string(nil), ips...)
	m.api = m.newAPILocked()
	m.logger.Info("webrtc mapped addresses updated", "count", len(m.nat1To1))
}

// SetSTUNServers replaces the address-discovery endpoints. It mirrors
// SetNAT1To1IPs: new values apply to the next discovery, and a closed Manager
// ignores the update.
func (m *Manager) SetSTUNServers(servers []string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return
	}
	m.stunServers = append([]string(nil), servers...)
}

// DiscoverMappedAddress asks the first configured STUN server what address it
// sees, using the shared ICE socket. The answer therefore describes the exact
// socket every session already gathers on, which is what makes it publishable
// as a reflexive candidate.
func (m *Manager) DiscoverMappedAddresses(ctx context.Context) ([]netip.AddrPort, error) {
	m.mu.Lock()
	closed := m.closed
	var server string
	if len(m.stunServers) > 0 {
		server = m.stunServers[0]
	}
	m.mu.Unlock()

	if closed {
		return nil, errManagerClosed
	}
	if server == "" {
		return nil, errNoSTUNServer
	}
	hostPort, err := stunHostPort(server)
	if err != nil {
		return nil, err
	}

	// Deliberately one family. IPv6 needs no reflexive discovery at all: there
	// is no NAT to reflect through, so a global IPv6 address is already a
	// reachable host candidate and pion gathers it directly (the socket is
	// dual-stack and UDP6 is in the network types). Asking for it anyway costs
	// real time — pion's universal mux cannot answer an IPv6 XOR-mapped request
	// through the shared socket, verified against v4.4.1, so every attempt would
	// burn the full discovery timeout on a keepalive that runs every 10 s.
	mapped, err := m.discoverOn(ctx, "udp", hostPort)
	if err != nil {
		return nil, fmt.Errorf("webrtclink: stun address discovery: %w", err)
	}
	return []netip.AddrPort{mapped}, nil
}

// discoverOn asks the STUN server for the mapping of the shared socket in one
// address family.
func (m *Manager) discoverOn(ctx context.Context, network, hostPort string) (netip.AddrPort, error) {
	serverAddr, err := net.ResolveUDPAddr(network, hostPort)
	if err != nil {
		return netip.AddrPort{}, fmt.Errorf("resolve %s stun server: %w", network, err)
	}
	ctx, cancel := context.WithTimeout(ctx, stunDiscoveryTimeout)
	defer cancel()
	reflected, err := m.mux.GetXORMappedAddrContext(ctx, serverAddr, stunDiscoveryTimeout)
	if err != nil {
		return netip.AddrPort{}, fmt.Errorf("%s: %w", network, err)
	}
	addr, ok := netip.AddrFromSlice(reflected.IP)
	if !ok {
		return netip.AddrPort{}, fmt.Errorf("%s: stun server returned an unusable address", network)
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

// PublishMappedAddresses advertises the discovered mappings to ICE. Rebuilding
// the API makes every mapping available to future sessions. Existing sessions
// were created from the previous API, so each mapping is also trickled to them;
// this matters even when the NAT preserved the port, particularly when initial
// discovery races the first phone offer or the relay changes networks.
func (m *Manager) PublishMappedAddresses(mapped []netip.AddrPort) {
	usable := make([]netip.AddrPort, 0, len(mapped))
	for _, addr := range mapped {
		if !addr.IsValid() || addr.Addr().IsUnspecified() {
			continue
		}
		usable = append(usable, netip.AddrPortFrom(addr.Addr().Unmap(), addr.Port()))
	}
	if len(usable) == 0 {
		return
	}

	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return
	}
	m.discovered = usable
	m.api = m.newAPILocked()
	// Sessions already reserved captured the previous API. Trickle every
	// mapping to them; session-level deduplication keeps repeated discovery from
	// emitting the same candidate twice.
	live := make([]*session, 0, len(m.sessions))
	for _, s := range m.sessions {
		live = append(live, s)
	}
	m.mu.Unlock()

	for _, s := range live {
		for _, addr := range usable {
			s.publishSrflx(addr)
		}
	}
	m.logger.Info("webrtc mapped addresses discovered",
		"families", len(usable), "sessions_notified", len(live))
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

// Candidate types are the only ICE detail worth reporting: they answer "why is
// this session relayed?" without ever exposing an address. The set is closed on
// purpose — the remote side supplies these strings, and an open set would let a
// peer grow the map keys without bound.
func candidateType(line string) string {
	fields := strings.Fields(line)
	for i := 0; i+1 < len(fields); i++ {
		if fields[i] != "typ" {
			continue
		}
		switch fields[i+1] {
		case "host", "srflx", "prflx", "relay":
			return fields[i+1]
		}
		return "other"
	}
	return "other"
}

// SessionReport is the observable shape of one direct session: which candidate
// types each side offered, and which pair ICE actually selected. An empty
// selection means no pair has been nominated yet, which is what a session stuck
// behind a hostile NAT looks like.
type SessionReport struct {
	LocalTypes     map[string]int
	RemoteTypes    map[string]int
	SelectedLocal  string
	SelectedRemote string
}

// SessionReports snapshots every live session for status reporting.
func (m *Manager) SessionReports() []SessionReport {
	m.mu.Lock()
	live := make([]*session, 0, len(m.sessions))
	for _, s := range m.sessions {
		live = append(live, s)
	}
	m.mu.Unlock()

	reports := make([]SessionReport, 0, len(live))
	for _, s := range live {
		reports = append(reports, s.report())
	}
	return reports
}

// SessionCount reports how many sessions are pending or live.
func (m *Manager) SessionCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.sessions)
}

// HandleOffer answers a client offer and returns the answer SDP. A repeated
// offer on a known key is a renegotiation (an ICE restart, typically) and reuses
// the existing peer connection and DataChannel.
func (m *Manager) HandleOffer(ctx context.Context, key SessionKey, offerSDP string) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}

	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return "", errManagerClosed
	}
	if existing, ok := m.sessions[key]; ok {
		m.mu.Unlock()
		answer, err := existing.negotiate(offerSDP)
		if err != nil {
			return "", err
		}
		m.logger.Info("webrtc session renegotiated", "sessions", m.SessionCount())
		return answer, nil
	}
	if len(m.sessions) >= m.maxSessions {
		live := len(m.sessions)
		m.mu.Unlock()
		return "", fmt.Errorf("webrtclink: webrtc session limit reached (%d of %d in use)", live, m.maxSessions)
	}
	s := m.newSessionLocked(key)
	api := m.api
	discovered := append([]netip.AddrPort(nil), m.discovered...)
	live := len(m.sessions)
	m.mu.Unlock()

	answer, err := s.start(api, offerSDP)
	if err != nil {
		s.close("negotiation failed")
		return "", err
	}
	// A discovery whose port the NAT changed is not in the API's NAT1To1 set,
	// so a session starting after it has to be told about each one separately.
	for _, mapped := range discovered {
		if mapped.IsValid() && int(mapped.Port()) != m.LocalPort() {
			s.publishSrflx(mapped)
		}
	}
	m.logger.Info("webrtc session negotiated", "sessions", live)
	return answer, nil
}

// AddRemoteCandidate feeds one trickled client candidate to its session.
func (m *Manager) AddRemoteCandidate(key SessionKey, c Candidate) error {
	s := m.lookup(key)
	if s == nil {
		return errNoSession
	}
	return s.addRemoteCandidate(c)
}

// CloseSession tears down one session; unknown keys are a no-op.
func (m *Manager) CloseSession(key SessionKey, reason string) {
	if s := m.lookup(key); s != nil {
		s.close(reason)
	}
}

// Close terminates every session, the sweeper, and the shared socket.
func (m *Manager) Close() error {
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return nil
	}
	m.closed = true
	live := make([]*session, 0, len(m.sessions))
	for _, s := range m.sessions {
		live = append(live, s)
	}
	m.mu.Unlock()

	for _, s := range live {
		s.close("relay shutting down")
	}
	m.cancel()
	m.wg.Wait()

	if err := m.mux.Close(); err != nil {
		return fmt.Errorf("webrtclink: close udp mux: %w", err)
	}
	m.logger.Info("webrtc listener closed")
	return nil
}

func (m *Manager) lookup(key SessionKey) *session {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.sessions[key]
}

// newSessionLocked reserves the session slot; the caller must hold mu.
func (m *Manager) newSessionLocked(key SessionKey) *session {
	// A DataChannel-only answer carries exactly one media section, mid "0";
	// the first real local candidate confirms it.
	s := &session{key: key, manager: m, sdpMid: "0"}
	s.ctx, s.cancel = context.WithCancel(m.baseCtx)
	m.sessions[key] = s
	return s
}

// forget releases a session slot and reports how many remain.
func (m *Manager) forget(s *session) int {
	m.mu.Lock()
	defer m.mu.Unlock()
	if current, ok := m.sessions[s.key]; ok && current == s {
		delete(m.sessions, s.key)
	}
	return len(m.sessions)
}

// Outcomes counts finished sessions by whether ICE ever nominated a pair.
// Monotonic since start, and the only aggregate the relay keeps: it answers
// "how often does the direct path actually form here" without retaining a
// single address.
type Outcomes struct {
	Direct  int64
	Relayed int64
}

// countOutcome records one finished session. Called from session.close, which
// is guarded by closeOnce, so every session lands in exactly one bucket.
func (m *Manager) countOutcome(nominated bool) {
	if nominated {
		m.direct.Add(1)
		return
	}
	m.relayed.Add(1)
}

// Outcomes snapshots the counters.
func (m *Manager) Outcomes() Outcomes {
	return Outcomes{Direct: m.direct.Load(), Relayed: m.relayed.Load()}
}

// goServe hands an open connection to the hub on its own goroutine, tracked so
// Close waits for it. It reports false once the Manager is closing.
func (m *Manager) goServe(s *session, conn *dataChannelConn) bool {
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return false
	}
	m.wg.Add(1)
	m.mu.Unlock()

	go func() {
		defer m.wg.Done()
		defer s.close("connection finished")
		m.serve(s.ctx, conn)
	}()
	return true
}

// sweep abandons sessions whose inbound frame assembly stalled.
func (m *Manager) sweep() {
	defer m.wg.Done()
	ticker := time.NewTicker(stallSweepInterval)
	defer ticker.Stop()
	for {
		select {
		case <-m.baseCtx.Done():
			return
		case now := <-ticker.C:
			m.sweepStalled(now)
		}
	}
}

// sweepStalled closes every session whose inbound assembly stopped making
// progress before now.
func (m *Manager) sweepStalled(now time.Time) {
	m.mu.Lock()
	live := make([]*session, 0, len(m.sessions))
	for _, s := range m.sessions {
		live = append(live, s)
	}
	m.mu.Unlock()
	for _, s := range live {
		if conn := s.connection(); conn != nil && conn.stalled(now) {
			s.close("inbound frame assembly stalled")
		}
	}
}

// session is one peer connection and the single DataChannel it may carry.
type session struct {
	key     SessionKey
	manager *Manager
	ctx     context.Context
	cancel  context.CancelFunc

	// negotiateMu serializes offer handling so a renegotiation cannot overlap
	// the initial answer.
	negotiateMu sync.Mutex

	mu         sync.Mutex
	pc         *webrtc.PeerConnection
	conn       *dataChannelConn
	candidates int
	pending    []Candidate
	closed     bool
	// sdpMid and sdpMLineIndex mirror what pion attaches to the session's own
	// local candidates, so a synthesised one lands on the same media section.
	sdpMid        string
	sdpMLineIndex uint16
	// srflxSent are the discovered mappings already trickled to this session,
	// one per address family; a repeated discovery must not duplicate them.
	srflxSent []netip.AddrPort
	// localTypes, remoteTypes and the selected pair answer why a session is or
	// is not direct. Counts only: an address here would leak into /healthz.
	localTypes     map[string]int
	remoteTypes    map[string]int
	selectedLocal  string
	selectedRemote string

	closeOnce sync.Once
}

// start creates the peer connection, wires the callbacks, and answers the first
// offer.
func (s *session) start(api *webrtc.API, offerSDP string) (string, error) {
	pc, err := api.NewPeerConnection(webrtc.Configuration{})
	if err != nil {
		return "", fmt.Errorf("webrtclink: new peer connection: %w", err)
	}

	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		_ = pc.Close()
		return "", errManagerClosed
	}
	s.pc = pc
	s.mu.Unlock()

	pc.OnICECandidate(s.handleLocalCandidate)
	pc.OnConnectionStateChange(s.handleConnectionState)
	pc.OnDataChannel(s.handleDataChannel)
	// The nominated pair is the whole answer to "is this session actually
	// direct, and over which candidate type?". SCTP carries the DataChannel, so
	// its transport chain owns the ICE agent for this session.
	if sctp := pc.SCTP(); sctp != nil {
		if dtls := sctp.Transport(); dtls != nil {
			if ice := dtls.ICETransport(); ice != nil {
				ice.OnSelectedCandidatePairChange(s.handleSelectedPair)
			}
		}
	}

	answer, err := s.negotiate(offerSDP)
	if err != nil {
		return "", err
	}
	s.flushPendingCandidates()
	return answer, nil
}

// handleSelectedPair records the nominated pair's candidate types.
func (s *session) handleSelectedPair(pair *webrtc.ICECandidatePair) {
	if pair == nil {
		return
	}
	s.mu.Lock()
	s.selectedLocal = pair.Local.Typ.String()
	s.selectedRemote = pair.Remote.Typ.String()
	s.mu.Unlock()
	s.manager.logger.Info("webrtc pair selected",
		"local", pair.Local.Typ.String(), "remote", pair.Remote.Typ.String())
}

// report snapshots the candidate shape of this session.
func (s *session) report() SessionReport {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := SessionReport{
		LocalTypes:     make(map[string]int, len(s.localTypes)),
		RemoteTypes:    make(map[string]int, len(s.remoteTypes)),
		SelectedLocal:  s.selectedLocal,
		SelectedRemote: s.selectedRemote,
	}
	for name, count := range s.localTypes {
		out.LocalTypes[name] = count
	}
	for name, count := range s.remoteTypes {
		out.RemoteTypes[name] = count
	}
	return out
}

// countTypeLocked tallies one candidate type. The caller must hold s.mu.
func countTypeLocked(counts map[string]int, line string) map[string]int {
	if counts == nil {
		counts = make(map[string]int, 4)
	}
	counts[candidateType(line)]++
	return counts
}

// negotiate answers an offer on an existing peer connection.
func (s *session) negotiate(offerSDP string) (string, error) {
	s.negotiateMu.Lock()
	defer s.negotiateMu.Unlock()

	s.mu.Lock()
	pc, closed := s.pc, s.closed
	s.mu.Unlock()
	if pc == nil || closed {
		return "", errNoSession
	}
	if err := pc.SetRemoteDescription(webrtc.SessionDescription{
		Type: webrtc.SDPTypeOffer,
		SDP:  offerSDP,
	}); err != nil {
		return "", errInvalidOffer
	}
	answer, err := pc.CreateAnswer(nil)
	if err != nil {
		return "", fmt.Errorf("webrtclink: create answer: %w", err)
	}
	if err := pc.SetLocalDescription(answer); err != nil {
		return "", fmt.Errorf("webrtclink: set local description: %w", err)
	}
	return answer.SDP, nil
}

// addRemoteCandidate applies one candidate, buffering it while the peer
// connection is still being created and dropping anything past the cap.
func (s *session) addRemoteCandidate(c Candidate) error {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return errNoSession
	}
	if s.candidates >= maxRemoteCandidates {
		s.mu.Unlock()
		return nil
	}
	s.candidates++
	s.remoteTypes = countTypeLocked(s.remoteTypes, c.Candidate)
	pc := s.pc
	if pc == nil {
		s.pending = append(s.pending, c)
		s.mu.Unlock()
		return nil
	}
	s.mu.Unlock()

	return applyCandidate(pc, c)
}

func applyCandidate(pc *webrtc.PeerConnection, c Candidate) error {
	init := webrtc.ICECandidateInit{Candidate: c.Candidate, SDPMLineIndex: &c.SDPMLineIndex}
	if c.SDPMid != "" {
		init.SDPMid = &c.SDPMid
	}
	if err := pc.AddICECandidate(init); err != nil {
		// The candidate string is never quoted: it carries client addresses.
		return errors.New("webrtclink: rejected ice candidate")
	}
	return nil
}

// flushPendingCandidates applies candidates that arrived before the peer
// connection existed.
func (s *session) flushPendingCandidates() {
	s.mu.Lock()
	pending := s.pending
	s.pending = nil
	pc := s.pc
	s.mu.Unlock()
	if pc == nil {
		return
	}
	for _, c := range pending {
		if err := applyCandidate(pc, c); err != nil {
			s.manager.logger.Debug("webrtc candidate rejected", "error", err)
		}
	}
}

func (s *session) connection() *dataChannelConn {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.conn
}

func (s *session) handleLocalCandidate(candidate *webrtc.ICECandidate) {
	if candidate == nil || s.manager.onLocal == nil {
		return
	}
	init := candidate.ToJSON()
	local := Candidate{Candidate: init.Candidate}
	if init.SDPMid != nil {
		local.SDPMid = *init.SDPMid
	}
	if init.SDPMLineIndex != nil {
		local.SDPMLineIndex = *init.SDPMLineIndex
	}
	s.mu.Lock()
	s.sdpMid, s.sdpMLineIndex = local.SDPMid, local.SDPMLineIndex
	s.localTypes = countTypeLocked(s.localTypes, local.Candidate)
	s.mu.Unlock()
	s.manager.onLocal(s.key, local)
}

// publishSrflx trickles the synthesised reflexive candidate for a discovered
// mapping whose port the NAT did not preserve. It is emitted at most once per
// address: the phone only needs the address to probe it, and the resulting
// connectivity check lands on the shared mux socket like any other.
func (s *session) publishSrflx(mapped netip.AddrPort) {
	if !mapped.IsValid() || s.manager.onLocal == nil {
		return
	}
	s.mu.Lock()
	if s.closed || slices.Contains(s.srflxSent, mapped) {
		s.mu.Unlock()
		return
	}
	s.srflxSent = append(s.srflxSent, mapped)
	local := Candidate{
		Candidate:     s.manager.srflxCandidate(mapped),
		SDPMid:        s.sdpMid,
		SDPMLineIndex: s.sdpMLineIndex,
	}
	s.mu.Unlock()
	s.manager.onLocal(s.key, local)
}

func (s *session) handleConnectionState(state webrtc.PeerConnectionState) {
	s.manager.logger.Info("webrtc connection state", "state", state.String(), "sessions", s.manager.SessionCount())
	switch state {
	case webrtc.PeerConnectionStateFailed,
		webrtc.PeerConnectionStateDisconnected,
		webrtc.PeerConnectionStateClosed:
		go s.close("peer connection " + state.String())
	}
}

// handleDataChannel accepts the one expected channel. It runs before the channel
// opens, so the message handler is installed before any frame can arrive.
func (s *session) handleDataChannel(dc *webrtc.DataChannel) {
	if dc.Label() != dataChannelLabel {
		s.manager.logger.Warn("webrtc datachannel refused", "reason", "unexpected label")
		go s.close("unexpected datachannel label")
		return
	}

	conn := newDataChannelConn(dc, func(reason string) { s.close(reason) })

	s.mu.Lock()
	if s.closed || s.conn != nil {
		s.mu.Unlock()
		go s.close("duplicate datachannel")
		return
	}
	s.conn = conn
	s.mu.Unlock()

	dc.SetBufferedAmountLowThreshold(resumeBufferedAmount)
	dc.OnBufferedAmountLow(conn.signalLowWater)
	dc.OnMessage(conn.handleMessage)
	dc.OnClose(func() { go s.close("datachannel closed") })
	dc.OnError(func(error) { go s.close("datachannel failed") })
	dc.OnOpen(func() {
		if !s.manager.goServe(s, conn) {
			s.close("relay shutting down")
		}
	})
}

// close tears the session down exactly once: the hub connection first so its
// reader unblocks, then the peer connection, then the session slot.
func (s *session) close(reason string) {
	s.closeOnce.Do(func() {
		s.cancel()

		s.mu.Lock()
		s.closed = true
		conn := s.conn
		pc := s.pc
		nominated := s.selectedRemote != ""
		s.mu.Unlock()

		// Classify the finished session exactly once. A session that never
		// nominated a pair is one the direct path failed to carry, which is the
		// population that decides whether symmetric-NAT port prediction is worth
		// building; closeOnce is what keeps the counter honest.
		s.manager.countOutcome(nominated)

		if conn != nil {
			conn.shutdown()
		}
		if pc != nil {
			if err := pc.Close(); err != nil {
				s.manager.logger.Debug("webrtc peer connection close", "error", err)
			}
		}
		s.manager.logger.Info("webrtc session closed", "reason", reason, "sessions", s.manager.forget(s))
	})
}
