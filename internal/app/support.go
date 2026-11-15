package app

import (
	"context"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/0cv/herdr-mobile-relay/internal/coordinator"
	"github.com/0cv/herdr-mobile-relay/internal/protocol"
	"github.com/0cv/herdr-mobile-relay/internal/support"
)

func (s *Server) storePhoneAppOrigin(raw string) error {
	if raw == "" {
		return fmt.Errorf("origin is required")
	}
	parsed, err := url.Parse(raw)
	if err != nil || parsed.Scheme != "https" || parsed.Host == "" || parsed.User != nil {
		return fmt.Errorf("origin must be an HTTPS origin")
	}
	if parsed.Path != "" && parsed.Path != "/" || parsed.RawQuery != "" || parsed.Fragment != "" {
		return fmt.Errorf("origin must not contain a path, query, or fragment")
	}
	origin := "https://" + parsed.Host
	if err := os.MkdirAll(s.cfg.RuntimeDir, 0o700); err != nil {
		return err
	}
	path := filepath.Join(s.cfg.RuntimeDir, "phone-app-origin")
	temp := path + ".tmp"
	if err := os.WriteFile(temp, []byte(origin+"\n"), 0o600); err != nil {
		return err
	}
	return os.Rename(temp, path)
}

func (s *Server) writeSupportLoop(ctx context.Context) {
	s.writeSupportSnapshot()
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			s.writeSupportSnapshot()
			return
		case <-ticker.C:
			s.writeSupportSnapshot()
		}
	}
}

func (s *Server) writeSupportSnapshot() {
	if s.cfg.RuntimeDir == "" {
		return
	}
	readiness := "starting"
	if s.state.InventoryReady() {
		readiness = "ready"
	}
	releaseDirectory := ""
	if executable, err := os.Executable(); err == nil {
		releaseDirectory = filepath.Dir(executable)
	}
	webHash, webVersion := "", ""
	if s.webH != nil {
		webHash = s.webH.BundleHash()
		webVersion = s.webH.BundleVersion()
	}
	metrics := coordinator.SchedulerMetrics{}
	activityFailures := uint64(0)
	if s.dispatcher != nil {
		metrics = s.dispatcher.Metrics()
		activityFailures = s.dispatcher.ActivityFailures()
	}
	udpMetrics := coordinator.UDPMetrics{}
	if s.udp != nil {
		udpMetrics = s.udp.Metrics()
	}
	snapshot := support.Snapshot{
		Version:          s.version,
		Revision:         s.revision,
		Protocol:         protocol.Version,
		ReleaseDirectory: releaseDirectory,
		WebHash:          webHash,
		WebVersion:       webVersion,
		Readiness:        readiness,
		Inventory:        s.state.InventoryStatus(),
		Components: map[string]string{
			"http":        "running",
			"poller":      "running",
			"udp":         componentState(s.udp != nil),
			"persistence": componentState(s.journal != nil),
			"push":        componentState(s.pushM != nil),
		},
		Scheduler:        metrics,
		Transport:        s.hub.Metrics(),
		UDP:              udpMetrics,
		ActivityFailures: activityFailures,
		TopologyRetries:  s.state.TopologyRetryCount(),
		PollFailures:     s.poller.ConsecutiveFailures(),
		RecentErrors:     s.recentSafeErrors(),
	}
	if err := support.Write(s.cfg.RuntimeDir, snapshot); err != nil {
		s.recordSafeError("support snapshot write failed", err)
		s.logger.Debug("support snapshot write failed", "error", err)
	}
}

func (s *Server) recordSafeError(component string, err error) {
	message := strings.Join(strings.Fields(component), " ")
	if err != nil {
		detail := strings.Join(strings.Fields(err.Error()), " ")
		runes := []rune(detail)
		if len(runes) > 300 {
			detail = string(runes[:300])
		}
		if detail != "" {
			message += ": " + detail
		}
	}
	message = time.Now().UTC().Format(time.RFC3339) + " " + message
	s.mu.Lock()
	s.errors = append(s.errors, message)
	if len(s.errors) > 20 {
		s.errors = append([]string(nil), s.errors[len(s.errors)-20:]...)
	}
	s.mu.Unlock()
}

func (s *Server) recentSafeErrors() []string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return append([]string(nil), s.errors...)
}
