package transport

import (
	"crypto/hmac"
	"net/http"
	"strings"

	"github.com/0cv/herdr-mobile-relay/internal/config"
)

func Authenticate(cfg *config.Config, r *http.Request) bool {
	if !originAllowed(cfg, r) {
		return false
	}
	if cfg.Token == "" {
		return true
	}
	return tokenMatches(cfg.Token, requestToken(r))
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

func tokenMatches(expected, provided string) bool {
	if expected == "" || provided == "" {
		return false
	}
	return hmac.Equal([]byte(expected), []byte(provided))
}

func requestToken(r *http.Request) string {
	auth := r.Header.Get("Authorization")
	if auth != "" {
		if strings.HasPrefix(auth, "Bearer ") {
			return strings.TrimPrefix(auth, "Bearer ")
		}
		return auth
	}
	return r.URL.Query().Get("token")
}
