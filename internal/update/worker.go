package update

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	relayrelease "github.com/0cv/herdr-mobile-relay/internal/release"
	"github.com/0cv/herdr-mobile-relay/internal/setuphelper"
)

const (
	updateStartupGrace  = 30 * time.Second
	updateWorkerTimeout = 15 * time.Minute
	processTermGrace    = 2 * time.Second
	processWaitDelay    = 4 * time.Second
	updateRepository    = "0cv/herdr-mobile-relay"
)

var ErrConcurrent = errors.New("another update is already running")

type Job struct {
	ReleaseRoot       string `json:"release_root"`
	HerdrBin          string `json:"herdr_bin"`
	TargetVersion     string `json:"target_version"`
	TargetRevision    string `json:"target_revision"`
	StatePath         string `json:"state_path"`
	HealthURL         string `json:"health_url"`
	DeployAppFirst    bool   `json:"deploy_app_first,omitempty"`
	ExpectedAppOrigin string `json:"expected_app_origin,omitempty"`
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
}
type stagedRelease struct {
	Root     string
	Manifest relayrelease.Manifest
}

type Worker struct {
	Prepare func(context.Context, Job) (stagedRelease, error)
	Deploy  func(context.Context, Job, stagedRelease) error
	Install func(context.Context, Job) error
	Verify  func(context.Context, string, relayrelease.Manifest) error
}

func Run(ctx context.Context, jobPath string) error {
	ctx, cancel := context.WithTimeout(ctx, updateWorkerTimeout)
	defer cancel()
	worker := Worker{}
	return worker.Run(ctx, jobPath)
}

func (w Worker) Run(ctx context.Context, jobPath string) error {
	job, err := loadJob(jobPath)
	if err != nil {
		return err
	}
	started := time.Now().UTC().Format(time.RFC3339)
	state := State{
		State:          "scheduled",
		TargetVersion:  job.TargetVersion,
		TargetRevision: job.TargetRevision,
		Target:         relayrelease.CurrentTarget(),
		StartedAt:      started,
		Mode:           "plugin",
		Eligible:       true,
	}
	failStartup := func(startupErr error) error {
		if job.StatePath == "" || !filepath.IsAbs(job.StatePath) {
			return startupErr
		}
		return fail(job.StatePath, state, startupErr)
	}
	if err := validateJob(job); err != nil {
		return failStartup(err)
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
	state.State = "preparing"
	if err := writeState(job.StatePath, state); err != nil {
		return fmt.Errorf("write preparing state: %w", err)
	}
	prepare := w.Prepare
	if prepare == nil {
		prepare = prepareTargetRelease
	}
	staged, prepareErr := prepare(ctx, job)
	if prepareErr != nil {
		return fail(job.StatePath, state, fmt.Errorf("prepare target release: %w", prepareErr))
	}
	defer os.RemoveAll(staged.Root)

	if job.DeployAppFirst {
		state.State = "deploying_app"
		if err := writeState(job.StatePath, state); err != nil {
			return fmt.Errorf("write app deployment state: %w", err)
		}
		deploy := w.Deploy
		if deploy == nil {
			deploy = deployStagedApp
		}
		if err := deploy(ctx, job, staged); err != nil {
			return fail(job.StatePath, state, fmt.Errorf("deploy target app before relay: %w", err))
		}
	}

	state.State = "installing"
	if err := writeState(job.StatePath, state); err != nil {
		return fmt.Errorf("write installing state: %w", err)
	}
	install := w.Install
	if install == nil {
		install = installPlugin
	}
	if err := install(ctx, job); err != nil {
		return fail(job.StatePath, state, err)
	}

	state.State = "restarting"
	if err := writeState(job.StatePath, state); err != nil {
		return err
	}
	verify := w.Verify
	if verify == nil {
		verify = verifyHealth
	}
	expected := staged.Manifest
	if err := verify(ctx, job.HealthURL, expected); err != nil {
		return fail(job.StatePath, state, fmt.Errorf("verify Herdr plugin update: %w", err))
	}

	state.State = "succeeded"
	state.CurrentVersion = job.TargetVersion
	state.CurrentRevision = job.TargetRevision
	state.CheckedAt = time.Now().Unix()
	state.FinishedAt = time.Now().UTC().Format(time.RFC3339)
	state.Error = ""
	if err := writeState(job.StatePath, state); err != nil {
		return err
	}
	_ = os.Remove(jobPath)
	return nil
}

func installPlugin(ctx context.Context, job Job) error {
	command := exec.Command(
		job.HerdrBin,
		"plugin",
		"install",
		updateRepository,
		"--ref",
		strings.ToLower(job.TargetRevision),
		"--yes",
	)
	command.Env = environmentWith("HERDR_MOBILE_RELAY_NO_AUTO_SETUP", "1")
	output, err := runCommandContext(ctx, command)
	if err != nil {
		return fmt.Errorf(
			"Herdr plugin install failed: %s: %s",
			err,
			compact(string(output), 500),
		)
	}
	return nil
}

func runCommandContext(ctx context.Context, command *exec.Cmd) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	command.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	command.WaitDelay = processWaitDelay
	stdout := &bytes.Buffer{}
	stderr := &bytes.Buffer{}
	command.Stdout = stdout
	command.Stderr = stderr
	if err := command.Start(); err != nil {
		return nil, err
	}

	waitCh := make(chan error, 1)
	go func() {
		waitCh <- command.Wait()
	}()
	select {
	case err := <-waitCh:
		return combinedCommandOutput(stdout, stderr), err
	case <-ctx.Done():
		if terminateProcessGroup(command.Process.Pid, waitCh) {
			return combinedCommandOutput(stdout, stderr), ctx.Err()
		}
		return nil, ctx.Err()
	}
}

func combinedCommandOutput(stdout, stderr *bytes.Buffer) []byte {
	output := make([]byte, 0, stdout.Len()+stderr.Len())
	output = append(output, stdout.Bytes()...)
	return append(output, stderr.Bytes()...)
}

func terminateProcessGroup(pgid int, waitCh <-chan error) bool {
	_ = syscall.Kill(-pgid, syscall.SIGTERM)
	timer := time.NewTimer(processTermGrace)
	defer timer.Stop()
	waitCompleted := false
	select {
	case <-waitCh:
		waitCompleted = true
		if !processGroupAlive(pgid) {
			return true
		}
		<-timer.C
	case <-timer.C:
	}
	_ = syscall.Kill(-pgid, syscall.SIGKILL)

	if !waitCompleted {
		select {
		case <-waitCh:
			waitCompleted = true
		case <-time.After(processWaitDelay):
		}
	}
	deadline := time.Now().Add(processTermGrace)
	for processGroupAlive(pgid) && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	return waitCompleted
}

func processGroupAlive(pgid int) bool {
	err := syscall.Kill(-pgid, 0)
	return err == nil || errors.Is(err, syscall.EPERM)
}

func environmentWith(key, value string) []string {
	prefix := key + "="
	environment := os.Environ()
	result := make([]string, 0, len(environment)+1)
	for _, item := range environment {
		if strings.HasPrefix(item, prefix) {
			continue
		}
		result = append(result, item)
	}
	return append(result, prefix+value)
}

func validateJob(job Job) error {
	if job.ReleaseRoot == "" || !filepath.IsAbs(job.ReleaseRoot) {
		return errors.New("release_root must be absolute")
	}
	if job.HerdrBin == "" || !filepath.IsAbs(job.HerdrBin) {
		return errors.New("herdr_bin must be absolute")
	}
	info, err := os.Stat(job.HerdrBin)
	if err != nil || info.IsDir() || info.Mode()&0o111 == 0 {
		return errors.New("herdr_bin must be an executable file")
	}
	if !semverPattern.MatchString(job.TargetVersion) {
		return errors.New("target_version must be semantic versioned")
	}
	if !validRevision(job.TargetRevision) {
		return errors.New("target_revision must be an exact commit")
	}
	if job.StatePath == "" || !filepath.IsAbs(job.StatePath) {
		return errors.New("state_path must be absolute")
	}
	if job.DeployAppFirst {
		origin, originErr := setuphelper.NormalizeOrigin(job.ExpectedAppOrigin, false)
		if originErr != nil || origin != job.ExpectedAppOrigin {
			return errors.New("expected_app_origin must be a canonical HTTPS origin")
		}
	} else if job.ExpectedAppOrigin != "" {
		return errors.New("expected_app_origin requires an app-first update")
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
	return job, nil
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
		if err := makeReleaseDirectoriesWritable(candidate); err != nil {
			return err
		}
		if err := os.RemoveAll(candidate); err != nil {
			return err
		}
	}
	return nil
}

func makeReleaseDirectoriesWritable(root string) error {
	return filepath.WalkDir(root, func(filename string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if !entry.IsDir() {
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if err := os.Chmod(filename, info.Mode().Perm()|0o700); err != nil {
			return fmt.Errorf("make release directory removable: %w", err)
		}
		return nil
	})
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
				strings.EqualFold(health.Revision, manifest.Revision) &&
				(manifest.WebHash == "" || strings.EqualFold(health.BundleHash, manifest.WebHash)) {
				return nil
			}
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-deadline.C:
			return fmt.Errorf(
				"relay did not report release %s (%s)",
				manifest.Version,
				manifest.Revision,
			)
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

func fail(filename string, state State, err error) error {
	state.State = "failed"
	state.Error = safeError(err)
	state.FinishedAt = time.Now().UTC().Format(time.RFC3339)
	_ = writeState(filename, state)
	return err
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
