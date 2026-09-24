package tailscale

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// Source-derived synthetic fixtures (not runtime captures), from the pinned
// ipnstate.Status, version.Meta, and ipn.ServeConfig types cited in CONTRACT.md.
const sourceLong = "1.102.4-tbbcd7d1fc"
const sourceStatus = `{"Version":"1.102.4-tbbcd7d1fc","BackendState":"Running","Self":{"ID":"node-1","UserID":123,"DNSName":"relay.tailnet.ts.net."},"CurrentTailnet":{"Name":"example-account","MagicDNSSuffix":"tailnet.ts.net","MagicDNSEnabled":true},"CertDomains":["relay.tailnet.ts.net"],"User":{"123":{"ID":123,"LoginName":"user@example.invalid","DisplayName":"Example","ProfilePicURL":""}}}`
const sourceVersion = `{"majorMinorPatch":"1.102.4","short":"1.102.4","long":"1.102.4-tbbcd7d1fc","gitCommit":"bbcd7d1fc2054b9189ebc1531acf74bd880ca0c8","daemonLong":"1.102.4-tbbcd7d1fc","cap":141}`
const sourceScope = `{"TCP":{"443":{"HTTPS":true}},"Web":{"relay.tailnet.ts.net:443":{"Handlers":{"/":{"Proxy":"http://127.0.0.1:8375"}}}}}`
const sourceForeground = `{"Foreground":{"independent-session":` + sourceScope + `}}`

func TestTailscaleS5VersionContract(t *testing.T) {
	if err := validateVersion([]byte(sourceVersion), sourceLong); err != nil {
		t.Fatal(err)
	}
	for _, input := range []string{
		strings.ReplaceAll(sourceVersion, "1.102.4", "1.102.5"),
		strings.Replace(sourceVersion, SourceCommit, "", 1),
		strings.Replace(sourceVersion, SourceCommit, "abcd", 1),
		strings.Replace(sourceVersion, `"cap":141`, `"cap":"141"`, 1),
		strings.Replace(sourceVersion, `"cap":141`, `"cap":141,"gitDirty":true`, 1),
		strings.Replace(sourceVersion, `"cap":141`, `"cap":141,"isDev":true`, 1),
		strings.Replace(sourceVersion, `"cap":141`, `"cap":141,"unstableBranch":true`, 1),
		strings.Replace(sourceVersion, `"short":"1.102.4"`, `"short":null`, 1),
		strings.Replace(sourceVersion, `"gitCommit":`, `"GitCommit":`, 1),
		strings.Replace(sourceVersion, `"cap":141`, `"cap":141,"gitCommit":"`+SourceCommit+`"`, 1),
		strings.Replace(sourceVersion, `"cap":141`, `"cap":141,"isDev":"false"`, 1),
		strings.Replace(sourceVersion, `"gitCommit":"`+SourceCommit+`",`, "", 1),
		strings.ReplaceAll(sourceVersion, sourceLong, "1.102.4-dev20260922-tbbcd7d1fc"),
		strings.ReplaceAll(sourceVersion, sourceLong, "1.102.4-123-tbbcd7d1fc"),
	} {
		if validateVersion([]byte(input), sourceLong) == nil {
			t.Errorf("accepted invalid version: %s", input)
		}
	}
	if validateVersion([]byte(sourceVersion), "1.102.4-tother") == nil {
		t.Fatal("status drift accepted")
	}
	for _, long := range []string{"1.102.4", "1.102.4-dev20260922-tbbcd7d1fc", "1.102.4-123-tbbcd7d1fc", "1.102.4-tbbcd7d1fc-dirty", "1.102.4-t0000000"} {
		if validateVersion([]byte(strings.ReplaceAll(sourceVersion, sourceLong, long)), long) == nil {
			t.Errorf("invalid coherent long format accepted: %s", long)
		}
	}
	extra := strings.Repeat("a", 40)
	long := sourceLong + "-gaaaaaaa"
	input := strings.ReplaceAll(sourceVersion, sourceLong, long)
	input = strings.Replace(input, `"cap":141`, `"cap":141,"extraGitCommit":"`+extra+`"`, 1)
	if err := validateVersion([]byte(input), long); err != nil {
		t.Fatalf("supplemental source build: %v", err)
	}
}

func TestTailscaleS5ForegroundRoutes(t *testing.T) {
	for _, input := range []string{sourceForeground, strings.ReplaceAll(strings.ReplaceAll(sourceForeground, "443", "8443"), "8375", "9375")} {
		s, e := ParseServeStatus([]byte(input))
		if e != nil || !s.Complete || !s.Configured || len(s.ObservedRoutes) != 1 || s.ObservedRoutes[0].Session != "independent-session" {
			t.Fatalf("one TCP+Web route: %+v %v", s, e)
		}
	}
	for _, input := range []string{
		strings.TrimSuffix(sourceScope, "}") + `,"UnknownExposure":true}`,
		`{"Foreground":{"s":` + strings.TrimSuffix(sourceScope, "}") + `,"UnknownExposure":true}}}`,
		`{"Foreground":{"s":` + strings.TrimSuffix(sourceScope, "}") + `,"Foreground":{"nested":` + sourceScope + `}}}}`,
		strings.Replace(sourceForeground, `http://127.0.0.1:8375`, `not a backend`, 1),
		strings.Replace(sourceForeground, `"Proxy":"http://127.0.0.1:8375"`, `"Proxy":null`, 1),
		strings.Replace(sourceForeground, `"HTTPS":true`, `"HTTPS":true,"HTTP":true`, 1),
		strings.Replace(sourceForeground, `"Proxy":"http://127.0.0.1:8375"`, `"Proxy":"http://127.0.0.1:8375","Text":"extra"`, 1),
	} {
		if _, e := ParseServeStatus([]byte(input)); e == nil {
			t.Errorf("mixed/unknown exposure accepted: %s", input)
		}
	}
	input := `{"Foreground":{"z":` + sourceScope + `,"a":` + sourceScope + `}}`
	s, e := ParseServeStatus([]byte(input))
	if e != nil || len(s.ObservedRoutes) != 2 || s.ObservedRoutes[0].Session != "a" {
		t.Fatalf("deterministic sessions: %+v %v", s, e)
	}
	for _, input := range []string{`{"TCP":{"443":{"HTTPS":true}}}`, `{"Web":{"relay.tailnet.ts.net:443":{"Handlers":{}}}}`, strings.Replace(sourceForeground, `"Proxy"`, `"Text"`, 1), strings.Replace(sourceForeground, `relay.tailnet.ts.net:443`, `https://relay.tailnet.ts.net:443`, 1), `{"Foreground":{"x":{}}}`, strings.Replace(sourceForeground, `"HTTPS":true`, `"HTTPS":false`, 1)} {
		if _, e := ParseServeStatus([]byte(input)); e == nil {
			t.Errorf("incomplete route accepted: %s", input)
		}
	}
}

func TestTailscaleS5FunnelAndServices(t *testing.T) {
	for _, scope := range []string{`{"AllowFunnel":{"relay.tailnet.ts.net:443":true}}`, strings.TrimSuffix(sourceScope, "}") + `,"AllowFunnel":{"relay.tailnet.ts.net:443":true}}`} {
		for _, input := range []string{scope, `{"Foreground":{"s":` + scope + `}}`} {
			s, e := ParseServeStatus([]byte(input))
			if e != nil || !s.Complete || !s.Configured || !s.FunnelConfigured {
				t.Fatalf("Funnel lost: %+v %v", s, e)
			}
		}
	}
	s, e := ParseServeStatus([]byte(`{"AllowFunnel":{"relay.tailnet.ts.net:443":false}}`))
	if e != nil || !s.Configured || s.FunnelConfigured || !s.RetainedFunnel {
		t.Fatalf("false Funnel is retained state: %+v %v", s, e)
	}
	for _, input := range []string{`{"Services":{}}`, `{"Services":{"svc:foo":{"Tun":true}}}`, `{"TCP":{"443":{"TCPForward":"127.0.0.1:8375"}}}`, `{"AllowFunnel":{"relay.tailnet.ts.net:443":{}}}`, `{"AllowFunnel":{"relay.tailnet.ts.net:443":null}}`, `{"AllowFunnel":false}`, `{"Web":{},"Services":{"svc:foo":{"Tun":true}}}`} {
		if _, e := ParseServeStatus([]byte(input)); e == nil {
			t.Errorf("unsupported exposure accepted: %s", input)
		}
	}
}

type fakeStep struct {
	args   string
	output string
	err    error
}

// Every call is intercepted; there is no fallback runner, filesystem or exec.
func inspectFake(t *testing.T, steps []fakeStep) (Inspection, error) {
	t.Helper()
	n := 0
	runner := func(_ context.Context, binary string, args ...string) ([]byte, error) {
		t.Helper()
		if n >= len(steps) {
			t.Fatalf("unexpected runner call: %s %v", binary, args)
		}
		step := steps[n]
		n++
		if binary != "inert-only" || strings.Join(args, " ") != step.args {
			t.Fatalf("unexpected argv: %s %v", binary, args)
		}
		return []byte(step.output), step.err
	}
	got, e := inspectWithRunner(context.Background(), "inert-only", 443, runner)
	if n != len(steps) {
		t.Fatalf("calls=%d want %d", n, len(steps))
	}
	return got, e
}
func TestTailscaleS5InspectionCompleteness(t *testing.T) {
	status := fakeStep{"status --json", sourceStatus, nil}
	version := fakeStep{"version --json --daemon", sourceVersion, nil}
	got, e := inspectFake(t, []fakeStep{status, version, {"serve status --json", "{}", nil}})
	if e != nil || !got.ServeInspected || !got.ExposureComplete || got.ServeConfigured {
		t.Fatalf("complete empty: %+v %v", got, e)
	}
	for _, steps := range [][]fakeStep{
		{{"status --json", "", errors.New("timeout")}},
		{{"status --json", `{"BackendState":"Stopped"}`, nil}},
		{{"status --json", `{"BackendState":"NeedsLogin","Self":null}`, nil}},
		{{"status --json", `{"BackendState":"Running","Self":{}}`, nil}},
		// F001: identity refusal must precede both version and Serve calls.
		{{"status --json", strings.TrimSuffix(sourceStatus, "}") + `,"MagicDNSSuffix":null}`, nil}},
		{status, {"version --json --daemon", sourceVersion, errors.New("command error")}},
		{status, {"version --json --daemon", strings.ReplaceAll(sourceVersion, "1.102.4", "1.102.5"), nil}},
		{status, version, {"serve status --json", "", errors.New("timeout")}},
		{status, version, {"serve status --json", `{"Foreground":`, nil}},
		{status, version, {"serve status --json", `{"Web":{}}`, nil}},
	} {
		got, e := inspectFake(t, steps)
		if e == nil || got.ServeInspected || got.ExposureComplete || got.ServeRouteOwned {
			t.Fatalf("incomplete inspection accepted: %+v %v", got, e)
		}
	}
}
func TestTailscaleS5RouteMatching(t *testing.T) {
	expected := Route{Session: "independent-session", Listener: "HTTPS", Host: "relay.tailnet.ts.net", Port: 443, Handler: "Proxy", Path: "/", Backend: "http://127.0.0.1:8375"}
	s, e := ParseServeStatus([]byte(sourceForeground))
	if e != nil || !ExactRouteMatch(s, expected) {
		t.Fatalf("exact independently supplied tuple: %+v %v", s, e)
	}
	for _, change := range []func(*Route){
		func(r *Route) { r.Session = "other" }, func(r *Route) { r.Host = "other.tailnet.ts.net" }, func(r *Route) { r.Port = 8443 },
		func(r *Route) { r.Backend = "http://127.0.0.1:9375" }, func(r *Route) { r.Path = "/other" }, func(r *Route) { r.Handler = "Text" }, func(r *Route) { r.Listener = "HTTP" },
	} {
		wrong := expected
		change(&wrong)
		if ExactRouteMatch(s, wrong) {
			t.Fatalf("wrong expected tuple matched: %+v", wrong)
		}
	}
	custom := expected
	custom.Port = 8443
	custom.Backend = "http://127.0.0.1:9375"
	customObserved, e := ParseServeStatus([]byte(strings.ReplaceAll(strings.ReplaceAll(sourceForeground, "443", "8443"), "8375", "9375")))
	if e != nil || !ExactRouteMatch(customObserved, custom) {
		t.Fatalf("nondefault independently expected tuple: %+v %v", customObserved, e)
	}
	for _, input := range []string{
		sourceScope,
		strings.TrimSuffix(sourceScope, "}") + `,"Foreground":{"independent-session":` + sourceScope + `}}`,
		strings.Replace(sourceForeground, "independent-session", "replacement", 1),
		strings.Replace(sourceForeground, "8375", "9375", 1),
		strings.ReplaceAll(sourceForeground, "443", "8443"),
		strings.Replace(sourceForeground, `"/":`, `"/other":`, 1),
		`{"Foreground":{"independent-session":` + sourceScope + `,"extra":` + sourceScope + `}}`,
		strings.TrimSuffix(sourceForeground, "}") + `,"AllowFunnel":{"relay.tailnet.ts.net:443":false}}`,
		strings.TrimSuffix(sourceForeground, "}") + `,"AllowFunnel":{"relay.tailnet.ts.net:443":true}}`,
	} {
		s, e := ParseServeStatus([]byte(input))
		if e != nil {
			t.Fatal(e)
		}
		if ExactRouteMatch(s, expected) {
			t.Errorf("unowned tuple matched: %s", input)
		}
	}
	expected.Backend = "http://example.invalid:8375"
	if ExactRouteMatch(s, expected) {
		t.Fatal("nonloopback expectation accepted")
	}
}
func TestTailscaleS5BoundsAndDuplicates(t *testing.T) {
	for _, input := range []string{
		"null", "false", "[]", `{} {}`, `{"Web":{},"Web":{}}`, `{"Web":{},"\u0057eb":{}}`,
		`{"Foreground":{"a":` + sourceScope + `,"a":` + sourceScope + `}}`,
		`{"Web":{},"web":{}}`, `{"TCP":null}`, `{"TCP":{}}`, `{"Foreground":{}}`,
		strings.Repeat(" ", MaxOutputBytes) + "{}", string([]byte{'{', '"', 0xff, '"', ':', '0', '}'}),
		`{"x":` + strings.Repeat("[", 34) + "0" + strings.Repeat("]", 34) + `}`,
	} {
		if _, e := ParseServeStatus([]byte(input)); e == nil {
			t.Errorf("malformed/bounded JSON accepted (%d bytes)", len(input))
		}
	}
	if s, e := ParseServeStatus([]byte(" \n{}\t")); e != nil || s.Configured || !s.Complete {
		t.Fatalf("canonical absence: %+v %v", s, e)
	}
	for _, input := range []string{
		strings.Replace(sourceStatus, `"ID":"node-1"`, `"ID":123`, 1),
		strings.Replace(sourceStatus, "relay.tailnet.ts.net.", "relay.other.ts.net.", 1),
		strings.Replace(sourceStatus, "relay.tailnet.ts.net.", "bad/path.tailnet.ts.net.", 1),
		strings.Replace(sourceStatus, `"Running"`, `"running"`, 1),
		strings.Replace(sourceStatus, `"UserID":123`, `"UserID":"123"`, 1),
		strings.Replace(sourceStatus, `"Self":`, `"self":`, 1),
		strings.Replace(sourceStatus, `"BackendState":"Running"`, `"BackendState":"Running","backend_state":"Running"`, 1),
		strings.Replace(sourceStatus, `"UserID":123`, `"UserID":123,"user_id":123`, 1),
		strings.Replace(sourceStatus, `"ID":"node-1"`, `"ID":"node-1","\u0049D":"node-2"`, 1),
		strings.Replace(sourceStatus, `"ID":"node-1"`, `"ID":"node-1","ID":"node-2"`, 1),
	} {
		if s, e := ParseStatus([]byte(input)); e == nil || s.LoggedIn {
			t.Errorf("invalid identity accepted: %+v %v", s, e)
		}
	}
	s, e := ParseStatus([]byte(sourceStatus))
	if e != nil || !s.LoggedIn || s.UserID != 123 || s.TailnetName != "example-account" || len(s.CertDomains) != 1 || len(s.Account) == 0 {
		t.Fatalf("source identity not retained: %+v %v", s, e)
	}
}
func TestTailscaleS5NoOwnershipInference(t *testing.T) {
	got, e := inspectFake(t, []fakeStep{{"status --json", sourceStatus, nil}, {"version --json --daemon", sourceVersion, nil}, {"serve status --json", sourceForeground, nil}})
	if e != nil || !got.ExposureComplete || !got.ServeInspected || got.ServeRouteCount != 1 || len(got.ObservedRoutes) != 1 || got.ServeRouteOwned || got.AccountLoginName != "user@example.invalid" || string(got.VersionMetadata) != sourceVersion {
		t.Fatalf("observation is not ownership: %+v %v", got, e)
	}
}

// These are in-memory adversarial inputs, not captured CLI output.
func TestTailscaleS5MalformedServe(t *testing.T) {
	for _, input := range []string{
		`{"Web":{},"UnknownExposure":{"Proxy":"http://127.0.0.1:8375"}}`,
		`{"TCP":{},"Foreground":{"session":{"Foreground":{"nested":{}}}}}`,
	} {
		if got, err := ParseServeStatus([]byte(input)); err == nil {
			t.Errorf("incomplete mixed/nested exposure accepted: %s: %+v", input, got)
		}
	}
}

func TestTailscaleS5StatusIdentity(t *testing.T) {
	// F001: this source string is optional, but not nullable. Preserve the
	// intentional absent/empty policy and require agreement when nonempty.
	for _, suffix := range []string{`null`, `false`, `123`, `{}`, `[]`, `"other.ts.net"`} {
		input := strings.TrimSuffix(sourceStatus, "}") + `,"MagicDNSSuffix":` + suffix + `}`
		if got, err := ParseStatus([]byte(input)); err == nil || got.LoggedIn {
			t.Errorf("invalid legacy suffix accepted: %s: %+v, %v", suffix, got, err)
		}
	}
	for _, input := range []string{sourceStatus,
		strings.TrimSuffix(sourceStatus, "}") + `,"MagicDNSSuffix":""}`,
		strings.TrimSuffix(sourceStatus, "}") + `,"MagicDNSSuffix":"tailnet.ts.net"}`} {
		if got, err := ParseStatus([]byte(input)); err != nil || !got.LoggedIn {
			t.Errorf("valid legacy suffix policy refused: %+v, %v", got, err)
		}
	}
	for _, input := range []string{
		`{"BackendState":"Running","Self":{}}`,
		`{"BackendState":"Running","Self":{"ID":123,"DNSName":"node.example.ts.net."}}`,
	} {
		if got, err := ParseStatus([]byte(input)); err == nil && got.LoggedIn {
			t.Errorf("incomplete/numeric authenticated identity accepted: %s: %+v", input, got)
		}
	}
}
