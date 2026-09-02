package transport

import (
	"bytes"
	"context"
	"crypto/ecdh"
	"crypto/hkdf"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/0cv/herdr-mobile-relay/internal/config"
	"github.com/coder/websocket"
)

type e2eeVector struct {
	Protocol string `json:"protocol"`
	Version  int    `json:"version"`
	RelayKey string `json:"relay_key"`
	Client   struct {
		PrivateKey string          `json:"private_key"`
		Nonce      string          `json:"nonce"`
		PublicKey  string          `json:"public_key"`
		Proof      string          `json:"proof"`
		Hello      e2eeClientHello `json:"hello"`
	} `json:"client"`
	Server struct {
		PrivateKey string          `json:"private_key"`
		Nonce      string          `json:"nonce"`
		PublicKey  string          `json:"public_key"`
		Proof      string          `json:"proof"`
		Hello      e2eeServerHello `json:"hello"`
	} `json:"server"`
	Transcript   string `json:"transcript"`
	SharedSecret string `json:"shared_secret"`
	KeySalt      string `json:"key_salt"`
	ClientKey    string `json:"c2s_key"`
	ServerKey    string `json:"s2c_key"`
	Records      struct {
		Finish e2eeVectorRecord `json:"client_finish"`
		Client e2eeVectorRecord `json:"c2s"`
		Server e2eeVectorRecord `json:"s2c"`
	} `json:"records"`
}

type e2eeVectorRecord struct {
	Sequence    uint64    `json:"sequence"`
	Plaintext   string    `json:"plaintext"`
	Nonce       string    `json:"nonce"`
	AAD         string    `json:"aad"`
	Ciphertext  string    `json:"ciphertext"`
	Frame       e2eeFrame `json:"frame"`
	BinaryFrame string    `json:"binary_frame"`
}
type fixedE2EEAuthResolver struct {
	secret []byte
}

func (r fixedE2EEAuthResolver) ResolveE2EESecret(_ context.Context, selector E2EEAuthSelector) ([]byte, error) {
	if selector != (E2EEAuthSelector{Kind: E2EEAuthCredential, ID: "credential-test", Version: 7}) {
		return nil, errors.New("unknown test credential")
	}
	return append([]byte(nil), r.secret...), nil
}

func (r fixedE2EEAuthResolver) CompleteE2EEAuth(_ context.Context, selector E2EEAuthSelector, authenticated bool) (E2EEAuthResult, error) {
	if !authenticated {
		return E2EEAuthResult{}, errors.New("test proof rejected")
	}
	if _, err := r.ResolveE2EESecret(context.Background(), selector); err != nil {
		return E2EEAuthResult{}, err
	}
	return E2EEAuthResult{Identity: AuthenticatedIdentity{
		DeviceID: "device-test", CredentialID: selector.ID, Role: "controller", Locale: "en", CredentialVersion: selector.Version,
	}}, nil
}

func (fixedE2EEAuthResolver) IsE2EEAuthRejected(error) bool { return true }

func TestE2EEVersionTwoVector(t *testing.T) {
	rawVector, err := os.ReadFile("../../contracts/fixtures/e2ee/v2.json")
	if err != nil {
		t.Fatal(err)
	}
	var vector e2eeVector
	if err := json.Unmarshal(rawVector, &vector); err != nil {
		t.Fatal(err)
	}
	if vector.Protocol != e2eeSubprotocol || vector.Version != e2eeVersion {
		t.Fatalf("vector protocol = %q v%d", vector.Protocol, vector.Version)
	}

	curve := ecdh.P256()
	clientPrivate, err := curve.NewPrivateKey(decodeVectorField(t, vector.Client.PrivateKey))
	if err != nil {
		t.Fatal(err)
	}
	serverPrivate, err := curve.NewPrivateKey(decodeVectorField(t, vector.Server.PrivateKey))
	if err != nil {
		t.Fatal(err)
	}
	clientNonce := decodeVectorField(t, vector.Client.Nonce)
	serverNonce := decodeVectorField(t, vector.Server.Nonce)
	clientPublic := clientPrivate.PublicKey().Bytes()
	serverPublic := serverPrivate.PublicKey().Bytes()
	if !bytes.Equal(clientPublic, decodeVectorField(t, vector.Client.PublicKey)) {
		t.Fatal("client public key does not match vector")
	}
	if !bytes.Equal(serverPublic, decodeVectorField(t, vector.Server.PublicKey)) {
		t.Fatal("server public key does not match vector")
	}
	selector := E2EEAuthSelector{Kind: vector.Client.Hello.AuthKind, ID: vector.Client.Hello.AuthID, Version: vector.Client.Hello.AuthVersion}
	binding := e2eeAuthBinding(selector)
	clientProof := e2eeAuthTag([]byte(vector.RelayKey), e2eeClientProofLabel, binding, clientNonce, clientPublic)
	if !bytes.Equal(clientProof, decodeVectorField(t, vector.Client.Proof)) {
		t.Fatal("client proof does not match vector")
	}
	if vector.Client.Hello != (e2eeClientHello{
		Type:        "e2ee_client_hello",
		Version:     e2eeVersion,
		AuthKind:    selector.Kind,
		AuthID:      selector.ID,
		AuthVersion: selector.Version,
		Nonce:       vector.Client.Nonce,
		PublicKey:   vector.Client.PublicKey,
		Proof:       vector.Client.Proof,
	}) {
		t.Fatal("client hello does not match vector fields")
	}

	transcript := e2eeTranscript(binding, clientNonce, clientPublic, serverNonce, serverPublic)
	if !bytes.Equal(transcript, decodeVectorField(t, vector.Transcript)) {
		t.Fatal("transcript does not match vector")
	}
	serverProof := e2eeAuthTag([]byte(vector.RelayKey), e2eeServerProofLabel, transcript)
	if !bytes.Equal(serverProof, decodeVectorField(t, vector.Server.Proof)) {
		t.Fatal("server proof does not match vector")
	}
	if vector.Server.Hello != (e2eeServerHello{
		Type:      "e2ee_server_hello",
		Version:   e2eeVersion,
		Nonce:     vector.Server.Nonce,
		PublicKey: vector.Server.PublicKey,
		Proof:     vector.Server.Proof,
	}) {
		t.Fatal("server hello does not match vector fields")
	}

	sharedSecret, err := clientPrivate.ECDH(serverPrivate.PublicKey())
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(sharedSecret, decodeVectorField(t, vector.SharedSecret)) {
		t.Fatal("shared secret does not match vector")
	}
	keySalt := e2eeAuthTag([]byte(vector.RelayKey), e2eeKeySaltLabel, transcript)
	if !bytes.Equal(keySalt, decodeVectorField(t, vector.KeySalt)) {
		t.Fatal("key salt does not match vector")
	}
	clientKey, err := hkdf.Key(sha256.New, sharedSecret, keySalt, "herdr-e2ee-v2 c2s", 32)
	if err != nil {
		t.Fatal(err)
	}
	serverKey, err := hkdf.Key(sha256.New, sharedSecret, keySalt, "herdr-e2ee-v2 s2c", 32)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(clientKey, decodeVectorField(t, vector.ClientKey)) {
		t.Fatal("client key does not match vector")
	}
	if !bytes.Equal(serverKey, decodeVectorField(t, vector.ServerKey)) {
		t.Fatal("server key does not match vector")
	}

	clientSession, err := newE2EESession(clientKey, serverKey, e2eeClientDirection, e2eeServerDirection)
	if err != nil {
		t.Fatal(err)
	}
	serverSession, err := newE2EESession(serverKey, clientKey, e2eeServerDirection, e2eeClientDirection)
	if err != nil {
		t.Fatal(err)
	}
	assertVectorRecord(t, clientSession, serverSession, vector.Records.Finish)
	assertVectorRecord(t, clientSession, serverSession, vector.Records.Client)
	assertVectorRecord(t, serverSession, clientSession, vector.Records.Server)
}

func assertVectorRecord(t *testing.T, sender, receiver *e2eeSession, record e2eeVectorRecord) {
	t.Helper()
	nonce := e2eeFrameNonce(record.Sequence)
	if !bytes.Equal(nonce[:], decodeVectorField(t, record.Nonce)) {
		t.Fatal("record nonce does not match vector")
	}
	if !bytes.Equal(e2eeAAD(sender.sendDirection, record.Sequence), decodeVectorField(t, record.AAD)) {
		t.Fatal("record AAD does not match vector")
	}
	rawFrame, err := sender.seal([]byte(record.Plaintext))
	if err != nil {
		t.Fatal(err)
	}
	var frame e2eeFrame
	if err := json.Unmarshal(rawFrame, &frame); err != nil {
		t.Fatal(err)
	}
	if frame != record.Frame || frame.Ciphertext != record.Ciphertext {
		t.Fatalf("encrypted frame = %+v, want %+v", frame, record.Frame)
	}
	opened, err := receiver.open(rawFrame)
	if err != nil {
		t.Fatal(err)
	}
	if string(opened) != record.Plaintext {
		t.Fatalf("opened plaintext = %q, want %q", opened, record.Plaintext)
	}
	// The binary codec is what the gateway and WebRTC paths carry; the phone
	// sniffs its header to tell sealed frames from the JSON handshake, so the
	// exact bytes are a cross-language contract.
	binaryFrame, err := CodecBinary.encodeFrame(record.Sequence, decodeVectorField(t, record.Ciphertext))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(binaryFrame, decodeVectorField(t, record.BinaryFrame)) {
		t.Fatalf("binary frame = %x, want %x", binaryFrame, decodeVectorField(t, record.BinaryFrame))
	}
}

func decodeVectorField(t *testing.T, value string) []byte {
	t.Helper()
	decoded, err := base64.RawURLEncoding.DecodeString(value)
	if err != nil {
		t.Fatal(err)
	}
	return decoded
}

func TestParseE2EEClientFinishRejectsMalformedPlaintext(t *testing.T) {
	if err := parseE2EEClientFinish([]byte(`{"type":"e2ee_client_finish","version":2}`)); err != nil {
		t.Fatal(err)
	}
	invalidUTF8 := append(
		[]byte(`{"type":"e2ee_client_finish","version":2,"unknown":"`),
		0xff,
	)
	invalidUTF8 = append(invalidUTF8, '"', '}')
	for _, plaintext := range [][]byte{
		[]byte(`null`),
		[]byte(`{"type":"e2ee_client_finish","version":1}`),
		invalidUTF8,
	} {
		if err := parseE2EEClientFinish(plaintext); err == nil {
			t.Fatal("malformed client finish was accepted")
		}
	}
}

func TestE2EESessionEncryptsAuthenticatesAndOrdersFrames(t *testing.T) {
	clientKey := bytes.Repeat([]byte{0x11}, 32)
	serverKey := bytes.Repeat([]byte{0x22}, 32)
	client, err := newE2EESession(clientKey, serverKey, e2eeClientDirection, e2eeServerDirection)
	if err != nil {
		t.Fatal(err)
	}
	server, err := newE2EESession(serverKey, clientKey, e2eeServerDirection, e2eeClientDirection)
	if err != nil {
		t.Fatal(err)
	}

	plaintext := []byte(`{"type":"submit_prompt","text":"cloudflare must not see this"}`)
	frame, err := client.seal(plaintext)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(frame, []byte("cloudflare")) || bytes.Contains(frame, []byte("submit_prompt")) {
		t.Fatalf("encrypted frame exposed plaintext: %s", frame)
	}
	opened, err := server.open(frame)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(opened, plaintext) {
		t.Fatalf("opened plaintext = %q, want %q", opened, plaintext)
	}
	if _, err := server.open(frame); err == nil {
		t.Fatal("replayed frame was accepted")
	}

	reply := []byte(`{"type":"pane_content","content":"private output"}`)
	replyFrame, err := server.seal(reply)
	if err != nil {
		t.Fatal(err)
	}
	openedReply, err := client.open(replyFrame)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(openedReply, reply) {
		t.Fatalf("opened reply = %q, want %q", openedReply, reply)
	}

	client.sendSequence = maxE2EESequence
	server.receiveSequence = maxE2EESequence
	lastFrame, err := client.seal([]byte(`{"type":"last"}`))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := server.open(lastFrame); err != nil {
		t.Fatalf("maximum safe sequence was rejected: %v", err)
	}
	if _, err := client.seal([]byte(`{"type":"overflow"}`)); err == nil {
		t.Fatal("send sequence beyond maximum safe integer was accepted")
	}
}

func TestEncryptedHubAuthenticatesBeforeRegistrationAndProtectsMessages(t *testing.T) {
	const token = "0123456789abcdef0123456789abcdef"
	hub := NewHub(&config.Config{Token: token}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	hub.SetE2EEAuthResolver(fixedE2EEAuthResolver{secret: []byte(token)})
	connected := make(chan struct{}, 1)
	received := make(chan map[string]any, 1)
	hub.SetOnConnect(func(client *ClientConn) {
		connected <- struct{}{}
		hub.Send(client, map[string]any{
			"type":    "pane_content",
			"content": "private terminal output",
		})
	})
	hub.SetHandler(func(_ *ClientConn, message map[string]any, admitted func()) {
		admitted()
		received <- message
	})

	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Skipf("local sockets unavailable: %v", err)
	}
	server := httptest.NewUnstartedServer(http.HandlerFunc(hub.HandleWebSocket))
	server.Listener = listener
	server.Start()
	defer server.Close()
	wsURL := "ws" + strings.TrimPrefix(server.URL, "http")

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	conn, response, err := websocket.Dial(ctx, wsURL, &websocket.DialOptions{
		Subprotocols: []string{e2eeSubprotocol},
	})
	if err != nil {
		t.Fatalf("dial encrypted websocket: %v", err)
	}
	defer conn.CloseNow()
	if response == nil || response.Header.Get("Sec-WebSocket-Protocol") != e2eeSubprotocol {
		t.Fatalf("negotiated subprotocol = %q, want %q", response.Header.Get("Sec-WebSocket-Protocol"), e2eeSubprotocol)
	}
	select {
	case <-connected:
		t.Fatal("client registered before encrypted handshake")
	default:
	}

	clientSession, capturedHello, capturedFinish := testClientE2EEHandshake(t, ctx, conn, token, func() {
		select {
		case <-connected:
			t.Fatal("client registered before encrypted finish")
		case <-time.After(50 * time.Millisecond):
		}
	})
	select {
	case <-connected:
	case <-ctx.Done():
		t.Fatal("client was not registered after encrypted handshake")
	}

	messageType, encryptedSnapshot, err := conn.Read(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if messageType != websocket.MessageText {
		t.Fatalf("snapshot message type = %v, want text", messageType)
	}
	if bytes.Contains(encryptedSnapshot, []byte("private terminal output")) {
		t.Fatalf("snapshot exposed plaintext: %s", encryptedSnapshot)
	}
	snapshot, err := clientSession.open(encryptedSnapshot)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(snapshot, []byte("private terminal output")) {
		t.Fatalf("decrypted snapshot = %s", snapshot)
	}

	command, err := clientSession.seal([]byte(`{"type":"refresh_agents"}`))
	if err != nil {
		t.Fatal(err)
	}
	if err := conn.Write(ctx, websocket.MessageText, command); err != nil {
		t.Fatal(err)
	}
	select {
	case message := <-received:
		if message["type"] != "refresh_agents" {
			t.Fatalf("received type = %v", message["type"])
		}
	case <-ctx.Done():
		t.Fatal("relay did not receive encrypted client message")
	}

	replayConn, _, err := websocket.Dial(ctx, wsURL, &websocket.DialOptions{
		Subprotocols: []string{e2eeSubprotocol},
	})
	if err != nil {
		t.Fatalf("dial replay websocket: %v", err)
	}
	defer replayConn.CloseNow()
	if err := replayConn.Write(ctx, websocket.MessageText, capturedHello); err != nil {
		t.Fatal(err)
	}
	if _, _, err := replayConn.Read(ctx); err != nil {
		t.Fatalf("read replay server hello: %v", err)
	}
	if err := replayConn.Write(ctx, websocket.MessageText, capturedFinish); err != nil {
		t.Fatal(err)
	}
	if _, _, err := replayConn.Read(ctx); err == nil {
		t.Fatal("captured client finish authenticated against a fresh server hello")
	}
	select {
	case <-connected:
		t.Fatal("replayed handshake registered a client")
	case <-time.After(50 * time.Millisecond):
	}

	malformed, err := clientSession.seal([]byte(`not-json`))
	if err != nil {
		t.Fatal(err)
	}
	if err := conn.Write(ctx, websocket.MessageText, malformed); err != nil {
		t.Fatal(err)
	}
	if _, _, err := conn.Read(ctx); err == nil {
		t.Fatal("relay kept encrypted connection open after malformed plaintext")
	}

	conn.CloseNow()
	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer shutdownCancel()
	if err := hub.Shutdown(shutdownCtx); err != nil {
		t.Fatal(err)
	}
}

func TestEncryptedHubRejectsHTTPCredentials(t *testing.T) {
	hub := NewHub(&config.Config{Token: "secret"}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	request := httptest.NewRequest("GET", "/ws?token=secret", nil)
	request.Header.Set("Sec-WebSocket-Protocol", e2eeSubprotocol)
	response := httptest.NewRecorder()
	hub.HandleWebSocket(response, request)
	if response.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d", response.Code, http.StatusBadRequest)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := hub.Shutdown(ctx); err != nil {
		t.Fatal(err)
	}
}

func testClientE2EEHandshake(
	t *testing.T,
	ctx context.Context,
	conn *websocket.Conn,
	token string,
	beforeFinish func(),
) (*e2eeSession, []byte, []byte) {
	t.Helper()
	curve := ecdh.P256()
	privateKey, err := curve.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	clientNonce := make([]byte, e2eeNonceBytes)
	if _, err := rand.Read(clientNonce); err != nil {
		t.Fatal(err)
	}
	selector := E2EEAuthSelector{Kind: E2EEAuthCredential, ID: "credential-test", Version: 7}
	binding := e2eeAuthBinding(selector)
	clientPublic := privateKey.PublicKey().Bytes()
	clientProof := e2eeAuthTag([]byte(token), e2eeClientProofLabel, binding, clientNonce, clientPublic)
	hello, err := json.Marshal(e2eeClientHello{
		Type:        "e2ee_client_hello",
		Version:     e2eeVersion,
		AuthKind:    selector.Kind,
		AuthID:      selector.ID,
		AuthVersion: selector.Version,
		Nonce:       base64.RawURLEncoding.EncodeToString(clientNonce),
		PublicKey:   base64.RawURLEncoding.EncodeToString(clientPublic),
		Proof:       base64.RawURLEncoding.EncodeToString(clientProof),
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := conn.Write(ctx, websocket.MessageText, hello); err != nil {
		t.Fatal(err)
	}
	messageType, rawServerHello, err := conn.Read(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if messageType != websocket.MessageText {
		t.Fatalf("server hello message type = %v", messageType)
	}
	var serverHello e2eeServerHello
	if err := json.Unmarshal(rawServerHello, &serverHello); err != nil {
		t.Fatal(err)
	}
	serverNonce, err := decodeE2EEField(serverHello.Nonce, e2eeNonceBytes)
	if err != nil {
		t.Fatal(err)
	}
	serverPublicBytes, err := decodeE2EEField(serverHello.PublicKey, e2eePublicKeyBytes)
	if err != nil {
		t.Fatal(err)
	}
	serverProof, err := decodeE2EEField(serverHello.Proof, sha256.Size)
	if err != nil {
		t.Fatal(err)
	}
	transcript := e2eeTranscript(binding, clientNonce, clientPublic, serverNonce, serverPublicBytes)
	wantServerProof := e2eeAuthTag([]byte(token), e2eeServerProofLabel, transcript)
	if !hmac.Equal(serverProof, wantServerProof) {
		t.Fatal("server proof did not authenticate")
	}
	serverPublic, err := curve.NewPublicKey(serverPublicBytes)
	if err != nil {
		t.Fatal(err)
	}
	sharedSecret, err := privateKey.ECDH(serverPublic)
	if err != nil {
		t.Fatal(err)
	}
	keySalt := e2eeAuthTag([]byte(token), e2eeKeySaltLabel, transcript)
	clientKey, err := hkdf.Key(sha256.New, sharedSecret, keySalt, "herdr-e2ee-v2 c2s", 32)
	if err != nil {
		t.Fatal(err)
	}
	serverKey, err := hkdf.Key(sha256.New, sharedSecret, keySalt, "herdr-e2ee-v2 s2c", 32)
	if err != nil {
		t.Fatal(err)
	}
	session, err := newE2EESession(clientKey, serverKey, e2eeClientDirection, e2eeServerDirection)
	if err != nil {
		t.Fatal(err)
	}
	beforeFinish()
	finish, err := json.Marshal(e2eeClientFinish{
		Type:    "e2ee_client_finish",
		Version: e2eeVersion,
	})
	if err != nil {
		t.Fatal(err)
	}
	encryptedFinish, err := session.seal(finish)
	if err != nil {
		t.Fatal(err)
	}
	if err := conn.Write(ctx, websocket.MessageText, encryptedFinish); err != nil {
		t.Fatal(err)
	}
	messageType, encryptedServerFinish, err := conn.Read(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if messageType != websocket.MessageText {
		t.Fatalf("server finish message type = %v, want text", messageType)
	}
	plaintextServerFinish, err := session.open(encryptedServerFinish)
	if err != nil {
		t.Fatal(err)
	}
	var serverFinish e2eeServerFinish
	if err := json.Unmarshal(plaintextServerFinish, &serverFinish); err != nil {
		t.Fatal(err)
	}
	if serverFinish.Type != "e2ee_server_finish" || serverFinish.DeviceID != "device-test" ||
		serverFinish.CredentialID != selector.ID || serverFinish.CredentialVersion != selector.Version {
		t.Fatalf("server finish = %#v", serverFinish)
	}
	return session, hello, encryptedFinish
}
