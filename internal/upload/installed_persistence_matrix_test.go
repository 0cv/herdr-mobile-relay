package upload

import (
	"bytes"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// All fixtures use only private temporary files. Hooks record observations;
// assertions run after ownership has returned, never inside a close hook.
type persistenceReceipt struct {
	stages   []string
	closes   map[string]int
	badClose bool
}

func persistenceHooks(m *Manager, stage string, primary, closeFault, removeFault error) *persistenceReceipt {
	r := &persistenceReceipt{closes: make(map[string]int)}
	m.installed.denyStage = func(s, _ string) error {
		r.stages = append(r.stages, s)
		if s == stage {
			return primary
		}
		if s == "persist-temp-remove" {
			return removeFault
		}
		return nil
	}
	m.installed.afterClose = func(c io.Closer) error {
		f, ok := c.(*os.File)
		if !ok {
			return nil
		}
		kind := "other"
		if strings.HasPrefix(filepath.Base(f.Name()), ".attachments-") {
			kind = "temp"
		}
		if f.Name() == m.rootPath {
			kind = "directory"
		}
		r.closes[kind]++
		if _, err := f.Stat(); !errors.Is(err, os.ErrClosed) {
			r.badClose = true
		}
		if kind == "temp" || kind == "directory" {
			return closeFault
		}
		return nil
	}
	return r
}

func persistenceUpload(t *testing.T, m *Manager) FinishRequest {
	t.Helper()
	data := []byte("private persistence fixture\n")
	target := uploadTarget(1)
	b, err := m.Begin(BeginRequest{Target: target, Files: []FileSpec{{Name: "fixture.txt", MediaType: "text/plain", Bytes: int64(len(data))}}})
	if err != nil {
		t.Fatal(err)
	}
	sendChunk(t, m, target, b.UploadID, 0, 0, data)
	return FinishRequest{Target: target, UploadID: b.UploadID, Files: []FileDigest{{FileIndex: 0, SHA256: digestFor(data)}}}
}

func persistenceBaseline(t *testing.T, m *Manager) ([]byte, Attachment) {
	t.Helper()
	result, err := m.Finish(persistenceUpload(t, m))
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(m.rootPath, attachmentIndexFilename))
	if err != nil {
		t.Fatal(err)
	}
	return data, result.Attachments[0]
}

func persistenceIndex(t *testing.T, m *Manager) []byte {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(m.rootPath, attachmentIndexFilename))
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func persistenceTemps(t *testing.T, m *Manager) []string {
	t.Helper()
	names, err := filepath.Glob(filepath.Join(m.rootPath, ".attachments-*.tmp"))
	if err != nil {
		t.Fatal(err)
	}
	return names
}

func persistencePublic(t *testing.T, err error, published bool) {
	t.Helper()
	if err == nil || errorCode(t, err) != "upload_metadata_unavailable" {
		t.Fatalf("metadata error: %v", err)
	}
	var public *Error
	if !errors.As(err, &public) || public.Args != nil {
		t.Fatal("public metadata contains private arguments")
	}
	var p *attachmentIndexPublishedError
	if errors.As(err, &p) != published {
		t.Fatalf("publication classification: %v", err)
	}
	if strings.Contains(err.Error(), "private-fault") {
		t.Fatal("private error exposed")
	}
}

func TestInstalledPersistenceExactPredenials(t *testing.T) {
	stages := []string{"persist-temp-create", "persist-temp-chmod", "persist-temp-write", "persist-temp-sync", "persist-rename"}
	for i, stage := range stages {
		t.Run(stage, func(t *testing.T) {
			m := installedTestManager(t)
			original, attachment := persistenceBaseline(t, m)
			fault := errors.New("private-fault-primary")
			r := persistenceHooks(m, stage, fault, nil, nil)
			persistencePublic(t, m.persistAttachments(), false)
			if len(r.stages) != i+2+boolInt(i > 0) {
				t.Fatalf("stage receipts: %v", r.stages)
			}
			if r.stages[i+1] != stage {
				t.Fatalf("wrong boundary: %v", r.stages)
			}
			if r.closes["temp"] != boolInt(i > 0) || r.closes["directory"] != 0 || r.badClose {
				t.Fatalf("close receipts: %+v", r)
			}
			if len(persistenceTemps(t, m)) != 0 || !bytes.Equal(original, persistenceIndex(t, m)) {
				t.Fatal("unpublished index/temp changed")
			}
			if _, err := os.Stat(attachment.Path); err != nil {
				t.Fatal(err)
			}
			if m.installed.count != 1 || m.installed.witnesses[0] != fault {
				t.Fatal("original witness lost")
			}
		})
	}
}

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

func TestInstalledPersistenceRealMarshalAndEntry(t *testing.T) {
	m := installedTestManager(t)
	original, a := persistenceBaseline(t, m)
	m.attachments[a.Ref].attachment.ExpiresAt = time.Date(10000, 1, 1, 0, 0, 0, 0, time.UTC)
	entry := errors.New("private-fault-entry")
	r := persistenceHooks(m, "persist-create", entry, nil, nil)
	persistencePublic(t, m.persistAttachments(), false)
	if m.installed.witnesses[0] != entry {
		t.Fatal("entry denial moved after marshal")
	}
	m.installed.denyStage = func(s, _ string) error { r.stages = append(r.stages, s); return nil }
	persistencePublic(t, m.persistAttachments(), false)
	if m.installed.count != 2 || m.installed.witnesses[1] == nil || len(r.stages) != 2 || r.closes["temp"] != 0 {
		t.Fatal("marshal did not fail before acquisition")
	}
	if !bytes.Equal(original, persistenceIndex(t, m)) || len(persistenceTemps(t, m)) != 0 {
		t.Fatal("marshal changed disk")
	}
}

func TestInstalledPersistenceLexicalSiblings(t *testing.T) {
	for _, stage := range []string{"persist-temp-chmod", "persist-temp-write", "persist-temp-sync"} {
		t.Run(stage, func(t *testing.T) {
			m := installedTestManager(t)
			original, _ := persistenceBaseline(t, m)
			primary, closed, removed := errors.New("private-fault-primary"), errors.New("private-fault-close"), errors.New("private-fault-remove")
			r := persistenceHooks(m, stage, primary, closed, removed)
			persistencePublic(t, m.persistAttachments(), false)
			if r.badClose || r.closes["temp"] != 1 || r.closes["directory"] != 0 {
				t.Fatalf("close receipts: %+v", r)
			}
			if m.installed.count != 3 || m.installed.witnesses[0] != primary || !errors.Is(m.installed.witnesses[1], closed) || m.installed.witnesses[2] != removed {
				t.Fatal("siblings lost")
			}
			if len(persistenceTemps(t, m)) != 1 || !bytes.Equal(original, persistenceIndex(t, m)) {
				t.Fatal("closed leftover/index mismatch")
			}
			leftover, err := os.ReadFile(persistenceTemps(t, m)[0])
			if err != nil {
				t.Fatal(err)
			}
			if (stage == "persist-temp-sync") != (len(leftover) > 0) {
				t.Fatal("denied write/sync effects do not match leftover")
			}
		})
	}
}

func TestInstalledPersistenceFinishRollback(t *testing.T) {
	for _, stage := range []string{"persist-temp-create", "persist-temp-chmod", "persist-temp-write", "persist-temp-sync", "persist-rename"} {
		t.Run(stage, func(t *testing.T) {
			m := installedTestManager(t)
			original, a := persistenceBaseline(t, m)
			request := persistenceUpload(t, m)
			fault := errors.New("private-fault-write")
			r := persistenceHooks(m, stage, fault, nil, nil)
			_, err := m.Finish(request)
			persistencePublic(t, err, false)
			if len(m.attachments) != 1 || m.attachments[a.Ref] == nil || len(m.sessions) != 0 {
				t.Fatal("rollback records")
			}
			objects, err := os.ReadDir(filepath.Join(m.rootPath, "objects"))
			if err != nil || len(objects) != 1 {
				t.Fatalf("rollback objects: %v %v", objects, err)
			}
			if _, err := os.Stat(a.Path); err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(original, persistenceIndex(t, m)) || len(persistenceTemps(t, m)) != 0 || r.closes["temp"] != boolInt(stage != "persist-temp-create") || r.badClose {
				t.Fatal("rollback persistence effects")
			}
			if !errors.Is(m.DrainInstalled(), fault) {
				t.Fatal("lost rollback witness")
			}
		})
	}
}

func TestInstalledPersistencePublishedSiblings(t *testing.T) {
	for _, stage := range []string{"persist-directory-open", "persist-directory-sync", "directory-close"} {
		t.Run(stage, func(t *testing.T) {
			m := installedTestManager(t)
			original, a := persistenceBaseline(t, m)
			request := persistenceUpload(t, m)
			primary, closed := errors.New("private-fault-directory"), errors.New("private-fault-directory-close")
			r := persistenceHooks(m, stage, primary, nil, nil)
			prior := m.installed.afterClose
			m.installed.afterClose = func(c io.Closer) error {
				err := prior(c)
				if f, ok := c.(*os.File); ok && f.Name() == m.rootPath {
					return errors.Join(err, closed)
				}
				return err
			}
			_, err := m.Finish(request)
			persistencePublic(t, err, true)
			if len(m.attachments) != 2 || m.attachments[a.Ref] == nil || bytes.Equal(original, persistenceIndex(t, m)) {
				t.Fatal("published records/index rolled back")
			}
			for _, record := range m.attachments {
				if !bytes.Contains(persistenceIndex(t, m), []byte(record.attachment.Ref)) {
					t.Fatal("published index missing record")
				}
				if _, err := os.Stat(record.attachment.Path); err != nil {
					t.Fatal(err)
				}
			}
			wantDir := boolInt(stage != "persist-directory-open")
			if r.badClose || r.closes["temp"] != 1 || r.closes["directory"] != wantDir || len(persistenceTemps(t, m)) != 0 {
				t.Fatalf("published ownership: %+v", r)
			}
			for _, s := range r.stages {
				if s == "persist-temp-remove" {
					t.Fatal("transferred name removed")
				}
			}
			wantCount := 1
			if stage == "persist-directory-sync" {
				wantCount = 2
			}
			if m.installed.count != wantCount {
				t.Fatalf("witness count %d", m.installed.count)
			}
			result := m.DrainInstalled()
			if stage != "directory-close" && !errors.Is(result, primary) {
				t.Fatal("primary lost")
			}
			if wantDir == 1 && !errors.Is(result, closed) {
				t.Fatal("close sibling lost")
			}
		})
	}
}

func TestInstalledPersistenceCleanupRetainsSiblings(t *testing.T) {
	m := installedTestManager(t)
	_, a := persistenceBaseline(t, m)
	m.now = func() time.Time { return a.ExpiresAt.Add(time.Second) }
	primary, closed, removed := errors.New("private-fault-write"), errors.New("private-fault-close"), errors.New("private-fault-remove")
	persistenceHooks(m, "persist-temp-write", primary, closed, removed)
	if m.Cleanup() < 1 {
		t.Fatal("cleanup did not expire attachment")
	}
	if len(m.attachments) != 0 || m.installed.count != 3 || len(persistenceTemps(t, m)) != 1 {
		t.Fatal("count-only persistence lost")
	}
	m.installed.denyStage = nil
	m.installed.afterClose = nil
	if err := m.persistAttachments(); err != nil {
		t.Fatal(err)
	}
	result := m.DrainInstalled()
	for _, err := range []error{primary, closed, removed} {
		if !errors.Is(result, err) {
			t.Fatal("successful persist erased witness")
		}
	}
	if m.installed.count != 3 {
		t.Fatal("witness recount")
	}
}

func TestInstalledPersistenceConcurrentCachedOverflow(t *testing.T) {
	m := installedTestManager(t)
	equal := errors.New("private-fault-equal")
	persistenceHooks(m, "persist-temp-create", equal, nil, nil)
	for i := 0; i < 19; i++ {
		if m.persistAttachments() == nil {
			t.Fatal("denial ignored")
		}
	}
	entered, release := make(chan struct{}), make(chan struct{})
	admitted := make(chan struct{})
	admissionReceipt := &fixtureReceipt{done: admitted}
	m.installed.afterAdmission = func() { admissionReceipt.record(); <-release }
	var releaseOnce sync.Once
	var callers sync.WaitGroup
	results := make([]error, 4)
	completed := make(chan struct{}, len(results))
	waitReceipt := &fixtureReceipt{done: entered}
	m.installed.beforeWait = waitReceipt.record
	requestFence, fenced := fixtureFence(m, &callers)
	// Registered before callers: fence, unconditional release, join, then restore.
	t.Cleanup(func() {
		requestFence()
		releaseOnce.Do(func() { close(release) })
		callers.Wait()
		m.installed.beforeWait = nil
		m.installed.afterAdmission = nil
		_ = m.DrainInstalled()
	})
	callers.Add(1)
	go func() { defer callers.Done(); m.Cleanup() }()
	installedObserve(t, admitted)
	requestFence()
	installedObserve(t, fenced)
	callers.Add(len(results))
	for i := range results {
		go func(i int) { defer callers.Done(); results[i] = m.DrainInstalled(); completed <- struct{}{} }(i)
	}
	installedObserve(t, entered)
	// The boundary receipt returns; only admitted Cleanup holds work.Wait.
	// Historically the receipt also blocked, making this observation vacuous.
	timer := time.NewTimer(10 * time.Millisecond)
	defer timer.Stop()
	select {
	case <-completed:
		t.Fatal("drain returned while owner held")
	case <-timer.C:
	}
	releaseOnce.Do(func() { close(release) })
	callers.Wait()
	if admissionReceipt.count.Load() != 1 || waitReceipt.count.Load() != 1 {
		t.Fatal("unexpected admission/wait receipts")
	}
	for _, result := range results {
		if result != results[0] {
			t.Fatal("noncached drain result")
		}
	}
	if m.DrainInstalled() != results[0] || m.installed.count != 16 || m.installed.overflow != 3 {
		t.Fatal("cached recount/overflow")
	}
	for _, w := range m.installed.witnesses {
		if w != equal {
			t.Fatal("equal physical failures deduplicated")
		}
	}
	var overflow *CleanupOverflowError
	if !errors.As(results[0], &overflow) || overflow.Count != 3 {
		t.Fatal("missing typed overflow")
	}
}
