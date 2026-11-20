package push

import (
	"context"
	"encoding/json"
	"errors"
	"maps"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"sort"
	"sync"
	"testing"
	"time"
)

const (
	endpointOne = "https://fcm.googleapis.com/one"
	endpointTwo = "https://fcm.googleapis.com/two"
	stressRound = 25
)

var deliveryNow = time.Date(2026, time.September, 2, 13, 0, 0, 0, time.UTC)

type queueFileView struct {
	Entries   map[string]queueEntryView `json:"entries"`
	Delivered map[string]deliveredView  `json:"delivered"`
}

type queueEntryView struct {
	Event struct {
		Key     PushEventKey `json:"key"`
		Payload []byte       `json:"payload"`
	} `json:"event"`
	Subscription struct {
		Endpoint string `json:"endpoint"`
	} `json:"subscription"`
	Attempts int `json:"attempts"`
}

type deliveredView struct {
	Key          PushEventKey `json:"key"`
	Subscription struct {
		Endpoint string `json:"endpoint"`
	} `json:"subscription"`
}

type sentPush struct {
	Endpoint string
	Auth     string
	Payload  Payload
}

type sendLog struct {
	sent []sentPush
}

func (l *sendLog) record(subscription Subscription, payload []byte) error {
	var typed Payload
	if err := json.Unmarshal(payload, &typed); err != nil {
		return err
	}
	l.sent = append(l.sent, sentPush{Endpoint: subscription.Endpoint, Auth: subscription.Keys.Auth, Payload: typed})
	return nil
}

func (l *sendLog) endpoints() []string {
	endpoints := make([]string, 0, len(l.sent))
	for _, sent := range l.sent {
		endpoints = append(endpoints, sent.Endpoint)
	}
	return endpoints
}

func acceptingManager(manager *Manager) *sendLog {
	log := &sendLog{}
	manager.sendPush = func(_ context.Context, subscription Subscription, payload []byte, _ sendOptions) error {
		return log.record(subscription, payload)
	}
	return log
}

func newDeliveringManager(t *testing.T, endpoints ...string) (*Manager, string) {
	t.Helper()
	dir := t.TempDir()
	manager, err := NewManager(dir, testLogger())
	if err != nil {
		t.Fatal(err)
	}
	for _, endpoint := range endpoints {
		if err := manager.Subscribe(typedTestSubscription("device-1", endpoint)); err != nil {
			t.Fatal(err)
		}
	}
	policy := DefaultDevicePolicy("device-1", "en")
	policy.Settle = 0
	policy.Cooldown = 0
	if err := manager.SetPolicy(policy); err != nil {
		t.Fatal(err)
	}
	return manager, dir
}

func readQueueFile(t *testing.T, dir string) queueFileView {
	t.Helper()
	var view queueFileView
	data, err := os.ReadFile(filepath.Join(dir, "queue.json"))
	if os.IsNotExist(err) {
		return view
	}
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(data, &view); err != nil {
		t.Fatal(err)
	}
	return view
}

func queuedEndpoints(t *testing.T, dir string) []string {
	t.Helper()
	return queuedEndpointsOf(t, dir, "")
}

func queuedEndpointsOf(t *testing.T, dir, deviceID string) []string {
	t.Helper()
	endpoints := []string{}
	for _, entry := range readQueueFile(t, dir).Entries {
		if deviceID == "" || entry.Event.Key.DeviceID == deviceID {
			endpoints = append(endpoints, entry.Subscription.Endpoint)
		}
	}
	sort.Strings(endpoints)
	return endpoints
}

var otherDevices = []string{"device-2", "device-3", "device-4", "device-5"}

// seedDeviceDirs prepares a push directory with device-1 on the given endpoints
// and the other devices subscribed after it. Stress tests copy it for every
// round, because creating a fresh manager through Subscribe is slow.
func seedDeviceDirs(t *testing.T, endpoints ...string) string {
	t.Helper()
	manager, dir := newDeliveringManager(t, endpoints...)
	for _, device := range otherDevices {
		if err := manager.Subscribe(typedTestSubscription(device, "https://fcm.googleapis.com/"+device)); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func managerFromSeed(t *testing.T, seed string) (*Manager, string) {
	t.Helper()
	dir := t.TempDir()
	files, err := os.ReadDir(seed)
	if err != nil {
		t.Fatal(err)
	}
	for _, file := range files {
		info, err := file.Info()
		if err != nil || !info.Mode().IsRegular() {
			continue
		}
		data, err := os.ReadFile(filepath.Join(seed, file.Name()))
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, file.Name()), data, info.Mode().Perm()); err != nil {
			t.Fatal(err)
		}
	}
	manager, err := NewManager(dir, testLogger())
	if err != nil {
		t.Fatal(err)
	}
	return manager, dir
}

func queuedEventRef(t *testing.T, dir, eventID string) string {
	t.Helper()
	for _, entry := range readQueueFile(t, dir).Entries {
		if entry.Event.Key.EventID != eventID {
			continue
		}
		var payload Payload
		if err := json.Unmarshal(entry.Event.Payload, &payload); err != nil {
			t.Fatal(err)
		}
		return payload.EventRef
	}
	t.Fatalf("no queued entry for event %q", eventID)
	return ""
}

func exactKey(eventID string) PushEventKey {
	key := typedTestKey("device-1")
	key.EventID = eventID
	return key
}

func publishQuestionFor(t *testing.T, manager *Manager, eventID string, now time.Time, ttl time.Duration) {
	t.Helper()
	key := typedTestKey("")
	key.EventID = eventID
	result, err := manager.Publish(t.Context(), PublishRequest{
		Key: key, Preview: PreviewQuestion, CreatedAt: now, ExpiresAt: now.Add(ttl),
	})
	if err != nil || result.Queued != 1 {
		t.Fatalf("Publish(%s) = %#v, %v", eventID, result, err)
	}
}

func publishQuestion(t *testing.T, manager *Manager, eventID string, now time.Time) {
	t.Helper()
	publishQuestionFor(t, manager, eventID, now, time.Hour)
}

func runAt(ctx context.Context, manager *Manager, now time.Time) ([]DeliveryResult, error) {
	return manager.processDue(ctx, func() time.Time { return now })
}

func runPass(t *testing.T, manager *Manager, now time.Time) []DeliveryResult {
	t.Helper()
	results, err := runAt(t.Context(), manager, now)
	if err != nil {
		t.Fatal(err)
	}
	return results
}

func requireReferenceAccepted(t *testing.T, manager *Manager, token string) {
	t.Helper()
	if _, err := manager.VerifyEventReference(token, deliveryNow); err != nil {
		t.Fatalf("VerifyEventReference() = %v, want accepted", err)
	}
}

func requireReferenceStale(t *testing.T, manager *Manager, token string) {
	t.Helper()
	if _, err := manager.VerifyEventReference(token, deliveryNow); !errors.Is(err, ErrStaleReference) {
		t.Fatalf("VerifyEventReference() = %v, want %v", err, ErrStaleReference)
	}
}

func requireEndpoints(t *testing.T, label string, got []string, want ...string) {
	t.Helper()
	if want == nil {
		want = []string{}
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("%s = %#v, want %#v", label, got, want)
	}
}

func TestQueueFileShape(t *testing.T) {
	manager, dir := newDeliveringManager(t, endpointOne)
	acceptingManager(manager)
	publishQuestion(t, manager, "event-1", deliveryNow)

	readTop := func() map[string]json.RawMessage {
		t.Helper()
		data, err := os.ReadFile(filepath.Join(dir, "queue.json"))
		if err != nil {
			t.Fatal(err)
		}
		var top map[string]json.RawMessage
		if err := json.Unmarshal(data, &top); err != nil {
			t.Fatal(err)
		}
		if got := slices.Sorted(maps.Keys(top)); !reflect.DeepEqual(got, []string{"delivered", "entries"}) {
			t.Fatalf("top-level keys = %v", got)
		}
		return top
	}
	requireFields := func(label string, raw json.RawMessage, want ...string) {
		t.Helper()
		var records map[string]map[string]json.RawMessage
		if err := json.Unmarshal(raw, &records); err != nil {
			t.Fatal(err)
		}
		if len(records) != 1 {
			t.Fatalf("%s records = %d, want 1", label, len(records))
		}
		for _, record := range records {
			if got := slices.Sorted(maps.Keys(record)); !reflect.DeepEqual(got, want) {
				t.Fatalf("%s fields = %v, want %v", label, got, want)
			}
		}
	}

	requireFields("entry", readTop()["entries"], "attempts", "due_at", "event", "id", "subscription")
	if results := runPass(t, manager, deliveryNow); len(results) != 1 || results[0].Disposition != DeliveryAccepted {
		t.Fatalf("RunOnce() = %#v", results)
	}
	requireFields("delivered", readTop()["delivered"], "accepted_at", "key", "subscription", "tag")
}

func TestPublishQueuesOncePerLogicalDevice(t *testing.T) {
	manager, dir := newDeliveringManager(t, endpointOne, endpointTwo)
	result, err := manager.Publish(t.Context(), PublishRequest{
		Key: typedTestKey(""), Preview: PreviewQuestion, CreatedAt: deliveryNow, ExpiresAt: deliveryNow.Add(time.Minute),
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.Queued != 1 {
		t.Fatalf("publish result = %#v", result)
	}
	requireEndpoints(t, "queued endpoints", queuedEndpoints(t, dir), endpointTwo)
}

func TestSubscriptionReplacementMigratesQueuedDelivery(t *testing.T) {
	manager, dir := newDeliveringManager(t, endpointOne)
	log := acceptingManager(manager)
	publishQuestion(t, manager, "event-1", deliveryNow)
	if err := manager.Subscribe(typedTestSubscription("device-1", endpointTwo), []string{endpointOne}); err != nil {
		t.Fatal(err)
	}
	requireEndpoints(t, "queued endpoints", queuedEndpoints(t, dir), endpointTwo)
	runPass(t, manager, deliveryNow)
	requireEndpoints(t, "delivery endpoints", log.endpoints(), endpointTwo)
}

func TestTerminalSubscriptionFallsBackToRetainedEndpoint(t *testing.T) {
	manager, dir := newDeliveringManager(t, endpointOne, endpointTwo)
	publishQuestion(t, manager, "event-1", deliveryNow)
	var endpoints []string
	manager.sendPush = func(_ context.Context, subscription Subscription, _ []byte, _ sendOptions) error {
		endpoints = append(endpoints, subscription.Endpoint)
		if subscription.Endpoint == endpointTwo {
			return &pushError{statusCode: http.StatusGone}
		}
		return nil
	}
	results := runPass(t, manager, deliveryNow)
	if len(results) != 1 || results[0].Disposition != DeliveryPruned {
		t.Fatalf("first delivery results = %#v", results)
	}
	requireEndpoints(t, "fallback queued endpoints", queuedEndpoints(t, dir), endpointOne)
	if subscriptions := manager.Subscriptions(); len(subscriptions) != 1 || subscriptions[0].Endpoint != endpointOne {
		t.Fatalf("retained subscriptions after fallback = %#v", subscriptions)
	}
	runPass(t, manager, deliveryNow)
	requireEndpoints(t, "delivery endpoints", endpoints, endpointTwo, endpointOne)
}

func TestTerminalFallbackRestoresPrunedEventWhenSubscriptionsCannotPersist(t *testing.T) {
	manager, dir := newDeliveringManager(t, endpointOne, endpointTwo)
	publishQuestion(t, manager, "event-1", deliveryNow)
	manager.path = filepath.Join(t.TempDir(), "missing", "subscriptions.json")
	manager.sendPush = func(_ context.Context, subscription Subscription, _ []byte, _ sendOptions) error {
		if subscription.Endpoint != endpointTwo {
			t.Fatalf("first delivery endpoint = %q, want %q", subscription.Endpoint, endpointTwo)
		}
		return &pushError{statusCode: http.StatusGone}
	}
	if _, err := runAt(t.Context(), manager, deliveryNow); err == nil {
		t.Fatal("fallback recovery succeeded without subscription persistence")
	}
	requireEndpoints(t, "queued endpoints after failed recovery", queuedEndpoints(t, dir), endpointTwo)
	if subscriptions := manager.Subscriptions(); len(subscriptions) != 2 {
		t.Fatalf("subscriptions changed after failed recovery = %#v", subscriptions)
	}
	if keys := manager.RecoveredKeys(); len(keys) != 1 {
		t.Fatalf("RecoveredKeys() = %#v, want the restored event", keys)
	}
}

func TestTerminalEndpointIsSentOncePerQueuePass(t *testing.T) {
	manager, dir := newDeliveringManager(t, endpointOne)
	publishQuestion(t, manager, "event-1", deliveryNow)
	publishQuestion(t, manager, "event-2", deliveryNow)
	sends := 0
	manager.sendPush = func(context.Context, Subscription, []byte, sendOptions) error {
		sends++
		return &pushError{statusCode: http.StatusGone}
	}
	results := runPass(t, manager, deliveryNow)
	if sends != 1 || len(results) != 2 {
		t.Fatalf("terminal endpoint sends = %d, results = %#v", sends, results)
	}
	for _, result := range results {
		if result.Disposition != DeliveryPruned {
			t.Fatalf("terminal result = %#v", result)
		}
	}
	requireEndpoints(t, "queued endpoints", queuedEndpoints(t, dir))
	if len(manager.Subscriptions()) != 0 {
		t.Fatalf("terminal endpoint kept subscriptions = %#v", manager.Subscriptions())
	}
}

func TestTerminalTestDeliveryRetiresActiveAndDeliveredState(t *testing.T) {
	manager, dir := newDeliveringManager(t, endpointOne)
	log := acceptingManager(manager)
	key := PushEventKey{EventID: "test-event", Category: CategoryTest}
	if _, err := manager.Publish(t.Context(), PublishRequest{
		Key: key, Preview: PreviewHidden, CreatedAt: deliveryNow, ExpiresAt: deliveryNow.Add(time.Minute),
	}); err != nil {
		t.Fatal(err)
	}
	token := queuedEventRef(t, dir, "test-event")
	requireReferenceAccepted(t, manager, token)

	runPass(t, manager, deliveryNow)

	if len(log.sent) != 1 {
		t.Fatalf("sends = %d, want 1", len(log.sent))
	}
	requireReferenceStale(t, manager, token)
	if delivered := readQueueFile(t, dir).Delivered; len(delivered) != 0 {
		t.Fatalf("delivered records after test delivery = %#v", delivered)
	}
}

func TestRemoveDeviceClearsPushState(t *testing.T) {
	dir := t.TempDir()
	manager, err := NewManager(dir, testLogger())
	if err != nil {
		t.Fatal(err)
	}
	if err := manager.Subscribe(typedTestSubscription("device-a", endpointOne)); err != nil {
		t.Fatal(err)
	}
	policy := DefaultDevicePolicy("device-a", "en")
	policy.Categories[CategoryAttention] = false
	if err := manager.SetPolicy(policy); err != nil {
		t.Fatal(err)
	}
	result, err := manager.Publish(t.Context(), PublishRequest{
		Key:     PushEventKey{EventID: "event-a", Category: CategoryTest},
		Preview: PreviewHidden, CreatedAt: deliveryNow, ExpiresAt: deliveryNow.Add(time.Minute),
	})
	if err != nil || result.Queued != 1 {
		t.Fatalf("Publish() = %#v, %v", result, err)
	}
	token := queuedEventRef(t, dir, "event-a")
	requireReferenceAccepted(t, manager, token)

	if err := manager.RemoveDevice("device-a"); err != nil {
		t.Fatal(err)
	}

	if subscriptions := manager.Subscriptions(); len(subscriptions) != 0 {
		t.Fatalf("Subscriptions() = %#v", subscriptions)
	}
	if keys := manager.RecoveredKeys(); len(keys) != 0 {
		t.Fatalf("RecoveredKeys() = %#v", keys)
	}
	if !manager.Policy("device-a", "en").Categories[CategoryAttention] {
		t.Fatal("device policy was not removed")
	}
	requireReferenceStale(t, manager, token)
}

func TestSubscriptionReplacementPrunesDeliveredEndpoint(t *testing.T) {
	manager, dir := newDeliveringManager(t, endpointOne)
	log := acceptingManager(manager)
	publishQuestion(t, manager, "event-1", deliveryNow)
	runPass(t, manager, deliveryNow)
	requireEndpoints(t, "delivery endpoints", log.endpoints(), endpointOne)
	if delivered := readQueueFile(t, dir).Delivered; len(delivered) != 1 {
		t.Fatalf("delivered records = %#v, want 1", delivered)
	}

	if err := manager.Subscribe(typedTestSubscription("device-1", endpointTwo), []string{endpointOne}); err != nil {
		t.Fatal(err)
	}
	if err := manager.Resolve(t.Context(), exactKey("event-1")); err != nil {
		t.Fatal(err)
	}
	runPass(t, manager, time.Now().UTC())

	requireEndpoints(t, "endpoints after replacement", log.endpoints(), endpointOne)
	if delivered := readQueueFile(t, dir).Delivered; len(delivered) != 0 {
		t.Fatalf("replacement retained delivered records = %#v", delivered)
	}
}

func TestSubscriptionRefreshPreservesDeliveredEndpoint(t *testing.T) {
	manager, _ := newDeliveringManager(t, endpointOne)
	log := acceptingManager(manager)
	publishQuestion(t, manager, "event-1", deliveryNow)
	runPass(t, manager, deliveryNow)

	refreshed := typedTestSubscription("device-1", endpointOne)
	refreshed.Keys.Auth = "refreshed-auth"
	if err := manager.Subscribe(refreshed); err != nil {
		t.Fatal(err)
	}
	if err := manager.Resolve(t.Context(), exactKey("event-1")); err != nil {
		t.Fatal(err)
	}
	runPass(t, manager, time.Now().UTC())

	if len(log.sent) != 2 {
		t.Fatalf("sends = %#v, want notification then retract", log.sent)
	}
	retract := log.sent[1]
	if !retract.Payload.Retract || retract.Endpoint != endpointOne || retract.Auth != "refreshed-auth" {
		t.Fatalf("retract = %#v, want retract to the refreshed subscription", retract)
	}
}

func TestResolveWaitsForInFlightDelivery(t *testing.T) {
	manager, dir := newDeliveringManager(t, endpointOne)
	log := &sendLog{}
	started := make(chan struct{})
	release := make(chan struct{})
	manager.sendPush = func(_ context.Context, subscription Subscription, payload []byte, _ sendOptions) error {
		first := len(log.sent) == 0
		if err := log.record(subscription, payload); err != nil {
			return err
		}
		if first {
			close(started)
			<-release
		}
		return nil
	}
	publishQuestion(t, manager, "event-1", deliveryNow)

	processed := make(chan error, 1)
	go func() {
		_, err := runAt(t.Context(), manager, deliveryNow)
		processed <- err
	}()
	<-started
	resolved := make(chan error, 1)
	go func() { resolved <- manager.Resolve(t.Context(), exactKey("event-1")) }()
	select {
	case err := <-resolved:
		t.Fatalf("Resolve returned before the delivery completed: %v", err)
	case <-time.After(20 * time.Millisecond):
	}
	close(release)
	if err := <-processed; err != nil {
		t.Fatal(err)
	}
	if err := <-resolved; err != nil {
		t.Fatal(err)
	}
	runPass(t, manager, time.Now().UTC())

	if len(log.sent) != 2 || log.sent[0].Payload.Retract || !log.sent[1].Payload.Retract {
		t.Fatalf("sent payloads = %#v, want notification then retract", log.sent)
	}
	if entries := readQueueFile(t, dir).Entries; len(entries) != 0 {
		t.Fatalf("queue entries after retract = %#v", entries)
	}
}

// Other devices are subscribed after the two endpoints of device-1, so Publish
// queues them first and reaches device-1 late, which widens the interleaving.
func TestPublishDuringTerminalRecovery(t *testing.T) {
	seed := seedDeviceDirs(t, endpointOne, endpointTwo)
	for round := range stressRound {
		manager, dir := managerFromSeed(t, seed)
		first := exactKey("event-a")
		if _, err := manager.Publish(t.Context(), PublishRequest{
			Key: first, Preview: PreviewQuestion, CreatedAt: deliveryNow, ExpiresAt: deliveryNow.Add(time.Hour),
		}); err != nil {
			t.Fatal(err)
		}

		log := &sendLog{}
		started := make(chan struct{})
		release := make(chan struct{})
		var startOnce sync.Once
		manager.sendPush = func(_ context.Context, subscription Subscription, payload []byte, _ sendOptions) error {
			if err := log.record(subscription, payload); err != nil {
				return err
			}
			if subscription.Endpoint != endpointTwo {
				return nil
			}
			startOnce.Do(func() { close(started) })
			<-release
			return &pushError{statusCode: http.StatusGone}
		}

		passed := make(chan error, 1)
		go func() {
			_, err := runAt(t.Context(), manager, deliveryNow)
			passed <- err
		}()
		<-started
		published := make(chan error, 1)
		go func() {
			<-release
			key := typedTestKey("")
			key.EventID = "event-b"
			_, err := manager.Publish(t.Context(), PublishRequest{
				Key: key, Preview: PreviewQuestion, CreatedAt: deliveryNow, ExpiresAt: deliveryNow.Add(time.Hour),
			})
			published <- err
		}()
		close(release)
		if err := <-passed; err != nil {
			t.Fatalf("round %d: first pass: %v", round, err)
		}
		if err := <-published; err != nil {
			t.Fatalf("round %d: Publish: %v", round, err)
		}

		requireEndpoints(t, "queued endpoints after recovery", queuedEndpointsOf(t, dir, "device-1"), endpointOne, endpointOne)
		if _, err := runAt(t.Context(), manager, deliveryNow); err != nil {
			t.Fatalf("round %d: second pass: %v", round, err)
		}
		var after []string
		for _, sent := range log.sent[1:] {
			if sent.Payload.Key.DeviceID == "device-1" {
				after = append(after, sent.Endpoint)
			}
		}
		requireEndpoints(t, "sends to device-1 after recovery", after, endpointOne, endpointOne)
		requireEndpoints(t, "queued endpoints after second pass", queuedEndpointsOf(t, dir, "device-1"))
	}
}

// Same widening as above: the replaced device is reached last by Publish.
func TestPublishAndReplacementOutcome(t *testing.T) {
	seed := seedDeviceDirs(t, endpointOne)
	for round := range stressRound {
		manager, dir := managerFromSeed(t, seed)
		start := make(chan struct{})
		published := make(chan error, 1)
		replaced := make(chan error, 1)
		go func() {
			<-start
			_, err := manager.Publish(t.Context(), PublishRequest{
				Key: typedTestKey(""), Preview: PreviewQuestion, CreatedAt: deliveryNow, ExpiresAt: deliveryNow.Add(time.Hour),
			})
			published <- err
		}()
		go func() {
			<-start
			replaced <- manager.Subscribe(typedTestSubscription("device-1", endpointTwo), []string{endpointOne})
		}()
		close(start)
		if err := <-published; err != nil {
			t.Fatalf("round %d: Publish: %v", round, err)
		}
		if err := <-replaced; err != nil {
			t.Fatalf("round %d: Subscribe: %v", round, err)
		}
		requireEndpoints(t, "queued endpoints of the replaced device", queuedEndpointsOf(t, dir, "device-1"), endpointTwo)
		if entries := readQueueFile(t, dir).Entries; len(entries) != len(otherDevices)+1 {
			t.Fatalf("round %d: queue entries = %d, want %d", round, len(entries), len(otherDevices)+1)
		}
	}
}

func TestExpiredEntryIsDroppedWithoutSending(t *testing.T) {
	manager, dir := newDeliveringManager(t, endpointOne)
	log := acceptingManager(manager)
	publishQuestionFor(t, manager, "event-1", deliveryNow, time.Minute)

	results := runPass(t, manager, deliveryNow.Add(2*time.Minute))

	if len(results) != 1 || results[0].Disposition != DeliveryExpired || len(log.sent) != 0 {
		t.Fatalf("results = %#v, sends = %d", results, len(log.sent))
	}
	requireEndpoints(t, "queued endpoints", queuedEndpoints(t, dir))
}

func TestNonRetryableSendErrorDropsEntry(t *testing.T) {
	manager, dir := newDeliveringManager(t, endpointOne)
	sends := 0
	manager.sendPush = func(context.Context, Subscription, []byte, sendOptions) error {
		sends++
		return &pushError{statusCode: http.StatusBadRequest}
	}
	publishQuestion(t, manager, "event-1", deliveryNow)

	results := runPass(t, manager, deliveryNow)

	if len(results) != 1 || results[0].Disposition != DeliveryDropped || results[0].Attempts != 1 || sends != 1 {
		t.Fatalf("results = %#v, sends = %d", results, sends)
	}
	requireEndpoints(t, "queued endpoints", queuedEndpoints(t, dir))
	if len(manager.Subscriptions()) != 1 {
		t.Fatalf("subscriptions = %#v, want the endpoint kept", manager.Subscriptions())
	}
}

func TestRetryableSendErrorDropsEntryAfterAttemptLimit(t *testing.T) {
	manager, dir := newDeliveringManager(t, endpointOne)
	sends := 0
	manager.sendPush = func(context.Context, Subscription, []byte, sendOptions) error {
		sends++
		return &pushError{statusCode: http.StatusServiceUnavailable}
	}
	publishQuestion(t, manager, "event-1", deliveryNow)

	at := deliveryNow
	for attempt := 1; attempt < maxDeliveryAttempts; attempt++ {
		results := runPass(t, manager, at)
		if len(results) != 1 || results[0].Disposition != DeliveryRetrying || results[0].Attempts != attempt {
			t.Fatalf("attempt %d results = %#v", attempt, results)
		}
		if !results[0].NextAttempt.After(at) {
			t.Fatalf("attempt %d next attempt %s is not after %s", attempt, results[0].NextAttempt, at)
		}
		at = results[0].NextAttempt
	}
	results := runPass(t, manager, at)

	if len(results) != 1 || results[0].Disposition != DeliveryDropped || results[0].Attempts != maxDeliveryAttempts {
		t.Fatalf("final results = %#v", results)
	}
	if sends != maxDeliveryAttempts {
		t.Fatalf("sends = %d, want %d", sends, maxDeliveryAttempts)
	}
	requireEndpoints(t, "queued endpoints", queuedEndpoints(t, dir))
}

func TestCancelledContextStopsPassAndKeepsQueue(t *testing.T) {
	t.Run("before a send", func(t *testing.T) {
		manager, dir := newDeliveringManager(t, endpointOne)
		log := acceptingManager(manager)
		publishQuestion(t, manager, "event-1", deliveryNow)
		ctx, cancel := context.WithCancel(t.Context())
		cancel()

		if _, err := runAt(ctx, manager, deliveryNow); !errors.Is(err, context.Canceled) {
			t.Fatalf("RunOnce() error = %v, want %v", err, context.Canceled)
		}
		if len(log.sent) != 0 {
			t.Fatalf("sends = %#v, want none", log.sent)
		}
		requireEndpoints(t, "queued endpoints", queuedEndpoints(t, dir), endpointOne)
	})

	t.Run("during a failed send", func(t *testing.T) {
		manager, dir := newDeliveringManager(t, endpointOne)
		publishQuestion(t, manager, "event-1", deliveryNow)
		ctx, cancel := context.WithCancel(t.Context())
		manager.sendPush = func(context.Context, Subscription, []byte, sendOptions) error {
			cancel()
			return context.Canceled
		}

		if _, err := runAt(ctx, manager, deliveryNow); !errors.Is(err, context.Canceled) {
			t.Fatalf("RunOnce() error = %v, want %v", err, context.Canceled)
		}
		queue := readQueueFile(t, dir)
		if len(queue.Entries) != 1 {
			t.Fatalf("queue entries = %#v, want the entry kept", queue.Entries)
		}
		for _, entry := range queue.Entries {
			if entry.Attempts != 0 {
				t.Fatalf("attempts = %d, want 0", entry.Attempts)
			}
		}
	})

	t.Run("after a send", func(t *testing.T) {
		manager, dir := newDeliveringManager(t, endpointOne)
		log := &sendLog{}
		ctx, cancel := context.WithCancel(t.Context())
		manager.sendPush = func(_ context.Context, subscription Subscription, payload []byte, _ sendOptions) error {
			cancel()
			return log.record(subscription, payload)
		}
		publishQuestion(t, manager, "event-1", deliveryNow)
		publishQuestion(t, manager, "event-2", deliveryNow)

		results, err := runAt(ctx, manager, deliveryNow)

		if !errors.Is(err, context.Canceled) {
			t.Fatalf("RunOnce() error = %v, want %v", err, context.Canceled)
		}
		if len(results) != 1 || results[0].Disposition != DeliveryAccepted || len(log.sent) != 1 {
			t.Fatalf("results = %#v, sends = %d", results, len(log.sent))
		}
		queue := readQueueFile(t, dir)
		if len(queue.Entries) != 1 || len(queue.Delivered) != 1 {
			t.Fatalf("queue = %#v, want one entry left and one delivered record", queue)
		}
	})
}

func TestAcceptedDeliveryIsKeptWhenPolicyCannotPersist(t *testing.T) {
	manager, dir := newDeliveringManager(t, endpointOne)
	log := acceptingManager(manager)
	publishQuestion(t, manager, "event-1", deliveryNow)
	policyPath := filepath.Join(dir, "policies.json")
	if err := os.Remove(policyPath); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(policyPath, 0o700); err != nil {
		t.Fatal(err)
	}

	if _, err := runAt(t.Context(), manager, deliveryNow); err == nil {
		t.Fatal("RunOnce() succeeded although the policy file cannot be written")
	}

	if len(log.sent) != 1 {
		t.Fatalf("sends = %d, want 1", len(log.sent))
	}
	queue := readQueueFile(t, dir)
	if len(queue.Entries) != 0 || len(queue.Delivered) != 1 {
		t.Fatalf("queue = %#v, want the accepted delivery recorded", queue)
	}
}

func TestResolveWithoutDeliveredRecordSendsNothing(t *testing.T) {
	t.Run("queued event", func(t *testing.T) {
		manager, dir := newDeliveringManager(t, endpointOne)
		log := acceptingManager(manager)
		publishQuestion(t, manager, "event-1", deliveryNow)
		token := queuedEventRef(t, dir, "event-1")

		if err := manager.Resolve(t.Context(), exactKey("event-1")); err != nil {
			t.Fatal(err)
		}
		runPass(t, manager, deliveryNow)

		if len(log.sent) != 0 {
			t.Fatalf("sends = %#v, want none", log.sent)
		}
		requireEndpoints(t, "queued endpoints", queuedEndpoints(t, dir))
		requireReferenceStale(t, manager, token)
	})

	t.Run("unknown event", func(t *testing.T) {
		manager, dir := newDeliveringManager(t, endpointOne)
		log := acceptingManager(manager)

		if err := manager.Resolve(t.Context(), exactKey("missing")); err != nil {
			t.Fatal(err)
		}
		runPass(t, manager, deliveryNow)

		if len(log.sent) != 0 {
			t.Fatalf("sends = %#v, want none", log.sent)
		}
		requireEndpoints(t, "queued endpoints", queuedEndpoints(t, dir))
	})
}

func TestReconcileRetractsOnlyStaleRecoveredEvents(t *testing.T) {
	recoverManager := func(t *testing.T) *Manager {
		t.Helper()
		first, dir := newDeliveringManager(t, endpointOne)
		acceptingManager(first)
		publishQuestion(t, first, "event-1", deliveryNow)
		runPass(t, first, deliveryNow)
		recovered, err := NewManager(dir, testLogger())
		if err != nil {
			t.Fatal(err)
		}
		if keys := recovered.RecoveredKeys(); len(keys) != 1 || keys[0] != exactKey("event-1") {
			t.Fatalf("RecoveredKeys() = %#v", keys)
		}
		return recovered
	}

	t.Run("stale key", func(t *testing.T) {
		recovered := recoverManager(t)
		log := acceptingManager(recovered)

		if err := recovered.Reconcile(t.Context(), func(PushEventKey) bool { return false }); err != nil {
			t.Fatal(err)
		}
		runPass(t, recovered, time.Now().UTC())

		if len(log.sent) != 1 || !log.sent[0].Payload.Retract || log.sent[0].Payload.Key != exactKey("event-1") {
			t.Fatalf("sent payloads = %#v, want one retract", log.sent)
		}
	})

	t.Run("current key", func(t *testing.T) {
		recovered := recoverManager(t)
		log := acceptingManager(recovered)

		if err := recovered.Reconcile(t.Context(), func(key PushEventKey) bool { return key == exactKey("event-1") }); err != nil {
			t.Fatal(err)
		}
		runPass(t, recovered, time.Now().UTC())

		if len(log.sent) != 0 {
			t.Fatalf("sent payloads = %#v, want none", log.sent)
		}
		if keys := recovered.RecoveredKeys(); len(keys) != 1 {
			t.Fatalf("RecoveredKeys() = %#v, want the event kept", keys)
		}
	})
}

func TestPanePublicationKeepsExactActiveKey(t *testing.T) {
	manager, dir := newDeliveringManager(t, endpointOne)
	log := acceptingManager(manager)
	publishQuestion(t, manager, "event-1", deliveryNow)
	publishQuestion(t, manager, "event-2", deliveryNow)
	first := queuedEventRef(t, dir, "event-1")
	second := queuedEventRef(t, dir, "event-2")
	runPass(t, manager, deliveryNow)
	if len(log.sent) != 2 {
		t.Fatalf("sends = %d, want 2", len(log.sent))
	}

	result, err := manager.ApplyPaneTransition(t.Context(), "pane-1", &PublishRequest{
		Key: exactKey("event-2"), Preview: PreviewQuestion,
	}, func() bool { return true })
	if err != nil || result.Queued != 0 {
		t.Fatalf("republication = %#v, %v", result, err)
	}
	runPass(t, manager, time.Now().UTC())

	if len(log.sent) != 3 || !log.sent[2].Payload.Retract || log.sent[2].Payload.Key != exactKey("event-1") {
		t.Fatalf("sent payloads = %#v, want one retract for event-1", log.sent)
	}
	requireReferenceStale(t, manager, first)
	requireReferenceAccepted(t, manager, second)
}

func TestTerminalFailureWithoutOtherEndpointRemovesItsState(t *testing.T) {
	manager, dir := newDeliveringManager(t, endpointOne)
	acceptingManager(manager)
	publishQuestion(t, manager, "event-1", deliveryNow)
	runPass(t, manager, deliveryNow)
	publishQuestion(t, manager, "event-2", deliveryNow)
	if queue := readQueueFile(t, dir); len(queue.Entries) != 1 || len(queue.Delivered) != 1 {
		t.Fatalf("queue before terminal failure = %#v", queue)
	}
	manager.sendPush = func(context.Context, Subscription, []byte, sendOptions) error {
		return &pushError{statusCode: http.StatusGone}
	}

	results := runPass(t, manager, deliveryNow)

	if len(results) != 1 || results[0].Disposition != DeliveryPruned {
		t.Fatalf("results = %#v", results)
	}
	if queue := readQueueFile(t, dir); len(queue.Entries) != 0 || len(queue.Delivered) != 0 {
		t.Fatalf("queue = %#v, want no state for the removed endpoint", queue)
	}
	if len(manager.Subscriptions()) != 0 || len(manager.RecoveredKeys()) != 0 {
		t.Fatalf("subscriptions = %#v, recovered keys = %#v", manager.Subscriptions(), manager.RecoveredKeys())
	}
}

func TestTerminalFailureMovesOtherQueuedEntriesToRetainedEndpoint(t *testing.T) {
	manager, dir := newDeliveringManager(t, endpointOne, endpointTwo)
	var endpoints []string
	manager.sendPush = func(_ context.Context, subscription Subscription, _ []byte, _ sendOptions) error {
		endpoints = append(endpoints, subscription.Endpoint)
		if subscription.Endpoint == endpointTwo {
			return &pushError{statusCode: http.StatusGone}
		}
		return nil
	}
	publishQuestion(t, manager, "event-1", deliveryNow)
	publishQuestion(t, manager, "event-2", deliveryNow)
	later := DefaultDevicePolicy("device-1", "en")
	later.Settle = time.Hour
	later.Cooldown = 0
	if err := manager.SetPolicy(later); err != nil {
		t.Fatal(err)
	}
	publishQuestion(t, manager, "event-3", deliveryNow)

	runPass(t, manager, deliveryNow)

	requireEndpoints(t, "first pass endpoints", endpoints, endpointTwo)
	requireEndpoints(t, "queued endpoints", queuedEndpoints(t, dir), endpointOne, endpointOne, endpointOne)
	runPass(t, manager, deliveryNow)
	requireEndpoints(t, "second pass endpoints", endpoints, endpointTwo, endpointOne, endpointOne)
	requireEndpoints(t, "queued endpoints after second pass", queuedEndpoints(t, dir), endpointOne)
}

func TestWaitingSendUsesFreshTime(t *testing.T) {
	for _, elapsed := range []time.Duration{10 * time.Second, 5 * time.Minute} {
		t.Run(elapsed.String(), func(t *testing.T) {
			manager, _ := newDeliveringManager(t, endpointOne)
			now := deliveryNow
			manager.now = func() time.Time { return now }
			for _, name := range []string{"first", "second"} {
				key := exactKey(name)
				key.PaneID = name
				if _, err := manager.Publish(t.Context(), PublishRequest{Key: key, Preview: PreviewHidden}); err != nil {
					t.Fatal(err)
				}
			}
			var sent []sendOptions
			manager.sendPush = func(_ context.Context, _ Subscription, _ []byte, options sendOptions) error {
				sent = append(sent, options)
				if len(sent) == 1 {
					now = now.Add(elapsed)
					return &pushError{statusCode: http.StatusServiceUnavailable}
				}
				return nil
			}
			results, err := manager.RunOnce(t.Context())
			if err != nil || len(results) != 2 || len(sent) == 0 || sent[0].TTL != 86400 {
				t.Fatalf("results = %#v, sends = %#v, error = %v", results, sent, err)
			}
			if elapsed == 5*time.Minute {
				if len(sent) != 1 || results[1].Disposition != DeliveryExpired {
					t.Fatalf("sent past deadline: %#v, %#v", sent, results)
				}
				return
			}
			if len(sent) != 2 || sent[1].TTL != 86390 || !results[0].NextAttempt.Equal(now.Add(time.Second)) {
				t.Fatalf("second send or retry reused old time: %#v, %#v", sent, results)
			}
			for _, accepted := range manager.queue.state.Delivered {
				if !accepted.AcceptedAt.Equal(now) {
					t.Fatalf("accepted at %v, want %v", accepted.AcceptedAt, now)
				}
			}
		})
	}
}

func TestWaitingSendExpiresDuringRelaySleep(t *testing.T) {
	manager, dir := newDeliveringManager(t, endpointOne)
	now := deliveryNow
	manager.now = func() time.Time { return now }
	policy := manager.Policy("device-1", "en")
	policy.Settle = 2 * time.Second
	if err := manager.SetPolicy(policy); err != nil {
		t.Fatal(err)
	}
	log := acceptingManager(manager)
	key := exactKey("sleep")
	applyWaiting(t, manager, key, now)
	now = now.Add(10 * time.Minute)
	results, err := manager.RunOnce(t.Context())
	if err != nil || len(results) != 1 || results[0].Disposition != DeliveryExpired {
		t.Fatalf("wake-up results = %#v, %v", results, err)
	}
	if err := manager.Resolve(t.Context(), key); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.RunOnce(t.Context()); err != nil {
		t.Fatal(err)
	}
	queue := readQueueFile(t, dir)
	if len(log.sent) != 0 || len(queue.Delivered) != 0 || len(queue.Entries) != 0 {
		t.Fatalf("expired alert produced delivery state: %#v, %#v", log.sent, queue)
	}
}

func TestVerifyEventReference(t *testing.T) {
	manager, dir := newDeliveringManager(t, endpointOne)
	acceptingManager(manager)
	publishQuestionFor(t, manager, "event-1", deliveryNow, 5*time.Minute)
	token := queuedEventRef(t, dir, "event-1")

	for _, entry := range manager.queue.state.Entries {
		if !entry.Event.ExpiresAt.Equal(deliveryNow.Add(5 * time.Minute)) {
			t.Fatalf("local deadline = %v", entry.Event.ExpiresAt)
		}
	}
	if results := runPass(t, manager, deliveryNow); len(results) != 1 || results[0].Disposition != DeliveryAccepted {
		t.Fatalf("delivery = %#v", results)
	}
	late := deliveryNow.Add(47 * time.Hour)
	claims, err := manager.VerifyEventReference(token, late)
	if err != nil || claims.Key != exactKey("event-1") {
		t.Fatalf("VerifyEventReference() = %#v, %v", claims, err)
	}
	if _, err := manager.VerifyEventReference("not-a-token", deliveryNow); !errors.Is(err, ErrInvalidReference) {
		t.Fatalf("garbage token error = %v, want %v", err, ErrInvalidReference)
	}
	if _, err := manager.VerifyEventReference(token, deliveryNow.Add(48*time.Hour)); !errors.Is(err, ErrExpiredReference) {
		t.Fatalf("expired token error = %v, want %v", err, ErrExpiredReference)
	}

	recovered, err := NewManager(dir, testLogger())
	if err != nil {
		t.Fatal(err)
	}
	requireReferenceStale(t, recovered, token)
	if err := recovered.Reconcile(t.Context(), func(key PushEventKey) bool { return key == exactKey("event-1") }); err != nil {
		t.Fatal(err)
	}
	requireReferenceAccepted(t, recovered, token)

	if err := manager.Resolve(t.Context(), exactKey("event-1")); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.VerifyEventReference(token, late); !errors.Is(err, ErrStaleReference) {
		t.Fatalf("resolved token error = %v, want %v", err, ErrStaleReference)
	}
	policy := manager.Policy("device-1", "en")
	policy.Categories[CategoryFinished] = true
	if err := manager.SetPolicy(policy); err != nil {
		t.Fatal(err)
	}
	finished := exactKey("finished")
	finished.Category = CategoryFinished
	if _, err := manager.Publish(t.Context(), PublishRequest{Key: finished, Preview: PreviewBrief, CreatedAt: deliveryNow}); err != nil {
		t.Fatal(err)
	}
	ref := queuedEventRef(t, dir, "finished")
	requireReferenceAccepted(t, manager, ref)
	if _, err := manager.VerifyEventReference(ref, deliveryNow.Add(5*time.Minute)); !errors.Is(err, ErrExpiredReference) {
		t.Fatalf("finished token error = %v, want %v", err, ErrExpiredReference)
	}
}
