package doctor

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/0cv/herdr-mobile-relay/internal/protocol"
)

const localPrivateInstance = "private-installation-canary"
const localRegistrationID = "AAAAAAAAAAAAAAAAAAAAAA"

// All fixtures are disposable hosted-only test code; never call app/startup or
// source an environment file to create a synthetic health response.
func localConfig(mode Mode, port int) Inspection {
	text := "HERDR_RELAY_INSTANCE_ID='" + localPrivateInstance + "'\nHERDR_RELAY_PORT=" + strconv.Itoa(port) + "\nHERDR_RELAY_TRANSPORT=" + string(mode) + "\n"
	switch mode {
	case ModeGateway:
		text += "HERDR_GATEWAY_URL=wss://gateway.example.test\n"
	case ModeBYO:
		text += "HERDR_EXTERNAL_HTTPS_ORIGIN=https://relay.example.test\n"
	}
	return parseFixture(text)
}
func localTarget(cfg Inspection) LocalTarget { return selectedLocalTarget(cfg, testBuild()) }
func localInventoryFixture(state string) map[string]any {
	code := ""
	if state == "error" {
		code = "command_failed"
	}
	success := int64(1760000000)
	if state == "starting" {
		success = 0
	}
	return map[string]any{"state": state, "stale": state == "error", "error_code": code,
		"last_attempt_at": int64(1760000001), "last_success_at": success}
}
func localIdentityFixture() map[string]any {
	build := testBuild()
	return map[string]any{"instance": localPrivateInstance, "version": build.Version, "release_version": build.Version,
		"revision": build.Revision, "protocol": protocol.Version, "bundle_hash": build.WebHash,
		"bundle_version": build.Version, "bundle_revision": build.Revision, "bundle_build": strings.Repeat("c", 64),
		"inventory": localInventoryFixture("ready")}
}
func localEndpointFixture(path string, mode Mode) (int, http.Header, map[string]any, string) {
	h := http.Header{"Content-Type": {"application/json"}}
	if path == "/health" {
		h.Set("Content-Type", "text/plain; charset=utf-8")
		h.Set("X-Herdr-Relay-Instance", localPrivateInstance)
		return 200, h, nil, "ok\n"
	}
	object := localIdentityFixture()
	object["status"] = "ready"
	if path == "/healthz" {
		h.Set("X-Herdr-Relay-Instance", localPrivateInstance)
		object["status"], object["readiness"], object["transport"] = "ok", "ready", string(mode)
		object["gateway"] = map[string]any{"enabled": true, "registered": true, "relay_id": localRegistrationID,
			"url": "wss://gateway.example.test", "urls": []string{"wss://gateway.example.test"}, "version": "", "revision": ""}
		object["gateway_url"], object["gateway_version"], object["gateway_revision"] = "wss://gateway.example.test", "", ""
	}
	return 200, h, object, ""
}
func localEncode(t *testing.T, object any) string {
	t.Helper()
	body, err := json.Marshal(object)
	if err != nil {
		t.Fatal(err)
	}
	return string(body)
}

type localRT func(*http.Request) (*http.Response, error)

func (f localRT) RoundTrip(req *http.Request) (*http.Response, error) { return f(req) }

type localBody struct {
	reader               io.Reader
	reads, bytes, closes int
}

func (b *localBody) Read(p []byte) (int, error) {
	b.reads++
	n, err := b.reader.Read(p)
	b.bytes += n
	return n, err
}
func (b *localBody) Close() error { b.closes++; return nil }

type localMutation func(string, *int, http.Header, map[string]any, *string)
type localFixture struct {
	paths  []string
	bodies []*localBody
}

func fixtureLocal(t *testing.T, mode Mode, mutate localMutation) (*LocalQueries, Inspection, *localFixture) {
	t.Helper()
	cfg := localConfig(mode, 18377)
	q := NewLocalQueries(cfg, testBuild())
	f := &localFixture{}
	q.fixture = localRT(func(req *http.Request) (*http.Response, error) {
		f.paths = append(f.paths, req.URL.Path)
		if req.Method != http.MethodGet || req.URL.Scheme != "http" || req.URL.Host != "127.0.0.1:18377" ||
			req.Host != "127.0.0.1:18377" || req.URL.User != nil || req.URL.RawQuery != "" || req.URL.Fragment != "" || req.Body != nil || !req.Close {
			t.Fatal("request escaped fixed credential-free loopback GET contract")
		}
		if req.URL.Path != "/health" && req.URL.Path != "/healthz" && req.URL.Path != "/readyz" {
			t.Fatal("arbitrary/agent path requested")
		}
		if req.Header.Get("Accept-Encoding") != "identity" || req.Header.Get("User-Agent") != "herdr-doctor" ||
			req.Header.Get("Authorization") != "" || req.Header.Get("Cookie") != "" || req.Header.Get("Proxy-Authorization") != "" || len(req.Header) != 3 {
			t.Fatal("request included ambient authority/metadata")
		}
		deadline, ok := req.Context().Deadline()
		if !ok || time.Until(deadline) > CallBudget {
			t.Fatal("unbounded local request")
		}
		status, header, object, body := localEndpointFixture(req.URL.Path, mode)
		if mutate != nil {
			mutate(req.URL.Path, &status, header, object, &body)
		}
		if body == "" && object != nil {
			body = localEncode(t, object)
		}
		tracked := &localBody{reader: strings.NewReader(body)}
		f.bodies = append(f.bodies, tracked)
		return &http.Response{StatusCode: status, Header: header, ContentLength: -1, Body: tracked}, nil
	})
	return q, cfg, f
}
func assertLocalClosed(t *testing.T, f *localFixture) {
	t.Helper()
	assertLocalClosedLimit(t, f, 3)
}
func assertLocalClosedLimit(t *testing.T, f *localFixture, limit int) {
	t.Helper()
	if len(f.paths) > limit {
		t.Fatal("observation retried or exceeded fixed calls")
	}
	for _, body := range f.bodies {
		if body.closes != 1 || body.bytes > maxLocalBody+1 {
			t.Fatal("body closure/byte limit violated")
		}
	}
}
func assertLocalPrivacy(t *testing.T, datum Datum, report Report) {
	t.Helper()
	var human, data, status bytes.Buffer
	if report.RenderHuman(&human) != nil || report.RenderJSON(&data) != nil || report.RenderStatus(&status) != nil {
		t.Fatal("render failed")
	}
	text := fmt.Sprintf("%+v %#v", datum, datum) + human.String() + data.String() + status.String()
	for _, canary := range []string{localPrivateInstance, localRegistrationID, "secret-local-canary", "private-host-canary", "gateway.example.test", "127.0.0.1", "https://private", "/private/", "\x1b"} {
		if strings.Contains(text, canary) {
			t.Fatal("raw local diagnostic input escaped finite projection")
		}
	}
	assertRenderPrivacy(t, report)
}

func TestLocalExactOrdinaryHealthAndEmptyInventory(t *testing.T) {
	for _, mode := range []Mode{ModeCloudflare, ModeGateway, ModeBYO} {
		t.Run(string(mode), func(t *testing.T) {
			q, cfg, f := fixtureLocal(t, mode, nil)
			before := time.Now()
			d := q.Relay(context.Background(), localTarget(cfg))
			if d.State != OK || d.Evidence != EvidenceHealthy || d.Source != SourceQuery || d.At.Before(before) || d.At.After(time.Now()) {
				t.Fatal("exact ordinary empty-ready health not observed at acquisition")
			}
			if strings.Join(f.paths, ",") != "/health,/healthz,/readyz" {
				t.Fatal("bare liveness or agent fetch substituted for readiness")
			}
			assertLocalClosed(t, f)
			r := Run(context.Background(), cfg, testBuild(), Options{}, Dependencies{Host: q, Transport: q})
			if r.ExitCode() != 3 || findCheck(t, r, CheckRelay).State != OK {
				t.Fatal("local health alone qualified host or was not connected to real runner")
			}
			for _, id := range []CheckID{CheckBundle, CheckService, CheckHerdr, CheckPairing} {
				if findCheck(t, r, id).State != Unknown {
					t.Fatal("unimplemented category fabricated health")
				}
			}
			if mode == ModeGateway && findCheck(t, r, CheckGateway).State != OK {
				t.Fatal("exact host registration not observed")
			}
			if mode == ModeCloudflare && findCheck(t, r, CheckCloudflare).State != Unknown {
				t.Fatal("local adapter fabricated tunnel supervision")
			}
			assertLocalPrivacy(t, d, r)
		})
	}
}

func TestLocalRunnerRejectsStaleBuildAndGatewayBindingBeforeIO(t *testing.T) {
	oldSelection := "wss://gateway.example.test,wss://second.example.test"
	selected := func(gateways string) Inspection {
		return parseFixture("HERDR_RELAY_INSTANCE_ID='" + localPrivateInstance + "'\nHERDR_RELAY_PORT=18377\nHERDR_RELAY_TRANSPORT=gateway\nHERDR_GATEWAY_URL='" + gateways + "'\n")
	}
	oldConfig, oldBuild := selected(oldSelection), testBuild()
	fixture, _, f := fixtureLocal(t, ModeGateway, func(p string, _ *int, _ http.Header, object map[string]any, _ *string) {
		if p == "/healthz" {
			object["gateway"].(map[string]any)["urls"] = []string{"wss://gateway.example.test", "wss://second.example.test"}
		}
	})
	adapter := NewLocalQueries(oldConfig, oldBuild)
	adapter.fixture = fixture.fixture
	baseline := Run(context.Background(), oldConfig, oldBuild, Options{}, Dependencies{Host: adapter, Transport: adapter})
	if findCheck(t, baseline, CheckRelay).State != OK || findCheck(t, baseline, CheckGateway).State != OK || baseline.ExitCode() != 3 {
		t.Fatal("complete original binding fixture was not capable of observing local health/registration")
	}
	assertLocalClosedLimit(t, f, 6)
	tests := []struct {
		name  string
		cfg   Inspection
		build BuildIdentity
	}{
		{"changed leader", selected("wss://changed.example.test,wss://second.example.test"), oldBuild},
		{"changed fallback", selected("wss://gateway.example.test,wss://changed.example.test"), oldBuild},
		{"added fallback", selected(oldSelection + ",wss://third.example.test"), oldBuild},
		{"removed fallback", selected("wss://gateway.example.test"), oldBuild},
		{"reordered selection", selected("wss://second.example.test,wss://gateway.example.test"), oldBuild},
	}
	for _, field := range []string{"version", "revision", "web_hash", "target", "invalid"} {
		build := oldBuild
		switch field {
		case "version":
			build.Version = "0.22.5"
		case "revision":
			build.Revision = strings.Repeat("d", 40)
		case "web_hash":
			build.WebHash = strings.Repeat("e", 64)
		case "target":
			build.Target = "darwin/arm64"
		case "invalid":
			build.Revision = "secret-local-canary"
		}
		tests = append(tests, struct {
			name  string
			cfg   Inspection
			build BuildIdentity
		}{"changed build " + field, selected(oldSelection), build})
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if test.cfg.Code() != ConfigValid || test.cfg.mode != oldConfig.mode || test.cfg.port != oldConfig.port || test.cfg.pluginPort != oldConfig.pluginPort || test.cfg.instance != oldConfig.instance {
				t.Fatal("reload fixture did not preserve the original listener/instance tuple")
			}
			calls := 0
			adapter.fixture = localRT(func(*http.Request) (*http.Response, error) {
				calls++
				t.Fatal("stale adapter binding performed HTTP before refusing the runner mismatch")
				return nil, errors.New("secret-local-canary")
			})
			r := Run(context.Background(), test.cfg, test.build, Options{ProbeIngress: true}, Dependencies{Host: adapter, Transport: adapter})
			if calls != 0 || r.ExitCode() != 3 || r.build != safeBuild(test.build) {
				t.Fatal("stale adapter expectations qualified the newly described report")
			}
			for _, id := range []CheckID{CheckRelay, CheckGateway} {
				check := findCheck(t, r, id)
				if check.State != Unknown || check.Evidence != EvidenceUnavailable || !check.Required {
					t.Fatal("binding mismatch became observed health/failure or weakened requiredness")
				}
			}
			assertLocalPrivacy(t, localUnknown(), r)
		})
	}
}

func TestLocalRunnerAcceptsEquivalentReloadAndSnapshotsSelection(t *testing.T) {
	adapter, original, f := fixtureLocal(t, ModeGateway, nil)
	reloaded := localConfig(ModeGateway, 18377) // actual new literal parse, same values
	original.gateways[0] = "wss://changed.example.test"
	if adapter.target.gateways[0] != reloaded.gateways[0] || adapter.gateways[0] != reloaded.gateways[0] {
		t.Fatal("adapter snapshot aliased mutable configuration")
	}
	r := Run(context.Background(), reloaded, testBuild(), Options{}, Dependencies{Host: adapter, Transport: adapter})
	if findCheck(t, r, CheckRelay).State != OK || findCheck(t, r, CheckGateway).State != OK || r.ExitCode() != 3 || len(f.paths) != 6 {
		t.Fatal("equivalent reload refused or fabricated complete host health")
	}
	assertLocalClosedLimit(t, f, 6)
	assertLocalPrivacy(t, localUnknown(), r)
}

func TestLocalRunnerFixedCallCountGlobalBudgetAndCancellation(t *testing.T) {
	for _, cancelFirst := range []bool{false, true} {
		q, cfg, f := fixtureLocal(t, ModeGateway, nil)
		original := q.fixture
		ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
		parentDeadline, _ := ctx.Deadline()
		q.fixture = localRT(func(req *http.Request) (*http.Response, error) {
			deadline, ok := req.Context().Deadline()
			if !ok || deadline.After(parentDeadline) {
				t.Fatal("adapter enlarged remaining runner budget")
			}
			resp, err := original.RoundTrip(req)
			if cancelFirst {
				cancel()
			}
			return resp, err
		})
		r := Run(ctx, cfg, testBuild(), Options{Budget: 500 * time.Millisecond, ProbeIngress: true}, Dependencies{Host: q, Transport: q})
		cancel()
		want := "/health,/healthz,/readyz,/health,/healthz,/readyz"
		if cancelFirst {
			want = "/health"
			if findCheck(t, r, CheckRelay).Evidence != EvidenceCancelled || findCheck(t, r, CheckGateway).Evidence != EvidenceCancelled {
				t.Fatal("cancelled runner accepted late proof or scheduled another observation")
			}
		}
		if strings.Join(f.paths, ",") != want || r.ExitCode() != 3 {
			t.Fatal("actual runner retried or bypassed incomplete requiredness")
		}
		assertLocalClosedLimit(t, f, 6)
	}
}

func TestLocalMissingBindingUnsupportedModesAndMismatchedTargetsMakeNoRequests(t *testing.T) {
	q, cfg, _ := fixtureLocal(t, ModeCloudflare, nil)
	cases := []*LocalQueries{nil, NewLocalQueries(fixtureConfig(ModeCloudflare), testBuild()),
		NewLocalQueries(refusedConfig(ConfigUnavailable), testBuild()), NewLocalQueries(cfg, BuildIdentity{})}
	for _, mode := range []Mode{ModeManaged, ModeCLI, ModeUnknown} {
		c := cfg
		c.mode = mode
		cases = append(cases, NewLocalQueries(c, testBuild()))
	}
	for _, port := range []int{0, -1, 65536} {
		c := cfg
		c.port = port
		cases = append(cases, NewLocalQueries(c, testBuild()))
	}
	for _, candidate := range cases {
		if candidate != nil {
			candidate.fixture = localRT(func(*http.Request) (*http.Response, error) {
				t.Fatal("unbound/unsupported adapter performed I/O")
				return nil, nil
			})
		}
		if candidate.Relay(context.Background(), localTarget(cfg)).State != Unknown {
			t.Fatal("missing/unsupported binding accepted responder identity")
		}
	}
	q.fixture = localRT(func(*http.Request) (*http.Response, error) {
		t.Fatal("mismatched target/context performed I/O")
		return nil, nil
	})
	for _, change := range []func(*LocalTarget){
		func(t *LocalTarget) { t.port++ }, func(t *LocalTarget) { t.pluginPort++ },
		func(t *LocalTarget) { t.instance = "foreign-instance" }, func(t *LocalTarget) { t.mode = ModeBYO },
	} {
		target := localTarget(cfg)
		change(&target)
		if q.Relay(context.Background(), target).State != Unknown {
			t.Fatal("mismatched target acquired guessed proof")
		}
	}
	if q.Relay(nil, localTarget(cfg)).State != Unknown {
		t.Fatal("nil context admitted")
	}
	var typedNil *nilContext
	if q.Relay(typedNil, localTarget(cfg)).State != Unknown {
		t.Fatal("typed-nil context admitted")
	}
	if q.Gateway(context.Background(), localTarget(cfg)).State != Unknown {
		t.Fatal("ordinary mode observed gateway")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if q.Relay(ctx, localTarget(cfg)).State != Unknown {
		t.Fatal("cancelled context observed health")
	}
	ctx, stop := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer stop()
	if q.Relay(ctx, localTarget(cfg)).State != Unknown {
		t.Fatal("expired context performed I/O")
	}
	if _, err := json.Marshal(q); err == nil || strings.Contains(err.Error(), localPrivateInstance) {
		t.Fatal("adapter serialized private bindings")
	}
	if strings.Contains(fmt.Sprintf("%v %#v %v %#v", q, q, *q, *q), localPrivateInstance) {
		t.Fatal("adapter debug rendering leaked binding")
	}
	if _, err := json.Marshal(*q); err == nil || strings.Contains(err.Error(), localPrivateInstance) {
		t.Fatal("copied adapter serialized private binding")
	}
}

func TestLocalCompleteIdentityAndEndpointContradictionsStayUnknown(t *testing.T) {
	mutations := []struct {
		name   string
		change localMutation
	}{
		{"bare liveness", func(p string, _ *int, _ http.Header, o map[string]any, _ *string) {
			if p == "/healthz" {
				for k := range o {
					delete(o, k)
				}
				o["status"] = "ok"
			}
		}},
		{"health body", func(p string, _ *int, _ http.Header, _ map[string]any, b *string) {
			if p == "/health" {
				*b = "ok"
			}
		}},
		{"healthz header missing", func(p string, _ *int, h http.Header, _ map[string]any, _ *string) {
			if p == "/healthz" {
				h.Del("X-Herdr-Relay-Instance")
			}
		}},
		{"health header foreign", func(p string, _ *int, h http.Header, _ map[string]any, _ *string) {
			if p == "/health" {
				h.Set("X-Herdr-Relay-Instance", "private-host-canary")
			}
		}},
		{"health header duplicate", func(p string, _ *int, h http.Header, _ map[string]any, _ *string) {
			if p == "/health" {
				h.Add("X-Herdr-Relay-Instance", localPrivateInstance)
			}
		}},
		{"readyz optional header foreign", func(p string, _ *int, h http.Header, _ map[string]any, _ *string) {
			if p == "/readyz" {
				h.Set("X-Herdr-Relay-Instance", "private-host-canary")
			}
		}},
		{"transport wrong", func(p string, _ *int, _ http.Header, o map[string]any, _ *string) {
			if p == "/healthz" {
				o["transport"] = "tailscale-cli"
			}
		}},
		{"readyz transport contradiction", func(p string, _ *int, _ http.Header, o map[string]any, _ *string) {
			if p == "/readyz" {
				o["transport"] = "gateway"
			}
		}},
		{"ready status wrong", func(p string, _ *int, _ http.Header, o map[string]any, _ *string) {
			if p == "/readyz" {
				o["status"] = "unavailable"
			}
		}},
		{"ready status code wrong", func(p string, s *int, _ http.Header, _ map[string]any, _ *string) {
			if p == "/readyz" {
				*s = 503
			}
		}},
		{"inventory contradiction", func(p string, _ *int, _ http.Header, o map[string]any, _ *string) {
			if p == "/readyz" {
				o["inventory"] = localInventoryFixture("starting")
			}
		}},
		{"unknown readiness", func(p string, _ *int, _ http.Header, o map[string]any, _ *string) {
			if p == "/healthz" {
				o["readiness"] = "unknown-state"
			}
		}},
		{"stale ready", func(p string, _ *int, _ http.Header, o map[string]any, _ *string) {
			if p != "/health" {
				o["inventory"].(map[string]any)["stale"] = true
			}
		}},
		{"wrong stale type", func(p string, _ *int, _ http.Header, o map[string]any, _ *string) {
			if p != "/health" {
				o["inventory"].(map[string]any)["stale"] = "false"
			}
		}},
		{"unknown inventory", func(p string, _ *int, _ http.Header, o map[string]any, _ *string) {
			if p != "/health" {
				o["inventory"].(map[string]any)["state"] = "future"
			}
		}},
		{"incomplete inventory", func(p string, _ *int, _ http.Header, o map[string]any, _ *string) {
			if p != "/health" {
				delete(o["inventory"].(map[string]any), "last_success_at")
			}
		}},
		{"inventory timestamp type", func(p string, _ *int, _ http.Header, o map[string]any, _ *string) {
			if p != "/health" {
				o["inventory"].(map[string]any)["last_success_at"] = "1760000000"
			}
		}},
		{"inventory negative timestamp", func(p string, _ *int, _ http.Header, o map[string]any, _ *string) {
			if p != "/health" {
				o["inventory"].(map[string]any)["last_attempt_at"] = -1
			}
		}},
		{"inventory error contradiction", func(p string, _ *int, _ http.Header, o map[string]any, _ *string) {
			if p != "/health" {
				o["inventory"].(map[string]any)["error_code"] = "command_failed"
			}
		}},
		{"inventory null", func(p string, _ *int, _ http.Header, o map[string]any, _ *string) {
			if p != "/health" {
				o["inventory"] = nil
			}
		}},
		{"healthz status type", func(p string, _ *int, _ http.Header, o map[string]any, _ *string) {
			if p == "/healthz" {
				o["status"] = true
			}
		}},
		{"bundle build type", func(p string, _ *int, _ http.Header, o map[string]any, _ *string) {
			if p != "/health" {
				o["bundle_build"] = 397
			}
		}},
		{"bundle build shape", func(p string, _ *int, _ http.Header, o map[string]any, _ *string) {
			if p != "/health" {
				o["bundle_build"] = "397"
			}
		}},
		{"bundle build contradiction", func(p string, _ *int, _ http.Header, o map[string]any, _ *string) {
			if p == "/readyz" {
				o["bundle_build"] = strings.Repeat("d", 64)
			}
		}},
		{"missing bundle build", func(p string, _ *int, _ http.Header, o map[string]any, _ *string) {
			if p != "/health" {
				delete(o, "bundle_build")
			}
		}},
		{"degraded but ready", func(p string, _ *int, _ http.Header, o map[string]any, _ *string) {
			if p == "/healthz" {
				o["readiness"] = "degraded"
			}
		}},
		{"missing transport", func(p string, _ *int, _ http.Header, o map[string]any, _ *string) {
			if p == "/healthz" {
				delete(o, "transport")
			}
		}},
		{"wrong protocol type", func(p string, _ *int, _ http.Header, o map[string]any, _ *string) {
			if p != "/health" {
				o["protocol"] = "3"
			}
		}},
		{"fractional protocol", func(p string, _ *int, _ http.Header, o map[string]any, _ *string) {
			if p != "/health" {
				o["protocol"] = 3.1
			}
		}},
		{"protocol mismatch", func(p string, _ *int, _ http.Header, o map[string]any, _ *string) {
			if p != "/health" {
				o["protocol"] = protocol.Version + 1
			}
		}},
	}
	for _, path := range []string{"/healthz", "/readyz"} {
		for _, key := range []string{"instance", "version", "release_version", "revision", "bundle_hash", "bundle_version", "bundle_revision"} {
			for _, kind := range []string{"wrong", "missing", "type"} {
				mutations = append(mutations, struct {
					name   string
					change localMutation
				}{path + key + kind, func(p string, _ *int, _ http.Header, o map[string]any, _ *string) {
					if p != path {
						return
					}
					switch kind {
					case "wrong":
						o[key] = "secret-local-canary"
					case "missing":
						delete(o, key)
					case "type":
						o[key] = []any{"secret-local-canary"}
					}
				}})
			}
		}
	}
	for _, test := range mutations {
		t.Run(test.name, func(t *testing.T) {
			q, cfg, f := fixtureLocal(t, ModeCloudflare, test.change)
			d := q.Relay(context.Background(), localTarget(cfg))
			if d.State != Unknown || d.Evidence != EvidenceMalformed {
				t.Fatal("incomplete/contradictory/foreign evidence proved health/failure")
			}
			assertLocalClosed(t, f)
			r := Run(context.Background(), cfg, testBuild(), Options{}, Dependencies{Host: q})
			assertLocalPrivacy(t, d, r)
		})
	}
}

func TestLocalKnownStartingDegradedAndStaleInventoryAreObservedFailures(t *testing.T) {
	for _, state := range []string{"starting", "error"} {
		q, cfg, f := fixtureLocal(t, ModeCloudflare, func(p string, status *int, _ http.Header, object map[string]any, _ *string) {
			if p == "/health" {
				return
			}
			object["inventory"] = localInventoryFixture(state)
			if p == "/healthz" {
				object["readiness"] = "starting"
				if state == "error" {
					object["readiness"] = "degraded"
				}
			} else {
				object["status"] = "unavailable"
				*status = 503
			}
		})
		d := q.Relay(context.Background(), localTarget(cfg))
		if d.State != Error || d.Evidence != EvidenceFailed {
			t.Fatal("complete known local failure concealed as ready/unknown")
		}
		assertLocalClosed(t, f)
		r := Run(context.Background(), cfg, testBuild(), Options{}, Dependencies{Host: q})
		if r.ExitCode() != 1 {
			t.Fatal("observed required failure lost exit priority")
		}
		assertLocalPrivacy(t, d, r)
	}
}

func TestLocalGatewayRegistrationIsSeparateAttributedHostObservation(t *testing.T) {
	for _, test := range []struct {
		name  string
		state State
		edit  func(map[string]any)
	}{
		{"registered", OK, func(map[string]any) {}},
		{"pending", Error, func(g map[string]any) { g["registered"], g["relay_id"] = false, "" }},
		{"not registered", Error, func(g map[string]any) { g["registered"] = false }},
		{"disabled", Error, func(g map[string]any) {
			for k := range g {
				delete(g, k)
			}
			g["enabled"], g["registered"] = false, false
		}},
		{"contradictory flags", Unknown, func(g map[string]any) { g["enabled"] = false }},
		{"flag type", Unknown, func(g map[string]any) { g["registered"] = "true" }},
		{"missing flag", Unknown, func(g map[string]any) { delete(g, "enabled") }},
		{"missing registration identity", Unknown, func(g map[string]any) { g["relay_id"] = "" }},
		{"invalid registration identity", Unknown, func(g map[string]any) { g["relay_id"] = "secret-local-canary" }},
		{"registration identity type", Unknown, func(g map[string]any) { g["relay_id"] = 12 }},
		{"noncanonical base64 identity", Unknown, func(g map[string]any) { g["relay_id"] = strings.Repeat("A", 21) + "B" }},
		{"foreign gateway", Unknown, func(g map[string]any) { g["url"] = "wss://private-host-canary" }},
		{"foreign selection", Unknown, func(g map[string]any) { g["urls"] = []string{"wss://private-host-canary"} }},
		{"missing selection", Unknown, func(g map[string]any) { delete(g, "urls") }},
	} {
		t.Run(test.name, func(t *testing.T) {
			q, cfg, f := fixtureLocal(t, ModeGateway, func(p string, _ *int, _ http.Header, o map[string]any, _ *string) {
				if p == "/healthz" {
					test.edit(o["gateway"].(map[string]any))
				}
			})
			d := q.Gateway(context.Background(), localTarget(cfg))
			if d.State != test.state {
				t.Fatal("gateway registration matrix incorrect")
			}
			assertLocalClosed(t, f)
			if q.Relay(context.Background(), localTarget(cfg)).State != OK {
				t.Fatal("generic local relay incorrectly required registration")
			}
			r := Run(context.Background(), cfg, testBuild(), Options{}, Dependencies{Host: q, Transport: q})
			want := 3
			if test.state == Error {
				want = 1
			}
			if r.ExitCode() != want || findCheck(t, r, CheckRelay).State != OK {
				t.Fatal("runner registration requiredness weakened")
			}
			assertLocalPrivacy(t, d, r)
		})
	}
	q, cfg, _ := fixtureLocal(t, ModeGateway, func(p string, _ *int, _ http.Header, o map[string]any, _ *string) {
		if p == "/healthz" {
			o["instance"] = "foreign-instance"
		}
	})
	if q.Gateway(context.Background(), localTarget(cfg)).State != Unknown {
		t.Fatal("foreign local attribution proved registration")
	}
	for _, key := range []string{"gateway_url", "gateway_version", "gateway_revision"} {
		q, cfg, _ := fixtureLocal(t, ModeGateway, func(p string, _ *int, _ http.Header, o map[string]any, _ *string) {
			if p == "/healthz" {
				o[key] = "secret-local-canary"
			}
		})
		if q.Gateway(context.Background(), localTarget(cfg)).State != Unknown {
			t.Fatal("contradictory gateway projections proved registration")
		}
	}
}

func TestLocalGatewayActiveFirstOrderPreservesConfiguredFallbacks(t *testing.T) {
	g1, g2 := "wss://gateway.example.test", "wss://second.example.test"
	g3, g4 := "wss://third.example.test", "wss://fourth.example.test"
	for _, test := range []struct {
		name       string
		configured []string
		active     string
		returned   []any
		registered bool
		want       State
	}{
		{"configured leader", []string{g1, g2, g3, g4}, g1, []any{g1, g2, g3, g4}, true, OK},
		{"second active", []string{g1, g2, g3, g4}, g2, []any{g2, g1, g3, g4}, true, OK},
		{"middle active", []string{g1, g2, g3, g4}, g3, []any{g3, g1, g2, g4}, true, OK},
		{"last active", []string{g1, g2, g3, g4}, g4, []any{g4, g1, g2, g3}, true, OK},
		{"two candidate in-set contradiction", []string{g1, g2}, g2, []any{g1, g2}, true, Unknown},
		{"leader differs from active", []string{g1, g2, g3, g4}, g3, []any{g1, g2, g3, g4}, true, Unknown},
		{"cold fallback swap", []string{g1, g2, g3, g4}, g1, []any{g1, g3, g2, g4}, true, Unknown},
		{"incorrect rotation", []string{g1, g2, g3, g4}, g3, []any{g3, g4, g1, g2}, true, Unknown},
		{"duplicate candidate", []string{g1, g2, g3, g4}, g2, []any{g2, g1, g1, g4}, true, Unknown},
		{"foreign candidate", []string{g1, g2, g3, g4}, g1, []any{g1, g2, g3, "wss://private-host-canary"}, true, Unknown},
		{"wrong candidate type", []string{g1, g2}, g1, []any{g1, true}, true, Unknown},
		{"missing cold fallback", []string{g1, g2, g3, g4}, g1, []any{g1, g2, g3}, true, Unknown},
		{"valid pending failover", []string{g1, g2, g3, g4}, g3, []any{g3, g1, g2, g4}, false, Error},
		{"contradictory pending failover", []string{g1, g2}, g2, []any{g1, g2}, false, Unknown},
	} {
		t.Run(test.name, func(t *testing.T) {
			cfg := parseFixture("HERDR_RELAY_INSTANCE_ID='" + localPrivateInstance + "'\nHERDR_RELAY_PORT=18377\nHERDR_RELAY_TRANSPORT=gateway\nHERDR_GATEWAY_URL='" + strings.Join(test.configured, ",") + "'\n")
			if cfg.Code() != ConfigValid {
				t.Fatal("ordered selected gateway fixture refused")
			}
			fixture, _, f := fixtureLocal(t, ModeGateway, func(p string, _ *int, _ http.Header, object map[string]any, _ *string) {
				if p != "/healthz" {
					return
				}
				gateway := object["gateway"].(map[string]any)
				gateway["url"], gateway["urls"], gateway["registered"] = test.active, test.returned, test.registered
				// Repeated metadata deliberately agrees, so this regression tests
				// active-first/cold ordering, not the earlier top-level guard.
				object["gateway_url"] = test.active
			})
			adapter := NewLocalQueries(cfg, testBuild())
			adapter.fixture = fixture.fixture
			d := adapter.Gateway(context.Background(), localTarget(cfg))
			if d.State != test.want {
				t.Fatal("active-first selected candidate order misclassified")
			}
			if test.want == Unknown && d.Evidence != EvidenceMalformed {
				t.Fatal("contradictory registration became invented failure/health")
			}
			assertLocalClosed(t, f)
			if adapter.Relay(context.Background(), localTarget(cfg)).State != OK {
				t.Fatal("registration ordering contaminated generic relay health")
			}
			r := Run(context.Background(), cfg, testBuild(), Options{}, Dependencies{Host: adapter, Transport: adapter})
			wantExit := 3
			if test.want == Error {
				wantExit = 1
			}
			check := findCheck(t, r, CheckGateway)
			if r.ExitCode() != wantExit || check.State != test.want || !check.Required || findCheck(t, r, CheckRelay).State != OK {
				t.Fatal("runner changed ordering semantics, requiredness or incomplete exit")
			}
			assertLocalClosedLimit(t, f, 12)
			assertLocalPrivacy(t, d, r)
		})
	}
}

func TestLocalCompleteJSONBoundsAndDuplicateRejection(t *testing.T) {
	bad := []string{
		`{"k":1,"k":2}`, `{"extra":{"k":1,"k":2}}`, `{"extra":[{"k":1,"k":2}]}`,
		`{"k":true} {}`, `{"k":true}!`, `{"k":`, `[]`, `null`, `<html>secret-local-canary</html>`,
		"{\"k\":\"\xff\"}", `{"k":"` + strings.Repeat("x", maxJSONString+1) + `"}`,
		strings.Repeat(`{"k":`, maxJSONDepth+1) + `true` + strings.Repeat(`}`, maxJSONDepth+1),
		`{"k":[` + strings.Repeat("true,", maxJSONItems) + `true]}`,
		`{"k":[` + strings.TrimSuffix(strings.Repeat("["+strings.TrimSuffix(strings.Repeat("true,", maxJSONItems), ",")+"],", 17), ",") + `]}`,
	}
	var fields strings.Builder
	fields.WriteString("{")
	for n := 0; n <= maxJSONMembers; n++ {
		if n > 0 {
			fields.WriteString(",")
		}
		fmt.Fprintf(&fields, "\"k%d\":true", n)
	}
	fields.WriteString("}")
	bad = append(bad, fields.String())
	for _, body := range bad {
		if _, ok := localObject([]byte(body)); ok {
			t.Fatal("untrusted incomplete/duplicate/excessive JSON accepted")
		}
	}
	if _, ok := localObject([]byte(`{"safe":{"additive":[null,true,1,"private-host-canary"]}}`)); !ok {
		t.Fatal("bounded additive schema rejected")
	}
	body := `{}` + strings.Repeat(" ", maxLocalBody-2)
	if _, ok := localObject([]byte(body)); !ok {
		t.Fatal("exact complete body bound refused")
	}
	for _, suffix := range []string{" ", "!", strings.Repeat(" ", maxLocalBody) + "!"} {
		if _, ok := localObject([]byte(body + suffix)); ok {
			t.Fatal("oversized valid-prefix/suffix accepted")
		}
	}
}

func TestLocalUntrustedBodiesStatusesHeadersAndClosure(t *testing.T) {
	for _, test := range []struct {
		name   string
		change localMutation
	}{
		{"non JSON", func(p string, _ *int, _ http.Header, _ map[string]any, b *string) {
			if p == "/healthz" {
				*b = "secret-local-canary"
			}
		}},
		{"nested duplicate", func(p string, _ *int, _ http.Header, o map[string]any, b *string) {
			if p == "/healthz" {
				*b = strings.TrimSuffix(localEncode(t, o), "}") + `,"extra":{"secret":1,"secret":2}}`
			}
		}},
		{"duplicate required status", func(p string, _ *int, _ http.Header, o map[string]any, b *string) {
			if p == "/healthz" {
				*b = strings.TrimSuffix(localEncode(t, o), "}") + `,"status":"ok"}`
			}
		}},
		{"additive excessive array", func(p string, _ *int, _ http.Header, o map[string]any, b *string) {
			if p == "/healthz" {
				*b = strings.TrimSuffix(localEncode(t, o), "}") + `,"extra":[` + strings.Repeat("true,", maxJSONItems) + `true]}`
			}
		}},
		{"suffix", func(p string, _ *int, _ http.Header, o map[string]any, b *string) {
			if p == "/healthz" {
				*b = localEncode(t, o) + " {}"
			}
		}},
		{"over-limit valid prefix", func(p string, _ *int, _ http.Header, o map[string]any, b *string) {
			if p == "/healthz" {
				*b = localEncode(t, o) + strings.Repeat(" ", maxLocalBody) + "!"
			}
		}},
		{"truncated JSON", func(p string, _ *int, _ http.Header, o map[string]any, b *string) {
			if p == "/healthz" {
				text := localEncode(t, o)
				*b = text[:len(text)-1]
			}
		}},
		{"wrong status", func(_ string, status *int, _ http.Header, _ map[string]any, _ *string) { *status = 401 }},
		{"wrong media", func(_ string, _ *int, h http.Header, _ map[string]any, _ *string) { h.Set("Content-Type", "text/html") }},
		{"encoding", func(_ string, _ *int, h http.Header, _ map[string]any, _ *string) { h.Set("Content-Encoding", "gzip") }},
		{"oversized header", func(_ string, _ *int, h http.Header, _ map[string]any, _ *string) {
			h.Set("X-Untrusted", strings.Repeat("x", maxLocalHeaders+1))
		}},
		{"duplicate media", func(_ string, _ *int, h http.Header, _ map[string]any, _ *string) {
			h.Add("Content-Type", "application/json")
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			q, cfg, f := fixtureLocal(t, ModeCloudflare, test.change)
			d := q.Relay(context.Background(), localTarget(cfg))
			if d.State != Unknown {
				t.Fatal("untrusted response became definitive health/failure")
			}
			assertLocalClosed(t, f)
			if test.name == "encoding" || test.name == "oversized header" {
				if len(f.bodies) != 1 || f.bodies[0].reads != 0 {
					t.Fatal("refused encoding/headers caused a body read")
				}
			}
		})
	}
	for _, fail := range []string{"error", "short declared body", "declared oversize", "cancellation"} {
		q, cfg, f := fixtureLocal(t, ModeCloudflare, nil)
		original := q.fixture
		ctx, cancel := context.WithCancel(context.Background())
		q.fixture = localRT(func(req *http.Request) (*http.Response, error) {
			resp, err := original.RoundTrip(req)
			if req.URL.Path != "/healthz" {
				return resp, err
			}
			switch fail {
			case "error":
				f.bodies[len(f.bodies)-1].reader = io.MultiReader(strings.NewReader("{"), errorReader{})
			case "short declared body":
				resp.ContentLength = maxLocalBody
			case "declared oversize":
				resp.ContentLength = maxLocalBody + 1
			case "cancellation":
				cancel()
			}
			return resp, nil
		})
		d := q.Relay(ctx, localTarget(cfg))
		cancel()
		if d.State != Unknown || len(f.paths) != 2 {
			t.Fatal("failed acquisition retried/continued or manufactured failure")
		}
		assertLocalClosed(t, f)
	}
}

func TestLocalHTTPCompleteBodyExactLimitAndLimitPlusOne(t *testing.T) {
	for _, extra := range []int{0, 1} {
		q, cfg, f := fixtureLocal(t, ModeCloudflare, func(p string, _ *int, _ http.Header, o map[string]any, body *string) {
			if p == "/healthz" {
				encoded := localEncode(t, o)
				*body = encoded + strings.Repeat(" ", maxLocalBody-len(encoded)+extra)
			}
		})
		d := q.Relay(context.Background(), localTarget(cfg))
		want := OK
		if extra == 1 {
			want = Unknown
		}
		if d.State != want {
			t.Fatal("complete HTTP limit accepted truncation or refused exact bound")
		}
		if f.bodies[1].bytes != maxLocalBody+extra {
			t.Fatal("complete bounded input was not inspected")
		}
		assertLocalClosed(t, f)
	}
}

func TestLocalUnknownAdditiveCanariesNeverEscapeOrBecomeClockProof(t *testing.T) {
	q, cfg, _ := fixtureLocal(t, ModeCloudflare, func(p string, _ *int, h http.Header, o map[string]any, _ *string) {
		h.Set("X-Private", "https://private/secret-local-canary")
		if p != "/health" {
			o["extra"] = map[string]any{"private": "https://private/secret-local-canary\x1b[31m", "node": "private-host-canary", "path": "/private/credential"}
			o["timestamp"] = "2099-01-01T00:00:00Z"
		}
	})
	d := q.Relay(context.Background(), localTarget(cfg))
	if d.State != OK || d.At.Year() == 2099 {
		t.Fatal("additive fields rejected or remote clock used as observation")
	}
	assertLocalPrivacy(t, d, Run(context.Background(), cfg, testBuild(), Options{}, Dependencies{Host: q}))
	q.fixture = localRT(func(*http.Request) (*http.Response, error) {
		return nil, errors.New("https://private/secret-local-canary\x1b[31m")
	})
	d = q.Relay(context.Background(), localTarget(cfg))
	if d.State != Unknown || d.Evidence != EvidenceUnavailable {
		t.Fatal("raw acquisition error became observed failure")
	}
	assertLocalPrivacy(t, d, Run(context.Background(), cfg, testBuild(), Options{}, Dependencies{Host: q}))
}

func TestLocalActualRunnerBYONoTailscaleAndIngressRemainsIncomplete(t *testing.T) {
	for _, opt := range []bool{false, true} {
		q, cfg, f := fixtureLocal(t, ModeBYO, nil)
		trap := &queryFake{t: t, forbidTailscale: true}
		r := Run(context.Background(), cfg, testBuild(), Options{ProbeIngress: opt}, Dependencies{Host: q, Transport: trap})
		if len(trap.calls) != 0 || len(f.paths) != 3 || r.ExitCode() != 3 {
			t.Fatal("BYO touched transport seam or local-only evidence completed host")
		}
		if opt && (!findCheck(t, r, CheckIngress).Required || findCheck(t, r, CheckIngress).State != Unknown) {
			t.Fatal("missing requested ingress waived")
		}
		assertLocalClosed(t, f)
	}
	for _, mode := range []Mode{ModeManaged, ModeCLI} {
		cfg := localConfig(ModeBYO, 18377)
		cfg.mode = mode
		q := NewLocalQueries(cfg, testBuild())
		q.fixture = localRT(func(*http.Request) (*http.Response, error) {
			t.Fatal("unsupported policy performed ordinary local I/O")
			return nil, nil
		})
		r := Run(context.Background(), cfg, testBuild(), Options{ProbeIngress: true}, Dependencies{Host: q, Transport: q})
		if r.ExitCode() != 3 || findCheck(t, r, CheckRelay).State != Unknown {
			t.Fatal("unbound run-policy admitted ordinary local proof")
		}
		if mode == ModeCLI && findCheck(t, r, CheckCLI).Evidence != EvidenceDisabled {
			t.Fatal("CLI production inspector enabled")
		}
	}
	q := NewLocalQueries(localConfig(ModeBYO, 18377), testBuild())
	q.fixture = localRT(func(*http.Request) (*http.Response, error) {
		t.Fatal("malformed BYO configuration performed I/O")
		return nil, nil
	})
	cfg := parseFixture("HERDR_RELAY_TRANSPORT=tailscale-external\nK=$(bad)\n")
	if Run(context.Background(), cfg, testBuild(), Options{ProbeIngress: true}, Dependencies{Host: q, Transport: q}).ExitCode() != 2 {
		t.Fatal("invalid config bypassed runner policy")
	}
}

// Real constrained transport fixtures below are authored only, for later hosted
// execution. They must never be invoked as local verification in this phase.
func testLocalServerConfig(t *testing.T, server *httptest.Server) Inspection {
	t.Helper()
	_, port, err := net.SplitHostPort(server.Listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	n, err := strconv.Atoi(port)
	if err != nil {
		t.Fatal(err)
	}
	return localConfig(ModeCloudflare, n)
}
func TestLocalProductionTransportFixedRequestsClosesConnectionsAndIgnoresProxy(t *testing.T) {
	var proxyCalls atomic.Int32
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { proxyCalls.Add(1); w.WriteHeader(500) }))
	defer proxy.Close()
	for _, key := range []string{"HTTP_PROXY", "HTTPS_PROXY", "ALL_PROXY", "http_proxy", "https_proxy"} {
		t.Setenv(key, proxy.URL)
	}
	t.Setenv("NO_PROXY", "")
	t.Setenv("no_proxy", "")
	var mu sync.Mutex
	var paths []string
	closed := make(chan struct{}, 4)
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		paths = append(paths, r.URL.Path)
		mu.Unlock()
		if r.Method != "GET" || !strings.HasPrefix(r.Host, "127.0.0.1:") || r.URL.RawQuery != "" || r.Header.Get("Authorization") != "" || r.Header.Get("Cookie") != "" {
			t.Error("production request violated loopback contract")
		}
		status, header, object, body := localEndpointFixture(r.URL.Path, ModeCloudflare)
		for key, values := range header {
			w.Header()[key] = values
		}
		w.WriteHeader(status)
		if object != nil {
			body = localEncode(t, object)
		}
		fmt.Fprint(w, body)
	}))
	server.Config.ConnState = func(_ net.Conn, state http.ConnState) {
		if state == http.StateClosed {
			select {
			case closed <- struct{}{}:
			default:
			}
		}
	}
	server.Start()
	defer server.Close()
	cfg := testLocalServerConfig(t, server)
	q := NewLocalQueries(cfg, testBuild())
	if q.Relay(context.Background(), localTarget(cfg)).State != OK {
		t.Fatal("production bounded transport did not observe complete identity")
	}
	mu.Lock()
	got := strings.Join(paths, ",")
	mu.Unlock()
	if got != "/health,/healthz,/readyz" || proxyCalls.Load() != 0 {
		t.Fatal("production transport retried/escaped/proxied")
	}
	deadline := time.NewTimer(5 * time.Second)
	defer deadline.Stop()
	for n := 0; n < 3; n++ {
		select {
		case <-closed:
		case <-deadline.C:
			t.Fatal("observation left a connection open")
		}
	}
}

func TestLocalProductionTransportDoesNotFollowRedirects(t *testing.T) {
	// Same listener, different path: the pinned dial fence permits this
	// destination, so only the client's redirect policy can refuse it.
	t.Run("same-listener-path", func(t *testing.T) {
		var calls, followed atomic.Int32
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/redirect-target-canary" {
				followed.Add(1)
				w.Header().Set("Content-Type", "text/plain")
				w.Header().Set("X-Herdr-Relay-Instance", localPrivateInstance)
				fmt.Fprint(w, "ok\n")
				return
			}
			calls.Add(1)
			http.Redirect(w, r, "/redirect-target-canary", http.StatusFound)
		}))
		defer server.Close()
		cfg := testLocalServerConfig(t, server)
		q := NewLocalQueries(cfg, testBuild())
		if q.Relay(context.Background(), localTarget(cfg)).State != Unknown || calls.Load() != 1 || followed.Load() != 0 {
			t.Fatal("same-listener redirect followed/retried or counted as health")
		}
	})
	// Separate listener: defense in depth. The redirect policy refuses it and
	// the pinned dial fence would independently refuse the other listener.
	t.Run("separate-listener", func(t *testing.T) {
		var external atomic.Int32
		trap := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { external.Add(1) }))
		defer trap.Close()
		var calls atomic.Int32
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			calls.Add(1)
			http.Redirect(w, r, trap.URL+"/private/secret-local-canary", http.StatusFound)
		}))
		defer server.Close()
		cfg := testLocalServerConfig(t, server)
		q := NewLocalQueries(cfg, testBuild())
		if q.Relay(context.Background(), localTarget(cfg)).State != Unknown || calls.Load() != 1 || external.Load() != 0 {
			t.Fatal("redirect followed/retried or counted as health")
		}
	})
}

func TestLocalProductionTransportBlockedHeadersAndChunkedBodyRespectContext(t *testing.T) {
	for _, stage := range []string{"headers", "body"} {
		t.Run(stage, func(t *testing.T) {
			started, stopped := make(chan struct{}), make(chan struct{})
			var calls atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				close(started)
				if stage == "body" {
					w.Header().Set("Content-Type", "text/plain")
					w.Header().Set("X-Herdr-Relay-Instance", localPrivateInstance)
					fmt.Fprint(w, "o")
					w.(http.Flusher).Flush() // incomplete chunked /health
				}
				<-r.Context().Done()
				close(stopped)
			}))
			defer server.Close()
			cfg := testLocalServerConfig(t, server)
			q := NewLocalQueries(cfg, testBuild())
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			result := make(chan Datum, 1)
			go func() { result <- q.Relay(ctx, localTarget(cfg)) }()
			watch := time.NewTimer(5 * time.Second)
			defer watch.Stop()
			select {
			case <-started:
			case <-watch.C:
				t.Fatal("controlled request never started")
			}
			cancel()
			select {
			case d := <-result:
				if d.State != Unknown || calls.Load() != 1 {
					t.Fatal("cancelled acquisition became proof or retried")
				}
			case <-watch.C:
				t.Fatal("request ignored cancellation")
			}
			select {
			case <-stopped:
			case <-watch.C:
				t.Fatal("cancelled connection remained open")
			}
		})
	}
}

func TestLocalProductionTransportBlockedBodyExpiresWithoutRetry(t *testing.T) {
	started, stopped := make(chan struct{}), make(chan struct{})
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.Header().Set("Content-Type", "text/plain")
		w.Header().Set("X-Herdr-Relay-Instance", localPrivateInstance)
		fmt.Fprint(w, "o")
		w.(http.Flusher).Flush()
		close(started)
		<-r.Context().Done()
		close(stopped)
	}))
	defer server.Close()
	cfg := testLocalServerConfig(t, server)
	q := NewLocalQueries(cfg, testBuild())
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	result := make(chan Datum, 1)
	go func() { result <- q.Relay(ctx, localTarget(cfg)) }()
	watch := time.NewTimer(5 * time.Second)
	defer watch.Stop()
	select {
	case <-started:
	case <-watch.C:
		t.Fatal("deadline fixture never started")
	}
	select {
	case d := <-result:
		if d.State != Unknown || calls.Load() != 1 {
			t.Fatal("timed-out acquisition proved health or retried")
		}
	case <-watch.C:
		t.Fatal("body acquisition exceeded context")
	}
	select {
	case <-stopped:
	case <-watch.C:
		t.Fatal("timed-out connection remained open")
	}
}

func TestLocalSelectedPrivateConfigAndQueriesDoNotMutateFiles(t *testing.T) {
	root, path := privateFixture(t)
	text := "HERDR_RELAY_INSTANCE_ID='" + localPrivateInstance + "'\nHERDR_RELAY_PORT=18377\nHERDR_GITHUB_TOKEN_FILE='/private/unread-secret-local-canary'\n"
	if err := os.WriteFile(path, []byte(text), 0o600); err != nil {
		t.Fatal(err)
	}
	before, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatal(err)
	}
	parsed := ParseConfig(strings.NewReader(text))
	if parsed.Code() != ConfigValid || parsed.instance != localPrivateInstance {
		t.Fatal("platform-independent literal parser did not bind instance")
	}
	cfg := ReadConfig(path)
	if cfg.Code() != ConfigValid || cfg.instance != localPrivateInstance {
		t.Fatal("selected literal reader did not bind instance")
	}
	fixture, _, f := fixtureLocal(t, ModeCloudflare, nil)
	q := NewLocalQueries(cfg, testBuild())
	q.fixture = fixture.fixture
	r := Run(context.Background(), cfg, testBuild(), Options{ProbeIngress: true}, Dependencies{Host: q, Transport: q})
	if r.ExitCode() != 3 || findCheck(t, r, CheckRelay).State != OK {
		t.Fatal("actual selected-file/adapter/runner composition incorrect")
	}
	assertLocalClosed(t, f)
	assertLocalPrivacy(t, q.Service(context.Background(), localTarget(cfg)), r)
	after, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	bytes, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	afterEntries, err := os.ReadDir(root)
	if err != nil {
		t.Fatal(err)
	}
	if string(bytes) != text || !os.SameFile(before, after) || before.Mode() != after.Mode() || !before.ModTime().Equal(after.ModTime()) || !reflect.DeepEqual(entries, afterEntries) {
		t.Fatal("doctor changed selected configuration, permissions or directory inventory")
	}
}

func TestLocalProductionTransportTruncatedChunkedAndOversizedHeadersAreUnknown(t *testing.T) {
	for _, kind := range []string{"short declared body", "oversized chunked body", "oversized headers"} {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "text/plain")
			w.Header().Set("X-Herdr-Relay-Instance", localPrivateInstance)
			switch kind {
			case "short declared body":
				w.Header().Set("Content-Length", "100")
				fmt.Fprint(w, "ok\n")
			case "oversized chunked body":
				w.WriteHeader(200)
				w.(http.Flusher).Flush()
				fmt.Fprint(w, "ok\n"+strings.Repeat(" ", maxLocalBody))
			case "oversized headers":
				w.Header().Set("X-Private", strings.Repeat("x", maxLocalHeaders+1))
				fmt.Fprint(w, "ok\n")
			}
		}))
		cfg := testLocalServerConfig(t, server)
		q := NewLocalQueries(cfg, testBuild())
		d := q.Relay(context.Background(), localTarget(cfg))
		server.Close()
		if d.State != Unknown {
			t.Fatal("truncation/oversize became known health/failure")
		}
	}
}
