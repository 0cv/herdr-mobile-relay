package app

import (
	"context"
	"errors"
	"io"
	"net/http"

	"github.com/0cv/herdr-mobile-relay/internal/config"
	"github.com/0cv/herdr-mobile-relay/internal/tailscalecli"
)

// Both installed loopback identity and CLI HTTPS admission retain body-read
// evidence. An interrupted body is not by itself a malformed identity.
func decodeInstalledHealth(ctx context.Context, body io.Reader, destination any) error {
	if err := decodeManagedHealth(body, destination); err != nil {
		return redactInstalledEvidence(errors.Join(err, ctx.Err()))
	}
	return nil
}

type installedLocalHealth struct {
	Status    string `json:"status"`
	Readiness string `json:"readiness"`
	Transport string `json:"transport"`
	Instance  string `json:"instance"`
	Origin    string `json:"tailscale_cli_origin"`
	Run       string `json:"tailscale_cli_control_run_id"`
	Version   string `json:"version"`
	Revision  string `json:"revision"`
}

func (s *Server) matchesInstalledLocalHealth(h installedLocalHealth) bool {
	return s.matchesInstalledLocalIdentity(h) && h.Readiness == "ready"
}

func (s *Server) matchesInstalledLocalIdentity(h installedLocalHealth) bool {
	run := s.controlRunID()
	return run != "" && h.Run == run && h.Instance == s.cfg.InstanceID && h.Origin == s.cfg.TailscaleCLIOrigin && h.Transport == config.TransportTailscaleCLI && h.Version == s.version && h.Revision == s.revision && h.Status == "ok"
}

func (s *Server) checkInstalledLocalHealth(ctx context.Context) error {
	call, cancel := context.WithTimeout(ctx, managedHealthTimeout)
	defer cancel()
	request, err := http.NewRequestWithContext(call, http.MethodGet, "http://"+s.cfg.Addr()+"/healthz", nil)
	if err != nil {
		return err
	}
	response, err := managedHealthClientForServer(s, managedHealthTimeout).Do(request)
	if err != nil {
		return redactInstalledEvidence(errors.Join(err, call.Err()))
	}
	defer response.Body.Close()
	if response.StatusCode == http.StatusBadGateway || response.StatusCode == http.StatusServiceUnavailable || response.StatusCode == http.StatusGatewayTimeout {
		return tailscalecli.ErrTransientUnavailable
	}
	var health installedLocalHealth
	if response.StatusCode != http.StatusOK || response.Header.Get("X-Herdr-Relay-Instance") != s.cfg.InstanceID {
		return errors.New("installed local identity response was invalid")
	}
	if err := decodeInstalledHealth(call, response.Body, &health); err != nil {
		return err
	}
	if !s.matchesInstalledLocalIdentity(health) {
		return errors.New("installed local identity did not match")
	}
	if !s.matchesInstalledLocalHealth(health) {
		return tailscalecli.ErrTransientUnavailable
	}
	return call.Err()
}
