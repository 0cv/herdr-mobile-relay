package upload

import (
	"archive/zip"
	"bytes"
	"errors"
	"io"
	"os"
	"strings"
	"testing"
)

func TestInstalledRecoveryZIPValidation(t *testing.T) {
	for _, row := range []string{"read", "read-close", "content", "content-close"} {
		t.Run(row, func(t *testing.T) {
			var buf bytes.Buffer
			writer := zip.NewWriter(&buf)
			// In-memory writer has no external resource; close before any manager acquisition.
			w, e := writer.CreateHeader(&zip.FileHeader{Name: "mimetype", Method: zip.Store})
			if e != nil {
				_ = writer.Close()
				t.Fatal(e)
			}
			content := "application/vnd.oasis.opendocument.text"
			if strings.HasPrefix(row, "content") {
				content = "private-invalid-mimetype"
			}
			if _, e = w.Write([]byte(content)); e != nil {
				_ = writer.Close()
				t.Fatal(e)
			}
			if e = writer.Close(); e != nil {
				t.Fatal(e)
			}
			data := buf.Bytes()
			archive, e := zip.NewReader(bytes.NewReader(data), int64(len(data)))
			if e != nil {
				t.Fatal(e)
			}
			if strings.HasPrefix(row, "read") {
				offset, e := archive.File[0].DataOffset()
				if e != nil {
					t.Fatal(e)
				}
				data[offset] ^= 1 // Valid opened Store entry, bad CRC on real read, not Open.
			}
			m := installedTestManager(t)
			target := uploadTarget(0)
			begun := beginOne(t, m, target, "a.odt", "application/vnd.oasis.opendocument.text", int64(len(data)))
			sendChunk(t, m, target, begun.UploadID, 0, 0, data)
			fault := errors.New("private ZIP close fault")
			entryCloses, rootCloses, fileCloses := 0, 0, 0
			m.installed.afterClose = func(c io.Closer) error {
				switch c.(type) {
				case *os.File:
					fileCloses++
					return nil
				case *os.Root:
					rootCloses++
					return nil
				default:
					entryCloses++
					if strings.HasSuffix(row, "-close") {
						return fault
					}
					return nil
				}
			}
			_, e = m.Finish(FinishRequest{Target: target, UploadID: begun.UploadID, Files: []FileDigest{{SHA256: digestFor(data)}}})
			if e == nil || strings.Contains(e.Error(), "private") {
				t.Fatal("validation redaction")
			}
			result := m.DrainInstalled()
			if entryCloses != 1 || rootCloses != 1 || fileCloses != 1 {
				t.Fatal("first-close count")
			}
			if strings.HasSuffix(row, "-close") {
				if terminalMatrixOccurrences(t, result, fault) != 1 {
					t.Fatal("close sibling lost")
				}
			} else if result != nil {
				t.Fatal("clean validation poisoned drain")
			}
			if len(m.sessions) != 0 || len(m.attachments) != 0 {
				t.Fatal("validation published or retained session")
			}
			entries, e := os.ReadDir(m.rootPath + "/sessions")
			if e != nil || len(entries) != 0 {
				t.Fatal("session cleanup")
			}
			entries, e = os.ReadDir(m.rootPath + "/objects")
			if e != nil || len(entries) != 0 {
				t.Fatal("object cleanup")
			}
			if m.DrainInstalled() != result || entryCloses != 1 || rootCloses != 1 {
				t.Fatal("terminal cache")
			}
		})
	}
}
