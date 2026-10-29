package tailscalecli

import (
	"context"

	"github.com/0cv/herdr-mobile-relay/internal/tailscale"
)

// Independent package-level fail-closed gate: a direct constructor call must
// never bypass the command/config activation decision. M4 must review this and
// the profile-specific runtime qualification together, not flip a test flag.
const installedProductionActivated = false

// PreflightInstalledBinary reads the selected CLI's identity without selecting
// or publishing a route. Both production gates and the actual runtime profile
// must admit it before the installed chooser can persist a selection.
func PreflightInstalledBinary(ctx context.Context, binary string, httpsPort, backendPort int) (PreflightReport, error) {
	if !installedProductionActivated {
		return PreflightReport{}, ErrUnsupported
	}
	if httpsPort < 1 || httpsPort > 65535 || backendPort < 1 || backendPort > 65535 || httpsPort == backendPort {
		return PreflightReport{}, ErrConflict
	}
	client, err := newClient(binary)
	if err != nil || client.binary != binary {
		return PreflightReport{}, ErrProfileUnavailable
	}
	inspection, err := client.Inspect(ctx)
	if err != nil {
		return PreflightReport{}, err
	}
	if !inspection.RuntimeQualified || routeConflicts(inspection.Serve, httpsPort) || backendPortHasRoute(inspection.Serve, backendPort, nil) {
		return PreflightReport{}, ErrConflict
	}
	origin, err := tailscale.Origin(inspection.Identity.DNSName, httpsPort)
	if err != nil {
		return PreflightReport{}, ErrUnsupported
	}
	return PreflightReport{
		NodeID: inspection.Identity.NodeID, DNSName: inspection.Identity.DNSName,
		Origin: origin, Profile: inspection.Profile, HTTPSPort: httpsPort,
		RuntimeQualified: inspection.RuntimeQualified,
	}, nil
}

// NewInstalledProductionManager is reached only after the separate compiled
// activation decision in the installed command. It still requires the exact
// inspected runtime profile to be qualified on every operation. Shipped builds
// have no runtime-qualified CLI profile; fixture managers are separate.
func NewInstalledProductionManager(binding InstalledBinding) (*Manager, error) {
	if !installedProductionActivated {
		return nil, ErrUnsupported
	}
	if err := binding.Validate(); err != nil {
		return nil, err
	}
	client, err := newClient(binding.input.Binary)
	if err != nil {
		return nil, err
	}
	if client.binary != binding.input.Binary {
		return nil, ErrConflict
	}
	m, err := NewManager(binding.roots[0], binding.roots[1], client)
	if err != nil {
		return nil, err
	}
	m.installedProduction = true
	return m, nil
}

// NewInstalledManager uses only the selected absolute executable and existing
// private roots. It neither creates state nor searches PATH. Production scope
// remains closed independently of the command's profile activation gate.
func NewInstalledManager(binding InstalledBinding) (*Manager, error) {
	if err := binding.Validate(); err != nil {
		return nil, err
	}
	m := &Manager{}
	if err := m.requireDevelopmentScope(binding.input.Scope); err != nil {
		return nil, err
	}
	client, err := newClient(binding.input.Binary)
	if err != nil {
		return nil, err
	}
	// Do not silently replace the snapshot's executable identity with an alias.
	if client.binary != binding.input.Binary {
		return nil, ErrConflict
	}
	return NewManager(binding.roots[0], binding.roots[1], client)
}
