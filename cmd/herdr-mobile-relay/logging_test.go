package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/slogtest"
	"time"
)

type recordingWriter struct {
	mu     sync.Mutex
	writes [][]byte
	err    error
	short  bool
}

func (w *recordingWriter) Write(data []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.writes = append(w.writes, append([]byte(nil), data...))
	if w.err != nil {
		return 0, w.err
	}
	if w.short {
		return len(data) - 1, nil
	}
	return len(data), nil
}

func (w *recordingWriter) snapshot() [][]byte {
	w.mu.Lock()
	defer w.mu.Unlock()
	result := make([][]byte, len(w.writes))
	for i := range w.writes {
		result[i] = append([]byte(nil), w.writes[i]...)
	}
	return result
}

func TestJournalHandlerUsesRecordSeverity(t *testing.T) {
	writer := &recordingWriter{}
	handler := newJournalHandler(writer, "text", slog.Level(-10))
	levels := []struct {
		level  slog.Level
		prefix string
	}{
		{level: slog.Level(-8), prefix: "<7>"},
		{level: slog.LevelDebug, prefix: "<7>"},
		{level: slog.Level(-1), prefix: "<7>"},
		{level: slog.LevelInfo, prefix: "<6>"},
		{level: slog.Level(1), prefix: "<6>"},
		{level: slog.LevelWarn, prefix: "<4>"},
		{level: slog.Level(5), prefix: "<4>"},
		{level: slog.LevelError, prefix: "<3>"},
		{level: slog.Level(9), prefix: "<3>"},
	}
	for _, test := range levels {
		record := slog.NewRecord(testTime(), test.level, "level=ERROR <3>", 0)
		record.AddAttrs(slog.String("level", "ERROR"))
		if err := handler.Handle(context.Background(), record); err != nil {
			t.Fatalf("Handle(%v): %v", test.level, err)
		}
	}

	writes := writer.snapshot()
	if len(writes) != len(levels) {
		t.Fatalf("writes = %d, want %d", len(writes), len(levels))
	}
	for i, test := range levels {
		if !bytes.HasPrefix(writes[i], []byte(test.prefix)) {
			t.Errorf("write %d = %q, want prefix %q", i, writes[i], test.prefix)
		}
		if !bytes.HasSuffix(writes[i], []byte{'\n'}) {
			t.Errorf("write %d does not end in a newline: %q", i, writes[i])
		}
	}
}

func TestRelayLoggerFiltersBeforeResolving(t *testing.T) {
	var calls atomic.Int32
	valuer := countingLogValuer{calls: &calls}
	writer := &recordingWriter{}
	logger := newRelayLogger(writer, "text", slog.LevelInfo, true)

	logger.Debug("filtered", "value", valuer)
	if calls.Load() != 0 {
		t.Fatalf("filtered LogValuer calls = %d, want 0", calls.Load())
	}
	logger.Info("emitted", "value", valuer)
	if calls.Load() != 1 {
		t.Fatalf("emitted LogValuer calls = %d, want 1", calls.Load())
	}
}

type countingLogValuer struct {
	calls *atomic.Int32
}

func (v countingLogValuer) LogValue() slog.Value {
	v.calls.Add(1)
	return slog.StringValue("resolved")
}

func TestJournalHandlerFormatsTextAndJSON(t *testing.T) {
	for _, format := range []string{"text", "json"} {
		t.Run(format, func(t *testing.T) {
			writer := &recordingWriter{}
			logger := newRelayLogger(writer, format, slog.LevelInfo, true)
			logger.Debug("filtered")
			logger.Info("hello", "line", "one\ntwo")
			logger.Warn("warning")
			logger.Error("failure")

			writes := writer.snapshot()
			if len(writes) != 3 {
				t.Fatalf("writes = %d, want 3", len(writes))
			}
			for i, prefix := range []string{"<6>", "<4>", "<3>"} {
				if !bytes.HasPrefix(writes[i], []byte(prefix)) {
					t.Errorf("write %d = %q, want prefix %q", i, writes[i], prefix)
				}
			}
			if format == "json" {
				var record map[string]any
				if err := json.Unmarshal(writes[0][3:], &record); err != nil {
					t.Fatalf("JSON body = %q: %v", writes[0][3:], err)
				}
				if record["msg"] != "hello" {
					t.Errorf("message = %#v, want hello", record["msg"])
				}
			} else if bytes.HasPrefix(writes[0][3:], []byte("<")) {
				t.Fatalf("journal text record body has an unexpected prefix: %q", writes[0])
			}
		})
	}
}

func TestRelayLoggerNonJournalOutputHasNoPrefix(t *testing.T) {
	for _, format := range []string{"text", "json"} {
		t.Run(format, func(t *testing.T) {
			var output bytes.Buffer
			logger := newRelayLogger(&output, format, slog.LevelInfo, false)
			logger.Info("hello")
			if strings.HasPrefix(output.String(), "<") {
				t.Fatalf("output = %q, has journal prefix", output.String())
			}
			if format == "json" {
				var record map[string]any
				if err := json.Unmarshal(output.Bytes(), &record); err != nil {
					t.Fatalf("JSON output = %q: %v", output.Bytes(), err)
				}
			}
		})
	}
}

func TestJournalHandlerWithAttrsAndGroups(t *testing.T) {
	writer := &recordingWriter{}
	parent := newJournalHandler(writer, "json", slog.LevelDebug)
	bound := []slog.Attr{slog.String("host", "a")}
	child := parent.WithAttrs(bound).WithGroup("request").WithAttrs([]slog.Attr{slog.String("id", "b")})
	bound[0] = slog.String("host", "changed")
	sibling := parent.WithGroup("other")

	slog.New(parent).Info("parent", "value", "one")
	slog.New(child).Info("child", "value", "two")
	slog.New(sibling).Warn("sibling", "value", "three")

	writes := writer.snapshot()
	if len(writes) != 3 {
		t.Fatalf("writes = %d, want 3", len(writes))
	}
	var parentRecord, childRecord, siblingRecord map[string]any
	for i, target := range []*map[string]any{&parentRecord, &childRecord, &siblingRecord} {
		if err := json.Unmarshal(writes[i][3:], target); err != nil {
			t.Fatalf("record %d = %q: %v", i, writes[i], err)
		}
	}
	if parentRecord["host"] != nil || parentRecord["value"] != "one" {
		t.Errorf("parent record = %#v", parentRecord)
	}
	if childRecord["host"] != "a" || childRecord["request"] == nil {
		t.Fatalf("child record = %#v", childRecord)
	}
	request, ok := childRecord["request"].(map[string]any)
	if !ok || request["id"] != "b" || request["value"] != "two" {
		t.Errorf("child request = %#v", childRecord["request"])
	}
	if siblingRecord["other"] == nil || siblingRecord["value"] != nil {
		t.Errorf("sibling record = %#v", siblingRecord)
	}
}

func TestJournalHandlerContract(t *testing.T) {
	writer := &recordingWriter{}
	handler := newJournalHandler(writer, "json", slog.LevelInfo)
	if err := slogtest.TestHandler(handler, func() []map[string]any {
		writes := writer.snapshot()
		results := make([]map[string]any, 0, len(writes))
		for _, write := range writes {
			if !bytes.HasPrefix(write, []byte("<6>")) {
				t.Fatalf("write = %q, want info prefix", write)
			}
			var result map[string]any
			if err := json.Unmarshal(write[3:], &result); err != nil {
				t.Fatalf("write = %q: %v", write, err)
			}
			results = append(results, result)
		}
		return results
	}); err != nil {
		t.Fatal(err)
	}
}

func TestJournalHandlerWritesAreSerialized(t *testing.T) {
	writer := &recordingWriter{}
	logger := newRelayLogger(writer, "json", slog.LevelDebug, true)
	const count = 200
	var wait sync.WaitGroup
	for i := 0; i < count; i++ {
		wait.Add(1)
		go func(index int) {
			defer wait.Done()
			logger.With("index", index).Warn("warning")
		}(i)
	}
	wait.Wait()

	writes := writer.snapshot()
	if len(writes) != count {
		t.Fatalf("writes = %d, want %d", len(writes), count)
	}
	for _, write := range writes {
		if !bytes.HasPrefix(write, []byte("<4>")) || !bytes.HasSuffix(write, []byte{'\n'}) {
			t.Fatalf("incomplete write = %q", write)
		}
		var record map[string]any
		if err := json.Unmarshal(write[3:], &record); err != nil {
			t.Fatalf("write = %q: %v", write, err)
		}
	}
}

func TestJournalHandlerReturnsWriterErrors(t *testing.T) {
	wantErr := errors.New("write failed")
	for _, test := range []struct {
		name   string
		writer *recordingWriter
		want   error
	}{
		{name: "error", writer: &recordingWriter{err: wantErr}, want: wantErr},
		{name: "short write", writer: &recordingWriter{short: true}, want: io.ErrShortWrite},
	} {
		t.Run(test.name, func(t *testing.T) {
			handler := newJournalHandler(test.writer, "text", slog.LevelInfo)
			record := slog.NewRecord(testTime(), slog.LevelError, "failure", 0)
			err := handler.Handle(context.Background(), record)
			if !errors.Is(err, test.want) {
				t.Fatalf("Handle() error = %v, want %v", err, test.want)
			}
			writes := test.writer.snapshot()
			if len(writes) != 1 || !bytes.HasPrefix(writes[0], []byte("<3>")) {
				t.Fatalf("writes = %q, want one complete error record", writes)
			}
		})
	}
}

func TestReportError(t *testing.T) {
	for _, test := range []struct {
		name      string
		args      []string
		format    string
		journal   bool
		wantPlain bool
	}{
		{name: "default serve text", format: "text"},
		{name: "explicit serve json", args: []string{"serve"}, format: "json"},
		{name: "serve journal", args: []string{"serve", "bad"}, format: "json", journal: true},
		{name: "non serve", args: []string{"version"}, format: "json", wantPlain: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			file, err := os.CreateTemp(t.TempDir(), "stderr")
			if err != nil {
				t.Fatal(err)
			}
			defer file.Close()
			t.Setenv("HERDR_RELAY_LOG_FORMAT", test.format)
			t.Setenv("JOURNAL_STREAM", "")
			reportErrorWithJournal(file, test.args, errors.New("bad config"), test.journal)
			if _, err := file.Seek(0, io.SeekStart); err != nil {
				t.Fatal(err)
			}
			data, err := io.ReadAll(file)
			if err != nil {
				t.Fatal(err)
			}
			if test.wantPlain {
				if string(data) != "herdr-mobile-relay: bad config\n" {
					t.Errorf("output = %q", data)
				}
				return
			}
			if test.journal && !bytes.HasPrefix(data, []byte("<3>")) {
				t.Fatalf("output = %q, want error prefix", data)
			}
			body := data
			if test.journal {
				body = body[3:]
			}
			if test.format == "json" {
				var record map[string]any
				if err := json.Unmarshal(body, &record); err != nil {
					t.Fatalf("JSON output = %q: %v", body, err)
				}
				if record["level"] != "ERROR" || record["msg"] != "relay failed" {
					t.Errorf("record = %#v", record)
				}
			} else if !bytes.Contains(body, []byte("level=ERROR msg=\"relay failed\"")) {
				t.Errorf("output = %q", body)
			}
		})
	}
}

func TestReportErrorIgnoresInvalidLogLevel(t *testing.T) {
	file, err := os.CreateTemp(t.TempDir(), "stderr")
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	t.Setenv("HERDR_RELAY_LOG_LEVEL", "verbose")
	t.Setenv("HERDR_RELAY_LOG_FORMAT", "json")
	reportError(file, nil, errors.New("invalid log level"))
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		t.Fatal(err)
	}
	var record map[string]any
	if err := json.NewDecoder(file).Decode(&record); err != nil {
		t.Fatal(err)
	}
	if record["level"] != "ERROR" {
		t.Fatalf("record = %#v, want ERROR", record)
	}
}

func TestJournalLoggerLevelThresholds(t *testing.T) {
	for _, format := range []string{"text", "json"} {
		for _, test := range []struct {
			minimum  slog.Level
			prefixes []string
		}{
			{minimum: slog.LevelDebug, prefixes: []string{"<7>", "<6>", "<4>", "<3>"}},
			{minimum: slog.LevelInfo, prefixes: []string{"<6>", "<4>", "<3>"}},
			{minimum: slog.LevelWarn, prefixes: []string{"<4>", "<3>"}},
			{minimum: slog.LevelError, prefixes: []string{"<3>"}},
		} {
			t.Run(format+"/"+test.minimum.String(), func(t *testing.T) {
				writer := &recordingWriter{}
				logger := newRelayLogger(writer, format, test.minimum, true)
				logger.Debug("debug")
				logger.Info("info")
				logger.Warn("warn")
				logger.Error("error")
				writes := writer.snapshot()
				if len(writes) != len(test.prefixes) {
					t.Fatalf("writes = %d, want %d", len(writes), len(test.prefixes))
				}
				for i, prefix := range test.prefixes {
					if !bytes.HasPrefix(writes[i], []byte(prefix)) {
						t.Errorf("write %d = %q, want prefix %q", i, writes[i], prefix)
					}
				}
			})
		}
	}
}

func testTime() time.Time {
	return time.Date(2025, 1, 2, 3, 4, 5, 0, time.UTC)
}
