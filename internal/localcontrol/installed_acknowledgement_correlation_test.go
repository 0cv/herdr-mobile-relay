package localcontrol

import (
	"context"
	"testing"

	"github.com/0cv/herdr-mobile-relay/internal/tailscalecli"
)

func TestInstalledAcknowledgementCorrelationData(t *testing.T) {
	pin := [32]byte{1}
	p := InstalledProposal{InstalledSelection: InstalledSelection{"opaque-random-reference", "node", "profile", "https://synthetic.invalid", 443, 8080, "attempt"}, Run: "run", Installation: "installation"}
	d := tailscalecli.PublicationSnapshot{Selection: pin, Profile: p.Profile, Generation: p.Run, Installation: p.Installation, Attempt: p.Attempt, Node: p.Node, Origin: p.Origin, HTTPSPort: 443, BackendPort: 8080}
	if !publicationMatchesProposal(d, p, pin) {
		t.Fatal("matching data refused")
	}
	for _, field := range []string{"selection", "profile", "generation", "installation", "attempt", "node", "origin", "https", "backend", "missing-pin"} {
		t.Run(field, func(t *testing.T) {
			bad := d
			expected := pin
			switch field {
			case "selection":
				bad.Selection[0]++
			case "profile":
				bad.Profile = "foreign"
			case "generation":
				bad.Generation = "foreign"
			case "installation":
				bad.Installation = "foreign"
			case "attempt":
				bad.Attempt = "foreign"
			case "node":
				bad.Node = "foreign"
			case "origin":
				bad.Origin = "https://foreign.invalid"
			case "https":
				bad.HTTPSPort++
			case "backend":
				bad.BackendPort++
			case "missing-pin":
				expected = [32]byte{}
			}
			if publicationMatchesProposal(bad, p, expected) {
				t.Fatal("foreign data accepted")
			}
		})
	}
}

func TestInstalledAcknowledgementConstructionPins(t *testing.T) {
	base, r, _ := installedFixture(t, nil)
	selection := base.state.proposal.InstalledSelection
	for _, kind := range []string{"callback-only", "pin-only", "strict-both", "strict-pin", "strict-callback"} {
		t.Run(kind, func(t *testing.T) {
			b := base.state.backend
			if kind != "pin-only" && kind != "strict-pin" {
				b.AcknowledgedPublication = func(context.Context) (tailscalecli.AcknowledgedPublication, error) {
					t.Error("construction invoked callback")
					return tailscalecli.AcknowledgedPublication{}, nil
				}
			}
			if kind != "callback-only" && kind != "strict-callback" {
				b.PublicationSelection = [32]byte{1}
			}
			var c InstalledControl
			var err error
			s := selection
			if kind == "strict-both" || kind == "strict-pin" || kind == "strict-callback" {
				b.Publish = nil
				s.Attempt = ""
				c, _, err = NewInstalledResumeControl(r.Installation, s, b)
			} else {
				c, _, err = NewInstalledControl(r.Installation, s, b)
			}
			defer c.Retire()
			if err == nil {
				t.Fatal("mixed/strict configuration accepted")
			}
		})
	}
	r.Op = "acknowledged-publication"
	if installedCall(t, base, context.Background(), r).Code != "invalid_request" {
		t.Fatal("wire getter exposed")
	}
}
