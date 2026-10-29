//go:build linux || darwin

package localcontrol

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

func TestInstalledClientRawBounds(t *testing.T) {
	core, r, _ := installedFixture(t, nil)
	credentials := InstalledCredentials{r.Run, r.Installation, r.Capability}
	original := *installedConsent(t, core, r)
	for _, name := range []string{"selection", "node", "profile", "attempt", "installation", "origin", "proposal-id", "run", "padded-selection", "padded-operation", "escaped-overflow", "exact"} {
		t.Run(name, func(t *testing.T) {
			consent := original
			p := &consent.Proposal
			intent := InstalledIntent{Kind: InstalledPublish, Consent: &consent}
			large := strings.Repeat("x", 513)
			switch name {
			case "selection":
				p.Selection = large
			case "node":
				p.Node = large
			case "profile":
				p.Profile = large
			case "attempt":
				p.Attempt = large
			case "installation":
				p.Installation = large
			case "origin":
				p.Origin = large
			case "proposal-id":
				p.ID = large
			case "run":
				p.Run = large
			case "padded-selection":
				p.Selection = strings.Repeat(" ", 128) + "x"
			case "padded-operation":
				intent = InstalledIntent{Kind: InstalledArm, ExpectedRun: r.Run, OperationID: strings.Repeat(" ", 128) + "x"}
			case "escaped-overflow":
				p.Selection = strings.Repeat("<", 128)
				p.Node = p.Selection
				p.Profile = p.Selection
				p.Attempt = p.Selection
				p.Origin = strings.Repeat("<", 512)
				if !installedConsentBounded(&consent) {
					t.Fatal("fixture must pass raw bound")
				}
			case "exact":
				p.Selection = " selection " // Preserve permitted raw padding, never normalize.
			}
			data, err := installedIntentFrame(credentials, intent)
			if name != "exact" {
				if err == nil || data != nil {
					t.Fatal("oversize accepted")
				}
				return
			}
			var decoded InstalledRequest
			if err != nil || json.Unmarshal(data, &decoded) != nil || decoded.Consent == nil || *decoded.Consent != consent {
				t.Fatal("exact consent changed")
			}
		})
	}
}

func TestInstalledClientInvalidLifetime(t *testing.T) {
	for _, name := range []string{"nil-context", "cancelled", "zero-client"} {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			if name == "nil-context" {
				ctx = nil
			}
			if name == "cancelled" {
				var cancel context.CancelFunc
				ctx, cancel = context.WithCancel(ctx)
				cancel()
			}
			result, err := (InstalledClient{}).Do(ctx, InstalledIntent{Kind: InstalledPublish, Consent: &InstalledConsent{Proposal: InstalledProposal{InstalledSelection: InstalledSelection{Selection: strings.Repeat("x", 513)}}}})
			if err == nil || result.Transmission != InstalledNotSent || result.Reply.Invitation != "" {
				t.Fatal("invalid lifetime/client accepted")
			}
		})
	}
}
