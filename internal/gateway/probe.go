package gateway

import (
	"encoding/base64"
	"encoding/json"
	"net"
	"net/http"
	"net/netip"
	"time"
)

// probeWriteTimeout bounds the single UDP send.
const probeWriteTimeout = 2 * time.Second

// probeRequest asks the gateway to send one UDP datagram back to the caller so a
// relay can prove that a port mapping works from outside its own network.
//
// There is deliberately no destination address field: the datagram always goes
// to the source address the gateway observed. Any other field in the body is
// ignored, which makes the endpoint useless for amplification or reflection.
type probeRequest struct {
	Port  int    `json:"port"`
	Token string `json:"token"`
}

type probeResponse struct {
	Sent       bool   `json:"sent"`
	ObservedIP string `json:"observed_ip"`
}

type errorResponse struct {
	Error string `json:"error"`
}

func (s *Server) handleProbe(w http.ResponseWriter, r *http.Request) {
	var request probeRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxProbeBody)).Decode(&request); err != nil {
		writeJSONResponse(w, http.StatusBadRequest, errorResponse{Error: "invalid_body"})
		return
	}
	// Ports below 1024 are privileged: probing them would turn the gateway into a
	// scanner for services the caller may not own.
	if request.Port < 1024 || request.Port > 65535 {
		writeJSONResponse(w, http.StatusBadRequest, errorResponse{Error: "invalid_port"})
		return
	}
	token, err := base64.RawURLEncoding.DecodeString(request.Token)
	if err != nil || len(token) != probeTokenBytes {
		writeJSONResponse(w, http.StatusBadRequest, errorResponse{Error: "invalid_token"})
		return
	}

	observed := s.clientIP(r)
	addr, err := netip.ParseAddr(observed)
	if err != nil {
		writeJSONResponse(w, http.StatusBadRequest, errorResponse{Error: "unknown_source_address"})
		return
	}

	// The body is validated first so a malformed request cannot burn the caller's
	// probe budget.
	if !s.probeLimiter.allow("probe:"+observed, s.now()) {
		writeJSONResponse(w, http.StatusTooManyRequests, errorResponse{Error: "rate_limited"})
		return
	}

	target := net.UDPAddrFromAddrPort(netip.AddrPortFrom(addr.Unmap(), uint16(request.Port)))
	conn, err := net.DialUDP("udp", nil, target)
	if err != nil {
		s.logger.Debug("probe dial failed", "error", err)
		writeJSONResponse(w, http.StatusBadGateway, errorResponse{Error: "probe_failed"})
		return
	}
	defer conn.Close()

	if err := conn.SetWriteDeadline(time.Now().Add(probeWriteTimeout)); err != nil {
		writeJSONResponse(w, http.StatusBadGateway, errorResponse{Error: "probe_failed"})
		return
	}
	if _, err := conn.Write(token); err != nil {
		s.logger.Debug("probe send failed", "error", err)
		writeJSONResponse(w, http.StatusBadGateway, errorResponse{Error: "probe_failed"})
		return
	}

	s.logger.Info("reachability probe sent", "port", request.Port)
	writeJSONResponse(w, http.StatusOK, probeResponse{Sent: true, ObservedIP: observed})
}

// probeTokenBytes is the exact size of a probe token; it is echoed verbatim in
// the datagram so the relay can match the reply to its request.
const probeTokenBytes = 32
