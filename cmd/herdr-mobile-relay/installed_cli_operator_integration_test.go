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
	"sync"
	"testing"
	"time"

	"github.com/0cv/herdr-mobile-relay/internal/app"
	"github.com/0cv/herdr-mobile-relay/internal/config"
	"github.com/0cv/herdr-mobile-relay/internal/deviceauth"
	"github.com/0cv/herdr-mobile-relay/internal/localcontrol"
	"github.com/0cv/herdr-mobile-relay/internal/tailscalecli"
	"github.com/0cv/herdr-mobile-relay/internal/transport"
)

func commandOperatorRequest(ctx context.Context, core localcontrol.InstalledControl, c localcontrol.InstalledCredentials, op string) localcontrol.InstalledReply {
	data, _ := json.Marshal(localcontrol.InstalledRequest{Protocol: localcontrol.InstalledProtocol, Run: c.Run, Installation: c.Installation, Capability: c.Capability, Op: op})
	var reply localcontrol.InstalledReply
	_ = json.Unmarshal(core.Dispatch(ctx, data), &reply)
	return reply
}

func TestInstalledCommandOperatorComposition(t *testing.T) {
	for _, mode := range []string{"setup", "resume"} {
		for _, exit := range []string{"complete", "error", "cancel-drain", "strict-refusal"} {
			t.Run(mode+"/"+exit, func(t *testing.T) {
				app.IsolateInstalledFixture(t)
				app.DenyInstalledCommandHTTP(t)
				root := installedCLICommandEnv(t)
				registered := mode == "resume"
				if exit == "strict-refusal" {
					registered = !registered
				}
				var manager *tailscalecli.Manager
				var state, coordination, binary string
				mutations := func() int { return 0 }
				if registered {
					manager, state, coordination, binary = tailscalecli.NewInstalledInertFixture(t)
				} else {
					manager, state, coordination, binary, mutations = tailscalecli.NewInstalledSetupInertFixture(t)
				}
				for key, value := range map[string]string{
					"HERDR_TAILSCALE_CLI_STATE_ROOT": state, "HERDR_TAILSCALE_CLI_COORDINATION_ROOT": coordination,
					"HERDR_TAILSCALE_CLI_BIN": binary, "HERDR_RELAY_INSTANCE_ID": "install-fixture",
					"HERDR_TAILSCALE_CLI_ORIGIN":     "https://herdr.tailnet.ts.net:8443",
					"HERDR_PHONE_APP_URL":            "https://herdr.tailnet.ts.net:8443",
					"HERDR_TAILSCALE_CLI_HTTPS_PORT": "8443", "HERDR_RELAY_PORT": "18377", "HERDR_RELAY_PLUGIN_PORT": "18378",
				} {
					t.Setenv(key, value)
				}
				runtime := filepath.Join(root, "config", "relay")
				if err := os.MkdirAll(runtime, 0700); err != nil {
					t.Fatal(err)
				}
				seed, err := deviceauth.Open(filepath.Join(runtime, "device-auth"))
				if err != nil {
					t.Fatal(err)
				}
				var selectors []transport.E2EEAuthSelector
				var secrets [][]byte
				for _, role := range []deviceauth.Role{deviceauth.RoleController, deviceauth.RoleReader} {
					invitation, err := seed.CreateInvitation(string(role), role, "en")
					if err != nil {
						t.Fatal(err)
					}
					auth, err := seed.CompleteE2EEAuth(context.Background(), transport.E2EEAuthSelector{Kind: transport.E2EEAuthInvitation, ID: invitation.InvitationID, Version: invitation.Version}, true)
					if err != nil {
						t.Fatal(err)
					}
					selector := transport.E2EEAuthSelector{Kind: transport.E2EEAuthCredential, ID: auth.Identity.CredentialID, Version: auth.Identity.CredentialVersion, Locale: auth.Identity.Locale}
					secret, err := seed.ResolveE2EESecret(context.Background(), selector)
					if err != nil {
						t.Fatal(err)
					}
					selectors = append(selectors, selector)
					secrets = append(secrets, secret)
				}
				path := filepath.Join(runtime, "device-auth", "devices.json")
				before, err := os.ReadFile(path)
				if err != nil {
					t.Fatal(err)
				}
				roles := seed.ListCredentials("")
				ctx, cancel := context.WithCancel(context.Background())
				installedFixtureContext = ctx
				var original config.InstalledConfig
				installedFixtureManager = func(selected config.InstalledConfig) (*tailscalecli.Manager, error) {
					original = selected
					return manager, nil
				}
				t.Cleanup(func() { installedFixtureManager = nil; installedFixtureContext = nil; installedFixtureEndpoint = nil })
				generation := ""
				runnerEntered := make(chan struct{})
				runnerClosed := make(chan struct{})
				app.SetInstalledInertRunner(t, func(s *app.Server, runCtx context.Context) error {
					close(runnerEntered)
					defer close(runnerClosed)
					return app.RunInstalledCommandLogicalBackend(s, runCtx, func(store *deviceauth.Store, run string) error {
						generation = run
						if !reflect.DeepEqual(roles, store.ListCredentials("")) {
							return errors.New("roles changed")
						}
						for i, selector := range selectors {
							secret, err := store.ResolveE2EESecret(runCtx, selector)
							if err != nil || !bytes.Equal(secret, secrets[i]) {
								return errors.New("credential changed")
							}
						}
						return nil
					})
				})
				type entry struct {
					ctx         context.Context
					core        localcontrol.InstalledControl
					credentials localcontrol.InstalledCredentials
					err         error
				}
				entered := make(chan entry, 1)
				release := make(chan struct{})
				var releaseOnce sync.Once
				unlock := func() { releaseOnce.Do(func() { close(release) }) }
				installedFixtureEndpoint = func(call context.Context, selected config.InstalledConfig, core localcontrol.InstalledControl, c localcontrol.InstalledCredentials) error {
					var check error
					snapshot := selected.Snapshot()
					if !reflect.DeepEqual(original, selected) || selected.Validate(&snapshot) != nil || generation != c.Run || c.Installation != "install-fixture" || c.Capability == "" {
						check = errors.New("original selection or generation lost")
					}
					reply := commandOperatorRequest(call, core, c, "proposal")
					if mode == "setup" {
						if reply.Code != "ok" || reply.Proposal == nil {
							check = errors.New("setup proposal absent")
						}
					} else if reply.Code != "publication_disabled" {
						check = errors.New("resume gained proposal")
					}
					entered <- entry{call, core, c, check}
					if exit == "cancel-drain" {
						<-call.Done()
						<-release
						return call.Err()
					}
					if exit == "error" {
						return errors.New("/private-canary/capability")
					}
					return nil
				}
				type result struct {
					code int
					err  error
				}
				done := make(chan struct{})
				var outcome result
				command := "serve-installed"
				if mode == "setup" {
					command = "setup-installed"
				}
				go func() {
					defer close(done)
					outcome.code, outcome.err = runCommand([]string{command}, func(*app.Server, context.Context) error { return errors.New("legacy runner forbidden") }, func(context.Context) error { return errors.New("sweep forbidden") })
				}()
				// Every failure cancels, unconditionally releases the callback, then joins
				// the entire command before any environment/HTTP/manager restoration.
				defer func() { cancel(); unlock(); <-done }()
				wait := func(ch <-chan struct{}) {
					t.Helper()
					select {
					case <-ch:
					case <-time.After(5 * time.Second):
						t.Fatal("fixture join/notification timeout")
					}
				}
				if exit == "strict-refusal" {
					wait(done)
					if outcome.code != 1 || outcome.err == nil {
						t.Fatal("strict mode fell through")
					}
					select {
					case <-runnerEntered:
						t.Fatal("strict refusal constructed app")
					default:
					}
				} else {
					var e entry
					select {
					case e = <-entered:
					case <-done:
						t.Fatal("command ended before callback")
					case <-time.After(5 * time.Second):
						t.Fatal("callback not observed")
					}
					if e.err != nil {
						t.Fatal(e.err)
					}
					if exit == "cancel-drain" {
						cancel()
						wait(e.ctx.Done())
						select {
						case <-done:
							t.Fatal("command did not join callback")
						default:
						}
						unlock()
					} else {
						wait(e.ctx.Done()) // invokeInstalledOperator retires before canceling this.
						select {
						case <-done:
							t.Fatal("callback exit stopped service")
						default:
						}
						select {
						case <-runnerClosed:
							t.Fatal("healthy backend stopped")
						default:
						}
						if commandOperatorRequest(context.Background(), e.core, e.credentials, "status").Code != "retired" {
							t.Fatal("retained core active")
						}
						cancel()
					}
					wait(done)
					wait(runnerClosed)
					want := 0
					if exit == "error" {
						want = 1
					}
					if outcome.code != want {
						t.Fatal("wrong command exit")
					}
					if want == 1 && (outcome.err == nil || outcome.err.Error() != "installed Tailscale CLI startup refused") {
						t.Fatal("callback failure leaked")
					}
					if commandOperatorRequest(context.Background(), e.core, e.credentials, "status").Code != "retired" {
						t.Fatal("core survived service stop")
					}
				}
				if mutations() != 0 {
					t.Fatal("automatic publication")
				}
				after, err := os.ReadFile(path)
				if err != nil || !bytes.Equal(before, after) {
					t.Fatal("device state changed or automatically armed")
				}
			})
		}
	}
}

func TestInstalledCommandNilEndpointDenies(t *testing.T) {
	installedFixtureEndpoint = nil
	if err := installedCommandEndpoint(context.Background(), config.InstalledConfig{}, localcontrol.InstalledControl{}, localcontrol.InstalledCredentials{}); err == nil {
		t.Fatal("nil endpoint did not deny")
	}
}
