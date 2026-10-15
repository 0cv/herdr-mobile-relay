package herdr

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"testing"
	"time"

	"github.com/0cv/herdr-mobile-relay/internal/p1fixture"
)

type p1MemoryConn struct {
	net.Conn
	response *bytes.Reader
	writes [][]byte
	short bool
	readErr error
	deadlineErr error
	afterWrite func()
	closed chan struct{}
	closeOnce sync.Once
}

func (c *p1MemoryConn) Read(data []byte) (int, error) {
	if c.readErr != nil {
		return 0, c.readErr
	}
	if c.closed != nil {
		<-c.closed
		return 0, context.DeadlineExceeded
	}
	return c.response.Read(data)
}
func (c *p1MemoryConn) Write(data []byte) (int, error) {
	c.writes = append(c.writes, append([]byte(nil), data...))
	if c.afterWrite != nil { c.afterWrite() }
	if c.short {
		return len(data)/2, io.ErrShortWrite
	}
	return len(data), nil
}
func (c *p1MemoryConn) Close() error {
	if c.closed != nil {
		c.closeOnce.Do(func() { close(c.closed) })
	}
	return nil
}
func (c *p1MemoryConn) SetDeadline(time.Time) error { return c.deadlineErr }

func TestP1OfflineObserverNoninterference(t *testing.T) {
	for _, enabled := range []bool{false, true} {
		conn := &p1MemoryConn{response: bytes.NewReader([]byte("{\"id\":\"mobile-relay-api-1\",\"result\":{\"type\":\"ok\"}}\n"))}
		client := NewClient("must-not-execute", "/selected.sock")
		defer client.Close()
		client.api.unaryDial = func(context.Context, string, string) (net.Conn, error) { return conn, nil }
		observer, err := NewScrollEvidence(65536, 8)
		if err != nil { t.Fatal(err) }
		if enabled {
			if err := client.SetScrollEvidence(observer); err != nil { t.Fatal(err) }
		}
		if err := client.SendRawText(context.Background(), "pane", "\x1b[<64;40;10M"); err != nil { t.Fatal(err) }
		if len(conn.writes) != 1 { t.Fatal("raw writer retried") }
		if !enabled { continue }
		records, err := observer.Snapshot()
		if err != nil || len(records) != 1 || records[0].Written != len(conn.writes[0]) || records[0].WriteCalls != 1 || !bytes.Equal(records[0].Request, conn.writes[0]) || !bytes.HasSuffix(records[0].Response, []byte{'\n'}) { t.Fatalf("lost actual wire: %+v %v", records, err) }
		if err := client.SetScrollEvidence(observer); err == nil { t.Fatal("late observer replacement admitted") }
	}
}

func TestP1OfflineObserverPreuse(t *testing.T) {
	client := NewClient("must-not-execute", "/selected.sock")
	defer client.Close()
	entered, release := make(chan struct{}), make(chan struct{})
	client.api.unaryDial = func(context.Context, string, string) (net.Conn, error) {
		close(entered)
		<-release
		return nil, errors.New("controlled dial failure")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 250*time.Millisecond)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- client.SendRawText(ctx, "pane", "wheel") }()
	select {
	case <-entered:
	case <-ctx.Done():
		close(release)
		t.Fatal("controlled dial did not begin before deadline")
	}
	observer, err := NewScrollEvidence(65536, 8)
	if err != nil {
		close(release)
		t.Fatal(err)
	}
	installErr := client.SetScrollEvidence(observer)
	close(release)
	select {
	case err := <-done:
		if err == nil || installErr == nil {
			t.Fatal("observer installed after client use began")
		}
	case <-ctx.Done():
		t.Fatal("controlled dial did not complete before deadline")
	}
}

func TestP1OfflineObserverLimits(t *testing.T) {
	observer, err := NewScrollEvidence(4096, 1)
	if err != nil { t.Fatal(err) }
	observer.record(ScrollEvidenceRecord{Response: []byte("overflow")})
	if _, err := observer.Snapshot(); err == nil { t.Fatal("overflow accepted") }
	observer, err = NewScrollEvidence(65536, 1)
	if err != nil { t.Fatal(err) }
	observer.mu.Lock()
	observer.record(ScrollEvidenceRecord{})
	observer.mu.Unlock()
	if _, err := observer.Snapshot(); err == nil { t.Fatal("lost concurrent observation accepted") }
}

func TestP1OfflineRawFailureDispositions(t *testing.T) {
	for _, name := range []string{"connect", "pre-write-deadline", "pre-read", "read", "fence", "short", "malformed", "late-connect", "late-connect-fence", "late-ack"} {
		t.Run(name, func(t *testing.T) {
			conn := &p1MemoryConn{response: bytes.NewReader([]byte("{\n")), short: name == "short"}
			if name == "pre-write-deadline" { conn.deadlineErr = errors.New("controlled pre-write deadline refusal") }
			if name == "pre-read" { conn.afterWrite = func() { conn.readErr = errors.New("controlled transport failure after write and before first read") } }
			if name == "read" { conn.readErr = errors.New("controlled read refusal") }
			fenced := false
			client := NewClient("must-not-execute", "/selected.sock")
			defer client.Close()
			observer, err := NewScrollEvidence(65536, 8)
			if err != nil { t.Fatal(err) }
			if err := client.SetScrollEvidence(observer); err != nil { t.Fatal(err) }
			client.api.unaryDial = func(ctx context.Context, _, _ string) (net.Conn, error) {
				if name == "connect" { return nil, errors.New("controlled connect refusal") }
				if name == "late-connect" { <-ctx.Done(); return conn, nil }
				if name == "late-connect-fence" { fenced = true }
				return conn, nil
			}
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
			defer cancel()
			if name == "fence" { ctx = WithDispatchCheck(ctx, func() error { return errors.New("controlled fence") }) }
			if name == "late-connect-fence" { ctx = WithDispatchCheck(ctx, func() error {
				if fenced { return errors.New("controlled fence invalidated during dial") }
				return nil
			}) }
			if name == "late-ack" { conn.closed = make(chan struct{}) }
			err = client.SendRawText(ctx, "pane", "\x1b[<65;40;10M")
			prewrite := name == "connect" || name == "pre-write-deadline" || name == "fence" || name == "late-connect" || name == "late-connect-fence"
			if err == nil || errors.Is(err, ErrNotStarted) != prewrite || errors.Is(err, ErrDispatchedUnknown) == prewrite { t.Fatalf("wrong disposition: %v", err) }
			records, observeErr := observer.Snapshot()
			if observeErr != nil || len(records) != 1 || len(conn.writes) > 1 || prewrite && (len(conn.writes) != 0 || records[0].WriteCalls != 0 || records[0].Written != 0) || !prewrite && (len(conn.writes) != 1 || records[0].WriteCalls != 1) { t.Fatalf("wrong actual write boundary: %+v %v", records, observeErr) }
		})
	}
}

type p1SchemaMutation struct {
	name string
	change func(map[string]any)
}

func p1SchemaMutations() []p1SchemaMutation {
	return []p1SchemaMutation{
		{"wrong-text-type", func(r map[string]any) { schemaFixtureNode(r, "schemas", "request", "$defs", "PaneSendTextParams", "properties", "text")["type"] = "boolean" }},
		{"missing-reference", func(r map[string]any) { delete(schemaFixtureNode(r, "schemas", "request", "$defs"), "PaneSendTextParams") }},
		{"chained-missing-reference", func(r map[string]any) { schemaFixtureNode(r, "schemas", "request", "$defs")["PaneSendTextParams"] = map[string]any{"$ref": "#/schemas/request/$defs/Missing"} }},
		{"cyclic-reference", func(r map[string]any) { schemaFixtureNode(r, "schemas", "request", "$defs")["PaneSendTextParams"] = map[string]any{"$ref": "#/schemas/request/$defs/PaneSendTextParams"} }},
		{"remote-reference", func(r map[string]any) { schemaFixtureNode(r, "schemas", "request", "$defs")["PaneSendTextParams"] = map[string]any{"$ref": "https://example.invalid/params"} }},
		{"wrong-envelope", func(r map[string]any) { schemaFixtureNode(r, "schemas", "success_response", "properties", "id")["type"] = "integer" }},
		{"wrong-result-tag", func(r map[string]any) { branch, ok := (scrollWireSchema{r}).variant(schemaFixtureNode(r, "schemas", "success_response", "$defs", "ResponseResult"), "type", "ok"); if !ok { panic("captured schema lacks ok result") }; branch["properties"].(map[string]any)["type"].(map[string]any)["const"] = "accepted" }},
		{"wrong-process-info", func(r map[string]any) { schemaFixtureNode(r, "schemas", "success_response", "$defs", "PaneProcessInfo", "properties", "shell_pid")["type"] = "string" }},
		{"wrong-visible-source", func(r map[string]any) { schemaFixtureNode(r, "schemas", "request", "$defs", "ReadSource")["enum"] = []any{"recent"} }},
		{"wrong-visible-text", func(r map[string]any) { schemaFixtureNode(r, "schemas", "success_response", "$defs", "PaneReadResult", "properties", "text")["type"] = "array" }},
	}
}

func p1CheckDerivedSchema(t *testing.T, data []byte) []string {
	t.Helper()
	root, ok := piScrollSchemaDocument(data)
	if !ok || !qualifiesPiScrollWire(root) { t.Fatal("consumed schema does not qualify") }
	names := []string{}
	for _, mutation := range p1SchemaMutations() {
		copy, ok := piScrollSchemaDocument(data)
		if !ok { t.Fatal("captured schema copy failed") }
		mutation.change(copy)
		if qualifiesPiScrollWire(copy) { t.Fatalf("derived incompatible schema admitted: %s", mutation.name) }
		names = append(names, mutation.name)
	}
	return names
}

func TestP1OfflineDerivedWireNegatives(t *testing.T) {
	root := scrollSchemaFixture(t)
	data, err := json.Marshal(root)
	if err != nil { t.Fatal(err) }
	p1CheckDerivedSchema(t, data)
	if _, ok := piScrollSchemaDocument(nil); ok { t.Fatal("missing schema admitted") }
}

func TestP1CapturedWireSchema(t *testing.T) {
	if _, supplied := os.LookupEnv("HERDR_P1_CONTRACT_JSON"); !supplied {
		t.Skip("requires an owned disposable Pi fixture contract")
	}
	contract, err := p1fixture.Current()
	if err != nil { t.Fatal(err) }
	_, source, _, ok := runtime.Caller(0)
	if !ok { t.Fatal("source unavailable") }
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	proof, err := p1fixture.Run(ctx, os.Getenv("HERDR_P1_CONTRACT_BUN"), os.Getenv("HERDR_P1_CONTRACT_JSON"), "--no-env-file", "--no-install", filepath.Join(filepath.Dir(source), "../../tests/pi-scroll-qualification-ledger.mjs"), "fixture")
	var verified struct { Verified bool `json:"verified"` }
	if err != nil || json.Unmarshal(proof, &verified) != nil || !verified.Verified { t.Fatalf("live contract refused: %v", err) }
	client := NewClient(os.Getenv("HERDR_SCROLL_FIXTURE_BIN"), contract.SocketPath)
	defer client.Close()
	observer, err := NewScrollEvidence(1024*1024, 32)
	if err != nil { t.Fatal(err) }
	if err := client.SetScrollEvidence(observer); err != nil { t.Fatal(err) }
	status := client.RefreshCapabilities(ctx)
	if !client.SupportsPiScrollSocket() { t.Fatalf("actual schema refused: %+v", status) }
	records, err := observer.Snapshot()
	if err != nil { t.Fatal(err) }
	found := 0
	for index, record := range records {
		if record.Kind == "consumed_schema" {
			found++
			if record.Status == nil || record.Decision == nil || record.Decision.State != FeatureSupported || record.Status.SocketInstance != status.SocketInstance { t.Fatal("schema selection evidence mismatch") }
			names := p1CheckDerivedSchema(t, record.Response)
			if err := contract.Save("derived-wire-negatives.json", map[string]any{"schema_version": 1, "kind": "p1-derived-wire-negatives", "source_kind": "derived-wire-negatives", "complete": true, "truncated": false, "ambiguous": false, "skipped": false, "cases": names, "socket_instance": status.SocketInstance}); err != nil { t.Fatal(err) }
		}
		if err := contract.Save(filepath.Base("captured-wire-"+string(rune('a'+index))+".json"), map[string]any{"schema_version": 1, "kind": "p1-wire-observation", "source_kind": "real", "complete": true, "truncated": false, "ambiguous": false, "skipped": false, "record": record}); err != nil { t.Fatal(err) }
	}
	if found != 1 { t.Fatalf("consumed schema count: %d", found) }
}
