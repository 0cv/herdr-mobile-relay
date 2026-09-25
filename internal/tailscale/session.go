package tailscale

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strconv"
	"sync"
	"time"
)

var (
	errSessionNotPrepared   = errors.New("Tailscale session has not been prepared")
	errSessionUnavailable   = errors.New("Tailscale session authority is unavailable")
	errSessionQuarantined   = errors.New("Tailscale session authority is quarantined")
	errSessionIdentity      = errors.New("Tailscale identity changed")
	errSessionRoute         = errors.New("Tailscale foreground route is not exact")
	errSessionNotDispatched = errors.New("Tailscale foreground registration was not dispatched")
	errSessionNoWrite       = errors.New("Tailscale foreground registration was refused without a write")
	errSessionWrite         = errors.New("Tailscale foreground registration is unresolved")
	errSessionClearing      = errors.New("Tailscale foreground route is not cleared")
)

const sessionCleanupBudget = 5 * time.Second
const sessionCleanupAttempts = 3

type registrationState uint8

const (
	registrationNotDispatched registrationState = iota
	registrationSettledSuccess
	registrationSettledNoWrite
	registrationUnresolved
)

// AuthorityStatus is a redacted caller-facing snapshot. It intentionally has
// no session ID, ownership constructor flag, or implicit destructor signal.
type AuthorityStatus struct {
	Prepared                     bool
	Active                       bool
	RouteValidated               bool
	Invalidated                  bool
	Quarantined                  bool
	RouteCleared                 bool
	LocalWatchClosed             bool
	RemoteWatchRetirementUnknown bool
	RegistrationOutcome          string
}

// SessionAuthority owns one in-process LocalAPI watcher and the foreground
// configuration key derived from its initial notification. Callers must hold
// their managed owner lock before construction and keep the listener bound but
// fail-closed until Validate succeeds and until Retire reports RouteCleared.
type SessionAuthority struct {
	api         *localAPI
	httpsPort   int
	backendPort int

	opMu sync.Mutex
	mu   sync.Mutex

	invalidationCh chan struct{}
	invalidateOnce sync.Once

	prepared            bool
	activationAttempted bool
	identity            *sessionIdentity
	route               Route
	routeJSON           json.RawMessage
	entryJSON           json.RawMessage
	watch               *localAPIWatch
	watchStop           context.CancelFunc

	registration    registrationState
	writeSettled    bool
	invalidated     bool
	validated       bool
	quarantined     bool
	routeCleared    bool
	localClosed     bool
	closingWatch    bool
	cleanupDeadline time.Time
	cleanupWrites   int
}

type sessionIdentity struct {
	version         string
	nodeID          string
	userID          int64
	dnsName         string
	tailnetName     string
	magicDNSSuffix  string
	magicDNSEnabled bool
	httpsCapable    bool
	certDomains     []string
	account         []byte
}

// NewSessionAuthority creates the fixed production LocalAPI adapter. The
// managed O lock must already be held; activation is a separate post-consent
// operation. httpsPort is the Serve listener port and backendPort is the
// already-bound loopback relay listener port.
func NewSessionAuthority(expectedVersion string, versionMetadata []byte, httpsPort, backendPort int) (*SessionAuthority, error) {
	api, err := newLocalAPI(expectedVersion, versionMetadata)
	if err != nil {
		return nil, err
	}
	return newSessionAuthority(api, httpsPort, backendPort)
}

func newSessionAuthority(api *localAPI, httpsPort, backendPort int) (*SessionAuthority, error) {
	if api == nil || api.client == nil || httpsPort < 1 || httpsPort > 65535 || backendPort < 1 || backendPort > 65535 {
		return nil, errLocalAPIRequest
	}
	return &SessionAuthority{
		api:            api,
		httpsPort:      httpsPort,
		backendPort:    backendPort,
		invalidationCh: make(chan struct{}),
		registration:   registrationNotDispatched,
	}, nil
}

// Prepare is a read-only pre-consent check. It accepts only an empty pinned
// Serve config and the already-enabled HTTPS capability.
func (a *SessionAuthority) Prepare(ctx context.Context) error {
	if !a.usable() || ctx == nil {
		return errSessionUnavailable
	}
	a.opMu.Lock()
	defer a.opMu.Unlock()
	a.mu.Lock()
	if a.prepared {
		a.mu.Unlock()
		return errSessionUnavailable
	}
	a.mu.Unlock()
	status, identity, err := a.readIdentity(ctx)
	if err != nil {
		return err
	}
	if !identity.httpsCapable {
		return errSessionUnavailable
	}
	config, _, err := a.api.serveConfig(ctx)
	if err != nil {
		return err
	}
	if _, _, err := parseFullServeConfig(config); err != nil || !isEmptyServeConfig(config) {
		return errSessionRoute
	}
	route := Route{
		Listener: "HTTPS",
		Host:     status.DNSName,
		Port:     a.httpsPort,
		Handler:  "Proxy",
		Path:     "/",
		Backend:  "http://127.0.0.1:" + strconv.Itoa(a.backendPort),
	}
	routeJSON, err := serveRouteConfig(route)
	if err != nil {
		return errSessionRoute
	}
	a.mu.Lock()
	a.prepared = true
	a.identity = &identity
	a.route = route
	a.routeJSON = routeJSON
	a.mu.Unlock()
	return nil
}

// Activate revalidates prepare after consent, opens a new owned watch, then
// performs exactly one conditional registration. Its request context does not
// own the live watch; cancellation can leave an unresolved write quarantined.
func (a *SessionAuthority) Activate(ctx context.Context) error {
	if !a.usable() || ctx == nil {
		return errSessionUnavailable
	}
	a.opMu.Lock()
	defer a.opMu.Unlock()
	a.mu.Lock()
	prepared := a.prepared
	identity := cloneIdentity(a.identity)
	watch := a.watch
	invalidated := a.invalidated || a.quarantined
	attempted := a.activationAttempted
	if prepared && !attempted {
		a.activationAttempted = true
	}
	a.mu.Unlock()
	if !prepared {
		return errSessionNotPrepared
	}
	if watch != nil || invalidated || attempted {
		return errSessionUnavailable
	}
	status, currentIdentity, err := a.readIdentity(ctx)
	if err != nil || !sameSessionIdentity(identity, currentIdentity) || !currentIdentity.httpsCapable {
		return errSessionIdentity
	}
	config, etag, err := a.api.serveConfig(ctx)
	if err != nil {
		return err
	}
	if !isEmptyServeConfig(config) || !validServeETag(etag) {
		return errSessionRoute
	}

	watchCtx, stopWatch := context.WithCancel(context.Background())
	ownedWatch, err := a.api.watch(watchCtx)
	if err != nil {
		stopWatch()
		return err
	}
	sessionID := ownedWatch.sessionID()
	if !validWatchSessionID(sessionID) {
		stopWatch()
		ownedWatch.Close()
		return errLocalAPIResponse
	}

	a.mu.Lock()
	a.watch = ownedWatch
	a.watchStop = stopWatch
	a.route.Session = sessionID
	a.mu.Unlock()

	// Re-read the complete config, then identity as the final operation before
	// the single POST. The ETag protects config against a later foreign write.
	config, etag, err = a.api.serveConfig(ctx)
	if err != nil || !isEmptyServeConfig(config) || !validServeETag(etag) {
		a.quarantine()
		go a.monitorWatch(ownedWatch)
		return errSessionRoute
	}
	status, currentIdentity, err = a.readIdentity(ctx)
	if err != nil || !currentIdentity.httpsCapable || !sameSessionIdentity(identity, currentIdentity) || status.DNSName != a.route.Host {
		a.quarantine()
		go a.monitorWatch(ownedWatch)
		return errSessionIdentity
	}
	if !a.watchLive(ownedWatch) {
		a.quarantine()
		go a.monitorWatch(ownedWatch)
		return errSessionUnavailable
	}
	entry := append(json.RawMessage(nil), a.routeJSON...)
	requestConfig, err := json.Marshal(map[string]any{
		"Foreground": map[string]json.RawMessage{sessionID: entry},
	})
	if err != nil {
		a.quarantine()
		go a.monitorWatch(ownedWatch)
		return errSessionRoute
	}
	result := a.api.setServeConfig(ctx, etag, requestConfig)
	a.mu.Lock()
	a.entryJSON = entry
	switch result.disposition {
	case localAPIWriteSettledSuccess:
		a.registration = registrationSettledSuccess
		a.writeSettled = true
	case localAPIWriteSettledNoWrite:
		a.registration = registrationSettledNoWrite
		a.writeSettled = true
	case localAPIWriteNotDispatched:
		a.registration = registrationNotDispatched
	case localAPIWriteUnresolved:
		a.registration = registrationUnresolved
		a.invalidated = true
		a.quarantined = true
	}
	a.mu.Unlock()
	go a.monitorWatch(ownedWatch)
	if result.disposition != localAPIWriteSettledSuccess {
		a.quarantine()
		switch result.disposition {
		case localAPIWriteNotDispatched:
			return errSessionNotDispatched
		case localAPIWriteSettledNoWrite:
			return errSessionNoWrite
		default:
			return errSessionWrite
		}
	}
	go a.monitorAuthority(ownedWatch)
	return nil
}

// Validate performs fresh identity, HTTPS-capability, version, complete Serve
// config and owned-watch checks. Health/TLS/listener readiness remain caller
// obligations; a valid owner snapshot is not pairing readiness by itself.
func (a *SessionAuthority) Validate(ctx context.Context) error {
	if !a.usable() || ctx == nil {
		return errSessionUnavailable
	}
	a.opMu.Lock()
	defer a.opMu.Unlock()
	a.mu.Lock()
	watch := a.watch
	identity := cloneIdentity(a.identity)
	route := a.route
	routeJSON := append(json.RawMessage(nil), a.routeJSON...)
	entryJSON := append(json.RawMessage(nil), a.entryJSON...)
	state := a.registration
	invalid := a.invalidated || a.quarantined || a.routeCleared
	a.mu.Unlock()
	if invalid || watch == nil || state != registrationSettledSuccess {
		return errSessionUnavailable
	}
	if !a.watchLive(watch) {
		a.quarantine()
		return errSessionUnavailable
	}
	config, _, err := a.api.serveConfig(ctx)
	if err != nil {
		a.invalidate()
		return errSessionRoute
	}
	root, absent, err := parseFullServeConfig(config)
	if err != nil || absent || len(root) != 1 {
		a.invalidate()
		return errSessionRoute
	}
	status, currentIdentity, err := a.readIdentity(ctx)
	if err != nil || !currentIdentity.httpsCapable || !sameSessionIdentity(identity, currentIdentity) || status.DNSName != route.Host {
		a.invalidate()
		return errSessionIdentity
	}
	foreground, ok := root["Foreground"]
	if !ok {
		a.invalidate()
		return errSessionRoute
	}
	var sessions map[string]json.RawMessage
	if json.Unmarshal(foreground, &sessions) != nil || len(sessions) != 1 || !sameJSON(sessions[route.Session], entryJSON) || !sameJSON(sessions[route.Session], routeJSON) {
		a.invalidate()
		return errSessionRoute
	}
	if !a.watchLive(watch) {
		a.invalidate()
		return errSessionUnavailable
	}
	a.mu.Lock()
	a.validated = true
	a.mu.Unlock()
	return nil
}

// Retire selectively clears only this watch's exact foreground entry. It
// closes/joins the local watch only after routeCleared is authoritative. There
// is intentionally no unconditional Close or destructor.
func (a *SessionAuthority) Retire(ctx context.Context) error {
	if !a.usable() || ctx == nil {
		return errSessionUnavailable
	}
	a.opMu.Lock()
	defer a.opMu.Unlock()
	return a.retireLocked(ctx)
}

func (a *SessionAuthority) retireLocked(parent context.Context) error {
	a.mu.Lock()
	watch := a.watch
	identity := cloneIdentity(a.identity)
	route := a.route
	entry := append(json.RawMessage(nil), a.entryJSON...)
	registration := a.registration
	writeSettled := a.writeSettled
	cleared := a.routeCleared
	a.mu.Unlock()
	if cleared {
		return a.closeWatchAfterClear(watch)
	}
	if watch == nil || identity == nil {
		return errSessionUnavailable
	}
	now := time.Now()
	deadline := now.Add(sessionCleanupBudget)
	a.mu.Lock()
	if !a.cleanupDeadline.IsZero() {
		deadline = a.cleanupDeadline
	}
	a.mu.Unlock()
	ctx, cancel := context.WithDeadline(parent, deadline)
	defer cancel()

	for attempt := 0; attempt < sessionCleanupAttempts; attempt++ {
		if ctx.Err() != nil {
			break
		}
		config, etag, err := a.api.serveConfig(ctx)
		if err != nil {
			a.quarantine()
			return errSessionQuarantined
		}
		if _, _, err := parseFullServeConfig(config); err != nil {
			a.quarantine()
			return errSessionQuarantined
		}
		_, currentIdentity, err := a.readIdentity(ctx)
		if err != nil || !sameSessionIdentity(identity, currentIdentity) || !currentIdentity.httpsCapable {
			a.quarantine()
			return errSessionIdentity
		}
		observed, exists, err := foregroundEntry(config, route.Session)
		if err != nil {
			a.quarantine()
			return errSessionQuarantined
		}
		if !exists {
			if registration == registrationUnresolved && !writeSettled {
				a.quarantine()
				return errSessionClearing
			}
			a.markRouteCleared()
			return a.closeWatchAfterClear(watch)
		}
		if registration == registrationNotDispatched || registration == registrationSettledNoWrite || len(entry) == 0 || !sameJSON(observed, entry) || !sameJSON(observed, a.routeJSON) {
			a.quarantine()
			return errSessionQuarantined
		}
		if !validServeETag(etag) || !a.watchLive(watch) {
			a.quarantine()
			return errSessionQuarantined
		}
		a.mu.Lock()
		if a.cleanupDeadline.IsZero() {
			a.cleanupDeadline = deadline
		}
		if a.cleanupWrites >= sessionCleanupAttempts || !time.Now().Before(a.cleanupDeadline) {
			a.mu.Unlock()
			a.quarantine()
			return errSessionClearing
		}
		a.mu.Unlock()
		deleteConfig, changed, err := removeForegroundSession(config, route.Session)
		if err != nil || !changed {
			a.quarantine()
			return errSessionQuarantined
		}
		result := a.api.setServeConfig(ctx, etag, deleteConfig)
		if result.disposition != localAPIWriteNotDispatched {
			a.mu.Lock()
			a.cleanupWrites++
			a.mu.Unlock()
		}
		switch result.disposition {
		case localAPIWriteSettledSuccess:
			// The cleanup acknowledgement makes a previously observed exact late
			// registration deletion-only; a fresh full readback is still required.
			a.mu.Lock()
			a.writeSettled = true
			a.mu.Unlock()
			writeSettled = true
			readback, _, readErr := a.api.serveConfig(ctx)
			if readErr != nil {
				a.quarantine()
				return errSessionQuarantined
			}
			if _, stillPresent, readErr := foregroundEntry(readback, route.Session); readErr != nil {
				a.quarantine()
				return errSessionQuarantined
			} else if !stillPresent {
				_, currentIdentity, err = a.readIdentity(ctx)
				if err != nil || !sameSessionIdentity(identity, currentIdentity) || !currentIdentity.httpsCapable {
					a.quarantine()
					return errSessionIdentity
				}
				if !a.watchLive(watch) {
					a.quarantine()
					return errSessionUnavailable
				}
				a.mu.Lock()
				a.writeSettled = true
				a.mu.Unlock()
				a.markRouteCleared()
				return a.closeWatchAfterClear(watch)
			}
		case localAPIWriteSettledNoWrite:
			// 412 is source-proven no-write; next iteration rebases from a fresh GET.
		case localAPIWriteUnresolved:
			// Never replay this body/tag. The next iteration is a fresh read and
			// can only issue a new deletion for the exact remaining owned value.
		case localAPIWriteNotDispatched:
			a.quarantine()
			return errSessionQuarantined
		}
	}
	a.quarantine()
	return errSessionClearing
}

func (a *SessionAuthority) closeWatchAfterClear(watch *localAPIWatch) error {
	if watch == nil {
		a.mu.Lock()
		a.localClosed = true
		a.mu.Unlock()
		return nil
	}
	a.mu.Lock()
	a.closingWatch = true
	stop := a.watchStop
	a.mu.Unlock()
	if stop != nil {
		stop()
	}
	watch.Close()
	select {
	case <-watch.done():
		a.mu.Lock()
		a.localClosed = true
		a.mu.Unlock()
		return nil
	default:
		a.quarantine()
		return errSessionClearing
	}
}

func (a *SessionAuthority) monitorAuthority(watch *localAPIWatch) {
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-watch.done():
			return
		case <-ticker.C:
			ctx, cancel := context.WithTimeout(context.Background(), localAPIRequestTimeout)
			err := a.Validate(ctx)
			cancel()
			if err != nil {
				a.quarantine()
				return
			}
		}
	}
}

func (a *SessionAuthority) monitorWatch(watch *localAPIWatch) {
	<-watch.done()
	a.opMu.Lock()
	defer a.opMu.Unlock()
	a.mu.Lock()
	if a.closingWatch || a.watch != watch {
		a.localClosed = true
		a.mu.Unlock()
		return
	}
	a.invalidated = true
	a.quarantined = true
	a.localClosed = true
	a.invalidateOnce.Do(func() { close(a.invalidationCh) })
	identity := cloneIdentity(a.identity)
	registration := a.registration
	session := a.route.Session
	a.mu.Unlock()
	if identity == nil || registration == registrationUnresolved {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), localAPIRequestTimeout)
	defer cancel()
	config, _, err := a.api.serveConfig(ctx)
	if err != nil {
		return
	}
	_, currentIdentity, err := a.readIdentity(ctx)
	if err != nil || !sameSessionIdentity(identity, currentIdentity) || !currentIdentity.httpsCapable {
		return
	}
	if _, exists, err := foregroundEntry(config, session); err != nil || exists {
		return
	}
	a.markRouteCleared()
}

func (a *SessionAuthority) readIdentity(ctx context.Context) (Status, sessionIdentity, error) {
	status, httpsCapable, err := a.api.statusWithHTTPSCapability(ctx)
	if err != nil || !status.LoggedIn || status.BackendState != "Running" || status.Version != a.api.expectedVersion || status.NodeID == "" || status.UserID <= 0 || status.DNSName == "" || status.TailnetName == "" || status.MagicDNSSuffix == "" {
		return Status{}, sessionIdentity{}, errSessionUnavailable
	}
	identity, err := makeSessionIdentity(status, httpsCapable)
	if err != nil {
		return Status{}, sessionIdentity{}, err
	}
	return status, identity, nil
}

func makeSessionIdentity(status Status, httpsCapable bool) (sessionIdentity, error) {
	account, err := json.Marshal(status.Account)
	if err != nil {
		return sessionIdentity{}, errLocalAPIResponse
	}
	return sessionIdentity{
		version:         status.Version,
		nodeID:          status.NodeID,
		userID:          status.UserID,
		dnsName:         status.DNSName,
		tailnetName:     status.TailnetName,
		magicDNSSuffix:  status.MagicDNSSuffix,
		magicDNSEnabled: status.MagicDNSEnabled,
		httpsCapable:    httpsCapable,
		certDomains:     append([]string(nil), status.CertDomains...),
		account:         account,
	}, nil
}

func cloneIdentity(identity *sessionIdentity) *sessionIdentity {
	if identity == nil {
		return nil
	}
	clone := *identity
	clone.certDomains = append([]string(nil), identity.certDomains...)
	clone.account = append([]byte(nil), identity.account...)
	return &clone
}

func sameSessionIdentity(a *sessionIdentity, b sessionIdentity) bool {
	return a != nil && reflect.DeepEqual(*a, b)
}

func (a *SessionAuthority) watchLive(watch *localAPIWatch) bool {
	if watch == nil {
		return false
	}
	select {
	case <-watch.done():
		return false
	default:
		return watch.err() == nil
	}
}

func (a *SessionAuthority) quarantine() {
	a.mu.Lock()
	a.invalidated = true
	a.quarantined = true
	a.invalidateOnce.Do(func() { close(a.invalidationCh) })
	a.mu.Unlock()
}

// Invalidation closes when live authority is revoked or quarantined. It is a
// one-way caller signal to revoke pairing and keep the backend listener inert.
func (a *SessionAuthority) Invalidation() <-chan struct{} {
	if a == nil || a.invalidationCh == nil {
		closed := make(chan struct{})
		close(closed)
		return closed
	}
	return a.invalidationCh
}

func (a *SessionAuthority) invalidate() { a.quarantine() }

func (a *SessionAuthority) markRouteCleared() {
	a.mu.Lock()
	if a.registration != registrationUnresolved || a.writeSettled {
		a.routeCleared = true
		a.invalidated = true
		a.quarantined = false
		a.invalidateOnce.Do(func() { close(a.invalidationCh) })
	}
	a.mu.Unlock()
}

func registrationName(state registrationState) string {
	switch state {
	case registrationSettledSuccess:
		return "settled-success"
	case registrationSettledNoWrite:
		return "settled-no-write"
	case registrationUnresolved:
		return "unresolved"
	default:
		return "not-dispatched"
	}
}

// Status returns only redacted lifecycle facts. Remote watcher retirement has
// no source-backed acknowledgement and therefore remains explicitly unknown.
func (a *SessionAuthority) Status() AuthorityStatus {
	if !a.usable() {
		return AuthorityStatus{RemoteWatchRetirementUnknown: true}
	}
	a.mu.Lock()
	watch := a.watch
	active := watch != nil && !a.localClosed
	status := AuthorityStatus{
		Prepared:                     a.prepared,
		Active:                       active,
		RouteValidated:               active && a.validated && !a.invalidated && !a.quarantined && !a.routeCleared && a.registration == registrationSettledSuccess,
		Invalidated:                  a.invalidated,
		Quarantined:                  a.quarantined,
		RouteCleared:                 a.routeCleared,
		LocalWatchClosed:             a.localClosed,
		RemoteWatchRetirementUnknown: true,
		RegistrationOutcome:          registrationName(a.registration),
	}
	a.mu.Unlock()
	if active {
		select {
		case <-watch.done():
			status.Active = false
			status.RouteValidated = false
			if !status.RouteCleared {
				a.quarantine()
				status.Invalidated = true
				status.Quarantined = true
			}
		default:
		}
	}
	return status
}

func (a *SessionAuthority) usable() bool {
	return a != nil && a.api != nil && a.api.client != nil && a.invalidationCh != nil
}

func (a *SessionAuthority) String() string {
	status := a.Status()
	return fmt.Sprintf("Tailscale session active=%t routeValidated=%t routeCleared=%t localWatchClosed=%t remoteWatchRetirementUnknown=%t", status.Active, status.RouteValidated, status.RouteCleared, status.LocalWatchClosed, status.RemoteWatchRetirementUnknown)
}
