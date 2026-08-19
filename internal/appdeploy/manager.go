package appdeploy

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/0cv/herdr-mobile-relay/internal/release"
	"github.com/0cv/herdr-mobile-relay/internal/setuphelper"
)

type PublicState struct {
	Configured     bool   `json:"configured"`
	Origin         string `json:"origin"`
	Project        string `json:"project"`
	Branch         string `json:"branch"`
	Revision       string `json:"revision"`
	Reason         string `json:"reason"`
	State          string `json:"state"`
	TargetVersion  string `json:"target_version"`
	TargetRevision string `json:"target_revision"`
	CheckedAt      int64  `json:"checked_at"`
	Error          string `json:"error"`
}

type Manager struct {
	runtimeDir string
	webRoot    string
	version    string
	revision   string
	webHash    string
	origin     string
	project    string
	branch     string
	npxPath    string
	nodeDir    string
	reason     string
	required   bool

	mu     sync.Mutex
	launch func(context.Context, string) error
}

func NewManager(runtimeDir, webRoot, version, revision string) *Manager {
	recoverAppDeployState(runtimeDir, true)
	originValue := strings.TrimSpace(os.Getenv("HERDR_APP_DEPLOY_ORIGIN"))
	projectValue := strings.TrimSpace(os.Getenv("HERDR_CLOUDFLARE_PAGES_PROJECT"))
	manager := &Manager{
		runtimeDir: runtimeDir,
		webRoot:    webRoot,
		version:    version,
		revision:   revision,
		project:    strings.ToLower(projectValue),
		branch:     strings.TrimSpace(os.Getenv("HERDR_CLOUDFLARE_PAGES_BRANCH")),
		npxPath:    strings.TrimSpace(os.Getenv("HERDR_APP_DEPLOY_NPX")),
		nodeDir:    strings.TrimSpace(os.Getenv("HERDR_APP_DEPLOY_NODE_DIR")),
		required:   originValue != "" || projectValue != "",
	}
	manifest, manifestErr := release.Load(filepath.Dir(webRoot))
	if manifestErr != nil {
		manager.reason = "The verified release manifest is unavailable"
	} else if manifest.Version != version || manifest.Revision != revision || manifest.WebHash == "" {
		manager.reason = "The verified release manifest does not match this relay release"
	} else {
		manager.webHash = manifest.WebHash
	}
	if manager.branch == "" {
		manager.branch = "main"
	}
	var originReason string
	manager.origin, originReason = configuredOrigin(originValue)
	if manager.reason == "" {
		manager.reason = originReason
	}
	if manager.reason == "" {
		job := Job{
			RuntimeDir: runtimeDir,
			WebRoot:    webRoot,
			Origin:     manager.origin,
			Project:    manager.project,
			Branch:     manager.branch,
			Version:    version,
			Revision:   revision,
			WebHash:    manager.webHash,
			NPXPath:    manager.npxPath,
			NodeDir:    manager.nodeDir,
		}
		if err := validate(job); err != nil {
			manager.reason = err.Error()
		}
	}
	manager.launch = manager.launchWorker
	return manager
}

// Required reports whether this relay owns a separately deployed phone app.
// Invalid partial configuration remains required so relay updates fail closed
// instead of upgrading past an app that could not be deployed first.
func (m *Manager) Required() bool {
	return m.required
}

// ConfiguredOrigin returns the immutable app origin loaded when the relay
// started. Browser connections must not replace this deployment-owned origin.
func (m *Manager) ConfiguredOrigin() string {
	return m.origin
}

// ValidateOrigin pins a phone-triggered deployment to this relay's configured
// public app origin.
func (m *Manager) ValidateOrigin(expected string) error {
	if m.reason != "" {
		return errors.New(m.reason)
	}
	if expected == "" || expected != m.origin {
		return errors.New("The requested app deployment does not match the configured origin")
	}
	return nil
}

func RunConfigured(ctx context.Context, runtimeDir, webRoot, version, revision string) error {
	return runConfigured(ctx, runtimeDir, webRoot, version, revision, "")
}

func RunConfiguredAtOrigin(
	ctx context.Context,
	runtimeDir, webRoot, version, revision, expectedOrigin string,
) error {
	if expectedOrigin == "" {
		return errors.New("The expected app origin is required")
	}
	return runConfigured(ctx, runtimeDir, webRoot, version, revision, expectedOrigin)
}

func runConfigured(
	ctx context.Context,
	runtimeDir, webRoot, version, revision, expectedOrigin string,
) error {
	manager := NewManager(runtimeDir, webRoot, version, revision)
	if manager.reason != "" {
		return errors.New(manager.reason)
	}
	if expectedOrigin != "" {
		if err := manager.ValidateOrigin(expectedOrigin); err != nil {
			return err
		}
	}
	if err := os.MkdirAll(runtimeDir, 0o700); err != nil {
		return err
	}
	jobPath := filepath.Join(runtimeDir, fmt.Sprintf("app-deploy-job-%d.json", time.Now().UnixNano()))
	job := Job{
		RuntimeDir: runtimeDir,
		WebRoot:    webRoot,
		Origin:     manager.origin,
		Project:    manager.project,
		Branch:     manager.branch,
		Version:    version,
		Revision:   revision,
		WebHash:    manager.webHash,
		NPXPath:    manager.npxPath,
		NodeDir:    manager.nodeDir,
	}
	if err := writeManagerJSON(jobPath, job); err != nil {
		return err
	}
	return Run(ctx, jobPath)
}

func (m *Manager) State() PublicState {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.loadState()
}

func (m *Manager) Schedule(ctx context.Context, expectedVersion, expectedRevision, expectedOrigin string) (string, PublicState, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	state := m.loadState()
	if m.reason != "" {
		return "", state, errors.New(m.reason)
	}
	if state.State == "scheduled" || state.State == "deploying" {
		return "", state, errors.New("An app deployment is already running")
	}
	if expectedVersion != m.version || expectedRevision != m.revision || expectedOrigin != m.origin {
		return "", state, errors.New("The requested app deployment does not match this relay release and configured origin")
	}
	jobPath := filepath.Join(m.runtimeDir, fmt.Sprintf("app-deploy-job-%d.json", time.Now().UnixNano()))
	job := Job{
		RuntimeDir: m.runtimeDir,
		WebRoot:    m.webRoot,
		Origin:     m.origin,
		Project:    m.project,
		Branch:     m.branch,
		Version:    m.version,
		Revision:   m.revision,
		WebHash:    m.webHash,
		NPXPath:    m.npxPath,
		NodeDir:    m.nodeDir,
	}
	if err := writeManagerJSON(jobPath, job); err != nil {
		return "", state, err
	}
	scheduled := State{
		State:          "scheduled",
		TargetVersion:  m.version,
		TargetRevision: m.revision,
		StartedAt:      time.Now().UTC().Format(time.RFC3339),
	}
	if err := writeState(filepath.Join(m.runtimeDir, "app-deploy-state.json"), scheduled); err != nil {
		_ = os.Remove(jobPath)
		return "", state, err
	}
	if err := m.launch(ctx, jobPath); err != nil {
		scheduled.State = "failed"
		scheduled.Error = safeError(err)
		_ = writeState(filepath.Join(m.runtimeDir, "app-deploy-state.json"), scheduled)
		_ = os.Remove(jobPath)
		return "", m.public(scheduled), err
	}
	return filepath.Base(jobPath), m.public(scheduled), nil
}

func (m *Manager) loadState() PublicState {
	state := State{State: "idle"}
	statePath := filepath.Join(m.runtimeDir, "app-deploy-state.json")
	data, err := os.ReadFile(statePath)
	if err == nil {
		var loaded State
		if json.Unmarshal(data, &loaded) == nil && validDeployState(loaded.State) {
			state = loaded
		}
	}
	if state.State == "deploying" || scheduledStateExpired(state) {
		recoverAppDeployState(m.runtimeDir, false)
		data, err = os.ReadFile(statePath)
		if err == nil {
			var recovered State
			if json.Unmarshal(data, &recovered) == nil && validDeployState(recovered.State) {
				state = recovered
			}
		}
	}
	return m.public(state)
}

func (m *Manager) public(state State) PublicState {
	return PublicState{
		Configured:     m.reason == "",
		Origin:         m.origin,
		Project:        m.project,
		Branch:         m.branch,
		Revision:       valueIf(m.reason == "", m.revision),
		Reason:         compact(m.reason, 500),
		State:          state.State,
		TargetVersion:  state.TargetVersion,
		TargetRevision: state.TargetRevision,
		CheckedAt:      unixTime(state.FinishedAt, state.StartedAt),
		Error:          compact(state.Error, 500),
	}
}

type workerLaunch struct {
	application string
	args        []string
}

var workerEnvironmentKeys = [...]string{
	"HERDR_RELAY_ENV",
	"HERDR_PLUGIN_CONFIG_DIR",
}

func appDeployWorkerLaunch(
	goos, label, executable, jobPath string,
	lookupEnv func(string) (string, bool),
) workerLaunch {
	assignments := make([]string, 0, len(workerEnvironmentKeys))
	for _, key := range workerEnvironmentKeys {
		value, present := lookupEnv(key)
		if present && strings.TrimSpace(value) != "" {
			assignments = append(assignments, key+"="+value)
		}
	}

	worker := []string{executable, "app-deploy-worker", jobPath}
	if goos == "darwin" {
		args := []string{"submit", "-l", label, "--"}
		if len(assignments) > 0 {
			args = append(args, "/usr/bin/env")
			args = append(args, assignments...)
		}
		args = append(args, worker...)
		return workerLaunch{application: "launchctl", args: args}
	}

	args := []string{"--user", "--collect", "--unit=" + label}
	for _, assignment := range assignments {
		args = append(args, "--setenv="+assignment)
	}
	args = append(args, worker...)
	return workerLaunch{application: "systemd-run", args: args}
}

func (m *Manager) launchWorker(ctx context.Context, jobPath string) error {
	executable, err := os.Executable()
	if err != nil {
		return err
	}
	label := fmt.Sprintf("herdr-mobile-relay-app-deploy-%d", time.Now().Unix())
	launch := appDeployWorkerLaunch(runtime.GOOS, label, executable, jobPath, os.LookupEnv)
	command := exec.CommandContext(ctx, launch.application, launch.args...)
	output, err := command.CombinedOutput()
	if err != nil {
		return fmt.Errorf("schedule app deployment worker: %s: %s", err, compact(string(output), 300))
	}
	return nil
}

func configuredOrigin(value string) (string, string) {
	if strings.TrimSpace(value) == "" {
		return "", "No HTTPS app deployment origin is configured"
	}
	origin, err := setuphelper.NormalizeOrigin(value, false)
	if err != nil {
		return "", "The configured app deployment origin is invalid"
	}
	return origin, ""
}

func writeManagerJSON(filename string, value any) error {
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	if err := os.MkdirAll(filepath.Dir(filename), 0o700); err != nil {
		return err
	}
	temp, err := os.CreateTemp(filepath.Dir(filename), "."+filepath.Base(filename)+".")
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

func validDeployState(value string) bool {
	switch value {
	case "idle", "scheduled", "deploying", "succeeded", "failed":
		return true
	default:
		return false
	}
}

func recoverAppDeployState(runtimeDir string, includeScheduled bool) {
	statePath := filepath.Join(runtimeDir, "app-deploy-state.json")
	data, err := os.ReadFile(statePath)
	if err != nil {
		return
	}
	var state State
	if json.Unmarshal(data, &state) != nil {
		return
	}
	_, startedErr := time.Parse(time.RFC3339, state.StartedAt)
	recoverScheduled := state.State == "scheduled" &&
		(scheduledStateExpired(state) || (includeScheduled && startedErr != nil))
	if state.State != "deploying" && !recoverScheduled {
		return
	}
	lock, err := lockFile(filepath.Join(runtimeDir, "app-deploy.lock"))
	if err != nil {
		return
	}
	defer lock.Close()
	state.State = "failed"
	state.FinishedAt = time.Now().UTC().Format(time.RFC3339)
	state.Error = "App deployment worker stopped before completion"
	_ = writeState(statePath, state)
}

func scheduledStateExpired(state State) bool {
	if state.State != "scheduled" {
		return false
	}
	started, err := time.Parse(time.RFC3339, state.StartedAt)
	return err == nil && time.Since(started) >= deploymentStartupGrace
}

func unixTime(values ...string) int64 {
	for _, value := range values {
		if parsed, err := time.Parse(time.RFC3339, value); err == nil {
			return parsed.Unix()
		}
	}
	return 0
}

func valueIf(condition bool, value string) string {
	if condition {
		return value
	}
	return ""
}
