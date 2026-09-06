package profiles

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestAssociationReconcileHandlesEmptyConfigurationAndStore(t *testing.T) {
	resolver := NewResolver(t.TempDir(), nil)
	resolver.cached = []Profile{}
	resolver.expires = time.Now().Add(time.Minute)
	resolver.Remember("pane", "missing")
	if err := resolver.Reconcile(nil); err != nil {
		t.Fatal(err)
	}
	if len(resolver.remembered) != 0 {
		t.Fatalf("unconfigured remembered profile survived: %v", resolver.remembered)
	}

	stateDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(stateDir, associationStoreName), []byte(`{"version":1,"associations":[]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	loaded := NewResolver(t.TempDir(), nil, WithAssociationStore(stateDir))
	loaded.cached = []Profile{}
	loaded.expires = time.Now().Add(time.Minute)
	if err := loaded.Reconcile(nil); err != nil {
		t.Fatal(err)
	}
}

func TestAssociationReconcileRetainsLivePendingIntentUntilPaneAppears(t *testing.T) {
	configHome, stateDir := configuredCustomProfiles(t)
	resolver := NewResolver(configHome, nil, WithAssociationStore(stateDir))
	if err := resolver.Remember("pane", "personal"); err != nil {
		t.Fatal(err)
	}
	if err := resolver.Reconcile(nil); err != nil {
		t.Fatal(err)
	}
	if got := resolver.ResolvePane("pane", "copilot"); got != "personal" {
		t.Fatalf("pending ownership = %q, want personal", got)
	}
}

func TestCustomPaneAssociationsSurviveResolverRestart(t *testing.T) {
	configHome, stateDir := configuredCustomProfiles(t)
	observed := []Observation{
		{PaneID: "pane-personal", NativeSessionID: "session-personal"},
		{PaneID: "pane-emu", NativeSessionID: "session-emu"},
	}

	first := NewResolver(configHome, nil, WithAssociationStore(stateDir))
	first.Remember("pane-personal", "personal")
	first.Remember("pane-emu", "emu")
	if err := first.Reconcile(observed); err != nil {
		t.Fatalf("persist associations: %v", err)
	}

	restarted := NewResolver(configHome, nil, WithAssociationStore(stateDir))
	if err := restarted.Reconcile(observed); err != nil {
		t.Fatalf("reload associations: %v", err)
	}
	if got := restarted.ResolvePane("pane-personal", "copilot"); got != "personal" {
		t.Fatalf("personal pane resolved to %q", got)
	}
	if got := restarted.ResolvePane("pane-emu", "copilot"); got != "emu" {
		t.Fatalf("EMU pane resolved to %q", got)
	}
	info, err := os.Stat(filepath.Join(stateDir, associationStoreName))
	if err != nil {
		t.Fatalf("stat association store: %v", err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("association store mode = %o, want 600", info.Mode().Perm())
	}
}

func TestPendingAssociationAfterCrashNeverAuthorizesPaneIDReuse(t *testing.T) {
	configHome, stateDir := configuredCustomProfiles(t)
	started := NewResolver(configHome, nil, WithAssociationStore(stateDir))
	if err := started.Remember("pane-new", "personal"); err != nil {
		t.Fatalf("persist start intent: %v", err)
	}
	stored := readAssociationStore(t, stateDir)
	if len(stored.Associations) != 0 || len(stored.Pending) != 1 || stored.Pending[0] != (pendingAssociation{PaneID: "pane-new", ProfileID: "personal"}) {
		t.Fatalf("post-start store = %+v", stored)
	}

	restarted := NewResolver(configHome, nil, WithAssociationStore(stateDir))
	if err := restarted.Reconcile([]Observation{{PaneID: "pane-new", NativeSessionID: "session-new"}}); err != nil {
		t.Fatalf("reconcile after restart: %v", err)
	}
	if got := restarted.ResolvePaneSession("pane-new", "session-new", "copilot"); got != "" {
		t.Fatalf("crash-persisted intent authorized a reused pane as %q", got)
	}
	stored = readAssociationStore(t, stateDir)
	if len(stored.Pending) != 1 || stored.Pending[0] != (pendingAssociation{PaneID: "pane-new", ProfileID: "personal"}) || len(stored.Associations) != 0 {
		t.Fatalf("untrusted crash intent was promoted or lost: %+v", stored)
	}
}

func TestReplacementOwnershipSurvivesCrashesAtStartStopAndReconcileBoundaries(t *testing.T) {
	configHome, stateDir := configuredCustomProfiles(t)
	initial := NewResolver(configHome, nil, WithAssociationStore(stateDir))
	if err := initial.Remember("pane-old", "personal"); err != nil {
		t.Fatal(err)
	}
	if err := initial.Reconcile([]Observation{{PaneID: "pane-old", NativeSessionID: "session-old"}}); err != nil {
		t.Fatal(err)
	}

	if err := initial.Remember("pane-new", "personal"); err != nil {
		t.Fatalf("persist replacement start: %v", err)
	}
	stored := readAssociationStore(t, stateDir)
	if len(stored.Associations) != 1 || stored.Associations[0].PaneID != "pane-old" || len(stored.Pending) != 1 || stored.Pending[0].PaneID != "pane-new" {
		t.Fatalf("replacement-start boundary = %+v", stored)
	}

	afterStartCrash := NewResolver(configHome, nil, WithAssociationStore(stateDir))
	if err := afterStartCrash.Forget("pane-old"); err != nil {
		t.Fatalf("persist old-pane stop: %v", err)
	}
	stored = readAssociationStore(t, stateDir)
	if len(stored.Associations) != 0 || len(stored.Pending) != 1 || stored.Pending[0].PaneID != "pane-new" {
		t.Fatalf("old-stop boundary = %+v", stored)
	}

	afterStopCrash := NewResolver(configHome, nil, WithAssociationStore(stateDir))
	if got := afterStopCrash.ResolvePane("pane-new", "copilot"); got != "" {
		t.Fatalf("unverified pending ownership resolved as %q before reconciliation", got)
	}
	if err := afterStopCrash.Reconcile([]Observation{{PaneID: "pane-new", NativeSessionID: "session-new"}}); err != nil {
		t.Fatalf("reconcile replacement after restart: %v", err)
	}
	if got := afterStopCrash.ResolvePaneSession("pane-new", "session-new", "copilot"); got != "" {
		t.Fatalf("crash-persisted replacement intent authorized %q", got)
	}
	stored = readAssociationStore(t, stateDir)
	if len(stored.Pending) != 1 || stored.Pending[0].PaneID != "pane-new" || len(stored.Associations) != 0 {
		t.Fatalf("post-reconcile crash intent = %+v", stored)
	}
}

func TestAssociationReconcileRejectsConflictAndPrunesStaleEntries(t *testing.T) {
	configHome, stateDir := configuredCustomProfiles(t)
	store := filepath.Join(stateDir, associationStoreName)
	if err := os.MkdirAll(stateDir, 0o700); err != nil {
		t.Fatal(err)
	}
	data := []byte(`{"version":1,"associations":[{"pane_id":"pane-live","native_session_id":"session-live","profile_id":"personal"},{"pane_id":"pane-live","native_session_id":"session-live","profile_id":"emu"},{"pane_id":"pane-stale","native_session_id":"session-stale","profile_id":"personal"}]}`)
	if err := os.WriteFile(store, data, 0o600); err != nil {
		t.Fatal(err)
	}

	resolver := NewResolver(configHome, nil, WithAssociationStore(stateDir))
	err := resolver.Reconcile([]Observation{{PaneID: "pane-live", NativeSessionID: "session-live"}})
	if err == nil {
		t.Fatal("conflicting association store was accepted")
	}
	if got := resolver.ResolvePane("pane-live", "copilot"); got != "" {
		t.Fatalf("conflicting pane resolved to %q, want unknown", got)
	}
	if got := resolver.ResolvePane("pane-stale", "copilot"); got != "" {
		t.Fatalf("stale pane resolved to %q, want unknown", got)
	}
}

func TestAssociationRequiresExactNativeSessionAndConfiguredProfile(t *testing.T) {
	configHome, stateDir := configuredCustomProfiles(t)
	resolver := NewResolver(configHome, nil, WithAssociationStore(stateDir))
	resolver.Remember("pane", "personal")
	if err := resolver.Reconcile([]Observation{{PaneID: "pane", NativeSessionID: "session-one"}}); err != nil {
		t.Fatal(err)
	}

	restarted := NewResolver(configHome, nil, WithAssociationStore(stateDir))
	if err := restarted.Reconcile([]Observation{{PaneID: "pane", NativeSessionID: "session-two"}}); err != nil {
		t.Fatal(err)
	}
	if got := restarted.ResolvePane("pane", "copilot"); got != "" {
		t.Fatalf("changed native session resolved to %q, want unknown", got)
	}
}

func TestResolvePaneSessionNeverAuthorizesAReusedPaneID(t *testing.T) {
	configHome, stateDir := configuredCustomProfiles(t)
	resolver := NewResolver(configHome, nil, WithAssociationStore(stateDir))
	if err := resolver.Remember("pane", "personal"); err != nil {
		t.Fatal(err)
	}
	if err := resolver.Reconcile([]Observation{{PaneID: "pane", NativeSessionID: "session-old"}}); err != nil {
		t.Fatal(err)
	}
	if got := resolver.ResolvePaneSession("pane", "session-old", "copilot"); got != "personal" {
		t.Fatalf("exact session ownership = %q", got)
	}
	if got := resolver.ResolvePaneSession("pane", "session-new", "copilot"); got != "" {
		t.Fatalf("reused pane id inherited old ownership %q", got)
	}
	resolver.Invalidate()
	if got := resolver.ResolvePaneSession("pane", "session-old", "copilot"); got != "" {
		t.Fatalf("invalidated reconciliation retained ownership %q", got)
	}
}

func TestDeclaredProfileOwnershipSurvivesTemporaryExecutableLoss(t *testing.T) {
	configHome, stateDir := configuredCustomProfiles(t)
	observed := []Observation{{PaneID: "pane", NativeSessionID: "session"}}
	resolver := NewResolver(configHome, nil, WithAssociationStore(stateDir))
	resolver.Remember("pane", "personal")
	if err := resolver.Reconcile(observed); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(filepath.Dir(configHome), "bin", "personal")); err != nil {
		t.Fatal(err)
	}
	resolver.Reload()
	if err := resolver.Reconcile(observed); err != nil {
		t.Fatal(err)
	}
	if got := resolver.ResolvePane("pane", "copilot"); got != "personal" {
		t.Fatalf("declared ownership after executable loss = %q", got)
	}
	if stored := readAssociationStore(t, stateDir); len(stored.Associations) != 1 || stored.Associations[0].ProfileID != "personal" {
		t.Fatalf("temporary executable loss pruned ownership: %+v", stored.Associations)
	}

	ini := "[config]\nreplace_profiles = true\n[profiles]\nemu = EMU\n"
	if err := os.WriteFile(filepath.Join(configHome, "herdr", "agent-profiles.ini"), []byte(ini), 0o600); err != nil {
		t.Fatal(err)
	}
	resolver.Reload()
	if err := resolver.Reconcile(observed); err != nil {
		t.Fatal(err)
	}
	if stored := readAssociationStore(t, stateDir); len(stored.Associations) != 0 {
		t.Fatalf("removed profile retained ownership: %+v", stored.Associations)
	}
}

func TestPersistedIntegrationOwnershipSurvivesDiscoveryErrorAndRevalidatesOnRecovery(t *testing.T) {
	configHome := t.TempDir()
	stateDir := t.TempDir()
	status := &sequenceIntegrationStatus{responses: []integrationStatusResponse{
		{output: []byte("qodercli: current\n")},
		{err: errors.New("temporary Herdr timeout")},
		{},
	}}
	resolver := NewResolver(configHome, status, WithAssociationStore(stateDir))
	observed := []Observation{{PaneID: "pane", NativeSessionID: "session"}}
	_ = resolver.Profiles()
	resolver.Remember("pane", "qodercli")
	if err := resolver.Reconcile(observed); err != nil {
		t.Fatal(err)
	}

	resolver.Reload()
	if err := resolver.Reconcile(observed); err == nil {
		t.Fatal("transient integration discovery failure was hidden")
	}
	if got := resolver.ResolvePaneSession("pane", "session", "copilot"); got != "" {
		t.Fatalf("transient discovery error retained destructive authority: %q", got)
	}
	if stored := readAssociationStore(t, stateDir); len(stored.Associations) != 1 {
		t.Fatalf("transient discovery error pruned durable integration: %+v", stored.Associations)
	}

	resolver.Reload()
	if err := resolver.Reconcile(observed); err != nil {
		t.Fatal(err)
	}
	if got := resolver.ResolvePane("pane", "copilot"); got != "" {
		t.Fatalf("successful empty recovery retained removed integration: %q", got)
	}
	if stored := readAssociationStore(t, stateDir); len(stored.Associations) != 0 {
		t.Fatalf("successful empty recovery retained durable integration: %+v", stored.Associations)
	}
}

func TestMalformedProfileConfigPreservesLastKnownOwnershipUntilValidRecovery(t *testing.T) {
	configHome, stateDir := configuredCustomProfiles(t)
	configPath := filepath.Join(configHome, "herdr", "agent-profiles.ini")
	resolver := NewResolver(configHome, nil, WithAssociationStore(stateDir))
	observed := []Observation{{PaneID: "pane", NativeSessionID: "session"}}
	if err := resolver.Remember("pane", "personal"); err != nil {
		t.Fatal(err)
	}
	if err := resolver.Reconcile(observed); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(configPath, []byte("[profiles\npersonal = Personal\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	resolver.Reload()
	if err := resolver.Reconcile(observed); err == nil {
		t.Fatal("malformed profile config was treated as a complete discovery")
	}
	if got := resolver.ResolvePaneSession("pane", "session", "copilot"); got != "" {
		t.Fatalf("malformed discovery retained destructive authority %q", got)
	}
	if stored := readAssociationStore(t, stateDir); len(stored.Associations) != 1 || stored.Associations[0].ProfileID != "personal" {
		t.Fatalf("malformed discovery pruned last-known association: %+v", stored.Associations)
	}
	if err := os.WriteFile(configPath, []byte("[config]\nreplace_profiles = true\n[profiles]\npersonal = Personal\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	resolver.Reload()
	if err := resolver.Reconcile(observed); err != nil {
		t.Fatalf("valid recovery failed: %v", err)
	}
	if got := resolver.ResolvePaneSession("pane", "session", "copilot"); got != "personal" {
		t.Fatalf("valid recovery ownership = %q", got)
	}
}

func TestFirstDiscoveryAfterRestartPreservesDurableOwnershipOnMalformedConfig(t *testing.T) {
	configHome, stateDir := configuredCustomProfiles(t)
	observed := []Observation{{PaneID: "pane", NativeSessionID: "session"}}
	initial := NewResolver(configHome, nil, WithAssociationStore(stateDir))
	if err := initial.Remember("pane", "personal"); err != nil {
		t.Fatal(err)
	}
	if err := initial.Reconcile(observed); err != nil {
		t.Fatal(err)
	}
	configPath := filepath.Join(configHome, "herdr", "agent-profiles.ini")
	if err := os.WriteFile(configPath, []byte("[profiles\npersonal = Personal\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	restarted := NewResolver(configHome, nil, WithAssociationStore(stateDir))
	if err := restarted.Reconcile(observed); err == nil {
		t.Fatal("first malformed discovery after restart was treated as complete")
	}
	if got := restarted.ResolvePaneSession("pane", "session", "copilot"); got != "" {
		t.Fatalf("first malformed discovery authorized ownership %q", got)
	}
	if stored := readAssociationStore(t, stateDir); len(stored.Associations) != 1 || stored.Associations[0].ProfileID != "personal" {
		t.Fatalf("first malformed discovery pruned durable ownership: %+v", stored.Associations)
	}
	if err := os.WriteFile(configPath, []byte("[config]\nreplace_profiles = true\n[profiles]\npersonal = Personal\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	restarted.Reload()
	if err := restarted.Reconcile(observed); err != nil {
		t.Fatalf("recovery discovery failed: %v", err)
	}
	if got := restarted.ResolvePaneSession("pane", "session", "copilot"); got != "personal" {
		t.Fatalf("recovered ownership = %q", got)
	}
}

type integrationStatusResponse struct {
	output []byte
	err    error
}

type sequenceIntegrationStatus struct {
	responses []integrationStatusResponse
	next      int
}

func (s *sequenceIntegrationStatus) IntegrationStatus(context.Context) ([]byte, error) {
	response := s.responses[s.next]
	s.next++
	return response.output, response.err
}

func TestPresentUnverifiedPanesPreserveAndRevalidateDurableAssociations(t *testing.T) {
	configHome, stateDir := configuredCustomProfiles(t)
	initial := NewResolver(configHome, nil, WithAssociationStore(stateDir))
	initial.Remember("pane-personal", "personal")
	initial.Remember("pane-emu", "emu")
	exact := []Observation{
		{PaneID: "pane-personal", NativeSessionID: "session-personal"},
		{PaneID: "pane-emu", NativeSessionID: "session-emu"},
	}
	if err := initial.Reconcile(exact); err != nil {
		t.Fatal(err)
	}

	restarted := NewResolver(configHome, nil, WithAssociationStore(stateDir))
	unverified := []Observation{{PaneID: "pane-personal"}, {PaneID: "pane-emu"}}
	if err := restarted.Reconcile(unverified); err != nil {
		t.Fatalf("present panes with temporarily empty native IDs: %v", err)
	}
	for paneID, reportedAgent := range map[string]string{
		"pane-personal": "personal",
		"pane-emu":      "reported-emu",
	} {
		if got := restarted.ResolvePane(paneID, reportedAgent); got != "" {
			t.Fatalf("unverified %s guessed from reported agent %q as %q", paneID, reportedAgent, got)
		}
	}
	stored := readAssociationStore(t, stateDir)
	if len(stored.Associations) != 2 {
		t.Fatalf("unverified live panes erased durable associations: %+v", stored.Associations)
	}

	if err := restarted.Reconcile(exact); err != nil {
		t.Fatal(err)
	}
	if got := restarted.ResolvePane("pane-personal", "copilot"); got != "personal" {
		t.Fatalf("personal association did not revalidate: %q", got)
	}
	if got := restarted.ResolvePane("pane-emu", "copilot"); got != "emu" {
		t.Fatalf("EMU association did not revalidate: %q", got)
	}
}

func TestMismatchedLiveSessionPrunesStaleAssociationAndCanRecover(t *testing.T) {
	configHome, stateDir := configuredCustomProfiles(t)
	initial := NewResolver(configHome, nil, WithAssociationStore(stateDir))
	initial.Remember("pane", "personal")
	if err := initial.Reconcile([]Observation{{PaneID: "pane", NativeSessionID: "expected"}}); err != nil {
		t.Fatal(err)
	}

	restarted := NewResolver(configHome, nil, WithAssociationStore(stateDir))
	if err := restarted.Reconcile([]Observation{{PaneID: "pane", NativeSessionID: "different"}}); err != nil {
		t.Fatal(err)
	}
	for _, reportedAgent := range []string{"personal", "reported-personal"} {
		if got := restarted.ResolvePane("pane", reportedAgent); got != "" {
			t.Fatalf("mismatched live session guessed from reported agent %q as %q", reportedAgent, got)
		}
	}
	if stored := readAssociationStore(t, stateDir); len(stored.Associations) != 0 {
		t.Fatalf("mismatch retained stale durable association: %+v", stored.Associations)
	}
	if err := restarted.Remember("pane", "personal"); err != nil {
		t.Fatal(err)
	}
	if err := restarted.Reconcile([]Observation{{PaneID: "pane", NativeSessionID: "different"}}); err != nil {
		t.Fatal(err)
	}
	if got := restarted.ResolvePane("pane", "copilot"); got != "personal" {
		t.Fatalf("reused pane did not establish fresh ownership: %q", got)
	}
}

func TestAssociationPrunesOnlyAbsentOrExplicitlyForgottenPanes(t *testing.T) {
	configHome, stateDir := configuredCustomProfiles(t)
	resolver := NewResolver(configHome, nil, WithAssociationStore(stateDir))
	resolver.Remember("absent", "personal")
	resolver.Remember("forgotten", "emu")
	observed := []Observation{
		{PaneID: "absent", NativeSessionID: "session-absent"},
		{PaneID: "forgotten", NativeSessionID: "session-forgotten"},
	}
	if err := resolver.Reconcile(observed); err != nil {
		t.Fatal(err)
	}
	resolver.Forget("forgotten")
	if got := resolver.ResolvePane("forgotten", "emu"); got != "" {
		t.Fatalf("forgotten pane resolved before reconciliation as %q", got)
	}
	if err := resolver.Reconcile([]Observation{{PaneID: "forgotten", NativeSessionID: "session-forgotten"}}); err != nil {
		t.Fatal(err)
	}
	if got := resolver.ResolvePane("forgotten", "copilot"); got != "" {
		t.Fatalf("forgotten pane resolved to %q", got)
	}
	if stored := readAssociationStore(t, stateDir); len(stored.Associations) != 0 {
		t.Fatalf("absent/forgotten associations remained: %+v", stored.Associations)
	}
}

func TestAssociationReconcilePrunesRemovedProfileAndPersistsEmptyStore(t *testing.T) {
	configHome, stateDir := configuredCustomProfiles(t)
	resolver := NewResolver(configHome, nil, WithAssociationStore(stateDir))
	resolver.Remember(" PANE ", " PERSONAL ")
	observed := []Observation{{PaneID: "pane", NativeSessionID: "session"}}
	if err := resolver.Reconcile(observed); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(configHome, "herdr", "agent-profiles.ini"), []byte("[config]\nreplace_profiles = true\n[profiles]\nemu = EMU\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	resolver.Reload()
	if err := resolver.Reconcile(observed); err != nil {
		t.Fatal(err)
	}
	if got := resolver.ResolvePane("pane", "copilot"); got != "" {
		t.Fatalf("removed profile resolved to %q", got)
	}
	data, err := os.ReadFile(filepath.Join(stateDir, associationStoreName))
	if err != nil {
		t.Fatal(err)
	}
	var store associationStore
	if err := json.Unmarshal(data, &store); err != nil {
		t.Fatal(err)
	}
	if len(store.Associations) != 0 {
		t.Fatalf("pruned store = %+v", store.Associations)
	}
}

func TestAssociationReconcileRejectsInvalidAndAmbiguousObservations(t *testing.T) {
	configHome, stateDir := configuredCustomProfiles(t)
	tests := []struct {
		name     string
		observed []Observation
	}{
		{name: "empty pane", observed: []Observation{{NativeSessionID: "session"}}},
		{name: "control pane", observed: []Observation{{PaneID: "pane\n", NativeSessionID: "session"}}},
		{name: "control session", observed: []Observation{{PaneID: "pane", NativeSessionID: "session\x7f"}}},
		{name: "long pane", observed: []Observation{{PaneID: strings.Repeat("p", 257), NativeSessionID: "session"}}},
		{name: "duplicate pane", observed: []Observation{{PaneID: "pane", NativeSessionID: "one"}, {PaneID: "PANE", NativeSessionID: "two"}}},
		{name: "duplicate session", observed: []Observation{{PaneID: "one", NativeSessionID: "session"}, {PaneID: "two", NativeSessionID: "session"}}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			resolver := NewResolver(configHome, nil, WithAssociationStore(stateDir))
			if err := resolver.Reconcile(test.observed); err == nil {
				t.Fatal("invalid observations were accepted")
			}
		})
	}
}

func TestAssociationStoreFailsClosedForInvalidFiles(t *testing.T) {
	configHome, _ := configuredCustomProfiles(t)
	tests := []struct {
		name    string
		content string
	}{
		{name: "empty", content: ""},
		{name: "trailing value", content: `{"version":1,"associations":[]} {}`},
		{name: "trailing invalid", content: `{"version":1,"associations":[]} nope`},
		{name: "unknown field", content: `{"version":1,"associations":[],"future":true}`},
		{name: "wrong version", content: `{"version":2,"associations":[]}`},
		{name: "empty pane", content: `{"version":1,"associations":[{"pane_id":"","native_session_id":"s","profile_id":"personal"}]}`},
		{name: "empty session", content: `{"version":1,"associations":[{"pane_id":"p","native_session_id":"","profile_id":"personal"}]}`},
		{name: "empty profile", content: `{"version":1,"associations":[{"pane_id":"p","native_session_id":"s","profile_id":""}]}`},
		{name: "blank pane", content: `{"version":1,"associations":[{"pane_id":" ","native_session_id":"s","profile_id":"personal"}]}`},
		{name: "blank session", content: `{"version":1,"associations":[{"pane_id":"p","native_session_id":" ","profile_id":"personal"}]}`},
		{name: "blank profile", content: `{"version":1,"associations":[{"pane_id":"p","native_session_id":"s","profile_id":" "}]}`},
		{name: "duplicate pane", content: `{"version":1,"associations":[{"pane_id":"p","native_session_id":"s1","profile_id":"personal"},{"pane_id":"P","native_session_id":"s2","profile_id":"emu"}]}`},
		{name: "duplicate session", content: `{"version":1,"associations":[{"pane_id":"p1","native_session_id":"s","profile_id":"personal"},{"pane_id":"p2","native_session_id":"s","profile_id":"emu"}]}`},
		{name: "empty pending pane", content: `{"version":1,"associations":[],"pending":[{"pane_id":"","profile_id":"personal"}]}`},
		{name: "empty pending profile", content: `{"version":1,"associations":[],"pending":[{"pane_id":"pane","profile_id":""}]}`},
		{name: "blank pending pane", content: `{"version":1,"associations":[],"pending":[{"pane_id":" ","profile_id":"personal"}]}`},
		{name: "blank pending profile", content: `{"version":1,"associations":[],"pending":[{"pane_id":"pane","profile_id":" "]}`},
		{name: "duplicate pending pane", content: `{"version":1,"associations":[],"pending":[{"pane_id":"pane","profile_id":"personal"},{"pane_id":"PANE","profile_id":"emu"}]}`},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			stateDir := t.TempDir()
			if err := os.WriteFile(filepath.Join(stateDir, associationStoreName), []byte(test.content), 0o600); err != nil {
				t.Fatal(err)
			}
			resolver := NewResolver(configHome, nil, WithAssociationStore(stateDir))
			if err := resolver.Reconcile([]Observation{{PaneID: "pane", NativeSessionID: "session"}}); err == nil {
				t.Fatal("invalid store was accepted")
			}
			assertCorruptStoreDoesNotGuessOwnership(t, resolver)
		})
	}
}

func TestAssociationStoreRejectsUnsafeAndOversizedPaths(t *testing.T) {
	configHome, _ := configuredCustomProfiles(t)
	t.Run("no store configured", func(t *testing.T) {
		if err := NewResolver(configHome, nil).Reconcile(nil); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("unreadable parent", func(t *testing.T) {
		parent := filepath.Join(t.TempDir(), "parent")
		if err := os.WriteFile(parent, []byte("file"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := NewResolver(configHome, nil, WithAssociationStore(filepath.Join(parent, "state"))).Reconcile(nil); err == nil {
			t.Fatal("invalid association parent was accepted")
		}
	})
	t.Run("public file", func(t *testing.T) {
		stateDir := t.TempDir()
		path := filepath.Join(stateDir, associationStoreName)
		if err := os.WriteFile(path, []byte(`{"version":1,"associations":[]}`), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := NewResolver(configHome, nil, WithAssociationStore(stateDir)).Reconcile(nil); err == nil {
			t.Fatal("public store was accepted")
		}
		resolver := NewResolver(configHome, nil, WithAssociationStore(stateDir))
		if err := resolver.Reconcile([]Observation{{PaneID: "pane", NativeSessionID: "session"}}); err == nil {
			t.Fatal("public store was accepted during live reconciliation")
		}
		assertCorruptStoreDoesNotGuessOwnership(t, resolver)
	})
	t.Run("unreadable file", func(t *testing.T) {
		stateDir := t.TempDir()
		path := filepath.Join(stateDir, associationStoreName)
		if err := os.WriteFile(path, []byte(`{"version":1,"associations":[]}`), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(path, 0o000); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = os.Chmod(path, 0o600) })
		if err := NewResolver(configHome, nil, WithAssociationStore(stateDir)).Reconcile(nil); err == nil {
			t.Fatal("unreadable store was accepted")
		}
	})
	t.Run("store directory", func(t *testing.T) {
		stateDir := t.TempDir()
		if err := os.Mkdir(filepath.Join(stateDir, associationStoreName), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := NewResolver(configHome, nil, WithAssociationStore(stateDir)).Reconcile(nil); err == nil {
			t.Fatal("directory store was accepted")
		}
	})
	t.Run("store symlink", func(t *testing.T) {
		stateDir := t.TempDir()
		target := filepath.Join(stateDir, "target")
		if err := os.WriteFile(target, []byte(`{"version":1,"associations":[]}`), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(target, filepath.Join(stateDir, associationStoreName)); err != nil {
			t.Fatal(err)
		}
		resolver := NewResolver(configHome, nil, WithAssociationStore(stateDir))
		if err := resolver.Reconcile([]Observation{{PaneID: "pane", NativeSessionID: "session"}}); err == nil {
			t.Fatal("symlink store was accepted")
		}
		assertCorruptStoreDoesNotGuessOwnership(t, resolver)
	})
	t.Run("store hardlink", func(t *testing.T) {
		stateDir := t.TempDir()
		path := filepath.Join(stateDir, associationStoreName)
		if err := os.WriteFile(path, []byte(`{"version":1,"associations":[]}`), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Link(path, filepath.Join(stateDir, "second-link")); err != nil {
			t.Fatal(err)
		}
		if err := NewResolver(configHome, nil, WithAssociationStore(stateDir)).Reconcile(nil); err == nil {
			t.Fatal("hardlinked store was accepted")
		}
	})
	t.Run("oversized", func(t *testing.T) {
		stateDir := t.TempDir()
		if err := os.WriteFile(filepath.Join(stateDir, associationStoreName), make([]byte, maxAssociationStoreBytes+1), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := NewResolver(configHome, nil, WithAssociationStore(stateDir)).Reconcile(nil); err == nil {
			t.Fatal("oversized store was accepted")
		}
	})
}

func TestAssociationStoreReadFailureRetriesAfterFileIsRepaired(t *testing.T) {
	configHome, _ := configuredCustomProfiles(t)
	stateDir := t.TempDir()
	path := filepath.Join(stateDir, associationStoreName)
	content := `{"version":1,"associations":[{"pane_id":"pane","native_session_id":"session","profile_id":"personal"}]}`
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	resolver := NewResolver(configHome, nil, WithAssociationStore(stateDir))
	observed := []Observation{{PaneID: "pane", NativeSessionID: "session"}}
	if err := resolver.Reconcile(observed); err == nil {
		t.Fatal("unsafe store was accepted")
	}
	if err := os.Chmod(path, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := resolver.Reconcile(observed); err != nil {
		t.Fatalf("repaired store was not retried: %v", err)
	}
	if got := resolver.ResolvePaneSession("pane", "session", "copilot"); got != "personal" {
		t.Fatalf("repaired ownership = %q", got)
	}
}

func TestAssociationStoreRejectsSwapBetweenLstatAndOpen(t *testing.T) {
	directory := t.TempDir()
	path := filepath.Join(directory, associationStoreName)
	valid := `{"version":1,"associations":[]}`
	if err := os.WriteFile(path, []byte(valid), 0o600); err != nil {
		t.Fatal(err)
	}
	before, err := os.Lstat(path)
	if err != nil {
		t.Fatal(err)
	}
	other := filepath.Join(directory, "other")
	if err := os.WriteFile(other, []byte(valid), 0o600); err != nil {
		t.Fatal(err)
	}
	after, err := os.Lstat(other)
	if err != nil {
		t.Fatal(err)
	}
	ops := defaultAssociationStoreIO()
	ops.lstat = func(string) (os.FileInfo, error) { return before, nil }
	ops.open = func(string) (associationReadFile, error) {
		return &fixtureAssociationReadFile{Reader: strings.NewReader(valid), info: after}, nil
	}
	if _, err := readAssociationStoreWith(ops, path); err == nil {
		t.Fatal("swapped association store was accepted")
	}
}

func TestAssociationStoreRejectsSameInodeSymlinkSwap(t *testing.T) {
	directory := t.TempDir()
	path := filepath.Join(directory, associationStoreName)
	valid := `{"version":1,"associations":[]}`
	if err := os.WriteFile(path, []byte(valid), 0o600); err != nil {
		t.Fatal(err)
	}
	moved := filepath.Join(directory, "moved")
	ops := defaultAssociationStoreIO()
	ops.lstat = func(string) (os.FileInfo, error) {
		info, err := os.Lstat(path)
		if err != nil {
			return nil, err
		}
		if err := os.Rename(path, moved); err != nil {
			return nil, err
		}
		if err := os.Symlink(moved, path); err != nil {
			return nil, err
		}
		return info, nil
	}
	if _, err := readAssociationStoreWith(ops, path); err == nil {
		t.Fatal("same-inode symlink swap was accepted")
	}
}

func TestAssociationValidationHelpers(t *testing.T) {
	for _, value := range []string{"", strings.Repeat("x", 257), "value\n", "value\x7f"} {
		if validIdentifier(value) {
			t.Fatalf("invalid identifier accepted: %q", value)
		}
	}
	if !validIdentifier("value") {
		t.Fatal("valid identifier rejected")
	}
	if _, err := validObservations([]Observation{{PaneID: " ", NativeSessionID: "session"}}); err == nil {
		t.Fatal("normalized-empty pane was accepted")
	}
	if _, err := validObservations([]Observation{{PaneID: "pane"}}); err != nil {
		t.Fatalf("unknown live session was rejected: %v", err)
	}
	left := map[string]Association{"pane": {PaneID: "pane", NativeSessionID: "one", ProfileID: "personal"}}
	right := map[string]Association{"pane": {PaneID: "pane", NativeSessionID: "two", ProfileID: "personal"}}
	if sameAssociations(left, right) {
		t.Fatal("different associations compared equal")
	}
}

func TestAssociationWriteFailurePreservesMemoryAndRetries(t *testing.T) {
	configHome, stateDir := configuredCustomProfiles(t)
	resolver := NewResolver(configHome, nil, WithAssociationStore(stateDir))
	if err := resolver.Remember("pane", "personal"); err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(stateDir); err != nil {
		t.Fatal(err)
	}
	observed := []Observation{{PaneID: "pane", NativeSessionID: "session"}}
	if err := os.WriteFile(stateDir, []byte("not a directory"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := resolver.Reconcile(observed); err == nil {
		t.Fatal("write through file path succeeded")
	}
	if got := resolver.ResolvePane("pane", "copilot"); got != "personal" {
		t.Fatalf("pending association lost after write failure: %q", got)
	}
	if err := os.Remove(stateDir); err != nil {
		t.Fatal(err)
	}
	if err := resolver.Reconcile(observed); err != nil {
		t.Fatalf("retry failed: %v", err)
	}
	if got := resolver.ResolvePane("pane", "copilot"); got != "personal" {
		t.Fatalf("retried association = %q", got)
	}
}

type failingAssociationFile struct {
	name      string
	fail      string
	closeSeen bool
}

type fixtureAssociationReadFile struct {
	io.Reader
	info     os.FileInfo
	statErr  error
	closeErr error
}

func (f *fixtureAssociationReadFile) Stat() (os.FileInfo, error) { return f.info, f.statErr }
func (f *fixtureAssociationReadFile) Close() error               { return f.closeErr }

type failingAssociationReader struct{}

func (failingAssociationReader) Read([]byte) (int, error) { return 0, errors.New("read") }

type shortAssociationFile struct{ associationFile }

func (f shortAssociationFile) Write(data []byte) (int, error) {
	return f.associationFile.Write(data[:len(data)-1])
}

func (f *failingAssociationFile) Name() string { return f.name }
func (f *failingAssociationFile) Chmod(os.FileMode) error {
	if f.fail == "chmod" {
		return errors.New("chmod")
	}
	return nil
}
func (f *failingAssociationFile) Write(data []byte) (int, error) {
	if f.fail == "write" {
		return 0, errors.New("write")
	}
	return len(data), nil
}
func (f *failingAssociationFile) Sync() error {
	if f.fail == "sync" {
		return errors.New("sync")
	}
	return nil
}
func (f *failingAssociationFile) Close() error {
	f.closeSeen = true
	if f.fail == "close" {
		return errors.New("close")
	}
	return nil
}

type failingAssociationDirectory struct{ fail string }

func (d failingAssociationDirectory) Sync() error {
	if d.fail == "directory sync" {
		return errors.New("directory sync")
	}
	return nil
}
func (d failingAssociationDirectory) Close() error {
	if d.fail == "directory close" {
		return errors.New("directory close")
	}
	return nil
}

func TestAssociationAtomicWriterPropagatesEveryBoundaryFailure(t *testing.T) {
	directory := t.TempDir()
	info, err := os.Lstat(directory)
	if err != nil {
		t.Fatal(err)
	}
	associations := map[string]Association{
		"two": {PaneID: "two", NativeSessionID: "session-two", ProfileID: "emu"},
		"one": {PaneID: "one", NativeSessionID: "session-one", ProfileID: "personal"},
	}
	base := func() associationStoreIO {
		return associationStoreIO{
			lstat:    func(string) (os.FileInfo, error) { return info, nil },
			mkdirAll: func(string, os.FileMode) error { return nil },
			chmod:    func(string, os.FileMode) error { return nil },
			marshal:  json.Marshal,
			createTemp: func(string, string) (associationFile, error) {
				return &failingAssociationFile{name: filepath.Join(directory, "temporary")}, nil
			},
			rename: func(string, string) error { return nil },
			openDirectory: func(string) (associationDirectory, error) {
				return failingAssociationDirectory{}, nil
			},
		}
	}
	tests := []struct {
		name   string
		mutate func(*associationStoreIO)
	}{
		{name: "lstat", mutate: func(ops *associationStoreIO) {
			ops.lstat = func(string) (os.FileInfo, error) { return nil, errors.New("lstat") }
		}},
		{name: "mkdir", mutate: func(ops *associationStoreIO) {
			ops.lstat = func(string) (os.FileInfo, error) { return nil, os.ErrNotExist }
			ops.mkdirAll = func(string, os.FileMode) error { return errors.New("mkdir") }
		}},
		{name: "directory chmod", mutate: func(ops *associationStoreIO) {
			ops.chmod = func(string, os.FileMode) error { return errors.New("chmod") }
		}},
		{name: "marshal", mutate: func(ops *associationStoreIO) {
			ops.marshal = func(any) ([]byte, error) { return nil, errors.New("marshal") }
		}},
		{name: "create temp", mutate: func(ops *associationStoreIO) {
			ops.createTemp = func(string, string) (associationFile, error) { return nil, errors.New("create") }
		}},
		{name: "temp chmod", mutate: func(ops *associationStoreIO) { ops.createTemp = failingAssociationTemp(directory, "chmod") }},
		{name: "temp write", mutate: func(ops *associationStoreIO) { ops.createTemp = failingAssociationTemp(directory, "write") }},
		{name: "temp sync", mutate: func(ops *associationStoreIO) { ops.createTemp = failingAssociationTemp(directory, "sync") }},
		{name: "temp close", mutate: func(ops *associationStoreIO) { ops.createTemp = failingAssociationTemp(directory, "close") }},
		{name: "rename", mutate: func(ops *associationStoreIO) { ops.rename = func(string, string) error { return errors.New("rename") } }},
		{name: "open directory", mutate: func(ops *associationStoreIO) {
			ops.openDirectory = func(string) (associationDirectory, error) { return nil, errors.New("open") }
		}},
		{name: "directory sync", mutate: func(ops *associationStoreIO) {
			ops.openDirectory = func(string) (associationDirectory, error) {
				return failingAssociationDirectory{fail: "directory sync"}, nil
			}
		}},
		{name: "directory close", mutate: func(ops *associationStoreIO) {
			ops.openDirectory = func(string) (associationDirectory, error) {
				return failingAssociationDirectory{fail: "directory close"}, nil
			}
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			ops := base()
			test.mutate(&ops)
			if err := writeAssociationStoreWith(ops, directory, associations); err == nil {
				t.Fatal("boundary failure was ignored")
			}
		})
	}
}

func TestAssociationReaderPropagatesEveryBoundaryFailure(t *testing.T) {
	directory := t.TempDir()
	path := filepath.Join(directory, associationStoreName)
	if err := os.WriteFile(path, []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	info, err := os.Lstat(path)
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name string
		file *fixtureAssociationReadFile
	}{
		{name: "stat", file: &fixtureAssociationReadFile{Reader: strings.NewReader("{}"), info: info, statErr: errors.New("stat")}},
		{name: "read", file: &fixtureAssociationReadFile{Reader: failingAssociationReader{}, info: info}},
		{name: "close", file: &fixtureAssociationReadFile{Reader: strings.NewReader("{}"), info: info, closeErr: errors.New("close")}},
		{name: "post-open size", file: &fixtureAssociationReadFile{Reader: strings.NewReader(strings.Repeat("x", maxAssociationStoreBytes+1)), info: info}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			ops := defaultAssociationStoreIO()
			ops.lstat = func(string) (os.FileInfo, error) { return info, nil }
			ops.open = func(string) (associationReadFile, error) { return test.file, nil }
			if _, err := readAssociationStoreWith(ops, path); err == nil {
				t.Fatal("boundary failure was ignored")
			}
		})
	}
}

func TestAssociationStoreWrappersAndPendingComparison(t *testing.T) {
	directory := t.TempDir()
	associations := map[string]Association{"pane": {PaneID: "pane", NativeSessionID: "session", ProfileID: "personal"}}
	if err := writeAssociationStore(directory, associations); err != nil {
		t.Fatal(err)
	}
	if samePending(map[string]string{"pane": "personal"}, map[string]string{"pane": "emu"}) {
		t.Fatal("different pending associations compared equal")
	}
}

func TestAssociationAtomicWriterRejectsShortWriteAndPreservesPriorStore(t *testing.T) {
	directory := t.TempDir()
	path := filepath.Join(directory, associationStoreName)
	prior := []byte(`{"version":1,"associations":[{"pane_id":"old","native_session_id":"old-session","profile_id":"personal"}]}` + "\n")
	if err := os.WriteFile(path, prior, 0o600); err != nil {
		t.Fatal(err)
	}
	ops := defaultAssociationStoreIO()
	createTemp := ops.createTemp
	ops.createTemp = func(directory, pattern string) (associationFile, error) {
		file, err := createTemp(directory, pattern)
		if err != nil {
			return nil, err
		}
		return shortAssociationFile{associationFile: file}, nil
	}
	if err := writeAssociationStoreWith(ops, directory, map[string]Association{
		"new": {PaneID: "new", NativeSessionID: "new-session", ProfileID: "emu"},
	}); !errors.Is(err, io.ErrShortWrite) {
		t.Fatalf("short write error = %v, want io.ErrShortWrite", err)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(prior) {
		t.Fatalf("prior store changed after short write: %q", got)
	}
}

func failingAssociationTemp(directory, fail string) func(string, string) (associationFile, error) {
	return func(string, string) (associationFile, error) {
		return &failingAssociationFile{name: filepath.Join(directory, "temporary"), fail: fail}, nil
	}
}

func configuredCustomProfiles(t *testing.T) (string, string) {
	t.Helper()
	root := t.TempDir()
	configHome := filepath.Join(root, "config")
	stateDir := filepath.Join(root, "state")
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
	ini := "[config]\nreplace_profiles = true\n[profiles]\npersonal = Personal\nemu = EMU\n[aliases]\nreported-personal = personal\nreported-emu = emu\n"
	if err := os.WriteFile(filepath.Join(configHome, "herdr", "agent-profiles.ini"), []byte(ini), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", binDir)
	return configHome, stateDir
}

func assertCorruptStoreDoesNotGuessOwnership(t *testing.T, resolver *Resolver) {
	t.Helper()
	for _, reportedAgent := range []string{"personal", "reported-personal"} {
		if got := resolver.ResolvePane("pane", reportedAgent); got != "" {
			t.Fatalf("corrupt association store guessed %q from reported agent %q", got, reportedAgent)
		}
	}
}

func readAssociationStore(t *testing.T, stateDir string) associationStore {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(stateDir, associationStoreName))
	if err != nil {
		t.Fatal(err)
	}
	var store associationStore
	if err := json.Unmarshal(data, &store); err != nil {
		t.Fatal(err)
	}
	return store
}
