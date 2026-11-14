package herdr

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

type FeatureState string

const (
	FeatureSupported   FeatureState = "supported"
	FeatureUnsupported FeatureState = "unsupported"
	FeatureUnknown     FeatureState = "unknown"
)

const (
	FeatureJSONAvailability    = "ordinary_json"
	FeatureWorkspaceMoveBlock  = "workspace.move_block"
	FeatureWorkspaceReordered  = "workspace.reordered"
	FeaturePaneRead            = "pane.read"
	FeatureTabMove             = "tab.move"
	FeatureEndpoint            = "client_shell.endpoint"
	FeatureWorkspaceManagement = "workspace_management"
	FeatureWorkspaceRename     = "workspace_rename"
	FeatureWorkspaceReorder    = "workspace_reorder"
	FeatureWorktreeManagement  = "worktree_management"
	FeatureTabRename           = "tab_rename"
	FeatureAgentLaunch         = "agent_launch"
	FeatureManualTerminalInput = "manual_terminal_input"
	FeatureSecretInput         = "secret_input"
	FeatureSemanticInput       = "semantic_input"
	FeaturePiIntegration       = "pi_integration"
)

type FeatureEvidence struct {
	State      FeatureState `json:"state"`
	Reason     string       `json:"reason"`
	Generation uint64       `json:"generation"`
}

type ServerStatus struct {
	OneStepLaunch              bool                       `json:"-"`
	InstalledClientVersion     string                     `json:"installed_client_version,omitempty"`
	ServerVersion              string                     `json:"server_version,omitempty"`
	ServerProtocol             int                        `json:"server_protocol,omitempty"`
	ServerProtocolKnown        bool                       `json:"server_protocol_known"`
	EndpointProtocolGeneration *int                       `json:"endpoint_protocol_generation,omitempty"`
	Generation                 uint64                     `json:"generation"`
	Features                   map[string]FeatureEvidence `json:"features"`
}

var liveFeatures = []string{
	FeatureJSONAvailability,
	FeatureWorkspaceMoveBlock,
	FeatureWorkspaceReordered,
	FeaturePaneRead,
	FeatureTabMove,
	FeatureEndpoint,
}

func initialServerStatus() ServerStatus {
	features := map[string]FeatureEvidence{}
	for _, name := range liveFeatures {
		features[name] = FeatureEvidence{State: FeatureUnknown, Reason: "not_checked"}
	}
	for _, name := range []string{
		FeatureWorkspaceManagement,
		FeatureWorkspaceRename,
		FeatureWorkspaceReorder,
		FeatureWorktreeManagement,
		FeatureTabRename,
		FeatureAgentLaunch,
		FeatureManualTerminalInput,
		FeatureSecretInput,
		FeatureSemanticInput,
		FeaturePiIntegration,
	} {
		features[name] = FeatureEvidence{State: FeatureSupported, Reason: "client_supported"}
	}
	return ServerStatus{Features: features}
}

func (s ServerStatus) Feature(name string) FeatureEvidence {
	if feature, ok := s.Features[name]; ok {
		return feature
	}
	return FeatureEvidence{State: FeatureUnknown, Reason: "not_checked", Generation: s.Generation}
}

func (s ServerStatus) Supports(name string) bool {
	return s.Feature(name).State == FeatureSupported
}

func cloneServerStatus(status ServerStatus) ServerStatus {
	features := status.Features
	status.Features = make(map[string]FeatureEvidence, len(features))
	for name, feature := range features {
		status.Features[name] = feature
	}
	if status.EndpointProtocolGeneration != nil {
		value := *status.EndpointProtocolGeneration
		status.EndpointProtocolGeneration = &value
	}
	return status
}

type capabilityManager struct {
	client         *Client
	mu             sync.RWMutex
	refreshMu      sync.Mutex
	refreshNow     chan struct{}
	status         ServerStatus
	refreshSeq     atomic.Uint64
	appliedSeq     uint64
	liveEpoch      uint64
	serverIdentity string
	onChange       func(ServerStatus)
}

func newCapabilityManager(client *Client) *capabilityManager {
	return &capabilityManager{
		client:     client,
		refreshNow: make(chan struct{}, 1),
		status:     initialServerStatus(),
		liveEpoch:  1,
	}
}

func (m *capabilityManager) snapshot() ServerStatus {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return cloneServerStatus(m.status)
}

func (m *capabilityManager) setOnChange(fn func(ServerStatus)) {
	m.mu.Lock()
	m.onChange = fn
	m.mu.Unlock()
}

func (m *capabilityManager) updateFeature(name string, state FeatureState, reason string) {
	m.updateFeatureAt(m.epoch(), name, state, reason)
}

func (m *capabilityManager) updateFeatureAt(epoch uint64, name string, state FeatureState, reason string) {
	m.mu.Lock()
	if epoch != m.liveEpoch {
		m.mu.Unlock()
		return
	}
	if feature := m.status.Feature(name); feature.State == state && feature.Reason == reason {
		m.mu.Unlock()
		return
	}
	if sequence := m.refreshSeq.Load(); sequence > m.appliedSeq {
		m.appliedSeq = sequence
	}
	current := cloneServerStatus(m.status)
	current.Generation++
	current.Features[name] = FeatureEvidence{State: state, Reason: reason, Generation: current.Generation}
	m.status = current
	callback := m.onChange
	published := cloneServerStatus(current)
	m.mu.Unlock()
	if callback != nil {
		callback(published)
	}
}

func (m *capabilityManager) epoch() uint64 {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.liveEpoch
}

func (m *capabilityManager) reusableFeature(name, identity string) (FeatureEvidence, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	feature := m.status.Feature(name)
	if feature.State == FeatureUnknown {
		return FeatureEvidence{}, false
	}
	if m.serverIdentity == identity ||
		m.serverIdentity == "" && m.status.ServerVersion == "" {
		return feature, true
	}
	return FeatureEvidence{}, false
}

func (m *capabilityManager) invalidateMany(names ...string) {
	m.mu.Lock()
	m.liveEpoch++
	if sequence := m.refreshSeq.Load(); sequence > m.appliedSeq {
		m.appliedSeq = sequence
	}
	current := cloneServerStatus(m.status)
	features := current.Features
	changed := false
	for _, name := range names {
		feature := features[name]
		if feature.State == FeatureUnknown && feature.Reason == "reconnect_required" {
			continue
		}
		features[name] = FeatureEvidence{
			State:  FeatureUnknown,
			Reason: "reconnect_required",
		}
		changed = true
	}
	current.ServerVersion = ""
	current.ServerProtocol = 0
	current.ServerProtocolKnown = false
	current.EndpointProtocolGeneration = nil
	current.Generation++
	m.serverIdentity = ""
	m.status = current
	callback := m.onChange
	published := cloneServerStatus(current)
	m.mu.Unlock()
	if changed && callback != nil {
		callback(published)
	}
}

func (m *capabilityManager) requestRefresh() {
	select {
	case m.refreshNow <- struct{}{}:
	default:
	}
}

func (m *capabilityManager) refresh(ctx context.Context) ServerStatus {
	m.refreshMu.Lock()
	defer m.refreshMu.Unlock()
	sequence := m.refreshSeq.Add(1)
	epoch := m.epoch()
	installed := m.client.installedClientVersion(ctx)
	next := initialServerStatus()
	next.InstalledClientVersion = installed

	raw, wrote, err := m.client.api.requestUnary(ctx, "ping", map[string]any{})
	if err != nil {
		next.Features[FeatureJSONAvailability] = FeatureEvidence{State: FeatureUnknown, Reason: capabilityTransportReason(wrote, err)}
		m.applyRefresh(sequence, epoch, "", next)
		return m.snapshot()
	}
	ping, err := decodePing(raw)
	if err != nil {
		next.Features[FeatureJSONAvailability] = FeatureEvidence{State: FeatureUnknown, Reason: "malformed_ping"}
		m.applyRefresh(sequence, epoch, "", next)
		return m.snapshot()
	}
	identity := pingIdentity(ping)
	next.ServerVersion = *ping.Version
	next.ServerProtocol = *ping.Protocol
	next.ServerProtocolKnown = true
	next.EndpointProtocolGeneration = ping.Capabilities.EndpointProtocolGeneration
	next.Features[FeatureJSONAvailability] = FeatureEvidence{State: FeatureSupported, Reason: "ping"}
	if ping.Capabilities.EndpointProtocolGeneration != nil {
		next.Features[FeatureEndpoint] = FeatureEvidence{State: FeatureSupported, Reason: "server_advertised"}
	} else {
		next.Features[FeatureEndpoint] = FeatureEvidence{State: FeatureUnknown, Reason: "not_advertised"}
	}
	// Validation-refusal probes surface as API errors in Herdr's log, so a
	// conclusive result is reused for the same server identity. Reconnects and
	// identity changes invalidate it, and real operations still correct it.
	for _, probe := range capabilityProbes {
		if feature, ok := m.reusableFeature(probe.feature, identity); ok {
			next.Features[probe.feature] = feature
			continue
		}
		state, reason := probe.run(ctx, m.client.api)
		next.Features[probe.feature] = FeatureEvidence{State: state, Reason: reason}
	}
	m.applyRefresh(sequence, epoch, identity, next)
	return m.snapshot()
}

func (m *capabilityManager) applyRefresh(sequence, epoch uint64, identity string, status ServerStatus) {
	m.mu.Lock()
	if sequence <= m.appliedSeq || epoch != m.liveEpoch {
		m.mu.Unlock()
		return
	}
	identityChanged := identity != "" && identity != m.serverIdentity
	if identityChanged {
		m.liveEpoch++
	}
	if identity != "" && (identity == m.serverIdentity ||
		m.serverIdentity == "" && m.status.ServerVersion == "") {
		for name, feature := range m.status.Features {
			if name != FeatureWorkspaceReordered && slices.Contains(liveFeatures, name) {
				continue
			}
			status.Features[name] = feature
		}
	}
	m.appliedSeq = sequence
	status.Generation = m.status.Generation + 1
	for name, feature := range status.Features {
		feature.Generation = status.Generation
		status.Features[name] = feature
	}
	m.serverIdentity = identity
	m.status = status
	callback := m.onChange
	published := cloneServerStatus(status)
	m.mu.Unlock()
	if callback != nil {
		callback(published)
	}
}

func capabilityTransportReason(wrote bool, err error) string {
	if errors.Is(err, context.DeadlineExceeded) {
		return "timeout"
	}
	if wrote {
		return "server_reply_unavailable"
	}
	return "server_unavailable"
}

func (m *capabilityManager) run(ctx context.Context, interval time.Duration) {
	if interval <= 0 {
		interval = 30 * time.Second
	}
	m.refresh(ctx)
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			m.refresh(ctx)
		case <-m.refreshNow:
			m.refresh(ctx)
		}
	}
}

func (c *Client) CapabilityStatus() ServerStatus {
	return c.capabilities.snapshot()
}

func (c *Client) SetCapabilityChangeCallback(callback func(ServerStatus)) {
	c.capabilities.setOnChange(callback)
}

func (c *Client) RunCapabilityRefresh(ctx context.Context, interval time.Duration) {
	c.capabilities.run(ctx, interval)
}

func (c *Client) InvalidateLiveCapabilities() {
	c.capabilities.invalidateMany(liveFeatures...)
	c.capabilities.requestRefresh()
}

func (c *Client) ShouldAttemptWorkspaceReordered() bool {
	return c.CapabilityStatus().Feature(FeatureWorkspaceReordered).State != FeatureUnsupported
}

func (c *Client) NoteWorkspaceReorderedSupported() {
	c.capabilities.updateFeature(FeatureWorkspaceReordered, FeatureSupported, "subscription_acknowledged")
}

func (c *Client) NoteWorkspaceReorderedUnsupported() {
	c.capabilities.updateFeature(FeatureWorkspaceReordered, FeatureUnsupported, "subscription_rejected")
}

func (c *Client) noteFeatureSupportedAt(epoch uint64, name, reason string) {
	c.capabilities.updateFeatureAt(epoch, name, FeatureSupported, reason)
}

func (c *Client) capabilityEpoch() uint64 {
	return c.capabilities.epoch()
}

func (c *Client) installedClientVersion(ctx context.Context) string {
	queryCtx, cancel := context.WithTimeout(ctx, time.Second)
	defer cancel()
	out, err := c.runCommand(queryCtx, "--version")
	if err != nil {
		return ""
	}
	line := strings.TrimSpace(strings.SplitN(string(out), "\n", 2)[0])
	if len(line) > 64 {
		return line[:64]
	}
	return line
}

type pingResponse struct {
	Type         string  `json:"type"`
	Version      *string `json:"version"`
	Protocol     *int    `json:"protocol"`
	Capabilities struct {
		EndpointProtocolGeneration *int  `json:"endpoint_protocol_generation"`
		SurfaceInterest            *bool `json:"surface_interest"`
		HealthCheck                *bool `json:"health_check"`
	} `json:"capabilities"`
}

func decodePing(raw json.RawMessage) (pingResponse, error) {
	var response pingResponse
	if err := json.Unmarshal(raw, &response); err != nil {
		return pingResponse{}, err
	}
	if response.Type != "pong" || response.Version == nil || response.Protocol == nil ||
		*response.Version == "" || *response.Protocol <= 0 {
		return pingResponse{}, errors.New("invalid pong")
	}
	return response, nil
}

func pingIdentity(response pingResponse) string {
	endpointGeneration := 0
	if response.Capabilities.EndpointProtocolGeneration != nil {
		endpointGeneration = *response.Capabilities.EndpointProtocolGeneration
	}
	return fmt.Sprintf("%s\x00%d\x00%d\x00%t\x00%t",
		*response.Version,
		*response.Protocol,
		endpointGeneration,
		response.Capabilities.SurfaceInterest != nil && *response.Capabilities.SurfaceInterest,
		response.Capabilities.HealthCheck != nil && *response.Capabilities.HealthCheck,
	)
}

type capabilityProbe struct {
	feature string
	method  string
	params  map[string]any
	// successType is the result type that proves support. Probes without one
	// are built to be refused, so any success is unexpected.
	successType string
	refusalCode string
}

// The pane.read probe uses an empty explicit ID, which cannot target a real
// pane. Its validation refusal proves method support without reading output,
// scrolling, or resizing a user's terminal, including when the server has no
// panes yet.
var capabilityProbes = []capabilityProbe{
	{
		feature:     FeatureWorkspaceMoveBlock,
		method:      "workspace.move_block",
		params:      map[string]any{"workspace_ids": []string{}},
		successType: "workspace_list",
		refusalCode: "workspace_move_block_failed",
	},
	{
		feature:     FeatureTabMove,
		method:      "tab.move",
		params:      map[string]any{"tab_id": "", "insert_index": 0},
		refusalCode: "tab_not_found",
	},
	{
		feature: FeaturePaneRead,
		method:  "pane.read",
		params: map[string]any{
			"pane_id":    "",
			"source":     "visible",
			"lines":      1,
			"format":     "ansi",
			"strip_ansi": false,
		},
		refusalCode: "pane_not_found",
	},
}

func (p capabilityProbe) run(ctx context.Context, api *socketAPIClient) (FeatureState, string) {
	raw, wrote, err := api.requestUnary(ctx, p.method, p.params)
	if err == nil {
		if p.successType == "" {
			return FeatureUnknown, "unexpected_probe_success"
		}
		var result struct {
			Type string `json:"type"`
		}
		if json.Unmarshal(raw, &result) == nil && result.Type == p.successType {
			return FeatureSupported, "probe_succeeded"
		}
		return FeatureUnknown, "unexpected_probe_result"
	}
	var cliErr *CLIError
	if errors.As(err, &cliErr) && cliErr != nil && cliErr.Code == p.refusalCode {
		return FeatureSupported, "recognized_validation_refusal"
	}
	if isUnsupportedMethod(err) {
		return FeatureUnsupported, "method_not_supported"
	}
	if errors.Is(err, context.DeadlineExceeded) || !wrote {
		return FeatureUnknown, capabilityTransportReason(wrote, err)
	}
	return FeatureUnknown, "probe_failed"
}

func isUnsupportedMethod(err error) bool {
	var cliErr *CLIError
	if !errors.As(err, &cliErr) || cliErr == nil {
		return false
	}
	switch cliErr.Code {
	case "unknown_method", "method_not_found", "unsupported_method":
		return true
	}
	return false
}

func (c *Client) noteSocketFeature(epoch uint64, name string, err error) {
	if err == nil {
		c.noteFeatureSupportedAt(epoch, name, "operation_succeeded")
		return
	}
	if isUnsupportedMethod(err) {
		c.capabilities.updateFeatureAt(epoch, name, FeatureUnsupported, "method_not_supported")
	}
}
