package update

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/0cv/herdr-mobile-relay/internal/appdeploy"
	"github.com/0cv/herdr-mobile-relay/internal/fetch"
	relayrelease "github.com/0cv/herdr-mobile-relay/internal/release"
)

const (
	canonicalReleaseAssets = "https://github.com/0cv/herdr-mobile-relay/releases/download"
	maxChecksumBytes       = 1 * 1024 * 1024
	maxArchiveBytes        = 128 * 1024 * 1024
	appReloadSignalGrace   = 2 * time.Second
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
	archiveFile, err := os.OpenFile(archivePath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return stagedRelease{}, fmt.Errorf("download target release: %w", err)
	}
	actualChecksum, err := fetch.DownloadFile(ctx, client, base+"/"+url.PathEscape(archiveName), archiveFile, maxArchiveBytes)
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
	if err := fetch.ExtractTarGz(archivePath, releaseRoot, false); err != nil {
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
	response, err := fetch.Get(ctx, client, endpoint)
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
