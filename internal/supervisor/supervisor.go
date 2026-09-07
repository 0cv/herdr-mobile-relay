package supervisor

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/0cv/herdr-mobile-relay/internal/activeruntime"
	"github.com/0cv/herdr-mobile-relay/internal/readiness"
	"github.com/0cv/herdr-mobile-relay/internal/release"
)

var (
	ErrAlreadyRunning = errors.New("service supervisor is already running")
	ErrTripped        = errors.New("service supervisor is tripped")
)

const maxReadinessBytes = 64 * 1024

var readinessHTTPClient = &http.Client{
	CheckRedirect: func(*http.Request, []*http.Request) error {
		return errors.New("readiness redirects are refused")
	},
}

var errTopologyTransactionPending = errors.New("managed topology transaction pending")

type Command struct {
	Name string
	Path string
	Args []string
	Env  []string
}

type Config struct {
	Managed           bool
	Relay             Command
	Tunnel            Command
	StatePath         string
	LogDir            string
	ReleaseRoot       string
	Instance          string
	ActiveRuntimePath string
	LocalHealthURL    string
	PublicHealthURL   string
	MaxFailures       int
	InitialBackoff    time.Duration
	MaxBackoff        time.Duration
	StartupTimeout    time.Duration
	HealthInterval    time.Duration
	HealthTimeout     time.Duration
	StableAfter       time.Duration
	StopTimeout       time.Duration
	MaxLogBytes       int64
	LogBackups        int
}

type process interface {
	Wait() error
	Signal(os.Signal) error
	Kill() error
	Alive() bool
}

type startProcess func(Command, io.Writer, io.Writer) (process, error)
type healthCheck func(context.Context) error
type activeRuntimeLoad func() (activeruntime.Snapshot, error)
type stateRead func(string) (State, error)
type stateWrite func(string, State) error
type logOpen func(string, int64, int) (io.Writer, error)
type cycleRun func(context.Context, func(bool) error) cycleResult

type Supervisor struct {
	config     Config
	start      startProcess
	health     healthCheck
	active     activeRuntimeLoad
	readState  stateRead
	writeState stateWrite
	openLog    logOpen
	runCycle   cycleRun
	release    func() (release.Manifest, error)
}

func New(config Config) (*Supervisor, error) {
	if config.Relay.Name == "" || config.Relay.Path == "" || config.Tunnel.Name == "" || config.Tunnel.Path == "" {
		return nil, errors.New("relay and cloudflared commands are required")
	}
	if !filepath.IsAbs(config.Relay.Path) || !filepath.IsAbs(config.Tunnel.Path) || !filepath.IsAbs(config.StatePath) || !filepath.IsAbs(config.LogDir) || !filepath.IsAbs(config.ReleaseRoot) || config.Instance == "" || (config.Managed && !filepath.IsAbs(config.ActiveRuntimePath)) || (!config.Managed && config.ActiveRuntimePath != "") {
		return nil, errors.New("supervisor command and state paths must be absolute")
	}
	if config.MaxFailures < 1 || config.InitialBackoff <= 0 || config.MaxBackoff < config.InitialBackoff || config.StartupTimeout <= 0 || config.HealthInterval <= 0 || config.HealthTimeout <= 0 || config.StableAfter <= 0 || config.StopTimeout <= 0 || config.MaxLogBytes < 1 || config.LogBackups < 0 {
		return nil, errors.New("supervisor limits are invalid")
	}
	if !validLocalHealthURL(config.LocalHealthURL) || !validPublicHealthURL(config.PublicHealthURL) {
		return nil, errors.New("local and public relay health URLs are required")
	}
	supervisor := &Supervisor{
		config: config, start: startCommand, readState: ReadState, writeState: writeState,
		openLog: func(path string, maxBytes int64, backups int) (io.Writer, error) {
			return newRotatingWriter(path, maxBytes, backups)
		},
	}
	supervisor.release = func() (release.Manifest, error) {
		return release.Verify(config.ReleaseRoot, release.CurrentTarget())
	}
	if config.Managed {
		supervisor.active = func() (activeruntime.Snapshot, error) {
			return activeruntime.Load(config.ActiveRuntimePath)
		}
	}
	supervisor.health = supervisor.checkHealth
	supervisor.runCycle = supervisor.runPair
	return supervisor, nil
}

func (s *Supervisor) checkHealth(ctx context.Context) error {
	manifest, err := s.release()
	if err != nil {
		return fmt.Errorf("verify current release: %w", err)
	}
	expected := ReadinessExpectation{
		Managed: s.config.Managed, Instance: s.config.Instance, ReleaseVersion: manifest.Version, Revision: manifest.Revision, BundleHash: manifest.WebHash,
	}
	if s.config.Managed {
		active, err := s.active()
		if err != nil {
			return fmt.Errorf("load active runtime identity: %w", err)
		}
		expected.Generation = active.Generation
	}
	return checkJointReadinessExpectation(ctx, s.config.LocalHealthURL, s.config.PublicHealthURL, expected)
}

func (s *Supervisor) Run(ctx context.Context) error {
	lock, err := acquireLifetimeLock(s.config.StatePath + ".lock")
	if err != nil {
		return err
	}
	defer lock.Close()

	failures := 0
	if state, err := s.readState(s.config.StatePath); err == nil {
		if state.Status == StatusTripped {
			return ErrTripped
		}
		if state.Status == StatusStarting || state.Status == StatusRunning || state.Status == StatusRetrying {
			failures = state.Failures
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	for {
		if err := ctx.Err(); err != nil {
			return s.writeState(s.config.StatePath, State{Status: StatusStopped, Failures: failures})
		}
		if err := s.writeState(s.config.StatePath, State{Status: StatusStarting, Failures: failures}); err != nil {
			return err
		}
		result := s.runCycle(ctx, func(stable bool) error {
			if stable {
				failures = 0
			}
			return s.writeState(s.config.StatePath, State{Status: StatusRunning, Failures: failures})
		})
		if result.cancelled {
			if err := s.writeState(s.config.StatePath, State{Status: StatusStopped, Failures: failures}); err != nil {
				return err
			}
			return nil
		}
		if result.reload {
			continue
		}
		failures++
		if result.unsafe || failures >= s.config.MaxFailures {
			if err := s.writeState(s.config.StatePath, State{Status: StatusTripped, Failures: failures, Reason: result.reason}); err != nil {
				return err
			}
			return ErrTripped
		}
		if err := s.writeState(s.config.StatePath, State{Status: StatusRetrying, Failures: failures, Reason: result.reason}); err != nil {
			return err
		}
		timer := time.NewTimer(retryDelay(s.config, failures))
		select {
		case <-ctx.Done():
			timer.Stop()
			if err := s.writeState(s.config.StatePath, State{Status: StatusStopped, Failures: failures}); err != nil {
				return err
			}
			return nil
		case <-timer.C:
		}
	}
}

type lifetimeLockFile interface {
	Chmod(os.FileMode) error
	Stat() (os.FileInfo, error)
	Fd() uintptr
	Close() error
}

type lifetimeLockIO struct {
	mkdirAll func(string, os.FileMode) error
	chmod    func(string, os.FileMode) error
	openFile func(string, int, os.FileMode) (lifetimeLockFile, error)
	flock    func(int, int) error
}

func defaultLifetimeLockIO() lifetimeLockIO {
	return lifetimeLockIO{
		mkdirAll: os.MkdirAll,
		chmod:    os.Chmod,
		openFile: func(path string, flag int, mode os.FileMode) (lifetimeLockFile, error) {
			return os.OpenFile(path, flag, mode)
		},
		flock: syscall.Flock,
	}
}

func acquireLifetimeLock(path string) (lifetimeLockFile, error) {
	return acquireLifetimeLockWith(defaultLifetimeLockIO(), path)
}

func acquireLifetimeLockWith(ops lifetimeLockIO, path string) (lifetimeLockFile, error) {
	directory := filepath.Dir(path)
	if err := ops.mkdirAll(directory, 0o700); err != nil {
		return nil, err
	}
	if err := ops.chmod(directory, 0o700); err != nil {
		return nil, err
	}
	file, err := ops.openFile(path, os.O_CREATE|os.O_RDWR|syscall.O_NOFOLLOW, 0o600)
	if err != nil {
		return nil, err
	}
	closeWithError := func(err error) (lifetimeLockFile, error) {
		file.Close()
		return nil, err
	}
	if err := file.Chmod(0o600); err != nil {
		return closeWithError(err)
	}
	info, err := file.Stat()
	if err != nil {
		return closeWithError(err)
	}
	if !info.Mode().IsRegular() || info.Mode().Perm()&0o077 != 0 {
		return closeWithError(errors.New("supervisor lifetime lock must be a private regular file"))
	}
	if err := ops.flock(int(file.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		if errors.Is(err, syscall.EWOULDBLOCK) || errors.Is(err, syscall.EAGAIN) {
			return closeWithError(ErrAlreadyRunning)
		}
		return closeWithError(fmt.Errorf("lock supervisor lifetime: %w", err))
	}
	return file, nil
}

type cycleResult struct {
	reason    string
	cancelled bool
	unsafe    bool
	reload    bool
}

type runningChild struct {
	name    string
	process process
	done    chan struct{}
	err     error
}

func (s *Supervisor) runPair(ctx context.Context, reportRunning func(bool) error) cycleResult {
	var active activeruntime.Snapshot
	if s.config.Managed {
		var err error
		active, err = s.active()
		if err != nil {
			return cycleResult{reason: "load active runtime: " + safeReason(err.Error())}
		}
	}
	relayStdout, err := s.openLog(filepath.Join(s.config.LogDir, "relay.stdout.log"), s.config.MaxLogBytes, s.config.LogBackups)
	if err != nil {
		return cycleResult{reason: "open relay stdout: " + safeReason(err.Error())}
	}
	relayStderr, err := s.openLog(filepath.Join(s.config.LogDir, "relay.stderr.log"), s.config.MaxLogBytes, s.config.LogBackups)
	if err != nil {
		return cycleResult{reason: "open relay stderr: " + safeReason(err.Error())}
	}
	tunnelStdout, err := s.openLog(filepath.Join(s.config.LogDir, "cloudflared.stdout.log"), s.config.MaxLogBytes, s.config.LogBackups)
	if err != nil {
		return cycleResult{reason: "open cloudflared stdout: " + safeReason(err.Error())}
	}
	tunnelStderr, err := s.openLog(filepath.Join(s.config.LogDir, "cloudflared.stderr.log"), s.config.MaxLogBytes, s.config.LogBackups)
	if err != nil {
		return cycleResult{reason: "open cloudflared stderr: " + safeReason(err.Error())}
	}
	relayCommand := s.config.Relay
	if s.config.Managed {
		relayCommand = commandForRuntime(relayCommand, s.config.ActiveRuntimePath, active, s.config.Managed)
	}
	relay, err := s.startChild(relayCommand, relayStdout, relayStderr)
	if err != nil {
		if ctx.Err() != nil {
			return cycleResult{cancelled: true}
		}
		return cycleResult{reason: "start relay: " + safeReason(err.Error())}
	}
	tunnel, err := s.startChild(s.config.Tunnel, tunnelStdout, tunnelStderr)
	if err != nil {
		if result, cancelled := s.stopIfCancelled(ctx, relay, nil); cancelled {
			return result
		}
		stopErr := s.stopChildren(relay, nil)
		return cycleResult{reason: joinFailure("start cloudflared: "+safeReason(err.Error()), stopErr), unsafe: stopErr != nil}
	}

	healthy := false
	var healthySince time.Time
	var topologyPendingSince time.Time
	stableReported := false
	startupTimer := time.NewTimer(s.config.StartupTimeout)
	defer startupTimer.Stop()
	startup := startupTimer.C
	healthTimer := time.NewTimer(0)
	defer healthTimer.Stop()
	for {
		select {
		case <-ctx.Done():
			stopErr := s.stopChildren(relay, tunnel)
			return cycleResult{reason: safeReason(errorString(stopErr)), cancelled: stopErr == nil, unsafe: stopErr != nil}
		case <-relay.done:
			if result, cancelled := s.stopIfCancelled(ctx, relay, tunnel); cancelled {
				return result
			}
			stopErr := s.stopChildren(relay, tunnel)
			return cycleResult{reason: joinFailure(childFailure(relay), stopErr), unsafe: stopErr != nil}
		case <-tunnel.done:
			if result, cancelled := s.stopIfCancelled(ctx, relay, tunnel); cancelled {
				return result
			}
			stopErr := s.stopChildren(relay, tunnel)
			return cycleResult{reason: joinFailure(childFailure(tunnel), stopErr), unsafe: stopErr != nil}
		case <-startup:
			if result, cancelled := s.stopIfCancelled(ctx, relay, tunnel); cancelled {
				return result
			}
			stopErr := s.stopChildren(relay, tunnel)
			return cycleResult{reason: joinFailure("joint readiness startup timeout", stopErr), unsafe: stopErr != nil}
		case <-healthTimer.C:
			if s.config.Managed {
				current, activeErr := s.active()
				if result, cancelled := s.stopIfCancelled(ctx, relay, tunnel); cancelled {
					return result
				}
				if activeErr != nil {
					stopErr := s.stopChildren(relay, tunnel)
					return cycleResult{reason: joinFailure("active runtime became invalid: "+safeReason(activeErr.Error()), stopErr), unsafe: stopErr != nil}
				}
				if current != active {
					stopErr := s.stopChildren(relay, tunnel)
					return cycleResult{reason: joinFailure("active runtime promoted", stopErr), reload: stopErr == nil, unsafe: stopErr != nil}
				}
			}
			healthCtx, cancel := context.WithTimeout(ctx, s.config.HealthTimeout)
			healthErr := s.health(healthCtx)
			cancel()
			if result, cancelled := s.stopIfCancelled(ctx, relay, tunnel); cancelled {
				return result
			}
			if healthErr != nil && healthy {
				if errors.Is(healthErr, errTopologyTransactionPending) {
					if topologyPendingSince.IsZero() {
						topologyPendingSince = time.Now()
					} else if time.Since(topologyPendingSince) >= s.config.StartupTimeout {
						stopErr := s.stopChildren(relay, tunnel)
						return cycleResult{reason: joinFailure("managed topology transaction remained pending beyond recovery timeout", stopErr), unsafe: stopErr != nil}
					}
				} else {
					stopErr := s.stopChildren(relay, tunnel)
					return cycleResult{reason: joinFailure("joint readiness failed: "+safeReason(healthErr.Error()), stopErr), unsafe: stopErr != nil}
				}
			}
			if healthErr == nil {
				topologyPendingSince = time.Time{}
				if !healthy {
					healthy = true
					healthySince = time.Now()
					startupTimer.Stop()
					startup = nil
					err := reportRunning(false)
					if result, cancelled := s.stopIfCancelled(ctx, relay, tunnel); cancelled {
						return result
					}
					if err != nil {
						stopErr := s.stopChildren(relay, tunnel)
						return cycleResult{reason: joinFailure("write running state: "+safeReason(err.Error()), stopErr), unsafe: true}
					}
				}
				if !stableReported && time.Since(healthySince) >= s.config.StableAfter {
					stableReported = true
					err := reportRunning(true)
					if result, cancelled := s.stopIfCancelled(ctx, relay, tunnel); cancelled {
						return result
					}
					if err != nil {
						stopErr := s.stopChildren(relay, tunnel)
						return cycleResult{reason: joinFailure("write stable state: "+safeReason(err.Error()), stopErr), unsafe: true}
					}
				}
			}
			healthTimer.Reset(s.config.HealthInterval)
		}
	}
}

func (s *Supervisor) stopIfCancelled(ctx context.Context, relay, tunnel *runningChild) (cycleResult, bool) {
	if ctx.Err() == nil {
		return cycleResult{}, false
	}
	stopErr := s.stopChildren(relay, tunnel)
	return cycleResult{reason: safeReason(errorString(stopErr)), cancelled: stopErr == nil, unsafe: stopErr != nil}, true
}

func (s *Supervisor) startChild(command Command, stdout, stderr io.Writer) (*runningChild, error) {
	process, err := s.start(command, stdout, stderr)
	if err != nil {
		return nil, err
	}
	child := &runningChild{name: command.Name, process: process, done: make(chan struct{})}
	go func() {
		child.err = process.Wait()
		close(child.done)
	}()
	return child, nil
}

func (s *Supervisor) stopChildren(relay, tunnel *runningChild) error {
	children := []*runningChild{relay, tunnel}
	stop := func(child *runningChild) {
		if child != nil && child.process.Alive() {
			_ = child.process.Signal(syscall.SIGTERM)
		}
	}
	stop(relay)
	stop(tunnel)
	if waitChildren(children, s.config.StopTimeout) {
		return nil
	}
	kill := func(child *runningChild) {
		if child != nil && child.process.Alive() {
			_ = child.process.Kill()
		}
	}
	kill(relay)
	kill(tunnel)
	if waitChildren(children, s.config.StopTimeout) {
		return nil
	}
	return errors.New("owned child did not stop after kill")
}

func childDone(child *runningChild) bool {
	select {
	case <-child.done:
		return true
	default:
		return false
	}
}

func waitChildren(children []*runningChild, timeout time.Duration) bool {
	deadline := time.NewTimer(timeout)
	defer deadline.Stop()
	poll := time.NewTicker(5 * time.Millisecond)
	defer poll.Stop()
	for _, child := range children {
		if child == nil {
			continue
		}
		for !childDone(child) || child.process.Alive() {
			select {
			case <-poll.C:
			case <-deadline.C:
				return false
			}
		}
	}
	return true
}

func childFailure(child *runningChild) string {
	if child.err == nil {
		return child.name + " exited"
	}
	return child.name + " exited: " + safeReason(child.err.Error())
}

func joinFailure(reason string, stopErr error) string {
	if stopErr == nil {
		return safeReason(reason)
	}
	return safeReason(reason + "; cleanup: " + stopErr.Error())
}

func errorString(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

func retryDelay(config Config, failures int) time.Duration {
	delay := config.InitialBackoff
	for count := 1; count < failures && delay < config.MaxBackoff; count++ {
		if delay > config.MaxBackoff/2 {
			return config.MaxBackoff
		}
		delay *= 2
	}
	if delay > config.MaxBackoff {
		return config.MaxBackoff
	}
	return delay
}

var runtimeEnvironmentKeys = map[string]bool{
	"HERDR_RELAY_ACTIVE_GENERATION":  true,
	"HERDR_RELAY_ACTIVE_RUNTIME":     true,
	"HERDR_RELAY_EXPECTED_INVENTORY": true,
	"HERDR_RELAY_MANAGED_DEPLOYMENT": true,
	"HERDR_SOCKET_PATH":              true,
}

func commandForRuntime(command Command, activePath string, active activeruntime.Snapshot, managed bool) Command {
	environment := make([]string, 0, len(command.Env)+len(runtimeEnvironmentKeys))
	for _, entry := range command.Env {
		key, _, found := strings.Cut(entry, "=")
		if found && !runtimeEnvironmentKeys[key] {
			environment = append(environment, entry)
		}
	}
	environment = append(environment,
		"HERDR_RELAY_ACTIVE_GENERATION="+active.Generation,
		"HERDR_RELAY_ACTIVE_RUNTIME="+activePath,
		"HERDR_RELAY_EXPECTED_INVENTORY="+active.ExpectedInventoryPath,
		"HERDR_RELAY_MANAGED_DEPLOYMENT="+strconv.FormatBool(managed),
		"HERDR_SOCKET_PATH="+active.SocketPath,
	)
	sort.Strings(environment)
	command.Env = environment
	return command
}

func startCommand(command Command, stdout, stderr io.Writer) (process, error) {
	child := exec.Command(command.Path, command.Args...)
	child.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	child.Env = append([]string(nil), command.Env...)
	child.Stdout = stdout
	child.Stderr = stderr
	if err := child.Start(); err != nil {
		return nil, err
	}
	return &commandProcess{command: child, processGroup: child.Process.Pid}, nil
}

type commandProcess struct {
	command      *exec.Cmd
	processGroup int
}

func (p *commandProcess) Wait() error { return p.command.Wait() }

func (p *commandProcess) Signal(signal os.Signal) error {
	value, ok := signal.(syscall.Signal)
	if !ok {
		return errors.New("supervisor child signal is not a syscall signal")
	}
	return syscall.Kill(-p.processGroup, value)
}

func (p *commandProcess) Kill() error {
	return syscall.Kill(-p.processGroup, syscall.SIGKILL)
}

func (p *commandProcess) Alive() bool {
	err := syscall.Kill(-p.processGroup, 0)
	return err == nil || errors.Is(err, syscall.EPERM)
}

type readinessIdentity struct {
	Status            string                    `json:"status"`
	Instance          string                    `json:"instance"`
	ReleaseVersion    string                    `json:"release_version"`
	Revision          string                    `json:"revision"`
	BundleHash        string                    `json:"bundle_hash"`
	Generation        string                    `json:"generation"`
	ExpectedInventory *readinessInventoryResult `json:"expected_inventory"`
}

type readinessInventoryResult struct {
	Ready      bool   `json:"ready"`
	State      string `json:"state"`
	Generation string `json:"generation"`
	Expected   int    `json:"expected"`
	Observed   int    `json:"observed"`
}

type ReadinessExpectation struct {
	Managed        bool
	Instance       string
	ReleaseVersion string
	Revision       string
	BundleHash     string
	Generation     string
}

func parseExactReadinessURL(value string) (*url.URL, bool) {
	parsed, err := url.Parse(value)
	if err != nil || parsed.Host == "" || parsed.User != nil || parsed.Path != "/readyz" || parsed.RawPath != "" || parsed.RawQuery != "" || parsed.ForceQuery || parsed.Fragment != "" {
		return nil, false
	}
	return parsed, true
}

func validLocalHealthURL(value string) bool {
	parsed, ok := parseExactReadinessURL(value)
	if !ok || parsed.Scheme != "http" || parsed.Port() == "" {
		return false
	}
	switch strings.ToLower(parsed.Hostname()) {
	case "127.0.0.1", "localhost", "::1":
		return true
	default:
		return false
	}
}

func validPublicHealthURL(value string) bool {
	parsed, ok := parseExactReadinessURL(value)
	if !ok || parsed.Scheme != "https" {
		return false
	}
	host := strings.ToLower(parsed.Hostname())
	if host == "localhost" || net.ParseIP(host) != nil || !strings.Contains(host, ".") {
		return false
	}
	if strings.Trim(host, "abcdefghijklmnopqrstuvwxyz0123456789.-") != "" {
		return false
	}
	return true
}

func checkJointReadiness(ctx context.Context, localURL, publicURL string) error {
	return checkJointReadinessMode(ctx, localURL, publicURL, true)
}

func checkJointReadinessMode(ctx context.Context, localURL, publicURL string, requireGeneration bool) error {
	local, err := readReadinessMode(ctx, localURL, requireGeneration)
	if err != nil {
		return fmt.Errorf("local readiness: %w", err)
	}
	public, err := readReadinessMode(ctx, publicURL, requireGeneration)
	if err != nil {
		return fmt.Errorf("public readiness: %w", err)
	}
	if !sameReadinessIdentity(local, public) {
		return fmt.Errorf("local/public readiness identity mismatch: local=%s/%s/%s/%s public=%s/%s/%s/%s", local.Status, local.Instance, local.Revision, local.Generation, public.Status, public.Instance, public.Revision, public.Generation)
	}
	return validateReadinessMode(local, requireGeneration)
}

func checkJointReadinessExpectation(ctx context.Context, localURL, publicURL string, expected ReadinessExpectation) error {
	local, localErr := readReadinessMode(ctx, localURL, expected.Managed)
	if localErr != nil && !errors.Is(localErr, errTopologyTransactionPending) {
		return fmt.Errorf("local readiness: %w", localErr)
	}
	public, publicErr := readReadinessMode(ctx, publicURL, expected.Managed)
	if publicErr != nil && !errors.Is(publicErr, errTopologyTransactionPending) {
		return fmt.Errorf("public readiness: %w", publicErr)
	}
	pending := errors.Is(localErr, errTopologyTransactionPending) || errors.Is(publicErr, errTopologyTransactionPending)
	if (!pending && !sameReadinessIdentity(local, public)) || (pending && !sameRuntimeIdentity(local, public)) {
		return errors.New("local and public readiness identities differ")
	}
	if err := validateReadinessEndpoint(local, errors.Is(localErr, errTopologyTransactionPending), expected); err != nil {
		return err
	}
	if err := validateReadinessEndpoint(public, errors.Is(publicErr, errTopologyTransactionPending), expected); err != nil {
		return err
	}
	if pending {
		return errTopologyTransactionPending
	}
	return nil
}

func validateReadinessEndpoint(identity readinessIdentity, pending bool, expected ReadinessExpectation) error {
	if err := verifyReadinessIdentity(identity, expected); err != nil {
		return err
	}
	if pending {
		return nil
	}
	return validateReadinessMode(identity, expected.Managed)
}

func VerifyRunningReadiness(ctx context.Context, statePath, localURL, publicURL string, expected ReadinessExpectation) error {
	if !filepath.IsAbs(statePath) || !validLocalHealthURL(localURL) || !validPublicHealthURL(publicURL) || expected.Instance == "" || expected.ReleaseVersion == "" || expected.Revision == "" || expected.BundleHash == "" || (expected.Managed && expected.Generation == "") || (!expected.Managed && expected.Generation != "") {
		return errors.New("exact supervisor state, readiness boundaries, and release identity are required")
	}
	state, err := ReadState(statePath)
	if err != nil {
		return fmt.Errorf("read supervisor state: %w", err)
	}
	if state.Status != StatusRunning {
		return fmt.Errorf("supervisor state is %s, not running", state.Status)
	}
	probe, err := acquireLifetimeLock(statePath + ".lock")
	if err == nil {
		probe.Close()
		return errors.New("supervisor running state is stale: no process owns its lifetime lock")
	}
	if !errors.Is(err, ErrAlreadyRunning) {
		return fmt.Errorf("verify supervisor lifetime: %w", err)
	}
	local, err := readReadinessMode(ctx, localURL, expected.Managed)
	if err != nil {
		return fmt.Errorf("local readiness: %w", err)
	}
	public, err := readReadinessMode(ctx, publicURL, expected.Managed)
	if err != nil {
		return fmt.Errorf("public readiness: %w", err)
	}
	if !sameReadinessIdentity(local, public) {
		return errors.New("local and public readiness identities differ")
	}
	return verifyReadinessExpectation(local, expected)
}

func verifyReadinessExpectation(local readinessIdentity, expected ReadinessExpectation) error {
	if err := verifyReadinessIdentity(local, expected); err != nil {
		return err
	}
	return validateReadinessMode(local, expected.Managed)
}

func verifyReadinessIdentity(local readinessIdentity, expected ReadinessExpectation) error {
	if local.Instance != expected.Instance || local.ReleaseVersion != expected.ReleaseVersion || local.Revision != expected.Revision || local.BundleHash != expected.BundleHash || local.Generation != expected.Generation {
		return errors.New("running relay identity does not match the expected instance, release, web bundle, and generation")
	}
	return nil
}

func validateReadinessMode(identity readinessIdentity, managed bool) error {
	if managed {
		inventory := identity.ExpectedInventory
		if inventory == nil || !inventory.Ready || inventory.Generation != identity.Generation || inventory.Expected != inventory.Observed {
			return errors.New("managed readiness lacks an exact expected-inventory proof")
		}
		switch identity.Status {
		case "ready":
			if inventory.State != "ready" || inventory.Expected < 1 {
				return errors.New("managed ready state lacks a non-empty exact inventory")
			}
		case "acknowledged_empty":
			if inventory.State != "acknowledged_empty" || inventory.Expected != 0 || inventory.Observed != 0 {
				return errors.New("empty managed inventory was not explicitly acknowledged")
			}
		default:
			return errors.New("managed relay is not ready")
		}
	} else if identity.Status != "ready" {
		return errors.New("ordinary relay is not ready")
	}
	return nil
}

func sameReadinessIdentity(left, right readinessIdentity) bool {
	if left.Status != right.Status || left.Instance != right.Instance || left.ReleaseVersion != right.ReleaseVersion || left.Revision != right.Revision || left.BundleHash != right.BundleHash || left.Generation != right.Generation {
		return false
	}
	if left.ExpectedInventory == nil || right.ExpectedInventory == nil {
		return left.ExpectedInventory == nil && right.ExpectedInventory == nil
	}
	return *left.ExpectedInventory == *right.ExpectedInventory
}

func sameRuntimeIdentity(left, right readinessIdentity) bool {
	return left.Instance == right.Instance &&
		left.ReleaseVersion == right.ReleaseVersion &&
		left.Revision == right.Revision &&
		left.BundleHash == right.BundleHash &&
		left.Generation == right.Generation
}

func readReadiness(ctx context.Context, healthURL string) (readinessIdentity, error) {
	return readReadinessMode(ctx, healthURL, true)
}

func readReadinessMode(ctx context.Context, healthURL string, requireGeneration bool) (readinessIdentity, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, healthURL, nil)
	if err != nil {
		return readinessIdentity{}, err
	}
	response, err := readinessHTTPClient.Do(request)
	if err != nil {
		return readinessIdentity{}, err
	}
	defer response.Body.Close()
	data, err := io.ReadAll(io.LimitReader(response.Body, maxReadinessBytes+1))
	if err != nil {
		return readinessIdentity{}, err
	}
	if len(data) > maxReadinessBytes {
		return readinessIdentity{}, errors.New("readiness response exceeds size limit")
	}
	if response.StatusCode != http.StatusOK {
		if response.StatusCode == http.StatusServiceUnavailable && requireGeneration {
			var pending readinessIdentity
			if json.Unmarshal(data, &pending) == nil &&
				pending.Status == "unavailable" &&
				pending.Instance != "" &&
				pending.ReleaseVersion != "" &&
				pending.Revision != "" &&
				pending.BundleHash != "" &&
				pending.Generation != "" &&
				pending.ExpectedInventory != nil &&
				pending.ExpectedInventory.Generation == pending.Generation &&
				pending.ExpectedInventory.State == string(readiness.StateTopologyTransactionPending) {
				return pending, errTopologyTransactionPending
			}
		}
		return readinessIdentity{}, fmt.Errorf("HTTP %d", response.StatusCode)
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	var identity readinessIdentity
	if err := decoder.Decode(&identity); err != nil {
		return readinessIdentity{}, fmt.Errorf("decode readiness: %w", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return readinessIdentity{}, errors.New("readiness response has trailing content")
	}
	if (identity.Status != "ready" && identity.Status != "acknowledged_empty") || identity.Instance == "" || identity.ReleaseVersion == "" || identity.Revision == "" || identity.BundleHash == "" || (requireGeneration && identity.Generation == "") {
		return readinessIdentity{}, errors.New("readiness response has incomplete identity")
	}
	return identity, nil
}
