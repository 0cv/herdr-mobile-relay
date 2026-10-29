//go:build herdr_installed_inert_test

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"testing"
	"time"

	"github.com/0cv/herdr-mobile-relay/internal/app"
	"github.com/0cv/herdr-mobile-relay/internal/config"
	"github.com/0cv/herdr-mobile-relay/internal/deviceauth"
	"github.com/0cv/herdr-mobile-relay/internal/installedpromotion"
	"github.com/0cv/herdr-mobile-relay/internal/localcontrol"
	"github.com/0cv/herdr-mobile-relay/internal/release"
	"github.com/0cv/herdr-mobile-relay/internal/tailscalecli"
	"github.com/0cv/herdr-mobile-relay/internal/transport"
)

// No OS service, listener, Server.Run, socket, network, SQL or phone exists in
// this selection. Only CLI observations/readiness and native commands are
// substituted; the coordinator, capsule, core, consent, publication, journal,
// setup owner/join, definition and resume dispatch are production decisions.
func TestInstalledProductionPromotionInert(t *testing.T) {
	t.Run("owned update and scoped removal", func(t *testing.T) { testInstalledProductionPromotionInert(t, false, false) })
	t.Run("uncertain stop retains authority", func(t *testing.T) { testInstalledProductionPromotionInert(t, true, false) })
	t.Run("consented missing route removal", func(t *testing.T) { testInstalledProductionPromotionInert(t, false, true) })
}

func testInstalledProductionPromotionInert(t *testing.T, uncertain, missing bool) {
	app.IsolateInstalledFixture(t)
	app.DenyInstalledCommandHTTP(t)
	root := installedCLICommandEnv(t)
	if err := os.MkdirAll(filepath.Join(root, "config"), 0700); err != nil {
		t.Fatal(err)
	}
	m, state, coordination, binary, mutations := tailscalecli.NewInstalledSetupInertFixture(t)
	for key, value := range map[string]string{
		"HERDR_TAILSCALE_CLI_STATE_ROOT": state, "HERDR_TAILSCALE_CLI_COORDINATION_ROOT": coordination,
		"HERDR_TAILSCALE_CLI_BIN": binary, "HERDR_RELAY_INSTANCE_ID": "install-fixture",
		"HERDR_TAILSCALE_CLI_ORIGIN":     "https://herdr.tailnet.ts.net:8443",
		"HERDR_PHONE_APP_URL":            "https://herdr.tailnet.ts.net:8443",
		"HERDR_TAILSCALE_CLI_HTTPS_PORT": "8443", "HERDR_RELAY_PORT": "18377", "HERDR_RELAY_PLUGIN_PORT": "18378",
	} {
		t.Setenv(key, value)
	}
	cfg, err := config.LoadInstalled()
	if err != nil {
		t.Fatal(err)
	}
	selected, err := config.BindInstalledCLI(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if config.TailscaleCLIProfilesEnabled() {
		t.Fatal("production gate opened")
	}
	physical := filepath.Join(cfg.ReleaseRoot, "v1", "herdr-mobile-relay")
	if err := os.MkdirAll(filepath.Dir(physical), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(physical, []byte("inert executable; never launched"), 0700); err != nil {
		t.Fatal(err)
	}
	definition, err := promotionDefinitionPath()
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Dir(definition) // absent on a fresh host
	var legacy string
	switch runtime.GOOS {
	case "darwin":
		legacy = filepath.Join(dir, "com.herdr-remote.service.plist")
	case "linux":
		legacy = filepath.Join(dir, "herdr-remote.service")
	default:
		t.Fatal(errPromotion)
	}
	// Seed the same real store/roles used by the app lifecycle fixture before
	// setup, then compare its credential bytes and E2EE authentication on resume.
	storeRoot := filepath.Join(cfg.RuntimeDir, "device-auth")
	store, err := deviceauth.Open(storeRoot)
	if err != nil {
		t.Fatal(err)
	}
	var selectors []transport.E2EEAuthSelector
	var secrets [][]byte
	for _, role := range []deviceauth.Role{deviceauth.RoleController, deviceauth.RoleReader} {
		invitation, err := store.CreateInvitation(string(role), role, "en")
		if err != nil {
			t.Fatal(err)
		}
		enrolled, err := store.CompleteE2EEAuth(context.Background(), transport.E2EEAuthSelector{Kind: transport.E2EEAuthInvitation, ID: invitation.InvitationID, Version: invitation.Version}, true)
		if err != nil {
			t.Fatal(err)
		}
		selector := transport.E2EEAuthSelector{Kind: transport.E2EEAuthCredential, ID: enrolled.Identity.CredentialID, Version: enrolled.Identity.CredentialVersion, Locale: enrolled.Identity.Locale}
		if _, err := store.CompleteE2EEAuth(context.Background(), selector, true); err != nil {
			t.Fatal(err)
		}
		secret, err := store.ResolveE2EESecret(context.Background(), selector)
		if err != nil {
			t.Fatal(err)
		}
		selectors = append(selectors, selector)
		secrets = append(secrets, secret)
	}
	credentials := store.ListCredentials("")
	devicePath := filepath.Join(storeRoot, "devices.json")
	beforeDevices, err := os.ReadFile(devicePath)
	if err != nil {
		t.Fatal(err)
	}
	assertDevices := func(s *deviceauth.Store, call context.Context) error {
		if !reflect.DeepEqual(credentials, s.ListCredentials("")) {
			return errors.New("persisted credentials or roles changed")
		}
		for i, selector := range selectors {
			secret, err := s.ResolveE2EESecret(call, selector)
			if err != nil || !bytes.Equal(secret, secrets[i]) {
				return errors.New("persisted E2EE authentication changed")
			}
		}
		return nil
	}
	setupBackendExited := make(chan struct{})
	var actions []string
	claim := filepath.Join(root, ".local/state/herdr-mobile-relay/recovery/.herdr-native-claim-v1")
	native := &nativeCLIFresh{executable: physical, definition: definition, legacy: legacy, claim: claim, driver: func(ctx context.Context, action string) error {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		actions = append(actions, action)
		if action == "activate" {
			select {
			case <-setupBackendExited:
			default:
				return errors.New("activation preceded setup backend exit")
			}
			if mutations() != 1 {
				return errors.New("activation before owned publication")
			}
			if _, err := os.Stat(definition); err != nil {
				return err
			}
		}
		return nil
	}}
	installedFixtureManager = func(config.InstalledConfig) (*tailscalecli.Manager, error) { return m, nil }
	installedFixturePinnedExecutable = physical
	installedPromotionFixtureNative = func(config.InstalledConfig) (promotionNative, error) { return native, nil }
	ctx, cancel := context.WithCancel(context.Background())
	installedFixtureContext = ctx
	t.Cleanup(func() {
		cancel()
		installedFixtureManager = nil
		installedFixturePinnedExecutable = ""
		installedPromotionFixtureNative = nil
		installedFixtureContext = nil
		installedFixtureEndpoint = nil
	})
	app.SetInstalledInertRunner(t, func(s *app.Server, call context.Context) error {
		defer close(setupBackendExited)
		app.SetInstalledCommandReadiness(t, s)
		return app.RunInstalledCommandLogicalBackend(s, call, func(store *deviceauth.Store, _ string) error {
			return assertDevices(store, call)
		})
	})
	installedFixtureEndpoint = func(call context.Context, original config.InstalledConfig, core localcontrol.InstalledControl, c localcontrol.InstalledCredentials) error {
		if original.Validate(cfg) != nil {
			return errors.New("selection changed")
		}
		proposal := commandOperatorRequest(call, core, c, "proposal")
		if proposal.Code != "ok" || proposal.Proposal == nil {
			return errors.New("real proposal unavailable")
		}
		if _, err := os.Lstat(dir); !errors.Is(err, os.ErrNotExist) {
			return errors.New("native parent created before consent")
		}
		consent := localcontrol.InstalledConsent{Proposal: *proposal.Proposal, Production: true, Persistent: true, CheckToWriteRace: true, PortReuseRisk: true, NoRollback: true, NoRemoteDrain: true}
		wire, err := json.Marshal(localcontrol.InstalledRequest{Protocol: localcontrol.InstalledProtocol, Run: c.Run, Installation: c.Installation, Capability: c.Capability, Op: "publish", Consent: &consent})
		if err != nil {
			return err
		}
		var reply localcontrol.InstalledReply
		if json.Unmarshal(core.Dispatch(call, wire), &reply) != nil || reply.Code != "ok" || reply.Publication != "acknowledged" {
			return errors.New("owned publication refused")
		}
		if info, err := os.Lstat(dir); err != nil || !info.IsDir() || info.Mode().Perm() != 0700 {
			return errors.New("native parent absent after consented publication")
		}
		<-call.Done()
		return call.Err()
	}
	code, err := runCommand([]string{"promote-installed"}, func(*app.Server, context.Context) error { t.Fatal("legacy runner"); return nil }, func(context.Context) error { t.Fatal("sweep"); return nil })
	if code != 0 || err != nil || mutations() != 1 || len(actions) < 4 || actions[len(actions)-1] != "activate" {
		t.Fatalf("promotion refused: code=%d err=%v mutations=%d actions=%v", code, err, mutations(), actions)
	}
	data, err := os.ReadFile(filepath.Join(claim, "phase-a"))
	if err != nil {
		t.Fatal(err)
	}
	var record promotionRecord
	if json.Unmarshal(data, &record) != nil || record.Generation == "" || record.Digest == "" {
		t.Fatal("no durable selection")
	}
	ref := installedpromotion.Reference{Generation: record.Generation, Digest: record.Digest}
	freshAttempt := &nativeCLIFresh{executable: physical, definition: definition, legacy: legacy, claim: claim, driver: func(context.Context, string) error {
		t.Fatal("fresh observer contacted native driver after durable claim")
		return nil
	}}
	if freshAttempt.Fresh(context.Background()) == nil {
		t.Fatal("fresh attempt adopted existing native claim")
	}
	intent := filepath.Join(claim, "phase-i")
	if err := os.Rename(intent, intent+".held"); err != nil {
		t.Fatal(err)
	}
	if promotionResumeAdmitted(selected, ref) == nil {
		t.Fatal("resume admitted without durable activation intent")
	}
	if err := os.Rename(intent+".held", intent); err != nil {
		t.Fatal(err)
	}
	installedFixturePinnedExecutable = filepath.Join(root, "unrelated-binary")
	if promotionResumeAdmitted(selected, ref) == nil {
		t.Fatal("resume admitted a different executable")
	}
	installedFixturePinnedExecutable = physical
	resume, err := installedpromotion.Read(context.Background(), cfg.ConfigHome, cfg.InstanceID, ref)
	if err != nil || resume.Validate(cfg) != nil {
		t.Fatal("pinned selection changed")
	}
	before := mutations()
	serviceReady := make(chan struct{})
	endpointReturned := make(chan struct{})
	installedFixtureEndpoint = func(call context.Context, original config.InstalledConfig, core localcontrol.InstalledControl, c localcontrol.InstalledCredentials) error {
		defer close(endpointReturned)
		if original.Validate(cfg) != nil || commandOperatorRequest(call, core, c, "proposal").Code != "publication_disabled" {
			return errors.New("resume became setup")
		}
		return nil
	}
	serviceCtx, stopService := context.WithCancel(context.Background())
	defer stopService()
	installedFixtureContext = serviceCtx
	app.SetInstalledInertRunner(t, func(s *app.Server, call context.Context) error {
		return app.RunInstalledCommandLogicalBackend(s, call, func(store *deviceauth.Store, _ string) error {
			if err := assertDevices(store, call); err != nil {
				return err
			}
			close(serviceReady) // the service owns the logical backend lease
			return nil
		})
	})
	done := make(chan struct{})
	var serviceCode int
	var serviceErr error
	go func() {
		defer close(done)
		serviceCode, serviceErr = runCommand([]string{"serve-installed", "--selection", ref.Generation, ref.Digest, cfg.ConfigHome, cfg.InstanceID}, nil, nil)
	}()
	// Even a fatal assertion must join the service before the fixture/global
	// cleanup can restore its injected runner, endpoint and native driver.
	t.Cleanup(func() { stopService(); <-done })
	// Positive, ordered witnesses: the backend acquired its lease, and the
	// independent endpoint finished while that same backend was still running.
	for _, witnessed := range []<-chan struct{}{serviceReady, endpointReturned} {
		select {
		case <-witnessed:
		case <-done:
			t.Fatalf("pinned service ended before endpoint/readiness: code=%d err=%v", serviceCode, serviceErr)
		case <-time.After(5 * time.Second):
			stopService()
			<-done
			t.Fatal("service readiness/endpoint witness timed out")
		}
	}
	select {
	case <-done:
		t.Fatalf("endpoint retirement stopped the backend: code=%d err=%v", serviceCode, serviceErr)
	default:
	}
	stopService()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("service join timed out")
	}
	if serviceCode != 0 || serviceErr != nil || mutations() != before {
		t.Fatalf("resume republished: %d %v %d", serviceCode, serviceErr, mutations())
	}
	afterDevices, err := os.ReadFile(devicePath)
	if err != nil || !bytes.Equal(afterDevices, beforeDevices) {
		t.Fatal("promotion/resume changed persisted device credentials")
	}
	// Construct real, complete verified release manifests around the two inert
	// executable bytes. The only substituted effects remain manager/native/HTTP.
	makeRelease := func(path, version string) {
		t.Helper()
		for _, name := range []string{"web/index.html", "LICENSE", "README.md", "relay/common.sh", "relay/herdr-mobile-relay-service.sh", "relay/plugin-on-event.sh", "relay/plugin-tailscale-always-on.sh", "relay/setup-link.sh", "relay/stable-setup.sh", "relay/stable-teardown.sh", "relay/start.sh", "relay/transport-switch-locked.sh"} {
			destination := filepath.Join(filepath.Dir(path), name)
			if err := os.MkdirAll(filepath.Dir(destination), 0700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(destination, []byte("verified inert fixture"), 0600); err != nil {
				t.Fatal(err)
			}
		}
		if _, err := release.Build(filepath.Dir(path), version, version, release.CurrentTarget()); err != nil {
			t.Fatal(err)
		}
		if _, err := release.Verify(filepath.Dir(path), release.CurrentTarget()); err != nil {
			t.Fatal(err)
		}
	}
	makeRelease(physical, "v1")
	candidate := filepath.Join(cfg.ReleaseRoot, "v2", "herdr-mobile-relay")
	if err := os.MkdirAll(filepath.Dir(candidate), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(candidate, []byte("second inert executable; never launched"), 0700); err != nil {
		t.Fatal(err)
	}
	makeRelease(candidate, "v2")
	// The verifier must reject an altered candidate before any native stop.
	if err := os.WriteFile(candidate, []byte("tampered fixture binary"), 0700); err != nil {
		t.Fatal(err)
	}
	priorActions := len(actions)
	installedFixtureContext = context.Background()
	code, err = runCommand([]string{"update-installed", candidate}, nil, nil)
	if code != 1 || err == nil || len(actions) != priorActions || mutations() != before {
		t.Fatal("tampered release reached native effects")
	}
	if err := os.WriteFile(candidate, []byte("second inert executable; never launched"), 0700); err != nil {
		t.Fatal(err)
	}
	// A failed start is not a successful update. A second confirmed quiesce
	// permits only this attempt's exact definition to be rolled back.
	failStart := true
	native.driver = func(call context.Context, action string) error {
		if call.Err() != nil {
			return call.Err()
		}
		actions = append(actions, action)
		if action == "start" && failStart {
			failStart = false
			return errors.New("inert start failure")
		}
		return nil
	}
	installedFixtureContext = context.Background()
	if e := promotionRouteReady(context.Background(), selected, m); e != nil {
		status, detail := m.VerifyRegisteredRoute(context.Background(), cfg.TailscaleCLIScope, cfg.InstanceID, cfg.TailscaleCLIOrigin, 8443, cfg.Port)
		t.Fatalf("update route not ready: %v status=%+v detail=%v", e, status, detail)
	}
	if _, e := promotionOwnedBinding(selected, ref, native); e != nil {
		t.Fatalf("update ownership not admitted: %v", e)
	}
	code, err = runCommand([]string{"update-installed", candidate}, nil, nil)
	if code != 1 || err == nil || promotionResumeAdmitted(selected, ref) != nil || mutations() != before || actions[len(actions)-3] != "start" || actions[len(actions)-2] != "quiesce" || actions[len(actions)-1] != "start" {
		t.Fatalf("owned rollback failed: code=%d err=%v actions=%v", code, err, actions)
	}
	oldDefinition, err := promotionDefinition(physical, selected.Snapshot(), ref)
	if err != nil || promotionDefinitionAt(definition, oldDefinition) != nil {
		t.Fatal("rollback changed original definition")
	}
	code, err = runCommand([]string{"update-installed", candidate}, nil, nil)
	if code != 0 || err != nil || mutations() != before {
		t.Fatalf("owned update failed: %d %v", code, err)
	}
	newDefinition, err := promotionDefinition(candidate, selected.Snapshot(), ref)
	if err != nil || promotionDefinitionAt(definition, newDefinition) != nil {
		t.Fatal("candidate argv not pinned")
	}
	if promotionResumeAdmitted(selected, ref) == nil {
		t.Fatal("old binary still admitted after pin change")
	}
	installedFixturePinnedExecutable = candidate
	if promotionResumeAdmitted(selected, ref) != nil {
		t.Fatal("candidate strict resume refused")
	}
	// Operator-selected rollback is the same verified pinned transition, not
	// an implicit current-pointer reset or a second setup publication.
	code, err = runCommand([]string{"update-installed", physical}, nil, nil)
	if code != 0 || err != nil || promotionDefinitionAt(definition, oldDefinition) != nil || mutations() != before {
		t.Fatal("explicit release rollback refused")
	}
	if promotionResumeAdmitted(selected, ref) == nil {
		t.Fatal("new binary admitted after explicit rollback")
	}
	installedFixturePinnedExecutable = physical
	if promotionResumeAdmitted(selected, ref) != nil {
		t.Fatal("old pin not restored by explicit rollback")
	}
	code, err = runCommand([]string{"update-installed", candidate}, nil, nil)
	if code != 0 || err != nil || promotionDefinitionAt(definition, newDefinition) != nil || mutations() != before {
		t.Fatal("re-update after rollback refused")
	}
	installedFixturePinnedExecutable = candidate
	if promotionResumeAdmitted(selected, ref) != nil {
		t.Fatal("strict re-update not admitted")
	}
	if saved, e := os.ReadFile(devicePath); e != nil || !bytes.Equal(saved, beforeDevices) {
		t.Fatal("update changed device/role store")
	}
	updatedReady := make(chan struct{})
	updatedEndpointReady := make(chan struct{})
	app.SetInstalledInertRunner(t, func(s *app.Server, call context.Context) error {
		return app.RunInstalledCommandLogicalBackend(s, call, func(store *deviceauth.Store, _ string) error {
			if e := assertDevices(store, call); e != nil {
				return e
			}
			close(updatedReady)
			return nil
		})
	})
	installedFixtureEndpoint = func(call context.Context, original config.InstalledConfig, core localcontrol.InstalledControl, c localcontrol.InstalledCredentials) error {
		if original.Validate(cfg) != nil {
			return errors.New("updated service selection changed")
		}
		proposal := commandOperatorRequest(call, core, c, "proposal")
		if proposal.Code != "publication_disabled" {
			return errors.New("updated service proposal was not publication-disabled")
		}
		close(updatedEndpointReady)
		<-call.Done()
		return call.Err()
	}
	updatedCtx, stopUpdated := context.WithCancel(context.Background())
	installedFixtureContext = updatedCtx
	updatedDone := make(chan struct{})
	var updatedCode int
	var updatedErr error
	go func() {
		defer close(updatedDone)
		updatedCode, updatedErr = runCommand([]string{"serve-installed", "--selection", ref.Generation, ref.Digest, cfg.ConfigHome, cfg.InstanceID}, nil, nil)
	}()
	t.Cleanup(func() { stopUpdated(); <-updatedDone })
	// Backend observation precedes listener readiness and operator dispatch.
	// Witness the real resume proposal too, before cancellation can retire it.
	updatedStartup := time.NewTimer(5 * time.Second)
	defer updatedStartup.Stop()
	for _, witnessed := range []<-chan struct{}{updatedReady, updatedEndpointReady} {
		select {
		case <-witnessed:
		case <-updatedDone:
			t.Fatalf("updated STRICT service exited before backend/proposal witnesses: code=%d err_present=%t canceled=%t", updatedCode, updatedErr != nil, errors.Is(updatedErr, context.Canceled))
		case <-updatedStartup.C:
			t.Fatal("updated STRICT service backend/proposal witness timed out")
		}
	}
	select {
	case <-updatedDone:
		t.Fatalf("updated STRICT service exited before stop: code=%d err_present=%t canceled=%t", updatedCode, updatedErr != nil, errors.Is(updatedErr, context.Canceled))
	default:
	}
	stopUpdated()
	select {
	case <-updatedDone:
	case <-time.After(5 * time.Second):
		t.Fatal("updated STRICT service did not join")
	}
	if updatedCode != 0 || updatedErr != nil || mutations() != before {
		t.Fatalf("updated STRICT service result: code=%d err_present=%t canceled=%t mutations_unchanged=%t", updatedCode, updatedErr != nil, errors.Is(updatedErr, context.Canceled), mutations() == before)
	}
	installedFixtureContext = context.Background()
	// An older but verified current release can dispatch a consented lifecycle
	// command; it still cannot become the pinned STRICT service executable.
	installedFixturePinnedExecutable = physical
	if promotionResumeAdmitted(selected, ref) == nil {
		t.Fatal("lagging current release admitted as service")
	}
	// The operator's routine stop checks the pinned claim/definition but does
	// not unpublish, remove the claim or change any device credentials.
	priorActions = len(actions)
	code, err = runCommand([]string{"stop-installed"}, nil, nil)
	if code != 0 || err != nil || len(actions) != priorActions+2 || actions[priorActions] != "quiesce" || actions[priorActions+1] != "assert-stopped" || mutations() != before || promotionDefinitionAt(definition, newDefinition) != nil {
		t.Fatalf("owned stop changed route/definition or bypassed quiescence: %d %v %v", code, err, actions)
	}
	if _, err := os.Lstat(claim); err != nil {
		t.Fatal("routine stop discarded the owned claim")
	}
	if saved, e := os.ReadFile(devicePath); e != nil || !bytes.Equal(saved, beforeDevices) {
		t.Fatal("routine stop changed device/role store")
	}
	// A failed native observation during routine stop cannot report success or
	// discard route, credentials, definition or claim (distinct from removal).
	goodDriver := native.driver
	native.driver = func(call context.Context, action string) error {
		actions = append(actions, action)
		return errors.New("inert native quiescence unknown")
	}
	code, err = runCommand([]string{"stop-installed"}, nil, nil)
	if code != 1 || err == nil || mutations() != before || promotionDefinitionAt(definition, newDefinition) != nil {
		t.Fatal("uncertain routine stop reported success or changed authority")
	}
	if _, e := os.Lstat(claim); e != nil {
		t.Fatal("uncertain routine stop lost claim")
	}
	if saved, e := os.ReadFile(devicePath); e != nil || !bytes.Equal(saved, beforeDevices) {
		t.Fatal("uncertain routine stop changed device credentials")
	}
	native.driver = goodDriver
	// Foreign definitions and unresolved transitions never authorize a stop.
	if err := os.WriteFile(definition, []byte("bystander"), 0600); err != nil {
		t.Fatal(err)
	}
	priorActions = len(actions)
	code, err = runCommand([]string{"stop-installed"}, nil, nil)
	if code != 1 || err == nil || len(actions) != priorActions || mutations() != before {
		t.Fatal("foreign definition was stopped")
	}
	code, err = runCommand([]string{"remove-installed", "node-fixture"}, nil, nil)
	if code != 1 || err == nil || len(actions) != priorActions || mutations() != before {
		t.Fatal("foreign definition was removed")
	}
	if err := os.WriteFile(definition, []byte(newDefinition), 0600); err != nil {
		t.Fatal(err)
	}
	if missing {
		// A present registered route cannot be abandoned even with consent.
		args := []string{"remove-missing-installed", "node-fixture", "not-an-operation", "", "I-ACCEPT-ABSENCE-REMOVAL"}
		prior := len(actions)
		code, err = runCommand(args, nil, nil)
		if code != 1 || err == nil || len(actions) != prior || mutations() != before {
			t.Fatal("present route entered missing removal")
		}
		tailscalecli.SetInstalledSetupRouteAbsentFixture(m)
		report, _ := m.Recover(context.Background(), cfg.TailscaleCLIScope, cfg.InstanceID, cfg.TailscaleCLIOrigin, 8443, cfg.Port)
		if report.OperationID == "" || report.Observation != "selected-listener-absent" {
			t.Fatalf("missing recovery identity: %+v", report)
		}
		args[2], args[3] = report.OperationID, report.ReservationAttemptID
		code, err = runCommand([]string{"remove-installed", "node-fixture"}, nil, nil)
		if code != 1 || err == nil || len(actions) != prior {
			t.Fatal("ordinary unpublish fell back to absence removal")
		}
		for _, denied := range [][]string{args[:len(args)-1], {"remove-missing-installed", "node-fixture", "wrong-operation", report.ReservationAttemptID, "I-ACCEPT-ABSENCE-REMOVAL"}} {
			code, err = runCommand(denied, nil, nil)
			if code == 0 || err == nil || len(actions) != prior || mutations() != before {
				t.Fatal("missing consent or stale operation reached native effects")
			}
		}
		// A valid operation and reservation cannot authorize a different node's
		// stop or poison the intent needed by the correctly bound continuation.
		claimBefore, e := os.ReadFile(filepath.Join(claim, "phase-a"))
		if e != nil {
			t.Fatal(e)
		}
		wrongNode := append([]string(nil), args...)
		wrongNode[1] = "wrong-node"
		code, err = runCommand(wrongNode, nil, nil)
		if code != 1 || err == nil || len(actions) != prior || mutations() != before {
			t.Fatal("wrong node reached native or Serve effects")
		}
		if _, e := os.Lstat(filepath.Join(claim, "removal-intent")); !errors.Is(e, os.ErrNotExist) {
			t.Fatal("wrong node wrote removal intent")
		}
		claimAfter, e := os.ReadFile(filepath.Join(claim, "phase-a"))
		if e != nil || !bytes.Equal(claimBefore, claimAfter) || promotionDefinitionAt(definition, newDefinition) != nil {
			t.Fatal("wrong node changed claim or definition")
		}
		afterReport, _ := m.Recover(context.Background(), cfg.TailscaleCLIScope, cfg.InstanceID, cfg.TailscaleCLIOrigin, 8443, cfg.Port)
		if !reflect.DeepEqual(report, afterReport) {
			t.Fatal("wrong node changed route or journal")
		}
		if saved, e := os.ReadFile(devicePath); e != nil || !bytes.Equal(saved, beforeDevices) || assertDevices(store, context.Background()) != nil {
			t.Fatal("wrong node changed device credentials")
		}
		// An uncertain native stop retains the exact registered journal.
		native.driver = func(call context.Context, action string) error { return errors.New("native stop unknown") }
		code, err = runCommand(args, nil, nil)
		if code != 1 || err == nil || mutations() != before || promotionDefinitionAt(definition, newDefinition) != nil {
			t.Fatal("uncertain absence stop retired authority")
		}
		if _, e := os.Lstat(filepath.Join(claim, "removal-intent")); !errors.Is(e, os.ErrNotExist) {
			t.Fatal("failed stop wrote removal intent")
		}
		// A concurrent route reappearance after the owned stop must fence
		// removal with its exact intent, not retire the definition or claim.
		reappeared := false
		native.driver = func(call context.Context, action string) error {
			if action == "assert-stopped" && !reappeared {
				reappeared = true
				tailscalecli.SetInstalledSetupRoutePresentFixture(m)
			}
			return goodDriver(call, action)
		}
		code, err = runCommand(args, nil, nil)
		if code != 1 || err == nil || mutations() != before || promotionDefinitionAt(definition, newDefinition) != nil {
			t.Fatal("reappearing route retired owned authority")
		}
		if _, e := os.Lstat(filepath.Join(claim, "removal-intent")); e != nil || promotionResumeAdmitted(selected, ref) == nil {
			t.Fatal("uncertain missing removal lost its fence")
		}
		tailscalecli.SetInstalledSetupRouteAbsentFixture(m)
		native.driver = goodDriver
		code, err = runCommand(args, nil, nil)
		if code != 0 || err != nil || mutations() != before {
			t.Fatalf("exact absence removal refused: %d %v", code, err)
		}
		if _, e := os.Lstat(definition); !errors.Is(e, os.ErrNotExist) {
			t.Fatal("missing removal retained owned definition")
		}
		if _, e := os.Lstat(claim); !errors.Is(e, os.ErrNotExist) || promotionResumeAdmitted(selected, ref) == nil {
			t.Fatal("missing removal retained claim or admitted STRICT resume")
		}
		if saved, e := os.ReadFile(devicePath); e != nil || !bytes.Equal(saved, beforeDevices) {
			t.Fatal("missing removal changed device credentials")
		}
		return
	}
	if uncertain {
		native.driver = func(call context.Context, action string) error {
			if action != "quiesce" {
				t.Fatalf("unexpected native action after uncertain stop: %s", action)
			}
			actions = append(actions, action)
			return errors.New("native state unestablished")
		}
		code, err = runCommand([]string{"remove-installed", "node-fixture"}, nil, nil)
		if code != 1 || err == nil || mutations() != before || promotionDefinitionAt(definition, newDefinition) != nil {
			t.Fatal("uncertain stop changed owned route or definition")
		}
		if _, err := os.Stat(filepath.Join(claim, "removal-intent")); err != nil {
			t.Fatal("uncertain removal intent lost")
		}
		if promotionResumeAdmitted(selected, ref) == nil {
			t.Fatal("ambiguous removal admitted STRICT resume")
		}
		prior := len(actions)
		code, err = runCommand([]string{"update-installed", candidate}, nil, nil)
		if code != 1 || err == nil || len(actions) != prior || mutations() != before {
			t.Fatal("ambiguous removal was adopted for update")
		}
		return
	}
	// A second setup cannot adopt the registered route, replace the definition,
	// or consume an old publication receipt.
	installedFixtureContext = context.Background()
	code, err = runCommand([]string{"promote-installed"}, nil, nil)
	if code != 1 || err == nil || mutations() != before {
		t.Fatal("repeat promotion adopted route")
	}
	code, err = runCommand([]string{"remove-installed", "node-fixture"}, nil, nil)
	if code != 0 || err != nil || mutations() != before+1 || actions[len(actions)-5] != "quiesce" || actions[len(actions)-4] != "assert-stopped" || actions[len(actions)-3] != "assert-stopped" || actions[len(actions)-2] != "remove" || actions[len(actions)-1] != "retire" {
		t.Fatalf("scoped removal failed: %d %v %v", code, err, actions)
	}
	if _, err := os.Lstat(definition); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("owned definition retained after removal")
	}
	if promotionResumeAdmitted(selected, ref) == nil {
		t.Fatal("removed service resumed")
	}
	if _, err := os.Lstat(claim); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("exact owned claim retained after verified removal")
	}
	if saved, e := os.ReadFile(devicePath); e != nil || !bytes.Equal(saved, beforeDevices) {
		t.Fatal("removal changed paired devices")
	}
}
