package upload

import (
	"bytes"
	"encoding/base64"
	"errors"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func TestInstalledFinishUnindexedCollisionPreservation(t *testing.T) {
	for _, name := range []string{"first", "later"} {
		t.Run(name, func(t *testing.T) {
			m, target := finishFixture(t)
			prior, err := m.Finish(finishStage(t, m, target, 1))
			if err != nil {
				t.Fatal(err)
			}
			a := prior.Attachments[0]
			indexPath := filepath.Join(m.rootPath, attachmentIndexFilename)
			index, err := os.ReadFile(indexPath)
			if err != nil {
				t.Fatal(err)
			}
			priorBytes, err := os.ReadFile(a.Path)
			if err != nil {
				t.Fatal(err)
			}
			req := finishStage(t, m, target, 2)
			files := append([]*sessionFile(nil), m.sessions[req.UploadID].files...)
			handles := []io.Closer{files[0].file, files[1].file, m.root}
			id := func(b byte) string { return base64.RawURLEncoding.EncodeToString(bytes.Repeat([]byte{b}, 24)) }
			occupied := "objects/" + id(7) + canonicalExtension("text/plain")
			own := "objects/" + id(8) + canonicalExtension("text/plain")
			if _, exists := m.attachments[id(7)]; exists {
				t.Fatal("destination already indexed")
			}
			occupiedBytes := []byte("private unindexed bytes")
			if err := os.WriteFile(filepath.Join(m.rootPath, occupied), occupiedBytes, 0600); err != nil {
				t.Fatal(err)
			}
			random := bytes.Repeat([]byte{7}, 48)
			want := 0
			if name == "later" {
				random = append(bytes.Repeat([]byte{8}, 24), bytes.Repeat([]byte{7}, 24)...)
				want = 1
			}
			m.random = bytes.NewReader(random)
			physical := make(map[io.Closer]int)
			closed := make(map[io.Closer]bool)
			m.installed.afterClose = func(c io.Closer) error {
				physical[c]++
				switch v := c.(type) {
				case *os.File:
					_, e := v.Stat()
					closed[c] = errors.Is(e, os.ErrClosed)
				case *os.Root:
					_, e := v.Stat(".")
					closed[c] = errors.Is(e, os.ErrClosed)
				}
				return nil
			}
			var renames, removals []string
			ownExisted := false
			m.installed.denyStage = func(stage, path string) error {
				if stage == "finish-object-rename" {
					renames = append(renames, path)
				}
				if stage == "finish-object-remove" {
					removals = append(removals, path)
					data, e := m.root.ReadFile(path)
					ownExisted = path == own && e == nil && bytes.Equal(data, []byte("new"))
				}
				return nil
			}
			result, err := m.Finish(req)
			var public *Error
			if !errors.As(err, &public) || public.Code != "upload_staging_failed" || len(result.Attachments) != 0 {
				t.Fatal("collision accepted", err)
			}
			if len(renames) != want || len(removals) != want {
				t.Fatal("incorrect effect counts", renames, removals)
			}
			for _, path := range renames {
				if path == occupied || path != own {
					t.Fatal("rename into occupied or foreign destination")
				}
			}
			if want == 1 && (removals[0] != own || !ownExisted) {
				t.Fatal("rollback did not remove only real attempt object")
			}
			if len(m.attachments) != 1 {
				t.Fatal("accidental publication")
			}
			resolved, err := m.Resolve(target, a.Ref)
			if err != nil || !reflect.DeepEqual(resolved, a) {
				t.Fatal("prior ref changed", err)
			}
			if _, exists := m.attachments[id(7)]; exists {
				t.Fatal("unindexed object adopted")
			}
			if len(m.sessions) != 0 {
				t.Fatal("attempt session retained")
			}
			entries, err := os.ReadDir(filepath.Join(m.rootPath, "sessions"))
			if err != nil || len(entries) != 0 {
				t.Fatal("staging retained", err)
			}
			if err := m.DrainInstalled(); err != nil {
				t.Fatal(err)
			}
			if m.DrainInstalled() != nil || m.Close() != nil {
				t.Fatal("unclean cached terminal result")
			}
			for _, h := range handles {
				if physical[h] != 1 || !closed[h] {
					t.Fatal("missing first real close")
				}
			}
			for c, n := range physical {
				if n != 1 || !closed[c] {
					t.Fatal("repeated or skipped close")
				}
			}
			for _, f := range files {
				if f.file != nil || f.closeErr != nil {
					t.Fatal("session close state")
				}
			}
			for path, before := range map[string][]byte{a.Path: priorBytes, indexPath: index, filepath.Join(m.rootPath, occupied): occupiedBytes} {
				after, e := os.ReadFile(path)
				if e != nil || !bytes.Equal(before, after) {
					t.Fatal("preserved bytes changed", path, e)
				}
			}
			objects, err := os.ReadDir(filepath.Join(m.rootPath, "objects"))
			if err != nil || len(objects) != 2 {
				t.Fatal("rollback inventory", err)
			}
			if _, err := os.Stat(filepath.Join(m.rootPath, own)); !errors.Is(err, os.ErrNotExist) {
				t.Fatal("attempt object remains", err)
			}
		})
	}
}
