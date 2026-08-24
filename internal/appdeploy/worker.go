package appdeploy

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
	"regexp"
	"strings"
	"syscall"
	"time"

	"github.com/0cv/herdr-mobile-relay/internal/release"
	"github.com/0cv/herdr-mobile-relay/internal/setuphelper"
)

const (
	// Keep this pinned for reproducible deployments, but refresh it when
	// Wrangler's Pages uploader regresses. 4.114.0's differential asset
	// upload path can fail with a generic exit status; 4.125.0 is the
	// current tested release and the deployment below skips that cache path.
	WranglerVersion           = "4.125.0"
	deploymentStartupGrace    = 30 * time.Second
	publicVerificationTimeout = 2 * time.Minute
	publicRequestTimeout      = 20 * time.Second
	deploymentTimeout         = 15 * time.Minute
	processTermGrace          = 2 * time.Second
	processWaitDelay          = 4 * time.Second
)

var ansiEscapePattern = regexp.MustCompile(`\x1b\[[0-?]*[ -/]*[@-~]`)

var (
	projectPattern          = regexp.MustCompile(`^[a-z0-9](?:[a-z0-9-]{0,57}[a-z0-9])?$`)
	branchPattern           = regexp.MustCompile(`^[A-Za-z0-9](?:[A-Za-z0-9._/-]{0,118}[A-Za-z0-9])?$`)
	errDeployLocked         = errors.New("another app deployment is already running")
	errPublicOriginRedirect = errors.New("public version check redirected to another origin")
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
	ctx, cancel := context.WithTimeout(ctx, deploymentTimeout)
	defer cancel()
	job, err := loadJob(jobPath)
	if err != nil {
		return err
	}
	// A job file has exactly one consumer: the worker handed its path. Nothing
	// rescans the runtime directory, so a job that is not removed is garbage
	// for good. Removing it only on success leaked one file per failed
	// deployment, and a relay retrying a broken deployment accumulated
	// thousands.
	defer func() { _ = os.Remove(jobPath) }()
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
	// launchctl submit (macOS) and systemd-run (Linux) hand the worker an
	// inherited working directory the deploy does not own: on macOS that is the
	// read-only filesystem root. Wrangler resolves its account config cache at
	// $PWD/.wrangler/cache and aborts the whole deployment when that mkdir
	// fails, so pin every deployment to a relay-owned directory instead of
	// inheriting one. --skip-caching below does not cover this: it only skips
	// the Pages asset upload cache, not the config cache.
	workDir := filepath.Join(job.RuntimeDir, "wrangler")
	if err := os.MkdirAll(workDir, 0o700); err != nil {
		deployErr := fmt.Errorf("create Wrangler working directory: %w", err)
		write("failed", deployErr)
		return deployErr
	}
	command := exec.Command(job.NPXPath, wranglerDeployArgs(job)...)
	command.Dir = workDir
	environment, credentialErr := commandEnvironmentWithCloudflareCredentials(job.NodeDir, os.Environ())
	environment = replaceEnvironmentValue(environment, "NO_COLOR", "1")
	if credentialErr != nil {
		deployErr := fmt.Errorf("read Cloudflare credentials: %w", credentialErr)
		write("failed", deployErr)
		return deployErr
	}
	command.Env = environment
	output, err := runCommandContext(ctx, command)
	if err != nil {
		deployErr := fmt.Errorf("Wrangler deployment failed: %w", err)
		if diagnostic := compact(string(output), 500); diagnostic != "" {
			deployErr = fmt.Errorf("Wrangler deployment failed: %w: %s", err, diagnostic)
		}
		write("failed", deployErr)
		return deployErr
	}
	if err := verifyPublic(ctx, job); err != nil {
		write("failed", err)
		return err
	}
	write("succeeded", nil)
	return nil
}

func wranglerDeployArgs(job Job) []string {
	return []string{
		"--yes",
		"wrangler@" + WranglerVersion,
		"pages",
		"deploy",
		job.WebRoot,
		"--project-name",
		job.Project,
		"--branch",
		job.Branch,
		// Avoid Wrangler's differential asset upload path. It can fail with
		// a generic exit status when an older deployment's cache is stale;
		// this bundle is tiny and verified, so a complete asset upload is the
		// safer deployment path.
		"--skip-caching",
	}
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
		terminateProcessGroup(command.Process.Pid, waitCh)
		return nil, ctx.Err()
	}
}

func combinedCommandOutput(stdout, stderr *bytes.Buffer) []byte {
	output := make([]byte, 0, stdout.Len()+stderr.Len())
	output = append(output, stdout.Bytes()...)
	return append(output, stderr.Bytes()...)
}

func terminateProcessGroup(pgid int, waitCh <-chan error) {
	_ = syscall.Kill(-pgid, syscall.SIGTERM)
	timer := time.NewTimer(processTermGrace)
	defer timer.Stop()
	leaderDone := false
	select {
	case <-waitCh:
		leaderDone = true
		if !processGroupAlive(pgid) {
			return
		}
		<-timer.C
	case <-timer.C:
	}
	_ = syscall.Kill(-pgid, syscall.SIGKILL)

	if !leaderDone {
		select {
		case <-waitCh:
		case <-time.After(2 * time.Second):
		}
	}
	deadline := time.Now().Add(2 * time.Second)
	for processGroupAlive(pgid) && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
}

func processGroupAlive(pgid int) bool {
	err := syscall.Kill(-pgid, 0)
	return err == nil || errors.Is(err, syscall.EPERM)
}

var cloudflareCredentialKeys = [...]string{
	"CLOUDFLARE_API_TOKEN",
	"CLOUDFLARE_ACCOUNT_ID",
}

func commandEnvironmentWithCloudflareCredentials(nodeDir string, environment []string) ([]string, error) {
	result := commandEnvironment(nodeDir, environment)
	filename := strings.TrimSpace(os.Getenv("HERDR_RELAY_ENV"))
	if filename == "" {
		if directory := strings.TrimSpace(os.Getenv("HERDR_PLUGIN_CONFIG_DIR")); directory != "" {
			filename = filepath.Join(directory, "relay.env")
		}
	}
	values, err := readCloudflareCredentials(filename, result)
	if err != nil {
		return nil, err
	}
	for _, key := range cloudflareCredentialKeys {
		value := strings.TrimSpace(values[key])
		if value == "" {
			continue
		}
		existing, present := environmentValue(result, key)
		if present && strings.TrimSpace(existing) != "" {
			continue
		}
		result = replaceEnvironmentValue(result, key, value)
	}
	return result, nil
}

func readCloudflareCredentials(filename string, environment []string) (map[string]string, error) {
	values := make(map[string]string, len(cloudflareCredentialKeys))
	if filename == "" {
		return values, nil
	}
	data, err := os.ReadFile(filename)
	if err != nil {
		return values, nil
	}
	assignments := make(map[string]string)
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		line = strings.TrimSpace(strings.TrimPrefix(line, "export "))
		key, value, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		key = strings.TrimSpace(key)
		if !isShellVariableName(key) {
			continue
		}
		assignments[key] = value
	}
	resolver := &shellVariableResolver{
		assignments: assignments,
		environment: environmentValues(environment),
		states:      make(map[string]uint8),
		values:      make(map[string]string),
	}
	for _, key := range cloudflareCredentialKeys {
		value, err := resolver.resolve(key)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", key, err)
		}
		values[key] = value
	}
	return values, nil
}

type shellVariableResolver struct {
	assignments map[string]string
	environment map[string]string
	states      map[string]uint8
	values      map[string]string
}

func (r *shellVariableResolver) resolve(name string) (string, error) {
	if value, ok := r.values[name]; ok {
		return value, nil
	}
	if r.states[name] == 1 {
		return "", fmt.Errorf("cyclic relay environment variable %q", name)
	}
	raw, ok := r.assignments[name]
	if !ok {
		value := r.environment[name]
		r.values[name] = value
		return value, nil
	}
	r.states[name] = 1
	value, err := parseShellEnvironmentValue(raw, r.resolve)
	if err != nil {
		delete(r.states, name)
		return "", err
	}
	r.states[name] = 2
	r.values[name] = value
	return value, nil
}

func environmentValues(environment []string) map[string]string {
	values := make(map[string]string, len(environment))
	for _, item := range environment {
		key, value, ok := strings.Cut(item, "=")
		if ok && isShellVariableName(key) {
			values[key] = value
		}
	}
	return values
}

func isShellVariableName(value string) bool {
	if value == "" || !isShellVariableNameStart(value[0]) {
		return false
	}
	for index := range len(value) - 1 {
		if !isShellVariableNamePart(value[index+1]) {
			return false
		}
	}
	return true
}

func isShellVariableNameStart(value byte) bool {
	return value == '_' || value >= 'A' && value <= 'Z' || value >= 'a' && value <= 'z'
}

func isShellVariableNamePart(value byte) bool {
	return isShellVariableNameStart(value) || value >= '0' && value <= '9'
}

func shellVariableReference(value string, index int) (string, int, bool) {
	if index+1 >= len(value) {
		return "", index + 1, false
	}
	if value[index+1] == '{' {
		closing := strings.IndexByte(value[index+2:], '}')
		if closing < 0 {
			return "", index + 1, false
		}
		closing += index + 2
		name := value[index+2 : closing]
		if !isShellVariableName(name) {
			return "", closing + 1, false
		}
		return name, closing + 1, true
	}
	if !isShellVariableNameStart(value[index+1]) {
		return "", index + 1, false
	}
	end := index + 2
	for end < len(value) && isShellVariableNamePart(value[end]) {
		end++
	}
	return value[index+1 : end], end, true
}

func parseShellEnvironmentValue(value string, resolve func(string) (string, error)) (string, error) {
	return unquoteShellWord(strings.TrimSpace(stripShellInlineComment(value)), resolve)
}

func stripShellInlineComment(value string) string {
	quote := byte(0)
	escaped := false
	escapedWhitespace := false
	for index := range len(value) {
		char := value[index]
		if escaped {
			escaped = false
			escapedWhitespace = char == ' ' || char == '\t'
			continue
		}
		if char == '\\' && quote != '\'' {
			escaped = true
			escapedWhitespace = false
			continue
		}
		if quote != 0 {
			if char == quote {
				quote = 0
			}
			escapedWhitespace = false
			continue
		}
		if char == '\'' || char == '"' {
			quote = char
			escapedWhitespace = false
			continue
		}
		if char == '#' && index > 0 &&
			(value[index-1] == ' ' || value[index-1] == '\t') &&
			!escapedWhitespace {
			return value[:index]
		}
		escapedWhitespace = false
	}
	return value
}

func unquoteShellWord(value string, resolve func(string) (string, error)) (string, error) {
	var result strings.Builder
	quote := byte(0)
	escaped := false
	for index := 0; index < len(value); {
		char := value[index]
		if escaped {
			if quote == '"' && char != '$' && char != '`' && char != '"' && char != '\\' {
				result.WriteByte('\\')
			}
			result.WriteByte(char)
			escaped = false
			index++
			continue
		}
		if char == '$' && quote != '\'' {
			name, next, ok := shellVariableReference(value, index)
			if !ok || resolve == nil {
				return "", errors.New("unsupported shell variable expansion in relay.env")
			}
			expanded, err := resolve(name)
			if err != nil {
				return "", err
			}
			result.WriteString(expanded)
			index = next
			continue
		}
		if char == '`' && quote != '\'' {
			return "", errors.New("unsupported command substitution in relay.env")
		}
		if quote == 0 {
			switch char {
			case ' ', '\t':
				return "", errors.New("unquoted whitespace in relay.env value")
			case ';', '&', '|', '<', '>', '(', ')', '*', '?', '[', '~':
				return "", fmt.Errorf("unsupported shell syntax %q in relay.env value", char)
			}
		}
		if quote == '\'' {
			if char == '\'' {
				quote = 0
			} else {
				result.WriteByte(char)
			}
			index++
			continue
		}
		if quote == '"' {
			if char == '\\' {
				escaped = true
			} else if char == '"' {
				quote = 0
			} else {
				result.WriteByte(char)
			}
			index++
			continue
		}
		if char == '\\' {
			escaped = true
		} else if char == '\'' || char == '"' {
			quote = char
		} else {
			result.WriteByte(char)
		}
		index++
	}
	if escaped {
		return "", errors.New("unterminated shell escape in relay.env value")
	}
	if quote != 0 {
		return "", errors.New("unterminated shell quote in relay.env value")
	}
	return result.String(), nil
}

func environmentValue(environment []string, key string) (string, bool) {
	prefix := key + "="
	for _, value := range environment {
		if strings.HasPrefix(value, prefix) {
			return strings.TrimPrefix(value, prefix), true
		}
	}
	return "", false
}

func replaceEnvironmentValue(environment []string, key, value string) []string {
	prefix := key + "="
	result := make([]string, 0, len(environment)+1)
	for _, item := range environment {
		if strings.HasPrefix(item, prefix) {
			continue
		}
		result = append(result, item)
	}
	return append(result, key+"="+value)
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
	verifyCtx, cancel := context.WithTimeout(ctx, publicVerificationTimeout)
	defer cancel()
	client := &http.Client{
		Timeout: publicRequestTimeout,
		CheckRedirect: func(request *http.Request, via []*http.Request) error {
			if request.URL.Scheme+"://"+request.URL.Host != job.Origin {
				return errPublicOriginRedirect
			}
			return nil
		},
	}
	return verifyPublicWith(verifyCtx, job, client, publicRetryDelay)
}

func verifyPublicWith(
	ctx context.Context,
	job Job,
	client *http.Client,
	retryDelay func(int) time.Duration,
) error {
	nonce := time.Now().UnixNano()
	var lastErr error
	for attempt := 0; ; attempt++ {
		retryable, err := checkPublicVersion(ctx, job, client, nonce, attempt)
		if err == nil {
			return nil
		}
		if !retryable {
			return err
		}
		lastErr = err

		timer := time.NewTimer(retryDelay(attempt))
		select {
		case <-timer.C:
		case <-ctx.Done():
			timer.Stop()
			if errors.Is(ctx.Err(), context.DeadlineExceeded) {
				return fmt.Errorf("public app did not publish the expected bundle before timeout: %w", lastErr)
			}
			return fmt.Errorf("verify public app: %w", ctx.Err())
		}
	}
}

func checkPublicVersion(
	ctx context.Context,
	job Job,
	client *http.Client,
	nonce int64,
	attempt int,
) (bool, error) {
	cacheBust := url.QueryEscape(fmt.Sprintf("%s-%d-%d", job.Revision, nonce, attempt))
	request, err := http.NewRequestWithContext(
		ctx,
		http.MethodGet,
		job.Origin+"/version.json?herdr_deploy_check="+cacheBust,
		nil,
	)
	if err != nil {
		return false, err
	}
	request.Header.Set("Accept", "application/json")
	request.Header.Set("Cache-Control", "no-cache, no-store")
	response, err := client.Do(request)
	if err != nil {
		if errors.Is(err, errPublicOriginRedirect) {
			return false, err
		}
		return true, fmt.Errorf("verify public app: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		retryable := response.StatusCode == http.StatusNotFound ||
			response.StatusCode == http.StatusRequestTimeout ||
			response.StatusCode == http.StatusTooEarly ||
			response.StatusCode == http.StatusTooManyRequests ||
			response.StatusCode >= http.StatusInternalServerError
		return retryable, fmt.Errorf("verify public app: HTTP %d", response.StatusCode)
	}
	var identity map[string]any
	if err := json.NewDecoder(io.LimitReader(response.Body, 64*1024)).Decode(&identity); err != nil {
		return true, fmt.Errorf("decode public version: %w", err)
	}
	data, _ := json.Marshal(identity)
	if err := verifyVersion(data, job.Version, job.Revision); err != nil {
		return true, fmt.Errorf("public web bundle identity: %w", err)
	}
	return false, nil
}

func publicRetryDelay(attempt int) time.Duration {
	delay := time.Duration(attempt+1) * time.Second
	return min(delay, 5*time.Second)
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
	value = ansiEscapePattern.ReplaceAllString(value, "")
	value = strings.Join(strings.Fields(value), " ")
	if len(value) <= limit {
		return value
	}
	const separator = " … "
	if limit <= len(separator) {
		return value[:limit]
	}
	head := (limit - len(separator)) / 2
	tail := limit - len(separator) - head
	return value[:head] + separator + value[len(value)-tail:]
}

func safeError(err error) string {
	return compact(err.Error(), 500)
}
