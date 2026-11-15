package fetch

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

type testEntry struct {
	name string
	mode int64
	body []byte
	link string
}

func writeArchive(t *testing.T, entries []testEntry) string {
	t.Helper()
	var archive bytes.Buffer
	compressor := gzip.NewWriter(&archive)
	writer := tar.NewWriter(compressor)
	for _, entry := range entries {
		header := &tar.Header{
			Name:     entry.name,
			Mode:     entry.mode,
			Size:     int64(len(entry.body)),
			Typeflag: tar.TypeReg,
		}
		if entry.link != "" {
			header.Typeflag = tar.TypeSymlink
			header.Linkname = entry.link
			header.Size = 0
		} else if entry.body == nil {
			header.Typeflag = tar.TypeDir
			header.Size = 0
		}
		if err := writer.WriteHeader(header); err != nil {
			t.Fatal(err)
		}
		if _, err := writer.Write(entry.body); err != nil {
			t.Fatal(err)
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	if err := compressor.Close(); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "archive.tar.gz")
	if err := os.WriteFile(path, archive.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestExtractTarGzUnpacksFilesWithNormalizedModes(t *testing.T) {
	archive := writeArchive(t, []testEntry{
		{name: "app/", mode: 0o700},
		{name: "app/run", mode: 0o700, body: []byte("#!/bin/sh")},
		{name: "app/data", mode: 0o600, body: []byte("data")},
	})
	destination := t.TempDir()
	if err := ExtractTarGz(archive, destination, false); err != nil {
		t.Fatal(err)
	}
	for name, want := range map[string]os.FileMode{"app/run": 0o755, "app/data": 0o644} {
		info, err := os.Stat(filepath.Join(destination, name))
		if err != nil {
			t.Fatal(err)
		}
		if info.Mode().Perm() != want {
			t.Fatalf("%s mode = %v, want %v", name, info.Mode().Perm(), want)
		}
	}
}

func TestExtractTarGzRejectsPathTraversal(t *testing.T) {
	root := t.TempDir()
	archive := writeArchive(t, []testEntry{{name: "../escaped", mode: 0o600, body: []byte("no")}})
	destination := filepath.Join(root, "release")
	if err := os.Mkdir(destination, 0o700); err != nil {
		t.Fatal(err)
	}
	err := ExtractTarGz(archive, destination, true)
	if err == nil || !strings.Contains(err.Error(), "escapes") {
		t.Fatalf("path traversal error = %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "escaped")); !os.IsNotExist(err) {
		t.Fatalf("path traversal created a file: %v", err)
	}
}

func TestExtractTarGzRejectsChainedTraversal(t *testing.T) {
	archive := writeArchive(t, []testEntry{
		{name: "piper/", mode: 0o755},
		{name: "piper/a", mode: 0o755, link: "."},
		{name: "piper/b", mode: 0o755, link: "a/../.."},
		{name: "piper/b/escaped", mode: 0o644, body: []byte("outside")},
	})
	destination := t.TempDir()
	outside := filepath.Join(filepath.Dir(destination), "escaped")
	_ = os.Remove(outside)
	err := ExtractTarGz(archive, destination, true)
	if err == nil || !strings.Contains(err.Error(), "unsafe link target") {
		t.Fatalf("ExtractTarGz() error = %v, want unsafe link target", err)
	}
	if _, err := os.Lstat(outside); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("path outside extraction root = %v, want absent", err)
	}
}

func TestExtractTarGzLinksFollowThePolicy(t *testing.T) {
	archive := writeArchive(t, []testEntry{
		{name: "lib/", mode: 0o755},
		{name: "lib/real.so", mode: 0o644, body: []byte("lib")},
		{name: "lib/link.so", mode: 0o755, link: "real.so"},
	})
	err := ExtractTarGz(archive, t.TempDir(), false)
	if err == nil || !strings.Contains(err.Error(), "unsupported entry") {
		t.Fatalf("links refused: error = %v", err)
	}
	destination := t.TempDir()
	if err := ExtractTarGz(archive, destination, true); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(destination, "lib", "link.so"))
	if err != nil || string(data) != "lib" {
		t.Fatalf("link content = %q, %v", data, err)
	}
}
