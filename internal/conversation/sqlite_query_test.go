package conversation

import (
	"path/filepath"
	"testing"

	"github.com/0cv/herdr-mobile-relay/internal/sqliteexec"
)

func TestResolveQueryExecutor(t *testing.T) {
	t.Parallel()
	native, err := sqliteexec.Resolve(sqliteexec.BackendNative, "")
	if err != nil {
		t.Fatal(err)
	}

	got := resolveQueryExecutor(native, "sqlite3")
	if got != native {
		t.Fatal("default binary name must keep primary executor")
	}

	missing := filepath.Join(t.TempDir(), "missing-sqlite3")
	got = resolveQueryExecutor(native, missing)
	if got != native {
		t.Fatal("unusable CLI override must fall back to primary")
	}

	got = resolveQueryExecutor(nil, missing)
	if got == nil || !got.Ready() {
		t.Fatal("nil primary with missing CLI must still yield a Ready MustFromEnv executor")
	}
}
