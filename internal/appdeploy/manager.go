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
	origin     string
	project    string
	branch     string
	npxPath    string
	nodeDir    string
	reason     string

	mu     sync.Mutex
	launch func(context.Context, string) error
}

func NewManager(runtimeDir, webRoot, version, revision string) *Manager {
	manager := &Manager{
		runtimeDir: runtimeDir,
		webRoot:    webRoot,
		version:    version,
		revision:   revision,
		project:    strings.ToLower(strings.TrimSpace(os.Getenv("HERDR_CLOUDFLARE_PAGES_PROJECT"))),
		branch:     strings.TrimSpace(os.Getenv("HERDR_CLOUDFLARE_PAGES_BRANCH")),
		npxPath:    strings.TrimSpace(os.Getenv("HERDR_APP_DEPLOY_NPX")),
		nodeDir:    strings.TrimSpace(os.Getenv("HERDR_APP_DEPLOY_NODE_DIR")),
	}
	if manager.branch == "" {
		manager.branch = "main"
	}
	manager.origin, manager.reason = configuredOrigin(os.Getenv("HERDR_APP_DEPLOY_ORIGIN"))
	if manager.reason == "" {
		job := Job{
			RuntimeDir: runtimeDir,
			WebRoot:    webRoot,
			Origin:     manager.origin,
			Project:    manager.project,
			Branch:     manager.branch,
			Version:    version,
			Revision:   revision,
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

func RunConfigured(ctx context.Context, runtimeDir, webRoot, version, revision string) error {
	manager := NewManager(runtimeDir, webRoot, version, revision)
	if manager.reason != "" {
		return errors.New(manager.reason)
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
	data, err := os.ReadFile(filepath.Join(m.runtimeDir, "app-deploy-state.json"))
	if err == nil {
		var loaded State
		if json.Unmarshal(data, &loaded) == nil && validDeployState(loaded.State) {
			state = loaded
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

func (m *Manager) launchWorker(ctx context.Context, jobPath string) error {
	executable, err := os.Executable()
	if err != nil {
		return err
	}
	label := fmt.Sprintf("herdr-mobile-relay-app-deploy-%d", time.Now().Unix())
	var command *exec.Cmd
	if runtime.GOOS == "darwin" {
		command = exec.CommandContext(ctx, "launchctl", "submit", "-l", label, "--", executable, "app-deploy-worker", jobPath)
	} else {
		command = exec.CommandContext(
			ctx,
			"systemd-run",
			"--user",
			"--collect",
			"--unit="+label,
			executable,
			"app-deploy-worker",
			jobPath,
		)
	}
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
