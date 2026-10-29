package upload

import (
	"crypto/rand"
	"errors"
	"io"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/0cv/herdr-mobile-relay/internal/protocol"
)

func installedTestManager(t *testing.T) *Manager {
	t.Helper()
	m, err := NewInstalledManager(Config{Root: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = m.DrainInstalled() })
	return m
}
func installedObserve(t *testing.T, done <-chan struct{}) {
	t.Helper()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("private upload observation timed out")
	}
}

func TestInstalledUploadFenceAllEntries(t *testing.T) {
	m := installedTestManager(t)
	if err := m.BeginInstalledDrain(); err != nil {
		t.Fatal(err)
	}
	m.now = func() time.Time { t.Fatal("fenced clock called"); return time.Time{} }
	if _, err := m.Begin(BeginRequest{}); err != ErrInstalledDraining {
		t.Fatal(err)
	}
	if _, err := m.Chunk(ChunkRequest{}); err != ErrInstalledDraining {
		t.Fatal(err)
	}
	if _, err := m.Finish(FinishRequest{}); err != ErrInstalledDraining {
		t.Fatal(err)
	}
	if err := m.Cancel(protocol.TargetRef{}, ""); err != ErrInstalledDraining {
		t.Fatal(err)
	}
	if _, err := m.Resolve(protocol.TargetRef{}, ""); err != ErrInstalledDraining {
		t.Fatal(err)
	}
	if m.Cleanup() != 0 {
		t.Fatal("fenced cleanup")
	}
	if err := m.DrainInstalled(); err != nil {
		t.Fatal(err)
	}
}

type installedHeldRandom struct {
	once    sync.Once
	count   atomic.Int32
	entered chan struct{}
	release <-chan struct{}
}

func (r *installedHeldRandom) Read(p []byte) (int, error) {
	r.count.Add(1)
	r.once.Do(func() { close(r.entered) })
	<-r.release
	return rand.Read(p)
}

// The timer is a bounded negative observation, never a completion deadline.
// Callers must retain unconditional release and join cleanup on failure.
func installedRemainsHeld(t *testing.T, done <-chan struct{}) {
	t.Helper()
	timer := time.NewTimer(25 * time.Millisecond)
	defer timer.Stop()
	select {
	case <-done:
		t.Fatal("drain returned while admitted work remained held")
	case <-timer.C:
	}
}

func TestInstalledUploadHeldBeginJoins(t *testing.T) {
	m := installedTestManager(t)
	entered, release, done, drained := make(chan struct{}), make(chan struct{}), make(chan struct{}), make(chan struct{})
	var once sync.Once
	unblock := func() { once.Do(func() { close(release) }) }
	random := &installedHeldRandom{entered: entered, release: release}
	m.random = random
	waiting := make(chan struct{})
	waitReceipt := &fixtureReceipt{done: waiting}
	m.installed.beforeWait = waitReceipt.record
	var workErr, drainErr error
	var wg sync.WaitGroup
	// Cleanup is registered before either caller starts; every failure releases
	// the callback before joining both callers and before manager cleanup.
	requestFence, fenced := fixtureFence(m, &wg)
	defer func() { requestFence(); unblock(); wg.Wait() }()
	wg.Add(1)
	go func() {
		defer wg.Done()
		defer close(done)
		_, workErr = m.Begin(BeginRequest{Target: uploadTarget(0), Files: []FileSpec{{Name: "a.txt", MediaType: "text/plain", Bytes: 1}}})
	}()
	installedObserve(t, entered)
	requestFence()
	installedObserve(t, fenced)
	wg.Add(1)
	go func() { defer wg.Done(); defer close(drained); drainErr = m.DrainInstalled() }()
	installedObserve(t, waiting)
	installedRemainsHeld(t, drained)
	unblock()
	installedObserve(t, done)
	installedObserve(t, drained)
	wg.Wait()
	if random.count.Load() != 1 || waitReceipt.count.Load() != 1 {
		t.Fatal("unexpected random/wait receipts")
	}
	if workErr != nil || drainErr != nil {
		t.Fatalf("work=%v drain=%v", workErr, drainErr)
	}
}

func TestInstalledUploadDetachedCleanupJoins(t *testing.T) {
	m := installedTestManager(t)
	started := beginOne(t, m, uploadTarget(0), "a.txt", "text/plain", 1)
	entered, release, done := make(chan struct{}), make(chan struct{}), make(chan struct{})
	diskEntered, diskRelease := make(chan struct{}), make(chan struct{})
	var once, diskOnce sync.Once
	unblock := func() { once.Do(func() { close(release) }) }
	unblockDisk := func() { diskOnce.Do(func() { close(diskRelease) }) }
	var clockCalls atomic.Int32
	diskReceipt := &fixtureReceipt{done: diskEntered}
	m.now = func() time.Time {
		// The first call selects expiry. The later cleanupDiskLocked call is
		// after cleanSession, including closeFile's actual witness recording.
		if clockCalls.Add(1) > 1 {
			diskReceipt.record()
			<-diskRelease
		}
		return started.ExpiresAt.Add(time.Second)
	}
	var wg sync.WaitGroup
	var closes atomic.Int32
	closeReceipt := &fixtureReceipt{done: entered}
	requestFence, fenced := fixtureFence(m, &wg)
	witness := errors.New("post real session close")
	m.installed.afterClose = func(c io.Closer) error {
		if f, ok := c.(*os.File); ok && filepath.Ext(f.Name()) == ".part" {
			closes.Add(1)
			closeReceipt.record()
			<-release
			return witness
		}
		return nil
	}
	defer func() { requestFence(); unblock(); unblockDisk(); wg.Wait() }()
	wg.Add(1)
	go func() { defer wg.Done(); defer close(done); m.Cleanup() }()
	installedObserve(t, entered)
	// A close under m.mu must not block the sole releaser.
	probed := make(chan struct{})
	var remaining int
	wg.Add(1)
	go func() {
		defer wg.Done()
		defer close(probed)
		m.mu.Lock()
		remaining = len(m.sessions)
		m.mu.Unlock()
	}()
	installedObserve(t, probed) // completes while close remains held
	requestFence()
	installedObserve(t, fenced)
	// No terminal waiter exists until the nonnil close witness is recorded.
	// Keep this same admitted Cleanup held independently at its disk clock.
	unblock()
	installedObserve(t, diskEntered)
	waiting, drained := make(chan struct{}), make(chan struct{}, 4)
	waitReceipt := &fixtureReceipt{done: waiting}
	m.installed.beforeWait = waitReceipt.record
	results := make([]error, 4)
	for i := range results {
		wg.Add(1)
		go func() { defer wg.Done(); results[i] = m.DrainInstalled(); drained <- struct{}{} }()
	}
	installedObserve(t, waiting)
	installedRemainsHeld(t, done)
	installedRemainsHeld(t, drained)
	// A terminal waiter retaining the admission gate must fail this bounded
	// observation, without stranding the sole releaser or the late caller.
	lateDone := make(chan struct{})
	var lateErr error
	wg.Add(1)
	go func() {
		defer wg.Done()
		defer close(lateDone)
		lateErr = m.BeginInstalledDrain()
	}()
	installedObserve(t, lateDone)
	unblockDisk()
	installedObserve(t, done)
	wg.Wait()
	if lateErr != nil {
		t.Fatal(lateErr)
	}
	for _, result := range results {
		if result != results[0] || !errors.Is(result, witness) {
			t.Fatal("uncached/lost cleanup witness")
		}
	}
	if remaining != 0 || closeReceipt.count.Load() != 1 || waitReceipt.count.Load() != 1 ||
		clockCalls.Load() != 2 || diskReceipt.count.Load() != 1 {
		t.Fatal("not detached or unexpected receipts")
	}
	if closes.Load() != 1 {
		t.Fatal("session closed repeatedly")
	}
}

func TestInstalledUploadValidationDoesNotPoison(t *testing.T) {
	m := installedTestManager(t)
	_, _ = m.Begin(BeginRequest{})
	_, _ = m.Chunk(ChunkRequest{})
	_, _ = m.Finish(FinishRequest{})
	_ = m.Cancel(protocol.TargetRef{}, "missing")
	_, _ = m.Resolve(protocol.TargetRef{}, "missing")
	if err := m.DrainInstalled(); err != nil {
		t.Fatal(err)
	}
}

func TestInstalledUploadBoundedWitnesses(t *testing.T) {
	m := installedTestManager(t)
	first := errors.New("first")
	m.witness(first)
	for range 40 {
		m.witness(errors.New("cleanup"))
	}
	err := m.DrainInstalled()
	var overflow *CleanupOverflowError
	if !errors.Is(err, first) || !errors.As(err, &overflow) || overflow.Count != 25 {
		t.Fatalf("%v", err)
	}
	if m.Close() != err || m.DrainInstalled() != err {
		t.Fatal("terminal result not cached")
	}
}

func TestInstalledUploadConstructorCleanupFatal(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, attachmentIndexFilename), []byte("broken"), 0600); err != nil {
		t.Fatal(err)
	}
	witness := errors.New("constructor real close fault")
	ownership := &installedOwnership{afterClose: func(_ io.Closer) error { return witness }}
	m, err := newManager(Config{Root: root}, ownership)
	if m != nil {
		t.Cleanup(func() { _ = m.Close() })
		t.Fatal("failed construction returned manager")
	}
	var fatal *ConstructionCleanupError
	if !errors.As(err, &fatal) || !errors.Is(err, witness) {
		t.Fatalf("%v", err)
	}
	if ownership.result == nil {
		t.Fatal("constructor root not drained")
	}
}

func TestInstalledUploadPublishedClosePreservesObjects(t *testing.T) {
	m := installedTestManager(t)
	target := uploadTarget(0)
	started := beginOne(t, m, target, "a.txt", "text/plain", 1)
	sendChunk(t, m, target, started.UploadID, 0, 0, []byte("a"))
	witness := errors.New("post publication root-directory close")
	m.installed.afterClose = func(c io.Closer) error {
		if f, ok := c.(*os.File); ok && f.Name() == m.rootPath {
			return witness
		}
		return nil
	}
	_, err := m.Finish(FinishRequest{Target: target, UploadID: started.UploadID, Files: []FileDigest{{FileIndex: 0, SHA256: digestFor([]byte("a"))}}})
	var published *attachmentIndexPublishedError
	if !errors.As(err, &published) {
		t.Fatalf("lost published stage: %v", err)
	}
	if len(m.attachments) != 1 {
		t.Fatal("published attachment rolled back")
	}
	var object string
	for _, record := range m.attachments {
		object = record.attachment.Path
	}
	if err := m.DrainInstalled(); !errors.Is(err, witness) {
		t.Fatal(err)
	}
	if _, err := os.Stat(object); err != nil {
		t.Fatal("shutdown removed attachment", err)
	}
}

func TestInstalledUploadSessionCloseFirstResult(t *testing.T) {
	m := installedTestManager(t)
	target := uploadTarget(0)
	started := beginOne(t, m, target, "a.txt", "text/plain", 1)
	witness := errors.New("discard close")
	var count int
	m.installed.afterClose = func(c io.Closer) error {
		if f, ok := c.(*os.File); ok && filepath.Ext(f.Name()) == ".part" {
			count++
			return witness
		}
		return nil
	}
	_, _ = m.Chunk(ChunkRequest{Target: target, UploadID: started.UploadID})
	_ = m.Cancel(target, started.UploadID)
	m.Cleanup()
	if err := m.DrainInstalled(); !errors.Is(err, witness) {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatal("repeated physical close", count)
	}
}

func TestInstalledUploadOrdinaryAndZeroRefuseDrain(t *testing.T) {
	var zero Manager
	if zero.DrainInstalled() != ErrInstalledDrainRequired {
		t.Fatal("zero drain")
	}
	m, err := NewManager(Config{Root: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	if m.BeginInstalledDrain() != ErrInstalledDrainRequired || m.DrainInstalled() != ErrInstalledDrainRequired {
		t.Fatal("ordinary converted")
	}
	if _, err := m.Begin(BeginRequest{}); err == ErrInstalledDraining {
		t.Fatal("ordinary fenced")
	}
}
