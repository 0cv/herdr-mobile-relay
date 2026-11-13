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
	"maps"
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
)

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

	mu         sync.Mutex
	closed     bool
	nat1To1    string
	stunServer string
	discovered netip.AddrPort
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

	mux := ice.NewUniversalUDPMuxDefault(ice.UniversalUDPMuxParams{UDPConn: udp})

	m := &Manager{
		logger:      logger,
		serve:       opts.Serve,
		onLocal:     opts.OnLocalCandidate,
		maxSessions: maxSessions,
		udp:         udp,
		mux:         mux,
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

func (m *Manager) natIPsLocked() []string {
	var ips []string
	if m.nat1To1 != "" {
		ips = append(ips, m.nat1To1)
	}
	if mapped := m.discovered; mapped.IsValid() && int(mapped.Port()) == m.LocalPort() {
		if ip := mapped.Addr().String(); ip != m.nat1To1 {
			ips = append(ips, ip)
		}
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

func (m *Manager) SetNAT1To1IP(ip string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return
	}
	m.nat1To1 = ip
	m.api = m.newAPILocked()
	m.logger.Info("webrtc port mapping updated")
}

func (m *Manager) SetSTUNServer(server string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return
	}
	m.stunServer = server
}

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
	live := slices.Collect(maps.Values(m.sessions))
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
	discovered := m.discovered
	live := len(m.sessions)
	m.mu.Unlock()

	answer, err := s.start(api, offerSDP)
	if err != nil {
		s.close("negotiation failed")
		return "", err
	}
	if discovered.IsValid() && int(discovered.Port()) != m.LocalPort() {
		s.publishSrflx(discovered)
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
	live := slices.Collect(maps.Values(m.sessions))
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
	s := &session{
		key:         key,
		manager:     m,
		sdpMid:      "0",
		localTypes:  make(map[string]int, 4),
		remoteTypes: make(map[string]int, 4),
	}
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
	live := slices.Collect(maps.Values(m.sessions))
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
	srflxSent     netip.AddrPort
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
	return SessionReport{
		LocalTypes:     maps.Clone(s.localTypes),
		RemoteTypes:    maps.Clone(s.remoteTypes),
		SelectedLocal:  s.selectedLocal,
		SelectedRemote: s.selectedRemote,
	}
}

// countTypeLocked tallies one candidate type. The caller must hold s.mu.
func countTypeLocked(counts map[string]int, line string) map[string]int {
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

func (s *session) publishSrflx(mapped netip.AddrPort) {
	if !mapped.IsValid() || s.manager.onLocal == nil {
		return
	}
	s.mu.Lock()
	if s.closed || s.srflxSent == mapped {
		s.mu.Unlock()
		return
	}
	s.srflxSent = mapped
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
