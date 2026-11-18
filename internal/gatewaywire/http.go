package gatewaywire

import (
	"errors"
	"fmt"
	"strings"
)

const (
	RelayPath            = "/relay"
	ConnectPath          = "/connect"
	HealthPath           = "/healthz"
	ProbePath            = "/probe"
	ProbeTokenBytes      = 32
	MinProbePort         = 1024
	MaxProbeBody         = 1 << 10
	MaxHTTPResponseBytes = 4 << 10
)

// ProbeRequest asks the gateway to send one UDP datagram back to the caller so a
// relay can prove that a port mapping works from outside its own network.
//
// There is deliberately no destination address field: the datagram always goes
// to the source address the gateway observed. Any other field in the body is
// ignored, which makes the endpoint useless for amplification or reflection.
type ProbeRequest struct {
	Port  int    `json:"port"`
	Token string `json:"token"`
}

type ProbeResponse struct {
	Sent       bool   `json:"sent"`
	ObservedIP string `json:"observed_ip"`
}

type ErrorResponse struct {
	Error string `json:"error"`
}

type HealthResponse struct {
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

func HTTPBase(gatewayURL string) (string, error) {
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
