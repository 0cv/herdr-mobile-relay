package transport

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"strings"
	"sync"
	"time"

	"github.com/coder/websocket"

	"github.com/0cv/herdr-mobile-relay/internal/gatewaywire"
)

const (
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
	// URLs is the gateway candidate list. At startup the relay probes every
	// entry concurrently and registers with one healthy entry; Selection
	// decides which, and list order remains the fallback when probes fail.
	URLs []string
	// Selection is config.GatewaySelectionOrdered or
	// config.GatewaySelectionLatency. Empty means ordered: a caller that did
	// not think about the rule gets the configured list honoured.
	Selection string
	// RelayKey is the relay's pairing secret. Both gateway identifiers are
	// derived from it; the key itself never leaves the process.
	RelayKey string
	// RegistrationToken is the optional shared gateway registration secret.
	RegistrationToken string
	Logger            *slog.Logger
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
	selection         string
	relayID           string
	rendezvousKey     []byte
	registrationToken string
	maxClients        int
	logger            *slog.Logger

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
	logger := opts.Logger
	if logger == nil {
		logger = slog.Default()
	}
	return &GatewayClient{
		hub:               hub,
		urls:              urls,
		selection:         opts.Selection,
		relayID:           relayID,
		rendezvousKey:     rendezvousKey,
		registrationToken: opts.RegistrationToken,
		maxClients:        gatewayDefaultMaxClients,
		logger:            logger,
		backoffBase:       gatewayBackoffBase,
		backoffMax:        gatewayBackoffMax,
		probeTimeout:      gatewayProbeTimeout,
		healthInterval:    gatewayHealthInterval,
		probe:             measureGatewayRTT,
		conns:             make(map[uint32]*gatewayConn),
	}, nil
}

func gatewayURLs(opts GatewayOptions) ([]string, error) {
	list := make([]string, 0, len(opts.URLs))
	for _, raw := range opts.URLs {
		if entry := strings.TrimRight(strings.TrimSpace(raw), "/"); entry != "" {
			list = append(list, entry)
		}
	}
	if len(list) == 0 {
		return nil, errors.New("gateway url is required")
	}
	return list, nil
}

// STUNPort reports the address-discovery port the gateway last advertised, or
// 0 when discovery is unavailable. The caller pairs it with the gateway host
// it configured; the gateway never supplies a host.
func (c *GatewayClient) STUNPort() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.stunPort
}

// Status snapshots the registration for relay status output.
func (c *GatewayClient) Status() GatewayStatus {
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
	conn, _, err := websocket.Dial(dialCtx, base+gatewaywire.RelayPath, &websocket.DialOptions{
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
