package localcontrol

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/url"
	"strings"
	"sync"

	"github.com/0cv/herdr-mobile-relay/internal/tailscalecli"
)

const InstalledProtocol = "installed-control-v1"
const installedReceiptLimit = 128

// InstalledSelection contains public correlation metadata only. Selection is an
// opaque reference, NEVER a path or config/token snapshot. The trusted composer
// must derive this from the original binding and retain backend consent privately.
type InstalledSelection struct {
	Selection string `json:"selection"`
	Node      string `json:"node"`
	Profile   string `json:"profile"`
	Origin    string `json:"origin"`
	HTTPSPort uint16 `json:"https_port"`
	RelayPort uint16 `json:"relay_port"`
	Attempt   string `json:"attempt"`
}

type InstalledProposal struct {
	InstalledSelection
	Run          string `json:"run"`
	Installation string `json:"installation"`
	ID           string `json:"id"`
}

type InstalledConsent struct {
	Proposal         InstalledProposal `json:"proposal"`
	Production       bool              `json:"production"`
	Persistent       bool              `json:"persistent"`
	CheckToWriteRace bool              `json:"check_to_write_race"`
	PortReuseRisk    bool              `json:"port_reuse_risk"`
	NoRollback       bool              `json:"no_rollback"`
	NoRemoteDrain    bool              `json:"no_remote_drain"`
}

// InstalledRequest is deliberately unrelated to the legacy managed envelope.
// Formatting is redacted; JSON encoding is explicitly credential-bearing.
type InstalledRequest struct {
	Protocol     string            `json:"protocol"`
	Run          string            `json:"run"`
	Installation string            `json:"installation"`
	Capability   string            `json:"capability"`
	Op           string            `json:"op"`
	Consent      *InstalledConsent `json:"consent,omitempty"`
	OperationID  string            `json:"operation_id,omitempty"`
}

func (InstalledRequest) String() string     { return "installed request [redacted]" }
func (r InstalledRequest) GoString() string { return r.String() }

// InstalledBackend is TRUSTED service composition, not external authority or
// runtime qualification. Publish closes over the exact S1a proposal/confirmation
// and original selected binding, not wire data. Status must be read-only.
// Admit must establish current concrete ownership AND full HTTPS/bundle admission
// on every pairing call. Arm/Reprint must enforce admission through their own
// secret acquisition boundary too. Reprint never rotates or creates an invitation.
// Every callback must honor cancellation and return before retirement can finish.
type InstalledBackend struct {
	// AcknowledgedPublication is trusted in-process historical data only; never wire exposed.
	AcknowledgedPublication func(context.Context) (tailscalecli.AcknowledgedPublication, error)
	// PublicationSelection is a copied construction-time correlation pin, not authority.
	// It is required exactly when AcknowledgedPublication is configured.
	PublicationSelection [32]byte
	// BeforePublish is a trusted read-only readiness preflight, run only after
	// authentication and exact consent, immediately before consuming the proposal.
	// Failure leaves that SAME proposal available for a later explicit request.
	// Concrete service adapters must supply it; nil preserves pure core callers.
	// Publish must still repeat its physical lease/readiness transaction checks.
	BeforePublish func(context.Context) error
	Publish       func(context.Context) error
	Status        func(context.Context) (InstalledObservation, error)
	Admit         func(context.Context) error
	Arm           func(context.Context) (string, error)
	Reprint       func(context.Context) (string, error)
}

// Finite observations cannot carry arbitrary backend diagnostics or secrets.
type InstalledObservation struct {
	Registered          bool `json:"registered"`
	InvitationAvailable bool `json:"invitation_available"`
	Quarantined         bool `json:"quarantined"`
}

type InstalledReply struct {
	Code        string                `json:"code"`
	Publication string                `json:"publication,omitempty"`
	ArmReceipt  string                `json:"arm_receipt,omitempty"`
	Proposal    *InstalledProposal    `json:"proposal,omitempty"`
	Status      *InstalledObservation `json:"status,omitempty"`
	Invitation  string                `json:"invitation,omitempty"`
}

func (InstalledReply) String() string     { return "installed reply [redacted]" }
func (r InstalledReply) GoString() string { return r.String() }

type InstalledCredentials struct{ Run, Installation, Capability string }

func (InstalledCredentials) String() string     { return "installed credentials [redacted]" }
func (c InstalledCredentials) GoString() string { return c.String() }

// InstalledControl copies share all authority and retirement state.
type InstalledControl struct{ state *installedState }

func (InstalledControl) String() string     { return "installed control [redacted]" }
func (c InstalledControl) GoString() string { return c.String() }

type installedState struct {
	mu          sync.Mutex
	retired     bool
	active      sync.WaitGroup
	ctx         context.Context
	cancel      context.CancelFunc
	gate        chan struct{}
	slots       chan struct{}
	capability  [32]byte
	proposal    InstalledProposal
	backend     InstalledBackend
	publication string
	receipts    map[string]string
}

func NewInstalledControl(installation string, selection InstalledSelection, backend InstalledBackend) (InstalledControl, InstalledCredentials, error) {
	return newInstalledControl(installation, selection, backend, false)
}

// NewInstalledResumeControl constructs a non-publishing capability. Attempt must
// be empty and Publish must be nil; status is still solely a backend observation.
// Wire requests cannot change this trusted construction choice.
func NewInstalledResumeControl(installation string, selection InstalledSelection, backend InstalledBackend) (InstalledControl, InstalledCredentials, error) {
	return newInstalledControl(installation, selection, backend, true)
}

func newInstalledControl(installation string, selection InstalledSelection, backend InstalledBackend, resume bool) (InstalledControl, InstalledCredentials, error) {
	fail := func() (InstalledControl, InstalledCredentials, error) {
		return InstalledControl{}, InstalledCredentials{}, errors.New("installed control configuration refused")
	}
	if (backend.AcknowledgedPublication == nil) != (backend.PublicationSelection == [32]byte{}) {
		return fail()
	}
	origin, originErr := url.Parse(selection.Origin)
	if originErr != nil || origin.Scheme != "https" || origin.Hostname() == "" || origin.User != nil || origin.Path != "" || origin.RawQuery != "" || origin.Fragment != "" || origin.ForceQuery || strings.ContainsAny(selection.Origin, "\\?#") {
		return fail()
	}
	if !validIdentity(installation) || !validIdentity(selection.Selection) || !validIdentity(selection.Node) || !validIdentity(selection.Profile) || len(selection.Origin) == 0 || len(selection.Origin) > 512 || selection.HTTPSPort == 0 || selection.RelayPort == 0 || backend.Status == nil || backend.Admit == nil || backend.Arm == nil || backend.Reprint == nil {
		return fail()
	}
	if resume {
		if selection.Attempt != "" || backend.Publish != nil || backend.BeforePublish != nil || backend.AcknowledgedPublication != nil || backend.PublicationSelection != [32]byte{} {
			return fail()
		}
	} else if !validIdentity(selection.Attempt) || backend.Publish == nil {
		return fail()
	}
	var entropy [64]byte
	if _, err := rand.Read(entropy[:]); err != nil {
		return fail()
	}
	proposal := InstalledProposal{InstalledSelection: selection, Run: hex.EncodeToString(entropy[:16]), Installation: installation, ID: hex.EncodeToString(entropy[16:32])}
	credentials := InstalledCredentials{proposal.Run, installation, hex.EncodeToString(entropy[32:])}
	if !resume {
		// Bound the exact supported/default client encoding, including HTML
		// escaping and the installation repeated in envelope and proposal.
		confirmation := InstalledRequest{Protocol: InstalledProtocol, Run: credentials.Run, Installation: installation, Capability: credentials.Capability, Op: "publish",
			Consent: &InstalledConsent{proposal, true, true, true, true, true, true}}
		encoded, err := json.Marshal(confirmation)
		if err != nil || len(encoded) > MaxRequestBytes {
			return fail()
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	s := &installedState{ctx: ctx, cancel: cancel, gate: make(chan struct{}, 1), slots: make(chan struct{}, 16), backend: backend, publication: "not_dispatched", receipts: make(map[string]string)}
	copy(s.capability[:], entropy[32:])
	s.proposal = proposal
	if resume {
		s.publication = "disabled"
	}
	return InstalledControl{s}, credentials, nil
}

// Retire denies new admission, cancels active/queued requests, and drains ALL
// admitted work. It never releases ownership on a timeout or spawns a worker.
func (c InstalledControl) Retire() {
	s := c.state
	if s == nil {
		return
	}
	s.mu.Lock()
	s.retired = true
	s.cancel()
	s.mu.Unlock()
	s.active.Wait()
}

// Dispatch performs no I/O and starts no request worker. At most 16 calls are
// admitted (one callback at a time); excess calls refuse rather than grow a queue.
// A caller losing this reply must use status/reprint, never retry a mutation with
// a fresh ID. JSON output is the sole explicitly secret-bearing result surface.
func (c InstalledControl) Dispatch(ctx context.Context, data []byte) []byte {
	reply := c.dispatch(ctx, data)
	if (ctx != nil && ctx.Err() != nil) || (c.state != nil && c.state.ctx.Err() != nil && reply.Invitation != "") {
		reply.Invitation = ""
		reply.Code = "cancelled"
	}
	out, err := json.Marshal(reply)
	if err != nil || len(out) > 8192 {
		return []byte(`{"code":"result_refused"}`)
	}
	return out
}
func (c InstalledControl) dispatch(ctx context.Context, data []byte) InstalledReply {
	refuse := func(code string) InstalledReply { return InstalledReply{Code: code} }
	s := c.state
	if s == nil || ctx == nil {
		return refuse("unavailable")
	}
	var r InstalledRequest
	if len(data) == 0 || len(data) > MaxRequestBytes || installedDecode(data, &r) != nil {
		return refuse("invalid_request")
	}
	secret, err := hex.DecodeString(r.Capability)
	var fixed [32]byte
	copy(fixed[:], secret)
	equal := subtle.ConstantTimeCompare(fixed[:], s.capability[:])
	if err != nil || len(secret) != 32 || equal != 1 || r.Protocol != InstalledProtocol || r.Run != s.proposal.Run || r.Installation != s.proposal.Installation {
		return refuse("unauthorized")
	}
	switch r.Op {
	case "proposal", "status", "reprint":
		if r.Consent != nil || (r.Op != "status" && r.OperationID != "") {
			return refuse("invalid_request")
		}
	case "publish":
		if r.Consent == nil || r.OperationID != "" {
			return refuse("invalid_request")
		}
	case "arm":
		if r.Consent != nil || !validIdentity(r.OperationID) {
			return refuse("invalid_request")
		}
	default:
		return refuse("invalid_request")
	}
	if r.OperationID != "" && !validIdentity(r.OperationID) {
		return refuse("invalid_request")
	}
	s.mu.Lock()
	if s.retired {
		s.mu.Unlock()
		return refuse("retired")
	}
	select {
	case s.slots <- struct{}{}:
	default:
		s.mu.Unlock()
		return refuse("busy")
	}
	s.active.Add(1)
	s.mu.Unlock()
	defer s.active.Done()
	defer func() { <-s.slots }()
	timeout := StatusTimeout
	if r.Op == "publish" {
		timeout = ActivateTimeout
	}
	if r.Op == "arm" || r.Op == "reprint" {
		timeout = ArmTimeout
	}
	opctx, cancel := context.WithTimeout(s.ctx, timeout)
	joined := make(chan struct{})
	stop := context.AfterFunc(ctx, func() { cancel(); close(joined) })
	defer func() {
		if !stop() {
			<-joined
		}
		cancel()
	}()
	select {
	case <-ctx.Done():
		return refuse("cancelled")
	case <-s.ctx.Done():
		return refuse("retired")
	case <-opctx.Done():
		return refuse("cancelled")
	case s.gate <- struct{}{}:
	}
	defer func() { <-s.gate }()
	if ctx.Err() != nil || opctx.Err() != nil {
		return refuse("cancelled")
	}
	result := InstalledReply{Code: "ok", Publication: s.publication}
	switch r.Op {
	case "proposal":
		if s.backend.Publish == nil {
			result.Code = "publication_disabled"
			break
		}
		if s.publication != "not_dispatched" {
			result.Code = "consumed"
			break
		}
		p := s.proposal
		result.Proposal = &p
	case "publish":
		if s.backend.Publish == nil {
			result.Code = "publication_disabled"
			break
		}
		consent := r.Consent
		if consent.Proposal != s.proposal || !consent.Production || !consent.Persistent || !consent.CheckToWriteRace || !consent.PortReuseRisk || !consent.NoRollback || !consent.NoRemoteDrain {
			result.Code = "consent_refused"
			break
		}
		if s.publication != "not_dispatched" {
			result.Code = "consumed"
			break
		}
		if s.backend.BeforePublish != nil {
			if s.backend.BeforePublish(opctx) != nil {
				result.Code = "readiness_unavailable"
				break
			}
		}
		if opctx.Err() != nil || ctx.Err() != nil {
			result.Code = "cancelled"
			break
		}
		s.publication = "dispatched_unknown"
		err = s.backend.Publish(opctx)
		if err == nil && opctx.Err() == nil && ctx.Err() == nil {
			s.publication = "acknowledged"
		} else {
			result.Code = "outcome_unknown"
		}
		result.Publication = s.publication
	case "status":
		observation, e := s.backend.Status(opctx)
		if e != nil {
			result.Code = "backend_refused"
		} else {
			result.Status = &observation
		}
		result.ArmReceipt = s.receipts[r.OperationID]
	case "arm", "reprint":
		// Even replay receipts require fresh admission; they never contain secrets.
		if s.backend.Admit(opctx) != nil {
			result.Code = "admission_refused"
			break
		}
		if opctx.Err() != nil || ctx.Err() != nil {
			result.Code = "cancelled"
			break
		}
		if r.Op == "arm" {
			if receipt, ok := s.receipts[r.OperationID]; ok {
				result.Code = "consumed"
				result.ArmReceipt = receipt
				break
			}
			if len(s.receipts) >= installedReceiptLimit {
				result.Code = "receipt_capacity"
				break
			}
			s.receipts[r.OperationID] = "dispatched_unknown"
			result.Invitation, err = s.backend.Arm(opctx)
		} else {
			result.Invitation, err = s.backend.Reprint(opctx)
		}
		if err != nil || len(result.Invitation) == 0 || len(result.Invitation) > 4096 || opctx.Err() != nil || ctx.Err() != nil {
			result.Invitation = ""
			result.Code = "outcome_unknown"
		} else if r.Op == "arm" {
			s.receipts[r.OperationID] = "acknowledged"
		}
		if r.Op == "arm" {
			result.ArmReceipt = s.receipts[r.OperationID]
		}
	}
	if ctx.Err() != nil || opctx.Err() != nil {
		result.Invitation = ""
		result.Proposal = nil
		result.Status = nil
		result.Code = "cancelled"
	}
	return result
}

// The legacy decoder rejects duplicates, conflicting case, trailing data and
// unknown members. Additionally require exact key spelling at every level:
// encoding/json alone would accept case-insensitive field aliases.
func installedDecode(data []byte, destination *InstalledRequest) error {
	if err := decodeControlJSON(data, destination); err != nil {
		return err
	}
	var actual map[string]json.RawMessage
	if err := json.Unmarshal(data, &actual); err != nil || actual == nil {
		return errors.New("invalid envelope")
	}
	canonical, _ := json.Marshal(destination)
	var expected map[string]json.RawMessage
	_ = json.Unmarshal(canonical, &expected)
	var keys func(map[string]json.RawMessage, map[string]json.RawMessage) error
	keys = func(a, b map[string]json.RawMessage) error {
		for key, value := range a {
			other, ok := b[key]
			if !ok || bytes.Equal(value, []byte("null")) {
				return errors.New("invalid member")
			}
			value = bytes.TrimSpace(value)
			if len(value) > 0 && value[0] == '{' {
				var x, y map[string]json.RawMessage
				if json.Unmarshal(value, &x) != nil || json.Unmarshal(other, &y) != nil {
					return errors.New("invalid object")
				}
				if err := keys(x, y); err != nil {
					return err
				}
			}
		}
		return nil
	}
	return keys(actual, expected)
}
