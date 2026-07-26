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

const canonicalAPI = "https://api.github.com/repos/0cv/herdr-mobile-relay"

var semverPattern = regexp.MustCompile(`^(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)$`)

type Manager struct {
	releaseRoot string
	runtimeDir  string
	herdrBin    string
	version     string
	revision    string
	healthURL   string
	apiBase     string
	client      *http.Client
	tokenFile   string

	mu       sync.Mutex
	state    State
	metadata releaseMetadata
	launch   func(context.Context, string) error
}

type releaseMetadata struct {
	Version  string
	Revision string
}

type githubRelease struct {
	TagName    string `json:"tag_name"`
	Draft      bool   `json:"draft"`
	Prerelease bool   `json:"prerelease"`
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

func NewManager(releaseRoot, runtimeDir, herdrBin, version, revision, healthURL string) *Manager {
	manager := &Manager{
		releaseRoot: releaseRoot,
		runtimeDir:  runtimeDir,
		herdrBin:    herdrBin,
		version:     version,
		revision:    revision,
		healthURL:   healthURL,
		apiBase:     canonicalAPI,
		client: &http.Client{
			Timeout: 15 * time.Second,
		},
	}
	if tokenFile := strings.TrimSpace(os.Getenv("HERDR_GITHUB_TOKEN_FILE")); filepath.IsAbs(tokenFile) {
		manager.tokenFile = filepath.Clean(tokenFile)
	}
	manager.launch = manager.launchWorker
	manager.state = manager.loadState()
	manager.recoverOrphan(true)
	manager.state = manager.loadState()
	return manager
}

func (m *Manager) State() State {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.recoverOrphan(false)
	state := m.loadState()
	if state.State != "" {
		m.state = state
	}
	return m.publicState(m.state)
}

func (m *Manager) Check(ctx context.Context) State {
	m.mu.Lock()
	m.state.State = "checking"
	m.state.Error = ""
	m.state.CurrentVersion = m.version
	m.state.CurrentRevision = m.revision
	_ = writeState(m.statePath(), m.state)
	m.mu.Unlock()

	metadata, err := m.fetchRelease(ctx)
	m.mu.Lock()
	defer m.mu.Unlock()
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
		HerdrBin:       m.herdrBin,
		TargetVersion:  m.metadata.Version,
		TargetRevision: m.metadata.Revision,
		StatePath:      m.statePath(),
		HealthURL:      m.healthURL,
	}
	if err := writeJSONAtomic(jobPath, job); err != nil {
		return "", m.publicState(m.state), fmt.Errorf("persist update job: %w", err)
	}
	m.state.State = "scheduled"
	m.state.CanInstall = false
	m.state.Eligible = true
	m.state.Reason = ""
	m.state.Error = ""
	m.state.StartedAt = time.Now().UTC().Format(time.RFC3339)
	m.state.FinishedAt = ""
	if err := writeState(m.statePath(), m.state); err != nil {
		_ = os.Remove(jobPath)
		return "", m.publicState(m.state), fmt.Errorf("persist scheduled update: %w", err)
	}
	if err := m.launch(ctx, jobPath); err != nil {
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
	return releaseMetadata{
		Version:  version,
		Revision: revision,
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
	return ""
}

func (m *Manager) eligibility() (bool, string, string) {
	if !semverPattern.MatchString(m.version) || !validRevision(m.revision) {
		return false, "unsupported", "Managed updates require a released relay build"
	}
	if !filepath.IsAbs(m.herdrBin) {
		return false, "unsupported", "Managed updates require an absolute Herdr executable path"
	}
	info, err := os.Stat(m.herdrBin)
	if err != nil || info.IsDir() || info.Mode()&0o111 == 0 {
		return false, "unsupported", "The Herdr executable is unavailable"
	}
	return true, "plugin", ""
}

func (m *Manager) launchWorker(ctx context.Context, jobPath string) error {
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
	if transientUpdateState(state.State) &&
		state.TargetVersion == m.version &&
		strings.EqualFold(state.TargetRevision, m.revision) {
		state.State = "succeeded"
		state.CanInstall = false
		state.Eligible = true
		state.Mode = "plugin"
		state.Error = ""
		if state.FinishedAt == "" {
			state.FinishedAt = time.Now().UTC().Format(time.RFC3339)
		}
		_ = writeState(m.statePath(), state)
		return state
	}
	if state.State == "available" || state.State == "blocked" {
		candidate := state.AvailableVersion
		if candidate == "" {
			candidate = state.TargetVersion
		}
		if !NewerVersion(candidate, m.version) {
			upstreamVersion := state.UpstreamVersion
			if upstreamVersion == "" {
				upstreamVersion = candidate
			}
			upstreamRevision := state.UpstreamRevision
			if upstreamRevision == "" {
				upstreamRevision = state.TargetRevision
			}
			return State{
				State:            "current",
				CurrentVersion:   m.version,
				CurrentRevision:  m.revision,
				UpstreamVersion:  upstreamVersion,
				UpstreamRevision: upstreamRevision,
				CheckedAt:        state.CheckedAt,
				Target:           relayrelease.CurrentTarget(),
				Mode:             state.Mode,
				Eligible:         state.Eligible,
			}
		}
	}
	return state
}

func (m *Manager) recoverOrphan(includeScheduled bool) {
	statePath := m.statePath()
	state, err := readState(statePath)
	if err != nil {
		return
	}
	if !transientUpdateState(state.State) {
		return
	}
	if state.TargetVersion == m.version &&
		strings.EqualFold(state.TargetRevision, m.revision) {
		state.State = "succeeded"
		state.CurrentVersion = m.version
		state.CurrentRevision = m.revision
		state.Mode = "plugin"
		state.Eligible = true
		state.CanInstall = false
		state.Error = ""
		state.FinishedAt = time.Now().UTC().Format(time.RFC3339)
		_ = writeState(statePath, state)
		return
	}
	started, startedErr := time.Parse(time.RFC3339, state.StartedAt)
	if startedErr == nil && time.Since(started) < updateStartupGrace {
		return
	}
	if state.State == "scheduled" && !includeScheduled && startedErr != nil {
		return
	}
	if m.releaseRoot == "" || !filepath.IsAbs(m.releaseRoot) {
		return
	}
	if err := os.MkdirAll(m.releaseRoot, 0o700); err != nil {
		return
	}
	lock, err := acquireLock(filepath.Join(m.releaseRoot, "update.lock"))
	if err != nil {
		return
	}

	state.State = "failed"
	state.FinishedAt = time.Now().UTC().Format(time.RFC3339)
	state.Error = "Herdr plugin update worker stopped before completion; run the update again"
	_ = writeState(statePath, state)
	_ = lock.Close()
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
		"restarting", "recovering", "succeeded", "failed", "rolled_back", "unsupported":
		return true
	default:
		return false
	}
}

func transientUpdateState(value string) bool {
	switch value {
	case "scheduled", "installing", "restarting", "recovering":
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
