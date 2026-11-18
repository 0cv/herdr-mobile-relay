package conversation

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	bolt "go.etcd.io/bbolt"
)

const (
	jobSourceChanged = "The conversation source changed while it was being prepared."
	jobNoClient      = "History preparation paused because no client is waiting for it."
	jobIndexCorrupt  = "The prepared conversation index is corrupt."
)

func jobOutcomeRows() []map[string]any {
	rows := []map[string]any{
		{"type": "user", "uuid": "u0", "message": map[string]any{"content": strings.Repeat("hello", 20000)}},
	}
	for index := 1; index <= 70; index++ {
		rows = append(rows, map[string]any{
			"type": "assistant", "uuid": fmt.Sprintf("a%d", index),
			"message": map[string]any{"content": fmt.Sprintf("answer %d", index)},
		})
	}
	return rows
}

func waitForJobFailure(t *testing.T, job *browseJob) *BrowseError {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	for {
		job.mu.RLock()
		settled := job.state == BrowseFailed && !job.running
		failure := job.err
		job.mu.RUnlock()
		if settled {
			return failure
		}
		select {
		case <-ctx.Done():
			t.Fatal("preparation did not fail")
		case <-time.After(time.Millisecond):
		}
	}
}

func soleJob(browser *Browser) *browseJob {
	browser.mu.Lock()
	defer browser.mu.Unlock()
	if len(browser.jobs) != 1 {
		return nil
	}
	for _, job := range browser.jobs {
		return job
	}
	return nil
}

func jobSnapshotID(job *browseJob) string {
	job.mu.RLock()
	defer job.mu.RUnlock()
	return job.snapshotID
}

func cachedSnapshotFiles(t *testing.T, browser *Browser) []string {
	t.Helper()
	files, err := filepath.Glob(filepath.Join(browser.cacheRoot, "snapshot-*.db"))
	if err != nil {
		t.Fatal(err)
	}
	return files
}

type jobFailureEnv struct {
	t        *testing.T
	browser  *Browser
	path     string
	original []byte
	// ready is closed once the request that created the job has returned, so
	// that its interest renewal cannot land after a hook withdrew interest.
	ready chan struct{}
}

// onceAt runs act the first time the worker reaches the given observer kind
// (and, for capacity operations, the given stage before the capacity lock).
func (env *jobFailureEnv) onceAt(kind, stage string, act func(job *browseJob)) {
	var once sync.Once
	setBrowserObserver(env.browser, kind, func(observation browserObservation) {
		if kind == "capacity_operation" && (observation.stage != stage || observation.locked) {
			return
		}
		once.Do(func() {
			<-env.ready
			job := observation.job
			if job == nil {
				job = soleJob(env.browser)
			}
			if job == nil {
				env.t.Error("hook found no job")
				return
			}
			act(job)
		})
	})
}

// beforeStart runs act once, inside the request that admits the job: after the
// source was captured and before the worker starts.
func (env *jobFailureEnv) beforeStart(act func()) {
	var once sync.Once
	setBrowserObserver(env.browser, "capacity_operation", func(observation browserObservation) {
		if observation.stage == "admission" && !observation.locked {
			once.Do(act)
		}
	})
}

func (env *jobFailureEnv) writeInPlace(data []byte) {
	if err := os.WriteFile(env.path, data, 0o600); err != nil {
		env.t.Error(err)
	}
}

func (env *jobFailureEnv) withdrawInterest(job *browseJob) {
	job.mu.Lock()
	job.lastInterest = time.Now().Add(-time.Hour)
	job.mu.Unlock()
}

func (env *jobFailureEnv) damageIndex(job *browseJob, damage func(tx *bolt.Tx) error) {
	job.mu.RLock()
	index := job.index
	job.mu.RUnlock()
	if err := index.db.Update(damage); err != nil {
		env.t.Error(err)
	}
}

func TestBrowserJobFailureExits(t *testing.T) {
	type failureCase struct {
		name      string
		options   func(*BrowserOptions)
		arm       func(env *jobFailureEnv)
		restore   func(env *jobFailureEnv)
		code      string
		message   string
		retryable bool
	}
	restoreBytes := func(env *jobFailureEnv) {
		if err := os.WriteFile(env.path, env.original, 0o600); err != nil {
			env.t.Fatal(err)
		}
	}
	cases := []failureCase{
		{
			name: "source rewritten before the scan starts",
			arm: func(env *jobFailureEnv) {
				env.beforeStart(func() {
					env.writeInPlace(bytes.Replace(env.original, []byte("answer 70"), []byte("answer 71"), 1))
				})
			},
			restore: restoreBytes,
			code:    "source_changed", message: jobSourceChanged,
		},
		{
			name: "source path retargeted by a symlink before the scan starts",
			arm: func(env *jobFailureEnv) {
				env.beforeStart(func() {
					if err := os.Rename(env.path, env.path+".original"); err != nil {
						env.t.Error(err)
						return
					}
					if err := os.Symlink(env.path+".original", env.path); err != nil {
						env.t.Error(err)
					}
				})
			},
			restore: func(env *jobFailureEnv) {
				if err := os.Remove(env.path); err != nil {
					env.t.Fatal(err)
				}
				if err := os.Rename(env.path+".original", env.path); err != nil {
					env.t.Fatal(err)
				}
			},
			code: "source_changed", message: jobSourceChanged,
		},
		{
			name: "source truncated during the scan",
			arm: func(env *jobFailureEnv) {
				env.onceAt("capacity_operation", "batch", func(*browseJob) { env.writeInPlace(nil) })
			},
			restore: restoreBytes,
			code:    "source_changed", message: jobSourceChanged,
		},
		{
			name: "source rewritten after the scan",
			arm: func(env *jobFailureEnv) {
				env.onceAt("source_read", "", func(*browseJob) {
					env.writeInPlace(bytes.Replace(env.original, []byte("answer 70"), []byte("answer 71"), 1))
				})
			},
			restore: restoreBytes,
			code:    "source_changed", message: jobSourceChanged,
		},
		{
			name: "source replaced by another file",
			arm: func(env *jobFailureEnv) {
				env.onceAt("capacity_operation", "batch", func(*browseJob) {
					if err := os.Rename(env.path, env.path+".original"); err != nil {
						env.t.Error(err)
						return
					}
					env.writeInPlace(env.original)
				})
			},
			restore: func(env *jobFailureEnv) {
				if err := os.Rename(env.path+".original", env.path); err != nil {
					env.t.Fatal(err)
				}
			},
			code: "source_changed", message: jobSourceChanged,
		},
		{
			name: "no client waiting during the scan",
			arm: func(env *jobFailureEnv) {
				env.onceAt("capacity_operation", "batch", env.withdrawInterest)
			},
			code: "index_failed", message: jobNoClient, retryable: true,
		},
		{
			name: "no client waiting during verification",
			arm: func(env *jobFailureEnv) {
				env.onceAt("source_read", "", env.withdrawInterest)
			},
			code: "index_failed", message: jobNoClient, retryable: true,
		},
		{
			name: "scan cancelled by the browser context",
			arm: func(env *jobFailureEnv) {
				env.onceAt("capacity_operation", "batch", func(*browseJob) { env.browser.cancel() })
			},
			code: "request_cancelled", message: "History preparation was cancelled.", retryable: true,
		},
		{
			name: "index metadata damaged before the count",
			arm: func(env *jobFailureEnv) {
				env.onceAt("source_read", "", func(job *browseJob) {
					env.damageIndex(job, func(tx *bolt.Tx) error { return tx.Bucket(metaBucket).Delete(metaValueKey) })
				})
			},
			code: "index_failed", message: jobIndexCorrupt, retryable: true,
		},
		{
			name: "index entries damaged before the count",
			arm: func(env *jobFailureEnv) {
				env.onceAt("source_read", "", func(job *browseJob) {
					env.damageIndex(job, func(tx *bolt.Tx) error {
						return tx.Bucket(entriesBucket).Put(entryOffsetKey(1<<40), []byte("{"))
					})
				})
			},
			code: "index_failed", message: jobIndexCorrupt, retryable: true,
		},
		{
			name: "finalize write fails",
			arm: func(env *jobFailureEnv) {
				env.onceAt("capacity_operation", "finalize", func(job *browseJob) {
					job.mu.RLock()
					index := job.index
					job.mu.RUnlock()
					if err := index.close(); err != nil {
						env.t.Error(err)
					}
				})
			},
			code: "index_failed", message: "The prepared conversation index could not be finalized.", retryable: true,
		},
		{
			name: "storage quota exceeded",
			options: func(options *BrowserOptions) {
				options.SnapshotQuota = initialSnapshotBytes(t)
				options.AggregateQuota = 1024 * 1024
			},
			arm:  func(*jobFailureEnv) {},
			code: "index_capacity_exceeded", message: "The conversation history cache reached its storage limit.", retryable: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			reader, home := testReader(t)
			path := filepath.Join(home, ".claude", "projects", "-work", testSessionID+".jsonl")
			writeRows(t, path, jobOutcomeRows()...)
			original, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			options := DefaultBrowserOptions()
			options.RecentBytes = 1
			if tc.options != nil {
				tc.options(&options)
			}
			browser := newTestBrowser(t, reader, options)
			env := &jobFailureEnv{t: t, browser: browser, path: path, original: original, ready: make(chan struct{})}
			tc.arm(env)
			ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
			defer cancel()
			scope := BrowseScope{Provider: "claude", CWD: "/work", SessionID: testSessionID}
			latest, err := browser.ReadPage(ctx, BrowseRequest{Scope: scope, Limit: 1})
			if err != nil || latest.NextCursor == "" {
				t.Fatalf("latest page = %#v, err = %v", latest, err)
			}
			preparing, err := browser.ReadPage(ctx, BrowseRequest{Scope: scope, Cursor: latest.NextCursor, Limit: 1})
			close(env.ready)
			if err != nil || preparing.State != BrowsePreparing || preparing.NextCursor == "" {
				t.Fatalf("preparing page = %#v, err = %v", preparing, err)
			}
			job := soleJob(browser)
			if job == nil {
				t.Fatal("no preparation job was admitted")
			}

			failure := waitForJobFailure(t, job)
			want := &BrowseError{Code: tc.code, Message: tc.message, Retryable: tc.retryable}
			if !reflect.DeepEqual(failure, want) {
				t.Fatalf("job failure = %+v, want %+v", failure, want)
			}
			if files := cachedSnapshotFiles(t, browser); len(files) != 0 {
				t.Fatalf("failed job left %v in the cache", files)
			}

			if tc.restore != nil {
				tc.restore(env)
			}
			if tc.retryable {
				job.mu.Lock()
				job.autoRetried = true
				job.mu.Unlock()
			}
			page, err := browser.ReadPage(ctx, BrowseRequest{Scope: scope, Cursor: preparing.NextCursor, Limit: 1})
			if err != nil {
				t.Fatal(err)
			}
			wantPage := BrowsePage{
				Available: true, State: BrowseFailed, Mode: BrowseRecent, Entries: []Entry{},
				ReasonCode: tc.code, Reason: tc.message, Error: want,
			}
			if tc.retryable {
				wantPage.HasMore, wantPage.NextCursor = true, preparing.NextCursor
			}
			if !reflect.DeepEqual(page, wantPage) {
				t.Fatalf("failed page = %#v, want %#v", page, wantPage)
			}
		})
	}
}

func quotaFailureOptions(t *testing.T) BrowserOptions {
	options := DefaultBrowserOptions()
	options.RecentBytes = 1
	options.SnapshotQuota = initialSnapshotBytes(t)
	options.AggregateQuota = 1024 * 1024
	return options
}

// retryOutcomeSource is one way to reach a preparation that fails the same way
// every time, plus the page request that reads that preparation.
type retryOutcomeSource struct {
	browser *Browser
	scope   BrowseScope
	// cursor returns the cursor of a page that reads the failing preparation
	// once the job exists.
	cursor func(t *testing.T) string
	job    func() *browseJob
}

func singleFileRetrySource(t *testing.T) retryOutcomeSource {
	reader, home := testReader(t)
	path := filepath.Join(home, ".claude", "projects", "-work", testSessionID+".jsonl")
	writeRows(t, path,
		map[string]any{"type": "user", "uuid": "u1", "message": map[string]any{"content": strings.Repeat("hello", 20000)}},
		map[string]any{"type": "assistant", "uuid": "a1", "message": map[string]any{"content": "answer"}},
	)
	browser := newTestBrowser(t, reader, quotaFailureOptions(t))
	scope := BrowseScope{Provider: "claude", CWD: "/work", SessionID: testSessionID}
	return retryOutcomeSource{
		browser: browser, scope: scope, job: func() *browseJob { return soleJob(browser) },
		cursor: func(t *testing.T) string {
			latest, err := browser.ReadPage(t.Context(), BrowseRequest{Scope: scope, Limit: 1})
			if err != nil || latest.NextCursor == "" {
				t.Fatalf("latest page = %#v, err = %v", latest, err)
			}
			return latest.NextCursor
		},
	}
}

func chainRetrySource(t *testing.T) retryOutcomeSource {
	reader, home := testReader(t)
	root := filepath.Join(home, ".claude", "projects", "-work")
	parent := make([]map[string]any, 0, 4)
	for index := 0; index < 3; index++ {
		parent = append(parent, map[string]any{
			"type": "assistant", "uuid": fmt.Sprintf("parent-%d", index),
			"message": map[string]any{"content": strings.Repeat("parent", 20000)},
		})
	}
	parent = append(parent, map[string]any{"type": "continued-in", "sessionId": testSessionID, "continuedInSessionId": budgetChildID})
	writeRows(t, filepath.Join(root, testSessionID+".jsonl"), parent...)
	writeRows(t, filepath.Join(root, budgetChildID+".jsonl"), map[string]any{
		"type": "assistant", "uuid": "child-1", "message": map[string]any{"content": "child answer"},
	})
	options := quotaFailureOptions(t)
	options.RecentBytes = 128
	browser := newTestBrowser(t, reader, options)
	scope := BrowseScope{Provider: "claude", CWD: "/work", SessionID: testSessionID}
	return retryOutcomeSource{
		browser: browser, scope: scope, job: func() *browseJob { return soleJob(browser) },
		cursor: func(t *testing.T) string {
			latest, err := browser.ReadPage(t.Context(), BrowseRequest{Scope: scope, Limit: 1})
			if err != nil || latest.NextCursor == "" {
				t.Fatalf("latest page = %#v, err = %v", latest, err)
			}
			waiting, err := browser.ReadPage(t.Context(), BrowseRequest{Scope: scope, Cursor: latest.NextCursor, Limit: 1})
			if err != nil || waiting.State != BrowsePreparing || waiting.NextCursor == "" {
				t.Fatalf("first parent page = %#v, err = %v", waiting, err)
			}
			return waiting.NextCursor
		},
	}
}

func TestBrowserRetryOutcomes(t *testing.T) {
	for name, build := range map[string]func(*testing.T) retryOutcomeSource{
		"single file":   singleFileRetrySource,
		"chain segment": chainRetrySource,
	} {
		t.Run(name, func(t *testing.T) {
			source := build(t)
			browser, scope := source.browser, source.scope
			read := func(cursor string, retry bool) BrowsePage {
				t.Helper()
				page, err := browser.ReadPage(t.Context(), BrowseRequest{Scope: scope, Cursor: cursor, Limit: 1, Retry: retry})
				if err != nil {
					t.Fatal(err)
				}
				return page
			}
			cursor := source.cursor(t)
			first := read(cursor, false)
			if first.State != BrowsePreparing {
				t.Fatalf("first read = %#v, want a preparing page", first)
			}
			job := source.job()
			if job == nil {
				t.Fatal("no preparation job was admitted")
			}
			firstSnapshot := jobSnapshotID(job)
			if failure := waitForJobFailure(t, job); failure == nil || failure.Code != "index_capacity_exceeded" || !failure.Retryable {
				t.Fatalf("first failure = %+v", failure)
			}

			automatic := read(first.NextCursor, false)
			if automatic.State != BrowsePreparing {
				t.Fatalf("read of a retryable failure = %#v, want a preparing page from an automatic retry", automatic)
			}
			secondSnapshot := jobSnapshotID(job)
			if secondSnapshot == firstSnapshot {
				t.Fatal("automatic retry did not start a new preparation")
			}
			if failure := waitForJobFailure(t, job); failure == nil || failure.Code != "index_capacity_exceeded" || !failure.Retryable {
				t.Fatalf("retry failure = %+v", failure)
			}

			for attempt := 0; attempt < 2; attempt++ {
				failed := read(automatic.NextCursor, false)
				if failed.State != BrowseFailed || failed.ReasonCode != "index_capacity_exceeded" || failed.Error == nil || !failed.Error.Retryable {
					t.Fatalf("read %d after the retry failed = %#v, want a failed page", attempt, failed)
				}
				if jobSnapshotID(job) != secondSnapshot {
					t.Fatal("a read without Retry started a new preparation")
				}
			}

			explicit := read(automatic.NextCursor, true)
			if explicit.State != BrowsePreparing {
				t.Fatalf("explicit retry = %#v, want a preparing page", explicit)
			}
			if jobSnapshotID(job) == secondSnapshot {
				t.Fatal("explicit retry did not start a new preparation")
			}
		})
	}
}
