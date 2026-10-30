package conversation

import (
	"context"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

func TestBrowserFailurePublishesAfterWorkerRelinquishesJob(t *testing.T) {
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
	options.ActiveWorkers = 2
	browser, err := NewBrowser(reader, t.TempDir(), options)
	if err != nil {
		t.Fatal(err)
	}
	defer browser.Close()
	finishing := make(chan *browseJob, 2)
	release := make(chan struct{})
	unblock := sync.OnceFunc(func() { close(release) })
	defer unblock()
	browser.workerBeforeFinishObserver = func(job *browseJob) {
		finishing <- job
		<-release
	}
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	scope := BrowseScope{Provider: "claude", CWD: "/work", SessionID: testSessionID}
	latest, err := browser.ReadPage(ctx, BrowseRequest{Scope: scope, Limit: 1})
	if err != nil || latest.NextCursor == "" {
		t.Fatalf("latest page = %#v, err = %v", latest, err)
	}
	if _, err := browser.ReadPage(ctx, BrowseRequest{Scope: scope, Cursor: latest.NextCursor, Limit: 1}); err != nil {
		t.Fatal(err)
	}
	var job *browseJob
	select {
	case job = <-finishing:
	case <-ctx.Done():
		t.Fatal("worker did not reach failure cleanup")
	}
	job.mu.RLock()
	firstSnapshot := job.snapshotID
	failure := job.err
	job.mu.RUnlock()
	if failure == nil || failure.Code != "index_capacity_exceeded" || !failure.Retryable {
		t.Fatalf("worker failure = %+v", failure)
	}
	request := BrowseRequest{Scope: scope, Cursor: latest.NextCursor, Limit: 1, Retry: true}
	page, err := browser.ReadPage(ctx, request)
	if err != nil || page.State != BrowsePreparing {
		t.Fatalf("retry during worker cleanup = %#v, err = %v", page, err)
	}
	job.mu.RLock()
	unchanged := job.snapshotID == firstSnapshot && job.running && !job.queued && job.state == BrowsePreparing
	job.mu.RUnlock()
	if !unchanged {
		t.Fatal("retry admitted a second worker before the first relinquished the job")
	}
	unblock()
	for {
		job.mu.RLock()
		failed, running := job.state == BrowseFailed, job.running
		job.mu.RUnlock()
		if failed {
			if running {
				t.Fatal("retryable failure published while the worker still owns the job")
			}
			break
		}
		select {
		case <-ctx.Done():
			t.Fatal("failure was not published")
		case <-time.After(time.Millisecond):
		}
	}
	page, err = browser.ReadPage(ctx, request)
	if err != nil || page.State != BrowsePreparing {
		t.Fatalf("explicit retry after failure = %#v, err = %v", page, err)
	}
	select {
	case retried := <-finishing:
		if retried != job {
			t.Fatal("retry did not reuse the failed job")
		}
	case <-ctx.Done():
		t.Fatal("explicit retry did not run")
	}
	job.mu.RLock()
	replaced := job.snapshotID != firstSnapshot
	job.mu.RUnlock()
	if !replaced {
		t.Fatal("explicit retry did not create a fresh index")
	}
}
