package appdeploy

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/0cv/herdr-mobile-relay/internal/release"
)

func TestValidateRejectsOverridesAndUnpinnedIdentity(t *testing.T) {
	root := t.TempDir()
	nodeDir := filepath.Join(root, "node")
	if err := os.MkdirAll(nodeDir, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{filepath.Join(root, "npx"), filepath.Join(nodeDir, "node")} {
		if err := os.WriteFile(name, []byte("#!/bin/sh\n"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	web := filepath.Join(root, "web")
	if err := os.MkdirAll(web, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(web, "version.json"), []byte(`{"release_version":"1.2.3","revision":"abc"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	job := Job{
		RuntimeDir: root,
		WebRoot:    web,
		Origin:     "https://example.test",
		Project:    "relay-app",
		Branch:     "main",
		Version:    "1.2.3",
		Revision:   "abc",
		NPXPath:    filepath.Join(root, "npx"),
		NodeDir:    nodeDir,
	}
	webHash, err := release.WebHashFS(os.DirFS(web))
	if err != nil {
		t.Fatal(err)
	}
	job.WebHash = webHash
	if err := validate(job); err != nil {
		t.Fatal(err)
	}
	job.Origin = "https://example.test/override"
	if err := validate(job); err == nil {
		t.Fatal("origin with path accepted")
	}
	job.Origin = "https://example.test"
	job.Branch = "../preview"
	if err := validate(job); err == nil {
		t.Fatal("unsafe branch accepted")
	}
}

func TestRunRejectsWebBundleThatDoesNotMatchReleaseManifest(t *testing.T) {
	root := t.TempDir()
	nodeDir := filepath.Join(root, "node")
	web := filepath.Join(root, "web")
	if err := os.MkdirAll(nodeDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(web, 0o755); err != nil {
		t.Fatal(err)
	}
	npx := filepath.Join(root, "npx")
	for _, name := range []string{npx, filepath.Join(nodeDir, "node")} {
		if err := os.WriteFile(name, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(web, "version.json"), []byte(`{"release_version":"1.2.3","revision":"abc"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	webHash, err := release.WebHashFS(os.DirFS(web))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(web, "index.html"), []byte("tampered"), 0o644); err != nil {
		t.Fatal(err)
	}
	job := Job{
		RuntimeDir: root,
		WebRoot:    web,
		Origin:     "https://example.test",
		Project:    "relay-app",
		Branch:     "main",
		Version:    "1.2.3",
		Revision:   "abc",
		WebHash:    webHash,
		NPXPath:    npx,
		NodeDir:    nodeDir,
	}
	jobPath := filepath.Join(root, "job.json")
	if err := writeManagerJSON(jobPath, job); err != nil {
		t.Fatal(err)
	}
	if err := writeState(filepath.Join(root, "app-deploy-state.json"), State{
		State:          "scheduled",
		TargetVersion:  job.Version,
		TargetRevision: job.Revision,
	}); err != nil {
		t.Fatal(err)
	}
	err = Run(t.Context(), jobPath)
	if err == nil || !strings.Contains(err.Error(), "verified release manifest") {
		t.Fatalf("Run() error = %v", err)
	}
	data, readErr := os.ReadFile(filepath.Join(root, "app-deploy-state.json"))
	if readErr != nil {
		t.Fatal(readErr)
	}
	var state State
	if err := json.Unmarshal(data, &state); err != nil {
		t.Fatal(err)
	}
	if state.State != "failed" || state.FinishedAt == "" || !strings.Contains(state.Error, "verified release manifest") {
		t.Fatalf("state = %#v", state)
	}
	if _, err := os.Stat(jobPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("failed deployment left its job file behind: %v", err)
	}
}

func TestRunPinsWranglerToRelayOwnedWorkingDirectory(t *testing.T) {
	t.Setenv("HERDR_RELAY_ENV", "")
	t.Setenv("HERDR_PLUGIN_CONFIG_DIR", "")
	root := t.TempDir()
	nodeDir := filepath.Join(root, "node")
	web := filepath.Join(root, "web")
	if err := os.MkdirAll(nodeDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(web, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(nodeDir, "node"), []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	recorded := filepath.Join(root, "wrangler-cwd")
	npx := filepath.Join(root, "npx")
	script := fmt.Sprintf("#!/bin/sh\npwd -P > %q\nexit 1\n", recorded)
	if err := os.WriteFile(npx, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(web, "version.json"), []byte(`{"release_version":"1.2.3","revision":"abc"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	webHash, err := release.WebHashFS(os.DirFS(web))
	if err != nil {
		t.Fatal(err)
	}
	job := Job{
		RuntimeDir: root,
		WebRoot:    web,
		Origin:     "https://example.test",
		Project:    "relay-app",
		Branch:     "main",
		Version:    "1.2.3",
		Revision:   "abc",
		WebHash:    webHash,
		NPXPath:    npx,
		NodeDir:    nodeDir,
	}
	jobPath := filepath.Join(root, "job.json")
	if err := writeManagerJSON(jobPath, job); err != nil {
		t.Fatal(err)
	}
	if err := writeState(filepath.Join(root, "app-deploy-state.json"), State{
		State:          "scheduled",
		TargetVersion:  job.Version,
		TargetRevision: job.Revision,
	}); err != nil {
		t.Fatal(err)
	}

	// The worker is spawned by launchctl/systemd-run, so its inherited working
	// directory is unrelated to the relay and may be unwritable.
	t.Chdir(t.TempDir())

	if err := Run(t.Context(), jobPath); err == nil ||
		!strings.Contains(err.Error(), "Wrangler deployment failed") {
		t.Fatalf("Run() error = %v, want Wrangler deployment failure", err)
	}
	data, err := os.ReadFile(recorded)
	if err != nil {
		t.Fatalf("Wrangler did not run: %v", err)
	}
	want, err := filepath.EvalSymlinks(filepath.Join(root, "wrangler"))
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.TrimSpace(string(data)); got != want {
		t.Fatalf("Wrangler working directory = %q, want %q", got, want)
	}
	if _, err := os.Stat(jobPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("failed deployment left its job file behind: %v", err)
	}
}

func TestRunDoesNotOverwriteStateOwnedByAnotherWorker(t *testing.T) {
	root := t.TempDir()
	jobPath := filepath.Join(root, "job.json")
	if err := writeManagerJSON(jobPath, Job{RuntimeDir: root}); err != nil {
		t.Fatal(err)
	}
	if err := writeState(filepath.Join(root, "app-deploy-state.json"), State{
		State:          "deploying",
		TargetVersion:  "1.2.3",
		TargetRevision: "abc",
	}); err != nil {
		t.Fatal(err)
	}
	lock, err := lockFile(filepath.Join(root, "app-deploy.lock"))
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close()

	if err := Run(t.Context(), jobPath); !errors.Is(err, errDeployLocked) {
		t.Fatalf("Run() error = %v", err)
	}
	data, err := os.ReadFile(filepath.Join(root, "app-deploy-state.json"))
	if err != nil {
		t.Fatal(err)
	}
	var state State
	if err := json.Unmarshal(data, &state); err != nil {
		t.Fatal(err)
	}
	if state.State != "deploying" || state.Error != "" {
		t.Fatalf("state = %#v", state)
	}
}

func TestRunCommandContextTerminatesProcessGroup(t *testing.T) {
	root := t.TempDir()
	pidFile := filepath.Join(root, "child.pid")
	scriptPath := filepath.Join(root, "spawn-child.sh")
	script := fmt.Sprintf(
		"#!/bin/sh\ntrap '' TERM\nsleep 30 &\nchild=$!\nprintf '%%s\\n' \"$child\" > %q\nwait\n",
		pidFile,
	)
	if err := os.WriteFile(scriptPath, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(t.Context(), 500*time.Millisecond)
	defer cancel()
	_, err := runCommandContext(ctx, exec.Command("/bin/sh", scriptPath))
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("runCommandContext() error = %v, want deadline exceeded", err)
	}
	pidData, err := os.ReadFile(pidFile)
	if err != nil {
		t.Fatal(err)
	}
	childPID, err := strconv.Atoi(strings.TrimSpace(string(pidData)))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = syscall.Kill(childPID, syscall.SIGKILL)
	})
	deadline := time.Now().Add(time.Second)
	for {
		err := syscall.Kill(childPID, 0)
		if err != nil && !errors.Is(err, syscall.EPERM) {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("child process %d survived process-group cancellation", childPID)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestCommandEnvironmentPinsOneNodeFirstPath(t *testing.T) {
	environment := commandEnvironment("/opt/pinned-node", []string{
		"HOME=/tmp/home",
		"PATH=/usr/local/bin:/usr/bin",
		"TOKEN=secret",
	})
	pathCount := 0
	for _, value := range environment {
		if strings.HasPrefix(value, "PATH=") {
			pathCount++
			if value != "PATH=/opt/pinned-node"+string(os.PathListSeparator)+"/usr/local/bin:/usr/bin" {
				t.Fatalf("PATH = %q", value)
			}
		}
	}
	if pathCount != 1 {
		t.Fatalf("PATH entries = %d, want 1", pathCount)
	}
}

func TestCommandEnvironmentLoadsOnlyCloudflareCredentials(t *testing.T) {
	envFile := filepath.Join(t.TempDir(), "relay.env")
	if err := os.WriteFile(envFile, []byte(
		"CF_TOKEN='api token'\n"+
			"CF_ACCOUNT_ID='account-id # inside value'\n"+
			"CLOUDFLARE_API_TOKEN=\"$CF_TOKEN\" # trailing comment\n"+
			"CLOUDFLARE_ACCOUNT_ID=\"${CF_ACCOUNT_ID}\" # trailing comment\n"+
			"HERDR_RELAY_TOKEN='relay-secret'\n",
	), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HERDR_RELAY_ENV", envFile)
	t.Setenv("HERDR_PLUGIN_CONFIG_DIR", "")

	environment, err := commandEnvironmentWithCloudflareCredentials("/opt/pinned-node", []string{"PATH=/usr/bin"})
	if err != nil {
		t.Fatal(err)
	}
	if token, present := environmentValue(environment, "CLOUDFLARE_API_TOKEN"); !present || token != "api token" {
		t.Fatalf("Cloudflare API token = %q, present = %v", token, present)
	}
	if account, present := environmentValue(environment, "CLOUDFLARE_ACCOUNT_ID"); !present || account != "account-id # inside value" {
		t.Fatalf("Cloudflare account ID = %q, present = %v", account, present)
	}
	if _, present := environmentValue(environment, "HERDR_RELAY_TOKEN"); present {
		t.Fatal("relay token was imported into Wrangler environment")
	}
}

func TestCompactRemovesTerminalFormattingAndKeepsDeploymentCause(t *testing.T) {
	value := "\x1b[31mwrangler\x1b[0m pages deploy /web " +
		"\x1b[31mERROR\x1b[0m A request to Cloudflare failed: API token lacks Pages:Edit"
	got := compact(value, 64)
	if strings.ContainsAny(got, "\x1b") || strings.Contains(got, "[31m") {
		t.Fatalf("compact() retained terminal formatting: %q", got)
	}
	if !strings.Contains(got, "Pages:Edit") {
		t.Fatalf("compact() lost the deployment cause: %q", got)
	}
}

func TestWranglerDeployArgsUseFreshNoCacheUpload(t *testing.T) {
	got := wranglerDeployArgs(Job{
		WebRoot: "/tmp/release/web",
		Project: "herdr-0cv",
		Branch:  "main",
	})
	want := []string{
		"--yes",
		"wrangler@4.125.0",
		"pages",
		"deploy",
		"/tmp/release/web",
		"--project-name",
		"herdr-0cv",
		"--branch",
		"main",
		"--skip-caching",
	}
	if !slices.Equal(got, want) {
		t.Fatalf("wranglerDeployArgs() = %#v, want %#v", got, want)
	}
}
func TestCommandEnvironmentRejectsUnsupportedExpansion(t *testing.T) {
	envFile := filepath.Join(t.TempDir(), "relay.env")
	if err := os.WriteFile(envFile, []byte("CLOUDFLARE_API_TOKEN=$(printf bad)\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HERDR_RELAY_ENV", envFile)
	t.Setenv("HERDR_PLUGIN_CONFIG_DIR", "")

	if _, err := commandEnvironmentWithCloudflareCredentials("/opt/pinned-node", []string{"PATH=/usr/bin"}); err == nil {
		t.Fatal("unsupported command substitution was accepted")
	}
}

func TestUnquoteShellWord(t *testing.T) {
	tests := map[string]struct {
		value string
		want  string
	}{
		"escaped_space": {
			value: `api\ token # comment`,
			want:  "api token",
		},
		"escaped_double_quote": {
			value: `"api\"token" # comment`,
			want:  `api"token`,
		},
		"hash_inside_quotes": {
			value: `"api # token"`,
			want:  "api # token",
		},
		"single_quotes_keep_backslash": {
			value: `'api\ token'`,
			want:  `api\ token`,
		},
	}
	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			got, err := parseShellEnvironmentValue(test.value, nil)
			if err != nil {
				t.Fatalf("parseShellEnvironmentValue(%q) error = %v", test.value, err)
			}
			if got != test.want {
				t.Fatalf("parseShellEnvironmentValue(%q) = %q, want %q", test.value, got, test.want)
			}
		})
	}
}

func TestVerifyPublicRetriesUntilExpectedIdentityIsPublished(t *testing.T) {
	var requests atomic.Int32
	cacheBusters := make(chan string, 2)
	cacheControls := make(chan string, 2)
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		attempt := requests.Add(1)
		cacheBusters <- request.URL.Query().Get("herdr_deploy_check")
		cacheControls <- request.Header.Get("Cache-Control")
		writer.Header().Set("Content-Type", "application/json")
		if attempt == 1 {
			_, _ = writer.Write([]byte(`{"release_version":"1.2.2","revision":"old"}`))
			return
		}
		_, _ = writer.Write([]byte(`{"release_version":"1.2.3","revision":"abc"}`))
	}))
	defer server.Close()

	job := Job{Origin: server.URL, Version: "1.2.3", Revision: "abc"}
	err := verifyPublicWith(t.Context(), job, server.Client(), func(int) time.Duration { return 0 })
	if err != nil {
		t.Fatal(err)
	}
	if requests.Load() != 2 {
		t.Fatalf("requests = %d, want 2", requests.Load())
	}
	firstCacheBust, secondCacheBust := <-cacheBusters, <-cacheBusters
	if firstCacheBust == "" || secondCacheBust == "" || firstCacheBust == secondCacheBust {
		t.Fatalf("cache busters = %q, %q", firstCacheBust, secondCacheBust)
	}
	for range 2 {
		if cacheControl := <-cacheControls; cacheControl != "no-cache, no-store" {
			t.Fatalf("Cache-Control = %q", cacheControl)
		}
	}
}

func TestVerifyPublicTimesOutWithLastObservedIdentity(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		writer.Header().Set("Content-Type", "application/json")
		_, _ = writer.Write([]byte(`{"release_version":"1.2.2","revision":"old"}`))
	}))
	defer server.Close()
	ctx, cancel := context.WithTimeout(t.Context(), 50*time.Millisecond)
	defer cancel()

	job := Job{Origin: server.URL, Version: "1.2.3", Revision: "abc"}
	err := verifyPublicWith(ctx, job, server.Client(), func(int) time.Duration { return time.Minute })
	if err == nil || !strings.Contains(err.Error(), "before timeout") ||
		!strings.Contains(err.Error(), "got 1.2.2 (old)") {
		t.Fatalf("verifyPublicWith() error = %v", err)
	}
}

func TestVerifyPublicDoesNotRetryPermanentHTTPFailure(t *testing.T) {
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		requests.Add(1)
		http.Error(writer, "forbidden", http.StatusForbidden)
	}))
	defer server.Close()

	job := Job{Origin: server.URL, Version: "1.2.3", Revision: "abc"}
	err := verifyPublicWith(t.Context(), job, server.Client(), func(int) time.Duration {
		t.Fatal("permanent failure was retried")
		return 0
	})
	if err == nil || !strings.Contains(err.Error(), "HTTP 403") {
		t.Fatalf("verifyPublicWith() error = %v", err)
	}
	if requests.Load() != 1 {
		t.Fatalf("requests = %d, want 1", requests.Load())
	}
}
