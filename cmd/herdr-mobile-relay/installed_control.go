package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"

	"github.com/0cv/herdr-mobile-relay/internal/config"
	"github.com/0cv/herdr-mobile-relay/internal/localcontrol"
)

var errControlUsage = errors.New("invalid installed-control invocation or consent")
var errControlRefused = errors.New("installed-control refused")
var errControlOutput = errors.New("installed-control output failed; delivery may be partial")

type installedControlClient interface {
	Do(context.Context, localcontrol.InstalledIntent) (localcontrol.InstalledClientResult, error)
}

func controlHex(s string) bool {
	if len(s) != 32 {
		return false
	}
	for _, c := range s {
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
			return false
		}
	}
	return true
}
func controlIdentity(s string) bool {
	if len(s) == 0 || len(s) > 128 {
		return false
	}
	for _, c := range s {
		if c < 0x21 || c > 0x7e || c == '/' || c == '\\' {
			return false
		}
	}
	return true
}
func parseInstalledControl(args []string) (localcontrol.InstalledIntent, error) {
	var i localcontrol.InstalledIntent
	if len(args) == 0 {
		return i, errControlUsage
	}
	i.Kind = localcontrol.InstalledIntentKind(args[0])
	switch i.Kind {
	case localcontrol.InstalledGetProposal, localcontrol.InstalledGetStatus, localcontrol.InstalledPublish, localcontrol.InstalledArm, localcontrol.InstalledReprint:
	default:
		return i, errControlUsage
	}
	seen := map[string]bool{}
	for n := 1; n < len(args); n += 2 {
		if n+1 >= len(args) || seen[args[n]] {
			return i, errControlUsage
		}
		seen[args[n]] = true
		switch args[n] {
		case "--run":
			if !controlHex(args[n+1]) {
				return i, errControlUsage
			}
			i.ExpectedRun = args[n+1]
		case "--operation-id":
			if (i.Kind != localcontrol.InstalledArm && i.Kind != localcontrol.InstalledGetStatus) || !controlIdentity(args[n+1]) {
				return i, errControlUsage
			}
			i.OperationID = args[n+1]
		default:
			return i, errControlUsage
		}
	}
	if (i.Kind != localcontrol.InstalledGetProposal && i.Kind != localcontrol.InstalledGetStatus || i.OperationID != "") && i.ExpectedRun == "" {
		return i, errControlUsage
	}
	if i.Kind == localcontrol.InstalledArm && i.OperationID == "" {
		return i, errControlUsage
	}
	return i, nil
}

// Explicit v1 projection: no request, configuration, credentials or error strings.
type installedControlOutput struct {
	Version      int                                `json:"version"`
	Operation    localcontrol.InstalledIntentKind   `json:"operation"`
	Run          string                             `json:"run,omitempty"`
	Installation string                             `json:"installation,omitempty"`
	OperationID  string                             `json:"operation_id,omitempty"`
	Transmission localcontrol.InstalledTransmission `json:"transmission"`
	Code         string                             `json:"code"`
	Publication  string                             `json:"publication,omitempty"`
	ArmReceipt   string                             `json:"arm_receipt,omitempty"`
	Proposal     *localcontrol.InstalledProposal    `json:"proposal,omitempty"`
	Status       *localcontrol.InstalledObservation `json:"status,omitempty"`
	Invitation   string                             `json:"invitation,omitempty"`
}

func controlOneOf(s string, values ...string) bool {
	for _, v := range values {
		if s == v {
			return true
		}
	}
	return false
}
func projectInstalledControl(i localcontrol.InstalledIntent, installation string, r localcontrol.InstalledClientResult, requestErr error) (installedControlOutput, int) {
	o := installedControlOutput{Version: 1, Operation: i.Kind, Run: i.ExpectedRun, OperationID: i.OperationID, Transmission: localcontrol.InstalledNotSent, Code: "refused"}
	if controlIdentity(installation) {
		o.Installation = installation
	}
	if r.Transmission == localcontrol.InstalledNotSent {
		return o, 1
	}
	o.Transmission = localcontrol.InstalledPossibleDispatch
	o.Code = "uncertain"
	if requestErr != nil || r.Transmission != localcontrol.InstalledValidatedReply || r.Run != i.ExpectedRun || r.Installation != installation {
		return o, 3
	}
	p := r.Reply
	if !controlOneOf(p.Code, "ok", "unavailable", "invalid_request", "unauthorized", "retired", "busy", "cancelled", "result_refused", "publication_disabled", "consumed", "consent_refused", "readiness_unavailable", "outcome_unknown", "backend_refused", "admission_refused", "receipt_capacity") || !controlOneOf(p.Publication, "", "disabled", "not_dispatched", "dispatched_unknown", "acknowledged") || !controlOneOf(p.ArmReceipt, "", "dispatched_unknown", "acknowledged") {
		return o, 3
	}
	o.Transmission = localcontrol.InstalledValidatedReply
	o.Code = p.Code
	o.Publication = p.Publication
	if i.Kind == localcontrol.InstalledArm || i.Kind == localcontrol.InstalledGetStatus && i.OperationID != "" {
		o.ArmReceipt = p.ArmReceipt
	}
	if p.Code != "ok" {
		return o, 1
	}
	valid := p.Publication != ""
	switch i.Kind {
	case localcontrol.InstalledGetProposal:
		valid = valid && p.Publication == "not_dispatched" && p.Proposal != nil
		if valid {
			valid = controlProposal(*p.Proposal) && p.Proposal.Run == i.ExpectedRun && p.Proposal.Installation == installation
		}
		if valid {
			v := *p.Proposal
			o.Proposal = &v
		}
	case localcontrol.InstalledGetStatus:
		valid = valid && p.Status != nil
		if valid {
			v := *p.Status
			o.Status = &v
		}
	case localcontrol.InstalledPublish:
		valid = valid && p.Publication == "acknowledged"
	case localcontrol.InstalledArm, localcontrol.InstalledReprint:
		valid = valid && len(p.Invitation) > 0 && len(p.Invitation) <= 4096 && (i.Kind != localcontrol.InstalledArm || p.ArmReceipt == "acknowledged")
		if valid {
			o.Invitation = p.Invitation
		}
	}
	if !valid {
		o.Code = "refused"
		return o, 1
	}
	return o, 0
}
func writeInstalledControl(w io.Writer, o installedControlOutput, exit int) (int, error) {
	b, e := json.Marshal(o)
	if e != nil || len(b) > 8192 {
		return 1, errControlOutput
	}
	b = append(b, '\n')
	n, e := w.Write(b)
	if e != nil || n != len(b) {
		return 1, errControlOutput
	}
	return exit, nil
}
func runInstalledControl(args []string, in io.Reader, out io.Writer) (int, error) {
	i, e := parseInstalledControl(args)
	if e != nil {
		return 2, errControlUsage
	}
	cfg, e := config.LoadInstalled()
	if e != nil {
		return 1, errControlRefused
	}
	selected, e := config.BindInstalledCLI(cfg)
	if e != nil {
		return 1, errControlRefused
	}
	if !installedControlEnabled() {
		return 1, errControlRefused
	}
	if i.Kind == localcontrol.InstalledPublish {
		c, e := readInstalledConsent(in)
		if e != nil || c.Proposal.Run != i.ExpectedRun {
			return 2, errControlUsage
		}
		i.Consent = &c
	}
	// Generic stdin has completed before acquiring signal/request resources.
	ctx, stop := installedCommandContext()
	r := localcontrol.InstalledClientResult{Transmission: localcontrol.InstalledNotSent}
	if i.ExpectedRun == "" {
		i.ExpectedRun, e = installedControlDiscover(ctx, selected)
		if !controlHex(i.ExpectedRun) {
			i.ExpectedRun = ""
			e = errControlRefused
		}
	}
	if e == nil {
		var client installedControlClient
		client, e = installedControlNew(ctx, selected, i.ExpectedRun)
		if e == nil && client != nil {
			r, e = client.Do(ctx, i)
		} else {
			e = errControlRefused
		}
	}
	if ctx.Err() != nil {
		e = errControlRefused
	}
	// Freeze after concrete Do cleanup, BEFORE stop (which itself cancels).
	o, exit := projectInstalledControl(i, cfg.InstanceID, r, e)
	stop()
	// Prior signal behavior is restored before possibly blocking generic stdout.
	return writeInstalledControl(out, o, exit)
}
