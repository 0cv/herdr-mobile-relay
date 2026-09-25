//go:build herdr_tailscale_test

package app

import (
	"bytes"
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
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/0cv/herdr-mobile-relay/internal/deviceauth"
	"github.com/0cv/herdr-mobile-relay/internal/localcontrol"
	"github.com/0cv/herdr-mobile-relay/internal/protocol"
	"github.com/0cv/herdr-mobile-relay/internal/transport"
	"github.com/coder/websocket"
)

func TestTailscaleExternalBYOHostedE2EE(t *testing.T) {
	origin := strings.TrimSuffix(os.Getenv("HERDR_EXTERNAL_FIXTURE_ORIGIN"), "/")
	token := os.Getenv("HERDR_EXTERNAL_FIXTURE_TOKEN")
	if !strings.HasPrefix(origin, "https://") || len(token) != 32 {
		t.Fatal("hosted BYO HTTPS fixture origin/token is missing")
	}
	endpoint := "wss" + strings.TrimPrefix(origin, "https") + "/ws"
	client := &http.Client{Transport: &http.Transport{Proxy: nil}}
	connection, enrollment := managedFixtureEnrollOverWebSocketURL(t, endpoint, token, client)
	defer connection.CloseNow()
	if enrollment.Role != "controller" || enrollment.CredentialID == "" || enrollment.CredentialSecret == "" {
		t.Fatal("hosted BYO WSS did not complete authenticated E2EE enrollment")
	}
	enrollment.CredentialSecret = ""
}

func TestManagedTailscaleHostedPositiveActivationArmEnrollAndRetire(t *testing.T) {
	fixture := newManagedTailscaleFixture(t)
	var durableBeforeAcknowledgement atomic.Bool
	arm := func(ctx context.Context) (localcontrol.Status, error) {
		status, err := fixture.server.armForControl(ctx)
		if err != nil {
			return status, err
		}
		data, readErr := os.ReadFile(filepath.Join(fixture.root, "device-auth", "devices.json"))
		if readErr == nil && fixture.server.bootstrapGate.OpenStatus() {
			var state struct {
				Invitation struct {
					ID      string `json:"invitation_id"`
					Version uint64 `json:"version"`
					Secret  string `json:"secret"`
				} `json:"invitation"`
			}
			if json.Unmarshal(data, &state) == nil && state.Invitation.ID == "bootstrap" && state.Invitation.Version == 1 && state.Invitation.Secret != "" {
				durableBeforeAcknowledgement.Store(true)
			}
		}
		return status, nil
	}
	fixture.startControl(arm)

	activated := fixture.activate(t)
	if !activated.ServeReady || !activated.OwnerHeld || activated.RouteCleared {
		t.Fatalf("activation status = %+v, want live ready owned route", activated)
	}
	if _, err := os.Lstat(filepath.Join(fixture.root, "device-auth")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("device store exists before explicit arm: %v", err)
	}
	preArmCtx, cancelPreArm := context.WithTimeout(context.Background(), 5*time.Second)
	preArmConn, preArmResponse, preArmErr := websocket.Dial(preArmCtx, "wss://"+fixture.hostPort+"/ws", &websocket.DialOptions{
		HTTPClient:   managedHealthClientForServer(fixture.server, 0),
		Subprotocols: []string{protocol.EncryptedWebSocketSubprotocol},
	})
	cancelPreArm()
	if preArmConn != nil {
		_ = preArmConn.CloseNow()
	}
	if preArmResponse != nil && preArmResponse.Body != nil {
		_ = preArmResponse.Body.Close()
	}
	if preArmErr == nil || preArmResponse == nil || preArmResponse.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("pre-arm public WebSocket admission = status %v, error %v; want HTTP 503", preArmStatus(preArmResponse), preArmErr)
	}
	watchCount, postCount, watchQuery, registrationValid, _ := fixture.localAPI.snapshot()
	if watchCount != 1 || postCount != 1 || watchQuery != "mask=2" || !registrationValid {
		t.Fatalf("real LocalAPI registration = watch %d, POST %d, query %q, valid route=%t", watchCount, postCount, watchQuery, registrationValid)
	}

	armed, err := fixture.arm(t)
	if err != nil {
		t.Fatalf("localcontrol arm_bootstrap: %v", err)
	}
	if !armed.Ready || !armed.InvitationArmed || !durableBeforeAcknowledgement.Load() {
		t.Fatalf("arm response = %+v; durable commit before acknowledgement=%t", armed, durableBeforeAcknowledgement.Load())
	}
	if !fixture.server.bootstrapGate.OpenStatus() {
		t.Fatal("BootstrapGate did not open after the transactional invitation write")
	}
	if fixture.server.deviceStore() == nil {
		t.Fatal("managed device store was not attached after activation and preflight")
	}
	fixture.publicMu.Lock()
	prematureStore := fixture.deviceStoreAppearedDuringPublic
	fixture.publicMu.Unlock()
	if prematureStore {
		t.Fatal("device-auth store appeared before all actual public HTTPS health and bundle checks completed")
	}

	wsClient, enrolled := managedFixtureEnrollOverWebSocket(t, fixture)
	defer wsClient.CloseNow()
	waitManagedFixture(t, "authenticated websocket admission", func() bool { return fixture.server.hub.ClientCount() == 1 })
	identities := fixture.server.hub.ConnectedIdentities()
	if len(identities) != 1 || identities[0].DeviceID != enrolled.DeviceID || identities[0].CredentialID != enrolled.CredentialID {
		t.Fatalf("authenticated WebSocket identity = %+v; server finish identity = %q/%q", identities, enrolled.DeviceID, enrolled.CredentialID)
	}
	credentialSecret, err := base64.RawURLEncoding.DecodeString(enrolled.CredentialSecret)
	if err != nil || len(credentialSecret) != 32 {
		t.Fatalf("server did not issue a real 32-byte enrolled credential: len=%d err=%v", len(credentialSecret), err)
	}
	clear(credentialSecret)
	credentials := fixture.server.deviceStore().ListCredentials("")
	if len(credentials) != 1 {
		t.Fatalf("persisted enrollment credentials = %d", len(credentials))
	}

	retired, err := localcontrol.Request(context.Background(), fixture.server.cfg.PairingSocketPath, "retire", managedFixtureRun, managedFixtureInstance)
	if err != nil {
		t.Fatalf("localcontrol retire: %v", err)
	}
	if !retired.RouteCleared || !retired.LocalWatchClosed || !retired.OwnerHeld || fixture.server.bootstrapGate.OpenStatus() {
		t.Fatalf("retirement status = %+v; bootstrap open=%t", retired, fixture.server.bootstrapGate.OpenStatus())
	}
	waitManagedFixture(t, "websocket closure on retirement", func() bool { return fixture.server.hub.ClientCount() == 0 })
	credentials = fixture.server.deviceStore().ListCredentials("")
	if len(credentials) != 1 {
		t.Fatalf("retirement removed enrolled credentials: count=%d", len(credentials))
	}
}

func TestManagedTailscaleWatchEOFAtAdmissionHandoffNeverReopensOrEnrolls(t *testing.T) {
	fixture := newManagedTailscaleFixture(t)
	fixture.startControl(nil)
	activated := fixture.activate(t)
	if !activated.ServeReady {
		t.Fatalf("activation status = %+v", activated)
	}
	// This barrier runs while SessionAuthority still owns its operation lock,
	// after the app has observed the arm commit state but immediately before
	// the gate-to-Hub admission transition. The raw LocalAPI stream closes
	// synchronously; asynchronous invalidation cannot be the security gate.
	authority, ok := fixture.server.tailscaleSession.(interface {
		AdmissionChannels() (<-chan struct{}, <-chan struct{})
	})
	if !ok {
		t.Fatal("fixture does not expose the real authority admission channels")
	}
	watchEnded, _ := authority.AdmissionChannels()
	fixture.server.managedAdmissionHandoffObserver = func() {
		fixture.localAPI.endWatch()
		select {
		case <-watchEnded:
		case <-time.After(2 * time.Second):
		}
	}
	armed, armErr := fixture.arm(t)
	if armErr == nil {
		t.Fatalf("watch EOF at Hub handoff unexpectedly returned an arm acknowledgement: %+v", armed)
	}
	if fixture.server.bootstrapGate.OpenStatus() {
		t.Fatal("watch EOF at handoff left invitation resolution logically open")
	}
	if status := fixture.server.tailscaleSession.Status(); !status.Invalidated || status.RouteCleared {
		t.Fatalf("authority status after raw watch EOF = %+v", status)
	}
	store := fixture.server.deviceStore()
	if store == nil || len(store.ListCredentials("")) != 0 {
		t.Fatalf("watch EOF at handoff enrolled a device: store=%v", store != nil)
	}
	selector := transport.E2EEAuthSelector{Kind: transport.E2EEAuthInvitation, ID: "bootstrap", Version: 1, Locale: "en"}
	if _, err := fixture.server.bootstrapGate.ResolveE2EESecret(context.Background(), selector); !errors.Is(err, deviceauth.ErrBootstrapGateClosed) {
		t.Fatalf("watch-ended invitation resolution = %v", err)
	}
	if _, err := fixture.server.bootstrapGate.CompleteE2EEAuth(context.Background(), selector, true); !errors.Is(err, deviceauth.ErrBootstrapGateClosed) {
		t.Fatalf("watch-ended invitation completion = %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	connection, response, dialErr := websocket.Dial(ctx, "wss://"+fixture.hostPort+"/ws", &websocket.DialOptions{
		HTTPClient:   managedHealthClientForServer(fixture.server, 0),
		Subprotocols: []string{protocol.EncryptedWebSocketSubprotocol},
	})
	cancel()
	if connection != nil {
		_ = connection.CloseNow()
	}
	if response != nil && response.Body != nil {
		_ = response.Body.Close()
	}
	if dialErr == nil || response == nil || response.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("watch-ended Hub admission = status %v, error %v; want HTTP 503", preArmStatus(response), dialErr)
	}
}

func TestManagedTailscaleDelayedVerifierCompletesWithinLifecycleBudgets(t *testing.T) {
	fixture := newManagedTailscaleFixture(t)
	fixture.startControl(nil)
	started, release := fixture.delayPublicVersionAfter(1)
	t.Cleanup(release)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	type reply struct {
		status localcontrol.Response
		err    error
	}
	done := make(chan reply, 1)
	go func() {
		status, err := localcontrol.Request(ctx, fixture.server.cfg.PairingSocketPath, "activate", managedFixtureRun, managedFixtureInstance)
		done <- reply{status: status, err: err}
	}()
	select {
	case <-started:
	case <-time.After(15 * time.Second):
		cancel()
		t.Fatal("activation did not reach the deliberately delayed bundle verifier")
	}
	release()
	select {
	case result := <-done:
		if result.err != nil || !result.status.ServeReady || result.status.Ready {
			t.Fatalf("delayed activation response = %+v, err=%v; want owner-ready but still-unarmed", result.status, result.err)
		}
	case <-time.After(15 * time.Second):
		cancel()
		t.Fatal("activation exceeded its explicit deadline after the verifier was released")
	}
	fixture.waitControlCallback(t, "activate")
	if _, err := os.Lstat(filepath.Join(fixture.root, "device-auth")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("delayed activation created device-auth before explicit arm: %v", err)
	}

	armStarted, releaseArm := fixture.delayPublicVersionAfter(1)
	t.Cleanup(releaseArm)
	armCtx, cancelArm := context.WithCancel(context.Background())
	defer cancelArm()
	armDone := make(chan reply, 1)
	go func() {
		status, err := localcontrol.Request(armCtx, fixture.server.cfg.PairingSocketPath, "arm_bootstrap", managedFixtureRun, managedFixtureInstance)
		armDone <- reply{status: status, err: err}
	}()
	select {
	case <-armStarted:
	case <-time.After(15 * time.Second):
		cancelArm()
		t.Fatal("arm did not reach its deliberately delayed bundle verifier")
	}
	releaseArm()
	select {
	case result := <-armDone:
		if result.err != nil || !result.status.Ready || !result.status.InvitationArmed {
			t.Fatalf("delayed arm response = %+v, err=%v", result.status, result.err)
		}
	case <-time.After(15 * time.Second):
		cancelArm()
		t.Fatal("arm exceeded its explicit deadline after the verifier was released")
	}
	fixture.waitControlCallback(t, "arm_bootstrap")
	if !fixture.server.bootstrapGate.OpenStatus() {
		t.Fatal("delayed verifier arm did not open the durable invitation gate")
	}
}

func TestManagedTailscaleVerifierCancellationDoesNotPersistInvitation(t *testing.T) {
	for _, operation := range []string{"activate", "arm_bootstrap"} {
		t.Run(operation, func(t *testing.T) {
			fixture := newManagedTailscaleFixture(t)
			fixture.startControl(nil)
			if operation == "arm_bootstrap" {
				fixture.activate(t)
			}
			versionOffset := 1
			if operation == "arm_bootstrap" {
				// Two version representations belong to the arm preflight; this
				// stalls the subsequent full verification inside the commit gate.
				versionOffset = 3
			}
			started, release := fixture.delayPublicVersionAfter(versionOffset)
			t.Cleanup(release)
			requestErr := cancelManagedControlAtVerifier(t, fixture, operation, started)
			if requestErr == nil {
				t.Fatal("canceling the delayed owner operation unexpectedly received an acknowledgement")
			}
			fixture.waitControlCallback(t, operation)
			if _, err := os.Lstat(filepath.Join(fixture.root, "device-auth")); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("canceled %s persisted device-auth state: %v", operation, err)
			}
			if fixture.server.bootstrapGate.OpenStatus() {
				t.Fatalf("canceled %s opened BootstrapGate", operation)
			}
		})
	}
}

func cancelManagedControlAtVerifier(t *testing.T, fixture *managedTailscaleFixture, operation string, verifierStarted <-chan struct{}) error {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		_, err := localcontrol.Request(ctx, fixture.server.cfg.PairingSocketPath, operation, managedFixtureRun, managedFixtureInstance)
		done <- err
	}()
	select {
	case <-verifierStarted:
	case <-time.After(15 * time.Second):
		cancel()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
		}
		t.Fatalf("%s did not reach the deliberately delayed public verifier", operation)
	}
	cancel()
	select {
	case err := <-done:
		return err
	case <-time.After(15 * time.Second):
		t.Fatalf("canceled %s did not return promptly", operation)
		return nil
	}
}

func TestManagedTailscaleLaterAdmissionFailuresDoNotPersistInvitation(t *testing.T) {
	cases := []struct {
		name   string
		inject func(*managedTailscaleFixture)
	}{
		{name: "local readiness on final admission", inject: func(f *managedTailscaleFixture) { f.failAfterNextArmReadinessPass(true, false) }},
		{name: "trusted public health on final admission", inject: func(f *managedTailscaleFixture) { f.failAfterNextArmReadinessPass(false, true) }},
		{name: "public bundle identity on final admission", inject: (*managedTailscaleFixture).tamperBundleAfterArmPreflight},
		{name: "Tailscale owner identity on final admission", inject: (*managedTailscaleFixture).driftOwnerAfterArmPreflight},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fixture := newManagedTailscaleFixture(t)
			fixture.startControl(nil)
			if activated := fixture.activate(t); !activated.ServeReady {
				t.Fatalf("activation did not complete before late-failure injection: %+v", activated)
			}
			tc.inject(fixture)
			if _, err := fixture.arm(t); err == nil {
				t.Fatal("late readiness/owner failure unexpectedly armed a bootstrap invitation")
			}
			if _, err := os.Lstat(filepath.Join(fixture.root, "device-auth")); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("late pre-commit failure persisted device-auth state: %v", err)
			}
			if fixture.server.bootstrapGate.OpenStatus() {
				t.Fatal("late pre-commit failure opened BootstrapGate")
			}
			if tc.name == "Tailscale owner identity on final admission" {
				fixture.localAPI.restoreIdentity()
			}
		})
	}
}

func TestManagedTailscaleRearmAfterEnrollmentAndLostAcknowledgement(t *testing.T) {
	fixture := newManagedTailscaleFixture(t)
	var armCalls atomic.Int32
	committed := make(chan struct{})
	callbackDone := make(chan struct{})
	fixture.startControl(func(ctx context.Context) (localcontrol.Status, error) {
		call := armCalls.Add(1)
		status, err := fixture.server.armForControl(ctx)
		if err != nil || call != 2 {
			return status, err
		}
		close(committed)
		<-ctx.Done()
		close(callbackDone)
		return status, ctx.Err()
	})
	fixture.activate(t)
	if status, err := fixture.arm(t); err != nil || !status.InvitationArmed {
		t.Fatalf("initial arm = %+v, %v", status, err)
	}
	connection, enrolled := managedFixtureEnrollOverWebSocket(t, fixture)
	defer connection.CloseNow()
	waitManagedFixture(t, "first enrolled device", func() bool {
		return len(fixture.server.deviceStore().ListCredentials("")) == 1
	})

	ctx, cancel := context.WithCancel(context.Background())
	requestDone := make(chan error, 1)
	go func() {
		_, err := localcontrol.Request(ctx, fixture.server.cfg.PairingSocketPath, "arm_bootstrap", managedFixtureRun, managedFixtureInstance)
		requestDone <- err
	}()
	select {
	case <-committed:
	case <-time.After(30 * time.Second):
		cancel()
		t.Fatal("reprint did not commit after normal enrollment")
	}
	cancel()
	select {
	case err := <-requestDone:
		if err == nil {
			t.Fatal("lost reprint acknowledgement unexpectedly succeeded")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("lost reprint acknowledgement did not return")
	}
	select {
	case <-callbackDone:
	case <-time.After(time.Second):
		t.Fatal("lost reprint acknowledgement did not cancel the control callback")
	}
	fixture.waitControlCallback(t, "arm_bootstrap")

	store := fixture.server.deviceStore()
	credentials := store.ListCredentials("")
	if len(credentials) != 1 || credentials[0].CredentialID != enrolled.CredentialID {
		t.Fatalf("reprint reset or lost enrolled credentials: %+v", credentials)
	}
	if !fixture.server.bootstrapGate.OpenStatus() || !store.BootstrapStatus().Armed {
		t.Fatal("committed reprint invitation was rolled back after its lost acknowledgement")
	}
	selector := transport.E2EEAuthSelector{Kind: transport.E2EEAuthInvitation, ID: "bootstrap", Version: 1, Locale: "en"}
	secret, err := fixture.server.bootstrapGate.ResolveE2EESecret(context.Background(), selector)
	if err != nil || !bytes.Equal(secret, []byte(fixture.server.cfg.Token)) {
		t.Fatalf("persisted reprint invitation is unusable: err=%v", err)
	}
	clear(secret)
	secondConnection, secondEnrollment := managedFixtureEnrollOverWebSocket(t, fixture)
	defer secondConnection.CloseNow()
	waitManagedFixture(t, "reprinted invitation enrollment", func() bool {
		return len(store.ListCredentials("")) == 2
	})
	credentials = store.ListCredentials("")
	if secondEnrollment.CredentialID == enrolled.CredentialID || len(credentials) != 2 {
		t.Fatalf("reprinted invitation did not create a distinct credential: first=%q second=%q stored=%+v", enrolled.CredentialID, secondEnrollment.CredentialID, credentials)
	}
}

func TestManagedTailscaleCommittedArmSurvivesLostControlAcknowledgement(t *testing.T) {
	fixture := newManagedTailscaleFixture(t)
	committed := make(chan struct{})
	callbackDone := make(chan struct{})
	fixture.startControl(func(ctx context.Context) (localcontrol.Status, error) {
		status, err := fixture.server.armForControl(ctx)
		if err != nil {
			return status, err
		}
		close(committed)
		<-ctx.Done()
		close(callbackDone)
		return status, ctx.Err()
	})
	fixture.activate(t)
	connection, err := net.DialTimeout("unix", fixture.server.cfg.PairingSocketPath, time.Second)
	if err != nil {
		t.Fatalf("connect to real managed control socket: %v", err)
	}
	request := fmt.Sprintf(`{"protocol":1,"op":"arm_bootstrap","run_id":%q,"instance":%q}`+"\n", managedFixtureRun, managedFixtureInstance)
	if _, err := connection.Write([]byte(request)); err != nil {
		_ = connection.Close()
		t.Fatalf("write arm request: %v", err)
	}
	select {
	case <-committed:
	case <-time.After(30 * time.Second):
		_ = connection.Close()
		t.Fatal("durable arm did not commit")
	}
	if err := connection.Close(); err != nil {
		t.Fatalf("close control connection before acknowledgement: %v", err)
	}
	select {
	case <-callbackDone:
	case <-time.After(time.Second):
		t.Fatal("lost acknowledgement did not cancel the in-flight response")
	}
	if !fixture.server.bootstrapGate.OpenStatus() {
		t.Fatal("lost acknowledgement rolled back the committed BootstrapGate")
	}
	data, err := os.ReadFile(filepath.Join(fixture.root, "device-auth", "devices.json"))
	if err != nil || !strings.Contains(string(data), `"secret"`) {
		t.Fatalf("durable invitation disappeared after lost acknowledgement: read error=%v has-secret-field=%t bytes=%d", err, strings.Contains(string(data), `"secret"`), len(data))
	}
	if status := fixture.server.tailscaleSession.Status(); !status.Active || status.RouteCleared {
		t.Fatalf("lost acknowledgement incorrectly retired the live Tailscale route: %+v", status)
	}
}

type managedFixtureE2EEFinish struct {
	Type              string `json:"type"`
	Version           int    `json:"version"`
	DeviceID          string `json:"device_id"`
	CredentialID      string `json:"credential_id"`
	Role              string `json:"role"`
	Locale            string `json:"locale"`
	CredentialVersion uint64 `json:"credential_version"`
	CredentialSecret  string `json:"credential_secret"`
}

func preArmStatus(response *http.Response) any {
	if response == nil {
		return "no response"
	}
	return response.StatusCode
}

func managedFixtureEnrollOverWebSocket(t *testing.T, fixture *managedTailscaleFixture) (*websocket.Conn, managedFixtureE2EEFinish) {
	return managedFixtureEnrollOverWebSocketURL(t, "wss://"+fixture.hostPort+"/ws", fixture.server.cfg.Token, managedHealthClientForServer(fixture.server, 0))
}

func managedFixtureEnrollOverWebSocketURL(t *testing.T, endpoint, token string, client *http.Client) (*websocket.Conn, managedFixtureE2EEFinish) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	connection, response, err := websocket.Dial(ctx, endpoint, &websocket.DialOptions{
		HTTPClient:   client,
		Subprotocols: []string{protocol.EncryptedWebSocketSubprotocol},
	})
	if err != nil {
		t.Fatalf("dial real app WebSocket: %v", err)
	}
	if response == nil || response.Header.Get("Sec-WebSocket-Protocol") != protocol.EncryptedWebSocketSubprotocol {
		_ = connection.CloseNow()
		t.Fatalf("WebSocket subprotocol = %v, want encrypted protocol", response)
	}

	selectorKind, selectorID, selectorVersion, locale := "invitation", "bootstrap", uint64(1), "en"
	secret := []byte(token)
	defer clear(secret)
	privateKey, err := ecdh.P256().GenerateKey(rand.Reader)
	if err != nil {
		_ = connection.CloseNow()
		t.Fatal(err)
	}
	clientNonce := make([]byte, 32)
	if _, err := rand.Read(clientNonce); err != nil {
		_ = connection.CloseNow()
		t.Fatal(err)
	}
	clientPublic := privateKey.PublicKey().Bytes()
	binding := []byte("herdr-e2ee-v2 auth\x00" + selectorKind + "\x00" + selectorID + "\x00" + fmt.Sprint(selectorVersion) + "\x00")
	clientProof := managedFixtureAuthTag(secret, []byte("herdr-e2ee-v2 client\x00"), binding, clientNonce, clientPublic)
	defer clear(clientProof)
	hello, err := json.Marshal(struct {
		Type        string `json:"type"`
		Version     int    `json:"version"`
		AuthKind    string `json:"auth_kind"`
		AuthID      string `json:"auth_id"`
		AuthVersion uint64 `json:"auth_version"`
		Locale      string `json:"locale"`
		Nonce       string `json:"nonce"`
		PublicKey   string `json:"public_key"`
		Proof       string `json:"proof"`
	}{
		Type: "e2ee_client_hello", Version: 2, AuthKind: selectorKind, AuthID: selectorID,
		AuthVersion: selectorVersion, Locale: locale,
		Nonce:     base64.RawURLEncoding.EncodeToString(clientNonce),
		PublicKey: base64.RawURLEncoding.EncodeToString(clientPublic),
		Proof:     base64.RawURLEncoding.EncodeToString(clientProof),
	})
	if err != nil {
		_ = connection.CloseNow()
		t.Fatal(err)
	}
	if err := connection.Write(ctx, websocket.MessageText, hello); err != nil {
		_ = connection.CloseNow()
		t.Fatalf("write E2EE client hello: %v", err)
	}
	messageType, rawServerHello, err := connection.Read(ctx)
	if err != nil {
		_ = connection.CloseNow()
		t.Fatalf("read E2EE server hello: %v", err)
	}
	if messageType != websocket.MessageText {
		_ = connection.CloseNow()
		t.Fatalf("E2EE server hello frame type = %v", messageType)
	}
	var serverHello struct {
		Type      string `json:"type"`
		Version   int    `json:"version"`
		Nonce     string `json:"nonce"`
		PublicKey string `json:"public_key"`
		Proof     string `json:"proof"`
	}
	if err := json.Unmarshal(rawServerHello, &serverHello); err != nil || serverHello.Type != "e2ee_server_hello" || serverHello.Version != 2 {
		_ = connection.CloseNow()
		t.Fatalf("invalid E2EE server hello: %s (%v)", rawServerHello, err)
	}
	serverNonce, err := base64.RawURLEncoding.DecodeString(serverHello.Nonce)
	if err != nil || len(serverNonce) != 32 {
		_ = connection.CloseNow()
		t.Fatalf("invalid E2EE server nonce: %v", err)
	}
	serverPublic, err := base64.RawURLEncoding.DecodeString(serverHello.PublicKey)
	if err != nil || len(serverPublic) != 65 {
		_ = connection.CloseNow()
		t.Fatalf("invalid E2EE server public key: %v", err)
	}
	serverProof, err := base64.RawURLEncoding.DecodeString(serverHello.Proof)
	if err != nil || !hmac.Equal(serverProof, managedFixtureAuthTag(secret, []byte("herdr-e2ee-v2 server\x00"), binding, clientNonce, clientPublic, serverNonce, serverPublic)) {
		_ = connection.CloseNow()
		t.Fatalf("E2EE server proof did not authenticate: %v", err)
	}
	serverPublicKey, err := ecdh.P256().NewPublicKey(serverPublic)
	if err != nil {
		_ = connection.CloseNow()
		t.Fatal(err)
	}
	sharedSecret, err := privateKey.ECDH(serverPublicKey)
	if err != nil {
		_ = connection.CloseNow()
		t.Fatal(err)
	}
	transcript := make([]byte, 0, len(binding)+len(clientNonce)+len(clientPublic)+len(serverNonce)+len(serverPublic))
	transcript = append(transcript, binding...)
	transcript = append(transcript, clientNonce...)
	transcript = append(transcript, clientPublic...)
	transcript = append(transcript, serverNonce...)
	transcript = append(transcript, serverPublic...)
	keySalt := managedFixtureAuthTag(secret, []byte("herdr-e2ee-v2 key\x00"), transcript)
	clientKey, err := hkdf.Key(sha256.New, sharedSecret, keySalt, "herdr-e2ee-v2 c2s", 32)
	if err != nil {
		_ = connection.CloseNow()
		t.Fatal(err)
	}
	serverKey, err := hkdf.Key(sha256.New, sharedSecret, keySalt, "herdr-e2ee-v2 s2c", 32)
	clear(sharedSecret)
	clear(keySalt)
	if err != nil {
		_ = connection.CloseNow()
		t.Fatal(err)
	}
	clientCipher, err := managedFixtureAEAD(clientKey)
	clear(clientKey)
	if err != nil {
		_ = connection.CloseNow()
		t.Fatal(err)
	}
	serverCipher, err := managedFixtureAEAD(serverKey)
	clear(serverKey)
	if err != nil {
		_ = connection.CloseNow()
		t.Fatal(err)
	}
	finish := []byte(`{"type":"e2ee_client_finish","version":2}`)
	var nonce [12]byte
	clientCiphertext := clientCipher.Seal(nil, nonce[:], finish, managedFixtureAAD("c2s", 0))
	clientFrame, err := json.Marshal(struct {
		Type       string `json:"type"`
		Version    int    `json:"version"`
		Sequence   uint64 `json:"sequence"`
		Ciphertext string `json:"ciphertext"`
	}{Type: "e2ee", Version: 2, Ciphertext: base64.RawURLEncoding.EncodeToString(clientCiphertext)})
	if err != nil {
		_ = connection.CloseNow()
		t.Fatal(err)
	}
	if err := connection.Write(ctx, websocket.MessageText, clientFrame); err != nil {
		_ = connection.CloseNow()
		t.Fatalf("write encrypted E2EE finish: %v", err)
	}
	messageType, rawServerFinish, err := connection.Read(ctx)
	if err != nil {
		_ = connection.CloseNow()
		t.Fatalf("read encrypted E2EE server finish: %v", err)
	}
	if messageType != websocket.MessageText {
		_ = connection.CloseNow()
		t.Fatalf("E2EE server finish frame type = %v", messageType)
	}
	var encryptedServerFrame struct {
		Type       string `json:"type"`
		Version    int    `json:"version"`
		Sequence   uint64 `json:"sequence"`
		Ciphertext string `json:"ciphertext"`
	}
	if err := json.Unmarshal(rawServerFinish, &encryptedServerFrame); err != nil || encryptedServerFrame.Type != "e2ee" || encryptedServerFrame.Version != 2 || encryptedServerFrame.Sequence != 0 {
		_ = connection.CloseNow()
		t.Fatalf("invalid encrypted E2EE server finish: %s (%v)", rawServerFinish, err)
	}
	serverCiphertext, err := base64.RawURLEncoding.DecodeString(encryptedServerFrame.Ciphertext)
	if err != nil {
		_ = connection.CloseNow()
		t.Fatal(err)
	}
	plaintext, err := serverCipher.Open(nil, nonce[:], serverCiphertext, managedFixtureAAD("s2c", 0))
	if err != nil {
		_ = connection.CloseNow()
		t.Fatalf("authenticate E2EE server finish: %v", err)
	}
	var result managedFixtureE2EEFinish
	if err := json.Unmarshal(plaintext, &result); err != nil || result.Type != "e2ee_server_finish" || result.Version != 2 ||
		result.DeviceID == "" || result.CredentialID == "" || result.Role != "controller" || result.Locale != locale || result.CredentialVersion == 0 || result.CredentialSecret == "" {
		_ = connection.CloseNow()
		t.Fatalf("invalid E2EE enrollment result: type=%q version=%d device=%q credential=%q role=%q locale=%q credential version=%d (%v)", result.Type, result.Version, result.DeviceID, result.CredentialID, result.Role, result.Locale, result.CredentialVersion, err)
	}
	return connection, result
}

func managedFixtureAuthTag(secret []byte, parts ...[]byte) []byte {
	mac := hmac.New(sha256.New, secret)
	for _, part := range parts {
		_, _ = mac.Write(part)
	}
	return mac.Sum(nil)
}

func managedFixtureAEAD(key []byte) (cipher.AEAD, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(block)
}

func managedFixtureAAD(direction string, sequence uint64) []byte {
	aad := append([]byte("herdr-e2ee-v2 "+direction+"\x00"), make([]byte, 8)...)
	binary.BigEndian.PutUint64(aad[len(aad)-8:], sequence)
	return aad
}

func waitManagedFixture(t *testing.T, description string, ready func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if ready() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", description)
}
