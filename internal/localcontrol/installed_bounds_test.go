package localcontrol

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"
)

func TestInstalledControlQueueBound(t *testing.T) {
	entered, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	c, r, _ := installedFixture(t, func(b *InstalledBackend) {
		b.Publish = func(context.Context) error { close(entered); <-release; return nil }
	})
	r.Consent = installedConsent(t, c, r)
	r.Op = "publish"
	data, _ := json.Marshal(r)
	ctx, cancel := context.WithCancel(context.Background())
	var wg sync.WaitGroup
	t.Cleanup(func() { cancel(); once.Do(func() { close(release) }); wg.Wait() })
	wg.Add(1)
	go func() { defer wg.Done(); _ = c.Dispatch(ctx, data) }()
	<-entered
	for i := 0; i < 15; i++ {
		observed := &installedObservedContext{Context: ctx, observed: make(chan struct{})}
		wg.Add(1)
		go func() { defer wg.Done(); _ = c.Dispatch(observed, data) }()
		<-observed.observed
	}
	if got := installedCall(t, c, context.Background(), r); got.Code != "busy" {
		t.Fatal(got)
	}
	cancel()
	once.Do(func() { close(release) })
	wg.Wait()
}

func TestInstalledControlOperationShapeAndNestedCodec(t *testing.T) {
	c, r, calls := installedFixture(t, nil)
	consent := installedConsent(t, c, r)
	for _, bad := range []InstalledRequest{
		{Protocol: r.Protocol, Run: r.Run, Installation: r.Installation, Capability: r.Capability, Op: "publish"},
		{Protocol: r.Protocol, Run: r.Run, Installation: r.Installation, Capability: r.Capability, Op: "arm"},
		{Protocol: r.Protocol, Run: r.Run, Installation: r.Installation, Capability: r.Capability, Op: "reprint", OperationID: "one"},
		{Protocol: r.Protocol, Run: r.Run, Installation: r.Installation, Capability: r.Capability, Op: "status", Consent: consent},
	} {
		if installedCall(t, c, context.Background(), bad).Code != "invalid_request" {
			t.Fatal("operation shape accepted")
		}
	}
	r.Op = "publish"
	r.Consent = consent
	data, _ := json.Marshal(r)
	for _, bad := range []string{
		strings.Replace(string(data), `"node"`, `"Node"`, 1),
		strings.Replace(string(data), `"production":true`, `"production":true,"Production":true`, 1),
		strings.Replace(string(data), `"production":true`, `"production":null`, 1),
		strings.Replace(string(data), `"production":true`, `"production":true,"qualified":true`, 1),
	} {
		var result InstalledReply
		_ = json.Unmarshal(c.Dispatch(context.Background(), []byte(bad)), &result)
		if result.Code != "invalid_request" {
			t.Fatal(result)
		}
	}
	if *calls != [5]int{} {
		t.Fatal("invalid callback")
	}
}

func TestInstalledControlEncodedResultBound(t *testing.T) {
	c, r, _ := installedFixture(t, func(b *InstalledBackend) {
		b.Reprint = func(context.Context) (string, error) { return strings.Repeat("<", 2000), nil }
	})
	r.Op = "reprint"
	data, _ := json.Marshal(r)
	raw := c.Dispatch(context.Background(), data)
	if string(raw) != `{"code":"result_refused"}` {
		t.Fatal("escaped result not bounded")
	}
}
