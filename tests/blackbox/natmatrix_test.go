package blackbox

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/pion/ice/v4"
	"github.com/pion/webrtc/v4"
)

// The NAT behaviour matrix (plan item G10). Everything else in this suite proves
// the hybrid transport on loopback, where hole punching is free. This one runs
// the real gateway, the real relay and a real Pion phone in separate Linux
// network namespaces behind simulated NATs, and asserts which behaviour
// combinations form a direct pair and which correctly fall back to the gateway.
//
// The verdict is decided by the mapping dimension. A port-preserving
// (endpoint-independent) NAT lets each peer publish an address the other can
// actually reach, so the pair forms under either filtering rule. A port-rewriting
// (symmetric) NAT publishes the port it allocated for the STUN flow, which is not
// the port it will use towards the peer, so no pair can form without port
// prediction and the session must stay on the gateway. Those two facts are the
// whole reason G2 (symmetric-NAT port prediction) is decision-gated on evidence:
// this harness is where such a prediction would have to prove itself.
var natMatrixCells = []natCell{
	{name: "eim-adf", mapping: natMappingIndependent, filtering: natFilterAddress, direct: true},
	{name: "eim-apdf", mapping: natMappingIndependent, filtering: natFilterAddressPort, direct: true},
	{name: "symmetric-adf", mapping: natMappingSymmetric, filtering: natFilterAddress, direct: false},
	{name: "symmetric-apdf", mapping: natMappingSymmetric, filtering: natFilterAddressPort, direct: false},
}

const (
	// natPlanEnvVar carries one cell's whole plan as JSON into the phone
	// namespace, and natRespondEnvVar starts the probe responder in the internet
	// namespace. Both are how the harness re-execs this very test binary: the
	// phone has to run inside a namespace, and re-exec is what keeps it the same
	// code as the rest of the suite instead of a second implementation.
	natPlanEnvVar    = "HERDR_NAT_MATRIX_PLAN"
	natRespondEnvVar = "HERDR_NAT_MATRIX_RESPOND"

	// natDataChannelLabel is the only label the relay accepts. webrtclink keeps
	// its copy unexported, so the wire contract is restated here on purpose.
	natDataChannelLabel = "herdr-dc-v1"

	// natDirectWindow bounds how long a cell that should form a direct pair may
	// take; natRelayedWindow is how long a cell that should not form one must
	// stay silent before the fallback verdict is believed. Pion gives up on a
	// failing ICE agent after 25 s, so this window is a genuine "nothing formed"
	// observation rather than an impatient one.
	natDirectWindow  = 45 * time.Second
	natRelayedWindow = 20 * time.Second

	// natGatherWindow bounds the phone's candidate gathering. Everything it
	// gathers is local plus one STUN round trip inside the lab, so exceeding this
	// means something is wrong rather than slow.
	natGatherWindow = 10 * time.Second
)

// natCellPlan is everything the phone namespace needs to run one cell. It
// travels as one JSON environment variable so adding a field never means
// touching the exec plumbing.
type natCellPlan struct {
	Cell        string   `json:"cell"`
	Mapping     string   `json:"mapping"`
	Filtering   string   `json:"filtering"`
	Direct      bool     `json:"direct"`
	GatewayWS   string   `json:"gateway_ws"`
	GatewayHTTP string   `json:"gateway_http"`
	GatewaySTUN string   `json:"gateway_stun"`
	RelayHTTP   string   `json:"relay_http"`
	Responders  []string `json:"responders"`
}

// natBinaries are the real binaries under test plus the fixtures the relay needs.
type natBinaries struct {
	gateway  string
	relay    string
	herdr    string
	scenario string
	webRoot  string
}

// TestNATMatrixHolePunching is the harness entry point: it owns the topology and
// the processes, and delegates each cell's assertions to a phone that runs
// inside the phone namespace.
func TestNATMatrixHolePunching(t *testing.T) {
	ipBin, nftBin := requireNATMatrixHost(t)
	self, err := os.Executable()
	if err != nil {
		t.Fatalf("locate the test binary for the phone namespace: %v", err)
	}
	bins := buildNATBinaries(t)

	for _, cell := range natMatrixCells {
		t.Run(cell.name, func(t *testing.T) {
			runNATCell(t, ipBin, nftBin, self, bins, cell)
		})
	}
}

func runNATCell(t *testing.T, ipBin, nftBin, self string, bins natBinaries, cell natCell) {
	t.Helper()
	lab := newNATLab(t, ipBin, nftBin)

	// Ports are picked in the harness' own namespace, which is safe because the
	// namespaces this cell creates are empty: nothing else can be listening
	// there.
	gatewayPort := freePort(t)
	stunPort := freePort(t)
	relayPort := freePort(t)
	responders := []string{
		net.JoinHostPort(natGatewayAddr, strconv.Itoa(freePort(t))),
		net.JoinHostPort(natInetPhoneSide, strconv.Itoa(freePort(t))),
	}

	lab.applyNAT(cell, relayPort)
	lab.startGateway(bins, gatewayPort, stunPort)
	lab.startRelay(bins, relayPort, gatewayPort)
	lab.startResponder(self, responders)

	plan := natCellPlan{
		Cell:        cell.name,
		Mapping:     cell.mapping,
		Filtering:   cell.filtering,
		Direct:      cell.direct,
		GatewayWS:   fmt.Sprintf("ws://%s:%d", natGatewayAddr, gatewayPort),
		GatewayHTTP: fmt.Sprintf("http://%s:%d", natGatewayAddr, gatewayPort),
		GatewaySTUN: net.JoinHostPort(natGatewayAddr, strconv.Itoa(stunPort)),
		RelayHTTP:   fmt.Sprintf("http://%s:%d", natRelayExtAddr, relayPort),
		Responders:  responders,
	}
	out, err := lab.runPhone(self, plan)
	t.Logf("phone namespace (%s x %s):\n%s", cell.mapping, cell.filtering, out)
	if err != nil {
		t.Logf("relay NAT ruleset:\n%s", lab.dumpRuleset(lab.relayNAT))
		t.Logf("relay NAT udp flows:\n%s", lab.dumpConntrack(lab.relayNAT))
		t.Logf("phone NAT ruleset:\n%s", lab.dumpRuleset(lab.phoneNAT))
		t.Logf("phone NAT udp flows:\n%s", lab.dumpConntrack(lab.phoneNAT))
		t.Fatalf("cell %s (%s mapping, %s filtering, expected %s) failed: %v",
			cell.name, cell.mapping, cell.filtering, natVerdict(cell.direct), err)
	}
}

func natVerdict(direct bool) string {
	if direct {
		return "DIRECT"
	}
	return "RELAYED"
}

// buildNATBinaries compiles the binaries once for the whole matrix and writes
// the fake-Herdr fixtures the relay serves.
func buildNATBinaries(t *testing.T) natBinaries {
	t.Helper()
	root := repoRoot(t)
	dir := t.TempDir()

	bins := natBinaries{
		gateway:  filepath.Join(dir, "herdr-gateway"),
		relay:    filepath.Join(dir, "herdr-mobile-relay"),
		herdr:    filepath.Join(dir, "fake-herdr"),
		scenario: filepath.Join(dir, "scenario.json"),
		webRoot:  filepath.Join(dir, "web"),
	}
	for out, pkg := range map[string]string{
		bins.gateway: "./cmd/herdr-gateway",
		bins.relay:   "./cmd/herdr-mobile-relay",
		bins.herdr:   "./cmd/fake-herdr",
	} {
		build := exec.Command("go", "build", "-o", out, pkg)
		build.Dir = root
		if combined, err := build.CombinedOutput(); err != nil {
			t.Fatalf("build %s: %v\n%s", pkg, err, combined)
		}
	}

	scenario := `{"panes":[{"pane_id":"pane-1","agent":"claude","name":"test","agent_status":"working","tab_id":"tab-1","workspace_id":"ws-1","cwd":"/tmp","revision":1,"foreground_cwd":"/tmp"}],"tabs":[{"tab_id":"tab-1","workspace_id":"ws-1","label":"main","number":1,"cwd":"/tmp"}]}`
	if err := os.WriteFile(bins.scenario, []byte(scenario), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(bins.webRoot, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(bins.webRoot, "index.html"), []byte("<html>test</html>"), 0o644); err != nil {
		t.Fatal(err)
	}
	return bins
}

// startGateway runs the blind gateway in the internet namespace, bound to its
// service address so every reply carries it as the source.
func (l *natLab) startGateway(bins natBinaries, port, stunPort int) {
	l.t.Helper()
	cmd := l.inNS(l.inet, bins.gateway)
	cmd.Env = append(os.Environ(),
		fmt.Sprintf("HERDR_GATEWAY_ADDR=%s:%d", natGatewayAddr, port),
		fmt.Sprintf("HERDR_GATEWAY_STUN_ADDR=%s:%d", natGatewayAddr, stunPort),
		"HERDR_GATEWAY_LOG_FORMAT=text",
	)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		l.t.Fatalf("start gateway in %s: %v", l.inet, err)
	}
	l.t.Cleanup(func() { stopProcess(cmd) })
}

// startRelay runs the relay in its home namespace. It binds every address in
// that namespace because the phone reads its /healthz through the NAT's
// TCP-only management forward.
func (l *natLab) startRelay(bins natBinaries, relayPort, gatewayPort int) {
	l.t.Helper()
	dir := l.t.TempDir()
	cmd := l.inNS(l.relay, bins.relay)
	cmd.Env = append(os.Environ(),
		fmt.Sprintf("HERDR_RELAY_PORT=%d", relayPort),
		fmt.Sprintf("HERDR_RELAY_PLUGIN_PORT=%d", freePort(l.t)),
		"HERDR_RELAY_HOST=0.0.0.0",
		"HERDR_RELAY_TOKEN="+relayKey,
		"HERDR_RELAY_INSTANCE_ID=nat-matrix",
		fmt.Sprintf("HERDR_GATEWAY_URL=ws://%s:%d", natGatewayAddr, gatewayPort),
		// Port mapping would look for a router that does not exist in this
		// namespace, and the matrix is about NAT traversal without one.
		"HERDR_REACHABILITY_PORT_MAPPING=false",
		"HERDR_BIN="+bins.herdr,
		"HERDR_WEB_ROOT="+bins.webRoot,
		"HERDR_RELAY_POLL_INTERVAL=1",
		"FAKE_HERDR_SCENARIO="+bins.scenario,
		"FAKE_HERDR_OPERATIONS="+filepath.Join(dir, "operations.jsonl"),
		"XDG_CONFIG_HOME="+filepath.Join(dir, "config"),
		"XDG_CACHE_HOME="+filepath.Join(dir, "cache"),
		"XDG_DATA_HOME="+filepath.Join(dir, "data"),
		"HERDR_SOCKET_PATH="+filepath.Join(dir, "herdr.sock"),
	)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		l.t.Fatalf("start relay in %s: %v", l.relay, err)
	}
	l.t.Cleanup(func() { stopProcess(cmd) })
}

// startResponder runs the NAT behaviour responder in the internet namespace. It
// is this test binary re-exec'd, which is why the harness needs no fixture
// scripts and no extra command.
func (l *natLab) startResponder(self string, addrs []string) {
	l.t.Helper()
	cmd := l.inNS(l.inet, self, "-test.run=^TestNATMatrixResponder$", "-test.v")
	cmd.Env = append(os.Environ(), natRespondEnvVar+"="+strings.Join(addrs, ","))
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		l.t.Fatalf("start probe responder in %s: %v", l.inet, err)
	}
	l.t.Cleanup(func() { stopProcess(cmd) })
}

// runPhone executes one cell inside the phone namespace and returns its output
// for the harness log, whether it passed or failed.
func (l *natLab) runPhone(self string, plan natCellPlan) (string, error) {
	l.t.Helper()
	encoded, err := json.Marshal(plan)
	if err != nil {
		l.t.Fatalf("encode cell plan: %v", err)
	}
	cmd := l.inNS(l.phone, self,
		"-test.run=^TestNATMatrixPhoneCell$", "-test.v", "-test.timeout=5m")
	cmd.Env = append(os.Environ(), natPlanEnvVar+"="+string(encoded))
	out, err := cmd.CombinedOutput()
	return strings.TrimRight(string(out), "\n"), err
}

// TestNATMatrixPhoneCell is one cell, seen from the phone. The harness re-execs
// it inside the phone namespace; run directly it has nothing to do.
func TestNATMatrixPhoneCell(t *testing.T) {
	raw := os.Getenv(natPlanEnvVar)
	if raw == "" {
		t.Skipf("internal helper: TestNATMatrixHolePunching runs it inside the phone namespace with %s set", natPlanEnvVar)
	}
	var plan natCellPlan
	if err := json.Unmarshal([]byte(raw), &plan); err != nil {
		t.Fatalf("decode cell plan: %v", err)
	}

	// Measure the NAT in front of this namespace before trusting any transport
	// verdict it produces. Without this, a ruleset that silently stopped
	// simulating what it claims would keep reporting green cells.
	measured := probeNATBehaviour(t, plan.Responders)
	t.Logf("phone-side NAT configured as %s / %s, measured as %s / %s",
		plan.Mapping, plan.Filtering, measured.mapping, measured.filtering)
	if measured.mapping != plan.Mapping {
		t.Fatalf("phone-side mapping measured %s, want %s: this cell does not simulate what it claims",
			measured.mapping, plan.Mapping)
	}
	// Filtering is asserted only on the port-preserving row. The
	// address-dependent pinhole is a port-preserving DNAT, so a port-rewriting
	// mapping makes it undeliverable: the external port a peer aims at is not the
	// port the pinhole would hand to the host. A real symmetric NAT would
	// deliver it, but the outcome is the same either way — the peer never learns
	// the right port, which is exactly why both symmetric rows relay.
	if plan.Mapping == natMappingIndependent && measured.filtering != plan.Filtering {
		t.Fatalf("phone-side filtering measured %s, want %s", measured.filtering, plan.Filtering)
	}

	env := &hybridEnv{gatewayHTTP: plan.GatewayHTTP, gatewayWS: plan.GatewayWS, relayHTTP: plan.RelayHTTP}
	// Both processes are still starting in their own namespaces, and the retrying
	// readiness probes are what makes the cell independent of who won that race.
	waitForStatus(t, env.gatewayHTTP, "/healthz", http.StatusOK)
	waitForStatus(t, env.relayHTTP, "/readyz", http.StatusOK)
	waitForRegistration(t, env.relayHTTP)

	mappedPort, localPort := waitForRelayMapping(t, env.relayHTTP)
	t.Logf("relay ICE port %d is mapped to port %d through its own NAT", localPort, mappedPort)
	preserved := mappedPort == localPort
	if preserved != (plan.Mapping == natMappingIndependent) {
		t.Fatalf("relay-side NAT preserved the ICE port: %t, want %t", preserved, plan.Mapping == natMappingIndependent)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()

	session := newNATSession(ctx, t, env, plan)
	defer session.stop()
	session.offer(ctx)

	if plan.Direct {
		session.awaitDirect(ctx, natDirectWindow)
		if !session.channelOpen || session.pair == "" {
			t.Fatalf("expected a direct pair: data channel open=%t, nominated pair=%q, relay sessions=%s\ncandidates the relay offered:\n%s\ncandidates the phone offered:\n%s",
				session.channelOpen, session.pair, session.sessionsJSON(),
				strings.Join(session.remoteCandidates, "\n"), strings.Join(session.localCandidates, "\n"))
		}
		t.Logf("DIRECT: data channel open over the nominated pair %s", session.pair)
	} else {
		session.awaitDirect(ctx, natRelayedWindow)
		if session.channelOpen || session.pair != "" {
			t.Fatalf("a direct pair formed through a symmetric NAT: data channel open=%t, pair=%q, relay sessions=%s\ncandidates the relay offered:\n%s\ncandidates the phone offered:\n%s",
				session.channelOpen, session.pair, session.sessionsJSON(),
				strings.Join(session.remoteCandidates, "\n"), strings.Join(session.localCandidates, "\n"))
		}
		// The fallback is the point of these cells: after the direct attempt
		// fails the phone must still be served over the gateway.
		session.requireGatewayPathAlive(ctx)
		t.Logf("RELAYED: no pair formed in %s and the gateway path still serves commands", natRelayedWindow)
	}

	session.close(ctx)
	direct, relayed := waitForOutcomeCounters(t, env.relayHTTP, plan.Direct)
	t.Logf("relay outcome counters: direct=%d relayed=%d", direct, relayed)
}

// natSession drives one direct-path attempt from the phone side. Every send,
// every peer-connection call and every assertion happens in the test goroutine:
// the reader is the only other goroutine and it does nothing but hand raw frames
// over a channel, because a t.Fatal from a goroutine that outlives the test
// would panic the binary instead of failing the cell.
type natSession struct {
	t     *testing.T
	plan  natCellPlan
	phone *phone

	pc        *webrtc.PeerConnection
	dc        *webrtc.DataChannel
	requestID string

	frames     chan []byte
	readErr    chan error
	locals     chan webrtc.ICECandidateInit
	opened     chan struct{}
	readerDone chan struct{}
	stopReader context.CancelFunc
	poll       *time.Ticker

	pending      []webrtc.ICECandidateInit
	answered     bool
	channelOpen  bool
	pair         string
	lastSessions []any
	// Both candidate lists are kept for the failure report: which addresses the
	// two peers actually published is the first thing to look at when a cell
	// disagrees with its expectation.
	localCandidates  []string
	remoteCandidates []string
}

func newNATSession(ctx context.Context, t *testing.T, env *hybridEnv, plan natCellPlan) *natSession {
	t.Helper()
	s := &natSession{
		t:          t,
		plan:       plan,
		phone:      dialPhone(t, env),
		requestID:  "nat-matrix-" + plan.Cell,
		frames:     make(chan []byte, 8),
		readErr:    make(chan error, 1),
		locals:     make(chan webrtc.ICECandidateInit, 32),
		opened:     make(chan struct{}),
		readerDone: make(chan struct{}),
		poll:       time.NewTicker(250 * time.Millisecond),
	}

	settings := webrtc.SettingEngine{}
	// The namespaces are IPv4 only and the relay refuses TCP candidates, so
	// gathering anything else would only add dead candidates to every check
	// list.
	settings.SetNetworkTypes([]webrtc.NetworkType{webrtc.NetworkTypeUDP4})
	settings.DisableActiveTCP(true)
	settings.SetICEMulticastDNSMode(ice.MulticastDNSModeDisabled)
	pc, err := webrtc.NewAPI(webrtc.WithSettingEngine(settings)).NewPeerConnection(webrtc.Configuration{
		// The phone learns its own mapped address from the gateway's address
		// discovery listener, exactly as the PWA does.
		ICEServers: []webrtc.ICEServer{{URLs: []string{"stun:" + plan.GatewaySTUN}}},
	})
	if err != nil {
		t.Fatalf("new phone peer connection: %v", err)
	}
	t.Cleanup(func() { _ = pc.Close() })
	s.pc = pc

	dc, err := pc.CreateDataChannel(natDataChannelLabel, nil)
	if err != nil {
		t.Fatalf("create phone data channel: %v", err)
	}
	s.dc = dc
	dc.OnOpen(func() { close(s.opened) })
	pc.OnICECandidate(func(candidate *webrtc.ICECandidate) {
		if candidate == nil {
			return
		}
		select {
		case s.locals <- candidate.ToJSON():
		default:
		}
	})

	readerCtx, cancel := context.WithCancel(ctx)
	s.stopReader = cancel
	go s.readLoop(readerCtx)
	return s
}

func (s *natSession) stop() {
	s.poll.Stop()
	s.stopReader()
	<-s.readerDone
}

func (s *natSession) readLoop(ctx context.Context) {
	defer close(s.readerDone)
	for {
		frame, err := s.readFrame(ctx)
		if err != nil {
			select {
			case s.readErr <- err:
			default:
			}
			return
		}
		select {
		case s.frames <- frame:
		case <-ctx.Done():
			return
		}
	}
}

// readFrame is phone.readLogical with an error return instead of a t.Fatal: the
// relayed channel is idle on purpose while a relayed cell waits out its window,
// and the reader must be able to end quietly.
func (s *natSession) readFrame(ctx context.Context) ([]byte, error) {
	for {
		_, part, err := s.phone.conn.Read(ctx)
		if err != nil {
			return nil, err
		}
		frame, err := s.phone.assembly.Push(part)
		if err != nil {
			return nil, err
		}
		if frame != nil {
			return frame, nil
		}
	}
}

// offer sends the direct-connection offer through the encrypted relayed channel,
// which is the only path signaling ever takes.
func (s *natSession) offer(ctx context.Context) {
	s.t.Helper()
	offer, err := s.pc.CreateOffer(nil)
	if err != nil {
		s.t.Fatalf("create offer: %v", err)
	}
	// The offer waits for gathering to finish, so the relay has the phone's
	// reflexive address the moment it answers. A trickled-only offer makes the
	// two agents start punching at whatever moment their candidates happen to
	// cross the relayed channel, and a cell would then be measuring signaling
	// timing instead of the NAT in front of it.
	gathered := webrtc.GatheringCompletePromise(s.pc)
	if err := s.pc.SetLocalDescription(offer); err != nil {
		s.t.Fatalf("set local description: %v", err)
	}
	select {
	case <-gathered:
	case <-time.After(natGatherWindow):
		s.t.Logf("phone ICE gathering did not finish within %s; offering what it has", natGatherWindow)
	case <-ctx.Done():
		s.t.Fatalf("cell context ended while gathering: %v", ctx.Err())
	}
	local := s.pc.LocalDescription()
	if local == nil {
		s.t.Fatal("phone has no local description to offer")
	}
	s.phone.sendMessage(ctx, map[string]any{
		"type":       "webrtc_offer",
		"request_id": s.requestID,
		"sdp":        local.SDP,
	})
}

func (s *natSession) close(ctx context.Context) {
	s.t.Helper()
	s.phone.sendMessage(ctx, map[string]any{
		"type":       "webrtc_close",
		"request_id": s.requestID,
	})
}

// step advances the session by one event: a relay message, a locally gathered
// candidate, the data channel opening, or a poll of the relay's own session
// report. It returns the decoded relay message when the event was one, so a
// caller can wait for a message type without a second reader.
func (s *natSession) step(ctx context.Context, timeout time.Duration) map[string]any {
	s.t.Helper()
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case frame := <-s.frames:
		var message map[string]any
		if err := json.Unmarshal(s.phone.open(frame), &message); err != nil {
			s.t.Fatalf("decode relay message: %v", err)
		}
		s.dispatch(message)
		return message
	case candidate := <-s.locals:
		s.localCandidates = append(s.localCandidates, candidate.Candidate)
		s.phone.sendMessage(ctx, map[string]any{
			"type":            "webrtc_ice",
			"request_id":      s.requestID,
			"candidate":       candidate.Candidate,
			"sdp_mid":         natString(candidate.SDPMid),
			"sdp_mline_index": natUint16(candidate.SDPMLineIndex),
		})
	case err := <-s.readErr:
		s.t.Fatalf("the relayed signaling channel failed: %v", err)
	case <-s.opened:
		s.channelOpen = true
		// A closed channel is always ready; nil blocks forever, which is what
		// this event needs after it fired once.
		s.opened = nil
	case <-s.poll.C:
		s.observe()
	case <-ctx.Done():
		s.t.Fatalf("cell context ended early: %v", ctx.Err())
	case <-timer.C:
	}
	return nil
}

// dispatch handles the relay half of signaling.
func (s *natSession) dispatch(message map[string]any) {
	s.t.Helper()
	switch message["type"] {
	case "webrtc_answer":
		sdp, _ := message["sdp"].(string)
		if err := s.pc.SetRemoteDescription(webrtc.SessionDescription{
			Type: webrtc.SDPTypeAnswer,
			SDP:  sdp,
		}); err != nil {
			s.t.Fatalf("set relay answer: %v", err)
		}
		s.answered = true
		// An answer may already carry candidates; the rest arrive trickled.
		for _, line := range strings.Split(sdp, "\n") {
			if trimmed := strings.TrimSpace(line); strings.HasPrefix(trimmed, "a=candidate:") {
				s.remoteCandidates = append(s.remoteCandidates, strings.TrimPrefix(trimmed, "a="))
			}
		}
		for _, candidate := range s.pending {
			s.addRemote(candidate)
		}
		s.pending = nil
	case "webrtc_ice":
		candidate, ok := natCandidateInit(message)
		if !ok {
			return
		}
		s.remoteCandidates = append(s.remoteCandidates, candidate.Candidate)
		// The relay trickles from the moment it answers, and a candidate cannot
		// be added before the answer is applied.
		if !s.answered {
			s.pending = append(s.pending, candidate)
			return
		}
		s.addRemote(candidate)
	case "command_result":
		if message["request_id"] == s.requestID && message["ok"] != true {
			s.t.Fatalf("the relay refused the direct-path offer: %v", message)
		}
	}
}

func (s *natSession) addRemote(candidate webrtc.ICECandidateInit) {
	if err := s.pc.AddICECandidate(candidate); err != nil {
		s.t.Logf("relay candidate %q rejected: %v", candidate.Candidate, err)
	}
}

// awaitDirect pumps events until the direct path is proven or the window ends.
func (s *natSession) awaitDirect(ctx context.Context, window time.Duration) {
	s.t.Helper()
	deadline := time.Now().Add(window)
	for time.Now().Before(deadline) {
		s.step(ctx, 250*time.Millisecond)
		if s.channelOpen && s.pair != "" {
			return
		}
	}
}

// requireGatewayPathAlive proves the fallback: the relayed path still answers
// after the direct attempt failed.
func (s *natSession) requireGatewayPathAlive(ctx context.Context) {
	s.t.Helper()
	for range 20 {
		s.phone.sendMessage(ctx, map[string]any{"type": "refresh_agents"})
		message := s.awaitMessageType(ctx, "agents", 10*time.Second)
		if agents, _ := message["agents"].([]any); len(agents) > 0 {
			return
		}
	}
	s.t.Fatal("the gateway path never delivered a populated agent snapshot after the direct attempt failed")
}

func (s *natSession) awaitMessageType(ctx context.Context, wanted string, window time.Duration) map[string]any {
	s.t.Helper()
	deadline := time.Now().Add(window)
	for time.Now().Before(deadline) {
		if message := s.step(ctx, 250*time.Millisecond); message != nil && message["type"] == wanted {
			return message
		}
	}
	s.t.Fatalf("the relay never sent a %q message over the gateway path", wanted)
	return nil
}

// observe records the first nominated pair the relay reports. Polling is how the
// verdict survives a teardown: the relay drops a direct session whose encrypted
// handshake never arrives, and the pair would be gone by the time the window
// ended.
func (s *natSession) observe() {
	s.t.Helper()
	gateway := gatewayHealth(s.t, s.plan.RelayHTTP)
	sessions, _ := gateway["webrtc_sessions"].([]any)
	if len(sessions) > 0 {
		s.lastSessions = sessions
	}
	if s.pair != "" {
		return
	}
	for _, raw := range sessions {
		report, _ := raw.(map[string]any)
		local, _ := report["selected_local"].(string)
		remote, _ := report["selected_remote"].(string)
		if remote == "" {
			continue
		}
		s.pair = local + "/" + remote
		return
	}
}

func (s *natSession) sessionsJSON() string {
	encoded, err := json.Marshal(s.lastSessions)
	if err != nil {
		return "(unreportable)"
	}
	return string(encoded)
}

// waitForRelayMapping waits until the relay has learned its own mapped address
// through its NAT, and reports the mapped port next to the local ICE port.
// Address discovery is a prerequisite for the whole matrix: without it the relay
// publishes nothing a peer behind another NAT could reach.
func waitForRelayMapping(t *testing.T, base string) (mappedPort, localPort int) {
	t.Helper()
	for range 240 {
		gateway := gatewayHealth(t, base)
		local := natInt(gateway["webrtc_port"])
		discovery, _ := gateway["stun"].(map[string]any)
		mapped, _ := discovery["mapped"].([]any)
		for _, raw := range mapped {
			addr, _ := raw.(string)
			_, port, err := net.SplitHostPort(addr)
			if err != nil {
				continue
			}
			parsed, err := strconv.Atoi(port)
			if err != nil {
				continue
			}
			return parsed, local
		}
		time.Sleep(250 * time.Millisecond)
	}
	t.Fatal("the relay never discovered a mapped address through its NAT")
	return 0, 0
}

// waitForOutcomeCounters checks the /healthz aggregate the field telemetry uses:
// exactly one finished session, in the bucket this cell expects.
func waitForOutcomeCounters(t *testing.T, base string, wantDirect bool) (direct, relayed int) {
	t.Helper()
	for range 80 {
		gateway := gatewayHealth(t, base)
		direct = natInt(gateway["sessions_direct_total"])
		relayed = natInt(gateway["sessions_relayed_total"])
		if wantDirect && direct == 1 && relayed == 0 {
			return direct, relayed
		}
		if !wantDirect && relayed == 1 && direct == 0 {
			return direct, relayed
		}
		time.Sleep(250 * time.Millisecond)
	}
	t.Fatalf("relay counted direct=%d relayed=%d, want exactly one %s session",
		direct, relayed, natVerdict(wantDirect))
	return direct, relayed
}

func natCandidateInit(message map[string]any) (webrtc.ICECandidateInit, bool) {
	line, _ := message["candidate"].(string)
	if line == "" {
		return webrtc.ICECandidateInit{}, false
	}
	init := webrtc.ICECandidateInit{Candidate: line}
	if mid, ok := message["sdp_mid"].(string); ok && mid != "" {
		init.SDPMid = &mid
	}
	index := uint16(natInt(message["sdp_mline_index"]))
	init.SDPMLineIndex = &index
	return init, true
}

func natString(value *string) string {
	if value == nil {
		return ""
	}
	return *value
}

func natUint16(value *uint16) uint16 {
	if value == nil {
		return 0
	}
	return *value
}

func natInt(value any) int {
	switch typed := value.(type) {
	case float64:
		return int(typed)
	case int:
		return typed
	}
	return 0
}

// The NAT behaviour probe. It is RFC 5780 reduced to the two questions this
// matrix asks: does the mapping change with the destination, and does an
// unsolicited datagram from a known address on an unknown port get in? The
// responder answers from the internet namespace; the prober runs in the phone
// namespace, so the measurement covers the same NAT the transport run traverses.
const (
	natProbeEcho    = "ECHO"
	natProbeSeen    = "SEEN"
	natProbePunch   = "PUNCH"
	natProbeSent    = "SENT"
	natProbePunched = "PUNCHED"

	// natResponderLifetime bounds the responder when the harness that started it
	// dies without cleaning up.
	natResponderLifetime = 5 * time.Minute
)

type natBehaviour struct {
	mapping   string
	filtering string
}

// TestNATMatrixResponder is the probe responder. The harness re-execs it inside
// the internet namespace; run directly it has nothing to do.
func TestNATMatrixResponder(t *testing.T) {
	raw := os.Getenv(natRespondEnvVar)
	if raw == "" {
		t.Skipf("internal helper: TestNATMatrixHolePunching runs it inside the internet namespace with %s set", natRespondEnvVar)
	}
	addrs := strings.Split(raw, ",")
	if len(addrs) != 2 {
		t.Fatalf("%s must list two addresses, got %q", natRespondEnvVar, raw)
	}

	// A third socket on the first responder address but a different port: the
	// datagram it sends is the address-dependent-filtering probe.
	host, _, err := net.SplitHostPort(addrs[0])
	if err != nil {
		t.Fatalf("split responder address %q: %v", addrs[0], err)
	}
	punch, err := net.ListenPacket("udp4", net.JoinHostPort(host, "0"))
	if err != nil {
		t.Fatalf("listen for the filtering probe: %v", err)
	}
	defer punch.Close()

	deadline := time.Now().Add(natResponderLifetime)
	var wg sync.WaitGroup
	for _, addr := range addrs {
		conn, err := net.ListenPacket("udp4", addr)
		if err != nil {
			t.Fatalf("listen on %s: %v", addr, err)
		}
		defer conn.Close()
		wg.Add(1)
		go func() {
			defer wg.Done()
			serveNATProbe(conn, punch, deadline)
		}()
	}
	wg.Wait()
}

func serveNATProbe(conn, punch net.PacketConn, deadline time.Time) {
	buffer := make([]byte, 512)
	for time.Now().Before(deadline) {
		if err := conn.SetReadDeadline(time.Now().Add(time.Second)); err != nil {
			return
		}
		n, from, err := conn.ReadFrom(buffer)
		if err != nil {
			if errors.Is(err, os.ErrDeadlineExceeded) {
				continue
			}
			return
		}
		payload := string(buffer[:n])
		switch {
		case payload == natProbeEcho:
			// The source this datagram arrived from is the prober's external
			// address for this destination.
			_, _ = conn.WriteTo([]byte(natProbeSeen+" "+from.String()), from)
		case strings.HasPrefix(payload, natProbePunch+" "):
			target, err := net.ResolveUDPAddr("udp4", strings.TrimPrefix(payload, natProbePunch+" "))
			if err == nil {
				_, _ = punch.WriteTo([]byte(natProbePunched), target)
			}
			_, _ = conn.WriteTo([]byte(natProbeSent), from)
		}
	}
}

// probeNATBehaviour measures the NAT in front of this namespace.
func probeNATBehaviour(t *testing.T, responders []string) natBehaviour {
	t.Helper()
	if len(responders) != 2 {
		t.Fatalf("the behaviour probe needs two responder addresses, got %v", responders)
	}
	conn, err := net.ListenPacket("udp4", ":0")
	if err != nil {
		t.Fatalf("open the probe socket: %v", err)
	}
	defer conn.Close()

	first := probeMapping(t, conn, responders[0])
	second := probeMapping(t, conn, responders[1])
	behaviour := natBehaviour{mapping: natMappingSymmetric, filtering: natFilterAddressPort}
	if first == second {
		behaviour.mapping = natMappingIndependent
	}
	if probeFiltering(t, conn, responders[0], first) {
		behaviour.filtering = natFilterAddress
	}
	return behaviour
}

// probeMapping asks one responder which source it sees for this socket, which is
// this namespace's external address and port towards that destination.
func probeMapping(t *testing.T, conn net.PacketConn, responder string) string {
	t.Helper()
	target := natResolve(t, responder)
	for range 20 {
		if _, err := conn.WriteTo([]byte(natProbeEcho), target); err != nil {
			t.Fatalf("send the mapping probe to %s: %v", responder, err)
		}
		payload, ok := readNATProbe(conn, 300*time.Millisecond)
		if !ok || !strings.HasPrefix(payload, natProbeSeen+" ") {
			continue
		}
		return strings.TrimPrefix(payload, natProbeSeen+" ")
	}
	t.Fatalf("the probe responder on %s never reported a mapping for this namespace", responder)
	return ""
}

// probeFiltering asks the responder to send one datagram from a different port
// of an address this namespace has already written to. Arrival means filtering is
// address-dependent; silence means it is address-and-port-dependent.
func probeFiltering(t *testing.T, conn net.PacketConn, responder, mapped string) bool {
	t.Helper()
	target := natResolve(t, responder)
	acknowledged := false
	deadline := time.Now().Add(6 * time.Second)
	for time.Now().Before(deadline) {
		if !acknowledged {
			if _, err := conn.WriteTo([]byte(natProbePunch+" "+mapped), target); err != nil {
				t.Fatalf("send the filtering probe to %s: %v", responder, err)
			}
		}
		payload, ok := readNATProbe(conn, 500*time.Millisecond)
		if !ok {
			continue
		}
		switch {
		case strings.HasPrefix(payload, natProbePunched):
			return true
		case strings.HasPrefix(payload, natProbeSent):
			acknowledged = true
		}
	}
	if !acknowledged {
		t.Fatalf("the probe responder on %s never acknowledged the filtering probe", responder)
	}
	return false
}

func readNATProbe(conn net.PacketConn, timeout time.Duration) (string, bool) {
	if err := conn.SetReadDeadline(time.Now().Add(timeout)); err != nil {
		return "", false
	}
	buffer := make([]byte, 512)
	n, _, err := conn.ReadFrom(buffer)
	if err != nil {
		return "", false
	}
	return string(buffer[:n]), true
}

func natResolve(t *testing.T, addr string) *net.UDPAddr {
	t.Helper()
	resolved, err := net.ResolveUDPAddr("udp4", addr)
	if err != nil {
		t.Fatalf("resolve %s: %v", addr, err)
	}
	return resolved
}
