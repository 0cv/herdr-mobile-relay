package main

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"math/big"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/0cv/herdr-mobile-relay/internal/config"
	"github.com/0cv/herdr-mobile-relay/internal/deviceauth"
	"github.com/0cv/herdr-mobile-relay/internal/protocol"
	"github.com/0cv/herdr-mobile-relay/internal/transport"
	"github.com/0cv/herdr-mobile-relay/internal/web"
)

const (
	maxRequestRecords = 2_000
	controlSecretSize = 32
)

type fixtureOptions struct {
	oldRoot     string
	candidate   string
	runDir      string
	infoFile    string
	appHost     string
	controlHost string
}

type requestRecord struct {
	Method          string `json:"method"`
	Path            string `json:"path"`
	Accept          string `json:"accept_encoding,omitempty"`
	Release         string `json:"release"`
	Fault           string `json:"fault,omitempty"`
	FaultID         string `json:"fault_id,omitempty"`
	FaultGeneration string `json:"fault_generation,omitempty"`
	At              string `json:"at"`
}

type responseFault struct {
	ID         string    `json:"id,omitempty"`
	Generation string    `json:"generation,omitempty"`
	Method     string    `json:"method"`
	Path       string    `json:"path"`
	Kind       string    `json:"kind"`
	Remaining  int       `json:"remaining"`
	Barrier    string    `json:"barrier,omitempty"`
	LifetimeMs int       `json:"lifetime_ms,omitempty"`
	ExpiresAt  time.Time `json:"-"`
	key        string
	admitted   bool
	inFlight   int
}

type releaseRouter struct {
	mu                 sync.RWMutex
	old                *web.Handler
	oldRoot            string
	oldScript          string
	oldStyle           string
	candidate          *web.Handler
	active             string
	faults             map[string]*responseFault
	barriers           map[string]chan struct{}
	requests           []requestRecord
	invalidated        bool
	invalidationReason string
}

func newReleaseRouter(oldRoot, candidateRoot string) (*releaseRouter, error) {
	old, err := web.NewHandler(oldRoot)
	if err != nil {
		return nil, fmt.Errorf("open old web root: %w", err)
	}
	candidate, err := web.NewHandler(candidateRoot)
	if err != nil {
		_ = old.Close()
		return nil, fmt.Errorf("open candidate web root: %w", err)
	}
	return &releaseRouter{
		old: old, candidate: candidate, active: "old", oldRoot: oldRoot,
		oldScript: releaseAsset(oldRoot, "script", "/assets/app.js"),
		oldStyle:  releaseAsset(oldRoot, "style", "/assets/app.css"),
		faults:    make(map[string]*responseFault), barriers: make(map[string]chan struct{}),
		requests: make([]requestRecord, 0, maxRequestRecords),
	}, nil
}

func (r *releaseRouter) ServeHTTP(w http.ResponseWriter, request *http.Request) {
	release, handler, fault := r.route(request.Method, request.URL.Path)
	record := requestRecord{
		Method: request.Method, Path: request.URL.Path, Accept: request.Header.Get("Accept-Encoding"),
		Release: release, At: time.Now().UTC().Format(time.RFC3339Nano),
	}
	if fault != nil {
		record.Fault = fault.Kind
		record.FaultID = fault.ID
		record.FaultGeneration = fault.Generation
		r.record(record)
		r.applyFault(w, request, fault, handler)
		return
	}
	r.record(record)
	handler.ServeHTTP(w, request)
}

func (r *releaseRouter) route(method, path string) (string, *web.Handler, *responseFault) {
	r.mu.Lock()
	defer r.mu.Unlock()
	release := r.active
	handler := r.old
	if release == "candidate" {
		handler = r.candidate
	}
	r.invalidateExpiredLocked(time.Now())
	if r.invalidated {
		return release, handler, &responseFault{ID: "fixture-expired", Generation: r.invalidationReason, Method: "*", Path: "*", Kind: "expired", Remaining: -1}
	}
	key := faultKey(method, path)
	fault := r.faults[key]
	if fault == nil {
		key = faultKey("*", path)
		fault = r.faults[key]
	}
	if fault != nil && fault.Remaining != 0 {
		copy := *fault
		copy.key = key
		copy.admitted = true
		if fault.Remaining > 0 {
			fault.Remaining--
			if fault.Kind == "stall" {
				fault.inFlight++
				if fault.Remaining == 0 {
					copy.Remaining = 0
				}
			} else if fault.Remaining == 0 {
				delete(r.faults, key)
				if key != faultKey("*", path) {
					delete(r.faults, faultKey("*", path))
				}
			}
		}
		return release, handler, &copy
	}
	return release, handler, nil
}

func (r *releaseRouter) applyFault(w http.ResponseWriter, request *http.Request, fault *responseFault, handler *web.Handler) {
	switch fault.Kind {
	case "missing":
		http.NotFound(w, request)
	case "corrupt":
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("Content-Type", "application/javascript; charset=utf-8")
		if strings.HasSuffix(request.URL.Path, ".css") {
			w.Header().Set("Content-Type", "text/css; charset=utf-8")
		}
		_, _ = io.WriteString(w, "this is a deliberately corrupt mobile fixture response")
	case "old":
		if r.serveOldAsset(w, request) {
			return
		}
		r.mu.RLock()
		old := r.old
		r.mu.RUnlock()
		if old == nil {
			http.Error(w, "old release is unavailable", http.StatusServiceUnavailable)
			return
		}
		old.ServeHTTP(w, request)
	case "drop":
		hijacker, ok := w.(http.Hijacker)
		if !ok {
			http.Error(w, "connection drop is unavailable", http.StatusServiceUnavailable)
			return
		}
		connection, _, err := hijacker.Hijack()
		if err == nil {
			_ = connection.Close()
		}
	case "stall":
		defer r.finishFault(fault)
		barrier := r.barrier(fault.Barrier)
		select {
		case <-barrier:
			invalidated, _ := r.invalidationState()
			if invalidated {
				http.Error(w, "fixture fault lifetime expired; test invalidated", http.StatusServiceUnavailable)
				return
			}
			if handler != nil {
				handler.ServeHTTP(w, request)
			}
		case <-request.Context().Done():
		}
	case "expired":
		http.Error(w, "fixture fault lifetime expired; test invalidated", http.StatusServiceUnavailable)
	default:
		http.Error(w, "unknown fixture fault", http.StatusInternalServerError)
	}
}

func (r *releaseRouter) serveOldAsset(w http.ResponseWriter, request *http.Request) bool {
	if !strings.HasPrefix(request.URL.Path, "/assets/") {
		return false
	}
	asset := ""
	contentType := ""
	switch {
	case strings.HasSuffix(request.URL.Path, ".js"):
		asset, contentType = r.oldScript, "application/javascript; charset=utf-8"
	case strings.HasSuffix(request.URL.Path, ".css"):
		asset, contentType = r.oldStyle, "text/css; charset=utf-8"
	default:
		return false
	}
	data, err := os.ReadFile(filepath.Join(r.oldRoot, strings.TrimPrefix(asset, "/")))
	if err != nil {
		http.NotFound(w, request)
		return true
	}
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Type", contentType)
	_, _ = w.Write(data)
	return true
}

func releaseAsset(root, field, fallback string) string {
	data, err := os.ReadFile(filepath.Join(root, "version.json"))
	if err == nil {
		var metadata map[string]any
		if json.Unmarshal(data, &metadata) == nil {
			if value, ok := metadata[field].(string); ok && strings.HasPrefix(value, "/") && safeFixturePath(value[1:]) {
				return value
			}
		}
	}
	return fallback
}

func safeFixturePath(value string) bool {
	return value != "" && !strings.Contains(value, "..") && !strings.Contains(value, "\\") && !strings.ContainsRune(value, '\x00')
}

func (r *releaseRouter) activate(release string) error {
	if release != "old" && release != "candidate" {
		return errors.New("release must be old or candidate")
	}
	r.mu.Lock()
	r.active = release
	r.mu.Unlock()
	return nil
}

func (r *releaseRouter) addFault(fault responseFault) error {
	if fault.Kind != "missing" && fault.Kind != "corrupt" && fault.Kind != "old" && fault.Kind != "drop" && fault.Kind != "stall" {
		return errors.New("unsupported fixture fault")
	}
	if fault.Method == "" {
		fault.Method = "*"
	}
	if fault.Path == "" || !strings.HasPrefix(fault.Path, "/") {
		return errors.New("fault path must be absolute")
	}
	if fault.Remaining == 0 {
		fault.Remaining = 1
	}
	if fault.Kind == "stall" && fault.Barrier == "" {
		return errors.New("stall faults require a barrier")
	}
	if fault.ID == "" {
		fault.ID = fmt.Sprintf("fault-%d", time.Now().UnixNano())
	}
	if fault.Generation == "" {
		fault.Generation = fault.ID
	}
	if fault.LifetimeMs <= 0 {
		fault.LifetimeMs = 120_000
	}
	fault.ExpiresAt = time.Now().Add(time.Duration(fault.LifetimeMs) * time.Millisecond)
	r.mu.Lock()
	defer r.mu.Unlock()
	r.invalidateExpiredLocked(time.Now())
	if r.invalidated {
		return errors.New("fixture fault lifetime expired; fixture is invalidated")
	}
	if fault.Kind == "stall" {
		if _, ok := r.barriers[fault.Barrier]; !ok {
			r.barriers[fault.Barrier] = make(chan struct{})
		}
	}
	copy := fault
	r.faults[faultKey(fault.Method, fault.Path)] = &copy
	return nil
}

func (r *releaseRouter) clearFault(id, generation, method, path string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.invalidateExpiredLocked(time.Now())
	if r.invalidated {
		return errors.New("fixture fault lifetime expired; fixture is invalidated")
	}
	cleared := false
	for key, fault := range r.faults {
		matchesID := id != "" && fault.ID == id && (generation == "" || fault.Generation == generation)
		matchesPath := id == "" && key == faultKey(method, path)
		if matchesID || matchesPath {
			delete(r.faults, key)
			cleared = true
		}
	}
	if !cleared {
		return errors.New("fault was not active")
	}
	return nil
}

func (r *releaseRouter) invalidateExpiredLocked(now time.Time) {
	for _, fault := range r.faults {
		if !fault.ExpiresAt.IsZero() && !now.Before(fault.ExpiresAt) {
			r.invalidated = true
			if r.invalidationReason == "" {
				r.invalidationReason = fmt.Sprintf("fault %s/%s expired", fault.ID, fault.Generation)
			}
		}
	}
}

func (r *releaseRouter) activeFaults() []responseFault {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.invalidateExpiredLocked(time.Now())
	faults := make([]responseFault, 0, len(r.faults))
	for _, fault := range r.faults {
		faults = append(faults, *fault)
	}
	sort.Slice(faults, func(i, j int) bool { return faults[i].ID < faults[j].ID })
	return faults
}

func (r *releaseRouter) invalidationState() (bool, string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.invalidateExpiredLocked(time.Now())
	return r.invalidated, r.invalidationReason
}

func (r *releaseRouter) releaseBarrier(name string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.invalidateExpiredLocked(time.Now())
	barrier, ok := r.barriers[name]
	if !ok {
		return fmt.Errorf("barrier %q is not declared", name)
	}
	select {
	case <-barrier:
	default:
		close(barrier)
	}
	return nil
}

func (r *releaseRouter) finishFault(fault *responseFault) {
	if fault == nil || !fault.admitted || fault.key == "" || fault.Kind != "stall" {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	active := r.faults[fault.key]
	if active == nil || active.ID != fault.ID || active.Generation != fault.Generation {
		return
	}
	if active.inFlight > 0 {
		active.inFlight--
	}
	if active.Remaining == 0 && active.inFlight == 0 {
		delete(r.faults, fault.key)
	}
}

func (r *releaseRouter) barrier(name string) <-chan struct{} {
	r.mu.Lock()
	defer r.mu.Unlock()
	if channel, ok := r.barriers[name]; ok {
		return channel
	}
	channel := make(chan struct{})
	r.barriers[name] = channel
	return channel
}

func (r *releaseRouter) record(record requestRecord) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.requests) == maxRequestRecords {
		copy(r.requests, r.requests[len(r.requests)-maxRequestRecords/2:])
		r.requests = r.requests[:maxRequestRecords/2]
	}
	r.requests = append(r.requests, record)
}

func (r *releaseRouter) snapshotRequests() []requestRecord {
	r.mu.RLock()
	defer r.mu.RUnlock()
	result := make([]requestRecord, len(r.requests))
	copy(result, r.requests)
	return result
}

func (r *releaseRouter) close() error {
	r.mu.Lock()
	old, candidate := r.old, r.candidate
	r.old, r.candidate = nil, nil
	r.mu.Unlock()
	if old != nil {
		_ = old.Close()
	}
	if candidate != nil {
		_ = candidate.Close()
	}
	return nil
}

func faultKey(method, path string) string {
	return strings.ToUpper(method) + " " + path
}

type authEvidence struct {
	mu                  sync.Mutex
	invitationAuthCount int
	credentialAuthCount int
	credentials         map[string]struct{}
}

func newAuthEvidence() *authEvidence {
	return &authEvidence{credentials: make(map[string]struct{})}
}

func (e *authEvidence) snapshot() map[string]any {
	e.mu.Lock()
	defer e.mu.Unlock()
	credentials := make([]string, 0, len(e.credentials))
	for credential := range e.credentials {
		credentials = append(credentials, credential)
	}
	sort.Strings(credentials)
	return map[string]any{
		"invitation_auth_count": e.invitationAuthCount,
		"credential_auth_count": e.credentialAuthCount,
		"credential_pseudonyms": credentials,
	}
}

type recordingResolver struct {
	store    *deviceauth.Store
	evidence *authEvidence
}

func (r *recordingResolver) ResolveE2EESecret(ctx context.Context, selector transport.E2EEAuthSelector) ([]byte, error) {
	return r.store.ResolveE2EESecret(ctx, selector)
}

func (r *recordingResolver) CompleteE2EEAuth(ctx context.Context, selector transport.E2EEAuthSelector, authenticated bool) (transport.E2EEAuthResult, error) {
	result, err := r.store.CompleteE2EEAuth(ctx, selector, authenticated)
	if err == nil && authenticated {
		r.evidence.mu.Lock()
		if selector.Kind == transport.E2EEAuthInvitation {
			r.evidence.invitationAuthCount++
		} else if selector.Kind == transport.E2EEAuthCredential {
			r.evidence.credentialAuthCount++
			r.evidence.credentials[credentialPseudonym(result.Identity.CredentialID)] = struct{}{}
		}
		r.evidence.mu.Unlock()
	}
	return result, err
}

func (r *recordingResolver) IsE2EEAuthRejected(err error) bool {
	return r.store.IsE2EEAuthRejected(err)
}

func credentialPseudonym(value string) string {
	hash := sha256.Sum256([]byte(value))
	return hexPrefix(hash[:], 12)
}

func hexPrefix(value []byte, length int) string {
	encoded := fmt.Sprintf("%x", value)
	if len(encoded) > length {
		return encoded[:length]
	}
	return encoded
}

type scriptedRelay struct {
	name         string
	version      string
	target       string
	url          string
	invitation   deviceauth.Invitation
	store        *deviceauth.Store
	evidence     *authEvidence
	hub          *transport.Hub
	server       *http.Server
	listener     net.Listener
	mu           sync.Mutex
	installCount int
	deployCount  int
	connections  atomic.Int64
}

func newScriptedRelay(name, target, appOrigin, runDir string, certificate tls.Certificate) (*scriptedRelay, error) {
	store, err := deviceauth.Open(filepath.Join(runDir, "relays", name))
	if err != nil {
		return nil, err
	}
	invitation, err := store.CreateInvitation("Mobile CI "+name, deviceauth.RoleController, "en")
	if err != nil {
		return nil, err
	}
	token := make([]byte, 32)
	if _, err := rand.Read(token); err != nil {
		return nil, err
	}
	cfg := &config.Config{
		Host: "127.0.0.1", Port: 0, Token: string(token),
		AllowedOrigins: []string{appOrigin},
	}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	hub := transport.NewHub(cfg, logger)
	relay := &scriptedRelay{
		name: name, version: target, target: target, invitation: invitation,
		store: store, evidence: newAuthEvidence(), hub: hub,
	}
	resolver := &recordingResolver{store: store, evidence: relay.evidence}
	hub.SetE2EEAuthResolver(resolver)
	hub.SetOnConnect(func(client *transport.ClientConn) {
		relay.connections.Add(1)
		hub.Send(client, relay.pushConfig())
		hub.Send(client, map[string]any{
			"type": "agents", "agents": []map[string]any{{
				"pane_id": name + ":agent", "workspace_id": name + "-workspace", "status": "idle",
				"project": "mobile-ci", "agent": "codex", "tab_label": "mobile-ci-agent",
			}},
		})
		hub.Send(client, map[string]any{
			"type": "workspaces", "workspaces": []map[string]any{{
				"workspace_id": name + "-workspace", "number": 1, "label": "Mobile CI", "pane_count": 1, "tab_count": 1,
				"cwd": "/work/mobile-ci",
			}},
		})
		hub.Send(client, map[string]any{
			"type": "inventory_status", "state": "ready", "error_code": "", "message": "",
		})
	})
	hub.SetOnDisconnect(func(_ *transport.ClientConn) { relay.connections.Add(-1) })
	hub.SetHandler(relay.handle)
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, err
	}
	secureListener := tls.NewListener(listener, &tls.Config{Certificates: []tls.Certificate{certificate}, MinVersion: tls.VersionTLS12})
	relay.listener = secureListener
	relay.server = &http.Server{Handler: http.HandlerFunc(hub.HandleWebSocket)}
	go func() { _ = relay.server.Serve(secureListener) }()
	relay.url = "wss://localhost:" + portOf(secureListener.Addr())
	return relay, nil
}

func (r *scriptedRelay) pushConfig() map[string]any {
	return map[string]any{
		"type": "push_config", "protocol": protocol.Version, "version": r.version,
		"release_version": r.version, "revision": "mobile-ci-fixture", "host": r.name,
		"home": "/home/mobile-ci", "capabilities": []string{
			"attention_classification", "clear_activities", "directory_browser", "self_update",
			"structured_questions", "slash_commands", "device_management",
		},
		"agent_profiles": []map[string]any{{"id": "codex", "label": "Codex"}},
		"inventory":      map[string]any{"state": "ready"},
		"update": map[string]any{
			"state": "available", "current_version": r.version, "available_version": r.target,
			"target_revision": "mobile-ci-target", "available_revision": "mobile-ci-target", "can_install": true,
		},
	}
}

func (r *scriptedRelay) handle(client *transport.ClientConn, raw map[string]any, admitted func()) {
	defer admitted()
	inbound, err := protocol.DecodeMap(raw)
	if err != nil {
		r.hub.Send(client, protocol.DecodeFailureResponse(raw))
		return
	}
	switch inbound.Type {
	case "check_update":
		r.hub.Send(client, map[string]any{"type": "update_status", "update": r.updateState("available")})
		r.commandResult(client, inbound.RequestID, inbound.Type, true, map[string]any{"update": r.updateState("available")})
	case "install_update":
		r.mu.Lock()
		r.installCount++
		r.mu.Unlock()
		r.hub.Send(client, map[string]any{"type": "update_status", "update": r.updateState("installing")})
		r.hub.Send(client, map[string]any{"type": "update_status", "update": r.updateState("succeeded")})
		r.commandResult(client, inbound.RequestID, inbound.Type, true, map[string]any{"update": r.updateState("succeeded")})
	case "deploy_app_update":
		r.mu.Lock()
		r.deployCount++
		r.mu.Unlock()
		r.commandResult(client, inbound.RequestID, inbound.Type, true, map[string]any{"app_deploy": map[string]any{"state": "succeeded", "target_version": r.target}})
	case "create_device_invitation":
		identity, _ := client.Identity()
		locale := inbound.Locale
		if locale == "" {
			locale = identity.Locale
		}
		role := deviceauth.Role(inbound.Role)
		if role == "" {
			role = deviceauth.RoleController
		}
		invitation, invitationErr := r.store.CreateInvitation(inbound.Name, role, locale)
		if invitationErr != nil {
			r.commandResultError(client, inbound.RequestID, inbound.Type, invitationErr.Error())
			return
		}
		r.commandResult(client, inbound.RequestID, inbound.Type, true, map[string]any{"invitation": invitation})
	case "device_list":
		identity, authenticated := client.Identity()
		if !authenticated {
			r.commandResultError(client, inbound.RequestID, inbound.Type, "device identity unavailable")
			return
		}
		r.commandResult(client, inbound.RequestID, inbound.Type, true, map[string]any{
			"devices": r.store.ListCredentials(identity.CredentialID), "current_device_id": identity.DeviceID, "role": identity.Role,
		})
	case "refresh_agents":
		r.hub.Send(client, map[string]any{"type": "agents", "agents": []map[string]any{{
			"pane_id": r.name + ":agent", "workspace_id": r.name + "-workspace", "status": "idle",
			"project": "mobile-ci", "agent": "codex", "tab_label": "mobile-ci-agent",
		}}})
		r.commandResult(client, inbound.RequestID, inbound.Type, true, nil)
	default:
		result := map[string]any{}
		if inbound.Type == "lease_pane_size" {
			result = map[string]any{"columns": inbound.Columns, "rows": inbound.Rows}
		}
		r.commandResult(client, inbound.RequestID, inbound.Type, true, result)
	}
}

func (r *scriptedRelay) updateState(state string) map[string]any {
	return map[string]any{
		"state": state, "current_version": r.version, "current_revision": "mobile-ci-fixture",
		"available_version": r.target, "available_revision": "mobile-ci-target",
		"target_revision": "mobile-ci-target", "can_install": state == "available",
	}
}

func (r *scriptedRelay) commandResult(client *transport.ClientConn, requestID, action string, ok bool, data map[string]any) {
	message := map[string]any{"type": "command_result", "request_id": requestID, "action": action, "ok": ok, "phase": "completed"}
	if data != nil {
		message["data"] = data
	}
	r.hub.Send(client, message)
}

func (r *scriptedRelay) commandResultError(client *transport.ClientConn, requestID, action, detail string) {
	r.hub.Send(client, map[string]any{
		"type": "command_result", "request_id": requestID, "action": action,
		"ok": false, "phase": "failed", "error": detail,
	})
}

func (r *scriptedRelay) snapshot() map[string]any {
	r.mu.Lock()
	installCount, deployCount := r.installCount, r.deployCount
	r.mu.Unlock()
	snapshot := r.evidence.snapshot()
	snapshot["name"] = r.name
	snapshot["url"] = r.url
	snapshot["connections"] = r.connections.Load()
	snapshot["install_update_count"] = installCount
	snapshot["deploy_app_update_count"] = deployCount
	return snapshot
}

func (r *scriptedRelay) dropConnections() {
	r.hub.DropConnections()
}

func (r *scriptedRelay) close(ctx context.Context) error {
	if r.server != nil {
		_ = r.server.Shutdown(ctx)
	}
	return r.hub.Shutdown(ctx)
}

type fixture struct {
	router          *releaseRouter
	relays          []*scriptedRelay
	appServer       *http.Server
	appListener     net.Listener
	control         *http.Server
	controlListener net.Listener
	appURL          string
	controlURL      string
	controlSecret   string
	certificate     string
	setupURLs       []string
	candidate       string
	old             string
	shutdownOnce    sync.Once
}

func (f *fixture) snapshot() map[string]any {
	f.router.mu.RLock()
	active := f.router.active
	f.router.mu.RUnlock()
	faults := f.router.activeFaults()
	invalidated, invalidationReason := f.router.invalidationState()
	relays := make([]map[string]any, 0, len(f.relays))
	for _, relay := range f.relays {
		relays = append(relays, relay.snapshot())
	}
	return map[string]any{
		"active_release":      active,
		"app_url":             f.appURL,
		"candidate":           f.candidate,
		"old":                 f.old,
		"requests":            f.router.snapshotRequests(),
		"faults":              faults,
		"invalidated":         invalidated,
		"invalidation_reason": invalidationReason,
		"relays":              relays,
	}
}

func (f *fixture) controlHandler(w http.ResponseWriter, request *http.Request) {
	if !hmac.Equal([]byte(request.Header.Get("X-Herdr-Fixture-Secret")), []byte(f.controlSecret)) {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	if (request.URL.Path == "/activate" || request.URL.Path == "/fault" || request.URL.Path == "/fault/clear" || request.URL.Path == "/fault/release" || request.URL.Path == "/barrier/release" || request.URL.Path == "/relay/drop" || request.URL.Path == "/shutdown") && request.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	switch request.URL.Path {
	case "/state":
		writeJSON(w, f.snapshot())
	case "/setup":
		writeJSON(w, map[string]any{"app_url": f.appURL, "setup_urls": f.setupURLs, "ca_certificate": f.certificate})
	case "/activate":
		var payload struct {
			Release string `json:"release"`
		}
		if err := json.NewDecoder(io.LimitReader(request.Body, 16*1024)).Decode(&payload); err != nil {
			http.Error(w, "invalid request", http.StatusBadRequest)
			return
		}
		if err := f.router.activate(payload.Release); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		writeJSON(w, map[string]any{"active_release": payload.Release})
	case "/fault":
		var fault responseFault
		if err := json.NewDecoder(io.LimitReader(request.Body, 16*1024)).Decode(&fault); err != nil {
			http.Error(w, "invalid request", http.StatusBadRequest)
			return
		}
		if err := f.router.addFault(fault); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		writeJSON(w, map[string]any{"ok": true})
	case "/fault/clear", "/fault/release":
		var payload struct {
			ID         string `json:"id"`
			Generation string `json:"generation"`
			Method     string `json:"method"`
			Path       string `json:"path"`
		}
		if request.Body != nil {
			if err := json.NewDecoder(io.LimitReader(request.Body, 16*1024)).Decode(&payload); err != nil && !errors.Is(err, io.EOF) {
				http.Error(w, "invalid request", http.StatusBadRequest)
				return
			}
		}
		if err := f.router.clearFault(payload.ID, payload.Generation, payload.Method, payload.Path); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		writeJSON(w, map[string]any{"ok": true})
	case "/barrier/release":
		name := request.URL.Query().Get("name")
		if err := f.router.releaseBarrier(name); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		writeJSON(w, map[string]any{"ok": true})
	case "/relay/drop":
		name := request.URL.Query().Get("name")
		for _, relay := range f.relays {
			if relay.name == name {
				relay.dropConnections()
				writeJSON(w, map[string]any{"ok": true})
				return
			}
		}
		http.Error(w, "unknown relay", http.StatusNotFound)
	case "/requests":
		writeJSON(w, f.router.snapshotRequests())
	case "/shutdown":
		writeJSON(w, map[string]any{"ok": true})
		go f.shutdown()
	default:
		http.NotFound(w, request)
	}
}

func (f *fixture) shutdown() {
	f.shutdownOnce.Do(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if f.control != nil {
			_ = f.control.Shutdown(ctx)
		}
		if f.appServer != nil {
			_ = f.appServer.Shutdown(ctx)
		}
		for _, relay := range f.relays {
			_ = relay.close(ctx)
		}
		_ = f.router.close()
	})
}

type fixtureInfo struct {
	AppURL        string   `json:"app_url"`
	RelayURLs     []string `json:"relay_urls"`
	SetupURLs     []string `json:"setup_urls"`
	ControlURL    string   `json:"control_url"`
	ControlSecret string   `json:"control_secret"`
	Certificate   string   `json:"ca_certificate"`
	OldRelease    string   `json:"old_release"`
	Candidate     string   `json:"candidate_release"`
}

func main() {
	options := fixtureOptions{}
	flag.StringVar(&options.oldRoot, "old-root", "", "old web root")
	flag.StringVar(&options.candidate, "candidate-root", "", "candidate web root")
	flag.StringVar(&options.runDir, "run-dir", "", "private fixture directory")
	flag.StringVar(&options.infoFile, "info-file", "", "private fixture info file")
	flag.StringVar(&options.appHost, "app-host", "127.0.0.1", "app bind host")
	flag.StringVar(&options.controlHost, "control-host", "127.0.0.1", "control bind host")
	flag.Parse()
	if err := run(options); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run(options fixtureOptions) error {
	for name, value := range map[string]string{"old-root": options.oldRoot, "candidate-root": options.candidate, "run-dir": options.runDir, "info-file": options.infoFile} {
		if value == "" {
			return fmt.Errorf("-%s is required", name)
		}
	}
	if err := os.MkdirAll(options.runDir, 0o700); err != nil {
		return err
	}
	certificate, certificatePath, err := makeCertificate(options.runDir)
	if err != nil {
		return err
	}
	router, err := newReleaseRouter(options.oldRoot, options.candidate)
	if err != nil {
		return err
	}
	appListener, err := net.Listen("tcp", net.JoinHostPort(options.appHost, "0"))
	if err != nil {
		_ = router.close()
		return err
	}
	appTLS := tls.NewListener(appListener, &tls.Config{Certificates: []tls.Certificate{certificate}, MinVersion: tls.VersionTLS12})
	appServer := &http.Server{Handler: router}
	go func() { _ = appServer.Serve(appTLS) }()
	appURL := "https://localhost:" + portOf(appTLS.Addr())
	appOrigin := appURL

	fixture := &fixture{
		router: router, appServer: appServer, appListener: appTLS,
		appURL: appOrigin, certificate: certificatePath, candidate: options.candidate, old: options.oldRoot,
	}
	for _, name := range []string{"alpha", "beta"} {
		relay, relayErr := newScriptedRelay(name, releaseVersion(options.candidate), appOrigin, options.runDir, certificate)
		if relayErr != nil {
			fixture.shutdown()
			return relayErr
		}
		fixture.relays = append(fixture.relays, relay)
	}
	for _, relay := range fixture.relays {
		fixture.setupURLs = append(fixture.setupURLs, setupURL(appOrigin, relay.url, relay.invitation, relay.name))
	}
	controlSecretBytes := make([]byte, controlSecretSize)
	if _, err := rand.Read(controlSecretBytes); err != nil {
		fixture.shutdown()
		return err
	}
	fixture.controlSecret = base64.RawURLEncoding.EncodeToString(controlSecretBytes)
	controlListener, err := net.Listen("tcp", net.JoinHostPort(options.controlHost, "0"))
	if err != nil {
		fixture.shutdown()
		return err
	}
	fixture.controlListener = controlListener
	fixture.control = &http.Server{Handler: http.HandlerFunc(fixture.controlHandler)}
	go func() { _ = fixture.control.Serve(controlListener) }()
	fixture.controlURL = "http://127.0.0.1:" + portOf(controlListener.Addr())
	info := fixtureInfo{
		AppURL: fixture.appURL, RelayURLs: []string{fixture.relays[0].url, fixture.relays[1].url},
		SetupURLs: fixture.setupURLs, ControlURL: fixture.controlURL, ControlSecret: fixture.controlSecret,
		Certificate: certificatePath, OldRelease: options.oldRoot, Candidate: options.candidate,
	}
	if err := writePrivateJSON(options.infoFile, info); err != nil {
		fixture.shutdown()
		return err
	}
	fmt.Printf("fixture ready app=%s control=%s\n", fixture.appURL, fixture.controlURL)
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	<-ctx.Done()
	fixture.shutdown()
	return nil
}

func releaseVersion(root string) string {
	data, err := os.ReadFile(filepath.Join(root, "version.json"))
	if err != nil {
		return "candidate"
	}
	var value struct {
		Version        string `json:"version"`
		ReleaseVersion string `json:"release_version"`
	}
	if json.Unmarshal(data, &value) != nil {
		return "candidate"
	}
	if value.ReleaseVersion != "" {
		return value.ReleaseVersion
	}
	return value.Version
}

func setupURL(appOrigin, relayURL string, invitation deviceauth.Invitation, label string) string {
	values := url.Values{}
	values.Set("setup", invitation.Secret)
	values.Set("invite", invitation.InvitationID)
	values.Set("invite_version", fmt.Sprint(invitation.Version))
	values.Set("invite_expires", fmt.Sprint(invitation.ExpiresAt.UnixMilli()))
	values.Set("label", label)
	values.Set("relay", relayURL)
	return appOrigin + "/#" + values.Encode()
}

func makeCertificate(runDir string) (tls.Certificate, string, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return tls.Certificate{}, "", err
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 120))
	if err != nil {
		return tls.Certificate{}, "", err
	}
	template := &x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: "Herdr mobile CI"},
		NotBefore:             time.Now().Add(-time.Minute),
		NotAfter:              time.Now().Add(24 * time.Hour),
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
		IsCA:                  true,
		DNSNames:              []string{"localhost"},
		IPAddresses:           []net.IP{net.ParseIP("127.0.0.1"), net.ParseIP("::1")},
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		return tls.Certificate{}, "", err
	}
	certificatePath := filepath.Join(runDir, "fixture-ca.pem")
	keyPath := filepath.Join(runDir, "fixture-ca-key.pem")
	if err := os.WriteFile(certificatePath, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o600); err != nil {
		return tls.Certificate{}, "", err
	}
	keyBytes, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return tls.Certificate{}, "", err
	}
	if err := os.WriteFile(keyPath, pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyBytes}), 0o600); err != nil {
		return tls.Certificate{}, "", err
	}
	certificate, err := tls.LoadX509KeyPair(certificatePath, keyPath)
	return certificate, certificatePath, err
}

func writePrivateJSON(filename string, value any) error {
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	if err := os.WriteFile(filename, append(data, '\n'), 0o600); err != nil {
		return err
	}
	return os.Chmod(filename, 0o600)
}

func writeJSON(w http.ResponseWriter, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	data, err := json.Marshal(value)
	if err != nil {
		http.Error(w, "could not encode response", http.StatusInternalServerError)
		return
	}
	_, _ = w.Write(data)
}

func portOf(address net.Addr) string {
	_, port, err := net.SplitHostPort(address.String())
	if err != nil {
		return "0"
	}
	return port
}
