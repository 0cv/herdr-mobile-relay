package sqliteexec

import (
	"context"
	"errors"
	"math"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/0cv/herdr-mobile-relay/internal/conversationlife"
)

var installedDenied = errors.New("logical acquisition denied")

// All boundaries default to logical denial BEFORE resolveInstalled is called.
// No field is filled from productionInstalledDeps, even for a guard-only test.
func deniedInstalledDeps() installedDeps {
	return installedDeps{
		nativeReady: func(context.Context) bool { return false },
		lookup:      func(string) (string, error) { return "", installedDenied },
		open:        func(string) (installedDB, error) { return nil, installedDenied },
		run:         func(context.Context, string, string, string, int) ([]byte, error) { return nil, installedDenied },
	}
}

type logicalRows struct {
	columns                                     []string
	values                                      [][]any
	index                                       int
	columnsErr, scanErr, iterationErr, closeErr error
	closes                                      int
	nextHook                                    func()
}

func (r *logicalRows) Columns() ([]string, error) { return r.columns, r.columnsErr }
func (r *logicalRows) Next() bool {
	if r.nextHook != nil {
		r.nextHook()
	}
	r.index++
	return r.index <= len(r.values)
}
func (r *logicalRows) Scan(dest ...any) error {
	if r.scanErr != nil {
		return r.scanErr
	}
	for i := range dest {
		*(dest[i].(*any)) = r.values[r.index-1][i]
	}
	return nil
}
func (r *logicalRows) Err() error   { return r.iterationErr }
func (r *logicalRows) Close() error { r.closes++; return r.closeErr }

type logicalDB struct {
	rows                        installedRows
	pingErr, queryErr, closeErr error
	closes, pings, queries, max int
	pingHook                    func(context.Context)
	queryHook                   func(context.Context)
	closeHook                   func()
}

func (d *logicalDB) SetMaxOpenConns(n int) { d.max = n }
func (d *logicalDB) PingContext(ctx context.Context) error {
	d.pings++
	if d.pingHook != nil {
		d.pingHook(ctx)
	}
	return d.pingErr
}
func (d *logicalDB) QueryContext(ctx context.Context, _ string) (installedRows, error) {
	d.queries++
	if d.queryHook != nil {
		d.queryHook(ctx)
	}
	return d.rows, d.queryErr
}
func (d *logicalDB) Close() error {
	d.closes++
	if d.closeHook != nil {
		d.closeHook()
	}
	return d.closeErr
}

func TestA1InstalledSelection(t *testing.T) {
	for _, name := range []string{"auto_native", "explicit_native", "cli", "auto_cli", "missing_cli", "missing_owner", "fenced_owner", "incomplete_seams", "invalid_auto", "empty_auto"} {
		t.Run(name, func(t *testing.T) {
			owner, other := conversationlife.New(), conversationlife.New()
			t.Cleanup(owner.Join)
			t.Cleanup(other.Join)
			deps := deniedInstalledDeps()
			readyCalls, lookups, opens, runs := 0, 0, 0, 0
			deps.nativeReady = func(context.Context) bool { readyCalls++; return name != "auto_cli" }
			deps.lookup = func(binary string) (string, error) {
				lookups++
				if binary != "selected-sql" {
					return "", installedDenied
				}
				if name == "missing_cli" {
					return "", installedDenied
				}
				return binary, nil
			}
			deps.open = func(string) (installedDB, error) { opens++; return nil, installedDenied }
			deps.run = func(context.Context, string, string, string, int) ([]byte, error) {
				runs++
				return nil, installedDenied
			}
			backend := BackendAuto
			selectedOwner := owner
			switch name {
			case "explicit_native":
				backend = BackendNative
			case "cli", "missing_cli":
				backend = BackendCLI
			case "missing_owner":
				selectedOwner = nil
			case "fenced_owner":
				owner.Begin()
			case "incomplete_seams":
				deps.open = nil
			case "invalid_auto":
				backend = Backend("not-a-backend")
			case "empty_auto":
				backend = ""
			}
			e, err := resolveInstalled(context.Background(), selectedOwner, backend, "selected-sql", deps)
			refused := name == "missing_owner" || name == "fenced_owner" || name == "incomplete_seams" || name == "missing_cli"
			if refused {
				if err == nil || e != nil {
					t.Error("factory did not refuse")
				}
				if name != "missing_cli" && (readyCalls != 0 || lookups != 0) {
					t.Error("effect before guard")
				}
			} else {
				if err != nil || e == nil {
					t.Fatal("logical factory failed", err)
				}
				if !e.OwnedBy(owner) || e.OwnedBy(other) {
					t.Error("owner identity")
				}
				want := BackendNative
				if name == "cli" || name == "auto_cli" {
					want = BackendCLI
				}
				if e.backend != want {
					t.Error("selection", e.backend, want)
				}
				if !e.Ready() {
					t.Error("logical ready false")
				}
			}
			if opens != 0 || runs != 0 {
				t.Error("construction acquired query resource")
			}
		})
	}
}

func TestA1InstalledNativeMatrix(t *testing.T) {
	names := []string{"open", "partial_open", "nil_db", "ping", "query", "partial_rows", "nil_rows", "columns", "scan", "iteration", "encoding", "output_limit", "cancel_before", "cancel_ping", "cancel_query", "cancel_next", "success", "empty", "empty_exact", "empty_limit", "json_exact", "json_short", "close_rows", "close_db", "close_both", "primary_close_siblings", "dsn_refusal", "zero_budget", "negative_budget"}
	for _, name := range names {
		t.Run(name, func(t *testing.T) {
			for _, cleanup := range []string{"clean", "uncertain"} {
				t.Run(cleanup, func(t *testing.T) {
					owner := conversationlife.New()
					ctx, cancel := context.WithCancel(context.Background())
					f := newInstalledFixture(t, owner)
					f.releases = append(f.releases, cancel)
					escape, _ := f.hold()
					cancelEntry := newInstalledCapture[struct{}]()
					callerObserved := newInstalledCapture[struct{}]()
					nextContext := newInstalledCapture[context.Context]()
					cancelHook := func(merged context.Context) {
						cancelEntry.notify(struct{}{})
						cancel()
						select {
						case <-merged.Done():
							callerObserved.notify(struct{}{})
						case <-escape:
						}
					}
					primary := errors.New("primary")
					rowClose := errors.New("row close")
					dbClose := errors.New("db close")
					rows := &logicalRows{columns: []string{"value"}, values: [][]any{{[]byte("<&>")}}}
					db := &logicalDB{rows: rows}
					deps := deniedInstalledDeps()
					opens := 0
					var dsn string
					deps.nativeReady = func(context.Context) bool { return true }
					deps.open = func(s string) (installedDB, error) { opens++; dsn = s; return db, nil }
					wantPrimary := error(nil)
					wantDB, wantRows := 1, 1
					budget := 1024
					wantJSON := `[{"value":"<&>"}]`
					switch name {
					case "open":
						deps.open = func(string) (installedDB, error) { opens++; return nil, primary }
						wantPrimary = primary
						wantDB = 0
						wantRows = 0
					case "partial_open":
						deps.open = func(string) (installedDB, error) { opens++; return db, primary }
						wantPrimary = primary
						wantRows = 0
					case "nil_db":
						deps.open = func(string) (installedDB, error) { opens++; return nil, nil }
						wantPrimary = ErrUnavailable
						wantDB = 0
						wantRows = 0
					case "ping":
						db.pingErr = primary
						wantPrimary = primary
						wantRows = 0
					case "query":
						db.rows = nil
						db.queryErr = primary
						wantPrimary = primary
						wantRows = 0
					case "partial_rows":
						db.queryErr = primary
						wantPrimary = primary
					case "nil_rows":
						db.rows = nil
						wantPrimary = ErrUnavailable
						wantRows = 0
					case "columns":
						rows.columnsErr = primary
						wantPrimary = primary
					case "scan":
						rows.scanErr = primary
						wantPrimary = primary
					case "iteration":
						rows.iterationErr = primary
						wantPrimary = primary
					case "encoding":
						rows.values = [][]any{{math.NaN()}}
					case "output_limit":
						budget = 1
						wantPrimary = ErrOutputLimit
					case "zero_budget":
						budget = 0
						wantPrimary = ErrOutputLimit
					case "negative_budget":
						budget = -1
						wantPrimary = ErrOutputLimit
					case "dsn_refusal":
						wantDB, wantRows = 0, 0
					case "cancel_before":
						wantPrimary = context.Canceled
						wantDB = 0
						wantRows = 0
					case "cancel_ping":
						db.pingHook = cancelHook
						wantPrimary = context.Canceled
						wantRows = 0
					case "cancel_query":
						db.queryHook = cancelHook
						wantPrimary = context.Canceled
					case "cancel_next":
						db.pingHook = nextContext.notify
						rows.nextHook = func() { cancelHook(nextContext.value) }
						wantPrimary = context.Canceled
					case "empty", "empty_exact":
						rows.values = nil
						wantJSON = "[]"
						budget = 2
					case "empty_limit":
						rows.values = nil
						budget = 1
						wantPrimary = ErrOutputLimit
					case "json_exact":
						budget = len(wantJSON)
					case "json_short":
						budget = len(wantJSON) - 1
						wantPrimary = ErrOutputLimit
					case "close_rows":
						rows.closeErr = rowClose
					case "close_db":
						db.closeErr = dbClose
					case "close_both":
						rows.closeErr = rowClose
						db.closeErr = dbClose
					case "primary_close_siblings":
						rows.scanErr = primary
						wantPrimary = primary
						rows.closeErr = rowClose
						db.closeErr = dbClose
					}
					if cleanup == "uncertain" {
						rows.closeErr = rowClose
						db.closeErr = dbClose
					}
					e, err := resolveInstalled(ctx, owner, BackendNative, "never-executed", deps)
					if err != nil {
						t.Fatal(err)
					}
					if name == "cancel_before" {
						cancel()
					}
					database := "/logical/database.sqlite"
					if name == "dsn_refusal" {
						database = ""
					}
					var data []byte
					returned := f.launch(func() { data, err = e.QueryJSON(ctx, database, "logical query", budget) })
					cancellation := name == "cancel_ping" || name == "cancel_query" || name == "cancel_next"
					if cancellation {
						f.require(t, cancelEntry.ch, "caller cancellation hook entry")
						// This observation MUST succeed before finish/Join can cancel
						// the owner. Fallback delivery cannot turn a miss into PASS.
						f.require(t, callerObserved.ch, "caller cancellation delivery before fallback")
					}
					f.require(t, returned, "native query return")
					f.finish()
					if cancellation && (cancelEntry.count.Load() != 1 || callerObserved.count.Load() != 1) {
						t.Error("unexpected cancellation hook/delivery count", cancelEntry.count.Load(), callerObserved.count.Load())
					}
					if name == "cancel_next" && nextContext.count.Load() != 1 {
						t.Error("unexpected Ping context captures", nextContext.count.Load())
					}
					if db.closes != wantDB || rows.closes != wantRows {
						t.Error("unwind counts", db.closes, rows.closes, wantDB, wantRows)
					}
					if wantPrimary != nil && !errors.Is(err, wantPrimary) {
						t.Error("primary identity lost", err)
					}
					uncertain := (rows.closeErr != nil && wantRows != 0) || (db.closeErr != nil && wantDB != 0)
					if errors.Is(err, ErrCleanupUncertain) != uncertain {
						t.Error("uncertainty projection", err)
					}
					if errors.Is(err, rowClose) || errors.Is(err, dbClose) {
						t.Error("raw cleanup duplicated in query error")
					}
					ledger := owner.Witnesses()
					if errors.Is(ledger, rowClose) != (rows.closeErr != nil && wantRows != 0) || errors.Is(ledger, dbClose) != (db.closeErr != nil && wantDB != 0) {
						t.Error("raw witnesses lost", ledger)
					}
					if errors.Is(ledger, primary) || errors.Is(ledger, context.Canceled) {
						t.Error("operation poisoned ledger")
					}
					if (name == "encoding" || name == "dsn_refusal") && err == nil {
						t.Error("encoding accepted NaN")
					}
					if err != nil && data != nil {
						t.Error("failed query retained bytes")
					}
					if wantPrimary == nil && !uncertain && name != "encoding" && name != "dsn_refusal" && (err != nil || string(data) != wantJSON) {
						t.Error("JSON compatibility", string(data), err)
					}
					if (name == "cancel_before" || name == "dsn_refusal") && opens != 0 {
						t.Error("cancelled acquisition")
					}
					if dsn != "" && (!strings.Contains(dsn, "mode=ro") || !strings.Contains(dsn, "query_only(1)")) {
						t.Error("readonly DSN", dsn)
					}
				})
			}
		})
	}
}

func TestA1InstalledCLIContainment(t *testing.T) {
	for _, name := range []string{"success", "failure", "cancelled", "missing_lookup", "stale", "foreign", "nil_scope", "late_root"} {
		t.Run(name, func(t *testing.T) {
			owner, other := conversationlife.New(), conversationlife.New()
			ctx, cancel := context.WithCancel(context.Background())
			t.Cleanup(func() { owner.Begin(); other.Begin(); cancel(); owner.Join(); other.Join() })
			deps := deniedInstalledDeps()
			lookups, runs := 0, 0
			deps.lookup = func(binary string) (string, error) {
				lookups++
				if name == "missing_lookup" {
					return "", installedDenied
				}
				return binary, nil
			}
			deps.run = func(_ context.Context, binary, database, query string, max int) ([]byte, error) {
				runs++
				if binary != "exact-binary" || database != "logical-db" || query != "logical-query" || max != 1 {
					return nil, installedDenied
				}
				if name == "failure" {
					return nil, installedDenied
				}
				return []byte("[]"), nil
			}
			e, err := resolveInstalled(ctx, owner, BackendCLI, "exact-binary", deps)
			if name == "missing_lookup" {
				if err == nil || e != nil || runs != 0 {
					t.Error("lookup escaped")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			var stale, foreign *conversationlife.Call
			owner.Do(ctx, func(c *conversationlife.Call) error { stale = c; return nil })
			other.Do(ctx, func(c *conversationlife.Call) error { foreign = c; return nil })
			before := lookups
			switch name {
			case "cancelled":
				cancel()
				_, err = e.QueryJSON(ctx, "logical-db", "logical-query", 0)
			case "stale":
				_, err = e.QueryJSONIn(stale, "logical-db", "logical-query", 0)
				if e.ReadyIn(stale) {
					t.Error("stale ready")
				}
			case "foreign":
				_, err = e.QueryJSONIn(foreign, "logical-db", "logical-query", 0)
				if e.ReadyIn(foreign) {
					t.Error("foreign ready")
				}
			case "nil_scope":
				_, err = e.QueryJSONIn(nil, "logical-db", "logical-query", 0)
				if e.ReadyIn(nil) {
					t.Error("nil ready")
				}
			case "late_root":
				owner.Begin()
				_, err = e.QueryJSON(ctx, "logical-db", "logical-query", 0)
				if e.Ready() {
					t.Error("late ready")
				}
			default:
				_, err = e.QueryJSON(ctx, "logical-db", "logical-query", 0)
			}
			if name == "success" {
				if err != nil || runs != 1 {
					t.Error("logical CLI success", err, runs)
				}
			} else if err == nil {
				t.Error("missing refusal")
			}
			if name != "success" && name != "failure" && runs != 0 {
				t.Error("guard invoked runner")
			}
			if lookups != before {
				t.Error("guard invoked lookup")
			}
			if owner.Witnesses() != nil {
				t.Error("CLI primary poisoned cleanup")
			}
		})
	}
}

func TestA1InstalledHeldQuery(t *testing.T) {
	owner := conversationlife.New()
	deps := deniedInstalledDeps()
	rows := &logicalRows{}
	db := &logicalDB{rows: rows}
	f := newInstalledFixture(t, owner)
	hold, release := f.hold()
	entered := newInstalledCapture[context.Context]()
	var result error
	db.pingHook = func(ctx context.Context) { entered.notify(ctx); <-hold }
	var permit atomic.Bool
	permit.Store(true)
	deps.open = func(string) (installedDB, error) {
		if !permit.CompareAndSwap(true, false) {
			return nil, installedDenied
		}
		return db, nil
	}
	e, err := resolveInstalled(context.Background(), owner, BackendNative, "denied", deps)
	if err != nil {
		t.Fatal(err)
	}
	returned := f.launch(func() {
		_, result = e.QueryJSON(context.Background(), "/logical/db", "logical", 32)
	})
	f.require(t, entered.ch, "held Ping entry")
	receipt := f.fence(t, owner)
	f.require(t, entered.value.Done(), "held Ping owner cancellation")
	var lateErr error
	late := f.launch(func() { _, lateErr = e.QueryJSON(context.Background(), "/logical/late", "logical", 32) })
	f.require(t, late, "late root return")
	countHeld := installedHeld(receipt)
	queryHeld := installedHeld(returned)
	release()
	f.require(t, returned, "held query return")
	f.require(t, receipt, "held query operation-zero completion")
	f.finish()
	if !errors.Is(lateErr, conversationlife.ErrRefused) {
		t.Error("late query accepted", lateErr)
	}
	if entered.count.Load() != 1 {
		t.Error("unexpected Ping entries", entered.count.Load())
	}
	if !countHeld || !queryHeld {
		t.Error("actual operation receipt completed while resource held")
	}
	if !errors.Is(result, context.Canceled) || db.closes != 1 || rows.closes != 0 {
		t.Error("held operation unwind", result, db.closes, rows.closes)
	}
}
