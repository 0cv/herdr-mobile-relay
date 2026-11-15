package app

import (
	"encoding/json"
	"io"
	"log/slog"
	"strconv"
	"testing"
	"time"

	"github.com/0cv/herdr-mobile-relay/internal/protocol"
	"github.com/0cv/herdr-mobile-relay/internal/push"
	"github.com/0cv/herdr-mobile-relay/internal/transport"
	"github.com/coder/websocket"
)

var pushIdentity = transport.AuthenticatedIdentity{
	DeviceID: "device-1", CredentialID: "credential-1", Role: "reader", Locale: "zh-CN", CredentialVersion: 1,
}

func currentPaneTarget(t *testing.T, s *Server) protocol.TargetRef {
	t.Helper()
	agent, ok := s.state.Agent("pane-1")
	if !ok {
		t.Fatal("pane-1 is not in the inventory")
	}
	return protocol.TargetRef{
		ServerSessionID: protocol.PrimaryServerSessionID, PaneID: agent.PaneID, TerminalID: agent.TerminalID,
		Generation: agent.Generation, AgentSessionID: agent.SessionID,
	}
}

func newPushManager(t *testing.T, dir string) *push.Manager {
	t.Helper()
	manager, err := push.NewManager(dir, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	return manager
}

func subscribeDevice(t *testing.T, manager *push.Manager, deviceID, endpoint string) {
	t.Helper()
	subscription := push.Subscription{Endpoint: endpoint, DeviceID: deviceID, Locale: "en"}
	subscription.Keys.P256dh = "p256dh"
	subscription.Keys.Auth = "auth"
	if err := manager.Subscribe(subscription); err != nil {
		t.Fatal(err)
	}
}

func pushActionServer(t *testing.T) (*Server, *websocket.Conn, *transport.ClientConn, string) {
	t.Helper()
	s := handlerTestServer(t)
	dir := t.TempDir()
	s.pushM = newPushManager(t, dir)
	useProductionHandler(t, s)
	conn, client := connectedClient(t, s)
	return s, conn, client, dir
}

func publishAttention(t *testing.T, s *Server, deviceID, eventID string, target protocol.TargetRef) push.PublishResult {
	t.Helper()
	now := time.Now().UTC()
	result, err := s.pushM.Publish(t.Context(), push.PublishRequest{
		Key: push.PushEventKey{
			DeviceID: deviceID, ServerSessionID: target.ServerSessionID, PaneID: target.PaneID,
			TerminalID: target.TerminalID, AgentSessionID: target.AgentSessionID, Generation: target.Generation,
			EventID: eventID, Category: push.CategoryAttention,
		},
		Preview: push.PreviewHidden, CreatedAt: now, ExpiresAt: now.Add(time.Minute),
	})
	if err != nil {
		t.Fatal(err)
	}
	return result
}

func queuedEventRef(t *testing.T, dir, deviceID, eventID string) string {
	t.Helper()
	for _, payload := range queuedPushPayloads(t, dir) {
		if payload.Key.DeviceID == deviceID && payload.Key.EventID == eventID {
			return payload.EventRef
		}
	}
	t.Fatalf("no queued push for device %s event %s", deviceID, eventID)
	return ""
}

func requirePushResult(t *testing.T, response map[string]any, messageType string, ok bool) {
	t.Helper()
	if response["type"] != messageType || response["ok"] != ok {
		t.Fatalf("response = %#v, want %s with ok=%v", response, messageType, ok)
	}
}

func TestPushPolicyGetReturnsDefaults(t *testing.T) {
	s, conn, client, _ := pushActionServer(t)

	s.handlePushPolicyGet(client, pushIdentity)

	response := readFrame(t, conn)
	policy, _ := response["policy"].(map[string]any)
	categories, _ := policy["categories"].(map[string]any)
	if response["type"] != "push_policy" || policy["device_id"] != "device-1" || policy["locale"] != "zh-CN" ||
		policy["settle_ms"] != float64(2000) || policy["cooldown_ms"] != float64(30000) ||
		policy["snoozed"] != false || policy["update_once"] != true ||
		categories["attention"] != true || categories["finished"] != false {
		t.Fatalf("response = %#v, want the default policy", response)
	}
}

func TestPushPolicySet(t *testing.T) {
	s, conn, client, _ := pushActionServer(t)
	inbound := func(policy string) protocol.Inbound {
		return protocol.Inbound{RequestID: "req-1", Policy: json.RawMessage(policy)}
	}

	t.Run("a valid policy is stored and reported twice", func(t *testing.T) {
		s.handlePushPolicySet(client, inbound(`{"categories":{"attention":false,"finished":true},"settle_ms":500,"cooldown_ms":1000,"update_once":false}`), pushIdentity)

		requireCompleted(t, readFrame(t, conn), "push_policy_set")
		result := readFrame(t, conn)
		requirePushResult(t, result, "push_policy_result", true)
		policy, _ := result["policy"].(map[string]any)
		if policy["settle_ms"] != float64(500) || policy["cooldown_ms"] != float64(1000) {
			t.Fatalf("policy = %#v", policy)
		}
		stored := s.pushM.Policy("device-1", "zh-CN")
		if stored.Categories[push.CategoryAttention] || !stored.Categories[push.CategoryFinished] ||
			stored.Settle != 500*time.Millisecond || stored.Cooldown != time.Second || stored.UpdateOnce {
			t.Fatalf("stored policy = %#v", stored)
		}
	})

	for name, policy := range map[string]string{
		"a missing policy":      ``,
		"a malformed policy":    `[]`,
		"a negative duration":   `{"categories":{},"settle_ms":-1}`,
		"an unknown category":   `{"categories":{"bogus":true}}`,
		"an invalid snooze end": `{"categories":{},"snooze_until":"tomorrow"}`,
	} {
		t.Run(name+" is rejected", func(t *testing.T) {
			before := s.pushM.Policy("device-1", "zh-CN")

			s.handlePushPolicySet(client, inbound(policy), pushIdentity)

			requireFailedResult(t, readFrame(t, conn), "push_policy_set", "Notification policy was rejected")
			result := readFrame(t, conn)
			requirePushResult(t, result, "push_policy_result", false)
			if result["code"] != "push_invalid_policy" {
				t.Fatalf("result = %#v", result)
			}
			after := s.pushM.Policy("device-1", "zh-CN")
			if after.Settle != before.Settle || after.Categories[push.CategoryFinished] != before.Categories[push.CategoryFinished] {
				t.Fatalf("policy changed from %#v to %#v", before, after)
			}
		})
	}
}

func TestPushSnooze(t *testing.T) {
	s, conn, client, _ := pushActionServer(t)

	t.Run("a valid timestamp snoozes until then", func(t *testing.T) {
		until := time.Now().Add(time.Hour).UTC().Truncate(time.Second)
		s.handlePushSnooze(client, protocol.Inbound{Snoozed: true, SnoozeUntil: until.Format(time.RFC3339)}, pushIdentity)

		result := readFrame(t, conn)
		requirePushResult(t, result, "push_policy_result", true)
		policy, _ := result["policy"].(map[string]any)
		if policy["snoozed"] != true || policy["snooze_until"] != until.Format(time.RFC3339) {
			t.Fatalf("policy = %#v", policy)
		}
		if stored := s.pushM.Policy("device-1", "zh-CN"); !stored.Snoozed || !stored.SnoozeUntil.Equal(until) {
			t.Fatalf("stored policy = %#v", stored)
		}
	})

	t.Run("an invalid timestamp is refused and changes nothing", func(t *testing.T) {
		s.handlePushSnooze(client, protocol.Inbound{Snoozed: false, SnoozeUntil: "tomorrow"}, pushIdentity)

		result := readFrame(t, conn)
		requirePushResult(t, result, "push_policy_result", false)
		if result["code"] != "push_invalid_snooze" {
			t.Fatalf("result = %#v", result)
		}
		if stored := s.pushM.Policy("device-1", "zh-CN"); !stored.Snoozed {
			t.Fatalf("stored policy = %#v, want the earlier snooze kept", stored)
		}
	})
}

func TestPushViewedPane(t *testing.T) {
	s, conn, client, _ := pushActionServer(t)
	subscribeDevice(t, s.pushM, "device-1", "https://fcm.googleapis.com/fcm/send/one")
	target := currentPaneTarget(t, s)
	published := 0
	paneIsViewed := func() bool {
		published++
		result := publishAttention(t, s, "device-1", "event-"+strconv.Itoa(published), target)
		return result.Suppressed == 1
	}
	report := func(inbound protocol.Inbound) {
		t.Helper()
		s.handlePushViewedPane(client, inbound, pushIdentity)
		requirePushResult(t, readFrame(t, conn), "push_viewed_pane_result", true)
	}
	viewing := protocol.Inbound{Visible: true, Unlocked: true, Target: &target}
	stale := target
	stale.Generation++

	if paneIsViewed() {
		t.Fatal("pane is viewed before any report")
	}
	report(viewing)
	if !paneIsViewed() {
		t.Fatal("visible, unlocked, current target was not recorded")
	}

	for name, inbound := range map[string]protocol.Inbound{
		"a hidden page":          {Visible: false, Unlocked: true, Target: &target},
		"a locked phone":         {Visible: true, Unlocked: false, Target: &target},
		"a replaced pane":        {Visible: true, Unlocked: true, Target: &stale},
		"no target":              {Visible: true, Unlocked: true},
		"an unknown pane target": {Visible: true, Unlocked: true, Target: &protocol.TargetRef{PaneID: "gone"}},
	} {
		t.Run(name+" clears the record", func(t *testing.T) {
			report(viewing)
			if !paneIsViewed() {
				t.Fatal("target was not recorded")
			}
			report(inbound)
			if paneIsViewed() {
				t.Fatal("record was kept")
			}
		})
	}
}

func TestPushOpenRef(t *testing.T) {
	s, conn, client, dir := pushActionServer(t)
	subscribeDevice(t, s.pushM, "device-1", "https://fcm.googleapis.com/fcm/send/one")
	subscribeDevice(t, s.pushM, "device-2", "https://fcm.googleapis.com/fcm/send/two")
	target := currentPaneTarget(t, s)
	publishAttention(t, s, "device-1", "event-1", target)
	publishAttention(t, s, "device-2", "event-2", target)
	open := func(ref string) map[string]any {
		t.Helper()
		s.handlePushOpenRef(client, protocol.Inbound{RequestID: "req-1", EventRef: ref}, "push_open_ref", pushIdentity)
		return readFrame(t, conn)
	}
	const refused = "Notification target is no longer available"

	t.Run("a reference for the caller's current pane opens", func(t *testing.T) {
		response := open(queuedEventRef(t, dir, "device-1", "event-1"))
		requireCompleted(t, response, "push_open_ref")
		data := commandData(t, response)
		target, _ := data["target"].(map[string]any)
		if data["event_id"] != "event-1" || data["category"] != "attention" || target["pane_id"] != "pane-1" {
			t.Fatalf("data = %#v", data)
		}
	})

	t.Run("a reference for another device is refused", func(t *testing.T) {
		requireFailedResult(t, open(queuedEventRef(t, dir, "device-2", "event-2")), "push_open_ref", refused)
	})

	t.Run("a reference that cannot be verified is refused", func(t *testing.T) {
		requireFailedResult(t, open("not-a-reference"), "push_open_ref", refused)
	})

	t.Run("a reference for a replaced pane is refused", func(t *testing.T) {
		s.state.BumpGeneration("pane-1")
		requireFailedResult(t, open(queuedEventRef(t, dir, "device-1", "event-1")), "push_open_ref", refused)
	})
}

func TestPushTestDevice(t *testing.T) {
	s, conn, client, _ := pushActionServer(t)
	subscribeDevice(t, s.pushM, "device-1", "https://fcm.googleapis.com/fcm/send/one")
	stage := func(identity transport.AuthenticatedIdentity) any {
		t.Helper()
		s.handlePushTestDevice(t.Context(), client, identity)
		response := readFrame(t, conn)
		if response["type"] != "push_test_result" {
			t.Fatalf("response = %#v", response)
		}
		return response["stage"]
	}

	if got := stage(pushIdentity); got != "queued" {
		t.Fatalf("first test stage = %v, want queued", got)
	}
	if got := stage(pushIdentity); got != "rate_limited" {
		t.Fatalf("second test stage = %v, want rate_limited", got)
	}

	unsubscribed := pushIdentity
	unsubscribed.DeviceID = "device-without-subscription"
	if got := stage(unsubscribed); got != "dropped" {
		t.Fatalf("test without a subscription stage = %v, want dropped", got)
	}
}

func TestPushSubscribe(t *testing.T) {
	s, conn, client, _ := pushActionServer(t)
	subscribe := func(subscription string) map[string]any {
		t.Helper()
		s.handlePushSubscribe(client, protocol.Inbound{Subscription: json.RawMessage(subscription), ClientID: "client-1"}, pushIdentity)
		return readFrame(t, conn)
	}

	t.Run("the stored subscription takes its identity from the connection", func(t *testing.T) {
		requirePushResult(t, subscribe(`{
			"endpoint": "https://fcm.googleapis.com/fcm/send/one", "keys": {"p256dh": "p", "auth": "a"},
			"device_id": "someone-else", "locale": "de", "platform": "ios", "user_agent": "Mozilla"
		}`), "push_subscribed", true)

		subscriptions := s.pushM.Subscriptions()
		if len(subscriptions) != 1 {
			t.Fatalf("subscriptions = %#v", subscriptions)
		}
		stored := subscriptions[0]
		if stored.DeviceID != "device-1" || stored.Locale != "zh-CN" || stored.Platform != push.PlatformOther ||
			stored.UserAgent != "" || stored.ClientID != "client-1" {
			t.Fatalf("stored = %#v", stored)
		}
	})

	for name, subscription := range map[string]string{
		"malformed JSON":       `"endpoint"`,
		"missing keys":         `{"endpoint": "https://fcm.googleapis.com/fcm/send/two"}`,
		"a disallowed service": `{"endpoint": "https://example.com/push", "keys": {"p256dh": "p", "auth": "a"}}`,
	} {
		t.Run(name+" is refused", func(t *testing.T) {
			requirePushResult(t, subscribe(subscription), "push_subscribed", false)
			if got := len(s.pushM.Subscriptions()); got != 1 {
				t.Fatalf("subscriptions = %d, want the earlier one only", got)
			}
		})
	}
}

func TestPushUnsubscribe(t *testing.T) {
	s, conn, client, _ := pushActionServer(t)
	subscribeDevice(t, s.pushM, "device-1", "https://fcm.googleapis.com/fcm/send/one")
	subscribeDevice(t, s.pushM, "device-1", "https://fcm.googleapis.com/fcm/send/two")
	subscribeDevice(t, s.pushM, "device-2", "https://fcm.googleapis.com/fcm/send/three")

	s.handlePushUnsubscribe(client, protocol.Inbound{Endpoints: []string{
		"https://fcm.googleapis.com/fcm/send/one", "https://fcm.googleapis.com/fcm/send/three",
	}}, pushIdentity)

	requirePushResult(t, readFrame(t, conn), "push_unsubscribed", true)
	remaining := map[string]string{}
	for _, subscription := range s.pushM.Subscriptions() {
		remaining[subscription.Endpoint] = subscription.DeviceID
	}
	if len(remaining) != 2 || remaining["https://fcm.googleapis.com/fcm/send/two"] != "device-1" ||
		remaining["https://fcm.googleapis.com/fcm/send/three"] != "device-2" {
		t.Fatalf("remaining subscriptions = %#v, want only the caller's named endpoint removed", remaining)
	}
}
