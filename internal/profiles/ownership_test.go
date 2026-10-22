package profiles

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
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

var hermesTarget = PaneIdentity{PaneID: "pane", TerminalID: "terminal", TabID: "tab", WorkspaceID: "workspace"}

func requireNoError(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func hermesResolver(t *testing.T, dir, path string) (*Resolver, string) {
	t.Helper()
	resolver := NewResolver(dir, nil)
	requireNoError(t, resolver.SetOwnershipPath(path))
	_, hermes, _ := resolver.OwnedHermesProfile(hermesTarget, "wrapper")
	return resolver, hermes
}

// The Hermes profile survives a restart, and every path that releases or replaces
// the ownership record releases it too: a later record never inherits it.
func TestHermesProfileIsReleasedWithItsOwnershipRecord(t *testing.T) {
	other := PaneIdentity{PaneID: "pane", TerminalID: "replacement"}
	for name, release := range map[string]func(*Resolver) error{
		"forget":               func(r *Resolver) error { r.Forget("pane"); return nil },
		"terminal replaced":    func(r *Resolver) error { return r.ReconcileOwnership([]PaneIdentity{other}) },
		"pane gone twice":      func(r *Resolver) error { _ = r.ReconcileOwnership(nil); return r.ReconcileOwnership(nil) },
		"relaunch pending":     func(r *Resolver) error { return r.BeginLaunchOwnership(hermesTarget, "wrapper") },
		"verified without one": func(r *Resolver) error { return r.RememberVerified(hermesTarget, "wrapper") },
	} {
		t.Run(name, func(t *testing.T) {
			dir, path := ownershipFixture(t)
			resolver, _ := hermesResolver(t, dir, path)
			requireNoError(t, resolver.RememberVerifiedHermes(hermesTarget, "wrapper", "apollo"))
			restarted, hermes := hermesResolver(t, dir, path)
			if _, known := restarted.ResolveOwnedHermesProfile("pane"); hermes != "apollo" || known {
				t.Fatalf("profile after restart = %q, want apollo, and none for a pane not yet observed", hermes)
			}
			requireNoError(t, resolver.ReconcileOwnership([]PaneIdentity{hermesTarget}))
			if got, known := resolver.ResolveOwnedHermesProfile("pane"); got != "apollo" || !known {
				t.Fatalf("observed pane profile = %q, want apollo", got)
			}
			requireNoError(t, release(resolver))
			if owned, hermes, known := resolver.OwnedHermesProfile(hermesTarget, "wrapper"); hermes != "" || known || (name != "relaunch pending" && name != "verified without one" && owned) {
				t.Fatalf("released record still answers: owned=%v profile=%q known=%v", owned, hermes, known)
			}
			if got, known := resolver.ResolveOwnedHermesProfile("pane"); got != "" || known {
				t.Fatalf("a relaunch would keep the released profile %q", got)
			}
			data, err := os.ReadFile(hermesStorePath(path))
			requireNoError(t, err)
			if strings.TrimSpace(string(data)) != "[]" {
				t.Fatalf("hermes file after release = %s", data)
			}
			restarted, _ = hermesResolver(t, dir, path)
			requireNoError(t, restarted.RememberVerified(hermesTarget, "wrapper"))
			if _, hermes, known := restarted.OwnedHermesProfile(hermesTarget, "wrapper"); hermes != "" || known {
				t.Fatalf("a new record inherited %q (known=%v)", hermes, known)
			}
		})
	}
}

// An unreadable Hermes file makes the store unavailable, leaves the files alone and matches nothing.
func TestUnreadableHermesFileFailsClosed(t *testing.T) {
	good := `[{"target":{"pane_id":"pane","terminal_id":"terminal","tab_id":"tab","workspace_id":"workspace"},"hermes_profile":"apollo"}`
	for name, damage := range map[string]func(path string) error{
		"corrupt":        func(p string) error { return os.WriteFile(p, []byte("not-json"), 0600) },
		"oversized":      func(p string) error { return os.WriteFile(p, []byte(strings.Repeat(" ", 1024*1024+1)), 0600) },
		"mode":           func(p string) error { return os.Chmod(p, 0644) },
		"symlink":        func(p string) error { _ = os.Rename(p, p+".real"); return os.Symlink(p+".real", p) },
		"hard link":      func(p string) error { return os.Link(p, p+".link") },
		"unknown field":  func(p string) error { return os.WriteFile(p, []byte(good[:len(good)-1]+`,"x":1}]`), 0600) },
		"duplicate pane": func(p string) error { return os.WriteFile(p, []byte(good+","+good[1:]+"]"), 0600) },
		"invalid name": func(p string) error {
			return os.WriteFile(p, []byte(strings.Replace(good, "apollo", "Bad Name", 1)+"]"), 0600)
		},
		"trailing data":   func(p string) error { return os.WriteFile(p, []byte(good+"] []"), 0600) },
		"hermes is a dir": func(p string) error { _ = os.Remove(p); return os.Mkdir(p, 0700) },
	} {
		t.Run(name, func(t *testing.T) {
			dir, path := ownershipFixture(t)
			resolver, _ := hermesResolver(t, dir, path)
			requireNoError(t, resolver.RememberVerifiedHermes(hermesTarget, "wrapper", "apollo"))
			requireNoError(t, damage(hermesStorePath(path)))
			before, _ := os.ReadFile(path)
			if err := resolver.SetOwnershipPath(path); err == nil {
				t.Fatal("accepted an unreadable Hermes file")
			}
			if owned, hermes, known := resolver.OwnedHermesProfile(hermesTarget, "wrapper"); owned || hermes != "" || known || resolver.OwnsTarget(hermesTarget, "wrapper") {
				t.Fatalf("unreadable store matched: owned=%v profile=%q known=%v", owned, hermes, known)
			}
			resolver.Forget("pane")
			if resolver.RememberVerifiedHermes(hermesTarget, "wrapper", "zeus") == nil || resolver.ReconcileOwnership(nil) == nil {
				t.Fatal("wrote to or reconciled an unavailable store")
			}
			if after, _ := os.ReadFile(path); string(after) != string(before) {
				t.Fatalf("ownership file rewritten: %s", after)
			}
		})
	}
}

func TestStaleHermesRecordsAreDroppedOnLoad(t *testing.T) {
	dir, path := ownershipFixture(t)
	resolver, _ := hermesResolver(t, dir, path)
	moved := PaneIdentity{PaneID: "moved", TerminalID: "terminal"}
	pending := PaneIdentity{PaneID: "pending", TerminalID: "terminal"}
	requireNoError(t, resolver.RememberVerifiedHermes(hermesTarget, "wrapper", "apollo"))
	requireNoError(t, resolver.RememberVerified(moved, "wrapper"))
	requireNoError(t, resolver.BeginLaunchOwnership(pending, "wrapper"))
	records := []hermesOwnership{
		{Target: hermesTarget, HermesProfile: "apollo"},
		{Target: PaneIdentity{PaneID: "moved", TerminalID: "elsewhere"}, HermesProfile: "zeus"},
		{Target: pending, HermesProfile: "zeus"},
		{Target: PaneIdentity{PaneID: "gone", TerminalID: "terminal"}, HermesProfile: "zeus"},
	}
	data, err := json.Marshal(records)
	requireNoError(t, err)
	requireNoError(t, os.WriteFile(hermesStorePath(path), data, 0600))
	restarted, hermes := hermesResolver(t, dir, path)
	if _, got, known := restarted.OwnedHermesProfile(moved, "wrapper"); hermes != "apollo" || got != "" || known {
		t.Fatalf("profiles = %q and %q (known=%v), want apollo and an unknown one", hermes, got, known)
	}
	if owned, got, _ := restarted.OwnedHermesProfile(pending, "wrapper"); owned || got != "" {
		t.Fatalf("pending record answered: %v %q", owned, got)
	}
	if rewritten, _ := os.ReadFile(hermesStorePath(path)); !strings.Contains(string(rewritten), "apollo") || strings.Contains(string(rewritten), "zeus") {
		t.Fatalf("stale records kept on disk: %s", rewritten)
	}
}

func TestHermesRecordPersistenceFailureKeepsThePreviousRecord(t *testing.T) {
	dir, path := ownershipFixture(t)
	resolver, _ := hermesResolver(t, dir, path)
	requireNoError(t, resolver.RememberVerifiedHermes(hermesTarget, "wrapper", "apollo"))
	requireNoError(t, os.Chmod(filepath.Dir(path), 0500))
	t.Cleanup(func() { _ = os.Chmod(filepath.Dir(path), 0700) })
	if resolver.RememberVerifiedHermes(hermesTarget, "wrapper", "zeus") == nil {
		t.Fatal("ignored a write failure")
	}
	if owned, hermes, known := resolver.OwnedHermesProfile(hermesTarget, "wrapper"); !owned || hermes != "apollo" || !known {
		t.Fatalf("answer after a failed replacement = %v %q known=%v, want the previous apollo", owned, hermes, known)
	}
}

// A Hermes launch without `-p` is a known empty profile, unlike a pane with no record.
func TestHermesLaunchWithoutProfileIsKnownAcrossRestart(t *testing.T) {
	dir, path := ownershipFixture(t)
	resolver, _ := hermesResolver(t, dir, path)
	requireNoError(t, resolver.RememberVerifiedHermes(hermesTarget, "wrapper", ""))
	restarted, _ := hermesResolver(t, dir, path)
	requireNoError(t, restarted.ReconcileOwnership([]PaneIdentity{hermesTarget}))
	if got, known := restarted.ResolveOwnedHermesProfile("pane"); got != "" || !known {
		t.Fatalf("launch without -p = %q known=%v, want known and empty", got, known)
	}
	if owned, got, known := restarted.OwnedHermesProfile(hermesTarget, "wrapper"); !owned || got != "" || !known {
		t.Fatalf("reuse answer for a launch without -p = %v %q known=%v, want owned, empty and known", owned, got, known)
	}
}

func TestOwnershipRecordKeepsItsRolledBackShape(t *testing.T) {
	if got := reflect.TypeOf(profileOwnership{}).NumField(); got != 3 {
		t.Fatalf("profileOwnership has %d fields; an added field breaks a rolled-back relay", got)
	}
	dir, path := ownershipFixture(t)
	resolver, _ := hermesResolver(t, dir, path)
	requireNoError(t, resolver.RememberVerifiedHermes(hermesTarget, "wrapper", "apollo"))
	file, err := os.Open(path)
	requireNoError(t, err)
	defer file.Close()
	decoder := json.NewDecoder(file)
	decoder.DisallowUnknownFields()
	var records []profileOwnership
	if err := decoder.Decode(&records); err != nil || len(records) != 1 || records[0].ProfileID != "wrapper" {
		t.Fatalf("base decoder: %+v, %v", records, err)
	}
}
