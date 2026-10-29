package upload

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// All work is synchronous. Cleanup is registered before any subsequent assertion;
// there are no holders, callers, observers or AfterFuncs to release or join.
func recoveryProbeSeed(t *testing.T) (*Manager, []Attachment, Config) {
	t.Helper()
	now := time.Now().UTC()
	random := make([]byte, 240)
	for i := range random {
		random[i] = byte(i/24 + 1)
	}
	cfg := Config{Root: t.TempDir(), Now: func() time.Time { return now }, Random: bytes.NewReader(random)}
	m, err := NewInstalledManager(cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = m.BeginInstalledDrain(); _ = m.DrainInstalled() })
	var attachments []Attachment
	for range 2 {
		data := []byte("private text\n")
		target := uploadTarget(1)
		b, err := m.Begin(BeginRequest{Target: target, Files: []FileSpec{{Name: "note.txt", MediaType: "text/plain", Bytes: int64(len(data))}}})
		if err != nil {
			t.Fatal(err)
		}
		_, err = m.Chunk(ChunkRequest{Target: target, UploadID: b.UploadID, Data: data, SHA256: digestFor(data)})
		if err != nil {
			t.Fatal(err)
		}
		f, err := m.Finish(FinishRequest{Target: target, UploadID: b.UploadID, Files: []FileDigest{{SHA256: digestFor(data)}}})
		if err != nil {
			t.Fatal(err)
		}
		attachments = append(attachments, f.Attachments[0])
	}
	return m, attachments, cfg
}

type recoveryProbeCloses struct {
	counts  map[io.Closer]int
	open    bool
	probes  int
	indexes int
	roots   int
}

// No assertions or abnormal exits in callbacks. Stat checks the same closed
// object, never a recycled descriptor. Each added fault follows physical Close.
func (w *recoveryProbeCloses) observe(c io.Closer) {
	w.counts[c]++
	switch f := c.(type) {
	case *os.File:
		if _, err := f.Stat(); err == nil {
			w.open = true
		}
		if filepath.Base(f.Name()) == attachmentIndexFilename {
			w.indexes++
		}
		if strings.Contains(filepath.ToSlash(f.Name()), "objects/") {
			w.probes++
		}
	case *os.Root:
		w.roots++
		if _, err := f.Stat("."); err == nil {
			w.open = true
		}
	}
}
func (w *recoveryProbeCloses) check(t *testing.T) {
	t.Helper()
	if w.open {
		t.Error("afterClose observed open resource")
	}
	for _, n := range w.counts {
		if n != 1 {
			t.Error("physical close repeated")
		}
	}
	if w.roots != 1 {
		t.Errorf("root closes=%d", w.roots)
	}
}
func recoveryProbeBytes(t *testing.T, a Attachment) {
	t.Helper()
	b, err := os.ReadFile(a.Path)
	if err != nil || string(b) != "private text\n" {
		t.Fatalf("prior object changed: %q %v", b, err)
	}
}

func TestInstalledRecoveryProbeResolve(t *testing.T) {
	for _, name := range []string{"mismatch-clean", "mismatch-close", "mismatch-remove", "mismatch-persist", "mismatch-siblings", "wrong-size-clean", "wrong-size-close", "wrong-size-remove", "wrong-size-persist", "wrong-size-siblings", "invalid-clean", "invalid-close", "invalid-remove", "invalid-persist", "invalid-siblings"} {
		t.Run(name, func(t *testing.T) {
			m, a, _ := recoveryProbeSeed(t)
			bad, good := a[0], a[1]
			rel := m.attachments[bad.Ref].relPath
			closeFault := errors.New("private probe close")
			removeFault := errors.New("private recovery remove")
			persistFault := errors.New("private recovery persistence")
			siblings := strings.HasSuffix(name, "siblings")
			injectClose := siblings || strings.HasSuffix(name, "close")
			injectRemove := siblings || strings.HasSuffix(name, "remove")
			injectPersist := siblings || strings.HasSuffix(name, "persist")
			w := &recoveryProbeCloses{counts: make(map[io.Closer]int)}
			var callbackErr error
			var openedDifferent bool
			var original os.FileInfo
			var err error
			original, err = os.Stat(bad.Path)
			if err != nil {
				t.Fatal(err)
			}
			removeCalls, persistCalls, closeFaults := 0, 0, 0
			m.installed.afterClose = func(c io.Closer) error {
				w.observe(c)
				f, ok := c.(*os.File)
				if !ok {
					return nil
				}
				isProbe := filepath.Base(f.Name()) == filepath.Base(rel)
				if isProbe && strings.HasPrefix(name, "mismatch") {
					current, e := os.Stat(bad.Path)
					if e != nil {
						callbackErr = e
					} else {
						openedDifferent = !os.SameFile(original, current)
					}
				}
				// Invalid objects never acquire a probe; their close sibling is the
				// independently acquired persistence temporary, not a fabricated probe.
				isTemp := strings.HasPrefix(filepath.Base(f.Name()), ".attachments-")
				if injectClose && (isProbe || (strings.HasPrefix(name, "invalid") && isTemp)) {
					closeFaults++
					return closeFault
				}
				return nil
			}
			replaced := false
			m.installed.denyStage = func(stage, path string) error {
				if stage == "probe-open" && path == rel && strings.HasPrefix(name, "mismatch") && !replaced {
					replaced = true
					if e := os.Rename(bad.Path, bad.Path+".prior"); e != nil {
						callbackErr = e
						return e
					}
					if e := os.WriteFile(bad.Path, []byte("replacement!\n"), 0600); e != nil {
						callbackErr = e
						return e
					}
				}
				if stage == "resolve-remove" {
					removeCalls++
					if injectRemove {
						return removeFault
					}
				}
				// Invalid sibling close fails before publication; use chmod denial so
				// deferred real temp close and persistence denial are both independent.
				if stage == "persist-temp-chmod" {
					persistCalls++
					if injectPersist {
						return persistFault
					}
				}
				return nil
			}
			if strings.HasPrefix(name, "wrong-size") {
				if err := os.WriteFile(bad.Path, []byte("short"), 0600); err != nil {
					t.Fatal(err)
				}
			}
			if strings.HasPrefix(name, "invalid") {
				if err := os.Remove(bad.Path); err != nil {
					t.Fatal(err)
				}
				if err := os.Mkdir(bad.Path, 0700); err != nil {
					t.Fatal(err)
				}
			}
			_, public := m.Resolve(uploadTarget(1), bad.Ref)
			want := "attachment_changed"
			if injectPersist || (injectClose && strings.HasPrefix(name, "invalid")) {
				want = "upload_metadata_unavailable"
			}
			var typed *Error
			if !errors.As(public, &typed) || typed.Code != want {
				t.Fatalf("public classification: %v", public)
			}
			if callbackErr != nil {
				t.Fatal(callbackErr)
			}
			if strings.HasPrefix(name, "mismatch") && (!replaced || !openedDifferent || w.probes != 1) {
				t.Fatal("missing actually opened mismatch close")
			}
			if strings.HasPrefix(name, "invalid") && w.probes != 0 {
				t.Fatal("invalid object unexpectedly opened")
			}
			if removeCalls != 1 || persistCalls != 1 {
				t.Fatalf("recovery stages: %d %d", removeCalls, persistCalls)
			}
			if m.attachments[bad.Ref] != nil || m.attachments[good.Ref] == nil {
				t.Fatal("reference recovery incorrect")
			}
			recoveryProbeBytes(t, good)
			_, pathErr := os.Lstat(bad.Path)
			if injectRemove {
				if pathErr != nil {
					t.Fatal("denied removal changed object")
				}
			} else if !errors.Is(pathErr, os.ErrNotExist) {
				t.Fatal("recovery object not removed")
			}
			if strings.HasPrefix(name, "wrong-size") && w.probes != 1 {
				t.Fatal("wrong-size probe not closed once")
			}
			indexBytes, indexErr := os.ReadFile(filepath.Join(m.rootPath, attachmentIndexFilename))
			if indexErr != nil {
				t.Fatal(indexErr)
			}
			var saved attachmentIndex
			if err := json.Unmarshal(indexBytes, &saved); err != nil {
				t.Fatal(err)
			}
			foundBad, foundGood := false, false
			for _, row := range saved.Attachments {
				foundBad = foundBad || row.Attachment.Ref == bad.Ref
				foundGood = foundGood || row.Attachment.Ref == good.Ref
			}
			persistFailed := injectPersist || (injectClose && strings.HasPrefix(name, "invalid"))
			if !foundGood || foundBad != persistFailed {
				t.Fatal("persistence recovery effect mismatch")
			}
			if strings.HasPrefix(name, "mismatch") {
				b, e := os.ReadFile(bad.Path + ".prior")
				if e != nil || string(b) != "private text\n" {
					t.Fatal("moved original changed")
				}
			}
			result := m.DrainInstalled()
			if injectClose {
				if closeFaults != 1 || !errors.Is(result, closeFault) {
					t.Fatal("close witness lost")
				}
			}
			wantWitnesses := 0
			for _, expected := range []struct {
				enabled bool
				err     error
			}{{injectClose, closeFault}, {injectRemove, removeFault}, {injectPersist, persistFault}} {
				if expected.enabled {
					wantWitnesses++
				}
				if errors.Is(result, expected.err) != expected.enabled {
					t.Fatal("independent witness mismatch")
				}
			}
			if m.installed.count != wantWitnesses || m.installed.overflow != 0 {
				t.Fatal("duplicate or invented cleanup witnesses")
			}
			if wantWitnesses == 0 && result != nil {
				t.Fatalf("validation poisoned drain: %v", result)
			}
			if m.DrainInstalled() != result {
				t.Fatal("drain result not cached")
			}
			w.check(t)
		})
	}
}

func TestInstalledRecoveryProbeLoad(t *testing.T) {
	for _, name := range []string{"valid", "index-close", "probe-close", "expired-remove", "invalid-row", "wrong-size"} {
		t.Run(name, func(t *testing.T) {
			seed, a, cfg := recoveryProbeSeed(t)
			if err := seed.DrainInstalled(); err != nil {
				t.Fatal(err)
			}
			indexPath := filepath.Join(cfg.Root, attachmentIndexFilename)
			data, err := os.ReadFile(indexPath)
			if err != nil {
				t.Fatal(err)
			}
			var index attachmentIndex
			if err := json.Unmarshal(data, &index); err != nil {
				t.Fatal(err)
			}
			// Select by ref: persistence sorts rows, not by insertion order.
			for i := range index.Attachments {
				row := &index.Attachments[i]
				if row.Attachment.Ref != a[0].Ref {
					continue
				}
				switch name {
				case "expired-remove":
					row.Attachment.ExpiresAt = cfg.Now().Add(-time.Minute)
				case "invalid-row":
					row.Attachment.SHA256 = "invalid"
				case "wrong-size":
					row.Attachment.Bytes++
				}
			}
			data, err = json.Marshal(index)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(indexPath, data, 0600); err != nil {
				t.Fatal(err)
			}
			// Recent mtime is independent of expired metadata: no later TTL removal.
			if err := os.Chtimes(a[0].Path, cfg.Now(), cfg.Now()); err != nil {
				t.Fatal(err)
			}
			fault := errors.New("private independent load fault")
			w := &recoveryProbeCloses{counts: make(map[io.Closer]int)}
			o := &installedOwnership{}
			faultCalls, removeCalls := 0, 0
			o.afterClose = func(c io.Closer) error {
				w.observe(c)
				f, ok := c.(*os.File)
				if !ok {
					return nil
				}
				match := name == "index-close" && filepath.Base(f.Name()) == attachmentIndexFilename
				match = match || (name == "probe-close" && filepath.Base(f.Name()) == filepath.Base(a[0].Path))
				if match {
					faultCalls++
					return fault
				}
				return nil
			}
			o.denyStage = func(stage, path string) error {
				if stage == "load-expired-remove" {
					removeCalls++
					if name == "expired-remove" {
						faultCalls++
						return fault
					}
				}
				return nil
			}
			m, constructErr := newManager(cfg, o)
			if m != nil {
				t.Cleanup(func() { _ = m.BeginInstalledDrain(); _ = m.DrainInstalled() })
			}
			fatalExpected := name == "index-close" || name == "probe-close" || name == "expired-remove"
			var fatal *ConstructionCleanupError
			if fatalExpected {
				if m != nil || !errors.As(constructErr, &fatal) || !errors.Is(constructErr, fault) || faultCalls != 1 {
					t.Fatalf("fatal classification: %v calls=%d", constructErr, faultCalls)
				}
				if o.count != 1 || o.overflow != 0 {
					t.Fatal("independent load fault duplicated or poisoned")
				}
				if constructErr.Error() != "upload construction cleanup failed" {
					t.Fatal("unredacted constructor error")
				}
			} else {
				if constructErr != nil || m == nil {
					t.Fatalf("validation recovery poisoned construction: %v", constructErr)
				}
				if m.attachments[a[1].Ref] == nil {
					t.Fatal("valid ref lost")
				}
				wantBad := name == "valid"
				if (m.attachments[a[0].Ref] != nil) != wantBad {
					t.Fatal("row classification incorrect")
				}
				if err := m.DrainInstalled(); err != nil {
					t.Fatal(err)
				}
			}
			if (name == "expired-remove" && removeCalls != 1) || (name != "expired-remove" && removeCalls != 0) {
				t.Fatal("expired removal stage mismatch")
			}
			if w.indexes != 1 {
				t.Fatalf("index reader closes=%d", w.indexes)
			}
			wantProbes := 2
			if name == "invalid-row" || name == "expired-remove" {
				wantProbes = 1
			}
			if w.probes != wantProbes {
				t.Fatalf("probe closes=%d want=%d", w.probes, wantProbes)
			}
			w.check(t)
			recoveryProbeBytes(t, a[0])
			recoveryProbeBytes(t, a[1])
			// Even fatal cleanup does not delete valid objects or their published refs.
			saved, err := os.ReadFile(indexPath)
			if err != nil {
				t.Fatal(err)
			}
			var persisted attachmentIndex
			if err := json.Unmarshal(saved, &persisted); err != nil {
				t.Fatal(err)
			}
			foundGood, foundBad := false, false
			for _, row := range persisted.Attachments {
				foundGood = foundGood || row.Attachment.Ref == a[1].Ref
				foundBad = foundBad || row.Attachment.Ref == a[0].Ref
			}
			if !foundGood {
				t.Fatal("valid persisted reference lost")
			}
			wantBad := name == "valid" || name == "index-close" || name == "probe-close"
			if foundBad != wantBad {
				t.Fatal("persisted recovery classification incorrect")
			}
		})
	}
}
