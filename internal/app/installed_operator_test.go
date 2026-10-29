//go:build herdr_installed_inert_test

package app

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/0cv/herdr-mobile-relay/internal/config"
	"github.com/0cv/herdr-mobile-relay/internal/deviceauth"
	"github.com/0cv/herdr-mobile-relay/internal/localcontrol"
	"github.com/0cv/herdr-mobile-relay/internal/tailscalecli"
	"github.com/0cv/herdr-mobile-relay/internal/transport"
)

func installedOperatorFixture(t *testing.T, registered ...bool) (*tailscalecli.Manager, config.InstalledConfig, *config.Config, func() int) {
	t.Helper()
	IsolateInstalledFixture(t)
	previous := http.DefaultTransport
	http.DefaultTransport = installedTerminalDenyHTTP{}
	t.Cleanup(func() { http.DefaultTransport = previous })
	var m *tailscalecli.Manager
	var state, coordination, binary string
	mutations := func() int { return 0 }
	if len(registered) != 0 && registered[0] {
		m, state, coordination, binary = tailscalecli.NewInstalledInertFixture(t)
	} else {
		m, state, coordination, binary, mutations = tailscalecli.NewInstalledSetupInertFixture(t)
	}
	base := filepath.Dir(state)
	cfg := &config.Config{
		Transport: config.TransportTailscaleCLI, Host: "127.0.0.1", Port: 18377, PluginPort: 18378,
		TailscaleCLIScope: "production", TailscaleCLIOrigin: "https://herdr.tailnet.ts.net:8443", InstanceID: "install-fixture",
		TailscaleCLIStateRoot: state, TailscaleCLICoordinationRoot: coordination, TailscaleCLIBin: binary,
		RuntimeDir: filepath.Join(base, "config", "relay"), CacheDir: filepath.Join(base, "cache"), ConfigHome: filepath.Join(base, "config"), ReleaseRoot: filepath.Join(base, "release"), WebRoot: filepath.Join(base, "web"), PollInterval: 1,
		HerdrBin: filepath.Join(base, "absent-herdr"), SocketPath: filepath.Join(base, "absent-socket"), Token: "01234567890123456789012345678901", PhoneAppOrigin: "https://herdr.tailnet.ts.net:8443",
	}
	for _, path := range []string{cfg.ConfigHome, cfg.RuntimeDir, cfg.CacheDir, cfg.ReleaseRoot} {
		if err := os.MkdirAll(path, 0700); err != nil {
			t.Fatal(err)
		}
	}
	selected, err := config.BindInstalledCLI(cfg)
	if err != nil {
		t.Fatal(err)
	}
	return m, selected, cfg, mutations
}

func installedOperatorRequest(ctx context.Context, control localcontrol.InstalledControl, credentials localcontrol.InstalledCredentials, op string, consent *localcontrol.InstalledConsent) localcontrol.InstalledReply {
	request := localcontrol.InstalledRequest{Protocol: localcontrol.InstalledProtocol, Run: credentials.Run, Installation: credentials.Installation, Capability: credentials.Capability, Op: op, Consent: consent}
	if op == "arm" {
		request.OperationID = "explicit-fixture-arm"
	}
	data, err := json.Marshal(request)
	if err != nil {
		panic(err)
	}
	var reply localcontrol.InstalledReply
	if err := json.Unmarshal(control.Dispatch(ctx, data), &reply); err != nil {
		panic(err)
	}
	return reply
}

func installedOperatorConsent(p localcontrol.InstalledProposal) *localcontrol.InstalledConsent {
	return &localcontrol.InstalledConsent{Proposal: p, Production: true, Persistent: true, CheckToWriteRace: true, PortReuseRisk: true, NoRollback: true, NoRemoteDrain: true}
}

func TestInstalledOperatorSetupResumeComposition(t *testing.T) {
	m, selected, cfg, mutations := installedOperatorFixture(t)
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	// Synthetic controller and reader credential/role preservation through the
	// actual sole-store app, setup publication, endpoint retirement and resume.
	seed, err := deviceauth.Open(filepath.Join(cfg.RuntimeDir, "device-auth"))
	if err != nil {
		t.Fatal(err)
	}
	var selectors []transport.E2EEAuthSelector
	var secrets [][]byte
	for _, role := range []deviceauth.Role{deviceauth.RoleController, deviceauth.RoleReader} {
		inv, err := seed.CreateInvitation(string(role), role, "en")
		if err != nil {
			t.Fatal(err)
		}
		result, err := seed.CompleteE2EEAuth(context.Background(), transport.E2EEAuthSelector{Kind: transport.E2EEAuthInvitation, ID: inv.InvitationID, Version: inv.Version}, true)
		if err != nil {
			t.Fatal(err)
		}
		selector := transport.E2EEAuthSelector{Kind: transport.E2EEAuthCredential, ID: result.Identity.CredentialID, Version: result.Identity.CredentialVersion, Locale: result.Identity.Locale}
		secret, err := seed.ResolveE2EESecret(context.Background(), selector)
		if err != nil {
			t.Fatal(err)
		}
		selectors = append(selectors, selector)
		secrets = append(secrets, secret)
	}
	if _, err := seed.CreateInvitation("pending", deviceauth.RoleReader, "en"); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(cfg.RuntimeDir, "device-auth", "devices.json")
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var current *Server
	var startupGeneration string
	var retained localcontrol.InstalledControl
	var previous localcontrol.InstalledCredentials
	var ready atomic.Bool
	var observations atomic.Int32
	callbackContext := make(chan context.Context, 1)
	var resume bool
	serviceHeld := make(chan struct{})
	serviceRelease := make(chan struct{})
	SetInstalledInertRunner(t, func(s *Server, ctx context.Context) error {
		current = s
		startupGeneration = s.controlRunID()
		if startupGeneration == "" {
			return errors.New("generation missing before runner exposure")
		}
		s.developmentCLIHealthClient = func(time.Duration) *http.Client { return &http.Client{Transport: installedTerminalDenyHTTP{}} }
		resource := &installedLogicalListener{}
		s.installedBackend = newInstalledBackend(ctx, resource)
		defer s.installedBackend.Close()
		store := s.deviceStore()
		if store == nil || s.bootstrapGate.OpenStatus() {
			return errors.New("missing store or automatic arm")
		}
		if !resume {
			tailscalecli.SetInstalledSetupReadinessFixture(s.installedSetup.session, func(call context.Context) error {
				observations.Add(1)
				if call.Err() != nil {
					return call.Err()
				}
				// No UDP, HTTP or real admission is fabricated. This is the reviewed
				// lower-level publication fixture and also checks status lock reentry.
				if s.tailscaleCLIControlStatus(call).Ready {
					return errors.New("synthetic publication became HTTPS admission")
				}
				if !ready.Load() {
					return tailscalecli.ErrTransientUnavailable
				}
				return nil
			})
		}
		close(s.listenerBound)
		var retired <-chan struct{}
		if s.installedSetup != nil {
			retired = s.installedSetup.lifetime.Done()
		}
		var endpoint context.Context
		select {
		case endpoint = <-callbackContext:
		case <-retired:
			select {
			case endpoint = <-callbackContext:
			default:
				return errors.New("setup composition retired before callback")
			}
		case <-ctx.Done():
			return ctx.Err()
		}
		<-endpoint.Done()
		if ctx.Err() != nil {
			return errors.New("endpoint retirement stopped service")
		}
		if retainedReply := installedOperatorRequest(ctx, retained, previous, "status", nil); retainedReply.Code != "retired" {
			return errors.New("retained endpoint still active")
		}
		if s.controlRunID() != previous.Run || s.cfg.ControlRunID != "" || s.cfg.PairingSocketPath != "" {
			return errors.New("runtime generation/config drift")
		}
		for i, selector := range selectors {
			secret, err := store.ResolveE2EESecret(ctx, selector)
			if err != nil || !bytes.Equal(secret, secrets[i]) {
				return errors.New("credential changed")
			}
		}
		after, err := os.ReadFile(path)
		if err != nil || !bytes.Equal(before, after) || s.deviceStore() != store || s.bootstrapGate.OpenStatus() {
			return errors.New("store changed or implicitly armed")
		}
		if !resume {
			// The setup pane has returned and its control is retired, but the
			// service owner and sole store writer remain live until service stop.
			close(serviceHeld)
			select {
			case <-serviceRelease:
			case <-ctx.Done():
				return ctx.Err()
			}
		}
		return nil
	})
	callback := func(ctx context.Context, control localcontrol.InstalledControl, c localcontrol.InstalledCredentials) error {
		callbackContext <- ctx
		if resume && (c.Run == previous.Run || c.Capability == previous.Capability) {
			return errors.New("generation reused")
		}
		if resume && installedOperatorRequest(ctx, control, previous, "status", nil).Code != "unauthorized" {
			return errors.New("old generation authorized")
		}
		retained = control
		previous = c
		if current.controlRunID() != c.Run || startupGeneration != c.Run || c.Run == c.Capability {
			return errors.New("generation/capability confusion")
		}
		if err := selected.Validate(current.cfg); err != nil {
			return err
		}
		// In-memory handler only: no listener, HTTP client or qualification.
		recorder := httptest.NewRecorder()
		current.handleHealthz(recorder, httptest.NewRequest(http.MethodGet, "/healthz", nil))
		if !strings.Contains(recorder.Body.String(), c.Run) || strings.Contains(recorder.Body.String(), c.Capability) || strings.Contains(fmt.Sprintf("%v %#v", c, c), c.Capability) {
			return errors.New("public identity/redaction")
		}
		health := installedLocalHealth{Status: "ok", Readiness: "ready", Transport: config.TransportTailscaleCLI, Instance: cfg.InstanceID, Origin: cfg.TailscaleCLIOrigin, Run: c.Run, Version: "fixture", Revision: "fixture"}
		if !current.matchesInstalledLocalHealth(health) {
			return errors.New("exact identity refused")
		}
		health.Run = "other-generation"
		if current.matchesInstalledLocalHealth(health) {
			return errors.New("foreign generation accepted")
		}
		if resume {
			if installedOperatorRequest(ctx, control, c, "proposal", nil).Code != "publication_disabled" {
				return errors.New("resume proposed setup")
			}
			if installedOperatorRequest(ctx, control, c, "publish", &localcontrol.InstalledConsent{}).Code != "publication_disabled" {
				return errors.New("resume enabled publish")
			}
			metadata, err := current.installedCLI.Metadata(selected.Binding())
			if err != nil || metadata.NodeID != "node-fixture" || metadata.Profile == "" {
				return errors.New("resume metadata")
			}
		} else {
			p := installedOperatorRequest(ctx, control, c, "proposal", nil).Proposal
			if p == nil {
				return errors.New("missing proposal")
			}
			actual, err := current.installedSetup.session.Proposal(ctx, selected.Binding())
			if err != nil {
				return err
			}
			if p.Node != actual.NodeID || p.Profile != string(actual.Profile) || p.Attempt != actual.ReservationID || p.Selection == "" || strings.Contains(p.Selection, cfg.ConfigHome) {
				return errors.New("unowned metadata")
			}
			consent := installedOperatorConsent(*p)
			bad := c
			bad.Capability = strings.Repeat("0", 64)
			if installedOperatorRequest(ctx, control, bad, "publish", consent).Code != "unauthorized" || observations.Load() != 0 {
				return errors.New("authentication after backend")
			}
			consent.NoRollback = false
			if installedOperatorRequest(ctx, control, c, "publish", consent).Code != "consent_refused" || observations.Load() != 0 {
				return errors.New("consent after backend")
			}
			consent.NoRollback = true
			if installedOperatorRequest(ctx, control, c, "publish", consent).Code != "readiness_unavailable" || mutations() != 0 {
				return errors.New("unavailable dispatched")
			}
			if current.installedSetup.attempted {
				return errors.New("preflight consumed app")
			}
			again := installedOperatorRequest(ctx, control, c, "proposal", nil).Proposal
			if again == nil || *again != *p {
				return errors.New("proposal replaced")
			}
			cancelled, cancel := context.WithCancel(ctx)
			cancel()
			if installedOperatorRequest(cancelled, control, c, "publish", consent).Code != "cancelled" {
				return errors.New("cancelled request dispatched")
			}
			ready.Store(true)
			reply := installedOperatorRequest(ctx, control, c, "publish", consent)
			if reply.Code != "ok" || reply.Publication != "acknowledged" || mutations() != 1 {
				return errors.New("explicit publication failed")
			}
			if installedOperatorRequest(ctx, control, c, "publish", consent).Code != "consumed" || mutations() != 1 {
				return errors.New("publication replay")
			}
		}
		for _, op := range []string{"arm", "reprint"} {
			reply := installedOperatorRequest(ctx, control, c, op, nil)
			if reply.Code != "admission_refused" || reply.Invitation != "" || current.installedAdmissionRevoked() {
				return errors.New("transient unqualified admission/output/latch")
			}
		}
		return nil
	}
	var setupErr error
	setupDone := make(chan struct{})
	go func() {
		defer close(setupDone)
		setupErr = RunInstalledSetupOperatorCLI(context.Background(), selected, "fixture", "fixture", logger, m, callback)
	}()
	var releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { close(serviceRelease) }) }
	defer func() { release(); <-setupDone }()
	select {
	case <-serviceHeld:
	case <-setupDone:
		t.Fatalf("service exited with setup pane: %v", setupErr)
	}
	if current == nil || current.installedSetup == nil || current.installedSetup.lifetime.Err() == nil || installedOperatorRequest(context.Background(), retained, previous, "status", nil).Code != "retired" || current.controlRunID() != previous.Run || mutations() != 1 {
		t.Fatal("pane closure retired service, control or selected generation")
	}
	if err := selected.Validate(current.cfg); err != nil {
		t.Fatal(err)
	}
	registration := filepath.Join(cfg.TailscaleCLIStateRoot, "registration.json")
	published, err := os.ReadFile(registration)
	if err != nil || len(published) == 0 {
		t.Fatalf("setup did not retain its registration: %v", err)
	}
	release()
	<-setupDone
	if setupErr != nil {
		t.Fatal(setupErr)
	}
	resume = true
	if err := RunInstalledResumeOperatorCLI(context.Background(), selected, "fixture", "fixture", logger, m, callback); err != nil {
		t.Fatal(err)
	}
	afterRegistration, err := os.ReadFile(registration)
	if err != nil || !bytes.Equal(published, afterRegistration) || mutations() != 1 {
		t.Fatal("resume changed selected origin/route registration or republished")
	}
	if _, err := current.installedCLI.Metadata(selected.Binding()); err == nil {
		t.Fatal("retired metadata accepted")
	}
}

func TestInstalledOperatorStrictResumeWithoutRegistration(t *testing.T) {
	m, selected, _, mutations := installedOperatorFixture(t)
	constructed := false
	SetInstalledInertRunner(t, func(*Server, context.Context) error { constructed = true; return nil })
	err := RunInstalledResumeOperatorCLI(context.Background(), selected, "fixture", "fixture", slog.New(slog.NewTextHandler(io.Discard, nil)), m, func(context.Context, localcontrol.InstalledControl, localcontrol.InstalledCredentials) error {
		constructed = true
		return nil
	})
	if err == nil || constructed || mutations() != 0 {
		t.Fatal("resume fell through to setup")
	}
}
