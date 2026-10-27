package profiles

import (
	"os"
	"path/filepath"
	"testing"
)

func ownershipFixture(t *testing.T) (string, string) {
	t.Helper()
	dir := t.TempDir()
	bin := filepath.Join(dir, "wrapper")
	if err := os.WriteFile(bin, []byte("#!/bin/sh\nexit 0\n"), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir)
	config := filepath.Join(dir, "herdr")
	if err := os.Mkdir(config, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(config, "agent-profiles.ini"), []byte("[profiles]\nwrapper = Wrapper\n"), 0600); err != nil {
		t.Fatal(err)
	}
	return dir, filepath.Join(dir, "private", "ownership.json")
}

// A poll that started before the launch reports the new pane as absent. That
// single snapshot must not delete the ownership a live agent depends on.
func TestOwnershipSurvivesOneStaleObservation(t *testing.T) {
	dir, path := ownershipFixture(t)
	target := PaneIdentity{PaneID: "pane", TerminalID: "terminal", TabID: "tab", WorkspaceID: "workspace"}
	resolver := NewResolver(dir, nil)
	if err := resolver.SetOwnershipPath(path); err != nil {
		t.Fatal(err)
	}
	if err := resolver.RememberVerified(target, "wrapper"); err != nil {
		t.Fatal(err)
	}
	if err := resolver.ReconcileOwnership(nil); err != nil {
		t.Fatal(err)
	}
	if err := resolver.ReconcileOwnership([]PaneIdentity{target}); err != nil {
		t.Fatal(err)
	}
	if got := resolver.ResolveOwnedPane("pane"); got != "wrapper" {
		t.Fatalf("stale observation deleted live ownership: %q", got)
	}

	reloaded := NewResolver(dir, nil)
	if err := reloaded.SetOwnershipPath(path); err != nil {
		t.Fatal(err)
	}
	if err := reloaded.ReconcileOwnership([]PaneIdentity{target}); err != nil {
		t.Fatal(err)
	}
	if got := reloaded.ResolveOwnedPane("pane"); got != "wrapper" {
		t.Fatalf("ownership was not persisted: %q", got)
	}

	for range 2 {
		if err := resolver.ReconcileOwnership(nil); err != nil {
			t.Fatal(err)
		}
	}
	if got := resolver.ResolveOwnedPane("pane"); got != "" {
		t.Fatalf("ownership of a gone pane was kept: %q", got)
	}
	gone := NewResolver(dir, nil)
	if err := gone.SetOwnershipPath(path); err != nil {
		t.Fatal(err)
	}
	if err := gone.ReconcileOwnership([]PaneIdentity{target}); err != nil {
		t.Fatal(err)
	}
	if got := gone.ResolveOwnedPane("pane"); got != "" {
		t.Fatalf("deletion was not persisted: %q", got)
	}
}

func TestVerifiedOwnershipSurvivesRestartNotReplacement(t *testing.T) {
	dir, path := ownershipFixture(t)
	target := PaneIdentity{PaneID: "pane", TerminalID: "terminal", TabID: "tab", WorkspaceID: "workspace"}
	resolver := NewResolver(dir, nil)
	if err := resolver.SetOwnershipPath(path); err != nil {
		t.Fatal(err)
	}
	if err := resolver.BeginLaunchOwnership(target, "wrapper"); err != nil {
		t.Fatal(err)
	}
	if err := resolver.ReconcileOwnership([]PaneIdentity{target}); err != nil {
		t.Fatal(err)
	}
	if got := resolver.ResolveOwnedPane("pane"); got != "" {
		t.Fatalf("pending ownership resolved to %q", got)
	}
	if err := resolver.RememberVerified(target, "wrapper"); err != nil {
		t.Fatal(err)
	}
	resolver = NewResolver(dir, nil)
	if err := resolver.SetOwnershipPath(path); err != nil {
		t.Fatal(err)
	}
	if got := resolver.ResolveOwnedPane("pane"); got != "" {
		t.Fatal("resolved before authoritative observation")
	}
	if err := resolver.ReconcileOwnership([]PaneIdentity{target}); err != nil {
		t.Fatal(err)
	}
	if got := resolver.ResolvePane("pane", "claude"); got != "wrapper" {
		t.Fatalf("lost custom wrapper: %q", got)
	}
	replacement := target
	replacement.TerminalID = "replacement"
	if err := resolver.ReconcileOwnership([]PaneIdentity{replacement}); err != nil {
		t.Fatal(err)
	}
	if got := resolver.ResolveOwnedPane("pane"); got != "" {
		t.Fatalf("ownership transferred: %q", got)
	}
	if err := resolver.ReconcileOwnership([]PaneIdentity{target}); err != nil {
		t.Fatal(err)
	}
	if got := resolver.ResolveOwnedPane("pane"); got != "" {
		t.Fatal("old ownership revived")
	}
}

func TestPendingOwnershipAndCorruptStoreFailClosed(t *testing.T) {
	dir, path := ownershipFixture(t)
	target := PaneIdentity{PaneID: "pane", TerminalID: "terminal"}
	resolver := NewResolver(dir, nil)
	if err := resolver.SetOwnershipPath(path); err != nil {
		t.Fatal(err)
	}
	if err := resolver.BeginLaunchOwnership(target, "wrapper"); err != nil {
		t.Fatal(err)
	}
	restarted := NewResolver(dir, nil)
	if err := restarted.SetOwnershipPath(path); err != nil {
		t.Fatal(err)
	}
	if err := restarted.ReconcileOwnership([]PaneIdentity{target}); err != nil {
		t.Fatal(err)
	}
	if restarted.ResolveOwnedPane("pane") != "" {
		t.Fatal("interrupted launch became verified")
	}
	if err := os.WriteFile(path, []byte("not-json"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := restarted.SetOwnershipPath(path); err == nil {
		t.Fatal("accepted corrupt ownership")
	}
	if err := restarted.ReconcileOwnership(nil); err == nil {
		t.Fatal("overwrote corrupt ownership")
	}
	if restarted.ResolveOwnedPane("pane") != "" {
		t.Fatal("guessed relaunch ownership after corruption")
	}
	if restarted.ResolvePane("pane", "wrapper") != "wrapper" {
		t.Fatal("read-only profile resolution lost the reported agent")
	}
}

// A store the loader refused must stay byte-for-byte intact so it can be
// inspected or restored; stopping an agent must not replace it.
func TestForgetLeavesRejectedStoreUntouched(t *testing.T) {
	for name, store := range map[string]struct {
		data []byte
		mode os.FileMode
	}{
		"corrupt":    {[]byte("not-json"), 0600},
		"wrong mode": {[]byte(`[{"target":{"pane_id":"pane","terminal_id":"terminal","tab_id":"","workspace_id":""},"profile_id":"wrapper"}]`), 0644},
	} {
		t.Run(name, func(t *testing.T) {
			dir, path := ownershipFixture(t)
			if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, store.data, store.mode); err != nil {
				t.Fatal(err)
			}
			if err := os.Chmod(path, store.mode); err != nil {
				t.Fatal(err)
			}
			resolver := NewResolver(dir, nil)
			if err := resolver.SetOwnershipPath(path); err == nil {
				t.Fatal("accepted a store the loader should reject")
			}
			resolver.Forget("pane")
			data, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if string(data) != string(store.data) {
				t.Fatalf("forget overwrote the rejected store: %q", data)
			}
			info, err := os.Stat(path)
			if err != nil {
				t.Fatal(err)
			}
			if info.Mode().Perm() != store.mode {
				t.Fatalf("forget replaced the rejected store: mode %v", info.Mode().Perm())
			}
		})
	}
}

// A launch between two absent observations resets the count: the second
// absence is a first strike again, so a relaunch that just succeeded keeps its
// ownership.
func TestLaunchBetweenAbsencesKeepsOwnership(t *testing.T) {
	dir, path := ownershipFixture(t)
	target := PaneIdentity{PaneID: "pane", TerminalID: "terminal", TabID: "tab", WorkspaceID: "workspace"}
	resolver := NewResolver(dir, nil)
	if err := resolver.SetOwnershipPath(path); err != nil {
		t.Fatal(err)
	}
	for _, step := range []func() error{
		func() error { return resolver.RememberVerified(target, "wrapper") },
		func() error { return resolver.ReconcileOwnership(nil) },
		func() error { return resolver.BeginLaunchOwnership(target, "wrapper") },
		func() error { return resolver.RememberVerified(target, "wrapper") },
		func() error { return resolver.ReconcileOwnership(nil) },
		func() error { return resolver.ReconcileOwnership([]PaneIdentity{target}) },
	} {
		if err := step(); err != nil {
			t.Fatal(err)
		}
	}
	if got := resolver.ResolveOwnedPane("pane"); got != "wrapper" {
		t.Fatalf("absences straddling a launch deleted ownership: %q", got)
	}
}

// When the deletion cannot be written, memory and disk disagree. The store
// must then answer nothing until it is reloaded, so no relaunch trusts either
// copy.
func TestFailedReconcilePersistFailsClosed(t *testing.T) {
	dir, path := ownershipFixture(t)
	target := PaneIdentity{PaneID: "pane", TerminalID: "terminal", TabID: "tab", WorkspaceID: "workspace"}
	resolver := NewResolver(dir, nil)
	if err := resolver.SetOwnershipPath(path); err != nil {
		t.Fatal(err)
	}
	if err := resolver.RememberVerified(target, "wrapper"); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(filepath.Dir(path), 0500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(filepath.Dir(path), 0700) })
	if err := resolver.ReconcileOwnership(nil); err != nil {
		t.Fatal(err)
	}
	if err := resolver.ReconcileOwnership(nil); err == nil {
		t.Fatal("reported a deletion that was never written")
	}
	if err := resolver.ReconcileOwnership([]PaneIdentity{target}); err == nil {
		t.Fatal("kept reconciling after the store became unavailable")
	}
	if got := resolver.ResolveOwnedPane("pane"); got != "" {
		t.Fatalf("resolved ownership from an unavailable store: %q", got)
	}
	if resolver.OwnsTarget(target, "wrapper") {
		t.Fatal("claimed ownership from an unavailable store")
	}
	if err := resolver.RememberVerified(target, "wrapper"); err == nil {
		t.Fatal("wrote to an unavailable store")
	}
	if err := os.Chmod(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	if err := resolver.SetOwnershipPath(path); err != nil {
		t.Fatal(err)
	}
	if err := resolver.ReconcileOwnership([]PaneIdentity{target}); err != nil {
		t.Fatal(err)
	}
	if got := resolver.ResolveOwnedPane("pane"); got != "wrapper" {
		t.Fatalf("reload did not recover the persisted record: %q", got)
	}
}

func TestOwnershipPersistenceFailureAndRemovedProfile(t *testing.T) {
	dir, path := ownershipFixture(t)
	target := PaneIdentity{PaneID: "pane", TerminalID: "terminal"}
	resolver := NewResolver(dir, nil)
	if err := resolver.SetOwnershipPath(path); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Dir(path), nil, 0600); err != nil {
		t.Fatal(err)
	}
	if err := resolver.RememberVerified(target, "wrapper"); err == nil {
		t.Fatal("ignored write failure")
	}
	if err := os.Remove(filepath.Dir(path)); err != nil {
		t.Fatal(err)
	}
	if err := resolver.RememberVerified(target, "wrapper"); err != nil {
		t.Fatal(err)
	}
	if err := resolver.ReconcileOwnership([]PaneIdentity{target}); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(dir, "wrapper")); err != nil {
		t.Fatal(err)
	}
	resolver.Reload()
	if resolver.ResolveOwnedPane("pane") != "" {
		t.Fatal("resolved removed profile")
	}
}
