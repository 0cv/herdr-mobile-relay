//go:build !herdr_installed_inert_test

package main

import (
	"context"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/0cv/herdr-mobile-relay/internal/config"
	"github.com/0cv/herdr-mobile-relay/internal/readiness"
	"github.com/0cv/herdr-mobile-relay/internal/release"
	"github.com/0cv/herdr-mobile-relay/internal/tailscalecli"
)

// The immutable selection still serves its original web bundle. Resolve and
// verify it before a stop as well as after restart; no ambient current alias.
func promotionOriginalWeb(cfg config.Config, manifest release.Manifest) (release.Manifest, error) {
	if filepath.Base(cfg.WebRoot) != "web" {
		return release.Manifest{}, errPromotion
	}
	physicalWeb, err := filepath.EvalSymlinks(cfg.WebRoot)
	if err != nil || physicalWeb != cfg.WebRoot || !strings.HasPrefix(cfg.WebRoot, cfg.ReleaseRoot+string(filepath.Separator)) {
		return release.Manifest{}, errPromotion
	}
	webRelease, err := release.Verify(filepath.Dir(cfg.WebRoot), release.CurrentTarget())
	if err != nil || release.ValidateUpgradeCompatibility(webRelease, manifest) != nil {
		return release.Manifest{}, errPromotion
	}
	return webRelease, nil
}

func promotionBackendEligible(cfg config.Config, manifest release.Manifest) error {
	_, err := promotionOriginalWeb(cfg, manifest)
	return err
}

// Exact loopback release identity is required in addition to native state and
// the owned Serve journal. Neither a matching route nor a loaded job is ready.
func promotionBackendReady(ctx context.Context, cfg config.Config, manifest release.Manifest) error {
	if ctx == nil || ctx.Err() != nil || cfg.Port < 1 || cfg.Port > 65535 || cfg.InstanceID == "" {
		return errPromotion
	}
	webRelease, err := promotionOriginalWeb(cfg, manifest)
	if err != nil {
		return errPromotion
	}
	budget, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	transport := &http.Transport{Proxy: nil}
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: transport, Timeout: 2 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	for budget.Err() == nil {
		call, stop := context.WithTimeout(budget, 2*time.Second)
		request, err := http.NewRequestWithContext(call, http.MethodGet, "http://127.0.0.1:"+strconv.Itoa(cfg.Port)+"/readyz", nil)
		if err == nil {
			var response *http.Response
			response, err = client.Do(request)
			if err == nil {
				if response.StatusCode == http.StatusOK {
					err = readiness.Verify(response.Body, readiness.Expected{Instance: cfg.InstanceID, Version: manifest.Version, Revision: manifest.Revision, WebHash: webRelease.WebHash})
				} else {
					err = errPromotion
				}
				if response.Body.Close() != nil {
					err = errPromotion
				}
			}
		}
		stop()
		if err == nil {
			return nil
		}
		select {
		case <-budget.Done():
			return errPromotion
		case <-time.After(200 * time.Millisecond):
		}
	}
	return errPromotion
}

func promotionRouteQualification(_ *tailscalecli.Manager, route tailscalecli.RouteStatus) bool {
	return route.RuntimeQualified
}

func ownedPromotionDir(info os.FileInfo) bool {
	stat, ok := info.Sys().(*syscall.Stat_t)
	return ok && stat.Uid == uint32(os.Geteuid())
}

func installedPromotionNative(selected config.InstalledConfig) (promotionNative, error) {
	binary, err := os.Executable()
	if err != nil {
		return nil, errPromotion
	}
	binary, err = filepath.EvalSymlinks(binary)
	if err != nil {
		return nil, errPromotion
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, errPromotion
	}
	root := filepath.Dir(binary)
	script := filepath.Join(root, "relay", "native-cli-fresh.sh")
	if runtime.GOOS != "darwin" && runtime.GOOS != "linux" {
		return nil, errPromotion
	}
	var definition, legacy string
	if runtime.GOOS == "darwin" {
		definition = filepath.Join(home, "Library/LaunchAgents/com.herdr-mobile-relay.service.plist")
		legacy = filepath.Join(home, "Library/LaunchAgents/com.herdr-remote.service.plist")
	} else {
		definition = filepath.Join(home, ".config/systemd/user/herdr-mobile-relay.service")
		legacy = filepath.Join(home, ".config/systemd/user/herdr-remote.service")
	}
	// The physical executable and its adjacent driver, not current or PATH, are
	// selected once. The production gate is checked before constructing this.
	if selected.Snapshot().ReleaseRoot == "" || !strings.HasPrefix(binary, selected.Snapshot().ReleaseRoot+string(filepath.Separator)) {
		return nil, errPromotion
	}
	if info, err := os.Lstat(script); err != nil || !info.Mode().IsRegular() || info.Mode()&0111 == 0 {
		return nil, errPromotion
	}
	return &nativeCLIFresh{definition: definition, legacy: legacy, executable: binary,
		claim: filepath.Join(home, ".local/state/herdr-mobile-relay/recovery/.herdr-native-claim-v1"), driver: func(ctx context.Context, action string) error {
			call, cancel := context.WithTimeout(ctx, 15*time.Second)
			defer cancel()
			cmd := exec.CommandContext(call, script, action)
			cmd.Stdin = nil
			cmd.Stdout = io.Discard
			cmd.Stderr = io.Discard
			if cmd.Run() != nil || call.Err() != nil {
				return errPromotion
			}
			return nil
		}}, nil
}
