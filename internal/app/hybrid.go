package app

import (
	"context"
	"errors"
	"net"
	"net/netip"
	"net/url"
	"slices"
	"strconv"
	"sync"
	"time"

	"github.com/0cv/herdr-mobile-relay/internal/portmap"
	"github.com/0cv/herdr-mobile-relay/internal/protocol"
	"github.com/0cv/herdr-mobile-relay/internal/reachability"
	"github.com/0cv/herdr-mobile-relay/internal/transport"
	"github.com/0cv/herdr-mobile-relay/internal/webrtclink"
)

const (
	// maxWebRTCSessions bounds concurrent and pending PeerConnections.
	maxWebRTCSessions = 16
	// signalBurst and signalWindow rate-limit negotiation attempts per client.
	// A phone needs one offer per upgrade and one ICE restart per network flip;
	// anything beyond this is abuse or a broken client.
	signalBurst  = 8
	signalWindow = time.Minute
	// signalMinInterval keeps a client from re-offering in a tight loop.
	signalMinInterval = 2 * time.Second
	// selfTestInterval re-runs the reachability probe so a router reboot or a
	// lease change is reflected in status without a relay restart.
	selfTestInterval = 30 * time.Minute
	selfTestTimeout  = 8 * time.Second
	// stunFirstRetry starts address discovery retries close behind the gateway
	// hello that advertises the STUN port: it arrives after the loop's first
	// pass, and until a mapping is published every phone that connects is stuck
	// on the relayed path. The interval doubles up to stunRetryInterval so a
	// gateway without a STUN listener costs almost nothing.
	stunFirstRetry    = time.Second
	stunRetryInterval = 30 * time.Second
	// stunKeepaliveInterval re-runs discovery on a learned mapping. It is not
	// paranoia: a NAT drops an idle UDP mapping after 30 s on the impatient
	// routers and a couple of minutes on the rest, and the reflexive candidate
	// this relay advertises is only reachable while its mapping lives. Between
	// direct sessions the ICE socket is silent. One small exchange every 10 s
	// keeps the mapping open and bounds relay network-change discovery without
	// requiring platform-specific route monitors.
	stunKeepaliveInterval = 10 * time.Second
	// portMappingLifetime is the requested NAT mapping lifetime; the mapper
	// renews at half of whatever the router actually grants.
	portMappingLifetime = time.Hour
)

// hybridTransport owns everything the hybrid transport adds to the relay: the
// outbound gateway registration, the WebRTC responder, and the reachability
// helpers that raise the direct-path success rate.
type hybridTransport struct {
	gateway *transport.GatewayClient
	webrtc  *webrtclink.Manager
	mapper  *portmap.Mapper
	forced  bool
	logger  signalLogger
	// newProber builds a reachability prober for one gateway origin, and is nil
	// when the direct path cannot use one. The prober is built per self-test
	// because it is pinned to a single origin: after a failover, probing the
	// gateway we left reports someone else's reachability.
	newProber func(gatewayURL string) *reachability.Prober
	mu        sync.Mutex
	selfTest  reachability.Result
	stun      stunStatus
	signals   map[string]*signalBudget
}

// stunStatus is the last address-discovery outcome, surfaced on /healthz so an
// operator can tell "no STUN offered" apart from "STUN offered but silent".
type stunStatus struct {
	configured bool
	mapped     []netip.AddrPort
	checkedAt  time.Time
}

// signalLogger is the subset of slog the hybrid transport needs, kept narrow so
// tests can assert that no SDP ever reaches a log sink.
type signalLogger interface {
	Debug(msg string, args ...any)
	Info(msg string, args ...any)
	Warn(msg string, args ...any)
}

type signalBudget struct {
	windowStart time.Time
	count       int
	last        time.Time
}

// startHybridTransport builds the hybrid components the configuration enables
// and returns nil when neither the gateway nor the direct path is available.
func (s *Server) startHybridTransport(ctx context.Context) *hybridTransport {
	if len(s.cfg.GatewayURLs) == 0 {
		return nil
	}
	h := &hybridTransport{
		forced:  s.cfg.ForceRelayTransport,
		logger:  s.logger,
		signals: make(map[string]*signalBudget),
	}

	gateway, err := transport.NewGatewayClient(s.hub, transport.GatewayOptions{
		URLs:      s.cfg.GatewayURLs,
		Selection: s.cfg.GatewaySelection,
		RelayKey:  s.cfg.Token,
		Logger:    s.logger,
	})
	if err != nil {
		s.recordSafeError("gateway transport unavailable", err)
		s.logger.Warn("gateway transport unavailable", "error", err)
		return nil
	}
	h.gateway = gateway

	if !s.cfg.ForceRelayTransport {
		manager, err := webrtclink.New(webrtclink.Options{
			Logger:      s.logger,
			UDPPort:     s.cfg.WebRTCUDPPort,
			MaxSessions: maxWebRTCSessions,
			OnLocalCandidate: func(key webrtclink.SessionKey, candidate webrtclink.Candidate) {
				s.hub.SendByID(key.ClientID, map[string]any{
					"type":            "webrtc_ice",
					"request_id":      key.RequestID,
					"candidate":       candidate.Candidate,
					"sdp_mid":         candidate.SDPMid,
					"sdp_mline_index": candidate.SDPMLineIndex,
				})
			},
			Serve: s.hub.Serve,
		})
		if err != nil {
			s.recordSafeError("direct WebRTC path unavailable", err)
			s.logger.Warn("direct WebRTC path unavailable", "error", err)
		} else {
			h.webrtc = manager
		}
	}

	if h.webrtc != nil && s.cfg.PortMappingEnabled {
		h.mapper = portmap.NewMapper(s.logger)
		h.newProber = func(gatewayURL string) *reachability.Prober {
			return reachability.NewProber(gatewayURL, s.logger)
		}
	}
	return h
}

// run drives the long-lived hybrid services under the relay's run context.
func (h *hybridTransport) run(ctx context.Context, start func(func())) {
	start(func() { h.gateway.Run(ctx) })
	if h.webrtc == nil {
		return
	}
	port := uint16(h.webrtc.LocalPort())
	if port == 0 {
		return
	}
	if h.mapper != nil {
		start(func() { h.mapper.Run(ctx, port) })
	}
	// The loop also drives STUN address discovery, which is the only way a
	// relay behind a carrier-grade NAT learns its mapped address, so it runs
	// even when port mapping is disabled or unsupported by the router.
	start(func() { h.reachabilityLoop(ctx) })
}

// reachabilityLoop keeps the advertised external address and the self-test
// result current. The mapped address is published to ICE as a server-reflexive
// candidate, which is what lets a cellular phone reach a home computer.
func (h *hybridTransport) reachabilityLoop(ctx context.Context) {
	timer := time.NewTimer(0)
	defer timer.Stop()
	var lastSelfTest time.Time
	stunRetry := stunFirstRetry
	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
		}
		if h.mapper != nil {
			if mapping, ok := h.mapper.Current(); ok {
				h.webrtc.SetNAT1To1IPs([]string{mapping.External.Addr().String()})
			}
		}
		discovered := h.discoverMappedAddress(ctx)

		now := time.Now()
		if h.newProber != nil && now.Sub(lastSelfTest) >= selfTestInterval {
			lastSelfTest = now
			h.runSelfTest(ctx)
		}

		next, retry := reachabilitySchedule(discovered, h.stunExpected(), stunRetry)
		stunRetry = retry
		timer.Reset(next)
	}
}

// reachabilitySchedule decides when the loop wakes next and what the following
// discovery retry costs. A relay still without a mapping retries on a short
// doubling cadence instead of waiting out the half-hourly self-test; a relay
// that has one wakes on the keepalive cadence, because the mapping it advertises
// dies of idleness otherwise; and a gateway advertising no STUN listener settles
// onto the self-test interval, since there is nothing to keep alive.
func reachabilitySchedule(
	discovered, expected bool,
	retry time.Duration,
) (next, nextRetry time.Duration) {
	if discovered {
		return stunKeepaliveInterval, stunFirstRetry
	}
	if !expected {
		return selfTestInterval, stunFirstRetry
	}
	if retry < stunFirstRetry {
		retry = stunFirstRetry
	}
	return retry, min(retry*2, stunRetryInterval)
}

// runSelfTest refreshes the reachability probe result.
func (h *hybridTransport) runSelfTest(ctx context.Context) {
	testCtx, cancel := context.WithTimeout(ctx, selfTestTimeout)
	result, err := h.newProber(h.gateway.CurrentURL()).SelfTest(testCtx, selfTestTimeout)
	cancel()
	if err != nil {
		result = reachability.Result{Detail: err.Error(), CheckedAt: time.Now().UTC()}
	}
	h.mu.Lock()
	h.selfTest = result
	h.mu.Unlock()
	h.logger.Info("reachability self-test",
		"reachable", result.Reachable, "method", result.Method)
}

// discoverMappedAddress points the WebRTC manager at the gateway's STUN
// listener and publishes whatever address it reflects. The host comes from the
// URL this relay dialed and only the port from the gateway hello, so a gateway
// can never redirect address discovery at a third party. It reports whether a
// mapping was learned.
func (h *hybridTransport) discoverMappedAddress(ctx context.Context) bool {
	server := stunURL(h.gateway.CurrentURL(), h.gateway.STUNPort())
	if server == "" {
		h.webrtc.SetSTUNServers(nil)
		h.mu.Lock()
		h.stun.configured = false
		h.mu.Unlock()
		return false
	}
	h.webrtc.SetSTUNServers([]string{server})
	h.mu.Lock()
	h.stun.configured = true
	h.mu.Unlock()

	mapped, err := h.webrtc.DiscoverMappedAddresses(ctx)
	if err != nil {
		h.logger.Warn("stun address discovery failed", "error", err)
		return false
	}
	h.mu.Lock()
	changed := !slices.Equal(h.stun.mapped, mapped)
	h.stun.mapped = mapped
	h.stun.checkedAt = time.Now().UTC()
	h.mu.Unlock()
	// Re-publishing rebuilds the WebRTC API, so only a genuine change is worth
	// it: the steady-state call exists to keep the NAT mapping alive, not to
	// republish the same address every half minute.
	if !changed {
		return true
	}
	h.webrtc.PublishMappedAddresses(mapped)
	// The addresses themselves are the relay's public endpoints; only the shape
	// of the result is logged.
	h.logger.Info("stun address discovery", "families", len(mapped))
	return true
}

// stunExpected reports whether address discovery can still succeed: either the
// gateway hello has not arrived yet, or it advertised a usable port.
func (h *hybridTransport) stunExpected() bool {
	return !h.gateway.Status().Registered || h.gateway.STUNPort() > 0
}

// stunURL builds the address-discovery endpoint from the host of the gateway
// URL this relay dialed and the port the gateway advertised. It returns an
// empty string when discovery is disabled, which includes any advertised port
// outside the valid range: a hostile gateway must not be able to aim the relay
// anywhere, not even at another port of itself that is not a port.
func stunURL(gatewayURL string, port int) string {
	if port < 1 || port > 65535 {
		return ""
	}
	parsed, err := url.Parse(gatewayURL)
	if err != nil {
		return ""
	}
	host := parsed.Hostname()
	if host == "" {
		return ""
	}
	return "stun:" + net.JoinHostPort(host, strconv.Itoa(port))
}

func (h *hybridTransport) close() {
	if h.mapper != nil {
		h.mapper.Close()
	}
	if h.webrtc != nil {
		_ = h.webrtc.Close()
	}
}

// directEnabled reports whether clients should attempt the WebRTC upgrade.
func (h *hybridTransport) directEnabled() bool {
	return h != nil && h.webrtc != nil && !h.forced
}

// status renders the hybrid transport for /healthz and relay status output.
func (h *hybridTransport) status() map[string]any {
	if h == nil {
		return map[string]any{"enabled": false, "registered": false}
	}
	gateway := h.gateway.Status()
	out := map[string]any{
		"enabled":           gateway.Enabled,
		"registered":        gateway.Registered,
		"relay_id":          gateway.RelayID,
		"url":               gateway.URL,
		"urls":              gateway.URLs,
		"gateway_selection": h.gateway.Selection(),
		"clients":           gateway.Clients,
		"direct":            h.directEnabled(),
		"forced_relay":      h.forced,
		"last_error":        gateway.LastError,
		"last_notice":       gateway.LastNotice,
		"webrtc_active":     0,
	}
	if h.webrtc != nil {
		out["webrtc_active"] = h.webrtc.SessionCount()
		out["webrtc_port"] = h.webrtc.LocalPort()
		// Candidate types, never addresses: enough to tell a direct session from
		// a session that is still trying, and why.
		sessions := make([]map[string]any, 0, 4)
		for _, report := range h.webrtc.SessionReports() {
			sessions = append(sessions, map[string]any{
				"local_types":     report.LocalTypes,
				"remote_types":    report.RemoteTypes,
				"selected_local":  report.SelectedLocal,
				"selected_remote": report.SelectedRemote,
			})
		}
		out["webrtc_sessions"] = sessions
		// Monotonic since start. Two numbers are the whole telemetry story: how
		// often the direct path formed, and how often it never did.
		outcomes := h.webrtc.Outcomes()
		out["sessions_direct_total"] = outcomes.Direct
		out["sessions_relayed_total"] = outcomes.Relayed
	}
	h.mu.Lock()
	selfTest := h.selfTest
	stun := h.stun
	h.mu.Unlock()
	if !selfTest.CheckedAt.IsZero() {
		out["reachability"] = selfTest
	}
	if h.webrtc != nil {
		// Reported even when unconfigured, so an operator can tell "the
		// gateway offers no address discovery" from "it does and we heard
		// nothing back".
		discovery := map[string]any{"configured": stun.configured}
		// An array because a dual-stack relay discovers one mapping per family,
		// and the IPv6 one is usually the address that makes a direct path easy.
		addresses := make([]string, 0, len(stun.mapped))
		for _, mapped := range stun.mapped {
			addresses = append(addresses, mapped.String())
		}
		discovery["mapped"] = addresses
		if !stun.checkedAt.IsZero() {
			discovery["checked_at"] = stun.checkedAt
		}
		out["stun"] = discovery
	}
	return out
}

// allowSignal applies the per-client negotiation budget.
func (h *hybridTransport) allowSignal(clientID string, now time.Time) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	budget := h.signals[clientID]
	if budget == nil {
		budget = &signalBudget{windowStart: now}
		h.signals[clientID] = budget
	}
	if now.Sub(budget.windowStart) >= signalWindow {
		budget.windowStart = now
		budget.count = 0
	}
	if budget.count >= signalBurst || now.Sub(budget.last) < signalMinInterval {
		return false
	}
	budget.count++
	budget.last = now
	return true
}

func (h *hybridTransport) forgetClient(clientID string) {
	h.mu.Lock()
	delete(h.signals, clientID)
	h.mu.Unlock()
}

var errSignalUnavailable = errors.New("direct connections are not available on this relay")

// handleWebRTCSignal answers the four signaling message types. Signaling rides
// inside the already-authenticated encrypted channel, so no intermediary — the
// gateway operator included — ever sees SDP or ICE candidates.
func (s *Server) handleWebRTCSignal(
	ctx context.Context,
	client *transport.ClientConn,
	action string,
	requestID string,
	msg map[string]any,
) {
	h := s.hybrid
	if !h.directEnabled() {
		s.sendCommandResult(client, requestID, action, false, "failed", errSignalUnavailable.Error(), "", nil)
		return
	}
	// A DataChannel client is already direct; renegotiating from there would
	// stack a second peer connection on top of the first.
	if client.Transport() == transport.TransportWebRTC {
		s.sendCommandResult(client, requestID, action, false, "failed",
			"already connected over a direct channel", "", nil)
		return
	}
	if requestID == "" {
		s.sendCommandResult(client, requestID, action, false, "failed", "request_id is required", "", nil)
		return
	}
	key := webrtclink.SessionKey{ClientID: client.ID(), RequestID: requestID}

	switch action {
	case "webrtc_offer":
		if !h.allowSignal(client.ID(), time.Now()) {
			s.sendCommandResult(client, requestID, action, false, "failed",
				"too many direct-connection attempts; retry shortly", "", nil)
			return
		}
		offer := stringField(msg, "sdp")
		if offer == "" {
			s.sendCommandResult(client, requestID, action, false, "failed", "sdp is required", "", nil)
			return
		}
		answer, err := h.webrtc.HandleOffer(ctx, key, offer)
		if err != nil {
			s.logger.Debug("webrtc offer rejected", "client_id", client.ID(), "error", err)
			s.sendCommandResult(client, requestID, action, false, "failed", err.Error(), "", nil)
			return
		}
		s.hub.Send(client, map[string]any{
			"type":       "webrtc_answer",
			"request_id": requestID,
			"sdp":        answer,
		})
	case "webrtc_ice":
		candidate := webrtclink.Candidate{
			Candidate:     stringField(msg, "candidate"),
			SDPMid:        stringField(msg, "sdp_mid"),
			SDPMLineIndex: uint16(intField(msg, "sdp_mline_index")),
		}
		if candidate.Candidate == "" {
			return
		}
		if err := h.webrtc.AddRemoteCandidate(key, candidate); err != nil {
			s.logger.Debug("webrtc candidate rejected", "client_id", client.ID(), "error", err)
		}
	case "webrtc_close":
		h.webrtc.CloseSession(key, "closed by client")
		s.hub.Send(client, map[string]any{
			"type":       "webrtc_closed",
			"request_id": requestID,
			"reason":     "closed by client",
		})
	}
}

// stringField reads a signaling field verbatim. SDP and ICE candidate bodies
// are line-oriented and their trailing terminator is significant: trimming an
// offer's final newline makes every SDP parser fail at EOF.
func stringField(msg map[string]any, key string) string {
	value, _ := msg[key].(string)
	return value
}

func intField(msg map[string]any, key string) int {
	switch value := msg[key].(type) {
	case float64:
		return int(value)
	case int:
		return value
	}
	return 0
}

// hybridDescriptor is the bridge-window advertisement: an existing app that
// connected over the legacy WSS URL learns the relay now speaks the hybrid
// transport and can migrate without a QR re-scan.
func (s *Server) hybridDescriptor() map[string]any {
	if s.hybrid == nil {
		return nil
	}
	status := s.hybrid.gateway.Status()
	if !status.Enabled {
		return nil
	}
	return map[string]any{
		"transport":    protocol.HybridTransportCapability,
		"gateway_url":  status.URL,
		"gateway_urls": status.URLs,
		"relay_id":     status.RelayID,
		"direct":       s.hybrid.directEnabled(),
	}
}
