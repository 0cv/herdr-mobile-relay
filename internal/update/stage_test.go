package update

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestPrepareTargetReleaseDownloadsVerifiesAndChecksCompatibility(t *testing.T) {
	root := t.TempDir()
	releaseRoot := filepath.Join(root, "installed")
	currentRoot := filepath.Join(releaseRoot, "releases", "current-test")
	if err := os.MkdirAll(currentRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	writeWorkerTestRelease(t, currentRoot, "1.2.3", currentTestRevision)
	if err := os.Symlink(filepath.Join("releases", "current-test"), filepath.Join(releaseRoot, "current")); err != nil {
		t.Fatal(err)
	}

	targetRoot := filepath.Join(root, "target")
	if err := os.MkdirAll(targetRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	writeWorkerTestRelease(t, targetRoot, "1.2.4", nextTestRevision)
	archive := releaseArchive(t, targetRoot)
	checksum := sha256.Sum256(archive)
	archiveName := "herdr-mobile-relay_1.2.4_" + strings.ReplaceAll(currentTargetForTest(), "/", "_") + ".tar.gz"

	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		switch request.URL.Path {
		case "/v1.2.4/checksums.txt":
			fmt.Fprintf(writer, "%x  %s\n", checksum, archiveName)
		case "/v1.2.4/" + archiveName:
			_, _ = writer.Write(archive)
		default:
			http.NotFound(writer, request)
		}
	}))
	defer server.Close()

	runtimeDir := filepath.Join(root, "runtime")
	if err := os.MkdirAll(runtimeDir, 0o700); err != nil {
		t.Fatal(err)
	}
	job := Job{
		ReleaseRoot:    releaseRoot,
		TargetVersion:  "1.2.4",
		TargetRevision: nextTestRevision,
		StatePath:      filepath.Join(runtimeDir, "update-state.json"),
	}
	staged, err := prepareTargetReleaseFrom(t.Context(), job, server.URL, server.Client())
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(staged.Root)
	if staged.Manifest.Version != job.TargetVersion || staged.Manifest.Revision != job.TargetRevision {
		t.Fatalf("staged manifest = %#v", staged.Manifest)
	}
	if _, err := os.Stat(filepath.Join(staged.Root, "release", "web", "index.html")); err != nil {
		t.Fatalf("staged web bundle: %v", err)
	}
}

func TestExtractReleaseArchiveRejectsPathTraversal(t *testing.T) {
	root := t.TempDir()
	archivePath := filepath.Join(root, "malicious.tar.gz")
	archive := singleFileArchive(t, "../escaped", []byte("no"))
	if err := os.WriteFile(archivePath, archive, 0o600); err != nil {
		t.Fatal(err)
	}
	destination := filepath.Join(root, "release")
	if err := os.Mkdir(destination, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := extractReleaseArchive(archivePath, destination); err == nil || !strings.Contains(err.Error(), "escapes") {
		t.Fatalf("path traversal error = %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "escaped")); !os.IsNotExist(err) {
		t.Fatalf("path traversal created a file: %v", err)
	}
}

func releaseArchive(t *testing.T, root string) []byte {
	t.Helper()
	var buffer bytes.Buffer
	compressed := gzip.NewWriter(&buffer)
	archive := tar.NewWriter(compressed)
	err := filepath.WalkDir(root, func(filename string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if filename == root {
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		header, err := tar.FileInfoHeader(info, "")
		if err != nil {
			return err
		}
		relative, err := filepath.Rel(root, filename)
		if err != nil {
			return err
		}
		header.Name = "./" + filepath.ToSlash(relative)
		if err := archive.WriteHeader(header); err != nil {
			return err
		}
		if entry.IsDir() {
			return nil
		}
		file, err := os.Open(filename)
		if err != nil {
			return err
		}
		_, copyErr := io.Copy(archive, file)
		closeErr := file.Close()
		if copyErr != nil {
			return copyErr
		}
		return closeErr
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := archive.Close(); err != nil {
		t.Fatal(err)
	}
	if err := compressed.Close(); err != nil {
		t.Fatal(err)
	}
	return buffer.Bytes()
}

func singleFileArchive(t *testing.T, name string, contents []byte) []byte {
	t.Helper()
	var buffer bytes.Buffer
	compressed := gzip.NewWriter(&buffer)
	archive := tar.NewWriter(compressed)
	if err := archive.WriteHeader(&tar.Header{Name: name, Mode: 0o600, Size: int64(len(contents)), Typeflag: tar.TypeReg}); err != nil {
		t.Fatal(err)
	}
	if _, err := archive.Write(contents); err != nil {
		t.Fatal(err)
	}
	if err := archive.Close(); err != nil {
		t.Fatal(err)
	}
	if err := compressed.Close(); err != nil {
		t.Fatal(err)
	}
	return buffer.Bytes()
}
