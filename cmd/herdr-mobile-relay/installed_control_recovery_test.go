//go:build herdr_installed_inert_test

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/0cv/herdr-mobile-relay/internal/config"
	"github.com/0cv/herdr-mobile-relay/internal/localcontrol"
)

// Synthetic admission only: no concrete ownership, runtime, native or HTTPS claim.
func TestInstalledControlMemoryRecovery(t *testing.T) {
	selected := controlFixture(t)
	cfg := selected.Snapshot()
	publishes, arms, reprints, dispatches, discoveries := 0, 0, 0, 0, 0
	newCore := func() (localcontrol.InstalledControl, localcontrol.InstalledCredentials) {
		core, c, e := localcontrol.NewInstalledControl(cfg.InstanceID, controlTestConsent().Proposal.InstalledSelection, localcontrol.InstalledBackend{
			Publish: func(context.Context) error { publishes++; return nil },
			Status: func(context.Context) (localcontrol.InstalledObservation, error) {
				return localcontrol.InstalledObservation{Registered: true, InvitationAvailable: arms > 0}, nil
			},
			Admit:   func(context.Context) error { return nil },
			Arm:     func(context.Context) (string, error) { arms++; return "MEMORY-INVITATION", nil },
			Reprint: func(context.Context) (string, error) { reprints++; return "MEMORY-INVITATION", nil },
		})
		if e != nil {
			t.Fatal(e)
		}
		t.Cleanup(core.Retire)
		return core, c
	}
	core, credentials := newCore()
	lose := false
	race := ""
	installedControlFixtureDiscover = func(context.Context, config.InstalledConfig) (string, error) {
		discoveries++
		run := credentials.Run
		if race == "discovery" {
			core.Retire()
			core, credentials = newCore()
		}
		return run, nil
	}
	installedControlFixtureNew = func(ctx context.Context, s config.InstalledConfig, run string) (installedControlClient, error) {
		if run != credentials.Run {
			return nil, errControlRefused
		}
		pinned := credentials
		if race == "do" {
			core.Retire()
			core, credentials = newCore()
		}
		return controlScript(func(ctx context.Context, i localcontrol.InstalledIntent) (localcontrol.InstalledClientResult, error) {
			if pinned != credentials || i.ExpectedRun != pinned.Run {
				return localcontrol.InstalledClientResult{Transmission: localcontrol.InstalledNotSent}, errControlRefused
			}
			request := localcontrol.InstalledRequest{Protocol: localcontrol.InstalledProtocol, Run: pinned.Run, Installation: pinned.Installation, Capability: pinned.Capability, Op: string(i.Kind), OperationID: i.OperationID, Consent: i.Consent}
			b, _ := json.Marshal(request)
			dispatches++
			var reply localcontrol.InstalledReply
			if json.Unmarshal(core.Dispatch(ctx, b), &reply) != nil {
				t.Fatal("typed core reply")
			}
			if lose {
				return localcontrol.InstalledClientResult{Transmission: localcontrol.InstalledPossibleDispatch}, errors.New("lost reply canary")
			}
			return localcontrol.InstalledClientResult{Transmission: localcontrol.InstalledValidatedReply, Run: pinned.Run, Installation: pinned.Installation, Reply: reply}, nil
		}), nil
	}
	invoke := func(args []string, input []byte) (installedControlOutput, int) {
		t.Helper()
		var out bytes.Buffer
		code, e := runInstalledControl(args, bytes.NewReader(input), &out)
		if e != nil {
			t.Fatal(e)
		}
		var result installedControlOutput
		if json.Unmarshal(out.Bytes(), &result) != nil {
			t.Fatal("projection")
		}
		return result, code
	}
	var proposal localcontrol.InstalledProposal
	t.Run("proposal", func(t *testing.T) {
		o, code := invoke([]string{"proposal"}, nil)
		if code != 0 || o.Proposal == nil {
			t.Fatal("proposal")
		}
		proposal = *o.Proposal
	})
	t.Run("publish", func(t *testing.T) {
		c := controlTestConsent()
		c.Proposal = proposal
		b, _ := json.Marshal(c)
		_, code := invoke([]string{"publish", "--run", proposal.Run}, b)
		if code != 0 || publishes != 1 {
			t.Fatal("publish")
		}
	})
	t.Run("lost-arm", func(t *testing.T) {
		lose = true
		o, code := invoke([]string{"arm", "--run", proposal.Run, "--operation-id", "stable"}, nil)
		lose = false
		if code != 3 || arms != 1 || o.Invitation != "" || o.OperationID != "stable" {
			t.Fatal("lost arm retried or disclosed")
		}
	})
	t.Run("receipt", func(t *testing.T) {
		o, code := invoke([]string{"status", "--run", proposal.Run, "--operation-id", "stable"}, nil)
		if code != 0 || o.ArmReceipt != "acknowledged" || o.Invitation != "" || arms != 1 {
			t.Fatal("receipt")
		}
	})
	t.Run("replay", func(t *testing.T) {
		o, code := invoke([]string{"arm", "--run", proposal.Run, "--operation-id", "stable"}, nil)
		if code == 0 || o.Code != "consumed" || o.Invitation != "" || arms != 1 {
			t.Fatal("replayed arm")
		}
	})
	t.Run("reprint", func(t *testing.T) {
		o, code := invoke([]string{"reprint", "--run", proposal.Run}, nil)
		if code != 0 || o.Invitation != "MEMORY-INVITATION" || arms != 1 || reprints != 1 {
			t.Fatal("reprint changed invitation")
		}
	})
	t.Run("restart-old-pins", func(t *testing.T) {
		core.Retire()
		core, credentials = newCore()
		before := dispatches
		for _, args := range [][]string{{"arm", "--run", proposal.Run, "--operation-id", "stable"}, {"status", "--run", proposal.Run, "--operation-id", "stable"}, {"reprint", "--run", proposal.Run}} {
			o, code := invoke(args, nil)
			if code != 1 || o.Transmission != localcontrol.InstalledNotSent {
				t.Fatal("stale pin")
			}
		}
		if dispatches != before {
			t.Fatal("stale dispatch")
		}
	})
	t.Run("new-generation-missing-receipt", func(t *testing.T) {
		o, code := invoke([]string{"status", "--run", credentials.Run, "--operation-id", "stable"}, nil)
		if code != 0 || o.ArmReceipt != "" || o.Transmission != localcontrol.InstalledValidatedReply {
			t.Fatal("unknown receipt")
		}
	})
	for _, phase := range []string{"discovery", "do"} {
		t.Run("generation-race-"+phase, func(t *testing.T) {
			race = phase
			before, d := dispatches, discoveries
			o, code := invoke([]string{"status"}, nil)
			race = ""
			if code != 1 || o.Transmission != localcontrol.InstalledNotSent || dispatches != before || discoveries != d+1 {
				t.Fatal("generation refresh/retry")
			}
		})
	}
	if publishes != 1 || arms != 1 || reprints != 1 {
		t.Fatal("automatic mutation")
	}
}
func TestInstalledControlInputBeforeRequest(t *testing.T) {
	controlFixture(t)
	calls := 0
	installedFixtureContext = nil
	installedControlFixtureNew = func(context.Context, config.InstalledConfig, string) (installedControlClient, error) {
		calls++
		return nil, errControlRefused
	}
	for _, name := range []string{"malformed", "run-mismatch"} {
		t.Run(name, func(t *testing.T) {
			s := "{}"
			if name == "run-mismatch" {
				c := controlTestConsent()
				c.Proposal.Run = strings.Repeat("a", 32)
				b, _ := json.Marshal(c)
				s = string(b)
			}
			var out bytes.Buffer
			code, e := runInstalledControl([]string{"publish", "--run", controlTestRun}, strings.NewReader(s), &out)
			if code != 2 || e != errControlUsage || calls != 0 || out.Len() != 0 {
				t.Fatal("input reached request context")
			}
		})
	}
}
