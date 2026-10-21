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
		if writer.Header().Get("Content-Type") == "" {
			switch filepath.Ext(filename) {
			case ".html":
				writer.Header().Set("Content-Type", "text/html; charset=utf-8")
			case ".js":
				writer.Header().Set("Content-Type", "text/javascript; charset=utf-8")
			case ".css":
				writer.Header().Set("Content-Type", "text/css; charset=utf-8")
			case ".json":
				writer.Header().Set("Content-Type", "application/json")
			}
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

func TestVerifyPublicRedactsInvalidOrigins(t *testing.T) {
	for _, origin := range []string{
		"https://example.test:bad?token=synthetic-query-marker",
		"https://synthetic-user-marker:synthetic-password-marker@example.test:bad?token=synthetic-query-marker",
		"https://example.test/%ZZ?token=synthetic-query-marker",
		"https://synthetic-user-marker:synthetic-password-marker@example.test?token=synthetic-query-marker",
	} {
		err := VerifyPublic(t.Context(), filepath.Join(t.TempDir(), "unavailable"), origin, "", "")
		if err == nil || err.Error() != "public app origin must be a valid HTTPS origin without credentials, a path, query, or fragment" {
			t.Fatalf("VerifyPublic() invalid-origin error = %v", err)
		}
		if strings.Contains(err.Error(), "synthetic-") || strings.Contains(safeError(err), "synthetic-") {
			t.Fatal("invalid-origin diagnostics exposed query or userinfo markers")
		}
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

func TestVerifyPublicTimesOutWithSanitizedIdentityFailure(t *testing.T) {
	root := t.TempDir()
	writeWebReleaseFixture(t, root)
	server := httptest.NewServer(webFixtureHandler(root, func() []byte {
		return []byte(`{"release_version":"1.2.2","revision":"old"}`)
	}))
	defer server.Close()
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancel()

	job := Job{Origin: server.URL, WebRoot: root, Version: "1.2.3", Revision: "abc", WebHash: strings.Repeat("a", 64)}
	err := verifyPublicWith(ctx, job, server.Client(), func(int) time.Duration { return time.Minute })
	if err == nil || !strings.Contains(err.Error(), "before timeout") ||
		!strings.Contains(err.Error(), "does not match the expected version and revision") ||
		strings.Contains(err.Error(), "1.2.2") || strings.Contains(err.Error(), "old") {
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
	type probe struct {
		path, query, encoding, cacheControl string
	}
	requests := make(chan probe, 32)
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		requests <- probe{request.URL.Path, request.URL.RawQuery, request.Header.Get("Accept-Encoding"), request.Header.Get("Cache-Control")}
		writer.Header().Set("Cache-Control", "no-cache")
		if strings.HasSuffix(request.URL.Path, ".js") && request.Header.Get("Accept-Encoding") == "br" {
			writer.Header().Set("Content-Type", "application/javascript; charset=utf-8")
		}
		webFixtureHandler(root, nil)(writer, request)
	}))
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
	descriptor, err := release.LoadWebDescriptor(os.DirFS(root))
	if err != nil {
		t.Fatal(err)
	}
	counts := make(map[string]int)
	missingProbes := 0
	for len(requests) > 0 {
		request := <-requests
		if request.query == "" {
			if request.cacheControl != "" {
				t.Fatalf("canonical probe bypassed the edge cache: %#v", request)
			}
			counts[request.path+":"+request.encoding]++
			if strings.HasPrefix(request.path, "/assets/herdr-missing-") {
				missingProbes++
				name := strings.TrimSuffix(strings.TrimPrefix(request.path, "/assets/herdr-missing-"), ".js")
				if nonce, err := hex.DecodeString(name); err != nil || len(nonce) != 16 || !strings.HasSuffix(request.path, ".js") {
					t.Fatalf("missing probe path = %q", request.path)
				}
			}
			continue
		}
		counts[request.path+":query:"+request.encoding]++
	}
	for _, kind := range []string{"entry", "javascript", "stylesheet"} {
		for _, encoding := range []string{"identity", "br"} {
			key := "/" + descriptor.Files[kind].Path + ":" + encoding
			if counts[key] != 1 {
				t.Fatalf("canonical probe %s count = %d", key, counts[key])
			}
			key = "/" + descriptor.Files[kind].Path + ":query:" + encoding
			if counts[key] != 1 {
				t.Fatalf("query-busted probe %s count = %d", key, counts[key])
			}
		}
	}
	if counts["/release.json:query:identity"] != 1 || counts["/version.json:query:identity"] != 1 || counts["/version.json:query:br"] != 1 || missingProbes != 1 {
		t.Fatalf("metadata and missing probes = %#v, %d", counts, missingProbes)
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

func TestVerifyPublicWaitsForDescriptorBeforeCanonicalProbes(t *testing.T) {
	root := t.TempDir()
	writeWebReleaseFixture(t, root)
	var descriptorRequests atomic.Int32
	var earlyCanonicalRequests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.RawQuery == "" && descriptorRequests.Load() < 2 {
			earlyCanonicalRequests.Add(1)
		}
		if request.URL.Path == "/release.json" && descriptorRequests.Add(1) == 1 {
			writer.Header().Set("Content-Type", "application/json")
			_, _ = writer.Write([]byte(`{"schema":1,"version":"1.2.2","assets":0,"files":{}}`))
			return
		}
		webFixtureHandler(root, nil)(writer, request)
	}))
	defer server.Close()

	job := Job{Origin: server.URL, WebRoot: root, Version: "1.2.3", Revision: "abc"}
	if err := verifyPublicWith(t.Context(), job, server.Client(), func(int) time.Duration { return 0 }); err != nil {
		t.Fatal(err)
	}
	if descriptorRequests.Load() != 2 || earlyCanonicalRequests.Load() != 0 {
		t.Fatalf("descriptor requests = %d, early canonical requests = %d", descriptorRequests.Load(), earlyCanonicalRequests.Load())
	}
}

func TestVerifyPublicRejectsPoisonedCanonicalResources(t *testing.T) {
	tests := []struct {
		name, kind, encoding, contentType, cacheControl, body, want string
	}{
		{name: "bootstrap HTML at JavaScript URL", kind: "javascript", contentType: "text/html", body: "<!doctype html><script src=\"/herdr-bootstrap.js\"></script>\n", want: "Content-Type"},
		{name: "correct JavaScript with wrong MIME", kind: "javascript", contentType: "text/plain", want: "Content-Type"},
		{name: "wrong entry MIME", kind: "entry", contentType: "text/plain", want: "Content-Type"},
		{name: "wrong stylesheet MIME", kind: "stylesheet", contentType: "text/html", want: "Content-Type"},
		{name: "wrong digest", kind: "javascript", contentType: "text/javascript", body: "incorrect bytes must not appear in errors", want: "digest"},
		{name: "Brotli probe wrong MIME", kind: "javascript", encoding: "br", contentType: "text/html", want: "Content-Type"},
		{name: "Brotli probe wrong digest", kind: "stylesheet", encoding: "br", contentType: "text/css", body: "incorrect bytes must not appear in errors", want: "digest"},
		{name: "immutable JavaScript", kind: "javascript", cacheControl: "public, max-age=31536000, immutable", want: "Cache-Control"},
		{name: "immutable stylesheet", kind: "stylesheet", cacheControl: "public, immutable", want: "Cache-Control"},
		{name: "long max-age", kind: "javascript", cacheControl: "public, max-age=86400", want: "Cache-Control"},
		{name: "quoted mixed-case max-age", kind: "stylesheet", cacheControl: "public, Max-Age=\"31536000\"", want: "Cache-Control"},
		{name: "long shared max-age", kind: "javascript", cacheControl: "public, s-maxage=31536000", want: "Cache-Control"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			root := t.TempDir()
			writeWebReleaseFixture(t, root)
			descriptor, err := release.LoadWebDescriptor(os.DirFS(root))
			if err != nil {
				t.Fatal(err)
			}
			poisonPath := "/" + descriptor.Files[test.kind].Path
			var poisonedRequests atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
				if request.URL.Path != poisonPath || request.URL.RawQuery != "" ||
					test.encoding != "" && request.Header.Get("Accept-Encoding") != test.encoding {
					webFixtureHandler(root, nil)(writer, request)
					return
				}
				poisonedRequests.Add(1)
				if test.cacheControl != "" {
					writer.Header().Add("Cache-Control", "no-cache")
					writer.Header().Add("Cache-Control", test.cacheControl)
				}
				if test.contentType != "" {
					writer.Header().Set("Content-Type", test.contentType)
				}
				if test.body != "" {
					_, _ = writer.Write([]byte(test.body))
					return
				}
				webFixtureHandler(root, nil)(writer, request)
			}))
			defer server.Close()

			job := Job{Origin: server.URL, WebRoot: root, Version: "1.2.3", Revision: "abc"}
			err = verifyPublicWith(t.Context(), job, server.Client(), func(int) time.Duration {
				t.Fatal("poisoned canonical resource was retried")
				return 0
			})
			if err == nil || !strings.Contains(err.Error(), poisonPath) || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("verifyPublicWith() error = %v", err)
			}
			if test.body != "" && strings.Contains(err.Error(), test.body) || strings.Contains(err.Error(), "herdr_deploy_check") {
				t.Fatalf("verification error exposed response bytes or query: %v", err)
			}
			if poisonedRequests.Load() != 1 {
				t.Fatalf("poisoned requests = %d, want 1", poisonedRequests.Load())
			}
		})
	}
}

func TestVerifyPublicRejectsCanonicalCrossOriginRedirectWithoutLeakingQuery(t *testing.T) {
	root := t.TempDir()
	writeWebReleaseFixture(t, root)
	descriptor, err := release.LoadWebDescriptor(os.DirFS(root))
	if err != nil {
		t.Fatal(err)
	}
	canonicalPath := "/" + descriptor.Files["javascript"].Path
	var upstreamRequests atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		upstreamRequests.Add(1)
		http.NotFound(writer, request)
	}))
	defer upstream.Close()
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Path == canonicalPath && request.URL.RawQuery == "" {
			http.Redirect(writer, request, upstream.URL+"/secret-path?token=private-redirect-value", http.StatusFound)
			return
		}
		webFixtureHandler(root, nil)(writer, request)
	}))
	defer server.Close()

	job := Job{Origin: server.URL, WebRoot: root, Version: "1.2.3", Revision: "abc"}
	err = verifyPublicWith(t.Context(), job, server.Client(), func(int) time.Duration {
		t.Fatal("canonical cross-origin redirect was retried")
		return 0
	})
	if !errors.Is(err, errPublicOriginRedirect) || !strings.Contains(err.Error(), canonicalPath) {
		t.Fatalf("verifyPublicWith() error = %v", err)
	}
	if upstreamRequests.Load() != 0 || strings.Contains(err.Error(), "private-redirect-value") || strings.Contains(err.Error(), "secret-path") {
		t.Fatalf("redirect was followed or exposed its target: %v, requests = %d", err, upstreamRequests.Load())
	}
}

func TestVerifyPublicRedactsMalformedCanonicalRedirect(t *testing.T) {
	root := t.TempDir()
	writeWebReleaseFixture(t, root)
	descriptor, err := release.LoadWebDescriptor(os.DirFS(root))
	if err != nil {
		t.Fatal(err)
	}
	canonicalPath := "/" + descriptor.Files["javascript"].Path
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Path == canonicalPath && request.URL.RawQuery == "" {
			writer.Header().Set("Location", "/?token=private-malformed-value%XX")
			writer.WriteHeader(http.StatusFound)
			return
		}
		webFixtureHandler(root, nil)(writer, request)
	}))
	defer server.Close()

	job := Job{Origin: server.URL, WebRoot: root, Version: "1.2.3", Revision: "abc"}
	retryable, err := checkPublicBundle(t.Context(), job, publicClient(server.Client(), server.URL), 1, 0)
	if retryable || !errors.Is(err, errPublicCanonicalRedirect) || !strings.Contains(err.Error(), canonicalPath) || strings.Contains(err.Error(), "private-malformed-value") {
		t.Fatalf("checkPublicBundle() = %v, %v", retryable, err)
	}
}

func TestVerifyPublicRejectsCanonicalSameOriginRedirects(t *testing.T) {
	for _, kind := range []string{"entry", "javascript", "stylesheet", "missing"} {
		for _, encoding := range []string{"identity", "br"} {
			if kind == "missing" && encoding == "br" {
				continue
			}
			for _, target := range []string{"query", "path"} {
				t.Run(kind+"/"+encoding+"/"+target, func(t *testing.T) {
					root := t.TempDir()
					writeWebReleaseFixture(t, root)
					descriptor, err := release.LoadWebDescriptor(os.DirFS(root))
					if err != nil {
						t.Fatal(err)
					}
					canonicalPath := "/" + descriptor.Files[kind].Path
					if kind == "missing" {
						canonicalPath = "/assets/herdr-missing-"
					}
					var redirectedRequests atomic.Int32
					var targetRequests atomic.Int32
					paths := make(chan string, 1)
					server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
						if request.URL.Query().Has("fresh") || request.URL.Path == "/redirect-target.js" {
							targetRequests.Add(1)
							writer.Header().Set("Cache-Control", "no-cache")
							if request.URL.Path == "/redirect-target.js" && kind != "missing" {
								redirectedRequest := request.Clone(request.Context())
								redirectedRequest.URL.Path = canonicalPath
								webFixtureHandler(root, nil)(writer, redirectedRequest)
								return
							}
							webFixtureHandler(root, nil)(writer, request)
							return
						}
						matchesPath := request.URL.Path == canonicalPath || kind == "missing" && strings.HasPrefix(request.URL.Path, canonicalPath)
						if matchesPath && request.URL.RawQuery == "" && request.Header.Get("Accept-Encoding") == encoding {
							redirectedRequests.Add(1)
							paths <- request.URL.Path
							writer.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
							location := request.URL.Path + "?fresh=synthetic-redirect-marker"
							if target == "path" {
								location = "/redirect-target.js?fresh=synthetic-redirect-marker"
							}
							http.Redirect(writer, request, location, http.StatusFound)
							return
						}
						webFixtureHandler(root, nil)(writer, request)
					}))
					defer server.Close()

					job := Job{Origin: server.URL, WebRoot: root, Version: "1.2.3", Revision: "abc"}
					err = verifyPublicWith(t.Context(), job, server.Client(), func(int) time.Duration {
						t.Fatal("canonical redirect was retried")
						return 0
					})
					if !errors.Is(err, errPublicCanonicalRedirect) {
						t.Fatalf("verifyPublicWith() error = %v", err)
					}
					if targetRequests.Load() != 0 || redirectedRequests.Load() != 1 || strings.Contains(err.Error(), "synthetic-redirect-marker") || strings.Contains(err.Error(), "redirect-target.js") {
						t.Fatalf("canonical redirect was followed, retried, or leaked its target: %v", err)
					}
					select {
					case path := <-paths:
						if !strings.Contains(err.Error(), path) {
							t.Fatalf("redirect error did not name its canonical path: %v", err)
						}
					default:
						t.Fatal("canonical redirect was not requested")
					}
				})
			}
		}
	}
}

func TestVerifyPublicPreservesSameOriginRedirectsForQueryBustedChecks(t *testing.T) {
	root := t.TempDir()
	writeWebReleaseFixture(t, root)
	var redirects atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.RawQuery != "" && !request.URL.Query().Has("redirected") {
			redirects.Add(1)
			http.Redirect(writer, request, request.URL.String()+"&redirected=1", http.StatusFound)
			return
		}
		webFixtureHandler(root, nil)(writer, request)
	}))
	defer server.Close()

	job := Job{Origin: server.URL, WebRoot: root, Version: "1.2.3", Revision: "abc"}
	if err := verifyPublicWith(t.Context(), job, server.Client(), func(int) time.Duration { return 0 }); err != nil {
		t.Fatal(err)
	}
	if redirects.Load() != 9 {
		t.Fatalf("query-busted redirects = %d, want 9", redirects.Load())
	}
}

func TestVerifyPublicRedactsMetadataAndEncodingDiagnostics(t *testing.T) {
	const marker = "synthetic-response-marker?token=synthetic-token-marker&query=synthetic-query-marker"
	tests := []struct {
		name, resource, encoding, field, contentEncoding string
	}{
		{name: "revision", resource: "version.json", encoding: "identity", field: "revision"},
		{name: "release version", resource: "version.json", encoding: "identity", field: "release_version"},
		{name: "fallback version", resource: "version.json", encoding: "identity", field: "version"},
		{name: "compressed revision", resource: "version.json", encoding: "br", field: "revision"},
		{name: "invalid version assets", resource: "version.json", encoding: "identity", field: "assets"},
		{name: "invalid release schema", resource: "release.json", encoding: "identity", field: "schema"},
		{name: "release encoding", resource: "release.json", encoding: "identity", contentEncoding: marker},
		{name: "version encoding", resource: "version.json", encoding: "identity", contentEncoding: marker},
		{name: "compressed version encoding", resource: "version.json", encoding: "br", contentEncoding: marker},
		{name: "query JavaScript encoding", resource: "javascript", encoding: "br", contentEncoding: marker},
		{name: "malformed gzip", resource: "release.json", encoding: "identity", contentEncoding: "gzip"},
		{name: "malformed Brotli", resource: "release.json", encoding: "identity", contentEncoding: "br"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			root := t.TempDir()
			writeWebReleaseFixture(t, root)
			resource := test.resource
			if resource == "javascript" {
				descriptor, err := release.LoadWebDescriptor(os.DirFS(root))
				if err != nil {
					t.Fatal(err)
				}
				resource = descriptor.Files["javascript"].Path
			}
			data, err := os.ReadFile(filepath.Join(root, resource))
			if err != nil {
				t.Fatal(err)
			}
			if test.field != "" {
				var metadata map[string]any
				if err := json.Unmarshal(data, &metadata); err != nil {
					t.Fatal(err)
				}
				metadata[test.field] = marker
				if test.field == "version" {
					delete(metadata, "release_version")
				}
				data, err = json.Marshal(metadata)
				if err != nil {
					t.Fatal(err)
				}
			}
			if test.contentEncoding != "" {
				data = []byte(marker)
			}
			if test.encoding == "br" && test.contentEncoding == "" {
				var compressed bytes.Buffer
				encoder := brotli.NewWriter(&compressed)
				_, _ = encoder.Write(data)
				_ = encoder.Close()
				data = compressed.Bytes()
			}
			var faultyRequests atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
				if request.URL.Path != "/"+resource || request.URL.RawQuery == "" || request.Header.Get("Accept-Encoding") != test.encoding {
					webFixtureHandler(root, nil)(writer, request)
					return
				}
				faultyRequests.Add(1)
				if test.contentEncoding != "" {
					writer.Header().Set("Content-Encoding", test.contentEncoding)
				} else if test.encoding == "br" {
					writer.Header().Set("Content-Encoding", "br")
				}
				_, _ = writer.Write(data)
			}))
			defer server.Close()

			job := Job{Origin: server.URL, WebRoot: root, Version: "1.2.3", Revision: "abc"}
			retryable, err := checkPublicBundle(t.Context(), job, publicClient(server.Client(), server.URL), 1, 0)
			if !retryable || err == nil {
				t.Fatalf("checkPublicBundle() = %v, %v", retryable, err)
			}
			for _, text := range []string{err.Error(), safeError(err)} {
				if strings.Contains(text, "synthetic-") || strings.Contains(text, "herdr_deploy_check") {
					t.Fatal("metadata or encoding diagnostics exposed response/query markers")
				}
			}
			ctx, cancel := context.WithTimeout(t.Context(), 500*time.Millisecond)
			defer cancel()
			err = verifyPublicWith(ctx, job, server.Client(), func(int) time.Duration { return time.Minute })
			if err == nil || !strings.Contains(err.Error(), "before timeout") || strings.Contains(err.Error(), "synthetic-") || faultyRequests.Load() != 2 {
				t.Fatalf("verifyPublicWith() did not retain a sanitized retryable failure: %v, requests = %d", err, faultyRequests.Load())
			}
		})
	}
}

func TestRunPersistsSanitizedPublicVerificationFailure(t *testing.T) {
	for _, fault := range []string{"version", "encoding"} {
		t.Run(fault, func(t *testing.T) {
			t.Setenv("HERDR_RELAY_ENV", "")
			t.Setenv("HERDR_PLUGIN_CONFIG_DIR", "")
			root := t.TempDir()
			webRoot := filepath.Join(root, "web")
			nodeDir := filepath.Join(root, "node")
			for _, directory := range []string{webRoot, nodeDir} {
				if err := os.MkdirAll(directory, 0o755); err != nil {
					t.Fatal(err)
				}
			}
			writeWebReleaseFixture(t, webRoot)
			npx := filepath.Join(root, "npx")
			for _, executable := range []string{npx, filepath.Join(nodeDir, "node")} {
				if err := os.WriteFile(executable, []byte("#!/bin/sh\nexit 0\n"), 0o700); err != nil {
					t.Fatal(err)
				}
			}
			var faultyRequests atomic.Int32
			server := httptest.NewTLSServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
				if fault == "version" && request.URL.Path == "/version.json" {
					faultyRequests.Add(1)
					_, _ = writer.Write([]byte(`{"release_version":"synthetic-version-response?query=synthetic-query-value","revision":"synthetic-revision-response?token=synthetic-token-value"}`))
					return
				}
				if fault == "encoding" && request.URL.Path == "/release.json" {
					faultyRequests.Add(1)
					writer.Header().Set("Content-Encoding", "synthetic-encoding-response?token=synthetic-token-value")
				}
				webFixtureHandler(webRoot, nil)(writer, request)
			}))
			defer server.Close()
			previousTransport := http.DefaultTransport
			http.DefaultTransport = server.Client().Transport
			t.Cleanup(func() { http.DefaultTransport = previousTransport })
			webHash, err := release.WebHashFS(os.DirFS(webRoot))
			if err != nil {
				t.Fatal(err)
			}
			job := Job{
				RuntimeDir: root, WebRoot: webRoot, Origin: server.URL,
				Project: "fixture-app", Branch: "main", Version: "1.2.3", Revision: "abc",
				WebHash: webHash, NPXPath: npx, NodeDir: nodeDir,
			}
			jobPath := filepath.Join(root, "job.json")
			writeJSONFixture(t, jobPath, job)
			ctx, cancel := context.WithTimeout(t.Context(), time.Second)
			defer cancel()
			err = Run(ctx, jobPath)
			if err == nil || faultyRequests.Load() == 0 || strings.Contains(err.Error(), "synthetic-") {
				t.Fatalf("Run() did not return a sanitized public verification failure: %v", err)
			}
			stateData, err := os.ReadFile(filepath.Join(root, "app-deploy-state.json"))
			if err != nil {
				t.Fatal(err)
			}
			var state State
			if err := json.Unmarshal(stateData, &state); err != nil {
				t.Fatal(err)
			}
			if state.State != "failed" || state.FinishedAt == "" || state.Error == "" || bytes.Contains(stateData, []byte("synthetic-")) || bytes.Contains(stateData, []byte("herdr_deploy_check")) {
				t.Fatal("failed deployment state was missing or exposed response/query markers")
			}
		})
	}
}

func TestVerifyPublicRejectsSuccessfulMissingAssetProbe(t *testing.T) {
	root := t.TempDir()
	writeWebReleaseFixture(t, root)
	paths := make(chan string, 1)
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if strings.HasPrefix(request.URL.Path, "/assets/herdr-missing-") {
			paths <- request.URL.Path
			writer.Header().Set("Content-Type", "text/html")
			_, _ = writer.Write([]byte("<!doctype html><script src=\"/herdr-bootstrap.js\"></script>\n"))
			return
		}
		webFixtureHandler(root, nil)(writer, request)
	}))
	defer server.Close()

	job := Job{Origin: server.URL, WebRoot: root, Version: "1.2.3", Revision: "abc"}
	err := verifyPublicWith(t.Context(), job, server.Client(), func(int) time.Duration {
		t.Fatal("successful missing asset response was retried")
		return 0
	})
	if err == nil || !strings.Contains(err.Error(), "returned HTTP 200") {
		t.Fatalf("verifyPublicWith() error = %v", err)
	}
	select {
	case path := <-paths:
		if !strings.Contains(err.Error(), path) {
			t.Fatalf("verification error did not name the missing path: %v", err)
		}
	default:
		t.Fatal("missing asset probe was not requested")
	}
}

func TestVerifyPublicRetriesTransientCanonicalFailures(t *testing.T) {
	for _, status := range []int{http.StatusNotFound, http.StatusInternalServerError, 0} {
		t.Run(strconv.Itoa(status), func(t *testing.T) {
			root := t.TempDir()
			writeWebReleaseFixture(t, root)
			descriptor, err := release.LoadWebDescriptor(os.DirFS(root))
			if err != nil {
				t.Fatal(err)
			}
			canonicalPath := "/" + descriptor.Files["javascript"].Path
			var canonicalRequests atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
				if request.URL.Path == canonicalPath && request.URL.RawQuery == "" && canonicalRequests.Add(1) == 1 {
					if status == 0 {
						<-request.Context().Done()
						return
					}
					http.Error(writer, "transient failure", status)
					return
				}
				webFixtureHandler(root, nil)(writer, request)
			}))
			defer server.Close()
			client := server.Client()
			client.Timeout = 500 * time.Millisecond
			ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
			defer cancel()
			job := Job{Origin: server.URL, WebRoot: root, Version: "1.2.3", Revision: "abc"}
			if err := verifyPublicWith(ctx, job, client, func(int) time.Duration { return 0 }); err != nil {
				t.Fatal(err)
			}
			if canonicalRequests.Load() != 3 {
				t.Fatalf("canonical requests = %d, want 3", canonicalRequests.Load())
			}
		})
	}
}
