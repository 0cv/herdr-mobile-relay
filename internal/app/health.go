package app

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"github.com/0cv/herdr-mobile-relay/internal/protocol"
)

func canonicalHTTPPath(raw string) bool {
	if raw == "" || raw == "/" {
		return true
	}
	if !strings.HasPrefix(raw, "/") || strings.Contains(raw, "\\") || strings.Contains(raw, "\x00") {
		return false
	}
	for _, segment := range strings.Split(strings.TrimPrefix(raw, "/"), "/") {
		if segment == "" || segment == "." || segment == ".." {
			return false
		}
	}
	return true
}

func (s *Server) handleRoot(w http.ResponseWriter, r *http.Request) {
	if strings.EqualFold(r.Header.Get("Upgrade"), "websocket") {
		s.hub.HandleWebSocket(w, r)
		return
	}
	if s.webH != nil {
		s.webH.ServeHTTP(w, r)
		return
	}
	http.NotFound(w, r)
}

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("X-Herdr-Relay-Instance", s.cfg.InstanceID)
	fmt.Fprint(w, "ok\n")
}

func (s *Server) handleHealthz(w http.ResponseWriter, r *http.Request) {
	s.mu.RLock()
	ready := s.ready
	s.mu.RUnlock()

	inventory := s.state.InventoryStatus()
	delete(inventory, "message")

	readiness := "starting"
	if ready {
		switch inventory["state"] {
		case "ready":
			readiness = "ready"
		case "error":
			readiness = "degraded"
		}
	}

	resp := s.healthPayload(inventory)
	resp["status"] = "ok"
	resp["readiness"] = readiness
	gateway := s.hybrid.status()
	resp["gateway"] = gateway
	resp["gateway_url"] = gateway["url"]
	resp["gateway_version"] = gateway["version"]
	resp["gateway_revision"] = gateway["revision"]
	resp["gateway_available_version"] = s.gatewayAvailableVersion()

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(resp)
}

func (s *Server) handleReadyz(w http.ResponseWriter, r *http.Request) {
	s.mu.RLock()
	ready := s.ready
	s.mu.RUnlock()

	inventory := s.state.InventoryStatus()
	delete(inventory, "message")

	status := "unavailable"
	code := http.StatusServiceUnavailable
	if ready && inventory["state"] == "ready" {
		status = "ready"
		code = http.StatusOK
	}

	resp := s.healthPayload(inventory)
	resp["status"] = status

	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(resp)
}

func (s *Server) healthPayload(inventory map[string]any) map[string]any {
	resp := map[string]any{
		"inventory":       inventory,
		"instance":        s.cfg.InstanceID,
		"version":         s.version,
		"release_version": s.version,
		"revision":        s.revision,
		"protocol":        protocol.Version,
	}
	if s.webH != nil {
		resp["bundle_hash"] = s.webH.BundleHash()
		resp["bundle_version"] = s.webH.BundleVersion()
		resp["bundle_revision"] = s.webH.BundleRevision()
		resp["bundle_build"] = s.webH.BundleBuild()
	}
	return resp
}

func componentState(available bool) string {
	if available {
		return "running"
	}
	return "unavailable"
}
