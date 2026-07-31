package transport

import (
	"net/http"
	"strings"

	"github.com/0cv/herdr-mobile-relay/internal/config"
)

func webSocketUpgradeAllowed(cfg *config.Config, r *http.Request) bool {
	if !originAllowed(cfg, r) {
		return false
	}
	if cfg.Token == "" {
		return true
	}
	if r.Header.Get("Authorization") != "" || r.URL.Query().Has("token") {
		return false
	}
	for _, requested := range strings.Split(r.Header.Get("Sec-WebSocket-Protocol"), ",") {
		if strings.TrimSpace(requested) == e2eeSubprotocol {
			return true
		}
	}
	return false
}

func originAllowed(cfg *config.Config, r *http.Request) bool {
	origin := r.Header.Get("Origin")
	if origin == "" {
		return true
	}
	if cfg.Token != "" {
		return true
	}
	for _, allowed := range cfg.AllowedOrigins {
		if allowed == "*" || allowed == origin {
			return true
		}
	}
	return false
}
