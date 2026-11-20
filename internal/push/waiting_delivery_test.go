package push

import (
	"context"
	"errors"
	"net/http"
	"path/filepath"
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
			if err := manager.Resolve(t.Context(), a); err != nil {
				t.Fatal(err)
			}
			id := deliveryID(a, endpointOne)
			first := manager.queue.state.Entries[id]
			log := &sendLog{}
			fail := true
			sender := func(_ context.Context, sub Subscription, payload []byte, _ sendOptions) error {
				if err := log.record(sub, payload); err != nil {
					return err
				}
				if fail && log.sent[len(log.sent)-1].Payload.Retract {
					return &pushError{statusCode: http.StatusServiceUnavailable}
				}
				return nil
			}
			manager.sendPush = sender
			runPass(t, manager, first.DueAt)
			pending := manager.queue.state.Entries[id]
			if pending.Attempts != 1 || pending.Event.ExpiresAt != first.Event.ExpiresAt {
				t.Fatalf("pending retract = %#v", pending)
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
				got := manager.queue.state.Entries[id]
				if !sameQueueEntry(pending, got) {
					t.Fatalf("reconciliation changed pending retract: %#v, want %#v", got, pending)
				}
				if _, err := manager.VerifyEventReference(ref, deliveryNow); !errors.Is(err, ErrStaleReference) {
					t.Fatalf("withdrawn reference = %v, want stale", err)
				}
			}
			b := a
			b.InteractionRevision++
			request := PublishRequest{Key: b, Preview: PreviewHidden, CreatedAt: first.DueAt}
			if name == "alert expires" {
				request.ExpiresAt = first.DueAt.Add(500 * time.Millisecond)
			}
			if result, err := manager.ApplyPaneTransition(t.Context(), b.PaneID, &request, func() bool { return true }); err != nil || result.Queued != 1 {
				t.Fatalf("replacement = %#v, %v", result, err)
			}
			runPass(t, manager, first.DueAt)
			if len(log.sent) != 1 || !log.sent[0].Payload.Retract {
				t.Fatalf("replacement sent before retract retry: %#v", log.sent)
			}
			fail = name == "retries exhausted"
			at := pending.DueAt
			for {
				runPass(t, manager, at)
				retract, queued := manager.queue.state.Entries[id]
				if !queued {
					break
				}
				if !retract.DueAt.After(at) || len(log.sent) != retract.Attempts {
					t.Fatalf("wrong retry order: %#v", log.sent)
				}
				at = retract.DueAt
			}
			runPass(t, manager, at)
			alerts := 0
			for i, sent := range log.sent {
				if !sent.Payload.Retract {
					alerts++
					if sent.Payload.Key != b || i != len(log.sent)-1 {
						t.Fatalf("replacement sent out of order: %#v", log.sent)
					}
				}
			}
			if name == "alert expires" && alerts != 0 || name != "alert expires" && alerts != 1 {
				t.Fatalf("unexpected replacement count: %#v", log.sent)
			}
		})
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
	if err := manager.Resolve(t.Context(), a); err != nil {
		t.Fatal(err)
	}
	at := manager.queue.state.Entries[deliveryID(a, endpointTwo)].DueAt
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
	if _, err := manager.queue.enqueue(PushEvent{
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
