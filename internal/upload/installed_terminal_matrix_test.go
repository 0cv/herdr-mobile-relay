package upload

import (
	"bytes"
	"errors"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"sync"
	"testing"
	"time"
)

// Count identities, not errors.Is equivalence, with a hard traversal budget.
func terminalMatrixOccurrences(t *testing.T, root, want error) int {
	t.Helper()
	pending := []error{root}
	count := 0
	for visited := 0; len(pending) != 0; visited++ {
		if visited == 128 {
			t.Fatal("unbounded cleanup error tree")
		}
		e := pending[len(pending)-1]
		pending = pending[:len(pending)-1]
		if e == nil {
			continue
		}
		if e == want {
			count++
		}
		switch e := e.(type) {
		case interface{ Unwrap() []error }:
			children := e.Unwrap()
			if len(children)+len(pending) > 128 {
				t.Fatal("unbounded cleanup siblings")
			}
			pending = append(pending, children...)
		case interface{ Unwrap() error }:
			pending = append(pending, e.Unwrap())
		}
	}
	return count
}

func TestInstalledTerminalMatrixCloseSiblings(t *testing.T) {
	m := installedTestManager(t)
	req := finishStage(t, m, uploadTarget(0), 2)
	files := m.sessions[req.UploadID].files
	handles := []io.Closer{files[0].file, files[1].file, m.root}
	faults := []error{errors.New("equal terminal fault"), errors.New("equal terminal fault"), errors.New("equal terminal fault")}
	physical := make(map[io.Closer]int)
	closedBeforeFault := make(map[io.Closer]bool)
	admitted, held, release, waiting := make(chan struct{}), make(chan struct{}), make(chan struct{}), make(chan struct{})
	var releaseOnce sync.Once
	unblock := func() { releaseOnce.Do(func() { close(release) }) }
	var callers sync.WaitGroup
	// Installed manager cleanup was registered first. This defer always fences,
	// releases the actual clock work, and joins before that cleanup or TempDir.
	requestFence, fenced := fixtureFence(m, &callers)
	defer func() { requestFence(); unblock(); callers.Wait() }()
	now := time.Now()
	var heldOnce sync.Once
	m.now = func() time.Time { heldOnce.Do(func() { close(held) }); <-release; return now }
	admissionReceipt := &fixtureReceipt{done: admitted}
	waitReceipt := &fixtureReceipt{done: waiting}
	m.installed.afterAdmission = admissionReceipt.record
	m.installed.beforeWait = waitReceipt.record // receipt only; returns
	m.installed.afterClose = func(c io.Closer) error {
		for i, h := range handles {
			if c != h {
				continue
			}
			physical[c]++
			if f, ok := c.(*os.File); ok {
				_, err := f.Stat()
				closedBeforeFault[c] = errors.Is(err, os.ErrClosed)
			} else {
				_, err := m.root.Stat(".")
				closedBeforeFault[c] = errors.Is(err, os.ErrClosed)
			}
			return faults[i]
		}
		return nil
	}
	workDone := make(chan struct{})
	callers.Add(1)
	go func() { defer callers.Done(); defer close(workDone); m.Cleanup() }()
	installedObserve(t, admitted)
	installedObserve(t, held)
	requestFence()
	installedObserve(t, fenced)
	results := make([]error, 4)
	done := make(chan struct{}, len(results))
	for i := range results {
		callers.Add(1)
		go func() {
			defer callers.Done()
			if i%2 == 0 {
				results[i] = m.Close()
			} else {
				results[i] = m.DrainInstalled()
			}
			done <- struct{}{}
		}()
	}
	installedObserve(t, waiting)
	installedRemainsHeld(t, done)
	unblock()
	installedObserve(t, workDone)
	callers.Wait()
	if admissionReceipt.count.Load() != 1 || waitReceipt.count.Load() != 1 {
		t.Fatal("unexpected admission/wait receipts")
	}
	for _, result := range results {
		if result == nil || result != results[0] {
			t.Fatal("terminal result not shared")
		}
		for _, fault := range faults {
			if terminalMatrixOccurrences(t, result, fault) != 1 {
				t.Fatal("lost or duplicated independent sibling")
			}
		}
	}
	if m.Close() != results[0] || m.DrainInstalled() != results[0] {
		t.Fatal("terminal cache changed")
	}
	for _, h := range handles {
		if physical[h] != 1 || !closedBeforeFault[h] {
			t.Fatal("not exactly one real close before additional fault")
		}
	}
	for i, file := range files {
		if file.file != nil || terminalMatrixOccurrences(t, file.closeErr, faults[i]) != 1 {
			t.Fatal("first session close result lost")
		}
	}
}

func TestInstalledTerminalMatrixSuccessfulRestart(t *testing.T) {
	for _, mode := range []string{"ordinary", "installed"} {
		t.Run(mode, func(t *testing.T) {
			root := t.TempDir()
			open := func() (*Manager, func() error) {
				t.Helper()
				var m *Manager
				var err error
				if mode == "ordinary" {
					m, err = NewManager(Config{Root: root})
				} else {
					m, err = NewInstalledManager(Config{Root: root})
				}
				if err != nil {
					t.Fatal(err)
				}
				// Ordinary Close is deliberately invoked once, including failure cleanup.
				closed := false
				closeOnce := func() error {
					if closed {
						return nil
					}
					closed = true
					return m.Close()
				}
				t.Cleanup(func() { _ = closeOnce() })
				if mode == "ordinary" {
					if m.BeginInstalledDrain() != ErrInstalledDrainRequired || m.DrainInstalled() != ErrInstalledDrainRequired || m.installed != nil {
						t.Fatal("ordinary ownership converted")
					}
				}
				return m, closeOnce
			}
			m, closeFirst := open()
			target := uploadTarget(0)
			finished, err := m.Finish(finishStage(t, m, target, 1))
			if err != nil || len(finished.Attachments) != 1 {
				t.Fatalf("finish: %v", err)
			}
			attachment := finished.Attachments[0]
			equalAttachment := func(got, want Attachment) bool {
				if !got.ExpiresAt.Equal(want.ExpiresAt) {
					return false
				}
				got.ExpiresAt, want.ExpiresAt = time.Time{}, time.Time{}
				return reflect.DeepEqual(got, want)
			}
			verify := func(manager *Manager) {
				t.Helper()
				resolved, err := manager.Resolve(target, attachment.Ref)
				if err != nil || !equalAttachment(resolved, attachment) {
					t.Fatalf("reference changed: %v", err)
				}
				wrong := target
				wrong.TerminalID += "-other"
				if _, err := manager.Resolve(wrong, attachment.Ref); err == nil {
					t.Fatal("target binding lost")
				}
				resolved, err = manager.Resolve(target, attachment.Ref)
				if err != nil || !equalAttachment(resolved, attachment) {
					t.Fatal("wrong-target refusal damaged attachment")
				}
				data, err := os.ReadFile(attachment.Path)
				if err != nil || !bytes.Equal(data, []byte("new")) {
					t.Fatal("object bytes changed")
				}
			}
			verify(m)
			indexPath := filepath.Join(root, attachmentIndexFilename)
			index, err := os.ReadFile(indexPath)
			if err != nil {
				t.Fatal(err)
			}
			if err := closeFirst(); err != nil {
				t.Fatal(err)
			}
			if _, err := m.root.Stat("."); !errors.Is(err, os.ErrClosed) {
				t.Fatal("first manager root remains open")
			}
			if mode == "installed" && (m.DrainInstalled() != nil || m.Close() != nil) {
				t.Fatal("successful drain not cached")
			}
			checkIndex := func() {
				t.Helper()
				got, err := os.ReadFile(indexPath)
				if err != nil || !bytes.Equal(got, index) {
					t.Fatal("persisted index changed")
				}
				data, err := os.ReadFile(attachment.Path)
				if err != nil || !bytes.Equal(data, []byte("new")) {
					t.Fatal("shutdown removed object")
				}
			}
			checkIndex()
			reopened, closeSecond := open()
			verify(reopened)
			checkIndex()
			if err := closeSecond(); err != nil {
				t.Fatal(err)
			}
			if _, err := reopened.root.Stat("."); !errors.Is(err, os.ErrClosed) {
				t.Fatal("reopened manager root remains open")
			}
			if mode == "installed" && reopened.DrainInstalled() != nil {
				t.Fatal("reopened successful drain not cached")
			}
			checkIndex()
		})
	}
}
