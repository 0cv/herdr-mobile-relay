// Package portmap asks the local router for a UDP port mapping so the WebRTC
// ICE socket is reachable from the internet without manual configuration.
//
// Three protocols are attempted in order of preference: PCP (RFC 6887),
// NAT-PMP (RFC 6886) and UPnP IGD (SSDP discovery plus a SOAP AddPortMapping
// call). Everything is hand-rolled on the standard library; router responses
// are treated as hostile input, so every read is bounded and every field is
// length-checked before it is sliced.
//
// A mapping is best effort. Callers must keep working when MapUDP fails: the
// gateway fallback transport does not need any inbound reachability.
package portmap

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"sync"
	"time"
)

// Mapping methods reported in Mapping.Method and reachability status output.
const (
	MethodPCP    = "pcp"
	MethodNATPMP = "nat-pmp"
	MethodUPnP   = "upnp"
)

// DefaultLifetime is the mapping lifetime requested when a caller has no
// preference. Renewal happens at half of the granted lifetime.
const DefaultLifetime = time.Hour

const (
	// minLifetime keeps a router from handing out a mapping that expires
	// before the first renewal tick.
	minLifetime = 2 * time.Minute
	// maxLifetime bounds what we ask for; routers cap this anyway.
	maxLifetime = 24 * time.Hour
	// stepTimeout bounds a single network exchange with the router.
	stepTimeout = 3 * time.Second
	// releaseTimeout bounds the teardown exchange on shutdown.
	releaseTimeout = 3 * time.Second
)

// mapperPort is the well-known UDP port shared by PCP and NAT-PMP servers.
const mapperPort = 5351

// ErrNoMethod reports that no port-mapping protocol answered.
var ErrNoMethod = errors.New("portmap: no port mapping protocol available")

// Mapping describes a granted UDP port mapping. The unexported fields carry
// exactly what Release needs to hand the mapping back to the router.
type Mapping struct {
	// External is the public address the router forwards from. The address
	// part is unspecified when the protocol did not report it.
	External netip.AddrPort
	// Internal is the local UDP port that receives the forwarded traffic.
	Internal uint16
	// Method is one of MethodPCP, MethodNATPMP or MethodUPnP.
	Method string
	// Lifetime is the lifetime the router granted.
	Lifetime time.Duration
	// ExpiresAt is when the mapping lapses unless it is renewed.
	ExpiresAt time.Time

	owner   *client
	gateway netip.Addr
	local   netip.Addr
	nonce   [pcpNonceLen]byte
	control string
	service string
}

// Release hands the mapping back to the router. It is safe on a nil mapping
// and on a mapping that the router has already dropped.
func (m *Mapping) Release(ctx context.Context) error {
	if m == nil || m.owner == nil {
		return nil
	}
	switch m.Method {
	case MethodPCP:
		return m.owner.releasePCP(ctx, m)
	case MethodNATPMP:
		return m.owner.releaseNATPMP(ctx, m)
	case MethodUPnP:
		return m.owner.releaseUPnP(ctx, m)
	}
	return fmt.Errorf("portmap: cannot release unknown method %q", m.Method)
}

// MapUDP requests a UDP mapping for internalPort using the first protocol the
// router answers. The returned mapping must be released by the caller.
func MapUDP(ctx context.Context, internalPort uint16, lifetime time.Duration) (*Mapping, error) {
	return newClient(nil).mapUDP(ctx, internalPort, lifetime)
}

// client holds the tunables the tests replace: gateway discovery, the UDP port
// of the PCP/NAT-PMP server, the SSDP multicast address and the HTTP client
// used for UPnP.
type client struct {
	logger     *slog.Logger
	gateways   func(context.Context) ([]netip.Addr, error)
	serverPort uint16
	allowUPnP  bool
	ssdpAddr   string
	ssdpWait   time.Duration
	httpClient *http.Client
}

func newClient(logger *slog.Logger) *client {
	if logger == nil {
		logger = slog.Default()
	}
	return &client{
		logger:     logger,
		gateways:   defaultGateways,
		serverPort: mapperPort,
		allowUPnP:  true,
		ssdpAddr:   ssdpMulticastAddr,
		ssdpWait:   ssdpDefaultWait,
		httpClient: newUPnPHTTPClient(),
	}
}

func (c *client) mapUDP(ctx context.Context, internalPort uint16, lifetime time.Duration) (*Mapping, error) {
	if internalPort == 0 {
		return nil, errors.New("portmap: internal port must be non-zero")
	}
	lifetime = clampLifetime(lifetime)

	var failures []error
	gateways, err := c.gateways(ctx)
	if err != nil {
		failures = append(failures, fmt.Errorf("gateway discovery: %w", err))
	}
	for _, gw := range gateways {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		mapping, err := c.mapPCP(ctx, gw, internalPort, lifetime)
		if err == nil {
			return mapping, nil
		}
		failures = append(failures, fmt.Errorf("pcp %s: %w", gw, err))

		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		mapping, err = c.mapNATPMP(ctx, gw, internalPort, lifetime)
		if err == nil {
			return mapping, nil
		}
		failures = append(failures, fmt.Errorf("nat-pmp %s: %w", gw, err))
	}

	if c.allowUPnP && ctx.Err() == nil {
		mapping, err := c.mapUPnP(ctx, gateways, internalPort, lifetime)
		if err == nil {
			return mapping, nil
		}
		failures = append(failures, fmt.Errorf("upnp: %w", err))
	}

	if len(failures) == 0 {
		return nil, ErrNoMethod
	}
	return nil, errors.Join(append([]error{ErrNoMethod}, failures...)...)
}

func clampLifetime(lifetime time.Duration) time.Duration {
	if lifetime < minLifetime {
		return minLifetime
	}
	if lifetime > maxLifetime {
		return maxLifetime
	}
	return lifetime
}

// localAddrFor reports the source address the host uses to reach gateway. UDP
// dialing performs a route lookup without sending a packet.
func (c *client) localAddrFor(gateway netip.Addr) (netip.Addr, error) {
	conn, err := net.Dial("udp", netip.AddrPortFrom(gateway, c.serverPort).String())
	if err != nil {
		return netip.Addr{}, fmt.Errorf("route lookup to %s: %w", gateway, err)
	}
	defer conn.Close()

	addr, ok := conn.LocalAddr().(*net.UDPAddr)
	if !ok {
		return netip.Addr{}, fmt.Errorf("route lookup to %s: unexpected local address", gateway)
	}
	local, ok := netip.AddrFromSlice(addr.IP)
	if !ok {
		return netip.Addr{}, fmt.Errorf("route lookup to %s: unusable local address", gateway)
	}
	return local.Unmap(), nil
}

// Mapper keeps one UDP mapping alive for the lifetime of the relay process.
type Mapper struct {
	logger   *slog.Logger
	client   *client
	lifetime time.Duration
	after    func(time.Duration) <-chan time.Time

	mu      sync.Mutex
	wg      sync.WaitGroup
	cancel  context.CancelFunc
	current *Mapping
	closed  bool
}

// Backoff bounds for repeated mapping failures.
const (
	initialBackoff = 30 * time.Second
	maxBackoff     = 10 * time.Minute
	minRenew       = 30 * time.Second
	attemptTimeout = 20 * time.Second
)

// NewMapper returns a mapper that has not attempted anything yet.
func NewMapper(logger *slog.Logger) *Mapper {
	if logger == nil {
		logger = slog.Default()
	}
	return &Mapper{
		logger:   logger,
		client:   newClient(logger),
		lifetime: DefaultLifetime,
		after:    time.After,
	}
}

// Run maintains a mapping for internalPort until ctx is done or Close is
// called. It renews at half the granted lifetime and re-discovers the router
// after a failure, with bounded exponential backoff.
func (m *Mapper) Run(ctx context.Context, internalPort uint16) {
	if internalPort == 0 {
		m.logger.Debug("port mapping disabled: no internal port")
		return
	}

	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return
	}
	runCtx, cancel := context.WithCancel(ctx)
	m.cancel = cancel
	m.wg.Add(1)
	m.mu.Unlock()

	defer func() {
		cancel()
		m.wg.Done()
	}()

	backoff := initialBackoff
	for {
		wait := backoff
		attemptCtx, attemptCancel := context.WithTimeout(runCtx, attemptTimeout)
		mapping, err := m.client.mapUDP(attemptCtx, internalPort, m.lifetime)
		attemptCancel()

		switch {
		case err != nil:
			if runCtx.Err() != nil {
				return
			}
			m.setCurrent(nil)
			m.logger.Debug("udp port mapping attempt failed", "error", err)
			backoff = min(backoff*2, maxBackoff)
		default:
			m.setCurrent(mapping)
			backoff = initialBackoff
			wait = max(mapping.Lifetime/2, minRenew)
			m.logger.Info("udp port mapping active",
				"method", mapping.Method,
				"internal_port", mapping.Internal,
				"external_port", mapping.External.Port(),
				"lifetime", mapping.Lifetime)
		}

		select {
		case <-runCtx.Done():
			return
		case <-m.after(wait):
		}
	}
}

// Current returns the mapping in force, if any.
func (m *Mapper) Current() (*Mapping, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.current, m.current != nil
}

func (m *Mapper) setCurrent(mapping *Mapping) {
	m.mu.Lock()
	m.current = mapping
	m.mu.Unlock()
}

// Close stops renewal and releases the mapping. It blocks until Run has
// returned and the release exchange finished or timed out.
func (m *Mapper) Close() {
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return
	}
	m.closed = true
	cancel := m.cancel
	m.mu.Unlock()

	if cancel != nil {
		cancel()
	}
	m.wg.Wait()

	m.mu.Lock()
	mapping := m.current
	m.current = nil
	m.mu.Unlock()
	if mapping == nil {
		return
	}

	ctx, cancelRelease := context.WithTimeout(context.Background(), releaseTimeout)
	defer cancelRelease()
	if err := mapping.Release(ctx); err != nil {
		m.logger.Debug("releasing udp port mapping failed", "method", mapping.Method, "error", err)
		return
	}
	m.logger.Debug("udp port mapping released", "method", mapping.Method)
}
