package blackbox

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/ecdh"
	"crypto/hkdf"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/0cv/herdr-mobile-relay/internal/framing"
	"github.com/0cv/herdr-mobile-relay/internal/gatewaywire"
)

// relayKey is a fixed 32-byte relay key so the derived gateway credentials are
// reproducible across the phone and the relay under test.
const relayKey = "0123456789abcdef0123456789abcdef"

// hybridEnv runs the three real binaries the hybrid transport needs: the blind
// gateway, the relay registered with it, and fake-herdr behind the relay. No
// cloudflared is involved anywhere.
type hybridEnv struct {
	gatewayHTTP string
	gatewayWS   string
	relayHTTP   string
}

func setupHybridEnv(t *testing.T) *hybridEnv {
	t.Helper()
	root := repoRoot(t)
	tmpDir := t.TempDir()

	binaries := map[string]string{
		"fake-herdr":         "./cmd/fake-herdr",
		"herdr-mobile-relay": "./cmd/herdr-mobile-relay",
		"herdr-gateway":      "./cmd/herdr-gateway",
	}
	paths := make(map[string]string, len(binaries))
	for name, pkg := range binaries {
		out := filepath.Join(tmpDir, name)
		build := exec.Command("go", "build", "-o", out, pkg)
		build.Dir = root
		if combined, err := build.CombinedOutput(); err != nil {
			t.Fatalf("build %s: %v\n%s", name, err, combined)
		}
		paths[name] = out
	}

	gatewayPort := freePort(t)
	env := &hybridEnv{
		gatewayHTTP: fmt.Sprintf("http://127.0.0.1:%d", gatewayPort),
		gatewayWS:   fmt.Sprintf("ws://127.0.0.1:%d", gatewayPort),
	}

	gateway := exec.Command(paths["herdr-gateway"])
	gateway.Env = append(os.Environ(),
		fmt.Sprintf("HERDR_GATEWAY_ADDR=127.0.0.1:%d", gatewayPort),
		// The binary defaults address discovery to :3478. A test must never bind a
		// well-known port: it would fail wherever a real gateway already runs and
		// would race a second test binary. An ephemeral loopback port keeps the
		// advertised stun_port real without claiming anything shared.
		"HERDR_GATEWAY_STUN_ADDR=127.0.0.1:0",
		"HERDR_GATEWAY_LOG_FORMAT=text",
	)
	gateway.Stdout = os.Stdout
	gateway.Stderr = os.Stderr
	if err := gateway.Start(); err != nil {
		t.Fatalf("start gateway: %v", err)
	}
	t.Cleanup(func() { stopProcess(gateway) })
	waitForStatus(t, env.gatewayHTTP, "/healthz", http.StatusOK)

	scenario := `{"panes":[{"pane_id":"pane-1","terminal_id":"terminal-1","agent":"claude","name":"test","agent_status":"working","tab_id":"tab-1","workspace_id":"ws-1","cwd":"/tmp","revision":1,"foreground_cwd":"/tmp"}],"tabs":[{"tab_id":"tab-1","workspace_id":"ws-1","label":"main","number":1,"cwd":"/tmp"}]}`
	scenarioPath := filepath.Join(tmpDir, "scenario.json")
	if err := os.WriteFile(scenarioPath, []byte(scenario), 0o644); err != nil {
		t.Fatal(err)
	}
	webDir := filepath.Join(tmpDir, "web")
	if err := os.MkdirAll(webDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(webDir, "index.html"), []byte("<html>test</html>"), 0o644); err != nil {
		t.Fatal(err)
	}

	relayPort := freePort(t)
	env.relayHTTP = fmt.Sprintf("http://127.0.0.1:%d", relayPort)
	relay := exec.Command(paths["herdr-mobile-relay"])
	relay.Env = append(os.Environ(),
		fmt.Sprintf("HERDR_RELAY_PORT=%d", relayPort),
		fmt.Sprintf("HERDR_RELAY_PLUGIN_PORT=%d", freePort(t)),
		"HERDR_RELAY_HOST=127.0.0.1",
		"HERDR_RELAY_TOKEN="+relayKey,
		"HERDR_RELAY_INSTANCE_ID=hybrid-blackbox",
		"HERDR_GATEWAY_URL="+env.gatewayWS,
		// Port mapping would reach for the LAN router from a test run.
		"HERDR_REACHABILITY_PORT_MAPPING=false",
		fmt.Sprintf("HERDR_BIN=%s", paths["fake-herdr"]),
		fmt.Sprintf("HERDR_WEB_ROOT=%s", webDir),
		"HERDR_RELAY_POLL_INTERVAL=0.5",
		fmt.Sprintf("FAKE_HERDR_SCENARIO=%s", scenarioPath),
		fmt.Sprintf("FAKE_HERDR_OPERATIONS=%s", filepath.Join(tmpDir, "operations.jsonl")),
		fmt.Sprintf("XDG_CONFIG_HOME=%s", filepath.Join(tmpDir, "config")),
		fmt.Sprintf("XDG_CACHE_HOME=%s", filepath.Join(tmpDir, "cache")),
		fmt.Sprintf("XDG_DATA_HOME=%s", filepath.Join(tmpDir, "data")),
		fmt.Sprintf("HERDR_SOCKET_PATH=%s", filepath.Join(tmpDir, "herdr.sock")),
	)
	relay.Stdout = os.Stdout
	relay.Stderr = os.Stderr
	if err := relay.Start(); err != nil {
		t.Fatalf("start relay: %v", err)
	}
	t.Cleanup(func() { stopProcess(relay) })
	waitForStatus(t, env.relayHTTP, "/readyz", http.StatusOK)
	return env
}

func stopProcess(cmd *exec.Cmd) {
	_ = cmd.Process.Signal(os.Interrupt)
	done := make(chan struct{})
	go func() {
		_, _ = cmd.Process.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		_ = cmd.Process.Kill()
	}
}

// gatewayHealth reads the relay's own view of its gateway registration.
func gatewayHealth(t *testing.T, base string) map[string]any {
	t.Helper()
	resp, err := http.Get(base + "/healthz")
	if err != nil {
		t.Fatalf("healthz: %v", err)
	}
	defer resp.Body.Close()
	var body map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode healthz: %v", err)
	}
	gateway, _ := body["gateway"].(map[string]any)
	if gateway == nil {
		t.Fatalf("healthz has no gateway field: %v", body)
	}
	return gateway
}

func waitForRegistration(t *testing.T, base string) map[string]any {
	t.Helper()
	for range 200 {
		gateway := gatewayHealth(t, base)
		if gateway["registered"] == true {
			return gateway
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatal("relay never registered with the gateway")
	return nil
}

// phone speaks the full client half of the hybrid stack: the gateway pairing
// handshake, the chunk framing, and the binary-codec E2EE session.
type phone struct {
	t        *testing.T
	conn     *websocket.Conn
	send     cipher.AEAD
	receive  cipher.AEAD
	sendSeq  uint64
	recvSeq  uint64
	assembly *framing.Reassembler
	chunks   [][]byte
}

func dialPhone(t *testing.T, env *hybridEnv) *phone {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	conn, _, err := websocket.Dial(ctx, env.gatewayWS+"/connect", nil)
	if err != nil {
		t.Fatalf("dial gateway: %v", err)
	}
	t.Cleanup(func() { conn.CloseNow() })
	conn.SetReadLimit(gatewaywire.MaxFramePayload)

	kind, raw, err := conn.Read(ctx)
	if err != nil || kind != websocket.MessageText {
		t.Fatalf("read gateway hello: %v (kind %v)", err, kind)
	}
	var hello gatewaywire.ServerHello
	if err := json.Unmarshal(raw, &hello); err != nil || hello.Type != gatewaywire.TypeServerHello {
		t.Fatalf("unexpected gateway hello %q: %v", raw, err)
	}
	nonce, err := base64.RawURLEncoding.DecodeString(hello.Nonce)
	if err != nil || len(nonce) != gatewaywire.NonceBytes {
		t.Fatalf("invalid gateway nonce: %v", err)
	}

	relayID, err := gatewaywire.DeriveRelayID(relayKey)
	if err != nil {
		t.Fatal(err)
	}
	rendezvous, err := gatewaywire.DeriveRendezvousKey(relayKey)
	if err != nil {
		t.Fatal(err)
	}
	connect, err := json.Marshal(gatewaywire.ConnectHello{
		Type:    gatewaywire.TypeConnect,
		Proto:   gatewaywire.Proto,
		RelayID: relayID,
		Proof:   base64.RawURLEncoding.EncodeToString(gatewaywire.ConnectProof(rendezvous, relayID, nonce)),
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := conn.Write(ctx, websocket.MessageText, connect); err != nil {
		t.Fatalf("write connect hello: %v", err)
	}
	kind, raw, err = conn.Read(ctx)
	if err != nil || kind != websocket.MessageText {
		t.Fatalf("read ready: %v", err)
	}
	var ready gatewaywire.ReadyMessage
	if err := json.Unmarshal(raw, &ready); err != nil || ready.Type != gatewaywire.TypeReady {
		t.Fatalf("gateway refused the connection: %s", raw)
	}

	p := &phone{t: t, conn: conn, assembly: framing.NewReassembler(framing.GatewayChunkSize)}
	p.handshake(ctx)
	return p
}

// writeLogical chunks one logical frame exactly like the PWA does.
func (p *phone) writeLogical(ctx context.Context, frame []byte) {
	p.t.Helper()
	p.chunks = framing.Chunk(p.chunks[:0], frame, framing.GatewayChunkSize)
	for _, part := range p.chunks {
		if err := p.conn.Write(ctx, websocket.MessageBinary, part); err != nil {
			p.t.Fatalf("write chunk: %v", err)
		}
	}
}

func (p *phone) readLogical(ctx context.Context) []byte {
	p.t.Helper()
	for {
		_, part, err := p.conn.Read(ctx)
		if err != nil {
			p.t.Fatalf("read chunk: %v", err)
		}
		if len(part) > framing.GatewayChunkSize {
			p.t.Fatalf("relay sent a %d byte chunk, above the %d budget", len(part), framing.GatewayChunkSize)
		}
		frame, err := p.assembly.Push(part)
		if err != nil {
			p.t.Fatalf("reassemble: %v", err)
		}
		if frame != nil {
			return frame
		}
	}
}

func (p *phone) handshake(ctx context.Context) {
	p.t.Helper()
	private, err := ecdh.P256().GenerateKey(rand.Reader)
	if err != nil {
		p.t.Fatal(err)
	}
	clientNonce := make([]byte, 32)
	if _, err := rand.Read(clientNonce); err != nil {
		p.t.Fatal(err)
	}
	clientPublic := private.PublicKey().Bytes()

	binding := []byte("herdr-e2ee-v2 auth\x00invitation\x00bootstrap\x001\x00")
	hello, err := json.Marshal(map[string]any{
		"type":         "e2ee_client_hello",
		"version":      2,
		"auth_kind":    "invitation",
		"auth_id":      "bootstrap",
		"auth_version": 1,
		"nonce":        base64.RawURLEncoding.EncodeToString(clientNonce),
		"public_key":   base64.RawURLEncoding.EncodeToString(clientPublic),
		"proof": base64.RawURLEncoding.EncodeToString(
			authTag([]byte("herdr-e2ee-v2 client\x00"), binding, clientNonce, clientPublic)),
	})
	if err != nil {
		p.t.Fatal(err)
	}
	p.writeLogical(ctx, hello)

	var serverHello struct {
		Type      string `json:"type"`
		Nonce     string `json:"nonce"`
		PublicKey string `json:"public_key"`
		Proof     string `json:"proof"`
	}
	if err := json.Unmarshal(p.readLogical(ctx), &serverHello); err != nil {
		p.t.Fatalf("decode server hello: %v", err)
	}
	if serverHello.Type != "e2ee_server_hello" {
		p.t.Fatalf("unexpected server hello type %q", serverHello.Type)
	}
	serverNonce, err := base64.RawURLEncoding.DecodeString(serverHello.Nonce)
	if err != nil {
		p.t.Fatal(err)
	}
	serverPublicBytes, err := base64.RawURLEncoding.DecodeString(serverHello.PublicKey)
	if err != nil {
		p.t.Fatal(err)
	}
	serverProof, err := base64.RawURLEncoding.DecodeString(serverHello.Proof)
	if err != nil {
		p.t.Fatal(err)
	}

	transcript := append(append(append(append(append([]byte{}, binding...), clientNonce...), clientPublic...), serverNonce...), serverPublicBytes...)
	if !hmac.Equal(serverProof, authTag([]byte("herdr-e2ee-v2 server\x00"), transcript)) {
		p.t.Fatal("relay key verification failed over the gateway path")
	}
	serverPublic, err := ecdh.P256().NewPublicKey(serverPublicBytes)
	if err != nil {
		p.t.Fatal(err)
	}
	shared, err := private.ECDH(serverPublic)
	if err != nil {
		p.t.Fatal(err)
	}
	keySalt := authTag([]byte("herdr-e2ee-v2 key\x00"), transcript)
	clientKey, err := hkdf.Key(sha256.New, shared, keySalt, "herdr-e2ee-v2 c2s", 32)
	if err != nil {
		p.t.Fatal(err)
	}
	serverKey, err := hkdf.Key(sha256.New, shared, keySalt, "herdr-e2ee-v2 s2c", 32)
	if err != nil {
		p.t.Fatal(err)
	}
	p.send = newAEAD(p.t, clientKey)
	p.receive = newAEAD(p.t, serverKey)

	finish, err := json.Marshal(map[string]any{"type": "e2ee_client_finish", "version": 2})
	if err != nil {
		p.t.Fatal(err)
	}
	p.writeLogical(ctx, p.seal(finish))
}

func (p *phone) sendMessage(ctx context.Context, payload map[string]any) {
	p.t.Helper()
	plaintext, err := json.Marshal(payload)
	if err != nil {
		p.t.Fatal(err)
	}
	p.writeLogical(ctx, p.seal(plaintext))
}

func (p *phone) readMessage(ctx context.Context) map[string]any {
	p.t.Helper()
	plaintext := p.open(p.readLogical(ctx))
	var message map[string]any
	if err := json.Unmarshal(plaintext, &message); err != nil {
		p.t.Fatalf("decode relay message: %v", err)
	}
	return message
}

// awaitMessage reads until a message of the wanted type arrives.
func (p *phone) awaitMessage(ctx context.Context, wanted string) map[string]any {
	p.t.Helper()
	for range 60 {
		message := p.readMessage(ctx)
		if message["type"] == wanted {
			return message
		}
	}
	p.t.Fatalf("relay never sent a %q message", wanted)
	return nil
}

func (p *phone) seal(plaintext []byte) []byte {
	sequence := p.sendSeq
	p.sendSeq++
	ciphertext := p.send.Seal(nil, frameNonce(sequence), plaintext, frameAAD("c2s", sequence))
	frame := make([]byte, 10+len(ciphertext))
	frame[0] = 2
	frame[1] = 0
	binary.BigEndian.PutUint64(frame[2:], sequence)
	copy(frame[10:], ciphertext)
	return frame
}

func (p *phone) open(frame []byte) []byte {
	p.t.Helper()
	if len(frame) < 10 || frame[0] != 2 || frame[1] != 0 {
		p.t.Fatalf("relay sent a malformed binary frame of %d bytes", len(frame))
	}
	sequence := binary.BigEndian.Uint64(frame[2:])
	if sequence != p.recvSeq {
		p.t.Fatalf("relay frame sequence = %d, want %d", sequence, p.recvSeq)
	}
	p.recvSeq++
	plaintext, err := p.receive.Open(nil, frameNonce(sequence), frame[10:], frameAAD("s2c", sequence))
	if err != nil {
		p.t.Fatalf("relay frame did not authenticate: %v", err)
	}
	return plaintext
}

func authTag(parts ...[]byte) []byte {
	mac := hmac.New(sha256.New, []byte(relayKey))
	for _, part := range parts {
		_, _ = mac.Write(part)
	}
	return mac.Sum(nil)
}

func newAEAD(t *testing.T, key []byte) cipher.AEAD {
	t.Helper()
	block, err := aes.NewCipher(key)
	if err != nil {
		t.Fatal(err)
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		t.Fatal(err)
	}
	return aead
}

func frameNonce(sequence uint64) []byte {
	nonce := make([]byte, 12)
	binary.BigEndian.PutUint64(nonce[4:], sequence)
	return nonce
}

func frameAAD(direction string, sequence uint64) []byte {
	prefix := "herdr-e2ee-v2 " + direction
	aad := make([]byte, len(prefix)+1+8)
	copy(aad, prefix)
	binary.BigEndian.PutUint64(aad[len(prefix)+1:], sequence)
	return aad
}

// TestGatewayPathControlsRelay is the Cloudflare-free acceptance test: a phone
// reaches a relay entirely through the blind gateway and drives a real command
// against fake-herdr, with the gateway never holding a key or seeing plaintext.
func TestGatewayPathControlsRelay(t *testing.T) {
	env := setupHybridEnv(t)
	status := waitForRegistration(t, env.relayHTTP)

	relayID, err := gatewaywire.DeriveRelayID(relayKey)
	if err != nil {
		t.Fatal(err)
	}
	if status["relay_id"] != relayID {
		t.Fatalf("relay advertised relay_id %v, want %v", status["relay_id"], relayID)
	}
	if status["enabled"] != true {
		t.Fatalf("gateway status not enabled: %v", status)
	}
	urls, _ := status["urls"].([]any)
	if len(urls) != 1 || urls[0] != env.gatewayWS {
		t.Fatalf("gateway status URLs = %v, want [%s]", urls, env.gatewayWS)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	p := dialPhone(t, env)

	config := p.awaitMessage(ctx, "push_config")
	if config["protocol"] != float64(3) {
		t.Fatalf("protocol = %v", config["protocol"])
	}
	hybrid, _ := config["hybrid"].(map[string]any)
	if hybrid == nil || hybrid["gateway_url"] != env.gatewayWS {
		t.Fatalf("relay did not advertise its hybrid descriptor: %v", config["hybrid"])
	}
	gatewayURLs, _ := hybrid["gateway_urls"].([]any)
	if len(gatewayURLs) != 1 || gatewayURLs[0] != env.gatewayWS {
		t.Fatalf("hybrid gateway URLs = %v, want [%s]", gatewayURLs, env.gatewayWS)
	}
	if hybrid["transport"] != "herdr-hybrid-v2" {
		t.Fatalf("hybrid transport id = %v", hybrid["transport"])
	}
	if hybrid["gateway_version"] != "dev" || hybrid["gateway_revision"] != "unknown" {
		t.Fatalf("hybrid gateway build = %v (%v), want dev (unknown)", hybrid["gateway_version"], hybrid["gateway_revision"])
	}
	if hybrid["gateway_available_version"] != "dev" {
		t.Fatalf("hybrid available gateway version = %v, want dev", hybrid["gateway_available_version"])
	}

	p.sendMessage(ctx, map[string]any{"type": "refresh_agents"})
	for range 60 {
		agents := p.awaitMessage(ctx, "agents")
		list, _ := agents["agents"].([]any)
		if len(list) == 0 {
			continue
		}
		agent, _ := list[0].(map[string]any)
		if agent["pane_id"] != "pane-1" || agent["agent"] != "claude" {
			t.Fatalf("unexpected agent snapshot: %v", agent)
		}
		target := map[string]any{
			"server_session_id": agent["server_session_id"],
			"pane_id":           agent["pane_id"],
			"terminal_id":       agent["terminal_id"],
			"generation":        agent["generation"],
			"agent_session_id":  agent["agent_session_id"],
		}

		p.sendMessage(ctx, map[string]any{
			"type":              "send_text",
			"protocol":          3,
			"request_id":        "gw-1",
			"pane_id":           "pane-1",
			"server_session_id": agent["server_session_id"],
			"target":            target,
			"text":              "hello over the gateway",
		})
		result := p.awaitMessage(ctx, "command_result")
		if result["request_id"] != "gw-1" {
			t.Fatalf("command_result request_id = %v", result["request_id"])
		}
		if result["ok"] != true {
			t.Fatalf("relayed command failed: %v", result)
		}
		return
	}
	t.Fatal("relay never delivered a populated agent snapshot over the gateway path")
}

// TestGatewayRejectsUnprovenPhone proves the relay, not the gateway, is the
// authority: the gateway pairs anyone who claims a registered relay id, and the
// relay drops the connection when the rendezvous proof does not verify.
func TestGatewayRejectsUnprovenPhone(t *testing.T) {
	env := setupHybridEnv(t)
	waitForRegistration(t, env.relayHTTP)

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	conn, _, err := websocket.Dial(ctx, env.gatewayWS+"/connect", nil)
	if err != nil {
		t.Fatalf("dial gateway: %v", err)
	}
	defer conn.CloseNow()

	if _, _, err := conn.Read(ctx); err != nil {
		t.Fatalf("read gateway hello: %v", err)
	}
	relayID, err := gatewaywire.DeriveRelayID(relayKey)
	if err != nil {
		t.Fatal(err)
	}
	connect, err := json.Marshal(gatewaywire.ConnectHello{
		Type:    gatewaywire.TypeConnect,
		Proto:   gatewaywire.Proto,
		RelayID: relayID,
		Proof:   base64.RawURLEncoding.EncodeToString(make([]byte, sha256.Size)),
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := conn.Write(ctx, websocket.MessageText, connect); err != nil {
		t.Fatalf("write connect hello: %v", err)
	}
	// The gateway pairs the connection because it holds no secret to check.
	kind, raw, err := conn.Read(ctx)
	if err != nil || kind != websocket.MessageText {
		t.Fatalf("read ready: %v", err)
	}
	var ready gatewaywire.ReadyMessage
	if err := json.Unmarshal(raw, &ready); err != nil || ready.Type != gatewaywire.TypeReady {
		t.Fatalf("gateway did not pair the unproven phone: %s", raw)
	}
	// The relay verifies and closes it, so the next read fails rather than
	// returning an encrypted handshake frame.
	readCtx, readCancel := context.WithTimeout(ctx, 10*time.Second)
	defer readCancel()
	if _, payload, err := conn.Read(readCtx); err == nil {
		t.Fatalf("unproven phone stayed connected and received %d bytes", len(payload))
	}
}

func TestGatewayCarriesChunkedAttachmentBatch(t *testing.T) {
	env := setupHybridEnv(t)
	waitForRegistration(t, env.relayHTTP)

	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	p := dialPhone(t, env)
	p.awaitMessage(ctx, "push_config")

	raw := make([]byte, 6*1024*1024)
	for index := range raw {
		raw[index] = byte(index)
	}
	copy(raw, []byte{0x89, 0x50, 0x4e, 0x47, 0x0d, 0x0a, 0x1a, 0x0a})
	p.sendMessage(ctx, map[string]any{"type": "refresh_agents"})
	var target map[string]any
	for range 60 {
		agentsMessage := p.awaitMessage(ctx, "agents")
		agents, _ := agentsMessage["agents"].([]any)
		if len(agents) == 0 {
			continue
		}
		agent, _ := agents[0].(map[string]any)
		target = map[string]any{
			"server_session_id": agent["server_session_id"],
			"pane_id":           agent["pane_id"],
			"terminal_id":       agent["terminal_id"],
			"generation":        agent["generation"],
		}
		break
	}
	if target == nil {
		t.Fatal("relay never delivered an attachment target")
	}
	p.sendMessage(ctx, map[string]any{
		"type": "upload_begin", "protocol": 3, "request_id": "upload-begin-1",
		"target": target,
		"files": []map[string]any{{
			"name": "big.png", "media_type": "image/png", "bytes": len(raw),
		}},
	})
	beginMessage := p.awaitMessage(ctx, "upload_begin_result")
	beginResult, _ := beginMessage["result"].(map[string]any)
	uploadID, _ := beginResult["upload_id"].(string)
	chunkBytes := int(beginResult["chunk_bytes"].(float64))
	if uploadID == "" || chunkBytes <= 0 {
		t.Fatalf("invalid begin result: %v", beginResult)
	}

	for sequence, offset := 0, 0; offset < len(raw); sequence++ {
		end := min(offset+chunkBytes, len(raw))
		chunk := raw[offset:end]
		p.sendMessage(ctx, map[string]any{
			"type": "upload_chunk", "protocol": 3,
			"request_id": fmt.Sprintf("upload-chunk-%d", sequence),
			"target":     target, "upload_id": uploadID, "file_index": 0, "sequence": sequence,
			"data":   base64.StdEncoding.EncodeToString(chunk),
			"sha256": fmt.Sprintf("%x", sha256.Sum256(chunk)),
		})
		result := p.awaitMessage(ctx, "upload_chunk_result")
		if result["error"] != nil {
			t.Fatalf("chunk %d failed: %v", sequence, result["error"])
		}
		offset = end
	}

	digest := fmt.Sprintf("%x", sha256.Sum256(raw))
	p.sendMessage(ctx, map[string]any{
		"type": "upload_finish", "protocol": 3, "request_id": "upload-finish-1",
		"target": target, "upload_id": uploadID,
		"files": []map[string]any{{"file_index": 0, "sha256": digest}},
	})
	finishMessage := p.awaitMessage(ctx, "upload_finish_result")
	finishResult, _ := finishMessage["result"].(map[string]any)
	attachments, _ := finishResult["attachments"].([]any)
	if len(attachments) != 1 {
		t.Fatalf("finish result = %v", finishMessage)
	}
	attachment, _ := attachments[0].(map[string]any)
	if attachment["ref"] == "" || attachment["bytes"] != float64(len(raw)) {
		t.Fatalf("attachment = %v", attachment)
	}

	p.sendMessage(ctx, map[string]any{"type": "refresh_agents"})
	p.awaitMessage(ctx, "agents")
}

// TestRelayWithoutGatewayIsUnchanged is the backward-compatibility contract for
// everyone still on a Cloudflare tunnel. With no HERDR_GATEWAY_URL configured
// the relay must not register anywhere, must not advertise a hybrid descriptor
// that would migrate an app off its working transport, and must not claim the
// direct-path capability.
func TestRelayWithoutGatewayIsUnchanged(t *testing.T) {
	env := setupEnv(t)

	gateway := gatewayHealth(t, env.httpBase)
	if gateway["enabled"] != false || gateway["registered"] != false {
		t.Fatalf("a relay with no gateway reported %v", gateway)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	conn, _, err := websocket.Dial(ctx, env.wsURL, nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close(websocket.StatusNormalClosure, "")

	for range 20 {
		_, data, err := conn.Read(ctx)
		if err != nil {
			t.Fatalf("read: %v", err)
		}
		var message map[string]any
		if err := json.Unmarshal(data, &message); err != nil || message["type"] != "push_config" {
			continue
		}
		if _, present := message["hybrid"]; present {
			t.Fatalf("legacy relay advertised a hybrid descriptor: %v", message["hybrid"])
		}
		capabilities, _ := message["capabilities"].([]any)
		for _, capability := range capabilities {
			if capability == "webrtc_direct" {
				t.Fatal("legacy relay advertised the direct-path capability")
			}
		}
		return
	}
	t.Fatal("relay never sent push_config")
}
