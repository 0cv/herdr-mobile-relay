package app

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/0cv/herdr-mobile-relay/internal/protocol"
	"github.com/0cv/herdr-mobile-relay/internal/push"
	"github.com/0cv/herdr-mobile-relay/internal/transport"
)

const pushTestInterval = 10 * time.Second

type pushPolicyWire struct {
	Categories  map[push.Category]bool `json:"categories"`
	SettleMS    int64                  `json:"settle_ms"`
	CooldownMS  int64                  `json:"cooldown_ms"`
	SnoozeUntil string                 `json:"snooze_until,omitempty"`
	Snoozed     bool                   `json:"snoozed"`
	UpdateOnce  bool                   `json:"update_once"`
}

func (s *Server) handlePushPolicyGet(client *transport.ClientConn, identity transport.AuthenticatedIdentity) {
	policy := s.pushM.Policy(identity.DeviceID, identity.Locale)
	s.hub.Send(client, map[string]any{"type": "push_policy", "policy": pushPolicyResponse(policy)})
}

func (s *Server) handlePushPolicySet(client *transport.ClientConn, inbound protocol.Inbound, identity transport.AuthenticatedIdentity) {
	reply := s.reply(client, inbound.RequestID, "push_policy_set", "")
	current := s.pushM.Policy(identity.DeviceID, identity.Locale)
	policy, policyErr := boundPushPolicy(inbound.Policy, identity.DeviceID, identity.Locale, current)
	if policyErr != nil || s.pushM.SetPolicy(policy) != nil {
		reply.fail("Notification policy was rejected")
		s.hub.Send(client, map[string]any{"type": "push_policy_result", "ok": false, "code": "push_invalid_policy"})
		return
	}
	data := map[string]any{"policy": pushPolicyResponse(policy)}
	reply.complete(data)
	s.hub.Send(client, map[string]any{
		"type": "push_policy_result", "ok": true, "policy": pushPolicyResponse(policy),
	})
}

func (s *Server) handlePushSnooze(client *transport.ClientConn, inbound protocol.Inbound, identity transport.AuthenticatedIdentity) {
	policy := s.pushM.Policy(identity.DeviceID, identity.Locale)
	policy.Snoozed = inbound.Snoozed
	policy.SnoozeUntil = time.Time{}
	if inbound.SnoozeUntil != "" {
		until, parseErr := time.Parse(time.RFC3339, inbound.SnoozeUntil)
		if parseErr != nil {
			s.hub.Send(client, map[string]any{"type": "push_policy_result", "ok": false, "code": "push_invalid_snooze"})
			return
		}
		policy.SnoozeUntil = until
	}
	if policyErr := s.pushM.SetPolicy(policy); policyErr != nil {
		s.hub.Send(client, map[string]any{"type": "push_policy_result", "ok": false, "code": "push_invalid_snooze"})
		return
	}
	s.hub.Send(client, map[string]any{
		"type": "push_policy_result", "ok": true, "policy": pushPolicyResponse(policy),
	})
}

func (s *Server) handlePushViewedPane(client *transport.ClientConn, inbound protocol.Inbound, identity transport.AuthenticatedIdentity) {
	var target *protocol.TargetRef
	if inbound.Visible && inbound.Unlocked && inbound.Target != nil && s.pushTargetCurrent(*inbound.Target) {
		copyTarget := *inbound.Target
		target = &copyTarget
	}
	s.pushM.SetViewedPane(identity.DeviceID, target)
	s.hub.Send(client, map[string]any{"type": "push_viewed_pane_result", "ok": true})
}

func (s *Server) handlePushOpenRef(client *transport.ClientConn, inbound protocol.Inbound, action string, identity transport.AuthenticatedIdentity) {
	claims, verifyErr := s.pushM.VerifyEventReference(inbound.EventRef, time.Now().UTC())
	if verifyErr != nil || claims.Key.DeviceID != identity.DeviceID || !s.pushTargetCurrent(claims.Key.Target()) {
		s.reply(client, inbound.RequestID, action, "").fail("Notification target is no longer available")
		return
	}
	s.reply(client, inbound.RequestID, action, claims.Key.PaneID).complete(map[string]any{
		"target": claims.Key.Target(), "event_id": claims.Key.EventID, "category": claims.Key.Category,
	})
}

func (s *Server) handlePushTestDevice(ctx context.Context, client *transport.ClientConn, identity transport.AuthenticatedIdentity) {
	now := time.Now().UTC()
	if !s.reservePushTest(identity.DeviceID, now) {
		s.hub.Send(client, map[string]any{"type": "push_test_result", "stage": "rate_limited"})
		return
	}
	eventID := "test"
	key := push.PushEventKey{DeviceID: identity.DeviceID, EventID: eventID, Category: push.CategoryTest}
	published, publishErr := s.pushM.Publish(ctx, push.PublishRequest{
		Key: key, Preview: push.PreviewHidden, CreatedAt: now, ExpiresAt: now.Add(time.Minute),
	})
	stage := "dropped"
	if publishErr == nil && published.Queued > 0 {
		// The manager's worker performs the network send. Keeping it out
		// of this command path avoids holding the global queue processor
		// while one client waits for a push service.
		stage = "queued"
	}
	s.hub.Send(client, map[string]any{"type": "push_test_result", "stage": stage})
}

func (s *Server) handlePushSubscribe(client *transport.ClientConn, inbound protocol.Inbound, identity transport.AuthenticatedIdentity) {
	ok := false
	var sub push.Subscription
	if json.Unmarshal(inbound.Subscription, &sub) == nil {
		sub.ClientID = inbound.ClientID
		sub.DeviceID = identity.DeviceID
		sub.Locale = identity.Locale
		// The authenticated connection does not currently carry a
		// trusted browser platform. Defaulting to no actions is safe;
		// never infer an actionable platform from client JSON.
		sub.Platform = push.PlatformOther
		sub.UserAgent = ""
		ok = s.pushM.Subscribe(sub, inbound.ReplaceEndpoints) == nil
	}
	s.hub.Send(client, map[string]any{"type": "push_subscribed", "ok": ok})
}

func (s *Server) handlePushUnsubscribe(client *transport.ClientConn, inbound protocol.Inbound, identity transport.AuthenticatedIdentity) {
	ok := s.pushM.UnsubscribeDevice(identity.DeviceID, inbound.Endpoints, inbound.ClientID) == nil
	s.hub.Send(client, map[string]any{"type": "push_unsubscribed", "ok": ok})
}

func boundPushPolicy(raw json.RawMessage, deviceID, locale string, current push.DevicePolicy) (push.DevicePolicy, error) {
	var wire pushPolicyWire
	if len(raw) == 0 || json.Unmarshal(raw, &wire) != nil {
		return push.DevicePolicy{}, errors.New("push_invalid_policy")
	}
	if wire.SettleMS < 0 || wire.CooldownMS < 0 {
		return push.DevicePolicy{}, errors.New("push_invalid_duration")
	}
	current.DeviceID = deviceID
	current.Locale = locale
	current.Categories = wire.Categories
	current.Settle = time.Duration(wire.SettleMS) * time.Millisecond
	current.Cooldown = time.Duration(wire.CooldownMS) * time.Millisecond
	current.Snoozed = wire.Snoozed
	current.UpdateOnce = wire.UpdateOnce
	current.SnoozeUntil = time.Time{}
	if wire.SnoozeUntil != "" {
		until, err := time.Parse(time.RFC3339, wire.SnoozeUntil)
		if err != nil {
			return push.DevicePolicy{}, errors.New("push_invalid_snooze")
		}
		current.SnoozeUntil = until
	}
	return current, nil
}

func pushPolicyResponse(policy push.DevicePolicy) map[string]any {
	categories := make(map[push.Category]bool, len(policy.Categories))
	for category, enabled := range policy.Categories {
		categories[category] = enabled
	}
	result := map[string]any{
		"device_id":   policy.DeviceID,
		"locale":      policy.Locale,
		"categories":  categories,
		"settle_ms":   policy.Settle.Milliseconds(),
		"cooldown_ms": policy.Cooldown.Milliseconds(),
		"snoozed":     policy.Snoozed,
		"update_once": policy.UpdateOnce,
	}
	if !policy.SnoozeUntil.IsZero() {
		result["snooze_until"] = policy.SnoozeUntil.UTC().Format(time.RFC3339)
	}
	return result
}

func (s *Server) pushTargetCurrent(target protocol.TargetRef) bool {
	agent, ok := s.state.Agent(target.PaneID)
	return ok && targetMatchesAgent(target, agent)
}

// reservePushTest rate limits delivery tests per device. Entries older than one
// interval carry no decision, so they are dropped on the way past: a relay that
// enrols and revokes devices for months must not accumulate one timestamp per
// device that ever asked for a test.
func (s *Server) reservePushTest(deviceID string, now time.Time) bool {
	s.pushTestMu.Lock()
	defer s.pushTestMu.Unlock()
	for device, last := range s.pushTestLast {
		if now.Sub(last) >= pushTestInterval {
			delete(s.pushTestLast, device)
		}
	}
	if last, exists := s.pushTestLast[deviceID]; exists && now.Sub(last) < pushTestInterval {
		return false
	}
	s.pushTestLast[deviceID] = now
	return true
}

func (s *Server) forgetPushTest(deviceID string) {
	s.pushTestMu.Lock()
	defer s.pushTestMu.Unlock()
	delete(s.pushTestLast, deviceID)
}
