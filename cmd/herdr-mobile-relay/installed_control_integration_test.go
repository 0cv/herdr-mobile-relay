//go:build herdr_installed_inert_test

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"reflect"
	"strings"
	"testing"

	"github.com/0cv/herdr-mobile-relay/internal/app"
	"github.com/0cv/herdr-mobile-relay/internal/config"
	"github.com/0cv/herdr-mobile-relay/internal/localcontrol"
)

type controlScript func(context.Context, localcontrol.InstalledIntent) (localcontrol.InstalledClientResult, error)

func (f controlScript) Do(c context.Context, i localcontrol.InstalledIntent) (localcontrol.InstalledClientResult, error) {
	return f(c, i)
}

type controlJoinedWriter struct {
	t   *testing.T
	ctx *context.Context
	bytes.Buffer
}

func (w *controlJoinedWriter) Write(b []byte) (int, error) {
	if *w.ctx == nil || (*w.ctx).Err() == nil {
		w.t.Fatal("request scope not stopped before stdout")
	}
	return w.Buffer.Write(b)
}
func controlFixture(t *testing.T) config.InstalledConfig {
	t.Helper()
	app.IsolateInstalledFixture(t)
	installedCLICommandEnv(t)
	installedFixtureContext = context.Background()
	t.Cleanup(func() {
		installedControlFixtureNew = nil
		installedControlFixtureDiscover = nil
		installedFixtureContext = nil
	})
	c, e := config.LoadInstalled()
	if e != nil {
		t.Fatal(e)
	}
	s, e := config.BindInstalledCLI(c)
	if e != nil {
		t.Fatal(e)
	}
	return s
}
func TestInstalledControlScriptedCommand(t *testing.T) {
	for _, name := range []string{"proposal-discovery", "status-discovery", "publish-exact", "arm-stable", "receipt", "reprint", "negative", "lost-reply", "cancel-return", "constructor-race", "stale-fresh-invocation", "output-error"} {
		t.Run(name, func(t *testing.T) {
			selected := controlFixture(t)
			cfg := selected.Snapshot()
			var captured context.Context
			discover, newCalls, calls := 0, 0, 0
			root, cancel := context.WithCancel(context.Background())
			defer cancel()
			installedFixtureContext = root
			op := "status"
			switch name {
			case "proposal-discovery":
				op = "proposal"
			case "publish-exact":
				op = "publish"
			case "arm-stable", "lost-reply", "cancel-return", "negative", "output-error":
				op = "arm"
			case "reprint":
				op = "reprint"
			}
			args := []string{op}
			if !strings.HasSuffix(name, "discovery") {
				args = append(args, "--run", controlTestRun)
			}
			if op == "arm" || name == "receipt" {
				args = append(args, "--operation-id", "stable:exact")
			}
			consent := controlTestConsent()
			consent.Proposal.Installation = cfg.InstanceID
			consent.Proposal.Selection = "<exact>&"
			input, _ := json.Marshal(consent)
			installedControlFixtureDiscover = func(ctx context.Context, s config.InstalledConfig) (string, error) {
				discover++
				if !reflect.DeepEqual(s, selected) {
					t.Fatal("selection rebound")
				}
				return controlTestRun, nil
			}
			installedControlFixtureNew = func(ctx context.Context, s config.InstalledConfig, run string) (installedControlClient, error) {
				newCalls++
				captured = ctx
				if !reflect.DeepEqual(s, selected) || run != controlTestRun {
					t.Fatal("original selection/run lost")
				}
				if name == "constructor-race" || name == "stale-fresh-invocation" {
					return nil, errors.New("private-constructor-canary")
				}
				// Ambient changes cannot reselect this invocation.
				t.Setenv("HERDR_RELAY_INSTANCE_ID", "changed-after-selection")
				return controlScript(func(ctx context.Context, i localcontrol.InstalledIntent) (localcontrol.InstalledClientResult, error) {
					calls++
					if i.ExpectedRun != controlTestRun {
						t.Fatal("missing pin")
					}
					if op == "arm" || name == "receipt" {
						if i.OperationID != "stable:exact" {
							t.Fatal("ID changed")
						}
					}
					if op == "publish" && (i.Consent == nil || *i.Consent != consent) {
						t.Fatal("consent changed")
					}
					p := consent.Proposal
					r := localcontrol.InstalledClientResult{Transmission: localcontrol.InstalledValidatedReply, Run: controlTestRun, Installation: cfg.InstanceID, Reply: localcontrol.InstalledReply{Code: "ok", Publication: "acknowledged", ArmReceipt: "acknowledged", Status: &localcontrol.InstalledObservation{}, Proposal: &p, Invitation: "PRIVATE-INVITATION"}}
					if op == "proposal" {
						r.Reply.Publication = "not_dispatched"
					}
					if name == "negative" {
						r.Reply.Code = "consumed"
					}
					if name == "cancel-return" {
						cancel()
					}
					if name == "lost-reply" {
						r.Transmission = localcontrol.InstalledPossibleDispatch
						return r, errors.New("private-error-canary")
					}
					return r, nil
				}), nil
			}
			out := &controlJoinedWriter{t: t, ctx: &captured}
			var code int
			var e error
			if name == "output-error" {
				w := &controlTestWriter{err: errors.New("private-writer-canary")}
				code, e = runInstalledControl(args, bytes.NewReader(input), w)
				if code != 1 || e != errControlOutput || w.calls != 1 {
					t.Fatal("output policy")
				}
			} else {
				code, e = runInstalledControl(args, bytes.NewReader(input), out)
				if e != nil {
					t.Fatal("unexpected public error")
				}
			}
			wantDiscover := 0
			if strings.HasSuffix(name, "discovery") {
				wantDiscover = 1
			}
			wantCalls := 1
			if name == "constructor-race" || name == "stale-fresh-invocation" {
				wantCalls = 0
			}
			if discover != wantDiscover || newCalls != 1 || calls != wantCalls {
				t.Fatal("retry or omitted call")
			}
			secret := op == "arm" && name == "arm-stable" || op == "reprint"
			if strings.Contains(out.String(), "PRIVATE-INVITATION") != secret {
				t.Fatal("secret projection")
			}
			if strings.Contains(out.String(), "canary") {
				t.Fatal("error leaked")
			}
			if name == "lost-reply" || name == "cancel-return" {
				if code != 3 || !strings.Contains(out.String(), "possible_dispatch_lost_reply") {
					t.Fatal("uncertainty relabelled")
				}
			}
			if name == "constructor-race" || name == "stale-fresh-invocation" {
				if code != 1 {
					t.Fatal("stale accepted")
				}
			}
		})
	}
}
func TestInstalledControlFactoryDispatch(t *testing.T) {
	controlFixture(t)
	calls := 0
	installedControlFixtureNew = func(context.Context, config.InstalledConfig, string) (installedControlClient, error) {
		calls++
		return nil, errControlRefused
	}
	// Private regular file, not a pipe/socket; no worker or blocking output teardown.
	f, e := os.CreateTemp(t.TempDir(), "output")
	if e != nil {
		t.Fatal(e)
	}
	old := os.Stdout
	os.Stdout = f
	defer func() { os.Stdout = old; _ = f.Close() }()
	code, e := runCommand([]string{"installed-control", "status", "--run", controlTestRun}, nil, nil)
	if code != 1 || e != nil || calls != 1 {
		t.Fatal("actual dispatch/factory missing")
	}
}
func TestInstalledControlNilFactoryDenies(t *testing.T) {
	s := controlFixture(t)
	if installedControlEnabled() {
		t.Fatal("nil enabled")
	}
	if _, e := installedControlNew(context.Background(), s, controlTestRun); e == nil {
		t.Fatal("nil native fallback")
	}
	if _, e := installedControlDiscover(context.Background(), s); e == nil {
		t.Fatal("nil discovery fallback")
	}
}
