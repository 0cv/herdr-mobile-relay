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
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/0cv/herdr-mobile-relay/internal/setuphelper"
	"github.com/0cv/herdr-mobile-relay/internal/tailscale"
)

const (
	journalName                = "registration.json"
	journalMaxBytes            = 16 * 1024
	backendReservationMaxBytes = 4 * 1024
	mutationLockWait           = 5 * time.Second
	mutationLockDelay          = 20 * time.Millisecond
	publishConsentScope        = "persistent-route-and-four-risks-v1"
	removeConsentScope         = "explicit-route-removal-and-no-remote-drain-v1"
	reconcileConsentScope      = "explicit-journal-reconciliation-v1"
)

var ErrPublishNotDispatched = errors.New("Serve publish was not dispatched")

type RegistrationState string

const (
	StateUnconfigured      RegistrationState = "unconfigured"
	StatePublishPending    RegistrationState = "publish-pending"
	StateRegistered        RegistrationState = "registered"
	StatePublishUncertain  RegistrationState = "publish-uncertain"
	StateRemovePending     RegistrationState = "remove-pending"
	StateRemoveUncertain   RegistrationState = "remove-uncertain"
	StateRemoved           RegistrationState = "removed"
	StateReconciledPresent RegistrationState = "reconciled-present"
	StateReconciledAbsent  RegistrationState = "reconciled-absent"
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
	Origin                   string
	OperationID              string
	RecoveryObservation      string
	RecoveryAccepted         bool
}

type PublishRequest struct {
	InstallationID string
	Scope          string
	ExpectedNodeID string
	Origin         string
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

type backendPortReservation struct {
	Schema         int               `json:"schema"`
	InstallationID string            `json:"installation_id"`
	Scope          string            `json:"scope"`
	NodeID         string            `json:"node_id"`
	HTTPSPort      int               `json:"https_port"`
	BackendPort    int               `json:"backend_port"`
	Origin         string            `json:"origin"`
	State          RegistrationState `json:"state"`
	UpdatedAt      string            `json:"updated_at"`
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
	OperationID            string      `json:"recovery_operation_id,omitempty"`
	RequiresOperatorAction bool        `json:"requires_operator_action"`
}

type Manager struct {
	stateRoot            string
	coordinationRoot     string
	client               *Client
	fixtureMutations     bool
	skipBackendReadiness bool
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

// ReserveBackendPort durably claims a local backend port before setup starts
// the relay listener. Reservations live in the shared per-user coordination
// root until the exact route is explicitly unpublished or setup proves no
// route was published and releases its own reservation.
func (m *Manager) ReserveBackendPort(ctx context.Context, installationID, scope, nodeID, origin string, httpsPort, backendPort int) error {
	reservation := backendPortReservation{
		Schema: 1, InstallationID: installationID, Scope: scope, NodeID: nodeID,
		HTTPSPort: httpsPort, BackendPort: backendPort, Origin: origin,
		State: StatePublishPending, UpdatedAt: time.Now().UTC().Format(time.RFC3339Nano),
	}
	if reservation.Validate() != nil {
		return ErrConflict
	}
	inspection, err := m.client.Inspect(ctx)
	if err != nil {
		return err
	}
	if inspection.Identity.NodeID != nodeID || !originMatchesIdentity(origin, inspection.Identity, httpsPort) {
		return ErrConflict
	}
	if err := m.requireMutationProfile(inspection); err != nil {
		return err
	}
	return m.withBackendReservationLock(ctx, func() error {
		record, err := m.readRegistration()
		if err != nil {
			return err
		}
		if record != nil {
			switch record.State {
			case StateRegistered:
				if record.InstallationID != installationID || record.Scope != scope || record.NodeID != nodeID ||
					record.HTTPSPort != httpsPort || record.BackendPort != backendPort || !registrationOriginMatches(*record, origin) {
					return ErrConflict
				}
			case StateRemoved, StateUnconfigured, StateReconciledAbsent:
			default:
				return ErrUncertain
			}
		}
		routeAlreadyOurs := record != nil && record.State == StateRegistered && registeredRouteMatches(inspection, *record)
		if routeConflicts(inspection.Serve, httpsPort) && !routeAlreadyOurs {
			return ErrConflict
		}
		var allowedRoute *registration
		if routeAlreadyOurs {
			allowedRoute = record
		}
		if backendPortHasRoute(inspection.Serve, backendPort, allowedRoute) {
			return ErrConflict
		}
		return m.writeBackendReservation(reservation)
	})
}

// ReleaseBackendPort removes only the caller's reservation after read-only CLI
// inspection proves no Serve route still targets the selected backend port.
func (m *Manager) ReleaseBackendPort(ctx context.Context, installationID, scope, nodeID, origin string, httpsPort, backendPort int) error {
	reservation := backendPortReservation{
		Schema: 1, InstallationID: installationID, Scope: scope, NodeID: nodeID,
		HTTPSPort: httpsPort, BackendPort: backendPort, Origin: origin,
		State: StatePublishPending, UpdatedAt: time.Now().UTC().Format(time.RFC3339Nano),
	}
	if reservation.Validate() != nil {
		return ErrConflict
	}
	return m.withNodeLock(ctx, nodeID, func() error {
		return m.withBackendReservationLock(ctx, func() error {
			record, err := m.readRegistration()
			if err != nil {
				return err
			}
			if record != nil && record.State != StateUnconfigured && record.State != StateRemoved && record.State != StateReconciledAbsent {
				return ErrUncertain
			}
			inspection, err := m.client.Inspect(ctx)
			if err != nil {
				return err
			}
			if inspection.Identity.NodeID != nodeID || !originMatchesIdentity(origin, inspection.Identity, httpsPort) || !inspection.Serve.Complete {
				return ErrConflict
			}
			if backendPortHasRoute(inspection.Serve, backendPort, nil) {
				return ErrConflict
			}
			return m.removeBackendReservation(reservation)
		})
	})
}

// Publish creates exactly one persistent background HTTPS proxy route. Normal
// production managers fail closed because no profile is runtime-qualified.
func (m *Manager) Publish(ctx context.Context, request PublishRequest) error {
	if err := validateRequest(request); err != nil {
		return m.publishNotDispatched(err)
	}
	inspection, err := m.client.Inspect(ctx)
	if err != nil {
		return m.publishNotDispatched(err)
	}
	if inspection.Identity.NodeID != request.ExpectedNodeID || !originMatchesIdentity(request.Origin, inspection.Identity, request.HTTPSPort) {
		return m.publishNotDispatched(ErrConflict)
	}
	if err := m.requireMutationProfile(inspection); err != nil {
		return m.publishNotDispatched(err)
	}
	err = m.withNodeLock(ctx, inspection.Identity.NodeID, func() error {
		return m.withBackendReservationLock(ctx, func() error {
			current, err := m.client.Inspect(ctx)
			if err != nil {
				return err
			}
			if !sameInspectionIdentity(inspection, current) || current.Identity.NodeID != request.ExpectedNodeID ||
				!originMatchesIdentity(request.Origin, current.Identity, request.HTTPSPort) {
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
					return m.writeBackendReservation(reservationForRegistration(*record, StateRegistered))
				}
				return ErrConflict // drift never triggers automatic repair
			}
			if record != nil && record.State != StateRemoved && record.State != StateUnconfigured && record.State != StateReconciledAbsent {
				return fmt.Errorf("%w: recovery state %s requires explicit reconciliation", ErrUncertain, record.State)
			}
			desiredReservation := backendPortReservation{
				Schema: 1, InstallationID: request.InstallationID, Scope: request.Scope, NodeID: current.Identity.NodeID,
				HTTPSPort: request.HTTPSPort, BackendPort: request.BackendPort, Origin: request.Origin,
				State: StatePublishPending, UpdatedAt: time.Now().UTC().Format(time.RFC3339Nano),
			}
			if m.fixtureMutations {
				if existing, readErr := m.readBackendReservation(request.BackendPort); readErr != nil {
					return readErr
				} else if existing == nil {
					if err := m.writeBackendReservation(desiredReservation); err != nil {
						return err
					}
				}
			}
			reserved, err := m.readBackendReservation(request.BackendPort)
			if err != nil || reserved == nil || !sameBackendReservation(*reserved, desiredReservation) {
				return ErrConflict
			}
			if err := validatePublishConsent(request.Consent, request.ExpectedNodeID, request.Scope, request.Origin, request.HTTPSPort, request.BackendPort); err != nil {
				return err
			}
			if routeConflicts(current.Serve, request.HTTPSPort) || backendPortHasRoute(current.Serve, request.BackendPort, nil) {
				return ErrConflict
			}
			if !m.skipBackendReadiness {
				if err := verifyBackendReadiness(ctx, request.BackendPort, request.InstallationID, request.Origin); err != nil {
					return err
				}
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
			if err := m.writeBackendReservation(reservationForRegistration(entry, StatePublishPending)); err != nil {
				entry.State = StateUnconfigured
				entry.UpdatedAt = time.Now().UTC().Format(time.RFC3339Nano)
				_ = m.writeRegistration(entry)
				return fmt.Errorf("reserve backend port before publication: %w", err)
			}
			result, publishErr := m.client.execute(ctx, "serve", "--bg", fmt.Sprintf("--https=%d", request.HTTPSPort), "--set-path=/", entry.Backend)
			if publishErr != nil {
				if !result.dispatched {
					entry.State = StateUnconfigured
					entry.UpdatedAt = time.Now().UTC().Format(time.RFC3339Nano)
					if saveErr := m.writeRegistration(entry); saveErr != nil {
						return fmt.Errorf("publish was not dispatched but journal update failed: %w", saveErr)
					}
					return fmt.Errorf("publish invocation was not dispatched; backend reservation remains until the listener is stopped: %w",
						sanitizeCommandError("serve publish", publishErr))
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
				// The durable publish-pending record and port reservation remain as recovery evidence.
				return fmt.Errorf("%w: publish acknowledged but receipt could not be persisted", ErrUncertain)
			}
			if err := m.writeBackendReservation(reservationForRegistration(entry, StateRegistered)); err != nil {
				return fmt.Errorf("%w: publish receipt recorded but backend reservation update failed", ErrUncertain)
			}
			readback, err := m.client.Inspect(ctx)
			if err != nil || !registeredRouteMatches(readback, entry) {
				// Keep the durable receipt. Do not retry, roll back, or issue `off`.
				return fmt.Errorf("%w: publish receipt recorded; route readback is not ready", ErrConflict)
			}
			return nil
		})
	})
	return m.publishNotDispatched(err)
}

func (m *Manager) publishNotDispatched(err error) error {
	if err == nil {
		return nil
	}
	record, readErr := m.readRegistration()
	if readErr != nil {
		return err
	}
	if record != nil && record.State != StateUnconfigured && record.State != StateRemoved && record.State != StateReconciledAbsent {
		return err
	}
	return errors.Join(ErrPublishNotDispatched, err)
}

// VerifyRegisteredRoute performs bounded, read-only drift detection for a
// previously acknowledged registration. A matching observation without a
// valid journal is never adopted, and this method never repairs a route.
func (m *Manager) VerifyRegisteredRoute(ctx context.Context, scope, installationID, origin string, httpsPort, backendPort int) (RouteStatus, error) {
	status := RouteStatus{JournalState: StateUnconfigured, Readiness: ReadinessWaiting}
	if ctx == nil {
		ctx = context.Background()
	}
	if (scope != "production" && scope != "development") || !validLabel(installationID) || !validCanonicalOrigin(origin) ||
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
	if record.Scope != scope || record.InstallationID != installationID || record.HTTPSPort != httpsPort ||
		record.BackendPort != backendPort || !registrationOriginMatches(*record, origin) {
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
func (m *Manager) Recover(ctx context.Context, scope, installationID, origin string, httpsPort, backendPort int) (RecoveryReport, error) {
	report := RecoveryReport{Route: RouteStatus{JournalState: StateUnconfigured, Readiness: ReadinessWaiting}}
	if ctx == nil {
		ctx = context.Background()
	}
	if (scope != "production" && scope != "development") || !validLabel(installationID) || !validCanonicalOrigin(origin) ||
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
		if !originMatchesIdentity(origin, inspection.Identity, httpsPort) {
			report.Route.Readiness = ReadinessConflicted
			report.RequiresOperatorAction = true
			report.Observation = "live-node-origin-does-not-match-selection"
			return report, ErrConflict
		}
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
	if record.Scope != scope || record.InstallationID != installationID || record.HTTPSPort != httpsPort ||
		record.BackendPort != backendPort || !registrationOriginMatches(*record, origin) {
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
	if record.State == StatePublishPending || record.State == StatePublishUncertain ||
		record.State == StateRemovePending || record.State == StateRemoveUncertain {
		report.OperationID = record.OperationID
	}
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

// Reconcile changes only the local journal after an operator explicitly
// confirms the exact operation ID and one read-only Serve observation. It never
// invokes a Serve mutator or adopts a route without a matching pending intent.
func (m *Manager) Reconcile(ctx context.Context, scope, installationID, origin string, httpsPort, backendPort int, consent Consent) error {
	if ctx == nil {
		ctx = context.Background()
	}
	record, err := m.readRegistration()
	if err != nil {
		return err
	}
	if record == nil || (record.State != StatePublishPending && record.State != StatePublishUncertain &&
		record.State != StateRemovePending && record.State != StateRemoveUncertain) {
		return ErrUncertain
	}
	if record.Scope != scope || record.InstallationID != installationID || record.HTTPSPort != httpsPort ||
		record.BackendPort != backendPort || !registrationOriginMatches(*record, origin) {
		return ErrConflict
	}
	if err := validateRecoveryConsent(consent, *record, origin); err != nil {
		return err
	}
	initial, err := m.client.Inspect(ctx)
	if err != nil {
		return err
	}
	if initial.Identity.NodeID != record.NodeID || initial.Identity.DNSName != record.DNSName ||
		!originMatchesIdentity(origin, initial.Identity, record.HTTPSPort) || !sameProfile(initial, *record) ||
		m.client.binary != record.BinaryPath {
		return ErrConflict
	}
	if err := m.requireMutationProfile(initial); err != nil {
		return err
	}
	return m.withNodeLock(ctx, record.NodeID, func() error {
		return m.withBackendReservationLock(ctx, func() error {
			currentRecord, err := m.readRegistration()
			if err != nil {
				return err
			}
			if currentRecord == nil || *currentRecord != *record {
				return ErrUncertain
			}
			current, err := m.client.Inspect(ctx)
			if err != nil {
				return err
			}
			if current.Identity.NodeID != record.NodeID || current.Identity.DNSName != record.DNSName ||
				!originMatchesIdentity(origin, current.Identity, record.HTTPSPort) || !sameProfile(current, *record) ||
				m.client.binary != record.BinaryPath {
				return ErrConflict
			}
			matching := registeredRouteMatches(current, *record)
			listenerPresent := hasRouteAtPort(current.Serve, record.HTTPSPort)
			if consent.RecoveryObservation == "present" && !matching {
				return ErrConflict
			}
			if consent.RecoveryObservation == "absent" && listenerPresent {
				return ErrConflict
			}
			resolved := *record
			resolved.UpdatedAt = time.Now().UTC().Format(time.RFC3339Nano)
			switch consent.RecoveryObservation {
			case "present":
				resolved.State = StateReconciledPresent
				resolved.ConsentScope = reconcileConsentScope
				resolved.MutationAcknowledged = false
				if err := m.writeBackendReservation(reservationForRegistration(resolved, StateReconciledPresent)); err != nil {
					return err
				}
			case "absent":
				resolved.State = StateReconciledAbsent
				resolved.ConsentScope = reconcileConsentScope
				resolved.MutationAcknowledged = false
			default:
				return ErrConflict
			}
			if err := m.writeRegistration(resolved); err != nil {
				return err
			}
			if consent.RecoveryObservation == "absent" {
				return m.removeBackendReservation(reservationForRegistration(resolved, record.State))
			}
			return nil
		})
	})
}

func validateRecoveryConsent(consent Consent, record registration, origin string) error {
	if !consent.RecoveryAccepted || consent.OperationID != record.OperationID ||
		(consent.RecoveryObservation != "present" && consent.RecoveryObservation != "absent") {
		return errors.New("explicit recovery consent must bind to the pending operation and observed route state")
	}
	return validateConsentBinding(consent, record.NodeID, record.Scope, origin, record.HTTPSPort, record.BackendPort)
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
	if record == nil || !((record.State == StateRegistered && record.MutationAcknowledged) ||
		(record.State == StateReconciledPresent && !record.MutationAcknowledged)) {
		return ErrUncertain
	}
	origin, err := tailscale.Origin(record.DNSName, record.HTTPSPort)
	if err != nil {
		return ErrConflict
	}
	if err := validateUnpublishConsent(consent, record.NodeID, record.Scope, origin, record.HTTPSPort, record.BackendPort); err != nil {
		return err
	}
	initial, err := m.client.Inspect(ctx)
	if err != nil {
		return err
	}
	if initial.Identity.NodeID != record.NodeID || !originMatchesIdentity(origin, initial.Identity, record.HTTPSPort) ||
		!sameProfile(initial, *record) || m.client.binary != record.BinaryPath {
		return ErrConflict
	}
	if err := m.requireMutationProfile(initial); err != nil {
		return err
	}
	return m.withNodeLock(ctx, record.NodeID, func() error {
		return m.withBackendReservationLock(ctx, func() error {
			currentRecord, err := m.readRegistration()
			if err != nil {
				return err
			}
			if currentRecord == nil || *currentRecord != *record ||
				!((currentRecord.State == StateRegistered && currentRecord.MutationAcknowledged) ||
					(currentRecord.State == StateReconciledPresent && !currentRecord.MutationAcknowledged)) {
				return ErrUncertain
			}
			reservation, err := m.readBackendReservation(record.BackendPort)
			if err != nil || reservation == nil || !sameBackendReservation(*reservation, reservationForRegistration(*record, record.State)) {
				return ErrUncertain
			}
			current, err := m.client.Inspect(ctx)
			if err != nil {
				return err
			}
			if current.Identity.NodeID != record.NodeID || !originMatchesIdentity(origin, current.Identity, record.HTTPSPort) ||
				!sameProfile(current, *record) || m.client.binary != record.BinaryPath {
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
			if readback.Identity.NodeID != record.NodeID || !originMatchesIdentity(origin, readback.Identity, record.HTTPSPort) ||
				!sameProfile(readback, *record) || hasRouteAtPort(readback.Serve, record.HTTPSPort) ||
				backendPortHasRoute(readback.Serve, record.BackendPort, nil) {
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
			if err := m.removeBackendReservation(reservationForRegistration(pending, StateRemoved)); err != nil {
				return fmt.Errorf("%w: route was removed but backend reservation could not be released", ErrUncertain)
			}
			return nil
		})
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
		!validNodeID(request.ExpectedNodeID) || !validCanonicalOrigin(request.Origin) || !validLoopbackBackend(request.BackendPort) ||
		request.HTTPSPort < 1 || request.HTTPSPort > 65535 || request.HTTPSPort == request.BackendPort {
		return ErrConflict
	}
	return nil
}

func validateConsentBinding(consent Consent, nodeID, scope, origin string, httpsPort, backendPort int) error {
	if !consent.Accepted || consent.Scope != scope || consent.NodeID != nodeID || consent.Origin != origin ||
		consent.HTTPSPort != httpsPort || consent.BackendPort != backendPort {
		return errors.New("explicit consent must bind to this node, origin, scope and exact ports")
	}
	return nil
}

func validatePublishConsent(consent Consent, nodeID, scope, origin string, httpsPort, backendPort int) error {
	if err := validateConsentBinding(consent, nodeID, scope, origin, httpsPort, backendPort); err != nil {
		return err
	}
	if !consent.PersistentRouteAccepted || !consent.CheckToWriteRaceAccepted ||
		!consent.PortReuseRiskAccepted || !consent.NoRollbackAccepted || !consent.NoRemoteDrainAccepted {
		return errors.New("persistent route choice and all four residual-risk acknowledgements are required")
	}
	return nil
}

func validateUnpublishConsent(consent Consent, nodeID, scope, origin string, httpsPort, backendPort int) error {
	if err := validateConsentBinding(consent, nodeID, scope, origin, httpsPort, backendPort); err != nil {
		return err
	}
	if !consent.RouteRemovalAccepted || !consent.CheckToWriteRaceAccepted || !consent.NoRemoteDrainAccepted {
		return errors.New("explicit route-removal, check-to-write-race, and no-remote-drain acknowledgements are required")
	}
	return nil
}

func sameRequest(record registration, request PublishRequest, inspection Inspection, binary string) bool {
	return record.InstallationID == request.InstallationID && record.Scope == request.Scope &&
		record.NodeID == request.ExpectedNodeID && record.DNSName == inspection.Identity.DNSName &&
		registrationOriginMatches(record, request.Origin) &&
		record.Profile == inspection.Profile && record.BinaryPath == binary &&
		record.HTTPSPort == request.HTTPSPort && record.BackendPort == request.BackendPort &&
		record.Path == "/" && record.Backend == fmt.Sprintf("http://127.0.0.1:%d", request.BackendPort)
}

func validCanonicalOrigin(origin string) bool {
	canonical, err := setuphelper.NormalizeExternalHTTPSOrigin(origin)
	return err == nil && canonical == origin
}

func originMatchesIdentity(origin string, identity Identity, httpsPort int) bool {
	derived, err := tailscale.Origin(identity.DNSName, httpsPort)
	return err == nil && origin == derived
}

func registrationOriginMatches(record registration, origin string) bool {
	derived, err := tailscale.Origin(record.DNSName, record.HTTPSPort)
	return err == nil && origin == derived
}

func verifyBackendReadiness(ctx context.Context, port int, installationID, origin string) error {
	if ctx == nil {
		ctx = context.Background()
	}
	if port < 1 || port > 65535 || !validLabel(installationID) || !validCanonicalOrigin(origin) {
		return ErrConflict
	}
	checkCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	transport := &http.Transport{Proxy: nil}
	defer transport.CloseIdleConnections()
	client := &http.Client{
		Transport: transport,
		Timeout:   3 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	request, err := http.NewRequestWithContext(checkCtx, http.MethodGet, fmt.Sprintf("http://127.0.0.1:%d/healthz", port), nil)
	if err != nil {
		return ErrConflict
	}
	response, err := client.Do(request)
	if err != nil {
		return fmt.Errorf("%w: expected relay backend is not serving readiness", ErrConflict)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK || response.Header.Get("X-Herdr-Relay-Instance") != installationID {
		return fmt.Errorf("%w: loopback backend identity did not match this installation", ErrConflict)
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, 64*1024+1))
	if err != nil || len(body) > 64*1024 {
		return fmt.Errorf("%w: loopback backend health response was invalid", ErrConflict)
	}
	var health struct {
		Status    string `json:"status"`
		Readiness string `json:"readiness"`
		Transport string `json:"transport"`
		Instance  string `json:"instance"`
		CLIOrigin string `json:"tailscale_cli_origin"`
	}
	if json.Unmarshal(body, &health) != nil || health.Status != "ok" || health.Readiness != "ready" ||
		health.Transport != "tailscale-cli" || health.Instance != installationID || health.CLIOrigin != origin {
		return fmt.Errorf("%w: loopback backend is not the ready CLI-backed relay for this origin", ErrConflict)
	}
	return nil
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
	return matching == 1 && !backendPortHasRoute(inspection.Serve, record.BackendPort, &record)
}

func backendPortHasRoute(serve tailscale.ServeStatus, backendPort int, allowed *registration) bool {
	for _, route := range serve.ObservedRoutes {
		usesPort, err := loopbackBackendUsesPort(route.Backend, backendPort)
		if err != nil {
			return true
		}
		if !usesPort {
			continue
		}
		if allowed != nil && route.Port == allowed.HTTPSPort && route.Session == "" && route.Listener == "HTTPS" &&
			route.Host == allowed.DNSName && route.Handler == "Proxy" && route.Path == allowed.Path && route.Backend == allowed.Backend {
			continue
		}
		return true
	}
	return false
}

func loopbackBackendUsesPort(backend string, port int) (bool, error) {
	parsed, err := url.Parse(backend)
	if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Hostname() == "" {
		return false, ErrConflict
	}
	host := strings.TrimSuffix(strings.ToLower(parsed.Hostname()), ".")
	loopback := host == "localhost" || strings.HasSuffix(host, ".localhost")
	if ip := net.ParseIP(host); ip != nil {
		loopback = ip.IsLoopback()
		if ipv4 := ip.To4(); ipv4 != nil && ipv4[0] == 127 {
			loopback = true
		}
	}
	if !loopback {
		return false, nil
	}
	backendPort := parsed.Port()
	if backendPort == "" {
		if parsed.Scheme == "http" {
			return port == 80, nil
		}
		return port == 443, nil
	}
	parsedPort, err := strconv.Atoi(backendPort)
	if err != nil || parsedPort < 1 || parsedPort > 65535 {
		return false, ErrConflict
	}
	return parsedPort == port, nil
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
	digest := sha256.Sum256([]byte(nodeID))
	return m.withCoordinationLock(ctx, "node-"+hex.EncodeToString(digest[:])[:24]+".lock", operation)
}

func (m *Manager) withBackendReservationLock(ctx context.Context, operation func() error) error {
	return m.withCoordinationLock(ctx, "backend-reservations.lock", operation)
}

func (m *Manager) withCoordinationLock(ctx context.Context, name string, operation func() error) error {
	if ctx == nil {
		ctx = context.Background()
	}
	if _, err := validatePrivateDirectory(m.coordinationRoot); err != nil {
		return ErrPermissionDenied
	}
	lockPath := filepath.Join(m.coordinationRoot, name)
	fd, err := syscall.Open(lockPath, syscall.O_CREAT|syscall.O_RDWR|syscall.O_NOFOLLOW|syscall.O_CLOEXEC, 0o600)
	if err != nil {
		return ErrPermissionDenied
	}
	lockFile := os.NewFile(uintptr(fd), lockPath)
	defer lockFile.Close()
	info, err := lockFile.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm() != 0o600 ||
		linkCount(info) != 1 || !ownedByCurrentUser(info) {
		return ErrPermissionDenied
	}

	deadline := time.Now().Add(mutationLockWait)
	for {
		err = syscall.Flock(fd, syscall.LOCK_EX|syscall.LOCK_NB)
		if err == nil {
			break
		}
		if err != syscall.EWOULDBLOCK && err != syscall.EAGAIN && err != syscall.EINTR {
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
	operationErr := operation()
	if err := syscall.Flock(fd, syscall.LOCK_UN); err != nil {
		if operationErr != nil {
			return errors.Join(operationErr, ErrUncertain)
		}
		return fmt.Errorf("%w: coordination lock could not be released", ErrUncertain)
	}
	return operationErr
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

func reservationForRegistration(record registration, state RegistrationState) backendPortReservation {
	origin, _ := tailscale.Origin(record.DNSName, record.HTTPSPort)
	return backendPortReservation{
		Schema: 1, InstallationID: record.InstallationID, Scope: record.Scope, NodeID: record.NodeID,
		HTTPSPort: record.HTTPSPort, BackendPort: record.BackendPort, Origin: origin,
		State: state, UpdatedAt: time.Now().UTC().Format(time.RFC3339Nano),
	}
}

func (r backendPortReservation) Validate() error {
	if r.Schema != 1 || !validLabel(r.InstallationID) || (r.Scope != "production" && r.Scope != "development") ||
		!validNodeID(r.NodeID) || r.HTTPSPort < 1 || r.HTTPSPort > 65535 || r.BackendPort < 1 || r.BackendPort > 65535 ||
		r.HTTPSPort == r.BackendPort || !validCanonicalOrigin(r.Origin) {
		return ErrUncertain
	}
	switch r.State {
	case StateUnconfigured, StatePublishPending, StateRegistered, StatePublishUncertain,
		StateRemovePending, StateRemoveUncertain, StateRemoved, StateReconciledPresent, StateReconciledAbsent:
	default:
		return ErrUncertain
	}
	if _, err := time.Parse(time.RFC3339Nano, r.UpdatedAt); err != nil {
		return ErrUncertain
	}
	return nil
}

func sameBackendReservation(left, right backendPortReservation) bool {
	return left.Schema == right.Schema && left.InstallationID == right.InstallationID && left.Scope == right.Scope &&
		left.NodeID == right.NodeID && left.HTTPSPort == right.HTTPSPort && left.BackendPort == right.BackendPort && left.Origin == right.Origin
}

func (m *Manager) readBackendReservation(port int) (*backendPortReservation, error) {
	if port < 1 || port > 65535 {
		return nil, ErrConflict
	}
	root, err := validatePrivateDirectory(m.coordinationRoot)
	if err != nil || root != m.coordinationRoot {
		return nil, ErrUncertain
	}
	path := filepath.Join(m.coordinationRoot, fmt.Sprintf("backend-port-%d.json", port))
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil || info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() || info.Mode().Perm() != 0o600 ||
		info.Size() > backendReservationMaxBytes || linkCount(info) != 1 || !ownedByCurrentUser(info) {
		return nil, ErrUncertain
	}
	data, err := os.ReadFile(path)
	if err != nil || validateJSON(data, backendReservationMaxBytes, 6, 128) != nil {
		return nil, ErrUncertain
	}
	var reservation backendPortReservation
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&reservation) != nil || decoder.Decode(new(any)) != io.EOF || reservation.Validate() != nil || reservation.BackendPort != port {
		return nil, ErrUncertain
	}
	return &reservation, nil
}

func (m *Manager) writeBackendReservation(reservation backendPortReservation) error {
	root, err := validatePrivateDirectory(m.coordinationRoot)
	if err != nil || root != m.coordinationRoot || reservation.Validate() != nil {
		return ErrUncertain
	}
	reservations, err := m.readAllBackendReservations()
	if err != nil {
		return err
	}
	for _, current := range reservations {
		if current.BackendPort == reservation.BackendPort ||
			(current.NodeID == reservation.NodeID && current.HTTPSPort == reservation.HTTPSPort) {
			if !sameBackendReservation(current, reservation) {
				return ErrConflict
			}
		}
	}
	current, err := m.readBackendReservation(reservation.BackendPort)
	if err != nil {
		return err
	}
	if current != nil && !sameBackendReservation(*current, reservation) {
		return ErrConflict
	}
	data, err := json.Marshal(reservation)
	if err != nil || len(data) > backendReservationMaxBytes {
		return ErrUncertain
	}
	return writeAtomic(m.coordinationRoot, fmt.Sprintf("backend-port-%d.json", reservation.BackendPort), data)
}

func (m *Manager) readAllBackendReservations() ([]backendPortReservation, error) {
	root, err := validatePrivateDirectory(m.coordinationRoot)
	if err != nil || root != m.coordinationRoot {
		return nil, ErrUncertain
	}
	entries, err := os.ReadDir(m.coordinationRoot)
	if err != nil {
		return nil, ErrUncertain
	}
	reservations := make([]backendPortReservation, 0)
	for _, entry := range entries {
		name := entry.Name()
		if !strings.HasPrefix(name, "backend-port-") || !strings.HasSuffix(name, ".json") {
			continue
		}
		portText := strings.TrimSuffix(strings.TrimPrefix(name, "backend-port-"), ".json")
		port, parseErr := strconv.Atoi(portText)
		if parseErr != nil || port < 1 || port > 65535 || name != fmt.Sprintf("backend-port-%d.json", port) {
			return nil, ErrUncertain
		}
		reservation, readErr := m.readBackendReservation(port)
		if readErr != nil || reservation == nil {
			return nil, ErrUncertain
		}
		reservations = append(reservations, *reservation)
	}
	return reservations, nil
}

func (m *Manager) removeBackendReservation(reservation backendPortReservation) error {
	current, err := m.readBackendReservation(reservation.BackendPort)
	if err != nil || current == nil {
		return err
	}
	if !sameBackendReservation(*current, reservation) {
		return ErrConflict
	}
	path := filepath.Join(m.coordinationRoot, fmt.Sprintf("backend-port-%d.json", reservation.BackendPort))
	if err := os.Remove(path); err != nil {
		return ErrUncertain
	}
	return syncDirectory(m.coordinationRoot)
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
	case StateReconciledPresent, StateReconciledAbsent:
		if r.ConsentScope != reconcileConsentScope || r.MutationAcknowledged {
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
	rootInfo, err := os.Lstat(path)
	if err != nil || rootInfo.Mode()&os.ModeSymlink != 0 {
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
