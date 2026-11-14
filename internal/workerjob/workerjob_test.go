package workerjob

import (
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

type testJob struct {
	Name string `json:"name"`
}

func TestLoadReadsJobAndNamesTheJobInErrors(t *testing.T) {
	path := filepath.Join(t.TempDir(), "job.json")
	if err := os.WriteFile(path, []byte(`{"name":"deploy"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	job, err := Load[testJob](path, "sample")
	if err != nil || job.Name != "deploy" {
		t.Fatalf("job = %#v, err = %v", job, err)
	}

	if _, err := Load[testJob](filepath.Join(t.TempDir(), "missing.json"), "sample"); err == nil || !strings.HasPrefix(err.Error(), "read sample job:") {
		t.Fatalf("missing file error = %v", err)
	}
	if err := os.WriteFile(path, []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load[testJob](path, "sample"); err == nil || !strings.HasPrefix(err.Error(), "parse sample job:") {
		t.Fatalf("malformed file error = %v", err)
	}
}

func TestLockIsExclusiveAndRecordsProcessID(t *testing.T) {
	path := filepath.Join(t.TempDir(), "worker.lock")
	busy := errors.New("busy")
	first, err := Lock(path, busy)
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil || strings.TrimSpace(string(data)) != strconv.Itoa(os.Getpid()) {
		t.Fatalf("lock file = %q, err = %v", data, err)
	}
	if _, err := Lock(path, busy); !errors.Is(err, busy) {
		t.Fatalf("second lock error = %v, want %v", err, busy)
	}
	first.Close()
	second, err := Lock(path, busy)
	if err != nil {
		t.Fatalf("lock after release: %v", err)
	}
	second.Close()
}

func TestRecoverOrphanSkipsWhileWorkerHoldsLock(t *testing.T) {
	path := filepath.Join(t.TempDir(), "worker.lock")
	held, err := Lock(path, errBusy)
	if err != nil {
		t.Fatal(err)
	}
	called := false
	RecoverOrphan(path, func() { called = true })
	if called {
		t.Fatal("recovery ran while a worker held the lock")
	}
	held.Close()

	RecoverOrphan(path, func() { called = true })
	if !called {
		t.Fatal("recovery did not run once the lock was free")
	}
}
