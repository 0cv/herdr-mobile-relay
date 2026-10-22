package conversation

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"testing"
	"time"
)

func TestBrowserQuotaFailureWaitsForCleanupBeforeAdvertisingRetry(t *testing.T) {
	reader, home := testReader(t)
	path := filepath.Join(home, ".claude", "projects", "-work", testSessionID+".jsonl")
	writeRows(t, path,
		map[string]any{"type": "user", "uuid": "u1", "message": map[string]any{"content": "hello"}},
		map[string]any{"type": "assistant", "uuid": "a1", "message": map[string]any{"content": "answer"}},
	)
	options := DefaultBrowserOptions()
	options.RecentBytes = 1
	options.SnapshotQuota = 1024
	options.AggregateQuota = 1024 * 1024
	browser, err := NewBrowser(reader, t.TempDir(), options)
	if err != nil {
		t.Fatal(err)
	}
	defer browser.Close()
	started, release := make(chan struct{}), make(chan struct{})
	var pauseOnce, releaseOnce sync.Once
	defer releaseOnce.Do(func() { close(release) })
	browser.backgroundReadObserver = func(int64) {
		pauseOnce.Do(func() { close(started); <-release })
	}
	scope := BrowseScope{Provider: "claude", CWD: "/work", SessionID: testSessionID}
	latest, err := browser.ReadPage(context.Background(), BrowseRequest{Scope: scope, Limit: 1})
	if err != nil || latest.NextCursor == "" {
		t.Fatalf("latest page = %#v, err = %v", latest, err)
	}
	request := BrowseRequest{Scope: scope, Cursor: latest.NextCursor, Limit: 1}
	if _, err := browser.ReadPage(context.Background(), request); err != nil {
		t.Fatal(err)
	}
	select {
	case <-started:
	case <-time.After(3 * time.Second):
		t.Fatal("index worker did not reach the read barrier")
	}
	browser.mu.Lock()
	var job *browseJob
	for _, candidate := range browser.jobs {
		job = candidate
	}
	browser.mu.Unlock()
	if job == nil {
		t.Fatal("preparation job is missing")
	}
	job.mu.RLock()
	index := job.index
	job.mu.RUnlock()
	if index == nil {
		t.Fatal("preparation index is missing")
	}
	index.mu.RLock()
	var unlockOnce sync.Once
	defer unlockOnce.Do(index.mu.RUnlock)
	releaseOnce.Do(func() { close(release) })
	deadline := time.Now().Add(3 * time.Second)
	for {
		job.mu.RLock()
		detached := job.index == nil && job.err != nil && job.err.Code == "index_capacity_exceeded"
		job.mu.RUnlock()
		if detached {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("quota failure did not reach the cleanup barrier")
		}
		runtime.Gosched()
	}
	for _, retry := range []bool{false, true} {
		request.Retry = retry
		page, err := browser.ReadPage(context.Background(), request)
		if err != nil || page.State != BrowsePreparing || page.Error != nil {
			t.Errorf("cleanup blocked, retry=%v: page=%#v, err=%v", retry, page, err)
		}
	}
	unlockOnce.Do(index.mu.RUnlock)
	browser.workerWG.Wait()
	job.mu.RLock()
	failed := job.state == BrowseFailed && !job.running && job.err != nil && job.err.Retryable
	job.mu.RUnlock()
	if !failed {
		t.Fatal("completed cleanup did not publish an idle retryable failure")
	}
	if _, err := os.Stat(index.path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("failed index remains after cleanup: %v", err)
	}
	request.Retry = true
	retry, err := browser.ReadPage(context.Background(), request)
	if err != nil || retry.State != BrowsePreparing {
		t.Fatalf("explicit retry after cleanup = %#v, err = %v", retry, err)
	}
}
