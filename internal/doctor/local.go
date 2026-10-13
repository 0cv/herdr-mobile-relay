package doctor

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net"
	"net/http"
	"strconv"
	"time"
	"unicode/utf8"

	"github.com/0cv/herdr-mobile-relay/internal/protocol"
)

const (
	maxLocalBody = 64 * 1024
	maxLocalHeaders = 16 * 1024
	maxJSONDepth = 16
	maxJSONMembers = 128
	maxJSONItems = 128
	maxJSONNodes = 2048
	maxJSONString = 2048
)

// LocalQueries observes only selected ordinary local relay identities. It has
// no command, store, service, Tailscale or remote-ingress implementation. In
// particular, endpoint metadata is not an installed bundle verification.
// A loopback identity is cooperative attribution, not authentication against
// a malicious same-user server. This never proves phone/direct-path health.
// Each observation owns a fresh constrained transport; no cache or retry.
type LocalQueries struct {
	target LocalTarget
	build BuildIdentity
	gateways []string
	bound bool
	// Package-private fixture seam; never supplied by the production constructor.
	fixture http.RoundTripper
	herdrFixture *herdrHooks // private ping fixture only; nil in production
}

func NewLocalQueries(cfg Inspection, build BuildIdentity) *LocalQueries {
	q := &LocalQueries{target: selectedLocalTarget(cfg, build),
		build: build, gateways: append([]string(nil), cfg.gateways...)}
	q.bound = cfg.Code() == ConfigValid && validInstance(cfg.instance) && validBuild(build) &&
		cfg.port > 0 && cfg.port <= 65535 && cfg.pluginPort > 0 && cfg.pluginPort <= 65535 &&
		(cfg.mode == ModeCloudflare || cfg.mode == ModeGateway || cfg.mode == ModeBYO)
	if (cfg.mode == ModeGateway) != (len(q.gateways) > 0) || len(q.gateways) > MaxGateways { q.bound = false }
	for i, origin := range q.gateways {
		canonical, ok := gatewayOrigin(origin)
		if !ok || canonical != origin { q.bound = false }
		for _, prior := range q.gateways[:i] { if prior == origin { q.bound = false } }
	}
	return q
}
func (LocalQueries) String() string { return "private local diagnostic queries" }
func (LocalQueries) GoString() string { return "private local diagnostic queries" }
func (LocalQueries) MarshalJSON() ([]byte, error) { return nil, errors.New("diagnostic queries are not serializable") }

var _ HostQueries = (*LocalQueries)(nil)
var _ TransportQueries = (*LocalQueries)(nil)

func localDatum(state State, evidence Evidence) Datum { return Datum{state, evidence, SourceQuery, time.Now()} }
func localUnknown() Datum { return localDatum(Unknown, EvidenceUnavailable) }
func (*LocalQueries) Bundle(context.Context, LocalTarget, BuildIdentity) Datum { return localUnknown() }
func (*LocalQueries) Service(context.Context, LocalTarget) Datum { return localUnknown() }
func (q *LocalQueries) Herdr(ctx context.Context, target LocalTarget) Datum { return q.observeHerdr(ctx, target) }
func (*LocalQueries) Pairing(context.Context, LocalTarget) Datum { return localUnknown() }
func (*LocalQueries) Cloudflare(context.Context, LocalTarget) Datum { return localUnknown() }
func (*LocalQueries) ManagedStatus(context.Context, LocalTarget) Datum { return localUnknown() }
func (*LocalQueries) CLIStatus(context.Context, LocalTarget) Datum { return localUnknown() }
func (q *LocalQueries) Relay(ctx context.Context, target LocalTarget) Datum { return q.observe(ctx, target, false) }
func (q *LocalQueries) Gateway(ctx context.Context, target LocalTarget) Datum { return q.observe(ctx, target, true) }

// Only these three GETs are possible. Dial also pins the literal address, so
// even transport internals cannot select DNS, a proxy, or a different listener.
func localTransport(ctx context.Context, address string) *http.Transport {
	return &http.Transport{
		Proxy: nil,
		DialContext: func(call context.Context, network, destination string) (net.Conn, error) {
			if (network != "tcp" && network != "tcp4") || destination != address { return nil, errors.New("diagnostic dial refused") }
			dialer := net.Dialer{Timeout: time.Second, KeepAlive: -1}
			conn, err := dialer.DialContext(call, "tcp4", address)
			if err != nil { return nil, err }
			deadline, ok := ctx.Deadline()
			if !ok { conn.Close(); return nil, errors.New("diagnostic deadline unavailable") }
			if err := conn.SetDeadline(deadline); err != nil { conn.Close(); return nil, err }
			return conn, nil
		},
		DisableKeepAlives: true, // no reused connection, hence no implicit retry
		DisableCompression: true,
		ForceAttemptHTTP2: false,
		MaxResponseHeaderBytes: maxLocalHeaders,
		ResponseHeaderTimeout: time.Second,
		MaxConnsPerHost: 1,
		ReadBufferSize: 4096,
	}
}

type localResponse struct { status int; header http.Header; body []byte }

// The fixture path gets the same request, client, redirect policy, context and
// response projection. Only the production path owns the actual dial transport.
func (q *LocalQueries) get(ctx context.Context, client *http.Client, path string) (localResponse, bool, Evidence) {
	if contextEnded(ctx) { return localResponse{}, false, EvidenceUnavailable }
	if path != "/health" && path != "/healthz" && path != "/readyz" { return localResponse{}, false, EvidenceMalformed }
	url := "http://127.0.0.1:" + strconv.Itoa(q.target.port) + path
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil { return localResponse{}, false, EvidenceUnavailable }
	req.Close = true
	req.Header.Set("User-Agent", "herdr-doctor")
	req.Header.Set("Accept-Encoding", "identity")
	if path == "/health" { req.Header.Set("Accept", "text/plain") } else { req.Header.Set("Accept", "application/json") }
	resp, err := client.Do(req)
	if resp != nil && resp.Body != nil { defer resp.Body.Close() }
	if err != nil || resp == nil || resp.Body == nil { return localResponse{}, false, EvidenceUnavailable }
	if !boundedHeaders(resp.Header) || resp.ContentLength > maxLocalBody { return localResponse{}, false, EvidenceMalformed }
	encoding := resp.Header.Values("Content-Encoding")
	if len(encoding) > 1 || (len(encoding) == 1 && encoding[0] != "identity") { return localResponse{}, false, EvidenceMalformed }
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxLocalBody+1))
	if err != nil || contextEnded(ctx) { return localResponse{}, false, EvidenceUnavailable }
	if len(body) > maxLocalBody || (resp.ContentLength >= 0 && resp.ContentLength != int64(len(body))) { return localResponse{}, false, EvidenceMalformed }
	return localResponse{resp.StatusCode, resp.Header, body}, true, EvidenceHealthy
}

func boundedHeaders(h http.Header) bool {
	count, size := 0, 0
	for key, values := range h {
		for _, value := range values {
			count++
			size += len(key) + len(value) + 4
			if count > 128 || size > maxLocalHeaders { return false }
		}
	}
	return true
}
func localMedia(h http.Header, want string) bool {
	values := h.Values("Content-Type")
	if len(values) != 1 { return false }
	kind, _, err := mime.ParseMediaType(values[0])
	return err == nil && kind == want
}
func localInstanceHeader(h http.Header, instance string, required bool) bool {
	values := h.Values("X-Herdr-Relay-Instance")
	// The actual /readyz endpoint has a body identity but no instance header.
	if len(values) == 0 { return !required }
	return len(values) == 1 && values[0] == instance
}

func (q *LocalQueries) observe(parent context.Context, target LocalTarget, gateway bool) Datum {
	// Equality includes the runner's expected build and complete selected
	// gateways, not merely listener/instance identity. A stale adapter cannot
	// probe against its old expectations under a newly described report.
	if q == nil || !q.bound || target != q.target || absent(parent) ||
		(gateway && target.mode != ModeGateway) || contextEnded(parent) { return localUnknown() }
	ctx, cancel := context.WithTimeout(parent, CallBudget)
	defer cancel()
	address := net.JoinHostPort("127.0.0.1", strconv.Itoa(target.port))
	transport := localTransport(ctx, address)
	defer transport.CloseIdleConnections()
	var rt http.RoundTripper = transport
	if q.fixture != nil { rt = q.fixture }
	client := &http.Client{Transport: rt, Timeout: CallBudget,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	health, ok, evidence := q.get(ctx, client, "/health")
	if !ok { return localDatum(Unknown, evidence) }
	if health.status != http.StatusOK || !localMedia(health.header, "text/plain") ||
		!localInstanceHeader(health.header, target.instance, true) || string(health.body) != "ok\n" {
		return localDatum(Unknown, EvidenceMalformed)
	}
	hz, ok, evidence := q.get(ctx, client, "/healthz")
	if !ok { return localDatum(Unknown, evidence) }
	if hz.status != http.StatusOK || !localMedia(hz.header, "application/json") || !localInstanceHeader(hz.header, target.instance, true) {
		return localDatum(Unknown, EvidenceMalformed)
	}
	h, ok := localObject(hz.body)
	if !ok || !q.identity(h) || h["status"] != "ok" || h["transport"] != string(target.mode) { return localDatum(Unknown, EvidenceMalformed) }
	hi, ok := localInventory(h["inventory"])
	readiness, typed := h["readiness"].(string)
	if !ok || !typed || !compatibleReadiness(readiness, hi.state) { return localDatum(Unknown, EvidenceMalformed) }
	rz, ok, evidence := q.get(ctx, client, "/readyz")
	if !ok { return localDatum(Unknown, evidence) }
	if !localMedia(rz.header, "application/json") || !localInstanceHeader(rz.header, target.instance, false) { return localDatum(Unknown, EvidenceMalformed) }
	r, ok := localObject(rz.body)
	if !ok || !q.identity(r) || h["bundle_build"] != r["bundle_build"] { return localDatum(Unknown, EvidenceMalformed) }
	if mode, found := r["transport"]; found && mode != string(target.mode) { return localDatum(Unknown, EvidenceMalformed) }
	ri, ok := localInventory(r["inventory"])
	if !ok || hi.state != ri.state || hi.stale != ri.stale || hi.code != ri.code { return localDatum(Unknown, EvidenceMalformed) }
	// Finite matrix: complete exact starting/degraded + unavailable/503 is
	// observed failure. Missing, malformed, foreign, contradictory or unknown
	// evidence is unknown, never a manufactured failure or healthy observation.
	ready := readiness == "ready"
	if ready {
		if r["status"] != "ready" || rz.status != http.StatusOK { return localDatum(Unknown, EvidenceMalformed) }
	} else {
		if r["status"] != "unavailable" || rz.status != http.StatusServiceUnavailable { return localDatum(Unknown, EvidenceMalformed) }
	}
	state := OK
	if !ready { state = Error }
	if gateway {
		registration, valid := q.registration(h)
		if !valid { return localDatum(Unknown, EvidenceMalformed) }
		if !registration { state = Error }
	}
	if contextEnded(ctx) { return localUnknown() }
	if state == Error { return localDatum(Error, EvidenceFailed) }
	return localDatum(OK, EvidenceHealthy)
}

func (q *LocalQueries) identity(object map[string]any) bool {
	for key, expected := range map[string]string{
		"instance": q.target.instance, "version": q.build.Version, "release_version": q.build.Version,
		"revision": q.build.Revision, "bundle_hash": q.build.WebHash,
		"bundle_version": q.build.Version, "bundle_revision": q.build.Revision,
	} {
		if object[key] != expected { return false }
	}
	p, ok := localInteger(object["protocol"])
	if !ok || p != int64(protocol.Version) { return false }
	// web.Handler.BundleBuild is a SHA-256 string, not the asset counter.
	// Expected WebHash binds the complete packaged web tree; this additional
	// descriptor identity must be well formed and agree across both endpoints.
	bundleBuild, typed := object["bundle_build"].(string)
	return typed && hexField(bundleBuild, 64)
}

type localInventoryValue struct { state string; stale bool; code string }
func localInventory(value any) (localInventoryValue, bool) {
	object, ok := value.(map[string]any)
	if !ok { return localInventoryValue{}, false }
	state, s := object["state"].(string)
	stale, b := object["stale"].(bool)
	code, c := object["error_code"].(string)
	attempt, a := localInteger(object["last_attempt_at"])
	success, l := localInteger(object["last_success_at"])
	if !s || !b || !c || !a || !l || attempt < 0 || success < 0 { return localInventoryValue{}, false }
	switch state {
	case "ready": if stale || code != "" || success == 0 { return localInventoryValue{}, false }
	case "starting": if code != "" { return localInventoryValue{}, false }
	case "error":
		if code != "server_not_running" && code != "command_failed" && code != "topology_churn" { return localInventoryValue{}, false }
	default: return localInventoryValue{}, false
	}
	if stale != (state != "ready" && success != 0) { return localInventoryValue{}, false }
	// Endpoint timestamps describe its inventory only, never observation time.
	return localInventoryValue{state, stale, code}, true
}
func compatibleReadiness(readiness, inventory string) bool {
	switch readiness {
	case "ready": return inventory == "ready"
	case "degraded": return inventory == "error"
	case "starting": return true // app-ready flag may still be false
	default: return false
	}
}
func localInteger(value any) (int64, bool) {
	number, ok := value.(json.Number)
	if !ok { return 0, false }
	n, err := number.Int64()
	return n, err == nil
}

func (q *LocalQueries) registration(health map[string]any) (bool, bool) {
	object, ok := health["gateway"].(map[string]any)
	if !ok { return false, false }
	enabled, e := object["enabled"].(bool)
	registered, r := object["registered"].(bool)
	if !e || !r || (!enabled && registered) { return false, false }
	if !enabled { return false, true }
	id, i := object["relay_id"].(string)
	if !i { return false, false }
	// Actual gatewaywire registration ID: 16 raw bytes, canonical base64url.
	if id != "" {
		decoded, err := base64.RawURLEncoding.Strict().DecodeString(id)
		if err != nil || len(decoded) != 16 || len(id) != 22 { return false, false }
	} else if registered { return false, false }
	url, u := object["url"].(string)
	urls, us := object["urls"].([]any)
	if !u || !us || len(urls) != len(q.gateways) || len(urls) == 0 { return false, false }
	active := -1
	for index, configured := range q.gateways {
		if configured == url { active = index; break }
	}
	if active < 0 || urls[0] != url { return false, false }
	// GatewayClient.activeFirstLocked promotes exactly the active candidate,
	// then retains configured order for all cold fallbacks (not a rotation).
	// This also refuses duplicate, foreign and wrongly typed candidate entries.
	next := 1
	for index, configured := range q.gateways {
		if index == active { continue }
		if urls[next] != configured { return false, false }
		next++
	}
	// /healthz repeats gateway metadata at top level. If supplied, these
	// projections must agree; they never grant remote gateway qualification.
	for top, nested := range map[string]string{"gateway_url": "url", "gateway_version": "version", "gateway_revision": "revision"} {
		if value, supplied := health[top]; supplied {
			text, typed := value.(string)
			other, complete := object[nested].(string)
			if !typed || !complete || text != other { return false, false }
		}
	}
	return registered, true
}

// Complete bounded JSON including additive fields. Duplicate keys at ANY level
// are refused, not silently last-wins. Nothing raw escapes this projection.
// Node/member/item/string/depth bounds apply even to fields we do not use.
func localObject(body []byte) (map[string]any, bool) {
	if len(body) > maxLocalBody || !utf8.Valid(body) { return nil, false }
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.UseNumber()
	nodes := 0
	value, ok := localJSONValue(decoder, 0, &nodes)
	if !ok { return nil, false }
	if _, err := decoder.Token(); err != io.EOF { return nil, false }
	object, ok := value.(map[string]any)
	return object, ok
}
func localJSONValue(decoder *json.Decoder, depth int, nodes *int) (any, bool) {
	*nodes = *nodes + 1
	if depth > maxJSONDepth || *nodes > maxJSONNodes { return nil, false }
	token, err := decoder.Token()
	if err != nil { return nil, false }
	if text, ok := token.(string); ok && len(text) > maxJSONString { return nil, false }
	delim, compound := token.(json.Delim)
	if !compound { return token, true }
	switch delim {
	case '{':
		object := make(map[string]any)
		for decoder.More() {
			key, err := decoder.Token()
			name, ok := key.(string)
			if err != nil || !ok || len(name) > maxJSONString || len(object) >= maxJSONMembers { return nil, false }
			if _, exists := object[name]; exists { return nil, false }
			value, ok := localJSONValue(decoder, depth+1, nodes)
			if !ok { return nil, false }
			object[name] = value
		}
		end, err := decoder.Token()
		return object, err == nil && end == json.Delim('}')
	case '[':
		array := make([]any, 0)
		for decoder.More() {
			if len(array) >= maxJSONItems { return nil, false }
			value, ok := localJSONValue(decoder, depth+1, nodes)
			if !ok { return nil, false }
			array = append(array, value)
		}
		end, err := decoder.Token()
		return array, err == nil && end == json.Delim(']')
	default: return nil, false
	}
}
