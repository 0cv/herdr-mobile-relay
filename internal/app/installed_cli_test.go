package app

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/0cv/herdr-mobile-relay/internal/config"
	"github.com/0cv/herdr-mobile-relay/internal/tailscalecli"
)

func TestInstalledCLIRefusesBeforeConstruction(t *testing.T) {
	root := t.TempDir()
	cfg := &config.Config{Transport: config.TransportTailscaleCLI, RuntimeDir: filepath.Join(root, "must-not-exist")}
	if err := RunInstalledCLI(context.Background(), config.InstalledConfig{}, "", "", nil, nil); !errors.Is(err, tailscalecli.ErrWorkflowRequired) {
		t.Fatal(err)
	}
	if err := RunInstalledCLI(context.Background(), config.InstalledConfig{}, "", "", nil, &tailscalecli.Manager{}); !errors.Is(err, tailscalecli.ErrWorkflowRequired) {
		t.Fatal(err)
	}
	if _, err := os.Stat(cfg.RuntimeDir); !os.IsNotExist(err) {
		t.Fatalf("construction occurred: %v", err)
	}
}
