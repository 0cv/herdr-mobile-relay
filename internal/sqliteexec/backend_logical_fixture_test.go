//go:build herdr_sqlite_logical_test && !herdr_installed_hosted_test && !herdr_tailscale_test && !herdr_tailscale_cli_fixture_binary

package sqliteexec

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"

	"github.com/0cv/herdr-mobile-relay/internal/conversationlife"
)

func TestICXLogicalDefaultFactories(t *testing.T) {
	t.Run("from_env", func(t *testing.T) {
		executor, err := FromEnv()
		if executor != nil || err != ErrUnavailable {
			t.Fatal("expected nil executor and exact unavailable error")
		}
	})
	t.Run("must_from_env", func(t *testing.T) {
		executor := MustFromEnv()
		fallback, ok := executor.(*logicalUnavailableExecutor)
		if !ok || fallback == nil {
			t.Fatal("expected nonnil private unavailable executor, not CLI fallback")
		}
		requireICXLogicalDenial(t, executor)
	})
	for _, tc := range []struct {
		name    string
		backend Backend
	}{
		{"resolve_auto", BackendAuto},
		{"resolve_native", BackendNative},
		{"resolve_cli", BackendCLI},
		{"resolve_empty", ""},
		{"resolve_invalid", "invalid"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			binary := filepath.Join(t.TempDir(), "nonexistent-cli")
			executor, err := Resolve(tc.backend, binary)
			if executor != nil || err != ErrUnavailable {
				t.Fatal("expected nil executor and exact unavailable error")
			}
		})
	}
}

func TestICXLogicalDirectBackends(t *testing.T) {
	t.Run("native", func(t *testing.T) {
		executor := newNative()
		if executor == nil {
			t.Fatal("nil native instance")
		}
		requireICXLogicalDenial(t, executor)
	})
	t.Run("cli", func(t *testing.T) {
		executor := newCLI(filepath.Join(t.TempDir(), "nonexistent-cli"))
		if executor == nil {
			t.Fatal("nil CLI instance")
		}
		requireICXLogicalDenial(t, executor)
	})
}

func requireICXLogicalDenial(t *testing.T, executor Executor) {
	t.Helper()
	if executor.Ready() {
		t.Fatal("logical backend must not report readiness")
	}
	// These nonexistent operands are defensive, not filesystem containment.
	database := filepath.Join(t.TempDir(), "nonexistent.db")
	data, err := executor.QueryJSON(context.Background(), database, "SELECT 1", 1024)
	if data != nil || err != ErrUnavailable {
		t.Fatal("expected nil bytes and exact unavailable error")
	}
}

func TestICXLogicalInstalledDefaults(t *testing.T) {
	for _, tc := range []struct {
		name    string
		backend Backend
	}{
		{"auto", BackendAuto},
		{"native", BackendNative},
		{"cli", BackendCLI},
		{"empty", ""},
		{"invalid", "invalid"},
		{"nil_owner", BackendAuto},
		{"fenced_owner", BackendAuto},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var owner *conversationlife.Owner
			want := ErrUnavailable
			if tc.name != "nil_owner" {
				owner = conversationlife.New()
				t.Cleanup(owner.Join)
			}
			if tc.name == "nil_owner" {
				want = conversationlife.ErrRefused
			}
			if tc.name == "fenced_owner" {
				owner.Begin()
				want = conversationlife.ErrRefused
			}
			binary := filepath.Join(t.TempDir(), "nonexistent-cli")
			executor, err := NewInstalled(context.Background(), owner, tc.backend, binary)
			// Never invoke methods on an unexpectedly acquired executor.
			if executor != nil || err != want {
				t.Fatal("expected nil installed executor and exact refusal")
			}
			if owner != nil {
				owner.Join()
				if owner.Witnesses() != nil {
					t.Fatal("ordinary refusal must leave the witness ledger empty")
				}
			}
		})
	}
}

func TestICXLogicalDriverRegistration(t *testing.T) {
	// This is a POST-init assertion, NOT pre-init protection. Parent source
	// and target-specific dependency exclusion is mandatory before execution.
	// Never open SQL to establish driver absence (including fake drivers).
	if len(sql.Drivers()) != 0 {
		t.Fatal("logical fixture must have no registered SQL drivers")
	}
}
