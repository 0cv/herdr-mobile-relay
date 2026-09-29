//go:build herdr_tailscale_test

package app

import (
	"context"
	"crypto/tls"
	"encoding/base64"
	"errors"
	"net"
	"net/http"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/0cv/herdr-mobile-relay/internal/config"
	"github.com/0cv/herdr-mobile-relay/internal/coordinator"
	"github.com/0cv/herdr-mobile-relay/internal/protocol"
	"github.com/0cv/herdr-mobile-relay/internal/tailscalecli"
	"github.com/0cv/herdr-mobile-relay/internal/web"
	"github.com/coder/websocket"
)

// This tagged hosted fixture wires the actual CLI-specific local/HTTPS,
// release-bundle, invitation, and route-drift paths. Its only route authority
// is an injected verifier, and its certificate/network are fixture-local.
func TestTailscaleCLIReadinessArmAndDrift(t *testing.T) {
	root := t.TempDir()
	webRoot := filepath.Join(root, "web")
	writeManagedWebFixture(t, webRoot, managedFixtureVersion, managedFixtureRevision)

	backend, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer backend.Close()
	public, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer public.Close()
	publicPort := strconv.Itoa(portFromAddr(t, public.Addr()))
	origin := "https://" + managedFixtureHost + ":" + publicPort
	cfg := &config.Config{
		Host:               "127.0.0.1",
		Port:               portFromAddr(t, backend.Addr()),
		PluginPort:         18378,
		Token:              strings.Repeat("k", 32),
		InstanceID:         "cli-fixture-instance",
		RuntimeDir:         root,
		CacheDir:           filepath.Join(root, "cache"),
		ConfigHome:         filepath.Join(root, "config"),
		ReleaseRoot:        filepath.Join(root, "release"),
		WebRoot:            webRoot,
		SocketPath:         filepath.Join(root, "herdr.sock"),
		Transport:          config.TransportTailscaleCLI,
		TailscaleCLIOrigin: origin,
		TailscaleCLIScope:  "development",
		PhoneAppOrigin:     origin,
		ControlRunID:       "cli-fixture-control",
		PairingSocketPath:  filepath.Join(root, "pairing-control.sock"),
	}
	server := newServerWithSession(cfg, managedFixtureVersion, managedFixtureRevision, managedTestLogger(), nil, nil)
	webHandler, err := web.NewHandler(webRoot)
	if err != nil {
		t.Fatal(err)
	}
	server.webH = webHandler
	server.state.CommitInventory(nil, server.state.RevisionCounter())
	udp, err := coordinator.NewUDPListener("127.0.0.1:0", server.state, cfg.SocketPath, managedTestLogger())
	if err != nil {
		t.Fatal(err)
	}
	server.udp = udp
	t.Cleanup(func() { _ = udp.Close() })
	server.mu.Lock()
	server.ready = true
	server.backendBound = true
	server.mu.Unlock()

	activeServer := server
	activeHandler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		activeServer.httpHandler().ServeHTTP(w, r)
	})
	backendServer := &http.Server{Handler: activeHandler}
	go func() { _ = backendServer.Serve(backend) }()
	t.Cleanup(func() { _ = backendServer.Close() })

	certificate, roots := managedFixtureCertificate(t, managedFixtureHost)
	publicServer := &http.Server{Handler: activeHandler}
	publicTLS := tls.NewListener(public, &tls.Config{
		MinVersion:   tls.VersionTLS12,
		Certificates: []tls.Certificate{certificate},
	})
	go func() { _ = publicServer.Serve(publicTLS) }()
	t.Cleanup(func() { _ = publicServer.Close() })
	installManagedHealthTestNetwork(t, server, managedFixtureHost+":"+publicPort, public.Addr().String(), roots)

	verifier := &fixedCLIRouteVerifier{status: tailscalecli.RouteStatus{
		JournalState:                    tailscalecli.StateRegistered,
		Readiness:                       tailscalecli.ReadinessReady,
		DevelopmentQualificationEnabled: true,
		RuntimeQualified:                false,
	}}
	server.tailscaleCLIRegistration = verifier
	// A fresh service may poll before first publication. That expected absence
	// quarantines admission reversibly; a later exact route must still arm.
	verifier.status.JournalState = tailscalecli.StateUnconfigured
	verifier.status.Readiness = tailscalecli.ReadinessWaiting
	verifier.err = tailscalecli.ErrUncertain
	prePublication := server.tailscaleCLIControlStatus(context.Background())
	if prePublication.PersistentRouteReady || !prePublication.Quarantined || server.bootstrapGate.OpenStatus() {
		t.Fatalf("fresh pre-publication state was not safely suspended: %+v", prePublication)
	}
	verifier.status.JournalState = tailscalecli.StateRegistered
	verifier.status.Readiness = tailscalecli.ReadinessReady
	verifier.err = nil
	qualificationStatus := server.tailscaleCLIControlStatus(context.Background())
	if !qualificationStatus.PersistentRouteDevelopmentQualificationEnabled ||
		qualificationStatus.PersistentRouteRuntimeQualified || !qualificationStatus.PersistentRouteReady {
		t.Fatalf("status conflated development enablement with runtime qualification: %+v", qualificationStatus)
	}
	var bundleChecks int
	verifyExactBundle := server.verifyPublicBundle
	server.verifyPublicBundle = func(ctx context.Context, gotRoot, gotOrigin, gotVersion, gotRevision string) error {
		bundleChecks++
		if gotRoot != webRoot || gotOrigin != cfg.PhoneAppOrigin ||
			gotVersion != managedFixtureVersion || gotRevision != managedFixtureRevision {
			t.Fatalf("phone bundle verification tuple = %q, %q, %q, %q", gotRoot, gotOrigin, gotVersion, gotRevision)
		}
		return verifyExactBundle(ctx, gotRoot, gotOrigin, gotVersion, gotRevision)
	}

	if err := server.checkTailscaleCLIReadiness(context.Background()); err != nil {
		t.Fatalf("CLI readiness rejected exact trusted fixture: %v", err)
	}
	preArmCtx, cancelPreArm := context.WithTimeout(context.Background(), managedHealthTimeout)
	preArmConn, preArmResponse, preArmErr := websocket.Dial(preArmCtx, "wss://"+managedFixtureHost+":"+publicPort+"/ws", &websocket.DialOptions{
		HTTPClient:   managedHealthClientForServer(server, 0),
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
		t.Fatalf("CLI pre-arm websocket status=%v err=%v; want HTTP 503", preArmStatus(preArmResponse), preArmErr)
	}

	armed, err := server.armTailscaleCLI(context.Background())
	if err != nil {
		t.Fatalf("CLI bootstrap arm failed after readiness: %v (%+v)", err, armed)
	}
	if !armed.Ready || !armed.ServeReady || !armed.PersistentRouteReady || !armed.InvitationArmed ||
		!server.bootstrapGate.OpenStatus() || server.hub == nil {
		t.Fatalf("CLI admission did not open after the complete readiness tuple: %+v", armed)
	}
	if bundleChecks < 3 || verifier.calls < 4 {
		t.Fatalf("readiness did not revalidate all boundaries: bundle checks=%d route checks=%d", bundleChecks, verifier.calls)
	}
	before := server.deviceStore().BootstrapStatus()
	if !before.Armed {
		t.Fatal("CLI admission did not durably arm a bootstrap invitation")
	}

	fixture := &managedTailscaleFixture{server: server, hostPort: managedFixtureHost + ":" + publicPort}
	wsClient, enrolled := managedFixtureEnrollOverWebSocket(t, fixture)
	defer wsClient.CloseNow()
	waitManagedFixture(t, "CLI authenticated websocket admission", func() bool { return server.hub.ClientCount() == 1 })
	identities := server.hub.ConnectedIdentities()
	if len(identities) != 1 || identities[0].DeviceID != enrolled.DeviceID || identities[0].CredentialID != enrolled.CredentialID {
		t.Fatalf("CLI E2EE enrollment identity = %+v; finish = %q/%q", identities, enrolled.DeviceID, enrolled.CredentialID)
	}
	credentialSecret, err := base64.RawURLEncoding.DecodeString(enrolled.CredentialSecret)
	if err != nil || len(credentialSecret) != 32 {
		t.Fatalf("CLI enrollment credential secret length=%d err=%v", len(credentialSecret), err)
	}
	clear(credentialSecret)
	credentials := server.deviceStore().ListCredentials("")
	if len(credentials) != 1 {
		t.Fatalf("CLI enrolled credential count=%d; want 1", len(credentials))
	}

	// Reconstruct the relay against the same private roots and exact route as a
	// process restart. Admission stays closed until a fresh trusted arm, while
	// the previously enrolled device credential remains durable.
	if err := wsClient.CloseNow(); err != nil {
		t.Fatalf("close pre-restart CLI websocket: %v", err)
	}
	waitManagedFixture(t, "pre-restart CLI websocket closure", func() bool { return server.hub.ClientCount() == 0 })
	if server.udp != nil {
		_ = server.udp.Close()
	}
	shutdownCtx, cancelShutdown := context.WithTimeout(context.Background(), 5*time.Second)
	if err := server.hub.Shutdown(shutdownCtx); err != nil {
		cancelShutdown()
		t.Fatalf("close pre-restart CLI Hub: %v", err)
	}
	cancelShutdown()

	restarted := newServerWithSession(cfg, managedFixtureVersion, managedFixtureRevision, managedTestLogger(), nil, nil)
	activeServer = restarted
	restartedWeb, err := web.NewHandler(webRoot)
	if err != nil {
		t.Fatal(err)
	}
	restarted.webH = restartedWeb
	t.Cleanup(func() { _ = restartedWeb.Close() })
	restarted.state.CommitInventory(nil, restarted.state.RevisionCounter())
	restartedUDP, err := coordinator.NewUDPListener("127.0.0.1:0", restarted.state, cfg.SocketPath, managedTestLogger())
	if err != nil {
		t.Fatalf("reopen CLI UDP listener after restart: %v", err)
	}
	restarted.udp = restartedUDP
	t.Cleanup(func() { _ = restartedUDP.Close() })
	restarted.mu.Lock()
	restarted.ready = true
	restarted.backendBound = true
	restarted.mu.Unlock()
	restarted.tailscaleCLIRegistration = verifier
	managedHealthTestNetwork.mu.Lock()
	managedHealthTestNetwork.server = restarted
	managedHealthTestNetwork.mu.Unlock()

	persisted := restarted.deviceStore().ListCredentials("")
	if len(persisted) != 1 || persisted[0].CredentialID != enrolled.CredentialID ||
		persisted[0].DeviceID != enrolled.DeviceID {
		t.Fatalf("CLI restart did not reopen the enrolled device credential: %+v", persisted)
	}
	if err := restarted.checkTailscaleCLIReadiness(context.Background()); err != nil {
		t.Fatalf("CLI restart readiness did not verify the same route and bundle: %v", err)
	}
	preArmRestartCtx, cancelPreArmRestart := context.WithTimeout(context.Background(), managedHealthTimeout)
	preArmRestartConn, preArmRestartResponse, preArmRestartErr := websocket.Dial(preArmRestartCtx,
		"wss://"+managedFixtureHost+":"+publicPort+"/ws", &websocket.DialOptions{
			HTTPClient:   managedHealthClientForServer(restarted, 0),
			Subprotocols: []string{protocol.EncryptedWebSocketSubprotocol},
		})
	cancelPreArmRestart()
	if preArmRestartConn != nil {
		_ = preArmRestartConn.CloseNow()
	}
	if preArmRestartResponse != nil && preArmRestartResponse.Body != nil {
		_ = preArmRestartResponse.Body.Close()
	}
	if preArmRestartErr == nil || preArmRestartResponse == nil || preArmRestartResponse.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("restarted CLI process admitted before explicit arm: status=%v err=%v",
			preArmStatus(preArmRestartResponse), preArmRestartErr)
	}

	restartedArm, err := restarted.armTailscaleCLI(context.Background())
	if err != nil || !restartedArm.Ready || !restarted.bootstrapGate.OpenStatus() {
		t.Fatalf("restarted CLI process did not reopen after exact route/bundle checks: %+v err=%v", restartedArm, err)
	}
	if credential, ok := restarted.deviceStore().AuthorizeCredential(enrolled.CredentialID, enrolled.CredentialVersion); !ok ||
		credential.DeviceID != enrolled.DeviceID {
		t.Fatalf("restarted CLI process invalidated prior phone credential: %+v %t", credential, ok)
	}

	restartedFixture := &managedTailscaleFixture{server: restarted, hostPort: managedFixtureHost + ":" + publicPort}
	restartedClient, restartedEnrollment := managedFixtureEnrollOverWebSocket(t, restartedFixture)
	defer restartedClient.CloseNow()
	waitManagedFixture(t, "restarted CLI E2EE admission", func() bool { return restarted.hub.ClientCount() == 1 })
	identities = restarted.hub.ConnectedIdentities()
	if len(identities) != 1 || identities[0].DeviceID != restartedEnrollment.DeviceID ||
		identities[0].CredentialID != restartedEnrollment.CredentialID {
		t.Fatalf("restarted CLI E2EE identity = %+v", identities)
	}
	credentials = restarted.deviceStore().ListCredentials("")
	if len(credentials) != 2 {
		t.Fatalf("restarted CLI credential count=%d; want old and new devices", len(credentials))
	}
	restartedInvitation := restarted.deviceStore().BootstrapStatus()
	if !restartedInvitation.Armed || !restartedInvitation.Pending {
		t.Fatalf("restarted CLI E2EE invitation state = %+v", restartedInvitation)
	}

	// A transient read-only CLI outage closes Hub admission but does not
	// irreversibly revoke the invitation gate or reset enrolled credentials.
	verifier.status.Readiness = tailscalecli.ReadinessWaiting
	verifier.err = errors.New("fixture CLI temporarily unavailable")
	outage := restarted.tailscaleCLIControlStatus(context.Background())
	if outage.Ready || outage.PersistentRouteReady || !outage.Quarantined ||
		outage.PersistentRouteReadiness != string(tailscalecli.ReadinessWaiting) || restarted.bootstrapGate.OpenStatus() {
		t.Fatalf("transient route outage did not suspend admission reversibly: %+v", outage)
	}
	waitManagedFixture(t, "CLI outage closes websocket admission", func() bool { return restarted.hub.ClientCount() == 0 })
	outageInvitation := restarted.deviceStore().BootstrapStatus()
	if outageInvitation.Armed != restartedInvitation.Armed || outageInvitation.Pending != restartedInvitation.Pending ||
		!outageInvitation.ExpiresAt.Equal(restartedInvitation.ExpiresAt) {
		t.Fatalf("CLI outage reset invitation state: before=%+v after=%+v", restartedInvitation, outageInvitation)
	}
	if credential, ok := restarted.deviceStore().AuthorizeCredential(enrolled.CredentialID, enrolled.CredentialVersion); !ok ||
		credential.DeviceID != enrolled.DeviceID {
		t.Fatalf("CLI outage invalidated prior phone credential: %+v %t", credential, ok)
	}
	verifier.err = nil
	verifier.status.Readiness = tailscalecli.ReadinessReady
	recovered, recoverErr := restarted.admitTailscaleCLI(context.Background())
	if recoverErr != nil || !recovered.Ready || !restarted.bootstrapGate.OpenStatus() {
		t.Fatalf("CLI admission did not recover after transient inspection outage: %+v err=%v", recovered, recoverErr)
	}
	recoveredInvitation := restarted.deviceStore().BootstrapStatus()
	if recoveredInvitation.Armed != outageInvitation.Armed || recoveredInvitation.Pending != outageInvitation.Pending ||
		!recoveredInvitation.ExpiresAt.Equal(outageInvitation.ExpiresAt) {
		t.Fatalf("CLI recovery replaced the bootstrap invitation: before=%+v after=%+v", outageInvitation, recoveredInvitation)
	}

	verifier.status.Readiness = tailscalecli.ReadinessDegraded
	verifier.err = tailscalecli.ErrConflict
	drifted := restarted.tailscaleCLIControlStatus(context.Background())
	if drifted.Ready || drifted.PersistentRouteReady || !drifted.Quarantined || restarted.bootstrapGate.OpenStatus() {
		t.Fatalf("route drift did not revoke restarted CLI admission: %+v", drifted)
	}
	after := restarted.deviceStore().BootstrapStatus()
	if after.Armed != restartedInvitation.Armed || after.Pending != restartedInvitation.Pending ||
		!after.ExpiresAt.Equal(restartedInvitation.ExpiresAt) {
		t.Fatalf("route drift reset or replaced device invitation state: before=%+v after=%+v", restartedInvitation, after)
	}
	waitManagedFixture(t, "restarted CLI websocket closure on route drift", func() bool { return restarted.hub.ClientCount() == 0 })
	credentials = restarted.deviceStore().ListCredentials("")
	if len(credentials) != 2 ||
		(credentials[0].CredentialID != enrolled.CredentialID && credentials[1].CredentialID != enrolled.CredentialID) ||
		(credentials[0].CredentialID != restartedEnrollment.CredentialID && credentials[1].CredentialID != restartedEnrollment.CredentialID) {
		t.Fatalf("route drift reset authenticated device credentials: %+v", credentials)
	}
	if !errors.Is(verifier.err, tailscalecli.ErrConflict) {
		t.Fatal("fixture route failure was not retained")
	}
}
