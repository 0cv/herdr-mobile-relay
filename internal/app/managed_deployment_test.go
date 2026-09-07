package app

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/0cv/herdr-mobile-relay/internal/activeruntime"
	"github.com/0cv/herdr-mobile-relay/internal/config"
	"github.com/0cv/herdr-mobile-relay/internal/coordinator"
	"github.com/0cv/herdr-mobile-relay/internal/copyresponse"
	"github.com/0cv/herdr-mobile-relay/internal/herdr"
	"github.com/0cv/herdr-mobile-relay/internal/protocol"
	"github.com/0cv/herdr-mobile-relay/internal/readiness"
	"github.com/0cv/herdr-mobile-relay/internal/slashcmd"
	"github.com/0cv/herdr-mobile-relay/internal/transport"
	"github.com/coder/websocket"
)

func TestManagedTopologyDeltaAcceptsOnlyTheAuthorizedFleetChange(t *testing.T) {
	personal := activeruntime.TopologyPane{PaneID: "personal", NativeSessionID: "session-personal", ProfileID: "personal", WorkspaceID: "workspace-personal"}
	emu := activeruntime.TopologyPane{PaneID: "emu", NativeSessionID: "session-emu", ProfileID: "emu", WorkspaceID: "workspace-emu"}
	started := activeruntime.TopologyPane{PaneID: "new", NativeSessionID: "session-new", ProfileID: "personal", WorkspaceID: "workspace-personal"}
	replacement := activeruntime.TopologyPane{PaneID: "replacement", NativeSessionID: "session-replacement", ProfileID: "personal", WorkspaceID: "workspace-personal"}
	completed := func(action, paneID string) *coordinator.CommandResult {
		return &coordinator.CommandResult{Action: action, PaneID: paneID, OK: true, Phase: "completed"}
	}
	tests := []struct {
		name   string
		target managedTopologyTarget
		result *coordinator.CommandResult
		before []activeruntime.TopologyPane
		after  []activeruntime.TopologyPane
		valid  bool
	}{
		{name: "start adds requested profile", target: managedTopologyTarget{action: "agent_start", profileID: "personal"}, result: completed("agent_start", "new"), before: []activeruntime.TopologyPane{personal, emu}, after: []activeruntime.TopologyPane{personal, emu, started}, valid: true},
		{name: "start cannot remove another pane", target: managedTopologyTarget{action: "agent_start", profileID: "personal"}, result: completed("agent_start", "new"), before: []activeruntime.TopologyPane{personal, emu}, after: []activeruntime.TopologyPane{personal, started}},
		{name: "stop removes only its target", target: managedTopologyTarget{action: "agent_stop", paneID: "personal"}, result: completed("agent_stop", "personal"), before: []activeruntime.TopologyPane{personal, emu}, after: []activeruntime.TopologyPane{emu}, valid: true},
		{name: "stop cannot remove another pane", target: managedTopologyTarget{action: "agent_stop", paneID: "personal"}, result: completed("agent_stop", "personal"), before: []activeruntime.TopologyPane{personal, emu}, after: nil},
		{name: "clear replaces its target", target: managedTopologyTarget{action: "agent_clear", paneID: "personal"}, result: completed("agent_clear", "personal"), before: []activeruntime.TopologyPane{personal, emu}, after: []activeruntime.TopologyPane{replacement, emu}, valid: true},
		{name: "restart uses the clear implementation", target: managedTopologyTarget{action: "agent_restart", paneID: "personal"}, result: completed("agent_clear", "personal"), before: []activeruntime.TopologyPane{personal, emu}, after: []activeruntime.TopologyPane{replacement, emu}, valid: true},
		{name: "clear warning may retain old target", target: managedTopologyTarget{action: "agent_clear", paneID: "personal"}, result: &coordinator.CommandResult{Action: "agent_clear", PaneID: "personal", OK: true, Phase: "completed_with_warning"}, before: []activeruntime.TopologyPane{personal, emu}, after: []activeruntime.TopologyPane{personal, replacement, emu}, valid: true},
		{name: "clear cannot cross profiles", target: managedTopologyTarget{action: "agent_clear", paneID: "personal"}, result: completed("agent_clear", "personal"), before: []activeruntime.TopologyPane{personal, emu}, after: []activeruntime.TopologyPane{{PaneID: "replacement", NativeSessionID: "session-replacement", ProfileID: "emu", WorkspaceID: "workspace-personal"}, emu}},
		{name: "workspace close removes only contained panes", target: managedTopologyTarget{action: "workspace_close", workspaceID: "workspace-personal"}, result: completed("workspace_close", ""), before: []activeruntime.TopologyPane{personal, emu}, after: []activeruntime.TopologyPane{emu}, valid: true},
		{name: "workspace close cannot remove another workspace", target: managedTopologyTarget{action: "workspace_close", workspaceID: "workspace-personal"}, result: completed("workspace_close", ""), before: []activeruntime.TopologyPane{personal, emu}, after: nil},
		{name: "metadata topology preserves the fleet", target: managedTopologyTarget{action: "workspace_rename"}, result: completed("workspace_rename", ""), before: []activeruntime.TopologyPane{personal, emu}, after: []activeruntime.TopologyPane{emu, personal}, valid: true},
		{name: "safe failure cannot hide topology drift", target: managedTopologyTarget{action: "agent_stop", paneID: "personal"}, result: &coordinator.CommandResult{Action: "agent_stop", Phase: "not_started"}, before: []activeruntime.TopologyPane{personal, emu}, after: []activeruntime.TopologyPane{emu}},
		{name: "unknown stop may have landed", target: managedTopologyTarget{action: "agent_stop", paneID: "personal"}, result: &coordinator.CommandResult{Action: "agent_stop", PaneID: "personal", Phase: "dispatched_unknown"}, before: []activeruntime.TopologyPane{personal, emu}, after: []activeruntime.TopologyPane{emu}, valid: true},
		{name: "blank result action adopts target", target: managedTopologyTarget{action: "workspace_rename"}, result: completed("", ""), valid: true},
		{name: "nil result is ambiguous", target: managedTopologyTarget{action: "agent_stop", paneID: "personal"}, result: nil, before: []activeruntime.TopologyPane{personal}, after: nil},
		{name: "mismatched result action", target: managedTopologyTarget{action: "agent_stop", paneID: "personal"}, result: completed("agent_start", "personal"), before: []activeruntime.TopologyPane{personal}, after: nil},
		{name: "invalid prior inventory", target: managedTopologyTarget{action: "agent_stop"}, result: completed("agent_stop", ""), before: []activeruntime.TopologyPane{{PaneID: "", NativeSessionID: "s", ProfileID: "p"}}},
		{name: "invalid reconciled inventory", target: managedTopologyTarget{action: "agent_stop"}, result: completed("agent_stop", ""), after: []activeruntime.TopologyPane{{PaneID: "p", NativeSessionID: "", ProfileID: "p"}}},
		{name: "unknown start may have no visible delta yet", target: managedTopologyTarget{action: "agent_start", profileID: "personal"}, result: &coordinator.CommandResult{Action: "agent_start", Phase: "dispatched_unknown"}, valid: true},
		{name: "failed start with pane id may have no visible delta yet", target: managedTopologyTarget{action: "agent_start", profileID: "personal"}, result: &coordinator.CommandResult{Action: "agent_start", PaneID: "new", Phase: "failed"}, valid: true},
		{name: "successful start may reconcile exact existing pane", target: managedTopologyTarget{action: "agent_start", profileID: "personal"}, result: completed("agent_start", "personal"), before: []activeruntime.TopologyPane{personal, emu}, after: []activeruntime.TopologyPane{personal, emu}, valid: true},
		{name: "successful start requires one addition", target: managedTopologyTarget{action: "agent_start", profileID: "personal"}, result: completed("agent_start", "new")},
		{name: "start rejects blank profile", target: managedTopologyTarget{action: "agent_start"}, result: completed("agent_start", "new"), after: []activeruntime.TopologyPane{started}},
		{name: "start rejects wrong profile", target: managedTopologyTarget{action: "agent_start", profileID: "emu"}, result: completed("agent_start", "new"), after: []activeruntime.TopologyPane{started}},
		{name: "start rejects wrong result pane", target: managedTopologyTarget{action: "agent_start", profileID: "personal"}, result: completed("agent_start", "other"), after: []activeruntime.TopologyPane{started}},
		{name: "start rejects multiple additions", target: managedTopologyTarget{action: "agent_start", profileID: "personal"}, result: completed("agent_start", "new"), after: []activeruntime.TopologyPane{started, personal}},
		{name: "stop may still be unchanged when unknown", target: managedTopologyTarget{action: "agent_stop", paneID: "personal"}, result: &coordinator.CommandResult{Action: "agent_stop", Phase: "dispatched_unknown"}, before: []activeruntime.TopologyPane{personal}, after: []activeruntime.TopologyPane{personal}, valid: true},
		{name: "successful stop requires one removal", target: managedTopologyTarget{action: "agent_stop", paneID: "personal"}, result: completed("agent_stop", "personal"), before: []activeruntime.TopologyPane{personal}, after: []activeruntime.TopologyPane{personal}},
		{name: "stop rejects additions", target: managedTopologyTarget{action: "agent_stop", paneID: "personal"}, result: completed("agent_stop", "personal"), before: []activeruntime.TopologyPane{personal}, after: []activeruntime.TopologyPane{started}},
		{name: "stop rejects unrelated removal", target: managedTopologyTarget{action: "agent_stop", paneID: "other"}, result: completed("agent_stop", "other"), before: []activeruntime.TopologyPane{personal}, after: nil},
		{name: "replacement target must exist", target: managedTopologyTarget{action: "agent_clear", paneID: "missing"}, result: completed("agent_clear", "missing"), before: []activeruntime.TopologyPane{personal}, after: []activeruntime.TopologyPane{replacement}},
		{name: "unknown replacement may still be unchanged", target: managedTopologyTarget{action: "agent_clear", paneID: "personal"}, result: &coordinator.CommandResult{Action: "agent_clear", Phase: "dispatched_unknown"}, before: []activeruntime.TopologyPane{personal}, after: []activeruntime.TopologyPane{personal}, valid: true},
		{name: "successful replacement requires one addition", target: managedTopologyTarget{action: "agent_clear", paneID: "personal"}, result: completed("agent_clear", "personal"), before: []activeruntime.TopologyPane{personal}},
		{name: "replacement rejects unrelated removal", target: managedTopologyTarget{action: "agent_clear", paneID: "personal"}, result: &coordinator.CommandResult{Action: "agent_clear", Phase: "dispatched_unknown"}, before: []activeruntime.TopologyPane{personal, emu}, after: []activeruntime.TopologyPane{personal}},
		{name: "replacement must remove target without warning", target: managedTopologyTarget{action: "agent_clear", paneID: "personal"}, result: completed("agent_clear", "personal"), before: []activeruntime.TopologyPane{personal}, after: []activeruntime.TopologyPane{personal, replacement}},
		{name: "empty workspace removal is valid", target: managedTopologyTarget{action: "workspace_close", workspaceID: "empty"}, result: completed("workspace_close", ""), before: []activeruntime.TopologyPane{personal}, after: []activeruntime.TopologyPane{personal}, valid: true},
		{name: "workspace removal requires target", target: managedTopologyTarget{action: "workspace_close"}, result: completed("workspace_close", ""), valid: false},
		{name: "metadata rejects fleet change", target: managedTopologyTarget{action: "worktree_open"}, result: completed("worktree_open", ""), after: []activeruntime.TopologyPane{personal}},
		{name: "unsupported topology action", target: managedTopologyTarget{action: "future"}, result: completed("future", "")},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := validateManagedTopologyDelta(test.target, test.result, test.before, test.after)
			if (err == nil) != test.valid {
				t.Fatalf("delta validation error = %v, want valid=%t", err, test.valid)
			}
		})
	}
}

func TestManagedTopologyPaneIndexRejectsEveryIncompleteOrDuplicateIdentity(t *testing.T) {
	valid := activeruntime.TopologyPane{PaneID: "pane", NativeSessionID: "session", ProfileID: "profile"}
	if indexed, err := managedTopologyPaneIndex(nil); err != nil || len(indexed) != 0 {
		t.Fatalf("empty pane index = (%v, %v)", indexed, err)
	}
	for index, panes := range [][]activeruntime.TopologyPane{
		{{PaneID: "", NativeSessionID: "session", ProfileID: "profile"}},
		{{PaneID: "pane", NativeSessionID: "", ProfileID: "profile"}},
		{{PaneID: "pane", NativeSessionID: "session", ProfileID: ""}},
		{valid, {PaneID: "pane", NativeSessionID: "other", ProfileID: "profile"}},
		{valid, {PaneID: "other", NativeSessionID: "session", ProfileID: "profile"}},
	} {
		if _, err := managedTopologyPaneIndex(panes); err == nil {
			t.Fatalf("invalid pane identity %d was accepted", index)
		}
	}
}

func TestManagedTopologySnapshotRequiresAReadyCompleteInventory(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	state := coordinator.NewState(logger)
	if snapshot, ready := managedTopologySnapshotFromState(state); ready || len(snapshot.panes) != 0 {
		t.Fatalf("unready snapshot = (%+v, %t)", snapshot, ready)
	}
	state.CommitTopology(nil, []herdr.Workspace{{ID: "workspace", Label: "Workspace"}}, 0)
	if snapshot, ready := managedTopologySnapshotFromState(state); !ready || len(snapshot.panes) != 0 || len(snapshot.workspaces) != 1 {
		t.Fatalf("ready empty snapshot = (%+v, %t)", snapshot, ready)
	}
	state.CommitInventory([]*coordinator.AgentState{{PaneID: "pane", SessionID: "", ProfileID: "profile"}}, state.RevisionCounter())
	if _, ready := managedTopologySnapshotFromState(state); ready {
		t.Fatal("incomplete live identity produced a recovery snapshot")
	}
	state.CommitInventory([]*coordinator.AgentState{
		{PaneID: "z", SessionID: "session-z", ProfileID: "profile", WorkspaceID: "workspace"},
		{PaneID: "a", SessionID: "session-a", ProfileID: "profile", WorkspaceID: "workspace"},
	}, state.RevisionCounter())
	snapshot, ready := managedTopologySnapshotFromState(state)
	if !ready || len(snapshot.panes) != 2 || snapshot.panes[0].PaneID != "a" || snapshot.panes[1].PaneID != "z" {
		t.Fatalf("sorted managed snapshot = (%+v, %t)", snapshot, ready)
	}
}

func TestManagedTopologyCommitOrdersRecoveryCheckpointBeforeExpectedInventory(t *testing.T) {
	before := managedTopologySnapshot{panes: []activeruntime.TopologyPane{{PaneID: "old", NativeSessionID: "session-old", ProfileID: "personal", WorkspaceID: "workspace"}}}
	after := managedTopologySnapshot{panes: []activeruntime.TopologyPane{
		{PaneID: "old", NativeSessionID: "session-old", ProfileID: "personal", WorkspaceID: "workspace"},
		{PaneID: "new", NativeSessionID: "session-new", ProfileID: "personal", WorkspaceID: "workspace"},
	}}
	marker := &fakeManagedTopologyMarker{}
	var order []string
	commit := &managedTopologyCommit{
		target: managedTopologyTarget{action: "agent_start", profileID: "personal"},
		before: before,
		marker: marker,
		reconcile: func(context.Context) error {
			order = append(order, "reconcile")
			return nil
		},
		snapshot: func() (managedTopologySnapshot, bool) { return after, true },
		checkpoint: func(_ context.Context, acknowledgedEmpty bool) error {
			if acknowledgedEmpty {
				t.Fatal("non-empty fleet was checkpointed as empty")
			}
			order = append(order, "checkpoint")
			return nil
		},
		publish: func(panes []readiness.Pane, acknowledgedEmpty bool) error {
			if acknowledgedEmpty || len(panes) != 2 {
				t.Fatalf("published inventory = (%+v, %t)", panes, acknowledgedEmpty)
			}
			order = append(order, "publish")
			return nil
		},
	}
	result := &coordinator.CommandResult{Action: "agent_start", PaneID: "new", OK: true, Phase: "completed"}
	if err := commit.finish(t.Context(), result); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(order, []string{"reconcile", "checkpoint", "publish"}) || marker.resolves != 1 {
		t.Fatalf("commit order = %v, resolves = %d", order, marker.resolves)
	}
}

func TestManagedTopologyCommitKeepsTheRecoveryBlockerOnAnyAmbiguousBoundary(t *testing.T) {
	pane := activeruntime.TopologyPane{PaneID: "pane", NativeSessionID: "session", ProfileID: "personal", WorkspaceID: "workspace"}
	tests := []struct {
		name       string
		result     *coordinator.CommandResult
		after      managedTopologySnapshot
		reconcile  error
		checkpoint error
		publish    error
	}{
		{name: "reconcile", result: &coordinator.CommandResult{Action: "agent_stop", PaneID: "pane", OK: true, Phase: "completed"}, reconcile: errors.New("poll failed")},
		{name: "unexpected delta", result: &coordinator.CommandResult{Action: "agent_stop", PaneID: "pane", OK: true, Phase: "completed"}, after: managedTopologySnapshot{panes: []activeruntime.TopologyPane{{PaneID: "other", NativeSessionID: "other", ProfileID: "emu", WorkspaceID: "other"}}}},
		{name: "checkpoint", result: &coordinator.CommandResult{Action: "agent_stop", PaneID: "pane", OK: true, Phase: "completed"}, checkpoint: errors.New("snapshot failed")},
		{name: "publish", result: &coordinator.CommandResult{Action: "agent_stop", PaneID: "pane", OK: true, Phase: "completed"}, publish: errors.New("publish failed")},
		{name: "dispatched unknown", result: &coordinator.CommandResult{Action: "agent_stop", PaneID: "pane", Phase: "dispatched_unknown"}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			marker := &fakeManagedTopologyMarker{}
			after := test.after
			if after.panes == nil && test.name != "unexpected delta" {
				after = managedTopologySnapshot{}
			}
			commit := &managedTopologyCommit{
				target:     managedTopologyTarget{action: "agent_stop", paneID: "pane"},
				before:     managedTopologySnapshot{panes: []activeruntime.TopologyPane{pane}},
				marker:     marker,
				reconcile:  func(context.Context) error { return test.reconcile },
				snapshot:   func() (managedTopologySnapshot, bool) { return after, true },
				checkpoint: func(context.Context, bool) error { return test.checkpoint },
				publish:    func([]readiness.Pane, bool) error { return test.publish },
			}
			if err := commit.finish(t.Context(), test.result); err == nil {
				t.Fatal("ambiguous commit boundary was accepted")
			}
			if marker.resolves != 0 {
				t.Fatal("ambiguous commit removed the recovery blocker")
			}
		})
	}
}

func TestManagedTopologyCommitResolvesANoEffectFailureAfterExactReconcile(t *testing.T) {
	pane := activeruntime.TopologyPane{PaneID: "pane", NativeSessionID: "session", ProfileID: "personal", WorkspaceID: "workspace"}
	snapshot := managedTopologySnapshot{panes: []activeruntime.TopologyPane{pane}}
	marker := &fakeManagedTopologyMarker{}
	checkpointed := false
	commit := &managedTopologyCommit{
		target:     managedTopologyTarget{action: "agent_stop", paneID: "pane"},
		before:     snapshot,
		marker:     marker,
		reconcile:  func(context.Context) error { return nil },
		snapshot:   func() (managedTopologySnapshot, bool) { return snapshot, true },
		checkpoint: func(context.Context, bool) error { checkpointed = true; return nil },
		publish:    func([]readiness.Pane, bool) error { t.Fatal("no-effect failure published inventory"); return nil },
	}
	if err := commit.finish(t.Context(), &coordinator.CommandResult{Action: "agent_stop", PaneID: "pane", Phase: "not_started"}); err != nil {
		t.Fatal(err)
	}
	if checkpointed || marker.resolves != 1 {
		t.Fatalf("no-effect failure checkpointed=%t resolves=%d", checkpointed, marker.resolves)
	}
}

func TestManagedTopologyCommitRejectsEveryIncompleteDependency(t *testing.T) {
	base := func() *managedTopologyCommit {
		return &managedTopologyCommit{
			target:     managedTopologyTarget{action: "workspace_rename"},
			marker:     &fakeManagedTopologyMarker{},
			reconcile:  func(context.Context) error { return nil },
			snapshot:   func() (managedTopologySnapshot, bool) { return managedTopologySnapshot{}, true },
			checkpoint: func(context.Context, bool) error { return nil },
			publish:    func([]readiness.Pane, bool) error { return nil },
		}
	}
	commits := []*managedTopologyCommit{nil}
	for _, mutate := range []func(*managedTopologyCommit){
		func(commit *managedTopologyCommit) { commit.marker = nil },
		func(commit *managedTopologyCommit) { commit.reconcile = nil },
		func(commit *managedTopologyCommit) { commit.snapshot = nil },
		func(commit *managedTopologyCommit) { commit.checkpoint = nil },
		func(commit *managedTopologyCommit) { commit.publish = nil },
	} {
		commit := base()
		mutate(commit)
		commits = append(commits, commit)
	}
	for index, commit := range commits {
		if err := commit.finish(t.Context(), &coordinator.CommandResult{Action: "workspace_rename", OK: true}); err == nil {
			t.Fatalf("incomplete commit %d was accepted", index)
		}
	}
}

func TestManagedTopologyCommitHandlesUnavailableEmptyAndResolveFailureOutcomes(t *testing.T) {
	base := func(marker *fakeManagedTopologyMarker) *managedTopologyCommit {
		return &managedTopologyCommit{
			target:     managedTopologyTarget{action: "agent_stop", paneID: "pane"},
			before:     managedTopologySnapshot{panes: []activeruntime.TopologyPane{{PaneID: "pane", NativeSessionID: "session", ProfileID: "profile"}}},
			marker:     marker,
			reconcile:  func(context.Context) error { return nil },
			snapshot:   func() (managedTopologySnapshot, bool) { return managedTopologySnapshot{}, true },
			checkpoint: func(context.Context, bool) error { return nil },
			publish: func(panes []readiness.Pane, acknowledgedEmpty bool) error {
				if len(panes) != 0 || !acknowledgedEmpty {
					t.Fatalf("empty commit published = (%+v, %t)", panes, acknowledgedEmpty)
				}
				return nil
			},
		}
	}
	marker := &fakeManagedTopologyMarker{err: errors.New("resolve")}
	if err := base(marker).finish(t.Context(), &coordinator.CommandResult{Action: "agent_stop", OK: true, Phase: "completed"}); err == nil {
		t.Fatal("successful empty commit ignored marker resolution failure")
	}
	commit := base(&fakeManagedTopologyMarker{})
	commit.snapshot = func() (managedTopologySnapshot, bool) { return managedTopologySnapshot{}, false }
	if err := commit.finish(t.Context(), &coordinator.CommandResult{Action: "agent_stop", OK: true}); err == nil {
		t.Fatal("unavailable reconciled snapshot was accepted")
	}
	commit = base(&fakeManagedTopologyMarker{})
	if err := commit.finish(t.Context(), nil); err == nil {
		t.Fatal("nil command outcome was accepted")
	}
	commit = base(&fakeManagedTopologyMarker{})
	commit.snapshot = func() (managedTopologySnapshot, bool) { return commit.before, true }
	commit.marker.(*fakeManagedTopologyMarker).err = errors.New("resolve")
	if err := commit.finish(t.Context(), &coordinator.CommandResult{Action: "agent_stop", Phase: "not_started"}); err == nil {
		t.Fatal("no-effect failure ignored marker resolution failure")
	}
}

func TestManagedTopologyCheckpointUsesOnlyTheFixedHelperAndSanitizedEnvironment(t *testing.T) {
	base := t.TempDir()
	root := filepath.Join(base, "herdr")
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatal(err)
	}
	argumentsPath := filepath.Join(base, "arguments")
	environmentPath := filepath.Join(base, "environment")
	helper := filepath.Join(base, "OuroWorkbenchRemote")
	script := fmt.Sprintf("#!/bin/sh\nprintf '%%s\\n' \"$@\" > '%s'\nenv > '%s'\n", argumentsPath, environmentPath)
	if err := os.WriteFile(helper, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{
		ActiveRuntimePath:     filepath.Join(root, "active-runtime.json"),
		ActiveGeneration:      "g1",
		TopologyCommitHelper:  helper,
		TopologyRemoteConfig:  filepath.Join(base, "profiles.json"),
		TopologyLedgerRoot:    filepath.Join(base, "ledger"),
		TopologySessionMap:    filepath.Join(base, "session-map.json"),
		TopologyShimDirectory: filepath.Join(base, "shims"),
		TopologyZDOTDir:       filepath.Join(base, "zdotdir"),
	}
	t.Setenv("HERDR_RELAY_TOKEN", "must-not-reach-the-helper")
	if err := runManagedTopologyCheckpoint(t.Context(), cfg, false); err != nil {
		t.Fatal(err)
	}
	arguments, err := os.ReadFile(argumentsPath)
	if err != nil {
		t.Fatal(err)
	}
	want := strings.Join([]string{
		"snapshot", "--config", cfg.TopologyRemoteConfig, "--root", root, "--ledger", cfg.TopologyLedgerRoot,
		"--session-map", cfg.TopologySessionMap, "--shim-directory", cfg.TopologyShimDirectory, "--zdotdir", cfg.TopologyZDOTDir, "",
	}, "\n")
	if string(arguments) != want {
		t.Fatalf("checkpoint arguments = %q, want %q", arguments, want)
	}
	environment, err := os.ReadFile(environmentPath)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(environment), "HERDR_RELAY_TOKEN") || strings.Contains(string(environment), "must-not-reach") {
		t.Fatalf("checkpoint inherited relay credentials: %s", environment)
	}
	if err := runManagedTopologyCheckpoint(t.Context(), cfg, true); err != nil {
		t.Fatal(err)
	}
	arguments, err = os.ReadFile(argumentsPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(string(arguments), "acknowledge-empty\n") || !strings.Contains(string(arguments), "--generation\n"+cfg.ActiveGeneration+"\n") {
		t.Fatalf("empty checkpoint arguments = %q", arguments)
	}
}

func TestManagedTopologyCheckpointRetriesUntilTheWorkbenchHookSettles(t *testing.T) {
	base := t.TempDir()
	root := filepath.Join(base, "herdr")
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatal(err)
	}
	countPath := filepath.Join(base, "attempts")
	helper := filepath.Join(base, "OuroWorkbenchRemote")
	script := fmt.Sprintf("#!/bin/sh\ncount=0\n[ ! -f '%s' ] || count=$(cat '%s')\ncount=$((count + 1))\nprintf '%%s' \"$count\" > '%s'\n[ \"$count\" -ge 3 ]\n", countPath, countPath, countPath)
	if err := os.WriteFile(helper, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	cfg := validManagedTopologyCheckpointConfig(base, root, helper)
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancel()
	if err := runManagedTopologyCheckpoint(ctx, cfg, false); err != nil {
		t.Fatal(err)
	}
	attempts, err := os.ReadFile(countPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(attempts) != "3" {
		t.Fatalf("checkpoint attempts = %q, want 3", attempts)
	}
}

func TestManagedTopologyCheckpointStopsRetryingAtItsDeadline(t *testing.T) {
	base := t.TempDir()
	root := filepath.Join(base, "herdr")
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatal(err)
	}
	helper := filepath.Join(base, "OuroWorkbenchRemote")
	if err := os.WriteFile(helper, []byte("#!/bin/sh\nexit 1\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	cfg := validManagedTopologyCheckpointConfig(base, root, helper)
	ctx, cancel := context.WithTimeout(t.Context(), 25*time.Millisecond)
	defer cancel()
	if err := runManagedTopologyCheckpoint(ctx, cfg, false); err == nil || !strings.Contains(err.Error(), "deadline") {
		t.Fatalf("checkpoint deadline error = %v", err)
	}
}

func validManagedTopologyCheckpointConfig(base, root, helper string) *config.Config {
	return &config.Config{
		ActiveRuntimePath: filepath.Join(root, "active-runtime.json"), ActiveGeneration: "g1", TopologyCommitHelper: helper,
		TopologyRemoteConfig: filepath.Join(base, "profiles.json"), TopologyLedgerRoot: filepath.Join(base, "ledger"),
		TopologySessionMap: filepath.Join(base, "session-map.json"), TopologyShimDirectory: filepath.Join(base, "shims"), TopologyZDOTDir: filepath.Join(base, "zdotdir"),
	}
}

func TestManagedTopologyCheckpointRejectsIncompleteOrNonCanonicalConfigurationBeforeExec(t *testing.T) {
	base := t.TempDir()
	helper := filepath.Join(base, "OuroWorkbenchRemote")
	if err := os.WriteFile(helper, []byte("#!/bin/sh\nexit 99\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	valid := &config.Config{
		ActiveRuntimePath: filepath.Join(base, "herdr", "active-runtime.json"), ActiveGeneration: "g1", TopologyCommitHelper: helper,
		TopologyRemoteConfig: filepath.Join(base, "profiles.json"), TopologyLedgerRoot: filepath.Join(base, "ledger"),
		TopologySessionMap: filepath.Join(base, "session-map.json"), TopologyShimDirectory: filepath.Join(base, "shims"), TopologyZDOTDir: filepath.Join(base, "zdotdir"),
	}
	for _, mutate := range []func(*config.Config){
		func(value *config.Config) { value.ActiveRuntimePath = "relative/active-runtime.json" },
		func(value *config.Config) { value.ActiveRuntimePath = filepath.Join(base, "herdr", "other.json") },
		func(value *config.Config) {
			value.ActiveRuntimePath = filepath.Join(base, "runtime", "active-runtime.json")
		},
		func(value *config.Config) { value.TopologyCommitHelper = "relative" },
		func(value *config.Config) { value.TopologyCommitHelper = filepath.Join(base, "OtherHelper") },
		func(value *config.Config) { value.TopologyRemoteConfig = "" },
		func(value *config.Config) { value.TopologyLedgerRoot = "../ledger" },
		func(value *config.Config) { value.TopologySessionMap = "" },
		func(value *config.Config) { value.TopologyShimDirectory = "" },
		func(value *config.Config) { value.TopologyZDOTDir = "" },
		func(value *config.Config) { value.ActiveGeneration = "" },
		func(value *config.Config) { value.ActiveGeneration = " g1" },
		func(value *config.Config) { value.ActiveGeneration = "g/1" },
	} {
		copy := *valid
		mutate(&copy)
		if err := runManagedTopologyCheckpoint(t.Context(), &copy, false); err == nil {
			t.Fatal("invalid managed checkpoint configuration reached execution")
		}
	}
	if err := runManagedTopologyCheckpoint(t.Context(), nil, false); err == nil {
		t.Fatal("nil managed checkpoint configuration reached execution")
	}
}

func TestManagedTopologyCheckpointRejectsEveryUnsafeHelperFileShape(t *testing.T) {
	base := t.TempDir()
	root := filepath.Join(base, "herdr")
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatal(err)
	}
	helper := filepath.Join(base, "OuroWorkbenchRemote")
	if err := os.WriteFile(helper, []byte("#!/bin/sh\nexit 0\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	cfg := validManagedTopologyCheckpointConfig(base, root, helper)
	info, err := os.Lstat(helper)
	if err != nil {
		t.Fatal(err)
	}
	stat := *info.Sys().(*syscall.Stat_t)
	unsafeLink := stat
	unsafeLink.Nlink = 2
	unsafeOwner := stat
	unsafeOwner.Uid++
	tests := []struct {
		name string
		info os.FileInfo
		err  error
	}{
		{name: "lstat", err: errors.New("lstat")},
		{name: "nil info"},
		{name: "non stat info", info: managedFileInfo{FileInfo: info, mode: info.Mode(), size: info.Size(), sys: "not-stat"}},
		{name: "directory", info: managedFileInfo{FileInfo: info, mode: os.ModeDir | 0o700, size: info.Size(), sys: info.Sys()}},
		{name: "non executable", info: managedFileInfo{FileInfo: info, mode: 0o600, size: info.Size(), sys: info.Sys()}},
		{name: "multiple links", info: managedFileInfo{FileInfo: info, mode: info.Mode(), size: info.Size(), sys: &unsafeLink}},
		{name: "other owner", info: managedFileInfo{FileInfo: info, mode: info.Mode(), size: info.Size(), sys: &unsafeOwner}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if err := validateManagedTopologyCheckpointConfigWith(func(string) (os.FileInfo, error) { return test.info, test.err }, cfg); err == nil {
				t.Fatal("unsafe helper was accepted")
			}
		})
	}
}

func TestManagedTopologySmallHelpersCoverEveryOutcome(t *testing.T) {
	if got := managedTopologyCheckpointEnvironment(nil); len(got) != 0 {
		t.Fatalf("empty environment = %v", got)
	}
	got := managedTopologyCheckpointEnvironment([]string{"NO_EQUALS", "SECRET=value", "HOME=/home", "LC_ALL=C"})
	if !slices.Equal(got, []string{"HOME=/home", "LC_ALL=C"}) {
		t.Fatalf("sanitized environment = %v", got)
	}
	if !normalizedManagedPath("/absolute") || normalizedManagedPath("relative") || normalizedManagedPath("/tmp/../tmp/value") {
		t.Fatal("managed path normalization was not exact")
	}
	if stat, ok := infoSyscallStat(nil); ok || stat != nil {
		t.Fatalf("nil stat info = (%v, %t)", stat, ok)
	}
	if managedTopologyTargetName(managedTopologyTarget{paneID: "pane", workspaceID: "workspace", profileID: "profile"}) != "pane" ||
		managedTopologyTargetName(managedTopologyTarget{workspaceID: "workspace", profileID: "profile"}) != "workspace" ||
		managedTopologyTargetName(managedTopologyTarget{profileID: "profile"}) != "profile" {
		t.Fatal("managed topology target precedence changed")
	}
	for _, action := range []string{"agent_start", "agent_stop", "agent_clear", "agent_restart", "workspace_create", "workspace_rename", "workspace_reorder", "workspace_close", "worktree_create", "worktree_open", "worktree_remove"} {
		if !managedTopologyAction(action) {
			t.Fatalf("managed topology action %q was rejected", action)
		}
	}
	if managedTopologyAction("worktree_list") || managedTopologyAction("future") {
		t.Fatal("non-mutating or unknown topology action was accepted")
	}
}

func TestManagedAgentStopCommitsCheckpointAndExpectedInventoryBeforeSuccess(t *testing.T) {
	base := t.TempDir()
	root := filepath.Join(base, "herdr")
	session := filepath.Join(root, "sessions", "g1")
	webRoot := filepath.Join(base, "web")
	for _, directory := range []string{root, session, webRoot} {
		if err := os.MkdirAll(directory, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	configHome := filepath.Join(base, "config")
	if err := os.MkdirAll(filepath.Join(configHome, "herdr"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(configHome, "herdr", "agent-profiles.ini"), []byte("[config]\nreplace_profiles = true\n[profiles]\npersonal = Personal\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(webRoot, "index.html"), []byte("ok"), 0o600); err != nil {
		t.Fatal(err)
	}
	statePath := filepath.Join(base, "pane-present")
	if err := os.WriteFile(statePath, []byte("present"), 0o600); err != nil {
		t.Fatal(err)
	}
	fakeHerdr := filepath.Join(base, "herdr-cli")
	herdrScript := fmt.Sprintf(`#!/bin/sh
case "$*" in
  "agent list")
    if [ -s '%s' ]; then
      printf '%%s\n' '{"result":{"agents":[{"pane_id":"pane-1","terminal_id":"terminal-1","workspace_id":"workspace-1","agent":"copilot","agent_status":"idle","cwd":"/tmp/project","agent_session":{"value":"session-1","kind":"native"}}]}}'
    else
      printf '%%s\n' '{"result":{"agents":[]}}'
    fi ;;
  "workspace list") printf '%%s\n' '{"result":{"workspaces":[{"workspace_id":"workspace-1","label":"Workspace"}]}}' ;;
  "tab list") printf '%%s\n' '{"result":{"tabs":[]}}' ;;
  "pane list") printf '%%s\n' '{"result":{"panes":[]}}' ;;
  "pane close pane-1") : > '%s'; printf '%%s\n' '{"ok":true}' ;;
  "integration status") printf '%%s\n' '{"ok":true}' ;;
  *) printf '%%s\n' '{"ok":true}' ;;
esac
`, statePath, statePath)
	if err := os.WriteFile(fakeHerdr, []byte(herdrScript), 0o700); err != nil {
		t.Fatal(err)
	}
	activePath := filepath.Join(root, "active-runtime.json")
	expectedPath := filepath.Join(session, "expected-inventory.json")
	socketPath := filepath.Join(session, "herdr.sock")
	active := fmt.Sprintf(`{"schemaVersion":1,"generation":"g1","sessionName":"g1","socketPath":%q,"expectedInventoryPath":%q}`, socketPath, expectedPath)
	if err := os.WriteFile(activePath, []byte(active), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(expectedPath, []byte(`{"version":1,"generation":"g1","acknowledged_empty":false,"panes":[{"pane_id":"pane-1","native_session_id":"session-1","profile_id":"personal"}]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	checkpointPath := filepath.Join(base, "checkpoint")
	helper := filepath.Join(base, "OuroWorkbenchRemote")
	helperScript := fmt.Sprintf(`#!/bin/sh
test -f '%s' || exit 10
grep -q 'pane-1' '%s' || exit 11
printf '%%s\n' "$1" > '%s'
`, filepath.Join(root, activeruntime.TopologyTransactionName), expectedPath, checkpointPath)
	if err := os.WriteFile(helper, []byte(helperScript), 0o700); err != nil {
		t.Fatal(err)
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := listener.Addr().(*net.TCPAddr).Port
	listener.Close()
	cfg := &config.Config{
		Host: "127.0.0.1", Port: port, PluginPort: port + 1, ManagedDeployment: true, InstanceID: "topology-commit",
		ActiveRuntimePath: activePath, ActiveGeneration: "g1", ExpectedInventoryPath: expectedPath, SocketPath: socketPath,
		HerdrBin: fakeHerdr, WebRoot: webRoot, RuntimeDir: filepath.Join(base, "runtime"), CacheDir: filepath.Join(base, "cache"),
		ConfigHome: configHome, ReleaseRoot: filepath.Join(base, "release"), PollInterval: 60,
		TopologyCommitHelper: helper, TopologyRemoteConfig: filepath.Join(base, "profiles.json"), TopologyLedgerRoot: filepath.Join(base, "ledger"),
		TopologySessionMap: filepath.Join(base, "session-map.json"), TopologyShimDirectory: filepath.Join(base, "shims"), TopologyZDOTDir: filepath.Join(base, "zdotdir"),
	}
	var serverLog bytes.Buffer
	server := New(cfg, "test", "revision", slog.New(slog.NewTextHandler(&serverLog, nil)))
	if err := server.profiles.Remember("pane-1", "personal"); err != nil {
		t.Fatal(err)
	}
	runContext, stop := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- server.Run(runContext) }()
	t.Cleanup(func() {
		stop()
		select {
		case err := <-done:
			if err != nil {
				t.Errorf("server shutdown: %v", err)
			}
		case <-time.After(3 * time.Second):
			t.Error("server did not stop")
		}
	})
	deadline := time.Now().Add(5 * time.Second)
	for !server.managedInventoryReadiness().Ready && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if ready := server.managedInventoryReadiness(); !ready.Ready {
		agent, _ := server.state.Agent("pane-1")
		t.Fatalf("managed fixture never became ready: %+v agent=%+v", ready, agent)
	}
	connection, _, err := websocket.Dial(t.Context(), fmt.Sprintf("ws://127.0.0.1:%d", port), nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = connection.CloseNow() })
	payload, err := json.Marshal(map[string]any{"type": "agent_stop", "request_id": "stop-1", "pane_id": "pane-1", "protocol": protocol.Version})
	if err != nil {
		t.Fatal(err)
	}
	if err := connection.Write(t.Context(), websocket.MessageText, payload); err != nil {
		t.Fatal(err)
	}
	result := readManagedMessage(t, connection, func(message map[string]any) bool { return message["request_id"] == "stop-1" })
	if result["ok"] != true || result["phase"] != "completed" {
		agent, _ := server.state.Agent("pane-1")
		t.Fatalf("managed stop result = %#v readiness=%+v inventory=%+v agent=%+v log=%s", result, server.managedInventoryReadiness(), server.state.InventoryStatus(), agent, serverLog.String())
	}
	checkpoint, err := os.ReadFile(checkpointPath)
	if err != nil || string(checkpoint) != "acknowledge-empty\n" {
		t.Fatalf("checkpoint = %q, %v", checkpoint, err)
	}
	if pending, err := activeruntime.TopologyTransactionPending(activePath); err != nil || pending {
		t.Fatalf("completed topology transaction = (%t, %v)", pending, err)
	}
	if ready := server.managedInventoryReadiness(); !ready.Ready || ready.State != readiness.StateAcknowledgedEmpty {
		t.Fatalf("committed empty inventory = %+v", ready)
	}
}

type fakeManagedTopologyMarker struct {
	resolves int
	err      error
}

func (m *fakeManagedTopologyMarker) Resolve() error {
	m.resolves++
	return m.err
}

type managedFileInfo struct {
	os.FileInfo
	mode os.FileMode
	size int64
	sys  any
}

func (i managedFileInfo) Mode() os.FileMode { return i.mode }
func (i managedFileInfo) Size() int64       { return i.size }
func (i managedFileInfo) Sys() any          { return i.sys }

func TestManagedDeploymentOmitsAndRejectsRemoteUpdates(t *testing.T) {
	root := t.TempDir()
	webRoot := filepath.Join(root, "web")
	if err := os.MkdirAll(webRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(webRoot, "index.html"), []byte("<html></html>"), 0o600); err != nil {
		t.Fatal(err)
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := listener.Addr().(*net.TCPAddr).Port
	listener.Close()
	cfg := &config.Config{
		Host: "127.0.0.1", Port: port, InstanceID: "managed-test", ManagedDeployment: true,
		WebRoot: webRoot, RuntimeDir: filepath.Join(root, "runtime"), CacheDir: filepath.Join(root, "cache"),
		ConfigHome: filepath.Join(root, "config"), ReleaseRoot: filepath.Join(root, "release"), HerdrBin: "/bin/false",
	}
	server := New(cfg, "0.20.8", "managed-revision", slog.New(slog.NewTextHandler(io.Discard, nil)))
	runContext, stop := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- server.Run(runContext) }()
	t.Cleanup(func() {
		stop()
		select {
		case err := <-done:
			if err != nil {
				t.Errorf("server shutdown: %v", err)
			}
		case <-time.After(3 * time.Second):
			t.Error("server did not stop")
		}
	})

	base := fmt.Sprintf("http://127.0.0.1:%d", port)
	deadline := time.Now().Add(5 * time.Second)
	for {
		response, requestErr := http.Get(base + "/health")
		if requestErr == nil {
			response.Body.Close()
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("managed relay did not start: %v", requestErr)
		}
		time.Sleep(20 * time.Millisecond)
	}
	connection, _, err := websocket.Dial(context.Background(), "ws"+strings.TrimPrefix(base, "http"), nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = connection.CloseNow() })

	configMessage := readManagedMessage(t, connection, func(message map[string]any) bool { return message["type"] == "push_config" })
	rawCapabilities, _ := configMessage["capabilities"].([]any)
	capabilities := make([]string, 0, len(rawCapabilities))
	for _, capability := range rawCapabilities {
		capabilities = append(capabilities, fmt.Sprint(capability))
	}
	if slices.Contains(capabilities, "self_update") || slices.Contains(capabilities, "app_deploy") {
		t.Fatalf("managed capabilities advertise update/deploy: %v", capabilities)
	}

	for _, action := range []string{"install_update", "deploy_app_update"} {
		requestID := "deny-" + action
		payload, err := json.Marshal(map[string]any{
			"type": action, "request_id": requestID, "protocol": protocol.Version,
			"expected_version": "0.20.9", "expected_revision": "off-manifest",
		})
		if err != nil {
			t.Fatal(err)
		}
		if err := connection.Write(context.Background(), websocket.MessageText, payload); err != nil {
			t.Fatal(err)
		}
		message := readManagedMessage(t, connection, func(message map[string]any) bool { return message["request_id"] == requestID })
		apiError, _ := message["error"].(map[string]any)
		if message["type"] != "error" || apiError["code"] != protocol.ErrorManagedDeployment {
			t.Fatalf("%s response = %#v, want stable managed policy error", action, message)
		}
	}
}

func TestManagedDeploymentRejectsRuntimeCommandsUntilExactRuntimeIsReady(t *testing.T) {
	root := t.TempDir()
	if err := os.Chmod(root, 0o700); err != nil {
		t.Fatal(err)
	}
	sessionRoot := filepath.Join(root, "sessions", "g1")
	activePath := filepath.Join(root, "active-runtime.json")
	expectedPath := filepath.Join(sessionRoot, "expected-inventory.json")
	socketPath := filepath.Join(sessionRoot, "herdr.sock")
	webRoot := filepath.Join(root, "web")
	if err := os.MkdirAll(webRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(webRoot, "index.html"), []byte("<html></html>"), 0o600); err != nil {
		t.Fatal(err)
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := listener.Addr().(*net.TCPAddr).Port
	listener.Close()
	server := New(&config.Config{
		Host: "127.0.0.1", Port: port, ManagedDeployment: true,
		ActiveRuntimePath:     activePath,
		ActiveGeneration:      "g1",
		ExpectedInventoryPath: expectedPath,
		SocketPath:            socketPath,
		InstanceID:            "managed-test",
		WebRoot:               webRoot,
		RuntimeDir:            filepath.Join(root, "runtime"),
		CacheDir:              filepath.Join(root, "cache"),
		ConfigHome:            filepath.Join(root, "config"),
		ReleaseRoot:           filepath.Join(root, "release"),
		HerdrBin:              "/bin/false",
	}, "0.20.8", "managed-revision", slog.New(slog.NewTextHandler(io.Discard, nil)))
	runContext, stop := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- server.Run(runContext) }()
	t.Cleanup(func() {
		stop()
		select {
		case err := <-done:
			if err != nil {
				t.Errorf("server shutdown: %v", err)
			}
		case <-time.After(3 * time.Second):
			t.Error("server did not stop")
		}
	})
	base := fmt.Sprintf("http://127.0.0.1:%d", port)
	deadline := time.Now().Add(5 * time.Second)
	for {
		response, requestErr := http.Get(base + "/health")
		if requestErr == nil {
			response.Body.Close()
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("managed relay did not start: %v", requestErr)
		}
		time.Sleep(20 * time.Millisecond)
	}
	connection, _, err := websocket.Dial(context.Background(), "ws"+strings.TrimPrefix(base, "http"), nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = connection.CloseNow() })

	payload, err := json.Marshal(map[string]any{
		"type": "workspace_create", "request_id": "blocked-before-ready", "protocol": protocol.Version,
		"cwd": root, "label": "Must not run",
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := connection.Write(context.Background(), websocket.MessageText, payload); err != nil {
		t.Fatal(err)
	}
	message := readManagedMessage(t, connection, func(message map[string]any) bool {
		return message["request_id"] == "blocked-before-ready"
	})
	apiError, _ := message["error"].(map[string]any)
	if message["type"] != "error" || apiError["code"] != protocol.ErrorManagedRuntimeNotReady {
		t.Fatalf("not-ready response = %#v, want stable managed readiness error", message)
	}

	allowedPayload, err := json.Marshal(map[string]any{
		"type": "device_list", "request_id": "allowed-before-ready", "protocol": protocol.Version,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := connection.Write(context.Background(), websocket.MessageText, allowedPayload); err != nil {
		t.Fatal(err)
	}
	if allowed := readManagedMessage(t, connection, func(message map[string]any) bool {
		return message["request_id"] == "allowed-before-ready"
	}); allowed["type"] == "error" {
		t.Fatalf("recovery-safe command was blocked: %#v", allowed)
	}

	if err := os.MkdirAll(sessionRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	active := fmt.Sprintf(`{"schemaVersion":1,"generation":"g1","sessionName":"g1","socketPath":%q,"expectedInventoryPath":%q}`, socketPath, expectedPath)
	if err := os.WriteFile(activePath, []byte(active), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(expectedPath, []byte(`{"version":1,"generation":"g1","acknowledged_empty":true,"panes":[]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	server.state.CommitInventory(nil, server.state.RevisionCounter())
	readyPayload, err := json.Marshal(map[string]any{
		"type": "workspace_create", "request_id": "admitted-after-ready", "protocol": protocol.Version,
		"cwd": root, "label": "Ready",
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := connection.Write(context.Background(), websocket.MessageText, readyPayload); err != nil {
		t.Fatal(err)
	}
	if admitted := readManagedMessage(t, connection, func(message map[string]any) bool {
		return message["request_id"] == "admitted-after-ready"
	}); admitted["error"] != nil {
		if apiError, ok := admitted["error"].(map[string]any); ok && apiError["code"] == protocol.ErrorManagedRuntimeNotReady {
			t.Fatalf("exact-ready runtime command remained blocked: %#v", admitted)
		}
	}
}

func TestManagedNotReadyPolicyPreservesRecoveryReadsAndSecurityControls(t *testing.T) {
	allowed := []string{
		"refresh_agents", "read_pane", "device_list", "create_device_invitation", "rename_device",
		"revoke_device", "reset_devices", "push_policy_set", "push_unsubscribe", "upload_cancel",
	}
	for _, action := range allowed {
		inbound, err := protocol.DecodeMap(map[string]any{"type": action, "protocol": protocol.Version})
		if err != nil {
			t.Fatalf("decode %s: %v", action, err)
		}
		scope, known := protocol.ScopeFor(inbound)
		if !known || !managedCommandAllowedWhileNotReady(scope.Action) {
			t.Errorf("%s was blocked while managed recovery is red", action)
		}
	}
	for _, action := range []string{"workspace_create", "workspace_close", "agent_start", "agent_stop", "submit_prompt", "send_secret", "worktree_remove", "upload_begin"} {
		inbound, err := protocol.DecodeMap(map[string]any{"type": action, "protocol": protocol.Version})
		if err != nil {
			t.Fatalf("decode %s: %v", action, err)
		}
		scope, known := protocol.ScopeFor(inbound)
		if !known || managedCommandAllowedWhileNotReady(scope.Action) {
			t.Errorf("%s escaped the managed runtime readiness gate", action)
		}
	}
}

func TestManagedCommandFenceHoldsTheSharedRuntimeLeaseUntilTheEffectFinishes(t *testing.T) {
	server, _, root := managedEffectFenceFixture(t)
	fence := server.managedCommandFence("leased", "send_text", "pane-1")
	if err := fence.guard(); err != nil {
		t.Fatal(err)
	}
	lockPath := filepath.Join(root, "active-runtime.lock")
	exclusive, err := os.OpenFile(lockPath, os.O_RDWR|syscall.O_NOFOLLOW, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer exclusive.Close()
	if err := syscall.Flock(int(exclusive.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); !errors.Is(err, syscall.EWOULDBLOCK) {
		t.Fatalf("exclusive promotion lock while effect is active = %v, want would-block", err)
	}
	if err := fence.close(); err != nil {
		t.Fatal(err)
	}
	if err := syscall.Flock(int(exclusive.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		t.Fatalf("exclusive promotion lock after effect = %v", err)
	}
	_ = syscall.Flock(int(exclusive.Fd()), syscall.LOCK_UN)
}

func TestManagedCommandFenceKeepsCompensationOnTheLeasedGeneration(t *testing.T) {
	server, writeActive, _ := managedEffectFenceFixture(t)
	fence := server.managedCommandFence("compensate", "copy_agent_response", "pane-1")
	if err := fence.guard(); err != nil {
		t.Fatal(err)
	}
	writeActive("g2")
	if err := fence.guard(); err != nil {
		t.Fatalf("compensation was fenced after the transaction started: %v", err)
	}
	if err := fence.close(); err != nil {
		t.Fatal(err)
	}
}

func TestManagedTopologyFenceStartsBeforeTheEffectAndFailsClosedAtCommit(t *testing.T) {
	marker := &fakeManagedTopologyMarker{}
	var order []string
	fence := newManagedCommandFence(func() *coordinator.CommandResult {
		order = append(order, "readiness")
		return nil
	})
	fence.lockTopology = func() func() {
		order = append(order, "lock")
		return func() { order = append(order, "unlock") }
	}
	fence.beginTopology = func() (*managedTopologyCommit, error) {
		order = append(order, "begin")
		return &managedTopologyCommit{
			target:     managedTopologyTarget{action: "agent_stop", paneID: "pane"},
			before:     managedTopologySnapshot{panes: []activeruntime.TopologyPane{{PaneID: "pane", NativeSessionID: "session", ProfileID: "personal"}}},
			marker:     marker,
			reconcile:  func(context.Context) error { order = append(order, "reconcile"); return errors.New("unavailable") },
			snapshot:   func() (managedTopologySnapshot, bool) { return managedTopologySnapshot{}, false },
			checkpoint: func(context.Context, bool) error { return nil },
			publish:    func([]readiness.Pane, bool) error { return nil },
		}, nil
	}
	fence.topologyFailure = func(error) *coordinator.CommandResult {
		return &coordinator.CommandResult{Action: "agent_stop", PaneID: "pane", Phase: "dispatched_unknown", Error: "Topology recovery checkpoint did not commit"}
	}
	if err := fence.guard(); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(order, []string{"lock", "readiness", "begin"}) {
		t.Fatalf("pre-effect order = %v", order)
	}
	result := fence.finalize(t.Context(), &coordinator.CommandResult{Action: "agent_stop", PaneID: "pane", OK: true, Phase: "completed"})
	if result.OK || result.Phase != "dispatched_unknown" || marker.resolves != 0 {
		t.Fatalf("commit failure result = %+v, resolves = %d", result, marker.resolves)
	}
	if err := fence.close(); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(order, []string{"lock", "readiness", "begin", "reconcile", "unlock"}) {
		t.Fatalf("transaction order = %v", order)
	}
}

func TestManagedTopologyFenceRejectsMissingCheckpointHelperBeforeTheEffect(t *testing.T) {
	server, _, root := managedEffectFenceFixture(t)
	fence := server.managedCommandFence("blocked", "agent_stop", "pane-1", managedTopologyTarget{action: "agent_stop", paneID: "pane-1"})
	if err := fence.guard(); err == nil {
		t.Fatal("topology effect was admitted without a recovery checkpoint helper")
	}
	if fence.blocked == nil || fence.blocked.Phase != "not_started" {
		t.Fatalf("missing helper result = %+v", fence.blocked)
	}
	if _, err := os.Lstat(filepath.Join(root, activeruntime.TopologyTransactionName)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("missing helper left a topology marker: %v", err)
	}
	if err := fence.close(); err != nil {
		t.Fatal(err)
	}
}

func TestManagedCommandFenceCoversEveryLifecycleState(t *testing.T) {
	var nilFence *managedCommandFence
	result := &coordinator.CommandResult{Action: "noop", OK: true}
	if nilFence.check() != nil || nilFence.guard() != nil || nilFence.finalize(t.Context(), result) != result || nilFence.close() != nil || nilFence.guardFunc() != nil {
		t.Fatal("nil managed command fence was not inert")
	}

	ready := newManagedCommandFence(func() *coordinator.CommandResult { return nil })
	if ready.check() != nil || ready.guardFunc() == nil {
		t.Fatal("ready managed command fence did not admit the effect")
	}
	if got := ready.finalize(t.Context(), nil); got != nil {
		t.Fatalf("nil final result = %+v", got)
	}
	second := &coordinator.CommandResult{Action: "second"}
	if got := ready.finalize(t.Context(), second); got != second {
		t.Fatalf("second finalization after nil = %+v", got)
	}
	if err := ready.close(); err != nil {
		t.Fatal(err)
	}
	if err := ready.close(); err != nil {
		t.Fatal(err)
	}
	if err := ready.guard(); err == nil {
		t.Fatal("closed managed command fence admitted another effect")
	}

	blockedResult := &coordinator.CommandResult{Action: "blocked", Phase: "not_started"}
	blocked := newManagedCommandFence(func() *coordinator.CommandResult { return blockedResult })
	if blocked.check() != blockedResult || blocked.check() != blockedResult {
		t.Fatal("blocked managed command fence lost its first result")
	}
	if got := blocked.finalize(t.Context(), result); got != result {
		t.Fatalf("blocked fence finalization = %+v", got)
	}
	if got := blocked.finalize(t.Context(), second); got != result {
		t.Fatalf("repeated finalization replaced result = %+v", got)
	}
	if err := blocked.close(); err != nil {
		t.Fatal(err)
	}

	fallback := newManagedCommandFence(func() *coordinator.CommandResult { return nil })
	fallback.beginTopology = func() (*managedTopologyCommit, error) { return nil, errors.New("begin") }
	fallback.topologyFailure = func(error) *coordinator.CommandResult { return blockedResult }
	if err := fallback.guard(); err == nil || fallback.blocked != blockedResult {
		t.Fatalf("fallback begin failure = (%v, %+v)", err, fallback.blocked)
	}
	if err := fallback.close(); err != nil {
		t.Fatal(err)
	}

	server := testServer()
	if server.sendBlockedManagedCommand(nil, nil, nil) || server.sendBlockedManagedCommand(nil, nil, newManagedCommandFence(func() *coordinator.CommandResult { return nil })) {
		t.Fatal("empty blocked command was sent")
	}
}

func TestManagedCommandFenceFactoryFailsClosedAtLeaseBaselineMarkerAndCommitBoundaries(t *testing.T) {
	server, _, root := managedEffectFenceFixture(t)
	configureManagedCheckpointFixture(t, server, root)

	originalActivePath := server.cfg.ActiveRuntimePath
	server.cfg.ActiveRuntimePath = "relative/active-runtime.json"
	leaseFence := server.managedCommandFence("lease", "send_text", "pane-1")
	if err := leaseFence.guard(); err == nil || leaseFence.blocked == nil || leaseFence.blocked.Phase != "not_started" {
		t.Fatalf("lease failure = (%v, %+v)", err, leaseFence.blocked)
	}
	if err := leaseFence.close(); err != nil {
		t.Fatal(err)
	}
	server.cfg.ActiveRuntimePath = originalActivePath

	server.state.MarkInventoryFailure(errors.New("unavailable"))
	baselineFence := server.managedCommandFence("baseline", "agent_stop", "pane-1")
	baselineFence.checkReadiness = func() *coordinator.CommandResult { return nil }
	if err := baselineFence.guard(); err == nil || baselineFence.blocked == nil || baselineFence.blocked.Phase != "not_started" {
		t.Fatalf("baseline failure = (%v, %+v)", err, baselineFence.blocked)
	}
	if err := baselineFence.close(); err != nil {
		t.Fatal(err)
	}
	server.state.CommitInventory([]*coordinator.AgentState{{PaneID: "pane-1", SessionID: "session-1", ProfileID: "personal"}}, server.state.RevisionCounter())

	markerPath := filepath.Join(root, activeruntime.TopologyTransactionName)
	if err := os.WriteFile(markerPath, []byte("pending\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	markerFence := server.managedCommandFence("marker", "agent_stop", "pane-1")
	markerFence.checkReadiness = func() *coordinator.CommandResult { return nil }
	if err := markerFence.guard(); err == nil || markerFence.blocked == nil || markerFence.blocked.Phase != "not_started" {
		t.Fatalf("marker failure = (%v, %+v)", err, markerFence.blocked)
	}
	if err := markerFence.close(); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(markerPath); err != nil {
		t.Fatal(err)
	}

	commitFence := server.managedCommandFence("commit", "agent_stop", "pane-1")
	commitFence.checkReadiness = func() *coordinator.CommandResult { return nil }
	commitFence.beginTopology = func() (*managedTopologyCommit, error) {
		return &managedTopologyCommit{
			target: managedTopologyTarget{action: "agent_stop", paneID: "pane-1"}, marker: &fakeManagedTopologyMarker{},
			reconcile:  func(context.Context) error { return errors.New("reconcile") },
			snapshot:   func() (managedTopologySnapshot, bool) { return managedTopologySnapshot{}, false },
			checkpoint: func(context.Context, bool) error { return nil }, publish: func([]readiness.Pane, bool) error { return nil },
		}, nil
	}
	if err := commitFence.guard(); err != nil {
		t.Fatal(err)
	}
	committed := commitFence.finalize(t.Context(), &coordinator.CommandResult{Action: "agent_stop", OK: true, Phase: "completed"})
	if committed == nil || committed.Phase != "dispatched_unknown" {
		t.Fatalf("commit boundary result = %+v", committed)
	}
	if err := commitFence.close(); err != nil {
		t.Fatal(err)
	}
}

func configureManagedCheckpointFixture(t *testing.T, server *Server, root string) {
	t.Helper()
	base := filepath.Dir(root)
	helper := filepath.Join(base, "OuroWorkbenchRemote")
	if err := os.WriteFile(helper, []byte("#!/bin/sh\nexit 0\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	server.cfg.TopologyCommitHelper = helper
	server.cfg.TopologyRemoteConfig = filepath.Join(base, "profiles.json")
	server.cfg.TopologyLedgerRoot = filepath.Join(base, "ledger")
	server.cfg.TopologySessionMap = filepath.Join(base, "session-map.json")
	server.cfg.TopologyShimDirectory = filepath.Join(base, "shims")
	server.cfg.TopologyZDOTDir = filepath.Join(base, "zdotdir")
}

func TestManagedCopyRechecksPromotionAfterCopyLock(t *testing.T) {
	server, writeActive, _ := managedEffectFenceFixture(t)
	server.clipboardRead = func(context.Context) ([]byte, error) { return []byte("before"), nil }
	server.clipboardWrite = func(context.Context, []byte) error { return nil }
	runnerCalled := atomic.Bool{}
	server.copyRunner = func(
		context.Context,
		string,
		slashcmd.CopyProfile,
		copyresponse.Pane,
		copyresponse.ClipboardReader,
		copyresponse.ClipboardWriter,
		int64,
		copyresponse.RevisionReader,
		copyresponse.MutationGuard,
	) (copyresponse.Result, error) {
		runnerCalled.Store(true)
		return copyresponse.Result{}, nil
	}
	fence := newManagedCommandFence(server.managedExecutionFence("copy-promoted", "copy_agent_response", "pane-1"))
	started := make(chan struct{})
	var startedOnce sync.Once
	server.hub.SetHandler(func(client *transport.ClientConn, message map[string]any, admitted func()) {
		defer admitted()
		startedOnce.Do(func() { close(started) })
		server.copyAgentResponse(client, "copy-promoted", "pane-1", fence)
	})
	httpServer := httptest.NewServer(http.HandlerFunc(server.hub.HandleWebSocket))
	defer httpServer.Close()
	connection, _, err := websocket.Dial(context.Background(), "ws"+strings.TrimPrefix(httpServer.URL, "http"), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer connection.CloseNow()

	server.copyMu.Lock()
	locked := true
	defer func() {
		if locked {
			server.copyMu.Unlock()
		}
		shutdownCtx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_ = server.hub.Shutdown(shutdownCtx)
	}()
	payload, err := json.Marshal(map[string]any{"type": "copy_agent_response", "request_id": "copy-promoted", "pane_id": "pane-1"})
	if err != nil {
		t.Fatal(err)
	}
	if err := connection.Write(context.Background(), websocket.MessageText, payload); err != nil {
		t.Fatal(err)
	}
	<-started
	writeActive("g2")
	server.copyMu.Unlock()
	locked = false
	result := readManagedMessage(t, connection, func(message map[string]any) bool { return message["request_id"] == "copy-promoted" })
	if result["error"] != "Managed runtime changed before execution" || runnerCalled.Load() {
		t.Fatalf("promoted copy result = %#v, runner called=%t", result, runnerCalled.Load())
	}
}

func TestManagedUploadRechecksPromotionImmediatelyBeforeBegin(t *testing.T) {
	server, writeActive, root := managedEffectFenceFixture(t)
	fence := newManagedCommandFence(server.managedExecutionFence("upload-promoted", "upload_begin", "pane-1"))
	started := make(chan struct{})
	release := make(chan struct{})
	server.hub.SetHandler(func(client *transport.ClientConn, message map[string]any, admitted func()) {
		defer admitted()
		close(started)
		<-release
		server.handleUploadBegin(client, "upload-promoted", message, fence)
	})
	httpServer := httptest.NewServer(http.HandlerFunc(server.hub.HandleWebSocket))
	defer httpServer.Close()
	connection, _, err := websocket.Dial(context.Background(), "ws"+strings.TrimPrefix(httpServer.URL, "http"), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer connection.CloseNow()
	defer func() {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_ = server.hub.Shutdown(shutdownCtx)
	}()
	payload, err := json.Marshal(map[string]any{
		"type": "upload_begin", "request_id": "upload-promoted",
		"target": map[string]any{
			"server_session_id": "primary", "pane_id": "pane-1", "terminal_id": "terminal-1", "generation": 1, "agent_session_id": "session-1",
		},
		"files": []map[string]any{{"name": "note.txt", "media_type": "text/plain", "bytes": 1}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := connection.Write(context.Background(), websocket.MessageText, payload); err != nil {
		t.Fatal(err)
	}
	<-started
	writeActive("g2")
	close(release)
	result := readManagedMessage(t, connection, func(message map[string]any) bool { return message["request_id"] == "upload-promoted" })
	if result["error"] != "Managed runtime changed before execution" {
		t.Fatalf("promoted upload result = %#v", result)
	}
	entries, err := os.ReadDir(filepath.Join(root, "cache", "uploads", "sessions"))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("promoted upload created staging directories: %v", entries)
	}
}

func managedEffectFenceFixture(t *testing.T) (*Server, func(string), string) {
	t.Helper()
	root := filepath.Join(t.TempDir(), "herdr")
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatal(err)
	}
	sessionRoot := filepath.Join(root, "sessions", "g1")
	if err := os.MkdirAll(sessionRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	activePath := filepath.Join(root, "active-runtime.json")
	expectedPath := filepath.Join(sessionRoot, "expected-inventory.json")
	socketPath := filepath.Join(sessionRoot, "herdr.sock")
	writeActive := func(generation string) {
		t.Helper()
		generationRoot := filepath.Join(root, "sessions", generation)
		data := fmt.Sprintf(`{"schemaVersion":1,"generation":%q,"sessionName":%q,"socketPath":%q,"expectedInventoryPath":%q}`,
			generation, generation, filepath.Join(generationRoot, "herdr.sock"), filepath.Join(generationRoot, "expected-inventory.json"))
		temporary := filepath.Join(root, ".active-runtime-effect-test")
		if err := os.WriteFile(temporary, []byte(data), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Rename(temporary, activePath); err != nil {
			t.Fatal(err)
		}
	}
	writeActive("g1")
	if err := os.WriteFile(expectedPath, []byte(`{"version":1,"generation":"g1","panes":[{"pane_id":"pane-1","native_session_id":"session-1","profile_id":"personal"}]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	server := New(&config.Config{
		ManagedDeployment: true, ActiveRuntimePath: activePath, ActiveGeneration: "g1", ExpectedInventoryPath: expectedPath,
		SocketPath: socketPath, RuntimeDir: filepath.Join(root, "runtime"), CacheDir: filepath.Join(root, "cache"),
	}, "test", "revision", slog.New(slog.NewTextHandler(io.Discard, nil)))
	server.state.CommitInventory([]*coordinator.AgentState{{
		PaneID: "pane-1", TerminalID: "terminal-1", Generation: 1, SessionID: "session-1", ProfileID: "personal", Agent: "claude", Status: "idle", PaneRevision: 1,
	}}, server.state.RevisionCounter())
	server.profiles.Remember("pane-1", "claude")
	return server, writeActive, root
}

func TestDeploymentCapabilities(t *testing.T) {
	if got := removeCapability(nil, "self_update"); len(got) != 0 {
		t.Fatalf("empty capabilities = %v", got)
	}
	base := []string{"self_update", "other"}
	managed := deploymentCapabilities(base, true, true)
	if slices.Contains(managed, "self_update") || slices.Contains(managed, "app_deploy") {
		t.Fatalf("managed capabilities = %v", managed)
	}
	if !slices.Equal(base, []string{"self_update", "other"}) {
		t.Fatalf("input capabilities mutated: %v", base)
	}
	unmanaged := deploymentCapabilities(base, false, true)
	if !slices.Contains(unmanaged, "self_update") || !slices.Contains(unmanaged, "app_deploy") {
		t.Fatalf("unmanaged capabilities = %v", unmanaged)
	}
	plain := deploymentCapabilities(base, false, false)
	if slices.Contains(plain, "app_deploy") {
		t.Fatalf("unconfigured app deploy advertised: %v", plain)
	}
}

func TestManagedReadinessRequiresExactGenerationInventory(t *testing.T) {
	root := t.TempDir()
	if err := os.Chmod(root, 0o700); err != nil {
		t.Fatal(err)
	}
	sessionRoot := filepath.Join(root, "sessions", "g1")
	if err := os.MkdirAll(sessionRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	expectedPath := filepath.Join(sessionRoot, "expected-inventory.json")
	activePath := filepath.Join(root, "active-runtime.json")
	writeActiveRuntime := func(generation string) {
		t.Helper()
		activeSessionRoot := filepath.Join(root, "sessions", generation)
		data := fmt.Sprintf(`{"schemaVersion":1,"generation":%q,"sessionName":%q,"socketPath":%q,"expectedInventoryPath":%q}`,
			generation, generation, filepath.Join(activeSessionRoot, "herdr.sock"), filepath.Join(activeSessionRoot, "expected-inventory.json"))
		temporary := filepath.Join(root, ".active-runtime-test")
		if err := os.WriteFile(temporary, []byte(data), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Rename(temporary, activePath); err != nil {
			t.Fatal(err)
		}
	}
	writeActiveRuntime("g1")
	writeExpected := func(contents string) {
		t.Helper()
		if err := os.WriteFile(expectedPath, []byte(contents), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	writeExpected(`{"version":1,"generation":"g1","panes":[{"pane_id":"pane-1","native_session_id":"session-1","profile_id":"personal"}]}`)
	server := New(&config.Config{
		ManagedDeployment: true, ExpectedInventoryPath: expectedPath, ActiveGeneration: "g1", InstanceID: "relay-1",
		ActiveRuntimePath: activePath, SocketPath: filepath.Join(sessionRoot, "herdr.sock"),
		RuntimeDir: filepath.Join(root, "runtime"), CacheDir: filepath.Join(root, "cache"),
	}, "0.20.8", "revision", slog.New(slog.NewTextHandler(io.Discard, nil)))
	server.ready = true
	server.state.CommitInventory([]*coordinator.AgentState{{
		PaneID: "pane-1", SessionID: "session-1", ProfileID: "personal", Status: "idle",
	}}, server.state.RevisionCounter())
	response := httptest.NewRecorder()
	server.handleReadyz(response, httptest.NewRequest("GET", "/readyz", nil))
	if response.Code != http.StatusServiceUnavailable || !strings.Contains(response.Body.String(), `"bundle_hash":""`) {
		t.Fatalf("managed readiness accepted a missing web bundle = %d %s", response.Code, response.Body.String())
	}
	webHandler := attachTestWebBundle(t, server)
	fence := server.managedExecutionFence("fenced", "workspace_create", "")
	if result := fence(); result != nil {
		t.Fatalf("exact generation fence = %+v", result)
	}

	response = httptest.NewRecorder()
	server.handleReadyz(response, httptest.NewRequest("GET", "/readyz", nil))
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), `"status":"ready"`) ||
		!strings.Contains(response.Body.String(), `"instance":"relay-1"`) || !strings.Contains(response.Body.String(), `"revision":"revision"`) ||
		!strings.Contains(response.Body.String(), `"release_version":"0.20.8"`) || !strings.Contains(response.Body.String(), `"bundle_hash":"`+webHandler.BundleHash()+`"`) ||
		!strings.Contains(response.Body.String(), `"generation":"g1"`) {
		t.Fatalf("exact readiness = %d %s", response.Code, response.Body.String())
	}
	marker := filepath.Join(root, activeruntime.TopologyTransactionName)
	if err := os.WriteFile(marker, []byte(`{"schema_version":1}`), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Run("readiness responds during topology fence", func(t *testing.T) {
		server.managedTopologyMu.Lock()
		locked := true
		defer func() {
			if locked {
				server.managedTopologyMu.Unlock()
			}
		}()

		type endpointResult struct {
			name string
			code int
			body string
		}
		responses := make(chan endpointResult, 2)
		for name, handler := range map[string]http.HandlerFunc{
			"healthz": server.handleHealthz,
			"readyz":  server.handleReadyz,
		} {
			go func() {
				response := httptest.NewRecorder()
				handler(response, httptest.NewRequest(http.MethodGet, "/"+name, nil))
				responses <- endpointResult{name: name, code: response.Code, body: response.Body.String()}
			}()
		}

		for range 2 {
			select {
			case response := <-responses:
				switch response.name {
				case "readyz":
					if response.code != http.StatusServiceUnavailable || !strings.Contains(response.body, `"state":"topology_transaction_pending"`) {
						t.Fatalf("readyz during topology fence = %d %s", response.code, response.body)
					}
				case "healthz":
					if response.code != http.StatusOK || !strings.Contains(response.body, `"readiness":"blocked"`) ||
						!strings.Contains(response.body, `"state":"topology_transaction_pending"`) {
						t.Fatalf("healthz during topology fence = %d %s", response.code, response.body)
					}
				}
			case <-time.After(500 * time.Millisecond):
				server.managedTopologyMu.Unlock()
				locked = false
				t.Fatal("readiness endpoint blocked behind the topology fence")
			}
		}
		server.managedTopologyMu.Unlock()
		locked = false
	})
	if result := fence(); result == nil || result.Data.(map[string]any)["state"] != readiness.StateTopologyTransactionPending {
		t.Fatalf("unresolved topology transaction fence = %+v", result)
	}
	response = httptest.NewRecorder()
	server.handleReadyz(response, httptest.NewRequest("GET", "/readyz", nil))
	if response.Code != http.StatusServiceUnavailable || !strings.Contains(response.Body.String(), `"state":"topology_transaction_pending"`) {
		t.Fatalf("unresolved topology transaction readiness = %d %s", response.Code, response.Body.String())
	}
	if err := os.Remove(marker); err != nil {
		t.Fatal(err)
	}

	writeActiveRuntime("g2")
	if result := fence(); result == nil || result.Error != "Managed runtime changed before execution" {
		t.Fatalf("promoted generation fence = %+v", result)
	}
	response = httptest.NewRecorder()
	server.handleReadyz(response, httptest.NewRequest("GET", "/readyz", nil))
	if response.Code != http.StatusServiceUnavailable || !strings.Contains(response.Body.String(), `"state":"active_runtime_mismatch"`) {
		t.Fatalf("old relay accepted promoted runtime = %d %s", response.Code, response.Body.String())
	}
	writeActiveRuntime("g1")
	if err := os.Chmod(activePath, 0o640); err != nil {
		t.Fatal(err)
	}
	response = httptest.NewRecorder()
	server.handleReadyz(response, httptest.NewRequest("GET", "/readyz", nil))
	if response.Code != http.StatusServiceUnavailable || !strings.Contains(response.Body.String(), `"state":"invalid_active_runtime"`) {
		t.Fatalf("old relay accepted unsafe active runtime = %d %s", response.Code, response.Body.String())
	}
	if err := os.Chmod(activePath, 0o600); err != nil {
		t.Fatal(err)
	}

	server.state.CommitInventory(nil, server.state.RevisionCounter())
	response = httptest.NewRecorder()
	server.handleReadyz(response, httptest.NewRequest("GET", "/readyz", nil))
	if response.Code != http.StatusServiceUnavailable || !strings.Contains(response.Body.String(), `"state":"unexpected_empty"`) {
		t.Fatalf("collapsed readiness = %d %s", response.Code, response.Body.String())
	}
	health := httptest.NewRecorder()
	server.handleHealthz(health, httptest.NewRequest("GET", "/healthz", nil))
	if !strings.Contains(health.Body.String(), `"readiness":"blocked"`) {
		t.Fatalf("health accepted collapsed managed inventory: %s", health.Body.String())
	}

	writeExpected(`{"version":1,"generation":"g1","acknowledged_empty":true,"panes":[]}`)
	response = httptest.NewRecorder()
	server.handleReadyz(response, httptest.NewRequest("GET", "/readyz", nil))
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), `"status":"acknowledged_empty"`) {
		t.Fatalf("acknowledged-empty readiness = %d %s", response.Code, response.Body.String())
	}
	health = httptest.NewRecorder()
	server.handleHealthz(health, httptest.NewRequest("GET", "/healthz", nil))
	if !strings.Contains(health.Body.String(), `"readiness":"acknowledged_empty"`) {
		t.Fatalf("health collapsed acknowledged-empty state: %s", health.Body.String())
	}
	server.state.MarkInventoryFailure(fmt.Errorf("fixture inventory failure"))
	response = httptest.NewRecorder()
	server.handleReadyz(response, httptest.NewRequest("GET", "/readyz", nil))
	if response.Code != http.StatusServiceUnavailable || !strings.Contains(response.Body.String(), `"state":"unavailable"`) {
		t.Fatalf("readyz reused stale exact inventory after poll failure: %d %s", response.Code, response.Body.String())
	}
	if result := fence(); result == nil || result.Data.(map[string]any)["state"] != readiness.StateUnavailable {
		t.Fatalf("managed fence reused stale exact inventory after poll failure: %+v", result)
	}
	health = httptest.NewRecorder()
	server.handleHealthz(health, httptest.NewRequest("GET", "/healthz", nil))
	if !strings.Contains(health.Body.String(), `"readiness":"degraded"`) {
		t.Fatalf("health hid inventory degradation: %s", health.Body.String())
	}
}

func TestServerReconcilesPersistedProfileOwnership(t *testing.T) {
	root := t.TempDir()
	configHome := filepath.Join(root, "config")
	binDir := filepath.Join(root, "bin")
	if err := os.MkdirAll(filepath.Join(configHome, "herdr"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(binDir, 0o700); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"personal", "emu"} {
		if err := os.WriteFile(filepath.Join(binDir, name), []byte("#!/bin/sh\nexit 0\n"), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(configHome, "herdr", "agent-profiles.ini"), []byte("[config]\nreplace_profiles = true\n[profiles]\npersonal = Personal\nemu = EMU\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", binDir)
	stateDir := filepath.Join(root, "state")
	newServer := func() *Server {
		return New(&config.Config{
			ConfigHome: configHome, RuntimeDir: stateDir, CacheDir: filepath.Join(root, "cache"),
			ReleaseRoot: filepath.Join(root, "release"), HerdrBin: "/bin/false",
		}, "test", "revision", slog.New(slog.NewTextHandler(io.Discard, nil)))
	}
	first := newServer()
	first.profiles.Remember("pane-personal", "personal")
	agents := []*coordinator.AgentState{
		{PaneID: "pane-personal", Agent: "copilot", Session: "display-title", SessionID: "session-personal"},
		{PaneID: "pane-unknown", Agent: "copilot"},
	}
	first.reconcileProfileOwnership(agents)
	if agents[0].ProfileID != "personal" || agents[1].ProfileID != "" {
		t.Fatalf("first ownership = %q, %q", agents[0].ProfileID, agents[1].ProfileID)
	}
	if agents[0].SessionID != "session-personal" || agents[0].Session != "display-title" {
		t.Fatalf("ownership reconciliation rewrote resolved session identity: %+v", agents[0])
	}
	restarted := newServer()
	reloaded := []*coordinator.AgentState{{PaneID: "pane-personal", Agent: "copilot"}}
	restarted.reconcileProfileOwnership(reloaded)
	if reloaded[0].ProfileID != "" {
		t.Fatalf("temporarily unverifiable ownership = %q", reloaded[0].ProfileID)
	}
	reloaded[0].SessionID = "session-personal"
	restarted.reconcileProfileOwnership(reloaded)
	if reloaded[0].ProfileID != "personal" {
		t.Fatalf("reloaded ownership = %q", reloaded[0].ProfileID)
	}
}

func TestServerRecordsProfileAssociationFailure(t *testing.T) {
	root := t.TempDir()
	statePath := filepath.Join(root, "not-a-directory")
	if err := os.WriteFile(statePath, []byte("file"), 0o600); err != nil {
		t.Fatal(err)
	}
	server := New(&config.Config{
		RuntimeDir: statePath, CacheDir: filepath.Join(root, "cache"), HerdrBin: "/bin/false",
	}, "test", "revision", slog.New(slog.NewTextHandler(io.Discard, nil)))
	agent := &coordinator.AgentState{PaneID: "pane", SessionID: "session", ProfileID: "personal"}
	server.reconcileProfileOwnership([]*coordinator.AgentState{agent})
	if len(server.recentSafeErrors()) == 0 {
		t.Fatal("association failure was not recorded")
	}
	if agent.ProfileID != "" {
		t.Fatalf("failed ownership reconciliation retained profile %q", agent.ProfileID)
	}
	server.reconcileProfileOwnership(nil)
}

func TestServerPollReconcilesProfileOwnershipBeforeCommittingInventory(t *testing.T) {
	root := t.TempDir()
	fakeHerdr := filepath.Join(root, "herdr")
	if err := os.WriteFile(fakeHerdr, []byte(`#!/bin/sh
case "$*" in
  "agent list") printf '%s\n' '{"result":{"agents":[]}}' ;;
  "workspace list") printf '%s\n' '{"result":{"workspaces":[]}}' ;;
  "tab list") printf '%s\n' '{"result":{"tabs":[]}}' ;;
  "pane list") printf '%s\n' '{"result":{"panes":[]}}' ;;
	"integration status") exit 0 ;;
  *) exit 1 ;;
esac
`), 0o700); err != nil {
		t.Fatal(err)
	}
	webRoot := filepath.Join(root, "web")
	if err := os.MkdirAll(webRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(webRoot, "index.html"), []byte("ok"), 0o600); err != nil {
		t.Fatal(err)
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := listener.Addr().(*net.TCPAddr).Port
	listener.Close()
	server := New(&config.Config{
		Host: "127.0.0.1", Port: port, InstanceID: "poll-reconcile", HerdrBin: fakeHerdr,
		WebRoot: webRoot, RuntimeDir: filepath.Join(root, "runtime"), CacheDir: filepath.Join(root, "cache"),
		ConfigHome: filepath.Join(root, "config"), ReleaseRoot: filepath.Join(root, "release"), PollInterval: 60,
	}, "test", "revision", slog.New(slog.NewTextHandler(io.Discard, nil)))
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- server.Run(ctx) }()
	deadline := time.Now().Add(3 * time.Second)
	for server.state.InventoryStatus()["state"] != "ready" && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if state := server.state.InventoryStatus()["state"]; state != "ready" {
		t.Fatalf("inventory state = %v", state)
	}
}

func readManagedMessage(t *testing.T, connection *websocket.Conn, accept func(map[string]any) bool) map[string]any {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	for {
		_, data, err := connection.Read(ctx)
		if err != nil {
			t.Fatalf("read managed relay message: %v", err)
		}
		var message map[string]any
		if err := json.Unmarshal(data, &message); err != nil {
			t.Fatal(err)
		}
		if accept(message) {
			return message
		}
	}
}
