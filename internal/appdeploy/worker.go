package appdeploy

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
	"time"

	"github.com/0cv/herdr-mobile-relay/internal/release"
	"github.com/0cv/herdr-mobile-relay/internal/setuphelper"
)

const (
	WranglerVersion        = "4.112.0"
	deploymentStartupGrace = 30 * time.Second
)

var (
	projectPattern  = regexp.MustCompile(`^[a-z0-9](?:[a-z0-9-]{0,57}[a-z0-9])?$`)
	branchPattern   = regexp.MustCompile(`^[A-Za-z0-9](?:[A-Za-z0-9._/-]{0,118}[A-Za-z0-9])?$`)
	errDeployLocked = errors.New("another app deployment is already running")
)

type Job struct {
	RuntimeDir string `json:"runtime_dir"`
	WebRoot    string `json:"web_root"`
	Origin     string `json:"origin"`
	Project    string `json:"project"`
	Branch     string `json:"branch"`
	Version    string `json:"version"`
	Revision   string `json:"revision"`
	WebHash    string `json:"web_hash"`
	NPXPath    string `json:"npx_path"`
	NodeDir    string `json:"node_dir"`
}

type State struct {
	State          string `json:"state"`
	TargetVersion  string `json:"target_version,omitempty"`
	TargetRevision string `json:"target_revision,omitempty"`
	StartedAt      string `json:"started_at,omitempty"`
	FinishedAt     string `json:"finished_at,omitempty"`
	Error          string `json:"error,omitempty"`
}

func Run(ctx context.Context, jobPath string) error {
	job, err := loadJob(jobPath)
	if err != nil {
		return err
	}
	started := time.Now().UTC().Format(time.RFC3339)
	statePath := filepath.Join(job.RuntimeDir, "app-deploy-state.json")
	fail := func(deployErr error) error {
		stateErr := writeState(statePath, State{
			State:          "failed",
			TargetVersion:  job.Version,
			TargetRevision: job.Revision,
			StartedAt:      started,
			FinishedAt:     time.Now().UTC().Format(time.RFC3339),
			Error:          safeError(deployErr),
		})
		if stateErr != nil {
			return errors.Join(deployErr, fmt.Errorf("write failed app deployment state: %w", stateErr))
		}
		return deployErr
	}
	if !filepath.IsAbs(job.RuntimeDir) {
		return errors.New("app deployment runtime directory must be absolute")
	}
	if err := os.MkdirAll(job.RuntimeDir, 0o700); err != nil {
		return fail(err)
	}
	lock, err := lockFile(filepath.Join(job.RuntimeDir, "app-deploy.lock"))
	if err != nil {
		if errors.Is(err, errDeployLocked) {
			return err
		}
		return fail(err)
	}
	defer lock.Close()

	if err := validate(job); err != nil {
		return fail(err)
	}
	write := func(state string, deployErr error) {
		value := State{
			State:          state,
			TargetVersion:  job.Version,
			TargetRevision: job.Revision,
			StartedAt:      started,
		}
		if state == "succeeded" || state == "failed" {
			value.FinishedAt = time.Now().UTC().Format(time.RFC3339)
		}
		if deployErr != nil {
			value.Error = safeError(deployErr)
		}
		_ = writeState(statePath, value)
	}
	write("deploying", nil)

	if err := verifyWebBundle(job); err != nil {
		write("failed", err)
		return err
	}
	command := exec.CommandContext(
		ctx,
		job.NPXPath,
		"--yes",
		"wrangler@"+WranglerVersion,
		"pages",
		"deploy",
		job.WebRoot,
		"--project-name",
		job.Project,
		"--branch",
		job.Branch,
	)
	command.Env = commandEnvironment(job.NodeDir, os.Environ())
	output, err := command.CombinedOutput()
	if err != nil {
		deployErr := fmt.Errorf("Wrangler deployment failed: %s", compact(string(output), 500))
		write("failed", deployErr)
		return deployErr
	}
	if err := verifyPublic(ctx, job); err != nil {
		write("failed", err)
		return err
	}
	write("succeeded", nil)
	_ = os.Remove(jobPath)
	return nil
}

func commandEnvironment(nodeDir string, environment []string) []string {
	result := make([]string, 0, len(environment)+1)
	pathValue := ""
	for _, value := range environment {
		if strings.HasPrefix(value, "PATH=") {
			pathValue = strings.TrimPrefix(value, "PATH=")
			continue
		}
		result = append(result, value)
	}
	result = append(result, "PATH="+nodeDir+string(os.PathListSeparator)+pathValue)
	return result
}

func loadJob(filename string) (Job, error) {
	data, err := os.ReadFile(filename)
	if err != nil {
		return Job{}, fmt.Errorf("read app deployment job: %w", err)
	}
	var job Job
	if err := json.Unmarshal(data, &job); err != nil {
		return Job{}, fmt.Errorf("parse app deployment job: %w", err)
	}
	return job, nil
}

func validate(job Job) error {
	origin, err := setuphelper.NormalizeOrigin(job.Origin, false)
	if err != nil || origin != job.Origin {
		return errors.New("app deployment origin must be a canonical HTTPS origin")
	}
	if !projectPattern.MatchString(job.Project) {
		return errors.New("app deployment project name is invalid")
	}
	if !branchPattern.MatchString(job.Branch) || strings.Contains(job.Branch, "..") || strings.HasPrefix(job.Branch, "/") {
		return errors.New("app deployment branch is invalid")
	}
	if job.Version == "" || job.Revision == "" {
		return errors.New("installed version and revision are required")
	}
	if info, err := os.Stat(job.WebRoot); err != nil || !info.IsDir() {
		return errors.New("validated web bundle is unavailable")
	}
	if info, err := os.Stat(job.NPXPath); err != nil || info.IsDir() || info.Mode()&0o111 == 0 {
		return errors.New("configured npx executable is unavailable")
	}
	if info, err := os.Stat(filepath.Join(job.NodeDir, "node")); err != nil || info.IsDir() || info.Mode()&0o111 == 0 {
		return errors.New("configured Node.js executable is unavailable")
	}
	local, err := os.ReadFile(filepath.Join(job.WebRoot, "version.json"))
	if err != nil {
		return fmt.Errorf("read web bundle version: %w", err)
	}
	if err := verifyVersion(local, job.Version, job.Revision); err != nil {
		return fmt.Errorf("web bundle identity: %w", err)
	}
	if err := verifyWebBundle(job); err != nil {
		return err
	}
	return nil
}

func verifyWebBundle(job Job) error {
	if job.WebHash == "" {
		return errors.New("verified release web hash is required")
	}
	actual, err := release.WebHashFS(os.DirFS(job.WebRoot))
	if err != nil {
		return fmt.Errorf("hash web bundle: %w", err)
	}
	if actual != job.WebHash {
		return errors.New("web bundle does not match the verified release manifest")
	}
	return nil
}

func verifyPublic(ctx context.Context, job Job) error {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, job.Origin+"/version.json", nil)
	if err != nil {
		return err
	}
	client := &http.Client{
		Timeout: 20 * time.Second,
		CheckRedirect: func(request *http.Request, via []*http.Request) error {
			if request.URL.Scheme+"://"+request.URL.Host != job.Origin {
				return errors.New("public version check redirected to another origin")
			}
			return nil
		},
	}
	response, err := client.Do(request)
	if err != nil {
		return fmt.Errorf("verify public app: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return fmt.Errorf("verify public app: HTTP %d", response.StatusCode)
	}
	var identity map[string]any
	if err := json.NewDecoder(io.LimitReader(response.Body, 64*1024)).Decode(&identity); err != nil {
		return fmt.Errorf("decode public version: %w", err)
	}
	data, _ := json.Marshal(identity)
	if err := verifyVersion(data, job.Version, job.Revision); err != nil {
		return fmt.Errorf("public web bundle identity: %w", err)
	}
	return nil
}

func verifyVersion(data []byte, version, revision string) error {
	var identity struct {
		Version        string `json:"version"`
		ReleaseVersion string `json:"release_version"`
		Revision       string `json:"revision"`
	}
	if err := json.Unmarshal(data, &identity); err != nil {
		return err
	}
	actualVersion := identity.ReleaseVersion
	if actualVersion == "" {
		actualVersion = identity.Version
	}
	if actualVersion != version || identity.Revision != revision {
		return fmt.Errorf("expected %s (%s), got %s (%s)", version, revision, actualVersion, identity.Revision)
	}
	return nil
}

func lockFile(filename string) (*os.File, error) {
	file, err := os.OpenFile(filename, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(file.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		file.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return nil, errDeployLocked
		}
		return nil, fmt.Errorf("lock app deployment: %w", err)
	}
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
	temp, err := os.CreateTemp(directory, ".app-deploy-state.")
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

func compact(value string, limit int) string {
	value = strings.Join(strings.Fields(value), " ")
	if len(value) > limit {
		return value[:limit]
	}
	return value
}

func safeError(err error) string {
	return compact(err.Error(), 500)
}
