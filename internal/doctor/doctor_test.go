package doctor

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"reflect"
	"strings"
	"testing"
	"time"
)

func testNow() time.Time { return time.Date(2026, 10, 3, 12, 0, 0, 0, time.UTC) }
func testBuild() BuildIdentity {
	return BuildIdentity{"0.22.4", strings.Repeat("a", 40), strings.Repeat("b", 64), "linux/amd64"}
}
func healthyChecks(cfg Inspection, opt Options) map[CheckID]Check {
	checks := policy(cfg, opt)
	for id, c := range checks {
		if c.Applicable && c.Required {
			c.State, c.Evidence, c.Source = OK, EvidenceHealthy, SourceQuery
			checks[id] = c
		}
	}
	return checks
}
func TestHerdrPingEvidenceRemainsRequiredUnknownAndFinite(t *testing.T) {
	ping := Datum{Unknown, EvidenceHerdrPing, SourceQuery, testNow()}
	normalized, age, duration := normalizeDatum(ping, testNow(), time.Millisecond)
	if normalized != ping || age == nil || duration == nil {
		t.Fatal("finite incomplete ping lost query provenance")
	}
	for _, bad := range []Datum{{OK, EvidenceHerdrPing, SourceQuery, testNow()}, {Warn, EvidenceHerdrPing, SourceQuery, testNow()}, {Error, EvidenceHerdrPing, SourceQuery, testNow()}, {Unknown, EvidenceHerdrPing, SourceNone, testNow()}} {
		got, _, _ := normalizeDatum(bad, testNow(), 0)
		if got.State != Unknown || got.Evidence != EvidenceMalformed {
			t.Fatal("ping evidence promoted or unobserved source accepted")
		}
	}
	cfg := fixtureConfig(ModeCloudflare)
	checks := healthyChecks(cfg, Options{})
	c := checks[CheckHerdr]
	c.State, c.Evidence, c.Source, c.observedAt = Unknown, EvidenceHerdrPing, SourceQuery, testNow()
	checks[CheckHerdr] = c
	r := finalize(cfg, testBuild(), testNow(), false, checks)
	if r.ExitCode() != 3 || !findCheck(t, r, CheckHerdr).Required || findCheck(t, r, CheckHerdr).Evidence != EvidenceHerdrPing {
		t.Fatal("ping satisfied full Herdr requirement")
	}
	assertRenderPrivacy(t, r)
	c = checks[CheckRelay]
	c.State, c.Evidence = Error, EvidenceFailed
	checks[CheckRelay] = c
	if finalize(cfg, testBuild(), testNow(), false, checks).ExitCode() != 1 {
		t.Fatal("incomplete ping masked observed failure")
	}
	cfg.code = ConfigInvalid
	if finalize(cfg, testBuild(), testNow(), false, checks).ExitCode() != 2 {
		t.Fatal("ping masked invalid configuration")
	}
}

func TestFinalModelExitPriorityAndRequiredness(t *testing.T) {
	for _, test := range []struct {
		name              string
		config            ConfigCode
		failure           bool
		missing           bool
		warning           bool
		invalidInvocation bool
		want              int
		overall           string
	}{
		{"healthy", ConfigValid, false, false, false, false, 0, "ok"},
		{"nonfatal warning", ConfigValid, false, false, true, false, 0, "degraded"},
		{"required failure", ConfigValid, true, false, false, false, 1, "failed"},
		{"required missing", ConfigValid, false, true, false, false, 3, "unknown"},
		{"failure beats incomplete", ConfigValid, true, true, false, false, 1, "failed"},
		{"invalid config beats failure", ConfigInvalid, true, true, false, false, 2, "failed"},
		{"unsafe config", ConfigUnsafe, false, false, false, false, 2, "failed"},
		{"absent config", ConfigUnavailable, false, false, false, false, 3, "unknown"},
		{"invalid invocation beats failure", ConfigValid, true, true, false, true, 2, "failed"},
	} {
		t.Run(test.name, func(t *testing.T) {
			cfg := fixtureConfig(ModeCloudflare)
			checks := healthyChecks(cfg, Options{})
			cfg.code = test.config
			if test.failure {
				c := checks[CheckRelay]
				c.State, c.Evidence = Error, EvidenceFailed
				checks[CheckRelay] = c
			}
			if test.missing {
				c := checks[CheckHerdr]
				c.State, c.Evidence = Unknown, EvidenceUnavailable
				checks[CheckHerdr] = c
			}
			if test.warning {
				c := checks[CheckService]
				c.State, c.Evidence = Warn, EvidenceWarning
				checks[CheckService] = c
			}
			r := finalize(cfg, testBuild(), testNow(), test.invalidInvocation, checks)
			if r.ExitCode() != test.want || r.Overall() != test.overall {
				t.Fatalf("decision=%d/%s", r.ExitCode(), r.Overall())
			}
			phone := r.Checks()[len(checkOrder)-1]
			if phone.Required || phone.State != Unknown || phone.Evidence != EvidencePhone {
				t.Fatal("phone hint became host health requirement/proof")
			}
		})
	}
	cfg := fixtureConfig(ModeBYO)
	checks := healthyChecks(cfg, Options{ProbeIngress: true})
	c := checks[CheckIngress]
	c.State, c.Evidence = Unknown, EvidenceUnavailable
	checks[CheckIngress] = c
	if finalize(cfg, testBuild(), testNow(), false, checks).ExitCode() != 3 {
		t.Fatal("requested ingress unavailable became success")
	}
	checks = healthyChecks(cfg, Options{})
	c = checks[CheckRelay]
	c.State = NotApplicable
	checks[CheckRelay] = c
	if finalize(cfg, testBuild(), testNow(), false, checks).ExitCode() != 3 {
		t.Fatal("required check could opt out of success proof")
	}
	if (Report{}).ExitCode() != 3 || (Report{}).Overall() != "unknown" {
		t.Fatal("zero report proved health")
	}
}

func TestDatumNormalizationHistoricalMalformedAndBounds(t *testing.T) {
	for _, test := range []struct {
		name     string
		datum    Datum
		duration time.Duration
		evidence Evidence
	}{
		{"snapshot", Datum{OK, EvidenceHealthy, SourceHistorical, testNow()}, time.Second, EvidenceHistorical},
		{"old query", Datum{Error, EvidenceFailed, SourceQuery, testNow().Add(-time.Minute)}, time.Second, EvidenceHistorical},
		{"future", Datum{OK, EvidenceHealthy, SourceQuery, testNow().Add(time.Second)}, 0, EvidenceMalformed},
		{"missing time", Datum{OK, EvidenceHealthy, SourceQuery, time.Time{}}, 0, EvidenceMalformed},
		{"unknown state", Datum{State("raw-secret-canary"), EvidenceHealthy, SourceQuery, testNow()}, 0, EvidenceMalformed},
		{"unknown evidence", Datum{OK, Evidence("/private/path-canary"), SourceQuery, testNow()}, 0, EvidenceMalformed},
		{"unknown source", Datum{OK, EvidenceHealthy, Source("https://node-canary"), testNow()}, 0, EvidenceMalformed},
		{"mismatched evidence", Datum{OK, EvidenceFailed, SourceQuery, testNow()}, 0, EvidenceMalformed},
		{"not applicable injection", Datum{NotApplicable, EvidenceNotApplicable, SourceQuery, testNow()}, 0, EvidenceMalformed},
		{"negative duration", Datum{OK, EvidenceHealthy, SourceQuery, testNow()}, -1, EvidenceMalformed},
		{"long duration", Datum{OK, EvidenceHealthy, SourceQuery, testNow()}, GlobalBudget + 1, EvidenceMalformed},
	} {
		t.Run(test.name, func(t *testing.T) {
			d, _, _ := normalizeDatum(test.datum, testNow(), test.duration)
			if d.State != Unknown || d.Evidence != test.evidence {
				t.Fatal("untrusted/historical observation became proof")
			}
		})
	}
	for _, d := range []Datum{{OK, EvidenceHealthy, SourceQuery, testNow()}, {Warn, EvidenceWarning, SourceQuery, testNow()}, {Error, EvidenceFailed, SourceQuery, testNow()}, {Unknown, EvidenceUnavailable, SourceQuery, testNow()}} {
		got, age, duration := normalizeDatum(d, testNow(), time.Millisecond)
		if got != d || age == nil || *age != 0 || duration == nil || *duration != 1 {
			t.Fatal("valid current datum changed")
		}
	}
}

func assertRenderPrivacy(t *testing.T, r Report) {
	t.Helper()
	var human, status, document bytes.Buffer
	if r.RenderHuman(&human) != nil || r.RenderStatus(&status) != nil || r.RenderJSON(&document) != nil {
		t.Fatal("render failed")
	}
	for _, canary := range []string{"secret-canary", "/private/path-canary", "node-canary", "https://", "\x1b", "raw-error-canary"} {
		if strings.Contains(human.String()+status.String()+document.String(), canary) {
			t.Fatal("diagnostic rendering disclosed untrusted input")
		}
	}
	decoder := json.NewDecoder(&document)
	var wire struct {
		Schema            int     `json:"schema_version"`
		Overall           string  `json:"overall"`
		SelectedTransport Mode    `json:"selected_transport"`
		At                string  `json:"observed_at"`
		Exit              int     `json:"exit_code"`
		Checks            []Check `json:"checks"`
	}
	if err := decoder.Decode(&wire); err != nil {
		t.Fatal(err)
	}
	var suffix any
	if err := decoder.Decode(&suffix); err != io.EOF {
		t.Fatal("JSON output was not exactly one document")
	}
	if wire.SelectedTransport != r.SelectedTransport() || !strings.Contains(human.String(), "selected_transport="+string(wire.SelectedTransport)) || !strings.Contains(status.String(), "selected_transport="+string(wire.SelectedTransport)) {
		t.Fatal("selected transport renderings diverged")
	}
	if wire.Schema != 1 || wire.Exit != r.ExitCode() || wire.Overall != r.Overall() || wire.At != observationTime(r.at) || !reflect.DeepEqual(wire.Checks, r.Checks()) {
		t.Fatal("JSON diverged from finalized model")
	}
	for i, c := range wire.Checks {
		if c.ID != checkOrder[i] || !strings.Contains(human.String(), string(c.ID)+": "+string(c.State)+" ["+string(c.Evidence)+"]") ||
			!strings.Contains(status.String(), string(c.ID)+"="+string(c.State)) {
			t.Fatal("human/status rules diverged from JSON")
		}
	}
}

func TestFinalizedRenderingAgreementAndPrivacy(t *testing.T) {
	cfg := fixtureConfig(ModeBYO)
	checks := healthyChecks(cfg, Options{})
	r := finalize(cfg, testBuild(), testNow(), false, checks)
	assertRenderPrivacy(t, r)
	for _, build := range []BuildIdentity{
		{"secret-canary", strings.Repeat("a", 40), strings.Repeat("b", 64), "linux/amd64"},
		{"0.22.4", "/private/path-canary", strings.Repeat("b", 64), "linux/amd64"},
		{"0.22.4", strings.Repeat("a", 40), strings.Repeat("b", 64), "https://node-canary"},
		{"0.22.4", strings.Repeat("a", 40), strings.Repeat("x", 100000), "linux/amd64"},
	} {
		r := finalize(cfg, build, testNow(), false, checks)
		if r.ExitCode() != 3 {
			t.Fatal("invalid build identity counted as proved health")
		}
		assertRenderPrivacy(t, r)
	}
	copies := r.Checks()
	copies[0].Message = "secret-canary"
	copies[0].State = Error
	if r.Checks()[0].Message == "secret-canary" || r.ExitCode() != 0 {
		t.Fatal("returned checks mutated finalized report")
	}
	assertRenderPrivacy(t, r)
}

func TestSelectedTransportIsFiniteSelectionNotHealth(t *testing.T) {
	for _, mode := range []Mode{ModeCloudflare, ModeGateway, ModeManaged, ModeBYO, ModeCLI} {
		cfg := fixtureConfig(mode)
		// No actual adapters: recognizing even CLI selection cannot qualify it.
		r := finalize(cfg, testBuild(), testNow(), false, policy(cfg, Options{}))
		if r.SelectedTransport() != mode || r.ExitCode() != 3 {
			t.Fatal("selection changed health/activation semantics")
		}
		assertRenderPrivacy(t, r)
		for _, code := range []ConfigCode{ConfigInvalid, ConfigUnsafe, ConfigUnavailable} {
			bad := cfg
			bad.code = code
			r := finalize(bad, testBuild(), testNow(), false, nil)
			if r.SelectedTransport() != ModeUnknown {
				t.Fatal("refused/unavailable config projected a guessed transport")
			}
			assertRenderPrivacy(t, r)
		}
	}
	for _, cfg := range []Inspection{{}, {code: ConfigValid, mode: Mode("secret-canary /private/path-canary\x1b")}, ParseConfig(strings.NewReader("K=$(touch secret-canary)\n"))} {
		r := finalize(cfg, testBuild(), testNow(), false, nil)
		if r.SelectedTransport() != ModeUnknown {
			t.Fatal("unknown mode escaped finite projection")
		}
		assertRenderPrivacy(t, r)
	}
	if (Report{}).SelectedTransport() != ModeUnknown {
		t.Fatal("zero report guessed Cloudflare")
	}
	assertRenderPrivacy(t, Report{})
}

type errorWriter struct{}

func (errorWriter) Write([]byte) (int, error) {
	return 0, errors.New("raw-error-canary /private/path-canary")
}
func TestInvalidClockAndZeroReportRemainIncomplete(t *testing.T) {
	cfg := fixtureConfig(ModeBYO)
	r := finalize(cfg, testBuild(), time.Time{}, false, healthyChecks(cfg, Options{}))
	if r.ExitCode() != 3 || findCheck(t, r, CheckClock).State != Unknown {
		t.Fatal("invalid clock proved current health")
	}
	assertRenderPrivacy(t, r)
	assertRenderPrivacy(t, Report{})
}

type shortWriter struct{}

func (shortWriter) Write(data []byte) (int, error) { return len(data) / 2, nil }
func TestOutputErrorsAreFixed(t *testing.T) {
	r := finalize(fixtureConfig(ModeCloudflare), testBuild(), testNow(), false, healthyChecks(fixtureConfig(ModeCloudflare), Options{}))
	for _, writer := range []io.Writer{errorWriter{}, shortWriter{}, nil} {
		for _, err := range []error{r.RenderJSON(writer), r.RenderHuman(writer), r.RenderStatus(writer)} {
			if err == nil || err.Error() != "diagnostic output unavailable" || errors.Unwrap(err) != nil {
				t.Fatal("raw/partial writer failure exposed or ignored")
			}
		}
	}
}
