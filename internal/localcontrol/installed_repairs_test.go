package localcontrol

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

func installedRepairBackend(calls *[5]int) InstalledBackend {
	return InstalledBackend{
		Publish: func(context.Context) error { calls[0]++; return nil },
		Status:  func(context.Context) (InstalledObservation, error) { calls[1]++; return InstalledObservation{}, nil },
		Admit:   func(context.Context) error { calls[2]++; return nil },
		Arm:     func(context.Context) (string, error) { calls[3]++; return "synthetic-invitation", nil },
		Reprint: func(context.Context) (string, error) { calls[4]++; return "synthetic-invitation", nil },
	}
}

func installedRepairRequest(k InstalledCredentials) InstalledRequest {
	return InstalledRequest{Protocol: InstalledProtocol, Run: k.Run, Installation: k.Installation, Capability: k.Capability, Op: "status"}
}

func TestInstalledControlEscapedConfirmationBoundary(t *testing.T) {
	// Compute the default encoding independently with fixed-length synthetic IDs.
	size := func(value, origin string) int {
		p := InstalledProposal{InstalledSelection: InstalledSelection{value, value, value, origin, 443, 8080, value}, Run: strings.Repeat("0", 32), Installation: value, ID: strings.Repeat("0", 32)}
		r := InstalledRequest{Protocol: InstalledProtocol, Run: p.Run, Installation: value, Capability: strings.Repeat("0", 64), Op: "publish", Consent: &InstalledConsent{p, true, true, true, true, true, true}}
		raw, err := json.Marshal(r)
		if err != nil {
			t.Fatal("encoding failed")
		}
		return len(raw)
	}
	origin := "https://synthetic.invalid"
	n := 128
	for size(strings.Repeat("<", n), origin) > MaxRequestBytes {
		n--
	}
	value := strings.Repeat("<", n)
	origin = "https://" + strings.Repeat("a", MaxRequestBytes-size(value, origin)) + "synthetic.invalid"
	if size(value, origin) != MaxRequestBytes {
		t.Fatal("boundary fixture")
	}
	var calls [5]int
	b := installedRepairBackend(&calls)
	selection := InstalledSelection{value, value, value, origin, 443, 8080, value}
	c, k, err := NewInstalledControl(value, selection, b)
	if err != nil {
		t.Fatal("exact boundary refused")
	}
	t.Cleanup(c.Retire)
	r := installedRepairRequest(k)
	r.Consent = installedConsent(t, c, r)
	r.Op = "publish"
	raw, err := json.Marshal(r)
	if err != nil || len(raw) != MaxRequestBytes {
		t.Fatal("offered confirmation encoding bound")
	}
	if calls != [5]int{} {
		t.Fatal("proposal mutated backend")
	}
	if got := installedCall(t, c, context.Background(), r); got.Code != "ok" || got.Publication != "acknowledged" || calls[0] != 1 {
		t.Fatal("boundary round trip failed")
	}
	// One byte over and the parent's all-128 escaped counterexample both refuse.
	selection.Origin = strings.Replace(origin, "https://", "https://a", 1)
	for _, s := range []InstalledSelection{selection, {strings.Repeat("<", 128), strings.Repeat("<", 128), strings.Repeat("<", 128), "https://synthetic.invalid", 443, 8080, strings.Repeat("<", 128)}} {
		installation := value
		if s.Origin == "https://synthetic.invalid" {
			installation = strings.Repeat("<", 128)
		}
		before := calls
		refused, credentials, e := NewInstalledControl(installation, s, b)
		if e == nil {
			refused.Retire()
			t.Fatal("oversized confirmation accepted")
		}
		if e.Error() != "installed control configuration refused" || refused.state != nil || credentials != (InstalledCredentials{}) || calls != before {
			t.Fatal("refusal leaked authority or mutated backend")
		}
	}
}

func TestInstalledControlResumePairingAndObservation(t *testing.T) {
	var calls [5]int
	b := installedRepairBackend(&calls)
	b.Publish = nil
	denied := true
	b.Admit = func(context.Context) error {
		calls[2]++
		if denied {
			return errors.New("synthetic-secret")
		}
		return nil
	}
	observation := InstalledObservation{}
	b.Status = func(context.Context) (InstalledObservation, error) { calls[1]++; return observation, nil }
	c, k, err := NewInstalledResumeControl("installation", InstalledSelection{"selection", "node", "profile", "https://synthetic.invalid", 443, 8080, ""}, b)
	if err != nil {
		t.Fatal("resume refused")
	}
	t.Cleanup(c.Retire)
	r := installedRepairRequest(k)
	for _, observed := range []InstalledObservation{{}, {Registered: true, InvitationAvailable: true}, {Quarantined: true}} {
		observation = observed
		got := installedCall(t, c, context.Background(), r)
		if got.Code != "ok" || got.Status == nil || *got.Status != observed || got.Publication != "disabled" || got.Invitation != "" {
			t.Fatal("synthetic resume qualification")
		}
	}
	if calls != [5]int{0, 3, 0, 0, 0} {
		t.Fatal("status side effect")
	}
	for _, op := range []string{"arm", "reprint"} {
		r.Op = op
		r.OperationID = ""
		if op == "arm" {
			r.OperationID = "one"
		}
		got := installedCall(t, c, context.Background(), r)
		if got.Code != "admission_refused" || got.Invitation != "" {
			t.Fatal("admission escaped")
		}
	}
	if calls[3] != 0 || calls[4] != 0 {
		t.Fatal("denied secret callback")
	}
	denied = false
	// Reprint before arm reads the existing synthetic invitation, never creates one.
	if got := installedCall(t, c, context.Background(), r); got.Invitation != "synthetic-invitation" || calls[3] != 0 || calls[4] != 1 {
		t.Fatal("reprint mutated")
	}
	r.Op = "arm"
	r.OperationID = "one"
	if installedCall(t, c, context.Background(), r).Invitation != "synthetic-invitation" {
		t.Fatal("arm refused")
	}
	if got := installedCall(t, c, context.Background(), r); got.Code != "consumed" || got.Invitation != "" {
		t.Fatal("arm replay")
	}
	r.Op = "status"
	if got := installedCall(t, c, context.Background(), r); got.ArmReceipt != "acknowledged" || got.Invitation != "" {
		t.Fatal("receipt missing")
	}
	if calls[0] != 0 || calls[2] != 5 || calls[3] != 1 || calls[4] != 1 {
		t.Fatal("resume callback accounting")
	}
}

func TestInstalledControlResumeModeAndLifetimeRefusals(t *testing.T) {
	var calls [5]int
	b := installedRepairBackend(&calls)
	b.Publish = nil
	s := InstalledSelection{"selection", "node", "profile", "https://synthetic.invalid", 443, 8080, ""}
	c, k, err := NewInstalledResumeControl("installation", s, b)
	if err != nil {
		t.Fatal("resume refused")
	}
	t.Cleanup(c.Retire)
	r := installedRepairRequest(k)
	for _, op := range []string{"proposal", "publish"} {
		r.Op = op
		if op == "publish" {
			r.Consent = &InstalledConsent{c.state.proposal, true, true, true, true, true, true}
		}
		got := installedCall(t, c, context.Background(), r)
		if got.Code != "publication_disabled" || got.Proposal != nil || got.Publication != "disabled" {
			t.Fatal("resume offered publication")
		}
	}
	r.Op = "status"
	r.Consent = nil
	raw, _ := json.Marshal(r)
	for _, extra := range []string{`,"mode":"setup"`, `,"publish":true`, `,"attempt":"attempt"`} {
		bad := append([]byte(nil), raw[:len(raw)-1]...)
		bad = append(bad, []byte(extra+"}")...)
		var got InstalledReply
		if json.Unmarshal(c.Dispatch(context.Background(), bad), &got) != nil || got.Code != "invalid_request" {
			t.Fatal("wire mode upgrade")
		}
	}
	for _, op := range []string{"publish", "arm"} {
		bad := r
		bad.Op = op
		if installedCall(t, c, context.Background(), bad).Code != "invalid_request" {
			t.Fatal("shape accepted")
		}
	}
	next, _, err := NewInstalledResumeControl("installation", s, b)
	if err != nil {
		t.Fatal("next resume refused")
	}
	t.Cleanup(next.Retire)
	if installedCall(t, next, context.Background(), r).Code != "unauthorized" {
		t.Fatal("old generation accepted")
	}
	retained := c
	c.Retire()
	if installedCall(t, retained, context.Background(), r).Code != "retired" {
		t.Fatal("retired copy accepted")
	}
	if calls != [5]int{} {
		t.Fatal("refusal dispatched callback")
	}
}

func TestInstalledControlConstructionModeRefusals(t *testing.T) {
	var calls [5]int
	b := installedRepairBackend(&calls)
	base := InstalledSelection{"selection", "node", "profile", "https://synthetic.invalid", 443, 8080, ""}
	// Both constructors reject mixed capabilities; setup still requires both.
	for _, resume := range []bool{false, true} {
		for _, attempt := range []string{"", "attempt"} {
			for _, publish := range []bool{false, true} {
				if (resume && attempt == "" && !publish) || (!resume && attempt != "" && publish) {
					continue
				}
				s := base
				s.Attempt = attempt
				backend := b
				if !publish {
					backend.Publish = nil
				}
				constructor := NewInstalledControl
				if resume {
					constructor = NewInstalledResumeControl
				}
				c, k, err := constructor("installation", s, backend)
				if err == nil {
					c.Retire()
					t.Fatal("mixed construction accepted")
				}
				if c.state != nil || k != (InstalledCredentials{}) || err.Error() != "installed control configuration refused" {
					t.Fatal("construction refusal leaked")
				}
			}
		}
	}
	for _, change := range []func(*InstalledSelection, *InstalledBackend){
		func(s *InstalledSelection, b *InstalledBackend) { s.Node = "" },
		func(s *InstalledSelection, b *InstalledBackend) { s.Origin = "http://synthetic.invalid" },
		func(s *InstalledSelection, b *InstalledBackend) { s.RelayPort = 0 },
		func(s *InstalledSelection, b *InstalledBackend) { b.Status = nil },
		func(s *InstalledSelection, b *InstalledBackend) { b.Admit = nil },
		func(s *InstalledSelection, b *InstalledBackend) { b.Arm = nil },
		func(s *InstalledSelection, b *InstalledBackend) { b.Reprint = nil },
	} {
		s := base
		backend := b
		backend.Publish = nil
		change(&s, &backend)
		c, _, err := NewInstalledResumeControl("installation", s, backend)
		if err == nil {
			c.Retire()
			t.Fatal("invalid resume shape accepted")
		}
	}
	if calls != [5]int{} {
		t.Fatal("constructor called backend")
	}
}
