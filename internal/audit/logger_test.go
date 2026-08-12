package audit

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

func TestAppendWritesPrivateStructuredRecords(t *testing.T) {
	logger, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	ok := true
	if err := logger.Append(Record{
		Stage: "result", Action: "send_text", RequestID: "request-1",
		ClientID: "phone-1", ConnectionID: "connection-1", PaneID: "pane-1",
		OK: &ok, Phase: "completed", Details: map[string]any{"text_bytes": 12},
	}); err != nil {
		t.Fatal(err)
	}

	info, err := os.Stat(logger.path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("audit mode = %o", info.Mode().Perm())
	}
	data, err := os.ReadFile(logger.path)
	if err != nil {
		t.Fatal(err)
	}
	var record Record
	if err := json.Unmarshal(data, &record); err != nil {
		t.Fatal(err)
	}
	if record.Timestamp == "" || record.Action != "send_text" || record.ClientID != "phone-1" || record.OK == nil || !*record.OK {
		t.Fatalf("record = %#v", record)
	}
}

func TestAppendSerializesConcurrentWriters(t *testing.T) {
	logger, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	const writers = 12
	var wait sync.WaitGroup
	wait.Add(writers)
	for index := 0; index < writers; index++ {
		go func() {
			defer wait.Done()
			if appendErr := logger.Append(Record{Stage: "attempt", Action: "send_keys", ClientID: "phone"}); appendErr != nil {
				t.Errorf("append: %v", appendErr)
			}
		}()
	}
	wait.Wait()

	file, err := os.Open(logger.path)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	scanner := bufio.NewScanner(file)
	count := 0
	for scanner.Scan() {
		var record Record
		if err := json.Unmarshal(scanner.Bytes(), &record); err != nil {
			t.Fatalf("invalid JSONL row: %v", err)
		}
		count++
	}
	if err := scanner.Err(); err != nil {
		t.Fatal(err)
	}
	if count != writers {
		t.Fatalf("rows = %d, want %d", count, writers)
	}
}

func TestAppendRejectsSymlinkReplacement(t *testing.T) {
	logger, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(t.TempDir(), "outside")
	if err := os.WriteFile(outside, []byte("unchanged"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, logger.path); err != nil {
		t.Fatal(err)
	}
	if err := logger.Append(Record{Stage: "attempt", Action: "send_text", ClientID: "phone"}); err == nil {
		t.Fatal("append through a symlink unexpectedly succeeded")
	}
	data, err := os.ReadFile(outside)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "unchanged" {
		t.Fatalf("outside file changed to %q", data)
	}
}

func TestAppendRotatesBeforeCrossingLimit(t *testing.T) {
	logger, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(logger.path, []byte(strings.Repeat("x", maxAuditBytes)), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := logger.Append(Record{Stage: "attempt", Action: "send_text", ClientID: "phone"}); err != nil {
		t.Fatal(err)
	}
	if info, err := os.Stat(logger.path + ".1"); err != nil || info.Size() != maxAuditBytes {
		t.Fatalf("rotation info = %#v, %v", info, err)
	}
	data, err := os.ReadFile(logger.path)
	if err != nil {
		t.Fatal(err)
	}
	if !json.Valid(data) {
		t.Fatalf("new audit file is not JSONL: %q", data)
	}
}
