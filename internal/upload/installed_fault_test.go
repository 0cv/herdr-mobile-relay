package upload

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestInstalledUploadPrepublicationTempClose(t *testing.T) {
	m := installedTestManager(t)
	witness := errors.New("post real temporary close")
	m.installed.afterClose = func(c io.Closer) error {
		if f, ok := c.(*os.File); ok && strings.HasPrefix(filepath.Base(f.Name()), ".attachments-") {
			return witness
		}
		return nil
	}
	err := m.persistAttachments()
	var published *attachmentIndexPublishedError
	if err == nil || errors.As(err, &published) {
		t.Fatal("prepublication misclassified", err)
	}
	entries, err := os.ReadDir(m.rootPath)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), ".attachments-") {
			t.Fatal("temporary leaked")
		}
	}
	if err := m.DrainInstalled(); !errors.Is(err, witness) {
		t.Fatal(err)
	}
}

func TestInstalledUploadZIPCloseWitness(t *testing.T) {
	m := installedTestManager(t)
	data := documentArchive(t, map[string]string{"mimetype": "application/vnd.oasis.opendocument.text"})
	target := uploadTarget(0)
	begun := beginOne(t, m, target, "a.odt", "application/vnd.oasis.opendocument.text", int64(len(data)))
	sendChunk(t, m, target, begun.UploadID, 0, 0, data)
	witness := errors.New("post real ZIP entry close")
	m.installed.afterClose = func(c io.Closer) error {
		if _, ok := c.(*os.File); ok {
			return nil
		}
		if _, ok := c.(*os.Root); ok {
			return nil
		}
		return witness
	}
	_, err := m.Finish(FinishRequest{Target: target, UploadID: begun.UploadID, Files: []FileDigest{{SHA256: digestFor(data)}}})
	if err == nil {
		t.Fatal("ZIP close fault accepted")
	}
	if err := m.DrainInstalled(); !errors.Is(err, witness) {
		t.Fatal(err)
	}
}

func TestInstalledUploadResolveProbeCloseWitness(t *testing.T) {
	m := installedTestManager(t)
	target := uploadTarget(0)
	begun := beginOne(t, m, target, "a.txt", "text/plain", 1)
	sendChunk(t, m, target, begun.UploadID, 0, 0, []byte("a"))
	result, err := m.Finish(FinishRequest{Target: target, UploadID: begun.UploadID, Files: []FileDigest{{SHA256: digestFor([]byte("a"))}}})
	if err != nil {
		t.Fatal(err)
	}
	witness := errors.New("post real probe close")
	m.installed.afterClose = func(c io.Closer) error {
		if f, ok := c.(*os.File); ok && strings.HasSuffix(f.Name(), ".txt") {
			return witness
		}
		return nil
	}
	attachment, err := m.Resolve(target, result.Attachments[0].Ref)
	if err != nil || attachment.Ref != result.Attachments[0].Ref {
		t.Fatal("probe changed existing recovery semantics", err)
	}
	if err := m.DrainInstalled(); !errors.Is(err, witness) {
		t.Fatal(err)
	}
	if _, err := os.Stat(attachment.Path); err != nil {
		t.Fatal(err)
	}
}

func TestInstalledUploadCountOnlyCleanupRemovalWitness(t *testing.T) {
	m := installedTestManager(t)
	// Private nonempty directory forces a real removal error without preclosing
	// descriptors or replacing any host resource.
	name := "objects/not-empty"
	if err := m.root.Mkdir(name, 0700); err != nil {
		t.Fatal(err)
	}
	file, err := m.root.OpenFile(name+"/child", os.O_CREATE|os.O_WRONLY, 0600)
	if err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	record := &attachmentRecord{relPath: name, attachment: Attachment{Ref: "expired"}}
	m.attachments["expired"] = record
	m.Cleanup()
	if len(m.attachments) != 0 {
		t.Fatal("expired record not detached")
	}
	err = m.DrainInstalled()
	var pathError *os.PathError
	if !errors.As(err, &pathError) {
		t.Fatalf("count-only removal failure lost: %v", err)
	}
}
