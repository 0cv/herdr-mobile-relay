package app

import (
	"context"
	"io"
	"log/slog"
	"path/filepath"
	"strings"
	"testing"

	"github.com/0cv/herdr-mobile-relay/internal/config"
	"github.com/0cv/herdr-mobile-relay/internal/transport"
)

func TestArmBootstrapInvitationPairsOneMoreDevice(t *testing.T) {
	token := strings.Repeat("k", 32)
	root := t.TempDir()
	runtimeDir := filepath.Join(root, "runtime")
	enrollBootstrapDevice(t, runtimeDir, token)
	cfg := &config.Config{
		Token:      token,
		RuntimeDir: runtimeDir,
		CacheDir:   filepath.Join(root, "cache"),
	}
	s := New(cfg, "0.9.0", "abc123", slog.New(slog.NewTextHandler(io.Discard, nil)))
	if s.deviceAuth == nil {
		t.Fatal("device store was not initialized")
	}
	if _, err := s.deviceAuth.ResolveE2EESecret(context.Background(), transport.E2EEAuthSelector{
		Kind: transport.E2EEAuthInvitation, ID: "bootstrap", Version: 1,
	}); err == nil {
		t.Fatal("a consumed bootstrap still resolved before the re-arm")
	}

	if got := s.armBootstrapInvitation(); got != "armed for one more device" {
		t.Fatalf("armBootstrapInvitation() = %q", got)
	}
	if _, err := s.deviceAuth.CompleteE2EEAuth(context.Background(), transport.E2EEAuthSelector{
		Kind: transport.E2EEAuthInvitation, ID: "bootstrap", Version: 1,
	}, true); err != nil {
		t.Fatalf("second device could not pair after the re-arm: %v", err)
	}
	if got := len(s.deviceAuth.ListCredentials("")); got != 2 {
		t.Fatalf("paired devices = %d, want the first one kept plus the new one", got)
	}
}
