package update

import (
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
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"

	relayrelease "github.com/0cv/herdr-mobile-relay/internal/release"
)

const canonicalAPI = "https://api.github.com/repos/0cv/herdr-mobile-relay-dev"

var semverPattern = regexp.MustCompile(`^(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)$`)

type Manager struct {
	releaseRoot string
	runtimeDir  string
	version     string
	revision    string
	serviceName string
	healthURL   string
	apiBase     string
	client      *http.Client
	tokenFile   string

	mu       sync.Mutex
	state    State
	metadata releaseMetadata
	launch   func(context.Context, string, string) error
}

type releaseMetadata struct {
	Version     string
	Revision    string
	ArchiveName string
	ArchiveURL  string
	ChecksumURL string
	ReleaseTag  string
	PublishedAt string
}

type githubRelease struct {
	TagName     string `json:"tag_name"`
	Draft       bool   `json:"draft"`
	Prerelease  bool   `json:"prerelease"`
	PublishedAt string `json:"published_at"`
	Assets      []struct {
		Name string `json:"name"`
		URL  string `json:"url"`
	} `json:"assets"`
}

type gitObject struct {
	SHA    string `json:"sha"`
	Type   string `json:"type"`
	URL    string `json:"url"`
	Object *struct {
		SHA  string `json:"sha"`
		Type string `json:"type"`
		URL  string `json:"url"`
	} `json:"object,omitempty"`
}

func NewManager(releaseRoot, runtimeDir, version, revision, serviceName, healthURL string) *Manager {
	manager := &Manager{
		releaseRoot: releaseRoot,
		runtimeDir:  runtimeDir,
		version:     version,
		revision:    revision,
		serviceName: serviceName,
		healthURL:   healthURL,
		apiBase:     canonicalAPI,
		client: &http.Client{
			Timeout: 15 * time.Second,
		},
	}
	if token := os.Getenv("GH_TOKEN"); token != "" {
		tokenFile := filepath.Join(runtimeDir, "gh-token")
		if err := os.MkdirAll(runtimeDir, 0o700); err == nil {
			if err := os.WriteFile(tokenFile, []byte(token), 0o600); err == nil {
				manager.tokenFile = tokenFile
			}
		}
	}
	manager.launch = manager.launchWorker
	manager.state = manager.loadState()
	return manager
}

func (m *Manager) State() State {
	m.mu.Lock()
	defer m.mu.Unlock()
	state := m.loadState()
	if state.State != "" {
		m.state = state
	}
	return m.publicState(m.state)
}

func (m *Manager) Check(ctx context.Context) State {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.state.State = "checking"
	m.state.Error = ""
	m.state.CurrentVersion = m.version
	m.state.CurrentRevision = m.revision
	_ = writeState(m.statePath(), m.state)

	metadata, err := m.fetchRelease(ctx)
	if err != nil {
		m.state.State = "failed"
		m.state.CanInstall = false
		m.state.Eligible = false
		m.state.Error = safeError(err)
		m.state.CheckedAt = time.Now().Unix()
		_ = writeState(m.statePath(), m.state)
		return m.publicState(m.state)
	}
	m.metadata = metadata
	newer := NewerVersion(metadata.Version, m.version)
	eligible, mode, reason := m.eligibility()
	m.state = State{
		State:            "current",
		CurrentVersion:   m.version,
		CurrentRevision:  m.revision,
		UpstreamVersion:  metadata.Version,
		UpstreamRevision: metadata.Revision,
		CheckedAt:        time.Now().Unix(),
		Target:           relayrelease.CurrentTarget(),
		Mode:             mode,
		Eligible:         eligible,
		CanInstall:       newer && eligible,
		Reason:           "",
	}
	if newer {
		m.state.AvailableVersion = metadata.Version
		m.state.AvailableRevision = shortRevision(metadata.Revision)
		m.state.TargetVersion = metadata.Version
		m.state.TargetRevision = metadata.Revision
		if eligible {
			m.state.State = "available"
		} else {
			m.state.State = "blocked"
			m.state.Reason = reason
		}
	}
	_ = writeState(m.statePath(), m.state)
	return m.publicState(m.state)
}

func (m *Manager) Schedule(ctx context.Context, expectedVersion, expectedRevision string) (string, State, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	current := m.loadState()
	if current.State != "" {
		m.state = current
	}
	if m.state.State != "available" || !m.state.CanInstall {
		reason := m.state.Reason
		if reason == "" {
			reason = "No installable update is available"
		}
		return "", m.publicState(m.state), errors.New(reason)
	}
	if expectedVersion != m.state.AvailableVersion || expectedRevision != m.state.TargetRevision {
		return "", m.publicState(m.state), errors.New("The advertised update changed; check again before installing")
	}
	if m.metadata.Version != expectedVersion || m.metadata.Revision != expectedRevision {
		metadata, err := m.fetchRelease(ctx)
		if err != nil || metadata.Version != expectedVersion || metadata.Revision != expectedRevision {
			return "", m.publicState(m.state), errors.New("The advertised update changed; check again before installing")
		}
		m.metadata = metadata
	}
	if err := os.MkdirAll(m.runtimeDir, 0o700); err != nil {
		return "", m.publicState(m.state), err
	}
	jobPath := filepath.Join(m.runtimeDir, fmt.Sprintf("update-job-%d.json", time.Now().UnixNano()))
	job := Job{
		ReleaseRoot:    m.releaseRoot,
		DownloadURL:    m.metadata.ArchiveURL,
		ChecksumURL:    m.metadata.ChecksumURL,
		ArchiveName:    m.metadata.ArchiveName,
		TargetVersion:  m.metadata.Version,
		TargetRevision: m.metadata.Revision,
		Target:         relayrelease.CurrentTarget(),
		StatePath:      m.statePath(),
		ServiceName:    m.serviceName,
		HealthURL:      m.healthURL,
		TokenFile:      m.tokenFile,
	}
	if err := writeJSONAtomic(jobPath, job); err != nil {
		return "", m.publicState(m.state), fmt.Errorf("persist update job: %w", err)
	}
	m.state.State = "scheduled"
	m.state.CanInstall = false
	m.state.Eligible = true
	m.state.Reason = ""
	m.state.Error = ""
	if err := writeState(m.statePath(), m.state); err != nil {
		_ = os.Remove(jobPath)
		return "", m.publicState(m.state), fmt.Errorf("persist scheduled update: %w", err)
	}
	if err := m.launch(ctx, jobPath, m.serviceName); err != nil {
		m.state.State = "failed"
		m.state.Error = safeError(err)
		_ = writeState(m.statePath(), m.state)
		_ = os.Remove(jobPath)
		return "", m.publicState(m.state), err
	}
	return filepath.Base(jobPath), m.publicState(m.state), nil
}

func (m *Manager) fetchRelease(ctx context.Context) (releaseMetadata, error) {
	var release githubRelease
	if err := m.getJSON(ctx, m.apiBase+"/releases/latest", &release); err != nil {
		return releaseMetadata{}, fmt.Errorf("read canonical release: %w", err)
	}
	if release.Draft || release.Prerelease {
		return releaseMetadata{}, errors.New("canonical latest release is not a stable published release")
	}
	version := strings.TrimPrefix(release.TagName, "v")
	if !semverPattern.MatchString(version) {
		return releaseMetadata{}, fmt.Errorf("release tag %q is not semantic versioned", release.TagName)
	}
	revision, err := m.fetchTagRevision(ctx, release.TagName)
	if err != nil {
		return releaseMetadata{}, err
	}
	archiveName := fmt.Sprintf(
		"herdr-mobile-relay_%s_%s_%s.tar.gz",
		version,
		runtime.GOOS,
		runtime.GOARCH,
	)
	var archiveURL, checksumURL string
	for _, asset := range release.Assets {
		switch asset.Name {
		case archiveName:
			archiveURL = asset.URL
		case "checksums.txt":
			checksumURL = asset.URL
		}
	}
	if archiveURL == "" || checksumURL == "" {
		return releaseMetadata{}, fmt.Errorf("release is missing %s or checksums.txt", archiveName)
	}
	return releaseMetadata{
		Version:     version,
		Revision:    revision,
		ArchiveName: archiveName,
		ArchiveURL:  archiveURL,
		ChecksumURL: checksumURL,
		ReleaseTag:  release.TagName,
		PublishedAt: release.PublishedAt,
	}, nil
}

func (m *Manager) fetchTagRevision(ctx context.Context, tag string) (string, error) {
	var reference struct {
		Object gitObject `json:"object"`
	}
	tagURL := m.apiBase + "/git/ref/tags/" + url.PathEscape(tag)
	if err := m.getJSON(ctx, tagURL, &reference); err != nil {
		return "", fmt.Errorf("resolve release tag: %w", err)
	}
	object := reference.Object
	for depth := 0; depth < 3; depth++ {
		if object.Type == "commit" && validRevision(object.SHA) {
			return strings.ToLower(object.SHA), nil
		}
		if object.Type != "tag" || object.URL == "" {
			break
		}
		var annotated gitObject
		if err := m.getJSON(ctx, object.URL, &annotated); err != nil {
			return "", fmt.Errorf("resolve annotated release tag: %w", err)
		}
		if annotated.Object == nil {
			break
		}
		object = gitObject{
			SHA:  annotated.Object.SHA,
			Type: annotated.Object.Type,
			URL:  annotated.Object.URL,
		}
	}
	return "", errors.New("release tag did not resolve to an exact commit")
}

func (m *Manager) getJSON(ctx context.Context, endpoint string, destination any) error {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return err
	}
	request.Header.Set("Accept", "application/vnd.github+json")
	request.Header.Set("User-Agent", "herdr-mobile-relay-update-check")
	if token := m.token(); token != "" {
		request.Header.Set("Authorization", "token "+token)
	}
	response, err := m.client.Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return fmt.Errorf("HTTP %d", response.StatusCode)
	}
	return json.NewDecoder(io.LimitReader(response.Body, 2*1024*1024)).Decode(destination)
}

func (m *Manager) token() string {
	if m.tokenFile != "" {
		data, err := os.ReadFile(m.tokenFile)
		if err == nil {
			if token := strings.TrimSpace(string(data)); token != "" {
				return token
			}
		}
	}
	return os.Getenv("GH_TOKEN")
}

func (m *Manager) eligibility() (bool, string, string) {
	currentTarget, manifest, err := currentRelease(m.releaseRoot)
	if err != nil {
		return false, "unsupported", "Self-update requires a verified packaged release"
	}
	if manifest.Version != m.version {
		return false, "unsupported", "The running version does not match the active packaged release"
	}
	executable, err := os.Executable()
	if err != nil {
		return false, "unsupported", "The running executable could not be identified"
	}
	resolved, err := filepath.EvalSymlinks(executable)
	if err != nil {
		resolved = executable
	}
	if filepath.Dir(resolved) != currentTarget {
		return false, "unsupported", "The service is not running from the active release directory"
	}
	return true, "release", ""
}

func (m *Manager) launchWorker(ctx context.Context, jobPath, serviceName string) error {
	executable, err := os.Executable()
	if err != nil {
		return err
	}
	label := fmt.Sprintf("herdr-mobile-relay-update-%d", time.Now().Unix())
	var command *exec.Cmd
	if runtime.GOOS == "darwin" {
		command = exec.CommandContext(ctx, "launchctl", "submit", "-l", label, "--", executable, "update-worker", jobPath)
	} else {
		command = exec.CommandContext(
			ctx,
			"systemd-run",
			"--user",
			"--collect",
			"--unit="+label,
			executable,
			"update-worker",
			jobPath,
		)
	}
	output, err := command.CombinedOutput()
	if err != nil {
		return fmt.Errorf("schedule update worker: %s: %s", err, compact(string(output), 300))
	}
	return nil
}

func (m *Manager) loadState() State {
	data, err := os.ReadFile(m.statePath())
	if err != nil {
		return State{
			State:           "checking",
			CurrentVersion:  m.version,
			CurrentRevision: m.revision,
			Target:          relayrelease.CurrentTarget(),
		}
	}
	var state State
	if json.Unmarshal(data, &state) != nil || !validState(state.State) {
		return State{
			State:           "checking",
			CurrentVersion:  m.version,
			CurrentRevision: m.revision,
			Target:          relayrelease.CurrentTarget(),
		}
	}
	state.CurrentVersion = m.version
	state.CurrentRevision = m.revision
	return state
}

func (m *Manager) publicState(state State) State {
	state.CurrentVersion = m.version
	state.CurrentRevision = m.revision
	state.Error = compact(state.Error, 500)
	state.Reason = compact(state.Reason, 500)
	state.AvailableRevision = shortRevision(state.AvailableRevision)
	return state
}

func (m *Manager) statePath() string {
	return filepath.Join(m.runtimeDir, "update-state.json")
}

func NewerVersion(candidate, current string) bool {
	next, ok := parseSemver(candidate)
	if !ok {
		return false
	}
	installed, ok := parseSemver(current)
	if !ok {
		return false
	}
	for index := range next {
		if next[index] != installed[index] {
			return next[index] > installed[index]
		}
	}
	return false
}

func parseSemver(value string) ([3]int, bool) {
	match := semverPattern.FindStringSubmatch(value)
	if match == nil {
		return [3]int{}, false
	}
	var result [3]int
	for index := range result {
		number, err := strconv.Atoi(match[index+1])
		if err != nil {
			return [3]int{}, false
		}
		result[index] = number
	}
	return result, true
}

func validState(value string) bool {
	switch value {
	case "current", "checking", "available", "blocked", "scheduled", "installing",
		"restarting", "succeeded", "failed", "rolled_back", "unsupported":
		return true
	default:
		return false
	}
}

func validRevision(value string) bool {
	if len(value) != 40 {
		return false
	}
	for _, character := range value {
		if !strings.ContainsRune("0123456789abcdefABCDEF", character) {
			return false
		}
	}
	return true
}

func shortRevision(value string) string {
	if len(value) > 12 {
		return value[:12]
	}
	return value
}

func writeJSONAtomic(filename string, value any) error {
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	directory := filepath.Dir(filename)
	if err := os.MkdirAll(directory, 0o700); err != nil {
		return err
	}
	temp, err := os.CreateTemp(directory, "."+filepath.Base(filename)+".")
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
	return os.Rename(tempName, filename)
}
