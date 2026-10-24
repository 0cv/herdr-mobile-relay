package conversation

import (
	"context"
	"crypto/sha256"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/0cv/herdr-mobile-relay/internal/agentroots"
	"github.com/0cv/herdr-mobile-relay/internal/sqliteexec"
)

type changeCountingExecutor struct {
	sqliteexec.Executor
	queries int
}

func (e *changeCountingExecutor) QueryJSON(ctx context.Context, database, query string, maxBytes int) ([]byte, error) {
	e.queries++
	return e.Executor.QueryJSON(ctx, database, query, maxBytes)
}

func TestOMORecordedPlanChangeToken(t *testing.T) {
	home := t.TempDir()
	root := filepath.Join(home, "sessions")
	cwd := t.TempDir()
	session := "omo-live"
	path := filepath.Join(root, encodeOMOCWD(cwd), "2026-10-20T12-00-00-000Z_"+session+".jsonl")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv(agentroots.OMOListEnv, home)
	file, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	if _, err := fmt.Fprintf(file, "{\"type\":\"session\",\"id\":%q,\"cwd\":%q}\n", session, cwd); err != nil {
		t.Fatal(err)
	}
	reader := NewReader(home)
	scope := BrowseScope{Provider: "Oh My OpenCode", CWD: cwd, SessionID: session}
	before, err := reader.ChangeToken(t.Context(), scope)
	if err != nil || before == "" {
		t.Fatalf("missing OMO change token: %q %v", before, err)
	}
	if _, err := file.WriteString("{\"type\":\"custom\",\"customType\":\"senpi.todo-state\",\"data\":{\"schema\":\"v2\",\"phases\":[{\"name\":\"Build\",\"tasks\":[{\"content\":\"Ship\",\"status\":\"pending\"}]}]}}\n"); err != nil {
		t.Fatal(err)
	}
	after, err := reader.ChangeToken(t.Context(), scope)
	page, readErr := reader.ReadFor(scope.Provider, cwd, session, "", 10)
	if err != nil || readErr != nil || before == after || page.OMOPlan == nil || len(page.OMOPlan.Phases) != 1 {
		t.Fatalf("OMO plan did not update: %+v %v %v", page, err, readErr)
	}
}

func TestDatabaseChangeTokenIdleAndReplacement(t *testing.T) {
	for _, provider := range []string{"opencode", "hermes"} {
		t.Run(provider, func(t *testing.T) {
			root := t.TempDir()
			t.Setenv(agentroots.OpenCodeListEnv, root)
			t.Setenv(agentroots.HermesListEnv, root)
			t.Setenv("HERMES_HOME", "")
			name := "opencode.db"
			if provider == "hermes" {
				name = "state.db"
			}
			path := filepath.Join(root, name)
			if err := os.WriteFile(path, make([]byte, 4096), 0o400); err != nil {
				t.Fatal(err)
			}
			reader := NewReader(t.TempDir())
			executor := &changeCountingExecutor{Executor: sqliteexec.MustFromEnv()}
			reader.openCode.executor, reader.hermes.executor = executor, executor
			scope := BrowseScope{Provider: provider, SessionID: "ses_idle123"}
			before, err := reader.ChangeToken(t.Context(), scope)
			if err != nil || before == "" {
				t.Fatalf("missing database change token: %q %v", before, err)
			}
			start := time.Now()
			for range 1000 {
				token, err := reader.ChangeToken(t.Context(), scope)
				if err != nil || token != before {
					t.Fatalf("idle token changed: %q %v", token, err)
				}
			}
			t.Logf("idle provider=%s polls=1000 queries=%d elapsed=%s per_poll=%s", provider, executor.queries, time.Since(start), time.Since(start)/1000)
			if executor.queries != 0 {
				t.Fatal("idle polling executed SQL")
			}
			info, _ := os.Stat(path)
			if err := os.WriteFile(path+".next", make([]byte, 4096), 0o400); err != nil {
				t.Fatal(err)
			}
			if err := os.Chtimes(path+".next", info.ModTime(), info.ModTime()); err != nil {
				t.Fatal(err)
			}
			if err := os.Rename(path+".next", path); err != nil {
				t.Fatal(err)
			}
			after, err := reader.ChangeToken(t.Context(), scope)
			if err != nil || after == before {
				t.Fatal("same-size/time replacement did not wake feed")
			}
			raw, _ := os.ReadFile(path)
			if sha256.Sum256(raw) != sha256.Sum256(make([]byte, 4096)) {
				t.Fatal("polling modified source")
			}
			if err := os.WriteFile(path+"-wal", make([]byte, 32), 0o600); err != nil {
				t.Fatal(err)
			}
			wal, _ := reader.ChangeToken(t.Context(), scope)
			if wal == after {
				t.Fatal("WAL creation did not wake feed")
			}
			cancelled, cancel := context.WithCancel(t.Context())
			cancel()
			if _, err := reader.ChangeToken(cancelled, scope); err == nil {
				t.Fatal("cancelled polling ignored context")
			}
		})
	}
}
