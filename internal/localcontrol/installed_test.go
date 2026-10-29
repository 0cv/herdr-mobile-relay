package localcontrol

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
)

func installedFixture(t *testing.T, change func(*InstalledBackend)) (InstalledControl, InstalledRequest, *[5]int) {
	t.Helper()
	calls := new([5]int)
	b := InstalledBackend{
		Publish: func(context.Context) error { calls[0]++; return nil },
		Status: func(context.Context) (InstalledObservation, error) {
			calls[1]++
			return InstalledObservation{Registered: true}, nil
		},
		Admit:   func(context.Context) error { calls[2]++; return nil },
		Arm:     func(context.Context) (string, error) { calls[3]++; return "synthetic-invitation", nil },
		Reprint: func(context.Context) (string, error) { calls[4]++; return "synthetic-invitation", nil },
	}
	if change != nil {
		change(&b)
	}
	c, credentials, err := NewInstalledControl("installation", InstalledSelection{"selection", "node", "profile", "https://synthetic.invalid", 443, 8080, "attempt"}, b)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(c.Retire)
	return c, InstalledRequest{Protocol: InstalledProtocol, Run: credentials.Run, Installation: credentials.Installation, Capability: credentials.Capability, Op: "status"}, calls
}
func installedCall(t *testing.T, c InstalledControl, ctx context.Context, r InstalledRequest) InstalledReply {
	t.Helper()
	data, err := json.Marshal(r)
	if err != nil {
		t.Fatal(err)
	}
	var reply InstalledReply
	if err = json.Unmarshal(c.Dispatch(ctx, data), &reply); err != nil {
		t.Fatal(err)
	}
	return reply
}
func installedConsent(t *testing.T, c InstalledControl, r InstalledRequest) *InstalledConsent {
	t.Helper()
	r.Op = "proposal"
	p := installedCall(t, c, context.Background(), r)
	if p.Proposal == nil {
		t.Fatal("missing proposal")
	}
	return &InstalledConsent{*p.Proposal, true, true, true, true, true, true}
}

func TestInstalledControlAuthentication(t *testing.T) {
	c, r, calls := installedFixture(t, nil)
	for _, change := range []func(*InstalledRequest){
		func(r *InstalledRequest) { r.Capability = "" }, func(r *InstalledRequest) { r.Capability = r.Run },
		func(r *InstalledRequest) { r.Capability = strings.Repeat("00", 32) }, func(r *InstalledRequest) { r.Run = "stale" },
		func(r *InstalledRequest) { r.Installation = "other" }, func(r *InstalledRequest) { r.Protocol = "legacy" },
	} {
		bad := r
		change(&bad)
		if got := installedCall(t, c, context.Background(), bad); got.Code != "unauthorized" {
			t.Fatal(got)
		}
	}
	other, _, _ := installedFixture(t, nil)
	if installedCall(t, other, context.Background(), r).Code != "unauthorized" {
		t.Fatal("previous run accepted")
	}
	if *calls != [5]int{} {
		t.Fatal("unauthenticated callback")
	}
	if installedCall(t, c, context.Background(), r).Code != "ok" || calls[1] != 1 {
		t.Fatal("authenticated status refused")
	}
	if installedCall(t, InstalledControl{}, context.Background(), r).Code != "unavailable" {
		t.Fatal("zero handle")
	}
}

func TestInstalledControlStrictCodec(t *testing.T) {
	c, r, calls := installedFixture(t, nil)
	data, _ := json.Marshal(r)
	base := string(data)
	for _, bad := range []string{"null", "[]", "{", base + " {}", strings.Repeat(" ", MaxRequestBytes+1),
		strings.Replace(base, `"op":"status"`, `"op":"status","op":"arm"`, 1),
		strings.Replace(base, `"op":"status"`, `"op":"status","Op":"arm"`, 1),
		strings.Replace(base, `"op"`, `"Op"`, 1), strings.Replace(base, `"status"`, `"unsupported"`, 1),
		strings.Replace(base, `"op":"status"`, `"op":"status","ready":true`, 1),
		strings.Replace(base, `"op":"status"`, `"op":"status","consent":null`, 1),
	} {
		var got InstalledReply
		_ = json.Unmarshal(c.Dispatch(context.Background(), []byte(bad)), &got)
		if got.Code != "invalid_request" {
			t.Fatalf("code %s", got.Code)
		}
	}
	if *calls != [5]int{} {
		t.Fatal("invalid callback")
	}
}

func TestInstalledControlExactConsent(t *testing.T) {
	c, r, calls := installedFixture(t, nil)
	good := installedConsent(t, c, r)
	r.Op = "publish"
	changes := []func(*InstalledConsent){
		func(c *InstalledConsent) { c.Production = false }, func(c *InstalledConsent) { c.Persistent = false },
		func(c *InstalledConsent) { c.CheckToWriteRace = false }, func(c *InstalledConsent) { c.PortReuseRisk = false }, func(c *InstalledConsent) { c.NoRollback = false }, func(c *InstalledConsent) { c.NoRemoteDrain = false },
		func(c *InstalledConsent) { c.Proposal.Run = "other" }, func(c *InstalledConsent) { c.Proposal.Installation = "other" }, func(c *InstalledConsent) { c.Proposal.ID = "other" },
		func(c *InstalledConsent) { c.Proposal.Selection = "other" }, func(c *InstalledConsent) { c.Proposal.Node = "other" }, func(c *InstalledConsent) { c.Proposal.Profile = "other" }, func(c *InstalledConsent) { c.Proposal.Origin = "https://other.invalid" }, func(c *InstalledConsent) { c.Proposal.HTTPSPort++ }, func(c *InstalledConsent) { c.Proposal.RelayPort++ }, func(c *InstalledConsent) { c.Proposal.Attempt = "other" },
	}
	for _, change := range changes {
		bad := *good
		change(&bad)
		r.Consent = &bad
		if installedCall(t, c, context.Background(), r).Code != "consent_refused" {
			t.Fatal("consent accepted")
		}
	}
	if calls[0] != 0 {
		t.Fatal("premature publish")
	}
	r.Consent = good
	if got := installedCall(t, c, context.Background(), r); got.Publication != "acknowledged" {
		t.Fatal(got)
	}
	if installedCall(t, c, context.Background(), r).Code != "consumed" || calls[0] != 1 {
		t.Fatal("replay")
	}
	r.Op = "proposal"
	r.Consent = nil
	if installedCall(t, c, context.Background(), r).Code != "consumed" {
		t.Fatal("proposal renewed")
	}
	if calls[3] != 0 {
		t.Fatal("implicit arm")
	}
}

func TestInstalledControlPublicationUncertainty(t *testing.T) {
	c, r, _ := installedFixture(t, func(b *InstalledBackend) {
		b.Publish = func(context.Context) error { return errors.New("synthetic-secret-error") }
	})
	r.Consent = installedConsent(t, c, r)
	r.Op = "publish"
	got := installedCall(t, c, context.Background(), r)
	if got.Code != "outcome_unknown" || got.Publication != "dispatched_unknown" {
		t.Fatal(got)
	}
	if installedCall(t, c, context.Background(), r).Code != "consumed" {
		t.Fatal("replayed error")
	}
	r.Op = "status"
	r.Consent = nil
	if installedCall(t, c, context.Background(), r).Publication != "dispatched_unknown" {
		t.Fatal("lost uncertainty")
	}
}

func TestInstalledControlPairingAdmissionAndReplay(t *testing.T) {
	denied := false
	c, r, calls := installedFixture(t, func(b *InstalledBackend) {
		original := b.Admit
		b.Admit = func(ctx context.Context) error {
			_ = original(ctx)
			if denied {
				return errors.New("secret")
			}
			return nil
		}
	})
	for _, op := range []string{"arm", "reprint"} {
		r.Op = op
		if op == "arm" {
			r.OperationID = "one"
		} else {
			r.OperationID = ""
		}
		denied = true
		if got := installedCall(t, c, context.Background(), r); got.Code != "admission_refused" || got.Invitation != "" {
			t.Fatal(got)
		}
	}
	if calls[3] != 0 || calls[4] != 0 {
		t.Fatal("failed admission dispatched")
	}
	denied = false
	r.Op = "arm"
	r.OperationID = "one"
	if installedCall(t, c, context.Background(), r).Invitation != "synthetic-invitation" {
		t.Fatal("arm failed")
	}
	if got := installedCall(t, c, context.Background(), r); got.Code != "consumed" || got.Invitation != "" {
		t.Fatal(got)
	}
	r.OperationID = "two"
	if installedCall(t, c, context.Background(), r).Code != "ok" {
		t.Fatal("fresh arm")
	}
	r.Op = "reprint"
	r.OperationID = ""
	if installedCall(t, c, context.Background(), r).Invitation == "" || calls[3] != 2 || calls[4] != 1 || calls[2] != 6 {
		t.Fatal("reprint/admission accounting")
	}
	r.Op = "status"
	if installedCall(t, c, context.Background(), r).Invitation != "" {
		t.Fatal("status secret")
	}
}

func TestInstalledControlArmUncertaintyAndCapacity(t *testing.T) {
	arms := 0
	c, r, _ := installedFixture(t, func(b *InstalledBackend) {
		b.Arm = func(context.Context) (string, error) { arms++; return "secret", errors.New("secret") }
	})
	r.Op = "arm"
	for i := 0; i < installedReceiptLimit; i++ {
		r.OperationID = fmt.Sprint(i)
		got := installedCall(t, c, context.Background(), r)
		if got.ArmReceipt != "dispatched_unknown" || got.Invitation != "" {
			t.Fatal(got)
		}
	}
	r.OperationID = "overflow"
	if installedCall(t, c, context.Background(), r).Code != "receipt_capacity" {
		t.Fatal("unbounded receipts")
	}
	r.OperationID = "0"
	if installedCall(t, c, context.Background(), r).Code != "consumed" || arms != installedReceiptLimit {
		t.Fatal("evicted uncertainty")
	}
	r.Op = "status"
	if installedCall(t, c, context.Background(), r).ArmReceipt != "dispatched_unknown" {
		t.Fatal("missing receipt")
	}
}

func TestInstalledControlRedactionAndBounds(t *testing.T) {
	c, r, _ := installedFixture(t, func(b *InstalledBackend) {
		b.Reprint = func(context.Context) (string, error) { return strings.Repeat("x", 4097), nil }
	})
	for _, v := range []any{r, c, InstalledCredentials{Capability: r.Capability}, InstalledReply{Invitation: "synthetic-invitation"}} {
		for _, format := range []string{"%v", "%+v", "%#v"} {
			text := fmt.Sprintf(format, v)
			if strings.Contains(text, r.Capability) || strings.Contains(text, "synthetic-invitation") {
				t.Fatal("diagnostic secret")
			}
		}
	}
	r.Op = "reprint"
	got := installedCall(t, c, context.Background(), r)
	if got.Code != "outcome_unknown" || got.Invitation != "" {
		t.Fatal(got)
	}
}

// All spawned callers are joined in cleanup BEFORE fixture retirement. Release
// gates are closed even on Fatal; callbacks never call testing methods.
func TestInstalledControlCancellationQueueAndRetirement(t *testing.T) {
	entered := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once
	c, r, _ := installedFixture(t, func(b *InstalledBackend) {
		b.Publish = func(ctx context.Context) error { close(entered); <-release; return ctx.Err() }
	})
	r.Consent = installedConsent(t, c, r)
	r.Op = "publish"
	data, _ := json.Marshal(r)
	var wg sync.WaitGroup
	t.Cleanup(func() { once.Do(func() { close(release) }); wg.Wait() })
	first := make(chan []byte, 1)
	wg.Add(1)
	go func() { defer wg.Done(); first <- c.Dispatch(context.Background(), data) }()
	<-entered
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if got := installedCall(t, c, ctx, r); got.Code != "cancelled" {
		t.Fatal(got)
	}
	duplicate := make(chan []byte, 1)
	wg.Add(1)
	go func() { defer wg.Done(); duplicate <- c.Dispatch(context.Background(), data) }()
	copyHandle := c
	retired := make(chan struct{})
	wg.Add(1)
	go func() { defer wg.Done(); copyHandle.Retire(); close(retired) }()
	<-c.state.ctx.Done()
	select {
	case <-retired:
		t.Fatal("retirement escaped active callback")
	default:
	}
	once.Do(func() { close(release) })
	wg.Wait()
	for _, raw := range [][]byte{<-first, <-duplicate} {
		var got InstalledReply
		_ = json.Unmarshal(raw, &got)
		if got.Invitation != "" {
			t.Fatal("secret")
		}
	}
	if installedCall(t, c, context.Background(), r).Code != "retired" {
		t.Fatal("retained handle")
	}
}

func TestInstalledControlCancelledAdmissionAndArm(t *testing.T) {
	for _, stage := range []string{"admit", "arm"} {
		t.Run(stage, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			arms := 0
			c, r, _ := installedFixture(t, func(b *InstalledBackend) {
				b.Admit = func(context.Context) error {
					if stage == "admit" {
						cancel()
					}
					return nil
				}
				b.Arm = func(context.Context) (string, error) { arms++; cancel(); return "synthetic-secret", nil }
			})
			r.Op = "arm"
			r.OperationID = "one"
			got := installedCall(t, c, ctx, r)
			if got.Invitation != "" || got.Code != "cancelled" {
				t.Fatal(got)
			}
			if stage == "admit" && arms != 0 {
				t.Fatal("cancelled admission armed")
			}
			if stage == "arm" {
				if installedCall(t, c, context.Background(), r).Code != "consumed" || arms != 1 {
					t.Fatal("cancelled arm replay")
				}
			}
		})
	}
}
