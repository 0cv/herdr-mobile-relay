package doctor

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"regexp"
	"strings"
	"time"
)

type State string
const (
	OK State = "ok"
	Warn State = "warn"
	Error State = "error"
	Unknown State = "unknown"
	NotApplicable State = "not_applicable"
)

type Source string
const (
	SourceNone Source = "not_observed"
	SourceConfig Source = "selected_file"
	SourceBuild Source = "packaged_build"
	SourceClock Source = "local_clock"
	SourceQuery Source = "read_only_query"
	SourceHistorical Source = "historical_snapshot"
)

type Evidence string
const (
	EvidenceHealthy Evidence = "host_check_observed"
	EvidenceWarning Evidence = "host_warning_observed"
	EvidenceFailed Evidence = "host_check_failed"
	EvidenceUnavailable Evidence = "observation_unavailable"
	EvidenceMalformed Evidence = "observation_invalid"
	EvidenceHistorical Evidence = "historical_not_health"
	EvidenceCancelled Evidence = "observation_cancelled"
	EvidenceDisabled Evidence = "cli_disabled_not_observed"
	EvidencePhone Evidence = "phone_reachability_not_proved"
	EvidenceHerdrPing Evidence = "herdr_ping_incomplete"
	EvidenceConfigValid Evidence = "literal_config_valid"
	EvidenceConfigInvalid Evidence = "literal_config_refused"
	EvidenceConfigMissing Evidence = "selected_config_unavailable"
	EvidenceTokenMissing Evidence = "relay_key_not_configured"
	EvidenceBuildInvalid Evidence = "build_identity_unavailable"
	EvidenceNotApplicable Evidence = "check_not_applicable"
)

type CheckID string
const (
	CheckConfig CheckID = "configuration"
	CheckBuild CheckID = "build_identity"
	CheckClock CheckID = "observation_clock"
	CheckBundle CheckID = "installed_bundle"
	CheckService CheckID = "service_state"
	CheckRelay CheckID = "local_relay"
	CheckHerdr CheckID = "herdr_protocol"
	CheckPairing CheckID = "pairing_summary"
	CheckCloudflare CheckID = "cloudflare_supervision"
	CheckGateway CheckID = "gateway_registration"
	CheckManaged CheckID = "managed_session_status"
	CheckCLI CheckID = "cli_inspection"
	CheckIngress CheckID = "configured_ingress"
	CheckIngressBundle CheckID = "configured_app_bundle"
	CheckPhone CheckID = "phone_hints"
)

var checkOrder = []CheckID{
	CheckConfig, CheckBuild, CheckClock, CheckBundle, CheckService, CheckRelay, CheckHerdr, CheckPairing,
	CheckCloudflare, CheckGateway, CheckManaged, CheckCLI, CheckIngress, CheckIngressBundle, CheckPhone,
}

func category(id CheckID) string {
	switch id {
	case CheckConfig, CheckBuild, CheckClock, CheckBundle, CheckService: return "installation"
	case CheckRelay: return "local_relay"
	case CheckHerdr: return "herdr"
	case CheckPairing: return "pairing"
	case CheckPhone: return "phone"
	default: return "transport"
	}
}

// BuildIdentity is packaging metadata, never a host/node/installation identity.
// Invalid fields are replaced as a group rather than truncated/reflected.
type BuildIdentity struct {
	Version string `json:"version"`
	Revision string `json:"revision"`
	WebHash string `json:"web_hash"`
	Target string `json:"target"`
}

var buildVersion = regexp.MustCompile(`^[0-9]{1,3}\.[0-9]{1,3}\.[0-9]{1,3}(-[0-9]{1,8})?$`)
func hexField(s string, size int) bool {
	if len(s) != size { return false }
	for _, b := range []byte(s) { if (b < '0' || b > '9') && (b < 'a' || b > 'f') { return false } }
	return true
}
func validBuild(b BuildIdentity) bool {
	if len(b.Version) > 32 || !buildVersion.MatchString(b.Version) || !hexField(b.Revision, 40) || !hexField(b.WebHash, 64) { return false }
	switch b.Target {
	case "darwin/arm64", "darwin/amd64", "linux/arm64", "linux/amd64": return true
	default: return false
	}
}

// Datum is a typed minimized observation. Adapters must translate their raw
// bodies/errors to these finite codes and validate completeness/identity before
// claiming health. The runner never receives arbitrary status/error text.
type Datum struct {
	State State
	Evidence Evidence
	Source Source
	At time.Time
}

type Check struct {
	observedAt time.Time // acquisition input, cleared during finalization; never serialized
	ID CheckID `json:"id"`
	Category string `json:"category"`
	Applicable bool `json:"applicable"`
	Required bool `json:"required"`
	State State `json:"state"`
	Evidence Evidence `json:"evidence_code"`
	Message string `json:"evidence"`
	Source Source `json:"source"`
	AgeSeconds *int64 `json:"age_seconds,omitempty"`
	DurationMS *int64 `json:"duration_ms,omitempty"`
	RemedyID string `json:"remedy_id"`
	Remedy string `json:"remedy"`
}

// Report has no externally mutable fields. All renderings serialize the same
// finalized checks/decision, never inspection inputs or unfenced adapter text.
type Report struct {
	selectedTransport Mode // selection only, never a readiness/activation claim
	build BuildIdentity
	at time.Time
	overall string
	exit int
	checks []Check
}
func (r Report) SelectedTransport() Mode {
	switch r.selectedTransport {
	case ModeCloudflare, ModeGateway, ModeManaged, ModeBYO, ModeCLI: return r.selectedTransport
	default: return ModeUnknown
	}
}
func (r Report) ExitCode() int {
	if len(r.checks) == 0 { return 3 }
	return r.exit
}
func (r Report) Overall() string {
	if len(r.checks) == 0 { return "unknown" }
	return r.overall
}
func (r Report) Checks() []Check {
	if len(r.checks) == 0 { r = finalize(refusedConfig(ConfigUnavailable), BuildIdentity{}, time.Time{}, false, nil) }
	out := append([]Check(nil), r.checks...)
	for i := range out {
		if out[i].AgeSeconds != nil { n := *out[i].AgeSeconds; out[i].AgeSeconds = &n }
		if out[i].DurationMS != nil { n := *out[i].DurationMS; out[i].DurationMS = &n }
	}
	return out
}
func (r Report) MarshalJSON() ([]byte, error) {
	// A zero report is incomplete, never success.
	if len(r.checks) == 0 { r = finalize(refusedConfig(ConfigUnavailable), BuildIdentity{}, time.Time{}, false, nil) }
	return json.Marshal(struct {
		Schema int `json:"schema_version"`
		Build BuildIdentity `json:"relay_build"`
		SelectedTransport Mode `json:"selected_transport"`
		At string `json:"observed_at"`
		Overall string `json:"overall"`
		Exit int `json:"exit_code"`
		Checks []Check `json:"checks"`
	}{1, r.build, r.SelectedTransport(), observationTime(r.at), r.overall, r.exit, r.checks})
}

func message(e Evidence) string {
	switch e {
	case EvidenceHealthy: return "Required host observation completed; this does not prove phone reachability."
	case EvidenceWarning: return "A current host observation carries a nonfatal warning."
	case EvidenceFailed: return "A required host observation failed."
	case EvidenceHistorical: return "Historical evidence is not current health proof."
	case EvidenceMalformed: return "The observation could not be validated."
	case EvidenceCancelled: return "The diagnostic budget ended before a current observation completed."
	case EvidenceDisabled: return "Production CLI inspection is disabled; no workflow authority was supplied."
	case EvidencePhone: return "Host observations cannot establish phone or VPN reachability."
	case EvidenceHerdrPing: return "Herdr ping responded; installed-client, version and feature comparison remains incomplete."
	case EvidenceConfigValid: return "Selected literal configuration parsed; startup validity is not established."
	case EvidenceConfigInvalid: return "Selected configuration or invocation was refused without evaluation."
	case EvidenceConfigMissing: return "Selected private configuration could not be read."
	case EvidenceTokenMissing: return "A relay key is not configured; no key was generated."
	case EvidenceBuildInvalid: return "Validated packaged build identity is unavailable."
	case EvidenceNotApplicable: return "This check is not applicable to the selected mode/options."
	default: return "A required read-only observation is unavailable."
	}
}
func decorate(c Check) Check {
	c.Message = message(c.Evidence)
	c.RemedyID, c.Remedy = "none", "No action is performed by doctor."
	if c.State == Unknown || c.State == Error || c.State == Warn {
		c.RemedyID, c.Remedy = "review_observation", "Review the configured host and obtain a fresh read-only observation; do not delete recovery evidence."
	}
	if c.ID == CheckPhone {
		c.RemedyID, c.Remedy = "check_handset", "Check the handset connection and VPN separately; no phone probe was sent."
	}
	return c
}
func validTime(t time.Time) bool { return t.UTC().Year() >= 2000 && t.UTC().Year() <= 2100 }
func observationTime(t time.Time) string {
	if !validTime(t) { return "unknown" }
	return t.UTC().Format(time.RFC3339Nano)
}

func normalizeDatum(d Datum, now time.Time, duration time.Duration) (Datum, *int64, *int64) {
	if duration < 0 || duration > 20*time.Second { return Datum{Unknown, EvidenceMalformed, SourceNone, time.Time{}}, nil, nil }
	ms := duration.Milliseconds()
	if d.Source != SourceQuery && d.Source != SourceHistorical {
		return Datum{Unknown, EvidenceMalformed, SourceNone, time.Time{}}, nil, &ms
	}
	if !validTime(now) || !validTime(d.At) || d.At.After(now) {
		return Datum{Unknown, EvidenceMalformed, SourceNone, time.Time{}}, nil, &ms
	}
	age := now.Sub(d.At)
	// Validated timestamp years bound this integer without truncating history.
	n := int64(age / time.Second)
	if d.Source == SourceHistorical || age > 30*time.Second {
		return Datum{Unknown, EvidenceHistorical, SourceHistorical, d.At}, &n, &ms
	}
	switch {
	case d.State == OK && d.Evidence == EvidenceHealthy:
	case d.State == Warn && d.Evidence == EvidenceWarning:
	case d.State == Error && d.Evidence == EvidenceFailed:
	case d.State == Unknown && (d.Evidence == EvidenceUnavailable || d.Evidence == EvidenceHerdrPing):
	default:
		return Datum{Unknown, EvidenceMalformed, SourceNone, time.Time{}}, nil, &ms
	}
	return d, &n, &ms
}

func finalize(cfg Inspection, build BuildIdentity, now time.Time, invalidInvocation bool, observations map[CheckID]Check) Report {
	clockValid := validTime(now)
	if !clockValid { now = time.Time{} }
	r := Report{selectedTransport: cfg.Mode(), build: build, at: now.UTC(), overall: "ok"}
	invalid := invalidInvocation || cfg.code == ConfigInvalid || cfg.code == ConfigUnsafe
	if !validBuild(build) { r.build = BuildIdentity{"unknown", "unknown", "unknown", "unknown"} }
	failed, incomplete, warning := false, false, false
	for _, id := range checkOrder {
		c, found := observations[id]
		if !found {
			c = Check{ID: id, Category: category(id), Applicable: true, Required: true,
				State: Unknown, Evidence: EvidenceUnavailable, Source: SourceNone}
			// Transport requiredness/applicability is finalized by runner policy,
			// not by adapter data. Missing policy never proves all-host success.
			if id == CheckPhone { c.Required = false; c.Evidence = EvidencePhone }
		}
		// Age and freshness belong to the final report timestamp, not an early
		// callback's completion time. Revalidate before making the exit decision.
		if (c.Source == SourceQuery || c.Source == SourceHistorical) && !c.observedAt.IsZero() {
			duration := time.Duration(0)
			if c.DurationMS != nil { duration = time.Duration(*c.DurationMS) * time.Millisecond }
			d, age, elapsed := normalizeDatum(Datum{c.State, c.Evidence, c.Source, c.observedAt}, now, duration)
			c.State, c.Evidence, c.Source, c.AgeSeconds = d.State, d.Evidence, d.Source, age
			if c.DurationMS != nil { c.DurationMS = elapsed }
		}
		c.observedAt = time.Time{}
		if id == CheckConfig {
			c = Check{ID: id, Category: category(id), Applicable: true, Required: true, Source: SourceConfig}
			switch {
			case invalid: c.State, c.Evidence = Error, EvidenceConfigInvalid
			case cfg.code != ConfigValid: c.State, c.Evidence, c.Source = Unknown, EvidenceConfigMissing, SourceNone
			case !cfg.tokenPresent: c.State, c.Evidence = Warn, EvidenceTokenMissing
			default: c.State, c.Evidence = OK, EvidenceConfigValid
			}
		}
		if id == CheckBuild {
			c = Check{ID: id, Category: category(id), Applicable: true, Required: true, Source: SourceBuild, State: OK, Evidence: EvidenceHealthy}
			if !validBuild(build) { c.State, c.Evidence = Unknown, EvidenceBuildInvalid }
		}
		if id == CheckClock {
			c = Check{ID: id, Category: category(id), Applicable: true, Required: true, Source: SourceClock, State: OK, Evidence: EvidenceHealthy}
			if !clockValid { c.State, c.Evidence, c.Source = Unknown, EvidenceMalformed, SourceNone }
		}
		c = decorate(c)
		r.checks = append(r.checks, c)
		if c.Required && c.Applicable {
			failed = failed || c.State == Error
			incomplete = incomplete || c.State == Unknown || c.State == NotApplicable
		}
		warning = warning || c.State == Warn
	}
	switch {
	case invalid: r.exit, r.overall = 2, "failed"
	case failed: r.exit, r.overall = 1, "failed"
	case incomplete: r.exit, r.overall = 3, "unknown"
	case warning: r.overall = "degraded"
	}
	return r
}

func writeOutput(w io.Writer, output string) error {
	if absent(w) { return errors.New("diagnostic output unavailable") }
	written, err := io.WriteString(w, output)
	if err != nil || written != len(output) { return errors.New("diagnostic output unavailable") }
	return nil
}

// RenderJSON writes one document only. Writer failures are sanitized.
func (r Report) RenderJSON(w io.Writer) error {
	data, err := r.MarshalJSON()
	if err != nil { return errors.New("diagnostic output unavailable") }
	return writeOutput(w, string(data)+"\n")
}
func (r Report) RenderHuman(w io.Writer) error {
	if len(r.checks) == 0 { r = finalize(refusedConfig(ConfigUnavailable), BuildIdentity{}, time.Time{}, false, nil) }
	var out strings.Builder
	fmt.Fprintf(&out, "Doctor: %s (exit %d) selected_transport=%s at %s\n", r.overall, r.exit, r.SelectedTransport(), observationTime(r.at))
	for _, c := range r.checks {
		fmt.Fprintf(&out, "%s: %s [%s] %s", c.ID, c.State, c.Evidence, c.Message)
		fmt.Fprintf(&out, " Source: %s.", c.Source)
		if c.AgeSeconds != nil { fmt.Fprintf(&out, " Age: %ds.", *c.AgeSeconds) }
		if c.DurationMS != nil { fmt.Fprintf(&out, " Duration: %dms.", *c.DurationMS) }
		fmt.Fprintf(&out, " Remedy: %s\n", c.Remedy)
	}
	return writeOutput(w, out.String())
}
func (r Report) RenderStatus(w io.Writer) error {
	if len(r.checks) == 0 { r = finalize(refusedConfig(ConfigUnavailable), BuildIdentity{}, time.Time{}, false, nil) }
	var out strings.Builder
	fmt.Fprintf(&out, "selected_transport=%s status=%s exit=%d", r.SelectedTransport(), r.overall, r.exit)
	for _, c := range r.checks { fmt.Fprintf(&out, " %s=%s", c.ID, c.State) }
	out.WriteByte('\n')
	return writeOutput(w, out.String())
}
