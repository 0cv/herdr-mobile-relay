package upload

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/0cv/herdr-mobile-relay/internal/protocol"
)

func finishFixture(t *testing.T) (*Manager, protocol.TargetRef) {
	t.Helper()
	m, err := NewInstalledManager(Config{Root: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = m.DrainInstalled() })
	return m, protocol.TargetRef{ServerSessionID: "server", PaneID: "pane", TerminalID: "terminal"}
}
func finishStage(t *testing.T, m *Manager, target protocol.TargetRef, count int) FinishRequest {
	t.Helper()
	specs := make([]FileSpec, count)
	for i := range specs {
		specs[i] = FileSpec{Name: "note.txt", MediaType: "text/plain", Bytes: 3}
	}
	b, err := m.Begin(BeginRequest{Target: target, Files: specs})
	if err != nil {
		t.Fatal(err)
	}
	request := FinishRequest{Target: target, UploadID: b.UploadID}
	sum := sha256.Sum256([]byte("new"))
	digest := hex.EncodeToString(sum[:])
	for i := range specs {
		_, err = m.Chunk(ChunkRequest{Target: target, UploadID: b.UploadID, FileIndex: i, Sequence: i, Data: []byte("new"), SHA256: digest})
		if err != nil {
			t.Fatal(err)
		}
		request.Files = append(request.Files, FileDigest{FileIndex: i, SHA256: digest})
	}
	return request
}

func TestInstalledFinishBoundedStages(t *testing.T) {
	for _, name := range []string{"write", "sync", "validation", "first-close", "later-id", "later-close", "later-rename", "rollback-siblings", "objects-open", "objects-sync", "objects-close", "objects-siblings", "published-discard"} {
		t.Run(name, func(t *testing.T) {
			m, target := finishFixture(t)
			var req FinishRequest
			if name == "write" {
				specs := []FileSpec{{Name: "a.txt", MediaType: "text/plain", Bytes: 3}, {Name: "b.txt", MediaType: "text/plain", Bytes: 3}, {Name: "c.txt", MediaType: "text/plain", Bytes: 3}}
				b, e := m.Begin(BeginRequest{Target: target, Files: specs})
				if e != nil {
					t.Fatal(e)
				}
				sum := sha256.Sum256([]byte("new"))
				req = FinishRequest{Target: target, UploadID: b.UploadID, Files: []FileDigest{{SHA256: hex.EncodeToString(sum[:])}}}
			} else {
				req = finishStage(t, m, target, 3)
			}
			files := append([]*sessionFile(nil), m.sessions[req.UploadID].files...)
			physical := make(map[io.Closer]int)
			handles := make([]io.Closer, len(files))
			for i, f := range files {
				handles[i] = f.file
			}
			primary := errors.New("private-primary")
			closeFault := errors.New("private-close")
			removeFault := errors.New("private-remove")
			removeSibling := errors.New("private-remove-sibling")
			removeCalls := 0
			rootFault := errors.New("private-root")
			renameCalls := 0
			m.installed.afterClose = func(c io.Closer) error {
				physical[c]++
				if c == m.root {
					return rootFault
				}
				if (name == "first-close" && c == handles[0]) || (name == "later-close" && c == handles[1]) {
					return primary
				}
				if (name == "write" || name == "sync" || name == "validation" || name == "first-close") && c == handles[2] {
					return closeFault
				}
				if f, ok := c.(*os.File); ok && filepath.Base(f.Name()) == "objects" && (name == "objects-close" || name == "objects-siblings") {
					return closeFault
				}
				return nil
			}
			m.installed.denyStage = func(stage, path string) error {
				if stage == "session-remove" && (name == "write" || name == "sync" || name == "validation" || name == "published-discard") {
					return removeFault
				}
				if stage == "finish-object-remove" && name == "rollback-siblings" {
					removeCalls++
					if removeCalls == 2 {
						return removeSibling
					}
					return removeFault
				}
				if stage == "finish-object-rename" {
					renameCalls++
					if (name == "later-rename" || name == "rollback-siblings") && renameCalls == 3 {
						return primary
					}
				}
				if (stage == "chunk-write" && name == "write") || (stage == "finish-file-sync" && name == "sync") || (stage == "finish-objects-open" && name == "objects-open") || (stage == "finish-objects-sync" && (name == "objects-sync" || name == "objects-siblings")) {
					return primary
				}
				return nil
			}
			if name == "later-id" {
				m.random = bytes.NewReader(bytes.Repeat([]byte{7}, 24))
			}
			if name == "validation" {
				req.Files[0].SHA256 = strings.Repeat("0", 64)
			}
			var result FinishResult
			var err error
			if name == "write" {
				_, err = m.Chunk(ChunkRequest{Target: target, UploadID: req.UploadID, Data: []byte("new"), SHA256: req.Files[0].SHA256})
			} else {
				result, err = m.Finish(req)
			}
			if name == "published-discard" {
				if err != nil || len(result.Attachments) != 3 {
					t.Fatalf("publication: %v", err)
				}
				for _, a := range result.Attachments {
					if _, e := m.Resolve(target, a.Ref); e != nil {
						t.Fatal(e)
					}
				}
			} else {
				if err == nil || len(result.Attachments) != 0 || len(m.attachments) != 0 {
					t.Fatal("partial success")
				}
				if strings.Contains(err.Error(), "private") {
					t.Fatal("unredacted error")
				}
			}
			drain := m.DrainInstalled()
			if !errors.Is(drain, rootFault) {
				t.Fatal("root witness lost")
			}
			if m.DrainInstalled() != drain {
				t.Fatal("uncached drain")
			}
			for _, h := range handles {
				if physical[h] != 1 {
					t.Fatalf("close count %d", physical[h])
				}
			}
			if physical[m.root] != 1 {
				t.Fatal("root close count")
			}
			wantClose := name == "write" || name == "sync" || name == "validation" || name == "first-close" || name == "objects-close" || name == "objects-siblings"
			if errors.Is(drain, closeFault) != wantClose {
				t.Fatal("close sibling mismatch")
			}
			wantRemove := name == "write" || name == "sync" || name == "validation" || name == "published-discard" || name == "rollback-siblings"
			if errors.Is(drain, removeFault) != wantRemove {
				t.Fatal("remove sibling mismatch")
			}
			if errors.Is(drain, removeSibling) != (name == "rollback-siblings") {
				t.Fatal("rollback sibling lost")
			}
			wantPrimary := name == "first-close" || name == "later-close" || name == "objects-open" || name == "objects-sync" || name == "objects-siblings"
			if errors.Is(drain, primary) != wantPrimary {
				t.Fatal("primary witness mismatch")
			}
			objects, e := os.ReadDir(filepath.Join(m.rootPath, "objects"))
			if e != nil {
				t.Fatal(e)
			}
			want := 0
			if name == "rollback-siblings" {
				want = 2
			}
			if name == "published-discard" {
				want = 3
			}
			if len(objects) != want {
				t.Fatalf("objects %d want %d", len(objects), want)
			}
			_, e = os.Stat(filepath.Join(m.rootPath, "sessions", req.UploadID))
			sessionLeft := wantRemove && name != "rollback-siblings"
			if sessionLeft && e != nil {
				t.Fatal("missing leftover")
			}
			if !sessionLeft && !os.IsNotExist(e) {
				t.Fatal("session not removed")
			}
		})
	}
}

func TestInstalledFinishCollisionPreservesPublishedBytes(t *testing.T) {
	for _, name := range []string{"existing", "pending"} {
		t.Run(name, func(t *testing.T) {
			m, target := finishFixture(t)
			first := finishStage(t, m, target, 1)
			m.random = bytes.NewReader(bytes.Repeat([]byte{9}, 24))
			published, err := m.Finish(first)
			if err != nil {
				t.Fatal(err)
			}
			a := published.Attachments[0]
			before, err := os.ReadFile(a.Path)
			if err != nil {
				t.Fatal(err)
			}
			indexPath := filepath.Join(m.rootPath, attachmentIndexFilename)
			index, err := os.ReadFile(indexPath)
			if err != nil {
				t.Fatal(err)
			}
			m.random = bytes.NewReader(bytes.Repeat([]byte{8}, 24))
			next := finishStage(t, m, target, 2)
			value := byte(9)
			if name == "pending" {
				value = 7
			}
			m.random = bytes.NewReader(bytes.Repeat([]byte{value}, 48))
			renamed := 0
			m.installed.denyStage = func(stage, path string) error {
				if stage == "finish-object-rename" {
					renamed++
				}
				return nil
			}
			result, err := m.Finish(next)
			if err == nil || len(result.Attachments) != 0 {
				t.Fatal("collision accepted")
			}
			want := 0
			if name == "pending" {
				want = 1
			}
			if renamed != want {
				t.Fatal("collision rename dispatched")
			}
			after, e := os.ReadFile(a.Path)
			if e != nil || !bytes.Equal(before, after) {
				t.Fatal("published bytes lost")
			}
			after, e = os.ReadFile(indexPath)
			if e != nil || !bytes.Equal(index, after) {
				t.Fatal("index changed")
			}
			if _, e = m.Resolve(target, a.Ref); e != nil {
				t.Fatal(e)
			}
			entries, e := os.ReadDir(filepath.Join(m.rootPath, "objects"))
			if e != nil || len(entries) != 1 {
				t.Fatal("rollback object mismatch")
			}
			if e = m.DrainInstalled(); e != nil {
				t.Fatal(e)
			}
		})
	}
}
