package gateway

import (
	"encoding/base64"
	"encoding/json"
	"net"
	"net/http"
	"net/netip"
	"time"

	"github.com/0cv/herdr-mobile-relay/internal/gatewaywire"
)

// probeWriteTimeout bounds the single UDP send.
const probeWriteTimeout = 2 * time.Second

func (s *Server) handleProbe(w http.ResponseWriter, r *http.Request) {
	var request gatewaywire.ProbeRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, gatewaywire.MaxProbeBody)).Decode(&request); err != nil {
		writeJSONResponse(w, http.StatusBadRequest, gatewaywire.ErrorResponse{Error: "invalid_body"})
		return
	}
	// Ports below 1024 are privileged: probing them would turn the gateway into a
	// scanner for services the caller may not own.
	if request.Port < gatewaywire.MinProbePort || request.Port > 65535 {
		writeJSONResponse(w, http.StatusBadRequest, gatewaywire.ErrorResponse{Error: "invalid_port"})
		return
	}
	token, err := base64.RawURLEncoding.DecodeString(request.Token)
	if err != nil || len(token) != gatewaywire.ProbeTokenBytes {
		writeJSONResponse(w, http.StatusBadRequest, gatewaywire.ErrorResponse{Error: "invalid_token"})
		return
	}

	observed := s.clientIP(r)
	addr, err := netip.ParseAddr(observed)
	if err != nil {
		writeJSONResponse(w, http.StatusBadRequest, gatewaywire.ErrorResponse{Error: "unknown_source_address"})
		return
	}

	// The body is validated first so a malformed request cannot burn the caller's
	// probe budget.
	if !s.probeLimiter.allow("probe:"+observed, s.now()) {
		writeJSONResponse(w, http.StatusTooManyRequests, gatewaywire.ErrorResponse{Error: "rate_limited"})
		return
	}

	target := net.UDPAddrFromAddrPort(netip.AddrPortFrom(addr.Unmap(), uint16(request.Port)))
	conn, err := net.DialUDP("udp", nil, target)
	if err != nil {
		s.logger.Debug("probe dial failed", "error", err)
		writeJSONResponse(w, http.StatusBadGateway, gatewaywire.ErrorResponse{Error: "probe_failed"})
		return
	}
	defer conn.Close()

	if err := conn.SetWriteDeadline(time.Now().Add(probeWriteTimeout)); err != nil {
		writeJSONResponse(w, http.StatusBadGateway, gatewaywire.ErrorResponse{Error: "probe_failed"})
		return
	}
	if _, err := conn.Write(token); err != nil {
		s.logger.Debug("probe send failed", "error", err)
		writeJSONResponse(w, http.StatusBadGateway, gatewaywire.ErrorResponse{Error: "probe_failed"})
		return
	}

	s.logger.Info("reachability probe sent", "port", request.Port)
	writeJSONResponse(w, http.StatusOK, gatewaywire.ProbeResponse{Sent: true, ObservedIP: observed})
}
