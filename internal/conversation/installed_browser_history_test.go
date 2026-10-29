//go:build herdr_sqlite_logical_test && !herdr_installed_hosted_test && !herdr_tailscale_test && !herdr_tailscale_cli_fixture_binary

package conversation

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/0cv/herdr-mobile-relay/internal/conversationlife"
)

type countedBrowserSource struct {
	*os.File
	closed     *atomic.Int64
	closeFault error
	closeGate  *atomic.Bool
	readFault  error
}

func (s *countedBrowserSource) ReadAt(p []byte, offset int64) (int, error) {
	if s.readFault != nil {
		return 0, s.readFault
	}
	return s.File.ReadAt(p, offset)
}

func (s *countedBrowserSource) Close() error {
	s.closed.Add(1)
	if s.closeGate == nil || s.closeGate.Load() {
		return errors.Join(s.File.Close(), s.closeFault)
	}
	return s.File.Close()
}

// Only the named private transcripts can be opened. Metadata cannot leave
// the fixture home, and no SQL, CLI, service or listener dependency is used.
func installedBrowserHistoryFixture(t *testing.T, options BrowserOptions, paths []string, closeFault ...error) (*Browser, BrowseScope, *conversationlife.Owner, *atomic.Int64, *atomic.Int64) {
	t.Helper()
	return installedBrowserHistoryFixtureGated(t, options, paths, nil, closeFault...)
}

func installedBrowserHistoryFixtureGated(t *testing.T, options BrowserOptions, paths []string, gate *atomic.Bool, closeFault ...error) (*Browser, BrowseScope, *conversationlife.Owner, *atomic.Int64, *atomic.Int64) {
	t.Helper()
	home := filepath.Dir(filepath.Dir(filepath.Dir(filepath.Dir(paths[0]))))
	root := filepath.Join(home, ".claude", "projects")
	planned := make(map[string]bool, len(paths))
	for _, path := range paths {
		planned[path] = true
	}
	opens, closes, unplanned := &atomic.Int64{}, &atomic.Int64{}, &atomic.Int64{}
	f := newOpenCodeActualFixture(t)
	deps := productionInstalledReaderFileDeps()
	deps.roots = func(ctx context.Context, selectedHome, kind string) ([]string, error) {
		if kind != "claude" || selectedHome != home {
			unplanned.Add(1)
			return nil, conversationlife.ErrRefused
		}
		return []string{root}, ctx.Err()
	}
	inHome := func(path string) bool {
		rel, err := filepath.Rel(home, path)
		return err == nil && rel != "." && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
	}
	deps.source = installedSourceDeps{
		open: func(_ context.Context, owner *conversationlife.Owner, path string, onClose func(error)) (*sourceOwnership, error) {
			if !planned[path] {
				unplanned.Add(1)
				return nil, conversationlife.ErrRefused
			}
			return acquireSourceHandle(owner, onClose, func() (sourceHandle, error) {
				file, err := os.Open(path)
				if err != nil {
					return nil, err
				}
				opens.Add(1)
				source := &countedBrowserSource{File: file, closed: closes, closeGate: gate}
				if len(closeFault) > 0 {
					source.closeFault = closeFault[0]
				}
				if len(closeFault) > 1 {
					source.readFault = closeFault[1]
				}
				return source, nil
			})
		},
		eval: func(path string) (string, error) {
			if !inHome(path) {
				unplanned.Add(1)
				return "", conversationlife.ErrRefused
			}
			return filepath.EvalSymlinks(path)
		},
		stat: func(path string) (os.FileInfo, error) {
			if !inHome(path) {
				unplanned.Add(1)
				return nil, conversationlife.ErrRefused
			}
			return os.Stat(path)
		},
		same: os.SameFile,
	}
	deps.readDir = func(path string) ([]os.DirEntry, error) {
		if !inHome(path) {
			unplanned.Add(1)
			return nil, conversationlife.ErrRefused
		}
		return os.ReadDir(path)
	}
	var reader *Reader
	if err := f.owner.Do(context.Background(), func(call *conversationlife.Call) error {
		hermes, err := constructInstalledHermesIn(call, "/planned/home", f.binding, f.metadata)
		if err != nil {
			return err
		}
		return constructInstalledReaderIn(call, home, f.reader, hermes, deps, installedOMODependencies{roots: func(context.Context, string) ([]string, error) {
			return nil, conversationlife.ErrRefused
		}}, &reader)
	}); err != nil {
		t.Fatal(err)
	}
	browser, err := newInstalledBrowser(context.Background(), reader, f.owner, options, func() ([32]byte, error) { return [32]byte{7}, nil }, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := browser.Close(); err != nil && !(len(closeFault) > 0 && closeFault[0] != nil && errors.Is(err, closeFault[0]) && errors.Is(err, ErrSourceCleanupUncertain)) {
			t.Error(err)
		}
		f.owner.Begin()
		f.owner.Join()
		if opens.Load() != closes.Load() {
			t.Errorf("owned sources opened=%d closed=%d", opens.Load(), closes.Load())
		}
		if unplanned.Load() != 0 || f.unplanned != 0 {
			t.Errorf("unplanned file=%d sql=%d", unplanned.Load(), f.unplanned)
		}
		var expected error
		if len(closeFault) > 0 {
			expected = closeFault[0]
		}
		witness := f.owner.Witnesses()
		if expected == nil && witness != nil || expected != nil && !errors.Is(witness, expected) {
			t.Errorf("cleanup witness=%v want=%v", witness, expected)
		}
	})
	return browser, BrowseScope{Provider: "claude", CWD: "/work", SessionID: strings.TrimSuffix(filepath.Base(paths[0]), ".jsonl")}, f.owner, opens, closes
}

func privateBrowserHome(t *testing.T) string {
	t.Helper()
	home, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return home
}

func TestInstalledBrowserFullFileSnapshotAndChangedCursor(t *testing.T) {
	home := privateBrowserHome(t)
	path := filepath.Join(home, ".claude", "projects", "-work", testSessionID+".jsonl")
	writeRows(t, path,
		map[string]any{"type": "user", "uuid": "old", "message": map[string]any{"content": "before reader tail"}},
		map[string]any{"type": "assistant", "uuid": "oversized", "message": map[string]any{"content": strings.Repeat("x", maxConversationBytes+1)}},
		map[string]any{"type": "assistant", "uuid": "new", "message": map[string]any{"content": "latest"}},
	)
	options := DefaultBrowserOptions()
	options.RecentBytes = 1
	browser, scope, _, _, _ := installedBrowserHistoryFixture(t, options, []string{path})
	latest, err := browser.ReadPage(context.Background(), BrowseRequest{Scope: scope, Limit: 2})
	if err != nil || !latest.Available || latest.NextCursor == "" || !latest.HasMore {
		t.Fatalf("latest=%+v err=%v", latest, err)
	}
	preparing, err := browser.ReadPage(context.Background(), BrowseRequest{Scope: scope, Cursor: latest.NextCursor, Limit: 4})
	if err != nil || preparing.State != BrowsePreparing || preparing.NextCursor == "" {
		t.Fatalf("preparing=%+v err=%v", preparing, err)
	}
	page := waitForReadySnapshot(t, browser, scope, preparing.NextCursor, 4)
	if page.Mode != BrowseSnapshot || page.Total == nil || *page.Total != 2 || len(page.Entries) != 2 || page.Entries[0].Text != "before reader tail" || page.Entries[1].Text != "latest" || page.Diagnostics.OversizedRecords != 1 {
		t.Fatalf("full source snapshot=%+v", page)
	}
	if err := os.WriteFile(path, []byte(`{"type":"assistant","uuid":"rewrite","message":{"content":"changed"}}`+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	changed, err := browser.ReadPage(context.Background(), BrowseRequest{Scope: scope, Cursor: preparing.NextCursor, Limit: 2})
	if err != nil || changed.ReasonCode != "source_changed" {
		t.Fatalf("changed=%+v err=%v", changed, err)
	}
}

func TestInstalledBrowserClaudeContinuationOrderAndScope(t *testing.T) {
	home := privateBrowserHome(t)
	anchor, child := testSessionID, "123e4567-e89b-12d3-a456-426614174001"
	root := filepath.Join(home, ".claude", "projects", "-work")
	anchorPath, childPath := filepath.Join(root, anchor+".jsonl"), filepath.Join(root, child+".jsonl")
	writeRows(t, anchorPath, map[string]any{"type": "assistant", "uuid": "a1", "message": map[string]any{"content": "parent"}},
		map[string]any{"type": "continued-in", "sessionId": anchor, "continuedInSessionId": child})
	writeRows(t, childPath, map[string]any{"type": "assistant", "uuid": "b1", "message": map[string]any{"content": "child"}})
	browser, scope, _, _, _ := installedBrowserHistoryFixture(t, DefaultBrowserOptions(), []string{anchorPath, childPath})
	latest, err := browser.ReadPage(context.Background(), BrowseRequest{Scope: scope, Limit: 1})
	if err != nil || len(latest.Entries) != 1 || latest.Entries[0].Text != "child" || latest.NextCursor == "" || latest.Total != nil {
		t.Fatalf("latest=%+v err=%v", latest, err)
	}
	older, err := browser.ReadPage(context.Background(), BrowseRequest{Scope: scope, Cursor: latest.NextCursor, Limit: 1})
	if err != nil || len(older.Entries) != 1 || older.Entries[0].Text != "parent" || older.HasMore || older.Entries[0].ID == latest.Entries[0].ID || older.Total != nil {
		t.Fatalf("older=%+v err=%v", older, err)
	}
	changedScope := scope
	changedScope.CWD = "/other"
	invalid, err := browser.ReadPage(context.Background(), BrowseRequest{Scope: changedScope, Cursor: latest.NextCursor})
	if err != nil || invalid.ReasonCode != "invalid_cursor" {
		t.Fatalf("scope cursor=%+v err=%v", invalid, err)
	}
	if err := appendRows(childPath, map[string]any{"type": "assistant", "uuid": "b2", "message": map[string]any{"content": "appended"}}); err != nil {
		t.Fatal(err)
	}
	stable, err := browser.ReadPage(context.Background(), BrowseRequest{Scope: scope, Cursor: latest.NextCursor, Limit: 1})
	if err != nil || len(stable.Entries) != 1 || stable.Entries[0].Text != "parent" {
		t.Fatalf("saved continuation=%+v err=%v", stable, err)
	}
}

func TestInstalledBrowserFirstCloseFailureCannotPublish(t *testing.T) {
	home := privateBrowserHome(t)
	path := filepath.Join(home, ".claude", "projects", "-work", testSessionID+".jsonl")
	writeRows(t, path, map[string]any{"type": "user", "uuid": "one", "message": map[string]any{"content": "private"}})
	closeFault := errors.New("planned first-close fault")
	browser, scope, owner, opens, closes := installedBrowserHistoryFixture(t, DefaultBrowserOptions(), []string{path}, closeFault)
	page, err := browser.ReadPage(context.Background(), BrowseRequest{Scope: scope})
	if !errors.Is(err, ErrSourceCleanupUncertain) || page.Available {
		t.Fatalf("cleanup result=%+v err=%v", page, err)
	}
	browser.recentCache.mu.Lock()
	count := len(browser.recentCache.entries)
	browser.recentCache.mu.Unlock()
	if count != 0 {
		t.Fatalf("uncertain source published %d cache entries", count)
	}
	if opens.Load() == 0 || opens.Load() != closes.Load() {
		t.Fatalf("first-close accounting opened=%d closed=%d", opens.Load(), closes.Load())
	}
	if !errors.Is(owner.Witnesses(), closeFault) {
		t.Fatalf("owner lost physical close witness")
	}
	if closeErr := browser.Close(); !errors.Is(closeErr, closeFault) || !errors.Is(closeErr, ErrSourceCleanupUncertain) {
		t.Fatalf("joined first-close failure = %v", closeErr)
	}
}

// Partial acquisition must report its first physical-close fault to this
// Browser even though no source is returned to the caller to bind afterward.
func TestInstalledBrowserPartialAcquisitionCloseFailure(t *testing.T) {
	for _, route := range []string{"handle_open_error", "raw_cancel_before_adapt", "adapt_cancel_after_transfer"} {
		t.Run(route, func(t *testing.T) {
			home := privateBrowserHome(t)
			path := filepath.Join(home, ".claude", "projects", "-work", testSessionID+".jsonl")
			writeRows(t, path, map[string]any{"type": "user", "uuid": "one", "message": map[string]any{"content": "private"}})
			closeFault := errors.New("private partial-close fault")
			browser, scope, owner, opens, closes := installedBrowserHistoryFixture(t, DefaultBrowserOptions(), []string{path}, closeFault)
			fake := newSourceFixture() // A handle only; never an os.File or a real descriptor.
			fake.closeErr = closeFault
			ctx, cancel := context.WithCancel(context.Background())
			t.Cleanup(cancel)
			acquired, rawCloses, adaptations := 0, 0, 0
			var cancelWitnessErr error
			waitForCallCancellation := func(callCtx context.Context) error {
				select {
				case <-callCtx.Done():
					return nil
				case <-time.After(10 * time.Second):
					return errors.New("admitted source call did not cancel")
				}
			}
			entered, release := make(chan struct{}), make(chan struct{})
			var releaseOnce sync.Once
			var readDone, closeDone chan struct{}
			t.Cleanup(func() {
				releaseOnce.Do(func() { close(release) })
				if readDone != nil {
					<-readDone
				}
				if closeDone != nil {
					<-closeDone
				}
			})
			deps := browser.reader.installed.files.source
			deps.open = func(callCtx context.Context, owned *conversationlife.Owner, requested string, onClose func(error)) (*sourceOwnership, error) {
				if requested != path {
					return nil, conversationlife.ErrRefused
				}
				acquired++
				if route == "handle_open_error" {
					return acquireSourceHandle(owned, onClose, func() (sourceHandle, error) {
						return fake, context.DeadlineExceeded
					})
				}
				return acquireSourceRaw(callCtx, owned, onClose, func() (func() error, func() (sourceHandle, error), error) {
					// This closure models raw authority; no fake FD is passed to an OS API.
					closeRaw := func() error {
						rawCloses++
						if route == "raw_cancel_before_adapt" {
							close(entered)
							<-release
						}
						return closeFault
					}
					if route == "raw_cancel_before_adapt" {
						cancel()
						cancelWitnessErr = waitForCallCancellation(callCtx)
						if cancelWitnessErr != nil {
							return closeRaw, nil, cancelWitnessErr
						}
					}
					return closeRaw, func() (sourceHandle, error) {
						adaptations++
						cancel()
						cancelWitnessErr = waitForCallCancellation(callCtx)
						return fake, cancelWitnessErr
					}, nil
				})
			}
			browser.reader.installed.files.source = deps
			var page BrowsePage
			var err, first error
			if route == "raw_cancel_before_adapt" {
				readDone = make(chan struct{})
				go func() {
					defer close(readDone)
					page, err = browser.ReadPage(ctx, BrowseRequest{Scope: scope})
				}()
				select {
				case <-entered:
				case <-time.After(10 * time.Second):
					t.Fatal("raw unwind did not reach physical close")
				}
				closeDone = make(chan struct{})
				go func() { defer close(closeDone); first = browser.Close() }()
				// Closing is the real admission fence, set under mu before Close
				// waits for readWG. Observe it while the physical close is held.
				ticker := time.NewTicker(time.Millisecond)
				deadline := time.NewTimer(10 * time.Second)
				defer ticker.Stop()
				defer deadline.Stop()
				for {
					browser.mu.Lock()
					closing := browser.closing
					browser.mu.Unlock()
					if closing {
						break
					}
					select {
					case <-closeDone:
						t.Fatal("Browser.Close returned before its admission fence")
					case <-deadline.C:
						t.Fatal("Browser.Close did not enter its admission fence")
					case <-ticker.C:
					}
				}
				select {
				case <-closeDone:
					t.Fatal("Browser.Close returned while the partial physical close was held")
				default:
				}
				releaseOnce.Do(func() { close(release) })
				<-readDone
				<-closeDone
			} else {
				page, err = browser.ReadPage(ctx, BrowseRequest{Scope: scope})
			}
			primary := error(context.Canceled)
			wantRaw, wantAdapt, wantHandleClose := 0, 0, 1
			switch route {
			case "handle_open_error":
				primary = context.DeadlineExceeded
			case "raw_cancel_before_adapt":
				wantRaw, wantHandleClose = 1, 0
			case "adapt_cancel_after_transfer":
				wantAdapt = 1
			}
			if cancelWitnessErr != nil || !errors.Is(err, primary) || !errors.Is(err, ErrSourceCleanupUncertain) || errors.Is(err, closeFault) || strings.Contains(err.Error(), closeFault.Error()) || page.Available || len(page.Entries) != 0 || page.NextCursor != "" {
				t.Fatalf("unsafe partial request page=%+v err=%v cancellation witness=%v", page, err, cancelWitnessErr)
			}
			browser.recentCache.mu.Lock()
			cached := len(browser.recentCache.entries)
			browser.recentCache.mu.Unlock()
			browser.mu.Lock()
			published := len(browser.chains) + len(browser.jobs)
			browser.mu.Unlock()
			if cached != 0 || published != 0 || acquired != 1 || rawCloses != wantRaw || adaptations != wantAdapt || fake.calls["close"] != wantHandleClose || fake.calls["stat"] != 0 || fake.calls["read"] != 0 || opens.Load() != 0 || closes.Load() != 0 {
				t.Fatalf("partial acquisition published or misclosed: cache=%d chain-or-jobs=%d acquired=%d raw=%d adapted=%d handle=%v real=%d/%d", cached, published, acquired, rawCloses, adaptations, fake.calls, opens.Load(), closes.Load())
			}
			if !errors.Is(owner.Witnesses(), closeFault) {
				t.Fatal("owner lost first physical-close witness")
			}
			if route != "raw_cancel_before_adapt" {
				first = browser.Close()
			}
			second := browser.Close()
			owner.Begin()
			owner.Join()
			if !errors.Is(first, closeFault) || !errors.Is(first, ErrSourceCleanupUncertain) || !errors.Is(second, closeFault) || !errors.Is(second, ErrSourceCleanupUncertain) || rawCloses != wantRaw || fake.calls["close"] != wantHandleClose || !errors.Is(owner.Witnesses(), closeFault) {
				t.Fatalf("joined repeated cleanup first=%v second=%v raw=%d handle=%v", first, second, rawCloses, fake.calls)
			}
		})
	}
}

func TestInstalledBrowserTerminalReadDoesNotFallback(t *testing.T) {
	home := privateBrowserHome(t)
	path := filepath.Join(home, ".claude", "projects", "-work", testSessionID+".jsonl")
	writeRows(t, path, map[string]any{"type": "assistant", "uuid": "one", "message": map[string]any{"content": "private"}})
	browser, scope, _, opens, closes := installedBrowserHistoryFixture(t, DefaultBrowserOptions(), []string{path}, nil, context.DeadlineExceeded)
	page, err := browser.ReadPage(context.Background(), BrowseRequest{Scope: scope})
	if !errors.Is(err, context.DeadlineExceeded) || page.Available || len(page.Entries) != 0 {
		t.Fatalf("terminal read=%+v err=%v", page, err)
	}
	if opens.Load() == 0 || opens.Load() != closes.Load() {
		t.Fatalf("opened=%d closed=%d", opens.Load(), closes.Load())
	}
	browser.recentCache.mu.Lock()
	count := len(browser.recentCache.entries)
	browser.recentCache.mu.Unlock()
	if count != 0 {
		t.Fatalf("terminal read published %d cache entries", count)
	}
}

func TestInstalledBrowserWorkerCloseFailureIsTerminal(t *testing.T) {
	home := privateBrowserHome(t)
	anchor, child := testSessionID, "123e4567-e89b-12d3-a456-426614174003"
	root := filepath.Join(home, ".claude", "projects", "-work")
	anchorPath, childPath := filepath.Join(root, anchor+".jsonl"), filepath.Join(root, child+".jsonl")
	writeRows(t, anchorPath, map[string]any{"type": "assistant", "uuid": "a", "message": map[string]any{"content": "parent"}},
		map[string]any{"type": "continued-in", "sessionId": anchor, "continuedInSessionId": child})
	writeRows(t, childPath, map[string]any{"type": "assistant", "uuid": "b", "message": map[string]any{"content": "child"}})
	options := DefaultBrowserOptions()
	options.RecentBytes = 1
	gate := &atomic.Bool{}
	fault := errors.New("worker first-close fault")
	browser, scope, owner, _, _ := installedBrowserHistoryFixtureGated(t, options, []string{anchorPath, childPath}, gate, fault)
	entered := make(chan struct{})
	browser.chainWorkerBeforeCaptureObserver = func() { gate.Store(true); close(entered) }
	latest, err := browser.ReadPage(context.Background(), BrowseRequest{Scope: scope})
	if err != nil || latest.NextCursor == "" {
		t.Fatalf("latest=%+v err=%v", latest, err)
	}
	first, err := browser.ReadPage(context.Background(), BrowseRequest{Scope: scope, Cursor: latest.NextCursor})
	if err != nil || first.NextCursor == "" {
		t.Fatalf("first=%+v err=%v", first, err)
	}
	second, err := browser.ReadPage(context.Background(), BrowseRequest{Scope: scope, Cursor: first.NextCursor})
	if err != nil || second.NextCursor == "" {
		t.Fatalf("second=%+v err=%v", second, err)
	}
	select {
	case <-entered:
	case <-time.After(10 * time.Second):
		t.Fatal("worker was not admitted")
	}
	// Observe the actual job's failed state, not the passage of a fixed delay.
	deadline := time.After(10 * time.Second)
	for {
		browser.mu.Lock()
		var terminal error
		failed := false
		for _, job := range browser.jobs {
			job.mu.RLock()
			failed = job.state == BrowseFailed
			terminal = job.terminalErr
			job.mu.RUnlock()
		}
		browser.mu.Unlock()
		if failed && errors.Is(terminal, ErrSourceCleanupUncertain) {
			break
		}
		select {
		case <-deadline:
			t.Fatalf("job failed=%v terminal=%v", failed, terminal)
		case <-time.After(time.Millisecond):
		}
	}
	page, err := browser.ReadPage(context.Background(), BrowseRequest{Scope: scope, Cursor: second.NextCursor})
	if !errors.Is(err, ErrSourceCleanupUncertain) || page.Available || !errors.Is(owner.Witnesses(), fault) {
		t.Fatalf("worker close page=%+v err=%v witnesses=%v", page, err, owner.Witnesses())
	}
	if closeErr := browser.Close(); !errors.Is(closeErr, fault) || !errors.Is(closeErr, ErrSourceCleanupUncertain) {
		t.Fatalf("joined worker cleanup = %v", closeErr)
	}
}

func TestInstalledBrowserQueuedWorkerFencedClose(t *testing.T) {
	home := privateBrowserHome(t)
	anchor, child := testSessionID, "123e4567-e89b-12d3-a456-426614174002"
	root := filepath.Join(home, ".claude", "projects", "-work")
	anchorPath, childPath := filepath.Join(root, anchor+".jsonl"), filepath.Join(root, child+".jsonl")
	writeRows(t, anchorPath, map[string]any{"type": "assistant", "uuid": "a", "message": map[string]any{"content": "parent"}},
		map[string]any{"type": "continued-in", "sessionId": anchor, "continuedInSessionId": child})
	writeRows(t, childPath, map[string]any{"type": "assistant", "uuid": "b", "message": map[string]any{"content": "child"}})
	options := DefaultBrowserOptions()
	options.RecentBytes = 1
	browser, scope, owner, opens, closes := installedBrowserHistoryFixture(t, options, []string{anchorPath, childPath})
	entered, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	t.Cleanup(func() { once.Do(func() { close(release) }) })
	browser.chainWorkerBeforeCaptureObserver = func() { close(entered); <-release }
	latest, err := browser.ReadPage(context.Background(), BrowseRequest{Scope: scope})
	if err != nil || latest.NextCursor == "" {
		t.Fatalf("latest=%+v err=%v", latest, err)
	}
	preparing, err := browser.ReadPage(context.Background(), BrowseRequest{Scope: scope, Cursor: latest.NextCursor})
	if err != nil || preparing.NextCursor == "" {
		t.Fatalf("prepare=%+v err=%v", preparing, err)
	}
	_, err = browser.ReadPage(context.Background(), BrowseRequest{Scope: scope, Cursor: preparing.NextCursor})
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-entered:
	case <-time.After(10 * time.Second):
		t.Fatal("owned worker did not enter its actual capture boundary")
	}
	// The hook is inside the entered worker. Release is prearmed on every test
	// exit, then Close joins that worker and all its sources before inspection.
	owner.Begin()
	once.Do(func() { close(release) })
	if err := browser.Close(); err != nil {
		t.Fatal(err)
	}
	owner.Join()
	if opens.Load() == 0 || opens.Load() != closes.Load() {
		t.Fatalf("after joined close: opened=%d closed=%d", opens.Load(), closes.Load())
	}
	if _, err := browser.ReadPage(context.Background(), BrowseRequest{Scope: scope}); !errors.Is(err, conversationlife.ErrRefused) {
		t.Fatalf("fenced request: %v", err)
	}
}
