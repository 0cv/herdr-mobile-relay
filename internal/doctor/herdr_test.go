package doctor

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

const herdrValidPong = "{\"id\":\"herdr-doctor-ping-v1\",\"result\":{\"type\":\"pong\",\"version\":\"0.9.0\",\"protocol\":1}}\n"

func TestHerdrFixedRequestIsOnlyCorrelatedPingWithEmptyParams(t *testing.T) {
	if herdrPingRequest != "{\"id\":\"herdr-doctor-ping-v1\",\"method\":\"ping\",\"params\":{}}\n" {
		t.Fatal("fixed request changed authority or framing")
	}
	var request map[string]json.RawMessage
	if json.Unmarshal([]byte(herdrPingRequest), &request) != nil || len(request) != 3 || string(request["method"]) != `"ping"` || string(request["params"]) != `{}` || string(request["id"]) != `"herdr-doctor-ping-v1"` {
		t.Fatal("request admits extra authority or mutable correlation")
	}
}

func TestHerdrCompleteCorrelatedPongStrictProjection(t *testing.T) {
	for _, frame := range []string{
		herdrValidPong,
		strings.Replace(herdrValidPong, `"protocol":1`, `"protocol":65535`, 1),
		strings.Replace(herdrValidPong, `"0.9.0"`, `"99.0.0-private-node-canary+build"`, 1),
		strings.Replace(herdrValidPong, `"protocol":1`, `"protocol":1,"capabilities":{"surface_interest":false,"health_check":true,"endpoint_protocol_generation":2,"new":[{"opaque":"private-node-canary"}]}`, 1),
		strings.Replace(herdrValidPong, `"protocol":1`, `"protocol":1,"capabilities":{"surface_interest":null,"health_check":null,"endpoint_protocol_generation":null}`, 1),
		strings.Replace(herdrValidPong, `"result":`, `"additive":{"Result":{"secret":"secret-canary"},"unicode":"\ud83d\ude00"},"result":`, 1),
		strings.Replace(herdrValidPong, `"protocol":1`, `"protocol":1,"Protocol":999,"Version":"secret-canary"`, 1),
	} {
		if herdrPong([]byte(frame)) != EvidenceHerdrPing {
			t.Fatal("complete independent Herdr pong rejected")
		}
	}
	cases := []string{
		"", "[]\n", "null\n", strings.TrimSuffix(herdrValidPong, "\n"),
		strings.Replace(herdrValidPong, herdrPingID, "wrong-id-canary", 1),
		strings.Replace(herdrValidPong, `"id":"`+herdrPingID+`"`, `"id":7`, 1),
		strings.Replace(herdrValidPong, `"id":`, `"ID":`, 1),
		strings.Replace(herdrValidPong, `"result":`, `"Result":`, 1),
		strings.Replace(herdrValidPong, `"result":`, `"error":null,"result":`, 1),
		strings.Replace(herdrValidPong, `"result":`, `"error":{"code":"raw-error-canary","message":"secret-canary"},"result":`, 1),
		strings.Replace(herdrValidPong, `"type":"pong"`, `"type":"pong","type":"pong"`, 1),
		strings.Replace(herdrValidPong, `"id":`, `"id":"x","id":`, 1),
		strings.Replace(herdrValidPong, `"type":"pong"`, `"type":{}`, 1),
		strings.Replace(herdrValidPong, `"type":"pong"`, `"type":"pane_read"`, 1),
		strings.Replace(herdrValidPong, `"type":"pong",`, "", 1),
		strings.Replace(herdrValidPong, `"version":"0.9.0",`, "", 1),
		strings.Replace(herdrValidPong, `"version":"0.9.0"`, `"version":null`, 1),
		strings.Replace(herdrValidPong, `"version":"0.9.0"`, `"version":""`, 1),
		strings.Replace(herdrValidPong, `"version":"0.9.0"`, `"version":"0.9.0\u001b"`, 1),
		strings.Replace(herdrValidPong, `"version":"0.9.0"`, `"version":"unknown"`, 1),
		strings.Replace(herdrValidPong, `"version":"0.9.0"`, `"version":"1.2.3-`+strings.Repeat("a", 59)+`"`, 1),
		strings.Replace(herdrValidPong, `"protocol":1`, `"protocol":0`, 1),
		strings.Replace(herdrValidPong, `"protocol":1`, `"protocol":-1`, 1),
		strings.Replace(herdrValidPong, `"protocol":1`, `"protocol":65536`, 1),
		strings.Replace(herdrValidPong, `"protocol":1`, `"protocol":1.0`, 1),
		strings.Replace(herdrValidPong, `"protocol":1`, `"protocol":1e0`, 1),
		strings.Replace(herdrValidPong, `"protocol":1`, `"protocol":"1"`, 1),
		strings.Replace(herdrValidPong, `"protocol":1`, `"Protocol":1`, 1),
		strings.Replace(herdrValidPong, `"protocol":1`, `"protocol":null`, 1),
		strings.Replace(herdrValidPong, `"protocol":1`, `"protocol":9223372036854775808`, 1),
		strings.Replace(herdrValidPong, `"protocol":1`, `"protocol":1,"capabilities":[]`, 1),
		strings.Replace(herdrValidPong, `"protocol":1`, `"protocol":1,"capabilities":{"health_check":"yes"}`, 1),
		strings.Replace(herdrValidPong, `"protocol":1`, `"protocol":1,"capabilities":{"surface_interest":0}`, 1),
		strings.Replace(herdrValidPong, `"protocol":1`, `"protocol":1,"capabilities":{"endpoint_protocol_generation":-1}`, 1),
		strings.Replace(herdrValidPong, `"protocol":1`, `"protocol":1,"capabilities":{"endpoint_protocol_generation":1.5}`, 1),
		strings.Replace(herdrValidPong, `"protocol":1`, `"protocol":1,"capabilities":{"new":{"a":1,"a":2}}`, 1),
		strings.Replace(herdrValidPong, `"result":`, `"new":{"nested":{"k":1,"k":2}},"result":`, 1),
		strings.Replace(herdrValidPong, `"result":`, `"new":"\ud800","result":`, 1),
		strings.Replace(herdrValidPong, `"result":`, `"new":"\udfff","result":`, 1),
		strings.Replace(herdrValidPong, `"result":`, "\"new\":\"\xff\",\"result\":", 1),
		strings.TrimSuffix(herdrValidPong, "\n") + " {}\n",
		herdrValidPong + herdrValidPong, herdrValidPong + "\n", "\n" + herdrValidPong,
		"{\"id\":\"" + herdrPingID + "\",\"result\":null}\n",
		"{\"id\":\"" + herdrPingID + "\",\"error\":null}\n",
		"{\"id\":\"" + herdrPingID + "\",\"error\":{\"code\":3,\"message\":\"x\"}}\n",
	}
	for i, frame := range cases {
		t.Run(fmt.Sprintf("invalid-%d", i), func(t *testing.T) {
			if herdrPong([]byte(frame)) != EvidenceMalformed {
				t.Fatal("malformed/incomplete/conflicting pong accepted")
			}
		})
	}
	failure := "{\"id\":\"" + herdrPingID + "\",\"error\":{\"code\":\"server_error\",\"message\":\"secret-canary/private/path\"}}\n"
	if herdrPong([]byte(failure)) != EvidenceUnavailable {
		t.Fatal("correlated server error fabricated ping/failure authority")
	}
}

func TestHerdrParserBudgetsIncludeDiscardedAdditiveFields(t *testing.T) {
	add := func(raw string) string {
		return strings.Replace(herdrValidPong, `"result":`, `"future":`+raw+`,"result":`, 1)
	}
	object := func(n int) string {
		parts := []string{}
		for i := 0; i < n; i++ {
			parts = append(parts, fmt.Sprintf(`"k%d":0`, i))
		}
		return "{" + strings.Join(parts, ",") + "}"
	}
	array := func(n int) string { return "[" + strings.TrimSuffix(strings.Repeat("0,", n), ",") + "]" }
	for _, raw := range []string{object(maxJSONMembers), array(maxJSONItems), `"` + strings.Repeat("x", maxJSONString) + `"`, strings.Repeat("[", maxJSONDepth-1) + "0" + strings.Repeat("]", maxJSONDepth-1), "1" + strings.Repeat("0", 127)} {
		if herdrPong([]byte(add(raw))) != EvidenceHerdrPing {
			t.Fatal("exact compatible parser boundary rejected")
		}
	}
	for _, raw := range []string{object(maxJSONMembers + 1), array(maxJSONItems + 1), `"` + strings.Repeat("x", maxJSONString+1) + `"`, strings.Repeat("[", maxJSONDepth+1) + "0" + strings.Repeat("]", maxJSONDepth+1), "1" + strings.Repeat("0", 128), "[" + strings.TrimSuffix(strings.Repeat(array(128)+",", 16), ",") + "]"} {
		if herdrPong([]byte(add(raw))) != EvidenceMalformed {
			t.Fatal("discarded additive field bypassed finite budget")
		}
	}
	// Root/id/future/result/type/version/protocol contribute 7 nodes; fifteen
	// 128-element arrays contribute 1935, leaving 106 scalars for exactly 2048.
	nodes := "[" + strings.Repeat(array(128)+",", 15) + strings.TrimSuffix(strings.Repeat("0,", 106), ",") + "]"
	if herdrPong([]byte(add(nodes))) != EvidenceHerdrPing || herdrPong([]byte(add(strings.TrimSuffix(nodes, "]")+",0]"))) != EvidenceMalformed {
		t.Fatal("exact global node boundary incorrect")
	}
	exact := strings.TrimSuffix(herdrValidPong, "\n") + strings.Repeat(" ", maxHerdrFrame-len(herdrValidPong)) + "\n"
	if len(exact) != maxHerdrFrame || herdrPong([]byte(exact)) != EvidenceHerdrPing || herdrPong([]byte(" "+exact)) != EvidenceMalformed {
		t.Fatal("complete including-framing byte bound incorrect")
	}
}

type herdrMemoryConn struct {
	reader     io.Reader
	writes     []string
	short      bool
	writeError bool
	readBytes  int
}

func (c *herdrMemoryConn) Read(p []byte) (int, error) {
	n, e := c.reader.Read(p)
	c.readBytes += n
	return n, e
}
func (c *herdrMemoryConn) Write(p []byte) (int, error) {
	c.writes = append(c.writes, string(p))
	if c.writeError {
		return 0, errors.New("raw-error-canary")
	}
	if c.short {
		return len(p) - 1, nil
	}
	return len(p), nil
}
func (*herdrMemoryConn) Close() error                     { return nil }
func (*herdrMemoryConn) LocalAddr() net.Addr              { return &net.UnixAddr{Net: "unix"} }
func (*herdrMemoryConn) RemoteAddr() net.Addr             { return &net.UnixAddr{Net: "unix"} }
func (*herdrMemoryConn) SetDeadline(time.Time) error      { return nil }
func (*herdrMemoryConn) SetReadDeadline(time.Time) error  { return nil }
func (*herdrMemoryConn) SetWriteDeadline(time.Time) error { return nil }

func TestHerdrExchangeExactOneRequestFramingShortWritesAndBounds(t *testing.T) {
	exact := strings.TrimSuffix(herdrValidPong, "\n") + strings.Repeat(" ", maxHerdrFrame-len(herdrValidPong)) + "\n"
	for _, test := range []struct {
		frame             string
		short, writeError bool
		want              Evidence
	}{
		{herdrValidPong, false, false, EvidenceHerdrPing},
		{exact, false, false, EvidenceHerdrPing},
		{" " + exact, false, false, EvidenceMalformed},
		{strings.Repeat("x", maxHerdrFrame+100), false, false, EvidenceMalformed},
		{herdrValidPong + herdrValidPong, false, false, EvidenceMalformed},
		{herdrValidPong + " ", false, false, EvidenceMalformed},
		{strings.TrimSuffix(herdrValidPong, "\n"), false, false, EvidenceMalformed},
		{"", false, false, EvidenceUnavailable},
		{herdrValidPong, true, false, EvidenceUnavailable},
		{herdrValidPong, false, true, EvidenceUnavailable},
	} {
		conn := &herdrMemoryConn{reader: strings.NewReader(test.frame), short: test.short, writeError: test.writeError}
		if herdrExchange(context.Background(), conn) != test.want || len(conn.writes) != 1 || conn.writes[0] != herdrPingRequest || conn.readBytes > maxHerdrFrame {
			t.Fatal("exchange retried, changed request, overread or accepted prefix")
		}
		if (test.short || test.writeError) && conn.readBytes != 0 {
			t.Fatal("partial/failed write continued to read")
		}
	}
	badRead := &herdrMemoryConn{reader: errorReader{}}
	if herdrExchange(context.Background(), badRead) != EvidenceUnavailable {
		t.Fatal("read error accepted")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	conn := &herdrMemoryConn{reader: strings.NewReader(herdrValidPong)}
	if herdrExchange(ctx, conn) != EvidenceCancelled || len(conn.writes) != 0 {
		t.Fatal("cancelled exchange dispatched")
	}
}

// ALL actual socket/permission fixtures below are authored for disposable
// GitHub-hosted runners ONLY. A missing required gate is a failure, not a skip.
func requireHerdrHosted(t *testing.T) {
	t.Helper()
	if os.Getenv("GITHUB_ACTIONS") != "true" || os.Getenv("RUNNER_ENVIRONMENT") != "github-hosted" {
		t.Fatal("owned Herdr socket fixtures require disposable GitHub-hosted runner")
	}
}

type herdrServerResult struct {
	accepted bool
	request  string
	extra    bool
	closed   bool
}
type herdrServerFixture struct {
	path, dir string
	listener  *net.UnixListener
	accepted  chan struct{}
	done      chan herdrServerResult
	stop      chan struct{}
	once      sync.Once
	result    herdrServerResult
}

func newHerdrServer(t *testing.T, response string, stall bool) *herdrServerFixture {
	t.Helper()
	requireHerdrHosted(t)
	dir, err := os.MkdirTemp("/tmp", "hd-")
	if err != nil {
		t.Fatal("fixture directory unavailable")
	}
	physical, err := filepath.EvalSymlinks(dir)
	if err != nil {
		os.RemoveAll(dir)
		t.Fatal("fixture physical selection unavailable")
	}
	s := &herdrServerFixture{dir: physical, path: filepath.Join(physical, "herdr.sock"), accepted: make(chan struct{}), done: make(chan herdrServerResult, 1), stop: make(chan struct{})}
	listener, err := net.ListenUnix("unix", &net.UnixAddr{Name: s.path, Net: "unix"})
	if err != nil {
		os.RemoveAll(physical)
		t.Fatal("owned socket unavailable")
	}
	s.listener = listener
	listener.SetUnlinkOnClose(false)
	if os.Chmod(s.path, 0o600) != nil {
		listener.Close()
		os.RemoveAll(physical)
		t.Fatal("fixture private mode unavailable")
	}
	t.Cleanup(func() {
		s.finish(t)
		if os.RemoveAll(s.dir) != nil {
			t.Error("fixture cleanup unavailable")
		}
	})
	go func() {
		result := herdrServerResult{}
		defer func() { s.done <- result }()
		_ = listener.SetDeadline(time.Now().Add(CallBudget + time.Second))
		conn, err := listener.AcceptUnix()
		if err != nil {
			return
		}
		result.accepted = true
		close(s.accepted)
		defer conn.Close()
		_ = conn.SetDeadline(time.Now().Add(CallBudget + time.Second))
		reader := bufio.NewReaderSize(conn, 1024)
		request, err := reader.ReadSlice('\n')
		result.request = string(request)
		if err != nil {
			result.closed = err == io.EOF
			return
		}
		if stall {
			<-s.stop
			return
		}
		_, _ = conn.Write([]byte(response))
		_, err = reader.ReadByte()
		result.closed, result.extra = err == io.EOF, err == nil
	}()
	return s
}
func (s *herdrServerFixture) finish(t *testing.T) herdrServerResult {
	t.Helper()
	s.once.Do(func() {
		close(s.stop)
		_ = s.listener.Close()
		select {
		case s.result = <-s.done:
		case <-time.After(CallBudget + 2*time.Second):
			t.Error("owned socket actor failed bounded join")
		}
	})
	return s.result
}
func herdrSelected(mode Mode, path string) Inspection {
	text := "HERDR_RELAY_TOKEN='" + strings.Repeat("k", 32) + "'\nHERDR_RELAY_INSTANCE_ID=private-instance-canary\nHERDR_RELAY_TRANSPORT='" + string(mode) + "'\nHERDR_SOCKET_PATH='" + strings.ReplaceAll(path, "'", "'\\''") + "'\n"
	switch mode {
	case ModeGateway:
		text += "HERDR_GATEWAY_URL='wss://gateway.example.test,wss://second.example.test'\nHERDR_PHONE_APP_URL='https://app.example.test'\n"
	case ModeBYO:
		text += "HERDR_EXTERNAL_HTTPS_ORIGIN='https://relay.example.test'\n"
	case ModeManaged:
		text += "HERDR_TAILSCALE_ORIGIN='https://relay.example.test'\n"
	case ModeCLI:
		text += "HERDR_TAILSCALE_CLI_ORIGIN='https://relay.example.test:8443'\n"
	}
	return parseFixture(text)
}

type herdrHTTPRefusal struct {
	t     *testing.T
	calls int
}

func (h *herdrHTTPRefusal) RoundTrip(req *http.Request) (*http.Response, error) {
	h.calls++
	if req.Method != http.MethodGet || req.URL.Host != "127.0.0.1:8375" || (req.URL.Path != "/health" && req.URL.Path != "/healthz" && req.URL.Path != "/readyz") {
		h.t.Error("Herdr wiring broadened HTTP authority")
	}
	return nil, errors.New("raw-error-canary/private/path-canary")
}

type herdrTransportTrap struct {
	*LocalQueries
	t *testing.T
}

func (q herdrTransportTrap) ManagedStatus(context.Context, LocalTarget) Datum {
	q.t.Fatal("BYO acquired managed authority")
	return localUnknown()
}
func (q herdrTransportTrap) CLIStatus(context.Context, LocalTarget) Datum {
	q.t.Fatal("CLI inspection acquired authority")
	return localUnknown()
}

func TestHerdrActualRunSinglePingModesPrivacyClosureAndNonmutation(t *testing.T) {
	for _, mode := range []Mode{ModeCloudflare, ModeGateway, ModeBYO} {
		t.Run(string(mode), func(t *testing.T) {
			server := newHerdrServer(t, strings.Replace(herdrValidPong, `"0.9.0"`, `"99.0.0-private-node-canary"`, 1), false)
			canary := filepath.Join(server.dir, "private-adjacent-canary")
			if os.WriteFile(canary, []byte("secret-canary\x1bprivate-data"), 0o600) != nil {
				t.Fatal("owned canary unavailable")
			}
			before, err := os.Lstat(canary)
			if err != nil {
				t.Fatal("fixture metadata unavailable")
			}
			socketBefore, err := os.Lstat(server.path)
			if err != nil {
				t.Fatal("fixture socket metadata unavailable")
			}
			cfg := herdrSelected(mode, server.path)
			q := NewLocalQueries(cfg, testBuild())
			httpTrap := &herdrHTTPRefusal{t: t}
			q.fixture = httpTrap
			fdBalance, peakFDs, dials := 0, 0, 0
			q.herdrFixture = &herdrHooks{fdEvent: func(stage string, _ int) {
				if stage == "open" {
					fdBalance++
					if fdBalance > peakFDs {
						peakFDs = fdBalance
					}
				} else {
					fdBalance--
				}
			}, dial: func(ctx context.Context, path string) (net.Conn, error) {
				dials++
				deadline, ok := ctx.Deadline()
				if !ok || time.Until(deadline) > CallBudget || path != server.path {
					t.Error("standalone call budget/selection not bound")
				}
				return (&net.Dialer{}).DialContext(ctx, "unix", path)
			}}
			r := Run(context.Background(), cfg, testBuild(), Options{}, Dependencies{Host: q, Transport: herdrTransportTrap{q, t}})
			c := findCheck(t, r, CheckHerdr)
			want := EvidenceUnavailable
			if runtime.GOOS == "linux" {
				want = EvidenceHerdrPing
			}
			if r.ExitCode() != 3 || c.State != Unknown || c.Evidence != want || !c.Required || c.Source != SourceQuery || fdBalance != 0 || peakFDs > 3 {
				t.Fatal("actual ping completed report, lost finite provenance or leaked descriptors")
			}
			result := server.finish(t)
			if runtime.GOOS == "linux" {
				if dials != 1 || !result.accepted || result.request != herdrPingRequest || result.extra || !result.closed {
					t.Fatal("actual adapter did not send exactly one fixed request and close")
				}
			} else if dials != 0 || result.accepted {
				t.Fatal("unsupported native writer model connected")
			}
			data, err := os.ReadFile(canary)
			after, statErr := os.Lstat(canary)
			socketAfter, socketErr := os.Lstat(server.path)
			if err != nil || statErr != nil || socketErr != nil || !bytes.Equal(data, []byte("secret-canary\x1bprivate-data")) || before.Mode() != after.Mode() || !before.ModTime().Equal(after.ModTime()) || !os.SameFile(before, after) || socketBefore.Mode() != socketAfter.Mode() || !os.SameFile(socketBefore, socketAfter) {
				t.Fatal("query mutated fixture/private adjacent data")
			}
			entries, err := os.ReadDir(server.dir)
			if err != nil || len(entries) != 2 {
				t.Fatal("query created fixture objects")
			}
			var output bytes.Buffer
			_ = r.RenderJSON(&output)
			_ = r.RenderHuman(&output)
			_ = r.RenderStatus(&output)
			for _, raw := range []string{server.path, server.dir, "private-node-canary", "private-instance-canary", "secret-canary", "private-adjacent-canary"} {
				if strings.Contains(output.String(), raw) {
					t.Fatal("public report reflected private socket/server data")
				}
			}
			if _, err := json.Marshal(q); err == nil || strings.Contains(err.Error(), server.path) {
				t.Fatal("queries serialized private socket")
			}
			if strings.Contains(fmt.Sprintf("%v %#v", q, q), server.path) {
				t.Fatal("queries reflected private socket")
			}
			assertRenderPrivacy(t, r)
		})
	}
}

func TestHerdrActualAdapterCompleteByteBoundaryWithoutWaitingForEOF(t *testing.T) {
	exact := strings.TrimSuffix(herdrValidPong, "\n") + strings.Repeat(" ", maxHerdrFrame-len(herdrValidPong)) + "\n"
	server := newHerdrServer(t, exact, false)
	cfg := herdrSelected(ModeCloudflare, server.path)
	q := NewLocalQueries(cfg, testBuild())
	started := time.Now()
	d := q.Herdr(context.Background(), selectedLocalTarget(cfg, testBuild()))
	want := EvidenceUnavailable
	if runtime.GOOS == "linux" {
		want = EvidenceHerdrPing
	}
	if len(exact) != maxHerdrFrame || d.State != Unknown || d.Evidence != want || time.Since(started) > 2*time.Second {
		t.Fatal("exact frame rejected or persistent peer required indefinite EOF")
	}
	result := server.finish(t)
	if runtime.GOOS == "linux" && (result.request != herdrPingRequest || !result.closed || result.extra) {
		t.Fatal("exact frame did not exercise one request and closure")
	}
}

func TestHerdrActualAdapterRejectsFramingAndMalformedResponses(t *testing.T) {
	boundary := strings.TrimSuffix(herdrValidPong, "\n") + strings.Repeat(" ", herdrReadBuffer-len(herdrValidPong)) + "\n" + herdrValidPong
	for i, response := range []string{herdrValidPong + herdrValidPong, boundary, strings.TrimSuffix(herdrValidPong, "\n"), strings.Replace(herdrValidPong, `"protocol":1`, `"protocol":null`, 1), strings.Repeat("x", maxHerdrFrame+1) + "\n"} {
		t.Run(fmt.Sprintf("response-%d", i), func(t *testing.T) {
			server := newHerdrServer(t, response, false)
			cfg := herdrSelected(ModeCloudflare, server.path)
			q := NewLocalQueries(cfg, testBuild())
			d := q.Herdr(context.Background(), selectedLocalTarget(cfg, testBuild()))
			if d.State != Unknown || d.Evidence == EvidenceHerdrPing {
				t.Fatal("native adapter accepted invalid/prefix response")
			}
			result := server.finish(t)
			if runtime.GOOS == "linux" && result.request != herdrPingRequest {
				t.Fatal("negative fixture did not exercise genuine adapter")
			}
		})
	}
}

func TestHerdrUnsupportedNativeBeforeAnyAcquisition(t *testing.T) {
	if runtime.GOOS == "linux" {
		if !herdrPathSafetyAvailable() {
			t.Fatal("Linux policy unexpectedly unavailable")
		}
		return
	}
	cfg := herdrSelected(ModeCloudflare, "/tmp/private/socket-canary.sock")
	q := NewLocalQueries(cfg, testBuild())
	calls := 0
	q.herdrFixture = &herdrHooks{event: func(string, *net.UnixConn) { calls++ }, fdEvent: func(string, int) { calls++ }, dial: func(context.Context, string) (net.Conn, error) { calls++; return nil, errors.New("must not connect") }}
	d := q.Herdr(context.Background(), selectedLocalTarget(cfg, testBuild()))
	if herdrPathSafetyAvailable() || calls != 0 || d.State != Unknown || d.Evidence != EvidenceUnavailable {
		t.Fatal("Darwin mode bits guessed ACL writer safety")
	}
}

func TestHerdrAbsentUnboundReloadedForgedAndCancelledZeroIO(t *testing.T) {
	base := herdrSelected(ModeGateway, "/tmp/private/socket-canary.sock")
	for _, change := range []func(*Inspection, *BuildIdentity, *LocalTarget){
		func(c *Inspection, _ *BuildIdentity, _ *LocalTarget) { c.herdrSocket = "" },
		func(c *Inspection, _ *BuildIdentity, _ *LocalTarget) { c.instance = "" },
		func(c *Inspection, _ *BuildIdentity, _ *LocalTarget) { c.code = ConfigUnavailable },
		func(c *Inspection, _ *BuildIdentity, _ *LocalTarget) { c.code = ConfigInvalid },
		func(c *Inspection, _ *BuildIdentity, _ *LocalTarget) { c.mode = ModeManaged },
		func(c *Inspection, _ *BuildIdentity, _ *LocalTarget) { c.mode = ModeCLI },
		func(_ *Inspection, b *BuildIdentity, _ *LocalTarget) { b.WebHash = "" },
	} {
		cfg, build := base, testBuild()
		target := selectedLocalTarget(cfg, build)
		change(&cfg, &build, &target)
		q := NewLocalQueries(cfg, build)
		calls := 0
		q.herdrFixture = &herdrHooks{event: func(string, *net.UnixConn) { calls++ }, fdEvent: func(string, int) { calls++ }}
		if q.Herdr(context.Background(), selectedLocalTarget(cfg, build)).Evidence != EvidenceUnavailable || calls != 0 {
			t.Fatal("unbound/unsupported selection acquired")
		}
	}
	for _, change := range []func(*LocalTarget){
		func(t *LocalTarget) { t.herdrSocket = "/tmp/other.sock" }, func(t *LocalTarget) { t.instance = "other" }, func(t *LocalTarget) { t.port++ }, func(t *LocalTarget) { t.pluginPort++ },
		func(t *LocalTarget) { t.mode = ModeCloudflare }, func(t *LocalTarget) { t.build.Revision = strings.Repeat("c", 40) }, func(t *LocalTarget) { t.build.WebHash = strings.Repeat("d", 64) },
		func(t *LocalTarget) { t.gateways[0], t.gateways[1] = t.gateways[1], t.gateways[0] }, func(t *LocalTarget) { t.gatewayCount-- },
	} {
		q := NewLocalQueries(base, testBuild())
		target := selectedLocalTarget(base, testBuild())
		change(&target)
		calls := 0
		q.herdrFixture = &herdrHooks{fdEvent: func(string, int) { calls++ }}
		if q.Herdr(context.Background(), target).Evidence != EvidenceUnavailable || calls != 0 {
			t.Fatal("forged/reloaded target bypassed complete construction binding")
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	q := NewLocalQueries(base, testBuild())
	calls := 0
	q.herdrFixture = &herdrHooks{fdEvent: func(string, int) { calls++ }}
	if q.Herdr(ctx, selectedLocalTarget(base, testBuild())).Evidence != EvidenceCancelled || calls != 0 {
		t.Fatal("cancelled target acquired")
	}
	var nilQueries *LocalQueries
	if nilQueries.Herdr(context.Background(), LocalTarget{}).Evidence != EvidenceUnavailable {
		t.Fatal("typed nil query manufactured observation")
	}
	q.fixture = &herdrHTTPRefusal{t: t}
	for _, cfg := range []Inspection{refusedConfig(ConfigInvalid), refusedConfig(ConfigUnavailable), herdrSelected(ModeCLI, base.herdrSocket), herdrSelected(ModeManaged, base.herdrSocket)} {
		bound := NewLocalQueries(cfg, testBuild())
		bound.fixture = &herdrHTTPRefusal{t: t}
		bound.herdrFixture = q.herdrFixture
		r := Run(context.Background(), cfg, testBuild(), Options{}, Dependencies{Host: bound, Transport: bound})
		if r.ExitCode() == 0 || findCheck(t, r, CheckHerdr).Evidence == EvidenceHerdrPing || calls != 0 {
			t.Fatal("actual Run dispatched unqualified Herdr socket")
		}
		if cfg.Mode() == ModeCLI && findCheck(t, r, CheckCLI).Evidence != EvidenceDisabled {
			t.Fatal("CLI admission relaxed")
		}
	}
}

func TestHerdrMetadataOwnershipACLMaskFilesystemAndPeerUIDPolicy(t *testing.T) {
	uid := uint32(os.Geteuid())
	foreign := uid + 1
	private := unix.Stat_t{Mode: unix.S_IFDIR | 0o700, Uid: uid, Nlink: 1}
	socket := unix.Stat_t{Mode: unix.S_IFSOCK | 0o600, Uid: uid, Nlink: 1}
	if !herdrDirectory(&private, true) || !herdrSocket(&socket) || !herdrPeerUIDAllowed(uid) || herdrPeerUIDAllowed(foreign) {
		t.Fatal("effective-user attribution policy diverged")
	}
	for _, st := range []unix.Stat_t{{Mode: unix.S_IFDIR | 0o750, Uid: uid}, {Mode: unix.S_IFDIR | 0o730, Uid: uid}, {Mode: unix.S_IFDIR | 0o2700, Uid: uid}, {Mode: unix.S_IFDIR | 0o700, Uid: foreign}} {
		if herdrDirectory(&st, true) {
			t.Fatal("private parent ACL-mask/ownership/special mode admitted")
		}
	}
	for _, st := range []unix.Stat_t{{Mode: unix.S_IFSOCK | 0o620, Uid: uid, Nlink: 1}, {Mode: unix.S_IFSOCK | 0o602, Uid: uid, Nlink: 1}, {Mode: unix.S_IFSOCK | 0o4600, Uid: uid, Nlink: 1}, {Mode: unix.S_IFSOCK | 0o600, Uid: foreign, Nlink: 1}, {Mode: unix.S_IFSOCK | 0o600, Uid: uid, Nlink: 2}, {Mode: unix.S_IFREG | 0o600, Uid: uid, Nlink: 1}} {
		if herdrSocket(&st) {
			t.Fatal("foreign write ACL-mask/owner/link/type evidence admitted")
		}
	}
	sticky := unix.Stat_t{Mode: unix.S_IFDIR | 0o1777, Uid: 0}
	if !herdrDirectory(&sticky, false) {
		t.Fatal("trusted root sticky deletion rule lost")
	}
	sticky.Mode = unix.S_IFDIR | 0o777
	if herdrDirectory(&sticky, false) {
		t.Fatal("unsafe ancestor writers admitted")
	}
	for _, kind := range []uint64{0xef53, 0x58465342, 0x9123683e, 0x01021994, 0x858458f6} {
		if !herdrFilesystemType(kind) {
			t.Fatal("declared Linux local ACL-mask model refused")
		}
	}
	for _, kind := range []uint64{0, 0x6969, 0x65735546, 0xff534d42, 0x2fc12fc1, 0x794c7630} {
		if herdrFilesystemType(kind) {
			t.Fatal("remote/FUSE/rich/unknown model guessed safe")
		}
	}
}

func TestHerdrOwnedUnsafePathsRefusedBeforeConnection(t *testing.T) {
	for _, scenario := range []string{"missing", "regular", "directory", "fifo", "socket-symlink", "socket-hardlink", "parent-symlink", "parent-public", "socket-writable", "socket-special", "ancestor-writable"} {
		t.Run(scenario, func(t *testing.T) {
			server := newHerdrServer(t, herdrValidPong, false)
			path := server.path
			switch scenario {
			case "missing":
				path = filepath.Join(server.dir, "missing.sock")
			case "regular":
				path = filepath.Join(server.dir, "regular")
				if os.WriteFile(path, []byte("secret-canary"), 0o600) != nil {
					t.Fatal("fixture unavailable")
				}
			case "directory":
				path = server.dir
			case "fifo":
				path = filepath.Join(server.dir, "fifo")
				if unix.Mkfifo(path, 0o600) != nil {
					t.Fatal("fixture unavailable")
				}
			case "socket-symlink":
				path = filepath.Join(server.dir, "link")
				if os.Symlink(server.path, path) != nil {
					t.Fatal("fixture unavailable")
				}
			case "socket-hardlink":
				path = filepath.Join(server.dir, "hardlink")
				if runtime.GOOS == "linux" {
					if os.Link(server.path, path) != nil {
						t.Fatal("owned Linux socket hardlink fixture unavailable")
					}
				} else {
					// Native refusal before acquisition; no unsupported Darwin socket-link claim.
					if os.WriteFile(path, []byte("secret-canary"), 0o600) != nil || os.Link(path, path+".alias") != nil {
						t.Fatal("owned native link fixture unavailable")
					}
				}
			case "parent-symlink":
				link := filepath.Join(server.dir, "link")
				if os.Symlink(server.dir, link) != nil {
					t.Fatal("fixture unavailable")
				}
				path = filepath.Join(link, "herdr.sock")
			case "parent-public":
				if os.Chmod(server.dir, 0o750) != nil {
					t.Fatal("fixture unavailable")
				}
			case "socket-writable":
				if os.Chmod(server.path, 0o620) != nil {
					t.Fatal("fixture unavailable")
				}
			case "socket-special":
				if os.Chmod(server.path, os.FileMode(0o600)|os.ModeSetuid) != nil {
					t.Fatal("fixture unavailable")
				}
			case "ancestor-writable":
				nested := filepath.Join(server.dir, "nested")
				if os.Mkdir(nested, 0o700) != nil {
					t.Fatal("fixture unavailable")
				}
				path = filepath.Join(nested, "herdr.sock")
				owned, err := net.ListenUnix("unix", &net.UnixAddr{Name: path, Net: "unix"})
				if err != nil {
					t.Fatal("owned nested socket unavailable")
				}
				owned.SetUnlinkOnClose(false)
				t.Cleanup(func() { _ = owned.Close() })
				if os.Chmod(path, 0o600) != nil || os.Chmod(server.dir, 0o777) != nil {
					t.Fatal("fixture unavailable")
				}
			}
			cfg := herdrSelected(ModeCloudflare, path)
			q := NewLocalQueries(cfg, testBuild())
			dials, balance := 0, 0
			q.herdrFixture = &herdrHooks{fdEvent: func(stage string, _ int) {
				if stage == "open" {
					balance++
				} else {
					balance--
				}
			}, dial: func(context.Context, string) (net.Conn, error) { dials++; return nil, errors.New("must not connect") }}
			d := q.Herdr(context.Background(), selectedLocalTarget(cfg, testBuild()))
			if d.State != Unknown || d.Evidence != EvidenceUnavailable || dials != 0 || balance != 0 || server.finish(t).accepted {
				t.Fatal("unsafe metadata connected/read/leaked descriptors")
			}
		})
	}
}

func TestHerdrReplacementCancellationAndOwnedConnectionClosure(t *testing.T) {
	for _, scenario := range []string{"before-dial-replace", "before-dial-parent-replace", "connected-replace", "connected-peer-unavailable", "before-send-cancel", "before-final-permissions", "before-final-parent-replace", "before-final-cancel"} {
		t.Run(scenario, func(t *testing.T) {
			server := newHerdrServer(t, herdrValidPong, false)
			cfg := herdrSelected(ModeCloudflare, server.path)
			q := NewLocalQueries(cfg, testBuild())
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			balance, dials := 0, 0
			q.herdrFixture = &herdrHooks{fdEvent: func(stage string, _ int) {
				if stage == "open" {
					balance++
				} else {
					balance--
				}
			}, dial: func(ctx context.Context, path string) (net.Conn, error) {
				dials++
				return (&net.Dialer{}).DialContext(ctx, "unix", path)
			}, event: func(stage string, conn *net.UnixConn) {
				// Dial can complete before the server's AcceptUnix. For the
				// no-request/EOF cases, wait for that accept before closing or
				// cancelling the client; otherwise finish may close the listener
				// first and report a false missing EOF.
				if stage == "connected" && (scenario == "connected-replace" || scenario == "connected-peer-unavailable" || scenario == "before-send-cancel") {
					select {
					case <-server.accepted:
					case <-time.After(CallBudget + 2*time.Second):
						t.Error("owned socket actor did not accept connected client")
						cancel()
						_ = conn.Close()
						return
					}
				}
				if (scenario == "before-dial-replace" && stage == "before-dial") || (scenario == "connected-replace" && stage == "connected") {
					if os.Rename(server.path, server.path+".old") != nil || os.WriteFile(server.path, []byte("secret-canary"), 0o600) != nil {
						t.Error("replacement fixture unavailable")
					}
				}
				if (scenario == "before-dial-parent-replace" && stage == "before-dial") || (scenario == "before-final-parent-replace" && stage == "before-final") {
					old := server.dir + ".old"
					t.Cleanup(func() { _ = os.RemoveAll(old) })
					if os.Rename(server.dir, old) != nil || os.Mkdir(server.dir, 0o700) != nil || os.WriteFile(server.path, []byte("secret-canary"), 0o600) != nil {
						t.Error("owned parent replacement unavailable")
					}
				}
				if scenario == "connected-peer-unavailable" && stage == "connected" {
					_ = conn.Close()
				}
				if (scenario == "before-send-cancel" && stage == "before-send") || (scenario == "before-final-cancel" && stage == "before-final") {
					cancel()
				}
				if scenario == "before-final-permissions" && stage == "before-final" {
					if os.Chmod(server.dir, 0o750) != nil {
						t.Error("permission fixture unavailable")
					}
				}
			}}
			d := q.Herdr(ctx, selectedLocalTarget(cfg, testBuild()))
			if d.State != Unknown || d.Evidence == EvidenceHerdrPing || balance != 0 {
				t.Fatal("changing/late/cancelled evidence accepted or descriptors leaked")
			}
			result := server.finish(t)
			if runtime.GOOS == "linux" {
				wantDials := 1
				if strings.HasPrefix(scenario, "before-dial") {
					wantDials = 0
				}
				if dials != wantDials {
					t.Fatal("replacement retried or connected before validated path")
				}
				if scenario == "connected-replace" || scenario == "connected-peer-unavailable" || scenario == "before-send-cancel" {
					if !result.accepted || result.request != "" || !result.closed {
						t.Fatal("unsafe peer/path/cancellation sent request or left connection")
					}
				}
				if strings.HasPrefix(scenario, "before-final") && result.request != herdrPingRequest {
					t.Fatal("final refusal fixture did not acquire actual pong")
				}
			}
		})
	}
}

func TestHerdrStandaloneBackgroundAndLongParentClampBeforeSingleDial(t *testing.T) {
	for _, longParent := range []bool{false, true} {
		server := newHerdrServer(t, herdrValidPong, false)
		cfg := herdrSelected(ModeCloudflare, server.path)
		q := NewLocalQueries(cfg, testBuild())
		ctx := context.Background()
		cancel := func() {}
		if longParent {
			ctx, cancel = context.WithTimeout(ctx, GlobalBudget)
		}
		defer cancel()
		dials := 0
		q.herdrFixture = &herdrHooks{dial: func(ctx context.Context, path string) (net.Conn, error) {
			dials++
			deadline, ok := ctx.Deadline()
			if !ok || ctx.Done() == nil || time.Until(deadline) > CallBudget || path != server.path {
				t.Error("standalone deadline or selected path not bound")
			}
			return nil, errors.New("raw-error-canary/private/path-canary")
		}}
		d := q.Herdr(ctx, selectedLocalTarget(cfg, testBuild()))
		want := 0
		if runtime.GOOS == "linux" {
			want = 1
		}
		if d.Evidence != EvidenceUnavailable || dials != want || server.finish(t).accepted {
			t.Fatal("standalone failed dial retried or authority expanded")
		}
	}
}

func TestHerdrStandaloneAndRunDeadlinesCancelBlockedRead(t *testing.T) {
	for _, viaRun := range []bool{false, true} {
		t.Run(fmt.Sprintf("run-%t", viaRun), func(t *testing.T) {
			server := newHerdrServer(t, herdrValidPong, true)
			cfg := herdrSelected(ModeBYO, server.path)
			q := NewLocalQueries(cfg, testBuild())
			q.fixture = &herdrHTTPRefusal{t: t}
			ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
			defer cancel()
			started := time.Now()
			evidence := EvidenceUnavailable
			if viaRun {
				r := Run(ctx, cfg, testBuild(), Options{}, Dependencies{Host: q, Transport: herdrTransportTrap{q, t}})
				evidence = findCheck(t, r, CheckHerdr).Evidence
				if r.ExitCode() != 3 {
					t.Fatal("timed-out ping made report healthy")
				}
			} else {
				evidence = q.Herdr(ctx, selectedLocalTarget(cfg, testBuild())).Evidence
			}
			if time.Since(started) > 2*time.Second || (runtime.GOOS == "linux" && evidence != EvidenceCancelled) {
				t.Fatal("parent deadline did not interrupt/discard blocked observation")
			}
			result := server.finish(t)
			if runtime.GOOS == "linux" && result.request != herdrPingRequest {
				t.Fatal("blocked-read fixture missed actual ping")
			}
		})
	}
}
