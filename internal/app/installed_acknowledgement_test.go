//go:build herdr_installed_inert_test

package app

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"

	"github.com/0cv/herdr-mobile-relay/internal/localcontrol"
	"github.com/0cv/herdr-mobile-relay/internal/tailscalecli"
)

func TestInstalledAcknowledgementOperatorHistory(t *testing.T) {
	m, selected, _, mutations := installedOperatorFixture(t)
	var current *Server
	SetInstalledInertRunner(t, func(s *Server, ctx context.Context) error {
		current = s
		s.installedBackend = newInstalledBackend(ctx, &installedLogicalListener{})
		defer s.installedBackend.Close()
		tailscalecli.SetInstalledSetupReadinessFixture(s.installedSetup.session, func(call context.Context) error { return call.Err() })
		close(s.listenerBound)
		<-s.installedSetup.lifetime.Done()
		return nil
	})
	err := RunInstalledSetupOperatorCLI(context.Background(), selected, "fixture", "fixture", slog.New(slog.NewTextHandler(io.Discard, nil)), m, func(ctx context.Context, c localcontrol.InstalledControl, credentials localcontrol.InstalledCredentials) error {
		devicesPath := filepath.Join(current.cfg.RuntimeDir, "device-auth", "devices.json")
		before, beforeErr := os.ReadFile(devicesPath)
		if beforeErr != nil && !os.IsNotExist(beforeErr) {
			return beforeErr
		}
		p := installedOperatorRequest(ctx, c, credentials, "proposal", nil).Proposal
		if p == nil {
			return errors.New("missing proposal")
		}
		if reply := installedOperatorRequest(ctx, c, credentials, "publish", installedOperatorConsent(*p)); reply.Publication != "acknowledged" {
			return errors.New("publish refused")
		}
		fingerprint, err := selected.Binding().Fingerprint()
		if err != nil {
			return err
		}
		check := func() error {
			v, err := c.AcknowledgedPublication(ctx)
			if err != nil {
				return err
			}
			d := v.Snapshot()
			if d.Selection != fingerprint || d.Profile != p.Profile || d.Generation != credentials.Run || d.Attempt != p.Attempt || d.Installation != credentials.Installation {
				return errors.New("correlation mismatch")
			}
			if current.bootstrapGate.OpenStatus() {
				return errors.New("historical data opened admission")
			}
			return nil
		}
		if err := check(); err != nil {
			return err
		}
		current.mu.Lock()
		current.quarantined = true
		current.mu.Unlock()
		if err := check(); err != nil {
			return err
		}
		current.mu.Lock()
		current.installedTerminal = true
		current.mu.Unlock()
		if v, err := c.AcknowledgedPublication(ctx); err == nil || v != (tailscalecli.AcknowledgedPublication{}) {
			return errors.New("terminal data accepted")
		}
		current.mu.Lock()
		current.installedTerminal = false
		current.mu.Unlock()
		after, afterErr := os.ReadFile(devicesPath)
		if (afterErr != nil && !os.IsNotExist(afterErr)) || os.IsNotExist(beforeErr) != os.IsNotExist(afterErr) || !bytes.Equal(before, after) {
			return errors.New("publication/getter changed devices")
		}
		original := current.cfg.Token
		current.cfg.Token = "selected-drift"
		defer func() { current.cfg.Token = original }()
		if v, err := c.AcknowledgedPublication(ctx); err == nil || v != (tailscalecli.AcknowledgedPublication{}) {
			return errors.New("selected drift accepted")
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if mutations() != 1 {
		t.Fatal("getter mutated CLI")
	}
}
