package doctor

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"time"
)

const GlobalBudget = 20 * time.Second
const CallBudget = 3 * time.Second

// Options deliberately has no URL, command, environment or authority override.
type Options struct {
	ProbeIngress bool
	Budget time.Duration // zero uses GlobalBudget; values above it are refused
	PerCall time.Duration // zero uses CallBudget; values above it are refused
}

// LocalTarget carries private selected routing, expected package identity and
// the complete ordered gateway selection and explicit private Herdr socket. A concrete adapter must compare this
// value with its construction snapshot before I/O; ports/instance alone cannot
// bind a reloaded configuration or a different package. It grants no authority.
type LocalTarget struct {
	mode Mode
	port, pluginPort int
	instance string
	herdrSocket string
	build BuildIdentity
	gateways [MaxGateways]string
	gatewayCount int
}

// Value copies prevent slice aliasing across config reloads. Count plus array
// preserves the entire ordered selection, including additions and removals.
// Invalid build fields are never propagated to a query adapter as raw metadata.
func selectedLocalTarget(cfg Inspection, build BuildIdentity) LocalTarget {
	t := LocalTarget{mode: cfg.mode, port: cfg.port, pluginPort: cfg.pluginPort, instance: cfg.instance,
		herdrSocket: cfg.herdrSocket, build: safeBuild(build), gatewayCount: len(cfg.gateways)}
	copy(t.gateways[:], cfg.gateways)
	return t
}
func (t LocalTarget) Mode() Mode { return t.mode }
func (t LocalTarget) Port() int { return t.port }
func (t LocalTarget) PluginPort() int { return t.pluginPort }
func (LocalTarget) String() string { return "private local diagnostic target" }
func (LocalTarget) GoString() string { return "private local diagnostic target" }
func (LocalTarget) MarshalJSON() ([]byte, error) { return nil, errors.New("diagnostic target is not serializable") }

// IngressTarget can only be constructed by mode policy from validated selected
// configuration. Origin is for trusted read-only adapters, not shared output.
// An adapter must use fixed allowed GETs, ordinary TLS trust, no redirects or
// auth/synthetic-phone requests, bounded bodies and complete exact identity.
type IngressTarget struct { origin string }
func (t IngressTarget) Origin() string { return t.origin }
func (IngressTarget) String() string { return "private configured ingress target" }
func (IngressTarget) GoString() string { return "private configured ingress target" }
func (IngressTarget) MarshalJSON() ([]byte, error) { return nil, errors.New("diagnostic target is not serializable") }

type HostQueries interface {
	Bundle(context.Context, LocalTarget, BuildIdentity) Datum
	Service(context.Context, LocalTarget) Datum
	Relay(context.Context, LocalTarget) Datum
	Herdr(context.Context, LocalTarget) Datum
	Pairing(context.Context, LocalTarget) Datum // running private aggregate only; no store opener
}

type TransportQueries interface {
	Cloudflare(context.Context, LocalTarget) Datum
	Gateway(context.Context, LocalTarget) Datum
	ManagedStatus(context.Context, LocalTarget) Datum // no activate/admit/watch creation
	CLIStatus(context.Context, LocalTarget) Datum // NEVER dispatched without a bound workflow; none in this slice
}

type IngressQueries interface {
	Health(context.Context, IngressTarget) Datum
	CompleteBundle(context.Context, IngressTarget, BuildIdentity) Datum
}

type Dependencies struct {
	Host HostQueries
	Transport TransportQueries
	Ingress IngressQueries
}

func policy(cfg Inspection, opt Options) map[CheckID]Check {
	checks := make(map[CheckID]Check)
	for _, id := range checkOrder {
		checks[id] = Check{ID: id, Category: category(id), Applicable: true, Required: true,
			State: Unknown, Evidence: EvidenceUnavailable, Source: SourceNone}
	}
	for _, id := range []CheckID{CheckCloudflare, CheckGateway, CheckManaged, CheckCLI, CheckIngress, CheckIngressBundle} {
		checks[id] = Check{ID: id, Category: category(id), State: NotApplicable,
			Evidence: EvidenceNotApplicable, Source: SourceNone}
	}
	selected := CheckID("")
	if cfg.code == ConfigValid {
		switch cfg.mode {
		case ModeCloudflare: selected = CheckCloudflare
		case ModeGateway: selected = CheckGateway
		case ModeManaged: selected = CheckManaged
		case ModeCLI: selected = CheckCLI
		}
	}
	if selected != "" {
		c := checks[selected]
		c.Applicable, c.Required, c.State, c.Evidence = true, true, Unknown, EvidenceUnavailable
		if selected == CheckCLI { c.Evidence = EvidenceDisabled }
		checks[selected] = c
	}
	if opt.ProbeIngress {
		for _, id := range []CheckID{CheckIngress, CheckIngressBundle} {
			c := checks[id]
			c.Applicable, c.Required, c.State, c.Evidence = true, true, Unknown, EvidenceUnavailable
			checks[id] = c
		}
	}
	checks[CheckPhone] = Check{ID: CheckPhone, Category: category(CheckPhone), Applicable: true,
		State: Unknown, Evidence: EvidencePhone, Source: SourceNone}
	return checks
}

// Run schedules sequential read-only observations. It launches no goroutines,
// subprocesses, HTTP/CLI/socket work or mutations itself. Injected adapters MUST
// honor cancellation and output bounds. A noncooperative callback can overrun;
// its result is discarded and subsequent calls stop, not a hard wall-clock promise.
func Run(ctx context.Context, cfg Inspection, build BuildIdentity, opt Options, deps Dependencies) Report {
	return run(ctx, cfg, build, opt, deps, time.Now)
}

func absent(value any) bool {
	if value == nil { return true }
	v := reflect.ValueOf(value)
	switch v.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map, reflect.Pointer, reflect.Slice:
		return v.IsNil()
	default:
		return false
	}
}

func contextEnded(ctx context.Context) bool {
	if ctx.Err() != nil { return true }
	deadline, bounded := ctx.Deadline()
	return bounded && !time.Now().Before(deadline)
}

func run(parent context.Context, cfg Inspection, build BuildIdentity, opt Options, deps Dependencies, now func() time.Time) Report {
	checks := policy(cfg, opt)
	invalid := opt.Budget < 0 || opt.Budget > GlobalBudget || opt.PerCall < 0 || opt.PerCall > CallBudget || absent(parent)
	if invalid { return finalize(cfg, build, now(), true, checks) }
	budget, callBudget := opt.Budget, opt.PerCall
	if budget == 0 { budget = GlobalBudget }
	if callBudget == 0 { callBudget = CallBudget }
	ctx, cancel := context.WithTimeout(parent, budget)
	defer cancel()
	exhausted := false
	call := func(id CheckID, completeBundle bool, query func(context.Context) Datum) {
		c := checks[id]
		if exhausted || contextEnded(ctx) {
			exhausted = true
			c.State, c.Evidence, c.Source = Unknown, EvidenceCancelled, SourceNone
			checks[id] = c
			return
		}
		if query == nil { return }
		child := ctx
		stop := func() {}
		if !completeBundle { child, stop = context.WithTimeout(ctx, callBudget) }
		start := now()
		result := query(child)
		end := now()
		if contextEnded(child) || contextEnded(ctx) {
			exhausted = true
			c.State, c.Evidence, c.Source = Unknown, EvidenceCancelled, SourceNone
		} else {
			d, age, duration := normalizeDatum(result, end, end.Sub(start))
			c.State, c.Evidence, c.Source = d.State, d.Evidence, d.Source
			c.AgeSeconds, c.DurationMS = age, duration
			c.observedAt = d.At.UTC()
		}
		stop()
		checks[id] = c
	}
	// Unknown/invalid configuration cannot dispatch transport reads or acquire
	// guessed targets. Even a forged package-local mode gets no transport seam.
	validMode := cfg.mode == ModeCloudflare || cfg.mode == ModeGateway || cfg.mode == ModeManaged || cfg.mode == ModeBYO || cfg.mode == ModeCLI
	if cfg.code != ConfigValid || !validMode {
		return finalize(cfg, build, now(), false, checks)
	}
	target := selectedLocalTarget(cfg, build)
	for _, id := range []CheckID{CheckBundle, CheckService, CheckRelay, CheckHerdr, CheckPairing} {
		var query func(context.Context) Datum
		if !absent(deps.Host) {
			switch id {
			case CheckBundle: query = func(ctx context.Context) Datum { return deps.Host.Bundle(ctx, target, safeBuild(build)) }
			case CheckService: query = func(ctx context.Context) Datum { return deps.Host.Service(ctx, target) }
			case CheckRelay: query = func(ctx context.Context) Datum { return deps.Host.Relay(ctx, target) }
			case CheckHerdr: query = func(ctx context.Context) Datum { return deps.Host.Herdr(ctx, target) }
			case CheckPairing: query = func(ctx context.Context) Datum { return deps.Host.Pairing(ctx, target) }
			}
		}
		call(id, id == CheckBundle, query)
	}
	// Mode policy precedes even interface dispatch. BYO and production CLI make
	// zero Tailscale/LocalAPI/CLI calls on every path, including opt-in ingress.
	var id CheckID
	var query func(context.Context) Datum
	switch cfg.mode {
	case ModeCloudflare:
		id = CheckCloudflare
		if !absent(deps.Transport) { query = func(ctx context.Context) Datum { return deps.Transport.Cloudflare(ctx, target) } }
	case ModeGateway:
		id = CheckGateway
		if !absent(deps.Transport) { query = func(ctx context.Context) Datum { return deps.Transport.Gateway(ctx, target) } }
	case ModeManaged:
		id = CheckManaged
		if !absent(deps.Transport) { query = func(ctx context.Context) Datum { return deps.Transport.ManagedStatus(ctx, target) } }
	}
	if id != "" { call(id, false, query) }
	if opt.ProbeIngress && cfg.mode != ModeCLI {
		healthOrigin, appOrigin := cfg.relayOrigin, cfg.appOrigin
		if cfg.mode == ModeGateway && len(cfg.gateways) > 0 {
			// Only trusted HTTPS gateway GETs are admissible here, not a WSS
			// registration or insecure loopback WS synthetic phone challenge.
			if strings.HasPrefix(cfg.gateways[0], "wss://") { healthOrigin = strings.Replace(cfg.gateways[0], "wss://", "https://", 1) }
		}
		if appOrigin == "" && (cfg.mode == ModeBYO || cfg.mode == ModeManaged) { appOrigin = cfg.relayOrigin }
		var health, bundle func(context.Context) Datum
		if !absent(deps.Ingress) && healthOrigin != "" {
			health = func(ctx context.Context) Datum { return deps.Ingress.Health(ctx, IngressTarget{healthOrigin}) }
		}
		if !absent(deps.Ingress) && appOrigin != "" && validBuild(build) {
			bundle = func(ctx context.Context) Datum { return deps.Ingress.CompleteBundle(ctx, IngressTarget{appOrigin}, build) }
		}
		call(CheckIngress, false, health)
		call(CheckIngressBundle, true, bundle)
	}
	return finalize(cfg, build, now(), false, checks)
}

func safeBuild(b BuildIdentity) BuildIdentity {
	if validBuild(b) { return b }
	return BuildIdentity{"unknown", "unknown", "unknown", "unknown"}
}
