package reachability

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/0cv/herdr-mobile-relay/internal/portmap"
)

func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// fakeGateway implements the gateway endpoints the prober depends on.
type fakeGateway struct {
	// send controls whether /probe actually emits the datagram.
	send bool
	// decoy makes /probe emit an unrelated datagram first.
	decoy bool
	// whoami is the body returned by GET /whoami.
	whoami string
	// whoamiStatus overrides the /whoami status code when non-zero.
	whoamiStatus int

	mu     sync.Mutex
	ports  []uint16
	tokens []string
	errs   []string
}

func (g *fakeGateway) routes(t *testing.T) http.Handler {
	t.Helper()

	mux := http.NewServeMux()
	mux.HandleFunc("/whoami", func(w http.ResponseWriter, r *http.Request) {
		if g.whoamiStatus != 0 {
			w.WriteHeader(g.whoamiStatus)
		}
		io.WriteString(w, g.whoami)
	})
	mux.HandleFunc("/probe", func(w http.ResponseWriter, r *http.Request) {
		var req probeRequest
		body, err := io.ReadAll(io.LimitReader(r.Body, maxResponseBytes))
		if err != nil || json.Unmarshal(body, &req) != nil {
			http.Error(w, `{"error":"bad_request"}`, http.StatusBadRequest)
			return
		}
		token, err := base64.RawURLEncoding.DecodeString(req.Token)
		if err != nil || len(token) != tokenBytes {
			g.record(0, req.Token, "malformed token")
			http.Error(w, `{"error":"bad_token"}`, http.StatusBadRequest)
			return
		}
		if req.Port < minProbePort {
			g.record(req.Port, req.Token, "invalid port")
			http.Error(w, `{"error":"invalid_port"}`, http.StatusBadRequest)
			return
		}

		host, _, err := net.SplitHostPort(r.RemoteAddr)
		if err != nil {
			http.Error(w, `{"error":"no_source"}`, http.StatusInternalServerError)
			return
		}
		g.record(req.Port, req.Token, "")

		if g.send {
			conn, err := net.Dial("udp", net.JoinHostPort(host, itoa(req.Port)))
			if err != nil {
				http.Error(w, `{"error":"dial"}`, http.StatusInternalServerError)
				return
			}
			defer conn.Close()
			if g.decoy {
				conn.Write([]byte("not-the-token"))
			}
			conn.Write(token)
		}

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(probeResponse{Sent: true, ObservedIP: host})
	})
	return mux
}

func (g *fakeGateway) record(port uint16, token, failure string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.ports = append(g.ports, port)
	g.tokens = append(g.tokens, token)
	if failure != "" {
		g.errs = append(g.errs, failure)
	}
}

func (g *fakeGateway) snapshot() ([]uint16, []string, []string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	return append([]uint16(nil), g.ports...), append([]string(nil), g.tokens...), append([]string(nil), g.errs...)
}

func itoa(port uint16) string {
	return strconv.FormatUint(uint64(port), 10)
}

func TestHTTPBase(t *testing.T) {
	tests := []struct {
		in   string
		want string
	}{
		{in: "wss://gw.example.com", want: "https://gw.example.com"},
		{in: "WSS://gw.example.com/", want: "https://gw.example.com"},
		{in: "ws://127.0.0.1:8080", want: "http://127.0.0.1:8080"},
		{in: "https://gw.example.com//", want: "https://gw.example.com"},
		{in: "  http://gw.example.com  ", want: "http://gw.example.com"},
	}
	for _, tc := range tests {
		got, err := httpBase(tc.in)
		if err != nil {
			t.Fatalf("httpBase(%q): %v", tc.in, err)
		}
		if got != tc.want {
			t.Errorf("httpBase(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}

	for _, bad := range []string{"", "   ", "gw.example.com", "ftp://gw.example.com", "wss://"} {
		if got, err := httpBase(bad); err == nil {
			t.Errorf("httpBase(%q) = %q, want an error", bad, got)
		}
	}
}

func TestPublicIP(t *testing.T) {
	gateway := &fakeGateway{whoami: `{"ip":"203.0.113.9"}`}
	server := httptest.NewServer(gateway.routes(t))
	defer server.Close()

	prober := NewProber(server.URL, testLogger())
	addr, err := prober.PublicIP(context.Background())
	if err != nil {
		t.Fatalf("PublicIP: %v", err)
	}
	if addr != netip.MustParseAddr("203.0.113.9") {
		t.Fatalf("PublicIP = %s, want 203.0.113.9", addr)
	}
}

func TestPublicIPErrors(t *testing.T) {
	tests := []struct {
		name    string
		gateway *fakeGateway
	}{
		{name: "malformed json", gateway: &fakeGateway{whoami: "{not json"}},
		{name: "malformed ip", gateway: &fakeGateway{whoami: `{"ip":"not-an-ip"}`}},
		{name: "empty ip", gateway: &fakeGateway{whoami: `{}`}},
		{name: "server error", gateway: &fakeGateway{whoami: `{"ip":"203.0.113.9"}`, whoamiStatus: http.StatusBadGateway}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(tc.gateway.routes(t))
			defer server.Close()

			prober := NewProber(server.URL, testLogger())
			if addr, err := prober.PublicIP(context.Background()); err == nil {
				t.Fatalf("PublicIP = %s, want an error", addr)
			}
		})
	}

	t.Run("unreachable gateway", func(t *testing.T) {
		server := httptest.NewServer((&fakeGateway{whoami: "{}"}).routes(t))
		url := server.URL
		server.Close()

		prober := NewProber(url, testLogger())
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		if _, err := prober.PublicIP(ctx); err == nil {
			t.Fatal("PublicIP succeeded against a closed gateway")
		}
	})

	t.Run("bad gateway url", func(t *testing.T) {
		prober := NewProber("nonsense", testLogger())
		if _, err := prober.PublicIP(context.Background()); err == nil {
			t.Fatal("PublicIP accepted a malformed gateway url")
		}
		if _, err := prober.SelfTest(context.Background(), time.Second); err == nil {
			t.Fatal("SelfTest accepted a malformed gateway url")
		}
	})
}

func TestSelfTestReachableWithPortMapping(t *testing.T) {
	gateway := &fakeGateway{send: true, decoy: true, whoami: `{"ip":"203.0.113.9"}`}
	server := httptest.NewServer(gateway.routes(t))
	defer server.Close()

	prober := NewProber(server.URL, testLogger())

	var mapped uint16
	prober.mapUDP = func(_ context.Context, internalPort uint16, lifetime time.Duration) (*portmap.Mapping, error) {
		mapped = internalPort
		if lifetime != probeLifetime {
			t.Errorf("mapping lifetime = %s, want %s", lifetime, probeLifetime)
		}
		// A loopback test cannot translate ports, so the external port has
		// to stay the bound one; the address is what the router reported.
		return &portmap.Mapping{
			External:  netip.AddrPortFrom(netip.MustParseAddr("203.0.113.11"), internalPort),
			Internal:  internalPort,
			Method:    portmap.MethodPCP,
			Lifetime:  lifetime,
			ExpiresAt: time.Now().Add(lifetime),
		}, nil
	}

	result, err := prober.SelfTest(context.Background(), 5*time.Second)
	if err != nil {
		t.Fatalf("SelfTest: %v", err)
	}
	if !result.Reachable {
		t.Fatalf("result = %+v, want reachable", result)
	}
	if result.Method != portmap.MethodPCP {
		t.Errorf("method = %s, want %s", result.Method, portmap.MethodPCP)
	}
	if result.ExternalIP != "203.0.113.11" {
		t.Errorf("external ip = %s, want the mapped address", result.ExternalIP)
	}
	if result.ExternalPort != mapped || mapped == 0 {
		t.Errorf("external port = %d, want the mapped port %d", result.ExternalPort, mapped)
	}
	if result.CheckedAt.IsZero() {
		t.Error("CheckedAt not set")
	}
	if result.Detail == "" {
		t.Error("Detail not set on success")
	}

	ports, tokens, failures := gateway.snapshot()
	if len(ports) != 1 || ports[0] != mapped {
		t.Fatalf("gateway saw ports %v, want [%d]", ports, mapped)
	}
	if len(failures) != 0 {
		t.Fatalf("gateway rejected the request: %v", failures)
	}
	if decoded, err := base64.RawURLEncoding.DecodeString(tokens[0]); err != nil || len(decoded) != tokenBytes {
		t.Fatalf("token %q is not %d unpadded base64url bytes", tokens[0], tokenBytes)
	}
}

func TestSelfTestReachableWithoutPortMapping(t *testing.T) {
	gateway := &fakeGateway{send: true}
	server := httptest.NewServer(gateway.routes(t))
	defer server.Close()

	prober := NewProber(server.URL, testLogger())
	prober.mapUDP = func(context.Context, uint16, time.Duration) (*portmap.Mapping, error) {
		return nil, errors.New("no router here")
	}

	result, err := prober.SelfTest(context.Background(), 5*time.Second)
	if err != nil {
		t.Fatalf("SelfTest: %v", err)
	}
	if !result.Reachable {
		t.Fatalf("result = %+v, want reachable", result)
	}
	if result.Method != MethodDirect {
		t.Errorf("method = %s, want %s", result.Method, MethodDirect)
	}
	if result.ExternalIP != "127.0.0.1" {
		t.Errorf("external ip = %s, want the address the gateway observed", result.ExternalIP)
	}
}

func TestSelfTestTimesOutCleanly(t *testing.T) {
	gateway := &fakeGateway{send: false}
	server := httptest.NewServer(gateway.routes(t))
	defer server.Close()

	prober := NewProber(server.URL, testLogger())
	prober.mapUDP = func(context.Context, uint16, time.Duration) (*portmap.Mapping, error) {
		return nil, errors.New("no router here")
	}

	start := time.Now()
	result, err := prober.SelfTest(context.Background(), 300*time.Millisecond)
	elapsed := time.Since(start)

	if err != nil {
		t.Fatalf("SelfTest returned an error for a plain timeout: %v", err)
	}
	if result.Reachable {
		t.Fatal("result claims reachability without a datagram")
	}
	if !strings.Contains(result.Detail, "no probe datagram") {
		t.Errorf("detail = %q, want the timeout explanation", result.Detail)
	}
	if result.ExternalPort == 0 {
		t.Error("external port missing from a failed result")
	}
	if elapsed > 3*time.Second {
		t.Fatalf("SelfTest took %s, want it bounded by the timeout", elapsed)
	}
}

func TestSelfTestStopsWithContext(t *testing.T) {
	gateway := &fakeGateway{send: false}
	server := httptest.NewServer(gateway.routes(t))
	defer server.Close()

	prober := NewProber(server.URL, testLogger())
	prober.mapUDP = func(context.Context, uint16, time.Duration) (*portmap.Mapping, error) {
		return nil, errors.New("no router here")
	}

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(150 * time.Millisecond)
		cancel()
	}()

	start := time.Now()
	result, err := prober.SelfTest(ctx, time.Minute)
	if err != nil {
		t.Fatalf("SelfTest: %v", err)
	}
	if result.Reachable {
		t.Fatal("cancelled self-test claims reachability")
	}
	if elapsed := time.Since(start); elapsed > 10*time.Second {
		t.Fatalf("SelfTest ignored cancellation for %s", elapsed)
	}
}

func TestSelfTestGatewayRefusal(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/probe", func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"error":"rate_limited"}`, http.StatusTooManyRequests)
	})
	server := httptest.NewServer(mux)
	defer server.Close()

	prober := NewProber(server.URL, testLogger())
	prober.mapUDP = func(context.Context, uint16, time.Duration) (*portmap.Mapping, error) {
		return nil, errors.New("no router here")
	}

	result, err := prober.SelfTest(context.Background(), 5*time.Second)
	if err == nil {
		t.Fatal("SelfTest hid a gateway error")
	}
	if result.Reachable {
		t.Fatal("refused self-test claims reachability")
	}
	if result.Detail == "" {
		t.Error("Detail not set on a gateway failure")
	}
}

func TestResultJSONTags(t *testing.T) {
	encoded, err := json.Marshal(Result{
		Reachable:    true,
		Method:       portmap.MethodNATPMP,
		ExternalIP:   "203.0.113.11",
		ExternalPort: 50000,
		Detail:       "ok",
		CheckedAt:    time.Unix(1700000000, 0).UTC(),
	})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	want := `{"reachable":true,"method":"nat-pmp","external_ip":"203.0.113.11","external_port":50000,"detail":"ok","checked_at":"2023-11-14T22:13:20Z"}`
	if string(encoded) != want {
		t.Fatalf("json = %s\nwant %s", encoded, want)
	}
}
