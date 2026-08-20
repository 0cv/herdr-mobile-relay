package transport

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"math/rand/v2"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/coder/websocket"

	"github.com/0cv/herdr-mobile-relay/internal/config"
	"github.com/0cv/herdr-mobile-relay/internal/framing"
	"github.com/0cv/herdr-mobile-relay/internal/gatewaywire"
)

const (
	// gatewayRelayPath is the multiplexed registration endpoint on a gateway.
	gatewayRelayPath  = "/relay"
	gatewayHealthPath = "/healthz"

	gatewayDialTimeout  = 15 * time.Second
	gatewayHelloTimeout = 10 * time.Second
	// A registered WebSocket can survive while the gateway's public proxy stops
	// routing new phone connections. Probe the public path independently and
	// fail over after two consecutive misses. At 15 seconds this catches the
	// split failure within 30 seconds without turning health checks into traffic.
	gatewayHealthInterval     = 15 * time.Second
	gatewayHealthFailureLimit = 2
	gatewayProbeTimeout       = 2 * time.Second
	gatewayProbeTieWindow     = 20 * time.Millisecond
	gatewayReadLimit          = int64(gatewaywire.HeaderSize + gatewaywire.MaxFramePayload)
	gatewayWriteQueue         = 64
	gatewayConnQueue          = 32
	gatewayDefaultMaxClients  = 8

	// gatewayStallSweep is how often a logical connection whose inbound frame
	// assembly stopped making progress is looked for.
	gatewayStallSweep = 5 * time.Second

	gatewayBackoffBase   = 1 * time.Second
	gatewayBackoffMax    = 60 * time.Second
	gatewayBackoffJitter = 0.2
)

// Close reasons the relay attaches to OpClose. They are diagnostic strings for
// the gateway and the phone; they never carry key material.
const (
	gatewayReasonUnauthorized = "unauthorized"
	gatewayReasonBusy         = "busy"
	gatewayReasonSlow         = "slow"
	gatewayReasonUnknownConn  = "unknown"
	// gatewayReasonFraming is deliberately static: a chunk framing violation
	// must not leak anything derived from frame bytes to the blind gateway.
	gatewayReasonFraming = "framing"
)

// errGatewayWriteQueueFull reports that the shared link writer is saturated.
// The offending logical connection is dropped instead of stalling every other
// connection multiplexed onto the same link.
var errGatewayWriteQueueFull = errors.New("gateway write queue is full")

// GatewayStatus is a serializable snapshot of the gateway registration. The
// relay id is derived one-way from the relay key and is safe to display; the
// rendezvous key is never exposed.
type GatewayStatus struct {
	Enabled        bool      `json:"enabled"`
	Registered     bool      `json:"registered"`
	RelayID        string    `json:"relay_id"`
	URL            string    `json:"url"`
	URLs           []string  `json:"urls"`
	Version        string    `json:"version"`
	Revision       string    `json:"revision"`
	Clients        int       `json:"clients"`
	LastError      string    `json:"last_error"`
	LastNotice     string    `json:"last_notice"`
	ConnectedSince time.Time `json:"connected_since"`
}

// GatewayOptions configures the outbound registration.
type GatewayOptions struct {
	// URL is the gateway base, for example wss://gw.example.com. It is the
	// first entry of the candidate list, and must be the first element of URLs
	// when both are set.
	URL string
	// URLs is the gateway candidate list. At startup the relay probes every
	// entry concurrently and registers with one healthy entry; Selection
	// decides which, and list order remains the fallback when probes fail.
	// URL stays for single-gateway callers.
	URLs []string
	// Selection is config.GatewaySelectionOrdered or
	// config.GatewaySelectionLatency. Empty means ordered: a caller that did
	// not think about the rule gets the configured list honoured.
	Selection string
	// RelayKey is the relay's pairing secret. Both gateway identifiers are
	// derived from it; the key itself never leaves the process.
	RelayKey string
	// MaxClients bounds the phone connections accepted through the gateway.
	MaxClients int
	Logger     *slog.Logger
}

// GatewayClient maintains one outbound WebSocket registration to a blind
// gateway and demultiplexes the phone connections it forwards into ordinary
// hub clients. The gateway only ever sees opaque frames: authentication of a
// phone happens here, against the locally derived rendezvous key.
type GatewayClient struct {
	hub *Hub
	// urls is the configured order; active indexes the selected entry and is
	// guarded by mu because cold selection runs beside status readers.
	urls []string
	// selection is the rule for picking among healthy candidates. Anything but
	// config.GatewaySelectionLatency is ordered.
	selection     string
	relayID       string
	rendezvousKey []byte
	maxClients    int
	logger        *slog.Logger

	backoffBase     time.Duration
	backoffMax      time.Duration
	probeTimeout    time.Duration
	healthInterval  time.Duration
	probe           func(context.Context, string) (time.Duration, error)
	mu              sync.Mutex
	active          int
	conns           map[uint32]*gatewayConn
	registered      bool
	lastError       string
	lastNotice      string
	connectedSince  time.Time
	gatewayVersion  string
	gatewayRevision string
	// stunPort is the address-discovery port the gateway advertised in its
	// hello. Only the port is kept: the host always comes from the URL this
	// relay dialed, so a gateway can never point discovery at a third party.
	stunPort int
}

// gatewayRejectedError reports an explicit refusal from the gateway. It is a
// configuration or capacity problem rather than a transient network fault, so
// it is logged loudly even though the client keeps retrying.
type gatewayRejectedError struct {
	code    string
	message string
}

func (e *gatewayRejectedError) Error() string {
	return fmt.Sprintf("gateway rejected registration: %s (%s)", e.code, e.message)
}

// NewGatewayClient validates the gateway configuration and derives the relay
// identifiers. It fails fast so a misconfigured relay never starts a retry
// loop that can not succeed.
func NewGatewayClient(hub *Hub, opts GatewayOptions) (*GatewayClient, error) {
	if hub == nil {
		return nil, errors.New("gateway client requires a hub")
	}
	urls, err := gatewayURLs(opts)
	if err != nil {
		return nil, err
	}
	if opts.RelayKey == "" {
		return nil, errors.New("gateway registration requires a relay key")
	}
	relayID, err := gatewaywire.DeriveRelayID(opts.RelayKey)
	if err != nil {
		return nil, err
	}
	rendezvousKey, err := gatewaywire.DeriveRendezvousKey(opts.RelayKey)
	if err != nil {
		return nil, err
	}
	maxClients := opts.MaxClients
	if maxClients <= 0 {
		maxClients = gatewayDefaultMaxClients
	}
	logger := opts.Logger
	if logger == nil {
		logger = slog.Default()
	}
	return &GatewayClient{
		hub:            hub,
		urls:           urls,
		selection:      opts.Selection,
		relayID:        relayID,
		rendezvousKey:  rendezvousKey,
		maxClients:     maxClients,
		logger:         logger,
		backoffBase:    gatewayBackoffBase,
		backoffMax:     gatewayBackoffMax,
		probeTimeout:   gatewayProbeTimeout,
		healthInterval: gatewayHealthInterval,
		probe:          measureGatewayRTT,
		conns:          make(map[uint32]*gatewayConn),
	}, nil
}

// gatewayURLs normalizes the configured endpoints into a validated candidate
// list. URLs wins over URL so a caller that fills both can never end up dialing
// an endpoint the list does not name, and the two must agree on the first
// candidate.
func gatewayURLs(opts GatewayOptions) ([]string, error) {
	primary := strings.TrimRight(strings.TrimSpace(opts.URL), "/")
	list := make([]string, 0, len(opts.URLs))
	for _, raw := range opts.URLs {
		if entry := strings.TrimRight(strings.TrimSpace(raw), "/"); entry != "" {
			list = append(list, entry)
		}
	}
	if len(list) == 0 {
		if primary == "" {
			return nil, errors.New("gateway url is required")
		}
		list = append(list, primary)
	} else if primary != "" && list[0] != primary {
		return nil, fmt.Errorf("gateway url %q must be the first entry of the gateway url list, got %q",
			primary, list[0])
	}
	for _, entry := range list {
		parsed, err := url.Parse(entry)
		if err != nil {
			return nil, fmt.Errorf("invalid gateway url %q: %w", entry, err)
		}
		if parsed.Host == "" || (parsed.Scheme != "ws" && parsed.Scheme != "wss") {
			return nil, fmt.Errorf("invalid gateway url %q: want ws:// or wss:// base url", entry)
		}
	}
	return list, nil
}

// CurrentURL reports the gateway entry the client is registered with, or the one
// its next attempt will dial. Status output and address discovery follow it, so
// a failover is visible instead of pointing at a gateway we left behind.
func (c *GatewayClient) CurrentURL() string {
	if c == nil {
		return ""
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.urls[c.active]
}

// advance rotates to the next configured entry, wrapping at the end, so a dead
// gateway does not absorb every reconnect attempt.
func (c *GatewayClient) advance() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.active = (c.active + 1) % len(c.urls)
}

// activeFirstLocked returns the selected gateway followed by every cold
// fallback in configured order. The active entry leads the descriptor the
// phone stores, so it does not waste a failed connection on the next resume.
func (c *GatewayClient) activeFirstLocked() []string {
	ordered := make([]string, 0, len(c.urls))
	ordered = append(ordered, c.urls[c.active])
	for index, entry := range c.urls {
		if index != c.active {
			ordered = append(ordered, entry)
		}
	}
	return ordered
}

func (c *GatewayClient) activeIndex() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.active
}

func (c *GatewayClient) setActive(index int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.active = index
}

type gatewayProbeResult struct {
	index int
	rtt   time.Duration
	err   error
}

// latencyRanked reports whether measured round trip decides the winner. Only
// an interchangeable list, such as the community gateways, opts into it.
func (c *GatewayClient) latencyRanked() bool {
	return c.selection == config.GatewaySelectionLatency
}

// Selection reports the rule in force, so status output can answer "why that
// gateway" without the log. A nil client has no gateway to pick.
func (c *GatewayClient) Selection() string {
	if c == nil {
		return ""
	}
	if c.latencyRanked() {
		return config.GatewaySelectionLatency
	}
	return config.GatewaySelectionOrdered
}

// selectGateway probes every candidate concurrently and commits to one healthy
// entry. A failed active entry may be excluded for one selection pass; this
// prevents a working /healthz route from immediately winning after its
// WebSocket registration path failed. Probing is identical in both selection
// modes: ordered still has to learn which entries answer at all.
func (c *GatewayClient) selectGateway(parent context.Context, excluded int) bool {
	candidates := make([]int, 0, len(c.urls))
	for index := range c.urls {
		if index != excluded {
			candidates = append(candidates, index)
		}
	}
	if len(candidates) == 0 {
		return false
	}
	if len(candidates) == 1 {
		c.setActive(candidates[0])
		return true
	}

	ctx, cancel := context.WithTimeout(parent, c.probeTimeout)
	defer cancel()
	results := make(chan gatewayProbeResult, len(candidates))
	for _, index := range candidates {
		go func() {
			rtt, err := c.probe(ctx, c.urls[index])
			results <- gatewayProbeResult{index: index, rtt: rtt, err: err}
		}()
	}

	successful := make([]gatewayProbeResult, 0, len(candidates))
	for range candidates {
		select {
		case result := <-results:
			if result.err != nil {
				c.logger.Debug("gateway probe failed",
					"url", c.urls[result.index], "error", result.err)
				continue
			}
			successful = append(successful, result)
		case <-ctx.Done():
			return c.applyProbeResults(successful)
		}
	}
	return c.applyProbeResults(successful)
}

// applyProbeResults commits the winner among the healthy candidates. This is
// the only step the selection rule changes.
func (c *GatewayClient) applyProbeResults(results []gatewayProbeResult) bool {
	if len(results) == 0 {
		return false
	}
	if !c.latencyRanked() {
		return c.applyConfiguredOrder(results)
	}
	minRTT := results[0].rtt
	for _, result := range results[1:] {
		if result.rtt < minRTT {
			minRTT = result.rtt
		}
	}
	bestIndex := len(c.urls)
	var bestRTT time.Duration
	for _, result := range results {
		if result.rtt-minRTT <= gatewayProbeTieWindow && result.index < bestIndex {
			bestIndex, bestRTT = result.index, result.rtt
		}
	}
	c.setActive(bestIndex)
	c.logger.Info("gateway selected by latency", "url", c.urls[bestIndex], "rtt", bestRTT)
	return true
}

// applyConfiguredOrder takes the earliest healthy entry, however slow it is: an
// explicitly listed gateway is a priority, not a suggestion. The log names the
// position it landed on and how many entries answered, so "why is it not on my
// first gateway" is answerable from the log alone.
func (c *GatewayClient) applyConfiguredOrder(results []gatewayProbeResult) bool {
	best := results[0]
	for _, result := range results[1:] {
		if result.index < best.index {
			best = result
		}
	}
	c.setActive(best.index)
	c.logger.Info("gateway selected by configured order",
		"url", c.urls[best.index], "position", best.index+1, "candidates", len(c.urls),
		"healthy", len(results), "rtt", best.rtt)
	return true
}

// measureGatewayRTT performs one fresh request to the gateway's real health
// endpoint. This needs no ICMP privilege and includes DNS, TCP, TLS and HTTP in
// the number used for cold selection.
func measureGatewayRTT(ctx context.Context, base string) (time.Duration, error) {
	target, err := url.Parse(base)
	if err != nil {
		return 0, err
	}
	switch target.Scheme {
	case "ws":
		target.Scheme = "http"
	case "wss":
		target.Scheme = "https"
	default:
		return 0, fmt.Errorf("unsupported gateway scheme %q", target.Scheme)
	}
	target.Path = gatewayHealthPath
	target.RawPath, target.RawQuery, target.Fragment = "", "", ""

	transport := &http.Transport{
		Proxy:             http.ProxyFromEnvironment,
		DisableKeepAlives: true,
		ForceAttemptHTTP2: true,
	}
	defer transport.CloseIdleConnections()
	client := &http.Client{
		Transport: transport,
		CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, target.String(), nil)
	if err != nil {
		return 0, err
	}
	started := time.Now()
	response, err := client.Do(request)
	if err != nil {
		return 0, err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return 0, fmt.Errorf("health returned HTTP %d", response.StatusCode)
	}
	var health struct {
		OK bool `json:"ok"`
	}
	if err := json.NewDecoder(io.LimitReader(response.Body, 4096)).Decode(&health); err != nil {
		return 0, fmt.Errorf("decode health response: %w", err)
	}
	if !health.OK {
		return 0, errors.New("health response is not ok")
	}
	return time.Since(started), nil
}

// RelayID returns the public rendezvous identifier the phone pairs against.
func (c *GatewayClient) RelayID() string { return c.relayID }

// STUNPort reports the address-discovery port the gateway last advertised, or
// 0 when discovery is unavailable. The caller pairs it with the gateway host
// it configured; the gateway never supplies a host.
func (c *GatewayClient) STUNPort() int {
	if c == nil {
		return 0
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.stunPort
}

// Status snapshots the registration for relay status output. A nil client
// reports a disabled gateway, so callers can hold an unset gateway.
func (c *GatewayClient) Status() GatewayStatus {
	if c == nil {
		return GatewayStatus{}
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	ordered := c.activeFirstLocked()
	return GatewayStatus{
		Enabled:        true,
		Registered:     c.registered,
		RelayID:        c.relayID,
		URL:            ordered[0],
		URLs:           ordered,
		Version:        c.gatewayVersion,
		Revision:       c.gatewayRevision,
		Clients:        len(c.conns),
		LastError:      c.lastError,
		LastNotice:     c.lastNotice,
		ConnectedSince: c.connectedSince,
	}
}

// Run keeps exactly one registration alive until ctx is cancelled, reconnecting
// with jittered exponential backoff. It blocks, matching the relay's other
// background services.
func (c *GatewayClient) Run(ctx context.Context) {
	delay := c.backoffBase
	if len(c.urls) > 1 {
		c.selectGateway(ctx, -1)
	}
	for {
		if ctx.Err() != nil {
			return
		}
		failedIndex := c.activeIndex()
		registered, err := c.runSession(ctx)
		if ctx.Err() != nil {
			return
		}
		if err != nil {
			c.recordError(err)
			var rejected *gatewayRejectedError
			if errors.As(err, &rejected) {
				c.logger.Error("gateway rejected relay registration",
					"code", rejected.code, "message", rejected.message)
			} else {
				c.logger.Warn("gateway link failed", "error", err)
			}
			// The entry that just failed is excluded for this pass, so failover
			// lands on the next healthy entry the selection rule prefers rather
			// than back on the gateway that just dropped us.
			if !c.selectGateway(ctx, failedIndex) {
				c.advance()
			}
		}
		if registered {
			delay = c.backoffBase
		}
		if !sleepJittered(ctx, delay) {
			return
		}
		delay = c.nextBackoff(delay)
	}
}

// nextBackoff doubles the reconnect interval up to the configured ceiling.
func (c *GatewayClient) nextBackoff(delay time.Duration) time.Duration {
	return min(delay*2, c.backoffMax)
}

// runSession owns one link from dial to teardown. It reports whether the link
// reached the registered state, which resets the reconnect backoff.
func (c *GatewayClient) runSession(ctx context.Context) (bool, error) {
	base := c.CurrentURL()
	dialCtx, cancelDial := context.WithTimeout(ctx, gatewayDialTimeout)
	conn, _, err := websocket.Dial(dialCtx, base+gatewayRelayPath, &websocket.DialOptions{
		CompressionMode: websocket.CompressionDisabled,
	})
	cancelDial()
	if err != nil {
		return false, fmt.Errorf("dial gateway: %w", err)
	}
	conn.SetReadLimit(gatewayReadLimit)

	if err := c.registerLink(ctx, conn); err != nil {
		conn.CloseNow()
		return false, err
	}
	c.markRegistered()
	c.logger.Info("gateway registered", "url", base, "relay_id", c.relayID)

	sessionCtx, cancelSession := context.WithCancel(ctx)
	session := &gatewaySession{
		client:     c,
		conn:       conn,
		ctx:        sessionCtx,
		cancel:     cancelSession,
		writes:     make(chan []byte, gatewayWriteQueue),
		done:       make(chan struct{}),
		writerDone: make(chan struct{}),
	}
	go session.writeLoop()
	session.serveWG.Add(2)
	go session.sweepLoop()
	go session.healthLoop(base)

	readErr := session.readLoop()
	session.stop()
	<-session.writerDone
	// Evicting first unblocks every hub connection, then the serve goroutines
	// drain before the link is reported down.
	c.evictAll()
	session.serveWG.Wait()
	c.markUnregistered()

	// CloseNow rather than a close handshake: the peer already went away in
	// every failure path, and a handshake can stall teardown for seconds. The
	// gateway sees the EOF immediately either way.
	conn.CloseNow()
	if ctx.Err() != nil {
		return true, nil
	}
	if failure := session.failure(); failure != nil {
		return true, failure
	}
	return true, readErr
}

// registerLink performs the JSON hello exchange that claims the relay id.
func (c *GatewayClient) registerLink(parent context.Context, conn *websocket.Conn) error {
	ctx, cancel := context.WithTimeout(parent, gatewayHelloTimeout)
	defer cancel()

	messageType, rawHello, err := conn.Read(ctx)
	if err != nil {
		return fmt.Errorf("read gateway hello: %w", err)
	}
	if messageType != websocket.MessageText {
		return errors.New("gateway hello was not a text frame")
	}
	var hello gatewaywire.ServerHello
	if err := json.Unmarshal(rawHello, &hello); err != nil {
		return fmt.Errorf("decode gateway hello: %w", err)
	}
	if hello.Type != gatewaywire.TypeServerHello {
		return fmt.Errorf("unexpected gateway hello type %q", hello.Type)
	}
	if hello.Proto != gatewaywire.Proto {
		return fmt.Errorf("unsupported gateway protocol %d, want %d", hello.Proto, gatewaywire.Proto)
	}
	c.setGatewayBuild(hello.Version, hello.Revision)
	c.setSTUNPort(hello.StunPort)

	register, err := json.Marshal(gatewaywire.RegisterHello{
		Type:    gatewaywire.TypeRegister,
		Proto:   gatewaywire.Proto,
		RelayID: c.relayID,
	})
	if err != nil {
		return fmt.Errorf("encode register hello: %w", err)
	}
	if err := conn.Write(ctx, websocket.MessageText, register); err != nil {
		return fmt.Errorf("write register hello: %w", err)
	}

	messageType, rawReply, err := conn.Read(ctx)
	if err != nil {
		return fmt.Errorf("read gateway registration reply: %w", err)
	}
	if messageType != websocket.MessageText {
		return errors.New("gateway registration reply was not a text frame")
	}
	var reply struct {
		Type string `json:"type"`
	}
	if err := json.Unmarshal(rawReply, &reply); err != nil {
		return fmt.Errorf("decode gateway registration reply: %w", err)
	}
	switch reply.Type {
	case gatewaywire.TypeReady:
		var ready gatewaywire.ReadyMessage
		if err := json.Unmarshal(rawReply, &ready); err != nil {
			return fmt.Errorf("decode gateway ready: %w", err)
		}
		if ready.Proto != gatewaywire.Proto {
			return fmt.Errorf("unsupported gateway protocol %d, want %d", ready.Proto, gatewaywire.Proto)
		}
		return nil
	case gatewaywire.TypeError:
		var rejection gatewaywire.ErrorMessage
		if err := json.Unmarshal(rawReply, &rejection); err != nil {
			return fmt.Errorf("decode gateway error: %w", err)
		}
		return &gatewayRejectedError{code: rejection.Code, message: rejection.Message}
	default:
		return fmt.Errorf("unexpected gateway registration reply %q", reply.Type)
	}
}

func (c *GatewayClient) setGatewayBuild(version, revision string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.gatewayVersion = version
	c.gatewayRevision = revision
}

// setSTUNPort records an advertised address-discovery port. Anything outside
// the valid port range is a broken or hostile gateway, and disables discovery
// rather than being passed on.
func (c *GatewayClient) setSTUNPort(port int) {
	if port < 1 || port > 65535 {
		port = 0
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.stunPort = port
}

func (c *GatewayClient) markRegistered() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.registered = true
	c.lastError = ""
	c.connectedSince = time.Now().UTC()
}

func (c *GatewayClient) markUnregistered() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.registered = false
	c.connectedSince = time.Time{}
}

func (c *GatewayClient) recordError(err error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.lastError = err.Error()
}

func (c *GatewayClient) recordNotice(message string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.lastNotice = message
}

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

// gatewaySession is one registered link. Every frame leaving the relay passes
// through a single writer goroutine, so per-connection sends can never
// interleave halves of a multiplexed frame.
type gatewaySession struct {
	client     *GatewayClient
	conn       *websocket.Conn
	ctx        context.Context
	cancel     context.CancelFunc
	writes     chan []byte
	done       chan struct{}
	writerDone chan struct{}
	stopOnce   sync.Once
	serveWG    sync.WaitGroup

	failOnce sync.Once
	failErr  error
}

// stop ends the session: queued frames are abandoned and every reader unblocks.
func (s *gatewaySession) stop() {
	s.stopOnce.Do(func() {
		close(s.done)
		s.cancel()
	})
}

// fail records the first fault and tears the session down.
func (s *gatewaySession) fail(err error) {
	s.failOnce.Do(func() { s.failErr = err })
	s.stop()
}

// failure reports the recorded fault. It is only safe once the read and write
// pumps have both finished.
func (s *gatewaySession) failure() error { return s.failErr }

// enqueue hands one complete multiplexed frame to the writer. It never blocks:
// a saturated queue means the link cannot keep up, and the caller drops its own
// logical connection rather than stalling the others.
func (s *gatewaySession) enqueue(frame []byte) error {
	select {
	case <-s.done:
		return ErrFrameConnClosed
	default:
	}
	select {
	case s.writes <- frame:
		return nil
	case <-s.done:
		return ErrFrameConnClosed
	default:
		return errGatewayWriteQueueFull
	}
}

func (s *gatewaySession) writeLoop() {
	defer close(s.writerDone)
	for {
		select {
		case <-s.done:
			return
		case frame := <-s.writes:
			ctx, cancel := context.WithTimeout(s.ctx, frameWriteTimeout)
			err := s.conn.Write(ctx, websocket.MessageBinary, frame)
			cancel()
			if err != nil {
				s.fail(fmt.Errorf("write gateway frame: %w", err))
				return
			}
		}
	}
}

// sweepLoop abandons logical connections whose inbound frame assembly stopped
// making progress, so a phone cannot pin a partially received frame on the
// relay by going quiet mid-sequence. It ends with the session.
func (s *gatewaySession) sweepLoop() {
	defer s.serveWG.Done()
	ticker := time.NewTicker(gatewayStallSweep)
	defer ticker.Stop()
	for {
		select {
		case <-s.done:
			return
		case now := <-ticker.C:
			s.client.sweepStalled(now)
		}
	}
}

// healthLoop verifies the same public route new phones use. The maintained
// /relay WebSocket can remain alive through Caddy while Docker DNS for new
// requests is broken; link ping/pong cannot detect that split failure.
func (s *gatewaySession) healthLoop(base string) {
	defer s.serveWG.Done()
	if s.client.healthInterval <= 0 {
		return
	}
	ticker := time.NewTicker(s.client.healthInterval)
	defer ticker.Stop()
	failures := 0
	for {
		select {
		case <-s.done:
			return
		case <-ticker.C:
			ctx, cancel := context.WithTimeout(s.ctx, s.client.probeTimeout)
			_, err := s.client.probe(ctx, base)
			cancel()
			if err == nil {
				failures = 0
				continue
			}
			failures++
			if failures < gatewayHealthFailureLimit {
				s.client.logger.Debug("gateway public health probe failed",
					"url", base, "error", err)
				continue
			}
			s.fail(fmt.Errorf("gateway public path failed %d consecutive health probes: %w",
				failures, err))
			return
		}
	}
}

// sweepStalled closes every logical connection whose inbound assembly stopped
// making progress before now.
func (c *GatewayClient) sweepStalled(now time.Time) {
	for connID, conn := range c.liveConns() {
		if !conn.stalled(now) {
			continue
		}
		c.logger.Debug("gateway connection evicted for a stalled frame assembly",
			"conn_id", connID)
		conn.pipe.Close(CloseGoingAway, gatewayReasonSlow)
	}
}

func (s *gatewaySession) readLoop() error {
	for {
		messageType, data, err := s.conn.Read(s.ctx)
		if err != nil {
			// After framing starts the gateway has no TEXT channel left for an
			// ErrorMessage, so a policy-violation close carries the refusal
			// (for example the relay id was claimed by a newer link).
			var closed websocket.CloseError
			if errors.As(err, &closed) && closed.Code == websocket.StatusPolicyViolation {
				return &gatewayRejectedError{code: gatewaywire.CodeRelayBusy, message: closed.Reason}
			}
			return fmt.Errorf("read gateway frame: %w", err)
		}
		if messageType != websocket.MessageBinary {
			return errors.New("gateway sent a non-binary frame on a registered link")
		}
		op, connID, payload, err := gatewaywire.DecodeFrame(data)
		if err != nil {
			return fmt.Errorf("decode gateway frame: %w", err)
		}
		switch op {
		case gatewaywire.OpData:
			s.handleData(connID, payload)
		case gatewaywire.OpOpen:
			s.handleOpen(connID, payload)
		case gatewaywire.OpClose:
			s.handleClose(connID)
		case gatewaywire.OpPing:
			if err := s.enqueue(gatewaywire.EncodeFrame(gatewaywire.OpPong, connID, payload)); err != nil {
				return fmt.Errorf("answer gateway ping: %w", err)
			}
		case gatewaywire.OpNotice:
			s.handleNotice(payload)
		}
	}
}

// handleOpen authenticates a forwarded phone connection and, on success, hands
// it to the hub as an ordinary client. The gateway cannot perform this check:
// only the relay and the paired phone know the rendezvous key.
func (s *gatewaySession) handleOpen(connID uint32, payload []byte) {
	var open gatewaywire.OpenPayload
	if err := json.Unmarshal(payload, &open); err != nil {
		s.reject(connID, gatewayReasonUnauthorized)
		s.client.logger.Debug("gateway open payload rejected", "conn_id", connID)
		return
	}
	nonce, nonceErr := base64.RawURLEncoding.DecodeString(open.Nonce)
	proof, proofErr := base64.RawURLEncoding.DecodeString(open.Proof)
	if nonceErr != nil || proofErr != nil ||
		!gatewaywire.VerifyConnectProof(s.client.rendezvousKey, s.client.relayID, nonce, proof) {
		s.reject(connID, gatewayReasonUnauthorized)
		s.client.logger.Debug("gateway connect proof rejected", "conn_id", connID)
		return
	}

	var conn *gatewayConn
	// The ctx is unused: enqueue never blocks, and the shared writer bounds the
	// actual socket write with frameWriteTimeout.
	send := func(_ context.Context, frame []byte) error {
		// Reachable only for a frame the hub built above the logical cap every
		// transport shares; the gateway's own per-frame cap is what the chunks
		// below stay under.
		if len(frame) > framing.MaxLogicalBytes {
			return fmt.Errorf("frame of %d bytes exceeds the logical frame limit of %d",
				len(frame), framing.MaxLogicalBytes)
		}
		// The chunks of one logical frame must reach the single writer
		// goroutine contiguously and in order: the peer reassembles START..END
		// per connection and rejects anything else.
		conn.sendMu.Lock()
		defer conn.sendMu.Unlock()
		conn.chunks = framing.Chunk(conn.chunks[:0], frame, framing.GatewayChunkSize)
		for _, part := range conn.chunks {
			// EncodeFrame copies the chunk, so the shared chunk buffer is free
			// to be reused by the next send once every part is queued.
			err := s.enqueue(gatewaywire.EncodeFrame(gatewaywire.OpData, connID, part))
			if err == nil {
				continue
			}
			if errors.Is(err, errGatewayWriteQueueFull) {
				// The queue is full, so OpClose cannot be sent either; drop the
				// connection locally and let the gateway observe the eviction.
				// Dropping it also keeps the truncated chunk sequence from ever
				// being mistaken for a complete frame.
				s.client.removeConn(connID, conn)
				conn.pipe.Shutdown()
			}
			return err
		}
		return nil
	}
	closeConn := func(_ CloseStatus, reason string) {
		s.client.removeConn(connID, conn)
		_ = s.enqueue(gatewaywire.EncodeFrame(gatewaywire.OpClose, connID, closeReasonBytes(reason)))
	}
	conn = &gatewayConn{recv: framing.NewReassembler(framing.GatewayChunkSize)}
	conn.pipe = newPipeConn(CodecBinary, TransportGateway, gatewayConnQueue, send, closeConn)

	if !s.client.addConn(connID, conn) {
		s.reject(connID, gatewayReasonBusy)
		s.client.logger.Warn("gateway connection refused at client cap",
			"conn_id", connID, "max_clients", s.client.maxClients)
		return
	}
	s.serveWG.Add(1)
	go func() {
		defer s.serveWG.Done()
		s.client.hub.Serve(s.ctx, conn.pipe)
		s.client.removeConn(connID, conn)
	}()
}

// handleData reassembles one inbound chunk. Only a completed logical frame
// reaches the hub.
func (s *gatewaySession) handleData(connID uint32, payload []byte) {
	conn := s.client.lookupConn(connID)
	if conn == nil {
		s.reject(connID, gatewayReasonUnknownConn)
		return
	}
	frame, err := conn.push(payload)
	if err != nil {
		// A framing violation is one phone's fault: every other connection on
		// this link keeps running. Nothing derived from the chunk is logged.
		s.client.logger.Debug("gateway chunk framing rejected", "conn_id", connID)
		conn.pipe.Close(CloseGoingAway, gatewayReasonFraming)
		return
	}
	if frame == nil {
		return
	}
	// Push already returned a frame that owns its bytes, so the hub needs no
	// copy of the reused read buffer.
	if !conn.pipe.Deliver(frame) {
		s.client.logger.Debug("gateway connection evicted for falling behind", "conn_id", connID)
		conn.pipe.Close(CloseGoingAway, gatewayReasonSlow)
	}
}

func (s *gatewaySession) handleClose(connID uint32) {
	if conn := s.client.takeConn(connID); conn != nil {
		conn.pipe.Shutdown()
	}
}

func (s *gatewaySession) handleNotice(payload []byte) {
	var notice gatewaywire.NoticePayload
	if err := json.Unmarshal(payload, &notice); err != nil {
		s.client.logger.Debug("gateway notice rejected")
		return
	}
	s.client.recordNotice(notice.Message)
	s.client.logger.Warn("gateway notice", "kind", notice.Kind, "message", notice.Message)
}

// reject closes a connection id the relay declined to serve.
func (s *gatewaySession) reject(connID uint32, reason string) {
	_ = s.enqueue(gatewaywire.EncodeFrame(gatewaywire.OpClose, connID, closeReasonBytes(reason)))
}

// closeReasonBytes trims a reason to the wire limit without splitting a rune.
func closeReasonBytes(reason string) []byte {
	if reason == "" {
		return nil
	}
	if len(reason) > gatewaywire.MaxCloseReason {
		trimmed := reason[:gatewaywire.MaxCloseReason]
		for len(trimmed) > 0 && !utf8.ValidString(trimmed) {
			trimmed = trimmed[:len(trimmed)-1]
		}
		reason = trimmed
	}
	return []byte(reason)
}

// jitterDelay spreads a backoff interval by +/-20 % so a restarted gateway is
// not hit by every relay at the same instant.
func jitterDelay(delay time.Duration) time.Duration {
	return time.Duration(float64(delay) * (1 + gatewayBackoffJitter*(2*rand.Float64()-1)))
}

// sleepJittered waits out one jittered backoff interval. It reports false when
// the context ended first.
func sleepJittered(ctx context.Context, delay time.Duration) bool {
	timer := time.NewTimer(jitterDelay(delay))
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}
