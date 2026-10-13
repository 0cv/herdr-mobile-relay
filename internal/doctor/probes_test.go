package doctor

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"
)

type queryFake struct {
	t *testing.T
	calls []string
	observe func(context.Context, string) Datum
	forbidTailscale bool
}
func (q *queryFake) read(ctx context.Context, name string) Datum {
	q.calls = append(q.calls, name)
	if q.forbidTailscale && (name == "managed" || name == "cli") { q.t.Fatal("BYO/CLI invoked forbidden Tailscale seam") }
	deadline, ok := ctx.Deadline()
	if !ok || time.Until(deadline) > GlobalBudget { q.t.Fatal("query has no bounded global context") }
	if name != "bundle" && name != "ingress-bundle" && time.Until(deadline) > CallBudget { q.t.Fatal("ordinary query exceeds per-call context") }
	if q.observe != nil { return q.observe(ctx, name) }
	return Datum{OK, EvidenceHealthy, SourceQuery, testNow()}
}
func (q *queryFake) Bundle(ctx context.Context, _ LocalTarget, _ BuildIdentity) Datum { return q.read(ctx, "bundle") }
func (q *queryFake) Service(ctx context.Context, _ LocalTarget) Datum { return q.read(ctx, "service") }
func (q *queryFake) Relay(ctx context.Context, _ LocalTarget) Datum { return q.read(ctx, "relay") }
func (q *queryFake) Herdr(ctx context.Context, _ LocalTarget) Datum { return q.read(ctx, "herdr") }
func (q *queryFake) Pairing(ctx context.Context, _ LocalTarget) Datum { return q.read(ctx, "pairing") }
func (q *queryFake) Cloudflare(ctx context.Context, _ LocalTarget) Datum { return q.read(ctx, "cloudflare") }
func (q *queryFake) Gateway(ctx context.Context, _ LocalTarget) Datum { return q.read(ctx, "gateway") }
func (q *queryFake) ManagedStatus(ctx context.Context, _ LocalTarget) Datum { return q.read(ctx, "managed") }
func (q *queryFake) CLIStatus(ctx context.Context, _ LocalTarget) Datum { return q.read(ctx, "cli") }
func (q *queryFake) Health(ctx context.Context, target IngressTarget) Datum {
	if origin, ok := httpsOrigin(target.Origin()); !ok || origin != target.Origin() { q.t.Fatal("ingress received arbitrary/noncanonical origin") }
	return q.read(ctx, "ingress")
}
func (q *queryFake) CompleteBundle(ctx context.Context, target IngressTarget, build BuildIdentity) Datum {
	if origin, ok := httpsOrigin(target.Origin()); !ok || origin != target.Origin() || !validBuild(build) { q.t.Fatal("bundle lacked canonical target/exact build") }
	return q.read(ctx, "ingress-bundle")
}
func fakeDependencies(q *queryFake) Dependencies { return Dependencies{q, q, q} }
func findCheck(t *testing.T, r Report, id CheckID) Check {
	t.Helper()
	for _, c := range r.Checks() { if c.ID == id { return c } }
	t.Fatal("missing stable check")
	return Check{}
}

func TestRunnerPropagatesPrivateSocketAndPingStaysIncomplete(t *testing.T) {
	cfg := parseFixture("HERDR_RELAY_INSTANCE_ID='private-instance-canary'\nHERDR_SOCKET_PATH='/tmp/private/socket-canary.sock'\n")
	q := &instanceHost{queryFake: &queryFake{t: t}}
	q.observe = func(_ context.Context, name string) Datum {
		if name == "herdr" { return Datum{Unknown, EvidenceHerdrPing, SourceQuery, testNow()} }
		return Datum{OK, EvidenceHealthy, SourceQuery, testNow()}
	}
	// Exercise actual target capture rather than a separate policy model.
	r := run(context.Background(), cfg, testBuild(), Options{}, Dependencies{Host: q, Transport: q}, testNow)
	if q.target.herdrSocket != cfg.herdrSocket || q.target.build != testBuild() || q.target.instance != cfg.instance { t.Fatal("runner lost complete private selection") }
	if r.ExitCode() != 3 || findCheck(t, r, CheckHerdr).State != Unknown || findCheck(t, r, CheckHerdr).Evidence != EvidenceHerdrPing || !findCheck(t, r, CheckHerdr).Required { t.Fatal("ping completed required category") }
	before := q.target
	cfg.herdrSocket = "/tmp/different.sock"
	if before == selectedLocalTarget(cfg, testBuild()) { t.Fatal("socket omitted from complete construction equality") }
	if strings.Contains(fmt.Sprintf("%v %#v", before, before), "socket-canary") { t.Fatal("socket target reflected") }
	if _, err := json.Marshal(before); err == nil || strings.Contains(err.Error(), "socket-canary") { t.Fatal("socket target serialized") }
	assertRenderPrivacy(t, r)
}

func TestPublicRunnerWithoutAdaptersIsIncomplete(t *testing.T) {
	r := Run(context.Background(), fixtureConfig(ModeGateway), testBuild(), Options{}, Dependencies{})
	if r.ExitCode() != 3 || findCheck(t, r, CheckRelay).State != Unknown { t.Fatal("public runner manufactured health without adapters") }
	assertRenderPrivacy(t, r)
}

func TestRunnerSequenceModeDispatchAndMissingAdapters(t *testing.T) {
	for _, mode := range []Mode{ModeCloudflare, ModeGateway, ModeManaged, ModeBYO, ModeCLI} {
		t.Run(string(mode), func(t *testing.T) {
			q := &queryFake{t: t, forbidTailscale: mode == ModeBYO || mode == ModeCLI}
			cfg := fixtureConfig(mode)
			r := run(context.Background(), cfg, testBuild(), Options{}, fakeDependencies(q), testNow)
			want := []string{"bundle", "service", "relay", "herdr", "pairing"}
			switch mode {
			case ModeCloudflare: want = append(want, "cloudflare")
			case ModeGateway: want = append(want, "gateway")
			case ModeManaged: want = append(want, "managed")
			}
			if !reflect.DeepEqual(q.calls, want) { t.Fatal("runner dispatched wrong sequence/mode") }
			wantExit := 0
			if mode == ModeCLI { wantExit = 3 }
			if r.ExitCode() != wantExit { t.Fatal("mode policy exit incorrect") }
			if mode == ModeCLI && findCheck(t, r, CheckCLI).Evidence != EvidenceDisabled { t.Fatal("production CLI became observed/enabled") }
			missing := run(context.Background(), cfg, testBuild(), Options{}, Dependencies{}, testNow)
			if missing.ExitCode() != 3 { t.Fatal("missing adapters manufactured host health") }
		})
	}
}

func TestRunnerBYOZeroTailscaleAcrossAllPaths(t *testing.T) {
	for _, scenario := range []string{"healthy", "ingress", "malformed", "historical", "cancelled", "deadline", "invalid config"} {
		t.Run(scenario, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			q := &queryFake{t: t, forbidTailscale: true}
			q.observe = func(_ context.Context, _ string) Datum {
				switch scenario {
				case "malformed": return Datum{OK, Evidence("secret-canary"), SourceQuery, testNow()}
				case "historical": return Datum{OK, EvidenceHealthy, SourceHistorical, testNow().Add(-time.Hour)}
				case "cancelled": cancel()
				}
				return Datum{OK, EvidenceHealthy, SourceQuery, testNow()}
			}
			cfg := fixtureConfig(ModeBYO)
			if scenario == "invalid config" { cfg = parseFixture("HERDR_RELAY_TRANSPORT=tailscale-external\nK=$(bad)\n") }
			if scenario == "deadline" {
				var stop context.CancelFunc
				ctx, stop = context.WithDeadline(ctx, time.Now().Add(-time.Second))
				defer stop()
			}
			r := run(ctx, cfg, testBuild(), Options{ProbeIngress: scenario != "healthy"}, fakeDependencies(q), testNow)
			if scenario == "healthy" || scenario == "ingress" {
				if r.ExitCode() != 0 { t.Fatal("complete BYO host checks not recognized") }
			} else if r.ExitCode() == 0 { t.Fatal("incomplete BYO evidence became success") }
			assertRenderPrivacy(t, r)
		})
	}
}

func TestRunnerIngressIsOptInConfiguredAndComplete(t *testing.T) {
	for _, optIn := range []bool{false, true} {
		q := &queryFake{t: t}
		r := run(context.Background(), fixtureConfig(ModeGateway), testBuild(), Options{ProbeIngress: optIn}, fakeDependencies(q), testNow)
		want := []string{"bundle", "service", "relay", "herdr", "pairing", "gateway"}
		if optIn { want = append(want, "ingress", "ingress-bundle") }
		if !reflect.DeepEqual(q.calls, want) || r.ExitCode() != 0 { t.Fatal("ingress option/config dispatch incorrect") }
	}
	q := &queryFake{t: t}
	cfg := fixtureConfig(ModeBYO)
	deps := fakeDependencies(q)
	deps.Ingress = nil
	r := run(context.Background(), cfg, testBuild(), Options{ProbeIngress: true}, deps, testNow)
	if r.ExitCode() != 3 || !findCheck(t, r, CheckIngress).Required { t.Fatal("requested but unavailable ingress became success") }
	q = &queryFake{t: t, observe: func(_ context.Context, name string) Datum {
		if name == "ingress-bundle" { return Datum{Unknown, EvidenceUnavailable, SourceQuery, testNow()} }
		return Datum{OK, EvidenceHealthy, SourceQuery, testNow()}
	}}
	r = run(context.Background(), cfg, testBuild(), Options{ProbeIngress: true}, fakeDependencies(q), testNow)
	if r.ExitCode() != 3 { t.Fatal("partial/unavailable complete bundle became health proof") }
	// Cloudflare has no validated relay ingress origin in this core parser;
	// an app origin must never be guessed to be its relay endpoint.
	r = run(context.Background(), fixtureConfig(ModeCloudflare), testBuild(), Options{ProbeIngress: true}, fakeDependencies(&queryFake{t: t}), testNow)
	if r.ExitCode() != 3 { t.Fatal("missing configured origin was fabricated") }
}

func TestRunnerStopsSchedulingAfterCancellationAndIgnoresLateSuccess(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	q := &queryFake{t: t, observe: func(_ context.Context, _ string) Datum {
		cancel()
		return Datum{OK, EvidenceHealthy, SourceQuery, testNow()}
	}}
	r := run(ctx, fixtureConfig(ModeGateway), testBuild(), Options{ProbeIngress: true}, fakeDependencies(q), testNow)
	if !reflect.DeepEqual(q.calls, []string{"bundle"}) || r.ExitCode() != 3 { t.Fatal("late result counted or post-cancel work scheduled") }
	for _, id := range []CheckID{CheckBundle, CheckService, CheckRelay, CheckHerdr, CheckPairing, CheckGateway, CheckIngress, CheckIngressBundle} {
		if c := findCheck(t, r, id); c.State != Unknown || c.Evidence != EvidenceCancelled { t.Fatal("cancelled required work presented as observed") }
	}
	ctx, cancelDeadline := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer cancelDeadline()
	q = &queryFake{t: t}
	r = run(ctx, fixtureConfig(ModeBYO), testBuild(), Options{ProbeIngress: true}, fakeDependencies(q), testNow)
	if len(q.calls) != 0 || r.ExitCode() != 3 { t.Fatal("already expired context scheduled work") }
}

func TestRunnerPerCallTimeoutStopsLaterScheduling(t *testing.T) {
	q := &queryFake{t: t, observe: func(ctx context.Context, name string) Datum {
		if name == "service" { <-ctx.Done() }
		return Datum{OK, EvidenceHealthy, SourceQuery, testNow()}
	}}
	r := run(context.Background(), fixtureConfig(ModeManaged), testBuild(), Options{PerCall: time.Millisecond}, fakeDependencies(q), testNow)
	if !reflect.DeepEqual(q.calls, []string{"bundle", "service"}) || r.ExitCode() != 3 { t.Fatal("per-call timeout allowed later queries/late health") }
	if findCheck(t, r, CheckService).Evidence != EvidenceCancelled || findCheck(t, r, CheckManaged).State != Unknown { t.Fatal("timeout evidence not incomplete") }
}

func TestRunnerFinalTimestampRevalidatesEarlyObservationAge(t *testing.T) {
	current := testNow()
	q := &queryFake{t: t, observe: func(_ context.Context, name string) Datum {
		if name == "bundle" { return Datum{OK, EvidenceHealthy, SourceQuery, testNow().Add(-28*time.Second)} }
		current = testNow().Add(5*time.Second)
		return Datum{OK, EvidenceHealthy, SourceQuery, current}
	}}
	r := run(context.Background(), fixtureConfig(ModeCloudflare), testBuild(), Options{}, fakeDependencies(q), func() time.Time { return current })
	c := findCheck(t, r, CheckBundle)
	if r.ExitCode() != 3 || c.State != Unknown || c.Evidence != EvidenceHistorical || c.AgeSeconds == nil || *c.AgeSeconds != 33 {
		t.Fatal("early observation remained health proof after aging before finalization")
	}
	assertRenderPrivacy(t, r)
}

func TestRunnerTypedNilDependenciesStayUnknown(t *testing.T) {
	var q *queryFake
	r := run(context.Background(), fixtureConfig(ModeGateway), testBuild(), Options{ProbeIngress: true}, Dependencies{q, q, q}, testNow)
	if r.ExitCode() != 3 { t.Fatal("typed nil adapters proved health") }
	var ctx *nilContext
	r = run(ctx, fixtureConfig(ModeGateway), testBuild(), Options{}, Dependencies{}, testNow)
	if r.ExitCode() != 2 { t.Fatal("typed nil context was not refused") }
}

type instanceHost struct { *queryFake; target LocalTarget }
func (q *instanceHost) Relay(ctx context.Context, target LocalTarget) Datum {
	q.target = target
	return q.read(ctx, "relay")
}
func TestRunnerPropagatesOnlySelectedPrivateInstance(t *testing.T) {
	cfg := parseFixture("HERDR_RELAY_INSTANCE_ID='private-instance-canary'\nHERDR_RELAY_PORT=18377\n")
	q := &instanceHost{queryFake: &queryFake{t: t}}
	r := run(context.Background(), cfg, testBuild(), Options{}, Dependencies{Host: q}, testNow)
	if q.target.instance != cfg.instance || q.target.port != 18377 || q.target.mode != ModeCloudflare || q.target.build != testBuild() { t.Fatal("selected identity/build was lost/guessed") }
	if q.target.String() != "private local diagnostic target" || q.target.GoString() != q.target.String() { t.Fatal("target reflected private identity") }
	assertRenderPrivacy(t, r)
	if r.ExitCode() != 3 { t.Fatal("one local observation proved the whole host") }
}

func TestRunnerPropagatesPrivateBuildAndCompleteGatewaySelection(t *testing.T) {
	cfg := parseFixture("HERDR_RELAY_INSTANCE_ID='private-instance-canary'\nHERDR_GATEWAY_URL='wss://private-target-canary.example.test,wss://second.example.test'\n")
	build := testBuild()
	build.Revision = strings.Repeat("d", 40)
	q := &instanceHost{queryFake: &queryFake{t: t}}
	r := run(context.Background(), cfg, build, Options{}, Dependencies{Host: q}, testNow)
	if q.target.build != build || q.target.gatewayCount != 2 || q.target.gateways[0] != cfg.gateways[0] || q.target.gateways[1] != cfg.gateways[1] { t.Fatal("runner did not propagate exact build/ordered gateway binding") }
	before := q.target
	cfg.gateways[0] = "wss://changed.example.test"
	if q.target != before || q.target.gateways[0] == cfg.gateways[0] { t.Fatal("target aliased mutable gateway input") }
	if strings.Contains(fmt.Sprintf("%v %#v", q.target, q.target), "private-target-canary") { t.Fatal("private target binding reflected") }
	if _, err := json.Marshal(q.target); err == nil || strings.Contains(err.Error(), "private-target-canary") { t.Fatal("private target binding serialized") }
	assertRenderPrivacy(t, r)
	invalid := build
	invalid.Revision = "private-build-canary"
	q = &instanceHost{queryFake: &queryFake{t: t}}
	r = run(context.Background(), cfg, invalid, Options{}, Dependencies{Host: q}, testNow)
	if q.target.build != safeBuild(invalid) || q.target.build.Revision == invalid.Revision || r.ExitCode() != 3 { t.Fatal("raw invalid package identity propagated or qualified") }
	assertRenderPrivacy(t, r)
}

type nilContext struct { context.Context }

func TestRunnerUnknownConfigurationAndInvalidOptionsNeverDispatch(t *testing.T) {
	for _, cfg := range []Inspection{Inspection{}, refusedConfig(ConfigUnavailable), refusedConfig(ConfigInvalid), {code: ConfigValid, mode: ModeUnknown}} {
		q := &queryFake{t: t, forbidTailscale: true}
		r := run(context.Background(), cfg, testBuild(), Options{ProbeIngress: true}, fakeDependencies(q), testNow)
		if len(q.calls) != 0 || r.ExitCode() == 0 { t.Fatal("unknown/invalid mode dispatched guessed reads") }
	}
	for _, opt := range []Options{{Budget: GlobalBudget+1}, {PerCall: CallBudget+1}, {Budget: -1}, {PerCall: -1}} {
		q := &queryFake{t: t}
		r := run(context.Background(), fixtureConfig(ModeGateway), testBuild(), opt, fakeDependencies(q), testNow)
		if len(q.calls) != 0 || r.ExitCode() != 2 { t.Fatal("invalid invocation acquired observations") }
	}
	q := &queryFake{t: t, forbidTailscale: true}
	r := run(context.Background(), fixtureConfig(ModeCLI), testBuild(), Options{ProbeIngress: true}, fakeDependencies(q), testNow)
	if r.ExitCode() != 3 || !reflect.DeepEqual(q.calls, []string{"bundle", "service", "relay", "herdr", "pairing"}) { t.Fatal("unbound production CLI obtained inspector/ingress reads") }
}
