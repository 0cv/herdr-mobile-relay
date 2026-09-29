package tailscalecli

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/0cv/herdr-mobile-relay/internal/tailscale"
)

const (
	journalName         = "registration.json"
	journalMaxBytes     = 16 * 1024
	mutationLockWait    = 5 * time.Second
	mutationLockDelay   = 20 * time.Millisecond
	publishConsentScope = "persistent-route-and-four-risks-v1"
	removeConsentScope  = "explicit-route-removal-and-no-remote-drain-v1"
)

type RegistrationState string

const (
	StateUnconfigured     RegistrationState = "unconfigured"
	StatePublishPending   RegistrationState = "publish-pending"
	StateRegistered       RegistrationState = "registered"
	StatePublishUncertain RegistrationState = "publish-uncertain"
	StateRemovePending    RegistrationState = "remove-pending"
	StateRemoveUncertain  RegistrationState = "remove-uncertain"
	StateRemoved          RegistrationState = "removed"
)

type Consent struct {
	Accepted                 bool
	Scope                    string
	NodeID                   string
	HTTPSPort                int
	BackendPort              int
	PersistentRouteAccepted  bool
	RouteRemovalAccepted     bool
	CheckToWriteRaceAccepted bool
	PortReuseRiskAccepted    bool
	NoRollbackAccepted       bool
	NoRemoteDrainAccepted    bool
}

type PublishRequest struct {
	InstallationID string
	Scope          string
	ExpectedNodeID string
	HTTPSPort      int
	BackendPort    int
	Consent        Consent
}

type registration struct {
	Schema               int               `json:"schema"`
	InstallationID       string            `json:"installation_id"`
	Scope                string            `json:"scope"`
	NodeID               string            `json:"node_id"`
	DNSName              string            `json:"dns_name"`
	Profile              Profile           `json:"profile"`
	BinaryPath           string            `json:"binary_path"`
	HTTPSPort            int               `json:"https_port"`
	BackendPort          int               `json:"backend_port"`
	Path                 string            `json:"path"`
	Backend              string            `json:"backend"`
	ConsentScope         string            `json:"consent_scope"`
	OperationID          string            `json:"operation_id"`
	State                RegistrationState `json:"state"`
	MutationAcknowledged bool              `json:"mutation_acknowledged"`
	UpdatedAt            string            `json:"updated_at"`
}

type RouteReadiness string

const (
	ReadinessWaiting     RouteReadiness = "waiting"
	ReadinessReady       RouteReadiness = "ready"
	ReadinessDegraded    RouteReadiness = "degraded"
	ReadinessConflicted  RouteReadiness = "conflicted"
	ReadinessUncertain   RouteReadiness = "uncertain"
	ReadinessUnqualified RouteReadiness = "unqualified"
)

// RouteStatus is a redacted read-only view of registration and observed route
// readiness. It deliberately omits account and node identifiers.
type RouteStatus struct {
	JournalState     RegistrationState `json:"journal_state"`
	Readiness        RouteReadiness    `json:"readiness"`
	Profile          Profile           `json:"profile,omitempty"`
	HTTPSPort        int               `json:"https_port,omitempty"`
	BackendPort      int               `json:"backend_port,omitempty"`
	RuntimeQualified bool              `json:"runtime_qualified"`
}

// RecoveryReport is a redacted, read-only reconciliation view. Observed route
// text is never ownership evidence and Recover never repairs or clears journal
// state, even when the observed route matches a pending operation.
type RecoveryReport struct {
	Route                  RouteStatus `json:"route"`
	Observation            string      `json:"observation"`
	RequiresOperatorAction bool        `json:"requires_operator_action"`
}

type Manager struct {
	stateRoot        string
	coordinationRoot string
	client           *Client
	fixtureMutations bool
}

// NewManager opens an adapter manager over existing private roots. It performs
// no CLI, daemon or service access and never creates or repairs either root.
func NewManager(stateRoot, coordinationRoot string, client *Client) (*Manager, error) {
	if client == nil || client.binary == "" {
		return nil, ErrProfileUnavailable
	}
	stateRoot, err := validatePrivateDirectory(stateRoot)
	if err != nil {
		return nil, fmt.Errorf("private registration root unavailable: %w", err)
	}
	coordinationRoot, err = validatePrivateDirectory(coordinationRoot)
	if err != nil {
		return nil, fmt.Errorf("private coordination root unavailable: %w", err)
	}
	if pathsOverlap(stateRoot, coordinationRoot) {
		return nil, ErrPermissionDenied
	}
	return &Manager{stateRoot: stateRoot, coordinationRoot: coordinationRoot, client: client}, nil
}

// Publish creates exactly one persistent background HTTPS proxy route. Normal
// production managers fail closed because no profile is runtime-qualified.
func (m *Manager) Publish(ctx context.Context, request PublishRequest) error {
	if err := validateRequest(request); err != nil {
		return err
	}
	inspection, err := m.client.Inspect(ctx)
	if err != nil {
		return err
	}
	if inspection.Identity.NodeID != request.ExpectedNodeID {
		return ErrConflict
	}
	if err := m.requireMutationProfile(inspection); err != nil {
		return err
	}
	return m.withNodeLock(ctx, inspection.Identity.NodeID, func() error {
		current, err := m.client.Inspect(ctx)
		if err != nil {
			return err
		}
		if !sameInspectionIdentity(inspection, current) || current.Identity.NodeID != request.ExpectedNodeID {
			return ErrConflict
		}
		if err := m.requireMutationProfile(current); err != nil {
			return err
		}
		record, err := m.readRegistration()
		if err != nil {
			return err
		}
		if record != nil && record.State == StateRegistered {
			if !sameRequest(*record, request, current, m.client.binary) {
				return ErrConflict
			}
			if registeredRouteMatches(current, *record) {
				return nil // acknowledged registration reuse: no Serve write
			}
			return ErrConflict // drift never triggers automatic repair
		}
		if record != nil && record.State != StateRemoved && record.State != StateUnconfigured {
			return fmt.Errorf("%w: recovery state %s requires explicit reconciliation", ErrUncertain, record.State)
		}
		if err := validatePublishConsent(request.Consent, request.ExpectedNodeID, request.Scope, request.HTTPSPort, request.BackendPort); err != nil {
			return err
		}
		if routeConflicts(current.Serve, request.HTTPSPort) {
			return ErrConflict
		}
		opID, err := newOperationID()
		if err != nil {
			return err
		}
		entry := registration{
			Schema:         1,
			InstallationID: request.InstallationID,
			Scope:          request.Scope,
			NodeID:         current.Identity.NodeID,
			DNSName:        current.Identity.DNSName,
			Profile:        current.Profile,
			BinaryPath:     m.client.binary,
			HTTPSPort:      request.HTTPSPort,
			BackendPort:    request.BackendPort,
			Path:           "/",
			Backend:        fmt.Sprintf("http://127.0.0.1:%d", request.BackendPort),
			ConsentScope:   publishConsentScope,
			OperationID:    opID,
			State:          StatePublishPending,
			UpdatedAt:      time.Now().UTC().Format(time.RFC3339Nano),
		}
		if err := m.writeRegistration(entry); err != nil {
			return fmt.Errorf("persist publish intent: %w", err)
		}
		result, publishErr := m.client.execute(ctx, "serve", "--bg", fmt.Sprintf("--https=%d", request.HTTPSPort), "--set-path=/", entry.Backend)
		if publishErr != nil {
			if !result.dispatched {
				entry.State = StateUnconfigured
				entry.UpdatedAt = time.Now().UTC().Format(time.RFC3339Nano)
				if saveErr := m.writeRegistration(entry); saveErr != nil {
					return fmt.Errorf("publish was not dispatched but journal update failed: %w", saveErr)
				}
				return sanitizeCommandError("serve publish", publishErr)
			}
			entry.State = StatePublishUncertain
			entry.UpdatedAt = time.Now().UTC().Format(time.RFC3339Nano)
			_ = m.writeRegistration(entry)
			return fmt.Errorf("%w: publish acknowledgement unavailable", ErrUncertain)
		}
		entry.State = StateRegistered
		entry.MutationAcknowledged = true
		entry.UpdatedAt = time.Now().UTC().Format(time.RFC3339Nano)
		if err := m.writeRegistration(entry); err != nil {
			// The durable publish-pending record remains as recovery evidence.
			return fmt.Errorf("%w: publish acknowledged but receipt could not be persisted", ErrUncertain)
		}
		readback, err := m.client.Inspect(ctx)
		if err != nil || !registeredRouteMatches(readback, entry) {
			// Keep the durable receipt. Do not retry, roll back, or issue `off`.
			return fmt.Errorf("%w: publish receipt recorded; route readback is not ready", ErrConflict)
		}
		return nil
	})
}

// VerifyRegisteredRoute performs bounded, read-only drift detection for a
// previously acknowledged registration. A matching observation without a
// valid journal is never adopted, and this method never repairs a route.
func (m *Manager) VerifyRegisteredRoute(ctx context.Context, scope, installationID string, httpsPort, backendPort int) (RouteStatus, error) {
	status := RouteStatus{JournalState: StateUnconfigured, Readiness: ReadinessWaiting}
	if ctx == nil {
		ctx = context.Background()
	}
	if (scope != "production" && scope != "development") || !validLabel(installationID) ||
		httpsPort < 1 || httpsPort > 65535 || backendPort < 1 || backendPort > 65535 || httpsPort == backendPort {
		status.Readiness = ReadinessConflicted
		return status, ErrConflict
	}
	record, err := m.readRegistration()
	if err != nil {
		status.Readiness = ReadinessUncertain
		return status, err
	}
	if record == nil {
		inspection, inspectErr := m.client.Inspect(ctx)
		if inspectErr != nil {
			status.Readiness = readinessForError(inspectErr)
			return status, inspectErr
		}
		status.Profile = inspection.Profile
		status.RuntimeQualified = inspection.RuntimeQualified
		if hasRouteAtPort(inspection.Serve, httpsPort) {
			status.Readiness = ReadinessConflicted
			return status, ErrConflict
		}
		return status, nil
	}
	status.JournalState = record.State
	status.Profile = record.Profile
	status.HTTPSPort = record.HTTPSPort
	status.BackendPort = record.BackendPort
	if record.Scope != scope || record.InstallationID != installationID ||
		record.HTTPSPort != httpsPort || record.BackendPort != backendPort {
		status.Readiness = ReadinessConflicted
		return status, ErrConflict
	}
	if record.State != StateRegistered || !record.MutationAcknowledged {
		status.Readiness = ReadinessUncertain
		return status, ErrUncertain
	}
	inspection, err := m.client.Inspect(ctx)
	if err != nil {
		status.Readiness = readinessForError(err)
		return status, err
	}
	status.Profile = inspection.Profile
	status.RuntimeQualified = inspection.RuntimeQualified
	if inspection.Identity.NodeID != record.NodeID || inspection.Identity.DNSName != record.DNSName ||
		inspection.Profile != record.Profile || m.client.binary != record.BinaryPath {
		status.Readiness = ReadinessConflicted
		return status, ErrConflict
	}
	if !registeredRouteMatches(inspection, *record) {
		status.Readiness = ReadinessDegraded
		return status, ErrConflict
	}
	status.Readiness = ReadinessReady
	return status, nil
}

// Recover inspects the journal and current Serve state without mutating either.
// Pending/uncertain operations remain unresolved even when a matching or absent
// route is observed; only a later explicit consented lifecycle operation can
// change an acknowledged registration.
func (m *Manager) Recover(ctx context.Context, scope, installationID string, httpsPort, backendPort int) (RecoveryReport, error) {
	report := RecoveryReport{Route: RouteStatus{JournalState: StateUnconfigured, Readiness: ReadinessWaiting}}
	if ctx == nil {
		ctx = context.Background()
	}
	if (scope != "production" && scope != "development") || !validLabel(installationID) ||
		httpsPort < 1 || httpsPort > 65535 || backendPort < 1 || backendPort > 65535 || httpsPort == backendPort {
		report.Route.Readiness = ReadinessConflicted
		report.RequiresOperatorAction = true
		return report, ErrConflict
	}
	record, err := m.readRegistration()
	if err != nil {
		report.Route.Readiness = ReadinessUncertain
		report.RequiresOperatorAction = true
		return report, err
	}
	inspection, err := m.client.Inspect(ctx)
	if err != nil {
		report.Route.Readiness = readinessForError(err)
		report.RequiresOperatorAction = record != nil || errors.Is(err, ErrConflict) || errors.Is(err, ErrUncertain)
		return report, err
	}
	report.Route.Profile = inspection.Profile
	report.Route.RuntimeQualified = inspection.RuntimeQualified
	if record == nil {
		if hasRouteAtPort(inspection.Serve, httpsPort) {
			report.Observation = "selected-listener-present-without-registration"
			report.Route.Readiness = ReadinessConflicted
			report.RequiresOperatorAction = true
			return report, ErrConflict
		}
		report.Observation = "selected-listener-absent-without-registration"
		return report, nil
	}
	report.Route.JournalState = record.State
	report.Route.HTTPSPort = record.HTTPSPort
	report.Route.BackendPort = record.BackendPort
	if record.Scope != scope || record.InstallationID != installationID ||
		record.HTTPSPort != httpsPort || record.BackendPort != backendPort {
		report.Route.Readiness = ReadinessConflicted
		report.RequiresOperatorAction = true
		return report, ErrConflict
	}
	if inspection.Identity.NodeID != record.NodeID || inspection.Identity.DNSName != record.DNSName ||
		!sameProfile(inspection, *record) || m.client.binary != record.BinaryPath {
		report.Route.Readiness = ReadinessConflicted
		report.RequiresOperatorAction = true
		report.Observation = "node-profile-or-cli-identity-changed"
		return report, ErrConflict
	}
	matching := registeredRouteMatches(inspection, *record)
	listenerPresent := hasRouteAtPort(inspection.Serve, record.HTTPSPort)
	switch {
	case matching:
		report.Observation = "exact-registered-route-present"
	case listenerPresent:
		report.Observation = "selected-listener-conflicts-with-registration"
	default:
		report.Observation = "selected-listener-absent"
	}
	switch record.State {
	case StateRegistered:
		if matching {
			report.Route.Readiness = ReadinessReady
			return report, nil
		}
		report.Route.Readiness = ReadinessDegraded
		report.RequiresOperatorAction = true
		return report, ErrConflict
	case StateRemoved:
		if !listenerPresent {
			return report, nil
		}
		report.Route.Readiness = ReadinessConflicted
		report.RequiresOperatorAction = true
		return report, ErrConflict
	case StateUnconfigured:
		if !listenerPresent {
			return report, nil
		}
		report.Route.Readiness = ReadinessConflicted
		report.RequiresOperatorAction = true
		return report, ErrConflict
	case StatePublishPending, StatePublishUncertain, StateRemovePending, StateRemoveUncertain:
		report.Route.Readiness = ReadinessUncertain
		report.RequiresOperatorAction = true
		return report, ErrUncertain
	default:
		report.Route.Readiness = ReadinessUncertain
		report.RequiresOperatorAction = true
		return report, ErrUncertain
	}
}

func readinessForError(err error) RouteReadiness {
	switch {
	case errors.Is(err, ErrConflict):
		return ReadinessConflicted
	case errors.Is(err, ErrUnsupported):
		return ReadinessUnqualified
	case errors.Is(err, ErrUncertain), errors.Is(err, ErrInvalidJSON), errors.Is(err, ErrOutputTooLong):
		return ReadinessUncertain
	default:
		return ReadinessWaiting
	}
}

// Unpublish removes only the exact route from an acknowledged journal. It does
// not infer ownership from an observed route, and it never calls serve reset.
func (m *Manager) Unpublish(ctx context.Context, consent Consent) error {
	if ctx == nil {
		ctx = context.Background()
	}
	record, err := m.readRegistration()
	if err != nil {
		return err
	}
	if record == nil || record.State != StateRegistered || !record.MutationAcknowledged {
		return ErrUncertain
	}
	if err := validateUnpublishConsent(consent, record.NodeID, record.Scope, record.HTTPSPort, record.BackendPort); err != nil {
		return err
	}
	initial, err := m.client.Inspect(ctx)
	if err != nil {
		return err
	}
	if initial.Identity.NodeID != record.NodeID || !sameProfile(initial, *record) || m.client.binary != record.BinaryPath {
		return ErrConflict
	}
	if err := m.requireMutationProfile(initial); err != nil {
		return err
	}
	return m.withNodeLock(ctx, record.NodeID, func() error {
		currentRecord, err := m.readRegistration()
		if err != nil {
			return err
		}
		if currentRecord == nil || *currentRecord != *record || currentRecord.State != StateRegistered || !currentRecord.MutationAcknowledged {
			return ErrUncertain
		}
		current, err := m.client.Inspect(ctx)
		if err != nil {
			return err
		}
		if current.Identity.NodeID != record.NodeID || !sameProfile(current, *record) || m.client.binary != record.BinaryPath {
			return ErrConflict
		}
		if err := m.requireMutationProfile(current); err != nil {
			return err
		}
		if !registeredRouteMatches(current, *record) {
			return ErrConflict
		}
		opID, err := newOperationID()
		if err != nil {
			return err
		}
		pending := *record
		pending.OperationID = opID
		pending.ConsentScope = removeConsentScope
		pending.State = StateRemovePending
		pending.MutationAcknowledged = false
		pending.UpdatedAt = time.Now().UTC().Format(time.RFC3339Nano)
		if err := m.writeRegistration(pending); err != nil {
			return fmt.Errorf("persist removal intent: %w", err)
		}
		result, removeErr := m.client.execute(ctx, "serve", "--bg", fmt.Sprintf("--https=%d", record.HTTPSPort), "--set-path=/", "off")
		if removeErr != nil {
			if !result.dispatched {
				// The prior successful publication receipt remains valid.
				if saveErr := m.writeRegistration(*record); saveErr != nil {
					return fmt.Errorf("removal was not dispatched but receipt restore failed: %w", saveErr)
				}
				return sanitizeCommandError("serve unpublish", removeErr)
			}
			pending.State = StateRemoveUncertain
			pending.UpdatedAt = time.Now().UTC().Format(time.RFC3339Nano)
			_ = m.writeRegistration(pending)
			return fmt.Errorf("%w: removal acknowledgement unavailable", ErrUncertain)
		}
		pending.MutationAcknowledged = true
		pending.UpdatedAt = time.Now().UTC().Format(time.RFC3339Nano)
		if err := m.writeRegistration(pending); err != nil {
			return fmt.Errorf("%w: removal acknowledged but receipt could not be persisted", ErrUncertain)
		}
		readback, err := m.client.Inspect(ctx)
		if err != nil {
			pending.State = StateRemoveUncertain
			pending.UpdatedAt = time.Now().UTC().Format(time.RFC3339Nano)
			_ = m.writeRegistration(pending)
			return fmt.Errorf("%w: removal readback unavailable", ErrUncertain)
		}
		if readback.Identity.NodeID != record.NodeID || !sameProfile(readback, *record) || hasRouteAtPort(readback.Serve, record.HTTPSPort) {
			pending.State = StateRemoveUncertain
			pending.UpdatedAt = time.Now().UTC().Format(time.RFC3339Nano)
			_ = m.writeRegistration(pending)
			return fmt.Errorf("%w: selected listener remains or changed", ErrUncertain)
		}
		pending.State = StateRemoved
		pending.UpdatedAt = time.Now().UTC().Format(time.RFC3339Nano)
		if err := m.writeRegistration(pending); err != nil {
			return fmt.Errorf("%w: removal verified but final journal update failed", ErrUncertain)
		}
		return nil
	})
}

func (m *Manager) requireMutationProfile(inspection Inspection) error {
	if !inspection.ProfileKnown {
		return ErrUnsupported
	}
	if inspection.RuntimeQualified || m.fixtureMutations {
		return nil
	}
	return fmt.Errorf("%w: %s is source/fixture-only; live qualification and enablement are pending P6", ErrUnsupported, inspection.Profile)
}

func validateRequest(request PublishRequest) error {
	if !validLabel(request.InstallationID) ||
		(request.Scope != "production" && request.Scope != "development") ||
		!validNodeID(request.ExpectedNodeID) || !validLoopbackBackend(request.BackendPort) ||
		request.HTTPSPort < 1 || request.HTTPSPort > 65535 || request.HTTPSPort == request.BackendPort {
		return ErrConflict
	}
	return nil
}

func validateConsentBinding(consent Consent, nodeID, scope string, httpsPort, backendPort int) error {
	if !consent.Accepted || consent.Scope != scope || consent.NodeID != nodeID ||
		consent.HTTPSPort != httpsPort || consent.BackendPort != backendPort {
		return errors.New("explicit consent must bind to this node, scope and exact ports")
	}
	return nil
}

func validatePublishConsent(consent Consent, nodeID, scope string, httpsPort, backendPort int) error {
	if err := validateConsentBinding(consent, nodeID, scope, httpsPort, backendPort); err != nil {
		return err
	}
	if !consent.PersistentRouteAccepted || !consent.CheckToWriteRaceAccepted ||
		!consent.PortReuseRiskAccepted || !consent.NoRollbackAccepted || !consent.NoRemoteDrainAccepted {
		return errors.New("persistent route choice and all four residual-risk acknowledgements are required")
	}
	return nil
}

func validateUnpublishConsent(consent Consent, nodeID, scope string, httpsPort, backendPort int) error {
	if err := validateConsentBinding(consent, nodeID, scope, httpsPort, backendPort); err != nil {
		return err
	}
	if !consent.RouteRemovalAccepted || !consent.NoRemoteDrainAccepted {
		return errors.New("explicit route-removal and no-remote-drain acknowledgements are required")
	}
	return nil
}

func sameRequest(record registration, request PublishRequest, inspection Inspection, binary string) bool {
	return record.InstallationID == request.InstallationID && record.Scope == request.Scope &&
		record.NodeID == request.ExpectedNodeID && record.DNSName == inspection.Identity.DNSName &&
		record.Profile == inspection.Profile && record.BinaryPath == binary &&
		record.HTTPSPort == request.HTTPSPort && record.BackendPort == request.BackendPort &&
		record.Path == "/" && record.Backend == fmt.Sprintf("http://127.0.0.1:%d", request.BackendPort)
}

func sameInspectionIdentity(left, right Inspection) bool {
	return left.Identity == right.Identity && left.Profile == right.Profile && left.Version == right.Version
}

func sameProfile(inspection Inspection, record registration) bool {
	return inspection.ProfileKnown && inspection.Profile == record.Profile
}

func routeConflicts(serve tailscale.ServeStatus, port int) bool {
	if !serve.Complete || serve.FunnelConfigured || serve.RetainedFunnel {
		return true
	}
	for _, route := range serve.ObservedRoutes {
		if route.Port == port {
			return true
		}
	}
	return false
}

func registeredRouteMatches(inspection Inspection, record registration) bool {
	if !inspection.ProfileKnown || inspection.Profile != record.Profile ||
		inspection.Identity.NodeID != record.NodeID || inspection.Identity.DNSName != record.DNSName ||
		!inspection.Serve.Complete || inspection.Serve.FunnelConfigured || inspection.Serve.RetainedFunnel {
		return false
	}
	matching := 0
	for _, route := range inspection.Serve.ObservedRoutes {
		if route.Port != record.HTTPSPort {
			continue
		}
		if route.Session != "" || route.Listener != "HTTPS" || route.Host != record.DNSName ||
			route.Handler != "Proxy" || route.Path != record.Path || route.Backend != record.Backend {
			return false
		}
		matching++
	}
	return matching == 1
}

func hasRouteAtPort(serve tailscale.ServeStatus, port int) bool {
	if !serve.Complete {
		return true
	}
	for _, route := range serve.ObservedRoutes {
		if route.Port == port {
			return true
		}
	}
	return false
}

func (m *Manager) withNodeLock(ctx context.Context, nodeID string, operation func() error) error {
	if ctx == nil {
		ctx = context.Background()
	}
	if _, err := validatePrivateDirectory(m.coordinationRoot); err != nil {
		return ErrPermissionDenied
	}
	digest := sha256.Sum256([]byte(nodeID))
	lockPath := filepath.Join(m.coordinationRoot, "node-"+hex.EncodeToString(digest[:])[:24]+".lock")
	deadline := time.Now().Add(mutationLockWait)
	var dev, ino uint64
	for {
		if err := os.Mkdir(lockPath, 0o700); err == nil {
			info, statErr := os.Lstat(lockPath)
			if statErr != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm() != 0o700 || !ownedByCurrentUser(info) {
				return ErrPermissionDenied
			}
			dev, ino = fileIDs(info)
			if err := syncDirectory(m.coordinationRoot); err != nil {
				_ = os.Remove(lockPath)
				return err
			}
			break
		} else if !os.IsExist(err) {
			return ErrPermissionDenied
		} else {
			info, statErr := os.Lstat(lockPath)
			if statErr != nil {
				if os.IsNotExist(statErr) {
					continue
				}
				return ErrPermissionDenied
			}
			if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 || !ownedByCurrentUser(info) {
				return ErrPermissionDenied
			}
			if !time.Now().Before(deadline) {
				return ErrUncertain
			}
			select {
			case <-ctx.Done():
				return ErrUncertain
			case <-time.After(mutationLockDelay):
			}
		}
	}
	operationErr := operation()
	if releaseErr := removeNodeLock(lockPath, m.coordinationRoot, dev, ino); releaseErr != nil {
		if operationErr != nil {
			return errors.Join(operationErr, releaseErr)
		}
		return fmt.Errorf("%w: coordination lock could not be durably released", ErrUncertain)
	}
	return operationErr
}

func removeNodeLock(lockPath, parent string, dev, ino uint64) error {
	info, err := os.Lstat(lockPath)
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return ErrPermissionDenied
	}
	currentDev, currentIno := fileIDs(info)
	if currentDev != dev || currentIno != ino {
		return ErrPermissionDenied
	}
	entries, err := os.ReadDir(lockPath)
	if err != nil || len(entries) != 0 {
		return ErrPermissionDenied
	}
	if err := os.Remove(lockPath); err != nil {
		return ErrPermissionDenied
	}
	return syncDirectory(parent)
}

func syncDirectory(path string) error {
	directory, err := os.Open(path)
	if err != nil {
		return ErrPermissionDenied
	}
	defer directory.Close()
	if err := directory.Sync(); err != nil {
		return ErrPermissionDenied
	}
	return nil
}

func (m *Manager) readRegistration() (*registration, error) {
	root, err := validatePrivateDirectory(m.stateRoot)
	if err != nil || root != m.stateRoot {
		return nil, ErrUncertain
	}
	path := filepath.Join(m.stateRoot, journalName)
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil || info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() || info.Mode().Perm() != 0o600 || info.Size() > journalMaxBytes || linkCount(info) != 1 || !ownedByCurrentUser(info) {
		return nil, ErrUncertain
	}
	data, err := os.ReadFile(path)
	if err != nil || validateJSON(data, journalMaxBytes, 8, 1000) != nil {
		return nil, ErrUncertain
	}
	var record registration
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&record) != nil || decoder.Decode(new(any)) != io.EOF || record.Validate() != nil {
		return nil, ErrUncertain
	}
	return &record, nil
}

func (m *Manager) writeRegistration(record registration) error {
	root, err := validatePrivateDirectory(m.stateRoot)
	if err != nil || root != m.stateRoot {
		return ErrUncertain
	}
	if err := record.Validate(); err != nil {
		return ErrUncertain
	}
	data, err := json.Marshal(record)
	if err != nil || len(data) > journalMaxBytes {
		return ErrUncertain
	}
	path := filepath.Join(m.stateRoot, journalName)
	if info, statErr := os.Lstat(path); statErr == nil {
		if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() || info.Mode().Perm() != 0o600 || linkCount(info) != 1 || !ownedByCurrentUser(info) {
			return ErrUncertain
		}
	} else if !errors.Is(statErr, os.ErrNotExist) {
		return ErrUncertain
	}
	return writeAtomic(m.stateRoot, journalName, data)
}

func (r registration) Validate() error {
	if r.Schema != 1 || !validLabel(r.InstallationID) ||
		(r.Scope != "production" && r.Scope != "development") || !validNodeID(r.NodeID) ||
		!validDNSName(r.DNSName) || !filepath.IsAbs(r.BinaryPath) ||
		(r.Profile != ProfileAppStoreSupplied && r.Profile != ProfileLinuxSource) ||
		r.HTTPSPort < 1 || r.HTTPSPort > 65535 || r.BackendPort < 1 || r.BackendPort > 65535 ||
		r.HTTPSPort == r.BackendPort || r.Path != "/" ||
		r.Backend != fmt.Sprintf("http://127.0.0.1:%d", r.BackendPort) || !validOperationID(r.OperationID) {
		return ErrUncertain
	}
	if _, err := time.Parse(time.RFC3339Nano, r.UpdatedAt); err != nil {
		return ErrUncertain
	}
	switch r.State {
	case StateUnconfigured:
		if r.ConsentScope != publishConsentScope || r.MutationAcknowledged {
			return ErrUncertain
		}
	case StatePublishPending, StatePublishUncertain:
		if r.ConsentScope != publishConsentScope || r.MutationAcknowledged {
			return ErrUncertain
		}
	case StateRegistered:
		if r.ConsentScope != publishConsentScope || !r.MutationAcknowledged {
			return ErrUncertain
		}
	case StateRemovePending, StateRemoveUncertain:
		if r.ConsentScope != removeConsentScope {
			return ErrUncertain
		}
		// The acknowledgement may or may not have arrived before interruption.
	case StateRemoved:
		if r.ConsentScope != removeConsentScope || !r.MutationAcknowledged {
			return ErrUncertain
		}
	default:
		return ErrUncertain
	}
	return nil
}

func ownedByCurrentUser(info os.FileInfo) bool {
	stat, ok := info.Sys().(*syscall.Stat_t)
	return ok && stat.Uid == uint32(os.Geteuid())
}

func validatePrivateDirectory(path string) (string, error) {
	if !filepath.IsAbs(path) {
		return "", ErrPermissionDenied
	}
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		return "", ErrPermissionDenied
	}
	info, err := os.Lstat(resolved)
	if err != nil || info.Mode()&os.ModeSymlink != 0 || !info.IsDir() || info.Mode().Perm() != 0o700 || !ownedByCurrentUser(info) {
		return "", ErrPermissionDenied
	}
	return filepath.Clean(resolved), nil
}

func pathsOverlap(left, right string) bool {
	left, right = filepath.Clean(left), filepath.Clean(right)
	if left == right {
		return true
	}
	within := func(parent, child string) bool {
		relative, err := filepath.Rel(parent, child)
		return err == nil && relative != "." && relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator))
	}
	return within(left, right) || within(right, left)
}

func writeAtomic(directory, name string, data []byte) error {
	file, err := os.CreateTemp(directory, name+".tmp-*")
	if err != nil {
		return ErrPermissionDenied
	}
	temp := file.Name()
	cleanup := func() { _ = os.Remove(temp) }
	if err := file.Chmod(0o600); err != nil {
		_ = file.Close()
		cleanup()
		return ErrPermissionDenied
	}
	if n, err := file.Write(data); err != nil || n != len(data) {
		_ = file.Close()
		cleanup()
		return ErrPermissionDenied
	}
	if err := file.Sync(); err != nil {
		_ = file.Close()
		cleanup()
		return ErrPermissionDenied
	}
	if err := file.Close(); err != nil {
		cleanup()
		return ErrPermissionDenied
	}
	if err := os.Rename(temp, filepath.Join(directory, name)); err != nil {
		cleanup()
		return ErrPermissionDenied
	}
	dir, err := os.Open(directory)
	if err != nil {
		return ErrPermissionDenied
	}
	defer dir.Close()
	if err := dir.Sync(); err != nil {
		return ErrPermissionDenied
	}
	return nil
}

func newOperationID() (string, error) {
	var bytes [16]byte
	if _, err := rand.Read(bytes[:]); err != nil {
		return "", ErrPermissionDenied
	}
	return hex.EncodeToString(bytes[:]), nil
}

func validOperationID(id string) bool {
	if len(id) != 32 {
		return false
	}
	_, err := hex.DecodeString(id)
	return err == nil
}

func validLabel(value string) bool {
	if value == "" || len(value) > 128 {
		return false
	}
	for _, r := range value {
		if r < 0x21 || r > 0x7e || r == '/' || r == '\\' {
			return false
		}
	}
	return true
}

func validNodeID(value string) bool {
	return value != "" && len(value) <= 256 && strings.TrimSpace(value) == value && !strings.ContainsAny(value, "\x00\r\n")
}

func validDNSName(value string) bool {
	if value == "" || strings.ToLower(value) != value || strings.HasSuffix(value, ".") || net.ParseIP(value) != nil {
		return false
	}
	for _, label := range strings.Split(value, ".") {
		if label == "" || len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' {
			return false
		}
		for _, r := range label {
			if (r < 'a' || r > 'z') && (r < '0' || r > '9') && r != '-' {
				return false
			}
		}
	}
	return true
}

func fileIDs(info os.FileInfo) (uint64, uint64) {
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, 0
	}
	return uint64(stat.Dev), uint64(stat.Ino)
}

func linkCount(info os.FileInfo) uint64 {
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return 0
	}
	return uint64(stat.Nlink)
}
