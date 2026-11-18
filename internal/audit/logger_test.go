package audit

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
)

func TestAppendWritesPrivateStructuredRecords(t *testing.T) {
	logger, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	ok := true
	if err := logger.Append(Record{
		Stage: "result", Action: "submit_prompt", RequestID: "request-1",
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
	if record.Timestamp == "" || record.Action != "submit_prompt" || record.ClientID != "phone-1" || record.OK == nil || !*record.OK {
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
	if file, err := logger.openAppendFile(); err == nil {
		_ = file.Close()
		t.Fatal("opening a symlink unexpectedly succeeded")
	}
	if err := logger.Append(Record{Stage: "attempt", Action: "submit_prompt", ClientID: "phone"}); err == nil {
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

func TestOpenAppendFileRejectsFIFO(t *testing.T) {
	for _, withReader := range []bool{false, true} {
		name := "without reader"
		if withReader {
			name = "with reader"
		}
		t.Run(name, func(t *testing.T) {
			logger, err := Open(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			if err := syscall.Mkfifo(logger.path, 0o600); err != nil {
				t.Fatal(err)
			}
			if withReader {
				reader, err := os.OpenFile(logger.path, os.O_RDONLY|syscall.O_NONBLOCK, 0)
				if err != nil {
					t.Fatal(err)
				}
				defer reader.Close()
			}
			if file, err := logger.openAppendFile(); err == nil {
				_ = file.Close()
				t.Fatal("opening a FIFO unexpectedly succeeded")
			}
		})
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
	if err := logger.Append(Record{Stage: "attempt", Action: "submit_prompt", ClientID: "phone"}); err != nil {
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
