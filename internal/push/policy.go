package push

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/0cv/herdr-mobile-relay/internal/localize"
	"github.com/0cv/herdr-mobile-relay/internal/protocol"
)

type Category string

const (
	CategoryAttention Category = "attention"
	CategoryQuestion  Category = "question"
	CategoryBrief     Category = "brief"
	CategoryFinished  Category = "finished"
	CategoryUpdate    Category = "update"
	CategoryTest      Category = "test"
)

type PreviewMode string

const (
	PreviewHidden   PreviewMode = "hidden"
	PreviewQuestion PreviewMode = "question"
	PreviewBrief    PreviewMode = "brief"
)

type PushEventKey struct {
	DeviceID            string   `json:"device_id"`
	ServerSessionID     string   `json:"server_session_id"`
	PaneID              string   `json:"pane_id"`
	TerminalID          string   `json:"terminal_id"`
	AgentSessionID      string   `json:"agent_session_id"`
	Generation          int64    `json:"generation"`
	EventID             string   `json:"event_id"`
	InteractionRevision int64    `json:"interaction_revision"`
	Category            Category `json:"category"`
}

func (k PushEventKey) Validate() error {
	if strings.TrimSpace(k.DeviceID) == "" || strings.TrimSpace(k.EventID) == "" {
		return errors.New("push_invalid_event_key")
	}
	switch k.Category {
	case CategoryAttention, CategoryQuestion, CategoryBrief, CategoryFinished:
		if k.ServerSessionID == "" || k.PaneID == "" || k.TerminalID == "" || k.Generation < 0 {
			return errors.New("push_invalid_event_key")
		}
	case CategoryUpdate, CategoryTest:
	default:
		return errors.New("push_invalid_category")
	}
	return nil
}

func (k PushEventKey) Target() protocol.TargetRef {
	return protocol.TargetRef{
		ServerSessionID: k.ServerSessionID,
		PaneID:          k.PaneID,
		TerminalID:      k.TerminalID,
		Generation:      k.Generation,
		AgentSessionID:  k.AgentSessionID,
	}
}

func sameTarget(a, b protocol.TargetRef) bool {
	return a.ServerSessionID == b.ServerSessionID &&
		a.PaneID == b.PaneID &&
		a.TerminalID == b.TerminalID &&
		a.Generation == b.Generation &&
		a.AgentSessionID == b.AgentSessionID
}

type DevicePolicy struct {
	DeviceID       string            `json:"device_id"`
	Locale         string            `json:"locale"`
	Categories     map[Category]bool `json:"categories"`
	Settle         time.Duration     `json:"settle"`
	Cooldown       time.Duration     `json:"cooldown"`
	SnoozeUntil    time.Time         `json:"snooze_until,omitempty"`
	Snoozed        bool              `json:"snoozed,omitempty"`
	UpdateOnce     bool              `json:"update_once"`
	UpdateVersions map[string]bool   `json:"update_versions,omitempty"`
}

func DefaultDevicePolicy(deviceID, locale string) DevicePolicy {
	return DevicePolicy{
		DeviceID: deviceID,
		Locale:   string(localize.NormalizeLocale(locale)),
		Categories: map[Category]bool{
			CategoryAttention: true,
			CategoryQuestion:  true,
			CategoryBrief:     true,
			CategoryFinished:  false,
			CategoryUpdate:    true,
			CategoryTest:      true,
		},
		Settle:         2 * time.Second,
		Cooldown:       30 * time.Second,
		UpdateOnce:     true,
		UpdateVersions: make(map[string]bool),
	}
}

type PolicyDecision struct {
	Deliver bool
	DueAt   time.Time
	Code    string
}

type policyFile struct {
	GlobalSnoozeUntil time.Time               `json:"global_snooze_until,omitempty"`
	Policies          map[string]DevicePolicy `json:"policies"`
	LastAccepted      map[string]time.Time    `json:"last_accepted,omitempty"`
}

type PolicyEngine struct {
	mu          sync.Mutex
	path        string
	state       policyFile
	viewedPanes map[string]protocol.TargetRef
}

func newPolicyEngine(path string) (*PolicyEngine, error) {
	p := &PolicyEngine{
		path: path,
		state: policyFile{
			Policies:     make(map[string]DevicePolicy),
			LastAccepted: make(map[string]time.Time),
		},
		viewedPanes: make(map[string]protocol.TargetRef),
	}
	data, err := os.ReadFile(path)
	if err == nil {
		if err := json.Unmarshal(data, &p.state); err != nil {
			return nil, fmt.Errorf("decode push policies: %w", err)
		}
		var rawState struct {
			Policies map[string]json.RawMessage `json:"policies"`
		}
		if json.Unmarshal(data, &rawState) == nil {
			for deviceID, policy := range p.state.Policies {
				if !bytes.Contains(rawState.Policies[deviceID], []byte(`"update_once"`)) {
					policy.UpdateOnce = true
					p.state.Policies[deviceID] = policy
				}
			}
		}
	} else if !os.IsNotExist(err) {
		return nil, fmt.Errorf("read push policies: %w", err)
	}
	if p.state.Policies == nil {
		p.state.Policies = make(map[string]DevicePolicy)
	}
	if p.state.LastAccepted == nil {
		p.state.LastAccepted = make(map[string]time.Time)
	}
	return p, nil
}

func normalizePolicy(policy DevicePolicy) (DevicePolicy, error) {
	if strings.TrimSpace(policy.DeviceID) == "" {
		return DevicePolicy{}, errors.New("push_device_required")
	}
	policy.Locale = string(localize.NormalizeLocale(policy.Locale))
	if policy.Settle < 0 || policy.Cooldown < 0 {
		return DevicePolicy{}, errors.New("push_invalid_duration")
	}
	if policy.Categories == nil {
		policy.Categories = DefaultDevicePolicy(policy.DeviceID, policy.Locale).Categories
	}
	allowed := map[Category]bool{
		CategoryAttention: true, CategoryQuestion: true, CategoryBrief: true,
		CategoryFinished: true, CategoryUpdate: true, CategoryTest: true,
	}
	for category := range policy.Categories {
		if !allowed[category] {
			return DevicePolicy{}, errors.New("push_invalid_category")
		}
	}
	if policy.UpdateVersions == nil {
		policy.UpdateVersions = make(map[string]bool)
	}
	return policy, nil
}

func (p *PolicyEngine) Set(policy DevicePolicy) error {
	policy, err := normalizePolicy(policy)
	if err != nil {
		return err
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	previous, hadPrevious := p.state.Policies[policy.DeviceID]
	p.state.Policies[policy.DeviceID] = policy
	if err := p.persistLocked(); err != nil {
		if hadPrevious {
			p.state.Policies[policy.DeviceID] = previous
		} else {
			delete(p.state.Policies, policy.DeviceID)
		}
		return err
	}
	return nil
}
func (p *PolicyEngine) RemoveDevice(deviceID string) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	previousPolicy, hadPolicy := p.state.Policies[deviceID]
	delete(p.state.Policies, deviceID)
	removedAccepted := make(map[string]time.Time)
	prefix := deviceID + "\x00"
	for slot, accepted := range p.state.LastAccepted {
		if strings.HasPrefix(slot, prefix) {
			removedAccepted[slot] = accepted
			delete(p.state.LastAccepted, slot)
		}
	}
	previousViewed, hadViewed := p.viewedPanes[deviceID]
	delete(p.viewedPanes, deviceID)
	if err := p.persistLocked(); err != nil {
		if hadPolicy {
			p.state.Policies[deviceID] = previousPolicy
		}
		for slot, accepted := range removedAccepted {
			p.state.LastAccepted[slot] = accepted
		}
		if hadViewed {
			p.viewedPanes[deviceID] = previousViewed
		}
		return err
	}
	return nil
}

func (p *PolicyEngine) Get(deviceID, locale string) DevicePolicy {
	p.mu.Lock()
	defer p.mu.Unlock()
	if policy, ok := p.state.Policies[deviceID]; ok {
		return clonePolicy(policy)
	}
	return DefaultDevicePolicy(deviceID, locale)
}

func clonePolicy(policy DevicePolicy) DevicePolicy {
	copyPolicy := policy
	copyPolicy.Categories = make(map[Category]bool, len(policy.Categories))
	for category, enabled := range policy.Categories {
		copyPolicy.Categories[category] = enabled
	}
	copyPolicy.UpdateVersions = make(map[string]bool, len(policy.UpdateVersions))
	for version, sent := range policy.UpdateVersions {
		copyPolicy.UpdateVersions[version] = sent
	}
	return copyPolicy
}

func (p *PolicyEngine) SetViewedPane(deviceID string, target *protocol.TargetRef) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if target == nil {
		delete(p.viewedPanes, deviceID)
		return
	}
	p.viewedPanes[deviceID] = *target
}

func (p *PolicyEngine) SetGlobalSnooze(until time.Time) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	previous := p.state.GlobalSnoozeUntil
	p.state.GlobalSnoozeUntil = until
	if err := p.persistLocked(); err != nil {
		p.state.GlobalSnoozeUntil = previous
		return err
	}
	return nil
}

func (p *PolicyEngine) Decide(key PushEventKey, locale string, now time.Time) PolicyDecision {
	p.mu.Lock()
	defer p.mu.Unlock()
	policy, ok := p.state.Policies[key.DeviceID]
	if !ok {
		policy = DefaultDevicePolicy(key.DeviceID, locale)
	}
	if !policy.Categories[key.Category] {
		return PolicyDecision{Code: "push_category_disabled"}
	}
	timedSnoozeActive := !policy.SnoozeUntil.IsZero() && now.Before(policy.SnoozeUntil)
	if policy.Snoozed && policy.SnoozeUntil.IsZero() || timedSnoozeActive || now.Before(p.state.GlobalSnoozeUntil) {
		return PolicyDecision{Code: "push_snoozed"}
	}
	if viewed, ok := p.viewedPanes[key.DeviceID]; ok && sameTarget(viewed, key.Target()) {
		return PolicyDecision{Code: "push_viewed_pane"}
	}
	due := now.Add(policy.Settle)
	if last := p.state.LastAccepted[policySlot(key)]; !last.IsZero() {
		cooldownEnd := last.Add(policy.Cooldown)
		if cooldownEnd.After(due) {
			due = cooldownEnd
		}
	}
	return PolicyDecision{Deliver: true, DueAt: due}
}
func (p *PolicyEngine) Allows(key PushEventKey, locale string, now time.Time) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	policy, ok := p.state.Policies[key.DeviceID]
	if !ok {
		policy = DefaultDevicePolicy(key.DeviceID, locale)
	}
	if !policy.Categories[key.Category] {
		return false
	}
	timedSnoozeActive := !policy.SnoozeUntil.IsZero() && now.Before(policy.SnoozeUntil)
	if policy.Snoozed && policy.SnoozeUntil.IsZero() || timedSnoozeActive || now.Before(p.state.GlobalSnoozeUntil) {
		return false
	}
	viewed, viewedExists := p.viewedPanes[key.DeviceID]
	return !viewedExists || !sameTarget(viewed, key.Target())
}

func (p *PolicyEngine) MarkAccepted(key PushEventKey, acceptedAt time.Time) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	slot := policySlot(key)
	previous, existed := p.state.LastAccepted[slot]
	p.state.LastAccepted[slot] = acceptedAt
	if err := p.persistLocked(); err != nil {
		if existed {
			p.state.LastAccepted[slot] = previous
		} else {
			delete(p.state.LastAccepted, slot)
		}
		return err
	}
	return nil
}

func (p *PolicyEngine) ClaimUpdateVersion(deviceID, version string) (bool, error) {
	if strings.TrimSpace(deviceID) == "" || strings.TrimSpace(version) == "" {
		return false, errors.New("push_update_version_required")
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	policy, ok := p.state.Policies[deviceID]
	if !ok {
		policy = DefaultDevicePolicy(deviceID, "en")
	}
	if !policy.UpdateOnce {
		return true, nil
	}
	if policy.UpdateVersions[version] {
		return false, nil
	}
	policy.UpdateVersions[version] = true
	p.state.Policies[deviceID] = policy
	if err := p.persistLocked(); err != nil {
		delete(policy.UpdateVersions, version)
		p.state.Policies[deviceID] = policy
		return false, err
	}
	return true, nil
}

func policySlot(key PushEventKey) string {
	return key.DeviceID + "\x00" + key.ServerSessionID + "\x00" + key.PaneID + "\x00" + key.TerminalID + "\x00" + fmt.Sprint(key.Generation) + "\x00" + key.AgentSessionID + "\x00" + string(key.Category)
}

func (p *PolicyEngine) persistLocked() error {
	data, err := json.MarshalIndent(p.state, "", "  ")
	if err != nil {
		return fmt.Errorf("encode push policies: %w", err)
	}
	return atomicWrite(p.path, append(data, '\n'), 0o600)
}
