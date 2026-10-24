package conversation

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/0cv/herdr-mobile-relay/internal/agentroots"
	"github.com/0cv/herdr-mobile-relay/internal/sqliteexec"
)

type liveBudgetExecutor struct {
	t        *testing.T
	base     sqliteexec.Executor
	active   bool
	queries  int
	rows     int
	duration time.Duration
	plans    map[string]bool
}

func (e *liveBudgetExecutor) Ready() bool { return e.base.Ready() }
func (e *liveBudgetExecutor) QueryJSON(ctx context.Context, database, query string, maxBytes int) ([]byte, error) {
	if e.active && (strings.Contains(query, "COUNT(*)") || strings.Contains(query, " OVER (") || strings.Contains(query, " OFFSET ")) {
		e.t.Fatalf("unbounded live query: %s", query)
	}
	start := time.Now()
	raw, err := e.base.QueryJSON(ctx, database, query, maxBytes)
	if !e.active {
		return raw, err
	}
	e.duration += time.Since(start)
	e.queries++
	var rows []map[string]any
	if err == nil && json.Unmarshal(raw, &rows) == nil {
		for _, row := range rows {
			e.rows++
			if parts, ok := row["parts"].(string); ok {
				var decoded []any
				if json.Unmarshal([]byte(parts), &decoded) == nil {
					e.rows += len(decoded)
				}
			}
			if detail, ok := row["detail"].(string); ok {
				e.plans[detail] = true
			}
		}
	}
	return raw, err
}

func liveDatabaseFixture(t *testing.T, format string, count int) (*sql.DB, *Browser, *DatabaseWatch, *liveBudgetExecutor) {
	t.Helper()
	root := t.TempDir()
	t.Setenv(agentroots.OpenCodeListEnv, root)
	t.Setenv(agentroots.HermesListEnv, root)
	t.Setenv("HERMES_HOME", "")
	name, provider, session := "opencode.db", "opencode", "ses_livebudget"
	if format == "hermes" {
		name, provider, session = "state.db", "hermes", "live-budget"
	}
	db, err := sql.Open("sqlite", filepath.Join(root, name))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	db.SetMaxOpenConns(1)
	liveExec(t, db, "PRAGMA journal_mode=WAL; PRAGMA wal_autocheckpoint=0")
	switch format {
	case "v1":
		liveExec(t, db, openCodeV1Schema+`CREATE INDEX message_session_id_time ON message(session_id,time_created,id); CREATE INDEX part_message_id ON part(message_id); INSERT INTO session VALUES('ses_livebudget','/fixture','Live',1,'opencode');`)
	case "v2":
		liveExec(t, db, openCodeV2Schema+`INSERT INTO session_v2(id,directory,title) VALUES('ses_livebudget','/fixture','Live')`)
	case "hermes":
		liveExec(t, db, `CREATE TABLE sessions(id TEXT PRIMARY KEY,cwd TEXT,title TEXT); `+hermesTestMessageSchema+`INSERT INTO sessions VALUES('live-budget',?,'Live')`, root)
	}
	seed := func(first, last int) {
		if first > last {
			return
		}
		numbers := fmt.Sprintf("WITH RECURSIVE n(i) AS (VALUES(%d) UNION ALL SELECT i+1 FROM n WHERE i<%d) ", first, last)
		switch format {
		case "v1":
			liveExec(t, db, numbers+`INSERT INTO message(rowid,id,session_id,time_created,data) SELECT i,printf('m%06d',i),'ses_livebudget',i,'{"role":"assistant"}' FROM n`)
			liveExec(t, db, numbers+`INSERT INTO part(rowid,id,message_id,data) SELECT (i-1)*10+j+1,printf('p%06d-%02d',i,j),printf('m%06d',i),'{"type":"text","text":"seed"}' FROM n CROSS JOIN (SELECT 0 j UNION ALL SELECT 1 UNION ALL SELECT 2 UNION ALL SELECT 3 UNION ALL SELECT 4 UNION ALL SELECT 5 UNION ALL SELECT 6 UNION ALL SELECT 7 UNION ALL SELECT 8 UNION ALL SELECT 9)`)
		case "v2":
			data := `{"content":[` + strings.TrimSuffix(strings.Repeat(`{"type":"text","text":"seed"},`, 10), ",") + `]}`
			liveExec(t, db, numbers+`INSERT INTO session_message(rowid,id,session_id,type,seq,time_created,time_updated,data) SELECT i,printf('m%06d',i),'ses_livebudget','assistant',i,i,i,? FROM n`, data)
		case "hermes":
			liveExec(t, db, numbers+`INSERT INTO messages(id,session_id,role,content,timestamp,active,compacted) SELECT i,'live-budget','assistant',?,i,1,0 FROM n`, strings.Repeat("seed\n", 10))
		}
	}
	seed(max(1, count-maxPageSize+1), count)
	reader := NewReader(t.TempDir())
	executor := &liveBudgetExecutor{t: t, base: sqliteexec.MustFromEnv(), plans: make(map[string]bool)}
	reader.openCode.executor, reader.hermes.executor = executor, executor
	browser, err := NewBrowser(reader, t.TempDir(), DefaultBrowserOptions())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = browser.Close() })
	watch := browser.NewDatabaseWatch(BrowseScope{Provider: provider, SessionID: session, CWD: root})
	page, reset := watch.Refresh(t.Context())
	if reset || page.State != BrowseReady || len(page.Entries) != min(count, maxPageSize) {
		t.Fatalf("initial page reset=%v page=%+v", reset, page)
	}
	seed(1, count-maxPageSize)
	var stored int
	if err := db.QueryRow("SELECT COUNT(*) FROM " + watch.table()).Scan(&stored); err != nil || stored != count {
		t.Fatalf("stored messages=%d want=%d error=%v", stored, count, err)
	}
	if format == "v1" {
		if err := db.QueryRow("SELECT COUNT(*) FROM part").Scan(&stored); err != nil || stored != count*10 {
			t.Fatalf("stored parts=%d want=%d error=%v", stored, count*10, err)
		}
	}
	executor.active = true
	for _, query := range []string{
		fmt.Sprintf("SELECT rowid FROM %s NOT INDEXED WHERE rowid>1 ORDER BY rowid LIMIT %d", watch.table(), databaseLiveAppend+1),
		fmt.Sprintf("SELECT rowid FROM %s NOT INDEXED WHERE rowid>=1 ORDER BY rowid LIMIT %d", watch.table(), databaseLiveRows+1),
	} {
		var plans []struct {
			Detail string `json:"detail"`
		}
		if code := watch.query(t.Context(), "EXPLAIN QUERY PLAN "+query, &plans); code != "" {
			t.Fatal(code)
		}
		found := false
		for _, plan := range plans {
			if strings.Contains(plan.Detail, "USING INTEGER PRIMARY KEY (rowid>?)") {
				found = true
			}
		}
		if !found {
			t.Fatalf("rowid budget query is not indexed: %s %+v", query, plans)
		}
	}
	return db, browser, watch, executor
}

func liveExec(t *testing.T, db *sql.DB, query string, args ...any) {
	t.Helper()
	if _, err := db.Exec(query, args...); err != nil {
		t.Fatal(err)
	}
}

func TestDatabaseLiveLoadedEditAndBounds(t *testing.T) {
	for _, format := range []string{"v1", "v2", "hermes"} {
		t.Run(format, func(t *testing.T) {
			db, browser, watch, executor := liveDatabaseFixture(t, format, 450)
			executor.active = false
			recent, err := browser.ReadPage(t.Context(), BrowseRequest{Scope: watch.scope, Limit: 200})
			if err != nil {
				t.Fatal(err)
			}
			older, err := browser.ReadPage(t.Context(), BrowseRequest{Scope: watch.scope, Cursor: recent.NextCursor, Limit: 200})
			if err != nil || len(older.Entries) != 200 {
				t.Fatalf("older=%+v error=%v", older, err)
			}
			watch.Observe(older, false)
			watch.Observe(recent, true)
			executor.active = true
			id := older.Entries[0].ID
			switch format {
			case "v1":
				liveExec(t, db, `UPDATE part SET data='{"type":"text","text":"loaded edit"}' WHERE id=?`, "p"+strings.TrimPrefix(id, "m")+"-00")
			case "v2":
				liveExec(t, db, `UPDATE session_message SET data='{"content":[{"type":"text","text":"loaded edit"}]}' WHERE id=?`, id)
			case "hermes":
				liveExec(t, db, `UPDATE messages SET content='loaded edit' WHERE id=?`, id)
			}
			page, reset := watch.Refresh(t.Context())
			if reset || len(page.Entries) != 1 || page.Entries[0].ID != id || !strings.Contains(page.Entries[0].Text, "loaded edit") {
				t.Fatalf("loaded edit reset=%v entries=%+v", reset, page.Entries)
			}
			t.Logf("loaded=%d rows_returned_including_parts_and_plans=%d queries=%d", len(watch.loaded), executor.rows, executor.queries)
			if format == "v1" {
				liveExec(t, db, "DROP INDEX part_message_id")
				if _, reset := watch.Refresh(t.Context()); !reset {
					t.Fatal("unindexed parts must request a snapshot, not scan the store")
				}
			} else {
				table := watch.table()
				liveExec(t, db, "DELETE FROM "+table+" WHERE id=?", id)
				if _, reset := watch.Refresh(t.Context()); !reset {
					t.Fatal("loaded removal must reset")
				}
			}
		})
	}
}

func TestDatabaseLiveOverflowAndHermesCompaction(t *testing.T) {
	for _, format := range []string{"v1", "v2", "hermes"} {
		t.Run(format, func(t *testing.T) {
			db, _, watch, executor := liveDatabaseFixture(t, format, 450)
			if format == "hermes" {
				liveExec(t, db, `INSERT INTO messages(session_id,role,content,timestamp,active,compacted) SELECT session_id,role,content,timestamp,active,compacted FROM messages WHERE id=1`)
			} else {
				for i := 451; i < 451+databaseLiveAppend+1; i++ {
					if format == "v1" {
						liveExec(t, db, `INSERT INTO message VALUES(?,'ses_livebudget',?,'{"role":"assistant"}')`, fmt.Sprintf("m%06d", i), i)
					} else {
						liveExec(t, db, `INSERT INTO session_message VALUES(?,'ses_livebudget','assistant',?,?,?, '{"content":[]}')`, fmt.Sprintf("m%06d", i), i, i, i)
					}
				}
			}
			executor.rows = 0
			if _, reset := watch.Refresh(t.Context()); !reset {
				t.Fatal("overflow/old-timestamp compaction must reset without a full scan")
			}
			if executor.rows > databaseLiveAppend+100 {
				t.Fatalf("fallback exceeded append budget: %d", executor.rows)
			}
			for i := 0; i < 3; i++ {
				if _, reset := watch.Refresh(t.Context()); reset {
					t.Fatal("suspended source repeated reset without a new snapshot")
				}
			}
		})
	}
}

func TestDatabaseLivePagingDoesNotConsumeAnotherViewersDelta(t *testing.T) {
	for _, format := range []string{"v1", "v2", "hermes"} {
		t.Run(format, func(t *testing.T) {
			db, browser, watch, executor := liveDatabaseFixture(t, format, 450)
			for _, appendNew := range []bool{false, true} {
				id, text := "m000450", "edited before page"
				if format == "hermes" {
					id = "450"
				}
				if appendNew {
					id, text = "m000451", "appended before page"
					if format == "hermes" {
						id = "451"
					}
				}
				switch format {
				case "v1":
					if appendNew {
						liveExec(t, db, `INSERT INTO message VALUES(?,'ses_livebudget',451,'{"role":"assistant"}')`, id)
						liveExec(t, db, `INSERT INTO part VALUES(?,?,?)`, "p"+id, id, `{"type":"text","text":"appended before page"}`)
					} else {
						liveExec(t, db, `UPDATE part SET data='{"type":"text","text":"edited before page"}' WHERE id='p000450-00'`)
					}
				case "v2":
					if appendNew {
						liveExec(t, db, `INSERT INTO session_message VALUES(?,'ses_livebudget','assistant',451,451,451,'{"content":[{"type":"text","text":"appended before page"}]}')`, id)
					} else {
						liveExec(t, db, `UPDATE session_message SET data='{"content":[{"type":"text","text":"edited before page"}]}' WHERE id=?`, id)
					}
				case "hermes":
					if appendNew {
						liveExec(t, db, `INSERT INTO messages(id,session_id,role,content,timestamp,active,compacted) VALUES(451,'live-budget','assistant','appended before page',451,1,0)`)
					} else {
						liveExec(t, db, `UPDATE messages SET content='edited before page' WHERE id=?`, id)
					}
				}
				executor.active = false
				fresh, err := browser.ReadPage(t.Context(), BrowseRequest{Scope: watch.scope, Limit: 200})
				if err != nil || fresh.State != BrowseReady {
					t.Fatalf("fresh page=%+v error=%v", fresh, err)
				}
				watch.Observe(fresh, true)
				if appendNew && format == "hermes" {
					watch.latestTimestamp = 451
				}
				executor.active = true
				page, reset := watch.Refresh(t.Context())
				if reset || len(page.Entries) != 1 || page.Entries[0].ID != id || !strings.Contains(page.Entries[0].Text, text) || page.RecordedEdits[id] == appendNew {
					t.Fatalf("page consumed delta or misclassified append: append=%v reset=%v page=%+v", appendNew, reset, page)
				}
			}
		})
	}
}

func liveCPU() time.Duration {
	var usage syscall.Rusage
	_ = syscall.Getrusage(syscall.RUSAGE_SELF, &usage)
	return time.Duration(usage.Utime.Sec+usage.Stime.Sec)*time.Second + time.Duration(usage.Utime.Usec+usage.Stime.Usec)*time.Microsecond
}

func TestDatabaseLiveLargeStore(t *testing.T) {
	for _, format := range []string{"v1", "v2", "hermes"} {
		t.Run(format, func(t *testing.T) {
			db, _, watch, executor := liveDatabaseFixture(t, format, 100000)
			ctx, cancel := context.WithCancel(t.Context())
			done := make(chan error, 1)
			go func() {
				ticker := time.NewTicker(50 * time.Millisecond)
				defer ticker.Stop()
				for serial := 100001; ; serial++ {
					select {
					case <-ctx.Done():
						done <- nil
						return
					case <-ticker.C:
					}
					tx, err := db.BeginTx(ctx, nil)
					if err != nil {
						if ctx.Err() != nil {
							err = nil
						}
						done <- err
						return
					}
					exec := func(query string, args ...any) {
						if err == nil {
							_, err = tx.ExecContext(ctx, query, args...)
						}
					}
					id := fmt.Sprintf("m%06d", serial)
					switch format {
					case "v1":
						exec(`INSERT INTO message VALUES(?,'ses_livebudget',?,'{"role":"assistant"}')`, id, serial)
						exec(`INSERT INTO part VALUES(?,?,?)`, "p"+id, id, fmt.Sprintf(`{"type":"text","text":"append %d"}`, serial))
						exec(`UPDATE part SET data=? WHERE id='p100000-00'`, fmt.Sprintf(`{"type":"text","text":"edit %d"}`, serial))
					case "v2":
						exec(`INSERT INTO session_message VALUES(?,'ses_livebudget','assistant',?,?,?,?)`, id, serial, serial, serial, fmt.Sprintf(`{"content":[{"type":"text","text":"append %d"}]}`, serial))
						exec(`UPDATE session_message SET data=? WHERE id='m100000'`, fmt.Sprintf(`{"content":[{"type":"text","text":"edit %d"}]}`, serial))
					case "hermes":
						exec(`INSERT INTO messages(id,session_id,role,content,timestamp,active,compacted) VALUES(?,'live-budget','assistant',?,?,1,0)`, serial, fmt.Sprintf("append %d", serial), serial)
						exec(`UPDATE messages SET content=? WHERE id=100000`, fmt.Sprintf("edit %d", serial))
					}
					if err == nil {
						err = tx.Commit()
					} else {
						_ = tx.Rollback()
					}
					if err != nil {
						if ctx.Err() != nil {
							err = nil
						}
						done <- err
						return
					}
				}
			}()
			t.Cleanup(func() {
				cancel()
				if err := <-done; err != nil {
					t.Error(err)
				}
			})
			start, cpu := time.Now(), liveCPU()
			samples := []float64{}
			maxRows, edits, appends := 0, 0, 0
			ticker := time.NewTicker(250 * time.Millisecond)
			defer ticker.Stop()
			for tick := 0; tick < 20; tick++ {
				<-ticker.C
				executor.rows, executor.queries, executor.duration = 0, 0, 0
				page, reset := watch.Refresh(t.Context())
				if reset || page.State != BrowseReady {
					t.Fatalf("large refresh reset=%v page=%+v", reset, page)
				}
				for _, entry := range page.Entries {
					if strings.Contains(entry.Text, "edit ") {
						edits++
					}
					if strings.Contains(entry.Text, "append ") {
						appends++
					}
				}
				if executor.rows > databaseLiveLoaded+databaseLiveRows+databaseLiveAppend+100 {
					t.Fatalf("row budget exceeded: %d", executor.rows)
				}
				maxRows = max(maxRows, executor.rows)
				samples = append(samples, float64(executor.duration)/float64(time.Millisecond))
			}
			if edits == 0 || appends == 0 {
				t.Fatalf("active writer not observed: edits=%d appends=%d", edits, appends)
			}
			sort.Float64s(samples)
			plans := []string{}
			for plan := range executor.plans {
				plans = append(plans, plan)
			}
			sort.Strings(plans)
			t.Logf("format=%s messages=100000 parts_or_blocks=1000000 ticks=%d query_ms_p50=%.3f query_ms_p95=%.3f process_cpu_ms=%.3f wall_ms=%.3f max_returned_rows_with_parts_and_plans=%d source_row_budget=%d plans=%q", format, len(samples), samples[len(samples)/2], samples[len(samples)*95/100-1], float64(liveCPU()-cpu)/float64(time.Millisecond), float64(time.Since(start))/float64(time.Millisecond), maxRows, databaseLiveLoaded+databaseLiveRows+databaseLiveAppend+100, plans)
		})
	}
}
