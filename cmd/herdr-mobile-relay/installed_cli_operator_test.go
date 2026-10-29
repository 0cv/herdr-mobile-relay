package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/0cv/herdr-mobile-relay/internal/app"
	"github.com/0cv/herdr-mobile-relay/internal/config"
)

type installedCancelSpoof struct{}

func (installedCancelSpoof) Error() string { return "private-canary" }
func (installedCancelSpoof) Is(error) bool { return true }

type installedEmptyTree struct{}

func (installedEmptyTree) Error() string   { return "private-canary" }
func (installedEmptyTree) Unwrap() []error { return nil }

func TestInstalledCommandCancellationTree(t *testing.T) {
	t.Run("root-deadline", func(t *testing.T) {
		ctx, cancel := context.WithDeadline(context.Background(), time.Unix(1, 0))
		defer cancel()
		for _, cause := range []error{nil, context.Canceled, context.DeadlineExceeded} {
			code, err := installedCommandResult(ctx, cause)
			if code != 1 || err == nil || err.Error() != "installed Tailscale CLI startup refused" {
				t.Fatal("root deadline suppressed or leaked")
			}
		}
	})
	for _, tc := range []struct {
		name     string
		err      error
		graceful bool
	}{
		{"nil", nil, true},
		{"cancel", context.Canceled, true},
		{"wrapped", fmt.Errorf("private-canary: %w", context.Canceled), true},
		{"joined-cancel", errors.Join(context.Canceled, fmt.Errorf("wrapped: %w", context.Canceled)), true},
		{"unknown", errors.New("/private-canary/token"), false},
		{"joined-unknown", errors.Join(context.Canceled, errors.New("/private-canary/token")), false},
		{"nested-cleanup", fmt.Errorf("private: %w", errors.Join(context.Canceled, errors.New("cleanup uncertain"))), false},
		{"deadline", errors.Join(context.Canceled, context.DeadlineExceeded), false},
		{"certificate", errors.Join(context.Canceled, errors.New("certificate unavailable")), false},
		{"spoofed-is", installedCancelSpoof{}, false},
		{"empty-tree", installedEmptyTree{}, false},
		{"uncertainty", errors.Join(context.Canceled, errors.New("uncertain publication")), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			cancel()
			code, err := installedCommandResult(ctx, tc.err)
			if tc.graceful {
				if code != 0 || err != nil {
					t.Fatal("cancellation-only refused")
				}
			} else if code != 1 || err == nil || err.Error() != "installed Tailscale CLI startup refused" {
				t.Fatal("failure hidden or raw error leaked")
			}
			if tc.err != nil {
				code, err = installedCommandResult(context.Background(), tc.err)
				if code != 1 || err == nil {
					t.Fatal("inner cancellation treated as root stop")
				}
			}
		})
	}
}

func TestInstalledSetupCommandRefusals(t *testing.T) {
	for _, mode := range []string{"arguments", "transport", "production", "rearm", "development"} {
		t.Run(mode, func(t *testing.T) {
			root := installedCLICommandEnv(t)
			args := []string{"setup-installed"}
			want := 1
			if mode == "arguments" {
				args = append(args, "private-canary")
				want = 2
			}
			if mode == "rearm" {
				t.Setenv("HERDR_RELAY_REARM_BOOTSTRAP", "private-canary")
			}
			if mode == "development" {
				t.Setenv("HERDR_TAILSCALE_CLI_SCOPE", "development")
			}
			if mode == "transport" {
				t.Setenv("HERDR_RELAY_TRANSPORT", string(config.TransportCloudflare))
			}
			code, err := runCommand(args, func(*app.Server, context.Context) error { t.Fatal("legacy runner"); return nil }, func(context.Context) error { t.Fatal("sweep"); return nil })
			if code != want || err == nil {
				t.Fatal("setup refusal missing")
			}
			if mode == "rearm" && err.Error() != "installed service refuses bootstrap re-enrollment policy" {
				t.Fatal("policy error changed or leaked")
			}
			if mode == "development" && err.Error() != "invalid installed service configuration" {
				t.Fatal("scope error changed or leaked")
			}
			if mode == "production" && err != errInstalledCLIUnavailable {
				t.Fatal("gate missing")
			}
			for _, leaf := range []string{"config", "cache", "registration", "coordination"} {
				if _, err := os.Lstat(filepath.Join(root, leaf)); !os.IsNotExist(err) {
					t.Fatal("refusal constructed resource")
				}
			}
		})
	}
}
