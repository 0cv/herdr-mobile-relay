package update

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"hash"
	"io"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"

	"github.com/0cv/herdr-mobile-relay/internal/appdeploy"
	relayrelease "github.com/0cv/herdr-mobile-relay/internal/release"
)

const (
	canonicalReleaseAssets = "https://github.com/0cv/herdr-mobile-relay/releases/download"
	maxChecksumBytes       = 1 * 1024 * 1024
	maxArchiveBytes        = 128 * 1024 * 1024
	maxExtractedBytes      = 256 * 1024 * 1024
	appReloadSignalGrace   = 2 * time.Second
	maxArchiveEntries      = 4096
)

func prepareTargetRelease(ctx context.Context, job Job) (stagedRelease, error) {
	client := &http.Client{Timeout: 2 * time.Minute}
	return prepareTargetReleaseFrom(ctx, job, canonicalReleaseAssets, client)
}

func prepareTargetReleaseFrom(
	ctx context.Context,
	job Job,
	assetBase string,
	client *http.Client,
) (stagedRelease, error) {
	current, err := relayrelease.Load(filepath.Join(job.ReleaseRoot, "current"))
	if err != nil {
		return stagedRelease{}, fmt.Errorf("load current release: %w", err)
	}

	target := strings.ReplaceAll(relayrelease.CurrentTarget(), "/", "_")
	archiveName := fmt.Sprintf("herdr-mobile-relay_%s_%s.tar.gz", job.TargetVersion, target)
	base := strings.TrimRight(assetBase, "/") + "/v" + url.PathEscape(job.TargetVersion)
	checksums, err := downloadBytes(ctx, client, base+"/checksums.txt", maxChecksumBytes)
	if err != nil {
		return stagedRelease{}, fmt.Errorf("download release checksums: %w", err)
	}
	expectedChecksum, err := checksumForArchive(checksums, archiveName)
	if err != nil {
		return stagedRelease{}, err
	}

	runtimeDir := filepath.Dir(job.StatePath)
	stageRoot, err := os.MkdirTemp(runtimeDir, ".update-stage-")
	if err != nil {
		return stagedRelease{}, fmt.Errorf("create update stage: %w", err)
	}
	prepared := stagedRelease{Root: stageRoot}
	keep := false
	defer func() {
		if !keep {
			_ = os.RemoveAll(stageRoot)
		}
	}()

	archivePath := filepath.Join(stageRoot, archiveName)
	actualChecksum, err := downloadFile(ctx, client, base+"/"+url.PathEscape(archiveName), archivePath, maxArchiveBytes)
	if err != nil {
		return stagedRelease{}, fmt.Errorf("download target release: %w", err)
	}
	if !strings.EqualFold(actualChecksum, expectedChecksum) {
		return stagedRelease{}, errors.New("target release archive checksum mismatch")
	}

	releaseRoot := filepath.Join(stageRoot, "release")
	if err := os.Mkdir(releaseRoot, 0o700); err != nil {
		return stagedRelease{}, err
	}
	if err := extractReleaseArchive(archivePath, releaseRoot); err != nil {
		return stagedRelease{}, fmt.Errorf("extract target release: %w", err)
	}
	if err := os.Remove(archivePath); err != nil {
		return stagedRelease{}, fmt.Errorf("remove staged archive: %w", err)
	}

	manifest, err := relayrelease.Verify(releaseRoot, relayrelease.CurrentTarget())
	if err != nil {
		return stagedRelease{}, fmt.Errorf("verify target release: %w", err)
	}
	if manifest.Version != job.TargetVersion || !strings.EqualFold(manifest.Revision, job.TargetRevision) {
		return stagedRelease{}, errors.New("target release identity does not match the advertised update")
	}
	if err := relayrelease.ValidateUpgradeCompatibility(current, manifest); err != nil {
		return stagedRelease{}, fmt.Errorf("transport compatibility: %w", err)
	}

	prepared.Manifest = manifest
	keep = true
	return prepared, nil
}

func deployStagedApp(ctx context.Context, job Job, staged stagedRelease) error {
	err := appdeploy.RunConfiguredAtOrigin(
		ctx,
		filepath.Dir(job.StatePath),
		filepath.Join(staged.Root, "release", "web"),
		staged.Manifest.Version,
		staged.Manifest.Revision,
		job.ExpectedAppOrigin,
	)
	if err != nil {
		return err
	}
	timer := time.NewTimer(appReloadSignalGrace)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func downloadBytes(ctx context.Context, client *http.Client, endpoint string, maximum int64) ([]byte, error) {
	response, err := get(ctx, client, endpoint)
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()
	data, err := io.ReadAll(io.LimitReader(response.Body, maximum+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > maximum {
		return nil, errors.New("response exceeds size limit")
	}
	return data, nil
}

func downloadFile(
	ctx context.Context,
	client *http.Client,
	endpoint, filename string,
	maximum int64,
) (string, error) {
	response, err := get(ctx, client, endpoint)
	if err != nil {
		return "", err
	}
	defer response.Body.Close()
	if response.ContentLength > maximum {
		return "", errors.New("response exceeds size limit")
	}

	file, err := os.OpenFile(filename, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return "", err
	}
	checksum := sha256.New()
	written, copyErr := io.Copy(io.MultiWriter(file, checksum), io.LimitReader(response.Body, maximum+1))
	closeErr := file.Close()
	if copyErr != nil {
		return "", copyErr
	}
	if closeErr != nil {
		return "", closeErr
	}
	if written > maximum {
		return "", errors.New("response exceeds size limit")
	}
	return checksumHex(checksum), nil
}

func get(ctx context.Context, client *http.Client, endpoint string) (*http.Response, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, err
	}
	request.Header.Set("User-Agent", "herdr-mobile-relay-update-stage")
	response, err := client.Do(request)
	if err != nil {
		return nil, err
	}
	if response.StatusCode != http.StatusOK {
		response.Body.Close()
		return nil, fmt.Errorf("HTTP %d", response.StatusCode)
	}
	return response, nil
}

func checksumForArchive(data []byte, archiveName string) (string, error) {
	for _, line := range strings.Split(string(data), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 2 || strings.TrimPrefix(fields[1], "*") != archiveName {
			continue
		}
		decoded, err := hex.DecodeString(fields[0])
		if err != nil || len(decoded) != sha256.Size {
			return "", errors.New("release archive checksum is invalid")
		}
		return strings.ToLower(fields[0]), nil
	}
	return "", fmt.Errorf("checksums.txt does not list %s", archiveName)
}

func checksumHex(value hash.Hash) string {
	return hex.EncodeToString(value.Sum(nil))
}

func extractReleaseArchive(archivePath, destination string) error {
	file, err := os.Open(archivePath)
	if err != nil {
		return err
	}
	defer file.Close()
	compressed, err := gzip.NewReader(file)
	if err != nil {
		return err
	}
	defer compressed.Close()

	reader := tar.NewReader(compressed)
	var extracted int64
	entries := 0
	for {
		header, err := reader.Next()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
		entries++
		if entries > maxArchiveEntries {
			return errors.New("release archive has too many entries")
		}
		if header.Size < 0 || header.Size > maxExtractedBytes-extracted {
			return errors.New("release archive exceeds extracted size limit")
		}
		extracted += header.Size

		name, err := cleanArchivePath(header.Name)
		if err != nil {
			return err
		}
		if name == "" {
			if header.Typeflag == tar.TypeDir {
				continue
			}
			return errors.New("release archive has an empty file path")
		}
		target := filepath.Join(destination, filepath.FromSlash(name))
		switch header.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(target, 0o755); err != nil {
				return err
			}
		case tar.TypeReg, tar.TypeRegA:
			if err := extractRegularFile(reader, header, target); err != nil {
				return err
			}
		default:
			return fmt.Errorf("release archive contains unsupported entry %q", header.Name)
		}
	}
}

func cleanArchivePath(name string) (string, error) {
	clean := path.Clean(name)
	if clean == "." {
		return "", nil
	}
	if path.IsAbs(clean) || clean == ".." || strings.HasPrefix(clean, "../") {
		return "", fmt.Errorf("release archive path %q escapes its destination", name)
	}
	return strings.TrimPrefix(clean, "./"), nil
}

func extractRegularFile(reader io.Reader, header *tar.Header, target string) error {
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		return err
	}
	mode := os.FileMode(0o644)
	if header.FileInfo().Mode()&0o111 != 0 {
		mode = 0o755
	}
	file, err := os.OpenFile(target, os.O_CREATE|os.O_EXCL|os.O_WRONLY, mode)
	if err != nil {
		return err
	}
	written, copyErr := io.CopyN(file, reader, header.Size)
	chmodErr := file.Chmod(mode)
	closeErr := file.Close()
	if copyErr != nil {
		return copyErr
	}
	if chmodErr != nil {
		return chmodErr
	}
	if closeErr != nil {
		return closeErr
	}
	if written != header.Size {
		return io.ErrUnexpectedEOF
	}
	return nil
}
