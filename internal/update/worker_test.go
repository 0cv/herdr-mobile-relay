package update

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	relayrelease "github.com/0cv/herdr-mobile-relay/internal/release"
)

func TestVerifyChecksumRequiresOneExactEntry(t *testing.T) {
	root := t.TempDir()
	archive := filepath.Join(root, "release.tar.gz")
	if err := os.WriteFile(archive, []byte("release"), 0o600); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256([]byte("release"))
	checksums := filepath.Join(root, "checksums.txt")
	if err := os.WriteFile(checksums, []byte(hex.EncodeToString(sum[:])+"  release.tar.gz\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := verifyChecksum(archive, checksums, "release.tar.gz"); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(checksums, []byte(
		hex.EncodeToString(sum[:])+"  release.tar.gz\n"+
			hex.EncodeToString(sum[:])+"  release.tar.gz\n",
	), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := verifyChecksum(archive, checksums, "release.tar.gz"); err == nil {
		t.Fatal("duplicate checksum accepted")
	}
}

func TestExtractArchiveRejectsTraversalAndLinks(t *testing.T) {
	for name, header := range map[string]tar.Header{
		"traversal": {Name: "../escape", Typeflag: tar.TypeReg, Size: 1},
		"symlink":   {Name: "link", Typeflag: tar.TypeSymlink, Linkname: "target"},
	} {
		t.Run(name, func(t *testing.T) {
			var data bytes.Buffer
			compressed := gzip.NewWriter(&data)
			writer := tar.NewWriter(compressed)
			if err := writer.WriteHeader(&header); err != nil {
				t.Fatal(err)
			}
			if header.Typeflag == tar.TypeReg {
				if _, err := writer.Write([]byte("x")); err != nil {
					t.Fatal(err)
				}
			}
			if err := writer.Close(); err != nil {
				t.Fatal(err)
			}
			if err := compressed.Close(); err != nil {
				t.Fatal(err)
			}
			archive := filepath.Join(t.TempDir(), "archive.tar.gz")
			if err := os.WriteFile(archive, data.Bytes(), 0o600); err != nil {
				t.Fatal(err)
			}
			err := extractArchive(archive, t.TempDir())
			if err == nil {
				t.Fatal("unsafe archive accepted")
			}
			if name == "traversal" && !strings.Contains(err.Error(), "escapes") {
				t.Fatalf("error = %v", err)
			}
		})
	}
}

func TestExtractArchiveAcceptsPackagerRootDirectoryEntry(t *testing.T) {
	var data bytes.Buffer
	compressed := gzip.NewWriter(&data)
	writer := tar.NewWriter(compressed)
	for _, header := range []tar.Header{
		{Name: "./", Typeflag: tar.TypeDir, Mode: 0o755},
		{Name: "./README.md", Typeflag: tar.TypeReg, Mode: 0o644, Size: 6},
	} {
		if err := writer.WriteHeader(&header); err != nil {
			t.Fatal(err)
		}
		if header.Typeflag == tar.TypeReg {
			if _, err := writer.Write([]byte("readme")); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	if err := compressed.Close(); err != nil {
		t.Fatal(err)
	}
	archive := filepath.Join(t.TempDir(), "archive.tar.gz")
	if err := os.WriteFile(archive, data.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
	destination := t.TempDir()
	if err := extractArchive(archive, destination); err != nil {
		t.Fatal(err)
	}
	if content, err := os.ReadFile(filepath.Join(destination, "README.md")); err != nil || string(content) != "readme" {
		t.Fatalf("extracted README = %q, %v", content, err)
	}
}

func TestActivateKeepsCompleteReleaseTarget(t *testing.T) {
	root := t.TempDir()
	releaseDir := filepath.Join(root, "releases", "one")
	if err := os.MkdirAll(releaseDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := Activate(root, releaseDir); err != nil {
		t.Fatal(err)
	}
	target, err := os.Readlink(filepath.Join(root, "current"))
	if err != nil {
		t.Fatal(err)
	}
	if target != filepath.Join("releases", "one") {
		t.Fatalf("target = %q", target)
	}
}

func TestWorkerRollsBackPostActivationFailures(t *testing.T) {
	for _, failure := range []string{"restart", "health"} {
		t.Run(failure, func(t *testing.T) {
			root := t.TempDir()
			releaseRoot := filepath.Join(root, "installed")
			previousDir := filepath.Join(releaseRoot, "releases", "previous")
			nextSource := filepath.Join(root, "next")
			writeWorkerTestRelease(t, previousDir, "1.0.0", "old-revision")
			writeWorkerTestRelease(t, nextSource, "1.1.0", "new-revision")
			if err := Activate(releaseRoot, previousDir); err != nil {
				t.Fatal(err)
			}

			archive := archiveWorkerTestRelease(t, nextSource)
			sum := sha256.Sum256(archive)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if strings.HasSuffix(r.URL.Path, "checksums.txt") {
					_, _ = io.WriteString(w, hex.EncodeToString(sum[:])+"  release.tar.gz\n")
					return
				}
				_, _ = w.Write(archive)
			}))
			defer server.Close()

			statePath := filepath.Join(root, "runtime", "update-state.json")
			jobPath := filepath.Join(root, "runtime", "update-job.json")
			job := Job{
				ReleaseRoot:    releaseRoot,
				DownloadURL:    server.URL + "/release.tar.gz",
				ChecksumURL:    server.URL + "/checksums.txt",
				ArchiveName:    "release.tar.gz",
				TargetVersion:  "1.1.0",
				TargetRevision: "new-revision",
				Target:         relayrelease.CurrentTarget(),
				StatePath:      statePath,
				ServiceName:    "test.service",
				HealthURL:      "http://127.0.0.1:1/healthz",
			}
			if err := os.MkdirAll(filepath.Dir(jobPath), 0o700); err != nil {
				t.Fatal(err)
			}
			jobData, _ := json.Marshal(job)
			if err := os.WriteFile(jobPath, jobData, 0o600); err != nil {
				t.Fatal(err)
			}

			restarts := 0
			verifies := 0
			worker := Worker{
				Client: server.Client(),
				Restart: func(_ context.Context, _ string) error {
					restarts++
					if failure == "restart" && restarts == 1 {
						return errors.New("injected restart failure")
					}
					return nil
				},
				Verify: func(_ context.Context, _ string, manifest relayrelease.Manifest) error {
					verifies++
					if failure == "health" && manifest.Version == "1.1.0" {
						return errors.New("injected health failure")
					}
					return nil
				},
			}
			err := worker.Run(context.Background(), jobPath)
			if err == nil || !strings.Contains(err.Error(), "rolled back") {
				t.Fatalf("worker error = %v, want successful rollback", err)
			}

			target, err := os.Readlink(filepath.Join(releaseRoot, "current"))
			if err != nil {
				t.Fatal(err)
			}
			if !filepath.IsAbs(target) {
				target = filepath.Join(releaseRoot, target)
			}
			if filepath.Clean(target) != filepath.Clean(previousDir) {
				t.Fatalf("current target = %q, want previous %q", target, previousDir)
			}
			var state State
			data, err := os.ReadFile(statePath)
			if err != nil {
				t.Fatal(err)
			}
			if err := json.Unmarshal(data, &state); err != nil {
				t.Fatal(err)
			}
			if state.State != "rolled_back" || state.CurrentVersion != "1.0.0" {
				t.Fatalf("rollback state = %+v", state)
			}
			wantVerifies := 1
			if failure == "health" {
				wantVerifies = 2
			}
			if restarts != 2 || verifies != wantVerifies {
				t.Fatalf("restart/verify calls = %d/%d, want 2/%d", restarts, verifies, wantVerifies)
			}
		})
	}
}

func writeWorkerTestRelease(t *testing.T, root, version, revision string) {
	t.Helper()
	files := []string{
		"herdr-mobile-relay",
		"web/index.html",
		"LICENSE",
		"README.md",
		"relay/common.sh",
		"relay/herdr-mobile-relay-service.sh",
		"relay/plugin-on-event.sh",
		"relay/setup-link.sh",
		"relay/stable-setup.sh",
		"relay/stable-teardown.sh",
		"relay/start.sh",
	}
	for _, name := range files {
		filename := filepath.Join(root, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(filename), 0o755); err != nil {
			t.Fatal(err)
		}
		mode := os.FileMode(0o644)
		if name == "herdr-mobile-relay" {
			mode = 0o755
		}
		if err := os.WriteFile(filename, []byte(name+"\n"), mode); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := relayrelease.Build(root, version, revision, relayrelease.CurrentTarget()); err != nil {
		t.Fatal(err)
	}
}

func archiveWorkerTestRelease(t *testing.T, root string) []byte {
	t.Helper()
	var output bytes.Buffer
	compressed := gzip.NewWriter(&output)
	writer := tar.NewWriter(compressed)
	err := filepath.Walk(root, func(filename string, info os.FileInfo, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if filename == root {
			return nil
		}
		name, err := filepath.Rel(root, filename)
		if err != nil {
			return err
		}
		header, err := tar.FileInfoHeader(info, "")
		if err != nil {
			return err
		}
		header.Name = filepath.ToSlash(name)
		if err := writer.WriteHeader(header); err != nil {
			return err
		}
		if info.Mode().IsRegular() {
			file, err := os.Open(filename)
			if err != nil {
				return err
			}
			_, copyErr := io.Copy(writer, file)
			closeErr := file.Close()
			if copyErr != nil {
				return copyErr
			}
			return closeErr
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	if err := compressed.Close(); err != nil {
		t.Fatal(err)
	}
	return output.Bytes()
}
