//go:build linux || darwin

package localcontrol

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/0cv/herdr-mobile-relay/internal/config"
	"golang.org/x/sys/unix"
)

type InstalledIntentKind string

const (
	InstalledGetProposal InstalledIntentKind = "proposal"
	InstalledGetStatus   InstalledIntentKind = "status"
	InstalledPublish     InstalledIntentKind = "publish"
	InstalledArm         InstalledIntentKind = "arm"
	InstalledReprint     InstalledIntentKind = "reprint"
)

// Arm requires ExpectedRun AND a stable OperationID even on a new client.
// Publish requires the exact proposal generation. There are no automatic retries.
type InstalledIntent struct {
	Kind        InstalledIntentKind
	ExpectedRun string
	OperationID string
	Consent     *InstalledConsent
}

func (InstalledIntent) String() string     { return "installed intent [redacted]" }
func (i InstalledIntent) GoString() string { return i.String() }

type InstalledTransmission string

const (
	InstalledNotSent          InstalledTransmission = "not_started"
	InstalledPossibleDispatch InstalledTransmission = "possible_dispatch_lost_reply"
	InstalledValidatedReply   InstalledTransmission = "validated_reply"
)

type InstalledClientResult struct {
	Transmission      InstalledTransmission
	Run, Installation string
	Reply             InstalledReply
}

func (InstalledClientResult) String() string     { return "installed client result [redacted]" }
func (r InstalledClientResult) GoString() string { return r.String() }

var errInstalledClient = errors.New("installed client refused")

// InstalledClient pins discovery once. It never refreshes after a restart.
// Neither a validated reply nor a missing receipt proves backend acknowledgement
// or absence. Status with the exact ID and reprint are explicit recovery tools.
type InstalledClient struct {
	selected config.InstalledConfig
	snapshot InstalledHandoffSnapshot
}

func (InstalledClient) String() string     { return "installed client [redacted]" }
func (c InstalledClient) GoString() string { return c.String() }
func NewInstalledClient(ctx context.Context, selected config.InstalledConfig, expectedRun string) (InstalledClient, error) {
	if !handoffHex(expectedRun, 32) {
		return InstalledClient{}, errInstalledClient
	}
	s, e := ReadInstalledHandoff(ctx, selected)
	if e != nil || s.credentials.Run != expectedRun {
		return InstalledClient{}, errInstalledClient
	}
	return InstalledClient{selected, s}, nil
}
func (c InstalledClient) Do(ctx context.Context, intent InstalledIntent) (result InstalledClientResult, err error) {
	result = InstalledClientResult{Transmission: InstalledNotSent}
	if ctx == nil || ctx.Err() != nil {
		return result, errInstalledClient
	}
	data, e := installedIntentFrame(c.snapshot.credentials, intent)
	if e != nil {
		return result, errInstalledClient
	}
	current, e := ReadInstalledHandoff(ctx, c.selected)
	if e != nil || current != c.snapshot {
		return result, errInstalledClient
	}
	var before, after, parent unix.Stat_t
	if unix.Lstat(filepath.Dir(current.socket), &parent) != nil || parent.Mode&unix.S_IFMT != unix.S_IFDIR || parent.Mode&07777 != 0700 || parent.Uid != uint32(os.Geteuid()) {
		return result, errInstalledClient
	}
	if unix.Lstat(current.socket, &before) != nil || before.Dev != parent.Dev || before.Mode&unix.S_IFMT != unix.S_IFSOCK || before.Mode&07777 != 0600 || before.Uid != uint32(os.Geteuid()) || before.Nlink != 1 {
		return result, errInstalledClient
	}
	dialCtx, cancel := context.WithTimeout(ctx, installedFrameTimeout)
	conn, e := (&net.Dialer{}).DialContext(dialCtx, "unix", current.socket)
	cancel()
	if e != nil {
		return result, errInstalledClient
	}
	defer func() {
		closeErr := conn.Close()
		if closeErr != nil || ctx.Err() != nil {
			result.Reply = InstalledReply{}
			if result.Transmission != InstalledNotSent {
				result.Transmission = InstalledPossibleDispatch
			}
			err = errInstalledClient
		}
	}()
	uc, ok := conn.(*net.UnixConn)
	if !ok || installedPeer(uc) != nil || unix.Lstat(current.socket, &after) != nil || handoffID(before) != handoffID(after) {
		return result, errInstalledClient
	}
	current, e = ReadInstalledHandoff(ctx, c.selected)
	if e != nil || current != c.snapshot {
		return result, errInstalledClient
	}
	return installedClientExchange(ctx, conn, c.snapshot.credentials, intent, data)
}
func installedIntentFrame(c InstalledCredentials, i InstalledIntent) ([]byte, error) {
	// Check raw lengths before validators that trim local copies or JSON escaping.
	if len(i.Kind) > 16 || len(i.ExpectedRun) > 32 || len(i.OperationID) > 128 || len(c.Installation) > 128 || len(c.Run) > 32 || len(c.Capability) > 64 || !handoffCredentials(c) {
		return nil, errInstalledClient
	}
	r := InstalledRequest{Protocol: InstalledProtocol, Run: c.Run, Installation: c.Installation, Capability: c.Capability, Op: string(i.Kind), Consent: i.Consent, OperationID: i.OperationID}
	if i.ExpectedRun != "" && i.ExpectedRun != c.Run {
		return nil, errInstalledClient
	}
	switch i.Kind {
	case InstalledPublish:
		if i.Consent == nil || !installedConsentBounded(i.Consent) || i.OperationID != "" || i.Consent.Proposal.Run != c.Run || i.Consent.Proposal.Installation != c.Installation {
			return nil, errInstalledClient
		}
	case InstalledArm:
		if i.ExpectedRun != c.Run || !validIdentity(i.OperationID) || i.Consent != nil {
			return nil, errInstalledClient
		}
	case InstalledGetStatus:
		if i.Consent != nil || (i.OperationID != "" && !validIdentity(i.OperationID)) {
			return nil, errInstalledClient
		}
	case InstalledGetProposal, InstalledReprint:
		if i.Consent != nil || i.OperationID != "" {
			return nil, errInstalledClient
		}
	default:
		return nil, errInstalledClient
	}
	b, e := json.Marshal(r)
	if e != nil || len(b) > MaxRequestBytes {
		return nil, errInstalledClient
	}
	return b, nil
}
func installedConsentBounded(c *InstalledConsent) bool {
	p := c.Proposal
	for _, s := range []string{p.Selection, p.Node, p.Profile, p.Attempt, p.Installation} {
		if len(s) > 128 || !validIdentity(s) {
			return false
		}
	}
	return len(p.Origin) <= 512 && len(p.Run) == 32 && len(p.ID) == 32
}

func installedClientExchange(ctx context.Context, c net.Conn, credentials InstalledCredentials, intent InstalledIntent, data []byte) (result InstalledClientResult, err error) {
	result.Transmission = InstalledNotSent
	stop := installedIOBridge(ctx, func() { _ = c.Close() })
	defer func() {
		stop()
		if ctx.Err() != nil {
			result.Reply = InstalledReply{}
			if result.Transmission != InstalledNotSent {
				result.Transmission = InstalledPossibleDispatch
			}
			err = errInstalledClient
		}
	}()
	if ctx.Err() != nil {
		return result, errInstalledClient
	}
	// Mark BEFORE the first Write: even a write error is not nondispatch proof.
	result.Transmission = InstalledPossibleDispatch
	if installedWriteFrame(c, data, MaxRequestBytes) != nil {
		return result, errInstalledClient
	}
	budget := StatusTimeout
	if intent.Kind == InstalledPublish {
		budget = ActivateTimeout
	}
	if intent.Kind == InstalledArm || intent.Kind == InstalledReprint {
		budget = ArmTimeout
	}
	if c.SetReadDeadline(time.Now().Add(budget+installedFrameTimeout)) != nil {
		return result, errInstalledClient
	}
	reader := bufio.NewReaderSize(c, 8194)
	if _, e := reader.ReadByte(); e != nil {
		return result, errInstalledClient
	}
	if reader.UnreadByte() != nil || c.SetReadDeadline(time.Now().Add(installedFrameTimeout)) != nil {
		return result, errInstalledClient
	}
	frame, e := installedReadFrame(reader, 8192)
	if e != nil || reader.Buffered() != 0 {
		return result, errInstalledClient
	}
	frame = append([]byte(nil), frame...)
	// The one-reply server closes the connection. Reject delayed trailing bytes
	// too; an idle peer is bounded by the short frame deadline above.
	if _, e = reader.ReadByte(); e != io.EOF {
		return result, errInstalledClient
	}
	reply, e := installedDecodeReply(frame, intent, credentials)
	if e != nil || ctx.Err() != nil {
		return result, errInstalledClient
	}
	result.Transmission = InstalledValidatedReply
	result.Run, result.Installation = credentials.Run, credentials.Installation
	result.Reply = reply
	return result, nil
}
func installedDecodeReply(data []byte, intent InstalledIntent, c InstalledCredentials) (InstalledReply, error) {
	fail := func() (InstalledReply, error) { return InstalledReply{}, errInstalledClient }
	var r InstalledReply
	if len(data) == 0 || len(data) > 8192 || decodeControlJSON(data, &r) != nil {
		return fail()
	}
	canonical, _ := json.Marshal(r)
	var actual, expected any
	if json.Unmarshal(data, &actual) != nil || json.Unmarshal(canonical, &expected) != nil || !installedExactShape(actual, expected) {
		return fail()
	}
	switch r.Code {
	case "ok", "unavailable", "invalid_request", "unauthorized", "retired", "busy", "cancelled", "result_refused", "publication_disabled", "consumed", "consent_refused", "readiness_unavailable", "outcome_unknown", "backend_refused", "admission_refused", "receipt_capacity":
	default:
		return fail()
	}
	switch r.Publication {
	case "", "disabled", "not_dispatched", "dispatched_unknown", "acknowledged":
	default:
		return fail()
	}
	switch r.ArmReceipt {
	case "", "dispatched_unknown", "acknowledged":
	default:
		return fail()
	}
	switch r.Code {
	case "publication_disabled":
		if intent.Kind != InstalledGetProposal && intent.Kind != InstalledPublish {
			return fail()
		}
	case "consumed":
		if intent.Kind != InstalledGetProposal && intent.Kind != InstalledPublish && intent.Kind != InstalledArm {
			return fail()
		}
	case "consent_refused", "readiness_unavailable":
		if intent.Kind != InstalledPublish {
			return fail()
		}
	case "outcome_unknown":
		if intent.Kind != InstalledPublish && intent.Kind != InstalledArm && intent.Kind != InstalledReprint {
			return fail()
		}
	case "backend_refused":
		if intent.Kind != InstalledGetStatus {
			return fail()
		}
	case "admission_refused":
		if intent.Kind != InstalledArm && intent.Kind != InstalledReprint {
			return fail()
		}
	case "receipt_capacity":
		if intent.Kind != InstalledArm {
			return fail()
		}
	}
	if r.ArmReceipt != "" && intent.Kind == InstalledGetStatus && intent.OperationID == "" {
		return fail()
	}
	if r.ArmReceipt != "" && intent.Kind != InstalledArm && intent.Kind != InstalledGetStatus {
		return fail()
	}
	if r.Proposal != nil && (intent.Kind != InstalledGetProposal || r.Code != "ok" || r.Proposal.Run != c.Run || r.Proposal.Installation != c.Installation) {
		return fail()
	}
	if p := r.Proposal; p != nil {
		u, e := url.Parse(p.Origin)
		if !handoffHex(p.ID, 32) || !validIdentity(p.Selection) || !validIdentity(p.Node) || !validIdentity(p.Profile) || !validIdentity(p.Attempt) || p.HTTPSPort == 0 || p.RelayPort == 0 || len(p.Origin) > 512 || e != nil || u.Scheme != "https" || u.Hostname() == "" || u.User != nil || u.Path != "" || u.RawQuery != "" || u.Fragment != "" || u.ForceQuery || strings.ContainsAny(p.Origin, "\\\\?#") {
			return fail()
		}
		// A proposal must also fit its exact subsequent confirmation envelope.
		consent := &InstalledConsent{*p, true, true, true, true, true, true}
		if _, e := installedIntentFrame(c, InstalledIntent{Kind: InstalledPublish, Consent: consent}); e != nil {
			return fail()
		}
	}
	if r.Status != nil && (intent.Kind != InstalledGetStatus || r.Code != "ok") {
		return fail()
	}
	if r.Invitation != "" && (r.Code != "ok" || (intent.Kind != InstalledArm && intent.Kind != InstalledReprint) || len(r.Invitation) > 4096) {
		return fail()
	}
	if r.Code == "ok" {
		if (intent.Kind == InstalledPublish && r.Publication != "acknowledged") || (intent.Kind == InstalledGetProposal && r.Publication != "not_dispatched") {
			return fail()
		}
		if r.Publication == "" || (intent.Kind == InstalledArm && r.ArmReceipt != "acknowledged") {
			return fail()
		}
		if intent.Kind == InstalledGetProposal && r.Proposal == nil {
			return fail()
		}
		if intent.Kind == InstalledGetStatus && r.Status == nil {
			return fail()
		}
		if (intent.Kind == InstalledArm || intent.Kind == InstalledReprint) && r.Invitation == "" {
			return fail()
		}
	}
	return r, nil
}
func installedExactShape(a, b any) bool {
	if a == nil {
		return false
	}
	am, ok := a.(map[string]any)
	if !ok {
		return true
	}
	bm, ok := b.(map[string]any)
	if !ok || len(am) != len(bm) {
		return false
	}
	for k, v := range am {
		w, ok := bm[k]
		if !ok || !installedExactShape(v, w) {
			return false
		}
	}
	// Reject noncanonical case and null recursively; typed decoding checks values.
	return true
}
