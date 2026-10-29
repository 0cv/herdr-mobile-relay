//go:build linux || darwin

package localcontrol

import (
	"context"
	"encoding/json"
	"net"
	"strings"
	"testing"
)

// net.Pipe rejects deadline updates after its peer closes, unlike the Unix
// stream used in production. Split the first reply read so the server's write
// stays blocked until the client has installed its short frame deadline.
type installedFirstReadConn struct {
	net.Conn
	read bool
}

func (c *installedFirstReadConn) Read(b []byte) (int, error) {
	if !c.read && len(b) > 0 {
		c.read = true
		b = b[:1]
	}
	return c.Conn.Read(b)
}

func installedMemoryExchange(t *testing.T, core InstalledControl, c InstalledCredentials, i InstalledIntent) InstalledClientResult {
	t.Helper()
	data, e := installedIntentFrame(c, i)
	if e != nil {
		t.Fatal("intent")
	}
	a, b := net.Pipe()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); installedServeConnection(ctx, a, core) }()
	defer func() { cancel(); a.Close(); b.Close(); <-done }()
	result, e := installedClientExchange(ctx, &installedFirstReadConn{Conn: b}, c, i, data)
	if e != nil {
		t.Fatal("in-memory exchange refused")
	}
	return result
}
func TestInstalledClientReceiptRecovery(t *testing.T) {
	core, r, calls := installedFixture(t, nil)
	c := InstalledCredentials{r.Run, r.Installation, r.Capability}
	arm := InstalledIntent{Kind: InstalledArm, ExpectedRun: c.Run, OperationID: "stable"}
	// Deliberately discard the first reply: this is synthetic lost delivery, NOT
	// proof of a backend transaction beyond the in-memory callback.
	data, e := installedIntentFrame(c, arm)
	if e != nil {
		t.Fatal("intent")
	}
	core.Dispatch(context.Background(), data)
	status := installedMemoryExchange(t, core, c, InstalledIntent{Kind: InstalledGetStatus, OperationID: "stable"})
	if status.Reply.ArmReceipt != "acknowledged" || status.Reply.Invitation != "" {
		t.Fatal("receipt recovery")
	}
	replay := installedMemoryExchange(t, core, c, arm)
	if replay.Reply.Code != "consumed" || replay.Reply.Invitation != "" || calls[3] != 1 {
		t.Fatal("arm replay")
	}
	reprint := installedMemoryExchange(t, core, c, InstalledIntent{Kind: InstalledReprint})
	if reprint.Reply.Invitation != "synthetic-invitation" || calls[3] != 1 || calls[4] != 1 {
		t.Fatal("reprint rotated")
	}
}
func TestInstalledClientPublishConsent(t *testing.T) {
	core, r, calls := installedFixture(t, nil)
	c := InstalledCredentials{r.Run, r.Installation, r.Capability}
	p := installedMemoryExchange(t, core, c, InstalledIntent{Kind: InstalledGetProposal})
	if p.Reply.Proposal == nil {
		t.Fatal("proposal")
	}
	consent := &InstalledConsent{*p.Reply.Proposal, true, true, true, true, true, true}
	data, e := installedIntentFrame(c, InstalledIntent{Kind: InstalledPublish, Consent: consent})
	if e != nil {
		t.Fatal("publish intent")
	}
	var got InstalledRequest
	if json.Unmarshal(data, &got) != nil || got.Consent == nil || *got.Consent != *consent {
		t.Fatal("consent altered")
	}
	stale := *consent
	stale.Proposal.Run = strings.Repeat("f", 32)
	if _, e = installedIntentFrame(c, InstalledIntent{Kind: InstalledPublish, Consent: &stale}); e == nil {
		t.Fatal("stale publish")
	}
	reply := installedMemoryExchange(t, core, c, InstalledIntent{Kind: InstalledPublish, Consent: consent})
	if reply.Reply.Publication != "acknowledged" || calls[0] != 1 {
		t.Fatal("publish")
	}
}
func TestInstalledClientOperationShapes(t *testing.T) {
	_, r, _ := installedFixture(t, nil)
	c := InstalledCredentials{r.Run, r.Installation, r.Capability}
	for _, tc := range []struct {
		name, data string
		kind       InstalledIntentKind
	}{
		{"status-secret", `{"code":"ok","publication":"disabled","invitation":"secret"}`, InstalledGetStatus},
		{"publish-secret", `{"code":"ok","publication":"acknowledged","invitation":"secret"}`, InstalledPublish},
		{"arm-missing-receipt", `{"code":"ok","publication":"disabled","invitation":"secret"}`, InstalledArm},
		{"status-wrong-code", `{"code":"consent_refused"}`, InstalledGetStatus},
		{"status-missing-fields", `{"code":"ok","publication":"disabled","status":{"registered":true}}`, InstalledGetStatus},
		{"nested-case", `{"code":"ok","publication":"disabled","status":{"Registered":true,"invitation_available":false,"quarantined":false}}`, InstalledGetStatus},
		{"unknown-code", `{"code":"private server diagnostic"}`, InstalledReprint},
	} {
		t.Run(tc.name, func(t *testing.T) {
			reply, e := installedDecodeReply([]byte(tc.data), InstalledIntent{Kind: tc.kind}, c)
			if e == nil || reply.Invitation != "" {
				t.Fatal("shape accepted")
			}
		})
	}
}
