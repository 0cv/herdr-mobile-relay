package conversation

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"reflect"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/0cv/herdr-mobile-relay/internal/conversationlife"
	"github.com/0cv/herdr-mobile-relay/internal/sqliteexec"
)

// These unexecuted logical fixtures exercise only the shared scoped query binding, not a
// concrete SQL constructor or the still-unintegrated provider entry methods.
// Every operand and callback is planned per instance. No production defaults.
type providerSQLFixture struct {
	binding                           *installedProviderSQL
	readyCalls, queryCalls, unplanned int
	readyHook                         func(*conversationlife.Call) bool
	queryHook                         func(*conversationlife.Call) ([]byte, error)
}

func newProviderSQLFixture(owner *conversationlife.Owner) *providerSQLFixture {
	f := &providerSQLFixture{}
	f.binding = &installedProviderSQL{owner: owner, binary: "sqlite3"}
	f.binding.deps = installedProviderSQLDeps{
		ready: func(call *conversationlife.Call) bool {
			f.readyCalls++
			if call == nil {
				f.unplanned++
				return false
			}
			if f.readyHook != nil {
				return f.readyHook(call)
			}
			return true
		},
		query: func(call *conversationlife.Call, database, query string, max int) ([]byte, error) {
			f.queryCalls++
			if call == nil || database != "/planned/state.db" || query != "SELECT planned" || max != 1024 {
				f.unplanned++
				return nil, conversationlife.ErrRefused
			}
			if f.queryHook != nil {
				return f.queryHook(call)
			}
			return []byte("[]"), nil
		},
	}
	return f
}

func providerFixtureQuery(f *providerSQLFixture, parent *conversationlife.Call, binary string) ([]byte, string, error) {
	return f.binding.queryIn(parent, binary, "/planned/state.db", "SELECT planned", 1024, 10*time.Second)
}

func TestInstalledProviderSQLScope(t *testing.T) {
	for _, name := range []string{"nil", "zero", "foreign", "stale", "nil-owner", "zero-owner", "missing-ready", "missing-query", "binary-empty", "binary-other", "fenced-child"} {
		t.Run(name, func(t *testing.T) {
			owner := conversationlife.New()
			foreign := conversationlife.New()
			t.Cleanup(func() { owner.Begin(); foreign.Begin(); owner.Join(); foreign.Join() })
			f := newProviderSQLFixture(owner)
			binary := "sqlite3"
			var parent *conversationlife.Call
			var got error
			run := func() { _, _, got = providerFixtureQuery(f, parent, binary) }
			switch name {
			case "nil":
				run()
			case "zero":
				parent = &conversationlife.Call{}
				run()
			case "foreign":
				_ = foreign.Do(context.Background(), func(c *conversationlife.Call) error { parent = c; run(); return nil })
			case "stale":
				_ = owner.Do(context.Background(), func(c *conversationlife.Call) error { parent = c; return nil })
				run()
			default:
				_ = owner.Do(context.Background(), func(c *conversationlife.Call) error {
					parent = c
					switch name {
					case "nil-owner":
						f.binding.owner = nil
					case "zero-owner":
						f.binding.owner = &conversationlife.Owner{}
					case "missing-ready":
						f.binding.deps.ready = nil
					case "missing-query":
						f.binding.deps.query = nil
					case "binary-empty":
						binary = ""
					case "binary-other":
						binary = "/unplanned/sqlite3"
					case "fenced-child":
						owner.Begin()
					}
					run()
					return nil
				})
			}
			want := conversationlife.ErrRefused
			if name == "fenced-child" {
				want = context.Canceled
			}
			if !errors.Is(got, want) || f.readyCalls != 0 || f.queryCalls != 0 || f.unplanned != 0 {
				t.Fatalf("error=%v ready=%d query=%d unplanned=%d", got, f.readyCalls, f.queryCalls, f.unplanned)
			}
		})
	}
}

func TestInstalledProviderSQLResults(t *testing.T) {
	primary := errors.New("logical query failure")
	for _, name := range []string{"success", "empty", "unavailable", "failure", "output-limit", "cleanup", "primary-cleanup", "cancel-cleanup"} {
		t.Run(name, func(t *testing.T) {
			owner := conversationlife.New()
			t.Cleanup(owner.Join)
			f := newProviderSQLFixture(owner)
			var want error
			wantCode := ""
			switch name {
			case "empty":
				f.queryHook = func(*conversationlife.Call) ([]byte, error) { return nil, nil }
			case "unavailable":
				f.readyHook = func(*conversationlife.Call) bool { return false }
				wantCode = "source_unavailable"
			case "failure":
				want = primary
				wantCode = "query_failed"
			case "output-limit":
				want = sqliteexec.ErrOutputLimit
				wantCode = "output_limit"
			case "cleanup":
				want = sqliteexec.ErrCleanupUncertain
				wantCode = "query_failed"
			case "primary-cleanup":
				want = errors.Join(primary, sqliteexec.ErrCleanupUncertain)
				wantCode = "query_failed"
			case "cancel-cleanup":
				want = errors.Join(context.Canceled, sqliteexec.ErrCleanupUncertain)
				wantCode = "query_failed"
			}
			if want != nil {
				f.queryHook = func(*conversationlife.Call) ([]byte, error) { return []byte("partial"), want }
			}
			var raw []byte
			var code string
			var got error
			admission := owner.Do(context.Background(), func(c *conversationlife.Call) error {
				raw, code, got = providerFixtureQuery(f, c, "sqlite3")
				return nil
			})
			if admission != nil || code != wantCode || (want == nil && got != nil) || (want != nil && !errors.Is(got, want)) {
				t.Fatalf("admission=%v code=%q err=%v", admission, code, got)
			}
			if code == "" && string(raw) != "[]" {
				t.Fatalf("raw=%q", raw)
			}
			if code != "" && raw != nil {
				t.Fatal("partial data escaped")
			}
			wantQueries := 1
			if name == "unavailable" {
				wantQueries = 0
			}
			if f.readyCalls != 1 || f.queryCalls != wantQueries || f.unplanned != 0 {
				t.Fatalf("counts=%+v", f)
			}
			if owner.Witnesses() != nil {
				t.Fatal("query helper recorded SQL errors")
			}
		})
	}
}

func TestInstalledProviderSQLContext(t *testing.T) {
	for _, name := range []string{"deadline-values", "expired", "caller-ready", "owner-ready", "caller-query", "owner-query"} {
		t.Run(name, func(t *testing.T) {
			owner := conversationlife.New()
			type key struct{}
			ctx, cancel := context.WithCancel(context.WithValue(context.Background(), key{}, "planned"))
			t.Cleanup(func() { cancel(); owner.Join() })
			if name == "expired" {
				var stop context.CancelFunc
				ctx, stop = context.WithDeadline(ctx, time.Now().Add(-time.Second))
				defer stop()
			}
			f := newProviderSQLFixture(owner)
			start := time.Now()
			var observed error
			f.readyHook = func(call *conversationlife.Call) bool {
				deadline, ok := call.Context().Deadline()
				if !ok || deadline.Before(start.Add(10*time.Second)) || deadline.After(time.Now().Add(10*time.Second)) || call.Context().Value(key{}) != "planned" {
					observed = errors.New("query deadline/value contract")
				}
				if name == "caller-ready" {
					cancel()
				}
				if name == "owner-ready" {
					owner.Begin()
				}
				// Caller cancellation is observed synchronously through the actual
				// bridge before releasing the selected callback.
				if name == "caller-ready" || name == "owner-ready" {
					<-call.Context().Done()
				}
				return true
			}
			f.queryHook = func(call *conversationlife.Call) ([]byte, error) {
				if name == "caller-query" {
					cancel()
				}
				if name == "owner-query" {
					owner.Begin()
				}
				if name == "caller-query" || name == "owner-query" {
					<-call.Context().Done()
				}
				return []byte("[]"), nil
			}
			var raw []byte
			var got error
			admission := owner.Do(ctx, func(c *conversationlife.Call) error { raw, _, got = providerFixtureQuery(f, c, "sqlite3"); return nil })
			if admission != nil || observed != nil {
				t.Fatalf("admission=%v observation=%v", admission, observed)
			}
			if name == "deadline-values" {
				if got != nil || string(raw) != "[]" {
					t.Fatalf("raw=%q error=%v", raw, got)
				}
			} else {
				want := context.Canceled
				if name == "expired" {
					want = context.DeadlineExceeded
				}
				if !errors.Is(got, want) || raw != nil {
					t.Fatalf("raw=%q error=%v", raw, got)
				}
			}
			if (name == "caller-ready" || name == "owner-ready" || name == "expired") && f.queryCalls != 0 {
				t.Fatal("query after cancellation")
			}
			if name == "expired" && f.readyCalls != 0 {
				t.Fatal("readiness after expiration")
			}
			if f.unplanned != 0 {
				t.Fatal("unplanned effect")
			}
		})
	}
}

// Cancellation is real: Err observes and cancels a stdlib context at a selected
// synchronous parser checkpoint. It is not Call authority or a fake deadline.
type providerParseContext struct {
	context.Context
	cancel     context.CancelFunc
	checks, at int
}

func (c *providerParseContext) Err() error {
	c.checks++
	if c.checks == c.at {
		c.cancel()
	}
	return c.Context.Err()
}

func TestInstalledProviderParserCancellation(t *testing.T) {
	for _, name := range []string{"opencode-v1", "opencode-v2", "opencode-parts", "opencode-output", "hermes-rows", "hermes-tools", "hermes-blocks"} {
		t.Run(name, func(t *testing.T) {
			base, cancel := context.WithCancel(context.Background())
			defer cancel()
			ctx := &providerParseContext{Context: base, cancel: cancel, at: 3}
			var got error
			switch name {
			case "opencode-v1":
				rows := []openCodeRow{{MessageID: "one", MessageData: `{"role":"user"}`, PartID: "p", PartData: `{"type":"text","text":"one"}`}, {MessageID: "two"}}
				entries, _, err := parseOpenCodeRowsContext(ctx, rows)
				got = err
				if entries != nil {
					t.Fatal("partial rows")
				}
			case "opencode-v2":
				rows := []openCodeRow{{MessageID: "one", MessageType: "user", MessageData: `{"text":"one"}`}, {MessageID: "two"}}
				entries, _, err := parseOpenCodeV2RowsContext(ctx, rows)
				got = err
				if entries != nil {
					t.Fatal("partial rows")
				}
			case "opencode-parts":
				entry := Entry{Text: "prior"}
				_, got = parseOpenCodeV2ContentContext(ctx, &entry, []json.RawMessage{json.RawMessage(`{"type":"text","text":"one"}`), json.RawMessage(`{"type":"text","text":"two"}`)})
				if entry.Text != "prior" {
					t.Fatal("partial content publication")
				}
			case "opencode-output":
				text, err := openCodeV2ToolOutputContext(ctx, json.RawMessage(`[{"text":"one"},{"text":"two"}]`), nil)
				got = err
				if text != "" {
					t.Fatal("partial output")
				}
			case "hermes-rows":
				entries, _, err := parseHermesRowsContext(ctx, []hermesRow{{MessageID: 1, Role: "user", ContentHex: "61"}})
				got = err
				if entries != nil {
					t.Fatal("partial rows")
				}
			case "hermes-tools":
				tools, _, err := parseHermesToolCallsContext(ctx, `[{"id":"one","name":"read"},{"id":"two","name":"read"}]`)
				got = err
				if tools != nil {
					t.Fatal("partial tools")
				}
			case "hermes-blocks":
				text, err := providerTextValueContext(ctx, []any{map[string]any{"type": "text", "text": "one"}, map[string]any{"type": "text", "text": "two"}})
				got = err
				if text != "" {
					t.Fatal("partial blocks")
				}
			}
			if !errors.Is(got, context.Canceled) || ctx.checks < ctx.at {
				t.Fatalf("error=%v checks=%d", got, ctx.checks)
			}
		})
	}
}

func TestInstalledProviderParserCompatibility(t *testing.T) {
	for _, name := range []string{"opencode-v1", "opencode-v2", "hermes", "hermes-tool-forms", "text-value"} {
		t.Run(name, func(t *testing.T) {
			switch name {
			case "opencode-v1":
				rows := []openCodeRow{{MessageID: "m", MessageData: `{"role":"assistant"}`, PartID: "p", PartData: `{"type":"tool","tool":"shell","callID":"c","state":{"output":"ok","metadata":{"exit":2}}}`}}
				entries, corrupt, err := parseOpenCodeRowsContext(context.Background(), rows)
				if err != nil || corrupt || len(entries) != 1 || len(entries[0].Tools) != 1 || !entries[0].Tools[0].Error || entries[0].Tools[0].Output != "ok" {
					t.Fatalf("entries=%+v corrupt=%v err=%v", entries, corrupt, err)
				}
			case "opencode-v2":
				rows := []openCodeRow{{V2: true, MessageID: "m", MessageType: "assistant", MessageData: `{"content":[{"type":"text","text":"hello"},{"type":"tool","id":"c","name":"shell","state":{"content":[{"text":"one"},{"text":"two"}],"metadata":{"exit":1}}}]}`}}
				entries, corrupt, err := parseOpenCodeRowsContext(context.Background(), rows)
				if err != nil || corrupt || len(entries) != 1 || entries[0].Text != "hello" || len(entries[0].Tools) != 1 || entries[0].Tools[0].Output != "one\ntwo" || !entries[0].Tools[0].Error {
					t.Fatalf("entries=%+v corrupt=%v err=%v", entries, corrupt, err)
				}
			case "hermes":
				rows := []hermesRow{{MessageID: 7, Role: "assistant", ContentHex: hex.EncodeToString([]byte("hello")), ToolCalls: `[{"id":"c","function":{"name":"read","arguments":{"path":"x"}}}]`}}
				entries, corrupt, err := parseHermesRowsContext(context.Background(), rows)
				if err != nil || corrupt || len(entries) != 1 || entries[0].ID != "7" || entries[0].Text != "hello" || len(entries[0].Tools) != 1 || toolAssociationID(entries[0].Tools[0]) != "c" || entries[0].Tools[0].Input != `{"path":"x"}` {
					t.Fatalf("entries=%+v corrupt=%v err=%v", entries, corrupt, err)
				}
			case "hermes-tool-forms":
				// Three representations, not nested test identities.
				var first []ToolActivity
				for _, raw := range []string{`{"id":"c","name":"read"}`, `[{"id":"c","name":"read"}]`, `{"tool_calls":[{"id":"c","name":"read"}]}`} {
					tools, corrupt, err := parseHermesToolCallsContext(context.Background(), raw)
					if err != nil || corrupt || len(tools) != 1 {
						t.Fatalf("tools=%+v corrupt=%v err=%v", tools, corrupt, err)
					}
					if first == nil {
						first = tools
					} else if !reflect.DeepEqual(first, tools) {
						t.Fatal("tool representation drift")
					}
				}
			case "text-value":
				for _, value := range []any{nil, "plain", map[string]any{"a": 1}, []any{map[string]any{"type": "text", "text": "one"}, map[string]any{"type": "output_text", "text": "two"}}, []any{false}} {
					text, err := providerTextValueContext(context.Background(), value)
					if err != nil || text != textValue(value) {
						t.Fatalf("text=%q err=%v", text, err)
					}
				}
			}
		})
	}
}

func TestInstalledProviderSQLWitness(t *testing.T) {
	owner := conversationlife.New()
	t.Cleanup(owner.Join)
	f := newProviderSQLFixture(owner)
	rawClose := errors.New("logical SQL close")
	primary := errors.New("logical primary")
	closes := 0
	slot, err := owner.Own(func() error { closes++; return rawClose })
	if err != nil {
		t.Fatal(err)
	}
	f.queryHook = func(*conversationlife.Call) ([]byte, error) {
		_ = slot.Close()
		_ = slot.Close()
		return nil, errors.Join(primary, sqliteexec.ErrCleanupUncertain)
	}
	var got error
	_ = owner.Do(context.Background(), func(c *conversationlife.Call) error { _, _, got = providerFixtureQuery(f, c, "sqlite3"); return nil })
	if !errors.Is(got, primary) || !errors.Is(got, sqliteexec.ErrCleanupUncertain) || errors.Is(got, rawClose) {
		t.Fatalf("error=%v", got)
	}
	witnesses, ok := owner.Witnesses().(interface{ Unwrap() []error })
	if !ok || len(witnesses.Unwrap()) != 1 || witnesses.Unwrap()[0] != rawClose || closes != 1 {
		t.Fatal("duplicate or missing witness")
	}
}

func TestInstalledProviderSQLHeld(t *testing.T) {
	for _, name := range []string{"ready", "query"} {
		t.Run(name, func(t *testing.T) {
			owner := conversationlife.New()
			f := newProviderSQLFixture(owner)
			entered := make(chan struct{})
			release := make(chan struct{})
			done := make(chan struct{})
			var releaseOnce sync.Once
			releaseAll := func() { releaseOnce.Do(func() { close(release) }) }
			var got error
			calls := 0
			hold := func() {
				calls++
				if calls == 1 {
					close(entered)
				}
				<-release
			}
			if name == "ready" {
				f.readyHook = func(*conversationlife.Call) bool { hold(); return true }
			} else {
				f.queryHook = func(*conversationlife.Call) ([]byte, error) { hold(); return []byte("[]"), nil }
			}
			// Cleanup and actual completion exist before the sole sealed launch.
			// No observer has a work-entry prerequisite or owns the sole release.
			t.Cleanup(func() { owner.Begin(); releaseAll(); <-done; owner.Join() })
			go func() {
				defer close(done)
				got = owner.Do(context.Background(), func(c *conversationlife.Call) error { _, _, err := providerFixtureQuery(f, c, "sqlite3"); return err })
			}()
			select {
			case <-done:
				t.Fatal("caller exited before entry")
			case <-entered:
			}
			receipt := owner.Begin()
			timer := time.NewTimer(20 * time.Millisecond)
			defer timer.Stop()
			select {
			case <-done:
				t.Fatal("caller detached held work")
			case <-receipt:
				t.Fatal("fenced count retired held work")
			case <-timer.C:
			}
			// Release all holders, join actual work and bridges, then inspect.
			releaseAll()
			<-done
			owner.Join()
			if !errors.Is(got, context.Canceled) || calls != 1 || f.unplanned != 0 {
				t.Fatalf("error=%v calls=%d unplanned=%d", got, calls, f.unplanned)
			}
		})
	}
}

// Actual OpenCode receiver fixtures. Every effect is logical and per-instance;
// construction uses the same admitted initializer as the concrete production
// binder, without acquiring a concrete executor or touching native storage.
type openCodeFixtureInfo struct {
	size int64
	tick int64
}

func (i openCodeFixtureInfo) Name() string       { return "opencode.db" }
func (i openCodeFixtureInfo) Size() int64        { return i.size }
func (i openCodeFixtureInfo) Mode() os.FileMode  { return 0600 }
func (i openCodeFixtureInfo) ModTime() time.Time { return time.Unix(0, i.tick) }
func (i openCodeFixtureInfo) IsDir() bool        { return false }
func (i openCodeFixtureInfo) Sys() any           { return nil }

type openCodeActualFixture struct {
	owner       *conversationlife.Owner
	reader      *openCodeReader
	binding     *installedProviderSQL
	metadata    installedProviderMetadata
	queries     map[string][]byte
	queryErrors map[string]error
	stats       map[string]os.FileInfo
	evals       map[string]string
	counts      map[string]int
	unplanned   int
	queryHook   func(*conversationlife.Call, string)
	readyHook   func(*conversationlife.Call)
	statHook    func(string)
	deadlines   []time.Time
}

const openCodeFixtureDB = "/planned/opencode/opencode.db"
const openCodeFixtureLayout = `SELECT name FROM sqlite_master WHERE type='table' AND name IN ('session','message','part','session_v2','session_message');`

func newOpenCodeActualFixture(t *testing.T) *openCodeActualFixture {
	t.Helper()
	f := &openCodeActualFixture{owner: conversationlife.New(), queries: make(map[string][]byte), queryErrors: make(map[string]error), counts: make(map[string]int),
		stats: map[string]os.FileInfo{openCodeFixtureDB: openCodeFixtureInfo{100, 1}, openCodeFixtureDB + "-wal": openCodeFixtureInfo{20, 2}},
		evals: map[string]string{"/planned/opencode": "/planned/opencode", openCodeFixtureDB: openCodeFixtureDB}}
	t.Cleanup(f.owner.Join)
	f.binding = &installedProviderSQL{owner: f.owner, binary: "sqlite3", deps: installedProviderSQLDeps{
		ready: func(c *conversationlife.Call) bool {
			f.counts["ready"]++
			if f.readyHook != nil {
				f.readyHook(c)
			}
			return true
		},
		query: func(c *conversationlife.Call, db, query string, max int) ([]byte, error) {
			f.counts["query"]++
			raw, ok := f.queries[query]
			if db != openCodeFixtureDB || max != 8*1024*1024 || !ok {
				f.unplanned++
				return nil, conversationlife.ErrRefused
			}
			deadline, ok := c.Context().Deadline()
			if !ok || deadline.Before(time.Now().Add(9*time.Second)) || deadline.After(time.Now().Add(10*time.Second)) {
				f.unplanned++
				return nil, conversationlife.ErrRefused
			}
			f.deadlines = append(f.deadlines, deadline)
			if f.queryHook != nil {
				f.queryHook(c, query)
			}
			return raw, f.queryErrors[query]
		},
	}}
	f.metadata = installedProviderMetadata{
		roots: func(ctx context.Context, home string) ([]string, error) {
			f.counts["roots"]++
			if home != "/planned/home" {
				f.unplanned++
				return nil, conversationlife.ErrRefused
			}
			return []string{"/planned/opencode"}, ctx.Err()
		},
		databases: func(ctx context.Context, home string) ([]string, error) {
			f.counts["databases"]++
			if home != "/planned/home" {
				f.unplanned++
				return nil, conversationlife.ErrRefused
			}
			return []string{openCodeFixtureDB}, ctx.Err()
		},
		eval: func(path string) (string, error) {
			f.counts["eval"]++
			value, ok := f.evals[path]
			if !ok {
				f.unplanned++
				return "", conversationlife.ErrRefused
			}
			return value, nil
		},
		stat: func(path string) (os.FileInfo, error) {
			f.counts["stat"]++
			value, ok := f.stats[path]
			if !ok {
				f.unplanned++
				return nil, conversationlife.ErrRefused
			}
			if f.statHook != nil {
				f.statHook(path)
			}
			if value == nil {
				return nil, os.ErrNotExist
			}
			return value, nil
		},
	}
	err := f.owner.Do(context.Background(), func(c *conversationlife.Call) error {
		var err error
		f.reader, err = constructInstalledOpenCodeIn(c, "/planned/home", f.binding, f.metadata)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	return f
}

func (f *openCodeActualFixture) plan(t *testing.T, v1, v2 bool, before string, limit int) {
	t.Helper()
	var tables []map[string]string
	if v1 {
		for _, name := range []string{"session", "message", "part"} {
			tables = append(tables, map[string]string{"name": name})
		}
	}
	if v2 {
		for _, name := range []string{"session_v2", "session_message"} {
			tables = append(tables, map[string]string{"name": name})
		}
	}
	f.queries[openCodeFixtureLayout], _ = json.Marshal(tables)
	// Independently specified SQL invariants supplement exact table membership.
	// These are logical shape assertions, not SQL-engine qualification.
	for _, version := range []bool{false, true} {
		query := openCodeV1SQL("ses_test", before, limit)
		fragments := []string{"FROM message AS m", "ORDER BY m.time_created DESC,m.id DESC LIMIT " + fmt.Sprint(limit+1), "LEFT JOIN part AS p ON p.message_id=sm.id", "ORDER BY sm.time_created DESC,sm.id DESC,p.id DESC;"}
		if version {
			query = openCodeV2SQL("ses_test", before, limit)
			fragments = []string{"FROM session_message AS m", "m.type IN ('user','assistant')", "ORDER BY m.seq DESC LIMIT " + fmt.Sprint(limit+1), "AS has_messages", "ORDER BY sm.seq DESC;"}
		}
		fragments = append(fragments, "CAST(X'7365735f74657374' AS TEXT)", "AS message_total", "AS cursor_found")
		if before != "" {
			fragments = append(fragments, "EXISTS(SELECT 1 FROM cursor)", "CAST(X'"+hex.EncodeToString([]byte(before))+"' AS TEXT)")
		}
		for _, fragment := range fragments {
			if !strings.Contains(query, fragment) {
				t.Fatalf("missing SQL contract %q", fragment)
			}
		}
		f.queries[query] = []byte("[]")
	}
}

func (f *openCodeActualFixture) setRows(v2 bool, before string, limit int, rows []openCodeRow) {
	query := openCodeV1SQL("ses_test", before, limit)
	if v2 {
		query = openCodeV2SQL("ses_test", before, limit)
	}
	f.queries[query], _ = json.Marshal(rows)
}

func openCodeFixtureRows(v2 bool) []openCodeRow {
	rows := []openCodeRow{
		{SessionID: "ses_test", MessageID: "new", MessageType: "user", MessageData: `{"text":"new","role":"user"}`, PartID: "p2", PartData: `{"type":"text","text":"new"}`, Total: 3, CursorFound: 1, HasMessages: 1},
		{SessionID: "ses_test", MessageID: "middle", MessageType: "user", MessageData: `{"text":"middle","role":"user"}`, PartID: "p1", PartData: `{"type":"text","text":"middle"}`, Total: 3, CursorFound: 1, HasMessages: 1},
		{SessionID: "ses_test", MessageID: "old", MessageType: "user", MessageData: `{"text":"old","role":"user"}`, PartID: "p0", PartData: `{"type":"text","text":"old"}`, Total: 3, CursorFound: 1, HasMessages: 1},
	}
	if !v2 {
		rows = append(rows[:1], append([]openCodeRow{rows[0]}, rows[1:]...)...)
	}
	return rows
}

func TestInstalledOpenCodeActualFlows(t *testing.T) {
	for _, name := range []string{"v1", "v2", "mixed-v2", "mixed-v1", "empty", "no-layout", "corrupt-layout", "corrupt-rows", "cursor", "missing-cursor", "invalid-session", "invalid-cursor", "output-limit", "cleanup"} {
		t.Run(name, func(t *testing.T) {
			f := newOpenCodeActualFixture(t)
			v2 := name == "v2" || name == "mixed-v2" || name == "mixed-v1"
			before := ""
			if name == "cursor" || name == "missing-cursor" {
				before = "later"
			}
			f.plan(t, name != "v2", v2, before, 2)
			rows := openCodeFixtureRows(v2)
			if name == "missing-cursor" {
				for i := range rows {
					rows[i].CursorFound = 0
				}
			}
			f.setRows(v2, before, 2, rows)
			if name == "mixed-v1" {
				f.setRows(true, before, 2, []openCodeRow{{SessionID: "ses_test"}})
				f.setRows(false, before, 2, openCodeFixtureRows(false))
			}
			query := openCodeV1SQL("ses_test", before, 2)
			if v2 {
				query = openCodeV2SQL("ses_test", before, 2)
			}
			session := "ses_test"
			wantCode := ""
			switch name {
			case "empty":
				f.queries[query] = []byte("[]")
				wantCode = "invalid_session"
			case "no-layout":
				f.queries[openCodeFixtureLayout] = []byte("[]")
				wantCode = "query_failed"
			case "corrupt-layout":
				f.queries[openCodeFixtureLayout] = []byte("{")
				wantCode = "source_corrupt"
			case "corrupt-rows":
				f.queries[query] = []byte("{")
				wantCode = "source_corrupt"
			case "missing-cursor":
				wantCode = "invalid_cursor"
			case "invalid-session":
				session = "bad"
				wantCode = "invalid_session"
			case "invalid-cursor":
				before = "\n"
				wantCode = "invalid_cursor"
			case "output-limit":
				f.queryErrors[query] = sqliteexec.ErrOutputLimit
				wantCode = "output_limit"
			case "cleanup":
				f.queryErrors[query] = sqliteexec.ErrCleanupUncertain
				wantCode = "query_failed"
			}
			var entries []Entry
			var more, corrupt bool
			var metadata openCodeRow
			var code string
			err := f.owner.Do(context.Background(), func(c *conversationlife.Call) error {
				var err error
				entries, more, corrupt, metadata, code, err = f.reader.readIn(c, session, before, 2)
				return err
			})
			if name == "cleanup" {
				if !errors.Is(err, sqliteexec.ErrCleanupUncertain) {
					t.Fatal(err)
				}
			} else if name == "output-limit" {
				if !errors.Is(err, sqliteexec.ErrOutputLimit) {
					t.Fatal(err)
				}
			} else if err != nil {
				t.Fatal(err)
			}
			if code != wantCode || f.unplanned != 0 {
				t.Fatalf("code=%q want=%q unplanned=%d", code, wantCode, f.unplanned)
			}
			if code == "" && (len(entries) != 2 || entries[0].ID != "middle" || entries[1].ID != "new" || !more || corrupt || metadata.Total != 3 || metadata.Database != openCodeFixtureDB) {
				t.Fatalf("entries=%+v more=%v corrupt=%v metadata=%+v", entries, more, corrupt, metadata)
			}
			if name == "mixed-v2" && f.counts["query"] != 2 {
				t.Fatal("unexpected V1 fallback")
			}
			if name == "mixed-v1" && f.counts["query"] != 3 {
				t.Fatal("missing true V1 fallback")
			}
			if wantCode != "" && len(f.reader.cache) != 0 && name != "missing-cursor" && name != "empty" {
				t.Fatal("failure cached")
			}
			if name == "invalid-session" || name == "invalid-cursor" {
				if len(f.counts) != 0 {
					t.Fatal("invalid argument effects")
				}
			}
		})
	}
}

func TestInstalledOpenCodeActualAdmission(t *testing.T) {
	for _, name := range []string{"nil", "zero", "foreign", "stale", "ordinary", "binary-empty", "binary-other", "missing-roots", "missing-databases", "missing-eval", "missing-stat", "missing-ready", "missing-query", "missing-cache", "missing-layouts", "fenced-child", "nil-executor"} {
		t.Run(name, func(t *testing.T) {
			f := newOpenCodeActualFixture(t)
			foreign := conversationlife.New()
			t.Cleanup(foreign.Join)
			var parent *conversationlife.Call
			var got error
			run := func() { _, _, got = f.reader.databasesIn(parent) }
			switch name {
			case "nil":
				run()
			case "zero":
				parent = &conversationlife.Call{}
				run()
			case "foreign":
				_ = foreign.Do(context.Background(), func(c *conversationlife.Call) error { parent = c; run(); return nil })
			case "stale":
				_ = f.owner.Do(context.Background(), func(c *conversationlife.Call) error { parent = c; return nil })
				run()
			default:
				_ = f.owner.Do(context.Background(), func(c *conversationlife.Call) error {
					parent = c
					switch name {
					case "ordinary":
						f.reader = &openCodeReader{}
					case "binary-empty":
						f.reader.binary = ""
					case "binary-other":
						f.reader.binary = "other"
					case "missing-roots":
						f.reader.metadata.roots = nil
					case "missing-databases":
						f.reader.metadata.databases = nil
					case "missing-eval":
						f.reader.metadata.eval = nil
					case "missing-stat":
						f.reader.metadata.stat = nil
					case "missing-ready":
						f.binding.deps.ready = nil
					case "missing-query":
						f.binding.deps.query = nil
					case "missing-cache":
						f.reader.cache = nil
					case "missing-layouts":
						f.reader.layouts = nil
					case "fenced-child":
						f.owner.Begin()
					case "nil-executor":
						_, got = newInstalledOpenCodeReaderIn(f.owner, c, "/planned/home", nil)
						return nil
					}
					run()
					return nil
				})
			}
			want := conversationlife.ErrRefused
			if name == "fenced-child" {
				want = context.Canceled
			}
			if !errors.Is(got, want) || len(f.counts) != 0 || f.unplanned != 0 {
				t.Fatalf("err=%v counts=%v", got, f.counts)
			}
		})
	}
}

func TestInstalledOpenCodeActualLegacyFence(t *testing.T) {
	for _, name := range []string{"read", "read-context", "databases", "query", "query-context", "layout", "v1", "v2", "rows", "cache-hit", "cache-store", "query-exec"} {
		t.Run(name, func(t *testing.T) {
			f := newOpenCodeActualFixture(t)
			f.owner.Begin()
			stamp := openCodeFileStamp{databaseSize: 100, databaseTime: 1, walSize: 20, walTime: 2}
			f.reader.cache["seed"] = openCodeCacheEntry{stamp: stamp, rows: []openCodeRow{{MessageID: "good"}}}
			ctx := context.Background()
			switch name {
			case "read":
				_, _, _, _, code := f.reader.read("bad", "", 2)
				if code == "" {
					t.Fatal("success")
				}
			case "read-context":
				_, _, _, _, code := f.reader.readContext(ctx, "bad", "", 2)
				if code == "" {
					t.Fatal("success")
				}
			case "databases":
				_, code := f.reader.databases()
				if code == "" {
					t.Fatal("success")
				}
			case "query":
				_, _, code := f.reader.query(openCodeFixtureDB, "ses_test", "", 2)
				if code == "" {
					t.Fatal("success")
				}
			case "query-context":
				_, _, code := f.reader.queryContext(ctx, openCodeFixtureDB, "ses_test", "", 2)
				if code == "" {
					t.Fatal("success")
				}
			case "layout":
				_, code := f.reader.layoutContext(ctx, openCodeFixtureDB, stamp, true)
				if code == "" {
					t.Fatal("success")
				}
			case "v1":
				_, _, code := f.reader.queryV1(ctx, openCodeFixtureDB, "ses_test", "", 2)
				if code == "" {
					t.Fatal("success")
				}
			case "v2":
				_, _, code := f.reader.queryV2(ctx, openCodeFixtureDB, "ses_test", "", 2)
				if code == "" {
					t.Fatal("success")
				}
			case "rows":
				_, _, code := f.reader.runRows(ctx, openCodeFixtureDB, "unplanned", 2, false)
				if code == "" {
					t.Fatal("success")
				}
			case "cache-hit":
				_, _, hit := f.reader.cachedQuery("seed", stamp)
				if hit {
					t.Fatal("unowned hit")
				}
			case "cache-store":
				f.reader.storeQuery("seed", stamp, nil, false)
			case "query-exec":
				if f.reader.queryExec() != nil {
					t.Fatal("ordinary executor escape")
				}
			}
			if len(f.counts) != 0 || f.reader.cache["seed"].rows[0].MessageID != "good" {
				t.Fatal("fenced effect/publication")
			}
		})
	}
}

func TestInstalledOpenCodeActualCacheCancellation(t *testing.T) {
	for _, stage := range []string{"cache-hit", "cache-copy", "cache-publish", "layout-hit", "layout-publish", "layout-row", "decode", "reverse", "version", "distinct", "filter"} {
		t.Run(stage, func(t *testing.T) {
			f := newOpenCodeActualFixture(t)
			f.plan(t, true, false, "", 2)
			f.setRows(false, "", 2, openCodeFixtureRows(false))
			seed := openCodeCacheEntry{rows: []openCodeRow{{MessageID: "good"}}}
			stamp := openCodeFileStamp{databaseSize: 100, databaseTime: 1, walSize: 20, walTime: 2}
			if stage == "cache-hit" || stage == "cache-copy" {
				seed.stamp = stamp
			}
			if stage == "layout-hit" {
				f.reader.layouts[openCodeFixtureDB] = openCodeLayoutEntry{stamp: stamp, layout: openCodeLayout{v1: true}}
			}
			key := fmt.Sprintf("%s\x00ses_test\x00\x002", openCodeFixtureDB)
			f.reader.cache[key] = seed
			calls := 0
			f.reader.observe = func(at string) {
				if at == stage {
					calls++
					f.owner.Begin()
				}
			}
			var rows []openCodeRow
			err := f.owner.Do(context.Background(), func(c *conversationlife.Call) error {
				var err error
				rows, _, _, err = f.reader.queryIn(c, openCodeFixtureDB, "ses_test", "", 2)
				return err
			})
			if !errors.Is(err, context.Canceled) || rows != nil || calls != 1 || f.unplanned != 0 {
				t.Fatalf("err=%v rows=%v calls=%d", err, rows, calls)
			}
			if !reflect.DeepEqual(f.reader.cache[key], seed) {
				t.Fatal("cancelled publication replaced good entry")
			}
		})
	}
}

func TestInstalledOpenCodeActualCache(t *testing.T) {
	f := newOpenCodeActualFixture(t)
	f.plan(t, true, false, "", 2)
	f.setRows(false, "", 2, openCodeFixtureRows(false))
	query := func() []openCodeRow {
		t.Helper()
		var rows []openCodeRow
		err := f.owner.Do(context.Background(), func(c *conversationlife.Call) error {
			var err error
			rows, _, _, err = f.reader.queryIn(c, openCodeFixtureDB, "ses_test", "", 2)
			return err
		})
		if err != nil {
			t.Fatal(err)
		}
		return rows
	}
	rows := query()
	rows[0].MessageID = "mutated"
	if got := query(); got[0].MessageID == "mutated" || f.counts["query"] != 2 {
		t.Fatal("cache isolation/hit")
	}
	f.stats[openCodeFixtureDB+"-wal"] = openCodeFixtureInfo{21, 3}
	query()
	if f.counts["query"] != 4 {
		t.Fatal("WAL invalidation")
	}
	for i := 0; i < maxOpenCodeCache; i++ {
		f.reader.cache[fmt.Sprint(i)] = openCodeCacheEntry{}
		f.reader.layouts[fmt.Sprint(i)] = openCodeLayoutEntry{}
	}
	f.stats[openCodeFixtureDB] = openCodeFixtureInfo{101, 4}
	query()
	if len(f.reader.cache) != 1 || len(f.reader.layouts) != 1 || f.unplanned != 0 {
		t.Fatal("bounded cache clear")
	}
}

func TestInstalledOpenCodeActualHeld(t *testing.T) {
	for _, name := range []string{"ready", "query", "metadata"} {
		t.Run(name, func(t *testing.T) {
			f := newOpenCodeActualFixture(t)
			f.plan(t, true, false, "", 2)
			entered, release, done := make(chan struct{}), make(chan struct{}), make(chan struct{})
			var once sync.Once
			calls := 0
			var got error
			releaseAll := func() { once.Do(func() { close(release) }) }
			hold := func() {
				calls++
				if calls == 1 {
					close(entered)
				}
				<-release
			}
			switch name {
			case "ready":
				f.readyHook = func(*conversationlife.Call) { hold() }
			case "query":
				f.queryHook = func(*conversationlife.Call, string) { hold() }
			case "metadata":
				f.statHook = func(string) { hold() }
			}
			// No observers or unretirable pre-entry prerequisites. Cleanup is
			// registered before the sole sealed launch and releases before joins.
			t.Cleanup(func() { f.owner.Begin(); releaseAll(); <-done; f.owner.Join() })
			go func() {
				defer close(done)
				got = f.owner.Do(context.Background(), func(c *conversationlife.Call) error {
					_, _, _, _, _, err := f.reader.readIn(c, "ses_test", "", 2)
					return err
				})
			}()
			select {
			case <-done:
				t.Fatal("caller exited before work entry")
			case <-entered:
			}
			receipt := f.owner.Begin()
			timer := time.NewTimer(20 * time.Millisecond)
			defer timer.Stop()
			select {
			case <-done:
				t.Fatal("detached actual work")
			case <-receipt:
				t.Fatal("held work lost occupancy")
			case <-timer.C:
			}
			releaseAll()
			<-done
			f.owner.Join()
			if !errors.Is(got, context.Canceled) || calls != 1 || f.unplanned != 0 || len(f.reader.cache) != 0 {
				t.Fatalf("err=%v calls=%d", got, calls)
			}
		})
	}
}

func TestInstalledOpenCodeActualCancellation(t *testing.T) {
	for _, name := range []string{"caller-ready", "caller-layout", "caller-message", "owner-message", "expired", "cleanup-primary", "cleanup-cancel"} {
		t.Run(name, func(t *testing.T) {
			f := newOpenCodeActualFixture(t)
			f.plan(t, true, true, "", 2)
			ctx, cancel := context.WithCancel(context.Background())
			t.Cleanup(cancel)
			if name == "expired" {
				var stop context.CancelFunc
				ctx, stop = context.WithDeadline(ctx, time.Now().Add(-time.Second))
				t.Cleanup(stop)
			}
			primary := errors.New("logical primary")
			closeErr := errors.New("logical close")
			closes := 0
			slot, err := f.owner.Own(func() error { closes++; return closeErr })
			if err != nil {
				t.Fatal(err)
			}
			selected := 0
			f.readyHook = func(c *conversationlife.Call) {
				if name == "caller-ready" {
					selected++
					cancel()
					<-c.Context().Done()
				}
			}
			f.queryHook = func(c *conversationlife.Call, query string) {
				if (name == "caller-layout" && query == openCodeFixtureLayout) || ((name == "caller-message" || name == "owner-message") && query != openCodeFixtureLayout) {
					selected++
					if name == "owner-message" {
						f.owner.Begin()
					} else {
						cancel()
					}
					<-c.Context().Done()
				}
				if strings.HasPrefix(name, "cleanup-") && query != openCodeFixtureLayout {
					selected++
					_ = slot.Close()
					_ = slot.Close()
				}
			}
			v2Query := openCodeV2SQL("ses_test", "", 2)
			if name == "cleanup-primary" {
				f.queryErrors[v2Query] = errors.Join(primary, sqliteexec.ErrCleanupUncertain)
			}
			if name == "cleanup-cancel" {
				f.queryErrors[v2Query] = errors.Join(context.Canceled, sqliteexec.ErrCleanupUncertain)
			}
			var entries []Entry
			err = f.owner.Do(ctx, func(c *conversationlife.Call) error {
				var err error
				entries, _, _, _, _, err = f.reader.readIn(c, "ses_test", "", 2)
				return err
			})
			want := context.Canceled
			if name == "expired" {
				want = context.DeadlineExceeded
			}
			if name == "cleanup-primary" {
				want = primary
			}
			if !errors.Is(err, want) || entries != nil || len(f.reader.cache) != 0 || f.unplanned != 0 {
				t.Fatalf("err=%v counts=%v", err, f.counts)
			}
			if name == "expired" {
				if len(f.counts) != 0 || selected != 0 {
					t.Fatal("expired effects")
				}
			} else if selected != 1 {
				t.Fatal("selected cancellation/failure not observed")
			}
			if strings.HasPrefix(name, "cleanup-") {
				witnesses, ok := f.owner.Witnesses().(interface{ Unwrap() []error })
				if !errors.Is(err, sqliteexec.ErrCleanupUncertain) || errors.Is(err, closeErr) || !ok || len(witnesses.Unwrap()) != 1 || witnesses.Unwrap()[0] != closeErr || closes != 1 {
					t.Fatal("witness duplication/loss")
				}
			}
			if f.counts["query"] > 2 {
				t.Fatal("cancelled/uncertain V1 fallback")
			}
		})
	}
}

func TestInstalledOpenCodeActualParentRetirement(t *testing.T) {
	f := newOpenCodeActualFixture(t)
	f.plan(t, true, false, "", 2)
	parentReady, entered, releaseParent, releaseWork := make(chan struct{}), make(chan struct{}), make(chan struct{}), make(chan struct{})
	parentDone, childDone, parentCancelled := make(chan struct{}), make(chan struct{}), make(chan struct{})
	var parent *conversationlife.Call
	var parentErr, childErr error
	var parentOnce, workOnce sync.Once
	retireParent := func() { parentOnce.Do(func() { close(releaseParent) }) }
	releaseAll := func() { retireParent(); workOnce.Do(func() { close(releaseWork) }) }
	calls := 0
	f.queryHook = func(c *conversationlife.Call, _ string) {
		calls++
		if calls == 1 {
			close(entered)
		}
		select {
		case <-releaseWork:
			return
		case <-c.Context().Done():
		}
		if calls == 1 {
			close(parentCancelled)
		}
		<-releaseWork
	}
	// Both jobs/completions and cleanup are registered before either launch.
	// The child can independently retire its parent-ready prerequisite via
	// releaseWork, so a failing parent cannot strand cleanup.
	t.Cleanup(func() { f.owner.Begin(); releaseAll(); <-parentDone; <-childDone; f.owner.Join() })
	go func() {
		defer close(parentDone)
		parentErr = f.owner.Do(context.Background(), func(c *conversationlife.Call) error { parent = c; close(parentReady); <-releaseParent; return nil })
	}()
	go func() {
		defer close(childDone)
		select {
		case <-releaseWork:
			return
		case <-parentReady:
		}
		_, _, _, childErr = f.reader.queryIn(parent, openCodeFixtureDB, "ses_test", "", 2)
	}()
	select {
	case <-parentDone:
		t.Fatal("parent exited before entry")
	case <-childDone:
		t.Fatal("child exited before entry")
	case <-entered:
	}
	retireParent()
	<-parentDone
	select {
	case <-childDone:
		t.Fatal("child returned before held cancellation observation")
	case <-parentCancelled:
	}
	// Parent retirement cancels the independently entered child without
	// retiring its pin while the synchronous query callback is still held.
	receipt := f.owner.Begin()
	timer := time.NewTimer(20 * time.Millisecond)
	defer timer.Stop()
	select {
	case <-childDone:
		t.Fatal("child detached")
	case <-receipt:
		t.Fatal("child pin lost")
	case <-timer.C:
	}
	releaseAll()
	<-childDone
	f.owner.Join()
	if parentErr != nil || !errors.Is(childErr, context.Canceled) || calls != 1 || f.unplanned != 0 {
		t.Fatalf("parent=%v child=%v calls=%d", parentErr, childErr, calls)
	}
}

func TestInstalledOpenCodeActualMetadata(t *testing.T) {
	for _, name := range []string{"ordered", "outside", "missing-db", "missing-wal", "cancel-roots", "cancel-stat", "next-db-cancel"} {
		t.Run(name, func(t *testing.T) {
			f := newOpenCodeActualFixture(t)
			f.plan(t, true, false, "", 2)
			const otherRoot = "/planned/other"
			const otherDB = otherRoot + "/opencode.db"
			f.evals[otherRoot], f.evals[otherDB] = otherRoot, otherDB
			f.stats[otherDB], f.stats[otherDB+"-wal"] = openCodeFixtureInfo{200, 2}, nil
			f.reader.metadata.roots = func(ctx context.Context, home string) ([]string, error) {
				f.counts["roots"]++
				if home != "/planned/home" {
					f.unplanned++
					return nil, conversationlife.ErrRefused
				}
				if name == "cancel-roots" {
					f.owner.Begin()
				}
				return []string{"/planned/opencode", otherRoot}, ctx.Err()
			}
			f.reader.metadata.databases = func(ctx context.Context, home string) ([]string, error) {
				f.counts["databases"]++
				if home != "/planned/home" {
					f.unplanned++
					return nil, conversationlife.ErrRefused
				}
				return []string{openCodeFixtureDB, otherDB}, ctx.Err()
			}
			if name == "outside" {
				f.evals[openCodeFixtureDB] = "/outside/opencode.db"
			}
			if name == "missing-db" {
				f.stats[openCodeFixtureDB] = nil
			}
			if name == "missing-wal" {
				f.stats[openCodeFixtureDB+"-wal"] = nil
			}
			if name == "cancel-stat" {
				f.statHook = func(string) { f.owner.Begin() }
			}
			var paths []string
			var got error
			if name == "next-db-cancel" {
				f.queryHook = func(c *conversationlife.Call, _ string) { f.owner.Begin(); <-c.Context().Done() }
				got = f.owner.Do(context.Background(), func(c *conversationlife.Call) error {
					_, _, _, _, _, err := f.reader.readIn(c, "ses_test", "", 2)
					return err
				})
			} else {
				got = f.owner.Do(context.Background(), func(c *conversationlife.Call) error {
					var err error
					paths, _, err = f.reader.databasesIn(c)
					return err
				})
			}
			if strings.HasPrefix(name, "cancel-") || name == "next-db-cancel" {
				if !errors.Is(got, context.Canceled) {
					t.Fatal(got)
				}
			} else {
				if got != nil {
					t.Fatal(got)
				}
				want := []string{openCodeFixtureDB, otherDB}
				if name == "outside" || name == "missing-db" {
					want = []string{otherDB}
				}
				if !reflect.DeepEqual(paths, want) {
					t.Fatalf("paths=%v", paths)
				}
				if name == "missing-wal" {
					got = f.owner.Do(context.Background(), func(c *conversationlife.Call) error {
						_, _, _, err := f.reader.queryIn(c, openCodeFixtureDB, "ses_test", "", 2)
						return err
					})
					if got != nil {
						t.Fatal(got)
					}
					for _, entry := range f.reader.cache {
						if entry.stamp.walSize != 0 || entry.stamp.walTime != 0 {
							t.Fatal("missing WAL stamp")
						}
					}
				}
			}
			if f.unplanned != 0 {
				t.Fatal("unplanned fallback")
			}
		})
	}
}

func TestInstalledOpenCodeActualConstruction(t *testing.T) {
	for _, name := range []string{"nil-binding", "nil-owner", "zero-owner", "nil-parent", "zero-parent", "stale-parent", "foreign-parent", "fenced-child", "missing-roots", "missing-databases", "missing-eval", "missing-stat", "missing-ready", "missing-query"} {
		t.Run(name, func(t *testing.T) {
			f := newOpenCodeActualFixture(t)
			foreign := conversationlife.New()
			t.Cleanup(foreign.Join)
			binding, metadata := f.binding, f.metadata
			var result *openCodeReader
			var got error
			var parent *conversationlife.Call
			run := func() { result, got = constructInstalledOpenCodeIn(parent, "/planned/home", binding, metadata) }
			switch name {
			case "nil-parent":
				run()
			case "zero-parent":
				parent = &conversationlife.Call{}
				run()
			case "stale-parent":
				_ = f.owner.Do(context.Background(), func(c *conversationlife.Call) error { parent = c; return nil })
				run()
			case "foreign-parent":
				_ = foreign.Do(context.Background(), func(c *conversationlife.Call) error { parent = c; run(); return nil })
			default:
				_ = f.owner.Do(context.Background(), func(c *conversationlife.Call) error {
					parent = c
					switch name {
					case "nil-binding":
						binding = nil
					case "nil-owner":
						binding.owner = nil
					case "zero-owner":
						binding.owner = &conversationlife.Owner{}
					case "fenced-child":
						f.owner.Begin()
					case "missing-roots":
						metadata.roots = nil
					case "missing-databases":
						metadata.databases = nil
					case "missing-eval":
						metadata.eval = nil
					case "missing-stat":
						metadata.stat = nil
					case "missing-ready":
						binding.deps.ready = nil
					case "missing-query":
						binding.deps.query = nil
					}
					run()
					return nil
				})
			}
			want := conversationlife.ErrRefused
			if name == "fenced-child" {
				want = context.Canceled
			}
			if !errors.Is(got, want) || result != nil || len(f.counts) != 0 {
				t.Fatalf("err=%v result=%v counts=%v", got, result, f.counts)
			}
		})
	}
}

func TestInstalledOpenCodeActualTools(t *testing.T) {
	for _, name := range []string{"v1", "v2", "corrupt"} {
		t.Run(name, func(t *testing.T) {
			f := newOpenCodeActualFixture(t)
			v2 := name == "v2"
			f.plan(t, !v2, v2, "", 2)
			row := openCodeRow{SessionID: "ses_test", MessageID: "tool", MessageType: "assistant", MessageData: `{"role":"assistant"}`, PartID: "p", PartData: `{"type":"tool","tool":"shell","callID":"c","state":{"output":"one\ntwo","metadata":{"exit":2}}}`, HasMessages: 1, CursorFound: 1}
			if v2 {
				row.MessageData = `{"content":[{"type":"tool","name":"shell","id":"c","state":{"content":[{"text":"one"},{"text":"two"}],"metadata":{"exit":2}}}]}`
			}
			if name == "corrupt" {
				row.PartData = "{"
			}
			f.setRows(v2, "", 2, []openCodeRow{row})
			var entries []Entry
			var corrupt bool
			err := f.owner.Do(context.Background(), func(c *conversationlife.Call) error {
				var err error
				entries, _, corrupt, _, _, err = f.reader.readIn(c, "ses_test", "", 2)
				return err
			})
			if err != nil || f.unplanned != 0 {
				t.Fatal(err)
			}
			if name == "corrupt" {
				if !corrupt || len(entries) != 0 {
					t.Fatal("corrupt parsing drift")
				}
				return
			}
			if corrupt || len(entries) != 1 || len(entries[0].Tools) != 1 || entries[0].Tools[0].Output != "one\ntwo" || !entries[0].Tools[0].Error {
				t.Fatalf("entries=%+v corrupt=%v", entries, corrupt)
			}
		})
	}
}

func TestInstalledOpenCodeActualAbnormalQuery(t *testing.T) {
	for _, name := range []string{"panic", "goexit"} {
		t.Run(name, func(t *testing.T) {
			f := newOpenCodeActualFixture(t)
			f.plan(t, true, false, "", 2)
			done := make(chan struct{})
			returned := false
			var recovered any
			f.queryHook = func(*conversationlife.Call, string) {
				if name == "panic" {
					panic("logical query panic")
				}
				runtime.Goexit()
			}
			t.Cleanup(func() { f.owner.Begin(); <-done; f.owner.Join() })
			go func() {
				defer close(done)
				defer func() { recovered = recover() }()
				_ = f.owner.Do(context.Background(), func(c *conversationlife.Call) error {
					_, _, _, err := f.reader.queryIn(c, openCodeFixtureDB, "ses_test", "", 2)
					return err
				})
				returned = true
			}()
			<-done
			f.owner.Join()
			if returned || (name == "panic" && recovered != "logical query panic") || (name == "goexit" && recovered != nil) || f.counts["query"] != 1 || f.unplanned != 0 || len(f.reader.cache) != 0 || len(f.reader.layouts) != 0 {
				t.Fatalf("returned=%v recovered=%v counts=%v", returned, recovered, f.counts)
			}
		})
	}
}

// Each leaf constructs fresh authority and complete logical dependencies. Even
// invalid arguments and populated caches must go through actual admission.
func TestInstalledOpenCodeScopedAuthorityMatrix(t *testing.T) {
	for _, adapter := range []string{"read", "databases", "query", "layout", "v1", "v2", "rows", "cache-hit", "cache-store"} {
		t.Run(adapter, func(t *testing.T) {
			for _, authority := range []string{"nil", "zero", "foreign", "stale", "live", "live-fenced"} {
				t.Run(authority, func(t *testing.T) {
					for _, mutation := range []string{"intact", "ordinary", "binary-empty", "binary-other", "missing-roots", "missing-databases", "missing-eval", "missing-stat", "missing-ready", "missing-query", "missing-cache", "missing-layouts"} {
						// Live/intact is not an invalid-authority combination.
						if authority == "live" && mutation == "intact" {
							continue
						}
						t.Run(mutation, func(t *testing.T) {
							for _, argument := range []string{"planned", "invalid"} {
								t.Run(argument, func(t *testing.T) {
									f := newOpenCodeActualFixture(t)
									foreign := conversationlife.New()
									t.Cleanup(foreign.Join)
									stamp := openCodeFileStamp{databaseSize: 100, databaseTime: 1, walSize: 20, walTime: 2}
									key := openCodeFixtureDB + "\x00ses_test\x00\x002"
									seed := openCodeCacheEntry{stamp: stamp, rows: []openCodeRow{{MessageID: "independent-good", MessageData: "preserved"}}, hasMore: true}
									// Independent row backing for the expected content, stamp and order.
									wantSeed := openCodeCacheEntry{stamp: stamp, rows: []openCodeRow{{MessageID: "independent-good", MessageData: "preserved"}}, hasMore: true}
									layout := openCodeLayoutEntry{stamp: stamp, layout: openCodeLayout{v1: true, v2: true}}
									f.reader.cache[key], f.reader.layouts[openCodeFixtureDB] = seed, layout
									switch mutation {
									case "ordinary":
										f.reader = &openCodeReader{cache: f.reader.cache, layouts: f.reader.layouts}
									case "binary-empty":
										f.reader.binary = ""
									case "binary-other":
										f.reader.binary = "other"
									case "missing-roots":
										f.reader.metadata.roots = nil
									case "missing-databases":
										f.reader.metadata.databases = nil
									case "missing-eval":
										f.reader.metadata.eval = nil
									case "missing-stat":
										f.reader.metadata.stat = nil
									case "missing-ready":
										f.binding.deps.ready = nil
									case "missing-query":
										f.binding.deps.query = nil
									case "missing-cache":
										f.reader.cache = nil
									case "missing-layouts":
										f.reader.layouts = nil
									}
									observations := 0
									f.reader.observe = func(string) { observations++ }
									session, before, limit := "ses_test", "", 2
									if argument == "invalid" {
										session, before, limit = "bad", "\n", -1
									}
									var got error
									run := func(c *conversationlife.Call) {
										switch adapter {
										case "read":
											_, _, _, _, _, got = f.reader.readIn(c, session, before, limit)
										case "databases":
											_, _, got = f.reader.databasesIn(c)
										case "query":
											_, _, _, got = f.reader.queryIn(c, openCodeFixtureDB, session, before, limit)
										case "layout":
											_, _, got = f.reader.layoutIn(c, openCodeFixtureDB, stamp, true)
										case "v1":
											_, _, _, got = f.reader.queryV1In(c, openCodeFixtureDB, session, before, limit)
										case "v2":
											_, _, _, got = f.reader.queryV2In(c, openCodeFixtureDB, session, before, limit)
										case "rows":
											_, _, _, got = f.reader.runRowsIn(c, openCodeFixtureDB, "denied-before-query", limit, false)
										case "cache-hit":
											_, _, _, got = f.reader.cachedQueryIn(c, key, stamp)
										case "cache-store":
											got = f.reader.storeQueryIn(c, key, stamp, nil, false)
										}
									}
									switch authority {
									case "nil":
										run(nil)
									case "zero":
										run(&conversationlife.Call{})
									case "foreign":
										_ = foreign.Do(context.Background(), func(c *conversationlife.Call) error { run(c); return nil })
									case "stale":
										var stale *conversationlife.Call
										_ = f.owner.Do(context.Background(), func(c *conversationlife.Call) error { stale = c; return nil })
										run(stale)
									default:
										_ = f.owner.Do(context.Background(), func(c *conversationlife.Call) error {
											if authority == "live-fenced" {
												f.owner.Begin()
											}
											run(c)
											return nil
										})
									}
									want := conversationlife.ErrRefused
									if authority == "live-fenced" && mutation == "intact" {
										want = context.Canceled
									}
									if !errors.Is(got, want) || len(f.counts) != 0 || observations != 0 || f.unplanned != 0 {
										t.Fatalf("error=%v want=%v effects=%v observations=%d unknown=%d", got, want, f.counts, observations, f.unplanned)
									}
									if mutation != "missing-cache" && (len(f.reader.cache) != 1 || !reflect.DeepEqual(f.reader.cache[key], wantSeed)) {
										t.Fatal("row publication before admission")
									}
									if mutation != "missing-layouts" && (len(f.reader.layouts) != 1 || !reflect.DeepEqual(f.reader.layouts[openCodeFixtureDB], layout)) {
										t.Fatal("layout publication before admission")
									}
								})
							}
						})
					}
				})
			}
		})
	}
}

func TestInstalledOpenCodeReceiverParserHeldCancellation(t *testing.T) {
	for _, version := range []string{"v1", "v2"} {
		t.Run(version, func(t *testing.T) {
			for _, source := range []string{"caller", "owner"} {
				t.Run(source, func(t *testing.T) {
					f := newOpenCodeActualFixture(t)
					v2 := version == "v2"
					f.plan(t, !v2, v2, "", 2)
					f.setRows(v2, "", 2, openCodeFixtureRows(v2))
					ctx, cancel := context.WithCancel(context.Background())
					entered, release, done := make(chan struct{}), make(chan struct{}), make(chan struct{})
					var once sync.Once
					releaseAll := func() { once.Do(func() { close(release) }) }
					checks := 0
					var entries []Entry
					var got error
					var recovered any
					returned := false
					var parserContext context.Context
					// V1 check 3 starts its second row after the first parsed row;
					// V2 check 4 starts its second row after the first parsed row.
					at := 3
					if v2 {
						at = 4
					}
					f.reader.parserObserve = func(consumed context.Context) {
						if parserContext == nil {
							parserContext = consumed
						}
						checks++
						if checks == at {
							close(entered)
							<-release
						}
					}
					// No observer jobs or entry prerequisites: the sole holder is
					// independently releasable even if actual parsing never enters.
					t.Cleanup(func() { cancel(); f.owner.Begin(); releaseAll(); <-done; f.owner.Join() })
					go func() {
						defer close(done)
						defer func() { recovered = recover() }()
						got = f.owner.Do(ctx, func(c *conversationlife.Call) error {
							var err error
							entries, _, _, _, _, err = f.reader.readIn(c, "ses_test", "", 2)
							return err
						})
						returned = true
					}()
					select {
					case <-done:
						t.Fatal("actual caller exited before parser hold")
					case <-entered:
					}
					if source == "caller" {
						cancel()
					} else {
						f.owner.Begin()
					}
					// Observe selected cancellation before fallback fencing. The
					// The consumed parser context was captured inside its checkpoint.
					<-parserContext.Done()
					if !errors.Is(parserContext.Err(), context.Canceled) {
						t.Fatal("selected cancellation absent")
					}
					receipt := f.owner.Begin()
					timer := time.NewTimer(20 * time.Millisecond)
					defer timer.Stop()
					select {
					case <-done:
						t.Fatal("actual parser detached")
					case <-receipt:
						t.Fatal("held parser lost occupancy")
					case <-timer.C:
					}
					releaseAll()
					<-done
					f.owner.Join()
					wantChecks := at + 1 // canceled checkpoint and v1 unwind
					if v2 {
						wantChecks++ // v2 and outer unwind
					}
					if !returned || recovered != nil || !errors.Is(got, context.Canceled) || entries != nil || checks != wantChecks || f.unplanned != 0 {
						t.Fatalf("returned=%v panic=%v error=%v entries=%v checks=%d unknown=%d", returned, recovered, got, entries, checks, f.unplanned)
					}
					// The earlier raw-row query already linearized successfully;
					// cancellation during parsing is not a rollback of that cache.
					if len(f.reader.cache) != 1 || f.counts["query"] != 2 {
						t.Fatal("completed raw query publication lost")
					}
				})
			}
		})
	}
}

// Legacy calls themselves are the workers: no proxy completion or observer job.
func TestInstalledOpenCodeLegacyLiveOccupancy(t *testing.T) {
	for _, name := range []string{"read", "readContext", "databases", "query", "queryContext", "layoutContext", "queryV1", "queryV2", "runRows", "cachedQuery", "storeQuery"} {
		t.Run(name, func(t *testing.T) {
			f := newOpenCodeActualFixture(t)
			f.plan(t, true, true, "", 2)
			stamp := openCodeFileStamp{databaseSize: 100, databaseTime: 1, walSize: 20, walTime: 2}
			key := openCodeFixtureDB + "\x00ses_test\x00\x002"
			// Separate literals allocate independent backing storage for the oracle.
			wantCache := map[string]openCodeCacheEntry{}
			wantLayouts := map[string]openCodeLayoutEntry{}
			if name == "cachedQuery" || name == "storeQuery" {
				f.reader.cache[key] = openCodeCacheEntry{stamp: stamp, rows: []openCodeRow{{MessageID: "good", MessageData: "preserved"}}, hasMore: true}
				wantCache[key] = openCodeCacheEntry{stamp: stamp, rows: []openCodeRow{{MessageID: "good", MessageData: "preserved"}}, hasMore: true}
				f.reader.layouts[openCodeFixtureDB] = openCodeLayoutEntry{stamp: stamp, layout: openCodeLayout{v1: true}}
				wantLayouts[openCodeFixtureDB] = openCodeLayoutEntry{stamp: stamp, layout: openCodeLayout{v1: true}}
			}
			entered, release, done := make(chan struct{}), make(chan struct{}), make(chan struct{})
			var once sync.Once
			releaseAll := func() { once.Do(func() { close(release) }) }
			var work context.Context
			holds := 0
			hold := func(ctx context.Context) {
				holds++
				if holds == 1 {
					work = ctx
					close(entered)
				}
				<-release
			}
			wantCounts := map[string]int{}
			wantObservations := map[string]int{}
			observations := map[string]int{}
			stage := ""
			switch name {
			case "read", "readContext", "databases":
				f.readyHook = func(c *conversationlife.Call) { hold(c.Context()) }
				wantCounts["ready"] = 1
			case "query":
				// stat has no Call: occupancy, not a fabricated context receipt.
				f.statHook = func(string) { hold(nil) }
				wantCounts["stat"] = 1
			case "cachedQuery":
				stage = "cache-copy"
				wantObservations["cache-hit"], wantObservations["cache-copy"] = 1, 1
			case "storeQuery":
				stage = "cache-publish"
				wantCounts["stat"] = 2
				wantObservations["cache-copy"], wantObservations["cache-publish"] = 1, 1
			default:
				f.queryHook = func(c *conversationlife.Call, _ string) { hold(c.Context()) }
				wantCounts["ready"], wantCounts["query"] = 1, 1
				if name == "queryContext" {
					wantCounts["stat"] = 2
					wantObservations["cache-hit"] = 1
				}
				if name == "queryContext" || name == "layoutContext" {
					wantObservations["layout-hit"] = 1
				}
			}
			f.reader.observe = func(at string) {
				observations[at]++
				if at == stage {
					hold(nil)
				}
			}
			var code string
			var rows []openCodeRow
			var entries []Entry
			var paths []string
			var layout openCodeLayout
			var more, hit, corrupt bool
			var metadata openCodeRow
			returned := false
			var recovered any
			t.Cleanup(func() { f.owner.Begin(); releaseAll(); <-done; f.owner.Join() })
			go func() {
				defer close(done)
				defer func() { recovered = recover() }()
				ctx := context.Background()
				switch name {
				case "read":
					entries, more, corrupt, metadata, code = f.reader.read("ses_test", "", 2)
				case "readContext":
					entries, more, corrupt, metadata, code = f.reader.readContext(ctx, "ses_test", "", 2)
				case "databases":
					paths, code = f.reader.databases()
				case "query":
					rows, more, code = f.reader.query(openCodeFixtureDB, "ses_test", "", 2)
				case "queryContext":
					rows, more, code = f.reader.queryContext(ctx, openCodeFixtureDB, "ses_test", "", 2)
				case "layoutContext":
					layout, code = f.reader.layoutContext(ctx, openCodeFixtureDB, stamp, true)
				case "queryV1":
					rows, more, code = f.reader.queryV1(ctx, openCodeFixtureDB, "ses_test", "", 2)
				case "queryV2":
					rows, more, code = f.reader.queryV2(ctx, openCodeFixtureDB, "ses_test", "", 2)
				case "runRows":
					rows, more, code = f.reader.runRows(ctx, openCodeFixtureDB, openCodeV1SQL("ses_test", "", 2), 2, false)
				case "cachedQuery":
					rows, more, hit = f.reader.cachedQuery(key, stamp)
				case "storeQuery":
					f.reader.storeQuery(key, stamp, []openCodeRow{{MessageID: "replacement"}}, false)
				}
				returned = true
			}()
			select {
			case <-done:
				t.Fatal("legacy caller exited before held work")
			case <-entered:
			}
			receipt := f.owner.Begin()
			if work != nil {
				// This is the actual readiness/query callback's Call context.
				select {
				case <-done:
					t.Fatal("caller detached before cancellation")
				case <-work.Done():
				}
				if !errors.Is(work.Err(), context.Canceled) {
					t.Fatal("actual work cancellation missing")
				}
			}
			timer := time.NewTimer(20 * time.Millisecond)
			defer timer.Stop()
			select {
			case <-done:
				t.Fatal("legacy caller detached")
			case <-receipt:
				t.Fatal("held operation lost count")
			case <-timer.C:
			}
			releaseAll()
			<-done
			f.owner.Join()
			wantCode := "query_failed"
			if name == "databases" {
				wantCode = "source_unavailable"
			}
			if name == "cachedQuery" || name == "storeQuery" {
				wantCode = ""
			}
			if !returned || recovered != nil || holds != 1 || code != wantCode || rows != nil || entries != nil || paths != nil || more || hit || corrupt || metadata != (openCodeRow{}) || layout != (openCodeLayout{}) {
				t.Fatalf("returned=%v panic=%v holds=%d code=%q rows=%v entries=%v paths=%v more=%v hit=%v corrupt=%v metadata=%+v layout=%+v", returned, recovered, holds, code, rows, entries, paths, more, hit, corrupt, metadata, layout)
			}
			if f.unplanned != 0 || !reflect.DeepEqual(f.counts, wantCounts) || !reflect.DeepEqual(observations, wantObservations) || !reflect.DeepEqual(f.reader.cache, wantCache) || !reflect.DeepEqual(f.reader.layouts, wantLayouts) {
				t.Fatalf("unknown=%d effects=%v observations=%v cache=%v layouts=%v", f.unplanned, f.counts, observations, f.reader.cache, f.reader.layouts)
			}
		})
	}
}

func TestInstalledOpenCodePageQueryBudgets(t *testing.T) {
	for _, mode := range []string{"scoped", "legacy-context"} {
		t.Run(mode, func(t *testing.T) {
			for _, version := range []string{"v1", "v2"} {
				t.Run(version, func(t *testing.T) {
					for _, minimum := range []string{"none", "earlier", "later"} {
						t.Run(minimum, func(t *testing.T) {
							f := newOpenCodeActualFixture(t)
							v2 := version == "v2"
							message := openCodeV1SQL("ses_test", "", 2)
							layout := []byte(`[{"name":"session"},{"name":"message"},{"name":"part"}]`)
							if v2 {
								message = openCodeV2SQL("ses_test", "", 2)
								layout = []byte(`[{"name":"session_v2"},{"name":"session_message"}]`)
							}
							// Complete private query table; never invoke the old 9–10s guard.
							payload, marshalErr := json.Marshal([]openCodeRow{{SessionID: "ses_test", MessageID: "one", MessageType: "user", MessageData: `{"role":"user","text":"hello"}`, PartID: "p", PartData: `{"type":"text","text":"hello"}`, Total: 1, CursorFound: 1, HasMessages: 1}})
							if marshalErr != nil {
								t.Fatal(marshalErr)
							}
							allowed := map[string][]byte{openCodeFixtureLayout: layout, message: payload}
							type valueKey struct{}
							base := context.WithValue(context.Background(), valueKey{}, "page-value")
							ctx, cancel := context.WithCancel(base)
							var parentDeadline time.Time
							stop := func() {}
							if minimum != "none" {
								duration := 30 * time.Second
								if minimum == "earlier" {
									duration = 5 * time.Second
								}
								parentDeadline = time.Now().Add(duration)
								ctx, stop = context.WithDeadline(ctx, parentDeadline)
							}
							entered, release, done := make(chan struct{}), make(chan struct{}), make(chan struct{})
							var once sync.Once
							releaseAll := func() { once.Do(func() { close(release) }) }
							type capture struct {
								ctx                context.Context
								deadline, observed time.Time
								timed              bool
								value              any
								err                error
							}
							snapshot := func(c *conversationlife.Call) capture {
								context := c.Context()
								deadline, timed := context.Deadline()
								return capture{context, deadline, time.Now(), timed, context.Value(valueKey{}), context.Err()}
							}
							var ready, queries []capture
							var order []string
							var firstRetired, secondLive bool
							f.binding.deps = installedProviderSQLDeps{
								ready: func(c *conversationlife.Call) bool {
									f.counts["ready"]++
									if c == nil || c.Context() == nil || len(ready) >= 3 {
										f.unplanned++
										return false
									}
									if c.Context().Value(valueKey{}) != "page-value" || c.Context().Err() != nil {
										f.unplanned++
										return false
									}
									ready = append(ready, snapshot(c))
									return true
								},
								query: func(c *conversationlife.Call, db, sql string, max int) ([]byte, error) {
									f.counts["query"]++
									raw, ok := allowed[sql]
									if c == nil || c.Context() == nil || !ok || db != openCodeFixtureDB || max != 8*1024*1024 || len(queries) >= 2 {
										f.unplanned++
										return nil, conversationlife.ErrRefused
									}
									if c.Context().Value(valueKey{}) != "page-value" || c.Context().Err() != nil {
										f.unplanned++
										return nil, conversationlife.ErrRefused
									}
									queries = append(queries, snapshot(c))
									order = append(order, sql)
									if len(queries) == 1 {
										close(entered)
										<-release
									} else {
										firstRetired = errors.Is(queries[0].ctx.Err(), context.Canceled)
										secondLive = c.Context().Err() == nil
									}
									return raw, nil
								},
							}
							var entries []Entry
							var more, corrupt bool
							var metadata openCodeRow
							var code string
							var got error
							var recovered any
							returned := false
							// All retirement paths are armed before the sole actual caller.
							t.Cleanup(func() {
								cancel()
								stop()
								f.owner.Begin()
								releaseAll()
								<-done
								f.owner.Join()
							})
							started := time.Now()
							go func() {
								defer close(done)
								defer func() { recovered = recover() }()
								if mode == "scoped" {
									got = f.owner.Do(ctx, func(c *conversationlife.Call) error {
										var err error
										entries, more, corrupt, metadata, code, err = f.reader.readIn(c, "ses_test", "", 2)
										return err
									})
								} else {
									entries, more, corrupt, metadata, code = f.reader.readContext(ctx, "ses_test", "", 2)
								}
								returned = true
							}()
							select {
							case <-done:
								t.Fatal("page exited before first SQL callback")
							case <-entered:
							}
							timer := time.NewTimer(20 * time.Millisecond)
							defer timer.Stop()
							select {
							case <-done:
								t.Fatal("page detached first query")
							case <-ctx.Done():
								t.Fatalf("parent expired during controlled hold: %v", ctx.Err())
							case <-timer.C:
							}
							released := time.Now()
							releaseAll()
							<-done
							// Check expiry before cleanup can manufacture cancellation.
							parentErr := ctx.Err()
							retiredBeforeFence := true
							for _, observation := range queries {
								retiredBeforeFence = retiredBeforeFence && errors.Is(observation.ctx.Err(), context.Canceled)
							}
							f.owner.Join()
							if !returned || recovered != nil || got != nil || parentErr != nil || code != "" || more || corrupt || len(entries) != 1 || entries[0].ID != "one" || entries[0].Text != "hello" || metadata.Database != openCodeFixtureDB || metadata.Total != 1 {
								t.Fatalf("returned=%v panic=%v err=%v parent=%v code=%q entries=%+v metadata=%+v", returned, recovered, got, parentErr, code, entries, metadata)
							}
							wantCounts := map[string]int{"ready": 3, "query": 2, "roots": 1, "databases": 1, "eval": 2, "stat": 7}
							if f.unplanned != 0 || !reflect.DeepEqual(f.counts, wantCounts) || len(ready) != 3 || len(queries) != 2 || !reflect.DeepEqual(order, []string{openCodeFixtureLayout, message}) {
								t.Fatalf("unknown=%d counts=%v ready=%d queries=%d order=%v", f.unplanned, f.counts, len(ready), len(queries), order)
							}
							for i, observation := range ready {
								if observation.value != "page-value" || observation.err != nil {
									t.Fatalf("ready %d value/error: %+v", i, observation)
								}
								if i == 0 {
									if observation.timed != (minimum != "none") || !observation.deadline.Equal(parentDeadline) {
										t.Fatal("discovery readiness inherited a query budget or lost parent deadline")
									}
								} else if !observation.deadline.Equal(queries[i-1].deadline) || !observation.timed {
									t.Fatal("timed readiness/query deadline mismatch")
								}
							}
							for i, observation := range queries {
								if !observation.timed || observation.value != "page-value" || observation.err != nil {
									t.Fatalf("query %d metadata: %+v", i, observation)
								}
								if minimum == "earlier" {
									if !observation.deadline.Equal(parentDeadline) {
										t.Fatalf("query %d did not preserve exact parent minimum", i)
									}
								} else {
									lower := started
									if i == 1 {
										lower = released
									}
									if observation.deadline.Before(lower.Add(10*time.Second)) || observation.deadline.After(ready[i+1].observed.Add(10*time.Second)) {
										t.Fatalf("query %d deadline=%v lower=%v ready=%v", i, observation.deadline, lower, ready[i+1].observed)
									}
								}
							}
							if minimum != "earlier" && queries[1].deadline.Sub(queries[0].deadline) < 20*time.Millisecond {
								t.Fatal("layout and message shared a whole-page budget")
							}
							if !firstRetired || !secondLive {
								t.Fatalf("first retired=%v second live=%v", firstRetired, secondLive)
							}
							if !retiredBeforeFence {
								t.Fatal("query context not retired before owner fencing")
							}
						})
					}
				})
			}
		})
	}
}

// A single ordered, private script covers ALL callback operands, including
// observations. A mismatched/excess effect is counted and denied, never treated
// as an ordinary planned metadata miss. Scripts are complete before construction.
type openCodeMetadataStep struct {
	kind, path, sql string
	paths           []string
	resolved        string
	info            os.FileInfo
	raw             string
	err             error
	ready           bool
	cancel          bool
}

type openCodeMetadataFixture struct {
	owner         *conversationlife.Owner
	reader        *openCodeReader
	steps         []openCodeMetadataStep
	seen          []string
	next, unknown int
	cancelled     error
}

func (f *openCodeMetadataFixture) take(kind, path, sql string) (openCodeMetadataStep, bool) {
	f.seen = append(f.seen, kind+":"+path+":"+sql)
	if f.next >= len(f.steps) || f.steps[f.next].kind != kind || f.steps[f.next].path != path || f.steps[f.next].sql != sql {
		f.unknown++
		f.owner.Begin()
		return openCodeMetadataStep{err: conversationlife.ErrRefused}, false
	}
	s := f.steps[f.next]
	f.next++
	return s, true
}

func newOpenCodeMetadataFixture(t *testing.T, steps []openCodeMetadataStep) *openCodeMetadataFixture {
	t.Helper()
	f := &openCodeMetadataFixture{owner: conversationlife.New(), steps: steps}
	t.Cleanup(f.owner.Join)
	binding := &installedProviderSQL{owner: f.owner, binary: "sqlite3", deps: installedProviderSQLDeps{
		ready: func(c *conversationlife.Call) bool {
			s, ok := f.take("ready", "", "")
			if !ok || c == nil || c.Context() == nil || c.Context().Err() != nil {
				if ok {
					f.unknown++
					f.owner.Begin()
				}
				return false
			}
			if s.cancel {
				// Actual reached readiness work, not an ancestor-context receipt.
				f.owner.Begin()
				<-c.Context().Done()
				f.cancelled = c.Context().Err()
			}
			return s.ready
		},
		query: func(c *conversationlife.Call, db, sql string, max int) ([]byte, error) {
			s, ok := f.take("query", db, sql)
			if !ok {
				return nil, conversationlife.ErrRefused
			}
			if max != 8*1024*1024 || c == nil || c.Context() == nil || c.Context().Err() != nil {
				f.unknown++
				f.owner.Begin()
				return nil, conversationlife.ErrRefused
			}
			return []byte(s.raw), s.err
		},
	}}
	metadata := installedProviderMetadata{
		roots: func(ctx context.Context, home string) ([]string, error) {
			s, ok := f.take("roots", home, "")
			if !ok {
				return nil, conversationlife.ErrRefused
			}
			return append([]string(nil), s.paths...), errors.Join(s.err, ctx.Err())
		},
		databases: func(ctx context.Context, home string) ([]string, error) {
			s, ok := f.take("databases", home, "")
			if !ok {
				return nil, conversationlife.ErrRefused
			}
			return append([]string(nil), s.paths...), errors.Join(s.err, ctx.Err())
		},
		eval: func(path string) (string, error) {
			s, ok := f.take("eval", path, "")
			if !ok {
				return "", conversationlife.ErrRefused
			}
			return s.resolved, s.err
		},
		stat: func(path string) (os.FileInfo, error) {
			s, ok := f.take("stat", path, "")
			if !ok {
				return nil, conversationlife.ErrRefused
			}
			return s.info, s.err
		},
	}
	err := f.owner.Do(context.Background(), func(c *conversationlife.Call) error {
		var err error
		f.reader, err = constructInstalledOpenCodeIn(c, "/planned/home", binding, metadata)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	f.reader.observe = func(stage string) { _, _ = f.take("stage", stage, "") }
	return f
}

func (f *openCodeMetadataFixture) check(t *testing.T) {
	t.Helper()
	// All actual synchronous calls must already have returned before this fence.
	f.owner.Join()
	if f.unknown != 0 || f.next != len(f.steps) || len(f.seen) != len(f.steps) {
		t.Fatalf("unknown=%d consumed=%d/%d effects=%v", f.unknown, f.next, len(f.steps), f.seen)
	}
	if f.owner.Witnesses() != nil {
		t.Fatal("metadata/query marker recorded as cleanup witness")
	}
}

type openCodeModeInfo struct {
	openCodeFixtureInfo
	mode os.FileMode
}

func (i openCodeModeInfo) Mode() os.FileMode { return i.mode }
func (i openCodeModeInfo) IsDir() bool       { return i.mode.IsDir() }

const openCodeMetadataRoot2 = "/planned/other"
const openCodeMetadataDB2 = "/planned/other/opencode.db"

func openCodeMetadataDiscovery(roots, databases []string) []openCodeMetadataStep {
	return []openCodeMetadataStep{
		{kind: "ready", ready: true},
		{kind: "roots", path: "/planned/home", paths: roots},
		{kind: "databases", path: "/planned/home", paths: databases},
	}
}

func openCodeMetadataCandidate(root, candidate, canonicalRoot, canonicalDB string) []openCodeMetadataStep {
	return []openCodeMetadataStep{
		{kind: "eval", path: root, resolved: canonicalRoot},
		{kind: "eval", path: candidate, resolved: canonicalDB},
		{kind: "stat", path: canonicalDB, info: openCodeFixtureInfo{100, 1}},
	}
}

func openCodeMetadataStages(names ...string) []openCodeMetadataStep {
	var steps []openCodeMetadataStep
	for _, name := range names {
		steps = append(steps, openCodeMetadataStep{kind: "stage", path: name})
	}
	return steps
}

func openCodeMetadataStamp(db string, info os.FileInfo, dbErr error, wal os.FileInfo, walErr error) []openCodeMetadataStep {
	steps := []openCodeMetadataStep{{kind: "stat", path: db, info: info, err: dbErr}}
	if dbErr == nil && info != nil {
		steps = append(steps, openCodeMetadataStep{kind: "stat", path: db + "-wal", info: wal, err: walErr})
	}
	return steps
}

func TestInstalledOpenCodeMetadataContainmentOutcomes(t *testing.T) {
	ordinary := errors.New("planned metadata failure")
	for _, name := range []string{"ready-false", "ready-cancel", "no-roots", "no-candidates", "extra-root", "extra-candidate", "ordered", "reverse-order", "root-error", "candidate-error", "stat-error", "stat-absent", "stat-nil", "directory", "special", "equal-root", "parent", "sibling-prefix", "relative-root", "relative-candidate", "alias", "rerouted", "roots-error", "candidates-error", "root-refusal", "candidate-cancel-marker", "stat-cleanup-marker", "accepted-then-stat-cleanup"} {
		t.Run(name, func(t *testing.T) {
			roots, candidates := []string{"/planned/opencode"}, []string{openCodeFixtureDB}
			want := []string{openCodeFixtureDB}
			var wantErr error
			code := ""
			candidate := openCodeMetadataCandidate(roots[0], candidates[0], roots[0], candidates[0])
			switch name {
			case "no-roots":
				roots = nil
				candidate = nil
				want = nil
			case "no-candidates":
				candidates = nil
				candidate = nil
				want = nil
			case "extra-root":
				roots = append(roots, openCodeMetadataRoot2)
			case "extra-candidate":
				candidates = append(candidates, openCodeMetadataDB2)
			case "ordered":
				roots = append(roots, openCodeMetadataRoot2)
				candidates = append(candidates, openCodeMetadataDB2)
				candidate = append(candidate, openCodeMetadataCandidate(openCodeMetadataRoot2, openCodeMetadataDB2, openCodeMetadataRoot2, openCodeMetadataDB2)...)
				want = []string{openCodeFixtureDB, openCodeMetadataDB2}
			case "reverse-order":
				roots = []string{openCodeMetadataRoot2, "/planned/opencode"}
				candidates = []string{openCodeMetadataDB2, openCodeFixtureDB}
				candidate = append(openCodeMetadataCandidate(openCodeMetadataRoot2, openCodeMetadataDB2, openCodeMetadataRoot2, openCodeMetadataDB2), candidate...)
				want = []string{openCodeMetadataDB2, openCodeFixtureDB}
			case "root-error", "root-refusal":
				candidate = candidate[:1]
				candidate[0].err = ordinary
				want = nil
				if name == "root-refusal" {
					candidate[0].err = conversationlife.ErrRefused
					wantErr = conversationlife.ErrRefused
				}
			case "candidate-error", "candidate-cancel-marker":
				candidate = candidate[:2]
				candidate[1].err = ordinary
				want = nil
				if name == "candidate-cancel-marker" {
					candidate[1].err = context.Canceled
					wantErr = context.Canceled
				}
			case "stat-error", "stat-absent", "stat-nil", "directory", "special", "stat-cleanup-marker":
				want = nil
				candidate[2].info = nil
				switch name {
				case "stat-error":
					candidate[2].err = ordinary
				case "stat-absent":
					candidate[2].err = os.ErrNotExist
				case "directory":
					candidate[2].info = openCodeModeInfo{openCodeFixtureInfo{100, 1}, os.ModeDir | 0700}
				case "special":
					candidate[2].info = openCodeModeInfo{openCodeFixtureInfo{100, 1}, os.ModeNamedPipe | 0600}
				case "stat-cleanup-marker":
					candidate[2].err = ErrSourceCleanupUncertain
					wantErr = ErrSourceCleanupUncertain
				}
			case "equal-root", "parent", "sibling-prefix", "relative-root", "relative-candidate", "rerouted":
				candidate = candidate[:2]
				want = nil
				switch name {
				case "equal-root":
					candidate[1].resolved = "/planned/opencode"
				case "parent":
					candidate[1].resolved = "/planned"
				case "sibling-prefix":
					candidate[1].resolved = "/planned/opencode-escape/opencode.db"
				case "relative-root":
					candidate[0].resolved = "planned/opencode"
				case "relative-candidate":
					candidate[1].resolved = "planned/opencode/opencode.db"
				case "rerouted":
					candidate[1].resolved = openCodeMetadataDB2
					roots = append(roots, openCodeMetadataRoot2)
				}
			case "alias":
				candidate = openCodeMetadataCandidate(roots[0], candidates[0], openCodeMetadataRoot2, openCodeMetadataDB2)
				want = []string{openCodeMetadataDB2}
			case "accepted-then-stat-cleanup":
				roots = append(roots, openCodeMetadataRoot2)
				candidates = append(candidates, openCodeMetadataDB2)
				later := openCodeMetadataCandidate(openCodeMetadataRoot2, openCodeMetadataDB2, openCodeMetadataRoot2, openCodeMetadataDB2)
				later[2].info, later[2].err = nil, ErrSourceCleanupUncertain
				candidate = append(candidate, later...)
				want, wantErr = nil, ErrSourceCleanupUncertain
			}
			steps := append(openCodeMetadataDiscovery(roots, candidates), candidate...)
			switch name {
			case "ready-false":
				steps = []openCodeMetadataStep{{kind: "ready"}}
				want = nil
			case "ready-cancel":
				steps = []openCodeMetadataStep{{kind: "ready", ready: true, cancel: true}}
				want = nil
				wantErr = context.Canceled
			case "roots-error":
				steps = steps[:2]
				steps[1].err = ordinary
				want = nil
				wantErr = ordinary
			case "candidates-error":
				steps = steps[:3]
				steps[2].err = ordinary
				want = nil
				wantErr = ordinary
			}
			if want == nil {
				code = "source_unavailable"
			}
			f := newOpenCodeMetadataFixture(t, steps)
			var paths []string
			var gotCode string
			err := f.owner.Do(context.Background(), func(c *conversationlife.Call) error {
				var err error
				paths, gotCode, err = f.reader.databasesIn(c)
				return err
			})
			f.check(t)
			if !errors.Is(err, wantErr) || gotCode != code || !reflect.DeepEqual(paths, want) {
				t.Fatalf("paths=%v want=%v code=%q err=%v wantErr=%v", paths, want, gotCode, err, wantErr)
			}
			if name == "ready-cancel" && !errors.Is(f.cancelled, context.Canceled) {
				t.Fatal("actual readiness cancellation not observed")
			}
			if len(f.reader.cache) != 0 || len(f.reader.layouts) != 0 {
				t.Fatal("discovery published query cache")
			}
		})
	}
}

func TestInstalledOpenCodeMetadataDynamicDiscovery(t *testing.T) {
	// The second callbacks return a new ordering only after the first actual
	// operation has fully returned; no receiver replacement or interim Join.
	steps := openCodeMetadataDiscovery([]string{"/planned/opencode"}, []string{openCodeFixtureDB})
	steps = append(steps, openCodeMetadataCandidate("/planned/opencode", openCodeFixtureDB, "/planned/opencode", openCodeFixtureDB)...)
	steps = append(steps, openCodeMetadataDiscovery([]string{openCodeMetadataRoot2, "/planned/opencode"}, []string{openCodeMetadataDB2, openCodeFixtureDB})...)
	steps = append(steps, openCodeMetadataCandidate(openCodeMetadataRoot2, openCodeMetadataDB2, openCodeMetadataRoot2, openCodeMetadataDB2)...)
	steps = append(steps, openCodeMetadataCandidate("/planned/opencode", openCodeFixtureDB, "/planned/opencode", openCodeFixtureDB)...)
	f := newOpenCodeMetadataFixture(t, steps)
	var results [2][]string
	var codes [2]string
	var errs [2]error
	for i := range results {
		errs[i] = f.owner.Do(context.Background(), func(c *conversationlife.Call) error {
			var err error
			results[i], codes[i], err = f.reader.databasesIn(c)
			return err
		})
	}
	f.check(t)
	if errs[0] != nil || errs[1] != nil || codes != [2]string{} || !reflect.DeepEqual(results[0], []string{openCodeFixtureDB}) || !reflect.DeepEqual(results[1], []string{openCodeMetadataDB2, openCodeFixtureDB}) {
		t.Fatalf("results=%v codes=%v errors=%v", results, codes, errs)
	}
}

// SQL keys deliberately reuse the builders in this bounded search slice; this
// is NOT an independent whole-SQL oracle or engine/byte-boundary qualification.
const openCodeMetadataRows = `[{"session_id":"ses_test","title":"selected","directory":"/planned/project","message_id":"new","message_data":"{\"role\":\"user\"}","part_id":"p2","part_data":"{\"type\":\"text\",\"text\":\"second\"}","message_total":2,"cursor_found":1},{"session_id":"ses_test","title":"selected","directory":"/planned/project","message_id":"old","message_data":"{\"role\":\"user\"}","part_id":"p1","part_data":"{\"type\":\"text\",\"text\":\"first\"}","message_total":2,"cursor_found":1}]`

func openCodeMetadataExpectedRows() []openCodeRow {
	// Independent literals, never decoded from payload or copied from cache.
	return []openCodeRow{
		{SessionID: "ses_test", Title: "selected", Directory: "/planned/project", MessageID: "old", MessageData: `{"role":"user"}`, PartID: "p1", PartData: `{"type":"text","text":"first"}`, Total: 2, CursorFound: 1},
		{SessionID: "ses_test", Title: "selected", Directory: "/planned/project", MessageID: "new", MessageData: `{"role":"user"}`, PartID: "p2", PartData: `{"type":"text","text":"second"}`, Total: 2, CursorFound: 1},
	}
}

func openCodeMetadataQuerySteps(db, outcome string, primary error, stamp []openCodeMetadataStep, stampOK bool) []openCodeMetadataStep {
	steps := append([]openCodeMetadataStep(nil), stamp...)
	if stampOK {
		steps = append(steps, openCodeMetadataStages("cache-hit", "layout-hit")...)
	}
	layout := openCodeMetadataStep{kind: "query", path: db, sql: openCodeFixtureLayout, raw: `[{"name":"session"},{"name":"message"},{"name":"part"}]`}
	if outcome == "layout-failure" {
		layout.err = primary
	}
	if outcome == "corrupt-layout" {
		layout.raw = "{"
	}
	steps = append(steps, openCodeMetadataStep{kind: "ready", ready: true}, layout)
	if outcome == "layout-failure" || outcome == "corrupt-layout" {
		return steps
	}
	steps = append(steps, openCodeMetadataStages("layout-row", "layout-row", "layout-row")...)
	steps = append(steps, stamp...)
	if stampOK {
		steps = append(steps, openCodeMetadataStages("layout-publish")...)
	}
	message := openCodeMetadataStep{kind: "query", path: db, sql: openCodeV1SQL("ses_test", "", 2), raw: openCodeMetadataRows}
	switch outcome {
	case "miss":
		message.raw = "[]"
	case "message-failure":
		message.err = primary
	case "corrupt-rows":
		message.raw = "{"
	}
	steps = append(steps, openCodeMetadataStep{kind: "ready", ready: true}, message)
	if outcome == "message-failure" {
		return steps
	}
	steps = append(steps, openCodeMetadataStages("decode")...)
	if outcome == "corrupt-rows" {
		return steps
	}
	if outcome == "success" {
		steps = append(steps, openCodeMetadataStages("reverse", "version", "version", "distinct", "distinct")...)
	}
	if stampOK {
		if outcome == "success" {
			steps = append(steps, openCodeMetadataStages("cache-copy", "cache-copy")...)
		}
		steps = append(steps, stamp...)
		steps = append(steps, openCodeMetadataStages("cache-publish")...)
	}
	return steps
}

func openCodeMetadataParserSteps() []openCodeMetadataStep {
	// V1: initial + two rows + two retained entries + deferred final check.
	return openCodeMetadataStages("parser-check", "parser-check", "parser-check", "parser-check", "parser-check", "parser-check")
}

func TestInstalledOpenCodeNormalMultiDatabaseSearch(t *testing.T) {
	first := errors.New("first logical SQL failure")
	second := errors.New("second logical SQL failure")
	cases := []struct {
		name, a, b, code, selected string
		want                       error
	}{
		{"first-success", "success", "unused", "", openCodeFixtureDB, nil},
		{"miss-success", "miss", "success", "", openCodeMetadataDB2, nil},
		{"layout-failure-success", "layout-failure", "success", "", openCodeMetadataDB2, nil},
		{"message-failure-success", "message-failure", "success", "", openCodeMetadataDB2, nil},
		{"both-misses", "miss", "miss", "invalid_session", "", nil},
		{"failure-miss", "message-failure", "miss", "query_failed", "", first},
		{"miss-failure", "miss", "layout-failure", "query_failed", "", second},
		{"both-failures", "layout-failure", "message-failure", "query_failed", "", first},
		{"both-failures-distinct-reason", "layout-failure", "message-failure", "output_limit", "", sqliteexec.ErrOutputLimit},
		{"corrupt-layout-miss", "corrupt-layout", "miss", "source_corrupt", "", nil},
		{"corrupt-layout-failure", "corrupt-layout", "message-failure", "source_corrupt", "", nil},
		{"corrupt-rows-miss", "corrupt-rows", "miss", "source_corrupt", "", nil},
		{"corrupt-rows-failure", "corrupt-rows", "layout-failure", "source_corrupt", "", nil},
		{"corrupt-layout-success", "corrupt-layout", "success", "", openCodeMetadataDB2, nil},
		{"corrupt-rows-success", "corrupt-rows", "success", "", openCodeMetadataDB2, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			steps := openCodeMetadataDiscovery([]string{"/planned/opencode", openCodeMetadataRoot2}, []string{openCodeFixtureDB, openCodeMetadataDB2})
			// Discovery is eager: DB2 metadata precedes even a DB1 SQL success.
			steps = append(steps, openCodeMetadataCandidate("/planned/opencode", openCodeFixtureDB, "/planned/opencode", openCodeFixtureDB)...)
			steps = append(steps, openCodeMetadataCandidate(openCodeMetadataRoot2, openCodeMetadataDB2, openCodeMetadataRoot2, openCodeMetadataDB2)...)
			for i, outcome := range []string{tc.a, tc.b} {
				if outcome == "unused" {
					continue
				}
				db, primary := openCodeFixtureDB, first
				if tc.name == "both-failures-distinct-reason" {
					primary = sqliteexec.ErrOutputLimit
				}
				if i == 1 {
					db, primary = openCodeMetadataDB2, second
				}
				stamp := openCodeMetadataStamp(db, openCodeFixtureInfo{100, 1}, nil, openCodeFixtureInfo{20, 2}, nil)
				steps = append(steps, openCodeMetadataQuerySteps(db, outcome, primary, stamp, true)...)
				if outcome == "success" {
					steps = append(steps, openCodeMetadataParserSteps()...)
				}
			}
			f := newOpenCodeMetadataFixture(t, steps)
			// Unrelated independently seeded good entries must survive all paths.
			f.reader.cache["independent-seed"] = openCodeCacheEntry{rows: []openCodeRow{{MessageID: "keep", MessageData: "unchanged"}}, hasMore: true}
			f.reader.layouts["independent-seed"] = openCodeLayoutEntry{layout: openCodeLayout{v2: true}}
			var entries []Entry
			var more, corrupt bool
			var metadata openCodeRow
			var code string
			err := f.owner.Do(context.Background(), func(c *conversationlife.Call) error {
				var err error
				entries, more, corrupt, metadata, code, err = f.reader.readIn(c, "ses_test", "", 2)
				return err
			})
			f.check(t)
			if !errors.Is(err, tc.want) || (tc.want != first && errors.Is(err, first)) || (tc.want != second && errors.Is(err, second)) || code != tc.code || more || corrupt {
				t.Fatalf("err=%v want=%v code=%q want=%q more=%v corrupt=%v", err, tc.want, code, tc.code, more, corrupt)
			}
			if tc.selected != "" {
				wantMetadata := openCodeRow{SessionID: "ses_test", Title: "selected", Directory: "/planned/project", MessageID: "old", MessageData: `{"role":"user"}`, PartID: "p1", PartData: `{"type":"text","text":"first"}`, Total: 2, CursorFound: 1, Database: tc.selected}
				if !reflect.DeepEqual(entries, []Entry{{ID: "old", Role: "user", Text: "first"}, {ID: "new", Role: "user", Text: "second"}}) || metadata != wantMetadata {
					t.Fatalf("entries=%+v metadata=%+v", entries, metadata)
				}
			} else if entries != nil || metadata != (openCodeRow{}) {
				t.Fatal("exhaustion returned partial page")
			}
			wantCache := map[string]openCodeCacheEntry{"independent-seed": {rows: []openCodeRow{{MessageID: "keep", MessageData: "unchanged"}}, hasMore: true}}
			wantLayouts := map[string]openCodeLayoutEntry{"independent-seed": {layout: openCodeLayout{v2: true}}}
			stamp := openCodeFileStamp{databaseSize: 100, databaseTime: 1, walSize: 20, walTime: 2}
			for i, outcome := range []string{tc.a, tc.b} {
				db := openCodeFixtureDB
				if i == 1 {
					db = openCodeMetadataDB2
				}
				if outcome == "unused" || outcome == "layout-failure" || outcome == "corrupt-layout" {
					continue
				}
				wantLayouts[db] = openCodeLayoutEntry{stamp: stamp, layout: openCodeLayout{v1: true}}
				if outcome == "success" {
					wantCache[db+"\x00ses_test\x00\x002"] = openCodeCacheEntry{stamp: stamp, rows: openCodeMetadataExpectedRows()}
				}
				if outcome == "miss" {
					wantCache[db+"\x00ses_test\x00\x002"] = openCodeCacheEntry{stamp: stamp, rows: []openCodeRow{}}
				}
			}
			if !reflect.DeepEqual(f.reader.cache, wantCache) || !reflect.DeepEqual(f.reader.layouts, wantLayouts) {
				t.Fatalf("cache=%+v want=%+v layouts=%+v want=%+v", f.reader.cache, wantCache, f.reader.layouts, wantLayouts)
			}
		})
	}
}

func TestInstalledOpenCodeCanonicalDatabaseRead(t *testing.T) {
	steps := openCodeMetadataDiscovery([]string{"/planned/opencode"}, []string{openCodeFixtureDB})
	steps = append(steps, openCodeMetadataCandidate("/planned/opencode", openCodeFixtureDB, openCodeMetadataRoot2, openCodeMetadataDB2)...)
	stamp := openCodeMetadataStamp(openCodeMetadataDB2, openCodeFixtureInfo{100, 1}, nil, openCodeFixtureInfo{20, 2}, nil)
	steps = append(steps, openCodeMetadataQuerySteps(openCodeMetadataDB2, "success", nil, stamp, true)...)
	steps = append(steps, openCodeMetadataParserSteps()...)
	f := newOpenCodeMetadataFixture(t, steps)
	var entries []Entry
	var more, corrupt bool
	var metadata openCodeRow
	var code string
	err := f.owner.Do(context.Background(), func(c *conversationlife.Call) error {
		var err error
		entries, more, corrupt, metadata, code, err = f.reader.readIn(c, "ses_test", "", 2)
		return err
	})
	f.check(t)
	if err != nil || code != "" || more || corrupt || metadata.Database != openCodeMetadataDB2 || metadata.Total != 2 || !reflect.DeepEqual(entries, []Entry{{ID: "old", Role: "user", Text: "first"}, {ID: "new", Role: "user", Text: "second"}}) {
		t.Fatalf("err=%v code=%q metadata=%+v entries=%+v", err, code, metadata, entries)
	}
}

func TestInstalledOpenCodeDatabaseWALStampOutcomes(t *testing.T) {
	ordinary := errors.New("planned stamp failure")
	for _, name := range []string{"present", "wal-absent", "wal-error", "db-error", "db-absent", "db-nil", "wal-nil", "db-directory-metadata", "wal-special-metadata", "db-refusal", "wal-cleanup-marker", "discovery-then-db-nil", "discovery-then-wal-nil"} {
		t.Run(name, func(t *testing.T) {
			var db os.FileInfo = openCodeFixtureInfo{100, 1}
			var wal os.FileInfo = openCodeFixtureInfo{20, 2}
			var dbErr, walErr, wantErr error
			stampOK := true
			wantStamp := openCodeFileStamp{databaseSize: 100, databaseTime: 1, walSize: 20, walTime: 2}
			switch name {
			case "wal-absent":
				wal = nil
				walErr = os.ErrNotExist
				wantStamp.walSize, wantStamp.walTime = 0, 0
			case "wal-error":
				wal = nil
				walErr = ordinary
				wantStamp.walSize, wantStamp.walTime = 0, 0
			case "db-error":
				db = nil
				dbErr = ordinary
				stampOK = false
			case "db-absent":
				db = nil
				dbErr = os.ErrNotExist
				stampOK = false
			case "db-nil", "discovery-then-db-nil":
				db = nil
				wantErr = conversationlife.ErrRefused
			case "wal-nil", "discovery-then-wal-nil":
				wal = nil
				wantErr = conversationlife.ErrRefused
			case "db-directory-metadata":
				db = openCodeModeInfo{openCodeFixtureInfo{100, 1}, os.ModeDir | 0700}
			case "wal-special-metadata":
				wal = openCodeModeInfo{openCodeFixtureInfo{20, 2}, os.ModeNamedPipe | 0600}
			case "db-refusal":
				db = nil
				dbErr = conversationlife.ErrRefused
				wantErr = conversationlife.ErrRefused
			case "wal-cleanup-marker":
				wal = nil
				walErr = sqliteexec.ErrCleanupUncertain
				wantErr = sqliteexec.ErrCleanupUncertain
			}
			stamp := openCodeMetadataStamp(openCodeFixtureDB, db, dbErr, wal, walErr)
			var steps []openCodeMetadataStep
			read := strings.HasPrefix(name, "discovery-then-")
			if read {
				steps = openCodeMetadataDiscovery([]string{"/planned/opencode"}, []string{openCodeFixtureDB})
				steps = append(steps, openCodeMetadataCandidate("/planned/opencode", openCodeFixtureDB, "/planned/opencode", openCodeFixtureDB)...)
			}
			if wantErr != nil {
				steps = append(steps, stamp...)
			} else {
				steps = append(steps, openCodeMetadataQuerySteps(openCodeFixtureDB, "success", nil, stamp, stampOK)...)
				// A second fully completed call proves stamp-based row cache use,
				// or a complete SQL retry when ordinary DB errors disabled caching.
				if stampOK {
					steps = append(steps, stamp...)
					steps = append(steps, openCodeMetadataStages("cache-hit", "cache-copy", "cache-copy")...)
				} else {
					steps = append(steps, openCodeMetadataQuerySteps(openCodeFixtureDB, "success", nil, stamp, false)...)
				}
			}
			f := newOpenCodeMetadataFixture(t, steps)
			var rows [2][]openCodeRow
			var more [2]bool
			var codes [2]string
			var errs [2]error
			var entries []Entry
			var metadata openCodeRow
			var corrupt bool
			calls := 2
			if wantErr != nil {
				calls = 1
			}
			for i := 0; i < calls; i++ {
				errs[i] = f.owner.Do(context.Background(), func(c *conversationlife.Call) error {
					var err error
					if read {
						entries, more[i], corrupt, metadata, codes[i], err = f.reader.readIn(c, "ses_test", "", 2)
					} else {
						rows[i], more[i], codes[i], err = f.reader.queryIn(c, openCodeFixtureDB, "ses_test", "", 2)
					}
					return err
				})
			}
			f.check(t)
			if wantErr != nil {
				if !errors.Is(errs[0], wantErr) || codes[0] != "query_failed" || rows[0] != nil || entries != nil || metadata != (openCodeRow{}) || corrupt || more[0] {
					t.Fatalf("errs=%v codes=%v rows=%v entries=%v", errs, codes, rows, entries)
				}
			} else if errs != [2]error{} || codes != [2]string{} || more != [2]bool{} || !reflect.DeepEqual(rows[0], openCodeMetadataExpectedRows()) || !reflect.DeepEqual(rows[1], openCodeMetadataExpectedRows()) {
				t.Fatalf("errs=%v codes=%v more=%v rows=%v", errs, codes, more, rows)
			}
			wantCache := map[string]openCodeCacheEntry{}
			wantLayouts := map[string]openCodeLayoutEntry{}
			if wantErr == nil && stampOK {
				wantCache[openCodeFixtureDB+"\x00ses_test\x00\x002"] = openCodeCacheEntry{stamp: wantStamp, rows: openCodeMetadataExpectedRows()}
				wantLayouts[openCodeFixtureDB] = openCodeLayoutEntry{stamp: wantStamp, layout: openCodeLayout{v1: true}}
			}
			if !reflect.DeepEqual(f.reader.cache, wantCache) || !reflect.DeepEqual(f.reader.layouts, wantLayouts) {
				t.Fatalf("cache=%+v layouts=%+v", f.reader.cache, f.reader.layouts)
			}
		})
	}
}

// Independent fixed contracts: no production SQL builder supplies any expected
// key below. These logical rows do not execute or qualify SQLite semantics.
const openCodeSQLOracleV1 = `WITH cursor AS (SELECT NULL AS time_created,NULL AS id WHERE 0),selected AS (` +
	`SELECT m.id,m.time_created,m.data FROM message AS m WHERE m.session_id=CAST(X'7365735f74657374' AS TEXT) ORDER BY m.time_created DESC,m.id DESC LIMIT 3` +
	`) SELECT s.id AS session_id,s.directory,s.title,s.time_updated,COALESCE(s.agent,'') AS agent,` +
	`COALESCE(sm.id,'') AS message_id,COALESCE(sm.time_created,0) AS time_created,COALESCE(sm.data,'null') AS message_data,` +
	`COALESCE(p.id,'') AS part_id,COALESCE(p.data,'null') AS part_data,` +
	`(SELECT COUNT(*) FROM message WHERE session_id=s.id) AS message_total,1 AS cursor_found ` +
	`FROM session AS s LEFT JOIN selected AS sm ON 1=1 LEFT JOIN part AS p ON p.message_id=sm.id WHERE s.id=CAST(X'7365735f74657374' AS TEXT) ORDER BY sm.time_created DESC,sm.id DESC,p.id DESC;`

const openCodeSQLOracleV1Cursor = `WITH cursor AS (SELECT time_created,id FROM message WHERE session_id=CAST(X'7365735f74657374' AS TEXT) AND id=CAST(X'6c61746572' AS TEXT)),selected AS (` +
	`SELECT m.id,m.time_created,m.data FROM message AS m WHERE m.session_id=CAST(X'7365735f74657374' AS TEXT) AND EXISTS(SELECT 1 FROM cursor) AND (m.time_created < (SELECT time_created FROM cursor) OR (m.time_created = (SELECT time_created FROM cursor) AND m.id < (SELECT id FROM cursor))) ORDER BY m.time_created DESC,m.id DESC LIMIT 3` +
	`) SELECT s.id AS session_id,s.directory,s.title,s.time_updated,COALESCE(s.agent,'') AS agent,` +
	`COALESCE(sm.id,'') AS message_id,COALESCE(sm.time_created,0) AS time_created,COALESCE(sm.data,'null') AS message_data,` +
	`COALESCE(p.id,'') AS part_id,COALESCE(p.data,'null') AS part_data,` +
	`(SELECT COUNT(*) FROM message WHERE session_id=s.id) AS message_total,EXISTS(SELECT 1 FROM cursor) AS cursor_found ` +
	`FROM session AS s LEFT JOIN selected AS sm ON 1=1 LEFT JOIN part AS p ON p.message_id=sm.id WHERE s.id=CAST(X'7365735f74657374' AS TEXT) ORDER BY sm.time_created DESC,sm.id DESC,p.id DESC;`

const openCodeSQLOracleV2 = `WITH cursor AS (SELECT NULL AS seq WHERE 0),selected AS (` +
	`SELECT m.id,m.type,m.seq,m.time_created,m.data FROM session_message AS m WHERE m.session_id=CAST(X'7365735f74657374' AS TEXT) AND m.type IN ('user','assistant') ORDER BY m.seq DESC LIMIT 3` +
	`) SELECT s.id AS session_id,COALESCE(s.directory,'') AS directory,COALESCE(s.title,'') AS title,COALESCE(s.time_updated,0) AS time_updated,COALESCE(s.agent,'') AS agent,` +
	`COALESCE(sm.id,'') AS message_id,COALESCE(sm.type,'') AS message_type,COALESCE(sm.time_created,0) AS time_created,COALESCE(sm.data,'null') AS message_data,` +
	`(SELECT COUNT(*) FROM session_message WHERE session_id=s.id AND type IN ('user','assistant')) AS message_total,EXISTS(SELECT 1 FROM session_message WHERE session_id=s.id) AS has_messages,1 AS cursor_found ` +
	`FROM session_v2 AS s LEFT JOIN selected AS sm ON 1=1 WHERE s.id=CAST(X'7365735f74657374' AS TEXT) ORDER BY sm.seq DESC;`

const openCodeSQLOracleV2Cursor = `WITH cursor AS (SELECT seq FROM session_message WHERE session_id=CAST(X'7365735f74657374' AS TEXT) AND id=CAST(X'6c61746572' AS TEXT) AND type IN ('user','assistant')),selected AS (` +
	`SELECT m.id,m.type,m.seq,m.time_created,m.data FROM session_message AS m WHERE m.session_id=CAST(X'7365735f74657374' AS TEXT) AND m.type IN ('user','assistant') AND EXISTS(SELECT 1 FROM cursor) AND m.seq < (SELECT seq FROM cursor) ORDER BY m.seq DESC LIMIT 3` +
	`) SELECT s.id AS session_id,COALESCE(s.directory,'') AS directory,COALESCE(s.title,'') AS title,COALESCE(s.time_updated,0) AS time_updated,COALESCE(s.agent,'') AS agent,` +
	`COALESCE(sm.id,'') AS message_id,COALESCE(sm.type,'') AS message_type,COALESCE(sm.time_created,0) AS time_created,COALESCE(sm.data,'null') AS message_data,` +
	`(SELECT COUNT(*) FROM session_message WHERE session_id=s.id AND type IN ('user','assistant')) AS message_total,EXISTS(SELECT 1 FROM session_message WHERE session_id=s.id) AS has_messages,EXISTS(SELECT 1 FROM cursor) AS cursor_found ` +
	`FROM session_v2 AS s LEFT JOIN selected AS sm ON 1=1 WHERE s.id=CAST(X'7365735f74657374' AS TEXT) ORDER BY sm.seq DESC;`

const openCodeSQLOracleV1Rows = `[
{"session_id":"ses_test","title":"oracle","directory":"/planned/project","time_updated":9000,"agent":"fixture","message_id":"new","time_created":3000,"message_data":"{\"role\":\"user\"}","part_id":"p5","part_data":"{\"type\":\"text\",\"text\":\"newest\"}","message_total":4,"cursor_found":1},
{"session_id":"ses_test","title":"oracle","directory":"/planned/project","time_updated":9000,"agent":"fixture","message_id":"middle","time_created":2000,"message_data":"{\"role\":\"assistant\"}","part_id":"p4","part_data":"{\"type\":\"text\",\"text\":\"second part\"}","message_total":4,"cursor_found":1},
{"session_id":"ses_test","title":"oracle","directory":"/planned/project","time_updated":9000,"agent":"fixture","message_id":"middle","time_created":2000,"message_data":"{\"role\":\"assistant\"}","part_id":"p3","part_data":"{\"type\":\"text\",\"text\":\"first part\"}","message_total":4,"cursor_found":1},
{"session_id":"ses_test","message_id":"old","message_data":"{\"role\":\"user\"}","part_id":"p2","part_data":"{\"type\":\"text\",\"text\":\"discard second\"}","message_total":4,"cursor_found":1},
{"session_id":"ses_test","message_id":"old","message_data":"{\"role\":\"user\"}","part_id":"p1","part_data":"{\"type\":\"text\",\"text\":\"discard first\"}","message_total":4,"cursor_found":1}]`

// Logical descending seq order is new,middle,old; timestamps intentionally run
// the other way. seq is selected/ordered in SQL, NOT projected into openCodeRow.
const openCodeSQLOracleV2Rows = `[
{"session_id":"ses_test","title":"oracle","directory":"/planned/project","time_updated":9000,"agent":"fixture","message_id":"new","time_created":1000,"message_type":"user","message_data":"{\"text\":\"newest\"}","message_total":4,"cursor_found":1,"has_messages":1},
{"session_id":"ses_test","title":"oracle","directory":"/planned/project","time_updated":9000,"agent":"fixture","message_id":"middle","time_created":2000,"message_type":"user","message_data":"{\"text\":\"middle turn\"}","message_total":4,"cursor_found":1,"has_messages":1},
{"session_id":"ses_test","message_id":"old","time_created":3000,"message_type":"user","message_data":"{\"text\":\"discard\"}","message_total":4,"cursor_found":1,"has_messages":1}]`

func openCodeSQLOracleRows(v2 bool) []openCodeRow {
	// Fresh independent literals on every call, never decode/copy cache storage.
	if v2 {
		return []openCodeRow{
			{SessionID: "ses_test", Title: "oracle", Directory: "/planned/project", Updated: 9000, Agent: "fixture", MessageID: "middle", Created: 2000, MessageType: "user", MessageData: `{"text":"middle turn"}`, Total: 4, CursorFound: 1, HasMessages: 1, V2: true},
			{SessionID: "ses_test", Title: "oracle", Directory: "/planned/project", Updated: 9000, Agent: "fixture", MessageID: "new", Created: 1000, MessageType: "user", MessageData: `{"text":"newest"}`, Total: 4, CursorFound: 1, HasMessages: 1, V2: true},
		}
	}
	return []openCodeRow{
		{SessionID: "ses_test", Title: "oracle", Directory: "/planned/project", Updated: 9000, Agent: "fixture", MessageID: "middle", Created: 2000, MessageData: `{"role":"assistant"}`, PartID: "p3", PartData: `{"type":"text","text":"first part"}`, Total: 4, CursorFound: 1},
		{SessionID: "ses_test", Title: "oracle", Directory: "/planned/project", Updated: 9000, Agent: "fixture", MessageID: "middle", Created: 2000, MessageData: `{"role":"assistant"}`, PartID: "p4", PartData: `{"type":"text","text":"second part"}`, Total: 4, CursorFound: 1},
		{SessionID: "ses_test", Title: "oracle", Directory: "/planned/project", Updated: 9000, Agent: "fixture", MessageID: "new", Created: 3000, MessageData: `{"role":"user"}`, PartID: "p5", PartData: `{"type":"text","text":"newest"}`, Total: 4, CursorFound: 1},
	}
}

// This assembles effects, not SQL. Callers supply independent literal SQL keys
// and explicit row/stage counts; no f.plan/openCodeMetadataQuerySteps/builders.
func openCodeSQLOracleSteps(sql, layout, raw string, tables, rows, kept, parser int, more bool, failure string) []openCodeMetadataStep {
	steps := openCodeMetadataDiscovery([]string{"/planned/opencode"}, []string{openCodeFixtureDB})
	steps = append(steps, openCodeMetadataCandidate("/planned/opencode", openCodeFixtureDB, "/planned/opencode", openCodeFixtureDB)...)
	stamp := openCodeMetadataStamp(openCodeFixtureDB, openCodeFixtureInfo{100, 1}, nil, openCodeFixtureInfo{20, 2}, nil)
	steps = append(steps, stamp...)
	steps = append(steps, openCodeMetadataStages("cache-hit", "layout-hit")...)
	steps = append(steps, openCodeMetadataStep{kind: "ready", ready: true}, openCodeMetadataStep{kind: "query", path: openCodeFixtureDB, sql: openCodeFixtureLayout, raw: layout})
	if failure == "layout" {
		return steps
	}
	for i := 0; i < tables; i++ {
		steps = append(steps, openCodeMetadataStages("layout-row")...)
	}
	steps = append(steps, stamp...)
	steps = append(steps, openCodeMetadataStages("layout-publish")...)
	steps = append(steps, openCodeMetadataStep{kind: "ready", ready: true}, openCodeMetadataStep{kind: "query", path: openCodeFixtureDB, sql: sql, raw: raw})
	if failure == "message" {
		return steps
	}
	steps = append(steps, openCodeMetadataStages("decode")...)
	for i := 0; i < rows/2; i++ {
		steps = append(steps, openCodeMetadataStages("reverse")...)
	}
	for _, stage := range []string{"version", "distinct", "filter"} {
		if stage == "filter" && !more {
			continue
		}
		for i := 0; i < rows; i++ {
			steps = append(steps, openCodeMetadataStages(stage)...)
		}
	}
	for i := 0; i < kept; i++ {
		steps = append(steps, openCodeMetadataStages("cache-copy")...)
	}
	steps = append(steps, stamp...)
	steps = append(steps, openCodeMetadataStages("cache-publish")...)
	for i := 0; i < parser; i++ {
		steps = append(steps, openCodeMetadataStages("parser-check")...)
	}
	return steps
}

func TestInstalledOpenCodeIndependentSQLPages(t *testing.T) {
	for _, tc := range []struct {
		name, sql, before string
		v2                bool
	}{
		{"v1-none", openCodeSQLOracleV1, "", false},
		{"v1-cursor", openCodeSQLOracleV1Cursor, "later", false},
		{"v2-none", openCodeSQLOracleV2, "", true},
		{"v2-cursor", openCodeSQLOracleV2Cursor, "later", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			layout, raw := `[{"name":"session"},{"name":"message"},{"name":"part"}]`, openCodeSQLOracleV1Rows
			tables, rows, kept, parser := 3, 5, 3, 7
			wantLayout := openCodeLayout{v1: true}
			wantEntries := []Entry{{ID: "middle", Role: "assistant", Text: "first part\nsecond part", Timestamp: "1970-01-01T00:00:02Z"}, {ID: "new", Role: "user", Text: "newest", Timestamp: "1970-01-01T00:00:03Z"}}
			if tc.v2 {
				layout, raw = `[{"name":"session_v2"},{"name":"session_message"}]`, openCodeSQLOracleV2Rows
				tables, rows, kept, parser = 2, 3, 2, 6
				wantLayout = openCodeLayout{v2: true}
				wantEntries = []Entry{{ID: "middle", Role: "user", Text: "middle turn", Timestamp: "1970-01-01T00:00:02Z"}, {ID: "new", Role: "user", Text: "newest", Timestamp: "1970-01-01T00:00:01Z"}}
			}
			f := newOpenCodeMetadataFixture(t, openCodeSQLOracleSteps(tc.sql, layout, raw, tables, rows, kept, parser, true, ""))
			var entries []Entry
			var more, corrupt bool
			var metadata openCodeRow
			var code string
			err := f.owner.Do(context.Background(), func(c *conversationlife.Call) error {
				var err error
				entries, more, corrupt, metadata, code, err = f.reader.readIn(c, "ses_test", tc.before, 2)
				return err
			})
			f.check(t)
			wantMetadata := openCodeSQLOracleRows(tc.v2)[0]
			wantMetadata.Database = openCodeFixtureDB
			if err != nil || code != "" || !more || corrupt || !reflect.DeepEqual(entries, wantEntries) || metadata != wantMetadata {
				t.Fatalf("err=%v code=%q more=%v corrupt=%v entries=%+v metadata=%+v", err, code, more, corrupt, entries, metadata)
			}
			stamp := openCodeFileStamp{databaseSize: 100, databaseTime: 1, walSize: 20, walTime: 2}
			wantCache := map[string]openCodeCacheEntry{openCodeFixtureDB + "\x00ses_test\x00" + tc.before + "\x002": {stamp: stamp, rows: openCodeSQLOracleRows(tc.v2), hasMore: true}}
			wantLayouts := map[string]openCodeLayoutEntry{openCodeFixtureDB: {stamp: stamp, layout: wantLayout}}
			if !reflect.DeepEqual(f.reader.cache, wantCache) || !reflect.DeepEqual(f.reader.layouts, wantLayouts) {
				t.Fatal("independent complete row/layout cache mismatch")
			}
		})
	}
}

const openCodeByteBoundaryLayout = `[{"name":"session"},{"name":"message"},{"name":"part"},{"name":"session_v2"},{"name":"session_message"}]`
const openCodeByteBoundaryRow = `[{"session_id":"ses_test","title":"bounded","directory":"/planned/project","time_updated":7000,"agent":"fixture","message_id":"one","message_type":"user","message_data":"{\"text\":\"hello\"}","message_total":1,"cursor_found":1,"has_messages":1}]`

func openCodeByteBoundaryExpectedRows() []openCodeRow {
	return []openCodeRow{{SessionID: "ses_test", Title: "bounded", Directory: "/planned/project", Updated: 7000, Agent: "fixture", MessageID: "one", MessageType: "user", MessageData: `{"text":"hello"}`, Total: 1, CursorFound: 1, HasMessages: 1, V2: true}}
}

type openCodeByteBoundaryReceipt struct {
	sql           string
	max, rawBytes int
}

func TestInstalledOpenCodeLogicalByteBoundaries(t *testing.T) {
	for _, tc := range []struct {
		name, stage string
		delta       int
	}{
		{"layout-under", "layout", -1}, {"layout-exact", "layout", 0}, {"layout-over", "layout", 1},
		{"message-under", "message", -1}, {"message-exact", "message", 0}, {"message-over", "message", 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			const expectedMax = 8 * 1024 * 1024
			target := expectedMax + tc.delta
			failure := ""
			if tc.delta > 0 {
				failure = tc.stage
			}
			// Only small literal replies live in the script. The wrapper allocates
			// ONE current raw reply; no six-case large-payload table or raw logs.
			f := newOpenCodeMetadataFixture(t, openCodeSQLOracleSteps(openCodeSQLOracleV2, openCodeByteBoundaryLayout, openCodeByteBoundaryRow, 5, 1, 1, 5, false, failure))
			baseQuery := f.reader.installed.deps.query
			var receipts []openCodeByteBoundaryReceipt
			f.reader.installed.deps.query = func(c *conversationlife.Call, db, sql string, max int) ([]byte, error) {
				// The complete private script validates exact DB/SQL/order/live Call
				// and literal expected max BEFORE this logical enforcement model.
				raw, err := baseQuery(c, db, sql, max)
				if err != nil {
					return nil, err
				}
				selected := (tc.stage == "layout" && sql == openCodeFixtureLayout) || (tc.stage == "message" && sql == openCodeSQLOracleV2)
				if selected {
					// ASCII whitespace pads valid JSON to an exact RAW BYTE length.
					// One target-sized []byte plus the small base conversion; no
					// target-sized string/conversion copy is needed or retained.
					padded := make([]byte, target)
					n := copy(padded, raw)
					for i := n; i < len(padded); i++ {
						padded[i] = ' '
					}
					raw = padded
				}
				receipts = append(receipts, openCodeByteBoundaryReceipt{sql: sql, max: max, rawBytes: len(raw)})
				// Explicit LOGICAL backend enforcement of the PASSED max, before
				// provider normalization. Not a provider/native/CLI limiter proof.
				if len(raw) > max {
					return nil, sqliteexec.ErrOutputLimit
				}
				return raw, nil
			}
			var entries []Entry
			var more, corrupt bool
			var metadata openCodeRow
			var code string
			err := f.owner.Do(context.Background(), func(c *conversationlife.Call) error {
				var err error
				entries, more, corrupt, metadata, code, err = f.reader.readIn(c, "ses_test", "", 2)
				return err
			})
			f.check(t)
			layoutBytes, messageBytes := len(openCodeByteBoundaryLayout), len(openCodeByteBoundaryRow)
			if tc.stage == "layout" {
				layoutBytes = target
			} else {
				messageBytes = target
			}
			wantReceipts := []openCodeByteBoundaryReceipt{{sql: openCodeFixtureLayout, max: expectedMax, rawBytes: layoutBytes}}
			if failure != "layout" {
				wantReceipts = append(wantReceipts, openCodeByteBoundaryReceipt{sql: openCodeSQLOracleV2, max: expectedMax, rawBytes: messageBytes})
			}
			if !reflect.DeepEqual(receipts, wantReceipts) {
				t.Fatalf("budget/raw-length receipts mismatch: got=%d want=%d", len(receipts), len(wantReceipts))
			}
			if failure != "" {
				if !errors.Is(err, sqliteexec.ErrOutputLimit) || code != "output_limit" || entries != nil || more || corrupt || metadata != (openCodeRow{}) {
					t.Fatalf("err=%v code=%q entries=%d more=%v corrupt=%v", err, code, len(entries), more, corrupt)
				}
			} else {
				wantMetadata := openCodeByteBoundaryExpectedRows()[0]
				wantMetadata.Database = openCodeFixtureDB
				if err != nil || code != "" || more || corrupt || !reflect.DeepEqual(entries, []Entry{{ID: "one", Role: "user", Text: "hello"}}) || metadata != wantMetadata {
					t.Fatalf("err=%v code=%q entries=%d more=%v corrupt=%v", err, code, len(entries), more, corrupt)
				}
			}
			stamp := openCodeFileStamp{databaseSize: 100, databaseTime: 1, walSize: 20, walTime: 2}
			wantCache := map[string]openCodeCacheEntry{}
			wantLayouts := map[string]openCodeLayoutEntry{}
			if failure != "layout" {
				// Earlier successful layout publication is valid even when the
				// message reply exceeds budget. No invented rollback assertion.
				wantLayouts[openCodeFixtureDB] = openCodeLayoutEntry{stamp: stamp, layout: openCodeLayout{v1: true, v2: true}}
			}
			if failure == "" {
				wantCache[openCodeFixtureDB+"\x00ses_test\x00\x002"] = openCodeCacheEntry{stamp: stamp, rows: openCodeByteBoundaryExpectedRows()}
			}
			if !reflect.DeepEqual(f.reader.cache, wantCache) || !reflect.DeepEqual(f.reader.layouts, wantLayouts) {
				t.Fatal("logical byte-boundary publication mismatch")
			}
		})
	}
}

// Fresh literals on EACH invocation: the seed and expected maps never share rows.
func openCodePublicationSeeds(rowCount, layoutCount int) (map[string]openCodeCacheEntry, map[string]openCodeLayoutEntry) {
	old := openCodeFileStamp{databaseSize: 90, databaseTime: 9, walSize: 10, walTime: 8}
	rows := map[string]openCodeCacheEntry{
		openCodeFixtureDB + "\x00ses_test\x00\x002": {stamp: old, rows: []openCodeRow{{MessageID: "good", MessageData: "preserved"}}, hasMore: true},
		"sibling": {rows: []openCodeRow{{MessageID: "sibling-good", PartData: "preserved"}}},
	}
	layouts := map[string]openCodeLayoutEntry{
		openCodeFixtureDB: {stamp: old, layout: openCodeLayout{v2: true}},
		"sibling":         {layout: openCodeLayout{v1: true, v2: true}},
	}
	for i := 2; i < rowCount; i++ {
		rows[fmt.Sprintf("seed-%d", i)] = openCodeCacheEntry{rows: []openCodeRow{{MessageID: fmt.Sprintf("good-%d", i)}}}
	}
	for i := 2; i < layoutCount; i++ {
		layouts[fmt.Sprintf("seed-%d", i)] = openCodeLayoutEntry{stamp: old, layout: openCodeLayout{v2: true}}
	}
	return rows, layouts
}

func openCodePublicationStamp(change string) []openCodeMetadataStep {
	db, wal := openCodeFixtureInfo{100, 1}, openCodeFixtureInfo{20, 2}
	if change == "db" {
		db = openCodeFixtureInfo{101, 3}
	}
	if change == "wal" {
		wal = openCodeFixtureInfo{21, 4}
	}
	return openCodeMetadataStamp(openCodeFixtureDB, db, nil, wal, nil)
}

// Retain the complete script validator underneath the extra timed-query guard.
func openCodeTerminalBudgetGuard(f *openCodeMetadataFixture) {
	base := f.reader.installed.deps.query
	f.reader.installed.deps.query = func(c *conversationlife.Call, db, sql string, max int) ([]byte, error) {
		raw, err := base(c, db, sql, max)
		if f.unknown != 0 {
			return nil, conversationlife.ErrRefused
		}
		deadline, ok := c.Context().Deadline()
		if !ok || !deadline.After(time.Now()) || deadline.After(time.Now().Add(10*time.Second)) {
			f.unknown++
			f.owner.Begin()
			return nil, conversationlife.ErrRefused
		}
		return raw, err
	}
}

func TestInstalledOpenCodePublicationProtection(t *testing.T) {
	for _, tc := range []struct {
		name, target, change    string
		empty, cancel, capacity bool
	}{
		{"cancel-positive", "row", "", false, true, false},
		{"cancel-empty", "row", "", true, true, false},
		{"cancel-layout", "layout", "", false, true, false},
		{"cancel-positive-capacity", "row", "", false, true, true},
		{"cancel-empty-capacity", "row", "", true, true, true},
		{"cancel-layout-capacity", "layout", "", false, true, true},
		{"stamp-positive-db", "row", "db", false, false, false},
		{"stamp-positive-wal", "row", "wal", false, false, false},
		{"stamp-empty-db", "row", "db", true, false, false},
		{"stamp-empty-wal", "row", "wal", true, false, false},
		{"stamp-layout-db", "layout", "db", false, false, false},
		{"stamp-layout-wal", "layout", "wal", false, false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var steps []openCodeMetadataStep
			if tc.target == "row" {
				steps = append(steps, openCodePublicationStamp("")...)
				steps = append(steps, openCodeMetadataStages("cache-hit")...)
			}
			steps = append(steps, openCodeMetadataStages("layout-hit")...)
			steps = append(steps, openCodeMetadataStep{kind: "ready", ready: true}, openCodeMetadataStep{kind: "query", path: openCodeFixtureDB, sql: openCodeFixtureLayout, raw: `[{"name":"session"},{"name":"message"},{"name":"part"}]`})
			steps = append(steps, openCodeMetadataStages("layout-row", "layout-row", "layout-row")...)
			layoutChange := ""
			if tc.target == "layout" {
				layoutChange = tc.change
			}
			steps = append(steps, openCodePublicationStamp(layoutChange)...)
			if layoutChange == "" {
				steps = append(steps, openCodeMetadataStages("layout-publish")...)
			}
			if tc.target == "row" {
				raw := openCodeMetadataRows
				if tc.empty {
					raw = "[]"
				}
				steps = append(steps, openCodeMetadataStep{kind: "ready", ready: true}, openCodeMetadataStep{kind: "query", path: openCodeFixtureDB, sql: openCodeSQLOracleV1, raw: raw})
				steps = append(steps, openCodeMetadataStages("decode")...)
				if !tc.empty {
					steps = append(steps, openCodeMetadataStages("reverse", "version", "version", "distinct", "distinct", "cache-copy", "cache-copy")...)
				}
				steps = append(steps, openCodePublicationStamp(tc.change)...)
				if tc.cancel {
					steps = append(steps, openCodeMetadataStages("cache-publish")...)
				}
			}
			f := newOpenCodeMetadataFixture(t, steps)
			openCodeTerminalBudgetGuard(f)
			rowCount, layoutCount := 2, 2
			if tc.capacity {
				if tc.target == "row" {
					rowCount = 128
				} else {
					layoutCount = 128
				}
			}
			f.reader.cache, f.reader.layouts = openCodePublicationSeeds(rowCount, layoutCount)
			wantCache, wantLayouts := openCodePublicationSeeds(rowCount, layoutCount)
			stamp := openCodeFileStamp{databaseSize: 100, databaseTime: 1, walSize: 20, walTime: 2}
			if tc.target == "row" {
				// This publication succeeds BEFORE the failed row replacement.
				wantLayouts[openCodeFixtureDB] = openCodeLayoutEntry{stamp: stamp, layout: openCodeLayout{v1: true}}
			}
			selected := 0
			baseObserve := f.reader.observe
			f.reader.observe = func(stage string) {
				baseObserve(stage)
				if f.unknown == 0 && tc.cancel && ((tc.target == "row" && stage == "cache-publish") || (tc.target == "layout" && stage == "layout-publish")) {
					selected++
					// Stage hooks expose no Call. This proves selected owner descent
					// and write suppression, NOT captured caller-context propagation.
					f.owner.Begin()
				}
			}
			var rows []openCodeRow
			var layout openCodeLayout
			var more bool
			var code string
			err := f.owner.Do(context.Background(), func(c *conversationlife.Call) error {
				var err error
				if tc.target == "layout" {
					layout, code, err = f.reader.layoutIn(c, openCodeFixtureDB, stamp, true)
				} else {
					rows, more, code, err = f.reader.queryIn(c, openCodeFixtureDB, "ses_test", "", 2)
				}
				return err
			})
			f.check(t)
			if tc.cancel {
				if !errors.Is(err, context.Canceled) || code != "query_failed" || selected != 1 || rows != nil || more || layout != (openCodeLayout{}) {
					t.Fatalf("cancel err=%v code=%q selected=%d rows=%v layout=%+v", err, code, selected, rows, layout)
				}
			} else {
				if err != nil || code != "" || selected != 0 || more {
					t.Fatalf("stamp err=%v code=%q selected=%d more=%v", err, code, selected, more)
				}
				if tc.target == "layout" {
					if layout != (openCodeLayout{v1: true}) || rows != nil {
						t.Fatal("fresh layout lost on stamp-only invalidation")
					}
				} else {
					wantRows := openCodeMetadataExpectedRows()
					if tc.empty {
						wantRows = []openCodeRow{}
					}
					if !reflect.DeepEqual(rows, wantRows) {
						t.Fatal("fresh rows lost on stamp-only invalidation")
					}
				}
			}
			if !reflect.DeepEqual(f.reader.cache, wantCache) || !reflect.DeepEqual(f.reader.layouts, wantLayouts) {
				t.Fatal("same-key replacement/clear changed independently seeded maps")
			}
		})
	}
}

// DB2 discovery is eager; the script forbids DB2 SQL, not its earlier metadata.
func openCodeTerminalSteps(stage string, failure error) []openCodeMetadataStep {
	steps := openCodeMetadataDiscovery([]string{"/planned/opencode", openCodeMetadataRoot2}, []string{openCodeFixtureDB, openCodeMetadataDB2})
	steps = append(steps, openCodeMetadataCandidate("/planned/opencode", openCodeFixtureDB, "/planned/opencode", openCodeFixtureDB)...)
	steps = append(steps, openCodeMetadataCandidate(openCodeMetadataRoot2, openCodeMetadataDB2, openCodeMetadataRoot2, openCodeMetadataDB2)...)
	steps = append(steps, openCodePublicationStamp("")...)
	steps = append(steps, openCodeMetadataStages("cache-hit", "layout-hit")...)
	layout := openCodeMetadataStep{kind: "query", path: openCodeFixtureDB, sql: openCodeFixtureLayout, raw: openCodeByteBoundaryLayout}
	if stage == "layout" {
		layout.err = failure
	}
	steps = append(steps, openCodeMetadataStep{kind: "ready", ready: true}, layout)
	if stage == "layout" {
		return steps
	}
	steps = append(steps, openCodeMetadataStages("layout-row", "layout-row", "layout-row", "layout-row", "layout-row")...)
	steps = append(steps, openCodePublicationStamp("")...)
	steps = append(steps, openCodeMetadataStages("layout-publish")...)
	return append(steps, openCodeMetadataStep{kind: "ready", ready: true}, openCodeMetadataStep{kind: "query", path: openCodeFixtureDB, sql: openCodeSQLOracleV2, raw: openCodeByteBoundaryRow, err: failure})
}

// Deliberate witnesses require a separate inspector, not weakening f.check.
func openCodeTerminalInspect(t *testing.T, f *openCodeMetadataFixture, raw []error, counts []int) {
	t.Helper()
	f.owner.Join()
	if f.unknown != 0 || f.next != len(f.steps) || len(f.seen) != len(f.steps) {
		t.Fatalf("unknown=%d consumed=%d/%d effects=%v", f.unknown, f.next, len(f.steps), f.seen)
	}
	witness := f.owner.Witnesses()
	if len(raw) == 0 {
		if witness != nil || len(counts) != 0 {
			t.Fatal("request cancellation/error poisoned logical witness ledger")
		}
		return
	}
	joined, ok := witness.(interface{ Unwrap() []error })
	if !ok || len(joined.Unwrap()) != len(raw) || len(counts) != len(raw) {
		t.Fatal("logical witness cardinality mismatch")
	}
	for i, operand := range raw {
		if joined.Unwrap()[i] != operand || counts[i] != 1 {
			t.Fatalf("logical witness identity/close count mismatch at %d: closes=%d", i, counts[i])
		}
	}
}

func openCodeTerminalSlots(t *testing.T, f *openCodeMetadataFixture, n int) ([]*conversationlife.CloseSlot, []error, []int) {
	t.Helper()
	var slots []*conversationlife.CloseSlot
	var raw []error
	counts := make([]int, n)
	// Equal ERROR identity, independent SLOT identities: each must be retained.
	equal := errors.New("private logical close operand")
	for i := 0; i < n; i++ {
		index := i
		slot, err := f.owner.Own(func() error { counts[index]++; return equal })
		if err != nil {
			t.Fatal(err)
		}
		// Armed immediately. Assertions run before cleanup can create evidence.
		t.Cleanup(func() { _ = slot.Close() })
		slots = append(slots, slot)
		raw = append(raw, equal)
	}
	return slots, raw, counts
}

func openCodeTerminalMaps(t *testing.T, f *openCodeMetadataFixture, stage string) {
	t.Helper()
	wantCache, wantLayouts := openCodePublicationSeeds(2, 2)
	if stage == "message" {
		wantLayouts[openCodeFixtureDB] = openCodeLayoutEntry{stamp: openCodeFileStamp{databaseSize: 100, databaseTime: 1, walSize: 20, walTime: 2}, layout: openCodeLayout{v1: true, v2: true}}
	}
	if !reflect.DeepEqual(f.reader.cache, wantCache) || !reflect.DeepEqual(f.reader.layouts, wantLayouts) {
		t.Fatal("terminal SQL published rows, lost seed, or rolled back earlier layout")
	}
}

func TestInstalledOpenCodeTerminalSQLMarkers(t *testing.T) {
	for _, stage := range []string{"layout", "message"} {
		t.Run(stage, func(t *testing.T) {
			primary := errors.New("independent logical primary")
			for _, tc := range []struct {
				name     string
				semantic error
				slots    int
			}{
				{"canceled", context.Canceled, 0}, {"deadline", context.DeadlineExceeded, 0},
				{"refusal", conversationlife.ErrRefused, 0}, {"cleanup", nil, 1},
				{"primary-cleanup", primary, 1}, {"cancel-cleanup", context.Canceled, 1},
				{"deadline-cleanup", context.DeadlineExceeded, 1}, {"equal-cleanup-siblings", primary, 2},
			} {
				t.Run(tc.name, func(t *testing.T) {
					failure := tc.semantic
					if tc.slots > 0 {
						failure = errors.Join(failure, sqliteexec.ErrCleanupUncertain)
					}
					f := newOpenCodeMetadataFixture(t, openCodeTerminalSteps(stage, failure))
					openCodeTerminalBudgetGuard(f)
					f.reader.cache, f.reader.layouts = openCodePublicationSeeds(2, 2)
					slots, raw, counts := openCodeTerminalSlots(t, f, tc.slots)
					base := f.reader.installed.deps.query
					selected, live, cached := 0, false, true
					f.reader.installed.deps.query = func(c *conversationlife.Call, db, sql string, max int) ([]byte, error) {
						data, err := base(c, db, sql, max)
						if f.unknown != 0 {
							return nil, conversationlife.ErrRefused
						}
						if (stage == "layout" && sql == openCodeFixtureLayout) || (stage == "message" && sql == openCodeSQLOracleV2) {
							selected++
							live = c.Context().Err() == nil
							for i, slot := range slots {
								first, repeat := slot.Close(), slot.Close()
								cached = cached && first == raw[i] && repeat == raw[i]
							}
							live = live && c.Context().Err() == nil
						}
						return data, err
					}
					var entries []Entry
					var more, corrupt bool
					var metadata openCodeRow
					var code string
					err := f.owner.Do(context.Background(), func(c *conversationlife.Call) error {
						var err error
						entries, more, corrupt, metadata, code, err = f.reader.readIn(c, "ses_test", "", 2)
						return err
					})
					openCodeTerminalInspect(t, f, raw, counts)
					if selected != 1 || !live || !cached || code != "query_failed" || entries != nil || more || corrupt || metadata != (openCodeRow{}) {
						t.Fatalf("selected=%d live=%v cached=%v code=%q err=%v", selected, live, cached, code, err)
					}
					for _, identity := range []error{context.Canceled, context.DeadlineExceeded, conversationlife.ErrRefused, primary, sqliteexec.ErrCleanupUncertain} {
						if errors.Is(err, identity) != errors.Is(failure, identity) {
							t.Fatalf("semantic identity lost/added: %v in %v", identity, err)
						}
					}
					for _, operand := range raw {
						if errors.Is(err, operand) {
							t.Fatal("raw close operand escaped safe SQL result")
						}
					}
					openCodeTerminalMaps(t, f, stage)
				})
			}
		})
	}
}

func TestInstalledOpenCodeTerminalSQLActualCancellation(t *testing.T) {
	for _, tc := range []struct {
		name, stage, source string
		slots               int
	}{
		{"caller-layout", "layout", "caller", 0}, {"owner-layout", "layout", "owner", 0},
		{"caller-message", "message", "caller", 0}, {"owner-message", "message", "owner", 0},
		{"caller-layout-cleanup", "layout", "caller", 2}, {"owner-layout-cleanup", "layout", "owner", 2},
		{"caller-message-cleanup", "message", "caller", 2}, {"owner-message-cleanup", "message", "owner", 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// The script supplies success; only actual reached cancellation supplies
			// Canceled. No semantic cancellation marker substitutes for observation.
			f := newOpenCodeMetadataFixture(t, openCodeTerminalSteps(tc.stage, nil))
			openCodeTerminalBudgetGuard(f)
			f.reader.cache, f.reader.layouts = openCodePublicationSeeds(2, 2)
			slots, raw, counts := openCodeTerminalSlots(t, f, tc.slots)
			ctx, cancel := context.WithCancel(context.Background())
			entered, observed, release, done := make(chan struct{}), make(chan struct{}), make(chan struct{}), make(chan struct{})
			var once sync.Once
			releaseAll := func() { once.Do(func() { close(release) }) }
			var work context.Context
			var selectedErr error
			selected, observations := 0, 0
			cached := true
			base := f.reader.installed.deps.query
			f.reader.installed.deps.query = func(c *conversationlife.Call, db, sql string, max int) ([]byte, error) {
				data, err := base(c, db, sql, max)
				if err != nil || f.unknown != 0 {
					return nil, err
				}
				if (tc.stage == "layout" && sql == openCodeFixtureLayout) || (tc.stage == "message" && sql == openCodeSQLOracleV2) {
					selected++
					if selected == 1 {
						work = c.Context() // THAT reached SQL Call, before notice.
						close(entered)
					}
					select {
					case <-c.Context().Done():
						observations++
						if observations == 1 {
							selectedErr = c.Context().Err()
							close(observed)
						}
					case <-release: // independently retireable even if propagation breaks
					}
					<-release
					for i, slot := range slots {
						first, repeat := slot.Close(), slot.Close()
						cached = cached && first == raw[i] && repeat == raw[i]
					}
					if len(slots) > 0 {
						return nil, errors.Join(c.Context().Err(), sqliteexec.ErrCleanupUncertain)
					}
					return nil, c.Context().Err()
				}
				return data, err
			}
			var entries []Entry
			var more, corrupt bool
			var metadata openCodeRow
			var code string
			var got error
			var recovered any
			returned := false
			// All completions and cleanup exist before the single sealed actual job.
			// This cleanup runs before slot fallback cleanup and before f's Join.
			t.Cleanup(func() { cancel(); f.owner.Begin(); releaseAll(); <-done; f.owner.Join() })
			go func() {
				defer close(done)
				defer func() { recovered = recover() }()
				got = f.owner.Do(ctx, func(c *conversationlife.Call) error {
					var err error
					entries, more, corrupt, metadata, code, err = f.reader.readIn(c, "ses_test", "", 2)
					return err
				})
				returned = true
			}()
			entryTimer := time.NewTimer(time.Second)
			defer entryTimer.Stop()
			select {
			case <-done:
				t.Fatal("actual SQL caller exited before work entry")
			case <-entered:
			case <-entryTimer.C:
				t.Fatal("actual SQL work entry not observed; cleanup releases and joins")
			}
			if tc.source == "caller" {
				cancel()
			} else {
				f.owner.Begin()
			}
			observeTimer := time.NewTimer(time.Second)
			defer observeTimer.Stop()
			select {
			case <-done:
				t.Fatal("actual caller exited before selected cancellation receipt")
			case <-observed:
			case <-observeTimer.C:
				t.Fatal("actual SQL cancellation absent before fallback; cleanup releases and joins")
			}
			if !errors.Is(selectedErr, context.Canceled) || !errors.Is(work.Err(), context.Canceled) {
				t.Fatal("chosen cancellation absent on actual SQL context")
			}
			// Only now may owner fallback fence. The selected observation predates it.
			receipt := f.owner.Begin()
			holdTimer := time.NewTimer(20 * time.Millisecond)
			defer holdTimer.Stop()
			select {
			case <-done:
				t.Fatal("actual SQL callback detached while held")
			case <-receipt:
				t.Fatal("actual held SQL operation lost owner count")
			case <-holdTimer.C:
			}
			releaseAll()
			<-done
			openCodeTerminalInspect(t, f, raw, counts)
			if !returned || recovered != nil || selected != 1 || observations != 1 || !cached || !errors.Is(got, context.Canceled) || errors.Is(got, context.DeadlineExceeded) || errors.Is(got, conversationlife.ErrRefused) || errors.Is(got, sqliteexec.ErrCleanupUncertain) != (tc.slots > 0) || code != "query_failed" || entries != nil || more || corrupt || metadata != (openCodeRow{}) {
				t.Fatalf("returned=%v panic=%v selected=%d observed=%d cached=%v code=%q err=%v", returned, recovered, selected, observations, cached, code, got)
			}
			for _, operand := range raw {
				if errors.Is(got, operand) {
					t.Fatal("raw logical cleanup error escaped")
				}
			}
			openCodeTerminalMaps(t, f, tc.stage)
		})
	}
}

// Inputs are chronological independent literals; only the backend serialization
// reverses them. Neither expected entries nor raw-cache expectations use a parser.
func openCodeSemanticRead(t *testing.T, v2 bool, rows []openCodeRow, want []Entry, damaged bool, checks int) {
	t.Helper()
	wire := make([]openCodeRow, len(rows))
	for i := range rows {
		wire[len(rows)-1-i] = rows[i]
	}
	raw, err := json.Marshal(wire)
	if err != nil {
		t.Fatal("input serialization failed")
	}
	sql, layout, tables := openCodeSQLOracleV1, `[{"name":"session"},{"name":"message"},{"name":"part"}]`, 3
	wantLayout := openCodeLayout{v1: true}
	if v2 {
		sql, layout, tables = openCodeSQLOracleV2, `[{"name":"session_v2"},{"name":"session_message"}]`, 2
		wantLayout = openCodeLayout{v2: true}
	}
	steps := openCodeSQLOracleSteps(sql, layout, string(raw), tables, len(rows), len(rows), checks, false, "")
	// Eagerly discover a real second logical candidate, but allow NO DB2 SQL:
	// empty/corrupt parsed content is still a selected DB1 result.
	discovery := openCodeMetadataDiscovery([]string{"/planned/opencode", openCodeMetadataRoot2}, []string{openCodeFixtureDB, openCodeMetadataDB2})
	discovery = append(discovery, openCodeMetadataCandidate("/planned/opencode", openCodeFixtureDB, "/planned/opencode", openCodeFixtureDB)...)
	discovery = append(discovery, openCodeMetadataCandidate(openCodeMetadataRoot2, openCodeMetadataDB2, openCodeMetadataRoot2, openCodeMetadataDB2)...)
	steps = append(discovery, steps[6:]...)
	f := newOpenCodeMetadataFixture(t, steps)
	openCodeTerminalBudgetGuard(f)
	var entries []Entry
	var more, corrupt bool
	var metadata openCodeRow
	var code string
	err = f.owner.Do(context.Background(), func(c *conversationlife.Call) error {
		var e error
		entries, more, corrupt, metadata, code, e = f.reader.readIn(c, "ses_test", "", 2)
		return e
	})
	f.check(t)
	wantMetadata := rows[0]
	wantMetadata.Database = openCodeFixtureDB
	if err != nil || code != "" || more || corrupt != damaged || metadata != wantMetadata || !reflect.DeepEqual(entries, want) {
		// Never log bounded-but-large inputs, outputs, metadata or cache rows.
		t.Fatalf("semantic result mismatch: error=%v code=%q more=%v corrupt=%v entries=%d checks=%d", err, code, more, corrupt, len(entries), checks)
	}
	stamp := openCodeFileStamp{databaseSize: 100, databaseTime: 1, walSize: 20, walTime: 2}
	wantCache := map[string]openCodeCacheEntry{openCodeFixtureDB + "\x00ses_test\x00\x002": {stamp: stamp, rows: rows}}
	wantLayouts := map[string]openCodeLayoutEntry{openCodeFixtureDB: {stamp: stamp, layout: wantLayout}}
	if !reflect.DeepEqual(f.reader.cache, wantCache) || !reflect.DeepEqual(f.reader.layouts, wantLayouts) {
		t.Fatal("complete independent raw cache/layout mismatch")
	}
	// Demonstrate expectation storage is not the cache's backing array.
	f.reader.cache[openCodeFixtureDB+"\x00ses_test\x00\x002"].rows[0].Title = "cache-only mutation"
	if rows[0].Title == "cache-only mutation" {
		t.Fatal("oracle aliases cache storage")
	}
}

func openCodeSemanticRow(v2 bool, message, part string) openCodeRow {
	return openCodeRow{SessionID: "ses_test", Title: "semantics", Directory: "/planned/project", Updated: 9000, Agent: "fixture", MessageID: "m", Created: 1000, MessageType: "assistant", MessageData: message, PartID: "p", PartData: part, Total: 1, CursorFound: 1, HasMessages: 1, V2: v2}
}

func TestInstalledOpenCodeFullToolValues(t *testing.T) {
	// Explicit shape contrasts, not a cross-product. State strings are embedded
	// verbatim inside message strings: { } and [ ] must NOT be re-marshalled.
	for _, tc := range []struct {
		name, state, output string
		v2, failed, corrupt bool
		checks              int
	}{
		{"v1-string", `{"input":{"path":"x"},"output":" one\r\n\u001b[31mtwo\u001b[0m\u0000 "}`, "one\ntwo", false, false, false, 4},
		{"v1-status-error", `{"status":"error","output":"ok"}`, "ok", false, true, false, 4},
		{"v1-status-case", `{"status":"Error","error":"  ","output":"ok"}`, "ok", false, false, false, 4},
		{"v1-error-empty", `{"error":"","output":"ok"}`, "ok", false, false, false, 4},
		{"v1-error-text", `{"status":"completed","error":"failed","output":"ok"}`, "ok", false, true, false, 4},
		{"v1-error-wrong-type", `{"error":false,"output":"ok"}`, "", false, false, true, 4},
		{"v1-output-object-corrupt", `{"output":{}}`, "", false, false, true, 4},
		{"v1-output-null", `{"output":null}`, "", false, false, false, 4},
		{"v2-blocks-precede-string", `{"input":{"path":"x"},"content":[{"text":"one\n"},{"type":"image","text":"ignored"},{"type":"text","text":"two"},{"text":""},{"text":"three\r\u0000\u001b[0m"}],"output":"fallback"}`, "one\ntwo\nthree", true, false, false, 16},
		{"v2-content-empty", `{"content":[],"output":"fallback"}`, "fallback", true, false, false, 11},
		{"v2-content-null", `{"content":null,"output":"fallback"}`, "fallback", true, false, false, 11},
		{"v2-content-object", `{"content":{},"output":"fallback"}`, "fallback", true, false, false, 11},
		{"v2-content-string", `{"content":"wrong","output":"fallback"}`, "fallback", true, false, false, 11},
		{"v2-content-unusable", `{"content":[{"type":"image","text":"ignored"},{"text":""}],"output":"fallback"}`, "fallback", true, false, false, 13},
		{"v2-block-wrong-text", `{"content":[{"text":1}],"output":"fallback"}`, "fallback", true, false, false, 11},
		{"v2-whitespace-suppresses-fallback", `{"content":[{"text":" \r\n\t"}],"output":"fallback"}`, "", true, false, false, 12},
		{"v2-output-null", `{"output":null}`, "", true, false, false, 11},
		{"v2-output-object", `{"output":{}}`, "", true, false, false, 11},
		{"v2-output-array", `{"output":[]}`, "", true, false, false, 11},
		{"v2-output-number", `{"output":7}`, "", true, false, false, 11},
		{"v2-output-bool", `{"output":false}`, "", true, false, false, 11},
		{"v2-state-wrong-shape", `[]`, "", true, false, true, 9},
		{"v1-state-wrong-shape", `[]`, "", false, false, true, 4},
		{"v2-error-absent", `{}`, "", true, false, false, 11},
		{"v2-error-null", `{"error":null}`, "", true, false, false, 11},
		{"v2-error-false", `{"error":false}`, "", true, false, false, 11},
		{"v2-error-empty-string", `{"error":""}`, "", true, false, false, 11},
		{"v2-error-whitespace-string", `{"error":" \n "}`, "", true, false, false, 11},
		{"v2-error-empty-object", `{"error":{}}`, "", true, false, false, 11},
		{"v2-error-empty-array", `{"error":[]}`, "", true, false, false, 11},
		{"v2-error-spaced-object", `{"error":{ }}`, "", true, true, false, 11},
		{"v2-error-spaced-array", `{"error":[ ]}`, "", true, true, false, 11},
		{"v2-error-true", `{"error":true}`, "", true, true, false, 11},
		{"v2-error-object", `{"error":{"why":"failed"}}`, "", true, true, false, 11},
		{"v2-error-array", `{"error":[false]}`, "", true, true, false, 11},
		{"v2-error-string", `{"error":"failed"}`, "", true, true, false, 11},
		{"v2-error-zero", `{"error":0}`, "", true, true, false, 11},
		{"v2-error-number", `{"error":2}`, "", true, true, false, 11},
		{"v2-status-error", `{"status":"error","error":false,"metadata":{"exit":0}}`, "", true, true, false, 11},
		{"v2-status-case", `{"status":"Error","error":false}`, "", true, false, false, 11},
		{"v2-status-wrong-type", `{"status":false}`, "", true, false, true, 9},
	} {
		t.Run(tc.name, func(t *testing.T) {
			part := `{"type":"tool","callID":" c ","tool":" shell ","state":` + tc.state + `}`
			message := `{"role":"assistant"}`
			if tc.v2 {
				message = `{"content":[{"type":"tool","id":" c ","name":" shell ","state":` + tc.state + `}]}`
			}
			input := ""
			if tc.name == "v1-string" || tc.name == "v2-blocks-precede-string" {
				input = `{"path":"x"}`
			}
			want := []Entry{{ID: "m", Role: "assistant", Timestamp: "1970-01-01T00:00:01Z", Tools: []ToolActivity{{ID: "c", associationID: "c", Name: "shell", Input: input, Output: tc.output, Error: tc.failed}}}}
			if tc.corrupt {
				want = []Entry{}
			}
			openCodeSemanticRead(t, tc.v2, []openCodeRow{openCodeSemanticRow(tc.v2, message, part)}, want, tc.corrupt, tc.checks)
		})
	}
}

func TestInstalledOpenCodeExitMetadataValues(t *testing.T) {
	for _, version := range []string{"v1", "v2"} {
		t.Run(version, func(t *testing.T) {
			for _, tc := range []struct {
				name, metadata string
				failed         bool
			}{
				{"zero", `{"exit":0}`, false}, {"positive", `{"exit":2}`, true},
				{"negative", `{"exit":-1}`, true}, {"fractional", `{"exit":0.5}`, true},
				{"string", `{"exit":"2"}`, false}, {"bool", `{"exit":true}`, false},
				{"null-exit", `{"exit":null}`, false}, {"missing-exit", `{}`, false},
				{"null-metadata", `null`, false}, {"array-metadata", `[]`, false},
				{"string-metadata", `"wrong"`, false}, {"number-metadata", `2`, false},
			} {
				t.Run(tc.name, func(t *testing.T) {
					v2 := version == "v2"
					state := `{"status":"completed","output":"ok","metadata":` + tc.metadata + `}`
					message, checks := `{"role":"assistant"}`, 4
					part := `{"type":"tool","callID":"c","tool":"shell","state":` + state + `}`
					if v2 {
						message, checks = `{"content":[{"type":"tool","id":"c","name":"shell","state":`+state+`}]}`, 11
					}
					want := []Entry{{ID: "m", Role: "assistant", Timestamp: "1970-01-01T00:00:01Z", Tools: []ToolActivity{{ID: "c", associationID: "c", Name: "shell", Output: "ok", Error: tc.failed}}}}
					openCodeSemanticRead(t, v2, []openCodeRow{openCodeSemanticRow(v2, message, part)}, want, false, checks)
				})
			}
		})
	}
}

func TestInstalledOpenCodeParserCompositionValues(t *testing.T) {
	t.Run("v1-multipart-two-messages", func(t *testing.T) {
		rows := []openCodeRow{
			{SessionID: "ses_test", MessageID: "old", Created: 1000, MessageData: `{"role":"user"}`, PartID: "a", PartData: `{"type":"text","text":" first\r\u0000 "}`, Total: 2, CursorFound: 1},
			{SessionID: "ses_test", MessageID: "old", Created: 1000, MessageData: `{"role":"user"}`, PartID: "b", PartData: `{"type":"tool","callID":"a","tool":"read","state":{"input":{"path":"x"},"output":"A"}}`, Total: 2, CursorFound: 1},
			{SessionID: "ses_test", MessageID: "old", Created: 1000, MessageData: `{"role":"user"}`, PartID: "c", PartData: `{"type":"text","text":"second\n"}`, Total: 2, CursorFound: 1},
			{SessionID: "ses_test", MessageID: "old", Created: 1000, MessageData: `{"role":"user"}`, PartID: "d", PartData: `{"type":"tool","callID":"b","state":{"input":null,"output":"B","metadata":{"exit":-2}}}`, Total: 2, CursorFound: 1},
			{SessionID: "ses_test", MessageID: "new", Created: 2000, MessageData: `{"role":"assistant"}`, PartID: "e", PartData: `{"type":"tool","callID":"c","tool":"shell","state":{"input":[1],"output":"C","error":"fail","metadata":{"exit":0}}}`, Total: 2, CursorFound: 1},
			{SessionID: "ses_test", MessageID: "new", Created: 2000, MessageData: `{"role":"assistant"}`, PartID: "f", PartData: `{"type":"text","text":"last"}`, Total: 2, CursorFound: 1},
		}
		want := []Entry{
			{ID: "old", Role: "user", Timestamp: "1970-01-01T00:00:01Z", Text: "first\nsecond", Tools: []ToolActivity{{ID: "a", associationID: "a", Name: "read", Input: `{"path":"x"}`, Output: "A"}, {ID: "b", associationID: "b", Name: "Tool", Output: "B", Error: true}}},
			{ID: "new", Role: "assistant", Timestamp: "1970-01-01T00:00:02Z", Text: "last", Tools: []ToolActivity{{ID: "c", associationID: "c", Name: "shell", Input: `[1]`, Output: "C", Error: true}}},
		}
		openCodeSemanticRead(t, false, rows, want, false, 10)
	})
	t.Run("v2-user-assistant-multiple-tools", func(t *testing.T) {
		rows := []openCodeRow{
			{SessionID: "ses_test", MessageID: "old", Created: 1000, MessageType: "user", MessageData: `{"text":" user\r\u0000\u001b[31m text\u001b[0m ","content":[{"type":"text","text":"ignored"}]}`, Total: 2, CursorFound: 1, HasMessages: 1, V2: true},
			{SessionID: "ses_test", MessageID: "new", Created: 2000, MessageType: "assistant", MessageData: `{"text":"ignored","content":[{"type":"text","text":"first\n"},{"type":"tool","id":"a","name":"read","state":{"input":{"path":"x"},"content":[{"text":"A"},{"text":"B"}]}},{"type":"image","text":"ignored"},{"type":"text","text":" \n "},{"type":"tool","id":"b","state":{"output":"C","error":true,"metadata":{"exit":0}}},{"type":"text","text":"second"}]}`, Total: 2, CursorFound: 1, HasMessages: 1, V2: true},
		}
		want := []Entry{{ID: "old", Role: "user", Timestamp: "1970-01-01T00:00:01Z", Text: "user text"}, {ID: "new", Role: "assistant", Timestamp: "1970-01-01T00:00:02Z", Text: "first\nsecond", Tools: []ToolActivity{{ID: "a", associationID: "a", Name: "read", Input: `{"path":"x"}`, Output: "A\nB"}, {ID: "b", associationID: "b", Name: "Tool", Output: "C", Error: true}}}}
		// outer+V2 initial/deferred=4; user=1; assistant row/content=4;
		// six items, two output initial/deferred pairs, two output blocks.
		openCodeSemanticRead(t, true, rows, want, false, 21)
	})
	for _, tc := range []struct {
		name, message, part, role, id string
		v2, damaged                   bool
		checks                        int
	}{
		{"v1-malformed-message", `{`, `{}`, "assistant", "m", false, true, 3},
		{"v1-message-wrong-role-type", `{"role":2}`, `{}`, "assistant", "m", false, true, 3},
		{"v1-invalid-role", `{"role":"system"}`, `{}`, "assistant", "m", false, true, 3},
		{"v1-malformed-part", `{"role":"assistant"}`, `{`, "assistant", "m", false, true, 4},
		{"v1-wrong-part-shape", `{"role":"assistant"}`, `[]`, "assistant", "m", false, true, 4},
		{"v1-wrong-part-type", `{"role":"assistant"}`, `{"type":1}`, "assistant", "m", false, true, 4},
		{"v1-ignored-part", `{"role":"assistant"}`, `{"type":"image","text":"ignored"}`, "assistant", "m", false, false, 4},
		{"v1-whitespace-text", `{"role":"assistant"}`, `{"type":"text","text":" \r\n\u0000 "}`, "assistant", "m", false, false, 4},
		{"v1-invalid-id", `{"role":"assistant"}`, `{}`, "assistant", "bad\n", false, true, 3},
		{"v1-empty-id", `{"role":"assistant"}`, `{`, "assistant", "", false, false, 3},
		{"v2-malformed-message", `{`, "", "assistant", "m", true, true, 5},
		{"v2-content-wrong-shape", `{"content":{}}`, "", "assistant", "m", true, true, 5},
		{"v2-item-wrong-shape", `{"content":[2]}`, "", "assistant", "m", true, true, 9},
		{"v2-item-wrong-type", `{"content":[{"type":2}]}`, "", "assistant", "m", true, true, 9},
		{"v2-invalid-type", `{"text":"ignored"}`, "", "system", "m", true, true, 5},
		{"v2-invalid-id", `{"text":"ignored"}`, "", "user", "bad\u0000", true, true, 5},
		{"v1-long-id", `{"role":"assistant"}`, `{}`, "assistant", strings.Repeat("i", 257), false, true, 3},
		{"v2-long-id", `{"text":"ignored"}`, "", "user", strings.Repeat("i", 257), true, true, 5},
		{"v2-empty-id", `{`, "", "assistant", "", true, false, 5},
		{"v2-empty-content", `{"content":[]}`, "", "assistant", "m", true, false, 8},
		{"v2-null-ignored-whitespace", `{"content":[null,{}, {"type":"image"},{"type":"text","text":" \n "}]}`, "", "assistant", "m", true, false, 12},
		{"v2-empty-user", `{"text":" \r\u0000 "}`, "", "user", "m", true, false, 5},
	} {
		t.Run(tc.name, func(t *testing.T) {
			row := openCodeSemanticRow(tc.v2, tc.message, tc.part)
			row.MessageID, row.MessageType = tc.id, tc.role
			openCodeSemanticRead(t, tc.v2, []openCodeRow{row}, []Entry{}, tc.damaged, tc.checks)
		})
	}
	t.Run("v1-absent-part-id", func(t *testing.T) {
		row := openCodeSemanticRow(false, `{"role":"assistant"}`, `{`)
		row.PartID = ""
		openCodeSemanticRead(t, false, []openCodeRow{row}, []Entry{}, false, 4)
	})
	t.Run("v1-valid-and-damaged", func(t *testing.T) {
		first := openCodeSemanticRow(false, `{"role":"assistant"}`, `{"type":"tool","callID":"c","tool":"read","state":{"output":"kept"}}`)
		second := openCodeSemanticRow(false, `{"role":"assistant"}`, `{`)
		second.PartID = "q"
		want := []Entry{{ID: "m", Role: "assistant", Timestamp: "1970-01-01T00:00:01Z", Tools: []ToolActivity{{ID: "c", associationID: "c", Name: "read", Output: "kept"}}}}
		openCodeSemanticRead(t, false, []openCodeRow{first, second}, want, true, 5)
	})
	t.Run("v2-valid-and-damaged", func(t *testing.T) {
		row := openCodeSemanticRow(true, `{"content":[{"type":"text","text":"kept"},false,{"type":"tool","id":"c","name":"read","state":{"output":"ok"}}, {"type":"tool","id":2}]}`, "")
		want := []Entry{{ID: "m", Role: "assistant", Timestamp: "1970-01-01T00:00:01Z", Text: "kept", Tools: []ToolActivity{{ID: "c", associationID: "c", Name: "read", Output: "ok"}}}}
		openCodeSemanticRead(t, true, []openCodeRow{row}, want, true, 14)
	})
}

// Quote INPUT strings only; it must never create an expected semantic value.
func openCodeSemanticQuote(t *testing.T, input string) string {
	t.Helper()
	raw, err := json.Marshal(input)
	if err != nil {
		t.Fatal("string serialization failed")
	}
	return string(raw)
}

func TestInstalledOpenCodeSemanticBounds(t *testing.T) {
	t.Run("v1-tool-only-over", func(t *testing.T) {
		output := openCodeSemanticQuote(t, strings.Repeat("x", 131073))
		row := openCodeSemanticRow(false, `{"role":"assistant"}`, `{"type":"tool","callID":"c","tool":"shell","state":{"output":`+output+`}}`)
		want := []Entry{{ID: "m", Role: "assistant", Timestamp: "1970-01-01T00:00:01Z", Truncated: false, Tools: []ToolActivity{{ID: "c", associationID: "c", Name: "shell", Output: strings.Repeat("x", 131072), Truncated: true}}}}
		openCodeSemanticRead(t, false, []openCodeRow{row}, want, false, 4)
	})
	t.Run("v2-text-only-over", func(t *testing.T) {
		text := openCodeSemanticQuote(t, strings.Repeat("x", 131073))
		row := openCodeSemanticRow(true, `{"content":[{"type":"text","text":`+text+`},{"type":"tool","id":"c","name":"shell","state":{"output":"ok"}}]}`, "")
		want := []Entry{{ID: "m", Role: "assistant", Timestamp: "1970-01-01T00:00:01Z", Text: strings.Repeat("x", 131072), Truncated: true, Tools: []ToolActivity{{ID: "c", associationID: "c", Name: "shell", Output: "ok", Truncated: false}}}}
		openCodeSemanticRead(t, true, []openCodeRow{row}, want, false, 12)
	})
	for _, tc := range []struct {
		name                string
		v2                  bool
		delta               int
		utf8, trim, damaged bool
	}{
		{"v1-exact", false, 0, false, false, false},
		{"v1-over", false, 1, false, false, false},
		{"v2-exact", true, 0, false, false, false},
		{"v2-over", true, 1, false, false, false},
		{"v1-utf8-cut-corrupt", false, 1, true, false, true},
		{"v2-utf8-cut-corrupt", true, 1, true, false, true},
		{"v1-trim-at-cut", false, 1, false, true, false},
		{"v2-trim-at-cut", true, 1, false, true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// Literal independent 128KiB limit; allocate only this current case.
			input, expected := strings.Repeat("x", 131072+tc.delta), strings.Repeat("x", 131072)
			if tc.utf8 {
				input, expected = strings.Repeat("x", 131071)+"é", strings.Repeat("x", 131071)
			}
			if tc.trim {
				input, expected = strings.Repeat("x", 131071)+" y", strings.Repeat("x", 131071)
			}
			quoted := openCodeSemanticQuote(t, input)
			part := `{"type":"tool","callID":"c","tool":"shell","state":{"output":` + quoted + `}}`
			text := `{"type":"text","text":` + quoted + `}`
			rows := []openCodeRow{openCodeSemanticRow(false, `{"role":"assistant"}`, text), openCodeSemanticRow(false, `{"role":"assistant"}`, part)}
			rows[1].PartID = "q"
			checks := 5
			if tc.damaged {
				row := openCodeSemanticRow(false, `{"role":"assistant"}`, `{`)
				row.PartID = "r"
				rows = append(rows, row)
				checks = 6
			}
			if tc.v2 {
				bad := ""
				checks = 12
				if tc.damaged {
					bad = ",false"
					checks = 13
				}
				rows = []openCodeRow{openCodeSemanticRow(true, `{"content":[`+text+`,{"type":"tool","id":"c","name":"shell","state":{"output":`+quoted+`}}`+bad+`]}`, "")}
			}
			want := []Entry{{ID: "m", Role: "assistant", Timestamp: "1970-01-01T00:00:01Z", Text: expected, Truncated: tc.delta > 0, Tools: []ToolActivity{{ID: "c", associationID: "c", Name: "shell", Output: expected, Truncated: tc.delta > 0}}}}
			openCodeSemanticRead(t, tc.v2, rows, want, tc.damaged, checks)
		})
	}
	for _, version := range []string{"v1", "v2"} {
		t.Run(version+"-input-id-name-assignment", func(t *testing.T) {
			id, name := strings.Repeat("i", 257), strings.Repeat("n", 161)
			// JSON string input remains quoted raw JSON, not decoded input text.
			input := openCodeSemanticQuote(t, " "+strings.Repeat("x", 65537)+" ")
			state := `{"input":` + input + `,"output":"ok"}`
			part := `{"type":"tool","callID":` + openCodeSemanticQuote(t, " "+id+" ") + `,"tool":` + openCodeSemanticQuote(t, " "+name+" ") + `,"state":` + state + `}`
			v2, message, checks := version == "v2", `{"role":"assistant"}`, 4
			if v2 {
				message, checks = `{"content":[{"type":"tool","id":`+openCodeSemanticQuote(t, " "+id+" ")+`,"name":`+openCodeSemanticQuote(t, " "+name+" ")+`,"state":`+state+`}]}`, 11
			}
			// Compatibility: output assignment replaces the earlier input/ID/name
			// truncation flag. Do NOT assert an invented aggregate flag here.
			want := []Entry{{ID: "m", Role: "assistant", Timestamp: "1970-01-01T00:00:01Z", Tools: []ToolActivity{{ID: strings.Repeat("i", 256), associationID: id, Name: strings.Repeat("n", 160), Input: `" ` + strings.Repeat("x", 65534), Output: "ok", Truncated: false}}}}
			openCodeSemanticRead(t, v2, []openCodeRow{openCodeSemanticRow(v2, message, part)}, want, false, checks)
		})
	}
}

func TestInstalledOpenCodeIsolatedPublicationStamps(t *testing.T) {
	for _, target := range []string{"positive-row", "empty-row", "layout"} {
		t.Run(target, func(t *testing.T) {
			for _, tc := range []struct {
				name    string
				db, wal openCodeFixtureInfo
			}{
				{"databaseSize", openCodeFixtureInfo{101, 1}, openCodeFixtureInfo{20, 2}},
				{"databaseTime", openCodeFixtureInfo{100, 3}, openCodeFixtureInfo{20, 2}},
				{"walSize", openCodeFixtureInfo{100, 1}, openCodeFixtureInfo{21, 2}},
				{"walTime", openCodeFixtureInfo{100, 1}, openCodeFixtureInfo{20, 4}},
			} {
				t.Run(tc.name, func(t *testing.T) {
					current := openCodeFileStamp{databaseSize: 100, databaseTime: 1, walSize: 20, walTime: 2}
					stable := openCodeMetadataStamp(openCodeFixtureDB, openCodeFixtureInfo{100, 1}, nil, openCodeFixtureInfo{20, 2}, nil)
					changed := openCodeMetadataStamp(openCodeFixtureDB, tc.db, nil, tc.wal, nil)
					var steps []openCodeMetadataStep
					if target != "layout" {
						steps = append(steps, stable...)
						steps = append(steps, openCodeMetadataStages("cache-hit")...)
					}
					steps = append(steps, openCodeMetadataStages("layout-hit")...)
					steps = append(steps, openCodeMetadataStep{kind: "ready", ready: true}, openCodeMetadataStep{kind: "query", path: openCodeFixtureDB, sql: openCodeFixtureLayout, raw: `[{"name":"session"},{"name":"message"},{"name":"part"}]`})
					steps = append(steps, openCodeMetadataStages("layout-row", "layout-row", "layout-row")...)
					if target == "layout" {
						steps = append(steps, changed...)
					} else {
						steps = append(steps, stable...)
						steps = append(steps, openCodeMetadataStages("layout-publish")...)
						raw := openCodeMetadataRows
						if target == "empty-row" {
							raw = "[]"
						}
						steps = append(steps, openCodeMetadataStep{kind: "ready", ready: true}, openCodeMetadataStep{kind: "query", path: openCodeFixtureDB, sql: openCodeSQLOracleV1, raw: raw})
						steps = append(steps, openCodeMetadataStages("decode")...)
						if target == "positive-row" {
							steps = append(steps, openCodeMetadataStages("reverse", "version", "version", "distinct", "distinct", "cache-copy", "cache-copy")...)
						}
						// Third DB/WAL pair ONLY. The earlier layout check is stable.
						steps = append(steps, changed...)
					}
					f := newOpenCodeMetadataFixture(t, steps)
					openCodeTerminalBudgetGuard(f)
					f.reader.cache, f.reader.layouts = openCodePublicationSeeds(2, 2)
					wantCache, wantLayouts := openCodePublicationSeeds(2, 2)
					if target != "layout" {
						wantLayouts[openCodeFixtureDB] = openCodeLayoutEntry{stamp: current, layout: openCodeLayout{v1: true}}
					}
					var rows []openCodeRow
					var layout openCodeLayout
					var more bool
					var code string
					err := f.owner.Do(context.Background(), func(c *conversationlife.Call) error {
						var e error
						if target == "layout" {
							layout, code, e = f.reader.layoutIn(c, openCodeFixtureDB, current, true)
						} else {
							rows, more, code, e = f.reader.queryIn(c, openCodeFixtureDB, "ses_test", "", 2)
						}
						return e
					})
					f.check(t)
					if err != nil || code != "" || more {
						t.Fatalf("stamp result err=%v code=%q more=%v", err, code, more)
					}
					if target == "layout" {
						if rows != nil || layout != (openCodeLayout{v1: true}) {
							t.Fatal("fresh layout mismatch")
						}
					} else {
						wantRows := openCodeMetadataExpectedRows()
						if target == "empty-row" {
							wantRows = []openCodeRow{}
						}
						if layout != (openCodeLayout{}) || !reflect.DeepEqual(rows, wantRows) {
							t.Fatal("fresh complete rows mismatch")
						}
					}
					if !reflect.DeepEqual(f.reader.cache, wantCache) || !reflect.DeepEqual(f.reader.layouts, wantLayouts) {
						t.Fatal("isolated final stamp change overwrote independently seeded maps")
					}
				})
			}
		})
	}
}

// Hermes scripts are closed ordered allowlists, not an executor implementation.
// Nil observation is the production mode: zero optional observations are planned.
type hermesAdmissionStep struct {
	kind, path, sql, resolved, raw string
	paths                          []string
	info                           os.FileInfo
	ready                          bool
	err                            error
}

type hermesAdmissionFixture struct {
	owner                             *conversationlife.Owner
	reader                            *hermesReader
	steps                             []hermesAdmissionStep
	next, seen, unknown, observations int
}

func (f *hermesAdmissionFixture) deny() {
	f.unknown++
	f.owner.Begin()
}

func (f *hermesAdmissionFixture) take(kind, path, sql string) (hermesAdmissionStep, bool) {
	f.seen++
	if f.next >= len(f.steps) || f.steps[f.next].kind != kind || f.steps[f.next].path != path || f.steps[f.next].sql != sql {
		f.deny()
		return hermesAdmissionStep{}, false
	}
	s := f.steps[f.next]
	f.next++
	return s, true
}

func (f *hermesAdmissionFixture) live(c *conversationlife.Call) bool {
	// Actual admission, not Context or Own, proves this is a live same-owner Call.
	if err := f.owner.DoIn(c, func(*conversationlife.Call) error { return nil }); err != nil {
		f.deny()
		return false
	}
	if c.Context() == nil || c.Context().Err() != nil {
		f.deny()
		return false
	}
	return true
}

func newHermesAdmissionFixture(t *testing.T, steps []hermesAdmissionStep) *hermesAdmissionFixture {
	t.Helper()
	f := &hermesAdmissionFixture{owner: conversationlife.New(), steps: steps}
	t.Cleanup(f.owner.Join)
	binding := &installedProviderSQL{owner: f.owner, binary: "sqlite3"}
	binding.deps = installedProviderSQLDeps{
		ready: func(c *conversationlife.Call) bool {
			s, ok := f.take("ready", "", "")
			return ok && f.live(c) && s.ready
		},
		query: func(c *conversationlife.Call, db, sql string, max int) ([]byte, error) {
			s, ok := f.take("query", db, sql)
			if !ok || !f.live(c) {
				return nil, conversationlife.ErrRefused
			}
			deadline, timed := c.Context().Deadline()
			now := time.Now()
			if max != 8*1024*1024 || !timed || !deadline.After(now) || deadline.After(now.Add(10*time.Second)) {
				f.deny()
				return nil, conversationlife.ErrRefused
			}
			return []byte(s.raw), s.err
		},
	}
	metadata := installedProviderMetadata{
		roots: func(ctx context.Context, home string) ([]string, error) {
			s, ok := f.take("roots", home, "")
			if !ok {
				return nil, conversationlife.ErrRefused
			}
			if ctx == nil || ctx.Err() != nil {
				f.deny()
				return nil, conversationlife.ErrRefused
			}
			return append([]string(nil), s.paths...), s.err
		},
		databases: func(ctx context.Context, home string) ([]string, error) {
			s, ok := f.take("databases", home, "")
			if !ok {
				return nil, conversationlife.ErrRefused
			}
			if ctx == nil || ctx.Err() != nil {
				f.deny()
				return nil, conversationlife.ErrRefused
			}
			return append([]string(nil), s.paths...), s.err
		},
		eval: func(path string) (string, error) {
			s, ok := f.take("eval", path, "")
			if !ok {
				return "", conversationlife.ErrRefused
			}
			return s.resolved, s.err
		},
		stat: func(path string) (os.FileInfo, error) {
			s, ok := f.take("stat", path, "")
			if !ok {
				return nil, conversationlife.ErrRefused
			}
			return s.info, s.err
		},
	}
	err := f.owner.Do(context.Background(), func(c *conversationlife.Call) error {
		var err error
		f.reader, err = constructInstalledHermesIn(c, "/planned/home", binding, metadata)
		return err
	})
	if err != nil || f.reader == nil || f.seen != 0 || f.unknown != 0 {
		t.Fatal("no-effect Hermes fixture construction failed", err)
	}
	return f
}

func (f *hermesAdmissionFixture) check(t *testing.T) {
	t.Helper()
	f.owner.Join()
	if f.next != len(f.steps) || f.seen != len(f.steps) || f.unknown != 0 || f.observations != 0 || f.owner.Witnesses() != nil {
		t.Fatalf("Hermes effects consumed=%d/%d seen=%d unknown=%d observations=%d witnesses=%v", f.next, len(f.steps), f.seen, f.unknown, f.observations, f.owner.Witnesses())
	}
}

func hermesIndependentInput() []Entry {
	return []Entry{{ID: "keep", Timestamp: "1970-01-01T00:00:01Z", Role: "assistant", Text: "original", Truncated: true,
		Tools: []ToolActivity{{ID: "display", associationID: "call_b", Name: "shell", Input: "pwd", Output: "old", Error: true, Truncated: true}}}}
}

func TestInstalledHermesConstructionBoundaries(t *testing.T) {
	for _, name := range []string{"success", "nil-binding", "nil-owner", "zero-owner", "nil-parent", "zero-parent", "foreign-parent", "stale-parent", "fenced-owner", "live-fenced", "ready", "query", "roots", "databases", "eval", "stat", "binary-empty", "binary-other", "concrete-nil", "concrete-zero", "concrete-nil-owner", "concrete-zero-owner"} {
		t.Run(name, func(t *testing.T) {
			f := newHermesAdmissionFixture(t, nil)
			foreign := conversationlife.New()
			t.Cleanup(foreign.Join)
			b, m := f.reader.installed, f.reader.metadata
			var result *hermesReader
			var got error
			run := func(c *conversationlife.Call) {
				switch name {
				case "concrete-nil":
					result, got = newInstalledHermesReaderIn(f.owner, c, "/planned/home", nil)
				case "concrete-zero":
					result, got = newInstalledHermesReaderIn(f.owner, c, "/planned/home", &sqliteexec.InstalledExecutor{})
				case "concrete-nil-owner":
					result, got = newInstalledHermesReaderIn(nil, c, "/planned/home", &sqliteexec.InstalledExecutor{})
				case "concrete-zero-owner":
					result, got = newInstalledHermesReaderIn(&conversationlife.Owner{}, c, "/planned/home", &sqliteexec.InstalledExecutor{})
				default:
					result, got = constructInstalledHermesIn(c, "/planned/home", b, m)
				}
			}
			switch name {
			case "nil-binding":
				b = nil
			case "nil-owner":
				b.owner = nil
			case "zero-owner":
				b.owner = &conversationlife.Owner{}
			case "ready":
				b.deps.ready = nil
			case "query":
				b.deps.query = nil
			case "roots":
				m.roots = nil
			case "databases":
				m.databases = nil
			case "eval":
				m.eval = nil
			case "stat":
				m.stat = nil
			case "binary-empty":
				b.binary = ""
			case "binary-other":
				b.binary = "other"
			}
			switch name {
			case "nil-parent":
				run(nil)
			case "zero-parent":
				run(&conversationlife.Call{})
			case "foreign-parent":
				_ = foreign.Do(context.Background(), func(c *conversationlife.Call) error { run(c); return nil })
			case "stale-parent", "fenced-owner":
				var stale *conversationlife.Call
				_ = f.owner.Do(context.Background(), func(c *conversationlife.Call) error { stale = c; return nil })
				if name == "fenced-owner" {
					f.owner.Begin()
				}
				run(stale)
			default:
				_ = f.owner.Do(context.Background(), func(c *conversationlife.Call) error {
					if name == "live-fenced" {
						f.owner.Begin()
					}
					run(c)
					return nil
				})
			}
			foreign.Join()
			f.check(t)
			if name == "success" {
				if got != nil || result == nil || result.home != "/planned/home" || result.binary != "sqlite3" || result.installed != b || result.executor != nil || result.observe != nil || !result.metadata.complete() {
					t.Fatal("direct initializer state", got)
				}
			} else {
				want := conversationlife.ErrRefused
				if name == "live-fenced" {
					want = context.Canceled
				}
				if result != nil || !errors.Is(got, want) {
					t.Fatalf("published=%v error=%v", result != nil, got)
				}
			}
		})
	}
}

// Seven adapters, one invalid/no-work operand each, no mutation cross-product.
func TestInstalledHermesScopedAdmission(t *testing.T) {
	for _, method := range []string{"databases", "sameDirectory", "locate", "query", "queryToolRows", "attachToolResults", "readFrom"} {
		t.Run(method, func(t *testing.T) {
			for _, authority := range []string{"nil", "zero", "foreign", "stale", "ordinary", "nil-receiver", "live-fenced"} {
				t.Run(authority, func(t *testing.T) {
					f := newHermesAdmissionFixture(t, nil)
					foreign := conversationlife.New()
					t.Cleanup(foreign.Join)
					r := f.reader
					if authority == "ordinary" {
						r = &hermesReader{}
					}
					if authority == "nil-receiver" {
						r = nil
					}
					if r != nil {
						r.observe = func(context.Context, string) { f.observations++; f.deny() }
					}
					input, wantInput := hermesIndependentInput(), hermesIndependentInput()
					shapeOK := false
					var got error
					run := func(c *conversationlife.Call) {
						switch method {
						case "databases":
							v, code, e := r.databasesIn(c)
							got = e
							shapeOK = v == nil && code == "source_unavailable"
						case "sameDirectory":
							v, e := r.sameDirectoryIn(c, "", "")
							got = e
							shapeOK = !v
						case "locate":
							v, code, e := r.locateIn(c, "", "bad/session")
							got = e
							shapeOK = v == (Location{}) && code == "query_failed"
						case "query":
							v, more, code, e := r.queryIn(c, "", "bad/session", "-1", 2)
							got = e
							shapeOK = v == nil && !more && code == "query_failed"
						case "queryToolRows":
							v, code, e := r.queryToolRowsIn(c, "", "", nil)
							got = e
							shapeOK = v == nil && code == "query_failed"
						case "attachToolResults":
							v, corrupt, e := r.attachToolResultsIn(c, "", "", input)
							got = e
							shapeOK = v == nil && !corrupt
						case "readFrom":
							v, more, corrupt, meta, code, e := r.readFromIn(c, "", "bad/session", "-1", 2)
							got = e
							shapeOK = v == nil && !more && !corrupt && meta == (hermesRow{}) && code == "query_failed"
						}
					}
					switch authority {
					case "nil":
						run(nil)
					case "zero":
						run(&conversationlife.Call{})
					case "foreign":
						_ = foreign.Do(context.Background(), func(c *conversationlife.Call) error { run(c); return nil })
					case "stale":
						var stale *conversationlife.Call
						_ = f.owner.Do(context.Background(), func(c *conversationlife.Call) error { stale = c; return nil })
						run(stale)
					default:
						_ = f.owner.Do(context.Background(), func(c *conversationlife.Call) error {
							if authority == "live-fenced" {
								f.owner.Begin()
							}
							run(c)
							return nil
						})
					}
					foreign.Join()
					f.check(t)
					want := conversationlife.ErrRefused
					if authority == "live-fenced" {
						want = context.Canceled
					}
					if !shapeOK || !errors.Is(got, want) || (authority == "live-fenced" && errors.Is(got, conversationlife.ErrRefused)) || !reflect.DeepEqual(input, wantInput) {
						t.Fatalf("authority error=%v input=%+v", got, input)
					}
				})
			}
		})
	}
}

func TestInstalledHermesDependencyRefusal(t *testing.T) {
	for _, member := range []string{"binding", "owner", "zero-owner", "ready", "query", "roots", "databases", "eval", "stat", "binary-empty", "binary-other", "executor-zero"} {
		t.Run(member, func(t *testing.T) {
			f := newHermesAdmissionFixture(t, nil)
			r := f.reader
			r.observe = func(context.Context, string) { f.observations++; f.deny() }
			switch member {
			case "binding":
				r.installed = nil
			case "owner":
				r.installed.owner = nil
			case "zero-owner":
				r.installed.owner = &conversationlife.Owner{}
			case "ready":
				r.installed.deps.ready = nil
			case "query":
				r.installed.deps.query = nil
			case "roots":
				r.metadata.roots = nil
			case "databases":
				r.metadata.databases = nil
			case "eval":
				r.metadata.eval = nil
			case "stat":
				r.metadata.stat = nil
			case "binary-empty":
				r.binary = ""
			case "binary-other":
				r.binary = "other"
			case "executor-zero":
				r.installed.executor = &sqliteexec.InstalledExecutor{}
			}
			input, want := hermesIndependentInput(), hermesIndependentInput()
			var candidate []Entry
			var corrupt bool
			err := f.owner.Do(context.Background(), func(c *conversationlife.Call) error {
				var e error
				candidate, corrupt, e = r.attachToolResultsIn(c, hermesFlowDB, "ses_test", input)
				return e
			})
			f.check(t)
			if !errors.Is(err, conversationlife.ErrRefused) || candidate != nil || corrupt || !reflect.DeepEqual(input, want) {
				t.Fatal("dependency refusal changed input or result", err)
			}
		})
	}
}

func TestInstalledHermesLegacyRefusal(t *testing.T) {
	for _, mode := range []string{"fenced", "binary-empty", "binary-other", "nil-context"} {
		t.Run(mode, func(t *testing.T) {
			for _, method := range []string{"databases", "locate", "locateContext", "readFrom", "readFromContext", "query", "queryContext", "queryToolRows", "queryToolRowsContext", "attachToolResults", "attachToolResultsContext", "queryExec"} {
				if mode == "nil-context" && !strings.HasSuffix(method, "Context") {
					continue
				}
				t.Run(method, func(t *testing.T) {
					f := newHermesAdmissionFixture(t, nil)
					r := f.reader
					r.observe = func(context.Context, string) { f.observations++; f.deny() }
					ctx := context.Background()
					switch mode {
					case "fenced":
						f.owner.Begin()
					case "binary-empty":
						r.binary = ""
					case "binary-other":
						r.binary = "other"
					case "nil-context":
						ctx = nil
					}
					input, want := hermesIndependentInput(), hermesIndependentInput()
					var entries []Entry
					var rows []hermesRow
					var tools []hermesToolRow
					var paths []string
					var location Location
					var meta hermesRow
					var more, corrupt bool
					code, wantCode := "", "query_failed"
					switch method {
					case "databases":
						paths, code = r.databases()
						wantCode = "source_unavailable"
					case "locate":
						location = r.locate("", "bad/session")
						wantCode = ""
					case "locateContext":
						location, code = r.locateContext(ctx, "", "bad/session")
					case "readFrom":
						entries, more, corrupt, meta, code = r.readFrom("", "bad/session", "-1", 2)
					case "readFromContext":
						entries, more, corrupt, meta, code = r.readFromContext(ctx, "", "bad/session", "-1", 2)
					case "query":
						rows, more, code = r.query("", "", "-1", 2)
					case "queryContext":
						rows, more, code = r.queryContext(ctx, "", "", "-1", 2)
					case "queryToolRows":
						tools, code = r.queryToolRows("", "", nil)
					case "queryToolRowsContext":
						tools, code = r.queryToolRowsContext(ctx, "", "", nil)
					case "attachToolResults":
						corrupt = r.attachToolResults("", "", input)
						wantCode = ""
					case "attachToolResultsContext":
						corrupt = r.attachToolResultsContext(ctx, "", "", input)
						wantCode = ""
					case "queryExec":
						if r.queryExec() != nil {
							t.Error("ordinary resolver escape")
						}
						wantCode = ""
					}
					f.check(t)
					wantCorrupt := strings.HasPrefix(method, "attachToolResults")
					if entries != nil || rows != nil || tools != nil || paths != nil || location != (Location{}) || meta != (hermesRow{}) || more || corrupt != wantCorrupt || code != wantCode || !reflect.DeepEqual(input, want) {
						t.Fatalf("legacy refusal shape method=%s code=%q corrupt=%v", method, code, corrupt)
					}
				})
			}
		})
	}
}

const hermesFlowDB = "/planned/hermes/state.db"
const hermesFlowDB2 = "/planned/second/state.db"

// Complete independently written literal callback keys. No production builder
// or parser creates these expectations. Logical replies do not execute SQLite.
const hermesLocateSQLOracle = `WITH raw AS (SELECT m.id,m.role,m.active,m.compacted,COALESCE(m.content,'') AS content,COALESCE(m.tool_call_id,'') AS tool_call_id,COALESCE(m.tool_calls,'') AS tool_calls,COALESCE(m.tool_name,'') AS tool_name,COALESCE(m.display_kind,'') AS display_kind,COALESCE(m.timestamp,0) AS logical_timestamp FROM messages AS m WHERE m.session_id=CAST(X'7365735f74657374' AS TEXT) AND (m.active=1 OR m.compacted=1) AND COALESCE(m.display_kind,'') <> 'hidden'), visible AS (SELECT raw.*,MIN(id) OVER (PARTITION BY role,content,tool_call_id,tool_calls,tool_name,logical_timestamp) AS logical_id,ROW_NUMBER() OVER (PARTITION BY role,content,tool_call_id,tool_calls,tool_name,logical_timestamp ORDER BY active DESC,id DESC) AS generation FROM raw), displayed AS (SELECT id,logical_id,role,content,tool_call_id,tool_calls,tool_name,display_kind,logical_timestamp FROM visible WHERE generation=1 AND role IN ('user','assistant')), cursor AS (SELECT NULL AS logical_id WHERE 0), selected AS (SELECT id,logical_id,role,content,tool_call_id,tool_calls,tool_name,display_kind,logical_timestamp FROM displayed AS d WHERE 1=1 ORDER BY logical_id DESC LIMIT 2) SELECT s.id AS session_id,COALESCE(s.cwd,'') AS cwd,COALESCE(s.title,'') AS title,COALESCE(sm.logical_id,0) AS message_id,COALESCE(sm.role,'') AS role,hex(COALESCE(sm.content,'')) AS content_hex,COALESCE(sm.tool_call_id,'') AS tool_call_id,COALESCE(sm.tool_calls,'') AS tool_calls,COALESCE(sm.tool_name,'') AS tool_name,COALESCE(sm.display_kind,'') AS display_kind,COALESCE(sm.logical_timestamp,0) AS timestamp,(SELECT COUNT(*) FROM displayed) AS message_total,1 AS cursor_found FROM sessions AS s LEFT JOIN selected AS sm ON 1=1 WHERE s.id=CAST(X'7365735f74657374' AS TEXT) ORDER BY sm.logical_id DESC;`

const hermesReadSQLOracle = `WITH raw AS (SELECT m.id,m.role,m.active,m.compacted,COALESCE(m.content,'') AS content,COALESCE(m.tool_call_id,'') AS tool_call_id,COALESCE(m.tool_calls,'') AS tool_calls,COALESCE(m.tool_name,'') AS tool_name,COALESCE(m.display_kind,'') AS display_kind,COALESCE(m.timestamp,0) AS logical_timestamp FROM messages AS m WHERE m.session_id=CAST(X'7365735f74657374' AS TEXT) AND (m.active=1 OR m.compacted=1) AND COALESCE(m.display_kind,'') <> 'hidden'), visible AS (SELECT raw.*,MIN(id) OVER (PARTITION BY role,content,tool_call_id,tool_calls,tool_name,logical_timestamp) AS logical_id,ROW_NUMBER() OVER (PARTITION BY role,content,tool_call_id,tool_calls,tool_name,logical_timestamp ORDER BY active DESC,id DESC) AS generation FROM raw), displayed AS (SELECT id,logical_id,role,content,tool_call_id,tool_calls,tool_name,display_kind,logical_timestamp FROM visible WHERE generation=1 AND role IN ('user','assistant')), cursor AS (SELECT NULL AS logical_id WHERE 0), selected AS (SELECT id,logical_id,role,content,tool_call_id,tool_calls,tool_name,display_kind,logical_timestamp FROM displayed AS d WHERE 1=1 ORDER BY logical_id DESC LIMIT 3) SELECT s.id AS session_id,COALESCE(s.cwd,'') AS cwd,COALESCE(s.title,'') AS title,COALESCE(sm.logical_id,0) AS message_id,COALESCE(sm.role,'') AS role,hex(COALESCE(sm.content,'')) AS content_hex,COALESCE(sm.tool_call_id,'') AS tool_call_id,COALESCE(sm.tool_calls,'') AS tool_calls,COALESCE(sm.tool_name,'') AS tool_name,COALESCE(sm.display_kind,'') AS display_kind,COALESCE(sm.logical_timestamp,0) AS timestamp,(SELECT COUNT(*) FROM displayed) AS message_total,1 AS cursor_found FROM sessions AS s LEFT JOIN selected AS sm ON 1=1 WHERE s.id=CAST(X'7365735f74657374' AS TEXT) ORDER BY sm.logical_id DESC;`

const hermesCursorSQLOracle = `WITH raw AS (SELECT m.id,m.role,m.active,m.compacted,COALESCE(m.content,'') AS content,COALESCE(m.tool_call_id,'') AS tool_call_id,COALESCE(m.tool_calls,'') AS tool_calls,COALESCE(m.tool_name,'') AS tool_name,COALESCE(m.display_kind,'') AS display_kind,COALESCE(m.timestamp,0) AS logical_timestamp FROM messages AS m WHERE m.session_id=CAST(X'7365735f74657374' AS TEXT) AND (m.active=1 OR m.compacted=1) AND COALESCE(m.display_kind,'') <> 'hidden'), visible AS (SELECT raw.*,MIN(id) OVER (PARTITION BY role,content,tool_call_id,tool_calls,tool_name,logical_timestamp) AS logical_id,ROW_NUMBER() OVER (PARTITION BY role,content,tool_call_id,tool_calls,tool_name,logical_timestamp ORDER BY active DESC,id DESC) AS generation FROM raw), displayed AS (SELECT id,logical_id,role,content,tool_call_id,tool_calls,tool_name,display_kind,logical_timestamp FROM visible WHERE generation=1 AND role IN ('user','assistant')), cursor AS (SELECT logical_id FROM visible WHERE id=9 AND role IN ('user','assistant') LIMIT 1), selected AS (SELECT id,logical_id,role,content,tool_call_id,tool_calls,tool_name,display_kind,logical_timestamp FROM displayed AS d WHERE 1=1 AND EXISTS(SELECT 1 FROM cursor) AND d.logical_id < (SELECT logical_id FROM cursor) ORDER BY logical_id DESC LIMIT 3) SELECT s.id AS session_id,COALESCE(s.cwd,'') AS cwd,COALESCE(s.title,'') AS title,COALESCE(sm.logical_id,0) AS message_id,COALESCE(sm.role,'') AS role,hex(COALESCE(sm.content,'')) AS content_hex,COALESCE(sm.tool_call_id,'') AS tool_call_id,COALESCE(sm.tool_calls,'') AS tool_calls,COALESCE(sm.tool_name,'') AS tool_name,COALESCE(sm.display_kind,'') AS display_kind,COALESCE(sm.logical_timestamp,0) AS timestamp,(SELECT COUNT(*) FROM displayed) AS message_total,EXISTS(SELECT 1 FROM cursor) AS cursor_found FROM sessions AS s LEFT JOIN selected AS sm ON 1=1 WHERE s.id=CAST(X'7365735f74657374' AS TEXT) ORDER BY sm.logical_id DESC;`

const hermesToolSingleOracle = `SELECT COALESCE(m.tool_call_id,'') AS tool_call_id,COALESCE(m.tool_name,'') AS tool_name,hex(COALESCE(m.content,'')) AS content_hex FROM messages AS m WHERE m.session_id=CAST(X'7365735f74657374' AS TEXT) AND m.role='tool' AND (m.active=1 OR m.compacted=1) AND COALESCE(m.display_kind,'') <> 'hidden' AND m.tool_call_id IN (CAST(X'63616c6c5f62' AS TEXT)) ORDER BY m.id;`
const hermesToolSortedOracle = `SELECT COALESCE(m.tool_call_id,'') AS tool_call_id,COALESCE(m.tool_name,'') AS tool_name,hex(COALESCE(m.content,'')) AS content_hex FROM messages AS m WHERE m.session_id=CAST(X'7365735f74657374' AS TEXT) AND m.role='tool' AND (m.active=1 OR m.compacted=1) AND COALESCE(m.display_kind,'') <> 'hidden' AND m.tool_call_id IN (CAST(X'63616c6c5f61' AS TEXT),CAST(X'63616c6c5f62' AS TEXT)) ORDER BY m.id;`
const hermesToolSuppliedOracle = `SELECT COALESCE(m.tool_call_id,'') AS tool_call_id,COALESCE(m.tool_name,'') AS tool_name,hex(COALESCE(m.content,'')) AS content_hex FROM messages AS m WHERE m.session_id=CAST(X'7365735f74657374' AS TEXT) AND m.role='tool' AND (m.active=1 OR m.compacted=1) AND COALESCE(m.display_kind,'') <> 'hidden' AND m.tool_call_id IN (CAST(X'63616c6c5f62' AS TEXT),CAST(X'63616c6c5f61' AS TEXT)) ORDER BY m.id;`

func hermesFlowQuery(db, sql, raw string, err error) []hermesAdmissionStep {
	return []hermesAdmissionStep{{kind: "ready", ready: true}, {kind: "query", path: db, sql: sql, raw: raw, err: err}}
}

func hermesFlowDiscovery(two bool) []hermesAdmissionStep {
	roots, dbs := []string{"/planned/hermes"}, []string{hermesFlowDB}
	if two {
		roots = append(roots, "/planned/second")
		dbs = append(dbs, hermesFlowDB2)
	}
	steps := []hermesAdmissionStep{{kind: "ready", ready: true}, {kind: "roots", path: "/planned/home", paths: roots}, {kind: "databases", path: "/planned/home", paths: dbs}}
	for i, root := range roots {
		steps = append(steps, hermesAdmissionStep{kind: "eval", path: root, resolved: root}, hermesAdmissionStep{kind: "eval", path: dbs[i], resolved: dbs[i]}, hermesAdmissionStep{kind: "stat", path: dbs[i], info: openCodeFixtureInfo{100, 1}})
	}
	return steps
}

const hermesLocateReply = `[{"session_id":"ses_test","cwd":"/planned/project","title":"Hermes title","message_id":7,"role":"assistant","content_hex":"6869","timestamp":2.5,"message_total":4,"cursor_found":1}]`

func TestInstalledHermesNormalLocate(t *testing.T) {
	first, second := errors.New("first Hermes SQL failure"), errors.New("second Hermes SQL failure")
	for _, name := range []string{"alias", "mismatch", "empty-cwd", "empty-stored-cwd", "miss-success", "failure-success", "misses", "failures", "failure-miss", "miss-failure", "reason-only-first", "output-limit-first", "canonical-db"} {
		t.Run(name, func(t *testing.T) {
			steps := hermesFlowDiscovery(true)
			cwd, db, raw := "", hermesFlowDB, hermesLocateReply
			want := Location{Path: hermesFlowDB, Root: "/planned/hermes", Title: "Hermes title"}
			code := ""
			var wantErr error
			switch name {
			case "alias", "mismatch":
				cwd = "/planned/link/../alias"
			case "empty-stored-cwd":
				cwd = "/planned/alias"
				raw = `[{"session_id":"ses_test","title":"Hermes title","cursor_found":1}]`
			case "canonical-db":
				steps[3].resolved = "/planned/canonical"
				steps[4].resolved = "/planned/canonical/state.db"
				steps[5].path = "/planned/canonical/state.db"
				db = "/planned/canonical/state.db"
				want = Location{Path: db, Root: "/planned/canonical", Title: "Hermes title"}
			}
			switch name {
			case "miss-success", "misses", "miss-failure":
				steps = append(steps, hermesFlowQuery(db, hermesLocateSQLOracle, "[]", nil)...)
			case "failure-success", "failures", "failure-miss":
				steps = append(steps, hermesFlowQuery(db, hermesLocateSQLOracle, "", first)...)
			case "reason-only-first":
				steps = append(steps, hermesAdmissionStep{kind: "ready", ready: false})
			case "output-limit-first":
				steps = append(steps, hermesFlowQuery(db, hermesLocateSQLOracle, "", sqliteexec.ErrOutputLimit)...)
			default:
				steps = append(steps, hermesFlowQuery(db, hermesLocateSQLOracle, raw, nil)...)
			}
			switch name {
			case "miss-success", "failure-success":
				steps = append(steps, hermesFlowQuery(hermesFlowDB2, hermesLocateSQLOracle, hermesLocateReply, nil)...)
				want = Location{Path: hermesFlowDB2, Root: "/planned/second", Title: "Hermes title"}
			case "misses", "failure-miss":
				steps = append(steps, hermesFlowQuery(hermesFlowDB2, hermesLocateSQLOracle, "[]", nil)...)
				want, code = Location{}, "invalid_session"
				if name == "failure-miss" {
					code, wantErr = "query_failed", first
				}
			case "failures", "miss-failure", "reason-only-first", "output-limit-first":
				steps = append(steps, hermesFlowQuery(hermesFlowDB2, hermesLocateSQLOracle, "", second)...)
				want, code, wantErr = Location{}, "query_failed", first
				if name == "miss-failure" {
					wantErr = second
				}
				if name == "reason-only-first" {
					code, wantErr = "source_unavailable", nil
				}
				if name == "output-limit-first" {
					code, wantErr = "output_limit", sqliteexec.ErrOutputLimit
				}
			case "alias", "mismatch":
				resolved := "/planned/project"
				if name == "mismatch" {
					resolved = "/planned/other-project"
					want, code = Location{}, "invalid_session"
				}
				steps = append(steps, hermesAdmissionStep{kind: "eval", path: "/planned/alias", resolved: resolved}, hermesAdmissionStep{kind: "eval", path: "/planned/project", resolved: "/planned/project"})
			}
			f := newHermesAdmissionFixture(t, steps)
			var location Location
			var gotCode string
			err := f.owner.Do(context.Background(), func(c *conversationlife.Call) error {
				var e error
				location, gotCode, e = f.reader.locateIn(c, cwd, "ses_test")
				return e
			})
			f.check(t)
			if location != want || gotCode != code || !errors.Is(err, wantErr) || (wantErr != first && errors.Is(err, first)) || (wantErr != second && errors.Is(err, second)) {
				t.Fatalf("location=%+v code=%q err=%v expected=%+v/%s/%v", location, gotCode, err, want, code, wantErr)
			}
		})
	}
}

func TestInstalledHermesDynamicDiscovery(t *testing.T) {
	steps := hermesFlowDiscovery(false)
	steps = append(steps, hermesAdmissionStep{kind: "ready", ready: true}, hermesAdmissionStep{kind: "roots", path: "/planned/home", paths: []string{"/planned/second"}}, hermesAdmissionStep{kind: "databases", path: "/planned/home", paths: []string{hermesFlowDB2}}, hermesAdmissionStep{kind: "eval", path: "/planned/second", resolved: "/planned/second"}, hermesAdmissionStep{kind: "eval", path: hermesFlowDB2, resolved: hermesFlowDB2}, hermesAdmissionStep{kind: "stat", path: hermesFlowDB2, info: openCodeFixtureInfo{200, 2}})
	f := newHermesAdmissionFixture(t, steps)
	var paths [2][]string
	var codes [2]string
	var errs [2]error
	for i := range paths {
		errs[i] = f.owner.Do(context.Background(), func(c *conversationlife.Call) error {
			var e error
			paths[i], codes[i], e = f.reader.databasesIn(c)
			return e
		})
	}
	f.check(t)
	if errs != [2]error{} || codes != [2]string{} || !reflect.DeepEqual(paths, [2][]string{{hermesFlowDB}, {hermesFlowDB2}}) {
		t.Fatalf("dynamic discovery=%v/%v/%v", paths, codes, errs)
	}
}

const hermesPageReply = `[
{"session_id":"ses_test","cwd":"/planned/project","title":"Page","message_id":7,"role":"assistant","content_hex":"6e6577","tool_call_id":"row-seven","tool_calls":"null","tool_name":"row-name","display_kind":"normal","timestamp":2.5,"message_total":4,"cursor_found":1},
{"session_id":"ses_test","cwd":"/planned/project","title":"Page","message_id":4,"role":"user","content_hex":"6f6c64","timestamp":1.25,"message_total":4,"cursor_found":1},
{"session_id":"ses_test","message_id":1,"role":"user","content_hex":"64726f70","message_total":4,"cursor_found":1}]`

func TestInstalledHermesIndependentPages(t *testing.T) {
	for _, name := range []string{"page", "cursor", "invalid-cursor", "missing-cursor", "invalid-session", "absent-session", "sentinel", "corrupt-json"} {
		t.Run(name, func(t *testing.T) {
			sql, before, session, raw := hermesReadSQLOracle, "", "ses_test", hermesPageReply
			want := []Entry{{ID: "4", Timestamp: "1970-01-01T00:00:01.25Z", Role: "user", Text: "old"}, {ID: "7", Timestamp: "1970-01-01T00:00:02.5Z", Role: "assistant", Text: "new"}}
			meta := hermesRow{SessionID: "ses_test", CWD: "/planned/project", Title: "Page", MessageID: 4, Role: "user", ContentHex: "6f6c64", Timestamp: 1.25, Total: 4, CursorFound: 1}
			more, code := true, ""
			switch name {
			case "cursor":
				sql, before = hermesCursorSQLOracle, "9"
			case "invalid-cursor":
				before, code = "0", "invalid_cursor"
			case "invalid-session":
				session, code = "bad/session", "invalid_session"
			case "missing-cursor":
				sql, before, raw, code = hermesCursorSQLOracle, "9", `[{"session_id":"ses_test","cursor_found":0}]`, "invalid_cursor"
			case "absent-session":
				raw, code = "[]", "invalid_session"
			case "sentinel":
				raw = `[{"session_id":"ses_test","cwd":"/planned/project","title":"Empty","message_total":0,"cursor_found":1}]`
				want, more = []Entry{}, false
				meta = hermesRow{SessionID: "ses_test", CWD: "/planned/project", Title: "Empty", CursorFound: 1}
			case "corrupt-json":
				raw, code = "{", "source_corrupt"
			}
			if code != "" {
				want, meta, more = nil, hermesRow{}, false
			}
			var steps []hermesAdmissionStep
			if name != "invalid-cursor" && name != "invalid-session" {
				steps = hermesFlowQuery(hermesFlowDB2, sql, raw, nil)
			}
			f := newHermesAdmissionFixture(t, steps)
			var entries []Entry
			var gotMore, corrupt bool
			var gotMeta hermesRow
			var gotCode string
			err := f.owner.Do(context.Background(), func(c *conversationlife.Call) error {
				var e error
				entries, gotMore, corrupt, gotMeta, gotCode, e = f.reader.readFromIn(c, hermesFlowDB2, session, before, 2)
				return e
			})
			f.check(t)
			var syntax *json.SyntaxError
			if (name == "corrupt-json" && !errors.As(err, &syntax)) || (name != "corrupt-json" && err != nil) || !reflect.DeepEqual(entries, want) || gotMore != more || corrupt || gotMeta != meta || gotCode != code {
				t.Fatalf("page mismatch err=%v code=%s more=%v corrupt=%v metadata=%+v entries=%+v", err, gotCode, gotMore, corrupt, gotMeta, entries)
			}
		})
	}
}

func TestInstalledHermesDirectToolOrder(t *testing.T) {
	for _, name := range []string{"single", "supplied-order"} {
		t.Run(name, func(t *testing.T) {
			ids, sql := []string{"call_b"}, hermesToolSingleOracle
			if name == "supplied-order" {
				ids, sql = []string{"call_b", "call_a"}, hermesToolSuppliedOracle
			}
			f := newHermesAdmissionFixture(t, hermesFlowQuery(hermesFlowDB, sql, `[{"tool_call_id":"call_b","tool_name":"shell","content_hex":"6f6b"}]`, nil))
			var rows []hermesToolRow
			var code string
			err := f.owner.Do(context.Background(), func(c *conversationlife.Call) error {
				var e error
				rows, code, e = f.reader.queryToolRowsIn(c, hermesFlowDB, "ses_test", ids)
				return e
			})
			f.check(t)
			if err != nil || code != "" || !reflect.DeepEqual(rows, []hermesToolRow{{ToolCallID: "call_b", ToolName: "shell", ContentHex: "6f6b"}}) {
				t.Fatal("tool row/order contract", err, rows)
			}
		})
	}
}

func TestInstalledHermesMessageRows(t *testing.T) {
	for _, name := range []string{"complete-rows", "low-level-hex-session"} {
		t.Run(name, func(t *testing.T) {
			session, sql := "ses_test", hermesReadSQLOracle
			if name == "low-level-hex-session" {
				session = "bad/session"
				// Independent fixed operand substitution, never a production builder.
				sql = strings.ReplaceAll(hermesReadSQLOracle, "7365735f74657374", "6261642f73657373696f6e")
			}
			f := newHermesAdmissionFixture(t, hermesFlowQuery(hermesFlowDB, sql, hermesPageReply, nil))
			var rows []hermesRow
			var more bool
			var code string
			err := f.owner.Do(context.Background(), func(c *conversationlife.Call) error {
				var e error
				rows, more, code, e = f.reader.queryIn(c, hermesFlowDB, session, "", 2)
				return e
			})
			f.check(t)
			want := []hermesRow{
				{SessionID: "ses_test", CWD: "/planned/project", Title: "Page", MessageID: 4, Role: "user", ContentHex: "6f6c64", Timestamp: 1.25, Total: 4, CursorFound: 1},
				{SessionID: "ses_test", CWD: "/planned/project", Title: "Page", MessageID: 7, Role: "assistant", ContentHex: "6e6577", ToolCallID: "row-seven", ToolCalls: "null", ToolName: "row-name", DisplayKind: "normal", Timestamp: 2.5, Total: 4, CursorFound: 1},
			}
			if err != nil || code != "" || !more || !reflect.DeepEqual(rows, want) {
				t.Fatal("complete row/low-level hex contract", err, code, rows)
			}
		})
	}
}

func TestInstalledHermesAttachmentStorage(t *testing.T) {
	for _, mode := range []string{"scoped", "legacy", "legacy-context"} {
		t.Run(mode, func(t *testing.T) {
			// Full association ID differs from display ID. Duplicate current output
			// is suppressed; a different output appends and replaces truncation flag.
			steps := hermesFlowQuery(hermesFlowDB, hermesToolSingleOracle, `[{"tool_call_id":"call_b","tool_name":"ignored","content_hex":"6f6c64"},{"tool_call_id":"call_b","tool_name":"ignored","content_hex":"6e6577"},{"tool_call_id":"call_b","tool_name":"ignored","content_hex":"6f6c64"}]`, nil)
			f := newHermesAdmissionFixture(t, steps)
			input, original := hermesIndependentInput(), hermesIndependentInput()
			want := []Entry{{ID: "keep", Timestamp: "1970-01-01T00:00:01Z", Role: "assistant", Text: "original", Truncated: true, Tools: []ToolActivity{{ID: "display", associationID: "call_b", Name: "shell", Input: "pwd", Output: "old\nnew\nold", Error: true, Truncated: false}}}}
			var candidate []Entry
			var corrupt bool
			var err error
			switch mode {
			case "scoped":
				err = f.owner.Do(context.Background(), func(c *conversationlife.Call) error {
					var e error
					candidate, corrupt, e = f.reader.attachToolResultsIn(c, hermesFlowDB, "ses_test", input)
					return e
				})
			case "legacy":
				corrupt = f.reader.attachToolResults(hermesFlowDB, "ses_test", input)
			case "legacy-context":
				corrupt = f.reader.attachToolResultsContext(context.Background(), hermesFlowDB, "ses_test", input)
			}
			f.check(t)
			if err != nil || corrupt {
				t.Fatal("attachment failed", err)
			}
			if mode == "scoped" {
				if !reflect.DeepEqual(input, original) || !reflect.DeepEqual(candidate, want) {
					t.Fatal("candidate or original snapshot mismatch")
				}
				candidate[0].Text = "candidate-only"
				candidate[0].Tools[0].Input = "candidate-only"
				candidate[0].Tools[0].Output = "candidate-only"
				if !reflect.DeepEqual(input, original) {
					t.Fatal("candidate Entry/Tools storage aliases caller")
				}
			} else if !reflect.DeepEqual(input, want) {
				t.Fatal("legacy owned commit mismatch")
			}
		})
	}
}

func TestInstalledHermesAttachmentNoWork(t *testing.T) {
	for _, name := range []string{"nil", "empty", "no-tools", "empty-tools", "refused-nil", "refused-empty", "refused-no-tools"} {
		t.Run(name, func(t *testing.T) {
			f := newHermesAdmissionFixture(t, nil)
			var input, want []Entry
			switch name {
			case "empty", "refused-empty":
				input, want = []Entry{}, []Entry{}
			case "no-tools", "refused-no-tools":
				input, want = []Entry{{ID: "one", Role: "user", Text: "keep"}}, []Entry{{ID: "one", Role: "user", Text: "keep"}}
			case "empty-tools":
				input, want = []Entry{{ID: "one", Tools: []ToolActivity{}}}, []Entry{{ID: "one", Tools: []ToolActivity{}}}
			}
			var candidate []Entry
			var corrupt bool
			var err error
			if strings.HasPrefix(name, "refused-") {
				f.reader.observe = func(context.Context, string) { f.observations++; f.deny() }
				candidate, corrupt, err = f.reader.attachToolResultsIn(nil, "", "", input)
			} else {
				err = f.owner.Do(context.Background(), func(c *conversationlife.Call) error {
					var e error
					candidate, corrupt, e = f.reader.attachToolResultsIn(c, hermesFlowDB, "ses_test", input)
					return e
				})
			}
			f.check(t)
			if corrupt || !reflect.DeepEqual(input, want) {
				t.Fatal("no-work input changed")
			}
			if strings.HasPrefix(name, "refused-") {
				if candidate != nil || !errors.Is(err, conversationlife.ErrRefused) {
					t.Fatal("empty input bypassed admission", err)
				}
			} else if err != nil || !reflect.DeepEqual(candidate, want) {
				t.Fatal("no-work nil/empty shape", err)
			}
		})
	}
}

func TestInstalledHermesParserAttachmentFlow(t *testing.T) {
	for _, name := range []string{"multiple-tools", "valid-and-damaged", "malformed-hex", "malformed-tool-calls", "malformed-tool-result", "ordinary-tool-failure"} {
		t.Run(name, func(t *testing.T) {
			calls := `[{"id":" call_b ","function":{"name":" shell ","arguments":{"command":"pwd"}}},{"id":"call_a","name":"read","input":" x "}]`
			// Input-only encoding; all decoded expectations below are independent.
			content := "\x00json:[{\"type\":\"text\",\"text\":\"answer\"},{\"type\":\"output_text\",\"text\":\"again\"}]"
			encoded := hex.EncodeToString([]byte(content))
			tools := []ToolActivity{{ID: "call_b", associationID: "call_b", Name: "shell", Input: `{"command":"pwd"}`, Output: "one\ntwo"}, {ID: "call_a", associationID: "call_a", Name: "read", Input: "x", Output: "A"}}
			damaged := name != "multiple-tools"
			text := "answer\nagain"
			if name == "valid-and-damaged" {
				calls = `[{"id":"call_b","name":"shell","input":{"command":"pwd"}},false,{"id":"call_a","name":"read","input":"x"}]`
			}
			if name == "malformed-hex" {
				encoded, text = "zz", ""
			}
			if name == "malformed-tool-calls" {
				calls, tools = "{", nil
			}
			if name == "malformed-tool-result" {
				tools[0].Output = "one"
			}
			if name == "ordinary-tool-failure" {
				tools[0].Output, tools[1].Output = "", ""
			}
			wire := []hermesRow{
				{SessionID: "ses_test", CWD: "/planned/project", Title: "Tools", MessageID: 7, Role: "assistant", ContentHex: encoded, ToolCalls: calls, Timestamp: 2.5, Total: 2, CursorFound: 1},
				{SessionID: "ses_test", CWD: "/planned/project", Title: "Tools", MessageID: 4, Role: "user", ContentHex: "7175657374696f6e", Timestamp: 1, Total: 2, CursorFound: 1},
			}
			raw, marshalErr := json.Marshal(wire)
			if marshalErr != nil {
				t.Fatal(marshalErr)
			}
			steps := hermesFlowQuery(hermesFlowDB2, hermesReadSQLOracle, string(raw), nil)
			toolRaw := `[{"tool_call_id":"call_b","tool_name":"ignored","content_hex":"6f6e65"},{"tool_call_id":"call_b","content_hex":"6f6e65"},{"tool_call_id":"call_b","content_hex":"74776f"},{"tool_call_id":"call_a","content_hex":"41"}]`
			if name == "malformed-tool-result" {
				toolRaw = `[{"tool_call_id":"call_b","content_hex":"6f6e65"},{"tool_call_id":"call_b","content_hex":"zz"},{"tool_call_id":"call_a","content_hex":"41"}]`
			}
			var toolErr error
			if name == "ordinary-tool-failure" {
				toolErr = errors.New("ordinary tool SQL failure")
			}
			if name != "malformed-tool-calls" {
				steps = append(steps, hermesFlowQuery(hermesFlowDB2, hermesToolSortedOracle, toolRaw, toolErr)...)
			}
			f := newHermesAdmissionFixture(t, steps)
			var entries []Entry
			var more, corrupt bool
			var meta hermesRow
			var code string
			err := f.owner.Do(context.Background(), func(c *conversationlife.Call) error {
				var e error
				entries, more, corrupt, meta, code, e = f.reader.readFromIn(c, hermesFlowDB2, "ses_test", "", 2)
				return e
			})
			f.check(t)
			want := []Entry{{ID: "4", Timestamp: "1970-01-01T00:00:01Z", Role: "user", Text: "question"}, {ID: "7", Timestamp: "1970-01-01T00:00:02.5Z", Role: "assistant", Text: text, Tools: tools}}
			wantMeta := hermesRow{SessionID: "ses_test", CWD: "/planned/project", Title: "Tools", MessageID: 4, Role: "user", ContentHex: "7175657374696f6e", Timestamp: 1, Total: 2, CursorFound: 1}
			if err != nil || code != "" || more || corrupt != damaged || meta != wantMeta || !reflect.DeepEqual(entries, want) {
				t.Fatalf("parser/attachment mismatch err=%v code=%q corrupt=%v entries=%+v meta=%+v", err, code, corrupt, entries, meta)
			}
		})
	}
}

// Lifetime holders publish only stable captures of consumed work. All accesses
// to counts after publication are inspected only after the actual worker joins.
type hermesLifetimeHold struct {
	entered, release chan struct{}
	notice, opening  sync.Once
	ctx              context.Context
	count            int
}

func newHermesLifetimeHold() *hermesLifetimeHold {
	return &hermesLifetimeHold{entered: make(chan struct{}), release: make(chan struct{})}
}

func (h *hermesLifetimeHold) wait(ctx context.Context) {
	h.count++
	h.notice.Do(func() { h.ctx = ctx; close(h.entered) })
	<-h.release
}

func (h *hermesLifetimeHold) open() { h.opening.Do(func() { close(h.release) }) }

type hermesLifetimeWorker struct {
	done       chan struct{}
	returned   bool
	panicValue any
}

func newHermesLifetimeWorker() *hermesLifetimeWorker {
	return &hermesLifetimeWorker{done: make(chan struct{})}
}

func (w *hermesLifetimeWorker) run(fn func()) {
	defer close(w.done)
	defer func() { w.panicValue = recover() }()
	fn()
	w.returned = true
}

func (w *hermesLifetimeWorker) check(t *testing.T) {
	t.Helper()
	if !w.returned || w.panicValue != nil {
		t.Fatalf("actual worker abnormal exit: returned=%v panic=%v", w.returned, w.panicValue)
	}
}

func hermesAwaitHeld(t *testing.T, h *hermesLifetimeHold, w *hermesLifetimeWorker) {
	t.Helper()
	select {
	case <-w.done:
		t.Fatal("actual caller exited before work entry")
	case <-h.entered:
	}
}

func hermesHeldReceipt(t *testing.T, receipt <-chan struct{}, w *hermesLifetimeWorker) {
	t.Helper()
	timer := time.NewTimer(20 * time.Millisecond)
	defer timer.Stop()
	select {
	case <-receipt:
		t.Fatal("actual count-zero receipt closed while work held")
	case <-w.done:
		t.Fatal("actual caller returned while work held")
	case <-timer.C:
	}
}

func hermesAwaitCancellation(t *testing.T, ctx context.Context, w *hermesLifetimeWorker) {
	t.Helper()
	if ctx == nil {
		t.Fatal("missing actual consumed context")
	}
	timer := time.NewTimer(time.Second)
	defer timer.Stop()
	select {
	case <-ctx.Done():
	case <-w.done:
		t.Fatal("caller exited while synchronous work held")
	case <-timer.C:
		t.Fatal("actual context did not observe cancellation; cleanup still releases and joins")
	}
	if !errors.Is(ctx.Err(), context.Canceled) {
		t.Fatalf("actual cancellation: %v", ctx.Err())
	}
}

// Wrappers consume and validate the original callback BEFORE the selected hold.
// In particular the frozen live guard never runs after deliberate cancellation.
func hermesHoldReady(f *hermesAdmissionFixture, h *hermesLifetimeHold) {
	original := f.reader.installed.deps.ready
	f.reader.installed.deps.ready = func(c *conversationlife.Call) bool {
		result := original(c)
		h.wait(c.Context())
		return result
	}
}

func hermesHoldQuery(f *hermesAdmissionFixture, h *hermesLifetimeHold, selected string) {
	original := f.reader.installed.deps.query
	f.reader.installed.deps.query = func(c *conversationlife.Call, db, sql string, max int) ([]byte, error) {
		raw, err := original(c, db, sql, max)
		if sql == selected {
			h.wait(c.Context())
		}
		return raw, err
	}
}

func TestInstalledHermesLegacyLiveOccupancy(t *testing.T) {
	for _, name := range []string{"databases", "locate", "locateContext", "readFrom", "readFromContext", "query", "queryContext", "queryToolRows", "queryToolRowsContext", "attachToolResults", "attachToolResultsContext", "metadata", "metadata-stat", "message-sql", "tool-sql", "empty-attachment", "no-tool-attachment", "invalid-locate"} {
		t.Run(name, func(t *testing.T) {
			steps := []hermesAdmissionStep{{kind: "ready", ready: true}}
			switch name {
			case "metadata":
				steps = hermesFlowDiscovery(false)[:2]
			case "metadata-stat":
				steps = hermesFlowDiscovery(false)
			case "message-sql":
				steps = hermesFlowQuery(hermesFlowDB, hermesReadSQLOracle, hermesPageReply, nil)
			case "tool-sql":
				steps = hermesFlowQuery(hermesFlowDB, hermesToolSingleOracle, `[{"tool_call_id":"call_b","content_hex":"6f6b"}]`, nil)
			case "empty-attachment", "no-tool-attachment", "invalid-locate":
				steps = nil
			}
			f := newHermesAdmissionFixture(t, steps)
			h, w := newHermesLifetimeHold(), newHermesLifetimeWorker()
			observations := 0
			switch name {
			case "metadata":
				original := f.reader.metadata.roots
				f.reader.metadata.roots = func(ctx context.Context, home string) ([]string, error) {
					paths, err := original(ctx, home)
					h.wait(ctx)
					return paths, err
				}
			case "metadata-stat":
				original := f.reader.metadata.stat
				f.reader.metadata.stat = func(path string) (os.FileInfo, error) {
					info, err := original(path)
					// This callback supplies no context and no Call authority.
					h.wait(nil)
					return info, err
				}
			case "message-sql":
				hermesHoldQuery(f, h, hermesReadSQLOracle)
			case "tool-sql":
				hermesHoldQuery(f, h, hermesToolSingleOracle)
			case "empty-attachment", "no-tool-attachment", "invalid-locate":
				stage := "attachment"
				if name == "invalid-locate" {
					stage = "locate"
				}
				f.reader.observe = func(ctx context.Context, at string) {
					observations++
					if at != stage || observations != 1 {
						f.deny()
						return
					}
					h.wait(ctx)
				}
			default:
				hermesHoldReady(f, h)
			}
			input, wantInput := hermesIndependentInput(), hermesIndependentInput()
			if name == "empty-attachment" {
				input, wantInput = []Entry{}, []Entry{}
			}
			if name == "no-tool-attachment" {
				input, wantInput = []Entry{{ID: "keep", Text: "original", Role: "user"}}, []Entry{{ID: "keep", Text: "original", Role: "user"}}
			}
			ids, wantIDs := []string{"call_b"}, []string{"call_b"}
			var paths []string
			var rows []hermesRow
			var tools []hermesToolRow
			var entries []Entry
			var location Location
			var meta hermesRow
			var more, corrupt bool
			var code string
			t.Cleanup(func() { f.owner.Begin(); h.open(); <-w.done; f.owner.Join() })
			go w.run(func() {
				r, ctx := f.reader, context.Background()
				switch name {
				case "databases", "metadata", "metadata-stat":
					paths, code = r.databases()
				case "locate":
					location = r.locate("", "ses_test")
				case "locateContext":
					location, code = r.locateContext(ctx, "", "ses_test")
				case "invalid-locate":
					location, code = r.locateContext(ctx, "", "bad/session")
				case "readFrom":
					entries, more, corrupt, meta, code = r.readFrom(hermesFlowDB, "ses_test", "", 2)
				case "readFromContext":
					entries, more, corrupt, meta, code = r.readFromContext(ctx, hermesFlowDB, "ses_test", "", 2)
				case "query", "message-sql":
					rows, more, code = r.query(hermesFlowDB, "ses_test", "", 2)
				case "queryContext":
					rows, more, code = r.queryContext(ctx, hermesFlowDB, "ses_test", "", 2)
				case "queryToolRows", "tool-sql":
					tools, code = r.queryToolRows(hermesFlowDB, "ses_test", ids)
				case "queryToolRowsContext":
					tools, code = r.queryToolRowsContext(ctx, hermesFlowDB, "ses_test", ids)
				case "attachToolResults", "empty-attachment", "no-tool-attachment":
					corrupt = r.attachToolResults(hermesFlowDB, "ses_test", input)
				case "attachToolResultsContext":
					corrupt = r.attachToolResultsContext(ctx, hermesFlowDB, "ses_test", input)
				}
			})
			hermesAwaitHeld(t, h, w)
			receipt := f.owner.Begin()
			if name != "metadata-stat" {
				hermesAwaitCancellation(t, h.ctx, w)
			}
			hermesHeldReceipt(t, receipt, w)
			h.open()
			<-w.done
			f.check(t)
			<-receipt
			w.check(t)
			wantCode, wantCorrupt, wantObservations := "query_failed", false, 0
			switch name {
			case "databases", "metadata", "metadata-stat":
				wantCode = "source_unavailable"
			case "locate":
				wantCode = ""
			case "attachToolResults", "attachToolResultsContext", "empty-attachment", "no-tool-attachment":
				wantCode, wantCorrupt = "", true
			}
			if name == "empty-attachment" || name == "no-tool-attachment" || name == "invalid-locate" {
				wantObservations = 1
			}
			if h.count != 1 || observations != wantObservations || code != wantCode || corrupt != wantCorrupt || more || paths != nil || rows != nil || tools != nil || entries != nil || location != (Location{}) || meta != (hermesRow{}) || !reflect.DeepEqual(input, wantInput) || !reflect.DeepEqual(ids, wantIDs) {
				t.Fatalf("legacy held outcome: hold=%d observations=%d code=%q corrupt=%v", h.count, observations, code, corrupt)
			}
		})
	}
}

func TestInstalledHermesIndependentChildRetirement(t *testing.T) {
	for _, name := range []string{"queryIn", "attachToolResultsIn"} {
		t.Run(name, func(t *testing.T) {
			steps := hermesFlowQuery(hermesFlowDB, hermesReadSQLOracle, hermesPageReply, nil)
			if name == "attachToolResultsIn" {
				steps = hermesFlowQuery(hermesFlowDB, hermesToolSingleOracle, `[{"tool_call_id":"call_b","content_hex":"6f6b"}]`, nil)
			}
			f := newHermesAdmissionFixture(t, steps)
			h := newHermesLifetimeHold()
			hermesHoldQuery(f, h, steps[1].sql)
			parent, child := newHermesLifetimeWorker(), newHermesLifetimeWorker()
			parentReady, allowReturn, abortStart := make(chan struct{}), make(chan struct{}), make(chan struct{})
			var returnOnce, abortOnce sync.Once
			retireParent := func() { returnOnce.Do(func() { close(allowReturn) }) }
			abort := func() { abortOnce.Do(func() { close(abortStart) }) }
			var call *conversationlife.Call
			var parentErr, childErr error
			var rows []hermesRow
			var candidate []Entry
			var more, corrupt bool
			var code string
			input, want := hermesIndependentInput(), hermesIndependentInput()
			callbackReturned := false
			t.Cleanup(func() {
				f.owner.Begin()
				h.open()
				retireParent()
				abort()
				<-parent.done
				<-child.done
				f.owner.Join()
			})
			// Sealed two-worker launch; parentReady is NEVER fake-closed by cleanup.
			go parent.run(func() {
				parentErr = f.owner.Do(context.Background(), func(c *conversationlife.Call) error {
					call = c
					close(parentReady)
					<-allowReturn
					callbackReturned = true
					return nil
				})
			})
			go child.run(func() {
				select {
				case <-abortStart:
					return
				case <-parentReady:
				}
				if name == "queryIn" {
					rows, more, code, childErr = f.reader.queryIn(call, hermesFlowDB, "ses_test", "", 2)
				} else {
					candidate, corrupt, childErr = f.reader.attachToolResultsIn(call, hermesFlowDB, "ses_test", input)
				}
			})
			select {
			case <-parent.done:
				t.Fatal("parent exited before child entry")
			case <-child.done:
				t.Fatal("actual child exited before work entry")
			case <-h.entered:
			}
			retireParent()
			<-parent.done
			// This observation precedes any fallback fence. The actual child
			// remains held even though both parent callback and caller returned.
			hermesAwaitCancellation(t, h.ctx, child)
			receipt := f.owner.Begin()
			hermesHeldReceipt(t, receipt, child)
			h.open()
			<-child.done
			f.check(t)
			<-receipt
			parent.check(t)
			child.check(t)
			wantCode := "query_failed"
			if name == "attachToolResultsIn" {
				wantCode = ""
			}
			if !callbackReturned || parentErr != nil || !errors.Is(childErr, context.Canceled) || errors.Is(childErr, conversationlife.ErrRefused) || h.count != 1 || rows != nil || candidate != nil || more || corrupt || code != wantCode || !reflect.DeepEqual(input, want) {
				t.Fatalf("independent child: parent=%v child=%v code=%q holds=%d", parentErr, childErr, code, h.count)
			}
		})
	}
}

const hermesBudgetMessageReply = `[{"session_id":"ses_test","cwd":"/planned/project","title":"Budget","message_id":7,"role":"assistant","content_hex":"6869","tool_calls":"[{\"id\":\"call_b\",\"name\":\"shell\",\"input\":\"pwd\"}]","timestamp":2.5,"message_total":1,"cursor_found":1}]`
const hermesBudgetToolReply = `[{"tool_call_id":"call_b","tool_name":"ignored","content_hex":"6f6b"}]`

type hermesBudgetValueKey struct{}

type hermesBudgetCapture struct {
	ctx                context.Context
	deadline, observed time.Time
	timed              bool
	value              any
	err                error
}

func captureHermesBudget(c *conversationlife.Call) hermesBudgetCapture {
	ctx := c.Context()
	deadline, timed := ctx.Deadline()
	return hermesBudgetCapture{ctx: ctx, deadline: deadline, observed: time.Now(), timed: timed, value: ctx.Value(hermesBudgetValueKey{}), err: ctx.Err()}
}

func TestInstalledHermesPageQueryBudgets(t *testing.T) {
	for _, mode := range []string{"scoped", "legacy-context"} {
		t.Run(mode, func(t *testing.T) {
			for _, minimum := range []string{"none", "earlier", "later"} {
				t.Run(minimum, func(t *testing.T) {
					steps := hermesFlowQuery(hermesFlowDB2, hermesReadSQLOracle, hermesBudgetMessageReply, nil)
					steps = append(steps, hermesFlowQuery(hermesFlowDB2, hermesToolSingleOracle, hermesBudgetToolReply, nil)...)
					f := newHermesAdmissionFixture(t, steps)
					h, w := newHermesLifetimeHold(), newHermesLifetimeWorker()
					base := context.WithValue(context.Background(), hermesBudgetValueKey{}, "hermes-page-value")
					ctx, cancel := context.WithCancel(base)
					stop := func() {}
					var parentDeadline time.Time
					if minimum != "none" {
						duration := 30 * time.Second
						if minimum == "earlier" {
							duration = 5 * time.Second
						}
						parentDeadline = time.Now().Add(duration)
						ctx, stop = context.WithDeadline(ctx, parentDeadline)
					}
					var ready, queries []hermesBudgetCapture
					firstRetired, secondLive := false, false
					originalReady, originalQuery := f.reader.installed.deps.ready, f.reader.installed.deps.query
					f.reader.installed.deps.ready = func(c *conversationlife.Call) bool {
						result := originalReady(c)
						if !result {
							return false
						}
						if len(ready) >= 2 || c.Context().Value(hermesBudgetValueKey{}) != "hermes-page-value" {
							f.deny()
							return false
						}
						ready = append(ready, captureHermesBudget(c))
						return result
					}
					f.reader.installed.deps.query = func(c *conversationlife.Call, db, sql string, max int) ([]byte, error) {
						// The original guard permits real earlier parent minima; it
						// is consumed while live, not reinvoked after the gate opens.
						raw, err := originalQuery(c, db, sql, max)
						if err != nil {
							return raw, err
						}
						if len(queries) >= 2 || c.Context().Value(hermesBudgetValueKey{}) != "hermes-page-value" {
							f.deny()
							return nil, conversationlife.ErrRefused
						}
						queries = append(queries, captureHermesBudget(c))
						if len(queries) == 1 {
							h.wait(c.Context())
						} else {
							firstRetired = errors.Is(queries[0].ctx.Err(), context.Canceled)
							secondLive = c.Context().Err() == nil
						}
						return raw, err
					}
					var entries []Entry
					var more, corrupt bool
					var meta hermesRow
					var code string
					var got error
					t.Cleanup(func() {
						cancel()
						f.owner.Begin()
						h.open()
						<-w.done
						f.owner.Join()
						stop()
					})
					preCall := time.Now()
					go w.run(func() {
						if mode == "scoped" {
							got = f.owner.Do(ctx, func(c *conversationlife.Call) error {
								var err error
								entries, more, corrupt, meta, code, err = f.reader.readFromIn(c, hermesFlowDB2, "ses_test", "", 2)
								return err
							})
						} else {
							entries, more, corrupt, meta, code = f.reader.readFromContext(ctx, hermesFlowDB2, "ses_test", "", 2)
						}
					})
					hermesAwaitHeld(t, h, w)
					timer := time.NewTimer(20 * time.Millisecond)
					defer timer.Stop()
					select {
					case <-w.done:
						t.Fatal("page caller detached from first query")
					case <-ctx.Done():
						t.Fatalf("real caller expired during hold: %v", ctx.Err())
					case <-timer.C:
					}
					released := time.Now()
					h.open()
					<-w.done
					// Capture both retirements BEFORE check/Join/Begin or caller
					// cancellation can manufacture the observed state.
					parentErr := ctx.Err()
					var retirements []error
					for _, query := range queries {
						retirements = append(retirements, query.ctx.Err())
					}
					f.check(t)
					w.check(t)
					want := []Entry{{ID: "7", Timestamp: "1970-01-01T00:00:02.5Z", Role: "assistant", Text: "hi", Tools: []ToolActivity{{ID: "call_b", associationID: "call_b", Name: "shell", Input: "pwd", Output: "ok"}}}}
					wantMeta := hermesRow{SessionID: "ses_test", CWD: "/planned/project", Title: "Budget", MessageID: 7, Role: "assistant", ContentHex: "6869", ToolCalls: `[{"id":"call_b","name":"shell","input":"pwd"}]`, Timestamp: 2.5, Total: 1, CursorFound: 1}
					if got != nil || parentErr != nil || code != "" || more || corrupt || meta != wantMeta || !reflect.DeepEqual(entries, want) {
						t.Fatalf("page tuple err=%v parent=%v code=%q more=%v corrupt=%v metadata=%+v entries=%+v", got, parentErr, code, more, corrupt, meta, entries)
					}
					if h.count != 1 || len(ready) != 2 || len(queries) != 2 || len(retirements) != 2 {
						t.Fatal("two exact ready/query captures required")
					}
					for i, query := range queries {
						if !query.timed || !ready[i].timed || query.value != "hermes-page-value" || ready[i].value != "hermes-page-value" || query.err != nil || ready[i].err != nil || !query.deadline.Equal(ready[i].deadline) || query.ctx != ready[i].ctx {
							t.Fatalf("query/readiness context metadata mismatch at %d", i)
						}
						if minimum == "earlier" {
							if !query.deadline.Equal(parentDeadline) || !ready[i].deadline.Equal(parentDeadline) {
								t.Fatal("lost exact earlier parent minimum")
							}
						} else {
							lower := preCall
							if i == 1 {
								lower = released
							}
							if query.deadline.Before(lower.Add(10*time.Second)) || query.deadline.After(ready[i].observed.Add(10*time.Second)) {
								t.Fatalf("independent budget %d: deadline=%v lower=%v ready=%v", i, query.deadline, lower, ready[i].observed)
							}
						}
						if !errors.Is(retirements[i], context.Canceled) {
							t.Fatalf("query %d not retired before fence: %v", i, retirements[i])
						}
					}
					if minimum != "earlier" && queries[1].deadline.Sub(queries[0].deadline) < 20*time.Millisecond {
						t.Fatal("message and tool SQL shared a whole-page timeout")
					}
					if queries[0].ctx == queries[1].ctx || ready[0].ctx == ready[1].ctx || !firstRetired || !secondLive {
						t.Fatal("independent query context lifetimes missing")
					}
				})
			}
		})
	}
}

// These receipts describe logical first-close attempts, not native resources.
// Owner.Own is the available A1 API (Call has no Own method). It is used only
// inside the already validated actual query Call, with immediate deferred close.
type hermesTerminalSlot struct {
	slot                   *conversationlife.CloseSlot
	operand, first, repeat error
	count                  int
}

func hermesTerminalClose(f *hermesAdmissionFixture, operands []error, receipts *[]*hermesTerminalSlot) error {
	for _, operand := range operands {
		r := &hermesTerminalSlot{operand: operand}
		slot, err := f.owner.Own(func() error { r.count++; return r.operand })
		if err != nil {
			f.deny()
			return err
		}
		defer slot.Close()
		r.slot = slot
		*receipts = append(*receipts, r)
		r.first = slot.Close()
		r.repeat = slot.Close()
	}
	return nil
}

// Deliberate cleanup must not weaken the frozen nil-witness inspector.
func hermesTerminalInspect(t *testing.T, f *hermesAdmissionFixture, operands []error, slots []*hermesTerminalSlot) {
	t.Helper()
	f.owner.Join()
	if f.next != len(f.steps) || f.seen != len(f.steps) || f.unknown != 0 || f.observations != 0 || f.reader.observe != nil {
		t.Fatalf("terminal effects consumed=%d/%d seen=%d unknown=%d observations=%d", f.next, len(f.steps), f.seen, f.unknown, f.observations)
	}
	witness := f.owner.Witnesses()
	if len(slots) != len(operands) {
		t.Fatal("logical slot cardinality")
	}
	if len(operands) == 0 {
		if witness != nil {
			t.Fatal("request-only error poisoned cleanup ledger")
		}
		return
	}
	joined, ok := witness.(interface{ Unwrap() []error })
	if !ok || len(joined.Unwrap()) != len(operands) {
		t.Fatal("raw witness cardinality")
	}
	for i, operand := range operands {
		r := slots[i]
		if joined.Unwrap()[i] != operand || r.operand != operand || r.first != operand || r.repeat != operand || r.count != 1 {
			t.Fatalf("raw witness/first/repeat/count mismatch at %d", i)
		}
		for j := 0; j < i; j++ {
			if slots[j].slot == r.slot {
				t.Fatal("equal-error siblings shared a slot")
			}
		}
	}
}

func hermesTerminalIdentities(t *testing.T, got, want, primary error, operands []error) {
	t.Helper()
	for _, identity := range []error{context.Canceled, context.DeadlineExceeded, conversationlife.ErrRefused, sqliteexec.ErrCleanupUncertain, ErrSourceCleanupUncertain, sqliteexec.ErrOutputLimit, primary} {
		if identity != nil && errors.Is(got, identity) != errors.Is(want, identity) {
			t.Fatal("semantic identity lost or added")
		}
	}
	for _, operand := range operands {
		if errors.Is(got, operand) || (got != nil && strings.Contains(got.Error(), operand.Error())) {
			t.Fatal("raw cleanup canary escaped presentation tree/string")
		}
	}
}

func hermesTerminalPage(output string) []Entry {
	return []Entry{{ID: "7", Timestamp: "1970-01-01T00:00:02.5Z", Role: "assistant", Text: "hi", Tools: []ToolActivity{{ID: "call_b", associationID: "call_b", Name: "shell", Input: "pwd", Output: output}}}}
}

func hermesTerminalMetadata() hermesRow {
	return hermesRow{SessionID: "ses_test", CWD: "/planned/project", Title: "Budget", MessageID: 7, Role: "assistant", ContentHex: "6869", ToolCalls: `[{"id":"call_b","name":"shell","input":"pwd"}]`, Timestamp: 2.5, Total: 1, CursorFound: 1}
}

func TestInstalledHermesTerminalSQLMarkers(t *testing.T) {
	for _, path := range []string{"locate", "message", "tool", "attachment"} {
		t.Run(path, func(t *testing.T) {
			primary := errors.New("independent Hermes primary")
			for _, tc := range []struct {
				name              string
				semantic, cleanup error
				n                 int
			}{
				{"canceled", errors.Join(primary, context.Canceled), nil, 0},
				{"deadline", errors.Join(primary, context.DeadlineExceeded), nil, 0},
				{"refused", errors.Join(primary, conversationlife.ErrRefused), nil, 0},
				{"sql-cleanup", nil, sqliteexec.ErrCleanupUncertain, 1},
				{"source-cleanup", nil, ErrSourceCleanupUncertain, 1},
				{"primary-cleanup", primary, sqliteexec.ErrCleanupUncertain, 1},
				{"cancel-cleanup", errors.Join(primary, context.Canceled), sqliteexec.ErrCleanupUncertain, 1},
				{"deadline-cleanup", errors.Join(primary, context.DeadlineExceeded), sqliteexec.ErrCleanupUncertain, 1},
				{"equal-cleanup-siblings", primary, sqliteexec.ErrCleanupUncertain, 2},
			} {
				t.Run(tc.name, func(t *testing.T) {
					selectedSQL := hermesReadSQLOracle
					var steps []hermesAdmissionStep
					if path == "locate" {
						steps = hermesFlowDiscovery(true)
						selectedSQL = hermesLocateSQLOracle
					}
					if path == "tool" {
						steps = hermesFlowQuery(hermesFlowDB, hermesReadSQLOracle, hermesBudgetMessageReply, nil)
					}
					if path == "tool" || path == "attachment" {
						selectedSQL = hermesToolSingleOracle
					}
					// Even an original scripted error must not skip logical closes.
					steps = append(steps, hermesFlowQuery(hermesFlowDB, selectedSQL, "[]", tc.semantic)...)
					f := newHermesAdmissionFixture(t, steps)
					var operands []error
					canary := errors.New("inert-hermes-close-canary")
					for i := 0; i < tc.n; i++ {
						operands = append(operands, canary)
					}
					var slots []*hermesTerminalSlot
					selected, live := 0, false
					base := f.reader.installed.deps.query
					f.reader.installed.deps.query = func(c *conversationlife.Call, db, sql string, max int) ([]byte, error) {
						raw, err := base(c, db, sql, max)
						if f.unknown != 0 {
							return nil, conversationlife.ErrRefused
						}
						if sql != selectedSQL {
							return raw, err
						}
						selected++
						live = c.Context().Err() == nil
						closeErr := hermesTerminalClose(f, operands, &slots)
						live = live && c.Context().Err() == nil
						return nil, errors.Join(err, tc.cleanup, closeErr)
					}
					input, original := hermesIndependentInput(), hermesIndependentInput()
					var entries []Entry
					var location Location
					var more, corrupt bool
					var meta hermesRow
					var code string
					err := f.owner.Do(context.Background(), func(c *conversationlife.Call) error {
						var err error
						switch path {
						case "locate":
							location, code, err = f.reader.locateIn(c, "", "ses_test")
						case "attachment":
							entries, corrupt, err = f.reader.attachToolResultsIn(c, hermesFlowDB, "ses_test", input)
						default:
							entries, more, corrupt, meta, code, err = f.reader.readFromIn(c, hermesFlowDB, "ses_test", "", 2)
						}
						return err
					})
					hermesTerminalInspect(t, f, operands, slots)
					wantCode := "query_failed"
					if path == "attachment" {
						wantCode = ""
					}
					if selected != 1 || !live || err == nil || entries != nil || location != (Location{}) || more || corrupt || meta != (hermesRow{}) || code != wantCode || !reflect.DeepEqual(input, original) {
						t.Fatal("terminal result, live callback, or independent input contract")
					}
					hermesTerminalIdentities(t, err, errors.Join(tc.semantic, tc.cleanup), primary, operands)
				})
			}
		})
	}
}

func TestInstalledHermesNonterminalTools(t *testing.T) {
	for _, path := range []string{"page", "attachment"} {
		t.Run(path, func(t *testing.T) {
			for _, name := range []string{"ordinary-error", "corrupt-json", "empty-valid"} {
				t.Run(name, func(t *testing.T) {
					raw := "[]"
					var queryErr error
					if name == "ordinary-error" {
						queryErr = errors.New("ordinary Hermes tool failure")
					}
					if name == "corrupt-json" {
						raw = "{"
					}
					var steps []hermesAdmissionStep
					if path == "page" {
						steps = hermesFlowQuery(hermesFlowDB, hermesReadSQLOracle, hermesBudgetMessageReply, nil)
					}
					steps = append(steps, hermesFlowQuery(hermesFlowDB, hermesToolSingleOracle, raw, queryErr)...)
					f := newHermesAdmissionFixture(t, steps)
					input, original := hermesIndependentInput(), hermesIndependentInput()
					var entries []Entry
					var more, corrupt bool
					var meta hermesRow
					var code string
					err := f.owner.Do(context.Background(), func(c *conversationlife.Call) error {
						var err error
						if path == "page" {
							entries, more, corrupt, meta, code, err = f.reader.readFromIn(c, hermesFlowDB, "ses_test", "", 2)
						} else {
							entries, corrupt, err = f.reader.attachToolResultsIn(c, hermesFlowDB, "ses_test", input)
						}
						return err
					})
					hermesTerminalInspect(t, f, nil, nil)
					want, wantMeta := hermesTerminalPage(""), hermesTerminalMetadata()
					if path == "attachment" {
						want, wantMeta = hermesIndependentInput(), hermesRow{}
					}
					if err != nil || code != "" || more || corrupt != (name != "empty-valid") || meta != wantMeta || !reflect.DeepEqual(entries, want) || !reflect.DeepEqual(input, original) {
						t.Fatal("nonterminal complete page/candidate policy")
					}
					entries[0].Text = "result-only"
					entries[0].Tools[0].Input = "result-only"
					entries[0].Tools[0].Output = "result-only"
					if !reflect.DeepEqual(input, original) || reflect.DeepEqual(entries, want) {
						t.Fatal("independent Entry/Tools storage")
					}
				})
			}
		})
	}
}

type hermesRawReceipt struct {
	db, sql       string
	max, rawBytes int
}

func TestInstalledHermesLogicalByteBoundaries(t *testing.T) {
	for _, tc := range []struct {
		name, stage string
		delta       int
		cleanup     bool
	}{
		{"message-under", "message", -1, false}, {"message-exact", "message", 0, false}, {"message-over", "message", 1, false},
		{"tool-under", "tool", -1, false}, {"tool-exact", "tool", 0, false}, {"tool-over", "tool", 1, false},
		{"message-over-cleanup", "message", 1, true}, {"tool-over-cleanup", "tool", 1, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			const budget = 8 * 1024 * 1024
			target := budget + tc.delta
			steps := hermesFlowQuery(hermesFlowDB2, hermesReadSQLOracle, hermesBudgetMessageReply, nil)
			if tc.stage != "message" || tc.delta <= 0 {
				steps = append(steps, hermesFlowQuery(hermesFlowDB2, hermesToolSingleOracle, hermesBudgetToolReply, nil)...)
			}
			f := newHermesAdmissionFixture(t, steps)
			var operands []error
			if tc.cleanup {
				operands = []error{errors.New("inert-hermes-bound-close-canary")}
			}
			var slots []*hermesTerminalSlot
			var receipts []hermesRawReceipt
			selected, live := 0, false
			base := f.reader.installed.deps.query
			f.reader.installed.deps.query = func(c *conversationlife.Call, db, sql string, max int) ([]byte, error) {
				raw, err := base(c, db, sql, max)
				if f.unknown != 0 {
					return nil, conversationlife.ErrRefused
				}
				chosen := (tc.stage == "message" && sql == hermesReadSQLOracle) || (tc.stage == "tool" && sql == hermesToolSingleOracle)
				if chosen {
					selected++
					live = c.Context().Err() == nil
					// One current large buffer; small JSON support only. Never trim
					// an oversized nonempty reply into compliance before enforcement.
					padded := make([]byte, target)
					n := copy(padded, raw)
					for i := n; i < len(padded); i++ {
						padded[i] = ' '
					}
					raw = padded
				}
				receipts = append(receipts, hermesRawReceipt{db: db, sql: sql, max: max, rawBytes: len(raw)})
				if len(raw) > max {
					err = errors.Join(err, sqliteexec.ErrOutputLimit)
				}
				if chosen {
					// Do not return early on the limit: close before safe uncertainty.
					err = errors.Join(err, hermesTerminalClose(f, operands, &slots))
					if tc.cleanup {
						err = errors.Join(err, sqliteexec.ErrCleanupUncertain)
					}
					live = live && c.Context().Err() == nil
				}
				if err != nil {
					return nil, err
				}
				return raw, nil
			}
			var entries []Entry
			var more, corrupt bool
			var meta hermesRow
			var code string
			err := f.owner.Do(context.Background(), func(c *conversationlife.Call) error {
				var err error
				entries, more, corrupt, meta, code, err = f.reader.readFromIn(c, hermesFlowDB2, "ses_test", "", 2)
				return err
			})
			hermesTerminalInspect(t, f, operands, slots)
			messageBytes, toolBytes := len(hermesBudgetMessageReply), len(hermesBudgetToolReply)
			if tc.stage == "message" {
				messageBytes = target
			} else {
				toolBytes = target
			}
			wantReceipts := []hermesRawReceipt{{db: hermesFlowDB2, sql: hermesReadSQLOracle, max: budget, rawBytes: messageBytes}}
			if tc.stage != "message" || tc.delta <= 0 {
				wantReceipts = append(wantReceipts, hermesRawReceipt{db: hermesFlowDB2, sql: hermesToolSingleOracle, max: budget, rawBytes: toolBytes})
			}
			if selected != 1 || !live || !reflect.DeepEqual(receipts, wantReceipts) {
				t.Fatal("exact raw byte/query/budget receipts")
			}
			var wantErr error
			wantCode := ""
			want, wantMeta, wantCorrupt := hermesTerminalPage("ok"), hermesTerminalMetadata(), false
			if tc.delta > 0 {
				if tc.stage == "message" || tc.cleanup {
					wantErr, wantCode = sqliteexec.ErrOutputLimit, "output_limit"
					want, wantMeta = nil, hermesRow{}
					if tc.cleanup {
						wantErr, wantCode = errors.Join(sqliteexec.ErrOutputLimit, sqliteexec.ErrCleanupUncertain), "query_failed"
					}
				} else {
					want, wantCorrupt = hermesTerminalPage(""), true
				}
			}
			if (err == nil) != (wantErr == nil) || code != wantCode || more || corrupt != wantCorrupt || meta != wantMeta || !reflect.DeepEqual(entries, want) {
				t.Fatal("raw limit complete page/terminal compatibility contract")
			}
			hermesTerminalIdentities(t, err, wantErr, nil, operands)
		})
	}
}

// Only real consumed-context cancellation may supply Canceled here. Inspect
// leaves as well as errors.Is so an unrelated primary cannot hide in a join.
func hermesSQLCancellationTree(err error, cleanup bool) bool {
	if err == nil {
		return false
	}
	if joined, ok := err.(interface{ Unwrap() []error }); ok {
		children := joined.Unwrap()
		if len(children) == 0 {
			return false
		}
		for _, child := range children {
			if !hermesSQLCancellationTree(child, cleanup) {
				return false
			}
		}
		return true
	}
	return err == context.Canceled || (cleanup && err == sqliteexec.ErrCleanupUncertain)
}

func TestInstalledHermesActualSQLCancellation(t *testing.T) {
	for _, stage := range []string{"message", "tool"} {
		t.Run(stage, func(t *testing.T) {
			for _, name := range []string{"caller", "owner", "parent", "caller-cleanup", "owner-cleanup"} {
				t.Run(name, func(t *testing.T) {
					cleanup := strings.HasSuffix(name, "-cleanup")
					cause := strings.TrimSuffix(name, "-cleanup")
					db, selected := hermesFlowDB, hermesReadSQLOracle
					if stage == "tool" {
						db, selected = hermesFlowDB2, hermesToolSingleOracle
					}
					steps := hermesFlowQuery(db, hermesReadSQLOracle, hermesBudgetMessageReply, nil)
					if stage == "tool" {
						steps = append(steps, hermesFlowQuery(db, hermesToolSingleOracle, hermesBudgetToolReply, nil)...)
					}
					// Independent script snapshot; no returned page/parser supplies expectations.
					wantSteps := hermesFlowQuery(db, hermesReadSQLOracle, hermesBudgetMessageReply, nil)
					if stage == "tool" {
						wantSteps = append(wantSteps, hermesFlowQuery(db, hermesToolSingleOracle, hermesBudgetToolReply, nil)...)
					}
					f := newHermesAdmissionFixture(t, steps)
					h := newHermesLifetimeHold()
					parent, child := newHermesLifetimeWorker(), newHermesLifetimeWorker()
					parentReady, allowReturn, abortStart := make(chan struct{}), make(chan struct{}), make(chan struct{})
					closeStarted := make(chan struct{})
					var returnOnce, abortOnce sync.Once
					retireParent := func() { returnOnce.Do(func() { close(allowReturn) }) }
					abort := func() { abortOnce.Do(func() { close(abortStart) }) }
					caller, cancel := context.WithCancel(context.Background())
					var call *conversationlife.Call
					var parentContext context.Context
					var parentErr, childErr error
					var entries []Entry
					var more, corrupt, callbackReturned bool
					var meta hermesRow
					var code string
					var slots []*hermesTerminalSlot
					var operands []error
					if cleanup {
						operands = []error{errors.New("inert-hermes-retained-sql-close-canary")}
					}
					var replies [][]byte
					preCloseCount := -1
					original := f.reader.installed.deps.query
					f.reader.installed.deps.query = func(c *conversationlife.Call, database, sql string, max int) ([]byte, error) {
						raw, err := original(c, database, sql, max)
						if err != nil || f.unknown != 0 {
							return raw, err
						}
						replies = append(replies, raw)
						if sql != selected {
							return raw, nil
						}
						var retained *hermesTerminalSlot
						if cleanup {
							retained = &hermesTerminalSlot{operand: operands[0]}
							slot, ownErr := f.owner.Own(func() error {
								close(closeStarted)
								retained.count++
								return retained.operand
							})
							if ownErr != nil {
								f.deny()
								return nil, ownErr
							}
							defer slot.Close()
							retained.slot = slot
							slots = append(slots, retained)
							preCloseCount = retained.count
						}
						// Stable first actual query context, before notice, while LIVE.
						h.wait(c.Context())
						if retained != nil {
							retained.first = retained.slot.Close()
							retained.repeat = retained.slot.Close()
							return raw, sqliteexec.ErrCleanupUncertain
						}
						// No injected cancellation and no rerun of the live validator.
						return raw, nil
					}
					// Registered after factory Join, before BOTH sealed launches.
					t.Cleanup(func() {
						f.owner.Begin()
						cancel()
						h.open()
						retireParent()
						abort()
						<-parent.done
						<-child.done
						f.owner.Join()
						f.reader.installed.deps.query = original
						hermesTerminalInspect(t, f, operands, slots)
						parent.check(t)
						child.check(t)
					})
					go parent.run(func() {
						parentErr = f.owner.Do(caller, func(c *conversationlife.Call) error {
							call, parentContext = c, c.Context()
							close(parentReady)
							<-allowReturn
							callbackReturned = true
							return nil
						})
					})
					go child.run(func() {
						select {
						case <-abortStart:
							return
						case <-parentReady:
						}
						entries, more, corrupt, meta, code, childErr = f.reader.readFromIn(call, db, "ses_test", "", 2)
					})
					select {
					case <-parent.done:
						t.Fatal("actual parent exited before SQL entry")
					case <-child.done:
						t.Fatal("actual reader child exited before SQL entry")
					case <-h.entered:
					}
					if h.ctx == nil || h.ctx.Err() != nil || parentContext == nil || h.ctx.Done() == parentContext.Done() || h.ctx.Done() == caller.Done() {
						t.Fatal("held SQL must consume its own live context, not an ancestor")
					}
					var receipt <-chan struct{}
					switch cause {
					case "caller":
						cancel() // ONLY caller: parent remains held and Owner unfenced.
					case "owner":
						receipt = f.owner.Begin()
					case "parent":
						retireParent()
						<-parent.done
						parent.check(t)
					}
					// Chosen-cause evidence BEFORE parent retirement/fallback for caller,
					// and BEFORE Begin for parent. Final errors cannot replace this.
					hermesAwaitCancellation(t, h.ctx, child)
					if cause != "caller" && caller.Err() != nil {
						t.Fatal("non-caller cause canceled the caller")
					}
					retireParent()
					<-parent.done
					parent.check(t)
					if !callbackReturned || parentErr != nil {
						t.Fatal("actual parent callback/caller did not return normally")
					}
					if receipt == nil {
						receipt = f.owner.Begin()
					}
					// Parent is gone: neither receipt nor child can use proxy occupancy.
					func() {
						timer := time.NewTimer(20 * time.Millisecond)
						defer timer.Stop()
						select {
						case <-receipt:
							t.Fatal("count-zero receipt closed with actual SQL held")
						case <-child.done:
							t.Fatal("actual reader returned with SQL held")
						case <-closeStarted:
							t.Fatal("retained logical slot closed before SQL release")
						case <-timer.C:
						}
					}()
					h.open()
					<-child.done
					f.owner.Join()
					<-receipt
					child.check(t)
					hermesTerminalInspect(t, f, operands, slots)
					wantErr := error(context.Canceled)
					if cleanup {
						wantErr = errors.Join(context.Canceled, sqliteexec.ErrCleanupUncertain)
						if preCloseCount != 0 {
							t.Fatal("slot was not open at entry")
						}
						select {
						case <-closeStarted:
						default:
							t.Fatal("missing actual close-start notice")
						}
					}
					hermesTerminalIdentities(t, childErr, wantErr, nil, operands)
					if !hermesSQLCancellationTree(childErr, cleanup) || h.count != 1 || entries != nil || more || corrupt || meta != (hermesRow{}) || code != "query_failed" {
						t.Fatal("actual cancellation must suppress the complete page without extra primary")
					}
					if !reflect.DeepEqual(steps, wantSteps) || len(replies) != len(wantSteps)/2 {
						t.Fatal("independent input/script preservation")
					}
					for i, raw := range replies {
						if string(raw) != wantSteps[2*i+1].raw {
							t.Fatal("saved valid SQL reply mutated")
						}
					}
				})
			}
		})
	}
}

// These finite traces are derived from the reached plain-text paths, not from
// observed calibration or production-generated expected values.
func hermesTraversalTrace(area string, control bool) []string {
	var trace []string
	add := func(stage string, n int) {
		for i := 0; i < n; i++ {
			trace = append(trace, stage)
		}
	}
	if area == "parser" {
		add("parser", 1)
		add("message-rows", 11)
		if control {
			add("parser", 30)
			add("attachment", 12)
			add("parser", 1)
		} else {
			add("parser", 10)
		}
	} else {
		add("attachment", 18)
		add("tool-rows", 5)
		if control {
			add("attachment", 39)
		} else {
			add("attachment", 15)
		}
	}
	return trace
}

const hermesTraversalMessages = `[
{"session_id":"ses_test","cwd":"/planned/project","title":"Traversal","message_id":4,"role":"assistant","content_hex":"666f7572","timestamp":4,"message_total":4,"cursor_found":1},
{"session_id":"ses_test","cwd":"/planned/project","title":"Traversal","message_id":3,"role":"user","content_hex":"7468726565","timestamp":3,"message_total":4,"cursor_found":1},
{"session_id":"ses_test","cwd":"/planned/project","title":"Traversal","message_id":2,"role":"assistant","content_hex":"74776f","timestamp":2,"message_total":4,"cursor_found":1},
{"session_id":"ses_test","cwd":"/planned/project","title":"Traversal","message_id":1,"role":"user","content_hex":"6f6e65","timestamp":1,"message_total":4,"cursor_found":1}]`

const hermesTraversalTools = `[{"tool_call_id":"call_b","tool_name":"ignored-one","content_hex":"6f6e65"},{"tool_call_id":"call_b","tool_name":"ignored-two","content_hex":"74776f"},{"tool_call_id":"call_b","tool_name":"ignored-three","content_hex":"7468726565"}]`

func hermesTraversalInput() []Entry {
	return []Entry{
		{ID: "a", Timestamp: "1970-01-01T00:00:01Z", Role: "assistant", Text: "alpha", Truncated: true, Tools: []ToolActivity{{ID: "display-a", associationID: "call_b", Name: "shell-a", Input: "pwd-a", Output: "old-a", Error: true, Truncated: true}}},
		{ID: "b", Timestamp: "1970-01-01T00:00:02Z", Role: "user", Text: "beta", Tools: []ToolActivity{{ID: "display-b", associationID: "call_b", Name: "shell-b", Input: "pwd-b", Output: "old-b", Truncated: true}}},
		{ID: "c", Timestamp: "1970-01-01T00:00:03Z", Role: "assistant", Text: "gamma", Truncated: true, Tools: []ToolActivity{{ID: "display-c", associationID: "call_b", Name: "shell-c", Input: "pwd-c", Output: "old-c", Error: true}}},
	}
}

func hermesTraversalCandidate() []Entry {
	return []Entry{
		{ID: "a", Timestamp: "1970-01-01T00:00:01Z", Role: "assistant", Text: "alpha", Truncated: true, Tools: []ToolActivity{{ID: "display-a", associationID: "call_b", Name: "shell-a", Input: "pwd-a", Output: "old-a\none\ntwo\nthree", Error: true}}},
		{ID: "b", Timestamp: "1970-01-01T00:00:02Z", Role: "user", Text: "beta", Tools: []ToolActivity{{ID: "display-b", associationID: "call_b", Name: "shell-b", Input: "pwd-b", Output: "old-b\none\ntwo\nthree"}}},
		{ID: "c", Timestamp: "1970-01-01T00:00:03Z", Role: "assistant", Text: "gamma", Truncated: true, Tools: []ToolActivity{{ID: "display-c", associationID: "call_b", Name: "shell-c", Input: "pwd-c", Output: "old-c\none\ntwo\nthree", Error: true}}},
	}
}

func hermesTraversalPage() []Entry {
	return []Entry{
		{ID: "1", Timestamp: "1970-01-01T00:00:01Z", Role: "user", Text: "one"},
		{ID: "2", Timestamp: "1970-01-01T00:00:02Z", Role: "assistant", Text: "two"},
		{ID: "3", Timestamp: "1970-01-01T00:00:03Z", Role: "user", Text: "three"},
		{ID: "4", Timestamp: "1970-01-01T00:00:04Z", Role: "assistant", Text: "four"},
	}
}

// All mutable observation state belongs to the one actual child. Only the hold's
// stable publication and quiescent input are read before that child is joined.
type hermesTraversalObservation struct {
	f          *hermesAdmissionFixture
	h          *hermesLifetimeHold
	area       string
	want, seen []string
	counts     map[string]int
	contexts   map[string]context.Context
	query      context.Context
	raw        []byte
	queries    int
}

func (o *hermesTraversalObservation) observe(ctx context.Context, stage string) {
	o.f.observations++
	o.seen = append(o.seen, stage)
	o.counts[stage]++
	i := len(o.seen) - 1
	if ctx == nil || i >= len(o.want) || o.want[i] != stage {
		o.f.deny()
		return
	}
	// Calling Err on a wrapper here would recursively invoke this observer.
	switch ctx.(type) {
	case hermesObservationContext, *hermesObservationContext:
		o.f.deny()
		return
	}
	if first := o.contexts[stage]; first != nil {
		if first.Done() != ctx.Done() {
			o.f.deny()
			return
		}
	} else {
		o.contexts[stage] = ctx
	}
	// Query completion divides each row stage at an independently fixed point.
	beforeQuery := (o.area == "parser" && i < 3) || (o.area == "attachment" && i < 21)
	if (beforeQuery && o.queries != 0) || (!beforeQuery && o.queries != 1) {
		o.f.deny()
		return
	}
	hold := (o.area == "parser" && stage == "parser" && o.counts[stage] == 10) ||
		(o.area == "attachment" && stage == "attachment" && o.counts[stage] == 32)
	if hold {
		rowStage, rowCount := "message-rows", 11
		if o.area == "attachment" {
			rowStage, rowCount = "tool-rows", 5
		}
		rows := o.contexts[rowStage]
		if o.queries != 1 || o.counts[rowStage] != rowCount || rows == nil || o.query == nil ||
			ctx.Err() != nil || ctx.Done() == rows.Done() || ctx.Done() == o.query.Done() ||
			rows.Done() == o.query.Done() || rows.Err() != context.Canceled || o.query.Err() != context.Canceled {
			o.f.deny()
			return
		}
		for _, retired := range []context.Context{rows, o.query} {
			select {
			case <-retired.Done():
			default:
				o.f.deny()
				return
			}
		}
		// Capture this checkpoint's RAW delegated context BEFORE publishing entry.
		o.h.wait(ctx)
	}
}

// Unlike the frozen nil-observer inspectors, this inspects the installed hook
// while still present. Restoration is the caller's deferred, post-join action.
func hermesTraversalInspect(t *testing.T, o *hermesTraversalObservation, steps []hermesAdmissionStep, parent, child *hermesLifetimeWorker, caller, parentContext context.Context, input, snapshot []Entry) {
	t.Helper()
	f := o.f
	if f.reader.observe == nil || f.reader.installed.deps.query == nil ||
		!reflect.DeepEqual(f.steps, steps) || f.next != len(steps) || f.seen != len(steps) || f.unknown != 0 ||
		f.observations != len(o.want) || !reflect.DeepEqual(o.seen, o.want) || f.owner.Witnesses() != nil ||
		o.queries != 1 || string(o.raw) != steps[1].raw || o.h.count != 1 {
		t.Fatal("traversal script/observer/trace/raw/witness/hold contract")
	}
	wantCounts := make(map[string]int)
	for _, stage := range o.want {
		wantCounts[stage]++
	}
	if !reflect.DeepEqual(o.counts, wantCounts) || len(o.contexts) != len(wantCounts) || !reflect.DeepEqual(input, snapshot) {
		t.Fatal("traversal exact counts/context cardinality/input preservation")
	}
	parent.check(t)
	child.check(t)
	if caller == nil || parentContext == nil || o.query == nil || o.h.ctx == nil ||
		o.h.ctx.Done() != o.contexts[o.area].Done() {
		t.Fatal("missing actual context identities")
	}
	contexts := []context.Context{caller, parentContext, o.query}
	for stage := range wantCounts {
		contexts = append(contexts, o.contexts[stage])
	}
	for i, ctx := range contexts {
		if ctx == nil || ctx.Done() == nil || ctx.Err() != context.Canceled {
			t.Fatal("joined context not retired")
		}
		for j := 0; j < i; j++ {
			if ctx.Done() == contexts[j].Done() {
				t.Fatal("delegated contexts aliased an ancestor or another stage")
			}
		}
	}
}

func TestInstalledHermesActualTraversalCancellation(t *testing.T) {
	for _, area := range []string{"parser", "attachment"} {
		t.Run(area, func(t *testing.T) {
			for _, cause := range []string{"control", "caller", "owner", "parent"} {
				t.Run(cause, func(t *testing.T) {
					control := cause == "control"
					db, sql, raw := hermesFlowDB2, hermesToolSingleOracle, hermesTraversalTools
					var input, snapshot []Entry
					if area == "parser" {
						const operand = " ORDER BY logical_id DESC LIMIT 3)"
						if strings.Count(hermesReadSQLOracle, operand) != 1 {
							t.Fatal("independent LIMIT operand no longer unique")
						}
						db, sql, raw = hermesFlowDB, strings.Replace(hermesReadSQLOracle, operand, " ORDER BY logical_id DESC LIMIT 5)", 1), hermesTraversalMessages
					} else {
						input, snapshot = hermesTraversalInput(), hermesTraversalInput()
					}
					f := newHermesAdmissionFixture(t, hermesFlowQuery(db, sql, raw, nil))
					wantSteps := hermesFlowQuery(db, sql, raw, nil)
					h := newHermesLifetimeHold()
					o := &hermesTraversalObservation{f: f, h: h, area: area, want: hermesTraversalTrace(area, control), counts: make(map[string]int), contexts: make(map[string]context.Context)}
					parent, child := newHermesLifetimeWorker(), newHermesLifetimeWorker()
					parentReady, allowReturn, abortStart := make(chan struct{}), make(chan struct{}), make(chan struct{})
					var returnOnce, abortOnce sync.Once
					retireParent := func() { returnOnce.Do(func() { close(allowReturn) }) }
					abort := func() { abortOnce.Do(func() { close(abortStart) }) }
					caller, cancel := context.WithCancel(context.Background())
					var call *conversationlife.Call
					var parentContext context.Context
					var parentErr, childErr error
					var entries []Entry
					var more, corrupt, callbackReturned bool
					var meta hermesRow
					var code string
					originalQuery, originalObserver := f.reader.installed.deps.query, f.reader.observe
					f.reader.observe = o.observe
					f.reader.installed.deps.query = func(c *conversationlife.Call, database, query string, max int) ([]byte, error) {
						data, err := originalQuery(c, database, query, max)
						if err != nil || f.unknown != 0 {
							return data, err
						}
						at := 3
						if area == "attachment" {
							at = 21
						}
						if o.queries != 0 || len(o.seen) != at {
							f.deny()
							return nil, conversationlife.ErrRefused
						}
						o.queries++
						o.query, o.raw = c.Context(), data
						return data, nil
					}
					// All gates and completion channels precede both sealed launches.
					// No assertion exit can wait before independently releasing all gates.
					t.Cleanup(func() {
						f.owner.Begin()
						cancel()
						h.open()
						retireParent()
						abort()
						<-parent.done
						<-child.done
						f.owner.Join()
						defer func() {
							f.reader.installed.deps.query = originalQuery
							f.reader.observe = originalObserver
						}()
						hermesTraversalInspect(t, o, wantSteps, parent, child, caller, parentContext, input, snapshot)
						if !callbackReturned || parentErr != nil {
							t.Fatal("actual parent callback/caller did not return normally")
						}
					})
					go parent.run(func() {
						parentErr = f.owner.Do(caller, func(c *conversationlife.Call) error {
							call, parentContext = c, c.Context()
							close(parentReady)
							<-allowReturn
							callbackReturned = true
							return nil
						})
					})
					go child.run(func() {
						select {
						case <-abortStart:
							return
						case <-parentReady:
						}
						if area == "parser" {
							entries, more, corrupt, meta, code, childErr = f.reader.readFromIn(call, db, "ses_test", "", 4)
						} else {
							entries, corrupt, childErr = f.reader.attachToolResultsIn(call, db, "ses_test", input)
						}
					})
					entryTimer := time.NewTimer(time.Second)
					defer entryTimer.Stop()
					select {
					case <-parent.done:
						t.Fatal("actual parent exited before traversal entry")
					case <-child.done:
						t.Fatal("actual child exited before traversal entry")
					case <-entryTimer.C:
						t.Fatal("target traversal not reached; cleanup releases and joins")
					case <-h.entered:
					}
					// Only stable, published evidence and genuinely quiescent input here.
					if h.ctx == nil || h.ctx.Err() != nil || parentContext == nil || parentContext.Err() != nil || caller.Err() != nil ||
						h.ctx.Done() == parentContext.Done() || h.ctx.Done() == caller.Done() ||
						o.query == nil || h.ctx.Done() == o.query.Done() || o.query.Err() != context.Canceled ||
						!reflect.DeepEqual(input, snapshot) {
						t.Fatal("held actual context/retired query/quiescent input contract")
					}
					if control {
						// No Begin, cancellation or parent retirement while traversal runs.
						func() {
							timer := time.NewTimer(20 * time.Millisecond)
							defer timer.Stop()
							select {
							case <-parent.done:
								t.Fatal("control parent returned at hold")
							case <-child.done:
								t.Fatal("control child returned at hold")
							case <-timer.C:
							}
						}()
						h.open()
						<-child.done
						child.check(t)
						if childErr != nil || more || corrupt || code != "" || !reflect.DeepEqual(o.seen, o.want) || !reflect.DeepEqual(input, snapshot) || caller.Err() != nil || parentContext.Err() != nil {
							t.Fatal("control must finish successfully before parent retirement/fence")
						}
						if area == "parser" {
							wantMeta := hermesRow{SessionID: "ses_test", CWD: "/planned/project", Title: "Traversal", MessageID: 1, Role: "user", ContentHex: "6f6e65", Timestamp: 1, Total: 4, CursorFound: 1}
							if !reflect.DeepEqual(entries, hermesTraversalPage()) || meta != wantMeta {
								t.Fatal("full chronological four-entry control/oldest metadata")
							}
						} else {
							if !reflect.DeepEqual(entries, hermesTraversalCandidate()) || meta != (hermesRow{}) {
								t.Fatal("full independent three-entry merge candidate")
							}
							for i := range entries {
								entries[i].Tools[0] = ToolActivity{ID: "mutated", associationID: "private-mutated", Output: "mutated", Truncated: true}
								entries[i].ID, entries[i].Text = "mutated", "mutated"
								if !reflect.DeepEqual(input, snapshot) {
									t.Fatal("candidate mutation reached original Entry/Tools")
								}
								want := hermesTraversalCandidate()
								for j := i + 1; j < len(entries); j++ {
									if !reflect.DeepEqual(entries[j], want[j]) {
										t.Fatal("candidate siblings share mutable backing")
									}
								}
							}
						}
						// Full success was captured above; normal context retirement is not rollback.
						retireParent()
						<-parent.done
						f.owner.Join()
						return
					}
					var receipt <-chan struct{}
					switch cause {
					case "caller":
						cancel()
					case "owner":
						receipt = f.owner.Begin()
					case "parent":
						retireParent()
						<-parent.done
						parent.check(t)
					}
					// Observe the actual consumed context before fallback can manufacture it.
					hermesAwaitCancellation(t, h.ctx, child)
					if cause != "caller" && caller.Err() != nil {
						t.Fatal("non-caller cause canceled caller")
					}
					retireParent()
					<-parent.done
					parent.check(t)
					if !callbackReturned || parentErr != nil {
						t.Fatal("parent not actually gone before child/receipt nonreturn")
					}
					if receipt == nil {
						receipt = f.owner.Begin()
					}
					hermesHeldReceipt(t, receipt, child)
					h.open()
					<-child.done
					f.owner.Join()
					<-receipt
					child.check(t)
					hermesTerminalIdentities(t, childErr, context.Canceled, nil, nil)
					if !hermesSQLCancellationTree(childErr, false) || entries != nil || more || corrupt || meta != (hermesRow{}) ||
						(area == "parser" && code != "query_failed") || (area == "attachment" && code != "") || !reflect.DeepEqual(input, snapshot) {
						t.Fatal("canceled traversal published data or extra error identity")
					}
				})
			}
		})
	}
}

// Abnormal traces are independently derived: parser's deferred Err survives
// the exception, but attachment's outer return-expression Err is skipped.
func hermesAbnormalTrace(site string) []string {
	var result []string
	add := func(stage string, n int) {
		for i := 0; i < n; i++ {
			result = append(result, stage)
		}
	}
	if site == "parser" {
		add("parser", 1)
		add("message-rows", 11)
		add("parser", 10)
	} else {
		add("attachment", 18)
		add("tool-rows", 5)
		add("attachment", 14)
	}
	return result
}

func hermesAbnormalScript(site, parserSQL string) []hermesAdmissionStep {
	switch site {
	case "message":
		return hermesFlowQuery(hermesFlowDB, hermesReadSQLOracle, hermesBudgetMessageReply, nil)
	case "tool":
		return append(hermesFlowQuery(hermesFlowDB2, hermesReadSQLOracle, hermesBudgetMessageReply, nil),
			hermesFlowQuery(hermesFlowDB2, hermesToolSingleOracle, hermesBudgetToolReply, nil)...)
	case "parser":
		return hermesFlowQuery(hermesFlowDB, parserSQL, hermesTraversalMessages, nil)
	case "attachment":
		return hermesFlowQuery(hermesFlowDB2, hermesToolSingleOracle, hermesTraversalTools, nil)
	default:
		panic("invalid closed abnormal script")
	}
}

// Fully populated independent RHS-assignment sentinel, including every metadata
// field. Each invocation allocates its own Entry and nested Tools backing.
func hermesAbnormalSentinel() ([]Entry, hermesRow) {
	return hermesTraversalInput(), hermesRow{
		SessionID: "sentinel-session", CWD: "sentinel-cwd", Title: "sentinel-title",
		MessageID: -91, Role: "sentinel-role", ContentHex: "sentinel-hex",
		ToolCallID: "sentinel-call", ToolCalls: "sentinel-calls", ToolName: "sentinel-tool",
		DisplayKind: "sentinel-display", Timestamp: -3.25, Total: -7, CursorFound: -9,
	}
}

func hermesAbnormalRetired(t *testing.T, contexts ...context.Context) {
	t.Helper()
	for _, ctx := range contexts {
		if ctx == nil || ctx.Done() == nil || ctx.Err() != context.Canceled {
			t.Fatal("actual abnormal scope was not retired")
		}
		select {
		case <-ctx.Done():
		default:
			t.Fatal("retired scope Done remains open")
		}
	}
}

func TestInstalledHermesActualAbnormalExit(t *testing.T) {
	for _, mode := range []string{"natural", "fenced"} {
		t.Run(mode, func(t *testing.T) {
			for _, site := range []string{"message", "tool", "parser", "attachment"} {
				t.Run(site, func(t *testing.T) {
					profiles := []string{"panic", "goexit"}
					if site == "message" || site == "tool" {
						profiles = append(profiles, "panic-cleanup", "goexit-cleanup")
					}
					for _, profile := range profiles {
						t.Run(profile, func(t *testing.T) {
							hermesActualAbnormalLeaf(t, mode, site, profile)
						})
					}
				})
			}
		})
	}
}

func hermesActualAbnormalLeaf(t *testing.T, mode, site, profile string) {
	t.Helper()
	const limitOperand = " ORDER BY logical_id DESC LIMIT 3)"
	if strings.Count(hermesReadSQLOracle, limitOperand) != 1 {
		t.Fatal("independent parser LIMIT operand is not unique")
	}
	parserSQL := strings.Replace(hermesReadSQLOracle, limitOperand, " ORDER BY logical_id DESC LIMIT 5)", 1)
	steps, wantSteps := hermesAbnormalScript(site, parserSQL), hermesAbnormalScript(site, parserSQL)
	f := newHermesAdmissionFixture(t, steps)
	h := newHermesLifetimeHold()
	parent, child := newHermesLifetimeWorker(), newHermesLifetimeWorker()
	parentReady, allowReturn, abortStart := make(chan struct{}), make(chan struct{}), make(chan struct{})
	closeStarted := make(chan struct{})
	var returnOnce, abortOnce sync.Once
	retireParent := func() { returnOnce.Do(func() { close(allowReturn) }) }
	abort := func() { abortOnce.Do(func() { close(abortStart) }) }
	caller, cancel := context.WithCancel(context.Background())
	var call, savedQuery *conversationlife.Call
	var parentContext context.Context
	var parentErr error
	var callbackReturned, invocationReturned bool
	entries, meta := hermesAbnormalSentinel()
	wantEntries, wantMeta := hermesAbnormalSentinel()
	more, corrupt, code := true, true, "unassigned-code"
	errorSentinel := errors.New("inert-unassigned-provider-error")
	childErr := errorSentinel
	var input, snapshot []Entry
	if site == "attachment" {
		input, snapshot = hermesTraversalInput(), hermesTraversalInput()
	}
	cleanup := strings.HasSuffix(profile, "-cleanup")
	goexit := strings.HasPrefix(profile, "goexit")
	// Comparable inert value, deliberately unrelated to the cleanup operand.
	token := struct{ Mode, Site, Profile string }{mode, site, profile}
	injections := 0
	inject := func() {
		injections++
		if goexit {
			runtime.Goexit()
		}
		panic(token)
	}
	var slots []*hermesTerminalSlot
	var operands []error
	var deferredClose error
	if cleanup {
		operands = []error{errors.New("inert-abnormal-logical-close-canary")}
	}
	var queries []context.Context
	var replies [][]byte
	observerSite := site == "parser" || site == "attachment"
	var o *hermesTraversalObservation
	originalQuery, originalObserver := f.reader.installed.deps.query, f.reader.observe
	if observerSite {
		o = &hermesTraversalObservation{f: f, h: h, area: site, want: hermesAbnormalTrace(site), counts: make(map[string]int), contexts: make(map[string]context.Context)}
		f.reader.observe = func(ctx context.Context, stage string) {
			// This complete observer validates raw contexts, finite stages, query
			// phases and retired row/query contexts BEFORE publishing the hold.
			o.observe(ctx, stage)
			if h.count == 1 && injections == 0 && f.unknown == 0 {
				inject()
			}
			// Parser's deferred callback returns normally, including natural
			// unwind where its underlying scope can still be live here.
		}
	}
	f.reader.installed.deps.query = func(c *conversationlife.Call, db, sql string, max int) ([]byte, error) {
		raw, err := originalQuery(c, db, sql, max)
		if err != nil || f.unknown != 0 {
			return raw, err
		}
		queries, replies = append(queries, c.Context()), append(replies, raw)
		if observerSite {
			at := 3
			if site == "attachment" {
				at = 21
			}
			if o.queries != 0 || len(o.seen) != at {
				f.deny()
				return nil, conversationlife.ErrRefused
			}
			o.queries++
			o.query, o.raw = c.Context(), raw
			return raw, nil
		}
		selected := hermesReadSQLOracle
		if site == "tool" {
			selected = hermesToolSingleOracle
		}
		if sql != selected {
			return raw, nil
		}
		savedQuery = c
		if cleanup {
			r := &hermesTerminalSlot{operand: operands[0]}
			slot, ownErr := f.owner.Own(func() error {
				r.count++
				close(closeStarted)
				return r.operand
			})
			if ownErr != nil {
				f.deny()
				return nil, ownErr
			}
			defer slot.Close() // Immediate obligation, before any publication/hold.
			r.slot = slot
			slots = append(slots, r)
			defer func() {
				// First real close is on abnormal unwind, not release-side work.
				r.first, r.repeat = slot.Close(), slot.Close()
				deferredClose = slot.Close()
			}()
		}
		h.wait(c.Context())
		inject() // Never consult canceled context to bypass the intended event.
		return raw, nil
	}
	installedQuery, installedObserver := f.reader.installed.deps.query, f.reader.observe
	checkAbnormal := func() {
		if child.returned || invocationReturned || injections != 1 ||
			(goexit && child.panicValue != nil) || (!goexit && child.panicValue != token) {
			t.Fatal("actual child did not preserve the intended abnormal exit")
		}
		if !reflect.DeepEqual(entries, wantEntries) || meta != wantMeta || !more || !corrupt || code != "unassigned-code" || childErr != errorSentinel {
			t.Fatal("interrupted invocation assigned output or error sentinels")
		}
		if !reflect.DeepEqual(input, snapshot) {
			t.Fatal("abnormal attachment modified original Entry or Tools")
		}
		hermesAbnormalRetired(t, h.ctx)
		hermesAbnormalRetired(t, queries...)
		if o != nil {
			for _, ctx := range o.contexts {
				hermesAbnormalRetired(t, ctx)
			}
		}
	}
	staleProbe := func() {
		if observerSite {
			return // Raw observer contexts are never converted to Calls.
		}
		if savedQuery == nil {
			t.Fatal("missing accepted query Call")
		}
		count := 0
		err := f.owner.DoIn(savedQuery, func(*conversationlife.Call) error { count++; return nil })
		if err != conversationlife.ErrRefused || count != 0 {
			t.Fatal("retired query Call retained admission authority")
		}
	}
	// Factory Join was registered first. All gates/completions exist before
	// this cleanup and BOTH sealed launches. No failure can wait on a sole releaser.
	t.Cleanup(func() {
		f.owner.Begin()
		cancel()
		h.open()
		retireParent()
		abort()
		<-parent.done
		<-child.done
		f.owner.Join()
		defer func() {
			f.reader.installed.deps.query = originalQuery
			f.reader.observe = originalObserver
		}()
		parent.check(t)
		if !callbackReturned || parentErr != nil {
			t.Fatal("actual parent callback/caller failed to return normally")
		}
		checkAbnormal()
		staleProbe()
		if reflect.ValueOf(f.reader.installed.deps.query).Pointer() != reflect.ValueOf(installedQuery).Pointer() ||
			!reflect.DeepEqual(steps, wantSteps) || f.next != len(wantSteps) || f.seen != len(wantSteps) || f.unknown != 0 || h.count != 1 || len(replies) != len(wantSteps)/2 {
			t.Fatal("abnormal installed hook/script/hold contract")
		}
		for i, raw := range replies {
			if string(raw) != wantSteps[2*i+1].raw {
				t.Fatal("independent raw query reply changed")
			}
		}
		if !observerSite {
			hermesTerminalInspect(t, f, operands, slots)
			if cleanup && (slots[0].slot == nil || deferredClose != operands[0]) {
				t.Fatal("deferred cached close lost raw identity")
			}
		} else {
			// Dedicated abnormal inspector: never apply the normal-child check,
			// and never erase the observer to satisfy a nil-hook inspector.
			wantCounts := make(map[string]int)
			for _, stage := range o.want {
				wantCounts[stage]++
			}
			if f.reader.observe == nil || reflect.ValueOf(f.reader.observe).Pointer() != reflect.ValueOf(installedObserver).Pointer() ||
				!reflect.DeepEqual(o.seen, o.want) || !reflect.DeepEqual(o.counts, wantCounts) || len(o.contexts) != len(wantCounts) ||
				f.observations != len(o.want) || o.queries != 1 || string(o.raw) != wantSteps[1].raw ||
				f.owner.Witnesses() != nil || len(slots) != 0 || o.contexts[site] == nil || h.ctx.Done() != o.contexts[site].Done() {
				t.Fatal("complete abnormal observer trace/context/witness contract")
			}
		}
		contexts := []context.Context{caller, parentContext}
		contexts = append(contexts, queries...)
		if o != nil {
			for _, ctx := range o.contexts {
				contexts = append(contexts, ctx)
			}
		}
		hermesAbnormalRetired(t, contexts...)
		for i, ctx := range contexts {
			for j := 0; j < i; j++ {
				if ctx.Done() == contexts[j].Done() {
					t.Fatal("actual caller/parent/query/stage contexts aliased")
				}
			}
		}
	})
	go parent.run(func() {
		parentErr = f.owner.Do(caller, func(c *conversationlife.Call) error {
			call, parentContext = c, c.Context()
			close(parentReady)
			<-allowReturn
			callbackReturned = true
			return nil
		})
	})
	go child.run(func() {
		select {
		case <-abortStart:
			return
		case <-parentReady:
		}
		db, limit := hermesFlowDB, 2
		if site == "tool" || site == "attachment" {
			db = hermesFlowDB2
		}
		if site == "parser" {
			limit = 4
		}
		if site == "attachment" {
			entries, corrupt, childErr = f.reader.attachToolResultsIn(call, db, "ses_test", input)
		} else {
			entries, more, corrupt, meta, code, childErr = f.reader.readFromIn(call, db, "ses_test", "", limit)
		}
		invocationReturned = true
	})
	entryTimer := time.NewTimer(time.Second)
	defer entryTimer.Stop()
	select {
	case <-parent.done:
		t.Fatal("actual parent exited before abnormal target")
	case <-child.done:
		t.Fatal("actual child exited before abnormal target")
	case <-entryTimer.C:
		t.Fatal("abnormal target not reached; cleanup still releases and joins")
	case <-h.entered:
	}
	if h.ctx == nil || parentContext == nil || h.ctx.Err() != nil || parentContext.Err() != nil || caller.Err() != nil ||
		h.ctx.Done() == parentContext.Done() || h.ctx.Done() == caller.Done() || !reflect.DeepEqual(input, snapshot) {
		t.Fatal("held raw target/ancestor/input contract")
	}
	var receipt <-chan struct{}
	var parentNonreturn <-chan struct{} = parent.done
	if mode == "fenced" {
		receipt = f.owner.Begin()
		hermesAwaitCancellation(t, h.ctx, child)
		if caller.Err() != nil {
			t.Fatal("owner fence canceled caller")
		}
		retireParent()
		<-parent.done
		parent.check(t)
		if !callbackReturned || parentErr != nil {
			t.Fatal("parent must really return before fenced nonreturn interval")
		}
		parentNonreturn = nil
	}
	func() {
		timer := time.NewTimer(20 * time.Millisecond)
		defer timer.Stop()
		select {
		case <-parentNonreturn:
			t.Fatal("natural parent returned while target held")
		case <-child.done:
			t.Fatal("actual child returned while target held")
		case <-receipt:
			t.Fatal("actual count-zero receipt closed while child held")
		case <-closeStarted:
			t.Fatal("retained logical close started before abnormal unwind")
		case <-timer.C:
		}
	}()
	if mode == "natural" && (caller.Err() != nil || parentContext.Err() != nil || h.ctx.Err() != nil) {
		t.Fatal("natural hold was canceled before release")
	}
	h.open()
	<-child.done
	checkAbnormal()
	if caller.Err() != nil {
		t.Fatal("abnormal child exit must not cancel the independent caller")
	}
	if mode == "natural" {
		// These observations and real stale-authority probe precede ALL fallback
		// fencing/caller cancellation/parent retirement; unwind is the cause.
		if caller.Err() != nil || parentContext.Err() != nil {
			t.Fatal("natural unwind required a live caller and parent")
		}
		staleProbe()
		if caller.Err() != nil || parentContext.Err() != nil {
			t.Fatal("stale probe changed live ancestor state")
		}
		retireParent()
		<-parent.done
		receipt = f.owner.Begin()
	}
	f.owner.Join()
	<-receipt
	parent.check(t)
	staleProbe()
	if cleanup {
		select {
		case <-closeStarted:
		default:
			t.Fatal("abnormal unwind omitted logical close")
		}
	}
}

// Deadline leaves deliberately reject Canceled, including when a real ancestor
// expired. A timeout/bridge composition defect must not relax this oracle.
func hermesExpiryTree(err error, cleanup bool) bool {
	if err == nil {
		return false
	}
	if joined, ok := err.(interface{ Unwrap() []error }); ok {
		children := joined.Unwrap()
		if len(children) == 0 {
			return false
		}
		for _, child := range children {
			if !hermesExpiryTree(child, cleanup) {
				return false
			}
		}
		return true
	}
	return err == context.DeadlineExceeded || (cleanup && err == sqliteexec.ErrCleanupUncertain)
}

// No Begin receipt is obtained here: the parent is deliberately still held.
// This proves actual-child nonreturn only, not independently counted occupancy.
func hermesExpiryWait(t *testing.T, ctx context.Context, deadline time.Time, parent, child *hermesLifetimeWorker) {
	t.Helper()
	timer := time.NewTimer(time.Until(deadline.Add(time.Second)))
	defer timer.Stop()
	select {
	case <-ctx.Done():
	case <-parent.done:
		t.Fatal("parent exited before real timer evidence")
	case <-child.done:
		t.Fatal("child exited before real timer evidence")
	case <-timer.C:
		t.Fatal("deadline plus propagation grace elapsed; cleanup still releases and joins")
	}
	if ctx.Err() != context.DeadlineExceeded {
		t.Fatalf("actual consumed timer identity: %v; want DeadlineExceeded only", ctx.Err())
	}
}

// Complete expiry-aware post-join inspector. The cancellation-only inspectors
// remain frozen; in particular no hook is cleared to satisfy their nil guards.
func hermesExpiryInspect(t *testing.T, f *hermesAdmissionFixture, o *hermesTraversalObservation,
	steps []hermesAdmissionStep, captures []hermesBudgetCapture, replies [][]byte,
	h *hermesLifetimeHold, caller, parentContext context.Context, parent, child *hermesLifetimeWorker,
	input, snapshot []Entry, operands []error, slots []*hermesTerminalSlot, control, local bool) {
	t.Helper()
	if !reflect.DeepEqual(f.steps, steps) || f.next != len(steps) || f.seen != len(steps) || f.unknown != 0 ||
		f.reader.installed.deps.query == nil || len(captures) != len(steps)/2 || len(replies) != len(captures) || h.count != 1 ||
		!reflect.DeepEqual(input, snapshot) {
		t.Fatal("expiry effects/query/raw/hold/input cardinality")
	}
	parent.check(t)
	child.check(t)
	contexts := []context.Context{caller, parentContext}
	for i, capture := range captures {
		if capture.err != nil || !capture.timed || !capture.deadline.After(capture.observed) || string(replies[i]) != steps[2*i+1].raw {
			t.Fatal("original live query capture or saved raw changed")
		}
		contexts = append(contexts, capture.ctx)
	}
	if o == nil {
		if f.reader.observe != nil || f.observations != 0 {
			t.Fatal("SQL-only fixture unexpectedly observed traversal")
		}
	} else {
		if f.reader.observe == nil || o.queries != 1 || o.query == nil || o.query.Done() != captures[0].ctx.Done() ||
			string(o.raw) != steps[1].raw || f.observations != len(o.want) || !reflect.DeepEqual(o.seen, o.want) {
			t.Fatal("expiry exact traversal trace/query phase")
		}
		counts := make(map[string]int)
		for _, stage := range o.want {
			counts[stage]++
		}
		if !reflect.DeepEqual(counts, o.counts) || len(o.contexts) != len(counts) || o.contexts[o.area] == nil || h.ctx.Done() != o.contexts[o.area].Done() {
			t.Fatal("expiry exact stage counts/target context")
		}
		for stage := range counts {
			contexts = append(contexts, o.contexts[stage])
		}
	}
	found := false
	for i, ctx := range contexts {
		if ctx == nil || ctx.Done() == nil {
			t.Fatal("missing actual context")
		}
		switch ctx.(type) {
		case hermesObservationContext, *hermesObservationContext:
			t.Fatal("raw delegated context required")
		}
		select {
		case <-ctx.Done():
		default:
			t.Fatal("joined context still live")
		}
		want := error(context.Canceled)
		if ctx.Done() == h.ctx.Done() {
			found = true
			if !control {
				want = context.DeadlineExceeded
			}
		}
		if !control && !local && i < 2 {
			want = context.DeadlineExceeded
		}
		// Control success already sampled both ancestors LIVE at publication.
		// Their later timer state is not a retroactive success oracle.
		if !(control && i < 2) && ctx.Err() != want {
			t.Fatalf("joined context %d identity=%v want=%v", i, ctx.Err(), want)
		}
		for j := 0; j < i; j++ {
			if ctx.Done() == contexts[j].Done() {
				t.Fatal("actual Done identities alias")
			}
		}
	}
	if !found {
		t.Fatal("held consumed context missing from complete captures")
	}
	witness := f.owner.Witnesses()
	if len(slots) != len(operands) {
		t.Fatal("expiry slot cardinality")
	}
	if len(operands) == 0 {
		if witness != nil {
			t.Fatal("request error poisoned cleanup ledger")
		}
		return
	}
	joined, ok := witness.(interface{ Unwrap() []error })
	if !ok || len(joined.Unwrap()) != 1 || len(slots) != 1 {
		t.Fatal("expiry exact raw witness cardinality")
	}
	r := slots[0]
	if r.slot == nil || r.count != 1 || r.operand != operands[0] || r.first != operands[0] || r.repeat != operands[0] || joined.Unwrap()[0] != operands[0] {
		t.Fatal("expiry first/repeat/deferred cached close or raw identity")
	}
}

func TestInstalledHermesActualTimerExpiry(t *testing.T) {
	for _, name := range []string{
		"caller.message.control", "caller.message.expire", "caller.tool.control", "caller.tool.expire",
		"caller.parser.control", "caller.parser.expire", "caller.attachment.control", "caller.attachment.expire",
		"caller.message.expire-cleanup", "caller.tool.expire-cleanup", "query-local.message.expire", "query-local.tool.expire",
	} {
		t.Run(name, func(t *testing.T) {
			parts := strings.Split(name, ".")
			local, area, control, cleanup := parts[0] == "query-local", parts[1], parts[2] == "control", parts[2] == "expire-cleanup"
			traversal := area == "parser" || area == "attachment"
			db, sql, raw, limit := hermesFlowDB, hermesReadSQLOracle, hermesBudgetMessageReply, 2
			var input, snapshot []Entry
			if area == "tool" {
				db = hermesFlowDB2
			}
			if area == "parser" {
				const operand = " ORDER BY logical_id DESC LIMIT 3)"
				if strings.Count(hermesReadSQLOracle, operand) != 1 {
					t.Fatal("independent LIMIT operand no longer unique")
				}
				sql, raw, limit = strings.Replace(hermesReadSQLOracle, operand, " ORDER BY logical_id DESC LIMIT 5)", 1), hermesTraversalMessages, 4
			}
			if area == "attachment" {
				db, sql, raw = hermesFlowDB2, hermesToolSingleOracle, hermesTraversalTools
				input, snapshot = hermesTraversalInput(), hermesTraversalInput()
			}
			steps, wantSteps := hermesFlowQuery(db, sql, raw, nil), hermesFlowQuery(db, sql, raw, nil)
			if area == "tool" || (area == "message" && control) {
				steps = append(steps, hermesFlowQuery(db, hermesToolSingleOracle, hermesBudgetToolReply, nil)...)
				wantSteps = append(wantSteps, hermesFlowQuery(db, hermesToolSingleOracle, hermesBudgetToolReply, nil)...)
			}
			f := newHermesAdmissionFixture(t, steps)
			h := newHermesLifetimeHold()
			parent, child := newHermesLifetimeWorker(), newHermesLifetimeWorker()
			parentReady, allowReturn, abortStart, closeStarted := make(chan struct{}), make(chan struct{}), make(chan struct{}), make(chan struct{})
			var returnOnce, abortOnce sync.Once
			retireParent := func() { returnOnce.Do(func() { close(allowReturn) }) }
			abort := func() { abortOnce.Do(func() { close(abortStart) }) }
			var call *conversationlife.Call
			var parentContext context.Context
			var parentErr, childErr error
			var entries []Entry
			var more, corrupt, callbackReturned bool
			var meta hermesRow
			var code string
			var captures []hermesBudgetCapture
			var replies [][]byte
			var slots []*hermesTerminalSlot
			var operands []error
			if cleanup {
				operands = []error{errors.New("inert-hermes-expiry-raw-close-canary")}
			}
			var o *hermesTraversalObservation
			originalQuery, originalObserver := f.reader.installed.deps.query, f.reader.observe
			if traversal {
				o = &hermesTraversalObservation{f: f, h: h, area: area, want: hermesTraversalTrace(area, control), counts: make(map[string]int), contexts: make(map[string]context.Context)}
				f.reader.observe = o.observe
			}
			f.reader.installed.deps.query = func(c *conversationlife.Call, database, query string, max int) ([]byte, error) {
				data, err := originalQuery(c, database, query, max)
				if err != nil || f.unknown != 0 {
					return data, err
				}
				if len(captures) >= len(wantSteps)/2 {
					f.deny()
					return nil, conversationlife.ErrRefused
				}
				captures = append(captures, captureHermesBudget(c))
				replies = append(replies, data)
				if traversal {
					at := 3
					if area == "attachment" {
						at = 21
					}
					if o.queries != 0 || len(o.seen) != at {
						f.deny()
						return nil, conversationlife.ErrRefused
					}
					o.queries++
					o.query, o.raw = c.Context(), data
					return data, nil
				}
				selected := area == "message" && query == hermesReadSQLOracle || area == "tool" && query == hermesToolSingleOracle
				if !selected {
					return data, nil
				}
				var retained *hermesTerminalSlot
				if cleanup {
					retained = &hermesTerminalSlot{operand: operands[0]}
					slot, ownErr := f.owner.Own(func() error {
						close(closeStarted)
						retained.count++
						return retained.operand
					})
					if ownErr != nil {
						f.deny()
						return nil, ownErr
					}
					defer slot.Close()
					retained.slot = slot
					slots = append(slots, retained)
				}
				h.wait(c.Context())
				if retained != nil {
					retained.first = retained.slot.Close()
					retained.repeat = retained.slot.Close()
					return data, sqliteexec.ErrCleanupUncertain
				}
				return data, nil // Real context, never a fake DeadlineExceeded marker.
			}
			// All data/hooks/gates are prepared BEFORE starting the generous budget.
			var caller context.Context
			var cancel context.CancelFunc
			var callerDeadline time.Time
			if local {
				caller, cancel = context.WithCancel(context.Background())
			} else {
				callerDeadline = time.Now().Add(2 * time.Second)
				caller, cancel = context.WithDeadline(context.Background(), callerDeadline)
			}
			t.Cleanup(cancel)
			t.Cleanup(func() {
				f.owner.Begin()
				cancel()
				h.open()
				retireParent()
				abort()
				<-parent.done
				<-child.done
				f.owner.Join()
				defer func() {
					f.reader.installed.deps.query = originalQuery
					f.reader.observe = originalObserver
				}()
				hermesExpiryInspect(t, f, o, wantSteps, captures, replies, h, caller, parentContext, parent, child, input, snapshot, operands, slots, control, local)
				if parentErr != nil || !callbackReturned {
					t.Fatal("actual parent callback/caller did not return normally")
				}
			})
			beforeInvocation := time.Now()
			go parent.run(func() {
				parentErr = f.owner.Do(caller, func(c *conversationlife.Call) error {
					call, parentContext = c, c.Context()
					close(parentReady)
					<-allowReturn
					callbackReturned = true
					return nil
				})
			})
			go child.run(func() {
				select {
				case <-abortStart:
					return
				case <-parentReady:
				}
				if area == "attachment" {
					entries, corrupt, childErr = f.reader.attachToolResultsIn(call, db, "ses_test", input)
				} else {
					entries, more, corrupt, meta, code, childErr = f.reader.readFromIn(call, db, "ses_test", "", limit)
				}
			})
			entryTimer := time.NewTimer(time.Second)
			defer entryTimer.Stop()
			select {
			case <-parent.done:
				t.Fatal("actual parent exited before target")
			case <-child.done:
				t.Fatal("actual child exited before target")
			case <-entryTimer.C:
				t.Fatal("target not entered within generous entry guard")
			case <-h.entered:
			}
			entryTimer.Stop()
			if h.ctx == nil || parentContext == nil || h.ctx.Err() != nil || parentContext.Err() != nil || caller.Err() != nil ||
				h.ctx.Done() == caller.Done() || h.ctx.Done() == parentContext.Done() || caller.Done() == parentContext.Done() || !reflect.DeepEqual(input, snapshot) {
				t.Fatal("live independent consumed target/caller/parent and held input required")
			}
			deadline, timed := h.ctx.Deadline()
			if !timed || !deadline.After(time.Now()) {
				t.Fatal("held actual target lacks a real future deadline")
			}
			if local {
				_, callerTimed := caller.Deadline()
				_, parentTimed := parentContext.Deadline()
				last := captures[len(captures)-1]
				if callerTimed || parentTimed || deadline.Before(beforeInvocation.Add(10*time.Second)) || deadline.After(last.observed.Add(10*time.Second)) || !deadline.Equal(last.deadline) {
					t.Fatal("query-local timer must be original independent 10s with untimed ancestors")
				}
			} else if !deadline.Equal(callerDeadline) || time.Until(callerDeadline) < time.Second {
				t.Fatal("held deadline must equal explicit earlier caller deadline with sufficient entry budget")
			}
			if area == "tool" {
				if len(captures) != 2 || captures[0].ctx.Err() != context.Canceled || captures[0].ctx.Done() == h.ctx.Done() {
					t.Fatal("first message query must already be retired and distinct")
				}
				select {
				case <-captures[0].ctx.Done():
				default:
					t.Fatal("first message query Done remains open")
				}
			}
			if !control {
				if !local {
					hermesExpiryWait(t, caller, callerDeadline, parent, child)
				}
				hermesExpiryWait(t, h.ctx, deadline, parent, child)
				if local && (caller.Err() != nil || parentContext.Err() != nil) {
					t.Fatal("query-local expiry contaminated live ancestors")
				}
			}
			func() {
				timer := time.NewTimer(20 * time.Millisecond)
				defer timer.Stop()
				select {
				case <-child.done:
					t.Fatal("actual child returned while consumed work held")
				case <-parent.done:
					t.Fatal("parent returned before semantic capture")
				case <-closeStarted:
					t.Fatal("logical slot closed before release")
				case <-timer.C:
				}
			}()
			h.open()
			<-child.done
			child.check(t)
			// FULL semantic capture before parent return, Begin, Join or cancellation.
			if (control || local) && (caller.Err() != nil || parentContext.Err() != nil) {
				t.Fatal("result capture required still-live caller and parent")
			}
			if !reflect.DeepEqual(input, snapshot) {
				t.Fatal("result publication changed original Entry or nested Tools")
			}
			if control {
				if childErr != nil || more || corrupt || code != "" {
					t.Fatal("control full successful tuple required before fallback")
				}
				want, wantMeta := hermesTerminalPage("ok"), hermesTerminalMetadata()
				if area == "parser" {
					want = hermesTraversalPage()
					wantMeta = hermesRow{SessionID: "ses_test", CWD: "/planned/project", Title: "Traversal", MessageID: 1, Role: "user", ContentHex: "6f6e65", Timestamp: 1, Total: 4, CursorFound: 1}
				} else if area == "attachment" {
					want, wantMeta = hermesTraversalCandidate(), hermesRow{}
				}
				if !reflect.DeepEqual(entries, want) || meta != wantMeta {
					t.Fatal("complete independently literal control page/candidate/metadata")
				}
				if area == "attachment" {
					for i := range entries {
						entries[i].ID, entries[i].Text = "mutated", "mutated"
						entries[i].Tools[0] = ToolActivity{ID: "mutated", associationID: "mutated", Output: "mutated", Error: true, Truncated: true}
						if !reflect.DeepEqual(input, snapshot) {
							t.Fatal("candidate aliases caller input")
						}
						for j := i + 1; j < len(entries); j++ {
							if !reflect.DeepEqual(entries[j], want[j]) {
								t.Fatal("candidate siblings alias")
							}
						}
					}
				}
			} else {
				wantErr := error(context.DeadlineExceeded)
				if cleanup {
					wantErr = errors.Join(wantErr, sqliteexec.ErrCleanupUncertain)
				}
				hermesTerminalIdentities(t, childErr, wantErr, nil, operands)
				wantCode := "query_failed"
				if area == "attachment" {
					wantCode = ""
				}
				if !hermesExpiryTree(childErr, cleanup) || entries != nil || more || corrupt || meta != (hermesRow{}) || code != wantCode || h.ctx.Err() != context.DeadlineExceeded {
					t.Fatal("real expiry must suppress whole result with DeadlineExceeded-only tree")
				}
				if cleanup {
					select {
					case <-closeStarted:
					default:
						t.Fatal("missing retained logical close")
					}
				}
			}
			// Capture normal private retirement now, not after fallback can create it.
			if o != nil {
				if !reflect.DeepEqual(o.seen, o.want) {
					t.Fatal("pre-fallback exact finite traversal trace")
				}
				for _, ctx := range o.contexts {
					want := error(context.Canceled)
					if !control && ctx.Done() == h.ctx.Done() {
						want = context.DeadlineExceeded
					}
					if ctx.Err() != want {
						t.Fatal("pre-fallback actual stage retirement identity")
					}
					select {
					case <-ctx.Done():
					default:
						t.Fatal("pre-fallback stage not retired")
					}
				}
			}
			for _, capture := range captures {
				if !local && !capture.deadline.Equal(callerDeadline) {
					t.Fatal("query lost exact earlier caller deadline")
				}
				if local && (capture.deadline.Before(beforeInvocation.Add(10*time.Second)) || capture.deadline.After(capture.observed.Add(10*time.Second))) {
					t.Fatal("independent original query timer bounds")
				}
				want := error(context.Canceled)
				if !control && capture.ctx.Done() == h.ctx.Done() {
					want = context.DeadlineExceeded
				}
				if capture.ctx.Err() != want {
					t.Fatal("pre-fallback query retirement identity")
				}
			}
			retireParent()
			<-parent.done
			parent.check(t)
			receipt := f.owner.Begin()
			f.owner.Join()
			<-receipt
		})
	}
}

// Semantic wire keys are test-owned, never marshaled from hermesRow tags.
// Expected values and preservation copies never borrow returned Entry/Tools storage.
type hermesSemanticCase struct {
	name, content, calls, text, timestamp string
	tools                                 []ToolActivity
	seconds                               float64
	id                                    int64
	role                                  string
	drop, corrupt, badHex                 bool
	typedField                            string
}

func hermesSemanticTool(id, name, input, output string) []ToolActivity {
	return []ToolActivity{{ID: id, associationID: id, Name: name, Input: input, Output: output, Error: false, Truncated: false}}
}

func hermesSemanticExpected(p hermesSemanticCase) []Entry {
	if p.typedField != "" {
		return nil
	}
	if p.drop {
		return []Entry{}
	}
	var tools []ToolActivity
	if p.tools != nil {
		tools = make([]ToolActivity, len(p.tools))
		copy(tools, p.tools)
	}
	return []Entry{{ID: "7", Timestamp: p.timestamp, Role: "assistant", Text: p.text, Tools: tools, Truncated: false}}
}

func hermesSemanticRun(t *testing.T, p hermesSemanticCase) {
	t.Helper()
	encoded := hex.EncodeToString([]byte(p.content))
	if p.badHex {
		encoded = "zz"
	}
	wire := map[string]any{
		"session_id": "ses_test", "cwd": "/planned/project", "title": "Semantic",
		"message_id": p.id, "role": p.role, "content_hex": encoded,
		"tool_call_id": "row-only", "tool_calls": p.calls, "tool_name": "row-name",
		"display_kind": "normal", "timestamp": p.seconds, "message_total": 1, "cursor_found": 1,
	}
	if p.typedField == "timestamp" {
		wire["timestamp"] = "not-a-number"
	}
	if p.typedField == "tool_calls" {
		wire["tool_calls"] = false
	}
	raw, marshalErr := json.Marshal([]map[string]any{wire})
	if marshalErr != nil {
		t.Fatal(marshalErr)
	}
	steps := hermesFlowQuery(hermesFlowDB2, hermesReadSQLOracle, string(raw), nil)
	associated := false
	for _, tool := range p.tools {
		if tool.associationID != "" {
			associated = true
		}
	}
	if associated {
		steps = append(steps, hermesFlowQuery(hermesFlowDB2, hermesToolSingleOracle,
			`[{"tool_call_id":"call_b","tool_name":"ignored-error-name","content_hex":"646f6e65"}]`, nil)...)
	}
	// Separate script backing; these ready/query steps have nil paths/info.
	plan := make([]hermesAdmissionStep, len(steps))
	copy(plan, steps)
	f := newHermesAdmissionFixture(t, steps)
	want, preserved := hermesSemanticExpected(p), hermesSemanticExpected(p)
	wantMeta := hermesRow{SessionID: "ses_test", CWD: "/planned/project", Title: "Semantic",
		MessageID: p.id, Role: p.role, ContentHex: encoded, ToolCallID: "row-only", ToolCalls: p.calls,
		ToolName: "row-name", DisplayKind: "normal", Timestamp: p.seconds, Total: 1, CursorFound: 1}
	if p.typedField != "" {
		wantMeta = hermesRow{}
	}
	var entries []Entry
	var more, corrupt bool
	var meta hermesRow
	var code string
	err := f.owner.Do(context.Background(), func(c *conversationlife.Call) error {
		var e error
		entries, more, corrupt, meta, code, e = f.reader.readFromIn(c, hermesFlowDB2, "ses_test", "", 2)
		return e
	})
	f.check(t)
	if f.reader.observe != nil || !reflect.DeepEqual(f.steps, plan) {
		t.Fatal("semantic observer or complete script changed")
	}
	wantCode := ""
	if p.typedField == "" {
		if err != nil {
			t.Fatalf("semantic success error: %v", err)
		}
	} else {
		wantCode = "source_corrupt"
		var typed *json.UnmarshalTypeError
		wantType, wantValue := reflect.TypeOf(float64(0)), "string"
		if p.typedField == "tool_calls" {
			wantType, wantValue = reflect.TypeOf(""), "bool"
		}
		if !errors.As(err, &typed) || typed.Field != "0."+p.typedField || typed.Struct != "" || typed.Value != wantValue || typed.Type != wantType {
			t.Fatalf("actual decoder type identity: %v", err)
		}
		for _, forbidden := range []error{context.Canceled, context.DeadlineExceeded, conversationlife.ErrRefused, sqliteexec.ErrCleanupUncertain, ErrSourceCleanupUncertain, sqliteexec.ErrOutputLimit} {
			if errors.Is(err, forbidden) {
				t.Fatal("unexpected semantic marker", err)
			}
		}
	}
	if code != wantCode || more || corrupt != p.corrupt || meta != wantMeta || !reflect.DeepEqual(entries, want) {
		t.Fatalf("semantic tuple: code=%q more=%v corrupt=%v meta=%+v entries=%+v", code, more, corrupt, meta, entries)
	}
	if len(entries) != 0 {
		entries[0].Text = "mutated-returned-text"
		if len(entries[0].Tools) != 0 {
			entries[0].Tools[0].ID = "mutated-id"
			entries[0].Tools[0].associationID = "mutated-association"
			entries[0].Tools[0].Input = "mutated-input"
			entries[0].Tools[0].Output = "mutated-output"
		}
		if reflect.DeepEqual(entries, want) || !reflect.DeepEqual(want, preserved) || !reflect.DeepEqual(p.tools, preserved[0].Tools) {
			t.Fatal("returned nested storage aliases independent value oracle")
		}
	}
}

func TestInstalledHermesToolSemanticValues(t *testing.T) {
	for _, p := range []hermesSemanticCase{
		{name: "array-call", calls: `[{"id":" call_b ","name":" shell ","arguments":" pwd "}]`, tools: hermesSemanticTool("call_b", "shell", "pwd", "done")},
		{name: "singleton-call", calls: `{"id":"call_b","name":"inspect","input":"ls -a"}`, tools: hermesSemanticTool("call_b", "inspect", "ls -a", "done")},
		{name: "wrapped-array", calls: `{"id":"ignored","name":"ignored","tool_calls":[{"id":"call_b","name":"wrapped","input":"inside"}]}`, tools: hermesSemanticTool("call_b", "wrapped", "inside", "done")},
		{name: "empty-array", calls: `[]`, tools: []ToolActivity{}},
		{name: "empty-wrapper", calls: `{"id":"ignored","tool_calls":[]}`, tools: []ToolActivity{}},
		{name: "null-tools", calls: `null`},
		{name: "blank-tools", calls: " \t\n "},
		{name: "scalar-tools", calls: `42`, corrupt: true},
		{name: "mixed-call-items", calls: `[{"id":"call_b","name":"mixed","input":"keep"},false,null,5,"skip",[]]`, tools: hermesSemanticTool("call_b", "mixed", "keep", "done"), corrupt: true},
		{name: "function-fallback", calls: `{"function":{"tool_call_id":"call_b","tool_name":"nested","arguments":"nested-input"}}`, tools: hermesSemanticTool("call_b", "nested", "nested-input", "done")},
		{name: "outer-precedence", calls: `{"id":"call_b","call_id":"wrong1","tool_call_id":"wrong2","name":"outer","tool_name":"wrong3","arguments":"winner","input":"wrong4","error":true,"is_error":true,"function":{"id":"wrong5","name":"wrong6","arguments":"wrong7"}}`, tools: hermesSemanticTool("call_b", "outer", "winner", "done")},
		{name: "alias-fields", calls: `{"call_id":"call_b","tool_call_id":"wrong","tool_name":"alias","input":"alias-input"}`, tools: hermesSemanticTool("call_b", "alias", "alias-input", "done")},
		{name: "missing-fields", calls: `{"unknown":1,"tool_calls":false,"function":[]}`, tools: hermesSemanticTool("", "Tool", "", "")},
		{name: "blank-name", calls: `{"id":"call_b","name":" \t ","function":{"name":"blocked"}}`, tools: hermesSemanticTool("call_b", "Tool", "", "done")},
		{name: "whitespace-id-precedence", calls: `{"id":" \t ","call_id":"call_b","tool_call_id":"call_b","name":"no-association","function":{"id":"call_b"}}`, tools: hermesSemanticTool("", "no-association", "", "")},
		{name: "input-empty-string-precedence", calls: `{"id":"call_b","name":"empty","arguments":"","input":"blocked","function":{"arguments":"blocked"}}`, tools: hermesSemanticTool("call_b", "empty", "", "done")},
		{name: "input-number", calls: `{"id":"call_b","name":"number","arguments":12.5}`, tools: hermesSemanticTool("call_b", "number", "12.5", "done")},
		{name: "input-boolean", calls: `{"id":"call_b","name":"boolean","arguments":false,"input":"blocked"}`, tools: hermesSemanticTool("call_b", "boolean", "false", "done")},
		{name: "input-object", calls: `{"id":"call_b","name":"object","input":{"z":2,"a":{"y":false,"b":1}}}`, tools: hermesSemanticTool("call_b", "object", `{"a":{"b":1,"y":false},"z":2}`, "done")},
		{name: "input-text-blocks", calls: `{"id":"call_b","name":"blocks","input":[{"type":"text","text":" \u001b[31mone\u001b[0m\r"},{"type":"input_text","text":"two\u0000"},{"type":"output_text","text":"three "},{"type":"image","text":"ignored"},"skip",{"type":"text","text":4}]}`, tools: hermesSemanticTool("call_b", "blocks", "one\ntwo\nthree", "done")},
		{name: "input-no-text-array", calls: `{"id":"call_b","name":"array","input":["x",3,{"type":"unknown","text":"ignored"},{"type":"text","text":""}]}`, tools: hermesSemanticTool("call_b", "array", `["x",3,{"text":"ignored","type":"unknown"},{"text":"","type":"text"}]`, "done")},
		{name: "input-nil", calls: `{"id":"call_b","name":"nil","arguments":null,"function":{"input":null}}`, tools: hermesSemanticTool("call_b", "nil", "", "done")},
	} {
		t.Run(p.name, func(t *testing.T) {
			p.content, p.text, p.id, p.role = "visible", "visible", 7, "assistant"
			p.seconds, p.timestamp = 2.5, "1970-01-01T00:00:02.5Z"
			hermesSemanticRun(t, p)
		})
	}
}

func TestInstalledHermesMessageSemanticValues(t *testing.T) {
	for _, p := range []hermesSemanticCase{
		{name: "json-string", content: "\x00json:\" \\u001b[31mhello\\u001b[0m\\r\\u0000 \"", text: "hello"},
		{name: "json-number", content: "\x00json:12.5", text: "12.5"},
		{name: "json-object", content: "\x00json:{\"z\":2,\"a\":{\"y\":false,\"b\":1}}", text: `{"a":{"b":1,"y":false},"z":2}`},
		{name: "json-null", content: "\x00json:null", drop: true},
		{name: "json-text-blocks", content: "\x00json:" + `[{"type":"text","text":" \u001b[31mone\u001b[0m\r"},{"type":"input_text","text":"two\u0000"},{"type":"output_text","text":"three "},{"type":"image","text":"ignored"},7,{"type":"text","text":false}]`, text: "one\ntwo\nthree"},
		{name: "json-no-text-array", content: "\x00json:" + `["x",3,{"type":"image","text":"ignored"}]`, text: `["x",3,{"text":"ignored","type":"image"}]`},
		{name: "json-empty-text-array", content: "\x00json:" + `[{"type":"text","text":""},{"type":"output_text","text":""}]`, text: `[{"text":"","type":"text"},{"text":"","type":"output_text"}]`},
		{name: "malformed-json-prefix", content: "\x00json:{bad\r\x00 ", text: "json:{bad"},
		{name: "sanitize-controls", content: " \t\x1b[31mred\x1b[0m\r\x00\nnext \t\n", text: "red\nnext"},
		{name: "empty-with-tool", content: " \r\n\x00", calls: `[{"id":"call_b","name":"shell","input":"pwd"}]`, tools: hermesSemanticTool("call_b", "shell", "pwd", "done")},
		{name: "empty-no-tool", content: " \r\n\x00", drop: true},
		{name: "bad-hex-no-tool", badHex: true, corrupt: true, drop: true},
		{name: "skip-role", role: "tool", badHex: true, calls: "{", drop: true},
		{name: "skip-id", id: -1, badHex: true, calls: "{", drop: true},
		{name: "time-zero", content: "visible", text: "visible"},
		{name: "time-negative", content: "visible", text: "visible", seconds: -2.5},
		{name: "time-fraction", content: "visible", text: "visible", seconds: 1.25, timestamp: "1970-01-01T00:00:01.25Z"},
		{name: "time-subnanosecond", content: "visible", text: "visible", seconds: 0.0000000005, timestamp: "1970-01-01T00:00:00Z"},
		{name: "typed-timestamp-error", content: "visible", typedField: "timestamp"},
		{name: "typed-tools-error", content: "visible", typedField: "tool_calls"},
	} {
		t.Run(p.name, func(t *testing.T) {
			if p.id == 0 {
				p.id = 7
			}
			if p.role == "" {
				p.role = "assistant"
			}
			hermesSemanticRun(t, p)
		})
	}
}

// Finite provider-layer bounds. Expected values never pass through Reader
// normalization, provider SQL builders, parsers or clamp helpers.
func hermesBoundsJSON(t *testing.T, value any) string {
	t.Helper()
	raw, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

func hermesBoundsSQL(ids ...string) string {
	operands := make([]string, len(ids))
	for i, id := range ids {
		operands[i] = "CAST(X'" + hex.EncodeToString([]byte(id)) + "' AS TEXT)"
	}
	// Callers explicitly supply the independent sorted full-ID set.
	return `SELECT COALESCE(m.tool_call_id,'') AS tool_call_id,COALESCE(m.tool_name,'') AS tool_name,hex(COALESCE(m.content,'')) AS content_hex FROM messages AS m WHERE m.session_id=CAST(X'7365735f74657374' AS TEXT) AND m.role='tool' AND (m.active=1 OR m.compacted=1) AND COALESCE(m.display_kind,'') <> 'hidden' AND m.tool_call_id IN (` + strings.Join(operands, ",") + `) ORDER BY m.id;`
}

func hermesBoundsRow(id, output string) map[string]any {
	return map[string]any{"tool_call_id": id, "tool_name": "ignored-error-name", "content_hex": hex.EncodeToString([]byte(output)), "error": true, "is_error": true}
}

func hermesBoundsCopy(entries []Entry) []Entry {
	if entries == nil {
		return nil
	}
	out := make([]Entry, len(entries))
	for i, entry := range entries {
		out[i] = entry
		if entry.Tools != nil {
			out[i].Tools = make([]ToolActivity, len(entry.Tools))
			copy(out[i].Tools, entry.Tools)
		}
	}
	return out
}

func hermesBoundsCheck(t *testing.T, f *hermesAdmissionFixture, plan []hermesAdmissionStep) {
	t.Helper()
	f.check(t)
	if f.reader.observe != nil || !reflect.DeepEqual(f.steps, plan) {
		t.Fatal("bounds observer/script changed")
	}
}

func hermesBoundsRead(t *testing.T, content, calls, sql, toolRaw string, expected []Entry) {
	t.Helper()
	encoded := hex.EncodeToString([]byte(content))
	raw := hermesBoundsJSON(t, []map[string]any{{
		"session_id": "ses_test", "cwd": "/planned/project", "title": "Bounds",
		"message_id": 7, "role": "assistant", "content_hex": encoded,
		"tool_call_id": "row-only", "tool_calls": calls, "tool_name": "row-name",
		"display_kind": "normal", "timestamp": 2.5, "message_total": 1, "cursor_found": 1,
	}})
	steps := hermesFlowQuery(hermesFlowDB2, hermesReadSQLOracle, raw, nil)
	if sql != "" {
		steps = append(steps, hermesFlowQuery(hermesFlowDB2, sql, toolRaw, nil)...)
	}
	plan := append([]hermesAdmissionStep(nil), steps...)
	want, preserved := hermesBoundsCopy(expected), hermesBoundsCopy(expected)
	metaWant := hermesRow{SessionID: "ses_test", CWD: "/planned/project", Title: "Bounds", MessageID: 7,
		Role: "assistant", ContentHex: encoded, ToolCallID: "row-only", ToolCalls: calls, ToolName: "row-name",
		DisplayKind: "normal", Timestamp: 2.5, Total: 1, CursorFound: 1}
	f := newHermesAdmissionFixture(t, steps)
	var got []Entry
	var more, corrupt bool
	var meta hermesRow
	var code string
	err := f.owner.Do(context.Background(), func(c *conversationlife.Call) error {
		var e error
		got, more, corrupt, meta, code, e = f.reader.readFromIn(c, hermesFlowDB2, "ses_test", "", 2)
		return e
	})
	hermesBoundsCheck(t, f, plan)
	if err != nil || code != "" || more || corrupt || meta != metaWant || !reflect.DeepEqual(got, want) {
		t.Fatalf("complete bounds page mismatch: err=%v code=%q more=%v corrupt=%v", err, code, more, corrupt)
	}
	got[0].Text = "mutated-entry"
	if len(got[0].Tools) > 0 {
		got[0].Tools[0] = ToolActivity{ID: "mutated-tool"}
		if !reflect.DeepEqual(got[0].Tools[1:], preserved[0].Tools[1:]) {
			t.Fatal("sibling tools changed")
		}
	}
	if !reflect.DeepEqual(want, preserved) || !reflect.DeepEqual(expected, preserved) {
		t.Fatal("expected backing aliased result")
	}
}

func TestInstalledHermesFieldByteBounds(t *testing.T) {
	for _, field := range []struct {
		name  string
		limit int
	}{
		{"text", 131072}, {"id", 256}, {"name", 160}, {"input", 65536}, {"output", 131072},
	} {
		for _, form := range []string{"ascii", "utf8"} {
			for _, edge := range []struct {
				name  string
				delta int
			}{{"under", -1}, {"exact", 0}, {"over", 1}} {
				t.Run(field.name+"."+form+"."+edge.name, func(t *testing.T) {
					n := field.limit + edge.delta
					value, want := strings.Repeat("x", n), strings.Repeat("x", n)
					if edge.delta == 1 {
						want = strings.Repeat("x", field.limit)
					}
					if form == "utf8" {
						value = strings.Repeat("x", n-3) + "€"
						want = strings.Repeat("x", n-3) + "€"
						if edge.delta == 1 {
							want = strings.Repeat("x", field.limit-2)
						}
					}
					hermesBoundsField(t, field.name, value, want, edge.delta == 1)
				})
			}
		}
	}
	for _, field := range []string{"text", "output"} {
		for _, edge := range []string{"under", "over"} {
			t.Run(field+".invalid-utf8."+edge, func(t *testing.T) {
				value, want := strings.Repeat("x", 131070)+"\xff", strings.Repeat("x", 131070)+"\xff"
				if edge == "over" {
					value = strings.Repeat("x", 131070) + "\xffyz"
					want = strings.Repeat("x", 131070)
				}
				hermesBoundsField(t, field, value, want, edge == "over")
			})
		}
	}
}

func hermesBoundsField(t *testing.T, field, value, want string, clipped bool) {
	t.Helper()
	entry := Entry{ID: "7", Timestamp: "1970-01-01T00:00:02.5Z", Role: "assistant", Text: "visible"}
	if field == "text" {
		entry.Text, entry.Truncated = want, clipped
		hermesBoundsRead(t, value, "", "", "", []Entry{entry})
		return
	}
	id, name, input := "call_b", "shell", "pwd"
	tool := ToolActivity{ID: "call_b", associationID: "call_b", Name: "shell", Input: "pwd", Truncated: clipped}
	switch field {
	case "id":
		id = value
		tool.ID, tool.associationID = want, value
	case "name":
		name = value
		tool.Name = want
	case "input":
		input = value
		tool.Input = want
	case "output":
		tool.Output = want
	}
	calls := hermesBoundsJSON(t, []map[string]any{{"id": " " + id + " ", "name": name, "input": input}})
	toolRaw := "[]"
	if field == "output" {
		toolRaw = hermesBoundsJSON(t, []map[string]any{hermesBoundsRow("call_b", value)})
	}
	entry.Tools = []ToolActivity{tool}
	hermesBoundsRead(t, "visible", calls, hermesBoundsSQL(id), toolRaw, []Entry{entry})
}

func TestInstalledHermesAssociationContracts(t *testing.T) {
	for _, name := range []string{"full-id-collision", "display-only-row"} {
		t.Run(name, func(t *testing.T) {
			prefix := strings.Repeat("i", 256)
			a, b := prefix+"A", prefix+"B"
			calls := hermesBoundsJSON(t, []map[string]any{{"id": " " + b + " ", "name": "B", "input": "input-B"}, {"id": a, "name": "A", "input": "input-A"}})
			tools := []ToolActivity{{ID: prefix, associationID: b, Name: "B", Input: "input-B", Output: "result-B"}, {ID: prefix, associationID: a, Name: "A", Input: "input-A", Output: "result-A"}}
			rows := []map[string]any{hermesBoundsRow(a, "result-A"), hermesBoundsRow(b, "result-B")}
			if name == "display-only-row" {
				rows = []map[string]any{hermesBoundsRow(prefix, "must-not-attach")}
				for i := range tools {
					tools[i].Output = ""
					tools[i].Truncated = true
				}
			}
			hermesBoundsRead(t, "visible", calls, hermesBoundsSQL(a, b), hermesBoundsJSON(t, rows), []Entry{{ID: "7", Timestamp: "1970-01-01T00:00:02.5Z", Role: "assistant", Text: "visible", Tools: tools}})
		})
	}
	for _, p := range []struct {
		name  string
		count int
	}{{"tool-count-under", 127}, {"tool-count-exact", 128}, {"tool-count-over", 129}} {
		t.Run(p.name, func(t *testing.T) {
			calls := make([]map[string]any, p.count)
			tools := make([]ToolActivity, p.count)
			for i := range calls {
				calls[i] = map[string]any{"name": fmt.Sprintf("tool-%03d", i), "input": fmt.Sprintf("input-%03d", i)}
				tools[i] = ToolActivity{Name: fmt.Sprintf("tool-%03d", i), Input: fmt.Sprintf("input-%03d", i)}
			}
			hermesBoundsRead(t, "visible", hermesBoundsJSON(t, calls), "", "", []Entry{{ID: "7", Timestamp: "1970-01-01T00:00:02.5Z", Role: "assistant", Text: "visible", Tools: tools}})
		})
	}
}

func hermesBoundsMergeEntries(output string, failed, truncated bool) []Entry {
	return []Entry{
		{ID: "first", Timestamp: "1970-01-01T00:00:01Z", Role: "assistant", Text: "keep-first", Truncated: true,
			Tools: []ToolActivity{{ID: "display-b", associationID: "call_b", Name: "shell", Input: "pwd", Output: output, Error: failed, Truncated: truncated},
				{ID: "display-a", associationID: "call_a", Name: "read", Input: "file", Output: "sibling", Error: true, Truncated: true}}},
		{ID: "second", Timestamp: "1970-01-01T00:00:02Z", Role: "user", Text: "keep-second",
			Tools: []ToolActivity{{Name: "unassociated", Input: "keep", Output: "untouched"}}},
	}
}

func TestInstalledHermesOutputMergeValues(t *testing.T) {
	for _, name := range []string{"blank-output", "equal-output", "append-distinct", "repeat-nonadjacent", "initial-assignment-clears", "aggregate-overflow", "aggregate-exact", "aggregate-utf8", "unmatched-association", "malformed-output-with-valid-sibling", "error-text-no-inference", "preserve-error", "output-json-blocks", "output-json-null", "output-malformed-json-prefix"} {
		t.Run(name, func(t *testing.T) {
			before, after := "old", "old"
			initialFlag, finalFlag, failed, damaged := true, true, false, false
			outputs := []string{"new"}
			rowID := "call_b"
			switch name {
			case "blank-output":
				outputs = []string{" \r\n\x00 "}
			case "equal-output":
				outputs = []string{"old"}
			case "append-distinct":
				after, finalFlag = "old\nnew", false
			case "repeat-nonadjacent":
				outputs = []string{"old", "new", "old"}
				after, finalFlag = "old\nnew\nold", false
			case "initial-assignment-clears":
				before, after, finalFlag = "", "new", false
			case "aggregate-overflow":
				before = strings.Repeat("x", 131071)
				after = strings.Repeat("x", 131071)
				outputs = []string{"y"}
			case "aggregate-exact":
				before = strings.Repeat("x", 131070)
				after = strings.Repeat("x", 131070) + "\ny"
				outputs = []string{"y"}
				finalFlag = false
			case "aggregate-utf8":
				before = strings.Repeat("x", 131069)
				after = strings.Repeat("x", 131069)
				outputs = []string{"€"}
			case "unmatched-association":
				rowID = "not-call_b"
			case "malformed-output-with-valid-sibling":
				after, finalFlag, damaged = "old\nnew", false, true
			case "error-text-no-inference":
				initialFlag, finalFlag = false, false
				outputs = []string{"ERROR: failed"}
				after = "old\nERROR: failed"
			case "preserve-error":
				failed = true
				after, finalFlag = "old\nnew", false
			case "output-json-blocks":
				outputs = []string{"\x00json:" + `[{"type":"text","text":" \u001b[31mone\u001b[0m\r"},{"type":"input_text","text":"two\u0000"},{"type":"output_text","text":"three "},{"type":"image","text":"ignored"}]`}
				after, finalFlag = "old\none\ntwo\nthree", false
			case "output-json-null":
				outputs = []string{"\x00json:null"}
			case "output-malformed-json-prefix":
				outputs = []string{"\x00json:{bad\r\x00 "}
				after, finalFlag = "old\njson:{bad", false
			}
			rows := make([]map[string]any, 0, len(outputs)+1)
			if damaged {
				rows = append(rows, map[string]any{"tool_call_id": "call_b", "tool_name": "ignored", "content_hex": "zz"})
			}
			for _, output := range outputs {
				rows = append(rows, hermesBoundsRow(rowID, output))
			}
			steps := hermesFlowQuery(hermesFlowDB2, hermesToolSortedOracle, hermesBoundsJSON(t, rows), nil)
			plan := append([]hermesAdmissionStep(nil), steps...)
			input, original := hermesBoundsMergeEntries(before, failed, initialFlag), hermesBoundsMergeEntries(before, failed, initialFlag)
			want, preserved := hermesBoundsMergeEntries(after, failed, finalFlag), hermesBoundsMergeEntries(after, failed, finalFlag)
			f := newHermesAdmissionFixture(t, steps)
			var candidate []Entry
			var corrupt bool
			err := f.owner.Do(context.Background(), func(c *conversationlife.Call) error {
				var e error
				candidate, corrupt, e = f.reader.attachToolResultsIn(c, hermesFlowDB2, "ses_test", input)
				return e
			})
			hermesBoundsCheck(t, f, plan)
			if err != nil || corrupt != damaged || !reflect.DeepEqual(candidate, want) || !reflect.DeepEqual(input, original) {
				t.Fatalf("complete merge/input mismatch: err=%v corrupt=%v", err, corrupt)
			}
			candidate[0].Text = "candidate-only"
			candidate[0].Tools[0] = ToolActivity{ID: "candidate-only"}
			if !reflect.DeepEqual(candidate[0].Tools[1:], preserved[0].Tools[1:]) || !reflect.DeepEqual(candidate[1:], preserved[1:]) || !reflect.DeepEqual(input, original) || !reflect.DeepEqual(want, preserved) {
				t.Fatal("candidate mutation escaped independent storage")
			}
			input[1].Text = "input-only"
			input[1].Tools[0].Output = "input-only"
			if !reflect.DeepEqual(candidate[1:], preserved[1:]) || !reflect.DeepEqual(want, preserved) || !reflect.DeepEqual(original, hermesBoundsMergeEntries(before, failed, initialFlag)) {
				t.Fatal("input mutation escaped independent storage")
			}
		})
	}
}

// Metadata plans own their path arrays, including nonnil empty arrays. Immutable
// FileInfo values and error identities are operands, not mutable snapshot state.
func hermesMetadataPlanCopy(in []hermesAdmissionStep) []hermesAdmissionStep {
	if in == nil {
		return nil
	}
	out := make([]hermesAdmissionStep, len(in))
	copy(out, in)
	for i := range in {
		if in[i].paths != nil {
			out[i].paths = make([]string, len(in[i].paths))
			copy(out[i].paths, in[i].paths)
		}
	}
	return out
}

func hermesMetadataInspect(t *testing.T, f *hermesAdmissionFixture, expected []hermesAdmissionStep) {
	t.Helper()
	f.check(t) // actual synchronous operations and their bridges have retired
	if f.reader.observe != nil || !f.reader.metadata.complete() || !reflect.DeepEqual(f.steps, expected) {
		t.Fatal("metadata observer, dependency bundle or complete script changed")
	}
	for i := range expected {
		if f.steps[i].err != expected[i].err {
			t.Fatal("script error identity changed")
		}
		if len(expected[i].paths) != 0 && &f.steps[i].paths[0] == &expected[i].paths[0] {
			t.Fatal("metadata expected paths alias subject storage")
		}
	}
}

// Only exact chosen leaves are permitted, with ordinary wrapping/joining allowed.
func hermesMetadataErrors(t *testing.T, got error, allowed ...error) {
	t.Helper()
	for _, want := range allowed {
		if want == nil || !errors.Is(got, want) {
			t.Fatalf("missing metadata error identity: %v", got)
		}
	}
	var visit func(error)
	visit = func(err error) {
		if err == nil {
			return
		}
		for _, want := range allowed {
			if err == want {
				return
			}
		}
		if joined, ok := err.(interface{ Unwrap() []error }); ok {
			children := joined.Unwrap()
			if len(children) == 0 {
				t.Fatal("empty unexpected error join")
			}
			for _, child := range children {
				visit(child)
			}
			return
		}
		if wrapped, ok := err.(interface{ Unwrap() error }); ok && wrapped.Unwrap() != nil {
			visit(wrapped.Unwrap())
			return
		}
		t.Fatalf("unexpected metadata error leaf: %v", err)
	}
	visit(got)
	if len(allowed) == 0 && got != nil {
		t.Fatal("expected nil semantic error")
	}
}

type hermesMetadataInfo struct{ mode os.FileMode }

func (i hermesMetadataInfo) Name() string       { return "state.db" }
func (i hermesMetadataInfo) Size() int64        { return 100 }
func (i hermesMetadataInfo) Mode() os.FileMode  { return i.mode }
func (i hermesMetadataInfo) ModTime() time.Time { return time.Unix(0, 1) }
func (i hermesMetadataInfo) IsDir() bool        { return i.mode.IsDir() }
func (i hermesMetadataInfo) Sys() any           { return nil }

func TestInstalledHermesMetadataDiscovery(t *testing.T) {
	for _, name := range []string{
		"ready-false", "empty-roots", "empty-candidates", "single-regular", "canonical-pair", "nested-regular",
		"candidates-exceed-roots", "roots-exceed-candidates", "canonical-duplicates", "first-root-eval-error",
		"first-db-eval-error", "first-stat-error", "first-stat-nil", "first-stat-directory", "first-stat-symlink",
		"first-stat-pipe", "first-path-is-root", "first-path-is-parent", "first-path-sibling-prefix",
		"first-relative-mismatch", "first-empty-canonical-path", "all-candidates-miss", "roots-request-error", "databases-request-error",
	} {
		t.Run(name, func(t *testing.T) {
			primary, second := errors.New("metadata primary"), errors.New("metadata second")
			steps := hermesFlowDiscovery(true)
			want, code := []string{hermesFlowDB2}, ""
			var identities []error
			switch name {
			case "ready-false":
				steps = []hermesAdmissionStep{{kind: "ready", ready: false}}
				want, code = nil, "source_unavailable"
			case "empty-roots":
				steps[1].paths = []string{}
				steps = steps[:3]
				want, code = nil, "source_unavailable"
			case "empty-candidates":
				steps[2].paths = []string{}
				steps = steps[:3]
				want, code = nil, "source_unavailable"
			case "single-regular", "canonical-pair", "nested-regular", "candidates-exceed-roots", "roots-exceed-candidates":
				steps = hermesFlowDiscovery(false)
				want = []string{hermesFlowDB}
				switch name {
				case "canonical-pair":
					steps[3].resolved = "/planned/canonical"
					steps[4].resolved, steps[5].path = "/planned/canonical/state.db", "/planned/canonical/state.db"
					want = []string{"/planned/canonical/state.db"}
				case "nested-regular":
					steps[4].resolved, steps[5].path = "/planned/hermes/nested/state.db", "/planned/hermes/nested/state.db"
					want = []string{"/planned/hermes/nested/state.db"}
				case "candidates-exceed-roots":
					steps[2].paths = []string{hermesFlowDB, hermesFlowDB2}
				case "roots-exceed-candidates":
					steps[1].paths = []string{"/planned/hermes", "/planned/second"}
				}
			case "canonical-duplicates":
				steps[6].resolved, steps[7].resolved, steps[8].path = "/planned/hermes", hermesFlowDB, hermesFlowDB
				want = []string{hermesFlowDB, hermesFlowDB}
			case "first-root-eval-error":
				steps[3].err = primary
				steps = append(steps[:4], steps[6:]...)
			case "first-db-eval-error":
				steps[4].err = primary
				steps = append(steps[:5], steps[6:]...)
			case "first-stat-error":
				steps[5].err = primary
			case "first-stat-nil":
				steps[5].info = nil
			case "first-stat-directory":
				steps[5].info = hermesMetadataInfo{os.ModeDir | 0700}
			case "first-stat-symlink":
				steps[5].info = hermesMetadataInfo{os.ModeSymlink | 0600}
			case "first-stat-pipe":
				steps[5].info = hermesMetadataInfo{os.ModeNamedPipe | 0600}
			case "first-path-is-root", "first-path-is-parent", "first-path-sibling-prefix", "first-relative-mismatch", "first-empty-canonical-path":
				resolved := map[string]string{"first-path-is-root": "/planned/hermes", "first-path-is-parent": "/planned", "first-path-sibling-prefix": "/planned/hermes-other/state.db", "first-relative-mismatch": "relative/state.db", "first-empty-canonical-path": ""}
				steps[4].resolved = resolved[name]
				steps = append(steps[:5], steps[6:]...)
			case "all-candidates-miss":
				steps[3].err, steps[6].err = primary, second
				steps = []hermesAdmissionStep{steps[0], steps[1], steps[2], steps[3], steps[6]}
				want, code = nil, "source_unavailable"
			case "roots-request-error":
				steps[1].err = primary
				steps = steps[:2]
				want, code, identities = nil, "source_unavailable", []error{primary}
			case "databases-request-error":
				steps[2].err = primary
				steps = steps[:3]
				want, code, identities = nil, "source_unavailable", []error{primary}
			}
			expected := hermesMetadataPlanCopy(steps)
			f := newHermesAdmissionFixture(t, steps)
			var paths []string
			var gotCode string
			err := f.owner.Do(context.Background(), func(c *conversationlife.Call) error {
				var e error
				paths, gotCode, e = f.reader.databasesIn(c)
				return e
			})
			hermesMetadataInspect(t, f, expected)
			if !reflect.DeepEqual(paths, want) || gotCode != code {
				t.Fatalf("discovery tuple=%v/%s", paths, gotCode)
			}
			hermesMetadataErrors(t, err, identities...)
			for i := range paths {
				paths[i] = "output-only"
			}
			hermesMetadataInspect(t, f, expected)
		})
	}
}

func TestInstalledHermesDirectorySemantics(t *testing.T) {
	for _, entry := range []string{"sameDirectory", "locate"} {
		for _, profile := range []string{"clean-match", "clean-mismatch", "first-eval-ordinary-error", "second-eval-ordinary-error", "first-eval-terminal", "second-eval-terminal"} {
			t.Run(entry+"."+profile, func(t *testing.T) {
				primary := errors.New("cwd primary")
				cwdSteps := []hermesAdmissionStep{{kind: "eval", path: "/planned/alias", resolved: "/planned/canonical-project"}, {kind: "eval", path: "/planned/project", resolved: "/planned/canonical-project"}}
				same := profile == "clean-match"
				var identities []error
				switch profile {
				case "clean-mismatch":
					cwdSteps[1].resolved = "/planned/other-project"
				case "first-eval-ordinary-error":
					cwdSteps[0].err = primary
				case "second-eval-ordinary-error":
					cwdSteps[1].err = primary
				case "first-eval-terminal":
					cwdSteps[0].err = ErrSourceCleanupUncertain
					cwdSteps = cwdSteps[:1]
					identities = []error{ErrSourceCleanupUncertain}
				case "second-eval-terminal":
					cwdSteps[1].err = errors.Join(primary, sqliteexec.ErrCleanupUncertain)
					identities = []error{primary, sqliteexec.ErrCleanupUncertain}
				}
				steps := cwdSteps
				wantLocation, wantCode := Location{}, ""
				if entry == "locate" {
					steps = hermesFlowDiscovery(true)
					// Stored cwd is nonclean too; only this independent wire operand changes.
					raw := strings.Replace(hermesLocateReply, "/planned/project", "/planned/stored/../project/.", 1)
					steps = append(steps, hermesFlowQuery(hermesFlowDB, hermesLocateSQLOracle, raw, nil)...)
					steps = append(steps, cwdSteps...)
					wantCode = "invalid_session"
					if same {
						wantLocation, wantCode = Location{Path: hermesFlowDB, Root: "/planned/hermes", Title: "Hermes title"}, ""
					}
					if len(identities) != 0 {
						wantCode = "query_failed"
					}
				}
				expected := hermesMetadataPlanCopy(steps)
				f := newHermesAdmissionFixture(t, steps)
				var gotSame bool
				var location Location
				var code string
				err := f.owner.Do(context.Background(), func(c *conversationlife.Call) error {
					var e error
					if entry == "sameDirectory" {
						gotSame, e = f.reader.sameDirectoryIn(c, "/planned/link/../alias/.", "/planned/stored/../project/.")
					} else {
						location, code, e = f.reader.locateIn(c, "/planned/link/../alias/.", "ses_test")
					}
					return e
				})
				hermesMetadataInspect(t, f, expected)
				wantSame := entry == "sameDirectory" && same
				if gotSame != wantSame || location != wantLocation || code != wantCode {
					t.Fatalf("cwd tuple=%v/%+v/%s", gotSame, location, code)
				}
				hermesMetadataErrors(t, err, identities...)
			})
		}
	}
}

// Prefix ends at the selected actual effect. DB2 stat is after private DB1
// acceptance, but before any SQL or externally published discovery result.
func hermesMetadataSite(site string) int {
	switch site {
	case "roots":
		return 1
	case "databases":
		return 2
	case "root-eval":
		return 3
	case "db-eval":
		return 4
	case "stat-first":
		return 5
	case "stat-second":
		return 8
	default:
		panic("unplanned metadata site")
	}
}

func TestInstalledHermesMetadataTerminalMarkers(t *testing.T) {
	for _, entry := range []string{"databases", "locate"} {
		for _, site := range []string{"roots", "databases", "root-eval", "db-eval", "stat-first", "stat-second"} {
			t.Run(entry+"."+site, func(t *testing.T) {
				primary := errors.New("terminal metadata primary")
				marker := map[string]error{"roots": context.Canceled, "databases": context.DeadlineExceeded, "root-eval": conversationlife.ErrRefused, "db-eval": sqliteexec.ErrCleanupUncertain, "stat-first": ErrSourceCleanupUncertain, "stat-second": errors.Join(primary, ErrSourceCleanupUncertain)}[site]
				identities := []error{marker}
				if site == "stat-second" {
					identities = []error{primary, ErrSourceCleanupUncertain}
				}
				index := hermesMetadataSite(site)
				steps := hermesFlowDiscovery(true)[:index+1]
				steps[index].err = marker
				expected := hermesMetadataPlanCopy(steps)
				f := newHermesAdmissionFixture(t, steps)
				var paths []string
				var location Location
				var code string
				err := f.owner.Do(context.Background(), func(c *conversationlife.Call) error {
					var e error
					if entry == "databases" {
						paths, code, e = f.reader.databasesIn(c)
					} else {
						location, code, e = f.reader.locateIn(c, "", "ses_test")
					}
					return e
				})
				hermesMetadataInspect(t, f, expected)
				wantCode := "source_unavailable"
				if entry == "locate" {
					wantCode = "query_failed"
				}
				if paths != nil || location != (Location{}) || code != wantCode {
					t.Fatalf("terminal metadata tuple=%v/%+v/%s", paths, location, code)
				}
				hermesMetadataErrors(t, err, identities...)
			})
		}
	}
}

func TestInstalledHermesMetadataOwnerCancellation(t *testing.T) {
	for _, site := range []string{"roots", "databases", "root-eval", "db-eval", "stat-first", "stat-second"} {
		for _, outcome := range []string{"clean", "primary"} {
			t.Run(site+"."+outcome, func(t *testing.T) {
				primary := errors.New("owner metadata primary")
				index := hermesMetadataSite(site)
				steps := hermesFlowDiscovery(true)[:index+1]
				identities := []error{context.Canceled}
				if outcome == "primary" {
					steps[index].err = primary
					identities = append(identities, primary)
				}
				expected := hermesMetadataPlanCopy(steps)
				f := newHermesAdmissionFixture(t, steps)
				original := f.reader.metadata
				// Installed through inspection; assertion exits still join before restore.
				defer func() { f.owner.Join(); f.reader.metadata = original }()
				var work, parent context.Context
				captures, lists, callbacks, triggers := 0, 0, 0, 0
				live, distinct, sameDone, closed := true, false, true, false
				var cause error
				before := func() {
					if work == nil || work.Err() != nil {
						live = false
					}
					if work != nil {
						select {
						case <-work.Done():
							live = false
						default:
						}
					}
				}
				after := func() {
					callbacks++
					if f.next != index+1 || f.unknown != 0 {
						return
					}
					triggers++
					f.owner.Begin() // nonwaiting; never Join from this active callback
					if work != nil {
						select {
						case <-work.Done():
							closed = true
						default:
						}
						cause = work.Err()
					}
				}
				f.reader.metadata.roots = func(ctx context.Context, home string) ([]string, error) {
					paths, err := original.roots(ctx, home) // original runs LIVE, even for planned primary
					captures++
					if captures == 1 {
						work = ctx
					} else {
						sameDone = false
					}
					distinct = parent != nil && ctx != nil && ctx.Done() != nil && ctx.Done() != parent.Done()
					before()
					after()
					return paths, err
				}
				f.reader.metadata.databases = func(ctx context.Context, home string) ([]string, error) {
					before()
					paths, err := original.databases(ctx, home)
					lists++
					if work == nil || ctx == nil || ctx.Done() != work.Done() {
						sameDone = false
					}
					after()
					return paths, err
				}
				f.reader.metadata.eval = func(path string) (string, error) {
					before()
					resolved, err := original.eval(path)
					after()
					return resolved, err
				}
				f.reader.metadata.stat = func(path string) (os.FileInfo, error) {
					before()
					info, err := original.stat(path)
					after()
					return info, err
				}
				var location Location
				var code string
				err := f.owner.Do(context.Background(), func(c *conversationlife.Call) error {
					parent = c.Context()
					var e error
					location, code, e = f.reader.locateIn(c, "", "ses_test")
					return e
				})
				hermesMetadataInspect(t, f, expected)
				wantLists := 1
				if site == "roots" {
					wantLists = 0
				}
				if captures != 1 || lists != wantLists || callbacks != index || triggers != 1 || !live || !distinct || !sameDone || !closed || cause != context.Canceled {
					t.Fatalf("actual work cancellation receipts captures=%d lists=%d callbacks=%d triggers=%d live=%v distinct=%v same=%v closed=%v cause=%v", captures, lists, callbacks, triggers, live, distinct, sameDone, closed, cause)
				}
				if location != (Location{}) || code != "query_failed" {
					t.Fatalf("owner metadata tuple=%+v/%s", location, code)
				}
				hermesMetadataErrors(t, err, identities...)
			})
		}
	}
}

// Retained inner attachment loops, distinct from outer result-row holds.
const hermesInnerToolsSQL = `SELECT COALESCE(m.tool_call_id,'') AS tool_call_id,COALESCE(m.tool_name,'') AS tool_name,hex(COALESCE(m.content,'')) AS content_hex FROM messages AS m WHERE m.session_id=CAST(X'7365735f74657374' AS TEXT) AND m.role='tool' AND (m.active=1 OR m.compacted=1) AND COALESCE(m.display_kind,'') <> 'hidden' AND m.tool_call_id IN (CAST(X'63616c6c5f61' AS TEXT),CAST(X'63616c6c5f62' AS TEXT),CAST(X'63616c6c5f63' AS TEXT),CAST(X'63616c6c5f64' AS TEXT),CAST(X'63616c6c5f65' AS TEXT),CAST(X'63616c6c5f66' AS TEXT),CAST(X'63616c6c5f67' AS TEXT),CAST(X'63616c6c5f68' AS TEXT),CAST(X'63616c6c5f69' AS TEXT)) ORDER BY m.id;`

const hermesInnerToolsWire = `[{"tool_call_id":"call_a","tool_name":"result-a","content_hex":"6e65772d61"},{"tool_call_id":"call_b","tool_name":"result-b","content_hex":"6e65772d62"},{"tool_call_id":"call_c","tool_name":"result-c","content_hex":"6e65772d63"},{"tool_call_id":"call_d","tool_name":"result-d","content_hex":"6e65772d64"},{"tool_call_id":"call_e","tool_name":"result-e","content_hex":"6e65772d65"},{"tool_call_id":"call_f","tool_name":"result-f","content_hex":"6e65772d66"},{"tool_call_id":"call_g","tool_name":"result-g","content_hex":"6e65772d67"},{"tool_call_id":"call_h","tool_name":"result-h","content_hex":"6e65772d68"},{"tool_call_id":"call_i","tool_name":"result-i","content_hex":"6e65772d69"}]`

func hermesInnerInput() []Entry {
	return []Entry{
		{ID: "entry-a", Timestamp: "1970-01-01T00:00:01Z", Role: "assistant", Text: "alpha", Truncated: true, Tools: []ToolActivity{
			{ID: "display-a", associationID: "call_a", Name: "shell-a", Input: "input-a", Output: "old-a", Error: true, Truncated: true},
			{ID: "display-b", associationID: "call_b", Name: "shell-b", Input: "input-b", Output: "old-b", Truncated: true},
			{ID: "display-c", associationID: "call_c", Name: "shell-c", Input: "input-c", Output: "old-c", Error: true},
		}},
		{ID: "entry-b", Timestamp: "1970-01-01T00:00:02Z", Role: "user", Text: "beta", Tools: []ToolActivity{
			{ID: "display-d", associationID: "call_d", Name: "shell-d", Input: "input-d", Output: "old-d", Truncated: true},
			{ID: "display-e", associationID: "call_e", Name: "shell-e", Input: "input-e", Output: "old-e", Error: true},
			{ID: "display-f", associationID: "call_f", Name: "shell-f", Input: "input-f", Output: "old-f", Error: true, Truncated: true},
		}},
		{ID: "entry-c", Timestamp: "1970-01-01T00:00:03Z", Role: "assistant", Text: "gamma", Truncated: true, Tools: []ToolActivity{
			{ID: "display-g", associationID: "call_g", Name: "shell-g", Input: "input-g", Output: "old-g"},
			{ID: "display-h", associationID: "call_h", Name: "shell-h", Input: "input-h", Output: "old-h", Error: true, Truncated: true},
			{ID: "display-i", associationID: "call_i", Name: "shell-i", Input: "input-i", Output: "old-i", Truncated: true},
		}},
	}
}

// Independent complete literals, not copies of input or observed publication.
func hermesInnerExpected() []Entry {
	return []Entry{
		{ID: "entry-a", Timestamp: "1970-01-01T00:00:01Z", Role: "assistant", Text: "alpha", Truncated: true, Tools: []ToolActivity{
			{ID: "display-a", associationID: "call_a", Name: "shell-a", Input: "input-a", Output: "old-a\nnew-a", Error: true},
			{ID: "display-b", associationID: "call_b", Name: "shell-b", Input: "input-b", Output: "old-b\nnew-b"},
			{ID: "display-c", associationID: "call_c", Name: "shell-c", Input: "input-c", Output: "old-c\nnew-c", Error: true},
		}},
		{ID: "entry-b", Timestamp: "1970-01-01T00:00:02Z", Role: "user", Text: "beta", Tools: []ToolActivity{
			{ID: "display-d", associationID: "call_d", Name: "shell-d", Input: "input-d", Output: "old-d\nnew-d"},
			{ID: "display-e", associationID: "call_e", Name: "shell-e", Input: "input-e", Output: "old-e\nnew-e", Error: true},
			{ID: "display-f", associationID: "call_f", Name: "shell-f", Input: "input-f", Output: "old-f\nnew-f", Error: true},
		}},
		{ID: "entry-c", Timestamp: "1970-01-01T00:00:03Z", Role: "assistant", Text: "gamma", Truncated: true, Tools: []ToolActivity{
			{ID: "display-g", associationID: "call_g", Name: "shell-g", Input: "input-g", Output: "old-g\nnew-g"},
			{ID: "display-h", associationID: "call_h", Name: "shell-h", Input: "input-h", Output: "old-h\nnew-h", Error: true},
			{ID: "display-i", associationID: "call_i", Name: "shell-i", Input: "input-i", Output: "old-i\nnew-i"},
		}},
	}
}

type hermesInnerSite struct {
	name             string
	target, canceled int
	queried          bool
}

func hermesInnerTrace(site hermesInnerSite, control bool) []string {
	var result []string
	add := func(stage string, n int) {
		for i := 0; i < n; i++ {
			result = append(result, stage)
		}
	}
	if control || site.queried {
		add("attachment", 38)
		add("tool-rows", 13)
		if control {
			add("attachment", 165)
		} else {
			add("attachment", site.canceled-38)
		}
	} else {
		add("attachment", site.canceled)
	}
	return result
}

type hermesInnerObservation struct {
	f          *hermesAdmissionFixture
	h          *hermesLifetimeHold
	site       hermesInnerSite
	want, seen []string
	counts     map[string]int
	contexts   map[string]context.Context
	query      context.Context
	raw        []byte
	queries    int
}

func (o *hermesInnerObservation) observe(ctx context.Context, stage string) {
	o.f.observations++
	o.seen = append(o.seen, stage)
	o.counts[stage]++
	i := len(o.seen) - 1
	if ctx == nil || i >= len(o.want) || o.want[i] != stage {
		o.f.deny()
		return
	}
	switch ctx.(type) {
	case hermesObservationContext, *hermesObservationContext:
		o.f.deny()
		return
	}
	if first := o.contexts[stage]; first != nil {
		if first.Done() != ctx.Done() {
			o.f.deny()
			return
		}
	} else {
		o.contexts[stage] = ctx
	}
	// Query callback follows A38,T11; decode checks are T12,T13.
	wantQueries := 0
	if i >= 49 {
		wantQueries = 1
	}
	if o.queries != wantQueries {
		o.f.deny()
		return
	}
	if stage != "attachment" || o.counts[stage] != o.site.target {
		return
	}
	if ctx.Err() != nil {
		o.f.deny()
		return
	}
	if o.site.queried {
		rows := o.contexts["tool-rows"]
		if rows == nil || o.query == nil || o.counts["tool-rows"] != 13 ||
			rows.Done() == ctx.Done() || o.query.Done() == ctx.Done() || rows.Done() == o.query.Done() {
			o.f.deny()
			return
		}
		for _, retired := range []context.Context{rows, o.query} {
			if retired.Err() != context.Canceled {
				o.f.deny()
				return
			}
			select {
			case <-retired.Done():
			default:
				o.f.deny()
				return
			}
		}
	} else if o.query != nil || o.contexts["tool-rows"] != nil {
		o.f.deny()
		return
	}
	o.h.wait(ctx)
}

// Hooks stay installed until this complete post-join inspection has finished.
func hermesInnerInspect(t *testing.T, o *hermesInnerObservation, steps []hermesAdmissionStep, parent, child *hermesLifetimeWorker, caller, parentContext context.Context, input, snapshot, pristine []Entry) {
	t.Helper()
	f := o.f
	queries := len(steps) / 2
	if f.reader.observe == nil || f.reader.installed.deps.query == nil ||
		!reflect.DeepEqual(f.steps, steps) || f.next != len(steps) || f.seen != len(steps) || f.unknown != 0 ||
		f.observations != len(o.want) || !reflect.DeepEqual(o.seen, o.want) || f.owner.Witnesses() != nil ||
		o.queries != queries || o.h.count != 1 || !reflect.DeepEqual(input, pristine) || !reflect.DeepEqual(snapshot, pristine) {
		t.Fatal("inner-loop complete script/trace/input/witness/hold contract")
	}
	if queries == 0 {
		if o.query != nil || o.raw != nil {
			t.Fatal("pre-query cancellation leaked query")
		}
	} else if string(o.raw) != hermesInnerToolsWire {
		t.Fatal("whole query reply changed")
	}
	counts := make(map[string]int)
	for _, stage := range o.want {
		counts[stage]++
	}
	if !reflect.DeepEqual(counts, o.counts) || len(o.contexts) != len(counts) {
		t.Fatal("exact stage cardinality")
	}
	parent.check(t)
	child.check(t)
	if o.h.ctx == nil || o.contexts["attachment"] == nil || o.h.ctx.Done() != o.contexts["attachment"].Done() {
		t.Fatal("actual target identity")
	}
	contexts := []context.Context{caller, parentContext}
	for stage := range counts {
		contexts = append(contexts, o.contexts[stage])
	}
	if queries != 0 {
		contexts = append(contexts, o.query)
	}
	for i, ctx := range contexts {
		if ctx == nil || ctx.Done() == nil || ctx.Err() != context.Canceled {
			t.Fatal("joined scope not retired")
		}
		select {
		case <-ctx.Done():
		default:
			t.Fatal("retired Done open")
		}
		for j := 0; j < i; j++ {
			if ctx.Done() == contexts[j].Done() {
				t.Fatal("distinct raw scopes aliased")
			}
		}
	}
}

func TestInstalledHermesRetainedInnerLoops(t *testing.T) {
	// Copy errors skip the outer return expression; association/merge errors
	// evaluate its additional attachment Err observation.
	for _, site := range []hermesInnerSite{
		{"copy-entries", 6, 6, false},
		{"copy-tools", 4, 4, false},
		{"collect-entries", 19, 20, false},
		{"collect-tools", 17, 18, false},
		{"collect-ids", 28, 29, false},
		{"merge-entries", 50, 51, true},
		{"merge-tools", 48, 49, true},
	} {
		for _, profile := range []string{"control", "caller", "owner", "parent"} {
			t.Run(site.name+"."+profile, func(t *testing.T) { hermesInnerLeaf(t, site, profile) })
		}
	}
}

func hermesInnerLeaf(t *testing.T, site hermesInnerSite, profile string) {
	t.Helper()
	control := profile == "control"
	var steps, wantSteps []hermesAdmissionStep
	if control || site.queried {
		steps = hermesFlowQuery(hermesFlowDB2, hermesInnerToolsSQL, hermesInnerToolsWire, nil)
		wantSteps = hermesFlowQuery(hermesFlowDB2, hermesInnerToolsSQL, hermesInnerToolsWire, nil)
	}
	input, snapshot, pristine := hermesInnerInput(), hermesInnerInput(), hermesInnerInput()
	expected, expectedSnapshot := hermesInnerExpected(), hermesInnerExpected()
	f := newHermesAdmissionFixture(t, steps)
	h := newHermesLifetimeHold()
	o := &hermesInnerObservation{f: f, h: h, site: site, want: hermesInnerTrace(site, control), counts: make(map[string]int), contexts: make(map[string]context.Context)}
	parent, child := newHermesLifetimeWorker(), newHermesLifetimeWorker()
	parentReady, allowReturn, abortStart := make(chan struct{}), make(chan struct{}), make(chan struct{})
	var returnOnce, abortOnce sync.Once
	retireParent := func() { returnOnce.Do(func() { close(allowReturn) }) }
	abort := func() { abortOnce.Do(func() { close(abortStart) }) }
	caller, cancel := context.WithCancel(context.Background())
	var call *conversationlife.Call
	var parentContext context.Context
	var parentErr, childErr error
	var candidate []Entry
	var corrupt, callbackReturned bool
	originalQuery, originalObserver := f.reader.installed.deps.query, f.reader.observe
	f.reader.observe = o.observe
	f.reader.installed.deps.query = func(c *conversationlife.Call, db, sql string, max int) ([]byte, error) {
		raw, err := originalQuery(c, db, sql, max)
		if err != nil || f.unknown != 0 {
			return raw, err
		}
		if o.queries != 0 || len(o.seen) != 49 {
			f.deny()
			return nil, conversationlife.ErrRefused
		}
		o.queries++
		o.query, o.raw = c.Context(), raw
		return raw, nil
	}
	t.Cleanup(func() {
		f.owner.Begin()
		cancel()
		h.open()
		retireParent()
		abort()
		<-parent.done
		<-child.done
		f.owner.Join()
		defer func() { f.reader.installed.deps.query = originalQuery; f.reader.observe = originalObserver }()
		hermesInnerInspect(t, o, wantSteps, parent, child, caller, parentContext, input, snapshot, pristine)
		if !callbackReturned || parentErr != nil || !reflect.DeepEqual(expected, expectedSnapshot) {
			t.Fatal("parent return/independent expected storage")
		}
	})
	go parent.run(func() {
		parentErr = f.owner.Do(caller, func(c *conversationlife.Call) error {
			call, parentContext = c, c.Context()
			close(parentReady)
			<-allowReturn
			callbackReturned = true
			return nil
		})
	})
	go child.run(func() {
		select {
		case <-abortStart:
			return
		case <-parentReady:
		}
		candidate, corrupt, childErr = f.reader.attachToolResultsIn(call, hermesFlowDB2, "ses_test", input)
	})
	entryTimer := time.NewTimer(time.Second)
	defer entryTimer.Stop()
	select {
	case <-parent.done:
		t.Fatal("parent exited before target")
	case <-child.done:
		t.Fatal("child exited before target")
	case <-entryTimer.C:
		t.Fatal("target absent; cleanup releases and joins")
	case <-h.entered:
	}
	if h.ctx == nil || h.ctx.Err() != nil || parentContext == nil || parentContext.Err() != nil || caller.Err() != nil ||
		h.ctx.Done() == parentContext.Done() || h.ctx.Done() == caller.Done() || !reflect.DeepEqual(input, pristine) || !reflect.DeepEqual(snapshot, pristine) {
		t.Fatal("held live actual target/ancestor/input contract")
	}
	if control {
		func() {
			timer := time.NewTimer(20 * time.Millisecond)
			defer timer.Stop()
			select {
			case <-parent.done:
				t.Fatal("control parent returned")
			case <-child.done:
				t.Fatal("control child returned")
			case <-timer.C:
			}
		}()
		h.open()
		<-child.done
		child.check(t)
		if childErr != nil || corrupt || !reflect.DeepEqual(candidate, expected) || !reflect.DeepEqual(o.seen, o.want) ||
			!reflect.DeepEqual(input, pristine) || !reflect.DeepEqual(snapshot, pristine) || caller.Err() != nil || parentContext.Err() != nil {
			t.Fatal("whole success/liveness before parent retirement")
		}
		// Capture natural retirement before any fence can manufacture it.
		if o.query == nil {
			t.Fatal("control query context missing")
		}
		retired := []context.Context{o.query}
		for _, ctx := range o.contexts {
			retired = append(retired, ctx)
		}
		for _, ctx := range retired {
			if ctx.Err() != context.Canceled {
				t.Fatal("successful child scope still live")
			}
			select {
			case <-ctx.Done():
			default:
				t.Fatal("successful child Done still open")
			}
		}
		retireParent()
		<-parent.done
		f.owner.Join()
		for i := range candidate {
			candidate[i].ID, candidate[i].Text = "mutated", "mutated"
			for j := range candidate[i].Tools {
				candidate[i].Tools[j] = ToolActivity{ID: "mutated", associationID: "mutated", Output: "mutated", Error: true, Truncated: true}
				for k := j + 1; k < len(candidate[i].Tools); k++ {
					if candidate[i].Tools[k] != expected[i].Tools[k] {
						t.Fatal("candidate sibling tools alias")
					}
				}
			}
			for k := i + 1; k < len(candidate); k++ {
				if !reflect.DeepEqual(candidate[k], expected[k]) {
					t.Fatal("candidate sibling entries alias")
				}
			}
			if !reflect.DeepEqual(input, pristine) || !reflect.DeepEqual(snapshot, pristine) || !reflect.DeepEqual(expected, expectedSnapshot) {
				t.Fatal("candidate mutation escaped independent backing")
			}
		}
		return
	}
	var receipt <-chan struct{}
	switch profile {
	case "caller":
		cancel()
	case "owner":
		receipt = f.owner.Begin()
	case "parent":
		retireParent()
		<-parent.done
		parent.check(t)
	}
	hermesAwaitCancellation(t, h.ctx, child)
	if h.ctx.Err() != context.Canceled || (profile != "caller" && caller.Err() != nil) {
		t.Fatal("selected raw cause before fallback")
	}
	retireParent()
	<-parent.done
	parent.check(t)
	if !callbackReturned || parentErr != nil {
		t.Fatal("parent not gone before occupancy interval")
	}
	if receipt == nil {
		receipt = f.owner.Begin()
	}
	hermesHeldReceipt(t, receipt, child)
	h.open()
	<-child.done
	f.owner.Join()
	<-receipt
	child.check(t)
	if candidate != nil || corrupt || !hermesSQLCancellationTree(childErr, false) || !reflect.DeepEqual(input, pristine) {
		t.Fatal("canceled inner traversal tuple/identity/input")
	}
}

// Provider-loop continuation: scope keys distinguish successive invocations,
// while stage names remain the exact production observer operands.
type hermesProviderEvent struct{ scope, stage string }
type hermesProviderPlan struct {
	control           bool
	name, targetScope string
	target            int
	trace             []hermesProviderEvent
	queryAt           []int
	steps             []hermesAdmissionStep
	liveAtHold        []string
	limit             int
	ids               []string
	want              hermesProviderResult
}
type hermesProviderResult struct {
	location      Location
	entries       []Entry
	rows          []hermesToolRow
	more, corrupt bool
	metadata      hermesRow
	code          string
	err           error
}

const hermesProviderDB3 = "/planned/third/state.db"
const hermesProviderBlocks = `[{"type":"output_text","text":"delta"},{"type":"text","text":"beta"},{"type":"input_text","text":"alpha"},{"type":"text","text":"gamma"}]`
const hermesProviderCalls = `[{"id":"call_d","name":"shell-d","arguments":"input-d"},{"id":"call_b","name":"shell-b","arguments":"input-b"},{"id":"call_a","name":"shell-a","arguments":"input-a"},{"id":"call_c","name":"shell-c","arguments":"input-c"}]`
const hermesProviderInputCall = `[{"id":"call_b","name":"shell-b","arguments":` + hermesProviderBlocks + `}]`
const hermesProviderToolWire = `[{"tool_call_id":"call_d","tool_name":"result-d","content_hex":"6f75742d64"},{"tool_call_id":"call_b","tool_name":"result-b","content_hex":"6f75742d62"},{"tool_call_id":"call_a","tool_name":"result-a","content_hex":"6f75742d61"},{"tool_call_id":"call_c","tool_name":"result-c","content_hex":"6f75742d63"}]`
const hermesProviderSingleWire = `[{"tool_call_id":"call_b","tool_name":"result-b","content_hex":"6f75742d62"}]`
const hermesProviderLocateWire = `[{"session_id":"ses_test","cwd":"/planned/project","title":"Third winner","message_id":1,"role":"user","content_hex":"6f6e65","timestamp":1,"message_total":1,"cursor_found":1}]`
const hermesProviderEightWire = `[
{"session_id":"ses_test","cwd":"/planned/project","title":"Eight","message_id":8,"role":"assistant","content_hex":"6569676874","timestamp":8,"message_total":8,"cursor_found":1},
{"session_id":"ses_test","cwd":"/planned/project","title":"Eight","message_id":7,"role":"user","content_hex":"736576656e","timestamp":7,"message_total":8,"cursor_found":1},
{"session_id":"ses_test","cwd":"/planned/project","title":"Eight","message_id":6,"role":"assistant","content_hex":"736978","timestamp":6,"message_total":8,"cursor_found":1},
{"session_id":"ses_test","cwd":"/planned/project","title":"Eight","message_id":5,"role":"user","content_hex":"66697665","timestamp":5,"message_total":8,"cursor_found":1},
{"session_id":"ses_test","cwd":"/planned/project","title":"Eight","message_id":4,"role":"assistant","content_hex":"666f7572","timestamp":4,"message_total":8,"cursor_found":1},
{"session_id":"ses_test","cwd":"/planned/project","title":"Eight","message_id":3,"role":"user","content_hex":"7468726565","timestamp":3,"message_total":8,"cursor_found":1},
{"session_id":"ses_test","cwd":"/planned/project","title":"Eight","message_id":2,"role":"assistant","content_hex":"74776f","timestamp":2,"message_total":8,"cursor_found":1},
{"session_id":"ses_test","cwd":"/planned/project","title":"Eight","message_id":1,"role":"user","content_hex":"6f6e65","timestamp":1,"message_total":8,"cursor_found":1}]`

// Full independent SQL template; operands are test-owned full hex IDs, never
// obtained from the production builder, returned tools or observed callbacks.
func hermesProviderToolSQL(operands string) string {
	return `SELECT COALESCE(m.tool_call_id,'') AS tool_call_id,COALESCE(m.tool_name,'') AS tool_name,hex(COALESCE(m.content,'')) AS content_hex FROM messages AS m WHERE m.session_id=CAST(X'7365735f74657374' AS TEXT) AND m.role='tool' AND (m.active=1 OR m.compacted=1) AND COALESCE(m.display_kind,'') <> 'hidden' AND m.tool_call_id IN (` + operands + `) ORDER BY m.id;`
}

const hermesProviderSuppliedIDs = `CAST(X'63616c6c5f64' AS TEXT),CAST(X'63616c6c5f62' AS TEXT),CAST(X'63616c6c5f61' AS TEXT),CAST(X'63616c6c5f63' AS TEXT)`
const hermesProviderSortedIDs = `CAST(X'63616c6c5f61' AS TEXT),CAST(X'63616c6c5f62' AS TEXT),CAST(X'63616c6c5f63' AS TEXT),CAST(X'63616c6c5f64' AS TEXT)`

func hermesProviderExpected(name string) hermesProviderResult {
	switch name {
	case "locate-candidates":
		return hermesProviderResult{location: Location{Path: hermesProviderDB3, Root: "/planned/third", Title: "Third winner"}}
	case "tool-id-encoding":
		return hermesProviderResult{rows: []hermesToolRow{
			{ToolCallID: "call_d", ToolName: "result-d", ContentHex: "6f75742d64"},
			{ToolCallID: "call_b", ToolName: "result-b", ContentHex: "6f75742d62"},
			{ToolCallID: "call_a", ToolName: "result-a", ContentHex: "6f75742d61"},
			{ToolCallID: "call_c", ToolName: "result-c", ContentHex: "6f75742d63"},
		}}
	case "message-anchor", "message-reversal":
		return hermesProviderResult{entries: []Entry{
			{ID: "1", Timestamp: "1970-01-01T00:00:01Z", Role: "user", Text: "one"},
			{ID: "2", Timestamp: "1970-01-01T00:00:02Z", Role: "assistant", Text: "two"},
			{ID: "3", Timestamp: "1970-01-01T00:00:03Z", Role: "user", Text: "three"},
			{ID: "4", Timestamp: "1970-01-01T00:00:04Z", Role: "assistant", Text: "four"},
			{ID: "5", Timestamp: "1970-01-01T00:00:05Z", Role: "user", Text: "five"},
			{ID: "6", Timestamp: "1970-01-01T00:00:06Z", Role: "assistant", Text: "six"},
			{ID: "7", Timestamp: "1970-01-01T00:00:07Z", Role: "user", Text: "seven"},
			{ID: "8", Timestamp: "1970-01-01T00:00:08Z", Role: "assistant", Text: "eight"},
		}, metadata: hermesRow{SessionID: "ses_test", CWD: "/planned/project", Title: "Eight", MessageID: 1, Role: "user", ContentHex: "6f6e65", Timestamp: 1, Total: 8, CursorFound: 1}}
	}
	entry := Entry{ID: "7", Timestamp: "1970-01-01T00:00:07Z", Role: "assistant", Text: "plain"}
	meta := hermesRow{SessionID: "ses_test", CWD: "/planned/project", Title: "Provider", MessageID: 7, Role: "assistant", ContentHex: "706c61696e", Timestamp: 7, Total: 1, CursorFound: 1}
	switch name {
	case "parser-tool-calls":
		meta.ToolCalls = hermesProviderCalls
		entry.Tools = []ToolActivity{
			{ID: "call_d", associationID: "call_d", Name: "shell-d", Input: "input-d", Output: "out-d"},
			{ID: "call_b", associationID: "call_b", Name: "shell-b", Input: "input-b", Output: "out-b"},
			{ID: "call_a", associationID: "call_a", Name: "shell-a", Input: "input-a", Output: "out-a"},
			{ID: "call_c", associationID: "call_c", Name: "shell-c", Input: "input-c", Output: "out-c"},
		}
	case "parser-message-blocks":
		meta.ContentHex = hex.EncodeToString([]byte("\x00json:" + hermesProviderBlocks))
		entry.Text = "delta\nbeta\nalpha\ngamma"
	case "parser-input-blocks":
		meta.ToolCalls = hermesProviderInputCall
		entry.Tools = []ToolActivity{{ID: "call_b", associationID: "call_b", Name: "shell-b", Input: "delta\nbeta\nalpha\ngamma", Output: "out-b"}}
	default:
		panic("unknown provider-loop expectation")
	}
	return hermesProviderResult{entries: []Entry{entry}, metadata: meta}
}

func hermesProviderPlanFor(t *testing.T, name string, control bool, primary error) hermesProviderPlan {
	t.Helper()
	p := hermesProviderPlan{control: control, name: name, limit: 2, want: hermesProviderExpected(name)}
	add := func(scope, stage string, n int) {
		for i := 0; i < n; i++ {
			p.trace = append(p.trace, hermesProviderEvent{scope, stage})
		}
	}
	query := func(db, sql, raw string, err error, at int) {
		p.steps = append(p.steps, hermesFlowQuery(db, sql, raw, err)...)
		p.queryAt = append(p.queryAt, at)
	}
	switch name {
	case "locate-candidates":
		p.targetScope, p.target, p.liveAtHold = "L", 4, []string{"L"}
		p.steps = []hermesAdmissionStep{
			{kind: "ready", ready: true},
			{kind: "roots", path: "/planned/home", paths: []string{"/planned/hermes", "/planned/second", "/planned/third"}},
			{kind: "databases", path: "/planned/home", paths: []string{hermesFlowDB, hermesFlowDB2, hermesProviderDB3}},
		}
		for _, pair := range [][2]string{{"/planned/hermes", hermesFlowDB}, {"/planned/second", hermesFlowDB2}, {"/planned/third", hermesProviderDB3}} {
			p.steps = append(p.steps, hermesAdmissionStep{kind: "eval", path: pair[0], resolved: pair[0]}, hermesAdmissionStep{kind: "eval", path: pair[1], resolved: pair[1]}, hermesAdmissionStep{kind: "stat", path: pair[1], info: openCodeFixtureInfo{size: 100, tick: 1}})
		}
		query(hermesFlowDB, hermesLocateSQLOracle, "", primary, 33)
		add("L", "locate", 1)
		add("D", "databases", 29)
		add("L", "locate", 1)
		add("M1", "message-rows", 2)
		add("L", "locate", 2)
		if control {
			query(hermesFlowDB2, hermesLocateSQLOracle, "[]", nil, 37)
			query(hermesProviderDB3, hermesLocateSQLOracle, hermesProviderLocateWire, nil, 44)
			add("M2", "message-rows", 5)
			add("L", "locate", 2)
			add("M3", "message-rows", 6)
			add("L", "locate", 2)
		}
	case "tool-id-encoding":
		p.targetScope, p.target, p.liveAtHold = "T", 3, []string{"T"}
		p.ids = []string{"call_d", "call_b", "call_a", "call_c"}
		if control {
			add("T", "tool-rows", 8)
			query(hermesFlowDB2, hermesProviderToolSQL(hermesProviderSuppliedIDs), hermesProviderToolWire, nil, 6)
		} else {
			add("T", "tool-rows", 3)
		}
	case "message-anchor", "message-reversal":
		p.targetScope, p.target, p.liveAtHold, p.limit = "M1", 6, []string{"P", "M1"}, 8
		if name == "message-reversal" {
			p.target = 14
		}
		const operand = " ORDER BY logical_id DESC LIMIT 3)"
		if strings.Count(hermesReadSQLOracle, operand) != 1 {
			t.Fatal("full independent LIMIT operand not unique")
		}
		query(hermesFlowDB2, strings.Replace(hermesReadSQLOracle, operand, " ORDER BY logical_id DESC LIMIT 9)", 1), hermesProviderEightWire, nil, 3)
		add("P", "parser", 1)
		if control {
			add("M1", "message-rows", 17)
			add("P", "parser", 58)
			add("A", "attachment", 20)
			add("P", "parser", 1)
		} else {
			add("M1", "message-rows", p.target)
		}
	default:
		p.targetScope, p.liveAtHold = "P", []string{"P"}
		content, calls := "706c61696e", ""
		switch name {
		case "parser-tool-calls":
			calls = hermesProviderCalls
			p.target = 8
		case "parser-message-blocks":
			content = hex.EncodeToString([]byte("\x00json:" + hermesProviderBlocks))
			p.target = 9
		case "parser-input-blocks":
			calls = hermesProviderInputCall
			p.target = 8
		default:
			t.Fatal("unknown provider-loop plan")
		}
		// Test-owned wire keys, not a marshaled production row or expected Entry.
		wire, err := json.Marshal([]map[string]any{{"session_id": "ses_test", "cwd": "/planned/project", "title": "Provider", "message_id": 7, "role": "assistant", "content_hex": content, "tool_calls": calls, "timestamp": 7, "message_total": 1, "cursor_found": 1}})
		if err != nil {
			t.Fatal(err)
		}
		query(hermesFlowDB2, hermesReadSQLOracle, string(wire), nil, 3)
		add("P", "parser", 1)
		add("M1", "message-rows", 6)
		switch name {
		case "parser-tool-calls":
			if control {
				add("P", "parser", 21)
				add("A", "attachment", 19)
				add("T", "tool-rows", 8)
				add("A", "attachment", 47)
				add("P", "parser", 1)
				query(hermesFlowDB2, hermesProviderToolSQL(hermesProviderSortedIDs), hermesProviderToolWire, nil, 53)
			} else {
				add("P", "parser", 9)
			}
		case "parser-message-blocks":
			if control {
				add("P", "parser", 13)
				add("A", "attachment", 6)
				add("P", "parser", 1)
			} else {
				add("P", "parser", 11)
			}
		case "parser-input-blocks":
			if control {
				add("P", "parser", 16)
				add("A", "attachment", 10)
				add("T", "tool-rows", 5)
				add("A", "attachment", 11)
				add("P", "parser", 1)
				query(hermesFlowDB2, hermesToolSingleOracle, hermesProviderSingleWire, nil, 36)
			} else {
				add("P", "parser", 10)
			}
		}
	}
	return p
}

// Each first capture is immutable; repeated stage names need not mean repeated
// scopes. The planned scope key also makes resumed outer scopes retain identity.
type hermesProviderObservation struct {
	f           *hermesAdmissionFixture
	h           *hermesLifetimeHold
	plan        hermesProviderPlan
	seen        []hermesProviderEvent
	contexts    map[string]context.Context
	counts      map[string]int
	queries     []context.Context
	replies     [][]byte
	queryErrors []error
	canceled    bool
}

func hermesProviderRetired(ctx context.Context) bool {
	if ctx == nil || ctx.Done() == nil || ctx.Err() != context.Canceled {
		return false
	}
	select {
	case <-ctx.Done():
		return true
	default:
		return false
	}
}
func (o *hermesProviderObservation) observe(ctx context.Context, stage string) {
	o.f.observations++
	i := len(o.seen)
	if ctx == nil || i >= len(o.plan.trace) {
		o.f.deny()
		return
	}
	switch ctx.(type) {
	case hermesObservationContext, *hermesObservationContext:
		o.f.deny()
		return
	}
	e := o.plan.trace[i]
	if stage != e.stage || ctx.Done() == nil {
		o.f.deny()
		return
	}
	wantQueries := 0
	for _, at := range o.plan.queryAt {
		if i >= at {
			wantQueries++
		}
	}
	if len(o.queries) != wantQueries {
		o.f.deny()
		return
	}
	if first := o.contexts[e.scope]; first != nil {
		if first.Done() != ctx.Done() {
			o.f.deny()
			return
		}
	} else {
		for key, prior := range o.contexts {
			if prior.Done() == ctx.Done() {
				o.f.deny()
				return
			}
			// New message scopes must follow retirement of every previous one.
			if strings.HasPrefix(e.scope, "M") && strings.HasPrefix(key, "M") && !hermesProviderRetired(prior) {
				o.f.deny()
				return
			}
		}
		o.contexts[e.scope] = ctx
	}
	// Source stack at EVERY checkpoint, not just at the selected hold. A query
	// callback has returned and retired before any subsequent observation.
	for key, captured := range o.contexts {
		active := key == e.scope || key == "L" || key == "P" || (key == "A" && e.scope == "T")
		if o.canceled || !active {
			if !hermesProviderRetired(captured) {
				o.f.deny()
				return
			}
		} else {
			if captured.Err() != nil {
				o.f.deny()
				return
			}
			select {
			case <-captured.Done():
				o.f.deny()
				return
			default:
			}
		}
	}
	for _, q := range o.queries {
		if !hermesProviderRetired(q) {
			o.f.deny()
			return
		}
	}
	o.seen = append(o.seen, e)
	o.counts[e.scope]++
	if e.scope != o.plan.targetScope || o.counts[e.scope] != o.plan.target {
		return
	}
	for key, captured := range o.contexts {
		live := false
		for _, active := range o.plan.liveAtHold {
			if key == active {
				live = true
			}
		}
		if live {
			if captured.Err() != nil {
				o.f.deny()
				return
			}
			select {
			case <-captured.Done():
				o.f.deny()
				return
			default:
			}
		} else if !hermesProviderRetired(captured) {
			o.f.deny()
			return
		}
	}
	for _, q := range o.queries {
		if !hermesProviderRetired(q) {
			o.f.deny()
			return
		}
	}
	o.h.wait(ctx)
	if !o.plan.control {
		o.canceled = true
		if !hermesProviderRetired(ctx) {
			o.f.deny()
		}
	}
}

func hermesProviderEffects(t *testing.T, o *hermesProviderObservation, expected hermesProviderPlan) {
	t.Helper()
	f := o.f
	if f.reader.observe == nil || f.reader.installed.deps.query == nil || f.unknown != 0 || f.next != len(expected.steps) || f.seen != len(expected.steps) ||
		!reflect.DeepEqual(f.steps, expected.steps) || !reflect.DeepEqual(o.plan, expected) || !reflect.DeepEqual(o.seen, expected.trace) ||
		f.observations != len(expected.trace) || f.owner.Witnesses() != nil || o.h.count != 1 {
		t.Fatal("provider full scripts/trace/guards/witness/hold")
	}
	counts := make(map[string]int)
	for _, e := range expected.trace {
		counts[e.scope]++
	}
	if !reflect.DeepEqual(counts, o.counts) || len(o.contexts) != len(counts) || len(o.queries) != len(expected.queryAt) || len(o.replies) != len(o.queries) || len(o.queryErrors) != len(o.queries) {
		t.Fatal("provider exact scope/query cardinality")
	}
	q := 0
	for _, step := range expected.steps {
		if step.kind != "query" {
			continue
		}
		if !reflect.DeepEqual(o.replies[q], []byte(step.raw)) || o.queryErrors[q] != step.err {
			t.Fatal("provider whole query reply/error identity")
		}
		q++
	}
}

func hermesProviderInspect(t *testing.T, o *hermesProviderObservation, expected hermesProviderPlan, caller, parentContext context.Context, parent, child *hermesLifetimeWorker) {
	t.Helper()
	hermesProviderEffects(t, o, expected)
	parent.check(t)
	child.check(t)
	target := o.contexts[expected.targetScope]
	if target == nil || o.h.ctx == nil || target.Done() != o.h.ctx.Done() {
		t.Fatal("provider actual target identity")
	}
	contexts := []context.Context{caller, parentContext}
	for _, ctx := range o.contexts {
		contexts = append(contexts, ctx)
	}
	contexts = append(contexts, o.queries...)
	for i, ctx := range contexts {
		if !hermesProviderRetired(ctx) {
			t.Fatal("provider joined raw scope not retired")
		}
		for j := 0; j < i; j++ {
			if ctx.Done() == contexts[j].Done() {
				t.Fatal("provider raw scope identities aliased")
			}
		}
	}
}

func TestInstalledHermesRetainedProviderLoops(t *testing.T) {
	for _, site := range []string{"locate-candidates", "message-anchor", "message-reversal", "tool-id-encoding", "parser-tool-calls", "parser-message-blocks", "parser-input-blocks"} {
		for _, profile := range []string{"control", "caller", "owner", "parent"} {
			t.Run(site+"."+profile, func(t *testing.T) { hermesProviderLeaf(t, site, profile) })
		}
	}
}

func hermesProviderLeaf(t *testing.T, site, profile string) {
	t.Helper()
	control := profile == "control"
	primary := errors.New("planned-first-database-ordinary-error")
	// Independent full plans allocate every paths/IDs/Entries/Tools backing.
	plan, expected := hermesProviderPlanFor(t, site, control, primary), hermesProviderPlanFor(t, site, control, primary)
	pristine := hermesProviderPlanFor(t, site, control, primary)
	// Probe independent path snapshots before construction/launch; preserve
	// nil/empty shapes rather than replacing every paths field with append(nil).
	for i := range plan.steps {
		for j := range plan.steps[i].paths {
			old := plan.steps[i].paths[j]
			plan.steps[i].paths[j] = "mutation-probe"
			independent := reflect.DeepEqual(expected, pristine) && expected.steps[i].paths[j] == old && pristine.steps[i].paths[j] == old
			for k := range plan.steps {
				if k != i && !reflect.DeepEqual(plan.steps[k], pristine.steps[k]) {
					independent = false
				}
			}
			plan.steps[i].paths[j] = old
			if !independent || !reflect.DeepEqual(plan, pristine) {
				t.Fatal("provider paths snapshot backing aliases")
			}
		}
	}
	for i := range plan.ids {
		old := plan.ids[i]
		plan.ids[i] = "mutation-probe"
		independent := reflect.DeepEqual(expected, pristine) && expected.ids[i] == old && pristine.ids[i] == old
		plan.ids[i] = old
		if !independent || !reflect.DeepEqual(plan, pristine) {
			t.Fatal("provider supplied IDs snapshot backing aliases")
		}
	}
	f := newHermesAdmissionFixture(t, plan.steps)
	h := newHermesLifetimeHold()
	o := &hermesProviderObservation{f: f, h: h, plan: plan, contexts: make(map[string]context.Context), counts: make(map[string]int)}
	parent, child := newHermesLifetimeWorker(), newHermesLifetimeWorker()
	parentReady, allowReturn, abortStart := make(chan struct{}), make(chan struct{}), make(chan struct{})
	var returnOnce, abortOnce sync.Once
	retireParent := func() { returnOnce.Do(func() { close(allowReturn) }) }
	abort := func() { abortOnce.Do(func() { close(abortStart) }) }
	caller, cancel := context.WithCancel(context.Background())
	var call *conversationlife.Call
	var parentContext context.Context
	var parentErr error
	var callbackReturned bool
	var result hermesProviderResult
	originalQuery, originalObserver := f.reader.installed.deps.query, f.reader.observe
	f.reader.observe = o.observe
	f.reader.installed.deps.query = func(c *conversationlife.Call, db, sql string, max int) ([]byte, error) {
		// Always run the frozen strict live/operand/deadline guard first, including
		// DB1's ordinary planned error. That error must not skip scope capture.
		raw, err := originalQuery(c, db, sql, max)
		if f.unknown != 0 {
			return raw, err
		}
		q := len(o.queries)
		if q >= len(plan.queryAt) || len(o.seen) != plan.queryAt[q] {
			f.deny()
			return nil, conversationlife.ErrRefused
		}
		ctx := c.Context()
		if ctx == nil || ctx.Err() != nil || ctx.Done() == nil {
			f.deny()
			return nil, conversationlife.ErrRefused
		}
		for _, previous := range o.queries {
			if previous.Done() == ctx.Done() || !hermesProviderRetired(previous) {
				f.deny()
				return nil, conversationlife.ErrRefused
			}
		}
		for _, scope := range o.contexts {
			if scope.Done() == ctx.Done() {
				f.deny()
				return nil, conversationlife.ErrRefused
			}
		}
		o.queries = append(o.queries, ctx)
		o.replies = append(o.replies, raw)
		o.queryErrors = append(o.queryErrors, err)
		return raw, err
	}
	// Registered before both sealed launches, after the factory's first Join.
	t.Cleanup(func() {
		f.owner.Begin()
		cancel()
		h.open()
		retireParent()
		abort()
		<-parent.done
		<-child.done
		f.owner.Join()
		defer func() { f.reader.installed.deps.query = originalQuery; f.reader.observe = originalObserver }()
		hermesProviderInspect(t, o, expected, caller, parentContext, parent, child)
		if !callbackReturned || parentErr != nil || !reflect.DeepEqual(expected, pristine) {
			t.Fatal("provider parent completion/independent expected storage")
		}
	})
	go parent.run(func() {
		parentErr = f.owner.Do(caller, func(c *conversationlife.Call) error {
			call, parentContext = c, c.Context()
			close(parentReady)
			<-allowReturn
			callbackReturned = true
			return nil
		})
	})
	go child.run(func() {
		select {
		case <-abortStart:
			return
		case <-parentReady:
		}
		switch site {
		case "locate-candidates":
			result.location, result.code, result.err = f.reader.locateIn(call, "", "ses_test")
		case "tool-id-encoding":
			result.rows, result.code, result.err = f.reader.queryToolRowsIn(call, hermesFlowDB2, "ses_test", plan.ids)
		default:
			result.entries, result.more, result.corrupt, result.metadata, result.code, result.err = f.reader.readFromIn(call, hermesFlowDB2, "ses_test", "", plan.limit)
		}
	})
	entryTimer := time.NewTimer(time.Second)
	defer entryTimer.Stop()
	select {
	case <-parent.done:
		t.Fatal("provider parent exited before hold")
	case <-child.done:
		t.Fatal("provider child exited before hold")
	case <-entryTimer.C:
		t.Fatal("provider target absent; cleanup releases then joins")
	case <-h.entered:
	}
	if h.ctx == nil || h.ctx.Err() != nil || caller.Err() != nil || parentContext == nil || parentContext.Err() != nil || h.ctx.Done() == caller.Done() || h.ctx.Done() == parentContext.Done() {
		t.Fatal("provider live actual target and ancestors")
	}
	if control {
		h.open()
		<-child.done
		child.check(t)
		if !reflect.DeepEqual(result, expected.want) || !reflect.DeepEqual(o.seen, expected.trace) || !reflect.DeepEqual(plan, expected) || caller.Err() != nil || parentContext.Err() != nil {
			t.Fatal("provider complete independent success before fence")
		}
		hermesProviderEffects(t, o, expected)
		// Natural retirement captured before the parent can return or any fence.
		for _, ctx := range o.contexts {
			if !hermesProviderRetired(ctx) {
				t.Fatal("provider success operation scope live")
			}
		}
		for _, ctx := range o.queries {
			if !hermesProviderRetired(ctx) {
				t.Fatal("provider success query scope live")
			}
		}
		retireParent()
		<-parent.done
		f.owner.Join()
		// Mutate all published backing, checking untouched siblings and BOTH
		// independently allocated plan/expectation trees after each mutation.
		for i := range result.entries {
			result.entries[i].ID, result.entries[i].Text = "mutated", "mutated"
			for j := range result.entries[i].Tools {
				result.entries[i].Tools[j] = ToolActivity{ID: "mutated", associationID: "mutated", Output: "mutated", Error: true, Truncated: true}
				for k := j + 1; k < len(result.entries[i].Tools); k++ {
					if result.entries[i].Tools[k] != expected.want.entries[i].Tools[k] {
						t.Fatal("provider tool sibling alias")
					}
				}
			}
			for k := i + 1; k < len(result.entries); k++ {
				if !reflect.DeepEqual(result.entries[k], expected.want.entries[k]) {
					t.Fatal("provider entry sibling alias")
				}
			}
			if !reflect.DeepEqual(plan, pristine) || !reflect.DeepEqual(expected, pristine) {
				t.Fatal("provider output mutation escaped")
			}
		}
		for i := range result.rows {
			result.rows[i] = hermesToolRow{ToolCallID: "mutated", ToolName: "mutated", ContentHex: "mutated"}
			for k := i + 1; k < len(result.rows); k++ {
				if result.rows[k] != expected.want.rows[k] {
					t.Fatal("provider row sibling alias")
				}
			}
			if !reflect.DeepEqual(plan, pristine) || !reflect.DeepEqual(expected, pristine) {
				t.Fatal("provider row mutation escaped")
			}
		}
		return
	}
	var receipt <-chan struct{}
	switch profile {
	case "caller":
		cancel()
	case "owner":
		receipt = f.owner.Begin()
	case "parent":
		retireParent()
		<-parent.done
		parent.check(t)
	}
	hermesAwaitCancellation(t, h.ctx, child)
	if h.ctx.Err() != context.Canceled || (profile != "caller" && caller.Err() != nil) {
		t.Fatal("provider selected raw cause before fallback")
	}
	retireParent()
	<-parent.done
	parent.check(t)
	if !callbackReturned || parentErr != nil {
		t.Fatal("provider parent not retired before occupancy interval")
	}
	if receipt == nil {
		receipt = f.owner.Begin()
	}
	hermesHeldReceipt(t, receipt, child)
	h.open()
	<-child.done
	f.owner.Join()
	<-receipt
	child.check(t)
	if !hermesSQLCancellationTree(result.err, false) {
		t.Fatal("provider cancellation tree contains non-Canceled identity")
	}
	zero := result
	zero.err = nil
	if !reflect.DeepEqual(zero, hermesProviderResult{code: "query_failed"}) || !reflect.DeepEqual(plan, expected) {
		t.Fatal("provider canceled full nil-zero tuple/inputs")
	}
}
