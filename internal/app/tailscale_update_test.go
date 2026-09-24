package app

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/0cv/herdr-mobile-relay/internal/config"
)

// Exercise the same narrow constructor used by App.New, without app startup,
// device stores, sockets, clipboard/speech probes or any update operations.
func TestTailscaleS3AppPolicy(t *testing.T) {
	for _, policy := range []string{"tailscale", "cloudflare", "gateway", "", "unknown"} {
		for _, raw := range []string{"", "cloudflare", "gateway", "tailscale"} {
			t.Run(policy+"/"+raw, func(t *testing.T) {
				t.Setenv("HERDR_RELAY_TRANSPORT", raw)
				root := t.TempDir()
				bin := filepath.Join(root, "herdr")
				if err := os.WriteFile(bin, []byte("private stat-only fixture"), 0700); err != nil {
					t.Fatal(err)
				}
				runtime := filepath.Join(root, "runtime")
				if err := os.Mkdir(runtime, 0700); err != nil {
					t.Fatal(err)
				}
				statePath := filepath.Join(runtime, "update-state.json")
				before := []byte(`{"state":"available","eligible":true,"can_install":true,"available_version":"1.2.4","target_revision":"89abcdef0123456789abcdef0123456789abcdef"}`)
				if err := os.WriteFile(statePath, before, 0600); err != nil {
					t.Fatal(err)
				}
				cfg := &config.Config{Transport: policy, ReleaseRoot: filepath.Join(root, "release"), RuntimeDir: runtime, HerdrBin: bin}
				manager := newUpdateManager(cfg, "1.2.3", "0123456789abcdef0123456789abcdef01234567", "http://127.0.0.1/healthz")
				state := manager.State()
				admitted := (policy == "cloudflare" || policy == "gateway") && (raw == "" || raw == policy)
				if admitted {
					if !state.Eligible || !state.CanInstall || state.Mode != "plugin" {
						t.Fatalf("legacy policy rejected: %#v", state)
					}
				} else if state.Eligible || state.CanInstall || state.State != "blocked" || state.Mode != "foreground" || !strings.Contains(state.Reason, "manual") {
					t.Fatalf("resolved policy not enforced: %#v", state)
				}
				after, err := os.ReadFile(statePath)
				if err != nil || string(after) != string(before) {
					t.Error("constructor/State changed bytes")
				}
				if _, err := os.Stat(cfg.ReleaseRoot); !os.IsNotExist(err) {
					t.Error("created release/recovery paths")
				}
			})
		}
	}
}
