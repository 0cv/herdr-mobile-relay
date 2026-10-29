//go:build herdr_installed_inert_test

package main

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/0cv/herdr-mobile-relay/internal/app"
	"github.com/0cv/herdr-mobile-relay/internal/config"
	"github.com/0cv/herdr-mobile-relay/internal/deviceauth"
	"github.com/0cv/herdr-mobile-relay/internal/tailscalecli"
	"github.com/0cv/herdr-mobile-relay/internal/transport"
)

func TestInstalledCLICommandConcreteLifecycle(t *testing.T) {
	app.IsolateInstalledFixture(t)
	root := installedCLICommandEnv(t)
	m, state, coordination, binary := tailscalecli.NewInstalledInertFixture(t)
	for key, value := range map[string]string{
		"HERDR_TAILSCALE_CLI_STATE_ROOT": state, "HERDR_TAILSCALE_CLI_COORDINATION_ROOT": coordination,
		"HERDR_TAILSCALE_CLI_BIN": binary, "HERDR_RELAY_INSTANCE_ID": "install-fixture",
		"HERDR_TAILSCALE_CLI_ORIGIN":     "https://herdr.tailnet.ts.net:8443",
		"HERDR_PHONE_APP_URL":            "https://herdr.tailnet.ts.net:8443",
		"HERDR_TAILSCALE_CLI_HTTPS_PORT": "8443", "HERDR_RELAY_PORT": "18377", "HERDR_RELAY_PLUGIN_PORT": "18378",
	} {
		t.Setenv(key, value)
	}
	installedFixtureManager = func(config.InstalledConfig) (*tailscalecli.Manager, error) { return m, nil }
	type contextKey struct{}
	installedFixtureContext = context.WithValue(context.Background(), contextKey{}, "inert")
	t.Cleanup(func() { installedFixtureManager = nil; installedFixtureContext = nil })
	if config.TailscaleCLIProfilesEnabled() {
		t.Fatal("fixture enabled production")
	}

	runtime := filepath.Join(root, "config", "relay")
	if err := os.MkdirAll(runtime, 0700); err != nil {
		t.Fatal(err)
	}
	store, err := deviceauth.Open(filepath.Join(runtime, "device-auth"))
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
		result, err := store.CompleteE2EEAuth(context.Background(), transport.E2EEAuthSelector{Kind: transport.E2EEAuthInvitation, ID: invitation.InvitationID, Version: invitation.Version}, true)
		if err != nil {
			t.Fatal(err)
		}
		selector := transport.E2EEAuthSelector{Kind: transport.E2EEAuthCredential, ID: result.Identity.CredentialID, Version: result.Identity.CredentialVersion, Locale: result.Identity.Locale}
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
	read := func(path string) []byte {
		t.Helper()
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		return data
	}
	path := filepath.Join(runtime, "device-auth", "devices.json")
	before, journal := read(path), read(filepath.Join(state, "registration.json"))
	for _, kind := range []string{"runtime", "cache", "runtime-wait", "cache-wait"} {
		t.Run("AliasDrift/"+kind, func(t *testing.T) {
			original := filepath.Join(root, "config")
			key, suffix := "HERDR_RELAY_ENV", filepath.Join("relay", "relay.env")
			if strings.HasPrefix(kind, "cache") {
				original = filepath.Join(root, "cache")
				key, suffix = "XDG_CACHE_HOME", ""
			}
			if err := os.MkdirAll(original, 0700); err != nil {
				t.Fatal(err)
			}
			replacement := filepath.Join(t.TempDir(), "replacement")
			if err := os.Mkdir(replacement, 0700); err != nil {
				t.Fatal(err)
			}
			leaf := "relay"
			if strings.HasPrefix(kind, "cache") {
				leaf = "herdr-mobile-relay"
			}
			// Existing private leaves make this a permitted ancestor alias,
			// not a refusal caused by a symlink at the nearest existing leaf.
			for _, base := range []string{original, replacement} {
				if err := os.MkdirAll(filepath.Join(base, leaf), 0700); err != nil {
					t.Fatal(err)
				}
			}
			alias := filepath.Join(root, "selected-"+kind)
			setAlias := func(target string) {
				t.Helper()
				if err := os.Remove(alias); err != nil && !os.IsNotExist(err) {
					t.Fatal(err)
				}
				if err := os.Symlink(target, alias); err != nil {
					t.Fatal(err)
				}
			}
			setAlias(original)
			t.Setenv(key, filepath.Join(alias, suffix))
			factoryCalls, appCalls := 0, 0
			installedFixtureManager = func(selected config.InstalledConfig) (*tailscalecli.Manager, error) {
				factoryCalls++
				snapshot := selected.Snapshot()
				if err := selected.Validate(&snapshot); err != nil {
					t.Fatal(err)
				}
				if strings.HasSuffix(kind, "-wait") {
					tailscalecli.ScriptInstalledInertStartup(t, m, []string{"Starting"}, func(ctx context.Context, _ time.Duration) error { setAlias(replacement); return ctx.Err() })
				} else {
					setAlias(replacement)
				}
				return m, nil
			}
			app.SetInstalledInertRunner(t, func(*app.Server, context.Context) error { appCalls++; return nil })
			code, err := dispatchInstalledCLI(t)
			if code != 1 || err == nil || appCalls != 0 || factoryCalls != 1 {
				t.Errorf("alias drift admitted: code=%d err=%v app=%d factory=%d", code, err, appCalls, factoryCalls)
			}
			entries, err := os.ReadDir(filepath.Join(replacement, leaf))
			if err != nil {
				t.Fatal(err)
			}
			if len(entries) != 0 {
				t.Error("replacement destination acquired application resources")
			}
			if !bytes.Equal(before, read(path)) || !bytes.Equal(journal, read(filepath.Join(state, "registration.json"))) {
				t.Fatal("alias drift changed original persistent state")
			}
			setAlias(original)
			installedFixtureManager = func(config.InstalledConfig) (*tailscalecli.Manager, error) { return m, nil }
			app.SetInstalledInertRunner(t, func(s *app.Server, ctx context.Context) error {
				actual, _ := app.InstalledInertState(t, s)
				if !reflect.DeepEqual(credentials, actual.ListCredentials("")) {
					t.Fatal("restored alias lost credentials/roles")
				}
				for i, selector := range selectors {
					secret, err := actual.ResolveE2EESecret(ctx, selector)
					if err != nil || !bytes.Equal(secret, secrets[i]) {
						t.Fatal("restored alias lost secret")
					}
				}
				return nil
			})
			if code, err := dispatchInstalledCLI(t); code != 0 || err != nil {
				t.Fatalf("restored alias restart: %d %v", code, err)
			}
			if !bytes.Equal(before, read(path)) || !bytes.Equal(journal, read(filepath.Join(state, "registration.json"))) {
				t.Fatal("restart changed original persistent state")
			}
		})
	}
	var retained *tailscalecli.InstalledSession
	var lifetime context.Context
	calls := 0
	outcome := error(nil)
	app.SetInstalledInertRunner(t, func(s *app.Server, ctx context.Context) error {
		calls++
		if ctx.Value(contextKey{}) != "inert" {
			t.Fatal("context lost")
		}
		actual, authority := app.InstalledInertState(t, s)
		retained, lifetime = authority, ctx
		if !reflect.DeepEqual(credentials, actual.ListCredentials("")) {
			t.Fatal("roles/credentials changed")
		}
		for i, selector := range selectors {
			secret, err := actual.ResolveE2EESecret(ctx, selector)
			if err != nil || !bytes.Equal(secret, secrets[i]) {
				t.Fatal("secret changed")
			}
		}
		return outcome
	})
	t.Run("StartupWait", func(t *testing.T) {
		delays := 0
		tailscalecli.ScriptInstalledInertStartup(t, m, []string{"Starting", "Stopped", "incomplete"}, func(ctx context.Context, d time.Duration) error {
			if d != time.Second<<delays {
				t.Fatal("wrong backoff")
			}
			delays++
			return ctx.Err()
		})
		outcome = tailscalecli.ErrTransientUnavailable
		if code, err := dispatchInstalledCLI(t); code != 1 || err == nil || calls != 1 || delays != 3 {
			t.Fatalf("startup: %d %v calls=%d waits=%d", code, err, calls, delays)
		}
		if lifetime.Err() == nil || retained.ValidateRuntimeBinding(tailscalecli.InstalledBinding{}) == nil {
			t.Fatal("authority not retired")
		}
		if !bytes.Equal(before, read(path)) || !bytes.Equal(journal, read(filepath.Join(state, "registration.json"))) {
			t.Fatal("startup changed persistent bytes")
		}
	})
	calls = 0
	t.Run("StartupCancel", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		original := installedFixtureContext
		installedFixtureContext = ctx
		defer func() { installedFixtureContext = original }()
		tailscalecli.ScriptInstalledInertStartup(t, m, []string{"Starting"}, func(ctx context.Context, _ time.Duration) error { cancel(); return ctx.Err() })
		_, _ = dispatchInstalledCLI(t)
		if calls != 0 {
			t.Fatal("waiting cancellation constructed app")
		}
	})
	for _, failure := range []bool{false, true, false} {
		outcome = nil
		if failure {
			outcome = errors.New("sensitive-token-path-canary")
		}
		code, err := dispatchInstalledCLI(t)
		if failure {
			if code != 1 || err == nil || err.Error() != "installed Tailscale CLI startup refused" {
				t.Fatalf("unredacted failure: %v", err)
			}
		} else if code != 0 || err != nil {
			t.Fatalf("command: %d %v", code, err)
		}
		if lifetime.Err() == nil {
			t.Fatal("lifetime not canceled")
		}
		if _, err := retained.VerifyRegisteredRoute(context.Background(), "production", "install-fixture", "https://herdr.tailnet.ts.net:8443", 8443, 18377); err == nil {
			t.Fatal("authority retained")
		}
		if !bytes.Equal(before, read(path)) || !bytes.Equal(journal, read(filepath.Join(state, "registration.json"))) {
			t.Fatal("persistent state changed")
		}
	}
	if calls != 3 {
		t.Fatal("concrete runner not reached")
	}
	outcome = nil
	// Still parse the actual selected environment; never substitute a Config.
	for _, pair := range [][2]string{
		{"HERDR_RELAY_PORT", "18379"}, {"HERDR_RELAY_CONTROL_RUN_ID", "wrong"},
		{"HERDR_RELAY_RUN_ID", "wrong"}, {"HERDR_RELAY_PAIRING_SOCKET", "/wrong"},
		{"HERDR_TAILSCALE_CLI_SCOPE", "development"}, {"HERDR_TAILSCALE_CLI_BIN", binary + "-wrong"},
		{"HERDR_TAILSCALE_CLI_STATE_ROOT", state + "-wrong"},
		{"HERDR_TAILSCALE_CLI_COORDINATION_ROOT", coordination + "-wrong"},
		{"HERDR_RELAY_ENV", filepath.Join(state, "relay.env")},
		{"HERDR_RELAY_REARM_BOOTSTRAP", "true"}, {"HERDR_REACHABILITY_PORT_MAPPING", "1"},
	} {
		t.Run(pair[0], func(t *testing.T) {
			t.Setenv(pair[0], pair[1])
			if code, err := dispatchInstalledCLI(t); code != 1 || err == nil {
				t.Fatal("mismatch admitted")
			}
			if calls != 3 {
				t.Fatal("mismatch reached app")
			}
		})
	}
	other, _, _, _ := tailscalecli.NewInstalledInertFixture(t)
	installedFixtureManager = func(config.InstalledConfig) (*tailscalecli.Manager, error) { return other, nil }
	if code, err := dispatchInstalledCLI(t); code != 1 || err == nil || calls != 3 {
		t.Fatal("wrong manager admitted")
	}
	installedFixtureManager = func(config.InstalledConfig) (*tailscalecli.Manager, error) {
		return nil, errors.New("sensitive-token-path-canary")
	}
	if _, err := dispatchInstalledCLI(t); err == nil || err.Error() != "installed Tailscale CLI startup refused" {
		t.Fatal("manager error leaked")
	}
	installedFixtureManager = func(config.InstalledConfig) (*tailscalecli.Manager, error) { return m, nil }
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	installedFixtureContext = ctx
	_, _ = dispatchInstalledCLI(t)
	if calls != 3 {
		t.Fatal("canceled entry constructed app")
	}
	for _, leaf := range []string{"runtime", "cache", "device-auth"} {
		if _, err := os.Lstat(filepath.Join(state, leaf)); !os.IsNotExist(err) {
			t.Fatal("registration credential copy")
		}
	}
	ctx, cancel = context.WithCancel(context.Background())
	defer cancel()
	installedFixtureContext = ctx
	app.SetInstalledInertRunner(t, func(s *app.Server, runCtx context.Context) error {
		_, retained = app.InstalledInertState(t, s)
		cancel()
		if runCtx.Err() != context.Canceled {
			t.Fatal("parent cancellation not propagated")
		}
		return runCtx.Err()
	})
	if code, err := dispatchInstalledCLI(t); code != 0 || err != nil {
		t.Fatalf("canceled lifecycle: %d %v", code, err)
	}
	if _, err := retained.VerifyRegisteredRoute(context.Background(), "production", "install-fixture", "https://herdr.tailnet.ts.net:8443", 8443, 18377); err == nil {
		t.Fatal("cancellation retained authority")
	}
	if !bytes.Equal(before, read(path)) {
		t.Fatal("original credential file changed")
	}
}
