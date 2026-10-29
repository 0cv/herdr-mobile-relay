package tailscalecli

import (
	"context"
	"os"
	"sync"
)

// InstalledSession is minted only while the manager owns the registered
// instance/backend. Its zero value and retained copies authorize nothing.
type InstalledSession struct {
	mu                                 sync.Mutex
	binding                            InstalledBinding
	metadata                           InstalledOwnerMetadata
	manager                            *Manager
	check                              func(context.Context) (RouteStatus, error)
	installationID, origin             string
	httpsPort, backendPort, pluginPort int
}

func (m *Manager) WithInstalledSession(ctx context.Context, binding InstalledBinding, run func(context.Context, *InstalledSession) error) error {
	if err := binding.Validate(); err != nil {
		return err
	}
	in := binding.input
	if m == nil || m.client == nil || run == nil || binding.roots[0] != m.stateRoot || binding.roots[1] != m.coordinationRoot || in.Binary != m.client.binary {
		return ErrWorkflowRequired
	}
	return m.waitInstalledStartup(ctx, binding, func(ctx context.Context, check func(context.Context) (RouteStatus, error), metadata InstalledOwnerMetadata) error {
		if err := binding.Validate(); err != nil {
			return err
		}
		s := &InstalledSession{metadata: metadata, binding: binding, manager: m, check: check, installationID: in.InstallationID, origin: in.Origin, httpsPort: in.HTTPSPort, backendPort: in.BackendPort, pluginPort: in.PluginPort}
		defer func() { s.mu.Lock(); defer s.mu.Unlock(); s.check = nil }()
		return run(ctx, s)
	})
}

// InstalledOwnerMetadata is pinned registered-owner metadata, not admission.
type InstalledOwnerMetadata struct {
	NodeID  string
	Profile Profile
}

func (s *InstalledSession) Metadata(binding InstalledBinding) (InstalledOwnerMetadata, error) {
	if s == nil {
		return InstalledOwnerMetadata{}, ErrWorkflowRequired
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.check == nil || s.manager == nil || binding != s.binding {
		return InstalledOwnerMetadata{}, ErrWorkflowRequired
	}
	if err := binding.Validate(); err != nil {
		return InstalledOwnerMetadata{}, err
	}
	return s.metadata, nil
}

func (s *InstalledSession) ValidateRuntimeBinding(binding InstalledBinding) error {
	if s == nil {
		return ErrWorkflowRequired
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.check == nil || s.manager == nil || binding != s.binding {
		return ErrWorkflowRequired
	}
	return binding.Validate()
}

// PrepareRuntime creates only missing application leaves while lifetime
// ownership is held. Existing unsafe metadata is refused, never repaired.
func (s *InstalledSession) PrepareRuntime(binding InstalledBinding) error {
	if s == nil {
		return ErrWorkflowRequired
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.check == nil || s.manager == nil || binding != s.binding {
		return ErrWorkflowRequired
	}
	if err := binding.Validate(); err != nil {
		return err
	}
	for _, root := range []string{binding.input.RuntimeRoot, binding.input.CacheRoot} {
		if err := os.MkdirAll(root, 0700); err != nil {
			return err
		}
	}
	return binding.Validate()
}

// VerifyRegisteredRoute honors both the call and service lifetime. A nil call
// context means no additional deadline; retirement still cancels and drains it.
func (s *InstalledSession) VerifyRegisteredRoute(ctx context.Context, scope, installationID, origin string, httpsPort, backendPort int) (RouteStatus, error) {
	if s == nil {
		return RouteStatus{}, ErrWorkflowRequired
	}
	s.mu.Lock()
	check := s.check
	s.mu.Unlock()
	if check == nil || scope != "production" || installationID != s.installationID || origin != s.origin || httpsPort != s.httpsPort || backendPort != s.backendPort {
		return RouteStatus{}, ErrWorkflowRequired
	}
	return check(ctx)
}
