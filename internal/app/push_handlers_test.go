package app

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/0cv/herdr-mobile-relay/internal/push"
)

func TestReservePushTestThrottlesPerDevice(t *testing.T) {
	server := testServer()
	now := time.Date(2026, time.September, 2, 12, 0, 0, 0, time.UTC)
	if !server.reservePushTest("device-1", now) {
		t.Fatal("first push test was throttled")
	}
	if server.reservePushTest("device-1", now.Add(pushTestInterval-time.Millisecond)) {
		t.Fatal("repeated push test inside interval was accepted")
	}
	if !server.reservePushTest("device-2", now.Add(time.Second)) {
		t.Fatal("one device throttled another device")
	}
	if !server.reservePushTest("device-1", now.Add(pushTestInterval)) {
		t.Fatal("push test remained throttled after interval")
	}
	// Nothing older than one interval can throttle anything, so stale rows are
	// dropped rather than accumulating one timestamp per device that ever asked
	// for a test.
	if !server.reservePushTest("device-3", now.Add(time.Hour)) {
		t.Fatal("later push test was throttled")
	}
	if len(server.pushTestLast) != 1 {
		t.Fatalf("push test throttle retained %d devices, want 1", len(server.pushTestLast))
	}
	// A revoked device leaves no throttle behind for the next enrolment.
	server.forgetPushTest("device-3")
	if !server.reservePushTest("device-3", now.Add(time.Hour)) {
		t.Fatal("re-enrolled device inherited the revoked device's throttle")
	}
}

func TestBoundPushPolicyUsesAuthenticatedDevice(t *testing.T) {
	current := push.DefaultDevicePolicy("old-device", "en")
	raw := json.RawMessage(`{
		"device_id":"spoofed-device",
		"locale":"spoofed-locale",
		"categories":{"attention":true,"question":false,"brief":true,"finished":false,"update":true,"test":true},
		"settle_ms":5000,
		"cooldown_ms":60000,
		"snoozed":true,
		"snooze_until":"2026-08-31T13:00:00Z",
		"update_once":false
	}`)
	policy, err := boundPushPolicy(raw, "authenticated-device", "zh-CN", current)
	if err != nil {
		t.Fatal(err)
	}
	if policy.DeviceID != "authenticated-device" || policy.Locale != "zh-CN" ||
		policy.Settle != 5*time.Second || policy.Cooldown != time.Minute ||
		!policy.Snoozed || policy.UpdateOnce {
		t.Fatalf("bound policy = %#v", policy)
	}
}
