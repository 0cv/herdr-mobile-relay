// Package reachability answers one question for the desktop relay: can a
// phone on the internet reach this machine directly over UDP?
//
// The self-test binds a throwaway UDP socket, asks the router to forward it
// with portmap, and has the gateway send a single random token datagram to the
// mapped address. Receiving that token proves both halves of the direct path —
// the router grants mappings and inbound UDP arrives — without touching the
// live ICE socket. The result is meant to be embedded in relay status output.
package reachability

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"os"
	"strings"
	"time"

	"github.com/0cv/herdr-mobile-relay/internal/portmap"
)

// MethodDirect is reported when no port mapping was needed or available and
// the external port equals the local one.
const MethodDirect = "direct"

const (
	// defaultTimeout bounds a self-test when the caller passes none.
	defaultTimeout = 15 * time.Second
	// httpTimeout bounds a single gateway HTTP request.
	httpTimeout = 5 * time.Second
	// probeLifetime is short: the mapping only has to outlive the test.
	probeLifetime = 2 * time.Minute
	// releaseTimeout bounds handing the temporary mapping back.
	releaseTimeout = 3 * time.Second
	// tokenBytes is the size of the probe token the gateway echoes over UDP.
	tokenBytes = 32
	// maxDatagram bounds a single read from the probe socket.
	maxDatagram = 1500
	// maxResponseBytes bounds a gateway JSON response.
	maxResponseBytes = 4 << 10
	// minProbePort mirrors the gateway's refusal to probe privileged ports.
	minProbePort = 1024
	// maxMapWait bounds the port-mapping half of a self-test.
	maxMapWait = 8 * time.Second
)

// errNoDatagram reports a self-test that timed out waiting for inbound UDP.
var errNoDatagram = errors.New("no probe datagram received before the deadline")

// Result is the outcome of a reachability self-test.
type Result struct {
	Reachable    bool      `json:"reachable"`
	Method       string    `json:"method"`
	ExternalIP   string    `json:"external_ip"`
	ExternalPort uint16    `json:"external_port"`
	Detail       string    `json:"detail"`
	CheckedAt    time.Time `json:"checked_at"`
}

// Prober talks to the gateway's reachability endpoints.
type Prober struct {
	base   string
	err    error
	client *http.Client
	logger *slog.Logger

	// mapUDP is portmap.MapUDP in production and a fake in tests.
	mapUDP func(ctx context.Context, internalPort uint16, lifetime time.Duration) (*portmap.Mapping, error)
}

// NewProber returns a prober for a gateway base URL such as
// wss://gw.example.com. A malformed URL is reported by the probe methods.
func NewProber(gatewayURL string, logger *slog.Logger) *Prober {
	if logger == nil {
		logger = slog.Default()
	}
	base, err := httpBase(gatewayURL)
	return &Prober{
		base:   base,
		err:    err,
		client: &http.Client{Timeout: httpTimeout},
		logger: logger,
		mapUDP: portmap.MapUDP,
	}
}

// httpBase converts a gateway WebSocket base URL into its HTTP origin.
func httpBase(gatewayURL string) (string, error) {
	trimmed := strings.TrimRight(strings.TrimSpace(gatewayURL), "/")
	if trimmed == "" {
		return "", errors.New("reachability: gateway url is empty")
	}
	scheme, rest, ok := strings.Cut(trimmed, "://")
	if !ok {
		return "", fmt.Errorf("reachability: gateway url %q has no scheme", gatewayURL)
	}
	switch strings.ToLower(scheme) {
	case "wss", "https":
		scheme = "https"
	case "ws", "http":
		scheme = "http"
	default:
		return "", fmt.Errorf("reachability: gateway url scheme %q unsupported", scheme)
	}
	if rest == "" {
		return "", fmt.Errorf("reachability: gateway url %q has no host", gatewayURL)
	}
	return scheme + "://" + rest, nil
}

type whoamiResponse struct {
	IP string `json:"ip"`
}

// PublicIP asks the gateway which source address it sees for this host.
func (p *Prober) PublicIP(ctx context.Context) (netip.Addr, error) {
	if p.err != nil {
		return netip.Addr{}, p.err
	}

	var payload whoamiResponse
	if err := p.getJSON(ctx, "/whoami", &payload); err != nil {
		return netip.Addr{}, err
	}
	addr, err := netip.ParseAddr(strings.TrimSpace(payload.IP))
	if err != nil {
		return netip.Addr{}, fmt.Errorf("reachability: gateway reported malformed ip %q", payload.IP)
	}
	return addr.Unmap(), nil
}

type probeRequest struct {
	Port  uint16 `json:"port"`
	Token string `json:"token"`
}

type probeResponse struct {
	Sent       bool   `json:"sent"`
	ObservedIP string `json:"observed_ip"`
}

// SelfTest proves or disproves inbound UDP reachability. A test that simply
// times out is not an error: it reports Reachable false with a detail string.
func (p *Prober) SelfTest(ctx context.Context, timeout time.Duration) (Result, error) {
	if p.err != nil {
		return Result{CheckedAt: time.Now()}, p.err
	}
	if timeout <= 0 {
		timeout = defaultTimeout
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	conn, err := net.ListenUDP("udp", &net.UDPAddr{Port: 0})
	if err != nil {
		return Result{CheckedAt: time.Now()}, fmt.Errorf("reachability: bind probe socket: %w", err)
	}
	defer conn.Close()

	// Cancellation and the overall timeout both unblock the socket read
	// without a helper goroutine outliving the call.
	stopWatch := context.AfterFunc(ctx, func() { _ = conn.SetReadDeadline(time.Now()) })
	defer stopWatch()

	local, ok := conn.LocalAddr().(*net.UDPAddr)
	if !ok {
		return Result{CheckedAt: time.Now()}, errors.New("reachability: probe socket has no udp address")
	}
	localPort := uint16(local.Port)

	// The mapping attempt only gets part of the budget: the datagram still
	// has to arrive before the deadline.
	mapCtx, cancelMap := context.WithTimeout(ctx, min(timeout/2, maxMapWait))
	mapping, mapErr := p.mapUDP(mapCtx, localPort, probeLifetime)
	cancelMap()

	result := Result{Method: MethodDirect, ExternalPort: localPort}
	if mapErr != nil {
		p.logger.Debug("reachability probe mapping unavailable", "error", mapErr)
	} else {
		defer p.release(ctx, mapping)
		result.Method = mapping.Method
		result.ExternalPort = mapping.External.Port()
		if addr := mapping.External.Addr(); addr.IsValid() && !addr.IsUnspecified() {
			result.ExternalIP = addr.String()
		}
	}

	token := make([]byte, tokenBytes)
	if _, err := rand.Read(token); err != nil {
		result.CheckedAt = time.Now()
		return result, fmt.Errorf("reachability: generate probe token: %w", err)
	}

	observed, err := p.requestProbe(ctx, result.ExternalPort, token)
	if err != nil {
		result.Detail = "gateway probe request failed"
		result.CheckedAt = time.Now()
		return result, err
	}
	if result.ExternalIP == "" {
		result.ExternalIP = observed
	}

	if err := awaitToken(conn, token); err != nil {
		result.Detail = err.Error()
		result.CheckedAt = time.Now()
		if errors.Is(err, errNoDatagram) {
			return result, nil
		}
		return result, fmt.Errorf("reachability: read probe socket: %w", err)
	}

	result.Reachable = true
	result.Detail = "inbound udp confirmed by gateway probe"
	result.CheckedAt = time.Now()
	return result, nil
}

func (p *Prober) release(ctx context.Context, mapping *portmap.Mapping) {
	releaseCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), releaseTimeout)
	defer cancel()
	if err := mapping.Release(releaseCtx); err != nil {
		p.logger.Debug("releasing probe mapping failed", "method", mapping.Method, "error", err)
	}
}

// requestProbe asks the gateway to send one token datagram back to us and
// returns the source address the gateway observed.
func (p *Prober) requestProbe(ctx context.Context, externalPort uint16, token []byte) (string, error) {
	// The gateway refuses privileged ports outright.
	if externalPort < minProbePort {
		return "", fmt.Errorf("reachability: external port %d is below the gateway minimum %d", externalPort, minProbePort)
	}

	body, err := json.Marshal(probeRequest{
		Port:  externalPort,
		Token: base64.RawURLEncoding.EncodeToString(token),
	})
	if err != nil {
		return "", fmt.Errorf("reachability: encode probe request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, p.base+"/probe", bytes.NewReader(body))
	if err != nil {
		return "", fmt.Errorf("reachability: build probe request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	var payload probeResponse
	if err := p.do(req, &payload); err != nil {
		return "", err
	}
	if !payload.Sent {
		return "", errors.New("reachability: gateway declined to send the probe datagram")
	}
	return strings.TrimSpace(payload.ObservedIP), nil
}

func (p *Prober) getJSON(ctx context.Context, path string, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, p.base+path, nil)
	if err != nil {
		return fmt.Errorf("reachability: build %s request: %w", path, err)
	}
	return p.do(req, out)
}

func (p *Prober) do(req *http.Request, out any) error {
	resp, err := p.client.Do(req)
	if err != nil {
		return fmt.Errorf("reachability: %s %s: %w", req.Method, req.URL.Path, err)
	}
	defer resp.Body.Close()

	data, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes))
	if err != nil {
		return fmt.Errorf("reachability: read %s response: %w", req.URL.Path, err)
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("reachability: gateway %s returned status %d", req.URL.Path, resp.StatusCode)
	}
	if err := json.Unmarshal(data, out); err != nil {
		return fmt.Errorf("reachability: decode %s response: %w", req.URL.Path, err)
	}
	return nil
}

// awaitToken blocks until the exact token arrives on conn or the socket
// deadline passes. Unrelated datagrams are ignored.
func awaitToken(conn *net.UDPConn, token []byte) error {
	buf := make([]byte, maxDatagram)
	for {
		n, _, err := conn.ReadFromUDP(buf)
		if err != nil {
			if errors.Is(err, os.ErrDeadlineExceeded) {
				return errNoDatagram
			}
			return err
		}
		if subtle.ConstantTimeCompare(buf[:n], token) == 1 {
			return nil
		}
	}
}
