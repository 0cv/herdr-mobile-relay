package upload

import (
	"encoding/hex"
	"errors"
	"io"
	"runtime"
	"sync"
	"testing"
	"time"
)

// Each root owns one independent manager and a real admitted operation. Cleanup
// requests a tracked fence, releases every holder, then joins all callers before
// the registered owner drain. A blocked fence never owns the release path.
func admissionHeldCase(t *testing.T, operation string) {
	t.Helper()
	m := installedTestManager(t)
	target := uploadTarget(0)
	started := beginOne(t, m, target, "a.txt", "text/plain", 1)
	chunk := ChunkRequest{Target: target, UploadID: started.UploadID, Data: []byte("a"), SHA256: digestFor([]byte("a"))}
	finish := FinishRequest{Target: target, UploadID: started.UploadID, Files: []FileDigest{{FileIndex: 0, SHA256: digestFor([]byte("a"))}}}
	var ref string
	if operation == "Finish" || operation == "Resolve" {
		sendChunk(t, m, target, started.UploadID, 0, 0, []byte("a"))
	}
	if operation == "Resolve" {
		result, err := m.Finish(finish)
		if err != nil {
			t.Fatal(err)
		}
		ref = result.Attachments[0].Ref
	}
	var unlock func()
	switch operation {
	case "Chunk", "Finish", "Cancel":
		current := m.sessions[started.UploadID]
		current.mu.Lock()
		unlock = current.mu.Unlock
	case "Resolve":
		m.attachmentMu.Lock()
		unlock = m.attachmentMu.Unlock
	case "Begin":
		m.randomMu.Lock()
		unlock = m.randomMu.Unlock
	}
	var once sync.Once
	release := func() { once.Do(unlock) }
	var wg sync.WaitGroup
	requestFence, fenced := fixtureFence(m, &wg)
	defer func() { requestFence(); release(); wg.Wait() }()
	admitted, waiting, done, drained := make(chan struct{}), make(chan struct{}), make(chan struct{}), make(chan struct{})
	receipt := &fixtureReceipt{done: admitted}
	waitReceipt := &fixtureReceipt{done: waiting}
	m.installed.afterAdmission = receipt.record
	m.installed.beforeWait = waitReceipt.record
	witness := errors.New("terminal root close witness")
	closes := 0
	m.installed.afterClose = func(c io.Closer) error {
		if c == m.root {
			closes++
			return witness
		}
		return nil
	}
	invoke := func() error {
		switch operation {
		case "Chunk":
			_, err := m.Chunk(chunk)
			return err
		case "Finish":
			_, err := m.Finish(finish)
			return err
		case "Cancel":
			return m.Cancel(target, started.UploadID)
		case "Resolve":
			_, err := m.Resolve(target, ref)
			return err
		default:
			_, err := m.Begin(BeginRequest{Target: target, Files: []FileSpec{{Name: "b.txt", MediaType: "text/plain", Bytes: 1}}})
			return err
		}
	}
	var workErr, drainErr error
	wg.Add(1)
	go func() { defer wg.Done(); defer close(done); workErr = invoke() }()
	installedObserve(t, admitted)
	requestFence()
	installedObserve(t, fenced)
	lateDone := make(chan struct{})
	var lateErr error
	wg.Add(1)
	go func() { defer wg.Done(); defer close(lateDone); lateErr = invoke() }()
	installedObserve(t, lateDone)
	wg.Add(1)
	go func() { defer wg.Done(); defer close(drained); drainErr = m.DrainInstalled() }()
	installedObserve(t, waiting)
	installedRemainsHeld(t, done)
	installedRemainsHeld(t, drained)
	release()
	installedObserve(t, done)
	installedObserve(t, drained)
	wg.Wait()
	if lateErr != ErrInstalledDraining || receipt.count.Load() != 1 {
		t.Fatalf("late admission: %v receipts=%d", lateErr, receipt.count.Load())
	}
	if workErr != nil || !errors.Is(drainErr, witness) || closes != 1 {
		t.Fatalf("work=%v drain=%v closes=%d", workErr, drainErr, closes)
	}
	if m.DrainInstalled() != drainErr || closes != 1 {
		t.Fatal("terminal result repeated")
	}
	// Inspect only after all callers joined; verify accepted work actually ran.
	switch operation {
	case "Chunk":
		if hex.EncodeToString(m.sessions[started.UploadID].files[0].hash.Sum(nil)) != digestFor([]byte("a")) {
			t.Fatal("chunk did not update digest")
		}
	case "Finish", "Resolve":
		if len(m.attachments) != 1 {
			t.Fatal("accepted attachment lost")
		}
	case "Cancel":
		if len(m.sessions) != 0 {
			t.Fatal("accepted cancellation did not remove session")
		}
	case "Begin":
		if len(m.sessions) != 2 {
			t.Fatal("accepted begin did not create session")
		}
	}
}

func TestInstalledAdmissionHeldChunk(t *testing.T)    { admissionHeldCase(t, "Chunk") }
func TestInstalledAdmissionHeldFinish(t *testing.T)   { admissionHeldCase(t, "Finish") }
func TestInstalledAdmissionHeldCancel(t *testing.T)   { admissionHeldCase(t, "Cancel") }
func TestInstalledAdmissionHeldResolve(t *testing.T)  { admissionHeldCase(t, "Resolve") }
func TestInstalledAdmissionRandomWaiter(t *testing.T) { admissionHeldCase(t, "Begin") }

func admissionNestedCleanup(t *testing.T, resolve bool) {
	t.Helper()
	m := installedTestManager(t)
	target := uploadTarget(0)
	var ref string
	if resolve {
		started := beginOne(t, m, target, "a.txt", "text/plain", 1)
		sendChunk(t, m, target, started.UploadID, 0, 0, []byte("a"))
		result, err := m.Finish(FinishRequest{Target: target, UploadID: started.UploadID, Files: []FileDigest{{FileIndex: 0, SHA256: digestFor([]byte("a"))}}})
		if err != nil {
			t.Fatal(err)
		}
		ref = result.Attachments[0].Ref
	}
	entered, release, done, waiting, drained := make(chan struct{}), make(chan struct{}), make(chan struct{}), make(chan struct{}), make(chan struct{})
	reentered, workRelease := make(chan struct{}), make(chan struct{})
	var once, workOnce, clockOnce sync.Once
	unblock := func() { once.Do(func() { close(release) }) }
	releaseWork := func() { workOnce.Do(func() { close(workRelease) }) }
	var wg sync.WaitGroup
	requestFence, fenced := fixtureFence(m, &wg)
	defer func() { requestFence(); unblock(); releaseWork(); wg.Wait() }()
	now := time.Now()
	var fenceErr error
	// Never reenter a known-held gate, including a regressed gate held by
	// this callback itself. Retain real reentry when the gate is free.
	var heldGate bool
	m.now = func() time.Time {
		clockOnce.Do(func() {
			if m.installed.mu.TryLock() {
				m.installed.mu.Unlock()
			} else {
				heldGate = true
			}
			close(entered)
			<-release
			// No terminal caller exists until reentered. On failure cleanup
			// only requests the short fence; it releases both holders first.
			if m.installed.mu.TryLock() {
				m.installed.mu.Unlock()
			} else {
				heldGate = true
			}
			if !heldGate {
				fenceErr = m.BeginInstalledDrain()
			}
			close(reentered)
			<-workRelease
		})
		return now
	}
	receipt := &fixtureReceipt{done: make(chan struct{})}
	waitReceipt := &fixtureReceipt{done: waiting}
	m.installed.afterAdmission = receipt.record
	m.installed.beforeWait = waitReceipt.record
	var workErr, drainErr error
	wg.Add(1)
	go func() {
		defer wg.Done()
		defer close(done)
		if resolve {
			_, workErr = m.Resolve(target, ref)
		} else {
			_, workErr = m.Begin(BeginRequest{Target: target, Files: []FileSpec{{Name: "b.txt", MediaType: "text/plain", Bytes: 1}}})
		}
	}()
	installedObserve(t, entered)
	requestFence()
	installedObserve(t, fenced)
	unblock()
	installedObserve(t, reentered)
	wg.Add(1)
	go func() { defer wg.Done(); defer close(drained); drainErr = m.DrainInstalled() }()
	installedObserve(t, waiting)
	installedRemainsHeld(t, done)
	installedRemainsHeld(t, drained)
	releaseWork()
	installedObserve(t, done)
	installedObserve(t, drained)
	wg.Wait()
	if heldGate || receipt.count.Load() != 1 || waitReceipt.count.Load() != 1 {
		t.Fatal("held admission gate or unexpected admission")
	}
	if workErr != nil || drainErr != nil || fenceErr != nil {
		t.Fatalf("work=%v drain=%v reentry=%v", workErr, drainErr, fenceErr)
	}
	if _, err := m.Begin(BeginRequest{}); err != ErrInstalledDraining {
		t.Fatal(err)
	}
}

func TestInstalledAdmissionBeginNestedCleanup(t *testing.T)   { admissionNestedCleanup(t, false) }
func TestInstalledAdmissionResolveNestedCleanup(t *testing.T) { admissionNestedCleanup(t, true) }

func TestInstalledAdmissionReceiptAbnormalExit(t *testing.T) {
	for _, name := range []string{"panic", "goexit"} {
		t.Run(name, func(t *testing.T) {
			m := installedTestManager(t)
			done := make(chan struct{})
			var wg sync.WaitGroup
			defer func() { _ = m.BeginInstalledDrain(); wg.Wait() }()
			m.installed.afterAdmission = func() {
				if name == "panic" {
					panic("receipt")
				}
				runtime.Goexit()
			}
			wg.Add(1)
			go func() {
				defer wg.Done()
				defer close(done)
				defer func() { _ = recover() }()
				_, _ = m.Begin(BeginRequest{})
			}()
			installedObserve(t, done)
			wg.Wait()
			if err := m.DrainInstalled(); err != nil {
				t.Fatal(err)
			}
		})
	}
}
