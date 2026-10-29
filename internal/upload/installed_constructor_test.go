package upload

import (
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// Observe after the real close, without preclosing or reusing descriptors.
func constructorCloses(t *testing.T, o *installedOwnership, rootFault, fileFault error) map[io.Closer]int {
	t.Helper()
	seen := make(map[io.Closer]int)
	o.afterClose = func(c io.Closer) error {
		seen[c]++
		if seen[c] != 1 {
			t.Error("repeated physical close")
		}
		switch v := c.(type) {
		case *os.Root:
			if _, err := v.Stat("."); err == nil {
				t.Error("root still open")
			}
			return rootFault
		case *os.File:
			if _, err := v.Stat(); err == nil {
				t.Error("file still open")
			}
			if strings.HasSuffix(v.Name(), ".part") {
				return fileFault
			}
		}
		return nil
	}
	return seen
}

func constructorRootCount(seen map[io.Closer]int) int {
	n := 0
	for c, count := range seen {
		if _, ok := c.(*os.Root); ok {
			n += count
		}
	}
	return n
}

func constructorFatal(t *testing.T, m *Manager, err error, witnesses ...error) {
	t.Helper()
	if m != nil {
		t.Cleanup(func() { _ = m.DrainInstalled() })
		t.Fatal("failed constructor returned manager")
	}
	var fatal *ConstructionCleanupError
	if !errors.As(err, &fatal) {
		t.Fatalf("not fatal: %v", err)
	}
	if err.Error() != "upload construction cleanup failed" {
		t.Fatal("public error leaked")
	}
	for _, witness := range witnesses {
		if !errors.Is(err, witness) {
			t.Fatalf("lost witness %v: %v", witness, err)
		}
	}
}

func TestInstalledConstructorOptionalRefusals(t *testing.T) {
	for _, name := range []string{"validation", "acquisition", "post-root"} {
		t.Run(name, func(t *testing.T) {
			root := t.TempDir()
			o := &installedOwnership{}
			seen := constructorCloses(t, o, nil, nil)
			cfg := Config{Root: root}
			primary := errors.New("private ensure denial")
			switch name {
			case "validation":
				cfg.ChunkBytes = 1
			case "acquisition":
				cfg.Root = filepath.Join(root, "file")
				if err := os.WriteFile(cfg.Root, []byte("unchanged"), 0600); err != nil {
					t.Fatal(err)
				}
			case "post-root":
				o.denyStage = func(stage, path string) error {
					if stage == "ensure-directory" && path == "objects" {
						return primary
					}
					return nil
				}
			}
			m, err := newManager(cfg, o)
			if m != nil {
				t.Cleanup(func() { _ = m.DrainInstalled() })
				t.Fatal("unexpected manager")
			}
			var fatal *ConstructionCleanupError
			if err == nil || errors.As(err, &fatal) || o.count != 0 {
				t.Fatalf("optional refusal: %v", err)
			}
			want := 0
			if name == "post-root" {
				want = 1
				if !errors.Is(err, primary) {
					t.Fatal(err)
				}
			}
			if constructorRootCount(seen) != want {
				t.Fatal("root ownership mismatch")
			}
		})
	}
}

func TestInstalledConstructorPrimaryAndRootClose(t *testing.T) {
	primary, sibling := errors.New("private ensure denial"), errors.New("root close sibling")
	o := &installedOwnership{denyStage: func(stage, path string) error {
		if stage == "ensure-directory" && path == "objects" {
			return primary
		}
		return nil
	}}
	seen := constructorCloses(t, o, sibling, nil)
	m, err := newManager(Config{Root: t.TempDir()}, o)
	constructorFatal(t, m, err, primary, sibling)
	if constructorRootCount(seen) != 1 || o.count != 1 {
		t.Fatal("root result not captured once")
	}
}

// Seed only private upload files through the real upload API; no app construction.
func constructorSeed(t *testing.T, root string) (Attachment, time.Time) {
	t.Helper()
	now := time.Now().UTC()
	m, err := NewInstalledManager(Config{Root: root, Now: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = m.DrainInstalled() })
	target := uploadTarget(0)
	begun := beginOne(t, m, target, "a.txt", "text/plain", 1)
	sendChunk(t, m, target, begun.UploadID, 0, 0, []byte("a"))
	result, err := m.Finish(FinishRequest{Target: target, UploadID: begun.UploadID, Files: []FileDigest{{SHA256: digestFor([]byte("a"))}}})
	if err != nil {
		t.Fatal(err)
	}
	if err := m.DrainInstalled(); err != nil {
		t.Fatal(err)
	}
	return result.Attachments[0], now
}

func TestInstalledConstructorUncertaintyFatal(t *testing.T) {
	for _, name := range []string{"startup-session", "count-only-persist", "dropped-row-persist", "final-persist"} {
		t.Run(name, func(t *testing.T) {
			root := t.TempDir()
			cfg := Config{Root: root}
			var preserved Attachment
			var originalIndex []byte
			if name == "startup-session" {
				if err := os.MkdirAll(filepath.Join(root, "sessions", "stale"), 0700); err != nil {
					t.Fatal(err)
				}
			}
			if name == "count-only-persist" {
				attachment, now := constructorSeed(t, root)
				calls := 0
				cfg.Now = func() time.Time {
					calls++
					if calls == 1 {
						return now
					}
					return attachment.ExpiresAt.Add(time.Second)
				}
			}
			if name == "dropped-row-persist" {
				attachment, now := constructorSeed(t, root)
				preserved = attachment
				cfg.Now = func() time.Time { return now }
				path := filepath.Join(root, attachmentIndexFilename)
				data, err := os.ReadFile(path)
				if err != nil {
					t.Fatal(err)
				}
				var index attachmentIndex
				if err := json.Unmarshal(data, &index); err != nil {
					t.Fatal(err)
				}
				index.Attachments = append(index.Attachments, diskAttachmentRecord{})
				data, err = json.Marshal(index)
				if err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(path, data, 0600); err != nil {
					t.Fatal(err)
				}
				originalIndex = data
			}
			witness := errors.New("private maintenance denial")
			denied, persistenceAttempts := 0, 0
			o := &installedOwnership{denyStage: func(stage, path string) error {
				if stage == "persist-create" {
					persistenceAttempts++
				}
				if (name == "startup-session" && stage == "session-remove") || (name != "startup-session" && stage == "persist-create" && denied == 0) {
					denied++
					return witness
				}
				return nil
			}}
			seen := constructorCloses(t, o, nil, nil)
			m, err := newManager(cfg, o)
			constructorFatal(t, m, err, witness)
			if denied != 1 || constructorRootCount(seen) != 1 || o.count != 1 {
				t.Fatal("maintenance witness accounting")
			}
			if name == "startup-session" {
				if _, err := os.Stat(filepath.Join(root, "sessions", "stale")); err != nil {
					t.Fatal("denied removal entered", err)
				}
			}
			if name == "count-only-persist" {
				data, err := os.ReadFile(filepath.Join(root, attachmentIndexFilename))
				if err != nil {
					t.Fatal(err)
				}
				var index attachmentIndex
				if err := json.Unmarshal(data, &index); err != nil || len(index.Attachments) != 0 || persistenceAttempts != 2 {
					t.Fatal("final persistence did not follow retained count-only failure", err)
				}
			}
			if name == "dropped-row-persist" {
				data, err := os.ReadFile(preserved.Path)
				if err != nil || string(data) != "a" {
					t.Fatal("valid object lost", err)
				}
				data, err = os.ReadFile(filepath.Join(root, attachmentIndexFilename))
				if err != nil || string(data) != string(originalIndex) {
					t.Fatal("denied persistence changed index", err)
				}
			}
			if name == "final-persist" {
				if _, err := os.Stat(filepath.Join(root, attachmentIndexFilename)); !errors.Is(err, os.ErrNotExist) || persistenceAttempts != 1 {
					t.Fatal("denied creation entered", err)
				}
			}
			temps, err := filepath.Glob(filepath.Join(root, ".attachments-*.tmp"))
			if err != nil || len(temps) != 0 {
				t.Fatal("temporary leaked", err)
			}
		})
	}
}

func TestInstalledConstructorQuarantineSiblings(t *testing.T) {
	root := t.TempDir()
	index := filepath.Join(root, attachmentIndexFilename)
	if err := os.WriteFile(index, []byte("broken"), 0600); err != nil {
		t.Fatal(err)
	}
	quarantine, closeFault := errors.New("private quarantine denial"), errors.New("root close sibling")
	o := &installedOwnership{denyStage: func(stage, path string) error {
		if stage == "quarantine" {
			return quarantine
		}
		return nil
	}}
	seen := constructorCloses(t, o, closeFault, nil)
	indexClose := errors.New("index close sibling")
	afterClose := o.afterClose
	o.afterClose = func(c io.Closer) error {
		err := afterClose(c)
		if f, ok := c.(*os.File); ok && filepath.Base(f.Name()) == attachmentIndexFilename {
			return errors.Join(err, indexClose)
		}
		return err
	}
	m, err := newManager(Config{Root: root}, o)
	constructorFatal(t, m, err, quarantine, closeFault, indexClose)
	var primary *Error
	if !errors.As(err, &primary) || primary.Code != "upload_metadata_invalid" {
		t.Fatal("load primary lost")
	}
	if constructorRootCount(seen) != 1 || len(seen) != 3 || o.count != 3 {
		t.Fatal("descriptor/witness accounting")
	}
	data, readErr := os.ReadFile(index)
	if readErr != nil || string(data) != "broken" {
		t.Fatal("denied rename changed index")
	}
}

func TestInstalledConstructorRecoveryNonpoisoning(t *testing.T) {
	for _, name := range []string{"corrupt-quarantine", "dropped-row"} {
		t.Run(name, func(t *testing.T) {
			root := t.TempDir()
			attachment, now := constructorSeed(t, root)
			indexPath := filepath.Join(root, attachmentIndexFilename)
			if name == "corrupt-quarantine" {
				if err := os.WriteFile(indexPath, []byte("broken"), 0600); err != nil {
					t.Fatal(err)
				}
			} else {
				data, err := os.ReadFile(indexPath)
				if err != nil {
					t.Fatal(err)
				}
				var index attachmentIndex
				if err := json.Unmarshal(data, &index); err != nil {
					t.Fatal(err)
				}
				index.Attachments = append(index.Attachments, diskAttachmentRecord{})
				data, err = json.Marshal(index)
				if err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(indexPath, data, 0600); err != nil {
					t.Fatal(err)
				}
			}
			m, err := NewInstalledManager(Config{Root: root, Now: func() time.Time { return now }})
			if m != nil {
				t.Cleanup(func() { _ = m.DrainInstalled() })
			}
			if err != nil {
				t.Fatal(err)
			}
			if name == "dropped-row" {
				if len(m.attachments) != 1 {
					t.Fatal("valid row lost")
				}
				if _, err := m.Resolve(uploadTarget(0), attachment.Ref); err != nil {
					t.Fatal(err)
				}
			} else {
				matches, err := filepath.Glob(filepath.Join(root, "attachments.invalid-*.json"))
				if err != nil || len(matches) != 1 {
					t.Fatal("quarantine missing")
				}
			}
			if err := m.DrainInstalled(); err != nil {
				t.Fatal("recovery poisoned drain", err)
			}
			data, err := os.ReadFile(attachment.Path)
			if err != nil || string(data) != "a" {
				t.Fatal("existing object lost")
			}
		})
	}
}

// This logical closer owns no descriptor. Actual descriptor-close ordering is
// independently checked by constructorCloses for every matrix resource.
type constructorErrorCloser struct {
	calls int
	err   error
}

func (c *constructorErrorCloser) Close() error { c.calls++; return c.err }

func TestInstalledCloseFaultCannotEraseActualError(t *testing.T) {
	actual, extra := errors.New("actual close result"), errors.New("additional close fault")
	c := &constructorErrorCloser{err: actual}
	m := &Manager{installed: &installedOwnership{afterClose: func(io.Closer) error { return extra }}}
	err := m.physicalClose(c)
	if c.calls != 1 || !errors.Is(err, actual) || !errors.Is(err, extra) {
		t.Fatal("actual close result replaced", err)
	}
}

func TestInstalledBeginPartialFailureJoins(t *testing.T) {
	m := installedTestManager(t)
	admitted, held, waiting, done, drained := make(chan struct{}), make(chan struct{}), make(chan struct{}), make(chan struct{}), make(chan struct{})
	release := make(chan struct{})
	var once sync.Once
	unblock := func() { once.Do(func() { close(release) }) }
	var wg sync.WaitGroup
	requestFence, fenced := fixtureFence(m, &wg)
	defer func() { requestFence(); unblock(); wg.Wait() }()
	receipt := &fixtureReceipt{done: admitted}
	waitReceipt := &fixtureReceipt{done: waiting}
	heldReceipt := &fixtureReceipt{done: held}
	m.installed.afterAdmission = receipt.record
	m.installed.beforeWait = waitReceipt.record
	primary := errors.New("later open denied")
	m.installed.denyStage = func(stage, path string) error {
		if stage == "begin-file" && strings.HasSuffix(path, "0001.part") {
			heldReceipt.record()
			<-release
			return primary
		}
		return nil
	}
	seen := make(map[io.Closer]int)
	closed := make(map[io.Closer]bool)
	m.installed.afterClose = func(c io.Closer) error {
		seen[c]++
		switch v := c.(type) {
		case *os.File:
			_, err := v.Stat()
			closed[c] = errors.Is(err, os.ErrClosed)
		case *os.Root:
			_, err := v.Stat(".")
			closed[c] = errors.Is(err, os.ErrClosed)
		}
		return nil
	}
	var workErr, drainErr error
	wg.Add(1)
	go func() {
		defer wg.Done()
		defer close(done)
		_, workErr = m.Begin(BeginRequest{Target: uploadTarget(0), Files: []FileSpec{{Name: "a.txt", MediaType: "text/plain", Bytes: 1}, {Name: "b.txt", MediaType: "text/plain", Bytes: 1}}})
	}()
	installedObserve(t, admitted)
	installedObserve(t, held)
	requestFence()
	installedObserve(t, fenced)
	lateDone := make(chan struct{})
	var lateErr error
	wg.Add(1)
	go func() { defer wg.Done(); defer close(lateDone); _, lateErr = m.Begin(BeginRequest{}) }()
	installedObserve(t, lateDone)
	wg.Add(1)
	go func() { defer wg.Done(); defer close(drained); drainErr = m.DrainInstalled() }()
	installedObserve(t, waiting)
	installedRemainsHeld(t, done)
	installedRemainsHeld(t, drained)
	unblock()
	installedObserve(t, done)
	installedObserve(t, drained)
	wg.Wait()
	if lateErr != ErrInstalledDraining || receipt.count.Load() != 1 {
		t.Fatal("unexpected late admission", lateErr)
	}
	for c, n := range seen {
		if n != 1 || !closed[c] {
			t.Fatal("first real close missing or repeated")
		}
	}
	var public *Error
	if !errors.As(workErr, &public) || public.Code != "upload_staging_failed" || drainErr != nil || len(m.sessions) != 0 || constructorRootCount(seen) != 1 {
		t.Fatal("partial work not joined", workErr, drainErr)
	}
	entries, err := os.ReadDir(filepath.Join(m.rootPath, "sessions"))
	if err != nil || len(entries) != 0 {
		t.Fatal("partial staging leaked", err)
	}
}

func TestInstalledBeginPartialAcquisitionMatrix(t *testing.T) {
	for _, name := range []string{"directory", "first-file", "later-file", "later-file-siblings"} {
		t.Run(name, func(t *testing.T) {
			m := installedTestManager(t)
			primary, closeFault, removeFault := errors.New("private staging denial"), errors.New("private staging close"), errors.New("private removal denial")
			sibling := name == "later-file-siblings"
			var injectedClose error
			if sibling {
				injectedClose = closeFault
			}
			seen := constructorCloses(t, m.installed, nil, injectedClose)
			opens, removals, admissions := 0, 0, 0
			m.installed.afterAdmission = func() { admissions++ }
			m.installed.denyStage = func(stage, path string) error {
				switch stage {
				case "begin-directory":
					if name == "directory" {
						return primary
					}
				case "begin-file":
					opens++
					if name == "first-file" || opens == 2 {
						return primary
					}
				case "session-remove":
					removals++
					if sibling {
						return removeFault
					}
				}
				return nil
			}
			result, err := m.Begin(BeginRequest{Target: uploadTarget(0), Files: []FileSpec{{Name: "a.txt", MediaType: "text/plain", Bytes: 1}, {Name: "b.txt", MediaType: "text/plain", Bytes: 1}}})
			var public *Error
			if !errors.As(err, &public) || public.Code != "upload_staging_failed" || public.Args != nil || errors.Is(err, primary) || result.UploadID != "" {
				t.Fatal("public error not redacted", err)
			}
			if len(m.sessions) != 0 || admissions != 1 || removals != 1 {
				t.Fatal("partial session remained")
			}
			entries, err := os.ReadDir(filepath.Join(m.rootPath, "sessions"))
			if err != nil {
				t.Fatal(err)
			}
			if sibling {
				if len(entries) != 1 {
					t.Fatal("denied removal effect misrepresented")
				}
				parts, err := os.ReadDir(filepath.Join(m.rootPath, "sessions", entries[0].Name()))
				if err != nil || len(parts) != 1 {
					t.Fatal("partial acquisition shape")
				}
			} else if len(entries) != 0 {
				t.Fatal("staging leaked after successful removal")
			}
			// Generic validation must not add another cleanup witness.
			_, _ = m.Begin(BeginRequest{})
			terminal := m.DrainInstalled()
			if sibling {
				if !errors.Is(terminal, closeFault) || !errors.Is(terminal, removeFault) || m.installed.count != 2 {
					t.Fatal("cleanup siblings lost", terminal)
				}
			} else if terminal != nil {
				t.Fatal("primary/validation poisoned drain", terminal)
			}
			parts := 0
			for c := range seen {
				if f, ok := c.(*os.File); ok && strings.HasSuffix(f.Name(), ".part") {
					parts++
				}
			}
			want := 0
			if strings.HasPrefix(name, "later-file") {
				want = 1
			}
			if parts != want || constructorRootCount(seen) != 1 || removals != 1 || m.DrainInstalled() != terminal {
				t.Fatal("ownership not exactly once")
			}
			// A denied removal intentionally leaves a closed private artifact; TempDir
			// removes it only after fenced drain. It is not claimed successfully removed.
		})
	}
}
