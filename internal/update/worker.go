package update

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"time"

	relayrelease "github.com/0cv/herdr-mobile-relay/internal/release"
)

const (
	maxArchiveBytes    = 128 * 1024 * 1024
	maxExtractedBytes  = 256 * 1024 * 1024
	maxArchiveFiles    = 10000
	updateStartupGrace = 30 * time.Second
)

var ErrConcurrent = errors.New("another update is already running")

type Job struct {
	ReleaseRoot    string `json:"release_root"`
	DownloadURL    string `json:"download_url"`
	ChecksumURL    string `json:"checksum_url"`
	ArchiveName    string `json:"archive_name"`
	TargetVersion  string `json:"target_version"`
	TargetRevision string `json:"target_revision"`
	Target         string `json:"target"`
	StatePath      string `json:"state_path"`
	ServiceName    string `json:"service_name"`
	HealthURL      string `json:"health_url"`
	TokenFile      string `json:"token_file,omitempty"`
}

type State struct {
	State             string `json:"state"`
	CurrentVersion    string `json:"current_version,omitempty"`
	CurrentRevision   string `json:"current_revision,omitempty"`
	AvailableVersion  string `json:"available_version,omitempty"`
	AvailableRevision string `json:"available_revision,omitempty"`
	UpstreamVersion   string `json:"upstream_version,omitempty"`
	UpstreamRevision  string `json:"upstream_revision,omitempty"`
	TargetVersion     string `json:"target_version,omitempty"`
	TargetRevision    string `json:"target_revision,omitempty"`
	Target            string `json:"target,omitempty"`
	CheckedAt         int64  `json:"checked_at,omitempty"`
	StartedAt         string `json:"started_at,omitempty"`
	FinishedAt        string `json:"finished_at,omitempty"`
	Mode              string `json:"mode,omitempty"`
	Eligible          bool   `json:"eligible"`
	CanInstall        bool   `json:"can_install"`
	Reason            string `json:"reason,omitempty"`
	Error             string `json:"error,omitempty"`
	RollbackTarget    string `json:"rollback_target,omitempty"`
	RollbackVersion   string `json:"rollback_version,omitempty"`
	RollbackRevision  string `json:"rollback_revision,omitempty"`
	RollbackWebHash   string `json:"rollback_web_hash,omitempty"`
}

type Worker struct {
	Client    *http.Client
	Restart   func(context.Context, string) error
	Verify    func(context.Context, string, relayrelease.Manifest) error
	tokenFile string
}

func Run(ctx context.Context, jobPath string) error {
	worker := Worker{}
	return worker.Run(ctx, jobPath)
}

func (w Worker) Run(ctx context.Context, jobPath string) error {
	job, err := loadJob(jobPath)
	if err != nil {
		return err
	}
	started := time.Now().UTC().Format(time.RFC3339)
	startupState := State{
		State:          "scheduled",
		TargetVersion:  job.TargetVersion,
		TargetRevision: job.TargetRevision,
		Target:         job.Target,
		StartedAt:      started,
		Mode:           "release",
		Eligible:       true,
		CanInstall:     false,
	}
	failStartup := func(startupErr error) error {
		if job.StatePath == "" || !filepath.IsAbs(job.StatePath) {
			return startupErr
		}
		return fail(job.StatePath, startupState, startupErr)
	}
	if job.ReleaseRoot == "" || !filepath.IsAbs(job.ReleaseRoot) {
		return failStartup(errors.New("release_root must be absolute"))
	}
	if err := os.MkdirAll(job.ReleaseRoot, 0o700); err != nil {
		return failStartup(err)
	}
	lock, err := acquireLock(filepath.Join(job.ReleaseRoot, "update.lock"))
	if err != nil {
		if errors.Is(err, ErrConcurrent) {
			return err
		}
		return failStartup(err)
	}
	defer func() {
		_ = syscall.Flock(int(lock.Fd()), syscall.LOCK_UN)
		_ = lock.Close()
	}()
	if err := validateJob(job); err != nil {
		return failStartup(err)
	}
	w.tokenFile = job.TokenFile
	if w.Client == nil {
		w.Client = &http.Client{Timeout: 60 * time.Second}
	}
	if w.Restart == nil {
		w.Restart = restartService
	}
	if w.Verify == nil {
		w.Verify = verifyHealth
	}
	if persisted, readErr := readState(job.StatePath); readErr == nil &&
		persisted.State == "recovering" {
		return recoverRollback(ctx, w, job, persisted, jobPath)
	}

	if err := os.MkdirAll(filepath.Join(job.ReleaseRoot, "releases"), 0o700); err != nil {
		return failStartup(err)
	}

	state := State{
		State:          "installing",
		TargetVersion:  job.TargetVersion,
		TargetRevision: job.TargetRevision,
		Target:         job.Target,
		StartedAt:      started,
		Mode:           "release",
		Eligible:       true,
		CanInstall:     false,
	}
	if err := writeState(job.StatePath, state); err != nil {
		return fmt.Errorf("write installing state: %w", err)
	}

	previousTarget, previousManifest, err := currentRelease(job.ReleaseRoot)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return fail(job.StatePath, state, fmt.Errorf("inspect current release: %w", err))
	}
	if previousManifest.Version != "" {
		state.CurrentVersion = previousManifest.Version
		state.CurrentRevision = previousManifest.Revision
		state.RollbackTarget = previousTarget
		state.RollbackVersion = previousManifest.Version
		state.RollbackRevision = previousManifest.Revision
		state.RollbackWebHash = previousManifest.WebHash
	}

	workDir, err := os.MkdirTemp(filepath.Join(job.ReleaseRoot, "releases"), ".update-")
	if err != nil {
		return fail(job.StatePath, state, err)
	}
	defer os.RemoveAll(workDir)

	archivePath := filepath.Join(workDir, job.ArchiveName)
	checksumPath := filepath.Join(workDir, "checksums.txt")
	if err := w.download(ctx, job.DownloadURL, archivePath, maxArchiveBytes); err != nil {
		return fail(job.StatePath, state, fmt.Errorf("download release archive: %w", err))
	}
	if err := w.download(ctx, job.ChecksumURL, checksumPath, 4*1024*1024); err != nil {
		return fail(job.StatePath, state, fmt.Errorf("download release checksums: %w", err))
	}
	if err := verifyChecksum(archivePath, checksumPath, job.ArchiveName); err != nil {
		return fail(job.StatePath, state, err)
	}

	extracted := filepath.Join(workDir, "extracted")
	if err := os.Mkdir(extracted, 0o700); err != nil {
		return fail(job.StatePath, state, err)
	}
	if err := extractArchive(archivePath, extracted); err != nil {
		return fail(job.StatePath, state, fmt.Errorf("extract release: %w", err))
	}
	releaseDir, err := findReleaseRoot(extracted)
	if err != nil {
		return fail(job.StatePath, state, err)
	}
	manifest, err := relayrelease.Verify(releaseDir, job.Target)
	if err != nil {
		return fail(job.StatePath, state, fmt.Errorf("verify release: %w", err))
	}
	if manifest.Version != job.TargetVersion || manifest.Revision != job.TargetRevision {
		return fail(job.StatePath, state, fmt.Errorf(
			"release identity mismatch: expected %s (%s), got %s (%s)",
			job.TargetVersion, job.TargetRevision, manifest.Version, manifest.Revision,
		))
	}

	finalName := safeReleaseName(manifest.Version, manifest.Revision, manifest.Target)
	finalDir := filepath.Join(job.ReleaseRoot, "releases", finalName)
	if _, err := os.Stat(finalDir); err == nil {
		existing, verifyErr := relayrelease.Verify(finalDir, job.Target)
		if verifyErr != nil || existing.Version != manifest.Version || existing.Revision != manifest.Revision {
			return fail(job.StatePath, state, fmt.Errorf("existing target release is invalid"))
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return fail(job.StatePath, state, err)
	} else if err := os.Rename(releaseDir, finalDir); err != nil {
		return fail(job.StatePath, state, fmt.Errorf("install release directory: %w", err))
	}

	if err := writeState(job.StatePath, state); err != nil {
		return fmt.Errorf("persist rollback generation: %w", err)
	}
	if err := Activate(job.ReleaseRoot, finalDir); err != nil {
		return fail(job.StatePath, state, fmt.Errorf("activate release: %w", err))
	}
	state.State = "restarting"
	if err := writeState(job.StatePath, state); err != nil {
		return rollback(ctx, w, job, state, previousTarget, previousManifest, err)
	}
	if err := w.Restart(ctx, job.ServiceName); err != nil {
		return rollback(ctx, w, job, state, previousTarget, previousManifest, fmt.Errorf("restart service: %w", err))
	}
	if err := w.Verify(ctx, job.HealthURL, manifest); err != nil {
		return rollback(ctx, w, job, state, previousTarget, previousManifest, fmt.Errorf("verify updated relay: %w", err))
	}
	if err := PruneOldReleases(job.ReleaseRoot, finalDir, previousTarget); err != nil {
		return rollback(ctx, w, job, state, previousTarget, previousManifest, fmt.Errorf("prune old releases: %w", err))
	}

	state.State = "succeeded"
	state.CurrentVersion = manifest.Version
	state.CurrentRevision = manifest.Revision
	state.CheckedAt = time.Now().Unix()
	state.FinishedAt = time.Now().UTC().Format(time.RFC3339)
	state.Error = ""
	if err := writeState(job.StatePath, state); err != nil {
		return err
	}
	_ = os.Remove(jobPath)
	return nil
}

func validateJob(job Job) error {
	if job.ReleaseRoot == "" || !filepath.IsAbs(job.ReleaseRoot) {
		return errors.New("release_root must be absolute")
	}
	if job.DownloadURL == "" || job.ChecksumURL == "" {
		return errors.New("archive and checksum URLs are required")
	}
	if job.ArchiveName == "" || filepath.Base(job.ArchiveName) != job.ArchiveName || strings.Contains(job.ArchiveName, `\`) {
		return errors.New("archive_name must be a plain filename")
	}
	if job.TargetVersion == "" || job.TargetRevision == "" {
		return errors.New("target version and revision are required")
	}
	if job.Target == "" {
		job.Target = relayrelease.CurrentTarget()
	}
	if job.Target != relayrelease.CurrentTarget() {
		return fmt.Errorf("job target %q does not match this worker %q", job.Target, relayrelease.CurrentTarget())
	}
	if job.StatePath == "" || !filepath.IsAbs(job.StatePath) {
		return errors.New("state_path must be absolute")
	}
	if job.ServiceName == "" || job.HealthURL == "" {
		return errors.New("service name and health URL are required")
	}
	health, err := url.Parse(job.HealthURL)
	if err != nil || health.Scheme != "http" || !isLoopback(health.Hostname()) {
		return errors.New("health_url must use HTTP on loopback")
	}
	return nil
}

func loadJob(filename string) (Job, error) {
	data, err := os.ReadFile(filename)
	if err != nil {
		return Job{}, fmt.Errorf("read update job: %w", err)
	}
	var job Job
	if err := json.Unmarshal(data, &job); err != nil {
		return Job{}, fmt.Errorf("parse update job: %w", err)
	}
	if job.Target == "" {
		job.Target = relayrelease.CurrentTarget()
	}
	return job, nil
}

func readState(filename string) (State, error) {
	data, err := os.ReadFile(filename)
	if err != nil {
		return State{}, err
	}
	var state State
	if err := json.Unmarshal(data, &state); err != nil {
		return State{}, err
	}
	return state, nil
}

func (w Worker) download(ctx context.Context, source, destination string, limit int64) error {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, source, nil)
	if err != nil {
		return err
	}
	request.Header.Set("Accept", "application/octet-stream")
	if token := w.token(); token != "" {
		request.Header.Set("Authorization", "token "+token)
	}
	response, err := w.Client.Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return fmt.Errorf("HTTP %d", response.StatusCode)
	}
	file, err := os.OpenFile(destination, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	reader := io.LimitReader(response.Body, limit+1)
	written, copyErr := io.Copy(file, reader)
	syncErr := file.Sync()
	closeErr := file.Close()
	if copyErr != nil {
		return copyErr
	}
	if written > limit {
		return errors.New("download exceeds size limit")
	}
	if syncErr != nil {
		return syncErr
	}
	return closeErr
}

func (w Worker) token() string {
	if w.tokenFile != "" {
		data, err := os.ReadFile(w.tokenFile)
		if err == nil {
			if token := strings.TrimSpace(string(data)); token != "" {
				return token
			}
		}
	}
	return ""
}

func PruneOldReleases(releaseRoot string, keep ...string) error {
	if !filepath.IsAbs(releaseRoot) || filepath.Clean(releaseRoot) == string(filepath.Separator) {
		return errors.New("release root must be a non-root absolute path")
	}
	releasesDir := filepath.Join(releaseRoot, "releases")
	entries, err := os.ReadDir(releasesDir)
	if err != nil {
		return err
	}
	kept := make(map[string]bool, len(keep))
	for _, item := range keep {
		if item == "" {
			continue
		}
		absolute, absErr := filepath.Abs(item)
		if absErr == nil {
			kept[filepath.Clean(absolute)] = true
		}
	}
	for _, entry := range entries {
		if !entry.IsDir() || strings.HasPrefix(entry.Name(), ".update-") {
			continue
		}
		candidate := filepath.Join(releasesDir, entry.Name())
		absolute, absErr := filepath.Abs(candidate)
		if absErr != nil || kept[filepath.Clean(absolute)] {
			continue
		}
		info, statErr := entry.Info()
		if statErr != nil || info.Mode()&os.ModeSymlink != 0 {
			continue
		}
		if _, verifyErr := relayrelease.Verify(candidate, relayrelease.CurrentTarget()); verifyErr != nil {
			continue
		}
		if err := os.RemoveAll(candidate); err != nil {
			return err
		}
	}
	return nil
}

func verifyChecksum(archivePath, checksumPath, expectedName string) error {
	data, err := os.ReadFile(checksumPath)
	if err != nil {
		return err
	}
	var expected string
	matches := 0
	for _, line := range strings.Split(string(data), "\n") {
		fields := strings.Fields(line)
		if len(fields) != 2 {
			continue
		}
		name := strings.TrimPrefix(fields[1], "*")
		if name == expectedName {
			expected = strings.ToLower(fields[0])
			matches++
		}
	}
	if matches != 1 || len(expected) != sha256.Size*2 {
		return fmt.Errorf("checksums.txt must contain exactly one entry for %s", expectedName)
	}
	if _, err := hex.DecodeString(expected); err != nil {
		return fmt.Errorf("invalid checksum for %s", expectedName)
	}
	file, err := os.Open(archivePath)
	if err != nil {
		return err
	}
	defer file.Close()
	hasher := sha256.New()
	if _, err := io.Copy(hasher, file); err != nil {
		return err
	}
	if actual := hex.EncodeToString(hasher.Sum(nil)); actual != expected {
		return fmt.Errorf("checksum mismatch for %s", expectedName)
	}
	return nil
}

func extractArchive(archivePath, destination string) error {
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
	var total int64
	files := 0
	for {
		header, err := reader.Next()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
		files++
		if files > maxArchiveFiles {
			return errors.New("release archive has too many entries")
		}
		if path.Clean(header.Name) == "." && header.Typeflag == tar.TypeDir {
			continue
		}
		name, err := cleanArchivePath(header.Name)
		if err != nil {
			return err
		}
		target := filepath.Join(destination, filepath.FromSlash(name))
		switch header.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(target, 0o755); err != nil {
				return err
			}
		case tar.TypeReg, tar.TypeRegA:
			total += header.Size
			if header.Size < 0 || total > maxExtractedBytes {
				return errors.New("release archive exceeds extracted size limit")
			}
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				return err
			}
			mode := os.FileMode(header.Mode) & 0o755
			if mode&0o111 == 0 {
				mode = 0o644
			}
			output, err := os.OpenFile(target, os.O_CREATE|os.O_EXCL|os.O_WRONLY, mode)
			if err != nil {
				return err
			}
			written, copyErr := io.CopyN(output, reader, header.Size)
			closeErr := output.Close()
			if copyErr != nil || written != header.Size {
				return errors.New("release archive entry was truncated")
			}
			if closeErr != nil {
				return closeErr
			}
		default:
			return fmt.Errorf("release archive contains unsupported entry %s", name)
		}
	}
}

func cleanArchivePath(name string) (string, error) {
	if name == "" || strings.ContainsRune(name, '\\') || path.IsAbs(name) {
		return "", errors.New("release archive contains an invalid path")
	}
	clean := path.Clean(name)
	if clean == "." || clean == ".." || strings.HasPrefix(clean, "../") {
		return "", errors.New("release archive path escapes extraction root")
	}
	return clean, nil
}

func findReleaseRoot(extracted string) (string, error) {
	if _, err := os.Stat(filepath.Join(extracted, relayrelease.ManifestName)); err == nil {
		return extracted, nil
	}
	entries, err := os.ReadDir(extracted)
	if err != nil {
		return "", err
	}
	if len(entries) == 1 && entries[0].IsDir() {
		candidate := filepath.Join(extracted, entries[0].Name())
		if _, err := os.Stat(filepath.Join(candidate, relayrelease.ManifestName)); err == nil {
			return candidate, nil
		}
	}
	return "", errors.New("release archive does not contain a root release-manifest.json")
}

func Activate(releaseRoot, releaseDir string) error {
	relative, err := filepath.Rel(releaseRoot, releaseDir)
	if err != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		return errors.New("release directory is outside release root")
	}
	temp := filepath.Join(releaseRoot, fmt.Sprintf(".current-%d", os.Getpid()))
	_ = os.Remove(temp)
	if err := os.Symlink(relative, temp); err != nil {
		return err
	}
	defer os.Remove(temp)
	return os.Rename(temp, filepath.Join(releaseRoot, "current"))
}

func currentRelease(releaseRoot string) (string, relayrelease.Manifest, error) {
	current := filepath.Join(releaseRoot, "current")
	target, err := os.Readlink(current)
	if err != nil {
		return "", relayrelease.Manifest{}, err
	}
	if !filepath.IsAbs(target) {
		target = filepath.Join(releaseRoot, target)
	}
	target, err = filepath.Abs(target)
	if err != nil {
		return "", relayrelease.Manifest{}, err
	}
	releases := filepath.Join(releaseRoot, "releases") + string(filepath.Separator)
	if !strings.HasPrefix(target+string(filepath.Separator), releases) {
		return "", relayrelease.Manifest{}, errors.New("current release points outside releases directory")
	}
	manifest, err := relayrelease.Verify(target, relayrelease.CurrentTarget())
	return target, manifest, err
}

func rollback(
	ctx context.Context,
	worker Worker,
	job Job,
	state State,
	previousTarget string,
	previous relayrelease.Manifest,
	updateErr error,
) error {
	if previousTarget == "" {
		state.State = "failed"
		state.Error = safeError(updateErr)
		state.FinishedAt = time.Now().UTC().Format(time.RFC3339)
		_ = writeState(job.StatePath, state)
		return updateErr
	}
	rollbackErr := Activate(job.ReleaseRoot, previousTarget)
	if rollbackErr == nil {
		rollbackErr = worker.Restart(ctx, job.ServiceName)
	}
	if rollbackErr == nil {
		rollbackErr = worker.Verify(ctx, job.HealthURL, previous)
	}
	state.FinishedAt = time.Now().UTC().Format(time.RFC3339)
	state.CurrentVersion = previous.Version
	state.CurrentRevision = previous.Revision
	if rollbackErr != nil {
		state.State = "failed"
		state.Error = safeError(fmt.Errorf("update failed: %v; rollback failed: %v", updateErr, rollbackErr))
	} else {
		state.State = "rolled_back"
		state.Error = safeError(updateErr)
	}
	_ = writeState(job.StatePath, state)
	if rollbackErr != nil {
		return fmt.Errorf("update failed and rollback failed: %v: %w", rollbackErr, updateErr)
	}
	return fmt.Errorf("update failed and was rolled back: %w", updateErr)
}

func recoverRollback(
	ctx context.Context,
	worker Worker,
	job Job,
	state State,
	jobPath string,
) error {
	if state.RollbackTarget == "" || state.RollbackVersion == "" ||
		state.RollbackRevision == "" {
		return fail(job.StatePath, state, errors.New("orphaned update has no verified rollback generation"))
	}
	rollbackManifest, err := relayrelease.Verify(state.RollbackTarget, job.Target)
	if err != nil {
		return fail(job.StatePath, state, fmt.Errorf("verify rollback release: %w", err))
	}
	if rollbackManifest.Version != state.RollbackVersion ||
		!strings.EqualFold(rollbackManifest.Revision, state.RollbackRevision) ||
		rollbackManifest.WebHash != state.RollbackWebHash {
		return fail(job.StatePath, state, errors.New("rollback release identity changed"))
	}
	if err := Activate(job.ReleaseRoot, state.RollbackTarget); err != nil {
		return fail(job.StatePath, state, fmt.Errorf("reactivate rollback release: %w", err))
	}
	if err := worker.Restart(ctx, job.ServiceName); err != nil {
		return fail(job.StatePath, state, fmt.Errorf("restart rollback service: %w", err))
	}
	if err := worker.Verify(ctx, job.HealthURL, rollbackManifest); err != nil {
		return fail(job.StatePath, state, fmt.Errorf("verify rollback service: %w", err))
	}
	state.State = "rolled_back"
	state.CurrentVersion = rollbackManifest.Version
	state.CurrentRevision = rollbackManifest.Revision
	state.FinishedAt = time.Now().UTC().Format(time.RFC3339)
	state.Error = "Update worker stopped before verification; the previous release was restored"
	if err := writeState(job.StatePath, state); err != nil {
		return err
	}
	_ = os.Remove(jobPath)
	return nil
}

func restartService(ctx context.Context, serviceName string) error {
	var command *exec.Cmd
	if runtime.GOOS == "darwin" {
		command = exec.CommandContext(ctx, "launchctl", "kickstart", "-k", fmt.Sprintf("gui/%d/%s", os.Getuid(), serviceName))
	} else {
		command = exec.CommandContext(ctx, "systemctl", "--user", "restart", serviceName)
	}
	output, err := command.CombinedOutput()
	if err != nil {
		return fmt.Errorf("%s: %s", err, compact(string(output), 300))
	}
	return nil
}

func verifyHealth(ctx context.Context, healthURL string, manifest relayrelease.Manifest) error {
	client := &http.Client{Timeout: 2 * time.Second}
	ticker := time.NewTicker(500 * time.Millisecond)
	defer ticker.Stop()
	deadline := time.NewTimer(15 * time.Second)
	defer deadline.Stop()
	for {
		request, err := http.NewRequestWithContext(ctx, http.MethodGet, healthURL, nil)
		if err != nil {
			return err
		}
		response, err := client.Do(request)
		if err == nil {
			var health struct {
				Status         string `json:"status"`
				ReleaseVersion string `json:"release_version"`
				Revision       string `json:"revision"`
				BundleHash     string `json:"bundle_hash"`
			}
			decodeErr := json.NewDecoder(io.LimitReader(response.Body, 64*1024)).Decode(&health)
			response.Body.Close()
			if decodeErr == nil && response.StatusCode == http.StatusOK &&
				health.Status == "ok" &&
				health.ReleaseVersion == manifest.Version &&
				health.Revision == manifest.Revision &&
				(manifest.WebHash == "" || health.BundleHash == manifest.WebHash) {
				return nil
			}
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-deadline.C:
			return fmt.Errorf("relay did not report release %s (%s) and web hash %s", manifest.Version, manifest.Revision, manifest.WebHash)
		case <-ticker.C:
		}
	}
}

func acquireLock(filename string) (*os.File, error) {
	file, err := os.OpenFile(filename, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(file.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		file.Close()
		return nil, ErrConcurrent
	}
	_ = file.Truncate(0)
	_, _ = fmt.Fprintf(file, "%d\n", os.Getpid())
	_ = file.Sync()
	return file, nil
}

func writeState(filename string, state State) error {
	data, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	directory := filepath.Dir(filename)
	if err := os.MkdirAll(directory, 0o700); err != nil {
		return err
	}
	temp, err := os.CreateTemp(directory, ".update-state.")
	if err != nil {
		return err
	}
	tempName := temp.Name()
	defer os.Remove(tempName)
	if err := temp.Chmod(0o600); err != nil {
		temp.Close()
		return err
	}
	if _, err := temp.Write(data); err != nil {
		temp.Close()
		return err
	}
	if err := temp.Sync(); err != nil {
		temp.Close()
		return err
	}
	if err := temp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tempName, filename); err != nil {
		return err
	}
	dir, err := os.Open(directory)
	if err == nil {
		err = dir.Sync()
		_ = dir.Close()
	}
	return err
}

func fail(filename string, state State, err error) error {
	state.State = "failed"
	state.Error = safeError(err)
	state.FinishedAt = time.Now().UTC().Format(time.RFC3339)
	_ = writeState(filename, state)
	return err
}

func safeReleaseName(version, revision, target string) string {
	replacer := strings.NewReplacer("/", "-", `\`, "-", " ", "-", ":", "-")
	return replacer.Replace(version + "-" + revision + "-" + target)
}

func safeError(err error) string {
	return compact(err.Error(), 500)
}

func compact(value string, limit int) string {
	value = strings.Join(strings.Fields(value), " ")
	if len(value) > limit {
		return value[:limit]
	}
	return value
}

func isLoopback(host string) bool {
	return host == "localhost" || host == "127.0.0.1" || host == "::1"
}
