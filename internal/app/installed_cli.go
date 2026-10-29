package app

import (
	"context"
	"errors"
	"log/slog"

	"github.com/0cv/herdr-mobile-relay/internal/config"
	"github.com/0cv/herdr-mobile-relay/internal/tailscalecli"
)

// RunInstalledCLI owns construction as well as Run. No server/device writer is
// returned to the caller. Stop preserves registration and device credentials.
// The caller's original selection survives through session acquisition; never
// rebind its pathnames to newly resolved application roots here.
func RunInstalledCLI(ctx context.Context, selected config.InstalledConfig, version, revision string, logger *slog.Logger, manager *tailscalecli.Manager) error {
	return runInstalledCLI(ctx, selected, version, revision, logger, manager, nil)
}

func runInstalledCLI(ctx context.Context, selected config.InstalledConfig, version, revision string, logger *slog.Logger, manager *tailscalecli.Manager, operator InstalledOperator) error {
	if ctx == nil || manager == nil || selected.Binding() == (tailscalecli.InstalledBinding{}) {
		return tailscalecli.ErrWorkflowRequired
	}
	snapshot := selected.Snapshot()
	cfg := &snapshot
	return manager.WithInstalledSession(ctx, selected.Binding(), func(ctx context.Context, authority *tailscalecli.InstalledSession) error {
		if err := selected.Validate(cfg); err != nil {
			return err
		}
		if err := authority.ValidateRuntimeBinding(selected.Binding()); err != nil {
			return err
		}
		if err := authority.PrepareRuntime(selected.Binding()); err != nil {
			return err
		}
		run := func(s *Server, ctx context.Context) error {
			if err := s.initializeInstalledGeneration(); err != nil {
				return err
			}
			return installedCLIRunner()(s, ctx)
		}
		if operator != nil {
			run = func(s *Server, ctx context.Context) error {
				return s.runInstalledOperator(ctx, selected, operator)
			}
		}
		return runInstalledCLIApp(ctx, cfg, version, revision, logger, authority, run)
	})
}

// The private runner seam lets inert tests stop before Run's external effects.
// Production has exactly one caller, above, using the concrete session and Run.
func runInstalledCLIApp(ctx context.Context, cfg *config.Config, version, revision string, logger *slog.Logger, authority *tailscalecli.InstalledSession, run func(*Server, context.Context) error) (result error) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	s := newServerWithResourcesIn(ctx, cfg, version, revision, logger, nil, nil, installedServiceStartup, cliInstalledUploadResources)
	if s.initErr != nil && s.hub == nil {
		return s.initErr
	}
	s.installedCLI = authority
	s.tailscaleCLIRegistration = authority
	defer func() {
		s.installedUpload.begin()
		cancel()
		s.quarantineTailscaleCLI()
		_ = s.hub.Shutdown(context.Background())
		if s.conversationB != nil {
			result = errors.Join(result, s.conversationB.Close())
		}
		result = errors.Join(result, s.collectInstalledConversation())
		result = s.installedUpload.result(result)
	}()
	if s.initErr != nil {
		return s.initErr
	}
	return run(s, ctx)
}
