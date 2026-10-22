package coordinator

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/0cv/herdr-mobile-relay/internal/herdr"
	"github.com/0cv/herdr-mobile-relay/internal/profiles"
)

func TestCustomAgentRenameRefusalIsPartiallyApplied(t *testing.T) {
	dir := t.TempDir()
	bin := writeScript(t, dir, "herdr", "#!/bin/sh\n"+
		"if [ \"$1 $2\" = \"agent get\" ]; then\n"+
		"  printf '%s' '{\"result\":{\"pane_id\":\"pane-1\",\"running\":true}}'\n"+
		"elif [ \"$1 $2\" = \"agent rename\" ]; then\n"+
		"  printf '%s' '{\"error\":{\"code\":\"pane_not_found\"}}' >&2\n"+
		"  exit 1\n"+
		"else\n  printf '%s' '{\"result\":{}}'\nfi\n")
	client := herdr.NewClient(bin, filepath.Join(dir, "sock"))
	t.Cleanup(func() { _ = client.Close() })
	lifecycle := &Lifecycle{herdr: client}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	err := lifecycle.startInTarget(ctx, profiles.Profile{Argv: []string{"custom-agent"}}, StartRequest{Name: "agent"}, "pane-1")
	if !errors.Is(err, herdr.ErrPartiallyApplied) {
		t.Fatalf("rename failure = %v, want partially applied", err)
	}
	result := (&Dispatcher{logger: testLogger()}).failErr("start", "agent_start", "pane-1", err)
	if result.Phase != "dispatched_unknown" {
		t.Fatalf("result = %+v, want uncertain dispatch", result)
	}
}

func TestCustomAgentTimeoutAfterPaneRunIsDispatchedUnknown(t *testing.T) {
	dir := t.TempDir()
	bin := writeScript(t, dir, "herdr", "#!/bin/sh\n"+
		"if [ \"$1 $2\" = \"agent get\" ]; then\n"+
		"  printf '{\"result\":{\"pane_id\":\"pane-1\",\"running\":false}}\\n'\n"+
		"else\n"+
		"  printf '{\"result\":{}}\\n'\n"+
		"fi\n")
	lifecycle := &Lifecycle{herdr: herdr.NewClient(bin, filepath.Join(dir, "sock"))}
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()

	err := lifecycle.startInTarget(ctx, profiles.Profile{Argv: []string{"custom-agent"}}, StartRequest{Name: "agent"}, "pane-1")
	if !errors.Is(err, herdr.ErrDispatchedUnknown) {
		t.Fatalf("custom agent timeout = %v, want dispatched_unknown", err)
	}
	if errors.Is(err, herdr.ErrNotStarted) {
		t.Fatalf("custom agent timeout was also classified not_started: %v", err)
	}
}

func TestLaunchRecordsHermesProfileOnlyWhereItWasSent(t *testing.T) {
	for _, test := range []struct{ name, kind, requested, want string }{
		{"built-in with a profile", "hermes", "apollo", "apollo"},
		{"built-in without a profile", "hermes", "", ""},
		{"custom argv profile", "", "apollo", ""},
	} {
		t.Run(test.name, func(t *testing.T) {
			lifecycle := hermesLifecycle(t, "apollo")
			requireNoError(t, lifecycle.profiles.BeginLaunchOwnership(reconcileTarget, "hermes"))
			requireNoError(t, lifecycle.rememberLaunch(reconcileTarget, profiles.Profile{ID: "hermes", Kind: test.kind}, StartRequest{HermesProfile: test.requested}))
			owned, _, _ := lifecycle.profiles.OwnedHermesProfile(reconcileTarget, "hermes")
			requireNoError(t, lifecycle.profiles.ReconcileOwnership([]profiles.PaneIdentity{reconcileTarget}))
			if got, known := lifecycle.profiles.ResolveOwnedHermesProfile(reconcileTarget.PaneID); !owned || got != test.want || known != (test.kind == "hermes") {
				t.Fatalf("verified=%v recorded profile %q known=%v, want %q", owned, got, known, test.want)
			}
		})
	}
	lifecycle := hermesLifecycle(t, "apollo")
	_ = lifecycle.profiles.SetOwnershipPath(lifecycle.home) // refused (a directory): the store requires recovery
	err := lifecycle.rememberLaunch(reconcileTarget, profiles.Profile{ID: "hermes", Kind: "hermes"}, StartRequest{HermesProfile: "apollo"})
	if !errors.Is(err, herdr.ErrPartiallyApplied) {
		t.Fatalf("unsaved record = %v, want partially applied", err)
	}
}

// Clear and restart (one handler) relaunch with the pane's recorded Hermes profile
// (refused by name here, as it is gone from disk) and refuse a Hermes pane whose
// profile is unknown, instead of starting Hermes' implicit profile.
func TestClearKeepsTheRecordedHermesProfile(t *testing.T) {
	for _, test := range []struct {
		name, recorded string
		legacy         bool
		want           string
	}{
		{"recorded profile", "apollo", false, `hermes profile "apollo" is not installed`},
		{"started without a profile", "", false, "Command failed"},
		{"no record from an older relay", "", true, "Hermes profile is unknown"},
	} {
		t.Run(test.name, func(t *testing.T) {
			f := newReconcileFixture(t, test.recorded, test.legacy)
			state := NewState(testLogger())
			d := NewDispatcher(nil, state, nil, testLogger())
			t.Cleanup(func() { _ = d.Close(context.Background()) })
			d.SetProfiles(f.lifecycle.profiles)
			d.lifecycle.home = f.lifecycle.home
			d.lifecycle.inventory = func(context.Context) (*herdr.Inventory, error) { return nil, errors.New("no herdr") }
			pane := reconcileTarget
			state.CommitInventory([]*AgentState{{PaneID: pane.PaneID, RawPaneID: pane.PaneID, TerminalID: pane.TerminalID, TabID: pane.TabID, WorkspaceID: pane.WorkspaceID, Agent: "hermes", Status: "idle", Cwd: f.cwd}}, state.RevisionCounter())
			requireNoError(t, os.RemoveAll(filepath.Join(f.lifecycle.home, ".hermes", "profiles", "apollo")))
			if result := d.handleClear(context.Background(), time.Now(), "clear-test", pane.PaneID); result.OK || !strings.Contains(result.Error, test.want) {
				t.Fatalf("handleClear = %+v, want an error containing %q", result, test.want)
			}
		})
	}
}
