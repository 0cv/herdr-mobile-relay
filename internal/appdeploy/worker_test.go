package appdeploy

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
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
	"github.com/andybalholm/brotli"
)

func writeWebReleaseFixture(t *testing.T, root string) {
	t.Helper()
	const version = "1.2.3"
	const revision = "abc"
	const build = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	javascriptPath := "assets/app-" + digestFixture([]byte("console.log('fixture');\n")) + ".js"
	stylesheetPath := "assets/app-" + digestFixture([]byte("body { color: black; }\n")) + ".css"
	javascript := []byte("console.log('fixture');\n")
	stylesheet := []byte("body { color: black; }\n")
	entryPath := "builds/1.2.3-1-aaaaaaaaaaaaaaaa/index.html"
	entry := []byte(`<!doctype html><html><head><link rel="stylesheet" href="/` + stylesheetPath + `" integrity="` + integrityFixture(stylesheet) + `" crossorigin="anonymous"></head><body><script src="/` + javascriptPath + `" integrity="` + integrityFixture(javascript) + `" crossorigin="anonymous"></script></body></html>`)
	files := map[string][]byte{
		javascriptPath: javascript,
		stylesheetPath: stylesheet,
		entryPath:      entry,
	}
	for filename, data := range files {
		if err := os.MkdirAll(filepath.Dir(filepath.Join(root, filename)), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(root, filename), data, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	descriptor := release.WebDescriptor{
		Schema:  release.WebDescriptorSchema,
		Version: version,
		Assets:  1,
		Build:   build,
		Entry:   "/" + entryPath,
		Files: map[string]release.WebDescriptorFile{
			"entry":      {Path: entryPath, SHA256: digestFixture(entry), Integrity: integrityFixture(entry)},
			"javascript": {Path: javascriptPath, SHA256: digestFixture(javascript), Integrity: integrityFixture(javascript)},
			"stylesheet": {Path: stylesheetPath, SHA256: digestFixture(stylesheet), Integrity: integrityFixture(stylesheet)},
		},
	}
	writeJSONFixture(t, filepath.Join(root, "release.json"), descriptor)
	writeJSONFixture(t, filepath.Join(root, "version.json"), map[string]any{
		"version":         version,
		"release_version": version,
		"revision":        revision,
		"assets":          1,
		"build":           build,
		"entry":           "/" + entryPath,
		"script":          "/" + javascriptPath,
		"style":           "/" + stylesheetPath,
		"script_sha256":   digestFixture(javascript),
		"style_sha256":    digestFixture(stylesheet),
	})
}

func addFixtureBrotliDigests(t *testing.T, root string) {
	t.Helper()
	descriptor, err := release.LoadWebDescriptor(os.DirFS(root))
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"entry", "javascript", "stylesheet"} {
		file := descriptor.Files[name]
		data, err := os.ReadFile(filepath.Join(root, file.Path))
		if err != nil {
			t.Fatal(err)
		}
		var compressed bytes.Buffer
		encoder := brotli.NewWriterLevel(&compressed, 11)
		if _, err := encoder.Write(data); err != nil {
			t.Fatal(err)
		}
		if err := encoder.Close(); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(root, file.Path+".br"), compressed.Bytes(), 0o644); err != nil {
			t.Fatal(err)
		}
		file.BrotliSHA256 = digestFixture(compressed.Bytes())
		file.BrotliIntegrity = integrityFixture(compressed.Bytes())
		descriptor.Files[name] = file
	}
	writeJSONFixture(t, filepath.Join(root, "release.json"), descriptor)
}

func digestFixture(data []byte) string {
	digest := sha256.Sum256(data)
	return hex.EncodeToString(digest[:])
}

func integrityFixture(data []byte) string {
	digest := sha256.Sum256(data)
	return "sha256-" + base64.StdEncoding.EncodeToString(digest[:])
}

func writeJSONFixture(t *testing.T, filename string, value any) {
	t.Helper()
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filename, data, 0o644); err != nil {
		t.Fatal(err)
	}
}

func webFixtureHandler(root string, versionBody func() []byte) http.HandlerFunc {
	return func(writer http.ResponseWriter, request *http.Request) {
		filename := filepath.Join(root, filepath.FromSlash(strings.TrimPrefix(request.URL.Path, "/")))
		data, err := os.ReadFile(filename)
		if err != nil {
			http.NotFound(writer, request)
			return
		}
		if request.URL.Path == "/version.json" && versionBody != nil {
			data = versionBody()
		}
		if request.Header.Get("Accept-Encoding") == "br" && request.URL.Path != "/version.json" {
			var compressed bytes.Buffer
			encoder := brotli.NewWriterLevel(&compressed, 11)
			if _, err := encoder.Write(data); err != nil {
				http.Error(writer, err.Error(), http.StatusInternalServerError)
				return
			}
			if err := encoder.Close(); err != nil {
				http.Error(writer, err.Error(), http.StatusInternalServerError)
				return
			}
			writer.Header().Set("Content-Encoding", "br")
			data = compressed.Bytes()
		}
		_, _ = writer.Write(data)
	}
}

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
	writeWebReleaseFixture(t, web)
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
	writeWebReleaseFixture(t, web)
	webHash, err := release.WebHashFS(os.DirFS(web))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(web, "builds/1.2.3-1-aaaaaaaaaaaaaaaa/index.html"), []byte("tampered"), 0o644); err != nil {
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
	writeWebReleaseFixture(t, web)
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

func TestVerifyPublicRetriesUntilExpectedBundleIsPublished(t *testing.T) {
	root := t.TempDir()
	writeWebReleaseFixture(t, root)
	var versionRequests atomic.Int32
	cacheBusters := make(chan string, 2)
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Path == "/version.json" && request.Header.Get("Accept-Encoding") == "identity" {
			cacheBusters <- request.URL.Query().Get("herdr_deploy_check")
			if versionRequests.Add(1) == 1 {
				writer.Header().Set("Content-Type", "application/json")
				_, _ = writer.Write([]byte(`{"release_version":"1.2.2","revision":"old"}`))
				return
			}
		}
		webFixtureHandler(root, nil)(writer, request)
	}))
	defer server.Close()

	job := Job{Origin: server.URL, WebRoot: root, Version: "1.2.3", Revision: "abc", WebHash: strings.Repeat("a", 64)}
	if err := verifyPublicWith(t.Context(), job, server.Client(), func(int) time.Duration { return 0 }); err != nil {
		t.Fatal(err)
	}
	if versionRequests.Load() != 2 {
		t.Fatalf("version requests = %d, want 2", versionRequests.Load())
	}
	firstCacheBust, secondCacheBust := <-cacheBusters, <-cacheBusters
	if firstCacheBust == "" || secondCacheBust == "" || firstCacheBust == secondCacheBust {
		t.Fatalf("cache busters = %q, %q", firstCacheBust, secondCacheBust)
	}
}

func TestVerifyPublicTimesOutWithLastObservedIdentity(t *testing.T) {
	root := t.TempDir()
	writeWebReleaseFixture(t, root)
	server := httptest.NewServer(webFixtureHandler(root, func() []byte {
		return []byte(`{"release_version":"1.2.2","revision":"old"}`)
	}))
	defer server.Close()
	ctx, cancel := context.WithTimeout(t.Context(), 50*time.Millisecond)
	defer cancel()

	job := Job{Origin: server.URL, WebRoot: root, Version: "1.2.3", Revision: "abc", WebHash: strings.Repeat("a", 64)}
	err := verifyPublicWith(ctx, job, server.Client(), func(int) time.Duration { return time.Minute })
	if err == nil || !strings.Contains(err.Error(), "before timeout") ||
		!strings.Contains(err.Error(), "got 1.2.2 (old)") {
		t.Fatalf("verifyPublicWith() error = %v", err)
	}
}

func TestVerifyPublicDoesNotRetryPermanentHTTPFailure(t *testing.T) {
	root := t.TempDir()
	writeWebReleaseFixture(t, root)
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		requests.Add(1)
		http.Error(writer, "forbidden", http.StatusForbidden)
	}))
	defer server.Close()

	job := Job{Origin: server.URL, WebRoot: root, Version: "1.2.3", Revision: "abc"}
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

func TestVerifyPublicBundleChecksIdentityAndBrotliRepresentations(t *testing.T) {
	root := t.TempDir()
	writeWebReleaseFixture(t, root)
	server := httptest.NewServer(webFixtureHandler(root, nil))
	defer server.Close()

	job := Job{
		Origin:   server.URL,
		WebRoot:  root,
		Version:  "1.2.3",
		Revision: "abc",
		WebHash:  strings.Repeat("a", 64),
	}
	if err := verifyPublicWith(t.Context(), job, server.Client(), func(int) time.Duration { return 0 }); err != nil {
		t.Fatal(err)
	}
}

func TestVerifyPublicAllowsIdentityFallbackForBrotliProbe(t *testing.T) {
	root := t.TempDir()
	writeWebReleaseFixture(t, root)
	addFixtureBrotliDigests(t, root)
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		// A valid origin may ignore Accept-Encoding: br and return the identity
		// representation. The verifier must compare decoded bytes, not CDN
		// compressor output.
		identityRequest := request.Clone(request.Context())
		identityRequest.Header.Set("Accept-Encoding", "identity")
		webFixtureHandler(root, nil)(writer, identityRequest)
	}))
	defer server.Close()

	job := Job{Origin: server.URL, WebRoot: root, Version: "1.2.3", Revision: "abc"}
	if err := verifyPublicWith(t.Context(), job, server.Client(), func(int) time.Duration { return 0 }); err != nil {
		t.Fatal(err)
	}
}

func TestVerifyPublicBundlePinsTheLocallyVerifiedDescriptor(t *testing.T) {
	targetRoot := t.TempDir()
	publicRoot := t.TempDir()
	writeWebReleaseFixture(t, targetRoot)
	writeWebReleaseFixture(t, publicRoot)
	localDescriptor, err := release.LoadWebDescriptor(os.DirFS(targetRoot))
	if err != nil {
		t.Fatal(err)
	}
	localDescriptor.Build = strings.Repeat("b", 64)
	writeJSONFixture(t, filepath.Join(targetRoot, "release.json"), localDescriptor)
	server := httptest.NewServer(webFixtureHandler(publicRoot, nil))
	defer server.Close()

	job := Job{Origin: server.URL, WebRoot: targetRoot, Version: "1.2.3", Revision: "abc"}
	retryable, err := checkPublicBundle(t.Context(), job, server.Client(), time.Now().UnixNano(), 0)
	if !retryable || err == nil || !strings.Contains(err.Error(), "does not match the verified local target") {
		t.Fatalf("checkPublicBundle() = %v, %v", retryable, err)
	}
}

func TestVerifyPublicBundleChecksCompressedVersionMetadata(t *testing.T) {
	root := t.TempDir()
	writeWebReleaseFixture(t, root)
	var compressedRequests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Path == "/version.json" {
			data, err := os.ReadFile(filepath.Join(root, "version.json"))
			if err != nil {
				http.Error(writer, err.Error(), http.StatusInternalServerError)
				return
			}
			if request.Header.Get("Accept-Encoding") == "br" {
				compressedRequests.Add(1)
				var metadata map[string]any
				if err := json.Unmarshal(data, &metadata); err != nil {
					http.Error(writer, err.Error(), http.StatusInternalServerError)
					return
				}
				metadata["build"] = strings.Repeat("b", 64)
				data, err = json.Marshal(metadata)
				if err != nil {
					http.Error(writer, err.Error(), http.StatusInternalServerError)
					return
				}
				var compressed bytes.Buffer
				encoder := brotli.NewWriterLevel(&compressed, 11)
				_, _ = encoder.Write(data)
				_ = encoder.Close()
				writer.Header().Set("Content-Encoding", "br")
				_, _ = writer.Write(compressed.Bytes())
				return
			}
			_, _ = writer.Write(data)
			return
		}
		webFixtureHandler(root, nil)(writer, request)
	}))
	defer server.Close()

	job := Job{Origin: server.URL, WebRoot: root, Version: "1.2.3", Revision: "abc"}
	retryable, err := checkPublicBundle(t.Context(), job, server.Client(), time.Now().UnixNano(), 0)
	if !retryable || err == nil || !strings.Contains(err.Error(), "compressed web bundle identity") {
		t.Fatalf("checkPublicBundle() = %v, %v", retryable, err)
	}
	if compressedRequests.Load() != 1 {
		t.Fatalf("compressed version requests = %d, want 1", compressedRequests.Load())
	}
}

func TestVerifyPublicBundleRejectsCrossOriginRedirect(t *testing.T) {
	root := t.TempDir()
	writeWebReleaseFixture(t, root)
	upstream := httptest.NewServer(http.NotFoundHandler())
	defer upstream.Close()
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		http.Redirect(writer, request, upstream.URL+request.URL.Path, http.StatusFound)
	}))
	defer server.Close()

	job := Job{
		Origin:   server.URL,
		WebRoot:  root,
		Version:  "1.2.3",
		Revision: "abc",
		WebHash:  strings.Repeat("a", 64),
	}
	err := verifyPublicWith(t.Context(), job, server.Client(), func(int) time.Duration {
		t.Fatal("cross-origin redirect was retried")
		return 0
	})
	if !errors.Is(err, errPublicOriginRedirect) {
		t.Fatalf("verifyPublicWith() error = %v", err)
	}
}
