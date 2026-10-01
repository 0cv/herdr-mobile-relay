// Package tailscalecli implements a separate, CLI-backed Tailscale Serve
// transport. It does not share LocalAPI session authority with internal/tailscale.
// Profile recognition and fixtures never imply live-runtime qualification.
package tailscalecli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/0cv/herdr-mobile-relay/internal/tailscale"
)

const (
	MaxOutputBytes = 1 << 20
	CommandTimeout = 5 * time.Second
	ChildWaitDelay = 2 * time.Second
	AppStoreCLI    = "/Applications/Tailscale.app/Contents/MacOS/Tailscale"
)

var (
	ErrProfileUnavailable   = errors.New("Tailscale CLI profile unavailable")
	ErrTransientUnavailable = errors.New("Tailscale CLI temporarily unavailable")
	ErrLoggedOut            = errors.New("Tailscale node is not authenticated")
	ErrPermissionDenied     = errors.New("Tailscale CLI permission denied")
	ErrUnsupported          = errors.New("unsupported Tailscale CLI profile or schema")
	ErrConflict             = errors.New("Tailscale Serve configuration conflicts with this route")
	ErrUncertain            = errors.New("Tailscale Serve operation has an uncertain outcome")
	ErrOutputTooLong        = errors.New("Tailscale CLI output exceeded the safety limit")
	ErrGUIMode              = errors.New("Tailscale App Store CLI entered GUI mode; verify that the installed build supports TAILSCALE_BE_CLI")
	ErrInvalidJSON          = errors.New("Tailscale CLI returned invalid JSON")
	ErrCommandFailed        = errors.New("Tailscale CLI command failed with an unclassified outcome")
	ErrUnclassified         = errors.New("Tailscale CLI failure is unclassified")
	ErrWorkflowRequired     = errors.New("Tailscale CLI development operations require an in-process isolated workflow")
)

const (
	DevelopmentHTTPSPort   = 8443
	DevelopmentBackendPort = 18377
	DevelopmentPluginPort  = 18378
)

// CommandFailureError records only that a started CLI process exited
// unsuccessfully. Exit codes and stderr are deliberately not interpreted or
// retained: their meanings are profile-specific and may contain private data.
type CommandFailureError struct{}

func (CommandFailureError) Error() string { return ErrCommandFailed.Error() }
func (CommandFailureError) Unwrap() error { return ErrCommandFailed }

type Profile string

const (
	ProfileAppStoreSupplied Profile = "macos-appstore-1.102.4-supplied-metadata"
	ProfileLinuxSource      Profile = "linux-amd64-1.102.4-source-candidate"
	ProfileUnknown          Profile = "unknown"
)

// Identity contains only the stable fields needed to bind an operational
// registration. Account profile details and raw status output are discarded.
type Identity struct {
	NodeID      string
	DNSName     string
	TailnetName string
	UserID      int64
}

type VersionMetadata struct {
	MajorMinorPatch string `json:"majorMinorPatch"`
	Short           string `json:"short"`
	Long            string `json:"long"`
	GitCommit       string `json:"gitCommit"`
	DaemonLong      string `json:"daemonLong"`
	ExtraGitCommit  string `json:"extraGitCommit,omitempty"`
	OSVariant       string `json:"osVariant,omitempty"`
	Capability      int    `json:"cap"`
	IsDev           bool   `json:"isDev,omitempty"`
	GitDirty        bool   `json:"gitDirty,omitempty"`
	UnstableBranch  bool   `json:"unstableBranch,omitempty"`
}

type Inspection struct {
	Identity     Identity
	Profile      Profile
	ProfileKnown bool
	// DevelopmentQualificationEnabled is a code policy limited to the exact
	// App Store candidate. It is not evidence of a real runtime qualification.
	DevelopmentQualificationEnabled bool
	// RuntimeQualified is independent. Shipped builds keep it false; the hosted
	// fixture build may set it only for a marked synthetic CLI executable.
	RuntimeQualified bool
	Version          VersionMetadata
	Serve            tailscale.ServeStatus
}

type PreflightReport struct {
	NodeID                          string  `json:"node_id"`
	DNSName                         string  `json:"dns_name"`
	Origin                          string  `json:"origin"`
	Profile                         Profile `json:"profile"`
	HTTPSPort                       int     `json:"https_port"`
	DevelopmentQualificationEnabled bool    `json:"development_qualification_enabled"`
	RuntimeQualified                bool    `json:"runtime_qualified"`
}

type commandResult struct {
	stdout     []byte
	dispatched bool
}

type runner func(context.Context, string, ...string) (commandResult, error)

type Client struct {
	binary         string
	run            runner
	validateBinary bool
	profileOS      string
	profileArch    string
}

// ResolveBinary chooses one absolute executable without running it. An invalid
// explicit override is an error, not a reason to fall back to PATH.
func ResolveBinary(override, pathValue, goos string) (string, error) {
	return resolveBinary(override, pathValue, goos, AppStoreCLI)
}

func resolveBinary(override, pathValue, goos, appStoreCLI string) (string, error) {
	if strings.TrimSpace(override) != "" {
		if !filepath.IsAbs(override) {
			return "", ErrProfileUnavailable
		}
		return verifiedExecutable(override)
	}

	appStore := ""
	if goos == "darwin" {
		if candidate, err := verifiedExecutable(appStoreCLI); err == nil {
			appStore = candidate
		}
	}
	candidates := make([]string, 0, 8)
	for _, directory := range filepath.SplitList(pathValue) {
		if directory == "" || !filepath.IsAbs(directory) {
			continue
		}
		candidate, err := verifiedExecutable(filepath.Join(directory, "tailscale"))
		if err != nil {
			continue
		}
		if appStore != "" && isAppStoreCLIWrapper(candidate, appStoreCLI) {
			candidate = appStore
		}
		candidates = append(candidates, candidate)
	}
	if appStore != "" {
		candidates = append(candidates, appStore)
	}
	unique := make(map[string]struct{}, len(candidates))
	for _, candidate := range candidates {
		unique[candidate] = struct{}{}
	}
	if len(unique) == 0 {
		return "", ErrProfileUnavailable
	}
	if len(unique) != 1 {
		return "", fmt.Errorf("%w: multiple executable candidates", ErrProfileUnavailable)
	}
	for candidate := range unique {
		return candidate, nil
	}
	return "", ErrProfileUnavailable
}

// appStoreCLIWrapper is the exact launcher installed by the App Store Tailscale
// app's "Install CLI" action. It only forwards to the bundle executable.
func appStoreCLIWrapper(appStoreCLI string) []byte {
	return []byte("#!/bin/sh\n" + appStoreCLI + " \"$@\"\n")
}

// isAppStoreCLIWrapper reports whether path holds exactly that launcher, making
// it an alias of the bundle candidate. The file is compared, never executed;
// any other content, even an equivalent script, stays a distinct candidate.
func isAppStoreCLIWrapper(path, appStoreCLI string) bool {
	expected := appStoreCLIWrapper(appStoreCLI)
	file, err := os.Open(path)
	if err != nil {
		return false
	}
	defer func() { _ = file.Close() }()
	content, err := io.ReadAll(io.LimitReader(file, int64(len(expected))+1))
	return err == nil && bytes.Equal(content, expected)
}

func verifiedExecutable(path string) (string, error) {
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		return "", ErrProfileUnavailable
	}
	if !filepath.IsAbs(resolved) {
		return "", ErrProfileUnavailable
	}
	info, err := os.Stat(resolved)
	if err != nil || !info.Mode().IsRegular() || info.Mode()&0o111 == 0 {
		return "", ErrProfileUnavailable
	}
	return filepath.Clean(resolved), nil
}

// newClient is package-private so production CLI execution can only be reached
// through the validated development workflow constructors. Test helpers may
// still inject synthetic command runners inside this package.
func newClient(binary string) (*Client, error) {
	if !filepath.IsAbs(binary) {
		return nil, ErrProfileUnavailable
	}
	selected, err := verifiedExecutable(binary)
	if err != nil {
		return nil, err
	}
	if fixtureCLIExecutableRequired() && !isSyntheticFixtureCLI(selected) {
		return nil, fmt.Errorf("%w: fixture builds require an injected synthetic CLI executable", ErrProfileUnavailable)
	}
	profileOS, profileArch := profilePlatform()
	return &Client{binary: selected, run: runCommand, validateBinary: true, profileOS: profileOS, profileArch: profileArch}, nil
}

const syntheticFixtureCLIMarker = "HERDR_SYNTHETIC_TAILSCALE_CLI_FIXTURE_V1"

func isSyntheticFixtureCLI(path string) bool {
	file, err := os.Open(path)
	if err != nil {
		return false
	}
	defer file.Close()
	prefix, err := io.ReadAll(io.LimitReader(file, 4096))
	return err == nil && bytes.Contains(prefix, []byte(syntheticFixtureCLIMarker))
}

func newTestClient(binary string, run runner) *Client {
	return &Client{binary: binary, run: run, profileOS: runtime.GOOS, profileArch: runtime.GOARCH}
}

func newTestClientForPlatform(binary string, run runner, goos, goarch string) *Client {
	return &Client{binary: binary, run: run, profileOS: goos, profileArch: goarch}
}

func (c *Client) execute(ctx context.Context, args ...string) (commandResult, error) {
	if c == nil || c.run == nil || c.binary == "" {
		return commandResult{}, ErrProfileUnavailable
	}
	if c.validateBinary {
		selected, err := verifiedExecutable(c.binary)
		if err != nil || selected != c.binary {
			return commandResult{}, ErrProfileUnavailable
		}
	}
	return c.run(ctx, c.binary, args...)
}

// Inspect runs only structured, read-only commands. Raw output is never
// returned, retained in Inspection, or included in errors.
func (c *Client) Inspect(ctx context.Context) (Inspection, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if c == nil || c.run == nil || c.binary == "" {
		return Inspection{}, ErrProfileUnavailable
	}
	statusResult, err := c.execute(ctx, "status", "--json")
	if err != nil {
		return Inspection{}, sanitizeCommandError("status", err)
	}
	if isGUIModeFailure(statusResult.stdout) {
		return Inspection{}, ErrGUIMode
	}
	status, err := tailscale.ParseStatus(statusResult.stdout)
	if err != nil {
		if validateJSON(statusResult.stdout, MaxOutputBytes, 32, 100000) != nil {
			return Inspection{}, ErrInvalidJSON
		}
		return Inspection{}, fmt.Errorf("%w: unsupported status schema", ErrUnsupported)
	}
	if !status.LoggedIn {
		switch status.BackendState {
		case "NeedsLogin", "NoState":
			return Inspection{}, ErrLoggedOut
		case "NeedsMachineAuth":
			return Inspection{}, ErrPermissionDenied
		case "Starting", "Stopped":
			return Inspection{}, ErrTransientUnavailable
		default:
			return Inspection{}, ErrUnsupported
		}
	}
	if !status.MagicDNSEnabled || status.DNSName == "" {
		return Inspection{}, fmt.Errorf("%w: canonical MagicDNS HTTPS identity unavailable", ErrUnsupported)
	}

	versionResult, err := c.execute(ctx, "version", "--json", "--daemon")
	if err != nil {
		return Inspection{}, sanitizeCommandError("version", err)
	}
	if isGUIModeFailure(versionResult.stdout) {
		return Inspection{}, ErrGUIMode
	}
	metadata, err := parseVersion(versionResult.stdout)
	if errors.Is(err, ErrUnsupported) {
		return Inspection{}, err
	}
	if err != nil {
		return Inspection{}, ErrInvalidJSON
	}
	if !consistentVersion(metadata, status.Version) {
		return Inspection{}, fmt.Errorf("%w: inconsistent version metadata", ErrUnsupported)
	}
	profile := identifyProfileFor(metadata, c.profileOS, c.profileArch)
	if profile == ProfileUnknown {
		return Inspection{}, fmt.Errorf("%w: unrecognized version metadata", ErrUnsupported)
	}
	if !developmentQualificationEnabledFor(profile) {
		return Inspection{}, fmt.Errorf("%w: %s is not enabled for development operations", ErrUnsupported, profile)
	}

	serveResult, err := c.execute(ctx, "serve", "status", "--json")
	if err != nil {
		return Inspection{}, sanitizeCommandError("serve status", err)
	}
	if isGUIModeFailure(serveResult.stdout) {
		return Inspection{}, ErrGUIMode
	}
	serve, err := tailscale.ParseServeStatus(serveResult.stdout)
	if err != nil {
		if validateJSON(serveResult.stdout, MaxOutputBytes, 32, 100000) != nil {
			return Inspection{}, ErrInvalidJSON
		}
		return Inspection{}, fmt.Errorf("%w: unsupported Serve schema", ErrUnsupported)
	}
	if !serve.Complete {
		return Inspection{}, fmt.Errorf("%w: incomplete Serve schema", ErrUnsupported)
	}
	return Inspection{
		Identity: Identity{
			NodeID:      status.NodeID,
			DNSName:     strings.TrimSuffix(status.DNSName, "."),
			TailnetName: status.TailnetName,
			UserID:      status.UserID,
		},
		Profile:                         profile,
		ProfileKnown:                    true,
		DevelopmentQualificationEnabled: developmentQualificationEnabledFor(profile),
		RuntimeQualified:                false,
		Version:                         metadata,
		Serve:                           serve,
	}, nil
}

// Preflight performs read-only CLI inspection and derives the exact canonical
// HTTPS origin for the selected live node. It does not reserve or mutate Serve.
func (c *Client) Preflight(ctx context.Context, httpsPort int) (PreflightReport, error) {
	if httpsPort < 1 || httpsPort > 65535 {
		return PreflightReport{}, ErrConflict
	}
	inspection, err := c.Inspect(ctx)
	if err != nil {
		return PreflightReport{}, err
	}
	origin, err := tailscale.Origin(inspection.Identity.DNSName, httpsPort)
	if err != nil {
		return PreflightReport{}, ErrUnsupported
	}
	return PreflightReport{
		NodeID: inspection.Identity.NodeID, DNSName: inspection.Identity.DNSName,
		Origin: origin, Profile: inspection.Profile, HTTPSPort: httpsPort,
		DevelopmentQualificationEnabled: inspection.DevelopmentQualificationEnabled,
		RuntimeQualified:                inspection.RuntimeQualified,
	}, nil
}

func isGUIModeFailure(output []byte) bool {
	return bytes.HasPrefix(bytes.TrimSpace(output), []byte("The Tailscale GUI failed to start:"))
}

func developmentQualificationEnabledFor(profile Profile) bool {
	return profile == ProfileAppStoreSupplied
}

func identifyProfileFor(metadata VersionMetadata, goos, goarch string) Profile {
	if metadata.MajorMinorPatch != "1.102.4" || metadata.Short != "1.102.4" ||
		metadata.IsDev || metadata.GitDirty || metadata.UnstableBranch {
		return ProfileUnknown
	}
	if goos == "darwin" && goarch == "arm64" && metadata.OSVariant == "appstore" &&
		metadata.GitCommit == "3caf7d9e7dcaba589cfc58beda596929733e4fea" &&
		metadata.ExtraGitCommit == "084ee3b64537a1276e56fc38cdf0a711da9f4936" &&
		metadata.Capability == 142 {
		return ProfileAppStoreSupplied
	}
	if goos == "linux" && goarch == "amd64" && metadata.OSVariant == "" &&
		metadata.GitCommit == tailscale.SourceCommit && metadata.ExtraGitCommit == "" &&
		metadata.Capability == 141 {
		return ProfileLinuxSource
	}
	return ProfileUnknown
}

func consistentVersion(metadata VersionMetadata, statusVersion string) bool {
	return metadata.Long == metadata.DaemonLong && metadata.Long == statusVersion &&
		metadata.Capability > 0 && versionLongMatchesMetadata(metadata)
}

func versionLongMatchesMetadata(metadata VersionMetadata) bool {
	parts := strings.Split(metadata.Long, "-")
	if len(parts) < 2 || len(parts) > 3 || parts[0] != metadata.MajorMinorPatch || !validCommitHash(metadata.GitCommit) {
		return false
	}
	if !strings.HasPrefix(parts[1], "t") {
		return false
	}
	commit := strings.TrimPrefix(parts[1], "t")
	if len(commit) < 7 || len(commit) > len(metadata.GitCommit) || !isLowerHex(commit) || !strings.HasPrefix(metadata.GitCommit, commit) {
		return false
	}
	if len(parts) == 2 {
		return metadata.ExtraGitCommit == ""
	}
	if !strings.HasPrefix(parts[2], "g") {
		return false
	}
	extra := strings.TrimPrefix(parts[2], "g")
	return validCommitHash(metadata.ExtraGitCommit) && len(extra) >= 7 &&
		len(extra) <= len(metadata.ExtraGitCommit) && isLowerHex(extra) && strings.HasPrefix(metadata.ExtraGitCommit, extra)
}

func validCommitHash(value string) bool { return len(value) == 40 && isLowerHex(value) }

func isLowerHex(value string) bool {
	if value == "" {
		return false
	}
	for _, character := range value {
		if !strings.ContainsRune("0123456789abcdef", character) {
			return false
		}
	}
	return true
}

func parseVersion(data []byte) (VersionMetadata, error) {
	if err := validateJSON(data, MaxOutputBytes, 32, 100000); err != nil {
		return VersionMetadata{}, err
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(data, &fields); err != nil || fields == nil {
		return VersionMetadata{}, ErrInvalidJSON
	}
	allowed := map[string]bool{
		"majorMinorPatch": true, "short": true, "long": true, "gitCommit": true,
		"daemonLong": true, "extraGitCommit": true, "osVariant": true,
		"cap": true, "isDev": true, "gitDirty": true, "unstableBranch": true,
		"gitCommitTime": true, "tailscaleGoGitHash": true, "upstream": true,
	}
	for key := range fields {
		if !allowed[key] {
			return VersionMetadata{}, ErrUnsupported
		}
	}
	for _, key := range []string{"majorMinorPatch", "short", "long", "gitCommit", "daemonLong", "cap"} {
		if _, ok := fields[key]; !ok {
			return VersionMetadata{}, ErrUnsupported
		}
	}
	var metadata VersionMetadata
	var err error
	if metadata.MajorMinorPatch, err = jsonString(fields, "majorMinorPatch", true); err != nil {
		return VersionMetadata{}, err
	}
	if metadata.Short, err = jsonString(fields, "short", true); err != nil {
		return VersionMetadata{}, err
	}
	if metadata.Long, err = jsonString(fields, "long", true); err != nil {
		return VersionMetadata{}, err
	}
	if metadata.GitCommit, err = jsonString(fields, "gitCommit", true); err != nil {
		return VersionMetadata{}, err
	}
	if metadata.DaemonLong, err = jsonString(fields, "daemonLong", true); err != nil {
		return VersionMetadata{}, err
	}
	if metadata.ExtraGitCommit, err = jsonString(fields, "extraGitCommit", false); err != nil {
		return VersionMetadata{}, err
	}
	if metadata.OSVariant, err = jsonString(fields, "osVariant", false); err != nil {
		return VersionMetadata{}, err
	}
	if metadata.Capability, err = jsonInteger(fields, "cap", true); err != nil || metadata.Capability < 0 {
		return VersionMetadata{}, ErrInvalidJSON
	}
	if metadata.IsDev, err = jsonBoolean(fields, "isDev"); err != nil {
		return VersionMetadata{}, err
	}
	if metadata.GitDirty, err = jsonBoolean(fields, "gitDirty"); err != nil {
		return VersionMetadata{}, err
	}
	if metadata.UnstableBranch, err = jsonBoolean(fields, "unstableBranch"); err != nil {
		return VersionMetadata{}, err
	}
	for _, key := range []string{"gitCommitTime", "tailscaleGoGitHash", "upstream"} {
		if _, err := jsonString(fields, key, false); err != nil {
			return VersionMetadata{}, err
		}
	}
	return metadata, nil
}

func jsonString(fields map[string]json.RawMessage, key string, required bool) (string, error) {
	raw, ok := fields[key]
	if !ok {
		if required {
			return "", ErrInvalidJSON
		}
		return "", nil
	}
	if bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return "", ErrInvalidJSON
	}
	var value string
	if json.Unmarshal(raw, &value) != nil {
		return "", ErrInvalidJSON
	}
	return value, nil
}

func jsonInteger(fields map[string]json.RawMessage, key string, required bool) (int, error) {
	raw, ok := fields[key]
	if !ok {
		if required {
			return 0, ErrInvalidJSON
		}
		return 0, nil
	}
	value, err := strconv.Atoi(string(bytes.TrimSpace(raw)))
	if err != nil || strconv.Itoa(value) != string(bytes.TrimSpace(raw)) {
		return 0, ErrInvalidJSON
	}
	return value, nil
}

func jsonBoolean(fields map[string]json.RawMessage, key string) (bool, error) {
	raw, ok := fields[key]
	if !ok {
		return false, nil
	}
	var value bool
	if json.Unmarshal(raw, &value) != nil || bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return false, ErrInvalidJSON
	}
	return value, nil
}

func validateJSON(data []byte, maxBytes, maxDepth, maxTokens int) error {
	if len(data) == 0 || len(data) > maxBytes || !utf8.Valid(data) {
		return ErrInvalidJSON
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	tokens := 0
	var walk func(int) error
	walk = func(depth int) error {
		tokens++
		if depth > maxDepth || tokens > maxTokens {
			return ErrInvalidJSON
		}
		token, err := decoder.Token()
		if err != nil {
			return ErrInvalidJSON
		}
		delim, ok := token.(json.Delim)
		if !ok {
			return nil
		}
		switch delim {
		case '{':
			seen := make(map[string]bool)
			for decoder.More() {
				keyToken, err := decoder.Token()
				if err != nil {
					return ErrInvalidJSON
				}
				key, ok := keyToken.(string)
				if !ok {
					return ErrInvalidJSON
				}
				folded := strings.ToLower(key)
				if seen[folded] {
					return ErrInvalidJSON
				}
				seen[folded] = true
				if err := walk(depth + 1); err != nil {
					return err
				}
			}
			end, err := decoder.Token()
			if err != nil || end != json.Delim('}') {
				return ErrInvalidJSON
			}
		case '[':
			for decoder.More() {
				if err := walk(depth + 1); err != nil {
					return err
				}
			}
			end, err := decoder.Token()
			if err != nil || end != json.Delim(']') {
				return ErrInvalidJSON
			}
		default:
			return ErrInvalidJSON
		}
		return nil
	}
	if err := walk(0); err != nil {
		return err
	}
	if _, err := decoder.Token(); err != io.EOF {
		return ErrInvalidJSON
	}
	return nil
}

func sanitizeCommandError(operation string, err error) error {
	if errors.Is(err, context.Canceled) {
		return context.Canceled
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return context.DeadlineExceeded
	}
	if errors.Is(err, ErrUncertain) {
		// These calls are read-only, so an incomplete observation is retryable;
		// it cannot represent an ambiguous Serve mutation.
		return fmt.Errorf("%w: Tailscale CLI %s observation was incomplete", ErrTransientUnavailable, operation)
	}
	if errors.Is(err, ErrOutputTooLong) || errors.Is(err, ErrInvalidJSON) ||
		errors.Is(err, ErrPermissionDenied) || errors.Is(err, ErrProfileUnavailable) ||
		errors.Is(err, ErrTransientUnavailable) || errors.Is(err, ErrLoggedOut) || errors.Is(err, ErrUnsupported) ||
		errors.Is(err, ErrConflict) {
		return err
	}
	if errors.Is(err, ErrUnclassified) {
		return fmt.Errorf("%w: Tailscale CLI %s failure requires diagnosis; details omitted", ErrUnclassified, operation)
	}
	var commandFailure CommandFailureError
	if errors.As(err, &commandFailure) {
		return fmt.Errorf("%w: Tailscale CLI %s failed; raw output omitted", commandFailure, operation)
	}
	if errors.Is(err, ErrCommandFailed) {
		return fmt.Errorf("%w: Tailscale CLI %s failed; raw output omitted", ErrCommandFailed, operation)
	}
	return fmt.Errorf("%w: Tailscale CLI %s failure requires diagnosis; details were omitted", ErrUnclassified, operation)
}

type captureBudget struct {
	mu        sync.Mutex
	remaining int
	overflow  bool
}

type boundedCapture struct {
	output bytes.Buffer
	budget *captureBudget
}

func (b *boundedCapture) Write(data []byte) (int, error) {
	b.budget.mu.Lock()
	defer b.budget.mu.Unlock()
	if len(data) > b.budget.remaining {
		b.budget.overflow = true
		return 0, ErrOutputTooLong
	}
	b.budget.remaining -= len(data)
	return b.output.Write(data)
}

func runCommand(parent context.Context, binary string, args ...string) (commandResult, error) {
	if parent == nil {
		parent = context.Background()
	}
	ctx, cancel := context.WithTimeout(parent, CommandTimeout)
	defer cancel()
	command := exec.CommandContext(ctx, binary, args...)
	configureProcessGroup(command)
	command.WaitDelay = ChildWaitDelay
	command.Env = cliEnvironment()
	budget := &captureBudget{remaining: MaxOutputBytes}
	stdout, stderr := boundedCapture{budget: budget}, boundedCapture{budget: budget}
	command.Stdout, command.Stderr = &stdout, &stderr
	if err := command.Start(); err != nil {
		if errors.Is(err, os.ErrPermission) {
			return commandResult{}, ErrPermissionDenied
		}
		return commandResult{}, ErrProfileUnavailable
	}
	result := commandResult{dispatched: true}
	waitErr := command.Wait()
	budget.mu.Lock()
	overflow := budget.overflow
	budget.mu.Unlock()
	if overflow {
		_ = terminateProcessGroup(command)
		return result, ErrOutputTooLong
	}
	if parent.Err() != nil {
		return result, parent.Err()
	}
	if ctx.Err() != nil {
		return result, ErrUncertain
	}
	if waitErr != nil {
		if errors.Is(waitErr, exec.ErrWaitDelay) {
			// Inherited output pipes did not close: output and any mutation
			// acknowledgement are incomplete even if the direct child exited 0.
			_ = terminateProcessGroup(command)
			return result, ErrUncertain
		}
		// Keep dispatch evidence for mutation recovery but discard all captured
		// stdout/stderr from an unsuccessful process.
		return commandResult{dispatched: result.dispatched}, CommandFailureError{}
	}
	budget.mu.Lock()
	result.stdout = append([]byte(nil), stdout.output.Bytes()...)
	budget.mu.Unlock()
	return result, nil
}

// cliEnvironment keeps the real user's app/session context but drops arbitrary
// shell and Tailscale override variables. The absolute CLI path does not need
// PATH for executable selection.
func cliEnvironment() []string {
	return cliEnvironmentFor(runtime.GOOS, os.LookupEnv)
}

// cliEnvironmentFor builds the curated child environment. On macOS the App
// Store CLI is the GUI executable: without a terminal-like environment it tries
// to start the GUI and prints "The Tailscale GUI failed to start" on stdout with
// exit status 0 (observed live with App Store 1.102.4). TAILSCALE_BE_CLI=1 is
// Tailscale's explicit escape hatch that forces CLI mode; any inherited value is
// replaced so a caller cannot select GUI mode.
func cliEnvironmentFor(goos string, lookup func(string) (string, bool)) []string {
	keys := []string{"HOME", "USER", "LOGNAME", "TMPDIR", "XDG_RUNTIME_DIR", "LANG", "LC_ALL"}
	result := make([]string, 0, len(keys)+2)
	for _, key := range keys {
		if value, ok := lookup(key); ok {
			result = append(result, key+"="+value)
		}
	}
	if goos == "darwin" {
		result = append(result, "TAILSCALE_BE_CLI=1")
	}
	// The selected CLI is absolute; its helpers receive only the platform's
	// system tool directories, never a caller-controlled PATH.
	return append(result, "PATH=/usr/bin:/bin:/usr/sbin:/sbin")
}

func validLoopbackBackend(port int) bool {
	return port >= 1 && port <= 65535 && net.ParseIP("127.0.0.1").IsLoopback()
}
