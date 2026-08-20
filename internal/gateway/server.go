// Package gateway implements the blind WebSocket rendezvous gateway that pairs
// a Herdr computer relay with the phones paired to it.
//
// The gateway holds no secrets. It issues a random challenge, forwards the
// phone's answer to the relay, and copies opaque frames; only the relay can
// verify that answer, because only the relay and the paired phone can derive
// the rendezvous key from the relay key. Everything the gateway observes is
// ciphertext, so it enforces nothing but anti-abuse limits: connect rate,
// concurrent clients per relay, relayed-byte quotas, frame sizes, and idle
// timeouts.
package gateway

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf8"

	"github.com/coder/websocket"

	"github.com/0cv/herdr-mobile-relay/internal/gatewaywire"
)

const (
	// helloTimeout bounds the JSON hello exchange on both endpoints.
	helloTimeout = 10 * time.Second
	// writeTimeout bounds a single WebSocket write on either link.
	writeTimeout = 5 * time.Second
	// pingInterval is how often the gateway pings a registered relay link.
	pingInterval = 30 * time.Second
	// maxUnansweredPings drops a relay that missed this many consecutive pongs.
	maxUnansweredPings = 2
	// displaceProbeTimeout bounds the liveness probe a relay link gets when the
	// same relay id registers again. It is deliberately short: a relay that is
	// really there answers a ping in one round trip, while a restart, a crashed
	// process, or a laptop that closed its lid must be able to reclaim its id
	// promptly. Only a link that stays silent this long is displaced.
	displaceProbeTimeout = 2 * time.Second
	// maxIdleSweepInterval bounds how coarsely idle connections are reaped.
	maxIdleSweepInterval = 30 * time.Second
	// persistInterval is how often relayed-byte counters reach disk.
	persistInterval = 30 * time.Second
	// probeInterval is the minimum spacing between /probe calls from one IP.
	probeInterval = 10 * time.Second
	// clientQueueDepth bounds queued outbound frames per phone connection.
	clientQueueDepth = 32
	// maxQueuedBytes bounds the per-phone outbound queue in bytes; a phone that
	// exceeds it is dropped as too slow rather than stalling its siblings.
	maxQueuedBytes = 4 << 20
	// maxProbeBody bounds the /probe request body.
	maxProbeBody = 1 << 10
	// relayIDLogPrefix is how much of a relay id may ever reach a log line.
	relayIDLogPrefix = 6
)

// Limit defaults applied by New when the matching Options field is zero.
const (
	DefaultMaxRelays            = 1024
	DefaultMaxClients           = 512
	DefaultMaxClientsPerRelay   = 8
	DefaultConnectRatePerMinute = 30
	DefaultMonthlyBytes         = 5 << 30
	DefaultQuotaWarnPercent     = 80
	DefaultIdleTimeout          = 5 * time.Minute
)

// Close reasons sent to a phone or relay. They describe transport state only;
// frame contents, nonces, and proofs never appear in them.
const (
	reasonGatewayShutdown = "gateway_shutdown"
	reasonRelayGone       = "relay_disconnected"
	reasonRelayReplaced   = "relay_replaced"
	reasonIdleTimeout     = "idle_timeout"
	reasonSlowClient      = "slow_client"
	reasonClientClosed    = "client_closed"
	reasonRelayWriteFail  = "relay_write_failed"
	reasonBinaryOnly      = "binary_frames_only"
	reasonPingTimeout     = "ping_timeout"
	reasonUnknownConn     = "unknown_conn"
)

// Options configures a Server. Every limit follows the same convention: a zero
// value selects the documented default, and a negative value removes the limit.
type Options struct {
	// MaxRelays caps how many relay ids may be registered at once. It bounds
	// the registration table and the goroutines and sockets behind it on a
	// shared public instance. Re-registering an id that is already present
	// replaces its link and is never refused.
	// Default DefaultMaxRelays; negative removes the cap.
	MaxRelays int
	// MaxClients caps concurrent phone connections across every relay, so the
	// per-relay cap cannot be bypassed by spreading load over many relay ids.
	// Each connection owns an outbound queue bounded at 4 MiB, so 512 is a
	// worst-case ceiling of about 2 GiB — survivable on the 1-2 GB VPS a
	// community gateway runs on, while typical queues sit near empty.
	// Default DefaultMaxClients; negative removes the cap.
	MaxClients int
	// MaxClientsPerRelay caps concurrent phone connections per registration.
	// Default DefaultMaxClientsPerRelay; negative removes the cap.
	MaxClientsPerRelay int
	// ConnectRatePerMinute caps /connect attempts from one client IP in a
	// rolling one-minute window. Relay registrations are counted separately
	// against the same limit so a phone flood cannot lock a relay out.
	// Default DefaultConnectRatePerMinute; negative removes the limit.
	ConnectRatePerMinute int
	// MonthlyBytes caps bytes copied in both directions per relay per calendar
	// month (UTC). Default DefaultMonthlyBytes; negative means unlimited.
	MonthlyBytes int64
	// QuotaWarnPercent is the percentage of MonthlyBytes at which the relay
	// receives a single advisory quota_warning notice.
	// Default DefaultQuotaWarnPercent; negative disables the warning.
	QuotaWarnPercent int
	// IdleTimeout closes a phone connection that has carried no traffic for
	// this long. Relay links are governed by ping/pong instead.
	// Default DefaultIdleTimeout; negative disables idle reaping.
	IdleTimeout time.Duration
	// StatePath, when set, is a JSON file holding relayed-byte counters only.
	// It is loaded at start and rewritten atomically every persistInterval and
	// on Close.
	StatePath string
	// STUNAddr, when set, is the UDP address of the built-in address-discovery
	// listener that lets both peers learn their mapped address. Empty disables
	// it, which is why this field has no default: a test or an embedder must
	// not bind a well-known UDP port by accident. The gateway binary supplies
	// the documented default (HERDR_GATEWAY_STUN_ADDR, ":3478").
	STUNAddr string
	// TrustProxyHeaders makes the gateway believe the leftmost X-Forwarded-For
	// entry. Enable it only when a trusted reverse proxy terminates TLS.
	TrustProxyHeaders bool
	// Version and Revision identify the gateway build in /healthz and hello
	// messages. They are operational metadata, not protocol negotiation: Proto
	// remains the compatibility contract.
	Version  string
	Revision string
	// Logger receives structured transport events. Defaults to a discarding
	// logger.
	Logger *slog.Logger
	// Now is the clock used for rate limits, quota months, and uptime. It
	// defaults to time.Now and exists for tests; I/O deadlines always use the
	// real clock.
	Now func() time.Time
}

// Server is the blind gateway. It is safe for concurrent use and serves every
// route from a single http.Handler.
type Server struct {
	opts    Options
	logger  *slog.Logger
	now     func() time.Time
	started time.Time
	mux     *http.ServeMux

	connectLimiter *rateLimiter
	probeLimiter   *rateLimiter
	quota          *quotaStore

	// stunLimiter and stunPort belong to the address-discovery listener.
	// stunPort is the port it actually bound, or 0 when discovery is disabled;
	// both are written by New before the Server is shared and never change.
	stunLimiter *stunLimiter
	stunPort    int

	// clients is the number of phone connections attached across every relay.
	// It is maintained on attach and detach so the global ceiling costs one
	// atomic per connection instead of a walk over every link.
	clients atomic.Int64

	mu     sync.Mutex
	relays map[string]*relayLink

	ctx       context.Context
	cancel    context.CancelFunc
	persistWG sync.WaitGroup
	stunWG    sync.WaitGroup
	closeOnce sync.Once
	closeErr  error
}

// New validates opts, applies defaults, loads persisted counters, and returns a
// ready Server. The caller serves Handler and must call Close on shutdown.
func New(opts Options) (*Server, error) {
	if opts.QuotaWarnPercent > 100 {
		return nil, fmt.Errorf("gateway: quota warn percent %d exceeds 100", opts.QuotaWarnPercent)
	}
	if opts.MaxRelays == 0 {
		opts.MaxRelays = DefaultMaxRelays
	}
	if opts.MaxClients == 0 {
		opts.MaxClients = DefaultMaxClients
	}
	if opts.MaxClientsPerRelay == 0 {
		opts.MaxClientsPerRelay = DefaultMaxClientsPerRelay
	}
	if opts.ConnectRatePerMinute == 0 {
		opts.ConnectRatePerMinute = DefaultConnectRatePerMinute
	}
	if opts.MonthlyBytes == 0 {
		opts.MonthlyBytes = DefaultMonthlyBytes
	}
	if opts.QuotaWarnPercent == 0 {
		opts.QuotaWarnPercent = DefaultQuotaWarnPercent
	}
	if opts.IdleTimeout == 0 {
		opts.IdleTimeout = DefaultIdleTimeout
	}
	if opts.Logger == nil {
		opts.Logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}

	ctx, cancel := context.WithCancel(context.Background())
	s := &Server{
		opts:           opts,
		logger:         opts.Logger,
		now:            opts.Now,
		started:        opts.Now(),
		mux:            http.NewServeMux(),
		connectLimiter: newRateLimiter(opts.ConnectRatePerMinute, time.Minute),
		probeLimiter:   newRateLimiter(1, probeInterval),
		quota:          newQuotaStore(opts.StatePath, opts.MonthlyBytes, opts.QuotaWarnPercent, opts.Logger),
		relays:         make(map[string]*relayLink),
		ctx:            ctx,
		cancel:         cancel,
	}
	if err := s.quota.load(s.now()); err != nil {
		cancel()
		return nil, err
	}

	s.mux.HandleFunc("GET /relay", s.handleRelay)
	s.mux.HandleFunc("GET /connect", s.handleConnect)
	s.mux.HandleFunc("GET /healthz", s.handleHealthz)
	s.mux.HandleFunc("GET /whoami", s.handleWhoami)
	s.mux.HandleFunc("POST /probe", s.handleProbe)

	if opts.STUNAddr != "" {
		if err := s.startSTUN(opts.STUNAddr); err != nil {
			cancel()
			return nil, err
		}
	}

	if opts.StatePath != "" {
		s.persistWG.Add(1)
		go s.runPersist()
	}
	return s, nil
}

// Handler returns the gateway routes: /relay, /connect, /healthz, /whoami and
// /probe.
func (s *Server) Handler() http.Handler { return s.mux }

// Close drops every relay link and its phone connections, then flushes the
// relayed-byte counters. It is idempotent.
func (s *Server) Close() error {
	s.closeOnce.Do(func() {
		s.cancel()

		s.mu.Lock()
		links := make([]*relayLink, 0, len(s.relays))
		for _, link := range s.relays {
			links = append(links, link)
		}
		s.relays = make(map[string]*relayLink)
		s.mu.Unlock()

		for _, link := range links {
			link.close(websocket.StatusGoingAway, reasonGatewayShutdown)
		}
		s.persistWG.Wait()
		s.stunWG.Wait()
		s.closeErr = s.quota.save(s.now())
	})
	return s.closeErr
}

// stopped reports whether Close has been called.
func (s *Server) stopped() bool {
	select {
	case <-s.ctx.Done():
		return true
	default:
		return false
	}
}

// runPersist rewrites the counter file periodically until the server closes.
func (s *Server) runPersist() {
	defer s.persistWG.Done()
	ticker := time.NewTicker(persistInterval)
	defer ticker.Stop()
	for {
		select {
		case <-s.ctx.Done():
			return
		case <-ticker.C:
			now := s.now()
			s.quota.prune(now)
			if err := s.quota.save(now); err != nil {
				s.logger.Warn("gateway state save failed", "error", err)
			}
		}
	}
}

// idleSweepInterval is how often a relay link reaps its idle phone
// connections. Zero means idle reaping is disabled.
func (s *Server) idleSweepInterval() time.Duration {
	if s.opts.IdleTimeout <= 0 {
		return 0
	}
	interval := s.opts.IdleTimeout / 2
	if interval > maxIdleSweepInterval {
		interval = maxIdleSweepInterval
	}
	if interval < time.Second {
		interval = time.Second
	}
	return interval
}

type healthResponse struct {
	OK            bool   `json:"ok"`
	Relays        int    `json:"relays"`
	Clients       int    `json:"clients"`
	UptimeSeconds int64  `json:"uptime_seconds"`
	Protocol      int    `json:"protocol"`
	Version       string `json:"version"`
	Revision      string `json:"revision"`
	// STUNPort tells an operator whether address discovery is live, and on
	// which port. It is the same number both hellos advertise; 0 means
	// disabled. Like every other field here it is a count or a setting, never
	// an address, a relay id, or a byte total.
	STUNPort int `json:"stun_port"`
}

func (s *Server) handleHealthz(w http.ResponseWriter, _ *http.Request) {
	relays, clients := s.counts()
	writeJSONResponse(w, http.StatusOK, healthResponse{
		OK:            true,
		Relays:        relays,
		Clients:       clients,
		UptimeSeconds: int64(s.now().Sub(s.started).Seconds()),
		Protocol:      gatewaywire.Proto,
		Version:       s.opts.Version,
		Revision:      s.opts.Revision,
		STUNPort:      s.stunPort,
	})
}

// counts reports what /healthz publishes. Both numbers come from counters the
// attach and detach paths already maintain, so a health check never walks the
// registration table or takes a per-link lock.
func (s *Server) counts() (relays, clients int) {
	s.mu.Lock()
	relays = len(s.relays)
	s.mu.Unlock()
	return relays, int(s.clients.Load())
}

// reserveClient claims one slot against the global client ceiling, reporting
// whether the connection may proceed. The slot is held until releaseClients
// gives it back, so a caller that fails afterwards must return it.
func (s *Server) reserveClient() bool {
	limit := int64(s.opts.MaxClients)
	if limit < 0 {
		s.clients.Add(1)
		return true
	}
	// Compare and swap rather than add-then-check: an add would let a burst of
	// simultaneous connections push the counter past the ceiling before any of
	// them noticed.
	for {
		current := s.clients.Load()
		if current >= limit {
			return false
		}
		if s.clients.CompareAndSwap(current, current+1) {
			return true
		}
	}
}

// releaseClients returns n slots to the global ceiling.
func (s *Server) releaseClients(n int) {
	if n > 0 {
		s.clients.Add(-int64(n))
	}
}

type whoamiResponse struct {
	IP string `json:"ip"`
}

func (s *Server) handleWhoami(w http.ResponseWriter, r *http.Request) {
	writeJSONResponse(w, http.StatusOK, whoamiResponse{IP: s.clientIP(r)})
}

// clientIP is the address the gateway attributes this request to. Behind a
// trusted proxy the leftmost X-Forwarded-For entry wins, because that is the
// only place the real client address survives.
func (s *Server) clientIP(r *http.Request) string {
	if s.opts.TrustProxyHeaders {
		if forwarded := r.Header.Get("X-Forwarded-For"); forwarded != "" {
			first := strings.TrimSpace(forwarded)
			if comma := strings.IndexByte(first, ','); comma >= 0 {
				first = strings.TrimSpace(first[:comma])
			}
			if first != "" {
				return first
			}
		}
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// lookupRelay returns the live registration for a relay id, or nil.
func (s *Server) lookupRelay(relayID string) *relayLink {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.relays[relayID]
}

// acceptOptions are shared by both WebSocket routes. Payloads are ciphertext:
// compression buys nothing and leaks length correlations, and the gateway has
// no origin to check because every client is a native WebSocket dialer or a
// PWA on an origin the gateway does not know.
func acceptOptions() *websocket.AcceptOptions {
	return &websocket.AcceptOptions{
		InsecureSkipVerify: true,
		CompressionMode:    websocket.CompressionDisabled,
	}
}

// randomNonce returns a fresh base64url challenge and its raw bytes.
func randomNonce() (string, error) {
	nonce := make([]byte, gatewaywire.NonceBytes)
	if _, err := rand.Read(nonce); err != nil {
		return "", fmt.Errorf("gateway: read nonce: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(nonce), nil
}

// writeJSON sends one JSON control message as a WebSocket text frame.
func writeJSON(ctx context.Context, conn *websocket.Conn, value any) error {
	data, err := json.Marshal(value)
	if err != nil {
		return fmt.Errorf("gateway: encode control message: %w", err)
	}
	ctx, cancel := context.WithTimeout(ctx, writeTimeout)
	defer cancel()
	return conn.Write(ctx, websocket.MessageText, data)
}

// readHello reads one bounded JSON text frame within timeout.
func readHello(ctx context.Context, conn *websocket.Conn, timeout time.Duration) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	messageType, data, err := conn.Read(ctx)
	if err != nil {
		return nil, err
	}
	if messageType != websocket.MessageText {
		return nil, fmt.Errorf("gateway: hello must be a text frame")
	}
	if len(data) > gatewaywire.MaxHelloBytes {
		return nil, fmt.Errorf("gateway: hello of %d bytes exceeds %d", len(data), gatewaywire.MaxHelloBytes)
	}
	return data, nil
}

// reject reports a coded refusal on a connection that never reached framing and
// then closes it. The close reason repeats the code so a client that misses the
// text frame still learns why.
func reject(ctx context.Context, conn *websocket.Conn, code, message string) {
	_ = writeJSON(ctx, conn, gatewaywire.ErrorMessage{
		Type:    gatewaywire.TypeError,
		Code:    code,
		Message: message,
	})
	_ = conn.Close(websocket.StatusPolicyViolation, code)
}

// validProof reports whether a hello proof is shaped like an HMAC-SHA256 tag.
// The gateway cannot verify it — only the relay can — but forwarding malformed
// input would just waste a relay round trip.
func validProof(proof string) bool {
	raw, err := base64.RawURLEncoding.DecodeString(proof)
	return err == nil && len(raw) == proofBytes
}

// proofBytes is the length of the HMAC-SHA256 tag carried in ConnectHello.
const proofBytes = 32

// sanitizeReason makes an untrusted close reason safe to hand to a WebSocket
// close frame: valid UTF-8, bounded length, no control characters.
func sanitizeReason(payload []byte) string {
	if len(payload) > gatewaywire.MaxCloseReason {
		payload = payload[:gatewaywire.MaxCloseReason]
	}
	if !utf8.Valid(payload) {
		return ""
	}
	reason := strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f {
			return -1
		}
		return r
	}, string(payload))
	return reason
}

// shortID truncates a relay id for logs so no full rendezvous identifier ever
// reaches disk.
func shortID(relayID string) string {
	if len(relayID) <= relayIDLogPrefix {
		return relayID
	}
	return relayID[:relayIDLogPrefix]
}

func writeJSONResponse(w http.ResponseWriter, status int, value any) {
	data, err := json.Marshal(value)
	if err != nil {
		http.Error(w, "encoding failed", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_, _ = w.Write(data)
}
