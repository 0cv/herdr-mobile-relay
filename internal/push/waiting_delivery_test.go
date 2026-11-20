package push

import (
	"context"
	"encoding/json"
	"net/http"
	"path/filepath"
	"slices"
	"testing"
	"time"
)

func applyWaiting(t *testing.T, manager *Manager, key PushEventKey, now time.Time) PublishResult {
	t.Helper()
	result, err := manager.ApplyPaneTransition(t.Context(), key.PaneID, &PublishRequest{
		Key: key, Preview: PreviewHidden, CreatedAt: now,
	}, func() bool { return true })
	if err != nil {
		t.Fatal(err)
	}
	return result
}

func TestPaneTransitionSupersedesOlderRetry(t *testing.T) {
	for _, suppress := range []bool{false, true} {
		name := "replacement accepted"
		if suppress {
			name = "replacement disabled"
		}
		t.Run(name, func(t *testing.T) {
			manager, _ := newDeliveringManager(t, endpointOne)
			log := &sendLog{}
			a := exactKey("blocked")
			b := a
			b.InteractionRevision++
			manager.sendPush = func(_ context.Context, sub Subscription, payload []byte, _ sendOptions) error {
				if err := log.record(sub, payload); err != nil {
					return err
				}
				if log.sent[len(log.sent)-1].Payload.Key == a {
					return &pushError{statusCode: http.StatusServiceUnavailable}
				}
				return nil
			}
			applyWaiting(t, manager, a, deliveryNow)
			results := runPass(t, manager, deliveryNow)
			if len(results) != 1 || results[0].Disposition != DeliveryRetrying {
				t.Fatalf("first send = %#v", results)
			}
			if suppress {
				policy := manager.Policy(a.DeviceID, "en")
				policy.Categories[b.Category] = false
				if err := manager.SetPolicy(policy); err != nil {
					t.Fatal(err)
				}
			}
			at := deliveryNow.Add(time.Millisecond)
			replacement := applyWaiting(t, manager, b, at)
			if suppress && replacement.Suppressed != 1 || !suppress && replacement.Queued != 1 {
				t.Fatalf("replacement = %#v", replacement)
			}
			runPass(t, manager, at)
			runPass(t, manager, results[0].NextAttempt)
			if suppress {
				if len(log.sent) != 1 {
					t.Fatalf("unexpected POSTs: %#v", log.sent)
				}
			} else if len(log.sent) != 2 || log.sent[1].Payload.Key != b || log.sent[1].Payload.Retract {
				t.Fatalf("POSTs = %#v, want A attempt then B", log.sent)
			}
		})
	}
}

func TestPaneTransitionCommitFailureKeepsAcceptedAlert(t *testing.T) {
	manager, dir := newDeliveringManager(t, endpointOne)
	log := acceptingManager(manager)
	a := exactKey("blocked")
	applyWaiting(t, manager, a, deliveryNow)
	ref := queuedEventRef(t, dir, a.EventID)
	runPass(t, manager, deliveryNow)

	manager.queue.path = filepath.Join(dir, "subscriptions.json", "not-a-directory")
	b := a
	b.InteractionRevision++
	result, err := manager.ApplyPaneTransition(t.Context(), a.PaneID, &PublishRequest{
		Key: b, Preview: PreviewHidden, CreatedAt: deliveryNow,
	}, func() bool { return true })
	if err == nil || result.Queued != 0 {
		t.Fatalf("publication = %#v, %v", result, err)
	}
	runPass(t, manager, deliveryNow)
	requireReferenceAccepted(t, manager, ref)
	if len(log.sent) != 1 || len(manager.queue.state.Delivered) != 1 || len(manager.queue.state.Entries) != 0 {
		t.Fatalf("failed commit changed accepted delivery: %#v, %#v", log.sent, manager.queue.state)
	}
}

func TestWaitingAlertWaitsForRetract(t *testing.T) {
	for _, name := range []string{"retract succeeds", "retries exhausted", "alert expires", "restart"} {
		t.Run(name, func(t *testing.T) {
			manager, dir := newDeliveringManager(t, endpointOne)
			acceptingManager(manager)
			a := exactKey("blocked")
			applyWaiting(t, manager, a, deliveryNow)
			ref := queuedEventRef(t, dir, a.EventID)
			runPass(t, manager, deliveryNow)
			retirePane(t, manager, a.PaneID)

			type send struct {
				key     PushEventKey
				retract bool
			}
			var sent []send
			fail := true
			sender := func(_ context.Context, _ Subscription, payload []byte, _ sendOptions) error {
				var alert Payload
				if err := json.Unmarshal(payload, &alert); err != nil {
					return err
				}
				sent = append(sent, send{alert.Key, alert.Retract})
				if fail && alert.Retract {
					return &pushError{statusCode: http.StatusServiceUnavailable}
				}
				return nil
			}
			manager.sendPush = sender
			first := runPass(t, manager, deliveryNow)
			if len(first) != 1 || first[0].Disposition != DeliveryRetrying {
				t.Fatalf("first withdrawal = %#v, want retry", first)
			}
			if name == "restart" {
				var err error
				manager, err = NewManager(dir, testLogger())
				if err != nil {
					t.Fatal(err)
				}
				manager.sendPush = sender
				if err := manager.Reconcile(t.Context(), func(PushEventKey) bool { return true }); err != nil {
					t.Fatal(err)
				}
				requireReferenceStale(t, manager, ref)
			}
			b := a
			b.InteractionRevision++
			request := PublishRequest{Key: b, Preview: PreviewHidden, CreatedAt: deliveryNow}
			if name == "alert expires" {
				request.ExpiresAt = deliveryNow.Add(500 * time.Millisecond)
			}
			if result, err := manager.ApplyPaneTransition(t.Context(), b.PaneID, &request, func() bool { return true }); err != nil || result.Queued != 1 {
				t.Fatalf("replacement = %#v, %v", result, err)
			}
			runPass(t, manager, first[0].NextAttempt.Add(-time.Nanosecond))
			if !slices.Equal(sent, []send{{a, true}}) {
				t.Fatalf("sent before withdrawal retry: %#v", sent)
			}

			fail = name == "retries exhausted"
			at := first[0].NextAttempt
			for {
				results := runPass(t, manager, at)
				index := slices.IndexFunc(results, func(result DeliveryResult) bool { return result.Event.Retract })
				if index < 0 {
					t.Fatalf("withdrawal not attempted at %v: %#v", at, results)
				}
				retract := results[index]
				if name == "restart" && (retract.Attempts != 2 || !retract.Event.ExpiresAt.Equal(first[0].Event.ExpiresAt)) {
					t.Fatalf("restart changed withdrawal attempt or deadline: %#v", retract)
				}
				if retract.Disposition != DeliveryRetrying {
					break
				}
				at = retract.NextAttempt
			}
			runPass(t, manager, at)
			want := []send{{a, true}, {a, true}}
			if fail {
				for len(want) < maxDeliveryAttempts {
					want = append(want, send{a, true})
				}
			}
			if name != "alert expires" {
				want = append(want, send{b, false})
			}
			if !slices.Equal(sent, want) {
				t.Fatalf("POSTs = %#v, want %#v", sent, want)
			}
		})
	}
}

func TestWaitingSequenceSurvivesRetryRetractAndEmptyQueueRestart(t *testing.T) {
	manager, dir := newDeliveringManager(t, endpointOne)
	a, b := exactKey("ordered-a"), exactKey("ordered-b")
	a.Category = CategoryAttention
	b.PaneID = "pane-2"
	for _, key := range []PushEventKey{a, b} {
		applyWaiting(t, manager, key, deliveryNow)
	}
	log := &sendLog{}
	manager.sendPush = func(_ context.Context, sub Subscription, payload []byte, _ sendOptions) error {
		if err := log.record(sub, payload); err != nil {
			return err
		}
		if log.sent[len(log.sent)-1].Payload.Key == a && len(log.sent) <= 2 {
			return &pushError{statusCode: http.StatusServiceUnavailable}
		}
		return nil
	}
	runPass(t, manager, deliveryNow)
	now := deliveryNow.Add(time.Second)
	runPass(t, manager, now)
	for _, key := range []PushEventKey{a, b} {
		retirePane(t, manager, key.PaneID)
	}
	runPass(t, manager, now)

	type display struct {
		sequence uint64
		retract  bool
	}
	displays := map[PushEventKey][]display{}
	for _, sent := range log.sent {
		alert := sent.Payload
		displays[alert.Key] = append(displays[alert.Key], display{alert.WaitingSeq, alert.Retract})
	}
	if len(displays) != 2 || !slices.Equal(displays[a], []display{{1, false}, {1, false}, {1, true}}) ||
		!slices.Equal(displays[b], []display{{2, false}, {2, true}}) {
		t.Fatalf("expected ordered alerts, retry and withdrawals, got %#v", displays)
	}
	if queue := readQueueFile(t, dir); len(queue.Entries)+len(queue.Delivered) != 0 {
		t.Fatalf("expected empty queue, got %#v", queue)
	}

	var err error
	manager, err = NewManager(dir, testLogger())
	if err != nil {
		t.Fatal(err)
	}
	manager.now = func() time.Time { return now }
	log = acceptingManager(manager)
	c := exactKey("after-restart")
	applyWaiting(t, manager, c, now)
	update := PushEventKey{Category: CategoryUpdate, EventID: "update", DeviceID: c.DeviceID}
	if result, err := manager.Publish(t.Context(), PublishRequest{Key: update, Preview: PreviewHidden}); err != nil || result.Queued != 1 {
		t.Fatalf("Publish = %#v, %v", result, err)
	}
	entry := readQueueFile(t, dir).Entries[deliveryID(update, endpointOne)]
	var wire map[string]json.RawMessage
	if err := json.Unmarshal(entry.Event.Payload, &wire); err != nil || wire["waiting_seq"] != nil {
		t.Fatalf("update contains waiting sequence: %s, %v", entry.Event.Payload, err)
	}
	runPass(t, manager, now)
	if len(log.sent) != 2 {
		t.Fatalf("expected new alert and update, got %#v", log.sent)
	}
	for _, sent := range log.sent {
		if sent.Payload.Key == c && sent.Payload.WaitingSeq != 3 {
			t.Fatalf("sequence reset on restart: %#v", sent.Payload)
		}
	}
}

func TestWaitingRetractDoesNotHoldOtherDeliveries(t *testing.T) {
	manager, _ := newDeliveringManager(t, endpointOne, endpointTwo)
	if err := manager.Subscribe(typedTestSubscription("device-2", "https://fcm.googleapis.com/device-2")); err != nil {
		t.Fatal(err)
	}
	policy := manager.Policy("device-2", "en")
	policy.Settle = 0
	if err := manager.SetPolicy(policy); err != nil {
		t.Fatal(err)
	}
	policy = manager.Policy("device-1", "en")
	policy.Categories[CategoryFinished] = true
	if err := manager.SetPolicy(policy); err != nil {
		t.Fatal(err)
	}

	a := exactKey("old")
	acceptingManager(manager)
	applyWaiting(t, manager, a, deliveryNow)
	runPass(t, manager, deliveryNow)
	retirePane(t, manager, a.PaneID)
	at := deliveryNow
	log := &sendLog{}
	manager.sendPush = func(_ context.Context, sub Subscription, payload []byte, _ sendOptions) error {
		if err := log.record(sub, payload); err != nil {
			return err
		}
		if log.sent[len(log.sent)-1].Payload.Retract {
			return &pushError{statusCode: http.StatusServiceUnavailable}
		}
		return nil
	}
	runPass(t, manager, at)

	b := a
	b.EventID = "new"
	pane, device, session, finished, endpoint := b, b, b, b, b
	pane.PaneID = "other-pane"
	device.DeviceID = "device-2"
	session.ServerSessionID = "other-session"
	finished.Category = CategoryFinished
	endpoint.EventID = "alternate-endpoint"
	for _, key := range []PushEventKey{b, pane, device, session, finished} {
		if result, err := manager.Publish(t.Context(), PublishRequest{Key: key, Preview: PreviewHidden, CreatedAt: at}); err != nil || result.Queued != 1 {
			t.Fatalf("Publish(%#v) = %#v, %v", key, result, err)
		}
	}
	manager.mu.Lock()
	manager.active[endpoint] = true
	manager.mu.Unlock()
	payload, err := BuildPayload(PayloadRequest{Key: endpoint, Preview: PreviewHidden, ExpiresAt: at.Add(time.Minute)}, manager.signer)
	if err != nil {
		t.Fatal(err)
	}
	if err := enqueueFixture(manager.queue, PushEvent{
		Key: endpoint, Payload: payload, CreatedAt: at, ExpiresAt: at.Add(time.Minute),
	}, typedTestSubscription("device-1", "https://fcm.googleapis.com/alternate"), at); err != nil {
		t.Fatal(err)
	}
	runPass(t, manager, at)
	if len(log.sent) != 6 {
		t.Fatalf("POSTs = %#v, want retract plus five unrelated sends", log.sent)
	}
	for _, sent := range log.sent[1:] {
		if sent.Payload.Key == b {
			t.Fatal("same-topic alert was not held")
		}
	}
}
